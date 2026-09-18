package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type InfosByName3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewInfosByName3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *InfosByName3Logic {
	return &InfosByName3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 按用户名批量查询用户基础信息。
// 参考 service.InfosByName：先按 name→mid 查本地 account_credential 表，
// 再用 mid 列表走 repository.Infos 的批量缓存+回源路径。
// names 截断到 100，避免无界查询。
func (l *InfosByName3Logic) InfosByName3(in *rpc.NamesReq) (*rpc.InfosReply, error) {
	names := in.Names
	if len(names) > 100 {
		names = names[:100]
	}
	nameToMid, err := l.svcCtx.Repository.MidsByName(l.ctx, names)
	if err != nil {
		l.Errorf("account/InfosByName3: MidsByName err=%v", err)
		return nil, err
	}
	mids := make([]int64, 0, len(nameToMid))
	for _, mid := range nameToMid {
		mids = append(mids, mid)
	}
	infos, err := l.svcCtx.Repository.Infos(l.ctx, mids)
	if err != nil {
		l.Errorf("account/InfosByName3: Infos err=%v", err)
		return nil, err
	}
	if infos == nil {
		infos = map[int64]*rpc.Info{}
	}
	return &rpc.InfosReply{Infos: infos}, nil
}
