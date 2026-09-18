package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameStrippedInfoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRealnameStrippedInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameStrippedInfoLogic {
	return &RealnameStrippedInfoLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询脱敏实名信息（不含姓名/证件号明文）。
// 参考 service.RealnameStrippedInfo：状态/渠道/国家/证件类型 + 成年状态。
func (l *RealnameStrippedInfoLogic) RealnameStrippedInfo(in *rpc.MemberMidReq) (*rpc.RealnameStrippedInfoReply, error) {
	reply, err := l.svcCtx.Repository.RealnameStrippedInfo(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/RealnameStrippedInfo: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
