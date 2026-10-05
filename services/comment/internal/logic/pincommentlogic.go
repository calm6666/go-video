package logic

import (
	"context"
	"errors"

	"go-video/services/comment/internal/svc"
	"go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PinCommentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPinCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PinCommentLogic {
	return &PinCommentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// PinComment 置顶/取消置顶评论。
// 调用方（gateway）需校验 admin_mid 是否为视频 UP 或管理员。
func (l *PinCommentLogic) PinComment(in *rpc.PinCommentReq) (*rpc.EmptyReply, error) {
	if in.Rpid <= 0 {
		return nil, errors.New("comment: invalid rpid")
	}
	if in.Oid <= 0 {
		return nil, errors.New("comment: invalid oid")
	}
	if in.AdminMid <= 0 {
		return nil, errors.New("comment: admin_mid required")
	}
	if err := l.svcCtx.Repository.PinComment(l.ctx, in.Rpid, in.Oid, in.Pin); err != nil {
		l.Errorf("comment/PinComment: rpid=%d oid=%d pin=%v err=%v", in.Rpid, in.Oid, in.Pin, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
