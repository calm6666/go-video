// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/search-query/internal/config"
	"go-video/services/search-query/internal/esclient"
	"go-video/services/search-query/internal/repository"
)

// ServiceContext 是 search-query 服务的运行时上下文。
type ServiceContext struct {
	Config config.Config

	// Engine 是 OpenSearch 只读客户端；nil 表示未配置，查询接口按降级语义返回错误。
	Engine *esclient.Client

	// Repository 汇总引擎、Redis 与 MySQL 访问。
	Repository *repository.Repository
}

// NewServiceContext 构造 ServiceContext。
//
// 注意：Redis/MySQL 客户端按 go-zero 约定在此创建（Redis 非 NonBlock 时会做连通性检查），
// 引擎客户端只做配置校验，不发起任何请求；索引是否可用由首次查询时的错误体现。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)

	var eng *esclient.Client
	cli, err := esclient.New(esclient.Options{
		Endpoints:       c.OpenSearch.Endpoints,
		Alias:           c.OpenSearch.Alias,
		Username:        c.OpenSearch.Username,
		Password:        c.OpenSearch.Password,
		Timeout:         time.Duration(c.OpenSearch.TimeoutMs) * time.Millisecond,
		MaxResultWindow: c.OpenSearch.MaxResultWindow,
		BreakerName:     fmt.Sprintf("search-query:%s", c.OpenSearch.Alias),
	})
	switch {
	case err == nil:
		eng = cli
		logx.Infof("search-query: opensearch enabled alias=%s endpoints=%d window=%d",
			cli.Alias(), len(c.OpenSearch.Endpoints), cli.MaxResultWindow())
	case errors.Is(err, esclient.ErrNotConfigured):
		// 诚实降级：不伪造引擎可用性，Search 会返回 ErrSearchUnavailable。
		logx.Errorf("search-query: opensearch not configured (endpoints/alias empty), search will degrade")
	default:
		logx.Severe("search-query: invalid opensearch config: ", err)
	}

	return &ServiceContext{
		Config:     c,
		Engine:     eng,
		Repository: repository.New(rds, conn, eng, c),
	}
}
