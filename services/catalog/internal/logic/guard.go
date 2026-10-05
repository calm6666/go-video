package logic

// 本文件封装 catalog 上架/建集流程的跨服务前置校验（AGENTS.md §5/§8）。
// 校验通过注入的 rights / asset 只读客户端完成，因此单元测试可以用 fake 替换，
// 不依赖真实 gRPC 连接。
//
// 默认策略是「严格」：需要校验但客户端未配置、或下游调用失败时，
// 返回明确错误并拒绝上架/建集。理由：版权内容误上架会造成越权播放与合规事故，
// 代价远高于一次误拒绝；运营可以在 rights/asset 恢复后重试。
// 仅在 rights/asset 故障演练或灰度回滚时才允许用 DisableRightsCheck /
// DisableAssetCheck 显式跳过；跳过按 Error 级日志记录，因为「未经版权/媒资校验就上架」
// 本身就是需要进告警的合规风险事件（AGENTS.md §4 日志要求、§8 审计要求）。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/catalog/internal/repository"
	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
)

// guard 是集状态推进的跨服务前置校验器。
type guard struct {
	log           logx.Logger
	rights        repository.RightsClient
	asset         repository.AssetClient
	enforceRights bool
	enforceAsset  bool
	defaultRegion string
}

// newGuard 从 ServiceContext 构造 guard。
// 客户端为 nil 不代表不校验：是否放行由 Config 的 Disable* 开关决定，
// 默认（开关为 false）时 nil 客户端会触发「校验器不可用」错误。
func newGuard(svcCtx *svc.ServiceContext, logger logx.Logger) *guard {
	return &guard{
		log:           logger,
		rights:        svcCtx.Rights,
		asset:         svcCtx.Asset,
		enforceRights: !svcCtx.Config.DisableRightsCheck,
		enforceAsset:  !svcCtx.Config.DisableAssetCheck,
		defaultRegion: svcCtx.Config.DefaultRegion,
	}
}

// rightsContentID 返回集在 rights 域中的内容 ID。
// 契约缺口：rights.Window.content_id 只写了「catalog 集/作品等」，未固化粒度，
// 本服务约定 content_id = catalog_episode.epid、content_type = PGC（见 README）。
func rightsContentID(epid int64) int64 { return epid }

// resolveRegion 解析上架地区代码：请求优先，其次配置 DefaultRegion。
// 两者都为空时返回 ErrMissingRegion —— 版权窗口按地区授予，地区未知不能放行。
func (g *guard) resolveRegion(requested string) (string, error) {
	if r := strings.TrimSpace(requested); r != "" {
		return r, nil
	}
	if r := strings.TrimSpace(g.defaultRegion); r != "" {
		g.log.Infof("catalog/guard: region 未随请求传入，使用配置 DefaultRegion=%s", r)
		return r, nil
	}
	return "", model.ErrMissingRegion
}

// rightsWindow 校验集在 region 内是否处于有效版权窗口。
// 返回值语义：
//   - nil：窗口有效，或校验被 DisableRightsCheck 显式关闭；
//   - ErrRightsWindowClosed：rights 明确回答不可播放（无窗口/未生效/已过期/已撤权）；
//   - ErrRightsCheckerUnavailable：客户端未配置或下游调用失败（默认严格拒绝）。
func (g *guard) rightsWindow(ctx context.Context, epid int64, region string) error {
	contentID := rightsContentID(epid)
	if !g.enforceRights {
		g.log.Errorf("catalog/guard: 版权窗口校验已被 DisableRightsCheck 关闭，epid=%d region=%s 未经窗口校验即放行", epid, region)
		return nil
	}
	if g.rights == nil {
		return fmt.Errorf("epid=%d region=%s rights 客户端未配置: %w", epid, region, model.ErrRightsCheckerUnavailable)
	}
	res, err := g.rights.CheckPlayable(ctx, contentID, repository.RightsContentTypePGC, region)
	if err != nil {
		return fmt.Errorf("epid=%d region=%s 版权校验失败: %w: %w", epid, region, model.ErrRightsCheckerUnavailable, err)
	}
	if res == nil || !res.Playable {
		return fmt.Errorf("epid=%d region=%s: %w", epid, region, model.ErrRightsWindowClosed)
	}
	g.log.Infof("catalog/guard: 版权窗口有效 epid=%d region=%s window_id=%d end_time=%d",
		epid, region, res.WindowID, res.EndTime)
	return nil
}

