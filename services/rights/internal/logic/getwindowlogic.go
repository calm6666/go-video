package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetWindowLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetWindowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetWindowLogic {
	return &GetWindowLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetWindow 查询单个窗口；不存在返回 ErrWindowNotFound。
func (l *GetWindowLogic) GetWindow(in *rpc.WindowReq) (*rpc.WindowReply, error) {
	if in.WindowId <= 0 {
		return nil, model.ErrInvalidWindowID
	}
	w, err := l.svcCtx.Repository.GetWindow(l.ctx, in.WindowId)
	if err != nil {
		l.Errorf("rights/GetWindow: window_id=%d err=%v", in.WindowId, err)
		return nil, err
	}
	if w == nil {
		return nil, model.ErrWindowNotFound
	}
	return &rpc.WindowReply{Window: windowToRPC(w)}, nil
}
