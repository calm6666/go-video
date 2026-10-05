package repository

// 本文件声明 catalog 对 rights / asset 两个下游服务的**只读**客户端契约。
// 依据 AGENTS.md §5：catalog 只能经版本化 RPC 访问跨域数据，
// 不直连 rights/asset 的 MySQL 表或 Redis key，也不 import 其内部 model 包；
// 这里的结构体只复制 catalog 用例需要的少量字段（本地只读视图）。
// 依据 AGENTS.md §8：版权内容上架前必须校验窗口有效，媒资必须完成扫描/转码，
// 因此这些客户端是 PublishEpisode / CreateEpisode 的前置条件，不是可选优化。
//
// 接口化是为了让 logic 层可以注入 fake（见 internal/logic/guard_test.go），
// 单元测试不需要真实 gRPC 连接。

import (
	"context"
)

// RightsContentType 镜像 rights 契约 rights.v1.ContentType 的取值。
// 按 §5 不 import rights 内部 model，只在适配器里转换为其 rpc 枚举。
type RightsContentType int32

// rights 内容类型（与 services/rights/rpc/rights.proto 的 ContentType 一致）。
const (
	RightsContentTypeUnspecified RightsContentType = 0
	RightsContentTypePGC         RightsContentType = 1 // 版权内容（电影/电视剧/番剧/纪录片）
	RightsContentTypeUGC         RightsContentType = 2 // UGC 稿件
)

// AssetState 镜像 asset 契约 asset.v1.AssetState 的取值。
// 与 services/asset/rpc/asset.proto 的枚举保持一致。
type AssetState int32

// asset 媒资状态（UPLOADED → SCANNED → TRANSCODED，FAILED 为终态失败）。
const (
	AssetStateUnspecified AssetState = 0
	AssetStateUploaded    AssetState = 1 // 上传完成，待扫描
	AssetStateScanned     AssetState = 2 // 文件扫描与探测完成
	AssetStateTranscoded  AssetState = 3 // 转码完成，可投入发布
	AssetStateFailed      AssetState = 4 // 处理失败
)

// String 返回媒资状态的可读名，用于日志与错误信息。
func (s AssetState) String() string {
	switch s {
	case AssetStateUploaded:
		return "UPLOADED"
	case AssetStateScanned:
		return "SCANNED"
	case AssetStateTranscoded:
		return "TRANSCODED"
	case AssetStateFailed:
		return "FAILED"
	default:
		return "UNSPECIFIED"
	}
}

// RightsCheckResult 是 rights 可播放性校验的只读结论。
type RightsCheckResult struct {
	// Playable 表示该内容在指定地区当前处于有效版权窗口内。
	Playable bool
	// WindowID 命中的窗口 ID；不可播放时为 0。仅用于日志与审计。
	WindowID int64
	// EndTime 命中窗口的结束时间（Unix 秒），可作为下游缓存 TTL 上限。
	EndTime int64
}

// AssetMeta 是 asset 媒资的只读视图，只包含 catalog 用例需要的字段。
type AssetMeta struct {
	AssetID  int64
	Duration int64 // 毫秒（asset 侧单位），catalog 集时长为秒，两者不互换
	State    AssetState
}

// RightsClient 是 catalog 对 rights 服务的只读视图。
type RightsClient interface {
	// CheckPlayable 校验内容在指定地区是否处于有效版权窗口。
	// contentID 语义由调用方约定：catalog 以集 epid 作为 PGC 内容 ID。
	// 传输或下游内部错误应原样返回 error，由调用方按严格策略处理。
	CheckPlayable(ctx context.Context, contentID int64, contentType RightsContentType, region string) (*RightsCheckResult, error)
}

// AssetClient 是 catalog 对 asset 服务的只读视图。
type AssetClient interface {
	// GetAsset 查询媒资元数据。
	// 媒资不存在时返回包装了 ErrAssetNotFound 的 error（asset 契约用错误而非空回复表达不存在）。
	GetAsset(ctx context.Context, assetID int64) (*AssetMeta, error)
}
