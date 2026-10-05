package repository

// 本文件是 catalog 服务对 asset 服务的 gRPC 客户端适配器。
// catalog 只通过 asset 的公开 RPC 契约读取媒资状态（AGENTS.md §5），
// 不引入 asset 的内部 model 包，也不写 asset_meta 表。

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/zrpc"

	assetrpc "go-video/services/asset/rpc"
	"go-video/services/catalog/model"
)

// assetNotFoundMarker 是 asset 服务 ErrAssetNotFound 的错误文本片段。
// asset 用 error（而非 found=false 字段）表达“媒资不存在”，跨 gRPC 后只剩
// Unknown + 文案，因此这里按文案识别；契约缺口已记录在 README 与交付报告。
const assetNotFoundMarker = "asset: not found"

// assetClient 通过 zrpc 调用 asset 服务，实现 AssetClient 接口。
type assetClient struct {
	cli assetrpc.AssetClient
}

// NewAssetClient 构造 asset 客户端适配器。
// 配置无效时 zrpc.MustNewClient 会 panic，因此调用方（ServiceContext）需先确认已配置。
func NewAssetClient(c zrpc.RpcClientConf) AssetClient {
	conn := zrpc.MustNewClient(c)
	return &assetClient{cli: assetrpc.NewAssetClient(conn.Conn())}
}

// GetAsset 查询媒资元数据，只保留 catalog 需要的字段。
// 媒资不存在时返回 model.ErrAssetNotFound，便于上层区分“不存在”和“下游不可用”。
func (c *assetClient) GetAsset(ctx context.Context, assetID int64) (*AssetMeta, error) {
	reply, err := c.cli.GetAsset(ctx, &assetrpc.AssetReq{AssetId: assetID})
	if err != nil {
		if strings.Contains(err.Error(), assetNotFoundMarker) {
			return nil, fmt.Errorf("asset %d: %w", assetID, model.ErrAssetNotFound)
		}
		return nil, fmt.Errorf("asset GetAsset(asset_id=%d): %w", assetID, err)
	}
	if reply == nil || reply.AssetId == 0 {
		return nil, fmt.Errorf("asset GetAsset(asset_id=%d): %w", assetID, model.ErrAssetNotFound)
	}
	return &AssetMeta{
		AssetID:  reply.AssetId,
		Duration: reply.Duration,
		State:    AssetState(reply.State),
	}, nil
}

// 编译期断言：适配器实现了接口。
var _ AssetClient = (*assetClient)(nil)
