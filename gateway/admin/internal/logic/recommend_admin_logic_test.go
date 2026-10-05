package logic

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	rankrpc "go-video/services/recommend-rank/rpc"
	recallrpc "go-video/services/recommend-recall/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 recommend-recall / recommend-rank 的口径，与 live/cron 测试同一套边界：
//
//  1. rpc→types **逐字段不丢**（batch_id/generator/schema_version/operator/note/versions_digest/
//     top_aids/过滤计数/降级原因/fallback/event_id 都是排障与审计证据，裁掉一条就等于让后台靠猜）；
//  2. 入参**原样交给下游**：池键、幂等键、批次号、特征 key 顺序（含重复项）都不改写、不去重、不填默认值；
//  3. 写入口门槛：会话身份（缺失即 fail-closed）、operator 只能由会话渲染、幂等/请求键非空（只去空格判空）；
//  4. 未配置下游、请求体缺失、入参形态非法时**不得**产生下游调用，也不得折叠成空结果冒充成功；
//  5. found/has_* 表达「服务有没有回这一条」，空列表投影成 []（非 nil），与「下游没接」严格区分。
//
// 刻意不断言下游业务结论（AGENTS.md §5/§8）：池键语法是否合法、版本能否回滚、keep_versions 是否低于
// MinKeepVersions、objective 是否在受控集合内、权重和是否超过 10、分桶区间是否合法且同层互斥、
// 状态迁移是否允许、页大小如何夹取——全部由 services/recommend-recall、services/recommend-rank 判定。
// 两个服务的 logic 仍在并行落地，这里一律用假客户端返回预置回包，只固定「网关不复算、原样转发、原样回传」。
// 打桩方式与 live/cron 一致：内嵌生成的 client 接口 + 只覆盖本轮用到的方法，不建 gRPC 连接、不碰数据库。

var errRecommendFakeDownstream = errors.New("recommend downstream unavailable")

// --- 假客户端：recommend-recall ---

// recallAdminFake 记录 9 个后台用到的方法的入参，并返回预置回包/错误。
type recallAdminFake struct {
	recallrpc.RecallClient

	err   error
	calls int

	snapshotReq *recallrpc.GetPoolSnapshotReq
	snapshot    *recallrpc.GetPoolSnapshotReply
	versionsReq *recallrpc.ListPoolVersionsReq
	versions    *recallrpc.ListPoolVersionsReply
	configReq   *recallrpc.GetRecallConfigReq
	config      *recallrpc.GetRecallConfigReply
	logReq      *recallrpc.GetRecallRequestLogReq
	log         *recallrpc.GetRecallRequestLogReply
	logsReq     *recallrpc.ListRecallRequestLogsReq
	logs        *recallrpc.ListRecallRequestLogsReply
	itemsReq    *recallrpc.UpsertPoolItemsReq
	items       *recallrpc.UpsertPoolItemsReply
	publishReq  *recallrpc.PublishPoolVersionReq
	publish     *recallrpc.PublishPoolVersionReply
	rollbackReq *recallrpc.RollbackPoolVersionReq
	rollback    *recallrpc.RollbackPoolVersionReply
	pruneReq    *recallrpc.PrunePoolVersionsReq
	prune       *recallrpc.PrunePoolVersionsReply
}

func (f *recallAdminFake) GetPoolSnapshot(_ context.Context, in *recallrpc.GetPoolSnapshotReq,
	_ ...grpc.CallOption) (*recallrpc.GetPoolSnapshotReply, error) {
	f.calls++
	f.snapshotReq = in
	return f.snapshot, f.err
}

func (f *recallAdminFake) ListPoolVersions(_ context.Context, in *recallrpc.ListPoolVersionsReq,
	_ ...grpc.CallOption) (*recallrpc.ListPoolVersionsReply, error) {
	f.calls++
	f.versionsReq = in
	return f.versions, f.err
}

func (f *recallAdminFake) GetRecallConfig(_ context.Context, in *recallrpc.GetRecallConfigReq,
	_ ...grpc.CallOption) (*recallrpc.GetRecallConfigReply, error) {
	f.calls++
	f.configReq = in
	return f.config, f.err
}

func (f *recallAdminFake) GetRecallRequestLog(_ context.Context, in *recallrpc.GetRecallRequestLogReq,
	_ ...grpc.CallOption) (*recallrpc.GetRecallRequestLogReply, error) {
	f.calls++
	f.logReq = in
	return f.log, f.err
}

func (f *recallAdminFake) ListRecallRequestLogs(_ context.Context, in *recallrpc.ListRecallRequestLogsReq,
	_ ...grpc.CallOption) (*recallrpc.ListRecallRequestLogsReply, error) {
	f.calls++
	f.logsReq = in
	return f.logs, f.err
}

func (f *recallAdminFake) UpsertPoolItems(_ context.Context, in *recallrpc.UpsertPoolItemsReq,
	_ ...grpc.CallOption) (*recallrpc.UpsertPoolItemsReply, error) {
	f.calls++
	f.itemsReq = in
	return f.items, f.err
}

func (f *recallAdminFake) PublishPoolVersion(_ context.Context, in *recallrpc.PublishPoolVersionReq,
	_ ...grpc.CallOption) (*recallrpc.PublishPoolVersionReply, error) {
	f.calls++
	f.publishReq = in
	return f.publish, f.err
}

func (f *recallAdminFake) RollbackPoolVersion(_ context.Context, in *recallrpc.RollbackPoolVersionReq,
	_ ...grpc.CallOption) (*recallrpc.RollbackPoolVersionReply, error) {
	f.calls++
	f.rollbackReq = in
	return f.rollback, f.err
}

func (f *recallAdminFake) PrunePoolVersions(_ context.Context, in *recallrpc.PrunePoolVersionsReq,
	_ ...grpc.CallOption) (*recallrpc.PrunePoolVersionsReply, error) {
	f.calls++
	f.pruneReq = in
	return f.prune, f.err
}

// --- 假客户端：recommend-rank ---

// rankAdminFake 记录 8 个后台用到的方法的入参，并返回预置回包/错误。
type rankAdminFake struct {
	rankrpc.RankClient

	err   error
	calls int

	decisionReq  *rankrpc.GetRankDecisionReq
	decision     *rankrpc.GetRankDecisionReply
	decisionsReq *rankrpc.ListRankDecisionsReq
	decisions    *rankrpc.ListRankDecisionsReply
	runtimeReq   *rankrpc.GetRankRuntimeConfigReq
	runtime      *rankrpc.GetRankRuntimeConfigReply
	modelReq     *rankrpc.UpsertModelVersionReq
	model        *rankrpc.UpsertModelVersionReply
	modelState   *rankrpc.SetModelVersionStateReq
	modelStateR  *rankrpc.SetModelVersionStateReply
	featureReq   *rankrpc.UpsertFeatureConfigReq
	feature      *rankrpc.UpsertFeatureConfigReply
	expReq       *rankrpc.UpsertExperimentReq
	exp          *rankrpc.UpsertExperimentReply
	expStateReq  *rankrpc.SetExperimentStateReq
	expState     *rankrpc.SetExperimentStateReply
}

func (f *rankAdminFake) GetRankDecision(_ context.Context, in *rankrpc.GetRankDecisionReq,
	_ ...grpc.CallOption) (*rankrpc.GetRankDecisionReply, error) {
	f.calls++
	f.decisionReq = in
	return f.decision, f.err
}

func (f *rankAdminFake) ListRankDecisions(_ context.Context, in *rankrpc.ListRankDecisionsReq,
	_ ...grpc.CallOption) (*rankrpc.ListRankDecisionsReply, error) {
	f.calls++
	f.decisionsReq = in
	return f.decisions, f.err
}

func (f *rankAdminFake) GetRankRuntimeConfig(_ context.Context, in *rankrpc.GetRankRuntimeConfigReq,
	_ ...grpc.CallOption) (*rankrpc.GetRankRuntimeConfigReply, error) {
	f.calls++
	f.runtimeReq = in
	return f.runtime, f.err
}

func (f *rankAdminFake) UpsertModelVersion(_ context.Context, in *rankrpc.UpsertModelVersionReq,
	_ ...grpc.CallOption) (*rankrpc.UpsertModelVersionReply, error) {
	f.calls++
	f.modelReq = in
	return f.model, f.err
}

func (f *rankAdminFake) SetModelVersionState(_ context.Context, in *rankrpc.SetModelVersionStateReq,
	_ ...grpc.CallOption) (*rankrpc.SetModelVersionStateReply, error) {
	f.calls++
	f.modelState = in
	return f.modelStateR, f.err
}

func (f *rankAdminFake) UpsertFeatureConfig(_ context.Context, in *rankrpc.UpsertFeatureConfigReq,
	_ ...grpc.CallOption) (*rankrpc.UpsertFeatureConfigReply, error) {
	f.calls++
	f.featureReq = in
	return f.feature, f.err
}

func (f *rankAdminFake) UpsertExperiment(_ context.Context, in *rankrpc.UpsertExperimentReq,
	_ ...grpc.CallOption) (*rankrpc.UpsertExperimentReply, error) {
	f.calls++
	f.expReq = in
	return f.exp, f.err
}

func (f *rankAdminFake) SetExperimentState(_ context.Context, in *rankrpc.SetExperimentStateReq,
	_ ...grpc.CallOption) (*rankrpc.SetExperimentStateReply, error) {
	f.calls++
	f.expStateReq = in
	return f.expState, f.err
}

// --- 测试脚手架 ---

// recommendSvc 只填本轮用到的两个客户端，其余字段留零值：logic 不该依赖别的下游。
func recommendSvc(rc recallrpc.RecallClient, rk rankrpc.RankClient) *svc.ServiceContext {
	return &svc.ServiceContext{RecommendRecall: rc, RecommendRank: rk}
}

// recommendSessionCtx 模拟 AdminPermission 中间件已解析出会话身份的请求上下文。
func recommendSessionCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77,
		Roles:   []string{"recommend_ops"},
	})
}

// recommendOperatorFixture 是会话身份渲染出的 operator：77 取自上面的会话，客户端无法声明。
const recommendOperatorFixture = "gateway/admin:77"

// recommendPool 是一个可寻址的池（SOURCE_HOT + global），与 recommendPoolFixture 配套。
func recommendPoolFixture() types.RecommendPoolRef {
	return types.RecommendPoolRef{Source: int32(recallrpc.Source_SOURCE_HOT), PoolKey: "global"}
}

