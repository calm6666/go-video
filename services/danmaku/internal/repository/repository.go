package repository

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/danmaku/internal/policy"
	"go-video/services/danmaku/model"
)

// Options 是仓库层的运行参数，由 svc 从配置映射而来，
// 使 repository 不依赖 internal/config 包。
type Options struct {
	// Cache 缓存 TTL 参数。
	Cache CacheOptions
	// MaxSegWindow 单次 ListDanmaku 允许的最大分段数。
	MaxSegWindow int
	// MaxListLimit 单次 ListDanmaku 允许返回的最大条数。
	MaxListLimit int
	// BlockWordCacheMaxWords 词库缓存条数上限，超出则直接回源 DB。
	BlockWordCacheMaxWords int
}

// Repository 是 danmaku 服务的数据访问入口。
type Repository struct {
	cache       Cacher
	conn        sqlx.SqlConn
	danmakuMd   model.DanmakuModel
	segmentMd   model.DanmakuSegmentModel
	blockWordMd model.BlockWordModel
	userBlockMd model.UserBlockModel
	reportMd    model.ReportModel
	opLogMd     model.OpLogModel
	opt         Options
}

// New 构造 Repository，是**唯一的生产构造入口**：真 Redis、真 MySQL 连接与 6 个 model。
func New(rds *redis.Redis, conn sqlx.SqlConn, opt Options) *Repository {
	return NewWithDeps(
		NewCache(rds, opt.Cache),
		conn,
		model.NewDanmakuModel(conn),
		model.NewDanmakuSegmentModel(conn),
		model.NewBlockWordModel(conn),
		model.NewUserBlockModel(conn),
		model.NewReportModel(conn),
		model.NewOpLogModel(conn),
		opt,
	)
}

// NewWithDeps 是注入缝：显式给出缓存、事务连接与 6 个 model，供 logic 单测用内存依赖
// 组装真实 Repository（见 cache.go 的 Cacher 注释）。生产代码不得调用本函数，
// 一律走 New；这里保持未导出依赖不变，只开放构造入口，不改变任何查询语义。
func NewWithDeps(
	cache Cacher,
	conn sqlx.SqlConn,
	danmakuMd model.DanmakuModel,
	segmentMd model.DanmakuSegmentModel,
	blockWordMd model.BlockWordModel,
	userBlockMd model.UserBlockModel,
	reportMd model.ReportModel,
	opLogMd model.OpLogModel,
	opt Options,
) *Repository {
	return &Repository{
		cache:       cache,
		conn:        conn,
		danmakuMd:   danmakuMd,
		segmentMd:   segmentMd,
		blockWordMd: blockWordMd,
		userBlockMd: userBlockMd,
		reportMd:    reportMd,
		opLogMd:     opLogMd,
		opt:         opt,
	}
}

// Ping 检查 Redis 与 MySQL 连通性（健康探针对外暴露用）。
func (r *Repository) Ping(ctx context.Context) error {
	if err := r.cache.Ping(ctx); err != nil {
		return err
	}
	var ok int
	if err := r.conn.QueryRowCtx(ctx, &ok, "SELECT 1"); err != nil {
		return fmt.Errorf("danmaku/repository: mysql ping failed: %w", err)
	}
	return nil
}

// Conn 暴露事务入口，供上层在同一连接内写多表。
func (r *Repository) Conn() sqlx.SqlConn { return r.conn }

// --- 防刷屏窗口 ---

// IncrSendWindows 对 mid 与 oid 两个分钟窗口各 +1，返回窗口内累计值。
// 阈值判定由 logic 层用 policy.CheckWindows 完成（业务规则不进 repository）。
func (r *Repository) IncrSendWindows(ctx context.Context, mid, oid int64, now time.Time) (midCount, oidCount int32, err error) {
	midCount, err = r.cache.IncrMidWindow(ctx, mid, now)
	if err != nil {
		return 0, 0, err
	}
	oidCount, err = r.cache.IncrOidWindow(ctx, oid, now)
	if err != nil {
		return midCount, 0, err
	}
	return midCount, oidCount, nil
}

