package repository

// 本文件是 user-profile 服务对 account 服务的 gRPC 客户端适配器（缓存失效方向）。
// 领域微服务之间只使用 RPC 通信（AGENTS.md §3/§5）：user-profile 的资料更新事件
// 通过 account 的 DelCache RPC 失效 account 侧 Info/Card/Profile/Vip 缓存，
// 替代参考仓库 databus 的 MemberService-AccountNotify 主题投递。
// 注意：account 的 UserProfileClient 是反向依赖（account 聚合查询 user-profile），
// 两条 RPC 职责单一、方向不同，属正常服务间契约；后续接入消息总线后
// user.profile.updated 可改为事件驱动消费，本客户端可平滑下线。

import (
	"context"

	"github.com/zeromicro/go-zero/zrpc"

	accountrpc "go-video/services/account/rpc"
)

// AccountCacheClient 抽象 account 的缓存失效 RPC，便于测试替换与后续切换事件驱动。
type AccountCacheClient interface {
	// DelCache 通知 account 失效指定用户的 Info/Card/Profile/Vip 缓存。
	// action=updateVip 时 account 会额外触发延迟二次失效。
	DelCache(ctx context.Context, mid int64, action string) error
}

type accountCacheClient struct {
	cli accountrpc.AccountClient
}

// NewAccountCacheClient 构造 account 缓存失效客户端。
// 配置为空（未部署 account）时返回 nil，Outbox 发布器对相关事件退避重试。
func NewAccountCacheClient(c zrpc.RpcClientConf) AccountCacheClient {
	if c.Target == "" && len(c.Etcd.Hosts) == 0 {
		return nil
	}
	conn := zrpc.MustNewClient(c)
	return &accountCacheClient{cli: accountrpc.NewAccountClient(conn.Conn())}
}

// DelCache 调用 account 的 DelCache RPC。
func (c *accountCacheClient) DelCache(ctx context.Context, mid int64, action string) error {
	_, err := c.cli.DelCache(ctx, &accountrpc.DelCacheReq{Mid: mid, Action: action})
	return err
}
