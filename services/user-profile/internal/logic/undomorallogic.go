package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UndoMoralLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUndoMoralLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UndoMoralLogic {
	return &UndoMoralLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 撤销节操值变更。
// 参考 service.UndoMoral：标记原日志撤销、记录撤销日志、反向扣回节操值。
func (l *UndoMoralLogic) UndoMoral(in *rpc.UndoMoralReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.UndoMoral(l.ctx, in.LogId, in.Remark, in.Operator); err != nil {
		l.Errorf("user-profile/UndoMoral: log_id=%s err=%v", in.LogId, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
