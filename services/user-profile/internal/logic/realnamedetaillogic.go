package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameDetailLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRealnameDetailLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameDetailLogic {
	return &RealnameDetailLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询实名详情（姓名、证件号、性别、手持照）。
// 参考 service.RealnameDetail：认证通过时按身份证解析性别并拼接手持照 URL。
func (l *RealnameDetailLogic) RealnameDetail(in *rpc.MemberMidReq) (*rpc.RealnameDetailReply, error) {
	reply, err := l.svcCtx.Repository.RealnameDetail(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/RealnameDetail: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
