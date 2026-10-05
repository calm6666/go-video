package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveClientSwitchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveClientSwitchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveClientSwitchLogic {
	return &SaveClientSwitchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新建/更新客户端开关（按 switch_key + platform 唯一定位）
//
// 开关**必须**绑定到具体端：「不限端」的语义属于配置项 cfg_key —— 这是本表与
// ops_config_item 的分工。允许空端就会出现「同一个 key 既有按端行又有全局行」，
// 读取时无从判定优先级。值域只有 1..4（不含小程序，AGENTS.md §6）。
func (l *SaveClientSwitchLogic) SaveClientSwitch(in *rpc.SaveClientSwitchReq) (*rpc.SaveClientSwitchReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkWriteContext(in.GetCtx(), lim); err != nil {
		return nil, err
	}
	if err := checkReason(in.GetReason(), lim); err != nil {
		return nil, err
	}
	switchKey := strings.TrimSpace(in.GetSwitchKey())
	if switchKey == "" {
		return nil, model.ErrSwitchKeyRequired
	}
	if !model.ValidSwitchKey(switchKey) {
		// 格式不合沿用 model 的语义：不新造错误码，让调用方按同一条规则自己校验。
		return nil, fmt.Errorf("%w: switch_key=%s 需匹配 ^[a-z][a-z0-9_.]{1,63}$", model.ErrSwitchKeyRequired, switchKey)
	}
	if err := checkPlatformEnum(in.GetPlatform()); err != nil {
		return nil, err
	}
	enabled := in.GetEnabled()
	if enabled == 0 {
		// 默认开等于给一个还没验证过的端上了新能力。
		enabled = model.StateOff
	}
	if err := checkState(enabled); err != nil {
		return nil, err
	}
	if in.GetExpectVersion() < 0 {
		return nil, model.ErrVersionConflict
	}
	m := &l.svcCtx.Models

	// 开关可以引用一个配置项：指向不存在的键，端上会拿到一个永远解析不出的引用。
	if in.GetConfigId() > 0 {
		bound, cerr := m.ConfigItem.FindByID(l.ctx, in.GetConfigId())
		if cerr != nil {
			if errors.Is(cerr, model.ErrConfigNotFound) {
				return nil, model.ErrConfigNotFound
			}
			return nil, cerr
		}
		if bound == nil {
			return nil, model.ErrConfigNotFound
		}
	}

	ts := model.NowUnix()
	platform := int32(in.GetPlatform())

	// --- 定位本次要写的逻辑行 ---
	// 未显式给 switch_id 时按 (switch_key, platform) 定位：
	// 否则后台连点两次会生成两条同键行，读取侧的「哪条算数」就变成了随机。
	var cur *model.ClientSwitch
	var err error
	if in.GetSwitchId() > 0 {
		cur, err = m.ClientSwitch.FindByID(l.ctx, in.GetSwitchId())
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, fmt.Errorf("%w: switch_id=%d", model.ErrSwitchNotFound, in.GetSwitchId())
		}
	} else {
		cur, err = m.ClientSwitch.FindByKeyPlatform(l.ctx, switchKey, platform)
		if err != nil {
			return nil, err
		}
	}

	if cur == nil {
		if in.GetExpectVersion() != 0 {
			// 期望改一个不存在的行：说明调用方手里的清单已经过期。
			return nil, fmt.Errorf("%w: switch_key=%s platform=%d 尚不存在", model.ErrSwitchNotFound, switchKey, platform)
		}
		sw := &model.ClientSwitch{
			SwitchKey:  switchKey,
			Platform:   platform,
			MinVersion: strings.TrimSpace(in.GetMinVersion()),
			MaxVersion: strings.TrimSpace(in.GetMaxVersion()),
			Enabled:    enabled,
			ConfigID:   in.GetConfigId(),
			OperatorID: in.GetCtx().GetOperatorId(),
			Remark:     in.GetRemark(),
			Ctime:      ts,
		}
		id, ierr := m.ClientSwitch.Insert(l.ctx, sw)
		if ierr != nil {
			if errors.Is(ierr, model.ErrSwitchConflict) {
				return nil, fmt.Errorf("%w: switch_key=%s platform=%d", model.ErrSwitchConflict, switchKey, platform)
			}
			// 版本区间倒挂等形态错误由 model 的 normalize 抛出（点分版本逐段比，绝不字典序）。
			return nil, ierr
		}
		sw.SwitchID = id
		l.invalidateBoundConfig(id, sw.ConfigID, lim)
		entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "create_client_switch",
			"ops_config:client_switch", strconv.FormatInt(id, 10), "", switchDigest(sw), in.GetReason(), ts)
		return &rpc.SaveClientSwitchReply{Switch: switchInfo(sw), AuditEntryId: entryID}, nil
	}

	if cur.Version != in.GetExpectVersion() {
		return nil, fmt.Errorf("%w: switch_id=%d expect=%d current=%d",
			model.ErrVersionConflict, cur.SwitchID, in.GetExpectVersion(), cur.Version)
	}
	next := &model.ClientSwitch{
		SwitchID:   cur.SwitchID,
		SwitchKey:  switchKey,
		Platform:   platform,
		MinVersion: strings.TrimSpace(in.GetMinVersion()),
		MaxVersion: strings.TrimSpace(in.GetMaxVersion()),
		Enabled:    enabled,
		ConfigID:   in.GetConfigId(),
		// 版本区间按**新端**校验：换端时必须用新端的版本语义重判，不能沿用旧行的区间。
		OperatorID: in.GetCtx().GetOperatorId(),
		Remark:     in.GetRemark(),
		Version:    cur.Version,
		Ctime:      cur.Ctime,
	}
	ok, uerr := m.ClientSwitch.UpdateWithVersion(l.ctx, next, in.GetExpectVersion(), ts)
	if uerr != nil {
		return nil, uerr
	}
	if !ok {
		return nil, fmt.Errorf("%w: switch_id=%d expect=%d", model.ErrVersionConflict, cur.SwitchID, in.GetExpectVersion())
	}
	l.invalidateBoundConfig(cur.SwitchID, next.ConfigID, lim)

	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "save_client_switch",
		"ops_config:client_switch", strconv.FormatInt(cur.SwitchID, 10),
		switchDigest(cur), switchDigest(next), in.GetReason(), ts)
	return &rpc.SaveClientSwitchReply{Switch: switchInfo(next), AuditEntryId: entryID}, nil
}

