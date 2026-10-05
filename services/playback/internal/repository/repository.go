package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/playback/model"
)

// Repository 是 playback 服务的数据访问入口。
type Repository struct {
	cache      Cacher
	conn       sqlx.SqlConn
	sessionMd  model.PlaybackSessionModel
	progressMd model.PlaybackProgressModel
	outboxMd   model.PlaybackOutboxModel
}

// New 构造 Repository（生产路径）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds), conn,
		model.NewPlaybackSessionModel(conn),
		model.NewPlaybackProgressModel(conn),
		model.NewPlaybackOutboxModel(conn))
}

// NewWithDeps 用显式依赖构造 Repository；只服务于测试注入，生产代码一律走 New。
// conn 仍然必需：ReportHeartbeat 的「进度 + Outbox」必须走同一个 TransactCtx。
func NewWithDeps(cache Cacher, conn sqlx.SqlConn, sessionMd model.PlaybackSessionModel,
	progressMd model.PlaybackProgressModel, outboxMd model.PlaybackOutboxModel) *Repository {
	return &Repository{
		cache:      cache,
		conn:       conn,
		sessionMd:  sessionMd,
		progressMd: progressMd,
		outboxMd:   outboxMd,
	}
}

// Ping 检查 Redis 连通性（供健康检查与启动自检使用）。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// OutboxModel 返回 playback_outbox 的读写模型，只供发布器轮询与推进状态。
// logic 不得拿它直接写事件：心跳事件必须与进度在同一事务内落库，走 ReportHeartbeat。
func (r *Repository) OutboxModel() model.PlaybackOutboxModel { return r.outboxMd }

// --- 播放会话 ---

// CreateSession 新建播放会话。request_id 冲突时返回 model.ErrDuplicateRequest，
// 由 logic 改为复用既有会话（幂等重放）。
func (r *Repository) CreateSession(ctx context.Context, s *model.PlaybackSession) error {
	return r.sessionMd.Insert(ctx, s)
}

// FindSession 查询播放会话：Redis 短缓存 miss 时回源 MySQL 并回填。
// 不存在返回 (nil, nil)。缓存故障不阻塞播放，降级直读数据库。
func (r *Repository) FindSession(ctx context.Context, sessionId string, now int64) (*model.PlaybackSession, error) {
	s, cacheErr := r.cache.GetSession(ctx, sessionId)
	if cacheErr == nil && s != nil {
		return s, nil
	}
	s, err := r.sessionMd.FindOne(ctx, sessionId)
	if err != nil || s == nil {
		return s, err
	}
	if err := r.cache.SetSession(ctx, s, now); err != nil {
		return s, fmt.Errorf("playback/repository FindSession cache set: %w", err)
	}
	return s, nil
}

// CacheSession 预热会话缓存（签发成功后调用），避免首个分片回源打穿数据库。
func (r *Repository) CacheSession(ctx context.Context, s *model.PlaybackSession, now int64) error {
	return r.cache.SetSession(ctx, s, now)
}

// FindSessionByRequest 按幂等键查询会话；不存在返回 (nil, nil)。
func (r *Repository) FindSessionByRequest(ctx context.Context, requestId string) (*model.PlaybackSession, error) {
	return r.sessionMd.FindByRequest(ctx, requestId)
}

// MarkSessionExpired 把过期会话状态推进为 expired，并失效缓存。
func (r *Repository) MarkSessionExpired(ctx context.Context, sessionId string, now int64) error {
	if err := r.sessionMd.MarkExpired(ctx, sessionId, now); err != nil {
		return err
	}
	return r.cache.DelSession(ctx, sessionId)
}

// RevokeSessionsByContent 撤销某内容下全部有效会话（版权撤回/下架时的授权收敛）。
// 当前没有 RPC 或消费者调用它：内容下架事件由 rights/catalog 发布，playback 侧的
// 消费者尚未接入（见 README「已知缺口」）。在接入之前，被撤回内容的会话最多存活
// 一个 TokenTTL（默认 30 分钟），属于可接受窗口。
func (r *Repository) RevokeSessionsByContent(ctx context.Context, contentType int32, contentId int64) (int64, error) {
	return r.sessionMd.RevokeByContent(ctx, contentType, contentId)
}

// --- 播放进度与事件 ---

// ReportHeartbeat 在同一事务内幂等更新播放进度并写入 Outbox 事件（AGENTS.md §5）。
// 返回落库后的进度（position_ms 只前进不回退）。
func (r *Repository) ReportHeartbeat(ctx context.Context, p *model.PlaybackProgress,
	env *eventenvelope.Envelope) (*model.PlaybackProgress, error) {
	envJSON, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("playback/repository marshal envelope: %w", err)
	}
	occurredAt, err := eventOccurUnix(env.OccurredAt)
	if err != nil {
		return nil, err
	}
	var out *model.PlaybackProgress
	err = r.conn.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		progress, err := r.progressMd.Upsert(tctx, session, p)
		if err != nil {
			return err
		}
		out = progress
		return r.outboxMd.Insert(tctx, session, &model.PlaybackOutbox{
			EventID:       env.EventID,
			EventType:     env.EventType,
			SchemaVersion: int32(env.SchemaVersion),
			AggregateType: env.AggregateType,
			AggregateID:   env.AggregateID,
			Payload:       string(envJSON),
			State:         model.OutboxStatePending,
			OccurredAt:    occurredAt,
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FindProgress 查询会话的播放进度；不存在返回 (nil, nil)。
func (r *Repository) FindProgress(ctx context.Context, sessionId string) (*model.PlaybackProgress, error) {
	return r.progressMd.FindOne(ctx, sessionId)
}

// FindLatestProgress 查询用户对某内容的最新进度（供管理后台断点核对）。
func (r *Repository) FindLatestProgress(ctx context.Context, mid int64, contentType int32, contentId int64) (*model.PlaybackProgress, error) {
	return r.progressMd.FindLatestByMid(ctx, mid, contentType, contentId)
}

// --- 播放计数 ---

// CountPlayOnce 按会话首次放行累加播放计数，返回当前累计值。
// CDN 会对同一次播放的每个分片回源，靠 Redis SETNX 标记保证一个会话只计一次。
// 会话剩余时间不足 1 秒时按 1 秒设置标记 TTL，避免计数丢失。
func (r *Repository) CountPlayOnce(ctx context.Context, s *model.PlaybackSession, now int64) (int64, error) {
	ttl := sessionTTL(s.ExpireAt, now)
	if ttl < 1 {
		ttl = 1
	}
	first, err := r.cache.MarkVerifiedOnce(ctx, s.SessionId, ttl)
	if err != nil {
		return 0, err
	}
	if !first {
		return r.cache.PlayCount(ctx, int64(s.ContentType), s.ContentId)
	}
	return r.cache.IncrPlayCount(ctx, int64(s.ContentType), s.ContentId)
}

// eventOccurUnix 把信封的 RFC3339 occurred_at 转成 outbox 表存储的 Unix 秒。
// 信封字段是契约真源，表列只是发布器用的检索索引。
func eventOccurUnix(occurredAt string) (int64, error) {
	t, err := timeutil.ParseRFC3339(occurredAt)
	if err != nil {
		return 0, fmt.Errorf("playback/repository occurred_at %q: %w", occurredAt, err)
	}
	return t.Unix(), nil
}
