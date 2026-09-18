package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LevelLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLevelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LevelLogic {
	return &LevelLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询等级信息（不含当前经验）。
// 参考 service.Level：BuildLevel(sexp=false)。
func (l *LevelLogic) Level(in *rpc.MidReq) (*rpc.LevelInfoReply, error) {
	reply, err := l.svcCtx.Repository.Level(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/Level: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
