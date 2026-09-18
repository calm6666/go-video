package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CookieInfoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCookieInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CookieInfoLogic {
	return &CookieInfoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// cookie 会话校验。
// 参考 identify.GetCookieInfo：解析 SESSDATA 后按 token 校验会话，
// 无效会话返回 is_login=false 而非错误。
func (l *CookieInfoLogic) CookieInfo(in *rpc.GetCookieInfoReq) (*rpc.GetCookieInfoReply, error) {
	reply, err := l.svcCtx.Repository.CookieInfo(l.ctx, in.Cookie)
	if err != nil {
		l.Errorf("account/CookieInfo: err=%v", err)
		return nil, err
	}
	return reply, nil
}
