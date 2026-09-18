package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ExpLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewExpLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpLogic {
	return &ExpLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询经验等级信息（含当前经验）。
// 参考 service.Exp：经验缓存→DB→BuildLevel(sexp=true)。
func (l *ExpLogic) Exp(in *rpc.MidReq) (*rpc.LevelInfoReply, error) {
	reply, err := l.svcCtx.Repository.Exp(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/Exp: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
