// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevRulesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 生效中的分成规则（终端面只读 ACTIVE）
func NewRevRulesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevRulesLogic {
	return &RevRulesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevRules 分成规则是公开参考数据（不需要 mid），但终端面固定只读 ACTIVE：
// ListRevenueRulesReq.State 由网关写死成 RULE_STATE_ACTIVE，不接受客户端传 state——
// DRAFT/ARCHIVED 规则一旦被枚举出来，端上就会把没生效的单价当成承诺价展示。
// 这是「读范围收窄」而不是业务判定：哪条规则生效、生效到哪个版本仍由 creator-revenue 决定。
// source_type 枚举位原样透传（0 不过滤），page/page_size 原样透传，网关不设上限也不改写。
// 单价/封顶是规则原值，网关不换算分↔元、不折算单价（显示口径归端上）。
// 与套餐列表同类的公开目录数据，TTL 60：新规则上架后最多一分钟可见。
func (l *RevRulesLogic) RevRules(req *types.ParamRevRules) (resp *types.RevRulesResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errors.New("creator-revenue service not configured")
	}
	reply, err := l.svcCtx.CreatorRevenue.ListRevenueRules(l.ctx, &creatorrevenuerpc.ListRevenueRulesReq{
		State:      creatorrevenuerpc.RuleState_RULE_STATE_ACTIVE,
		SourceType: creatorrevenuerpc.RevenueSourceType(req.SourceType),
		Page:       int64(req.Page),
		Size:       int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/revRules: source_type=%d page=%d err=%v", req.SourceType, req.Page, err)
		return nil, err
	}
	return &types.RevRulesResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevRulesData{
			Rules:    revRulesToAPI(reply.GetRules()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetSize(),
		},
		TTL: 60,
	}, nil
}
