// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"slices"
	"strings"

	"go-video/services/payment/internal/config"
	"go-video/services/payment/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 payment 服务的运行时上下文，承载跨请求共享的依赖。
//
// 装配原则（AGENTS.md §4、§5）：
//   - 资金台账只有 MySQL 一个事实源，本服务不写 outbox、不发 MQ、不接任何渠道 SDK；
//   - 不构造 Redis 客户端：余额判定、幂等重放都必须读穿到 MySQL，
//     缓存只可能带来「读到旧余额→重复入账」这类最坏形态，等真有读侧热点再评审；
//   - 配置里出现无法识别的渠道名只记错误日志并忽略，不猜渠道、不静默降级。
type ServiceContext struct {
	Config config.Config

	// Models 是本服务五张 pm_* 表的数据访问入口，并暴露事务执行器。
	Models *model.Models
}

// NewServiceContext 构造 ServiceContext。
func NewServiceContext(c config.Config) *ServiceContext {
	logChannelConfig(c.Payment)

	return &ServiceContext{
		Config: c,
		Models: model.NewModels(sqlx.NewMysql(c.DataSource)),
	}
}

// logChannelConfig 把「只允许哪些渠道」说成启动期可见的事实：
// 渠道白名单为空时按默认口径只放行 SANDBOX，拼错的渠道名一律忽略并告警。
func logChannelConfig(conf config.PaymentConf) {
	known := config.KnownChannels()
	if len(conf.AllowedChannels) == 0 {
		logx.Errorf("payment/svc: Payment.AllowedChannels 未配置，按默认口径只放行 %s（沙箱台账，无真实资金）",
			strings.Join(known, "/"))
		return
	}
	for _, name := range conf.AllowedChannels {
		upper := strings.ToUpper(strings.TrimSpace(name))
		if !slices.Contains(known, upper) {
			logx.Errorf("payment/svc: Payment.AllowedChannels 含无法识别的渠道 %q，已忽略；本项目只支持 %s",
				name, strings.Join(known, "/"))
		}
	}
}
