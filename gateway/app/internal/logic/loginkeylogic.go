// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type LoginKeyLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 获取密码加密 RSA 公钥
func NewLoginKeyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginKeyLogic {
	return &LoginKeyLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 获取密码加密 RSA 公钥（参考 passport-login /key）：
// 返回配置的 PEM 公钥与其 MD5 哈希，客户端用公钥加密登录/注册密码。
func (l *LoginKeyLogic) LoginKey() (resp *types.KeyResponse, err error) {
	pub := l.svcCtx.Config.PassportRSAPublicKey
	if pub == "" {
		return nil, errors.New("passport rsa public key not configured")
	}
	sum := md5.Sum([]byte(pub))
	return &types.KeyResponse{
		Code:    0,
		Message: "ok",
		Data:    types.KeyData{PublicKey: pub, Hash: hex.EncodeToString(sum[:])},
		TTL:     0,
	}, nil
}
