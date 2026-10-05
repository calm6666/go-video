package logic

import (
	"context"
	"fmt"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAnchorsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAnchorsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAnchorsLogic {
	return &ListAnchorsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询房间主播绑定
//
// 本方法是房间成员的唯一读出口：不做跨房间聚合（那是 creator 域），也不在此判定调用者权限
// （终端侧只读公开成员，运营侧由 gateway/admin 把关）。
func (l *ListAnchorsLogic) ListAnchors(in *rpc.ListAnchorsReq) (*rpc.ListAnchorsReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	role, err := anchorRoleFilter(in.GetRole())
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
	q := model.AnchorListQuery{
		RoomID:      in.GetRoomId(),
		Role:        role,
		OnlyEnabled: in.GetOnlyEnabled(),
		Offset:      pageOffset(page, size),
		Limit:       int32(size),
	}
	rows, err := l.svcCtx.Anchors.List(l.ctx, q)
	if err != nil {
		return nil, err
	}
	total, err := l.svcCtx.Anchors.Count(l.ctx, q)
	if err != nil {
		return nil, err
	}
	return &rpc.ListAnchorsReply{
		Anchors: anchorInfoList(rows),
		Total:   clampTotal(total),
	}, nil
}

// anchorRoleFilter 校验角色过滤：UNSPECIFIED 不过滤，未知取值拒绝（不做「未知即全放行」）。
func anchorRoleFilter(r rpc.AnchorRole) (int32, error) {
	v := int32(r)
	if v == model.AnchorRoleUnspecified {
		return 0, nil
	}
	if !model.ValidAnchorRole(v) {
		return 0, fmt.Errorf("%w: role=%d", model.ErrAnchorRoleInvalid, v)
	}
	return v, nil
}
