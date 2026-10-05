package logic

import (
	"context"
	"testing"

	"go-video/services/recommend-rank/internal/svc"
)

// testCtx 返回测试用 context（不接 trace_id：这些用例只验入口拒绝，不产出日志）。
func testCtx() context.Context { return context.Background() }

// newTestServiceContext 构造一个**不触碰任何存储**的 ServiceContext。
//
// 这里刻意不调用 svc.NewServiceContext：它会用 etc yaml 里的 DSN 去 sql.Open MySQL、
// 用 CacheRedis 去连 Redis，而单元测试必须在没有依赖实例的机器上可重复（AGENTS.md §9）。
// 零值上下文正好用来验「依赖缺席时必须失败关闭」这条契约：每个 rpc 方法拿到
// 未接线的 repository/下游都要回错误，而不是回一份看起来成功的空 reply
// （见 contract_consistency_test.go）。需要跑通成功路径的用例另配 fake repository，仍然不连库。
func newTestServiceContext(t *testing.T) *svc.ServiceContext {
	t.Helper()
	return &svc.ServiceContext{}
}
