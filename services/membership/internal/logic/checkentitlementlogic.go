package logic

import (
	"context"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CheckEntitlementLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCheckEntitlementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckEntitlementLogic {
	return &CheckEntitlementLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CheckEntitlement 单项权益判定——全站唯一的会员权益口径出口。
//
// granted=true 的唯一来源：mb_membership 里真有一行 expire_at > now 且档位达标
// （而那一行只能由 GrantMembership 的事务写入并留有 mb_grant 台账）。
// 这里没有任何默认值、没有任何「读不到就放行」的分支：
//   - 权益码不在目录 → ENTITLEMENT_CODE_UNKNOWN，granted=false；
//   - 权益码被下线 → ENTITLEMENT_CODE_DISABLED；
//   - 曾开通但已过期 → ENTITLEMENT_EXPIRED；
//   - 从未开通 → ENTITLEMENT_NO_MEMBERSHIP；
//   - mid 非法 → ENTITLEMENT_MID_INVALID；
//   - DB 读失败 → 上抛错误（不得折叠成 granted=false 的「正常未开通」）。
func (l *CheckEntitlementLogic) CheckEntitlement(in *rpc.CheckEntitlementReq) (*rpc.CheckEntitlementReply, error) {
	code := strings.TrimSpace(in.Code)

	if in.Mid <= 0 {
		// 结论而不是错误：游客/未登录是正常业务态，调用方要能直接分支处理。
		return &rpc.CheckEntitlementReply{
			Granted: false,
			Reason:  rpc.EntitlementReason_ENTITLEMENT_MID_INVALID,
		}, nil
	}

	var ent *model.Entitlement
	if code == "" {
		// 空码等价于「未知码」：没有口径可查，绝不能因为省了查询就默认放行。
		ent = nil
	} else {
		var err error
		ent, err = l.svcCtx.Entitlement.FindByCode(l.ctx, code)
		if err != nil {
			l.Errorf("membership/CheckEntitlement: read entitlement code=%s err=%v", code, err)
			return nil, err
		}
	}

	rows, err := l.svcCtx.Membership.ListByMid(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("membership/CheckEntitlement: read membership mid=%d err=%v", in.Mid, err)
		return nil, err
	}

	now := model.NowUnix()
	dec := decideEntitlement(code, ent, rows, now)
	return &rpc.CheckEntitlementReply{
		Granted:  dec.granted,
		Reason:   dec.reason,
		ExpireAt: dec.expireAt,
		VipType:  dec.vipType,
	}, nil
}

// decision 是一项权益的判定结论（含判定依据那一行的到期时间与档位）。
type decision struct {
	code     string
	granted  bool
	reason   rpc.EntitlementReason
	expireAt int64
	vipType  rpc.VipType
}

// decideEntitlement 是所有权益判定的唯一实现，CheckEntitlement / CheckEntitlements 共用，
// 避免两个接口对同一个 (mid, code) 给出不同答案。
//
// ent 为 nil 表示权益码不在目录（含空码）：暴露 CODE_UNKNOWN 且 granted=false。
// rows 是该用户全部档位身份行（含已过期），now 是服务端当前秒。
func decideEntitlement(code string, ent *model.Entitlement, rows []*model.Membership, now int64) decision {
	d := decision{code: code, reason: rpc.EntitlementReason_ENTITLEMENT_REASON_UNSPECIFIED}

	// 判定依据行：生效中的最高档；全都过期时退回曾达到的最高档，
	// 以便把 expire_at/vip_type 如实报给调用方（报告结论依据，不隐藏历史）。
	pick := model.PickMembershipFor(rows, 0, now)
	if pick != nil {
		d.expireAt = pick.ExpireAt
		d.vipType = rpc.VipType(pick.VipType)
	}

	switch {
	case ent == nil:
		d.reason = rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN
		return d
	case !ent.IsEnabled():
		d.reason = rpc.EntitlementReason_ENTITLEMENT_CODE_DISABLED
		return d
	case pick == nil:
		d.reason = rpc.EntitlementReason_ENTITLEMENT_NO_MEMBERSHIP
		d.expireAt = 0
		d.vipType = rpc.VipType_VIP_TYPE_UNSPECIFIED
		return d
	case !pick.IsActive(now):
		d.reason = rpc.EntitlementReason_ENTITLEMENT_EXPIRED
		return d
	case model.TierSufficient(pick.VipType, ent.MinVipType):
		d.granted = true
		d.reason = rpc.EntitlementReason_ENTITLEMENT_GRANTED
		return d
	default:
		// 持有档位不足（例：有大会员但码要求超级大会员）。这与「从没开通过」是两种结论：
		// 前者该引导升级，后者该引导开通，混成一个值客户端就只能猜。
		// 同时把实际持有的档位与到期时间带回 reply。
		d.reason = rpc.EntitlementReason_ENTITLEMENT_TIER_NOT_ENOUGH
		return d
	}
}
