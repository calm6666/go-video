package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// refreshEnumerateMaxPages 是一次刷新最多翻几页去枚举要删的键。
// 存在的理由：键只能从 MySQL 的行**构造**出来（禁用 KEYS/SCAN 做同步清理 ——
// 键数量不可控时会阻塞整个 Redis 实例）。千级配置表在 ps=100 下 20 页足够覆盖，
// 真超了就显式报错，而不是安静地只删前 2000 条。
const refreshEnumerateMaxPages = 20

type RefreshCacheLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRefreshCacheLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RefreshCacheLogic {
	return &RefreshCacheLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 手工失效本服务的读投影（只丢投影，不预热）
//
// 语义边界，三条都不能少：
//  1. 只碰 Cache.KeyPrefix 之下的键。绝不用 KEYS/SCAN 扫全库，也不构造任何
//     不属于本服务键空间的键（AGENTS.md §5：缓存键空间是服务边界的一部分）；
//  2. 不预热。预热会把一次手工刷新变成全表扫描，而缓存冷掉的第一批请求自会回源；
//  3. epoch 换代是**权威**失效手段，DEL 只是加速收敛：DEL 失败最多让旧值多活一个 TTL。
func (l *RefreshCacheLogic) RefreshCache(in *rpc.RefreshCacheReq) (*rpc.RefreshCacheReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkWriteContext(in.GetCtx(), lim); err != nil {
		return nil, err
	}
	target := strings.ToLower(strings.TrimSpace(in.GetTarget()))
	if target == "" {
		return nil, model.ErrRefreshTargetRequired
	}
	switch target {
	case model.RefreshTargetConfig, model.RefreshTargetTopic, model.RefreshTargetSlot, model.RefreshTargetAll:
	default:
		// 未知 target 绝不能变成「什么都不做也算成功」：那会让运维以为已经刷过了。
		return nil, fmt.Errorf("%w: target=%s", model.ErrRefreshTargetUnsupported, target)
	}

	reason := strings.TrimSpace(in.GetReason())
	if reason == "" {
		if target == model.RefreshTargetAll {
			return nil, model.ErrRefreshReasonRequired
		}
		return nil, model.ErrReasonRequired
	}
	if len([]rune(reason)) > lim.reasonMaxChars {
		return nil, model.ErrReasonTooLong
	}

	m := &l.svcCtx.Models
	ts := model.NowUnix()

	var (
		keys      []string
		configIDs []int64
		epoch     int64
		scoped    bool // 只命中单个配置项时才回一个确定的代次
	)

	switch target {
	case model.RefreshTargetConfig:
		cfgKey := strings.TrimSpace(in.GetCfgKey())
		if cfgKey != "" {
			if err := checkCfgKey(cfgKey, lim); err != nil {
				return nil, err
			}
			scope := normalizeScope(in.GetScope())
			if err := checkScope(scope); err != nil {
				return nil, err
			}
			item, err := m.ConfigItem.FindOne(l.ctx, cfgKey, scope)
			if err != nil {
				return nil, err
			}
			if item == nil {
				// 刷一个不存在的键 = 调用方的清单已经过期，必须显式失败。
				return nil, fmt.Errorf("%w: cfg_key=%s scope=%s", model.ErrConfigNotFound, cfgKey, scope)
			}
			keys = lim.configKeysOf(item)
			configIDs = []int64{item.ConfigID}
			epoch = item.Epoch + 1
			scoped = true
		} else {
			// 不给 cfg_key：刷该 scope 下全部生效键。scope 留空归一 global，
			// 「刷新所有 scope」是 target=all 的职责，不靠这里的空值表达。
			items, err := l.enumerateConfigItems(m, normalizeScope(in.GetScope()), 0)
			if err != nil {
				return nil, err
			}
			for _, it := range items {
				keys = append(keys, lim.configKeysOf(it)...)
				configIDs = append(configIDs, it.ConfigID)
			}
		}

	case model.RefreshTargetTopic:
		if in.GetTopicId() <= 0 {
			return nil, model.ErrTopicNotFound
		}
		topic, err := m.Topic.FindByID(l.ctx, in.GetTopicId())
		if err != nil {
			return nil, err
		}
		if topic == nil {
			return nil, fmt.Errorf("%w: topic_id=%d", model.ErrTopicNotFound, in.GetTopicId())
		}
		// 两份定位符都要删：端上按 slug 寻址、后台按 ID 操作。
		keys = lim.topicKeyLocators(topic)

	case model.RefreshTargetSlot:
		if in.GetSlotId() <= 0 {
			return nil, model.ErrSlotNotFound
		}
		slot, err := m.Slot.FindByID(l.ctx, in.GetSlotId())
		if err != nil {
			return nil, err
		}
		if slot == nil {
			return nil, fmt.Errorf("%w: slot_id=%d", model.ErrSlotNotFound, in.GetSlotId())
		}
		keys = lim.slotKeys(slot)

	case model.RefreshTargetAll:
		items, err := l.enumerateConfigItems(m, "", model.StateOn)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			keys = append(keys, lim.configKeysOf(it)...)
		}
		// 全域没有单一「代次」可回：每行 epoch 各自 +1，回 0 表示「整体已失效」。
		epoch = 0
	}

	// 1) 先换代（权威失效），再删键（加速收敛）。
	var bumped int64
	switch {
	case target == model.RefreshTargetAll:
		// 只代是「全部」，含停用项：停用项重新上线时不能带着旧代次被缓存命中。
		n, err := m.ConfigItem.BumpAllEpoch(l.ctx, 0, ts)
		if err != nil {
			return nil, err
		}
		bumped = n
	case len(configIDs) > 0:
		n, err := m.ConfigItem.BumpEpoch(l.ctx, configIDs, ts)
		if err != nil {
			return nil, err
		}
		bumped = n
		if !scoped {
			epoch = 0
		}
	default:
		// topic / slot 的投影不在 epoch 体系里（epoch 是配置项的字段），
		// 单靠 DEL 失效；没删掉的行最多活一个视图 TTL。
		bumped = 0
	}

	affected := int64(0)
	if lim.deleteOnBump {
		affected = cacheDel(l.ctx, l.svcCtx.Cache, dedupeStrings(keys), l.Logger)
	} else {
		// 过渡期开关：只换代不删键，靠调用方比对 epoch 收敛。README 里写明。
		l.Infof("ops-config/refresh: Cache.DeleteOnBump=false，本次只换代（bumped=%d）不删键", bumped)
	}

	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "refresh_cache",
		"ops_config:cache", target,
		"",
		fmt.Sprintf("target=%s,cfg_key=%s,scope=%s,topic_id=%d,slot_id=%d,keys=%d,affected=%d,epoch_bumped=%d",
			target, in.GetCfgKey(), in.GetScope(), in.GetTopicId(), in.GetSlotId(),
			len(dedupeStrings(keys)), affected, bumped),
		reason, ts)

	return &rpc.RefreshCacheReply{
		Affected: int32(affected),
		Epoch:    epoch,
		// 0 = 审计缺口可见（缓存失效不是业务状态变更，绝不因为它失败而回滚已完成的失效）。
		AuditEntryId: entryID,
	}, nil
}

// enumerateConfigItems 按 scope/state 翻出要删键的配置项行。
// 超过 refreshEnumerateMaxPages 显式报错：宁可让运维去处理「键太多」这件事，
// 也不回一个「只清了一部分」的成功。
func (l *RefreshCacheLogic) enumerateConfigItems(m *svc.Models, scope string, state int32) ([]*model.ConfigItem, error) {
	lim := newLimits(l.svcCtx.Config)
	var (
		out  []*model.ConfigItem
		page int32 = 1
	)
	for {
		rows, total, err := m.ConfigItem.List(l.ctx, model.ConfigItemFilter{
			Scope: scope, State: state, Pn: page, Ps: int32(lim.maxPageSize),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) == 0 || int64(len(out)) >= total {
			return out, nil
		}
		if page >= refreshEnumerateMaxPages {
			return nil, fmt.Errorf("%w: 待失效配置项超过 %d 条（已枚举 %d/%d），请改用 target=all 或缩小 scope",
				model.ErrBatchTooLarge, refreshEnumerateMaxPages*lim.maxPageSize, len(out), total)
		}
		page++
	}
}
