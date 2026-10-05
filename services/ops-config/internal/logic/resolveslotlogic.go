package logic

import (
	"context"
	"encoding/json"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ResolveSlotLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewResolveSlotLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ResolveSlotLogic {
	return &ResolveSlotLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运行时解析坑位：某端在某时刻这个位置上有哪些内容引用
//
// 只回引用（item_type + item_id），不回标题/封面/播放量：那些属于内容服务与网关聚合，
// 复制过来就是第二份真相（AGENTS.md §5）。本服务也不做个性化排序，
// weight 只是运营手工次序，真正的排序归 recommend-*（README 缺口）。
func (l *ResolveSlotLogic) ResolveSlot(in *rpc.ResolveSlotReq) (*rpc.ResolveSlotReply, error) {
	lim := newLimits(l.svcCtx.Config)
	code := strings.TrimSpace(in.GetCode())
	if code == "" {
		return nil, model.ErrSlotCodeRequired
	}
	if !model.ValidSlotCode(code) {
		return nil, model.ErrSlotCodeInvalid
	}
	// target 允许缺省，缺省语义是「该维度不过滤」——与 ResolveConfig 的 TargetContext 一致。
	// 端标识越界在这里就被拒：绝不把未知端当成「不做端过滤」，
	// 那会让一条只针对某端的排期被当成全端可见。
	target, err := rolloutInputOf(in.GetTarget())
	if err != nil {
		return nil, err
	}
	// 排期是否生效由服务端定时：接受客户端时钟会出现同一坑位两端结论不同。
	at := in.GetAt()
	if at <= 0 {
		at = model.NowUnix()
	}
	m := &l.svcCtx.Models

	slot, err := l.loadSlot(code, lim)
	if err != nil {
		return nil, err
	}
	if slot == nil {
		// 「这个位置还没配」是正常结果，不占 gRPC 错误码；回短 TTL 让新配的坑位较快可见。
		return &rpc.ResolveSlotReply{Found: false, Ttl: lim.clampTTL(itemPointerTTLSeconds)}, nil
	}
	// 停用 / 该端不可见一律 found=false：对这个端而言这个位置不存在。
	// 回 ttl=0 —— 一个刚被停掉的坑位被继续缓存，等于止血动作延后生效。
	if slot.State != model.StateOn || !slot.VisibleTo(target.Platform) {
		return &rpc.ResolveSlotReply{Found: false, Ttl: 0}, nil
	}

	limit := int(slot.Capacity)
	if in.GetLimit() > 0 {
		limit = int(in.GetLimit())
		if limit > int(slot.Capacity) {
			// 容量是硬边界：超过它就等于要一个装不下的结果集。
			limit = int(slot.Capacity)
		}
	}

	// 生效窗口下推到 SQL（ListEffective: state=ON 且窗口覆盖 at，
	// ORDER BY position ASC, weight DESC, id ASC —— 第三个 tiebreaker 保证同位置同权重时稳定），
	// 这里再判一次 InWindow：两侧同语义（model 是唯一实现），截断不会引入边界偏差。
	rows, err := m.SlotItem.ListEffective(l.ctx, slot.SlotID, at, limit)
	if err != nil {
		return nil, err
	}
	effective := make([]*model.SlotItem, 0, len(rows))
	for _, r := range rows {
		if r == nil || !r.InWindow(at) {
			continue
		}
		effective = append(effective, r)
	}
	// 契约缺口（README + 报告）：ops_recommend_slot_item 没有 app_version 区间列，
	// 因此 target.app_version 目前无法参与条目过滤。这里刻意不假装过滤过 ——
	// 静默把「本不该出现在该版本的内容」回给调用方，比多回几条更难排查。
	if target.AppVersion != "" && len(effective) > 0 {
		l.Debugf("ops-config/slot: code=%s 带 app_version=%s，但坑位条目无版本区间，未按版本过滤", code, target.AppVersion)
	}

	return &rpc.ResolveSlotReply{
		Slot:  slotInfo(slot),
		Items: slotItemList(effective),
		Found: true,
		Ttl:   lim.clampTTL(lim.slotTTL),
	}, nil
}

// loadSlot 只缓存坑位**定义行**（capacity / platforms / state），条目与排期永远回源。
//
// 为什么不像桩注释那样把结果集按 code+platform+app_version 缓存：
// 那些派生键在 SaveSlotItems 之后删不干净（本服务不用 SCAN/KEYS），
// 换内容会延迟一个 TTL 才可见；而一次带 idx_slot_position 的查询本来就足够便宜。
// 缓存 miss 同样不写：新建坑位要立刻可见。
func (l *ResolveSlotLogic) loadSlot(code string, lim limits) (*model.RecommendSlot, error) {
	key := lim.slotKey("code:" + code)
	if l.svcCtx.Cache != nil && lim.slotTTL > 0 {
		raw, cerr := l.svcCtx.Cache.Get(l.ctx, key)
		if cerr != nil {
			l.Errorf("ops-config/cache: 读 %s 失败，按未命中回源: %v", key, cerr)
		} else if raw != "" {
			var row model.RecommendSlot
			if uerr := json.Unmarshal([]byte(raw), &row); uerr == nil {
				return &row, nil
			} else {
				l.Errorf("ops-config/cache: 解析 %s 失败，按未命中回源: %v", key, uerr)
			}
		}
	}
	slot, err := l.svcCtx.Models.Slot.FindByCode(l.ctx, code)
	if err != nil || slot == nil || l.svcCtx.Cache == nil || lim.slotTTL <= 0 {
		return slot, err
	}
	if bs, merr := json.Marshal(slot); merr == nil {
		if serr := l.svcCtx.Cache.Setex(l.ctx, key, string(bs), lim.slotTTL); serr != nil {
			l.Errorf("ops-config/cache: 回填 %s 失败（本次响应仍正确）: %v", key, serr)
		}
	}
	return slot, nil
}
