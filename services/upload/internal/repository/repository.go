// Package repository 是 upload 服务的数据访问层。
// 组合 upload_session/upload_chunk/upload_outbox 三个 model，封装 Redis 会话状态短缓存，
// 并提供 OSS 预签名 URL 签发与分片上传完成/取消的占位实现。
// 依据 AGENTS.md §6：客户端直传 OSS，服务端只签发短期预签名 URL，
// 不向客户端下发 OSS 长期密钥；完成上传只推进到 COMPLETED，并与 media.task.v1 事件同事务落库，
// 由 asset/transcode 侧消费该事件接管后续状态机（发布循环见 internal/publisher）。
package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	cryptorand "crypto/rand"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/upload/internal/config"
	"go-video/services/upload/model"
)

// 默认分片大小：5 MiB（OSS/MinIO 分片上传最小分片大小）。
const defaultChunkSize int64 = 5 << 20

// 默认预签名 URL 有效期：15 分钟。
const defaultPresignTTL int64 = 15 * 60

// Cache 封装 upload 的 Redis 缓存操作。
// 会话状态短缓存，推进状态时刷新；分片清单不缓存（流量小，直接 DB）。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixSession   = "up:sess:%s" // upload_id → 会话状态
	cacheTTLSession = 60
)

func keySession(uploadID string) string { return fmt.Sprintf(prefixSession, uploadID) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache { return &Cache{rds: rds} }

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("upload/cache: redis ping failed")
}

// SetSession 刷新会话状态缓存（推进状态时调用）。
func (c *Cache) SetSession(ctx context.Context, uploadID string, state int32) error {
	return c.rds.SetexCtx(ctx, keySession(uploadID), strconv.Itoa(int(state)), cacheTTLSession)
}

// GetSession 读取会话状态缓存。返回 (state, hit, err)：hit=false 表示缓存 miss。
func (c *Cache) GetSession(ctx context.Context, uploadID string) (int32, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keySession(uploadID))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, false, nil
		}
		return 0, false, err
	}
	state, err := strconv.Atoi(bs)
	if err != nil {
		return 0, false, nil
	}
	return int32(state), true, nil
}

// DelSession 失效会话状态缓存。
func (c *Cache) DelSession(ctx context.Context, uploadID string) error {
	_, err := c.rds.DelCtx(ctx, keySession(uploadID))
	return err
}

// OSSConfig 是 OSS/MinIO 连接与签名配置（与 config.OSSConfig 同义）。
type OSSConfig = config.OSSConfig

// Cacher 是 Repository 对缓存面的依赖，*Cache 是唯一生产实现。
// 抽成接口的理由与 comment/rights/playback/transcode 一致：New 只认真 Redis，
// 用例无处塞替身，而「缓存命中是否真的跳过 DB」「终态是否回填缓存」恰恰是
// 本仓库唯一的行为承诺。NewWithDeps 让真实 Repository 带着内存替身跑完整判定链。
type Cacher interface {
	Ping(ctx context.Context) error
	SetSession(ctx context.Context, uploadID string, state int32) error
	GetSession(ctx context.Context, uploadID string) (int32, bool, error)
	DelSession(ctx context.Context, uploadID string) error
}

var _ Cacher = (*Cache)(nil)

// Repository 是 upload 服务的数据访问入口。
type Repository struct {
	cache   Cacher
	conn    sqlx.SqlConn
	session model.UploadSessionModel
	chunk   model.UploadChunkModel
	outbox  model.UploadOutboxModel
	oss     OSSConfig
}

// New 构造 Repository。
func New(rds *redis.Redis, conn sqlx.SqlConn, oss OSSConfig) *Repository {
	return NewWithDeps(NewCache(rds), conn, model.NewUploadSessionModel(conn), model.NewUploadChunkModel(conn),
		model.NewUploadOutboxModel(conn), oss)
}

