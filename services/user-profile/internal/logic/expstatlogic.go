package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ExpStatLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewExpStatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpStatLogic {
	return &ExpStatLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询当日经验奖励统计。
// 参考 service.Stat：Redis GETBIT 组合查询（登录/观看/分享 + 投币计数）。
func (l *ExpStatLogic) ExpStat(in *rpc.MidReq) (*rpc.ExpStatReply, error) {
	reply, err := l.svcCtx.Repository.Stat(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/ExpStat: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
