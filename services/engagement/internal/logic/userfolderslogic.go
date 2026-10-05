package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UserFoldersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUserFoldersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UserFoldersLogic {
	return &UserFoldersLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UserFolders 查询用户收藏夹列表。
// vmid != mid 时查他人公开收藏夹。
func (l *UserFoldersLogic) UserFolders(in *rpc.UserFoldersReq) (*rpc.UserFoldersReply, error) {
	if in.Vmid <= 0 {
		return nil, model.ErrInvalidMid
	}
	folders, err := l.svcCtx.Repository.UserFolders(l.ctx, in.Mid, in.Vmid)
	if err != nil {
		l.Errorf("engagement/UserFolders: mid=%d vmid=%d err=%v", in.Mid, in.Vmid, err)
		return nil, err
	}
	out := make([]*rpc.Folder, 0, len(folders))
	for _, f := range folders {
		if f == nil {
			continue
		}
		out = append(out, &rpc.Folder{
			Fid:         f.Fid,
			Mid:         f.Mid,
			Name:        f.Name,
			Description: f.Description,
			Cover:       f.Cover,
			Public:      f.Public,
			State:       f.State,
			Ctime:       f.Ctime,
			Mtime:       f.Mtime,
			Count:       f.Count,
		})
	}
	return &rpc.UserFoldersReply{Folders: out}, nil
}
