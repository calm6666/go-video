package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BaseLogic {
	return &BaseLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询单个用户基础资料。
// 参考 service.BaseInfo：缓存→DB→异步回填；不存在时返回 mid=0 防击穿结构。
func (l *BaseLogic) Base(in *rpc.MemberMidReq) (*rpc.BaseInfoReply, error) {
	reply, err := l.svcCtx.Repository.BaseInfo(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/Base: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
