package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameApplyStatusLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRealnameApplyStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameApplyStatusLogic {
	return &RealnameApplyStatusLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询实名申请流程状态。
// 参考 service.RealnameApplyStatus：0 审核中、1 通过、2 驳回、3 未申请。
func (l *RealnameApplyStatusLogic) RealnameApplyStatus(in *rpc.MemberMidReq) (*rpc.RealnameApplyInfoReply, error) {
	reply, err := l.svcCtx.Repository.RealnameApplyStatus(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/RealnameApplyStatus: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