// NewWithDeps 用给定依赖组装 Repository（测试注入缝）。
// conn 有两个用途：New 用它派生三个 model，CompleteUpload 用它开「状态推进 + Outbox」事务，
// 因此测试必须传事务替身而不是 nil（真出现 nil 连接被解引用即为 panic，比静默走错路径更容易发现）。
func NewWithDeps(cache Cacher, conn sqlx.SqlConn, sessionMd model.UploadSessionModel,
	chunkMd model.UploadChunkModel, outboxMd model.UploadOutboxModel, oss OSSConfig) *Repository {
	return &Repository{
		cache:   cache,
		conn:    conn,
		session: sessionMd,
		chunk:   chunkMd,
		outbox:  outboxMd,
		oss:     oss,
	}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error { return r.cache.Ping(ctx) }

// OutboxModel 返回 upload_outbox 的读写模型，只供发布器轮询与推进状态。
// logic 不得拿它直接写事件：media.task.v1 必须与 COMPLETED 在同一事务内落库，走 CompleteUpload。
func (r *Repository) OutboxModel() model.UploadOutboxModel { return r.outbox }

// refreshState 更新 DB 会话状态并刷新缓存。
func (r *Repository) refreshState(ctx context.Context, uploadID string, state int32) error {
	if err := r.session.UpdateState(ctx, uploadID, state); err != nil {
		return err
	}
	_ = r.cache.SetSession(ctx, uploadID, state)
	return nil
}

// InitUpload 初始化上传会话：生成 upload_id、分配 OSS bucket/object_key 占位、
// 写 upload_session 和 upload_chunk 清单。
// 秒传本期不实现（返回 instant=false），TODO 接入 asset 指纹查询后回填。
func (r *Repository) InitUpload(ctx context.Context, s *model.UploadSession) (*model.UploadSession, error) {
	if s.TotalChunks <= 0 {
		return nil, model.ErrInvalidTotalChunks
	}
	if s.ChunkSize <= 0 {
		s.ChunkSize = defaultChunkSize
	}
	now := nowUnix()
	s.UploadId = newUploadID()
	s.Bucket = r.oss.Bucket
	s.ObjectKey = fmt.Sprintf("uploads/%d/%s/%s", s.Mid, s.UploadId, s.Filename)
	s.State = model.SessionStateInitialized
	s.Ctime = now
	s.Mtime = now
	if err := r.session.Insert(ctx, s); err != nil {
		return nil, err
	}
	chunks := make([]*model.UploadChunk, 0, s.TotalChunks)
	for i := int32(1); i <= s.TotalChunks; i++ {
		chunks = append(chunks, &model.UploadChunk{
			UploadId: s.UploadId,
			ChunkNo:  i,
			Size:     s.ChunkSize,
			State:    model.ChunkStatePending,
			Ctime:    now,
			Mtime:    now,
		})
	}
	if err := r.chunk.InsertBatch(ctx, chunks); err != nil {
		return nil, err
	}
	_ = r.cache.SetSession(ctx, s.UploadId, s.State)
	return s, nil
}

// GetUploadUrl 为分片签发短期预签名 PUT URL。
// 本期占位实现：返回 mock URL；TODO 接入真实 OSS/MinIO SDK（含 STS 短期凭证）。
func (r *Repository) GetUploadUrl(ctx context.Context, uploadID string, chunkNo int32, chunkSize int64) (string, int64, error) {
	// 快路径：先查缓存状态，命中终态直接返回，避免 DB 读。
	if state, hit, err := r.cache.GetSession(ctx, uploadID); err == nil && hit {
		if state == model.SessionStateCompleted {
			return "", 0, model.ErrUploadCompleted
		}
		if state == model.SessionStateAborted {
			return "", 0, model.ErrUploadAborted
		}
	}
	sess, err := r.session.FindOne(ctx, uploadID)
	if err != nil {
		return "", 0, err
	}
	if sess == nil {
		return "", 0, model.ErrUploadNotFound
	}
	if sess.State == model.SessionStateCompleted {
		_ = r.cache.SetSession(ctx, uploadID, sess.State)
		return "", 0, model.ErrUploadCompleted
	}
	if sess.State == model.SessionStateAborted {
		_ = r.cache.SetSession(ctx, uploadID, sess.State)
		return "", 0, model.ErrUploadAborted
	}
	// 推进到 UPLOADING（幂等），刷新缓存
	if sess.State == model.SessionStateInitialized {
		if err := r.refreshState(ctx, uploadID, model.SessionStateUploading); err != nil {
			return "", 0, err
		}
	}
	ttl := r.oss.PresignTTLSeconds
	if ttl <= 0 {
		ttl = defaultPresignTTL
	}
	expiration := nowUnix() + ttl
	// TODO: 接入真实 OSS/MinIO SDK 签发预签名 PUT URL（短期凭证，不暴露长期密钥）。
	url := fmt.Sprintf("https://%s.%s/%s?uploadId=%s&partNumber=%d&X-Amz-Expires=%d&signature=MOCK",
		r.oss.Bucket, r.oss.Endpoint, sess.ObjectKey, uploadID, chunkNo, ttl)
	_ = chunkSize // 客户端透传，真实签名时纳入计算
	return url, expiration, nil
}

// CompleteUpload 校验分片清单、触发 OSS 完成分片上传，并在同一事务内推进状态 +
// 写入 media.task.v1 事件（AGENTS.md §5：业务写与 Outbox 同事务）。
// asset_id 占位返回（asset 服务消费事件后回填真实 ID）。
func (r *Repository) CompleteUpload(ctx context.Context, uploadID string, parts []model.ChunkPart, md5 string) (*model.UploadSession, error) {
	sess, err := r.session.FindOne(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, model.ErrUploadNotFound
	}
	if sess.State == model.SessionStateCompleted {
		return nil, model.ErrUploadCompleted
	}
	if sess.State == model.SessionStateAborted {
		return nil, model.ErrUploadAborted
	}
	chunks, err := r.chunk.ListByUpload(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	if int32(len(parts)) != sess.TotalChunks || int32(len(chunks)) != sess.TotalChunks {
		return nil, model.ErrChunkMismatch
	}
	// 校验清单覆盖全部分片，并标记已上传
	partMap := make(map[int32]string, len(parts))
	for _, p := range parts {
		partMap[p.ChunkNo] = p.Etag
	}
	for _, c := range chunks {
		etag, ok := partMap[c.ChunkNo]
		if !ok || etag == "" {
			return nil, model.ErrChunkMismatch
		}
		if err := r.chunk.MarkUploaded(ctx, uploadID, c.ChunkNo, etag); err != nil {
			return nil, err
		}
	}
	// TODO: 接入真实 OSS/MinIO SDK 调用 CompleteMultipartUpload。
	if err := r.completeMultipartUpload(ctx, sess.Bucket, sess.ObjectKey, uploadID, parts); err != nil {
		_ = r.refreshState(ctx, uploadID, model.SessionStateFailed)
		return nil, err
	}
	// asset_id 占位：后续 asset 服务消费 media.task.v1 事件后回填真实 ID。
	assetID := fmt.Sprintf("asset-placeholder:%s", uploadID)
	// 事件在事务**之前**组装：payload 里的 md5/asset_id 是本次将要写入的值，
	// 组失败（信封契约不合法）时一条写都不发，会话保持可重试而不是「已 COMPLETED 但没事件」。
	env, err := buildMediaTaskEvent(ctx, sess, md5, assetID, nowUnix())
	if err != nil {
		return nil, err
	}
	payload, err := marshalEnvelope(env)
	if err != nil {
		return nil, err
	}
	occurredAt, err := parseOccurredAt(env.OccurredAt)
	if err != nil {
		return nil, err
	}
	err = r.conn.TransactCtx(ctx, func(tctx context.Context, tx sqlx.Session) error {
		if md5 != "" {
			if err := r.session.SetMd5Tx(tctx, tx, uploadID, md5); err != nil {
				return err
			}
		}
		if err := r.session.UpdateAssetIdTx(tctx, tx, uploadID, assetID); err != nil {
			return err
		}
		if err := r.session.UpdateStateTx(tctx, tx, uploadID, model.SessionStateCompleted); err != nil {
			return err
		}
		return r.outbox.Insert(tctx, tx, &model.UploadOutbox{
			EventID:       env.EventID,
			EventType:     env.EventType,
			SchemaVersion: int32(env.SchemaVersion),
			AggregateType: env.AggregateType,
			AggregateID:   env.AggregateID,
			Payload:       payload,
			State:         model.OutboxStatePending,
			OccurredAt:    occurredAt,
		})
	})
	if err != nil {
		return nil, err
	}
	// 缓存只在事务提交后刷：回滚时若已写缓存，GetUploadUrl 的终态快路径会拒掉一个仍可重试的会话。
	if md5 != "" {
		sess.Md5 = md5
	}
	sess.AssetId = assetID
	sess.State = model.SessionStateCompleted
	_ = r.cache.SetSession(ctx, uploadID, sess.State)
	return sess, nil
}

// AbortUpload 取消上传：删除 OSS 分片、清理分片清单、标记会话已取消。
func (r *Repository) AbortUpload(ctx context.Context, uploadID string) error {
	sess, err := r.session.FindOne(ctx, uploadID)
	if err != nil {
		return err
	}
	if sess == nil {
		return model.ErrUploadNotFound
	}
	if sess.State == model.SessionStateCompleted {
		return model.ErrUploadCompleted
	}
	// TODO: 接入真实 OSS/MinIO SDK 调用 AbortMultipartUpload 删除 OSS 侧分片。
	if err := r.abortMultipartUpload(ctx, sess.Bucket, sess.ObjectKey, uploadID); err != nil {
		return err
	}
	if err := r.chunk.DeleteByUpload(ctx, uploadID); err != nil {
		return err
	}
	return r.refreshState(ctx, uploadID, model.SessionStateAborted)
}

// GetUploadStatus 查询会话状态与分片列表。
func (r *Repository) GetUploadStatus(ctx context.Context, uploadID string) (*model.UploadSession, []*model.UploadChunk, error) {
	sess, err := r.session.FindOne(ctx, uploadID)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, model.ErrUploadNotFound
	}
	chunks, err := r.chunk.ListByUpload(ctx, uploadID)
	if err != nil {
		return nil, nil, err
	}
	// 回填缓存，便于后续 GetUploadUrl 快路径判断终态
	_ = r.cache.SetSession(ctx, uploadID, sess.State)
	return sess, chunks, nil
}

// completeMultipartUpload 占位：本期不引入真实 OSS SDK。
// TODO: 接入 OSS/MinIO SDK 的 CompleteMultipartUpload。
func (r *Repository) completeMultipartUpload(ctx context.Context, bucket, objectKey, ossUploadID string, parts []model.ChunkPart) error {
	_ = ctx
	_ = bucket
	_ = objectKey
	_ = ossUploadID
	_ = parts
	return nil
}

// abortMultipartUpload 占位：本期不引入真实 OSS SDK。
// TODO: 接入 OSS/MinIO SDK 的 AbortMultipartUpload。
func (r *Repository) abortMultipartUpload(ctx context.Context, bucket, objectKey, ossUploadID string) error {
	_ = ctx
	_ = bucket
	_ = objectKey
	_ = ossUploadID
	return nil
}

// nowUnix 返回当前 Unix 秒时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// newUploadID 生成上传会话 ID（纳秒时间戳 + 随机 hex，避免依赖外部 ID 生成器）。
func newUploadID() string {
	var b [12]byte
	_, _ = cryptorand.Read(b[:])
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), b[:])
}
