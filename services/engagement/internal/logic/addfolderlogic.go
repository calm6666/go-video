package logic

import (
	"context"
	"time"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddFolderLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddFolderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddFolderLogic {
	return &AddFolderLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AddFolder 创建收藏夹。
// 每个用户最多创建 100 个收藏夹。
func (l *AddFolderLogic) AddFolder(in *rpc.AddFolderReq) (*rpc.AddFolderReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Name == "" {
		return nil, model.ErrFolderNameEmpty
	}
	now := time.Now().Unix()
	f := &model.FavoriteFolder{
		Mid:         in.Mid,
		Name:        in.Name,
		Description: in.Description,
		Cover:       in.Cover,
		Public:      in.Public,
		State:       model.FolderStateNormal,
		Ctime:       now,
		Mtime:       now,
	}
	fid, err := l.svcCtx.Repository.AddFolder(l.ctx, f)
	if err != nil {
		l.Errorf("engagement/AddFolder: mid=%d name=%s err=%v", in.Mid, in.Name, err)
		return nil, err
	}
	return &rpc.AddFolderReply{Fid: fid}, nil
}
