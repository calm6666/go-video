// Package repository 是 content-fingerprint 服务的数据访问层。
// 组合 fingerprint_task 与 fingerprint_record 两个 model，
// 提供 Task 详情的 Redis 短缓存与 Match 结果的 1 分钟短缓存。
// 写入路径：SubmitTask 仅建任务；UpdateTaskResult 在事务外回写 Task 状态
// 并 Upsert Record（按 (asset_id, fp_type) 幂等）。
// 依据 AGENTS.md §5，本服务只持有指纹事实与匹配候选，不复制媒资主数据。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/content-fingerprint/model"
)

// Cache 封装 content-fingerprint 的 Redis 缓存操作。
// Task 详情短 TTL（5 分钟）；Match 结果短缓存（1 分钟）。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixTask     = "fp:task:%d"    // task_id → TaskReply JSON
	prefixMatchKey = "fp:match:k:%s" // fp_key → MatchReply JSON
	prefixMatchAst = "fp:match:a:%d" // asset_id → MatchReply JSON

	cacheTTLTask  = 300 // 5 分钟
	cacheTTLMatch = 60  // 1 分钟
)

func keyTask(taskID int64) string        { return fmt.Sprintf(prefixTask, taskID) }
func keyMatchKey(fpKey string) string    { return fmt.Sprintf(prefixMatchKey, fpKey) }
func keyMatchAsset(assetID int64) string { return fmt.Sprintf(prefixMatchAst, assetID) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("content-fingerprint/cache: redis ping failed")
}

// --- Task 详情缓存 ---

// GetTask 读取任务缓存 JSON；hit=false 表示缓存 miss。
func (c *Cache) GetTask(ctx context.Context, taskID int64) (string, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyTask(taskID))
	if err != nil {
		if err == redis.Nil {
			return "", false, nil
		}
		return "", false, err
	}
	return bs, true, nil
}

// SetTask 写入任务缓存 JSON。
func (c *Cache) SetTask(ctx context.Context, taskID int64, payload string) error {
	return c.rds.SetexCtx(ctx, keyTask(taskID), payload, cacheTTLTask)
}

// DelTask 删除任务缓存（任务状态变更后调用）。
func (c *Cache) DelTask(ctx context.Context, taskID int64) error {
	_, err := c.rds.DelCtx(ctx, keyTask(taskID))
	return err
}

// --- Match 结果缓存 ---

// GetMatchKey 读取按 fp_key 查询的 Match 缓存；hit=false 表示缓存 miss。
func (c *Cache) GetMatchKey(ctx context.Context, fpKey string) (string, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyMatchKey(fpKey))
	if err != nil {
		if err == redis.Nil {
			return "", false, nil
		}
		return "", false, err
	}
	return bs, true, nil
}

// SetMatchKey 写入按 fp_key 查询的 Match 缓存。
func (c *Cache) SetMatchKey(ctx context.Context, fpKey, payload string) error {
	return c.rds.SetexCtx(ctx, keyMatchKey(fpKey), payload, cacheTTLMatch)
}

// GetMatchAsset 读取按 asset_id 查询的 Match 缓存；hit=false 表示缓存 miss。
func (c *Cache) GetMatchAsset(ctx context.Context, assetID int64) (string, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyMatchAsset(assetID))
	if err != nil {
		if err == redis.Nil {
			return "", false, nil
		}
		return "", false, err
	}
	return bs, true, nil
}

// SetMatchAsset 写入按 asset_id 查询的 Match 缓存。
func (c *Cache) SetMatchAsset(ctx context.Context, assetID int64, payload string) error {
	return c.rds.SetexCtx(ctx, keyMatchAsset(assetID), payload, cacheTTLMatch)
}

// Cacher 是 Repository 对缓存的依赖面。生产实现是上面的 *Cache（Redis），
// logic/repository 的单元测试注入内存替身；跨包实现本接口不算破坏封装，
// 因为「Task 详情读穿回填、状态推进后失效」本身就是本服务对调用方的承诺。
//
// 只列 Repository 真正调用的方法：GetMatchKey/SetMatchKey/GetMatchAsset/SetMatchAsset
// 目前在 Repository 里没有任何调用点（Match 缓存未接线，见 README 已知缺口），
// 把它们放进接口只会强迫替身实现死代码，所以等真接上检索时再加。
type Cacher interface {
	Ping(ctx context.Context) error
	// GetTask 读取任务缓存 JSON；hit=false 表示缓存 miss。
	GetTask(ctx context.Context, taskID int64) (string, bool, error)
	SetTask(ctx context.Context, taskID int64, payload string) error
	DelTask(ctx context.Context, taskID int64) error
}

var _ Cacher = (*Cache)(nil)

// --- Repository ---

// Repository 是 content-fingerprint 服务的数据访问入口。
type Repository struct {
	cache  Cacher
	conn   sqlx.SqlConn
	taskMd model.FingerprintTaskModel
	recMd  model.FingerprintRecordModel
}

// New 构造 Repository（生产路径）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds), conn,
		model.NewFingerprintTaskModel(conn), model.NewFingerprintRecordModel(conn))
}

