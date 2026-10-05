package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddShareLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddShareLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddShareLogic {
	return &AddShareLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AddShare 记录分享行为并返回当前对象累计分享数。
// 幂等：同一用户对同一对象只计一次。
func (l *AddShareLogic) AddShare(in *rpc.AddShareReq) (*rpc.AddShareReply, error) {
	if in.Oid <= 0 {
		return nil, model.ErrInvalidOid
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	count, err := l.svcCtx.Repository.AddShare(l.ctx, in.Oid, in.Mid, in.Type)
	if err != nil {
		l.Errorf("engagement/AddShare: oid=%d mid=%d err=%v", in.Oid, in.Mid, err)
		return nil, err
	}
	return &rpc.AddShareReply{Shares: count}, nil
}
