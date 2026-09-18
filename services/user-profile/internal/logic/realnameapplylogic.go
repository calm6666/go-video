package logic

import (
	"context"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameApplyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRealnameApplyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameApplyLogic {
	return &RealnameApplyLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 提交实名认证申请。
// 参考 service.RealnameApply：验证码校验→重复申请校验→证件查重→身份证格式校验→
// 证件照落库→证件号 RSA 加密→写申请单→失效验证码与实名缓存。
func (l *RealnameApplyLogic) RealnameApply(in *rpc.RealnameApplyReq) (*rpc.EmptyReply, error) {
	err := l.svcCtx.Repository.RealnameApply(l.ctx, &repository.RealnameApplyArg{
		Mid:           in.Mid,
		CaptureCode:   int(in.CaptureCode),
		Realname:      in.Realname,
		CardType:      int8(in.CardType),
		CardCode:      in.CardCode,
		Country:       int16(in.Country),
		HandIMGToken:  in.HandImgToken,
		FrontIMGToken: in.FrontImgToken,
		BackIMGToken:  in.BackImgToken,
	})
	if err != nil {
		l.Errorf("user-profile/RealnameApply: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