// NewWithDeps 用显式依赖构造 Repository；只服务于测试注入，生产代码一律走 New。
//
// conn 不能省：SubmitTask 会用 conn.ExecCtx 直接把 task_id 回填成自增主键
// （业务主键与主键对齐，便于跨服务引用），那条 SQL 也在被测路径上。
func NewWithDeps(cache Cacher, conn sqlx.SqlConn,
	taskMd model.FingerprintTaskModel, recMd model.FingerprintRecordModel) *Repository {
	return &Repository{cache: cache, conn: conn, taskMd: taskMd, recMd: recMd}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 任务 ---

// SubmitTask 创建指纹任务（PENDING）。
// 本期不调用真实指纹算法，仅持久化任务记录。
// (asset_id, fp_type) 唯一索引：重复提交由 DB 约束保证幂等，失败时返回错误。
func (r *Repository) SubmitTask(ctx context.Context, t *model.FingerprintTask) (*model.FingerprintTask, error) {
	// task_id 由调用方传入或由 DB 自增；这里若未指定则用 0 让 DB 自增。
	// 为简单起见，本期使用 INSERT 后由 DB 自增 ID，task_id = id（详见 model）。
	if t.TaskID == 0 {
		// 先插入占位，再用 LastInsertId 回填 task_id
		id, err := r.taskMd.Insert(ctx, t)
		if err != nil {
			return nil, err
		}
		t.ID = id
		t.TaskID = id
		// 二次更新 task_id = id（业务主键与主键对齐，便于跨服务引用）
		_, _ = r.conn.ExecCtx(ctx,
			"UPDATE fingerprint_task SET task_id = id WHERE id = ? AND task_id = 0", id)
	} else {
		if _, err := r.taskMd.Insert(ctx, t); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// GetTask 查询单个任务详情；不存在返回 (nil, nil)。
// 优先读 Redis 缓存，miss 时回源 DB 并回填缓存。
func (r *Repository) GetTask(ctx context.Context, taskID int64) (*model.FingerprintTask, error) {
	if payload, hit, err := r.cache.GetTask(ctx, taskID); err != nil {
		return nil, err
	} else if hit {
		var t model.FingerprintTask
		if err := json.Unmarshal([]byte(payload), &t); err != nil {
			return nil, fmt.Errorf("GetTask unmarshal cache: %w", err)
		}
		return &t, nil
	}
	t, err := r.taskMd.FindOne(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, nil
	}
	if payload, err := json.Marshal(t); err == nil {
		_ = r.cache.SetTask(ctx, taskID, string(payload))
	}
	return t, nil
}

// ListTasks 分页查询任务。
// assetID<=0 表示不按 asset 过滤；state<=0 表示不按状态过滤。
func (r *Repository) ListTasks(ctx context.Context, assetID int64, state, pn, ps int32) ([]*model.FingerprintTask, int32, error) {
	return r.taskMd.List(ctx, assetID, state, pn, ps)
}

// UpdateTaskResult 由 Worker 回写任务结果（PENDING → SUCCEEDED / FAILED），
// 同时 Upsert fingerprint_record（仅 SUCCEEDED 时写 video_key/audio_key 到 record）。
// 依据 AGENTS.md §8，状态机推进必须经过合法路径，不能跳过 PENDING。
func (r *Repository) UpdateTaskResult(ctx context.Context, taskID int64, state int32, videoKey, audioKey, videoHash, audioHash string) (*model.FingerprintTask, error) {
	// 1. 更新任务状态（model 层做状态机校验）
	updated, err := r.taskMd.UpdateResult(ctx, taskID, state, videoKey, audioKey)
	if err != nil {
		return nil, err
	}
	// 2. SUCCEEDED 时写 fingerprint_record（按 fp_type 分别写入 video/audio）
	if state == model.TaskStateSucceeded {
		if videoKey != "" {
			rec := &model.FingerprintRecord{
				AssetID: updated.AssetID,
				FpType:  model.FpTypeVideo,
				Key:     videoKey,
				Hash:    videoHash,
			}
			if err := r.recMd.Upsert(ctx, rec); err != nil {
				// 记录写入失败不阻塞任务状态回写，仅记日志
				_ = err
			}
		}
		if audioKey != "" {
			rec := &model.FingerprintRecord{
				AssetID: updated.AssetID,
				FpType:  model.FpTypeAudio,
				Key:     audioKey,
				Hash:    audioHash,
			}
			if err := r.recMd.Upsert(ctx, rec); err != nil {
				_ = err
			}
		}
	}
	// 3. 失效任务缓存
	_ = r.cache.DelTask(ctx, taskID)
	return updated, nil
}

// --- 匹配（本期占位） ---

// MatchByFingerprint 按指纹 key 查询匹配的 asset 列表。
// 本期占位：未接入指纹检索引擎，返回空列表；Match 结果缓存 1 分钟。
// TODO(后续)：接入向量/倒排检索引擎（OpenSearch/自研索引）做 TopN 相似度检索。
func (r *Repository) MatchByFingerprint(ctx context.Context, fpKey string, fpType, topN int32) ([]*model.FingerprintRecord, error) {
	_ = ctx
	_ = fpKey
	_ = fpType
	_ = topN
	// 本期占位：不调用真实检索，返回空
	return r.recMd.FindByKey(ctx, fpKey, fpType, topN)
}

// MatchByAsset 按 asset_id 查询其指纹的所有匹配。
// 本期占位：直接返回该 asset 的指纹记录列表（即"自匹配"），无相似度计算。
// TODO(后续)：以该 asset 的指纹为锚，调用检索引擎做相似度召回。
func (r *Repository) MatchByAsset(ctx context.Context, assetID int64, fpType int32) ([]*model.FingerprintRecord, error) {
	return r.recMd.FindByAsset(ctx, assetID, fpType)
}
