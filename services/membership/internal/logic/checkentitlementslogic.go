package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// maxCodesPerCheck 是 CheckEntitlements 单次可问的权益码数量。
// 上限存在的理由不是省 CPU，而是防止一个调用把 IN 列表撑到让判定路径退化成慢查——
// 播放详情页正常只需要几项。超限返回错误而不是静默截断：截断会让调用方以为「没问」。
const maxCodesPerCheck = 100

type CheckEntitlementsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCheckEntitlementsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckEntitlementsLogic {
	return &CheckEntitlementsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CheckEntitlements 多项权益判定（播放详情页一次问清多项能力）。
//
// 口径：
//  1. 与 CheckEntitlement 共用同一个判定实现 decideEntitlement，两项接口不可能给出矛盾答案；
//  2. decisions 与入参 codes 一一对应、顺序一致（重复的码就重复回答），调用方无需自己映射；
//  3. 一次读身份 + 一次批量读码目录，不逐码打库；
//  4. 任一 DB 读失败上抛错误，不把整批折叠成 granted=false。
func (l *CheckEntitlementsLogic) CheckEntitlements(in *rpc.CheckEntitlementsReq) (*rpc.CheckEntitlementsReply, error) {
	if len(in.Codes) > maxCodesPerCheck {
		return nil, fmt.Errorf("%w: too many codes (%d > %d)", model.ErrInvalidQueryFilter, len(in.Codes), maxCodesPerCheck)
	}

	now := model.NowUnix()

	// mid 非法：逐项给出 MID_INVALID 结论（游客态是正常业务态，不是错误）。
	if in.Mid <= 0 {
		out := make([]*rpc.EntitlementDecision, 0, len(in.Codes))
		for _, c := range in.Codes {
			out = append(out, &rpc.EntitlementDecision{
				Code:    c,
				Granted: false,
				Reason:  rpc.EntitlementReason_ENTITLEMENT_MID_INVALID,
			})
		}
		return &rpc.CheckEntitlementsReply{
			Decisions: out,
			VipType:   rpc.VipType_VIP_TYPE_UNSPECIFIED,
		}, nil
	}

	// 批量读权益码目录：去重后一次 IN 查询，缺失的码不在 map 里，判定为 CODE_UNKNOWN。
	seen := make(map[string]struct{}, len(in.Codes))
	unique := make([]string, 0, len(in.Codes))
	for _, c := range in.Codes {
		code := strings.TrimSpace(c)
		if code == "" {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		unique = append(unique, code)
	}

	var catalog map[string]*model.Entitlement
	if len(unique) > 0 {
		ents, err := l.svcCtx.Entitlement.ListByCodes(l.ctx, unique)
		if err != nil {
			l.Errorf("membership/CheckEntitlements: read entitlements mid=%d codes=%d err=%v", in.Mid, len(unique), err)
			return nil, err
		}
		catalog = make(map[string]*model.Entitlement, len(ents))
		for _, e := range ents {
			if e != nil {
				catalog[e.Code] = e
			}
		}
	}

	rows, err := l.svcCtx.Membership.ListByMid(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("membership/CheckEntitlements: read membership mid=%d err=%v", in.Mid, err)
		return nil, err
	}

	decisions := make([]*rpc.EntitlementDecision, 0, len(in.Codes))
	for _, c := range in.Codes {
		code := strings.TrimSpace(c)
		dec := decideEntitlement(code, catalog[code], rows, now)
		decisions = append(decisions, &rpc.EntitlementDecision{
			Code:    c,
			Granted: dec.granted,
			Reason:  dec.reason,
		})
	}

	out := &rpc.CheckEntitlementsReply{Decisions: decisions}
	if pick := model.PickMembershipFor(rows, 0, now); pick != nil {
		out.ExpireAt = pick.ExpireAt
		out.VipType = rpc.VipType(pick.VipType)
	}
	return out, nil
}