// recommendAssertEnvelope 固定后台面四字段信封：成功恒 code=0/message=ok，
// 且排障读数一律 ttl=0（客户端不该缓存「此刻的下游事实」）。
func recommendAssertEnvelope(t *testing.T, code int, message string, ttl int64) {
	t.Helper()
	if code != 0 || message != "ok" || ttl != 0 {
		t.Fatalf("响应信封不符合后台口径：code=%d message=%q ttl=%d", code, message, ttl)
	}
}

// recommendBlank 把入参结构里名为 field 的字符串字段改成**空白**（不是空串）：
// 门槛按 TrimSpace 判空，用空白才能证明「空白键同样被拒」这条口径。
func recommendBlank(t *testing.T, body any, field string) {
	t.Helper()
	v := reflect.ValueOf(body).Elem().FieldByName(field)
	if !v.IsValid() || v.Kind() != reflect.String {
		t.Fatalf("%T 没有字符串字段 %s", body, field)
	}
	v.SetString("   ")
}

// recommendErr 执行一条路由并返回错误（req 传 nil 即覆盖「请求体缺失」分支）。
type recommendCallFn func(ctx context.Context, s *svc.ServiceContext, req any) error

// recommendRoute 描述一条路由的测试位：域（决定未配置时应得哪个错误）、是否写入口、
// 合法入参构造器与调用位。
type recommendRoute struct {
	name    string
	domain  string // "recall" 或 "rank"
	write   bool
	newBody func() any
	call    recommendCallFn
}

// recommendRoutes 覆盖本轮新增的全部 17 条路由。
func recommendRoutes() []recommendRoute {
	return []recommendRoute{
		{"getPoolSnapshot", "recall", false, func() any { return recommendSnapshotBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendPoolSnapshot
				if req != nil {
					p, _ = req.(*types.ParamRecommendPoolSnapshot)
				}
				_, err := NewGetPoolSnapshotLogic(ctx, s).GetPoolSnapshot(p)
				return err
			}},
		{"listPoolVersions", "recall", false, func() any { return recommendVersionsBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendPoolVersionList
				if req != nil {
					p, _ = req.(*types.ParamRecommendPoolVersionList)
				}
				_, err := NewListPoolVersionsLogic(ctx, s).ListPoolVersions(p)
				return err
			}},
		{"getRecallConfig", "recall", false, func() any { return recommendConfigBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendRecallConfig
				if req != nil {
					p, _ = req.(*types.ParamRecommendRecallConfig)
				}
				_, err := NewGetRecallConfigLogic(ctx, s).GetRecallConfig(p)
				return err
			}},
		{"getRecallRequestLog", "recall", false, func() any { return recommendLogGetBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendRecallLogGet
				if req != nil {
					p, _ = req.(*types.ParamRecommendRecallLogGet)
				}
				_, err := NewGetRecallRequestLogLogic(ctx, s).GetRecallRequestLog(p)
				return err
			}},
		{"listRecallRequestLogs", "recall", false, func() any { return recommendLogListBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendRecallLogList
				if req != nil {
					p, _ = req.(*types.ParamRecommendRecallLogList)
				}
				_, err := NewListRecallRequestLogsLogic(ctx, s).ListRecallRequestLogs(p)
				return err
			}},
		{"getRankDecision", "rank", false, func() any { return recommendDecisionBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRankDecisionGet
				if req != nil {
					p, _ = req.(*types.ParamRankDecisionGet)
				}
				_, err := NewGetRankDecisionLogic(ctx, s).GetRankDecision(p)
				return err
			}},
		{"listRankDecisions", "rank", false, func() any { return recommendDecisionListBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRankDecisionList
				if req != nil {
					p, _ = req.(*types.ParamRankDecisionList)
				}
				_, err := NewListRankDecisionsLogic(ctx, s).ListRankDecisions(p)
				return err
			}},
		{"getRankRuntimeConfig", "rank", false, func() any { return recommendRuntimeBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRankRuntimeConfig
				if req != nil {
					p, _ = req.(*types.ParamRankRuntimeConfig)
				}
				_, err := NewGetRankRuntimeConfigLogic(ctx, s).GetRankRuntimeConfig(p)
				return err
			}},
		{"upsertPoolItems", "recall", true, func() any { return recommendItemsBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendPoolItemUpsert
				if req != nil {
					p, _ = req.(*types.ParamRecommendPoolItemUpsert)
				}
				_, err := NewUpsertPoolItemsLogic(ctx, s).UpsertPoolItems(p)
				return err
			}},
		{"publishPoolVersion", "recall", true, func() any { return recommendPublishBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendPoolVersionPublish
				if req != nil {
					p, _ = req.(*types.ParamRecommendPoolVersionPublish)
				}
				_, err := NewPublishPoolVersionLogic(ctx, s).PublishPoolVersion(p)
				return err
			}},
		{"rollbackPoolVersion", "recall", true, func() any { return recommendRollbackBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendPoolVersionRollback
				if req != nil {
					p, _ = req.(*types.ParamRecommendPoolVersionRollback)
				}
				_, err := NewRollbackPoolVersionLogic(ctx, s).RollbackPoolVersion(p)
				return err
			}},
		{"prunePoolVersions", "recall", true, func() any { return recommendPruneBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRecommendPoolVersionPrune
				if req != nil {
					p, _ = req.(*types.ParamRecommendPoolVersionPrune)
				}
				_, err := NewPrunePoolVersionsLogic(ctx, s).PrunePoolVersions(p)
				return err
			}},
		{"upsertModelVersion", "rank", true, func() any { return recommendModelBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRankModelVersionUpsert
				if req != nil {
					p, _ = req.(*types.ParamRankModelVersionUpsert)
				}
				_, err := NewUpsertModelVersionLogic(ctx, s).UpsertModelVersion(p)
				return err
			}},
		{"setModelVersionState", "rank", true, func() any { return recommendModelStateBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRankModelStateSet
				if req != nil {
					p, _ = req.(*types.ParamRankModelStateSet)
				}
				_, err := NewSetModelVersionStateLogic(ctx, s).SetModelVersionState(p)
				return err
			}},
		{"upsertFeatureConfig", "rank", true, func() any { return recommendFeatureBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRankFeatureConfigUpsert
				if req != nil {
					p, _ = req.(*types.ParamRankFeatureConfigUpsert)
				}
				_, err := NewUpsertFeatureConfigLogic(ctx, s).UpsertFeatureConfig(p)
				return err
			}},
		{"upsertExperiment", "rank", true, func() any { return recommendExperimentBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRankExperimentUpsert
				if req != nil {
					p, _ = req.(*types.ParamRankExperimentUpsert)
				}
				_, err := NewUpsertExperimentLogic(ctx, s).UpsertExperiment(p)
				return err
			}},
		{"setExperimentState", "rank", true, func() any { return recommendExpStateBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamRankExperimentStateSet
				if req != nil {
					p, _ = req.(*types.ParamRankExperimentStateSet)
				}
				_, err := NewSetExperimentStateLogic(ctx, s).SetExperimentState(p)
				return err
			}},
	}
}

// recommendRouteByName 取单条路由定义（缺失即测试写错，直接 fatal）。
func recommendRouteByName(t *testing.T, name string) recommendRoute {
	t.Helper()
	for _, rt := range recommendRoutes() {
		if rt.name == name {
			return rt
		}
	}
	t.Fatalf("路由 %s 未登记在 recommendRoutes()", name)
	return recommendRoute{}
}

// --- 合法入参（每条路由一份，既做 happy 路径也做门槛用例的基底） ---

func recommendSnapshotBody() *types.ParamRecommendPoolSnapshot {
	return &types.ParamRecommendPoolSnapshot{
		Source: int32(recallrpc.Source_SOURCE_HOT), PoolKey: "global", Version: 0, Pn: 1, Ps: 50,
	}
}

func recommendVersionsBody() *types.ParamRecommendPoolVersionList {
	return &types.ParamRecommendPoolVersionList{
		Source: int32(recallrpc.Source_SOURCE_TAG), PoolKey: "zone:21", Limit: 20, IncludeRetired: true,
	}
}

func recommendConfigBody() *types.ParamRecommendRecallConfig {
	return &types.ParamRecommendRecallConfig{Scene: "feed", Mid: 42}
}

func recommendLogGetBody() *types.ParamRecommendRecallLogGet {
	return &types.ParamRecommendRecallLogGet{RequestId: "req-20260920-0001"}
}

func recommendLogListBody() *types.ParamRecommendRecallLogList {
	return &types.ParamRecommendRecallLogList{
		Mid: 42, Scene: "feed", FromTime: 1700000000, ToTime: 1700086400, Pn: 2, Ps: 20,
	}
}

func recommendDecisionBody() *types.ParamRankDecisionGet {
	return &types.ParamRankDecisionGet{DecisionId: "dec-0001"}
}

func recommendDecisionListBody() *types.ParamRankDecisionList {
	return &types.ParamRankDecisionList{
		ExpKey: "exp_rank_v2", VariantKey: "treatment", ModelKey: "default", ModelVersion: "v3",
		Scene: "feed", FromTime: 1700000000, ToTime: 1700086400, OnlyDegraded: true, Pn: 1, Ps: 30,
	}
}

func recommendRuntimeBody() *types.ParamRankRuntimeConfig {
	return &types.ParamRankRuntimeConfig{Scene: "feed", ModelKey: "default"}
}

func recommendItemsBody() *types.ParamRecommendPoolItemUpsert {
	return &types.ParamRecommendPoolItemUpsert{
		Pool:           recommendPoolFixture(),
		Version:        42,
		BatchId:        "batch-20260920-01",
		Generator:      "cron:hot_pool_daily",
		SchemaVersion:  1,
		Items:          []types.RecommendPoolItemInput{{Aid: 1001, Score: 0.25}, {Aid: 1002, Score: 1.5}},
		IdempotencyKey: "idem-items-0001",
		IsLastBatch:    true,
	}
}

func recommendPublishBody() *types.ParamRecommendPoolVersionPublish {
	return &types.ParamRecommendPoolVersionPublish{
		Pool: recommendPoolFixture(), Version: 42,
		Reason: "日更批次校验通过", IdempotencyKey: "idem-publish-0001",
	}
}

