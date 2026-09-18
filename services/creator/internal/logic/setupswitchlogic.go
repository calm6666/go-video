package logic

import (
	"context"

	"go-video/services/creator/internal/svc"
	"go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetUpSwitchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetUpSwitchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetUpSwitchLogic {
	return &SetUpSwitchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 设置 UP 主关注弹窗开关。
// 参考 service.SetUpSwitch：DB 写入后失效缓存。
func (l *SetUpSwitchLogic) SetUpSwitch(in *rpc.UpSwitchReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, errInvalidMid
	}
	if in.From != 0 && in.From != 1 {
		return nil, errInvalidSwitchFrom
	}
	if in.State != 0 && in.State != 1 {
		return nil, errInvalidSwitchState
	}
	if err := l.svcCtx.Repository.SetUpSwitch(l.ctx, int32(in.Mid), in.From, in.State); err != nil {
		l.Errorf("creator/SetUpSwitch: mid=%d from=%d state=%d err=%v", in.Mid, in.From, in.State, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
