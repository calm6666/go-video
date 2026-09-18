package logic

import (
	"context"

	"go-video/services/creator/internal/svc"
	"go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpGroupsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpGroupsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpGroupsLogic {
	return &UpGroupsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询所有特殊用户组。
// 参考 service.UpGroups：内存 5 分钟缓存，DB JSON 字符串缓存。
func (l *UpGroupsLogic) UpGroups(in *rpc.NoArgReq) (*rpc.UpGroupsReply, error) {
	groups, err := l.svcCtx.Repository.UpGroups(l.ctx)
	if err != nil {
		l.Errorf("creator/UpGroups: err=%v", err)
		return nil, err
	}
	out := make(map[int64]*rpc.UpGroup, len(groups))
	for id, g := range groups {
		out[id] = &rpc.UpGroup{
			Id:        g.ID,
			Name:      g.Name,
			Tag:       g.Tag,
			ShortTag:  g.ShortTag,
			FontColor: g.FontColor,
			BgColor:   g.BgColor,
			Note:      g.Note,
		}
	}
	return &rpc.UpGroupsReply{UpGroups: out}, nil
}
