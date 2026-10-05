// Package repository 是 risk-control 的数据访问层。
//
// 组合 6 个 model（规则/处罚/名单/设备画像/设备关联/裁决日志）与 Redis 计数器，
// 并向 policy.Engine 提供 policy.Store 实现：
// 决策所需的一切外部依赖在这里被读成一份「事实快照」，
// policy 包本身不碰 IO，因此整条裁决链路可以离线单测。
//
// 依赖故障语义（AGENTS.md §9 要求显式定义，不伪造成功）：
//   - DB（规则/名单/处罚）不可用 → LoadFacts 返回 error，引擎按动作危险度降级；
//   - Redis 计数器不可用 → 相关规则计入 skipped_rule_ids，Result.Degraded=true，
//     并由进程内 localRateGuard 决定是否兜底拒绝；
//   - 裁决日志写入失败 → 只记错误日志，不翻转已产出的裁决。
package repository

import (
	"context"
	"fmt"
	"strconv"

	"go-video/common/validation"
	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Config 是仓库层参数，由 svc 从 config.RiskControlConf 映射。
type Config struct {
	// CounterTiers 是滑窗计数档位（秒）。
	CounterTiers []int64
	// WindowBuckets 是每个档位的桶数。
	WindowBuckets int
	// DefaultWindowSeconds 是上报与回包的默认窗口。
	DefaultWindowSeconds int64
	// DecisionCacheSeconds 是同一 request_id 裁决回放时间。
	DecisionCacheSeconds int64
	// RuleCacheSeconds 是启用规则集合缓存时间。
	RuleCacheSeconds int64
	// LocalFallbackLimit 是 Redis 故障时单实例每动作窗口兜底阈值，<=0 关闭。
	LocalFallbackLimit int64
}

// Repository 是 risk-control 的数据访问入口，同时实现 policy.Store。
type Repository struct {
	cfg     Config
	conn    sqlx.SqlConn
	cache   *Cache
	counter *Counter
	guard   *localRateGuard

	ruleMd   model.RiskRuleModel
	punishMd model.RiskPunishmentModel
	deviceMd model.RiskDeviceProfileModel
	midMd    model.RiskDeviceMidModel
	listMd   model.RiskListModel
	logMd    model.RiskCheckLogModel
}

// 编译期断言：Repository 必须满足引擎的事实来源契约。
var _ policy.Store = (*Repository)(nil)

// New 构造 Repository。生产路径的唯一入口，Redis/MySQL 全部是真实实现。
func New(rds *redis.Redis, conn sqlx.SqlConn, cfg Config) *Repository {
	repo := NewWithDeps(newRedisCacheBackend(rds), newRedisCounterBackend(rds), cfg,
		model.NewRiskRuleModel(conn),
		model.NewRiskPunishmentModel(conn),
		model.NewRiskDeviceProfileModel(conn),
		model.NewRiskDeviceMidModel(conn),
		model.NewRiskListModel(conn),
		model.NewRiskCheckLogModel(conn))
	repo.conn = conn // 与抽取前一致：conn 只作为连接句柄留在结构体上，不参与任何查询
	return repo
}

// NewWithDeps 是注入缝：显式给出 Redis 原语替身与 6 个 model，供 internal/logic 的单测
// 用内存依赖组装**真实 Repository**（含真实 Cache/Counter/localRateGuard 与真实桶数学），
// 而不是把 Repository 或 policy.Engine 一起 mock 掉。
//
// 与 New 的差别**只有依赖来源不同**：配置归一化、guard 构造、model 装配逐行一致，
// 不新增/删除任何判断，因此不改变生产语义。生产代码不得调用本函数，一律走 New。
func NewWithDeps(
	kv cacheBackend,
	cnt counterBackend,
	cfg Config,
	ruleMd model.RiskRuleModel,
	punishMd model.RiskPunishmentModel,
	deviceMd model.RiskDeviceProfileModel,
	midMd model.RiskDeviceMidModel,
	listMd model.RiskListModel,
	logMd model.RiskCheckLogModel,
) *Repository {
	normalized := normalizeTiers(cfg.CounterTiers)
	if cfg.DefaultWindowSeconds <= 0 {
		cfg.DefaultWindowSeconds = 60
	}
	if cfg.WindowBuckets <= 0 {
		cfg.WindowBuckets = 6
	}
	if cfg.DecisionCacheSeconds <= 0 {
		cfg.DecisionCacheSeconds = 60
	}
	if cfg.RuleCacheSeconds <= 0 {
		cfg.RuleCacheSeconds = 30
	}
	cfg.CounterTiers = normalized
	return &Repository{
		cfg:      cfg,
		cache:    &Cache{rds: kv},
		counter:  newCounter(cnt, normalized, cfg.WindowBuckets, cfg.DefaultWindowSeconds),
		guard:    newLocalRateGuard(cfg.DefaultWindowSeconds, cfg.WindowBuckets, cfg.LocalFallbackLimit),
		ruleMd:   ruleMd,
		punishMd: punishMd,
		deviceMd: deviceMd,
		midMd:    midMd,
		listMd:   listMd,
		logMd:    logMd,
	}
}

// Ping 检查 Redis 连通性（DB 连通性由 go-zero 连接池惰性建立）。
func (r *Repository) Ping(ctx context.Context) error { return r.counter.Ping(ctx) }

// LocalFallbackEnabled 报告进程内兜底限流是否启用（启动日志用）。
func (r *Repository) LocalFallbackEnabled() bool { return r.guard.Enabled() }

// --- 行为上报 ---

// ReportInput 是一次行为上报（已脱敏）。
type ReportInput struct {
	Mid        int64
	Action     int32
	DeviceHash string
	IPHash     string
	Count      int64
	OccurredAt int64
	EventID    string
}

// ReportOutput 是上报结果。
type ReportOutput struct {
	Deduplicated  bool
	WindowSeconds int64
	MidCount      int64
	DeviceCount   int64
	IPCount       int64
}

// Report 写入滑窗计数并回读当前窗口值。
// 幂等：event_id 非空时同 event_id 只累加一次；重复上报只读不写。
// 上报不做任何 SPM/推荐用途（AGENTS.md §7），只服务本服务的频率规则。
func (r *Repository) Report(ctx context.Context, in ReportInput) (*ReportOutput, error) {
	if !model.ValidAction(in.Action) {
		return nil, model.ErrInvalidTarget
	}
	now := in.OccurredAt
	if now <= 0 {
		now = nowUnix()
	}

	out := &ReportOutput{WindowSeconds: r.cfg.DefaultWindowSeconds}
	subjects := r.subjects(in.Mid, in.DeviceHash, in.IPHash)

	seen, err := r.counter.MarkEventOnce(ctx, in.EventID, dedupTTLSeconds(r.cfg.DefaultWindowSeconds))
	if err != nil {
		return nil, err
	}
	if !seen {
		out.Deduplicated = true
	} else {
		r.guard.observe(in.Action)
		if err := r.counter.Incr(ctx, IncrRequest{
			Action:     in.Action,
			Subjects:   subjects,
			Count:      in.Count,
			OccurredAt: now,
		}); err != nil {
			return nil, err
		}
	}

	// 回读窗口值：只读，读失败按 0 返回并在日志里保留原因，不影响上报本身成功。
	for _, s := range subjects {
		key := s.SubjectKey()
		if key == "" {
			continue
		}
		total, _, sErr := r.counter.Sum(ctx, metricForSubject(s.Kind), key, in.Action, r.cfg.DefaultWindowSeconds, now)
		if sErr != nil {
			continue
		}
		switch s.Kind {
		case "mid":
			out.MidCount = total
		case "device":
			out.DeviceCount = total
		case "ip":
			out.IPCount = total
		}
	}
	return out, nil
}

func (r *Repository) subjects(mid int64, deviceHash, ipHash string) []Subject {
	out := make([]Subject, 0, 3)
	if mid > 0 {
		out = append(out, Subject{Kind: "mid", Value: strconv.FormatInt(mid, 10)})
	}
	if deviceHash != "" {
		out = append(out, Subject{Kind: "device", Value: deviceHash})
	}
	if ipHash != "" {
		out = append(out, Subject{Kind: "ip", Value: ipHash})
	}
	return out
}

// --- 处罚 ---

// ApplyPunishment 下发处罚。
// 幂等两层：idempotency_key 唯一索引（重试返回既有处罚）；
// 同一 (mid, scope) 已有生效处罚时拒绝新增，避免裁决解释出现两条冲突处罚。
func (r *Repository) ApplyPunishment(ctx context.Context, p *model.RiskPunishment) (*model.RiskPunishment, bool, error) {
	if p.Operator <= 0 {
		return nil, false, model.ErrOperatorRequired
	}
	if p.IdempotencyKey == "" {
		return nil, false, model.ErrIdempotencyKeyRequired
	}
	if p.Mid <= 0 {
		return nil, false, model.ErrInvalidTarget
	}
	if !model.ValidRuleAction(p.Scope) {
		return nil, false, model.ErrInvalidTarget
	}
	switch p.Decision {
	case model.DecisionBlock, model.DecisionChallenge, model.DecisionReview:
	default:
		return nil, false, fmt.Errorf("%w: punishment decision must be CHALLENGE/BLOCK/REVIEW", model.ErrInvalidTarget)
	}
	if p.State == 0 {
		// INSERT 显式绑定 state 列，DDL 的 `DEFAULT 1` 不会生效；不在这里归一，
		// 新处罚就落在 state=0，而 ListActiveByMid/ExpireStale 都按 state=1 过滤 ⇒ 永不可见。
		p.State = model.PunishmentStateActive
	}
	if p.EndAt != 0 && p.EndAt <= p.StartAt {
		return nil, false, fmt.Errorf("%w: end_at must be later than start_at (or 0 for permanent)", model.ErrInvalidTarget)
	}

	if existed, err := r.punishMd.FindByIDempotencyKey(ctx, p.IdempotencyKey); err != nil {
		return nil, false, err
	} else if existed != nil {
		return existed, false, nil
	}

	// 惰性推进该账号已到期的处罚，保证「过期」状态不依赖 cron 是否已跑。
	if _, err := r.punishMd.ExpireStale(ctx, p.Mid, nowUnix()); err != nil {
		return nil, false, err
	}
	active, err := r.punishMd.ListActiveByMid(ctx, p.Mid, nowUnix())
	if err != nil {
		return nil, false, err
	}
	for _, a := range active {
		if a.MatchesScope(p.Mid, p.Scope) {
			return a, false, fmt.Errorf("%w: punishment_id=%d", model.ErrPunishmentAlreadyActive, a.PunishmentID)
		}
	}

	id, created, err := r.punishMd.Insert(ctx, p)
	if err != nil {
		return nil, false, err
	}
	fresh, err := r.punishMd.FindOne(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if fresh == nil {
		return nil, false, model.ErrPunishmentNotFound
	}
	return fresh, created, nil
}

// LiftPunishment 解除处罚。
// punishmentID 为 0 时按 (mid, scope) 精确定位当前生效处罚。
// 幂等：已处于终态时返回当前记录且 changed=false。
func (r *Repository) LiftPunishment(ctx context.Context, punishmentID, mid int64, scope int32, operator int64, reason string) (*model.RiskPunishment, bool, error) {
	if operator <= 0 {
		return nil, false, model.ErrOperatorRequired
	}
	target := punishmentID
	if target == 0 {
		if mid <= 0 {
			return nil, false, model.ErrInvalidTarget
		}
		if _, err := r.punishMd.ExpireStale(ctx, mid, nowUnix()); err != nil {
			return nil, false, err
		}
		active, err := r.punishMd.ListActiveByMid(ctx, mid, nowUnix())
		if err != nil {
			return nil, false, err
		}
		var found []*model.RiskPunishment
		for _, a := range active {
			if a.MatchesScope(mid, scope) {
				found = append(found, a)
			}
		}
		switch len(found) {
		case 0:
			return nil, false, model.ErrPunishmentNotFound
		case 1:
			target = found[0].PunishmentID
		default:
			return nil, false, model.ErrAmbiguousPunishment
		}
	}

	cur, err := r.punishMd.FindOne(ctx, target)
	if err != nil {
		return nil, false, err
	}
	if cur == nil {
		return nil, false, model.ErrPunishmentNotFound
	}
	if !cur.Effective(nowUnix()) && cur.State != model.PunishmentStateActive {
		// 已解除/已过期：幂等返回当前状态。
		return cur, false, nil
	}
	if err := r.punishMd.Lift(ctx, target, operator, reason); err != nil {
		if err == model.ErrPunishmentAlreadyFinished {
			fresh, ferr := r.punishMd.FindOne(ctx, target)
			if ferr != nil {
				return nil, false, ferr
			}
			if fresh == nil {
				return nil, false, model.ErrPunishmentNotFound
			}
			return fresh, false, nil
		}
		return nil, false, err
	}
	fresh, err := r.punishMd.FindOne(ctx, target)
	if err != nil {
		return nil, false, err
	}
	if fresh == nil {
		return nil, false, model.ErrPunishmentNotFound
	}
	return fresh, true, nil
}

// ListPunishments 分页查询处罚。
func (r *Repository) ListPunishments(ctx context.Context, mid int64, scope, state int32, onlyActive bool, page, size int) ([]*model.RiskPunishment, int32, error) {
	p := validation.NormalizePage(page, size, 50)
	return r.punishMd.List(ctx, mid, scope, state, onlyActive, nowUnix(), p.Offset(), p.PageSize)
}

// --- 规则 ---

// UpsertRule 新增或更新规则。
// 版本语义：任一影响评估结果的字段（动作/指标/比较符/阈值/窗口/裁决/优先级）变化即 version+1；
// 仅启停或改操作人不产生新版本，因为历史裁决的解释依赖的是评估条件。
func (r *Repository) UpsertRule(ctx context.Context, in *model.RiskRule) (*model.RiskRule, bool, error) {
	if in.Operator <= 0 {
		return nil, false, model.ErrOperatorRequired
	}
	if err := in.Validate(); err != nil {
		return nil, false, err
	}

	if in.RuleID == 0 {
		if existed, err := r.ruleMd.FindByName(ctx, in.Name); err != nil {
			return nil, false, err
		} else if existed != nil {
			return nil, false, fmt.Errorf("%w: %s", model.ErrRuleNameDuplicated, in.Name)
		}
		in.Version = 1
		if _, err := r.ruleMd.Insert(ctx, in); err != nil {
			return nil, false, err
		}
		_ = r.invalidateRuleCache(ctx, in.ActionType)
		return in, true, nil
	}

	old, err := r.ruleMd.FindOne(ctx, in.RuleID)
	if err != nil {
		return nil, false, err
	}
	if old == nil {
		return nil, false, model.ErrRuleNotFound
	}
	in.Name = old.Name // 规则名不可变更，避免审计断链
	if ruleSemanticsChanged(old, in) {
		in.Version = old.Version + 1
	} else {
		in.Version = old.Version
	}
	if err := r.ruleMd.Update(ctx, in); err != nil {
		return nil, false, err
	}
	_ = r.invalidateRuleCache(ctx, old.ActionType)
	_ = r.invalidateRuleCache(ctx, in.ActionType)
	fresh, err := r.ruleMd.FindOne(ctx, in.RuleID)
	if err != nil {
		return nil, false, err
	}
	if fresh == nil {
		return nil, false, model.ErrRuleNotFound
	}
	return fresh, false, nil
}

// ruleSemanticsChanged 判定影响评估结果的字段是否变化。
func ruleSemanticsChanged(old, next *model.RiskRule) bool {
	return old.ActionType != next.ActionType ||
		old.Metric != next.Metric ||
		old.Op != next.Op ||
		old.Threshold != next.Threshold ||
		old.WindowSeconds != next.WindowSeconds ||
		old.Decision != next.Decision ||
		old.Priority != next.Priority
}

// ListRules 分页查询规则。
func (r *Repository) ListRules(ctx context.Context, action int32, metric string, state int32, page, size int) ([]*model.RiskRule, int32, error) {
	p := validation.NormalizePage(page, size, 50)
	return r.ruleMd.List(ctx, action, metric, state, p.Offset(), p.PageSize)
}

// --- 名单 ---

// UpsertListEntry 写入或覆盖名单条目（唯一键 list_type+target_type+target_value）。
func (r *Repository) UpsertListEntry(ctx context.Context, in *model.RiskList) (*model.RiskList, bool, error) {
	// 与 ApplyPunishment / UpsertRule 同口径：审计字段在仓库层就把关，
	// 不能等到 model.Upsert 的 Validate 才发现——那时读写字段已经组装完毕，
	// 「谁把这条账号拉黑的」这类问题只会在最内层报一个笼统错误。
	// 注意：logic 层的守卫（state/target_value/duration）在这之前，不会触库。
	if in.Operator <= 0 {
		return nil, false, model.ErrOperatorRequired
	}
	entry, created, err := r.listMd.Upsert(ctx, in)
	if err != nil {
		return nil, false, err
	}
	return entry, created, nil
}

// GetListEntries 分页查询名单条目。
func (r *Repository) GetListEntries(ctx context.Context, listType, targetType int32, targetValue string, state int32, page, size int) ([]*model.RiskList, int32, error) {
	p := validation.NormalizePage(page, size, 100)
	return r.listMd.List(ctx, listType, targetType, targetValue, state, p.Offset(), p.PageSize)
}

// --- 设备画像 ---

// UpsertDeviceProfileInput 是画像写入入参。
type UpsertDeviceProfileInput struct {
	DeviceHash string
	Labels     []string
	RiskScore  int32 // <0 表示不修改
	Mid        int64 // >0 时登记设备-账号关联
	Source     string
	Operator   int64
}

// UpsertDeviceProfileOutput 是画像写入结果。
type UpsertDeviceProfileOutput struct {
	Profile       *model.RiskDeviceProfile
	Created       bool
	RelationAdded bool
}

// maxLabelsColumnLen 与 risk_device_profile.labels 的列宽（VARCHAR(512)）对齐，
// 用途见 UpsertDeviceProfile 内的注释。
const maxLabelsColumnLen = 512

// UpsertDeviceProfile 合并标签、刷新出现时间并登记账号关联。
// related_mid_count 由 risk_device_mid 重算，是事实投影而非唯一事实源。
func (r *Repository) UpsertDeviceProfile(ctx context.Context, in UpsertDeviceProfileInput) (*UpsertDeviceProfileOutput, error) {
	if in.DeviceHash == "" {
		return nil, model.ErrEmptyDeviceID
	}
	existing, err := r.deviceMd.FindOne(ctx, in.DeviceHash)
	if err != nil {
		return nil, err
	}
	profile := &model.RiskDeviceProfile{
		DeviceHash: in.DeviceHash,
		Source:     in.Source,
		Operator:   in.Operator,
		RiskScore:  in.RiskScore,
	}
	if existing != nil {
		profile.Labels = model.MergeLabels(existing.Labels, in.Labels)
		profile.FirstSeen = existing.FirstSeen
		if in.RiskScore < 0 {
			profile.RiskScore = existing.RiskScore
		}
		profile.RelatedMidCount = existing.RelatedMidCount
	} else {
		profile.Labels = model.MergeLabels("", in.Labels)
	}
	// 合并后的标签串必须容得下 risk_device_profile.labels 的列宽 VARCHAR(512)
	// （deploy/migrations/risk-control/000002_create_risk_device_and_check_log_tables.sql:47）。
	// logic 层限了「单请求 ≤20 条、单条 ≤32 字节」，但这里合并的是「库里已有 + 本次新增」，
	// 两者相乘可远超 512：超宽时 MySQL 严格模式报 1406、非严格模式**静默截断**，
	// 而截断会切在某个标签中间，下次 splitLabels 读回的是半个标签名 ——
	// 风险标签被改写成一个不存在的标签，依赖它的判定会静默改变，所以只能拒绝不能裁断。
	if len(profile.Labels) > maxLabelsColumnLen {
		return nil, fmt.Errorf("%w: labels exceed %d bytes after merge", model.ErrInvalidTarget, maxLabelsColumnLen)
	}
	if profile.RiskScore > 100 {
		profile.RiskScore = 100
	}
	if profile.RiskScore < 0 {
		profile.RiskScore = 0
	}

	saved, created, err := r.deviceMd.Upsert(ctx, profile)
	if err != nil {
		return nil, err
	}
	out := &UpsertDeviceProfileOutput{Profile: saved, Created: created}

	if in.Mid > 0 {
		added, err := r.midMd.AddRelation(ctx, in.DeviceHash, in.Mid)
		if err != nil {
			return out, err
		}
		out.RelationAdded = added
		count, err := r.midMd.CountByDevice(ctx, in.DeviceHash)
		if err != nil {
			return out, err
		}
		if err := r.deviceMd.UpdateRelatedCount(ctx, in.DeviceHash, count); err != nil {
			return out, err
		}
		saved.RelatedMidCount = count
	}
	return out, nil
}

// GetDeviceProfile 查询设备画像；不存在返回 (nil, nil)。
func (r *Repository) GetDeviceProfile(ctx context.Context, deviceHash string) (*model.RiskDeviceProfile, error) {
	if deviceHash == "" {
		return nil, model.ErrEmptyDeviceID
	}
	return r.deviceMd.FindOne(ctx, deviceHash)
}
