package logic

import (
	"context"

	"go-video/services/creator/internal/svc"
	"go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpSwitchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpSwitchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpSwitchLogic {
	return &UpSwitchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询 UP 主关注弹窗开关。
// 参考 service.UpSwitch：缓存→DB→回填，不存在视为默认关闭。
func (l *UpSwitchLogic) UpSwitch(in *rpc.UpSwitchReq) (*rpc.UpSwitchReply, error) {
	if in.Mid <= 0 {
		return nil, errInvalidMid
	}
	if in.From != 0 && in.From != 1 {
		return nil, errInvalidSwitchFrom
	}
	state, err := l.svcCtx.Repository.UpSwitch(l.ctx, in.Mid, in.From)
	if err != nil {
		l.Errorf("creator/UpSwitch: mid=%d from=%d err=%v", in.Mid, in.From, err)
		return nil, err
	}
	return &rpc.UpSwitchReply{State: state}, nil
}
