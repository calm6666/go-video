// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	commentrpc "go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PinCommentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 稿件 UP 主置顶/取消置顶评论
func NewPinCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PinCommentLogic {
	return &PinCommentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PinComment 聚合 comment PinComment RPC：稿件 UP 主置顶/取消置顶评论。
// admin_mid 只做透传，是否为该稿件 UP 主或管理员的归属校验由 comment 服务判定（AGENTS.md §5）。
func (l *PinCommentLogic) PinComment(req *types.ParamPinComment) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	if _, err = l.svcCtx.Comment.PinComment(l.ctx, &commentrpc.PinCommentReq{
		Rpid:     req.Rpid,
		Oid:      req.Oid,
		Pin:      req.Pin,
		AdminMid: req.AdminMid,
	}); err != nil {
		l.Errorf("gateway/app/pinComment: rpid=%d oid=%d pin=%t admin_mid=%d err=%v",
			req.Rpid, req.Oid, req.Pin, req.AdminMid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
