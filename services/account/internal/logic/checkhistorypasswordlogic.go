package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CheckHistoryPasswordLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCheckHistoryPasswordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckHistoryPasswordLogic {
	return &CheckHistoryPasswordLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 历史密码校验。
// 参考 passport /history/pwd/check：与历史密码逐一比对，
// 返回与入参一一对应的 "0,1" 命中序列。
func (l *CheckHistoryPasswordLogic) CheckHistoryPassword(in *rpc.CheckHistoryPwdReq) (*rpc.CheckHistoryPwdReply, error) {
	result, err := l.svcCtx.Repository.CheckHistoryPassword(l.ctx, in.Mid, in.Password)
	if err != nil {
		l.Errorf("account/CheckHistoryPassword: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.CheckHistoryPwdReply{Result: result}, nil
}
