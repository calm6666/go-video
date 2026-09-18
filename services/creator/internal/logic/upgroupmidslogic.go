package logic

import (
	"context"

	"go-video/services/creator/internal/svc"
	"go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpGroupMidsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpGroupMidsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpGroupMidsLogic {
	return &UpGroupMidsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询某个分组下的所有用户。
// 参考 service.UpGroupMids：分页查询 up_special 反向索引。
func (l *UpGroupMidsLogic) UpGroupMids(in *rpc.UpGroupMidsReq) (*rpc.UpGroupMidsReply, error) {
	if in.GroupId <= 0 {
		return nil, errInvalidGroupID
	}
	if in.Pn <= 0 {
		in.Pn = 1
	}
	if in.Ps <= 0 {
		in.Ps = 1000
	}
	if in.Ps > 1000 {
		// 与 obc request.proto 中 UpGroupMidsReq 的 ps max=10000 对齐
		// 但实际单页 1000 已是合理上限
		in.Ps = 1000
	}
	mids, total, err := l.svcCtx.Repository.UpGroupMids(l.ctx, in.GroupId, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("creator/UpGroupMids: gid=%d pn=%d ps=%d err=%v", in.GroupId, in.Pn, in.Ps, err)
		return nil, err
	}
	if mids == nil {
		mids = []int64{}
	}
	return &rpc.UpGroupMidsReply{Mids: mids, Total: total}, nil
}