func recommendRollbackBody() *types.ParamRecommendPoolVersionRollback {
	return &types.ParamRecommendPoolVersionRollback{
		Pool: recommendPoolFixture(), TargetVersion: 41,
		Reason: "v42 出数异常，回滚上一版", IdempotencyKey: "idem-rollback-0001",
	}
}

func recommendPruneBody() *types.ParamRecommendPoolVersionPrune {
	return &types.ParamRecommendPoolVersionPrune{
		Pool: recommendPoolFixture(), KeepVersions: 3, MaxRows: 500, DryRun: true,
		RequestId: "req-prune-0001",
	}
}

func recommendModelBody() *types.ParamRankModelVersionUpsert {
	return &types.ParamRankModelVersionUpsert{
		ModelKey: "default", Version: "v3", FeatureConfigVersion: "fc-7",
		ObjectiveWeights: []types.RankObjectiveWeight{
			{Objective: "pred_click", Weight: 0.6}, {Objective: "pred_finish", Weight: 0.4},
		},
		ArtifactRef:    "oss://rank-models/default/v3.txt",
		OfflineMetrics: "{\"auc\":0.713}",
		Reason:         "灰度前登记", IdempotencyKey: "idem-model-0001",
	}
}

func recommendModelStateBody() *types.ParamRankModelStateSet {
	return &types.ParamRankModelStateSet{
		ModelKey: "default", Version: "v3",
		TargetState: int32(rankrpc.ModelVersionState_MODEL_VERSION_STATE_ACTIVE),
		Reason:      "激活 v3", IdempotencyKey: "idem-model-state-0001",
	}
}

func recommendFeatureBody() *types.ParamRankFeatureConfigUpsert {
	return &types.ParamRankFeatureConfigUpsert{
		ConfigVersion: "fc-8",
		// 顺序与重复项都必须原样交给服务（去重会把「同一个特征登记两遍」藏起来）。
		FeatureKeys:       []string{"f_click_3d", "f_finish_3d", "f_click_3d"},
		MissingPolicy:     "default",
		FeatureStoreScene: "rank_feed",
		Reason:            "新增完播特征", IdempotencyKey: "idem-feature-0001",
	}
}

func recommendExperimentBody() *types.ParamRankExperimentUpsert {
	return &types.ParamRankExperimentUpsert{
		ExpKey: "exp_rank_v2", VariantKey: "treatment", LayerKey: "rank_main", HashSeed: "seed-2026w38",
		BucketStart: 0, BucketEnd: 100, ModelKey: "default", ModelVersion: "v3", FeatureConfigVersion: "fc-8",
		Overrides: "{\"score_budget_ms\":80}", StartAt: 1700000000, EndAt: 1700604800,
		Reason: "10% 开量", IdempotencyKey: "idem-exp-0001",
	}
}

func recommendExpStateBody() *types.ParamRankExperimentStateSet {
	return &types.ParamRankExperimentStateSet{
		ExpKey: "exp_rank_v2", VariantKey: "treatment",
		TargetState: int32(rankrpc.ExperimentState_EXPERIMENT_STATE_PAUSED),
		Reason:      "观察到负向指标，暂停", IdempotencyKey: "idem-exp-state-0001",
	}
}

// --- 1. 投影：rpc 回包逐字段进后台结构，一条都不能丢 ---