// --- 发送与幂等 ---

// FindByIdempotencyKey 按幂等键回查已落库弹幕；不存在返回 (nil, nil)。
func (r *Repository) FindByIdempotencyKey(ctx context.Context, key string) (*model.Danmaku, error) {
	if key == "" {
		return nil, nil
	}
	return r.danmakuMd.FindByIdempotencyKey(ctx, key)
}

// CreateDanmaku 写入弹幕并维护派生计数。
// 返回 created=false 表示命中幂等键、未产生新行（重放）。
// 唯一索引 uniq_idempotency 是并发去重的最终防线：插入失败会回查一次。
func (r *Repository) CreateDanmaku(ctx context.Context, d *model.Danmaku) (dmid int64, created bool, err error) {
	if d.IdempotencyKey != "" {
		old, qerr := r.danmakuMd.FindByIdempotencyKey(ctx, d.IdempotencyKey)
		if qerr != nil {
			return 0, false, qerr
		}
		if old != nil {
			return old.Dmid, false, nil
		}
	}

	dmid, err = r.danmakuMd.Insert(ctx, d)
	if err != nil {
		old, qerr := r.danmakuMd.FindByIdempotencyKey(ctx, d.IdempotencyKey)
		if qerr == nil && old != nil {
			return old.Dmid, false, nil
		}
		return 0, false, err
	}
	d.Dmid = dmid

	// 只有直接进普通池的弹幕（机审旁路）才影响下发包与段计数。
	if visible(d.State, d.Pool) {
		r.onBecomeVisible(ctx, d.Oid, d.SegNo)
	}
	return dmid, true, nil
}

// SetModerationTaskID 回填机审任务 ID。
func (r *Repository) SetModerationTaskID(ctx context.Context, dmid, taskID int64) error {
	return r.danmakuMd.SetModerationTaskID(ctx, dmid, taskID)
}

// --- 查询 ---

// GetDanmaku 按 dmid 查询弹幕；不存在返回 (nil, nil)。
func (r *Repository) GetDanmaku(ctx context.Context, dmid int64) (*model.Danmaku, error) {
	return r.danmakuMd.FindOne(ctx, dmid)
}

// ListVisibleBySegs 按分段窗口拉取可下发弹幕：
// 先读段缓存，miss 的分段回源 MySQL 并逐段回填（含空段，防击穿）。
// 返回的弹幕按 progress_ms 升序，cacheHits 为命中缓存的分段数。
func (r *Repository) ListVisibleBySegs(ctx context.Context, oid int64, segs []int32, limit int32) ([]*model.Danmaku, int32, error) {
	if limit <= 0 || limit > int32(r.opt.MaxListLimit) {
		limit = int32(r.opt.MaxListLimit)
	}

	var (
		out       []*model.Danmaku
		cacheHits int32
		misses    []int32
		seen      = make(map[int32]bool, len(segs))
	)
	for _, s := range segs {
		if seen[s] {
			continue
		}
		seen[s] = true
		rows, hit, err := r.cache.GetSegment(ctx, oid, s)
		if err != nil {
			return nil, 0, err
		}
		if hit {
			cacheHits++
			out = append(out, rows...)
			continue
		}
		misses = append(misses, s)
	}

	if len(misses) > 0 {
		rows, err := r.danmakuMd.ListVisibleBySegs(ctx, oid, misses, limit)
		if err != nil {
			return nil, 0, err
		}
		// 按分段回填，miss 段即使无数据也写入空数组，避免同窗口反复回源。
		grouped := make(map[int32][]*model.Danmaku, len(misses))
		for _, d := range rows {
			grouped[d.SegNo] = append(grouped[d.SegNo], d)
		}
		for _, s := range misses {
			if err := r.cache.SetSegment(ctx, oid, s, grouped[s]); err != nil {
				logx.Errorf("danmaku/repository: backfill segment oid=%d seg=%d err=%v", oid, s, err)
			}
			out = append(out, grouped[s]...)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ProgressMs != out[j].ProgressMs {
			return out[i].ProgressMs < out[j].ProgressMs
		}
		return out[i].Dmid < out[j].Dmid
	})
	if int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, cacheHits, nil
}

