package model

import "errors"

// catalog 域错误。
var (
	ErrInvalidSeasonID   = errors.New("catalog: invalid season_id")
	ErrInvalidWorkID     = errors.New("catalog: invalid work_id")
	ErrInvalidEpid       = errors.New("catalog: invalid epid")
	ErrInvalidType       = errors.New("catalog: invalid typeid")
	ErrInvalidTitle      = errors.New("catalog: title is empty")
	ErrInvalidSeasonNo   = errors.New("catalog: invalid season_no")
	ErrInvalidEpNo       = errors.New("catalog: invalid ep_no")
	ErrInvalidPs         = errors.New("catalog: ps exceeds 50")
	ErrTooManyTagIDs     = errors.New("catalog: tagids count exceeds 100")
	ErrWorkNotFound      = errors.New("catalog: work not found")
	ErrSeasonNotFound    = errors.New("catalog: season not found")
	ErrEpisodeNotFound   = errors.New("catalog: episode not found")
	ErrInvalidStateTrans = errors.New("catalog: invalid state transition")
	ErrInvalidAssetID    = errors.New("catalog: invalid asset_id")

	// 跨服务前置校验错误（AGENTS.md §5/§8）。
	// 这些是稳定业务错误：调用方（gateway/admin）可据此给出确定性提示。

	// ErrAssetNotFound 关联媒资不存在（asset 服务确认查无此 asset_id）。
	ErrAssetNotFound = errors.New("catalog: asset not found")
	// ErrAssetNotReady 媒资状态不满足当前阶段：建集需已过 SCANNED，上架需 TRANSCODED。
	ErrAssetNotReady = errors.New("catalog: asset not ready")
	// ErrAssetCheckerUnavailable 需要媒资校验但 asset 客户端未配置或调用失败。
	ErrAssetCheckerUnavailable = errors.New("catalog: asset checker unavailable")

	// ErrRightsWindowClosed 版权窗口在该地区不存在、未生效、已过期或已撤权。
	ErrRightsWindowClosed = errors.New("catalog: rights window not granted")
	// ErrRightsCheckerUnavailable 需要版权校验但 rights 客户端未配置或调用失败。
	ErrRightsCheckerUnavailable = errors.New("catalog: rights checker unavailable")
	// ErrMissingRegion 无法确定上架地区：请求未带 region 且服务未配置 DefaultRegion。
	// 版权窗口按地区授予，地区未知时一律拒绝上架。
	ErrMissingRegion = errors.New("catalog: region is required")
)

// 作品类型常量。
const (
	WorkTypeMovie = 1 // 电影
	WorkTypeDrama = 2 // 电视剧
	WorkTypeAnima = 3 // 番剧
	WorkTypeDoc   = 4 // 纪录片
)

// 作品/季状态常量。
const (
	StateDraft   = 0 // 草稿
	StateOnline  = 1 // 上架
	StateOffline = 2 // 下架
)

// 集状态常量。
const (
	EpStateDraft   = 0 // 草稿
	EpStateOnline  = 1 // 上架（PUBLISHED）
	EpStateOffline = 2 // 下架
)
