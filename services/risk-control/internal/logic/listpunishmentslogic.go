package logic

import (
	"context"

	"go-video/common/validation"
	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPunishmentsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListPunishmentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPunishmentsLogic {
	return &ListPunishmentsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// maxPageSize 处罚/规则/名单三个列表接口统一的分页上限（与 repository 保持一致）。
const maxPageSize = 50

// 分页查询处罚。
// 该接口返回运营视角的完整字段（含 reason、operator），只允许 admin 链路调用；
// 端上要用的是 CheckAction 响应里的 PunishmentSnapshot，二者故意分开。
func (l *ListPunishmentsLogic) ListPunishments(in *rpc.ListPunishmentsReq) (*rpc.ListPunishmentsReply, error) {
	scope := int32(in.GetScope())
	if !model.ValidRuleAction(scope) {
		return nil, model.ErrInvalidTarget
	}
	state := int32(in.GetState())
	if state != 0 && !model.ValidPunishmentState(state) {
		return nil, model.ErrInvalidTarget
	}
	page := validation.NormalizePage(int(in.GetPn()), int(in.GetPs()), maxPageSize)

	rows, total, err := l.svcCtx.Repository.ListPunishments(l.ctx,
		in.GetMid(), scope, state, in.GetOnlyActive(), page.Page, page.PageSize)
	if err != nil {
		l.Errorf("risk-control/ListPunishments: failed mid=%d scope=%d state=%d err=%v", in.GetMid(), scope, state, err)
		return nil, err
	}
	return &rpc.ListPunishmentsReply{
		Punishments: punishmentsToProto(rows),
		Total:       total,
		Pn:          int32(page.Page),
		Ps:          int32(page.PageSize),
	}, nil
}