func TestGetPoolSnapshotProjectsEveryField(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.snapshot = &recallrpc.GetPoolSnapshotReply{
		Pool:      &recallrpc.PoolRef{Source: recallrpc.Source_SOURCE_TAG, PoolKey: "zone:21"},
		Version:   42,
		BatchId:   "batch-20260920-01",
		State:     recallrpc.PoolVersionState_POOL_VERSION_STATE_CURRENT,
		ItemCount: 7000,
		Items: []*recallrpc.PoolItem{
			{Aid: 1001, Score: 0.25, Version: 42, Ctime: 1700000000},
			{Aid: 1002, Score: 1.5, Version: 42, Ctime: 1700000001},
		},
		HasMore:     true,
		PublishedAt: 1700000100,
	}
	req := recommendSnapshotBody()
	resp, err := NewGetPoolSnapshotLogic(context.Background(), recommendSvc(rc, rk)).GetPoolSnapshot(req)
	if err != nil {
		t.Fatalf("GetPoolSnapshot: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := types.RecommendPoolSnapshotData{
		Pool:        types.RecommendPoolRef{Source: int32(recallrpc.Source_SOURCE_TAG), PoolKey: "zone:21"},
		Version:     42,
		BatchId:     "batch-20260920-01",
		State:       int32(recallrpc.PoolVersionState_POOL_VERSION_STATE_CURRENT),
		ItemCount:   7000,
		Items:       []types.RecommendPoolItem{{Aid: 1001, Score: 0.25, Version: 42, Ctime: 1700000000}, {Aid: 1002, Score: 1.5, Version: 42, Ctime: 1700000001}},
		HasMore:     true,
		PublishedAt: 1700000100,
	}
	if !reflect.DeepEqual(resp.Data, want) {
		t.Fatalf("池快照投影不完整：\n got %+v\nwant %+v", resp.Data, want)
	}
	// 服务回传的 pool 优先于请求回显（它才知道最终读的是哪个池）。
	if rc.snapshotReq.GetPool().GetPoolKey() != "global" {
		t.Fatalf("请求池寻址被改写：%q", rc.snapshotReq.GetPool().GetPoolKey())
	}
	if got := rc.snapshotReq.GetPool().GetSource(); got != recallrpc.Source_SOURCE_HOT {
		t.Fatalf("请求 source 被改写：%d", got)
	}
	// version=0 是「读 CURRENT」的合法哨兵，pn/ps 不代填、不夹取（上限在服务）。
	if rc.snapshotReq.GetVersion() != 0 || rc.snapshotReq.GetPn() != 1 || rc.snapshotReq.GetPs() != 50 {
		t.Fatalf("version/pn/ps 未原样透传：%+v", rc.snapshotReq)
	}
}

func TestGetPoolSnapshotEchoesRequestWhenServiceOmitsPool(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.snapshot = &recallrpc.GetPoolSnapshotReply{Version: 7} // 服务没回 pool
	resp, err := NewGetPoolSnapshotLogic(context.Background(), recommendSvc(rc, rk)).
		GetPoolSnapshot(recommendSnapshotBody())
	if err != nil {
		t.Fatalf("GetPoolSnapshot: %v", err)
	}
	want := recommendPoolFixture()
	if resp.Data.Pool != want {
		t.Fatalf("pool 缺席时应回显请求寻址，got %+v want %+v", resp.Data.Pool, want)
	}
	if resp.Data.Items == nil {
		t.Fatal("items 为 nil：空池应投影成 []，与「下游没接」区分")
	}
}

func TestListPoolVersionsProjectsEveryField(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.versions = &recallrpc.ListPoolVersionsReply{
		Versions: []*recallrpc.PoolVersionInfo{{
			Pool:          &recallrpc.PoolRef{Source: recallrpc.Source_SOURCE_TAG, PoolKey: "zone:21"},
			Version:       41,
			BatchId:       "batch-20260919-01",
			Generator:     "cron:hot_pool_daily",
			SchemaVersion: 1,
			ItemCount:     6900,
			State:         recallrpc.PoolVersionState_POOL_VERSION_STATE_RETIRED,
			PublishedAt:   1699900000,
			Operator:      "gateway/admin:51",
			Note:          "上一版",
			Ctime:         1699890000,
			Mtime:         1699900001,
		}},
		CurrentVersion: 42,
	}
	resp, err := NewListPoolVersionsLogic(recommendSessionCtx(), recommendSvc(rc, rk)).
		ListPoolVersions(recommendVersionsBody())
	if err != nil {
		t.Fatalf("ListPoolVersions: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := []types.RecommendPoolVersionInfo{{
		Pool:          types.RecommendPoolRef{Source: int32(recallrpc.Source_SOURCE_TAG), PoolKey: "zone:21"},
		Version:       41,
		BatchId:       "batch-20260919-01",
		Generator:     "cron:hot_pool_daily",
		SchemaVersion: 1,
		ItemCount:     6900,
		State:         int32(recallrpc.PoolVersionState_POOL_VERSION_STATE_RETIRED),
		PublishedAt:   1699900000,
		Operator:      "gateway/admin:51",
		Note:          "上一版",
		Ctime:         1699890000,
		Mtime:         1699900001,
	}}
	if !reflect.DeepEqual(resp.Data.List, want) {
		t.Fatalf("版本台账投影不完整（batch/generator/operator/note 是回滚追溯证据）：\n got %+v\nwant %+v",
			resp.Data.List, want)
	}
	if resp.Data.CurrentVersion != 42 {
		t.Fatalf("current_version 未回传：%d", resp.Data.CurrentVersion)
	}
	if rc.versionsReq.GetLimit() != 20 || !rc.versionsReq.GetIncludeRetired() {
		t.Fatalf("limit/include_retired 未原样透传：%+v", rc.versionsReq)
	}
}

func TestGetRecallConfigProjectsEveryField(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.config = &recallrpc.GetRecallConfigReply{
		MaxCandidates:  600,
		DefaultLimit:   20,
		PerSourceMax:   200,
		EnabledSources: []recallrpc.Source{recallrpc.Source_SOURCE_HOT, recallrpc.Source_SOURCE_COLD},
		DefaultSources: []recallrpc.Source{recallrpc.Source_SOURCE_HOT},
		MaxSeedAids:    20,
		MaxSeedTags:    10,
		MaxExcludeAids: 500,
		DegradeEnabled: true,
		FallbackSource: recallrpc.Source_SOURCE_COLD,
		TtlSeconds:     30,
		ReadyPools: []*recallrpc.PoolStatus{{
			Pool:           &recallrpc.PoolRef{Source: recallrpc.Source_SOURCE_HOT, PoolKey: "global"},
			CurrentVersion: 42,
			BatchId:        "batch-20260920-01",
			ItemCount:      7000,
			PublishedAt:    1700000100,
			Stale:          true,
		}},
	}
	resp, err := NewGetRecallConfigLogic(context.Background(), recommendSvc(rc, rk)).
		GetRecallConfig(recommendConfigBody())
	if err != nil {
		t.Fatalf("GetRecallConfig: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := types.RecommendRecallConfigData{
		MaxCandidates: 600, DefaultLimit: 20, PerSourceMax: 200,
		EnabledSources: []int32{int32(recallrpc.Source_SOURCE_HOT), int32(recallrpc.Source_SOURCE_COLD)},
		DefaultSources: []int32{int32(recallrpc.Source_SOURCE_HOT)},
		MaxSeedAids:    20, MaxSeedTags: 10, MaxExcludeAids: 500,
		DegradeEnabled: true,
		FallbackSource: int32(recallrpc.Source_SOURCE_COLD),
		TtlSeconds:     30,
		ReadyPools: []types.RecommendPoolStatus{{
			Pool: types.RecommendPoolRef{
				Source:  int32(recallrpc.Source_SOURCE_HOT),
				PoolKey: "global",
			},
			CurrentVersion: 42, BatchId: "batch-20260920-01", ItemCount: 7000,
			PublishedAt: 1700000100, Stale: true,
		}},
	}
	if !reflect.DeepEqual(resp.Data, want) {
		t.Fatalf("在线召回参数投影不完整：\n got %+v\nwant %+v", resp.Data, want)
	}
	if rc.configReq.GetScene() != "feed" || rc.configReq.GetMid() != 42 {
		t.Fatalf("scene/mid 未原样透传：%+v", rc.configReq)
	}
}

func recallLogFixture() *recallrpc.RecallRequestLogInfo {
	return &recallrpc.RecallRequestLogInfo{
		RequestId:        "req-20260920-0001",
		SnapshotId:       "snap-20260920-0001",
		Mid:              42,
		Scene:            "feed",
		Platform:         recallrpc.Platform_PLATFORM_ANDROID,
		AppVersion:       "1.8.0",
		Region:           "CN-ZJ",
		RequestedSources: []recallrpc.Source{recallrpc.Source_SOURCE_HOT, recallrpc.Source_SOURCE_FOLLOW},
		PerSource: []*recallrpc.SourceStat{{
			Source:      recallrpc.Source_SOURCE_HOT,
			Planned:     200,
			Returned:    200,
			PoolVersion: 42,
			BatchId:     "batch-20260920-01",
			Degraded:    false,
			ErrorCode:   "",
		}, {
			Source:      recallrpc.Source_SOURCE_FOLLOW,
			Planned:     100,
			Returned:    0,
			PoolVersion: 0,
			BatchId:     "",
			Degraded:    true,
			ErrorCode:   "pool_not_ready",
		}},
		CandidateCount: 200,
		ReturnedCount:  200,
		Degraded:       true,
		Reason:         recallrpc.DegradeReason_DEGRADE_REASON_POOL_NOT_READY,
		CostMs:         37,
		VersionsDigest: "sha256:aaaa",
		TraceId:        "trace-0001",
		Ctime:          1700000200,
	}
}

func TestGetRecallRequestLogProjectsEveryField(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.log = &recallrpc.GetRecallRequestLogReply{Entry: recallLogFixture()}
	resp, err := NewGetRecallRequestLogLogic(context.Background(), recommendSvc(rc, rk)).
		GetRecallRequestLog(recommendLogGetBody())
	if err != nil {
		t.Fatalf("GetRecallRequestLog: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !resp.Data.Found {
		t.Fatal("服务回了这一条，found 必须为 true")
	}
	want := types.RecommendRequestLog{
		RequestId: "req-20260920-0001", SnapshotId: "snap-20260920-0001", Mid: 42, Scene: "feed",
		Platform: int32(recallrpc.Platform_PLATFORM_ANDROID), AppVersion: "1.8.0", Region: "CN-ZJ",
		RequestedSources: []int32{int32(recallrpc.Source_SOURCE_HOT), int32(recallrpc.Source_SOURCE_FOLLOW)},
		PerSource: []types.RecommendSourceStat{
			{Source: int32(recallrpc.Source_SOURCE_HOT), Planned: 200, Returned: 200,
				PoolVersion: 42, BatchId: "batch-20260920-01", Degraded: false, ErrorCode: ""},
			{Source: int32(recallrpc.Source_SOURCE_FOLLOW), Planned: 100, Returned: 0,
				PoolVersion: 0, BatchId: "", Degraded: true, ErrorCode: "pool_not_ready"},
		},
		CandidateCount: 200, ReturnedCount: 200, Degraded: true,
		Reason: int32(recallrpc.DegradeReason_DEGRADE_REASON_POOL_NOT_READY),
		CostMs: 37, VersionsDigest: "sha256:aaaa", TraceId: "trace-0001", Ctime: 1700000200,
	}
	if !reflect.DeepEqual(resp.Data.Entry, want) {
		t.Fatalf("召回回放投影不完整（per_source/versions_digest 是「读了哪个版本」的证据）：\n got %+v\nwant %+v",
			resp.Data.Entry, want)
	}
	if rc.logReq.GetRequestId() != "req-20260920-0001" {
		t.Fatalf("request_id 未原样透传：%q", rc.logReq.GetRequestId())
	}
}

func TestGetRecallRequestLogFoundFalseWhenEntryMissing(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.log = &recallrpc.GetRecallRequestLogReply{} // entry 为 null
	resp, err := NewGetRecallRequestLogLogic(context.Background(), recommendSvc(rc, rk)).
		GetRecallRequestLog(recommendLogGetBody())
	if err != nil {
		t.Fatalf("未命中不是错误：got %v", err)
	}
	if resp.Data.Found {
		t.Fatal("entry 缺席时 found 必须为 false，不能拿全零值冒充命中")
	}
	if resp.Data.Entry.RequestId != "" {
		t.Fatalf("未命中却回了 request_id：%q", resp.Data.Entry.RequestId)
	}
}

func TestListRecallRequestLogsProjectsAndPassesFilters(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.logs = &recallrpc.ListRecallRequestLogsReply{
		Entries: []*recallrpc.RecallRequestLogInfo{recallLogFixture()},
		HasMore: true,
	}
	resp, err := NewListRecallRequestLogsLogic(context.Background(), recommendSvc(rc, rk)).
		ListRecallRequestLogs(recommendLogListBody())
	if err != nil {
		t.Fatalf("ListRecallRequestLogs: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if len(resp.Data.List) != 1 || !reflect.DeepEqual(resp.Data.List[0].PerSource,
		recallRequestLogToAPI(recallLogFixture()).PerSource) {
		t.Fatalf("分页日志投影不完整：%+v", resp.Data.List)
	}
	if !resp.Data.HasMore {
		t.Fatal("has_more 未回传")
	}
	want := recommendLogListBody()
	got := rc.logsReq
	if got.GetMid() != want.Mid || got.GetScene() != want.Scene ||
		got.GetFromTime() != want.FromTime || got.GetToTime() != want.ToTime ||
		got.GetPn() != want.Pn || got.GetPs() != want.Ps {
		t.Fatalf("过滤条件未原样透传：%+v vs %+v", got, want)
	}
}

func rankDecisionFixture() *rankrpc.RankDecisionInfo {
	return &rankrpc.RankDecisionInfo{
		DecisionId:           "dec-0001",
		RequestId:            "req-20260920-0001",
		TraceId:              "trace-0001",
		SnapshotId:           "snap-20260920-0001",
		Mid:                  42,
		SubjectType:          rankrpc.SubjectType_SUBJECT_TYPE_MID,
		SubjectId:            "42",
		Scene:                "feed",
		Platform:             rankrpc.Platform_PLATFORM_IOS,
		AppVersion:           "1.8.0",
		ExpKey:               "exp_rank_v2",
		VariantKey:           "treatment",
		BucketNo:             733,
		ModelKey:             "default",
		ModelVersion:         "v3",
		FeatureConfigVersion: "fc-7",
		InputCount:           200,
		ReturnedCount:        20,
		ResultDigest:         "sha256:bbbb",
		TopAids:              []int64{1001, 1002, 1003},
		Degraded:             true,
		Reason:               rankrpc.RankDegradeReason_RANK_DEGRADE_REASON_BUDGET_EXHAUSTED,
		Fallback:             rankrpc.FallbackStrategy_FALLBACK_STRATEGY_RECALL_ORDER,
		Filters: &rankrpc.FilterStat{
			SafetyFiltered: 1, FrequencyFiltered: 2, DedupFiltered: 3, DiversifiedMoved: 4, Truncated: 5,
		},
		CostMs: 46,
		Ctime:  1700000300,
	}
}

func TestGetRankDecisionProjectsEveryField(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rk.decision = &rankrpc.GetRankDecisionReply{Entry: rankDecisionFixture()}
	resp, err := NewGetRankDecisionLogic(context.Background(), recommendSvc(rc, rk)).
		GetRankDecision(recommendDecisionBody())
	if err != nil {
		t.Fatalf("GetRankDecision: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !resp.Data.Found {
		t.Fatal("服务回了这一条，found 必须为 true")
	}
	want := types.RankDecisionInfo{
		DecisionId: "dec-0001", RequestId: "req-20260920-0001", TraceId: "trace-0001",
		SnapshotId: "snap-20260920-0001", Mid: 42,
		SubjectType: int32(rankrpc.SubjectType_SUBJECT_TYPE_MID), SubjectId: "42", Scene: "feed",
		Platform:             int32(rankrpc.Platform_PLATFORM_IOS),
		AppVersion:           "1.8.0",
		ExpKey:               "exp_rank_v2",
		VariantKey:           "treatment",
		BucketNo:             733,
		ModelKey:             "default",
		ModelVersion:         "v3",
		FeatureConfigVersion: "fc-7",
		InputCount:           200, ReturnedCount: 20, ResultDigest: "sha256:bbbb",
		TopAids:  []int64{1001, 1002, 1003},
		Degraded: true,
		Reason:   int32(rankrpc.RankDegradeReason_RANK_DEGRADE_REASON_BUDGET_EXHAUSTED),
		Fallback: int32(rankrpc.FallbackStrategy_FALLBACK_STRATEGY_RECALL_ORDER),
		Filters: types.RankFilterStat{SafetyFiltered: 1, FrequencyFiltered: 2, DedupFiltered: 3,
			DiversifiedMoved: 4, Truncated: 5},
		CostMs: 46, Ctime: 1700000300,
	}
	if !reflect.DeepEqual(resp.Data.Entry, want) {
		t.Fatalf("排序决策投影不完整（top_aids/filters/degraded/fallback 是「有没有凭空产出」的证据）：\n got %+v\nwant %+v",
			resp.Data.Entry, want)
	}
	if rk.decisionReq.GetDecisionId() != "dec-0001" || rk.decisionReq.GetRequestId() != "" {
		t.Fatalf("主键未原样透传：%+v", rk.decisionReq)
	}
}

func TestGetRankDecisionFoundFalseWhenEntryMissing(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rk.decision = &rankrpc.GetRankDecisionReply{}
	resp, err := NewGetRankDecisionLogic(context.Background(), recommendSvc(rc, rk)).
		GetRankDecision(recommendDecisionBody())
	if err != nil {
		t.Fatalf("未命中不是错误：got %v", err)
	}
	if resp.Data.Found {
		t.Fatal("entry 缺席时 found 必须为 false")
	}
	if resp.Data.Entry.InputCount != 0 || resp.Data.Entry.ReturnedCount != 0 {
		t.Fatal("未命中却回了计数，会被读成「这次排序真的 0 输入 0 输出」")
	}
}

func TestListRankDecisionsProjectsAndPassesEveryFilter(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rk.decisions = &rankrpc.ListRankDecisionsReply{
		Entries: []*rankrpc.RankDecisionInfo{rankDecisionFixture()}, HasMore: true,
	}
	body := recommendDecisionListBody()
	resp, err := NewListRankDecisionsLogic(context.Background(), recommendSvc(rc, rk)).ListRankDecisions(body)
	if err != nil {
		t.Fatalf("ListRankDecisions: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if len(resp.Data.List) != 1 || !reflect.DeepEqual(resp.Data.List[0], rankDecisionToAPI(rankDecisionFixture())) {
		t.Fatalf("决策摘要分页投影不完整：%+v", resp.Data.List)
	}
	if !resp.Data.HasMore {
		t.Fatal("has_more 未回传")
	}
	got := rk.decisionsReq
	if got.GetExpKey() != body.ExpKey || got.GetVariantKey() != body.VariantKey ||
		got.GetModelKey() != body.ModelKey || got.GetModelVersion() != body.ModelVersion ||
		got.GetScene() != body.Scene || got.GetFromTime() != body.FromTime ||
		got.GetToTime() != body.ToTime || got.GetOnlyDegraded() != body.OnlyDegraded ||
		got.GetPn() != body.Pn || got.GetPs() != body.Ps {
		t.Fatalf("过滤条件未原样透传：%+v vs %+v", got, body)
	}
}

func TestListRankDecisionsEmptyResultIsNotNilSlice(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rk.decisions = &rankrpc.ListRankDecisionsReply{}
	resp, err := NewListRankDecisionsLogic(context.Background(), recommendSvc(rc, rk)).
		ListRankDecisions(recommendDecisionListBody())
	if err != nil {
		t.Fatalf("ListRankDecisions: %v", err)
	}
	if resp.Data.List == nil {
		t.Fatal("空结果必须投影成 []，不能回 null")
	}
	if len(resp.Data.List) != 0 {
		t.Fatalf("空结果条数：%d", len(resp.Data.List))
	}
}

func rankExperimentFixture() *rankrpc.ExperimentInfo {
	return &rankrpc.ExperimentInfo{
		ExpKey: "exp_rank_v2", VariantKey: "treatment", LayerKey: "rank_main",
		BucketStart: 0, BucketEnd: 100, ModelKey: "default", ModelVersion: "v3",
		FeatureConfigVersion: "fc-8", Overrides: "{\"score_budget_ms\":80}",
		State:    rankrpc.ExperimentState_EXPERIMENT_STATE_RUNNING,
		Revision: 4, StartAt: 1700000000, EndAt: 1700604800,
		Operator: recommendOperatorFixture, Reason: "10% 开量", Ctime: 1700000000, Mtime: 1700000400,
	}
}

func TestGetRankRuntimeConfigProjectsEveryField(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rk.runtime = &rankrpc.GetRankRuntimeConfigReply{
		ModelKey: "default", ActiveModelVersion: "", FeatureConfigVersion: "fc-8",
		MaxCandidates: 400, MaxReturn: 20,
		Objectives:     []string{"pred_click", "pred_finish", "pred_interact", "pred_negative"},
		DegradeEnabled: true,
		Fallback:       rankrpc.FallbackStrategy_FALLBACK_STRATEGY_PREVIOUS_MODEL,
		ScoreBudgetMs:  80, TtlSeconds: 15,
		RunningExperiments: []*rankrpc.ExperimentInfo{rankExperimentFixture()},
		ConfigRevision:     "rev-2026w38-01",
	}
	resp, err := NewGetRankRuntimeConfigLogic(context.Background(), recommendSvc(rc, rk)).
		GetRankRuntimeConfig(recommendRuntimeBody())
	if err != nil {
		t.Fatalf("GetRankRuntimeConfig: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := types.RankRuntimeConfigData{
		ModelKey: "default", ActiveModelVersion: "", FeatureConfigVersion: "fc-8",
		MaxCandidates: 400, MaxReturn: 20,
		Objectives:     []string{"pred_click", "pred_finish", "pred_interact", "pred_negative"},
		DegradeEnabled: true,
		Fallback:       int32(rankrpc.FallbackStrategy_FALLBACK_STRATEGY_PREVIOUS_MODEL),
		ScoreBudgetMs:  80, TtlSeconds: 15,
		RunningExperiments: []types.RankExperimentInfo{rankExperimentToAPI(rankExperimentFixture())},
		ConfigRevision:     "rev-2026w38-01",
	}
	if !reflect.DeepEqual(resp.Data, want) {
		t.Fatalf("在线排序参数投影不完整：\n got %+v\nwant %+v", resp.Data, want)
	}
	// active_model_version 为空是「在线必然降级」的事实，网关不得兜成看起来正常。
	if resp.Data.ActiveModelVersion != "" {
		t.Fatalf("无 ACTIVE 模型时不得补默认值：%q", resp.Data.ActiveModelVersion)
	}
	if rk.runtimeReq.GetScene() != "feed" || rk.runtimeReq.GetModelKey() != "default" {
		t.Fatalf("scene/model_key 未原样透传：%+v", rk.runtimeReq)
	}
}

func TestGetRankRuntimeConfigEmptyObjectivesIsNotNilSlice(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rk.runtime = &rankrpc.GetRankRuntimeConfigReply{}
	resp, err := NewGetRankRuntimeConfigLogic(context.Background(), recommendSvc(rc, rk)).
		GetRankRuntimeConfig(recommendRuntimeBody())
	if err != nil {
		t.Fatalf("GetRankRuntimeConfig: %v", err)
	}
	if resp.Data.Objectives == nil || resp.Data.RunningExperiments == nil {
		t.Fatalf("列表必须是 [] 而非 null：%+v", resp.Data)
	}
	if rk.runtimeReq.GetModelKey() != "default" {
		t.Fatal("model_key 应按表单透传（空表示服务默认，网关不挑一个填上去）")
	}
}

// --- 2. 写入口：入参逐字段交给下游，回包逐字段回传，operator 只来自会话 ---

func TestUpsertPoolItemsPassesFormVerbatim(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.items = &recallrpc.UpsertPoolItemsReply{
		Version: 42, Written: 2, ItemCount: 7000,
		State: recallrpc.PoolVersionState_POOL_VERSION_STATE_BUILDING, Deduplicated: true,
	}
	body := recommendItemsBody()
	resp, err := NewUpsertPoolItemsLogic(recommendSessionCtx(), recommendSvc(rc, rk)).UpsertPoolItems(body)
	if err != nil {
		t.Fatalf("UpsertPoolItems: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := types.RecommendPoolItemUpsertData{
		Version: 42, Written: 2, ItemCount: 7000,
		State:        int32(recallrpc.PoolVersionState_POOL_VERSION_STATE_BUILDING),
		Deduplicated: true, // 幂等重放是正常结论，网关不改写成错误
	}
	if !reflect.DeepEqual(resp.Data, want) {
		t.Fatalf("写入结论投影不完整：\n got %+v\nwant %+v", resp.Data, want)
	}
	got := rc.itemsReq
	if got.GetVersion() != body.Version || got.GetBatchId() != body.BatchId ||
		got.GetSchemaVersion() != body.SchemaVersion || got.GetIsLastBatch() != body.IsLastBatch {
		t.Fatalf("批次字段未原样透传：%+v", got)
	}
	if got.GetIdempotencyKey() != "idem-items-0001" {
		t.Fatalf("idempotency_key 被改写：%q", got.GetIdempotencyKey())
	}
	// 契约缺口：UpsertPoolItemsReq 没有 operator 位（generator 描述产出作业，不是操作者）。
	if got.GetGenerator() != "cron:hot_pool_daily" {
		t.Fatalf("generator 应保留表单声明：%q", got.GetGenerator())
	}
	if len(got.GetItems()) != 2 || got.GetItems()[0].GetAid() != 1001 || got.GetItems()[1].GetScore() != 1.5 {
		t.Fatalf("条目未原样交给服务（不排序、不裁剪）：%+v", got.GetItems())
	}
}

func TestPublishAndRollbackPoolVersionProjectSwitch(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.publish = &recallrpc.PublishPoolVersionReply{
		Switched: true, PreviousVersion: 41, CurrentVersion: 42, Deduplicated: false,
		EventId: "evt-0001",
	}
	rc.rollback = &recallrpc.RollbackPoolVersionReply{
		Switched: false, PreviousVersion: 42, CurrentVersion: 42, Deduplicated: true, EventId: "",
	}
	ctx := recommendSessionCtx()
	pubResp, err := NewPublishPoolVersionLogic(ctx, recommendSvc(rc, rk)).PublishPoolVersion(recommendPublishBody())
	if err != nil {
		t.Fatalf("PublishPoolVersion: %v", err)
	}
	recommendAssertEnvelope(t, pubResp.Code, pubResp.Message, pubResp.TTL)
	if want := (types.RecommendPoolVersionSwitchData{
		Switched: true, PreviousVersion: 41, CurrentVersion: 42, EventId: "evt-0001",
	}); !reflect.DeepEqual(pubResp.Data, want) {
		t.Fatalf("发布结论投影不完整：\n got %+v\nwant %+v", pubResp.Data, want)
	}
	if rc.publishReq.GetVersion() != 42 || rc.publishReq.GetIdempotencyKey() != "idem-publish-0001" ||
		rc.publishReq.GetReason() != "日更批次校验通过" {
		t.Fatalf("发布入参未原样透传：%+v", rc.publishReq)
	}

	rbResp, err := NewRollbackPoolVersionLogic(ctx, recommendSvc(rc, rk)).RollbackPoolVersion(recommendRollbackBody())
	if err != nil {
		t.Fatalf("RollbackPoolVersion: %v", err)
	}
	recommendAssertEnvelope(t, rbResp.Code, rbResp.Message, rbResp.TTL)
	// switched=false + deduplicated=true 是重放结论：event_id 为空也不得伪造。
	if want := (types.RecommendPoolVersionSwitchData{
		PreviousVersion: 42, CurrentVersion: 42, Deduplicated: true,
	}); !reflect.DeepEqual(rbResp.Data, want) {
		t.Fatalf("回滚结论投影不完整：\n got %+v\nwant %+v", rbResp.Data, want)
	}
	if rc.rollbackReq.GetTargetVersion() != 41 || rc.rollbackReq.GetIdempotencyKey() != "idem-rollback-0001" {
		t.Fatalf("回滚入参未原样透传：%+v", rc.rollbackReq)
	}
}

func TestPrunePoolVersionsProjectsCountersAndPassesDryRun(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.prune = &recallrpc.PrunePoolVersionsReply{
		ScannedVersions: 12, DeletedRows: 0, DryRun: true, HasMore: true,
	}
	body := recommendPruneBody()
	resp, err := NewPrunePoolVersionsLogic(recommendSessionCtx(), recommendSvc(rc, rk)).PrunePoolVersions(body)
	if err != nil {
		t.Fatalf("PrunePoolVersions: %v", err)
	}
	recommendAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !reflect.DeepEqual(resp.Data, types.RecommendPoolVersionPruneData{
		ScannedVersions: 12, DryRun: true, HasMore: true,
	}) {
		t.Fatalf("清理结论投影不完整：%+v", resp.Data)
	}
	got := rc.pruneReq
	if got.GetKeepVersions() != body.KeepVersions || got.GetMaxRows() != body.MaxRows ||
		!got.GetDryRun() {
		t.Fatalf("清理入参未原样透传：%+v vs %+v", got, body)
	}
	// 契约缺口：PrunePoolVersionsReq 没有 idempotency_key/trace_id，request_id 只做网关侧留痕，
	// 所以它不该被塞进任何下游字段（服务侧靠 operator 留痕）。
	if got.GetOperator() != recommendOperatorFixture {
		t.Fatalf("operator 应来自会话：%q", got.GetOperator())
	}
}

func TestRankWriteRoutesProjectReplies(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rk.model = &rankrpc.UpsertModelVersionReply{
		ModelKey: "default", Version: "v3",
		State: rankrpc.ModelVersionState_MODEL_VERSION_STATE_DRAFT, Revision: 2, Deduplicated: true,
	}
	rk.modelStateR = &rankrpc.SetModelVersionStateReply{
		Changed: true, State: rankrpc.ModelVersionState_MODEL_VERSION_STATE_ACTIVE,
		PreviousActiveVersion: "v2", Deduplicated: false, EventId: "",
	}
	rk.feature = &rankrpc.UpsertFeatureConfigReply{
		ConfigVersion: "fc-8", FeatureCount: 3, Revision: 1, Deduplicated: false,
	}
	rk.exp = &rankrpc.UpsertExperimentReply{Experiment: rankExperimentFixture(), Deduplicated: false}
	rk.expState = &rankrpc.SetExperimentStateReply{
		Changed: false, State: rankrpc.ExperimentState_EXPERIMENT_STATE_PAUSED, Deduplicated: true,
	}
	s := recommendSvc(rc, rk)
	ctx := recommendSessionCtx()

	modelResp, err := NewUpsertModelVersionLogic(ctx, s).UpsertModelVersion(recommendModelBody())
	if err != nil {
		t.Fatalf("UpsertModelVersion: %v", err)
	}
	recommendAssertEnvelope(t, modelResp.Code, modelResp.Message, modelResp.TTL)
	if want := (types.RankModelVersionUpsertData{
		ModelKey: "default", Version: "v3",
		State: int32(rankrpc.ModelVersionState_MODEL_VERSION_STATE_DRAFT), Revision: 2, Deduplicated: true,
	}); !reflect.DeepEqual(modelResp.Data, want) {
		t.Fatalf("模型登记结论投影不完整：\n got %+v\nwant %+v", modelResp.Data, want)
	}
	// 权重逐条投影（objective/weight 是「按什么排序」的证据）。
	ws := rk.modelReq.GetObjectiveWeights()
	if len(ws) != 2 || ws[0].GetObjective() != "pred_click" || ws[0].GetWeight() != 0.6 ||
		ws[1].GetObjective() != "pred_finish" || ws[1].GetWeight() != 0.4 {
		t.Fatalf("objective_weights 未原样交给服务：%+v", ws)
	}
	if rk.modelReq.GetFeatureConfigVersion() != "fc-7" || rk.modelReq.GetArtifactRef() != "oss://rank-models/default/v3.txt" ||
		rk.modelReq.GetOfflineMetrics() != "{\"auc\":0.713}" {
		t.Fatalf("模型元数据未原样透传：%+v", rk.modelReq)
	}

	stateResp, err := NewSetModelVersionStateLogic(ctx, s).SetModelVersionState(recommendModelStateBody())
	if err != nil {
		t.Fatalf("SetModelVersionState: %v", err)
	}
	recommendAssertEnvelope(t, stateResp.Code, stateResp.Message, stateResp.TTL)
	if want := (types.RankModelStateSetData{
		Changed: true, State: int32(rankrpc.ModelVersionState_MODEL_VERSION_STATE_ACTIVE),
		PreviousActiveVersion: "v2",
	}); !reflect.DeepEqual(stateResp.Data, want) {
		t.Fatalf("激活结论投影不完整（被换掉的版本必须看得见）：\n got %+v\nwant %+v", stateResp.Data, want)
	}
	if rk.modelState.GetTargetState() != rankrpc.ModelVersionState_MODEL_VERSION_STATE_ACTIVE ||
		rk.modelState.GetIdempotencyKey() != "idem-model-state-0001" {
		t.Fatalf("目标状态/幂等键未原样透传：%+v", rk.modelState)
	}

	featResp, err := NewUpsertFeatureConfigLogic(ctx, s).UpsertFeatureConfig(recommendFeatureBody())
	if err != nil {
		t.Fatalf("UpsertFeatureConfig: %v", err)
	}
	recommendAssertEnvelope(t, featResp.Code, featResp.Message, featResp.TTL)
	if want := (types.RankFeatureConfigUpsertData{
		ConfigVersion: "fc-8", FeatureCount: 3, Revision: 1,
	}); !reflect.DeepEqual(featResp.Data, want) {
		t.Fatalf("特征配置登记结论投影不完整：%+v", featResp.Data)
	}
	if keys := rk.featureReq.GetFeatureKeys(); !reflect.DeepEqual(keys,
		[]string{"f_click_3d", "f_finish_3d", "f_click_3d"}) {
		t.Fatalf("feature_keys 顺序/重复项必须原样交给服务：%+v", keys)
	}
	if rk.featureReq.GetMissingPolicy() != "default" || rk.featureReq.GetFeatureStoreScene() != "rank_feed" {
		t.Fatalf("missing_policy/feature_store_scene 未原样透传：%+v", rk.featureReq)
	}

	expResp, err := NewUpsertExperimentLogic(ctx, s).UpsertExperiment(recommendExperimentBody())
	if err != nil {
		t.Fatalf("UpsertExperiment: %v", err)
	}
	recommendAssertEnvelope(t, expResp.Code, expResp.Message, expResp.TTL)
	if want := (types.RankExperimentUpsertData{
		Experiment: rankExperimentToAPI(rankExperimentFixture()),
	}); !reflect.DeepEqual(expResp.Data, want) {
		t.Fatalf("实验整行必须按服务结论回传（revision 由服务 +1）：\n got %+v\nwant %+v", expResp.Data, want)
	}
	got := rk.expReq
	body := recommendExperimentBody()
	if got.GetExpKey() != body.ExpKey || got.GetVariantKey() != body.VariantKey ||
		got.GetLayerKey() != body.LayerKey || got.GetHashSeed() != body.HashSeed ||
		got.GetBucketStart() != body.BucketStart || got.GetBucketEnd() != body.BucketEnd ||
		got.GetModelKey() != body.ModelKey || got.GetModelVersion() != body.ModelVersion ||
		got.GetFeatureConfigVersion() != body.FeatureConfigVersion || got.GetOverrides() != body.Overrides ||
		got.GetStartAt() != body.StartAt || got.GetEndAt() != body.EndAt {
		t.Fatalf("实验入参未原样交给服务：%+v vs %+v", got, body)
	}

	expStateResp, err := NewSetExperimentStateLogic(ctx, s).SetExperimentState(recommendExpStateBody())
	if err != nil {
		t.Fatalf("SetExperimentState: %v", err)
	}
	recommendAssertEnvelope(t, expStateResp.Code, expStateResp.Message, expStateResp.TTL)
	if want := (types.RankExperimentStateSetData{
		Changed: false, State: int32(rankrpc.ExperimentState_EXPERIMENT_STATE_PAUSED), Deduplicated: true,
	}); !reflect.DeepEqual(expStateResp.Data, want) {
		t.Fatalf("实验状态结论投影不完整：\n got %+v\nwant %+v", expStateResp.Data, want)
	}
	if rk.expStateReq.GetTargetState() != rankrpc.ExperimentState_EXPERIMENT_STATE_PAUSED {
		t.Fatalf("target_state 枚举转换错误：%v", rk.expStateReq.GetTargetState())
	}
}

// TestOperatorComesFromSessionAndCannotBeForged 覆盖两处：
//  1. 9 个写入口的表单类型都**没有** operator 位（客户端无法自称某个后台账号）；
//  2. 8 个在 proto 里有 operator 位的方法，收到的都是会话渲染出的 gateway/admin:<admin_id>。
//
// UpsertPoolItems 例外：契约里就没有 operator 位，因此它只能靠会话门槛 + 日志留痕（见 README 缺口）。
func TestOperatorComesFromSessionAndCannotBeForged(t *testing.T) {
	writeBodies := map[string]any{
		"upsertPoolItems":      recommendItemsBody(),
		"publishPoolVersion":   recommendPublishBody(),
		"rollbackPoolVersion":  recommendRollbackBody(),
		"prunePoolVersions":    recommendPruneBody(),
		"upsertModelVersion":   recommendModelBody(),
		"setModelVersionState": recommendModelStateBody(),
		"upsertFeatureConfig":  recommendFeatureBody(),
		"upsertExperiment":     recommendExperimentBody(),
		"setExperimentState":   recommendExpStateBody(),
	}
	if len(writeBodies) != 9 {
		t.Fatalf("写入口数量应为 9，实际 %d", len(writeBodies))
	}
	for name, body := range writeBodies {
		tp := reflect.TypeOf(body).Elem()
		for i := 0; i < tp.NumField(); i++ {
			f := tp.Field(i)
			tag := strings.ToLower(f.Tag.Get("json"))
			if strings.Contains(tag, "operator") || strings.Contains(tag, "admin") ||
				strings.EqualFold(f.Name, "Operator") {
				t.Fatalf("%s 的表单暴露了操作者位 %s（%s）：只能由会话渲染", name, f.Name, tag)
			}
		}
	}

	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.publish = &recallrpc.PublishPoolVersionReply{}
	rc.rollback = &recallrpc.RollbackPoolVersionReply{}
	rc.prune = &recallrpc.PrunePoolVersionsReply{}
	rk.model = &rankrpc.UpsertModelVersionReply{}
	rk.modelStateR = &rankrpc.SetModelVersionStateReply{}
	rk.feature = &rankrpc.UpsertFeatureConfigReply{}
	rk.exp = &rankrpc.UpsertExperimentReply{}
	rk.expState = &rankrpc.SetExperimentStateReply{}
	s := recommendSvc(rc, rk)
	ctx := recommendSessionCtx()
	for _, rt := range recommendRoutes() {
		if !rt.write {
			continue
		}
		if err := rt.call(ctx, s, rt.newBody()); err != nil {
			t.Fatalf("%s: %v", rt.name, err)
		}
	}
	for name, got := range map[string]string{
		"publishPoolVersion":   rc.publishReq.GetOperator(),
		"rollbackPoolVersion":  rc.rollbackReq.GetOperator(),
		"prunePoolVersions":    rc.pruneReq.GetOperator(),
		"upsertModelVersion":   rk.modelReq.GetOperator(),
		"setModelVersionState": rk.modelState.GetOperator(),
		"upsertFeatureConfig":  rk.featureReq.GetOperator(),
		"upsertExperiment":     rk.expReq.GetOperator(),
		"setExperimentState":   rk.expStateReq.GetOperator(),
	} {
		if got != recommendOperatorFixture {
			t.Fatalf("%s 的 operator 必须由会话渲染，实际 %q", name, got)
		}
	}
}

// --- 3. fail-closed 门槛：门槛不过就不产生下游调用 ---

// recommendCast 把表里的 any 入参还原成具体类型（类型不对说明测试表写错了）。
func recommendCast[T any](t *testing.T, body any) T {
	t.Helper()
	v, ok := body.(T)
	if !ok {
		var zero T
		t.Fatalf("入参类型应为 %T，实际 %T", zero, body)
	}
	return v
}

func TestRecommendRouteTableCoversAllSeventeenRoutes(t *testing.T) {
	var reads, writes int
	for _, rt := range recommendRoutes() {
		if rt.write {
			writes++
		} else {
			reads++
		}
	}
	if reads != 8 || writes != 9 {
		t.Fatalf("本轮应为 8 个读入口 + 9 个写入口，实际 %d + %d", reads, writes)
	}
}

func TestRecommendRoutesFailClosedWithoutClient(t *testing.T) {
	// 未配置下游时不退化成伪造空池/空决策：那会让后台把「下游没接」读成「池是空的」。
	for _, rt := range recommendRoutes() {
		err := rt.call(recommendSessionCtx(), &svc.ServiceContext{}, rt.newBody())
		want := errRecallServiceNotConfigured
		if rt.domain == "rank" {
			want = errRankServiceNotConfigured
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s 未配置客户端时应返回明确的 not configured，got %v", rt.name, err)
		}
		if !strings.Contains(err.Error(), "service not configured") {
			t.Fatalf("%s 错误消息不可定位：%v", rt.name, err)
		}
	}
}

func TestRecommendRoutesRejectMissingBody(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	for _, rt := range recommendRoutes() {
		err := rt.call(recommendSessionCtx(), recommendSvc(rc, rk), nil)
		if !errors.Is(err, errRecommendRequestMissing) {
			t.Fatalf("%s 请求体缺失时应返回 %v，got %v", rt.name, errRecommendRequestMissing, err)
		}
	}
	if rc.calls != 0 || rk.calls != 0 {
		t.Fatalf("请求体缺失却打了下游：recall=%d rank=%d", rc.calls, rk.calls)
	}
}

func TestRecommendRoutesPassDownstreamErrorsThrough(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.err = errRecommendFakeDownstream
	rk.err = errRecommendFakeDownstream
	s := recommendSvc(rc, rk)
	ctx := recommendSessionCtx()
	for _, rt := range recommendRoutes() {
		err := rt.call(ctx, s, rt.newBody())
		if !errors.Is(err, errRecommendFakeDownstream) {
			t.Fatalf("%s 把下游错误改写了：%v", rt.name, err)
		}
	}
	if rc.calls != 9 || rk.calls != 8 {
		t.Fatalf("下游调用数应为 9/8，实际 %d/%d", rc.calls, rk.calls)
	}
}

func TestRecommendWriteRoutesRequireSessionIdentity(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.publish = &recallrpc.PublishPoolVersionReply{}
	s := recommendSvc(rc, rk)
	for _, rt := range recommendRoutes() {
		if !rt.write {
			continue
		}
		// context.Background()：拿不到会话身份，说明这条路由没被 AdminPermission 保护（表/挂载漂移），
		// logic 必须 fail-closed 而不是照发下游。
		err := rt.call(context.Background(), s, rt.newBody())
		if !errors.Is(err, errRecommendSessionRequired) {
			t.Fatalf("%s 缺会话身份时应拒绝，got %v", rt.name, err)
		}
		if !strings.Contains(err.Error(), "session identity required") {
			t.Fatalf("%s 错误消息不可定位：%v", rt.name, err)
		}
	}
	if rc.calls != 0 || rk.calls != 0 {
		t.Fatalf("无会话身份却打了下游：recall=%d rank=%d", rc.calls, rk.calls)
	}
}

func TestRecommendWriteRoutesRequireNonBlankKey(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	s := recommendSvc(rc, rk)
	ctx := recommendSessionCtx()
	for _, rt := range recommendRoutes() {
		if !rt.write {
			continue
		}
		body := rt.newBody()
		// PrunePoolVersionsReq 契约里没有 idempotency_key，网关用 request_id 做防重与留痕键。
		field := "IdempotencyKey"
		want := "idempotency_key"
		if rt.name == "prunePoolVersions" {
			field, want = "RequestId", "request_id"
		}
		recommendBlank(t, body, field)
		err := rt.call(ctx, s, body)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s 应拒绝空白的 %s，got %v", rt.name, want, err)
		}
	}
	if rc.calls != 0 || rk.calls != 0 {
		t.Fatalf("空白幂等键却打了下游：recall=%d rank=%d", rc.calls, rk.calls)
	}
}

func TestRecommendPoolRefMustBeComplete(t *testing.T) {
	cases := []struct {
		name   string
		source int32
		key    string
	}{
		{"unspecified source", 0, "global"},
		{"negative source", -1, "global"},
		{"blank pool_key", int32(recallrpc.Source_SOURCE_HOT), "   "},
		{"empty pool_key", int32(recallrpc.Source_SOURCE_HOT), ""},
	}
	for _, tc := range cases {
		rc, rk := &recallAdminFake{}, &rankAdminFake{}
		s := recommendSvc(rc, rk)
		snap := recommendSnapshotBody()
		snap.Source, snap.PoolKey = tc.source, tc.key
		if _, err := NewGetPoolSnapshotLogic(context.Background(), s).GetPoolSnapshot(snap); !errors.Is(err, errRecommendPoolRefRequired) {
			t.Fatalf("读入口 %s 应拒绝半截池寻址，got %v", tc.name, err)
		}
		vers := recommendVersionsBody()
		vers.Source, vers.PoolKey = tc.source, tc.key
		if _, err := NewListPoolVersionsLogic(context.Background(), s).ListPoolVersions(vers); !errors.Is(err, errRecommendPoolRefRequired) {
			t.Fatalf("读入口 %s 应拒绝半截池寻址，got %v", tc.name, err)
		}
		items := recommendItemsBody()
		items.Pool.Source, items.Pool.PoolKey = tc.source, tc.key
		if _, err := NewUpsertPoolItemsLogic(recommendSessionCtx(), s).UpsertPoolItems(items); !errors.Is(err, errRecommendPoolRefRequired) {
			t.Fatalf("写入口 %s 应拒绝半截池寻址，got %v", tc.name, err)
		}
		if rc.calls != 0 {
			t.Fatalf("%s 却打了下游 %d 次", tc.name, rc.calls)
		}
	}
}

func TestRecommendPoolKeyIsNeverRewritten(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	body := recommendSnapshotBody()
	body.PoolKey = " zone:21 " // 前后空白：只用来判空，不改写原值（截断/归一化会改变键本身）
	if _, err := NewGetPoolSnapshotLogic(context.Background(), recommendSvc(rc, rk)).GetPoolSnapshot(body); err != nil {
		t.Fatalf("GetPoolSnapshot: %v", err)
	}
	if got := rc.snapshotReq.GetPool().GetPoolKey(); got != " zone:21 " {
		t.Fatalf("pool_key 被网关改写：%q", got)
	}
}

func TestRecommendSubjectKeyRequired(t *testing.T) {
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	s := recommendSvc(rc, rk)

	logReq := recommendLogGetBody()
	logReq.RequestId, logReq.SnapshotId = "", "  "
	if _, err := NewGetRecallRequestLogLogic(context.Background(), s).GetRecallRequestLog(logReq); !errors.Is(err, errRecallLogSubjectRequired) {
		t.Fatalf("两个键都为空时应拒绝，got %v", err)
	}
	// 只给 snapshot_id 也合法：谁优先由服务判定，网关不猜。
	only := recommendLogGetBody()
	only.RequestId, only.SnapshotId = "", "snap-9"
	if _, err := NewGetRecallRequestLogLogic(context.Background(), s).GetRecallRequestLog(only); err != nil {
		t.Fatalf("按 snapshot_id 回放应通过：got %v", err)
	}
	if rc.logReq.GetRequestId() != "" || rc.logReq.GetSnapshotId() != "snap-9" {
		t.Fatalf("键未原样透传：%+v", rc.logReq)
	}

	decReq := recommendDecisionBody()
	decReq.DecisionId, decReq.RequestId = " ", ""
	if _, err := NewGetRankDecisionLogic(context.Background(), s).GetRankDecision(decReq); !errors.Is(err, errRankDecisionSubjectRequired) {
		t.Fatalf("两个键都为空时应拒绝，got %v", err)
	}
	// 两次被拒的查询都没打下游；recall 侧唯一的 1 次调用来自上面那次合法读取。
	if rc.calls != 1 || rk.calls != 0 {
		t.Fatalf("被拒的请求不应产生下游调用：recall=%d rank=%d", rc.calls, rk.calls)
	}
}

// TestRecommendShapeGates 固定「网关只做形态门槛」这条边界：下面断言的全是形态/符号问题
// （负数、缺必填字符串、时间窗倒置、枚举 0 值），不包括任何领域结论（池键语法、版本状态机、
// keep_versions 下限、权重和、分桶区间合法性都在服务侧）。
func TestRecommendShapeGates(t *testing.T) {
	cases := []struct {
		name   string
		route  string
		mutate func(t *testing.T, body any)
		want   string
	}{
		{"快照 version 为负", "getPoolSnapshot", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolSnapshot](t, b).Version = -1
		}, "version must be >= 0"},
		{"快照 pn 为负", "getPoolSnapshot", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolSnapshot](t, b).Pn = -1
		}, "pn must be >= 0"},
		{"快照 ps 为负", "getPoolSnapshot", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolSnapshot](t, b).Ps = -50
		}, "ps must be >= 0"},
		{"版本台账 limit 为负", "listPoolVersions", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolVersionList](t, b).Limit = -1
		}, "limit must be >= 0"},
		{"配置 mid 为负", "getRecallConfig", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendRecallConfig](t, b).Mid = -1
		}, "mid must be >= 0"},
		{"日志窗口倒置", "listRecallRequestLogs", func(t *testing.T, b any) {
			p := recommendCast[*types.ParamRecommendRecallLogList](t, b)
			p.FromTime, p.ToTime = 200, 100
		}, "from_time must be <= to_time"},
		{"日志窗口为负", "listRecallRequestLogs", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendRecallLogList](t, b).ToTime = -1
		}, "from_time and to_time must be >= 0"},
		{"决策分页 pn 为负", "listRankDecisions", func(t *testing.T, b any) {
			recommendCast[*types.ParamRankDecisionList](t, b).Pn = -2
		}, "pn must be >= 0"},
		{"决策分页 ps 为负", "listRankDecisions", func(t *testing.T, b any) {
			recommendCast[*types.ParamRankDecisionList](t, b).Ps = -2
		}, "ps must be >= 0"},
		{"写入 version 为负", "upsertPoolItems", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolItemUpsert](t, b).Version = -1
		}, "version must be >= 0"},
		{"写入 batch_id 空白", "upsertPoolItems", func(t *testing.T, b any) {
			recommendBlank(t, b, "BatchId")
		}, "batch_id required"},
		{"写入 schema_version 为负", "upsertPoolItems", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolItemUpsert](t, b).SchemaVersion = -1
		}, "schema_version must be >= 0"},
		{"发布 reason 空白", "publishPoolVersion", func(t *testing.T, b any) {
			recommendBlank(t, b, "Reason")
		}, "reason required"},
		{"回滚 target_version 为负", "rollbackPoolVersion", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolVersionRollback](t, b).TargetVersion = -3
		}, "target_version must be >= 0"},
		{"清理 keep_versions 为负", "prunePoolVersions", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolVersionPrune](t, b).KeepVersions = -1
		}, "keep_versions must be >= 0"},
		{"清理 max_rows 为负", "prunePoolVersions", func(t *testing.T, b any) {
			recommendCast[*types.ParamRecommendPoolVersionPrune](t, b).MaxRows = -1
		}, "max_rows must be >= 0"},
		{"模型 model_key 空白", "upsertModelVersion", func(t *testing.T, b any) {
			recommendBlank(t, b, "ModelKey")
		}, "model_key required"},
		{"模型 version 空白", "upsertModelVersion", func(t *testing.T, b any) {
			recommendBlank(t, b, "Version")
		}, "version required"},
		{"模型 feature_config_version 空白", "upsertModelVersion", func(t *testing.T, b any) {
			recommendBlank(t, b, "FeatureConfigVersion")
		}, "feature_config_version required"},
		{"模型目标状态 UNSPECIFIED", "setModelVersionState", func(t *testing.T, b any) {
			recommendCast[*types.ParamRankModelStateSet](t, b).TargetState = 0
		}, "target_state required (0 = UNSPECIFIED)"},
		{"模型目标状态为负", "setModelVersionState", func(t *testing.T, b any) {
			recommendCast[*types.ParamRankModelStateSet](t, b).TargetState = -1
		}, "target_state required"},
		{"特征 config_version 空白", "upsertFeatureConfig", func(t *testing.T, b any) {
			recommendBlank(t, b, "ConfigVersion")
		}, "config_version required"},
		{"实验 exp_key 空白", "upsertExperiment", func(t *testing.T, b any) {
			recommendBlank(t, b, "ExpKey")
		}, "exp_key required"},
		{"实验 variant_key 空白", "upsertExperiment", func(t *testing.T, b any) {
			recommendBlank(t, b, "VariantKey")
		}, "variant_key required"},
		{"实验 bucket_start 为负", "upsertExperiment", func(t *testing.T, b any) {
			recommendCast[*types.ParamRankExperimentUpsert](t, b).BucketStart = -1
		}, "bucket_start must be >= 0"},
		{"实验 bucket_end 为负", "upsertExperiment", func(t *testing.T, b any) {
			recommendCast[*types.ParamRankExperimentUpsert](t, b).BucketEnd = -1
		}, "bucket_end must be >= 0"},
		{"实验 end_at 为负", "upsertExperiment", func(t *testing.T, b any) {
			recommendCast[*types.ParamRankExperimentUpsert](t, b).EndAt = -1
		}, "end_at must be >= 0"},
		{"实验目标状态 UNSPECIFIED", "setExperimentState", func(t *testing.T, b any) {
			recommendCast[*types.ParamRankExperimentStateSet](t, b).TargetState = 0
		}, "target_state required (0 = UNSPECIFIED)"},
	}
	ctx := recommendSessionCtx()
	for _, tc := range cases {
		rt := recommendRouteByName(t, tc.route)
		rc, rk := &recallAdminFake{}, &rankAdminFake{}
		body := rt.newBody()
		tc.mutate(t, body)
		err := rt.call(ctx, recommendSvc(rc, rk), body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s：%s 期望错误含 %q，got %v", tc.name, tc.route, tc.want, err)
		}
		if !strings.HasPrefix(err.Error(), "gateway/admin:") {
			t.Fatalf("%s：错误消息应带 gateway/admin 前缀以便定位，got %v", tc.name, err)
		}
		if rc.calls != 0 || rk.calls != 0 {
			t.Fatalf("%s：形态不过却打了下游（recall=%d rank=%d）", tc.name, rc.calls, rk.calls)
		}
	}
}