// ListMineBySegs 拉取本人指定分段内的非可见态弹幕（发送后回显，不走缓存）。
func (r *Repository) ListMineBySegs(ctx context.Context, oid, mid int64, segs []int32) ([]*model.Danmaku, error) {
	return r.danmakuMd.ListMineBySegs(ctx, oid, mid, segs)
}

// SegmentCounts 返回各分段可见弹幕数，miss 时回源 danmaku_segment 并回填。
func (r *Repository) SegmentCounts(ctx context.Context, oid int64, segs []int32) (map[int32]int32, error) {
	cached, err := r.cache.GetSegmentCounts(ctx, oid, segs)
	if err != nil {
		return nil, err
	}
	missing := make([]int32, 0, len(segs))
	for _, s := range segs {
		if _, ok := cached[s]; !ok {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		rows, err := r.segmentMd.ListBySegs(ctx, oid, missing)
		if err != nil {
			return nil, err
		}
		got := make(map[int32]int32, len(rows))
		for _, s := range rows {
			got[s.SegNo] = s.Count
		}
		for _, s := range missing {
			cnt := got[s] // 段表无行即为 0，同样回填，避免每段都回源
			cached[s] = cnt
			if err := r.cache.SetSegmentCount(ctx, oid, s, cnt); err != nil {
				logx.Errorf("danmaku/repository: backfill segment count oid=%d seg=%d err=%v", oid, s, err)
			}
		}
	}
	return cached, nil
}

// CountVisible 统计某内容可见弹幕总数（直接回源，用于运营/排障）。
func (r *Repository) CountVisible(ctx context.Context, oid int64) (int64, error) {
	return r.danmakuMd.CountVisibleByOid(ctx, oid)
}

// --- 状态推进 + 审计 ---

// ApplyStateTransition 在同一事务内推进状态并写 op_log（保留审计证据）。
// 事务提交成功后才调整段缓存/段计数，保证缓存不会比 DB 更“可见”。
// 返回 false 表示 CAS 未命中（并发修改）或 event_id 重复投递。
func (r *Repository) ApplyStateTransition(ctx context.Context, d *model.Danmaku, toState, toPool int32, taskID int64, log *model.OpLog) (bool, error) {
	if log != nil {
		log.Dmid = d.Dmid
		log.FromState = d.State
		log.ToState = toState
	}

	updatedByEvent := false
	err := r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		if log != nil && log.EventID != "" {
			// event_id 唯一索引兜底并发重复消费：重复投递会触发唯一键冲突并回滚。
			if _, err := r.opLogMd.InsertTx(ctx, session, log); err != nil {
				return err
			}
			updatedByEvent = true
		}
		ok, err := r.danmakuMd.TransitionStateTx(ctx, session, d.Dmid, d.State, toState, toPool)
		if err != nil {
			return err
		}
		if !ok {
			return errStateCasMiss
		}
		if taskID > 0 {
			if err := r.danmakuMd.SetModerationTaskIDTx(ctx, session, d.Dmid, taskID); err != nil {
				return err
			}
		}
		if log != nil && log.EventID == "" {
			if _, err := r.opLogMd.InsertTx(ctx, session, log); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if err == errStateCasMiss {
			return false, nil
		}
		// 事件重复投递：状态已被先前消费者推进，按已处理返回。
		if updatedByEvent && isDuplicateErr(err) {
			return false, nil
		}
		return false, err
	}

	visibleBefore := visible(d.State, d.Pool)
	visibleAfter := visible(toState, toPool)
	switch {
	case !visibleBefore && visibleAfter:
		r.onBecomeVisible(ctx, d.Oid, d.SegNo)
	case visibleBefore && !visibleAfter:
		r.onBecomeInvisible(ctx, d.Oid, d.SegNo)
	default:
		_ = r.cache.DelSegment(ctx, d.Oid, d.SegNo)
	}
	return true, nil
}

// EventConsumed 判断事件是否已消费（moderation.result.v1 去重快查）。
func (r *Repository) EventConsumed(ctx context.Context, eventID string) (bool, error) {
	return r.opLogMd.ExistsEvent(ctx, eventID)
}

// ListOpLogs 查询某条弹幕的操作留痕。
func (r *Repository) ListOpLogs(ctx context.Context, dmid int64, limit int32) ([]*model.OpLog, error) {
	return r.opLogMd.ListByDmid(ctx, dmid, limit)
}

// --- 举报 ---

// ReportDanmaku 写入举报记录，返回 report_id 与是否新建。
func (r *Repository) ReportDanmaku(ctx context.Context, rep *model.Report) (int64, bool, error) {
	return r.reportMd.Insert(ctx, rep)
}

// --- 屏蔽词 ---

// UpsertBlockWord 新增或重新启用屏蔽词，并失效词库缓存。
func (r *Repository) UpsertBlockWord(ctx context.Context, w *model.BlockWord) (int64, error) {
	id, err := r.blockWordMd.Upsert(ctx, w)
	if err != nil {
		return 0, err
	}
	r.invalidateBlockWordCache(ctx, w.Oid)
	return id, nil
}

// DisableBlockWord 停用屏蔽词并失效词库缓存。
func (r *Repository) DisableBlockWord(ctx context.Context, word string, operator int64, oid int64) (bool, error) {
	ok, err := r.blockWordMd.Disable(ctx, word, operator)
	if err != nil {
		return false, err
	}
	r.invalidateBlockWordCache(ctx, oid)
	return ok, nil
}

// DeleteBlockWord 物理删除词条并失效词库缓存。
func (r *Repository) DeleteBlockWord(ctx context.Context, word string, oid int64) (bool, error) {
	ok, err := r.blockWordMd.Delete(ctx, word)
	if err != nil {
		return false, err
	}
	r.invalidateBlockWordCache(ctx, oid)
	return ok, nil
}

// GetBlockWord 查询词条；不存在返回 (nil, nil)。
func (r *Repository) GetBlockWord(ctx context.Context, word string) (*model.BlockWord, error) {
	return r.blockWordMd.FindOne(ctx, word)
}

// ListBlockWords 运营侧分页查询。
func (r *Repository) ListBlockWords(ctx context.Context, scope int32, oid int64, onlyEnabled bool, pn, ps int32) ([]*model.BlockWord, int32, error) {
	return r.blockWordMd.List(ctx, scope, oid, onlyEnabled, pn, ps)
}

// BlockWordFilter 返回指定内容的发送侧屏蔽词匹配器（全局 + 分区词）。
// 词库读缓存，miss 回源 DB；缓存不可用时降级为直接读 DB，不阻断发送。
func (r *Repository) BlockWordFilter(ctx context.Context, oid int64) (*policy.BlockFilter, error) {
	words, err := r.loadBlockWords(ctx, 0)
	if err != nil {
		return nil, err
	}
	if oid > 0 {
		part, err := r.loadBlockWords(ctx, oid)
		if err != nil {
			return nil, err
		}
		words = append(words, part...)
	}
	return policy.NewBlockFilter(words), nil
}

// loadBlockWords 读取某个作用域的生效词表（cache → DB → 回填）。
func (r *Repository) loadBlockWords(ctx context.Context, oid int64) ([]string, error) {
	cached, hit, err := r.cache.GetBlockWords(ctx, oid)
	if err == nil && hit {
		return cached, nil
	}
	rows, dbErr := r.blockWordMd.ListEnabled(ctx, oid)
	if dbErr != nil {
		return nil, dbErr
	}
	words := make([]string, 0, len(rows))
	for _, w := range rows {
		words = append(words, w.Word)
	}
	if r.opt.BlockWordCacheMaxWords > 0 && len(words) > r.opt.BlockWordCacheMaxWords {
		// 词库过大时不写缓存，避免热 key 大 value；下次直接回源。
		return words, nil
	}
	if err := r.cache.SetBlockWords(ctx, oid, words); err != nil {
		logx.Errorf("danmaku/repository: cache block words oid=%d err=%v", oid, err)
	}
	return words, nil
}

// invalidateBlockWordCache 失效词库缓存，失败只记日志（TTL 兜底）。
func (r *Repository) invalidateBlockWordCache(ctx context.Context, oid int64) {
	if err := r.cache.DelBlockWords(ctx, oid); err != nil {
		logx.Errorf("danmaku/repository: invalidate block words oid=%d err=%v", oid, err)
	}
}

// --- 用户级屏蔽 ---

// UpsertUserBlock 写入/更新用户屏蔽项并失效该用户缓存。
func (r *Repository) UpsertUserBlock(ctx context.Context, b *model.UserBlock) (int64, error) {
	id, err := r.userBlockMd.Upsert(ctx, b)
	if err != nil {
		return 0, err
	}
	if err := r.cache.DelUserBlocks(ctx, b.Mid); err != nil {
		logx.Errorf("danmaku/repository: invalidate user block mid=%d err=%v", b.Mid, err)
	}
	return id, nil
}

// ListUserBlocks 分页查询用户屏蔽项。
func (r *Repository) ListUserBlocks(ctx context.Context, mid int64, blockType int32, pn, ps int32) ([]*model.UserBlock, int32, error) {
	return r.userBlockMd.List(ctx, mid, blockType, pn, ps)
}

// UserBlockFilter 返回查看者的屏蔽视图：被屏蔽用户集合 + 关键词匹配器。
// viewerMid<=0 返回空视图（游客只有服务端下发的普通池数据）。
func (r *Repository) UserBlockFilter(ctx context.Context, viewerMid int64) (map[int64]bool, *policy.BlockFilter, error) {
	if viewerMid <= 0 {
		return nil, policy.NewBlockFilter(nil), nil
	}
	rows, cached, err := r.cache.GetUserBlocks(ctx, viewerMid)
	if err != nil || !cached {
		rows, err = r.userBlockMd.ListEnabled(ctx, viewerMid)
		if err != nil {
			return nil, nil, err
		}
		if err := r.cache.SetUserBlocks(ctx, viewerMid, rows); err != nil {
			logx.Errorf("danmaku/repository: cache user blocks mid=%d err=%v", viewerMid, err)
		}
	}
	mids := make(map[int64]bool, len(rows))
	keywords := make([]string, 0, len(rows))
	for _, b := range rows {
		switch b.Type {
		case model.UserBlockMid:
			if b.BlockedMid > 0 {
				mids[b.BlockedMid] = true
			}
		case model.UserBlockKeyword:
			if b.Keyword != "" {
				keywords = append(keywords, b.Keyword)
			}
		}
	}
	return mids, policy.NewBlockFilter(keywords), nil
}

// --- 段计数派生 ---

// visible 判断某状态/池组合是否进入下发包。
func visible(state, pool int32) bool {
	return state == model.StateNormal && pool == model.PoolNormal
}

// onBecomeVisible 段计数 +1 并失效段列表缓存。
func (r *Repository) onBecomeVisible(ctx context.Context, oid int64, segNo int32) {
	r.incrSegmentCount(ctx, oid, segNo, 1)
	_ = r.cache.DelSegment(ctx, oid, segNo)
}

// onBecomeInvisible 段计数 -1 并失效段列表缓存。
func (r *Repository) onBecomeInvisible(ctx context.Context, oid int64, segNo int32) {
	r.incrSegmentCount(ctx, oid, segNo, -1)
	_ = r.cache.DelSegment(ctx, oid, segNo)
}

// incrSegmentCount 同步维护 MySQL 派生表与 Redis 计数器；
// 任一失败都只记日志，由 cron 对账修复（见 README 缺口）。
func (r *Repository) incrSegmentCount(ctx context.Context, oid int64, segNo, delta int32) {
	if err := r.segmentMd.Incr(ctx, oid, segNo, delta); err != nil {
		logx.Errorf("danmaku/repository: incr segment db oid=%d seg=%d err=%v", oid, segNo, err)
	}
	if err := r.cache.IncrSegmentCount(ctx, oid, segNo, delta); err != nil {
		logx.Errorf("danmaku/repository: incr segment cache oid=%d seg=%d err=%v", oid, segNo, err)
	}
}
