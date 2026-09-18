package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameStatusLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRealnameStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameStatusLogic {
	return &RealnameStatusLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询实名认证状态。
// 参考 service.RealnameStatus：实名信息缓存→通过则为 1，否则 0。
func (l *RealnameStatusLogic) RealnameStatus(in *rpc.MemberMidReq) (*rpc.RealnameStatusReply, error) {
	status, err := l.svcCtx.Repository.RealnameStatus(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/RealnameStatus: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.RealnameStatusReply{RealnameStatus: int32(status)}, nil
}