func TestRecommendReadRoutesDoNotRequireSession(t *testing.T) {
	// 读面不挂 AdminPermission（避免操作读放大），因此没有会话身份也必须能读；
	// 这同时钉住「读路由不写操作者」这条口径——网关不会凭空造一个 operator 传给服务。
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rc.snapshot = &recallrpc.GetPoolSnapshotReply{}
	rc.versions = &recallrpc.ListPoolVersionsReply{}
	rc.config = &recallrpc.GetRecallConfigReply{}
	rc.log = &recallrpc.GetRecallRequestLogReply{}
	rc.logs = &recallrpc.ListRecallRequestLogsReply{}
	rk.decision = &rankrpc.GetRankDecisionReply{}
	rk.decisions = &rankrpc.ListRankDecisionsReply{}
	rk.runtime = &rankrpc.GetRankRuntimeConfigReply{}
	s := recommendSvc(rc, rk)
	for _, rt := range recommendRoutes() {
		if rt.write {
			continue
		}
		if err := rt.call(context.Background(), s, rt.newBody()); err != nil {
			t.Fatalf("读入口 %s 不该要求会话身份，got %v", rt.name, err)
		}
	}
	if rc.calls != 5 || rk.calls != 3 {
		t.Fatalf("读调用数应为 5/3，实际 %d/%d", rc.calls, rk.calls)
	}
}

func TestRecommendWriteSessionGateRunsBeforeShapeGate(t *testing.T) {
	// 写入口顺序：客户端未配置 → 请求体缺失 → 会话身份 → 形态。
	// 这里证明「无会话」优先于「池寻址不全」：fail-closed 的鉴权结论不能被参数错误盖掉。
	rc, rk := &recallAdminFake{}, &rankAdminFake{}
	rt := recommendRouteByName(t, "publishPoolVersion")
	body := recommendPublishBody()
	body.Pool.Source, body.Pool.PoolKey = 0, ""
	err := rt.call(context.Background(), recommendSvc(rc, rk), body)
	if !errors.Is(err, errRecommendSessionRequired) {
		t.Fatalf("应先拒会话身份，got %v", err)
	}
	if rc.calls != 0 {
		t.Fatalf("被拒的写请求打了下游 %d 次", rc.calls)
	}
	// 有会话时同一份入参落到池寻址门槛上，证明两道门槛都在。
	err = rt.call(recommendSessionCtx(), recommendSvc(rc, rk), body)
	if !errors.Is(err, errRecommendPoolRefRequired) {
		t.Fatalf("有会话时应报池寻址不全，got %v", err)
	}
}
