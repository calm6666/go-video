package logic

import (
	"context"
	"fmt"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRoomBansLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRoomBansLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRoomBansLogic {
	return &ListRoomBansLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询禁播记录（运营侧审计）
//
// 必须有 operator_mid：reason/lift_reason 是运营内部说明，无归因主体的读取本身就不该发生。
// 本服务不按调用者角色裁剪字段——「猜谁是运营」是越权风险最高的地方，
// 脱敏与可见性由 gateway/admin 决定。
func (l *ListRoomBansLogic) ListRoomBans(in *rpc.ListRoomBansReq) (*rpc.ListRoomBansReply, error) {
	if in == nil {
		return nil, model.ErrOperatorRequired
	}
	if err := checkOperator(in.GetOperatorMid()); err != nil {
		return nil, err
	}
	if in.GetRoomId() < 0 || in.GetMid() < 0 {
		return nil, model.ErrInvalidRoomID
	}
	state, err := banStateFilter(in.GetState())
	if err != nil {
		return nil, err
	}
	size, err := l.svcCtx.PageSize(in.GetPageSize())
	if err != nil {
		return nil, err
	}
	page, err := clampPage(in.GetPage())
	if err != nil {
		return nil, err
	}
	q := model.BanListQuery{
		RoomID: in.GetRoomId(),
		Mid:    in.GetMid(),
		State:  state,
		Offset: pageOffset(page, size),
		Limit:  int32(size),
	}
	rows, err := l.svcCtx.Bans.List(l.ctx, q)
	if err != nil {
		return nil, err
	}
	total, err := l.svcCtx.Bans.Count(l.ctx, q)
	if err != nil {
		return nil, err
	}
	return &rpc.ListRoomBansReply{
		Bans:     banInfoList(rows),
		Total:    clampTotal(total),
		Page:     int32(page),
		PageSize: int32(size),
	}, nil
}

// banStateFilter 校验禁播记录状态过滤：0 表示不过滤，其余必须是已定义取值。
func banStateFilter(state int32) (int32, error) {
	if state == 0 {
		return 0, nil
	}
	if !model.ValidBanState(state) {
		return 0, fmt.Errorf("%w: state=%d", model.ErrInvalidBanTransition, state)
	}
	return state, nil
}
