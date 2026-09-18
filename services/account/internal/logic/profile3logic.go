package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Profile3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewProfile3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Profile3Logic {
	return &Profile3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询用户完整资料。
// 参考 service.Profile：缓存 miss 时由 repository 聚合 user-profile + 本地 account/account_credential。
func (l *Profile3Logic) Profile3(in *rpc.MidReq) (*rpc.ProfileReply, error) {
	profile, err := l.svcCtx.Repository.Profile(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("account/Profile3: repository.Profile mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	if profile == nil {
		profile = &rpc.Profile{Mid: in.Mid}
	}
	return &rpc.ProfileReply{Profile: profile}, nil
}
