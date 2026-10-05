package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OfflineEpisodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewOfflineEpisodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OfflineEpisodeLogic {
	return &OfflineEpisodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// OfflineEpisode 下架集：状态流转到下架（EpStateOffline）。
// 合法状态机：仅已上架可下架；已下架幂等返回（见 statemachine.go）。
// 下架应同步失效 CDN/搜索/推荐投影（待各下游接入，见 README「后续工作」）。
// 下架是版权撤回的常见路径，按 AGENTS.md §8 记录操作人与地区作为审计证据。
func (l *OfflineEpisodeLogic) OfflineEpisode(in *rpc.EpisodeReq) (*rpc.EpisodeReply, error) {
	if in.Epid <= 0 {
		return nil, model.ErrInvalidEpid
	}
	e, err := l.svcCtx.Repository.GetEpisode(l.ctx, in.Epid)
	if err != nil {
		l.Errorf("catalog/OfflineEpisode get: epid=%d err=%v", in.Epid, err)
		return nil, err
	}
	if e == nil {
		return nil, model.ErrEpisodeNotFound
	}
	if e.State == model.EpStateOffline {
		return toEpisodeReply(e), nil
	}
	if !canTransitionEpisode(e.State, model.EpStateOffline) {
		l.Errorf("catalog/OfflineEpisode illegal transition: epid=%d from=%d operator_mid=%d",
			in.Epid, e.State, in.OperatorMid)
		return nil, model.ErrInvalidStateTrans
	}
	if err := l.svcCtx.Repository.UpdateEpisodeState(l.ctx, in.Epid, model.EpStateOffline); err != nil {
		l.Errorf("catalog/OfflineEpisode update: epid=%d err=%v", in.Epid, err)
		return nil, err
	}
	l.Infof("catalog/OfflineEpisode offline: epid=%d season_id=%d asset_id=%d region=%q operator_mid=%d",
		in.Epid, e.SeasonID, e.AssetID, in.Region, in.OperatorMid)
	e.State = model.EpStateOffline
	return toEpisodeReply(e), nil
}
