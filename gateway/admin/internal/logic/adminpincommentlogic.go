// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	commentrpc "go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminPinCommentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营置顶/取消置顶评论
func NewAdminPinCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminPinCommentLogic {
	return &AdminPinCommentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminPinComment 聚合 comment PinComment RPC：pin=true 置顶、false 取消。
// comment.v1.PinCommentReq 用 admin_mid 表达操作者（服务侧据此判定是否为该 oid 的 UP 或管理员），
// 因此后台的 operator_mid 原样映射过去；oid 只用于下游校验，网关不改写评论状态。
func (l *AdminPinCommentLogic) AdminPinComment(req *types.ParamAdminPinComment) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	if err := adminSubjectGate(l.ctx, "adminPinComment", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}

	if _, err := l.svcCtx.Comment.PinComment(l.ctx, &commentrpc.PinCommentReq{
		Rpid:     req.Rpid,
		Oid:      req.Oid,
		Pin:      req.Pin,
		AdminMid: req.OperatorMid,
	}); err != nil {
		l.Errorf("gateway/admin/adminPinComment: operator=%d rpid=%d oid=%d pin=%v err=%v",
			req.OperatorMid, req.Rpid, req.Oid, req.Pin, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
