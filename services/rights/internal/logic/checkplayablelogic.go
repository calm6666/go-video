package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CheckPlayableLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCheckPlayableLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckPlayableLogic {
	return &CheckPlayableLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CheckPlayable 校验内容在某地区是否可播放。
// 校验条件：state=active AND start_time <= now AND end_time > now AND region 命中。
// 优先查 Redis 短缓存，miss 时落库；过期窗口会被后台推进为 expired 并强制失效缓存。
func (l *CheckPlayableLogic) CheckPlayable(in *rpc.CheckReq) (*rpc.CheckReply, error) {
	if in.ContentId <= 0 {
		return nil, model.ErrInvalidContentID
	}
	if in.Region == "" {
		return nil, model.ErrInvalidRegion
	}
	playable, windowID, endTime, err := l.svcCtx.Repository.CheckPlayable(l.ctx,
		in.ContentId, int32(in.ContentType), in.Region)
	if err != nil {
		l.Errorf("rights/CheckPlayable: content_id=%d content_type=%v region=%s err=%v",
			in.ContentId, in.ContentType, in.Region, err)
		return nil, err
	}
	return &rpc.CheckReply{
		Playable: playable,
		WindowId: windowID,
		EndTime:  endTime,
	}, nil
}
