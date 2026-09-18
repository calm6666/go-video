package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameTelCaptureLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRealnameTelCaptureLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameTelCaptureLogic {
	return &RealnameTelCaptureLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 发送实名手机验证码。
// 参考 service.RealnameTelCapture：发送次数限制（>5 拒绝）→生成 6 位验证码→
// SMS 下发（notification 服务未接入前仅记录日志）→写入 Redis。
func (l *RealnameTelCaptureLogic) RealnameTelCapture(in *rpc.MemberMidReq) (*rpc.EmptyReply, error) {
	if _, err := l.svcCtx.Repository.RealnameTelCapture(l.ctx, in.Mid); err != nil {
		l.Errorf("user-profile/RealnameTelCapture: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
