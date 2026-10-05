package repository

// 本文件是 playback 服务对 rights 服务的 gRPC 客户端适配器（手写扩展，参考
// services/account/internal/repository/userprofile_client.go 的约定）。
// playback 只通过 rights 的公开 RPC 契约校验版权窗口（AGENTS.md §5），
// 不 import rights 的内部 model 包，也不直连它的 MySQL 表。

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/zrpc"

	"go-video/services/playback/model"
	rightsrpc "go-video/services/rights/rpc"
)

// RightsChecker 抽象版权可播放性校验，便于单元测试替换真实 RPC。
type RightsChecker interface {
	// CheckPlayable 校验内容在指定地区是否处于有效版权窗口。
	// 返回 (可播放, 窗口结束时间 Unix 秒, 错误)；错误表示判定不可得，调用方必须拒绝签发。
	CheckPlayable(ctx context.Context, contentId int64, contentType int32, region string) (bool, int64, error)
}

// rightsClient 通过 zrpc 调用 rights 服务的 Rights.CheckPlayable。
type rightsClient struct {
	cli rightsrpc.RightsClient
}

// NewRightsClient 构造 rights 客户端适配器。
func NewRightsClient(c zrpc.RpcClientConf) RightsChecker {
	conn := zrpc.MustNewClient(c)
	return &rightsClient{cli: rightsrpc.NewRightsClient(conn.Conn())}
}

// CheckPlayable 调用 rights 的 CheckPlayable RPC。
// RPC 失败不降级为"可播放"——播放授权失败默认拒绝（services/playback/README.md）。
func (c *rightsClient) CheckPlayable(ctx context.Context, contentId int64, contentType int32, region string) (bool, int64, error) {
	ct, err := toRightsContentType(contentType)
	if err != nil {
		return false, 0, err
	}
	reply, err := c.cli.CheckPlayable(ctx, &rightsrpc.CheckReq{
		ContentId:   contentId,
		ContentType: ct,
		Region:      region,
	})
	if err != nil {
		return false, 0, fmt.Errorf("playback/rights CheckPlayable content_id=%d: %w", contentId, err)
	}
	if reply == nil {
		return false, 0, model.ErrRightsUnavailable
	}
	return reply.GetPlayable(), reply.GetEndTime(), nil
}

// toRightsContentType 把 playback 的内容类型映射为 rights 契约的枚举值。
//
// 两个服务的编号不同，禁止 int32 直转（有单元测试锁定该映射）：
//
//	playback: 1=UGC、2=PGC
//	rights  : 1=PGC、2=UGC
func toRightsContentType(contentType int32) (rightsrpc.ContentType, error) {
	switch contentType {
	case model.ContentTypeUGC:
		return rightsrpc.ContentType_CONTENT_TYPE_UGC, nil
	case model.ContentTypePGC:
		return rightsrpc.ContentType_CONTENT_TYPE_PGC, nil
	default:
		return rightsrpc.ContentType_CONTENT_TYPE_UNSPECIFIED, model.ErrInvalidContentType
	}
}

// rightsChecker 是 rights 客户端未配置时的显式失败实现。
// 用它而不是 nil 判断，保证 PGC 签发一定返回明确错误而不是伪造成功。
type unavailableRights struct{}

// NewUnavailableRights 返回始终报"rights 服务不可用"的校验器。
func NewUnavailableRights() RightsChecker { return unavailableRights{} }

func (unavailableRights) CheckPlayable(context.Context, int64, int32, string) (bool, int64, error) {
	return false, 0, model.ErrRightsUnavailable
}