// invalidateBoundConfig 让开关引用的那个配置项换代。
//
// 本服务没有面向端上的开关读取接口（契约缺口，见 README），因此 Redis 里
// **不存在**「某端开关全集」这类投影键，也就无键可删 —— 这里刻意不去删一个
// 自己从没写过的键空间。开关若绑定了 cfg_key，则真正改变展示面的是那个配置项，
// 所以 bump 它的 epoch 并删它的指针投影。
func (l *SaveClientSwitchLogic) invalidateBoundConfig(switchID, configID int64, lim limits) {
	if configID <= 0 {
		return
	}
	if _, err := l.svcCtx.Models.ConfigItem.BumpEpoch(l.ctx, []int64{configID}, model.NowUnix()); err != nil {
		l.Errorf("ops-config/switch: switch_id=%d 绑定的 config_id=%d epoch 换代失败: %v", switchID, configID, err)
	}
	item, err := l.svcCtx.Models.ConfigItem.FindByID(l.ctx, configID)
	if err != nil {
		l.Errorf("ops-config/switch: 找不到绑定的 config_id=%d，读缓存未删: %v", configID, err)
		return
	}
	cacheDel(l.ctx, l.svcCtx.Cache, lim.configKeysOf(item), l.Logger)
}

func switchDigest(s *model.ClientSwitch) string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("switch_key=%s,platform=%d,version_range=%s/%s,enabled=%d,config_id=%d,version=%d",
		s.SwitchKey, s.Platform, s.MinVersion, s.MaxVersion, s.Enabled, s.ConfigID, s.Version)
}
