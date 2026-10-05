package logic

// 本文件是手写领域转换层（不是 goctl 生成产物）：
// 只负责 model ↔ rpc 的字段映射，不含业务规则与判定口径。

import (
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"
)

// planToRPC 套餐行 → PlanInfo。
// platforms 由位掩码还原为枚举列表；auto_renew_supported 从 0/1 转 bool。
func planToRPC(p *model.Plan) *rpc.PlanInfo {
	if p == nil {
		return nil
	}
	return &rpc.PlanInfo{
		PlanId:             p.PlanID,
		PlanCode:           p.PlanCode,
		Name:               p.Name,
		Description:        p.Description,
		VipType:            rpc.VipType(p.VipType),
		DurationDays:       p.DurationDays,
		UnitCount:          p.UnitCount,
		PriceMinor:         p.PriceMinor,
		PromPriceMinor:     p.PromPriceMinor,
		Currency:           p.Currency,
		Platforms:          planPlatformsToRPC(p.PlatformMask),
		AutoRenewSupported: p.AutoRenewSupported == 1,
		State:              rpc.PlanSaleState(p.State),
		Version:            p.Version,
		Ctime:              p.Ctime,
		Mtime:              p.Mtime,
		CreatedBy:          p.CreatedBy,
		UpdatedBy:          p.UpdatedBy,
	}
}

// planListToRPC 批量转换，跳过空指针。
func planListToRPC(rows []*model.Plan) []*rpc.PlanInfo {
	out := make([]*rpc.PlanInfo, 0, len(rows))
	for _, p := range rows {
		if p != nil {
			out = append(out, planToRPC(p))
		}
	}
	return out
}

// planPlatformsToRPC 位掩码 → 平台枚举列表（升序）。
func planPlatformsToRPC(mask uint32) []rpc.PlanPlatform {
	list := model.PlatformsOfMask(mask)
	out := make([]rpc.PlanPlatform, 0, len(list))
	for _, p := range list {
		out = append(out, rpc.PlanPlatform(p))
	}
	return out
}

// planPlatformsFromRPC 平台枚举列表 → 位掩码；任一枚举非法整体报错。
// 不静默忽略非法值：漏掉一个平台会让该端看不到套餐，且没有任何报错线索。
func planPlatformsFromRPC(list []rpc.PlanPlatform) (uint32, error) {
	ords := make([]int32, 0, len(list))
	for _, p := range list {
		ords = append(ords, int32(p))
	}
	return model.PlatformMask(ords)
}

// entitlementToRPC 权益码行 → EntitlementInfo。
func entitlementToRPC(e *model.Entitlement) *rpc.EntitlementInfo {
	if e == nil {
		return nil
	}
	return &rpc.EntitlementInfo{
		Code:        e.Code,
		Name:        e.Name,
		Description: e.Description,
		MinVipType:  rpc.VipType(e.MinVipType),
		Enabled:     e.IsEnabled(),
		Version:     e.Version,
		Ctime:       e.Ctime,
		Mtime:       e.Mtime,
	}
}

// entitlementListToRPC 批量转换。
func entitlementListToRPC(rows []*model.Entitlement) []*rpc.EntitlementInfo {
	out := make([]*rpc.EntitlementInfo, 0, len(rows))
	for _, e := range rows {
		if e != nil {
			out = append(out, entitlementToRPC(e))
		}
	}
	return out
}

// membershipToRPC 会员身份行 → MembershipInfo；nil 返回 nil（未开通时不编造结构）。
func membershipToRPC(m *model.Membership) *rpc.MembershipInfo {
	if m == nil {
		return nil
	}
	return &rpc.MembershipInfo{
		Mid:               m.Mid,
		VipType:           rpc.VipType(m.VipType),
		StartAt:           m.StartAt,
		ExpireAt:          m.ExpireAt,
		AutoRenew:         m.AutoRenew == 1,
		AutoRenewChannel:  m.AutoRenewChannel,
		AutoRenewSignedAt: m.AutoRenewSignedAt,
		Source:            rpc.GrantSource(m.Source),
		PaidMonthCount:    m.PaidMonthCount,
		Version:           m.Version,
		Ctime:             m.Ctime,
		Mtime:             m.Mtime,
	}
}

// membershipListToRPC 批量转换。
func membershipListToRPC(rows []*model.Membership) []*rpc.MembershipInfo {
	out := make([]*rpc.MembershipInfo, 0, len(rows))
	for _, m := range rows {
		if m != nil {
			out = append(out, membershipToRPC(m))
		}
	}
	return out
}

// grantToRPC 授予台账行 → GrantInfo。
func grantToRPC(g *model.Grant) *rpc.GrantInfo {
	if g == nil {
		return nil
	}
	return &rpc.GrantInfo{
		GrantId:        g.GrantID,
		Mid:            g.Mid,
		VipType:        rpc.VipType(g.VipType),
		Action:         g.Action,
		DeltaDays:      g.DeltaDays,
		PlanId:         g.PlanID,
		Source:         rpc.GrantSource(g.Source),
		BizOrderNo:     g.BizOrderNo,
		PaymentNo:      g.PaymentNo,
		BeforeExpireAt: g.BeforeExpireAt,
		AfterExpireAt:  g.AfterExpireAt,
		Operator:       g.Operator,
		RequestId:      g.RequestID,
		Reason:         g.Reason,
		Ctime:          g.Ctime,
	}
}

// grantListToRPC 批量转换。
func grantListToRPC(rows []*model.Grant) []*rpc.GrantInfo {
	out := make([]*rpc.GrantInfo, 0, len(rows))
	for _, g := range rows {
		if g != nil {
			out = append(out, grantToRPC(g))
		}
	}
	return out
}
