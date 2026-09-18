// Code scaffolded by goctl. Safe to edit.
// goctl 1.9.2

package config

import (
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 gateway/app 的配置结构。
// 网关只做入口与聚合（AGENTS.md §3）：HTTP 收口在此，领域数据经下游 RPC 获取。
type Config struct {
	rest.RestConf

	// AccountRPC 是 account 服务的 zrpc client 配置（Info/Card/Profile/Vip/DelCache 聚合）。
	AccountRPC zrpc.RpcClientConf

	// UserProfileRPC 是 user-profile 服务的 zrpc client 配置
	// （资料查询/编辑、节操、经验、实名认证聚合）。
	UserProfileRPC zrpc.RpcClientConf

	// PrivacyAppKeys 是 /account/privacy 接口的调用方白名单。
	// 对应参考仓库 conf.AppkeyFilter.Privacy，仅列表内的 appkey 可以查询隐私信息。
	// 为空列表表示不启用 appkey 校验（仅限开发环境）。
	PrivacyAppKeys []string `json:",optional"`

	// PassportRSAPublicKey 登录密码加密的 RSA 公钥（PEM 格式）。
	// /x/passport-login/key 返回给客户端用于加密登录/注册密码；
	// 必须与 account 服务配置的 PassportRSA.PrivateKey 配对。
	// 留空表示未配置（仅限开发环境，/key 返回明确错误）。
	PassportRSAPublicKey string `json:",optional"`
}
