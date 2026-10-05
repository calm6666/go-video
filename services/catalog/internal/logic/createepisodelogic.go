package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateEpisodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateEpisodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateEpisodeLogic {
	return &CreateEpisodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateEpisode 运营创建集（关联 asset_id）。
// 集初始状态为草稿；上架需通过 PublishEpisode 推进并再次校验媒资与版权窗口。
//
// 依据 AGENTS.md §5/§8，建集前用 asset RPC 校验 asset_id 存在且媒资已完成
// 文件扫描与探测（SCANNED 或 TRANSCODED）：
//   - 媒资不存在 → ErrAssetNotFound；
//   - 仍处 UPLOADED 或已 FAILED → ErrAssetNotReady；
//   - asset 客户端未配置或调用失败 → ErrAssetCheckerUnavailable（默认严格拒绝）。
//
// 只有显式设置 DisableAssetCheck: true（灰度/回滚）才跳过该校验，见 internal/logic/guard.go。
func (l *CreateEpisodeLogic) CreateEpisode(in *rpc.CreateEpisodeReq) (*rpc.EpisodeReply, error) {
	if in.SeasonId <= 0 {
		return nil, model.ErrInvalidSeasonID
	}
	if in.EpNo <= 0 {
		return nil, model.ErrInvalidEpNo
	}
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	if in.Operator == "" {
		// 建集是运营写操作，缺操作人时审计证据不完整；不阻塞写入，但按 Error 级记录。
		l.Errorf("catalog/CreateEpisode audit: operator 未透传，season_id=%d ep_no=%d asset_id=%d",
			in.SeasonId, in.EpNo, in.AssetId)
	}
	if err := newGuard(l.svcCtx, l).assetBindable(l.ctx, in.AssetId); err != nil {
		l.Errorf("catalog/CreateEpisode precheck rejected: season_id=%d ep_no=%d asset_id=%d operator=%s err=%v",
			in.SeasonId, in.EpNo, in.AssetId, in.Operator, err)
		return nil, err
	}
	e := &model.Episode{
		SeasonID: in.SeasonId,
		EpNo:     in.EpNo,
		Title:    in.Title,
		AssetID:  in.AssetId,
		Duration: in.Duration,
		State:    model.EpStateDraft,
	}
	id, err := l.svcCtx.Repository.CreateEpisode(l.ctx, e)
	if err != nil {
		l.Errorf("catalog/CreateEpisode: season_id=%d ep_no=%d asset_id=%d operator=%s err=%v",
			in.SeasonId, in.EpNo, in.AssetId, in.Operator, err)
		return nil, err
	}
	return &rpc.EpisodeReply{
		Epid:     id,
		SeasonId: e.SeasonID,
		EpNo:     e.EpNo,
		Title:    e.Title,
		AssetId:  e.AssetID,
		Duration: e.Duration,
		State:    e.State,
	}, nil
}