// assetBindable 建集前置：媒资必须存在且已完成扫描与探测（SCANNED 或 TRANSCODED）。
func (g *guard) assetBindable(ctx context.Context, assetID int64) error {
	return g.checkAsset(ctx, assetID, "create", repository.AssetStateScanned)
}

// precheckPublish 上架前置校验：媒资已转码就绪 + 版权窗口有效（含地区解析）。
// 返回实际生效的地区代码（供审计日志使用）。
// 任一项不通过都返回稳定的 catalog 域错误，调用方不需要解析下游错误文本。
// 校验顺序先媒资后版权：媒资缺失时地区解析失败的噪音会掩盖真实原因。
func (g *guard) precheckPublish(ctx context.Context, e *model.Episode, requestedRegion string) (string, error) {
	if err := g.assetPublishable(ctx, e.AssetID); err != nil {
		return "", err
	}
	region := requestedRegion
	if g.enforceRights {
		resolved, err := g.resolveRegion(requestedRegion)
		if err != nil {
			return "", err
		}
		region = resolved
	}
	if err := g.rightsWindow(ctx, e.Epid, region); err != nil {
		return region, err
	}
	return region, nil
}

// assetPublishable 上架前置：媒资必须已转码完成（TRANSCODED），
// 依据 AGENTS.md §8「上传完成不代表可播放」。
func (g *guard) assetPublishable(ctx context.Context, assetID int64) error {
	return g.checkAsset(ctx, assetID, "publish", repository.AssetStateTranscoded)
}

// checkAsset 校验媒资状态不低于 minState，且不处于 FAILED。
// 返回值语义与 rightsWindow 对应：ErrAssetNotFound / ErrAssetNotReady /
// ErrAssetCheckerUnavailable，或在 DisableAssetCheck 打开时放行。
func (g *guard) checkAsset(ctx context.Context, assetID int64, stage string, minState repository.AssetState) error {
	if !g.enforceAsset {
		g.log.Errorf("catalog/guard: 媒资校验已被 DisableAssetCheck 关闭，asset_id=%d stage=%s 未经就绪校验即放行", assetID, stage)
		return nil
	}
	if g.asset == nil {
		return fmt.Errorf("asset_id=%d stage=%s asset 客户端未配置: %w", assetID, stage, model.ErrAssetCheckerUnavailable)
	}
	meta, err := g.asset.GetAsset(ctx, assetID)
	if err != nil {
		if errors.Is(err, model.ErrAssetNotFound) {
			return fmt.Errorf("asset_id=%d stage=%s: %w", assetID, stage, model.ErrAssetNotFound)
		}
		return fmt.Errorf("asset_id=%d stage=%s 媒资校验失败: %w: %w", assetID, stage, model.ErrAssetCheckerUnavailable, err)
	}
	if meta == nil {
		// 下游/适配器返回 (nil, nil) 时按“无法确认媒资存在”拒绝，不放行。
		return fmt.Errorf("asset_id=%d stage=%s 媒资回复为空: %w", assetID, stage, model.ErrAssetNotFound)
	}
	if meta.State == repository.AssetStateFailed {
		return fmt.Errorf("asset_id=%d stage=%s state=%s: %w", assetID, stage, meta.State, model.ErrAssetNotReady)
	}
	if meta.State < minState {
		return fmt.Errorf("asset_id=%d stage=%s state=%s 未达 %s: %w",
			assetID, stage, meta.State, minState, model.ErrAssetNotReady)
	}
	g.log.Infof("catalog/guard: 媒资校验通过 asset_id=%d stage=%s state=%s", assetID, stage, meta.State)
	return nil
}
