package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PublishEpisodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPublishEpisodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PublishEpisodeLogic {
	return &PublishEpisodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// PublishEpisode 上架集：状态流转到 PUBLISHED（EpStateOnline）。
//
// 依据 AGENTS.md §8，上架按两步推进，两步都通过才写状态：
//  1. 状态机：仅草稿/下架可上架；已上架幂等返回；其它来源态返回 ErrInvalidStateTrans。
//  2. 跨服务前置校验（guard.precheckPublish，见 internal/logic/guard.go）：
//     - asset RPC：集关联的媒资必须存在且已 TRANSCODED（上传完成不代表可播放）；
//     - rights RPC：以 content_id=epid、content_type=PGC、region=请求地区（缺省回落到
//     配置 DefaultRegion）调用 CheckPlayable，窗口无效返回 ErrRightsWindowClosed。
//     校验器不可用（客户端未配置或下游故障）时默认拒绝上架，
//     只有 DisableRightsCheck/DisableAssetCheck 显式置 true 才跳过并打 Error 日志。
//
// 审计：operator_mid 与 region 随每次状态推进落日志（AGENTS.md §8 要求保留证据）。
// 上游未透传 operator_mid 时仍放行但记为 0，属 gateway/admin 的契约缺口（见 README）。
func (l *PublishEpisodeLogic) PublishEpisode(in *rpc.EpisodeReq) (*rpc.EpisodeReply, error) {
	if in.Epid <= 0 {
		return nil, model.ErrInvalidEpid
	}
	e, err := l.svcCtx.Repository.GetEpisode(l.ctx, in.Epid)
	if err != nil {
		l.Errorf("catalog/PublishEpisode get: epid=%d err=%v", in.Epid, err)
		return nil, err
	}
	if e == nil {
		return nil, model.ErrEpisodeNotFound
	}
	// 幂等：已上架直接返回，不重复校验、不重复推进。
	// 窗口过期不由「重复调用上架」来收回：过期/撤权由 rights 侧推进，
	// 再由 cron 或运营调用 OfflineEpisode 下架（AGENTS.md §8）。
	if e.State == model.EpStateOnline {
		return toEpisodeReply(e), nil
	}
	// 状态机：非法来源态直接拒绝，避免跳过校验把内容推到 PUBLISHED。
	if !canTransitionEpisode(e.State, model.EpStateOnline) {
		l.Errorf("catalog/PublishEpisode illegal transition: epid=%d from=%d operator_mid=%d",
			in.Epid, e.State, in.OperatorMid)
		return nil, model.ErrInvalidStateTrans
	}
	if in.OperatorMid <= 0 {
		// 契约缺口：gateway/admin 目前只传 epid（见 README），operator_mid 为 0 时
		// 审计证据不完整，但地区维度的版权校验仍然生效，不因缺操作人就放行或拒绝。
		l.Errorf("catalog/PublishEpisode audit: operator_mid 未透传，epid=%d region=%q 的上架证据不完整",
			in.Epid, in.Region)
	}
	region, err := newGuard(l.svcCtx, l).precheckPublish(l.ctx, e, in.Region)
	if err != nil {
		l.Errorf("catalog/PublishEpisode precheck rejected: epid=%d asset_id=%d season_id=%d region=%q operator_mid=%d err=%v",
			in.Epid, e.AssetID, e.SeasonID, in.Region, in.OperatorMid, err)
		return nil, err
	}
	if err := l.svcCtx.Repository.UpdateEpisodeState(l.ctx, in.Epid, model.EpStateOnline); err != nil {
		l.Errorf("catalog/PublishEpisode update: epid=%d err=%v", in.Epid, err)
		return nil, err
	}
	l.Infof("catalog/PublishEpisode published: epid=%d season_id=%d ep_no=%d asset_id=%d region=%s operator_mid=%d",
		in.Epid, e.SeasonID, e.EpNo, e.AssetID, region, in.OperatorMid)
	e.State = model.EpStateOnline
	return toEpisodeReply(e), nil
}

func toEpisodeReply(e *model.Episode) *rpc.EpisodeReply {
	return &rpc.EpisodeReply{
		Epid:     e.Epid,
		SeasonId: e.SeasonID,
		EpNo:     e.EpNo,
		Title:    e.Title,
		AssetId:  e.AssetID,
		Duration: e.Duration,
		State:    e.State,
	}
}
