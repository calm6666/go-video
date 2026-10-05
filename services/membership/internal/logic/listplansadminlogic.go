package logic

import (
	"context"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPlansAdminLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListPlansAdminLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPlansAdminLogic {
	return &ListPlansAdminLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListPlansAdmin 运营面分页查询：含 DRAFT 与 OFF_SALE（终端面的 ListPlans 永远不给）。
//
// 分页口径：page<1 归 1；size<=0 用 DefaultPageSize；size 超过 MaxPageSize 直接裁剪，
// 并把实际生效的 page/size 回显在 reply 里（契约带了这两个字段），不返回错误。
func (l *ListPlansAdminLogic) ListPlansAdmin(in *rpc.ListPlansAdminReq) (*rpc.ListPlansAdminReply, error) {
	cfg := l.svcCtx.Config.Membership

	var state int32
	if in.State != rpc.PlanSaleState_PLAN_SALE_STATE_UNSPECIFIED {
		if !model.ValidPlanState(int32(in.State)) {
			return nil, model.ErrInvalidPlanState
		}
		state = int32(in.State)
	}
	var vipType int32
	if in.VipType != rpc.VipType_VIP_TYPE_UNSPECIFIED {
		if err := requireVipType(in.VipType); err != nil {
			return nil, err
		}
		vipType = int32(in.VipType)
	}

	page, size, offset := clampPage(cfg, in.Page, in.Size)
	rows, total, err := l.svcCtx.Plan.ListAdmin(l.ctx, model.PlanQuery{
		State:   state,
		VipType: vipType,
		Keyword: in.Keyword,
		Offset:  offset,
		Limit:   size,
	})
	if err != nil {
		l.Errorf("membership/ListPlansAdmin: query failed page=%d size=%d err=%v", page, size, err)
		return nil, err
	}
	return &rpc.ListPlansAdminReply{
		Plans: planListToRPC(rows),
		Total: total,
		Page:  page,
		Size:  size,
	}, nil
}
