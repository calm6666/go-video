package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type TokenInfoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewTokenInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TokenInfoLogic {
	return &TokenInfoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// token 校验。
// 参考 identify.GetTokenInfo：缓存→DB 校验有效期与状态，供网关统一鉴权。
// 无效 token 返回 is_login=false 而非错误。
func (l *TokenInfoLogic) TokenInfo(in *rpc.GetTokenInfoReq) (*rpc.GetTokenInfoReply, error) {
	reply, err := l.svcCtx.Repository.TokenInfo(l.ctx, in.Token)
	if err != nil {
		l.Errorf("account/TokenInfo: err=%v", err)
		return nil, err
	}
	return reply, nil
}
