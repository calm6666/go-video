package logic

import (
	"context"

	"go-video/services/creator/internal/svc"
	"go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpSpecialLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpSpecialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpSpecialLogic {
	return &UpSpecialLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个 UP 主特殊属性。
// 参考 service.UpSpecial：缓存→DB→回填。
func (l *UpSpecialLogic) UpSpecial(in *rpc.UpSpecialReq) (*rpc.UpSpecialReply, error) {
	ids, err := l.svcCtx.Repository.UpSpecial(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("creator/UpSpecial: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.UpSpecialReply{
		UpSpecial: &rpc.UpSpecial{GroupIds: ids},
	}, nil
}
