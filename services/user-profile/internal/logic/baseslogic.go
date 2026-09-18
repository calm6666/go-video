package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BasesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBasesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BasesLogic {
	return &BasesLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 批量查询用户基础资料。
// 参考 service.BatchBaseInfo：最多 100 个，缓存批量命中后按 miss 回源。
func (l *BasesLogic) Bases(in *rpc.MemberMidsReq) (*rpc.BaseInfosReply, error) {
	bases, err := l.svcCtx.Repository.BatchBaseInfo(l.ctx, in.Mids)
	if err != nil {
		l.Errorf("user-profile/Bases: err=%v", err)
		return nil, err
	}
	return &rpc.BaseInfosReply{BaseInfos: bases}, nil
}
