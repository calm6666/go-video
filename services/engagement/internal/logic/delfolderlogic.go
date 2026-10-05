package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DelFolderLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDelFolderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelFolderLogic {
	return &DelFolderLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DelFolder 删除收藏夹（软删）。
// 删除前应保证收藏夹为空，由 gateway 或本服务调用前校验；本方法不级联清空。
func (l *DelFolderLogic) DelFolder(in *rpc.DelFolderReq) (*rpc.EmptyReply, error) {
	if in.Fid <= 0 {
		return nil, model.ErrInvalidFid
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if err := l.svcCtx.Repository.DelFolder(l.ctx, in.Fid, in.Mid); err != nil {
		l.Errorf("engagement/DelFolder: fid=%d mid=%d err=%v", in.Fid, in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
