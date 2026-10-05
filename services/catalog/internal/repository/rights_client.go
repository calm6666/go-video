package repository

// 本文件是 catalog 服务对 rights 服务的 gRPC 客户端适配器。
// catalog 只通过 rights 的公开 RPC 契约读取窗口结论（AGENTS.md §5），
// 不引入 rights 的内部 model 包，也不写 rights_window 表。

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/zrpc"

	rightsrpc "go-video/services/rights/rpc"
)

// rightsClient 通过 zrpc 调用 rights 服务，实现 RightsClient 接口。
type rightsClient struct {
	cli rightsrpc.RightsClient
}

// NewRightsClient 构造 rights 客户端适配器。
// 配置无效时 zrpc.MustNewClient 会 panic，因此调用方（ServiceContext）需先确认已配置。
func NewRightsClient(c zrpc.RpcClientConf) RightsClient {
	conn := zrpc.MustNewClient(c)
	return &rightsClient{cli: rightsrpc.NewRightsClient(conn.Conn())}
}

// CheckPlayable 校验内容在某地区是否处于有效版权窗口。
// content_type 固定由调用方给出：catalog 的集属于 PGC（AGENTS.md §1）。
func (c *rightsClient) CheckPlayable(ctx context.Context, contentID int64, contentType RightsContentType, region string) (*RightsCheckResult, error) {
	reply, err := c.cli.CheckPlayable(ctx, &rightsrpc.CheckReq{
		ContentId:   contentID,
		ContentType: rightsrpc.ContentType(contentType),
		Region:      region,
	})
	if err != nil {
		return nil, fmt.Errorf("rights CheckPlayable(content_id=%d,type=%d,region=%s): %w",
			contentID, contentType, region, err)
	}
	if reply == nil {
		// 空回复按“无法确认窗口有效”处理，交由调用方按严格策略拒绝上架。
		return nil, fmt.Errorf("rights CheckPlayable(content_id=%d,region=%s): empty reply", contentID, region)
	}
	return &RightsCheckResult{
		Playable: reply.Playable,
		WindowID: reply.WindowId,
		EndTime:  reply.EndTime,
	}, nil
}

// 编译期断言：适配器实现了接口。
var _ RightsClient = (*rightsClient)(nil)
