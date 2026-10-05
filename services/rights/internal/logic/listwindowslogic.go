package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListWindowsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListWindowsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListWindowsLogic {
	return &ListWindowsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListWindows 按 content_id 或 contract_id 查询窗口；ps 上限 50。
func (l *ListWindowsLogic) ListWindows(in *rpc.ListWindowsReq) (*rpc.WindowsReply, error) {
	if in.Ps <= 0 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	rows, total, err := l.svcCtx.Repository.ListWindows(l.ctx,
		in.ContentId, in.ContractId, int32(in.ContentType), int32(in.State), in.Pn, in.Ps)
	if err != nil {
		l.Errorf("rights/ListWindows: content_id=%d contract_id=%d err=%v",
			in.ContentId, in.ContractId, err)
		return nil, err
	}
	out := make([]*rpc.Window, 0, len(rows))
	for _, w := range rows {
		out = append(out, windowToRPC(w))
	}
	return &rpc.WindowsReply{Total: total, Windows: out}, nil
}
