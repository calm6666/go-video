package logic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"google.golang.org/grpc"
)

// 本文件只锁 gateway/admin 面向 creator-revenue 的 11 条路由口径，刻意不断言本域业务结论
// （AGENTS.md §1/§5/§7）：单价护栏上限、min_quantity 与 monthly_cap_minor 的组合合法性、
// rule_code 唯一性、规则状态机怎么迁、period 是不是 YYYYMM/是不是未来周期、
// 「这一期有没有台账」「这个作者配不配暂停」「这张单现在能不能确认」「封顶砍掉多少」——
// 全部由 services/creator-revenue 判定，网关自己复算一遍只会和服务口径漂移
// （本轮刻意**不**挡的三位都留在用例里：单价负数、currency 留空、force_void_confirmed
// 的条件必填 reason）。
//
// 锁死的是网关自己的责任边界：
//  1. 不伪造成功/成功台账：没配客户端时 11 条路由一律 not configured，绝不回空规则列表、
//     found=false、空结算台账或「这一期没有单」；下游错误原样上抛。nil 客户端一被调用就会
//     在空接口上 panic，所以「跑完并拿到哨兵错误」就是「零 RPC」的机器可检证明。
//  2. 主体不信任请求体：五条写路由（rule/upsert、rule/state、enrollment/state、
//     settlement/generate、settlement/confirm）的 operator 一律由会话渲染成
//     gateway/admin:<admin_id>，表单自称的 9001 只能进日志当线索；
//     无会话或 AdminID<=0 即 fail-closed，一次调用都不发。
//  3. 幂等键与 0 哨兵原样下传：idempotency_key→request_id 不改写；
//     page/size/mid/aid/state/source_type/version/rule_id/min_quantity/
//     monthly_cap_minor/effective_from/expected_version=0 都是合法哨兵，网关不代填、
//     不裁剪，分页三元组照抄服务回显而不是回显请求值。
//  4. 契约里没有的位不伪造：出金/提现/打款/退款到卡/发票/对账文件在 proto 里就没有方法，
//     五条写请求体里也不允许出现 payout/withdraw/paid/trace_id 位；
//     UpsertRevenueRule 与 ConfirmSettlement 的响应没有 duplicated 位，网关也不造一位。
//  5. 结算语义如实：payout_state 逐字转达服务值（本期恒 NOT_PAYABLE，但服务回 0 时
//     网关也不写死成 1）；confirm 只是「认账」，confirmed + failed_nos 原样回，
//     不合并成「全部成功」；generate 的 truncated 不得隐瞒。
//  6. 投影逐字段不丢：规则的 state/version/effective_from、参与行的 agreed_rule_version、
//     台账的 amount_minor 与 capped_amount_minor 两位、结算单的 cap_applied_minor 与
//     void_reason 是审计与对账的证据位，裁掉一位后台就得靠猜。
//
// 打桩方式与 membership/payment/order/coin 一致：内嵌生成的 client 接口 + 只覆盖本批用到的
// 11 个方法，其余方法（EnrollCreator/LeavePlan/GetEnrollment/RecordRevenueMetric/
// GetRevenueSummary）一旦被调用会 panic 在 nil 接口上——这正是 admin.api 里
// 「刻意不开的路由」那条边界的机器可检表达：加入/退出计划必须本人签（后台代签在争议时
// 不能当证据），计量事实的写入方是 spm/coin/cron（后台代写就污染计量源），
// GetEnrollment/GetRevenueSummary 是创作者本人视角。不建 gRPC 连接、不碰数据库。

var errCommerceRevenueDownstream = errors.New("commerce creator-revenue: downstream unavailable")

// 下面几条逐字照抄服务侧哨兵错误（services/creator-revenue/model/errors.go）的消息文本，
// 用来验证「服务的拒绝」在网关这一侧不被吞掉、不被折叠成空结果或 found=false。
// 不 import 服务 internal 包：测试只关心消息原样透出这一件事，本地副本就够。
var errRevenueNegativeUnitPrice = errors.New("creatorrevenue: unit_price_per_1000_minor must not be negative")
var errRevenueFuturePeriod = errors.New("creatorrevenue: period is in the future")
var errRevenueInvalidPeriod = errors.New("creatorrevenue: invalid period, expected YYYYMM")
var errRevenueQueryScopeRequired = errors.New("creatorrevenue: period or mid is required to bound the ledger scan")
var errRevenueSettlementNosRequired = errors.New("creatorrevenue: settlement_nos is required")
var errRevenueBatchTooLarge = errors.New("creatorrevenue: batch too large")
var errRevenueForceVoidReasonRequired = errors.New("creatorrevenue: reason is required when force_void_confirmed is true")
var errRevenueVersionConflict = errors.New("creatorrevenue: expected_version mismatch, reload and retry")
var errRevenueRuleStateTransition = errors.New("creatorrevenue: illegal rule state transition")
var errRevenueRuleTargetRequired = errors.New("creatorrevenue: rule_id or rule_code is required")
var errRevenueEnrollmentStateTransition = errors.New("creatorrevenue: illegal enrollment state transition")
var errRevenueInvalidRuleState = errors.New("creatorrevenue: invalid rule state")
var errRevenueSettlementForbidden = errors.New("creatorrevenue: settlement does not belong to the given mid")
var errRevenueRequestReplayed = errors.New("creatorrevenue: request_id already applied")

// --- creator-revenue fake ---

type commerceRevenueFake struct {
	creatorrevenuerpc.CreatorRevenueClient

	calls    int
	lastCall string
	err      error

	rulesReq       *creatorrevenuerpc.ListRevenueRulesReq
	rules          *creatorrevenuerpc.ListRevenueRulesReply
	ruleGetReq     *creatorrevenuerpc.GetRevenueRuleReq
	ruleGet        *creatorrevenuerpc.GetRevenueRuleReply
	enrollListReq  *creatorrevenuerpc.ListEnrollmentsReq
	enrollList     *creatorrevenuerpc.ListEnrollmentsReply
	metricsReq     *creatorrevenuerpc.ListRevenueMetricsReq
	metrics        *creatorrevenuerpc.ListRevenueMetricsReply
	settleListReq  *creatorrevenuerpc.ListSettlementsReq
	settleList     *creatorrevenuerpc.ListSettlementsReply
	settleGetReq   *creatorrevenuerpc.GetSettlementReq
	settleGet      *creatorrevenuerpc.GetSettlementReply
	upsertReq      *creatorrevenuerpc.UpsertRevenueRuleReq
	upserted       *creatorrevenuerpc.RevenueRuleInfo
	ruleStateReq   *creatorrevenuerpc.SetRevenueRuleStateReq
	ruleStateInfo  *creatorrevenuerpc.RevenueRuleInfo
	enrollStateReq *creatorrevenuerpc.SetEnrollmentStateReq
	enrollState    *creatorrevenuerpc.SetEnrollmentStateReply
	generateReq    *creatorrevenuerpc.GenerateSettlementReq
	generated      *creatorrevenuerpc.GenerateSettlementReply
	confirmReq     *creatorrevenuerpc.ConfirmSettlementReq
	confirmed      *creatorrevenuerpc.ConfirmSettlementReply
}

// record 记一次调用；返回 true 表示这次要模拟下游失败。
func (f *commerceRevenueFake) record(call string) bool {
	f.calls++
	f.lastCall = call
	return f.err != nil
}

func (f *commerceRevenueFake) ListRevenueRules(_ context.Context, in *creatorrevenuerpc.ListRevenueRulesReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.ListRevenueRulesReply, error) {
	f.rulesReq = in
	if f.record("ListRevenueRules") {
		return nil, f.err
	}
	return f.rules, nil
}

func (f *commerceRevenueFake) GetRevenueRule(_ context.Context, in *creatorrevenuerpc.GetRevenueRuleReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.GetRevenueRuleReply, error) {
	f.ruleGetReq = in
	if f.record("GetRevenueRule") {
		return nil, f.err
	}
	return f.ruleGet, nil
}

func (f *commerceRevenueFake) ListEnrollments(_ context.Context, in *creatorrevenuerpc.ListEnrollmentsReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.ListEnrollmentsReply, error) {
	f.enrollListReq = in
	if f.record("ListEnrollments") {
		return nil, f.err
	}
	return f.enrollList, nil
}

func (f *commerceRevenueFake) ListRevenueMetrics(_ context.Context, in *creatorrevenuerpc.ListRevenueMetricsReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.ListRevenueMetricsReply, error) {
	f.metricsReq = in
	if f.record("ListRevenueMetrics") {
		return nil, f.err
	}
	return f.metrics, nil
}

func (f *commerceRevenueFake) ListSettlements(_ context.Context, in *creatorrevenuerpc.ListSettlementsReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.ListSettlementsReply, error) {
	f.settleListReq = in
	if f.record("ListSettlements") {
		return nil, f.err
	}
	return f.settleList, nil
}

func (f *commerceRevenueFake) GetSettlement(_ context.Context, in *creatorrevenuerpc.GetSettlementReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.GetSettlementReply, error) {
	f.settleGetReq = in
	if f.record("GetSettlement") {
		return nil, f.err
	}
	return f.settleGet, nil
}

func (f *commerceRevenueFake) UpsertRevenueRule(_ context.Context, in *creatorrevenuerpc.UpsertRevenueRuleReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.RevenueRuleInfo, error) {
	f.upsertReq = in
	if f.record("UpsertRevenueRule") {
		return nil, f.err
	}
	return f.upserted, nil
}

func (f *commerceRevenueFake) SetRevenueRuleState(_ context.Context, in *creatorrevenuerpc.SetRevenueRuleStateReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.RevenueRuleInfo, error) {
	f.ruleStateReq = in
	if f.record("SetRevenueRuleState") {
		return nil, f.err
	}
	return f.ruleStateInfo, nil
}

func (f *commerceRevenueFake) SetEnrollmentState(_ context.Context, in *creatorrevenuerpc.SetEnrollmentStateReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.SetEnrollmentStateReply, error) {
	f.enrollStateReq = in
	if f.record("SetEnrollmentState") {
		return nil, f.err
	}
	return f.enrollState, nil
}

func (f *commerceRevenueFake) GenerateSettlement(_ context.Context, in *creatorrevenuerpc.GenerateSettlementReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.GenerateSettlementReply, error) {
	f.generateReq = in
	if f.record("GenerateSettlement") {
		return nil, f.err
	}
	return f.generated, nil
}

func (f *commerceRevenueFake) ConfirmSettlement(_ context.Context, in *creatorrevenuerpc.ConfirmSettlementReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.ConfirmSettlementReply, error) {
	f.confirmReq = in
	if f.record("ConfirmSettlement") {
		return nil, f.err
	}
	return f.confirmed, nil
}

func commerceRevenueSvc(fake creatorrevenuerpc.CreatorRevenueClient) *svc.ServiceContext {
	return &svc.ServiceContext{CreatorRevenue: fake}
}

// --- 会话身份 ---

// commerceRevenueSession 是中间件判定通过后写入 context 的会话身份。
// admin_id=77 与表单里自称的 operator=9001 故意不同，用来验证「谁赢」。
func commerceRevenueSession() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77, Roles: []string{"revenue_operator"},
	})
}

// --- 台账样例（每位都非零，投影丢一位就能被 diff 点名）---

func commerceRevenueFullRule() *creatorrevenuerpc.RevenueRuleInfo {
	return &creatorrevenuerpc.RevenueRuleInfo{
		RuleId: 5001, RuleCode: "vip_watch_minute",
		SourceType:             creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH,
		Name:                   "会员有效观看每分钟单价",
		Description:            "按 spm 给出的有效观看分钟折算（不含广告参数）",
		UnitPricePer_1000Minor: 120, Currency: "CNY", Unit: "minute",
		MinQuantity: 60, MonthlyCapMinor: 500000,
		State:         creatorrevenuerpc.RuleState_RULE_STATE_DRAFT, // DRAFT：对未来没有任何影响
		EffectiveFrom: 1700000000, Version: 4,
		Ctime: 1700000000, Mtime: 1700000600,
		CreatedBy: "gateway/admin:77", UpdatedBy: "gateway/admin:77",
	}
}

func commerceRevenueFullEnrollment() *creatorrevenuerpc.EnrollmentInfo {
	return &creatorrevenuerpc.EnrollmentInfo{
		Mid:               10001,
		State:             creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_SUSPENDED,
		AgreedRuleVersion: 4, // 本人参加时看到的规则版本，争议复核的证据位
		EnrolledAt:        1700000000, LeftAt: 0, UpdatedAt: 1700000800,
		Operator: "gateway/admin:77", Remark: "违规搬运，本期不结算",
	}
}

func commerceRevenueFullMetric() *creatorrevenuerpc.RevenueMetricInfo {
	return &creatorrevenuerpc.RevenueMetricInfo{
		MetricId: 9001, Period: "202608", Mid: 10001, Aid: 20001,
		SourceType: creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH,
		RuleCode:   "vip_watch_minute", RuleVersion: 4,
		Quantity: 12345, Unit: "minute",
		AmountMinor: 1481, CappedAmountMinor: 1200, // 封顶砍掉 281 分，两位必须并存
		SourceDetail: "有效播放 12,345 分钟（来自 spm，不含 PII）",
		Ctime:        1700000000, Mtime: 1700000600,
	}
}

func commerceRevenueFullSettlement() *creatorrevenuerpc.SettlementInfo {
	return &creatorrevenuerpc.SettlementInfo{
		SettlementNo: "st_202608_10001", Period: "202608", Mid: 10001,
		AmountMinor: 1200, CapAppliedMinor: 281, Currency: "CNY", MetricCount: 3,
		State:       creatorrevenuerpc.SettlementState_SETTLEMENT_STATE_DRAFT,
		PayoutState: creatorrevenuerpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE, // 恒此值
		ConfirmedAt: 1700000900, ConfirmedBy: "gateway/admin:77",
		VoidReason: "",
		Ctime:      1700000000, Mtime: 1700000900,
	}
}

func commerceRevenueFullSettlementItem() *creatorrevenuerpc.SettlementItem {
	return &creatorrevenuerpc.SettlementItem{
		SourceType:  creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_COIN,
		RuleCode:    "coin_received",
		Quantity:    800,
		AmountMinor: 400,
	}
}

// --- 写路由的合法入参样例（每个用例取新副本，避免互相污染）---

func commerceRevenueGoodRuleUpsert() *types.ParamRevenueRuleUpsert {
	return &types.ParamRevenueRuleUpsert{
		RuleId:                5001,
		RuleCode:              "vip_watch_minute",
		SourceType:            int32(creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH),
		Name:                  "会员有效观看每分钟单价",
		Description:           "按 spm 有效观看分钟折算",
		UnitPricePer1000Minor: 120,
		Currency:              "CNY",
		Unit:                  "minute",
		MinQuantity:           60,
		MonthlyCapMinor:       500000,
		EffectiveFrom:         1700000000,
		ExpectedVersion:       3,
		Reason:                "运营调价（工单 WO-2026-0924-03）",
		Operator:              9001,
		IdempotencyKey:        "k-rule-upsert-1",
		TraceId:               "t-rule-upsert-1",
	}
}

func commerceRevenueGoodRuleState() *types.ParamRevenueRuleState {
	return &types.ParamRevenueRuleState{
		RuleId:          5001,
		TargetState:     int32(creatorrevenuerpc.RuleState_RULE_STATE_ACTIVE),
		ExpectedVersion: 4,
		Reason:          "复核通过，202609 起按新版折算",
		Operator:        9001,
		IdempotencyKey:  "k-rule-state-1",
		TraceId:         "t-rule-state-1",
	}
}

func commerceRevenueGoodEnrollmentState() *types.ParamRevenueEnrollmentState {
	return &types.ParamRevenueEnrollmentState{
		Mid:            10001,
		TargetState:    int32(creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_SUSPENDED),
		Reason:         "违规搬运处置，处置单 DR-2026-09-11",
		Operator:       9001,
		IdempotencyKey: "k-enroll-state-1",
		TraceId:        "t-enroll-state-1",
	}
}

func commerceRevenueGoodGenerate() *types.ParamRevenueSettlementGenerate {
	return &types.ParamRevenueSettlementGenerate{
		Period:             "202608",
		Mid:                0, // 0 = 该周期全量出单
		ForceVoidConfirmed: false,
		Reason:             "",
		Operator:           9001,
		IdempotencyKey:     "k-generate-1",
		TraceId:            "t-generate-1",
	}
}

func commerceRevenueGoodConfirm() *types.ParamRevenueSettlementConfirm {
	return &types.ParamRevenueSettlementConfirm{
		SettlementNos:  []string{"st_202608_10001", "st_202608_10002"},
		Reason:         "202608 期已逐张复核",
		Operator:       9001,
		IdempotencyKey: "k-confirm-1",
		TraceId:        "t-confirm-1",
	}
}

// --- 路由表 ---

type revenueRoute struct {
	name string
	run  func(ctx context.Context, s *svc.ServiceContext) error
}

var commerceRevenueRoutes = []revenueRoute{
	{"rule/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueRuleListLogic(ctx, s).RevenueRuleList(&types.ParamRevenueRuleList{Page: 1, Size: 20})
		return err
	}},
	{"rule/get", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueRuleGetLogic(ctx, s).RevenueRuleGet(&types.ParamRevenueRuleGet{
			RuleCode: "vip_watch_minute",
		})
		return err
	}},
	{"enrollment/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueEnrollmentListLogic(ctx, s).RevenueEnrollmentList(&types.ParamRevenueEnrollmentList{
			Page: 1, Size: 20,
		})
		return err
	}},
	{"metric/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueMetricListLogic(ctx, s).RevenueMetricList(&types.ParamRevenueMetricList{
			Period: "202608", Page: 1, Size: 20,
		})
		return err
	}},
	{"settlement/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueSettlementListLogic(ctx, s).RevenueSettlementList(&types.ParamRevenueSettlementList{
			Period: "202608", Page: 1, Size: 20,
		})
		return err
	}},
	{"settlement/get", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueSettlementGetLogic(ctx, s).RevenueSettlementGet(&types.ParamRevenueSettlementGet{
			SettlementNo: "st_202608_10001",
		})
		return err
	}},
	{"rule/upsert", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(commerceRevenueGoodRuleUpsert())
		return err
	}},
	{"rule/state", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueRuleStateLogic(ctx, s).RevenueRuleState(commerceRevenueGoodRuleState())
		return err
	}},
	{"enrollment/state", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueEnrollmentStateLogic(ctx, s).RevenueEnrollmentState(commerceRevenueGoodEnrollmentState())
		return err
	}},
	{"settlement/generate", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueSettlementGenerateLogic(ctx, s).RevenueSettlementGenerate(commerceRevenueGoodGenerate())
		return err
	}},
	{"settlement/confirm", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewRevenueSettlementConfirmLogic(ctx, s).RevenueSettlementConfirm(commerceRevenueGoodConfirm())
		return err
	}},
}

// commerceRevenueIsWriteRoute 只认真正挂了 AdminPermission 的五条写路由（admin.api 写面段）。
func commerceRevenueIsWriteRoute(name string) bool {
	switch name {
	case "rule/upsert", "rule/state", "enrollment/state", "settlement/generate", "settlement/confirm":
		return true
	default:
		return false
	}
}

// runRevenueWithoutClient 跑一条「客户端位为 nil」的路由。CreatorRevenue 是接口字段，
// nil 接口上一被调用就会 panic，所以能正常返回错误就等价于「一次 RPC 都没发」——
// 这比在 fake 上数计数器更硬（计数器版本见 fail-closed 用例）。
func runRevenueWithoutClient(t *testing.T, route revenueRoute, ctx context.Context) error {
	t.Helper()
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("%s: 未配客户端却试图下传 RPC（panic: %v）", route.name, p)
			}
		}()
		err = route.run(ctx, &svc.ServiceContext{})
	}()
	return err
}

// newReplyFake 给 11 条路由都备好一份「成功且每位非零」的回复。
func newReplyFake() *commerceRevenueFake {
	return &commerceRevenueFake{
		rules: &creatorrevenuerpc.ListRevenueRulesReply{
			Rules: []*creatorrevenuerpc.RevenueRuleInfo{commerceRevenueFullRule()},
			Total: 7, Page: 3, Size: 100,
		},
		ruleGet: &creatorrevenuerpc.GetRevenueRuleReply{Found: true, Rule: commerceRevenueFullRule()},
		enrollList: &creatorrevenuerpc.ListEnrollmentsReply{
			Enrollments: []*creatorrevenuerpc.EnrollmentInfo{commerceRevenueFullEnrollment()},
			Total:       7, Page: 3, Size: 100,
		},
		metrics: &creatorrevenuerpc.ListRevenueMetricsReply{
			Metrics: []*creatorrevenuerpc.RevenueMetricInfo{commerceRevenueFullMetric()},
			Total:   7, Page: 3, Size: 100,
		},
		settleList: &creatorrevenuerpc.ListSettlementsReply{
			Settlements: []*creatorrevenuerpc.SettlementInfo{commerceRevenueFullSettlement()},
			Total:       7, Page: 3, Size: 100,
		},
		settleGet: &creatorrevenuerpc.GetSettlementReply{
			Found: true, Settlement: commerceRevenueFullSettlement(),
			Items: []*creatorrevenuerpc.SettlementItem{commerceRevenueFullSettlementItem()},
		},
		upserted:      commerceRevenueFullRule(),
		ruleStateInfo: commerceRevenueFullRule(),
		enrollState: &creatorrevenuerpc.SetEnrollmentStateReply{
			Duplicated: false, Enrollment: commerceRevenueFullEnrollment(),
		},
		generated: &creatorrevenuerpc.GenerateSettlementReply{
			Duplicated: false, Generated: 2,
			Settlements: []*creatorrevenuerpc.SettlementInfo{commerceRevenueFullSettlement()},
			Truncated:   true,
		},
		confirmed: &creatorrevenuerpc.ConfirmSettlementReply{Confirmed: 1, FailedNos: []string{"st_202608_10002"}},
	}
}

// --- 1. 不伪造成功 ---

func TestCommerceRevenueAllRoutes_NilClientDoesNotFakeLedger(t *testing.T) {
	ctx := commerceRevenueSession()
	for _, route := range commerceRevenueRoutes {
		err := runRevenueWithoutClient(t, route, ctx)
		if !errors.Is(err, errRevenueServiceNotConfigured) {
			t.Fatalf("%s: 未配置客户端必须回 not configured 而不是空列表/found=false，实际 %v", route.name, err)
		}
		if !strings.Contains(err.Error(), "creator-revenue") {
			t.Fatalf("%s: 错误消息要点名是哪个下游没接，实际 %s", route.name, err.Error())
		}
	}
	if len(commerceRevenueRoutes) != 11 {
		t.Fatalf("本域应为 11 条路由，实际 %d", len(commerceRevenueRoutes))
	}
}

func TestCommerceRevenueAllRoutes_DownstreamErrorPropagatesVerbatim(t *testing.T) {
	bad := &commerceRevenueFake{err: errCommerceRevenueDownstream}
	ctx := commerceRevenueSession()
	wantCalls := []string{
		"ListRevenueRules", "GetRevenueRule", "ListEnrollments", "ListRevenueMetrics",
		"ListSettlements", "GetSettlement", "UpsertRevenueRule", "SetRevenueRuleState",
		"SetEnrollmentState", "GenerateSettlement", "ConfirmSettlement",
	}
	for i, route := range commerceRevenueRoutes {
		err := route.run(ctx, commerceRevenueSvc(bad))
		if !errors.Is(err, errCommerceRevenueDownstream) {
			t.Fatalf("%s: 下游错误必须原样上抛（不得折成成功或空数据），实际 %v", route.name, err)
		}
		if bad.calls != i+1 {
			t.Fatalf("%s: 每条路由都要真打一次下游（累计 %d 次，lastCall=%s）", route.name, bad.calls, bad.lastCall)
		}
		if bad.lastCall != wantCalls[i] {
			t.Fatalf("%s: 该调 %s，实际调了 %s", route.name, wantCalls[i], bad.lastCall)
		}
	}
	if bad.calls != 11 {
		t.Fatalf("十一条路由各一次，实际 %d", bad.calls)
	}
}

// TestCommerceRevenueAdminGateway_DeliberatelyUnwiredMethodsStayUntouched 锁 admin.api
// 「刻意不开的路由」那段：EnrollCreator/LeavePlan/GetEnrollment/RecordRevenueMetric/
// GetRevenueSummary 都没有后台路由，任何一条 revenue 路由都不准顺手调它们。
// fake 只实现了 11 个方法，多调一个就会 panic 在 nil 接口上（recover 里点名失败）。
func TestCommerceRevenueAdminGateway_DeliberatelyUnwiredMethodsStayUntouched(t *testing.T) {
	fake := newReplyFake()
	ctx := commerceRevenueSession()
	for _, route := range commerceRevenueRoutes {
		before := fake.calls
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%s: 调到了本域不开的方法（panic: %v）", route.name, p)
				}
			}()
			if err := route.run(ctx, commerceRevenueSvc(fake)); err != nil {
				t.Fatalf("%s: %v", route.name, err)
			}
		}()
		if fake.calls != before+1 {
			t.Fatalf("%s: 一条路由恰好一次下游调用（before=%d after=%d）", route.name, before, fake.calls)
		}
	}
}

// --- 2. 操作者身份 ---

// TestCommerceRevenueWriteRoutes_OperatorAlwaysFromSession 覆盖五条写裁决：
// 进 RPC 的 operator 永远是会话渲染值，request_id 永远是幂等键原值
// （五个 Req 都没有 trace 字段，trace_id 只进日志）。
func TestCommerceRevenueWriteRoutes_OperatorAlwaysFromSession(t *testing.T) {
	fake := newReplyFake()
	svcCtx := commerceRevenueSvc(fake)
	ctx := commerceRevenueSession()
	if _, err := NewRevenueRuleUpsertLogic(ctx, svcCtx).RevenueRuleUpsert(commerceRevenueGoodRuleUpsert()); err != nil {
		t.Fatalf("rule/upsert: %v", err)
	}
	if _, err := NewRevenueRuleStateLogic(ctx, svcCtx).RevenueRuleState(commerceRevenueGoodRuleState()); err != nil {
		t.Fatalf("rule/state: %v", err)
	}
	if _, err := NewRevenueEnrollmentStateLogic(ctx, svcCtx).RevenueEnrollmentState(commerceRevenueGoodEnrollmentState()); err != nil {
		t.Fatalf("enrollment/state: %v", err)
	}
	if _, err := NewRevenueSettlementGenerateLogic(ctx, svcCtx).RevenueSettlementGenerate(commerceRevenueGoodGenerate()); err != nil {
		t.Fatalf("settlement/generate: %v", err)
	}
	if _, err := NewRevenueSettlementConfirmLogic(ctx, svcCtx).RevenueSettlementConfirm(commerceRevenueGoodConfirm()); err != nil {
		t.Fatalf("settlement/confirm: %v", err)
	}

	wantOperator := "gateway/admin:77" // 会话 admin_id=77；表单自称的 9001 必须作废
	cases := []struct {
		route     string
		operator  string
		requestID string
		want      string
	}{
		{"rule/upsert", fake.upsertReq.GetOperator(), fake.upsertReq.GetRequestId(), "k-rule-upsert-1"},
		{"rule/state", fake.ruleStateReq.GetOperator(), fake.ruleStateReq.GetRequestId(), "k-rule-state-1"},
		{"enrollment/state", fake.enrollStateReq.GetOperator(), fake.enrollStateReq.GetRequestId(), "k-enroll-state-1"},
		{"settlement/generate", fake.generateReq.GetOperator(), fake.generateReq.GetRequestId(), "k-generate-1"},
		{"settlement/confirm", fake.confirmReq.GetOperator(), fake.confirmReq.GetRequestId(), "k-confirm-1"},
	}
	for _, c := range cases {
		if c.operator != wantOperator {
			t.Fatalf("%s: operator 必须来自会话而不是请求体，实际 %q", c.route, c.operator)
		}
		if c.requestID != c.want {
			t.Fatalf("%s: idempotency_key 要原样映射成 request_id，实际 %q want %q", c.route, c.requestID, c.want)
		}
	}
}

// TestCommerceRevenueWriteRoutes_IdempotencyKeyRelayedByteForByte 幂等键是唯一能让「同一次
// 点击」在下游退化成一次重放的位，任何"顺手清理"都会造出一个新键：首尾空格、制表符、中文、
// 引号、超长串都必须逐字节进到 request_id。反向断言同样关键——空白键被拒且 calls==0，
// 网关绝不会「好心」生成一个 UUID 顶上（那等于每次重试都是新请求，会重复改价/重复出单）。
func TestCommerceRevenueWriteRoutes_IdempotencyKeyRelayedByteForByte(t *testing.T) {
	ctx := commerceRevenueSession()
	routes := []struct {
		name string
		call func(context.Context, *svc.ServiceContext, string) error
		got  func(*commerceRevenueFake) string
	}{
		{"rule/upsert", func(c context.Context, s *svc.ServiceContext, k string) error {
			r := commerceRevenueGoodRuleUpsert()
			r.IdempotencyKey = k
			_, err := NewRevenueRuleUpsertLogic(c, s).RevenueRuleUpsert(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.upsertReq.GetRequestId() }},
		{"rule/state", func(c context.Context, s *svc.ServiceContext, k string) error {
			r := commerceRevenueGoodRuleState()
			r.IdempotencyKey = k
			_, err := NewRevenueRuleStateLogic(c, s).RevenueRuleState(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.ruleStateReq.GetRequestId() }},
		{"enrollment/state", func(c context.Context, s *svc.ServiceContext, k string) error {
			r := commerceRevenueGoodEnrollmentState()
			r.IdempotencyKey = k
			_, err := NewRevenueEnrollmentStateLogic(c, s).RevenueEnrollmentState(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.enrollStateReq.GetRequestId() }},
		{"settlement/generate", func(c context.Context, s *svc.ServiceContext, k string) error {
			r := commerceRevenueGoodGenerate()
			r.IdempotencyKey = k
			_, err := NewRevenueSettlementGenerateLogic(c, s).RevenueSettlementGenerate(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.generateReq.GetRequestId() }},
		{"settlement/confirm", func(c context.Context, s *svc.ServiceContext, k string) error {
			r := commerceRevenueGoodConfirm()
			r.IdempotencyKey = k
			_, err := NewRevenueSettlementConfirmLogic(c, s).RevenueSettlementConfirm(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.confirmReq.GetRequestId() }},
	}
	// 五个都是「网关最容易顺手处理掉」的形状。
	keys := []string{"  padded key  ", "a b\tc", "规则-中文#1", `quote"and,comma`, strings.Repeat("k", 200)}
	for _, route := range routes {
		for _, key := range keys {
			fake := newReplyFake()
			if err := route.call(ctx, commerceRevenueSvc(fake), key); err != nil {
				t.Fatalf("%s/key=%q: %v", route.name, key, err)
			}
			if got := route.got(fake); got != key {
				t.Fatalf("%s: idempotency_key 要逐字节下传，实际 %q want %q", route.name, got, key)
			}
		}
		// 只含空白的键：拒绝，且一次 RPC 都不发（等价于「没有键」，不能替它编一个）。
		fake := newReplyFake()
		err := route.call(ctx, commerceRevenueSvc(fake), " \t")
		if err == nil || !strings.Contains(err.Error(), "idempotency_key") {
			t.Fatalf("%s: 空白 idempotency_key 要点名拒绝，实际 %v", route.name, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 被拒的入参不该打到下游（calls=%d lastCall=%s）", route.name, fake.calls, fake.lastCall)
		}
	}
}

// TestCommerceRevenueWriteRoutes_ReasonRelayedWithoutTruncation reason 是 §5 的审计证据正文，
// 列宽归服务判（超了要报错让人重写），网关「贴心截断」只会留下一条读不通的审计行。
func TestCommerceRevenueWriteRoutes_ReasonRelayedWithoutTruncation(t *testing.T) {
	ctx := commerceRevenueSession()
	long := "运营复核结论：" + strings.Repeat("依据工单 WO-2026-0924-03 逐条核对。", 40)
	routes := []struct {
		name string
		call func(context.Context, *svc.ServiceContext) error
		got  func(*commerceRevenueFake) string
	}{
		{"rule/upsert", func(c context.Context, s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.Reason = long
			_, err := NewRevenueRuleUpsertLogic(c, s).RevenueRuleUpsert(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.upsertReq.GetReason() }},
		{"rule/state", func(c context.Context, s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleState()
			r.Reason = long
			_, err := NewRevenueRuleStateLogic(c, s).RevenueRuleState(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.ruleStateReq.GetReason() }},
		{"enrollment/state", func(c context.Context, s *svc.ServiceContext) error {
			r := commerceRevenueGoodEnrollmentState()
			r.Reason = long
			_, err := NewRevenueEnrollmentStateLogic(c, s).RevenueEnrollmentState(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.enrollStateReq.GetReason() }},
		{"settlement/confirm", func(c context.Context, s *svc.ServiceContext) error {
			r := commerceRevenueGoodConfirm()
			r.Reason = long
			_, err := NewRevenueSettlementConfirmLogic(c, s).RevenueSettlementConfirm(r)
			return err
		}, func(f *commerceRevenueFake) string { return f.confirmReq.GetReason() }},
	}
	for _, route := range routes {
		fake := newReplyFake()
		if err := route.call(ctx, commerceRevenueSvc(fake)); err != nil {
			t.Fatalf("%s: %v", route.name, err)
		}
		if got := route.got(fake); got != long {
			t.Fatalf("%s: reason 要原样下传（%d 字符），实际 %d 字符 %q",
				route.name, utf8.RuneCountInString(long), utf8.RuneCountInString(got), got)
		}
	}
}

// TestCommerceRevenueWriteRoutes_NoTraceOrClaimedOperatorDownstream 表单里的 trace_id 与
// 自称的 operator=9001 都只属于传输层：五个 Req 都没有对应字段，串到下游日志/请求体里
// 就等于把「未证实的后台主体」和「可被重放的追踪串」写进别人持有的审计事实。
func TestCommerceRevenueWriteRoutes_NoTraceOrClaimedOperatorDownstream(t *testing.T) {
	ctx := commerceRevenueSession()
	fake := newReplyFake()
	svcCtx := commerceRevenueSvc(fake)
	if _, err := NewRevenueRuleUpsertLogic(ctx, svcCtx).RevenueRuleUpsert(commerceRevenueGoodRuleUpsert()); err != nil {
		t.Fatalf("rule/upsert: %v", err)
	}
	if _, err := NewRevenueRuleStateLogic(ctx, svcCtx).RevenueRuleState(commerceRevenueGoodRuleState()); err != nil {
		t.Fatalf("rule/state: %v", err)
	}
	if _, err := NewRevenueEnrollmentStateLogic(ctx, svcCtx).RevenueEnrollmentState(commerceRevenueGoodEnrollmentState()); err != nil {
		t.Fatalf("enrollment/state: %v", err)
	}
	if _, err := NewRevenueSettlementGenerateLogic(ctx, svcCtx).RevenueSettlementGenerate(commerceRevenueGoodGenerate()); err != nil {
		t.Fatalf("settlement/generate: %v", err)
	}
	if _, err := NewRevenueSettlementConfirmLogic(ctx, svcCtx).RevenueSettlementConfirm(commerceRevenueGoodConfirm()); err != nil {
		t.Fatalf("settlement/confirm: %v", err)
	}
	reqs := []struct {
		name string
		s    string
	}{
		{"rule/upsert", fake.upsertReq.String()},
		{"rule/state", fake.ruleStateReq.String()},
		{"enrollment/state", fake.enrollStateReq.String()},
		{"settlement/generate", fake.generateReq.String()},
		{"settlement/confirm", fake.confirmReq.String()},
	}
	for _, req := range reqs {
		if !strings.Contains(req.s, "operator:") {
			// 先确认这条 Req 真的被记录并序列化出来了，否则下面的「不含」全是空断言。
			t.Fatalf("%s: 下游请求没被记录下来，防外溢断言会退化成空断言: %q", req.name, req.s)
		}
		for _, leak := range []string{"trace", "Trace", "9001", "admin_id"} {
			if strings.Contains(req.s, leak) {
				t.Fatalf("%s: 下游请求体不得出现 %q，实际 %s", req.name, leak, req.s)
			}
		}
	}
}

// TestCommerceRevenueWriteRoutes_FailClosedWithoutSession：拿不到会话身份 = 这条路由没被
// AdminPermission 保护（权限表/挂载漂移），必须在下传前拒绝，一次 RPC 都不能发。
// AdminID<=0 与「完全没有身份」都要挡住。
func TestCommerceRevenueWriteRoutes_FailClosedWithoutSession(t *testing.T) {
	sessions := []struct {
		name string
		ctx  context.Context
	}{
		{"无身份", context.Background()},
		{"身份非法（admin_id=0）", middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: 0})},
	}
	for _, route := range commerceRevenueRoutes {
		if !commerceRevenueIsWriteRoute(route.name) {
			continue
		}
		for _, session := range sessions {
			fake := newReplyFake()
			err := route.run(session.ctx, commerceRevenueSvc(fake))
			if !errors.Is(err, errRevenueSessionRequired) {
				t.Fatalf("%s/%s: 缺会话身份必须 fail-closed，实际 %v", route.name, session.name, err)
			}
			if fake.calls != 0 {
				t.Fatalf("%s/%s: fail-closed 却已经打了下游（calls=%d lastCall=%s）",
					route.name, session.name, fake.calls, fake.lastCall)
			}
		}
	}
}

// --- 3. 必填与不可能形状：逐个点名，且被拒的入参一次都不打到下游 ---

func TestCommerceRevenueWriteRoutes_RequiredFieldsGateBeforeDownstream(t *testing.T) {
	ctx := commerceRevenueSession()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"改价缺幂等键", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.IdempotencyKey = "  "
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "idempotency_key"},
		{"改价缺 rule_code", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.RuleCode = ""
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "rule_code"},
		{"改价缺 name", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.Name = " "
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "name"},
		{"改价缺 unit", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.Unit = ""
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "unit"},
		{"改价缺 reason（改单价是有后果的动作，必须留证）", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.Reason = ""
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "reason"},
		{"改价未选来源类型", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.SourceType = 0
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "source_type"},
		{"改价 operator 位非法（表单没填审计主体）", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.Operator = 0
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "operator"},
		{"生效缺 rule_id", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleState()
			r.RuleId = 0
			_, err := NewRevenueRuleStateLogic(ctx, s).RevenueRuleState(r)
			return err
		}, "rule_id"},
		{"生效缺 target_state（0 不是一次迁移）", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleState()
			r.TargetState = 0
			_, err := NewRevenueRuleStateLogic(ctx, s).RevenueRuleState(r)
			return err
		}, "target_state"},
		{"生效缺 reason", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleState()
			r.Reason = "   "
			_, err := NewRevenueRuleStateLogic(ctx, s).RevenueRuleState(r)
			return err
		}, "reason"},
		{"生效缺幂等键", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleState()
			r.IdempotencyKey = ""
			_, err := NewRevenueRuleStateLogic(ctx, s).RevenueRuleState(r)
			return err
		}, "idempotency_key"},
		{"暂停 mid=0（分成没有游客作者号）", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodEnrollmentState()
			r.Mid = 0
			_, err := NewRevenueEnrollmentStateLogic(ctx, s).RevenueEnrollmentState(r)
			return err
		}, "mid"},
		{"暂停缺 reason", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodEnrollmentState()
			r.Reason = ""
			_, err := NewRevenueEnrollmentStateLogic(ctx, s).RevenueEnrollmentState(r)
			return err
		}, "reason"},
		{"出单缺 period", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodGenerate()
			r.Period = " "
			_, err := NewRevenueSettlementGenerateLogic(ctx, s).RevenueSettlementGenerate(r)
			return err
		}, "period"},
		{"出单缺幂等键", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodGenerate()
			r.IdempotencyKey = ""
			_, err := NewRevenueSettlementGenerateLogic(ctx, s).RevenueSettlementGenerate(r)
			return err
		}, "idempotency_key"},
		{"确认缺 reason", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodConfirm()
			r.Reason = ""
			_, err := NewRevenueSettlementConfirmLogic(ctx, s).RevenueSettlementConfirm(r)
			return err
		}, "reason"},
		{"确认缺幂等键", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodConfirm()
			r.IdempotencyKey = "  "
			_, err := NewRevenueSettlementConfirmLogic(ctx, s).RevenueSettlementConfirm(r)
			return err
		}, "idempotency_key"},
	}
	for _, c := range cases {
		fake := newReplyFake()
		err := c.run(commerceRevenueSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误消息要点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

func TestCommerceRevenueAllRoutes_RejectImpossibleShapes(t *testing.T) {
	// 写路由需要会话（读路由忽略它），统一用会话身份，保证被拒的原因就是字段形状本身。
	ctx := commerceRevenueSession()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"规则列表 state 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueRuleListLogic(ctx, s).RevenueRuleList(&types.ParamRevenueRuleList{State: -1})
			return err
		}, "state"},
		{"规则列表 source_type 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueRuleListLogic(ctx, s).RevenueRuleList(&types.ParamRevenueRuleList{SourceType: -1})
			return err
		}, "source_type"},
		{"规则列表页码为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueRuleListLogic(ctx, s).RevenueRuleList(&types.ParamRevenueRuleList{Page: -1})
			return err
		}, "page"},
		{"规则列表页大小为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueRuleListLogic(ctx, s).RevenueRuleList(&types.ParamRevenueRuleList{Size: -2})
			return err
		}, "size"},
		{"规则读取 rule_id 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueRuleGetLogic(ctx, s).RevenueRuleGet(&types.ParamRevenueRuleGet{RuleId: -1})
			return err
		}, "rule_id"},
		{"规则读取 version 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueRuleGetLogic(ctx, s).RevenueRuleGet(&types.ParamRevenueRuleGet{
				RuleId: 5001, Version: -3,
			})
			return err
		}, "version"},
		{"参与名单 state 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueEnrollmentListLogic(ctx, s).RevenueEnrollmentList(&types.ParamRevenueEnrollmentList{State: -1})
			return err
		}, "state"},
		{"计量台账 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueMetricListLogic(ctx, s).RevenueMetricList(&types.ParamRevenueMetricList{Mid: -1})
			return err
		}, "mid"},
		{"计量台账 aid 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueMetricListLogic(ctx, s).RevenueMetricList(&types.ParamRevenueMetricList{Aid: -1})
			return err
		}, "aid"},
		{"计量台账 source_type 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueMetricListLogic(ctx, s).RevenueMetricList(&types.ParamRevenueMetricList{SourceType: -1})
			return err
		}, "source_type"},
		{"计量台账页码为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueMetricListLogic(ctx, s).RevenueMetricList(&types.ParamRevenueMetricList{Page: -1})
			return err
		}, "page"},
		{"结算单列表 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueSettlementListLogic(ctx, s).RevenueSettlementList(&types.ParamRevenueSettlementList{Mid: -1})
			return err
		}, "mid"},
		{"结算单列表 state 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueSettlementListLogic(ctx, s).RevenueSettlementList(&types.ParamRevenueSettlementList{State: -1})
			return err
		}, "state"},
		{"结算单详情缺单号", func(s *svc.ServiceContext) error {
			_, err := NewRevenueSettlementGetLogic(ctx, s).RevenueSettlementGet(&types.ParamRevenueSettlementGet{
				SettlementNo: "  ",
			})
			return err
		}, "settlement_no"},
		{"结算单详情 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewRevenueSettlementGetLogic(ctx, s).RevenueSettlementGet(&types.ParamRevenueSettlementGet{
				SettlementNo: "st_202608_10001", Mid: -1,
			})
			return err
		}, "mid"},
		{"改价 rule_id 为负", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.RuleId = -1
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "rule_id"},
		{"改价 min_quantity 为负", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.MinQuantity = -1
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "min_quantity"},
		{"改价 monthly_cap_minor 为负", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.MonthlyCapMinor = -1
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "monthly_cap_minor"},
		{"改价 effective_from 为负", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.EffectiveFrom = -1
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "effective_from"},
		{"改价 CAS 版本为负", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleUpsert()
			r.ExpectedVersion = -1
			_, err := NewRevenueRuleUpsertLogic(ctx, s).RevenueRuleUpsert(r)
			return err
		}, "expected_version"},
		{"生效 expected_version 为负", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodRuleState()
			r.ExpectedVersion = -1
			_, err := NewRevenueRuleStateLogic(ctx, s).RevenueRuleState(r)
			return err
		}, "expected_version"},
		{"暂停 mid 为负", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodEnrollmentState()
			r.Mid = -9
			_, err := NewRevenueEnrollmentStateLogic(ctx, s).RevenueEnrollmentState(r)
			return err
		}, "mid"},
		{"出单 mid 为负", func(s *svc.ServiceContext) error {
			r := commerceRevenueGoodGenerate()
			r.Mid = -1
			_, err := NewRevenueSettlementGenerateLogic(ctx, s).RevenueSettlementGenerate(r)
			return err
		}, "mid"},
	}
	for _, c := range cases {
		fake := newReplyFake()
		err := c.run(commerceRevenueSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 要在下传前拒掉并点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

// TestCommerceRevenueEnrollmentState_TargetStateWhitelist 锁 §5/§1：
// 本路由只处理 ENROLLED↔SUSPENDED。LEFT=2 是合法枚举值，但「退出计划」是创作者本人动作
// （LeavePlan 归 gateway/app），从后台塞进来等于代签退出；UNSPECIFIED/未知编号没有动作。
// 全部在下传前拒掉，且**不改写、不降级成 SUSPENDED**。
func TestCommerceRevenueEnrollmentState_TargetStateWhitelist(t *testing.T) {
	ctx := commerceRevenueSession()
	for _, targetState := range []int32{0, 2, 5, -1} {
		fake := newReplyFake()
		r := commerceRevenueGoodEnrollmentState()
		r.TargetState = targetState
		_, err := NewRevenueEnrollmentStateLogic(ctx, commerceRevenueSvc(fake)).RevenueEnrollmentState(r)
		if err == nil || !strings.Contains(err.Error(), "target_state") {
			t.Fatalf("target_state=%d 必须被拒并点名，实际 %v", targetState, err)
		}
		if fake.calls != 0 {
			t.Fatalf("target_state=%d: 非法目标态被发到下游了（lastCall=%s）", targetState, fake.lastCall)
		}
	}
	// 两个合法档位各自原样下传，网关不改写方向。
	for _, targetState := range []int32{
		int32(creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_ENROLLED),
		int32(creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_SUSPENDED),
	} {
		fake := newReplyFake()
		r := commerceRevenueGoodEnrollmentState()
		r.TargetState = targetState
		if _, err := NewRevenueEnrollmentStateLogic(ctx, commerceRevenueSvc(fake)).RevenueEnrollmentState(r); err != nil {
			t.Fatalf("target_state=%d 是合法档位: %v", targetState, err)
		}
		if fake.enrollStateReq.GetTargetState() != creatorrevenuerpc.EnrollmentState(targetState) {
			t.Fatalf("目标态被网关改写: %+v", fake.enrollStateReq)
		}
	}
}

// --- 4. 归服务的判定：网关一个都不复算 ---

// TestCommerceRevenueRuleUpsert_ServiceOwnedJudgementsPropagate 锁三条「看起来网关也该挡」
// 但实际归服务的规则（admin.api 与 proto 都写明了）：
//   - unit_price_per_1000_minor 负数：.api 注「负数由服务拒」→ 网关原样下传；
//   - currency 留空：服务按 DefaultCurrency 补齐 → 网关预先拒空会把这条默认路径堵死；
//   - 单价护栏上限 / 封顶护栏上限只有服务知道配置值。
func TestCommerceRevenueRuleUpsert_ServiceOwnedJudgementsPropagate(t *testing.T) {
	ctx := commerceRevenueSession()

	negative := newReplyFake()
	negative.err = errRevenueNegativeUnitPrice
	r1 := commerceRevenueGoodRuleUpsert()
	r1.UnitPricePer1000Minor = -1
	if _, err := NewRevenueRuleUpsertLogic(ctx, commerceRevenueSvc(negative)).RevenueRuleUpsert(r1); !errors.Is(err, errRevenueNegativeUnitPrice) {
		t.Fatalf("负单价要由服务拒并原样透出: %v", err)
	}
	if negative.upsertReq.GetUnitPricePer_1000Minor() != -1 {
		t.Fatalf("网关不得改写或预拒单价: %+v", negative.upsertReq)
	}

	noCurrency := newReplyFake()
	r2 := commerceRevenueGoodRuleUpsert()
	r2.Currency = ""
	if _, err := NewRevenueRuleUpsertLogic(ctx, commerceRevenueSvc(noCurrency)).RevenueRuleUpsert(r2); err != nil {
		t.Fatalf("currency 留空由服务补默认币种，网关不得拒: %v", err)
	}
	if noCurrency.upsertReq.GetCurrency() != "" {
		t.Fatalf("网关不得替调用方补币种: %+v", noCurrency.upsertReq)
	}

	unknownSource := newReplyFake()
	unknownSource.err = errors.New("creatorrevenue: invalid source_type: 9")
	r3 := commerceRevenueGoodRuleUpsert()
	r3.SourceType = 9
	if _, err := NewRevenueRuleUpsertLogic(ctx, commerceRevenueSvc(unknownSource)).RevenueRuleUpsert(r3); !errors.Is(err, unknownSource.err) {
		t.Fatalf("「这个来源编号存不存在」归服务判: %v", err)
	}
	if int32(unknownSource.upsertReq.GetSourceType()) != 9 {
		t.Fatalf("网关不得替未知来源挑一个值: %+v", unknownSource.upsertReq)
	}
}

// TestCommerceRevenueRuleUpsert_NoStateSlotAndRelaysDraft 锁两件事：
//   - 表单与 Req 都没有 state 位（改草稿与生效是两个权限点）：网关不补默认档位，
//     也不「顺手」调 SetRevenueRuleState 让它生效；
//   - 服务回的是 DRAFT 就转达 DRAFT，绝不显示成已生效。
func TestCommerceRevenueRuleUpsert_NoStateSlotAndRelaysDraft(t *testing.T) {
	fake := newReplyFake()
	resp, err := NewRevenueRuleUpsertLogic(commerceRevenueSession(), commerceRevenueSvc(fake)).
		RevenueRuleUpsert(commerceRevenueGoodRuleUpsert())
	if err != nil {
		t.Fatalf("rule/upsert: %v", err)
	}
	reqText := fake.upsertReq.String()
	for _, forbidden := range []string{"state", "payout", "withdraw", "paid", "bank", "trace_id", "invoice", "remit"} {
		if strings.Contains(reqText, forbidden) {
			t.Fatalf("网关不得塞进契约没有的 %s 位（改草稿不得顺带生效，出金更不在本期范围）: %s", forbidden, reqText)
		}
	}
	for _, slot := range []string{"rule_code", "source_type", "unit_price_per_1000_minor", "operator", "request_id", "reason", "effective_from"} {
		if !strings.Contains(reqText, slot) {
			t.Fatalf("UpsertRevenueRuleReq 少了 %s 位，网关没把必填项发下去: %s", slot, reqText)
		}
	}
	if resp.Data.Rule.State != int32(creatorrevenuerpc.RuleState_RULE_STATE_DRAFT) {
		t.Fatalf("DRAFT 被网关显示成别的档位了: %+v", resp.Data.Rule)
	}
	if resp.Data.Rule.Version != 4 || resp.Data.Rule.EffectiveFrom != 1700000000 {
		t.Fatalf("改完要回读 CAS 位点与生效起点（后台下一轮生效要带回去）: %+v", resp.Data.Rule)
	}
}

// TestCommerceRevenueRuleState_CasZeroAndTransitionStayOnService 锁本域最重的一位：
//   - expected_version=0 原样下传（.api 标 optional），由服务按「状态切换必须带 CAS 版本」
//     拒（ErrVersionConflict 且消息带当前版本）；网关**不**先读一次规则替它补版本号
//     ——那会把乐观锁换成「保证不冲突」；
//   - 「这一刀合不合法」需要知道前态，是服务 checkRuleTransition 的结论，逐字透出，
//     网关不重试、不改目标态。
func TestCommerceRevenueRuleState_CasZeroAndTransitionStayOnService(t *testing.T) {
	zero := newReplyFake()
	r := commerceRevenueGoodRuleState()
	r.ExpectedVersion = 0
	if _, err := NewRevenueRuleStateLogic(commerceRevenueSession(), commerceRevenueSvc(zero)).RevenueRuleState(r); err != nil {
		t.Fatalf("expected_version=0 该交给服务判: %v", err)
	}
	if zero.ruleStateReq.GetExpectedVersion() != 0 {
		t.Fatalf("网关不得代填 CAS 版本: %+v", zero.ruleStateReq)
	}
	if zero.ruleStateReq.GetTargetState() != creatorrevenuerpc.RuleState_RULE_STATE_ACTIVE {
		t.Fatalf("目标态被改写: %+v", zero.ruleStateReq)
	}
	// 非 0 的 CAS 版本原样下传。
	if _, err := NewRevenueRuleStateLogic(commerceRevenueSession(), commerceRevenueSvc(zero)).
		RevenueRuleState(commerceRevenueGoodRuleState()); err != nil {
		t.Fatalf("rule/state: %v", err)
	}
	if zero.ruleStateReq.GetExpectedVersion() != 4 {
		t.Fatalf("非 0 的 CAS 版本要原样下传: %+v", zero.ruleStateReq)
	}

	conflict := newReplyFake()
	conflict.err = errRevenueRuleStateTransition
	if _, err := NewRevenueRuleStateLogic(commerceRevenueSession(), commerceRevenueSvc(conflict)).
		RevenueRuleState(commerceRevenueGoodRuleState()); !errors.Is(err, errRevenueRuleStateTransition) {
		t.Fatalf("非法迁移必须由服务拒绝并原样透出: %v", err)
	}
	if conflict.calls != 1 {
		t.Fatalf("写路由不得自动重放（calls=%d）", conflict.calls)
	}

	cas := newReplyFake()
	cas.err = errRevenueVersionConflict
	if _, err := NewRevenueRuleStateLogic(commerceRevenueSession(), commerceRevenueSvc(cas)).
		RevenueRuleState(commerceRevenueGoodRuleState()); !errors.Is(err, errRevenueVersionConflict) {
		t.Fatalf("CAS 冲突要原样透出（网关不重新取版本号）: %v", err)
	}
}

// TestCommerceRevenueRuleGet_TargetRequiredStaysOnService：rule_id 与 rule_code 都为空时
// 服务回 ErrRuleTargetRequired；网关不预先拒（不自己判「至少给一个」），也不替调用方挑
// 一种定位方式。
func TestCommerceRevenueRuleGet_TargetRequiredStaysOnService(t *testing.T) {
	fake := newReplyFake()
	fake.err = errRevenueRuleTargetRequired
	_, err := NewRevenueRuleGetLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueRuleGet(&types.ParamRevenueRuleGet{})
	if !errors.Is(err, errRevenueRuleTargetRequired) {
		t.Fatalf("服务的「至少给一个定位位」拒绝必须原样透出: %v", err)
	}
	if fake.ruleGetReq.GetRuleId() != 0 || fake.ruleGetReq.GetRuleCode() != "" || fake.ruleGetReq.GetVersion() != 0 {
		t.Fatalf("网关不得凭空造定位条件: %+v", fake.ruleGetReq)
	}
}

// --- 5. 分页 / 0 哨兵 / 服务回显照抄 ---

func TestCommerceRevenueRuleList_PagingPassesThroughAndEchoesServiceValues(t *testing.T) {
	fake := newReplyFake()
	resp, err := NewRevenueRuleListLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueRuleList(&types.ParamRevenueRuleList{State: 3, SourceType: 4, Page: 3, Size: 500})
	if err != nil {
		t.Fatalf("翻页不该被网关拒绝: %v", err)
	}
	got := fake.rulesReq
	if got.GetPage() != 3 || got.GetSize() != 500 {
		t.Fatalf("page/size 被网关改写或裁剪（上限归服务拒）: %+v", got)
	}
	if got.GetState() != creatorrevenuerpc.RuleState_RULE_STATE_ARCHIVED ||
		got.GetSourceType() != creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_ACTIVITY {
		t.Fatalf("过滤条件没按契约下传: %+v", got)
	}
	if resp.Data.Total != 7 || resp.Data.Page != 3 || resp.Data.Size != 100 {
		t.Fatalf("分页三元组必须照抄服务回显而不是回显请求值: %+v", resp.Data)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封必须固定 code=0/message=ok/ttl=0: %+v", resp)
	}
	if len(resp.Data.List) != 1 || resp.Data.List[0].MonthlyCapMinor != 500000 {
		t.Fatalf("规则行要逐位转达（含 0=不限 与封顶值）: %+v", resp.Data.List)
	}
}

// TestCommerceRevenueListRoutes_ZeroMeansNoFilterOrFullScan 锁 0 哨兵：四条读路的
// state/source_type/mid/aid/page/size 全 0 都是合法请求（不过滤 / 默认页），
// 网关一个都不代填；有界性与枚举合法性由服务拒。
func TestCommerceRevenueListRoutes_ZeroMeansNoFilterOrFullScan(t *testing.T) {
	fake := newReplyFake()
	ruleResp, err := NewRevenueRuleListLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueRuleList(&types.ParamRevenueRuleList{})
	if err != nil {
		t.Fatalf("全 0 的规则检索本身合法（合法性归服务判）: %v", err)
	}
	if fake.rulesReq.GetState() != creatorrevenuerpc.RuleState_RULE_STATE_UNSPECIFIED ||
		fake.rulesReq.GetSourceType() != creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_TYPE_UNSPECIFIED ||
		fake.rulesReq.GetPage() != 0 || fake.rulesReq.GetSize() != 0 {
		t.Fatalf("0 是合法哨兵，网关不得代填: %+v", fake.rulesReq)
	}
	if ruleResp.Data.List == nil {
		t.Fatalf("空列表要投影成 []，客户端才能直接遍历: %#v", ruleResp.Data.List)
	}

	enrollResp, err := NewRevenueEnrollmentListLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueEnrollmentList(&types.ParamRevenueEnrollmentList{})
	if err != nil {
		t.Fatalf("全 0 的名单检索本身合法: %v", err)
	}
	if fake.enrollListReq.GetState() != creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_UNSPECIFIED {
		t.Fatalf("state=0 表示含 LEFT/SUSPENDED 全量，网关不得收窄: %+v", fake.enrollListReq)
	}
	if len(enrollResp.Data.List) != 1 || enrollResp.Data.List[0].State != 3 ||
		enrollResp.Data.List[0].AgreedRuleVersion != 4 {
		t.Fatalf("暂停态与本人确认版本要如实转达: %+v", enrollResp.Data.List)
	}
	if enrollResp.Data.List[0].Operator != "gateway/admin:77" {
		t.Fatalf("operator 是「谁处置的」证据位，不得裁掉: %+v", enrollResp.Data.List[0])
	}
}

// TestCommerceRevenueMetricList_ScopeAndFormatStayOnService：
// period 与 mid 都不给 → 服务回 ErrQueryScopeRequired；period 格式错 → ErrInvalidPeriod。
// 两条都逐字透出，网关既不预拒、不代填上月，也不折成「这一期没有计量」。
func TestCommerceRevenueMetricList_ScopeAndFormatStayOnService(t *testing.T) {
	scope := newReplyFake()
	scope.err = errRevenueQueryScopeRequired
	if _, err := NewRevenueMetricListLogic(context.Background(), commerceRevenueSvc(scope)).
		RevenueMetricList(&types.ParamRevenueMetricList{Page: 1, Size: 20}); !errors.Is(err, errRevenueQueryScopeRequired) {
		t.Fatalf("服务的有界性拒绝必须原样透出: %v", err)
	}
	if scope.metricsReq.GetPeriod() != "" || scope.metricsReq.GetMid() != 0 {
		t.Fatalf("网关不得凭空造周期或作者: %+v", scope.metricsReq)
	}

	badPeriod := newReplyFake()
	badPeriod.err = errRevenueInvalidPeriod
	if _, err := NewRevenueMetricListLogic(context.Background(), commerceRevenueSvc(badPeriod)).
		RevenueMetricList(&types.ParamRevenueMetricList{Period: "2026-08"}); !errors.Is(err, errRevenueInvalidPeriod) {
		t.Fatalf("period 格式由服务判（ValidatePeriod），网关不预先拒: %v", err)
	}
	if badPeriod.metricsReq.GetPeriod() != "2026-08" {
		t.Fatalf("网关不得改写 period（含不得替它补零或去掉分隔符）: %+v", badPeriod.metricsReq)
	}

	// aid=0 是「不挂具体内容」的合法位（活动激励类台账），网关不代填。
	fake := newReplyFake()
	resp, err := NewRevenueMetricListLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueMetricList(&types.ParamRevenueMetricList{Period: "202608", Mid: 10001, Aid: 0})
	if err != nil {
		t.Fatalf("metric/list: %v", err)
	}
	if fake.metricsReq.GetAid() != 0 {
		t.Fatalf("aid=0 被网关代填: %+v", fake.metricsReq)
	}
	if resp.Data.List[0].AmountMinor != 1481 || resp.Data.List[0].CappedAmountMinor != 1200 {
		t.Fatalf("封顶前后两位应计必须并存，合并就看不出砍了多少: %+v", resp.Data.List[0])
	}
}

// TestCommerceRevenueSettlementList_StateFilterPassthrough：越界状态编号由服务回
// ErrInvalidRuleState（消息里写明「运营面板拿查不到去催确认会催错方向」），
// 网关不预先剔除 VOIDED，也不替调用方挑状态。
func TestCommerceRevenueSettlementList_StateFilterPassthrough(t *testing.T) {
	bad := newReplyFake()
	bad.err = errRevenueInvalidRuleState
	if _, err := NewRevenueSettlementListLogic(context.Background(), commerceRevenueSvc(bad)).
		RevenueSettlementList(&types.ParamRevenueSettlementList{Period: "202608", State: 9}); !errors.Is(err, errRevenueInvalidRuleState) {
		t.Fatalf("服务的状态校验要逐字透出，不退化成「查不到」: %v", err)
	}
	if int32(bad.settleListReq.GetState()) != 9 {
		t.Fatalf("网关不得替未知状态挑一个值: %+v", bad.settleListReq)
	}

	fake := newReplyFake()
	resp, err := NewRevenueSettlementListLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueSettlementList(&types.ParamRevenueSettlementList{Period: "202608", State: 3})
	if err != nil {
		t.Fatalf("settlement/list: %v", err)
	}
	if fake.settleListReq.GetState() != creatorrevenuerpc.SettlementState_SETTLEMENT_STATE_VOIDED {
		t.Fatalf("VOIDED 过滤要原样下传（作废单正是审计要看的历史）: %+v", fake.settleListReq)
	}
	if resp.Data.Total != 7 || resp.Data.Page != 3 || resp.Data.Size != 100 {
		t.Fatalf("分页三元组要照抄服务回显: %+v", resp.Data)
	}
}

// --- 6. 结算语义：payout 一位都不许被美化 ---

// TestCommerceRevenuePayoutStateRelayedVerbatim 是本域最容易被「美化」的一位。
// 服务回 NOT_PAYABLE(1) 就转达 1；服务（理论上）回 UNSPECIFIED(0) 也转达 0 ——
// 网关既不写死成 1（那是在伪造「服务说了」），也不因为 state=CONFIRMED 就翻成任何
// 已出账含义。本期没有出金通道，响应里也不该出现任何「可提现/已打款」位。
func TestCommerceRevenuePayoutStateRelayedVerbatim(t *testing.T) {
	confirmed := commerceRevenueFullSettlement()
	confirmed.State = creatorrevenuerpc.SettlementState_SETTLEMENT_STATE_CONFIRMED
	fake := newReplyFake()
	fake.settleList = &creatorrevenuerpc.ListSettlementsReply{
		Settlements: []*creatorrevenuerpc.SettlementInfo{confirmed},
	}
	resp, err := NewRevenueSettlementListLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueSettlementList(&types.ParamRevenueSettlementList{Period: "202608"})
	if err != nil {
		t.Fatalf("settlement/list: %v", err)
	}
	row := resp.Data.List[0]
	if row.PayoutState != int32(creatorrevenuerpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE) {
		t.Fatalf("payout_state 要转达服务值 NOT_PAYABLE: %+v", row)
	}
	if row.State != int32(creatorrevenuerpc.SettlementState_SETTLEMENT_STATE_CONFIRMED) {
		t.Fatalf("已确认这一事实被改写: %+v", row)
	}
	if row.AmountMinor != 1200 || row.CapAppliedMinor != 281 || row.MetricCount != 3 {
		t.Fatalf("应计合计/封顶扣减/台账条数是一位都不能裁的对账位: %+v", row)
	}

	// 服务回 UNSPECIFIED(0) 时也不美化成 1：网关不替服务的结论「补正确」。
	unspecified := commerceRevenueFullSettlement()
	unspecified.PayoutState = creatorrevenuerpc.PayoutState_PAYOUT_STATE_UNSPECIFIED
	fake2 := newReplyFake()
	fake2.settleGet = &creatorrevenuerpc.GetSettlementReply{
		Found: true, Settlement: unspecified,
		Items: []*creatorrevenuerpc.SettlementItem{commerceRevenueFullSettlementItem()},
	}
	got, err := NewRevenueSettlementGetLogic(context.Background(), commerceRevenueSvc(fake2)).
		RevenueSettlementGet(&types.ParamRevenueSettlementGet{SettlementNo: "st_202608_10001"})
	if err != nil {
		t.Fatalf("settlement/get: %v", err)
	}
	if got.Data.Settlement.PayoutState != 0 {
		t.Fatalf("网关不得把 payout_state 写死: %+v", got.Data.Settlement)
	}
	if len(got.Data.Items) != 1 {
		t.Fatalf("分项要逐条转达，不与合计配平: %+v", got.Data.Items)
	}
	item := got.Data.Items[0]
	wantItem := commerceRevenueFullSettlementItem()
	if item.SourceType != int32(wantItem.GetSourceType()) || item.RuleCode != wantItem.GetRuleCode() ||
		item.Quantity != wantItem.GetQuantity() || item.AmountMinor != wantItem.GetAmountMinor() {
		t.Fatalf("分项四位都得原样转达: got %+v want %+v", item, wantItem)
	}
}

// TestCommerceRevenueSettlementGet_ForbiddenNotFoldedIntoNotFound：归属不符时服务回
// ErrForbidden（人话拒绝，含单号与两侧 mid），网关逐字上抛，绝不折叠成 found=false
// ——那会让运营以为单号写错了，然后去重发一次出单。
func TestCommerceRevenueSettlementGet_ForbiddenNotFoldedIntoNotFound(t *testing.T) {
	fake := newReplyFake()
	fake.err = errRevenueSettlementForbidden
	_, err := NewRevenueSettlementGetLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueSettlementGet(&types.ParamRevenueSettlementGet{SettlementNo: "st_202608_10001", Mid: 10002})
	if !errors.Is(err, errRevenueSettlementForbidden) {
		t.Fatalf("归属校验失败必须原样透出: %v", err)
	}
	if fake.settleGetReq.GetMid() != 10002 {
		t.Fatalf("mid 要原样下传（归属校验在服务）: %+v", fake.settleGetReq)
	}

	// found=false 是真实读结论，不是错误，也不填一个看起来真的结算单。
	miss := newReplyFake()
	miss.settleGet = &creatorrevenuerpc.GetSettlementReply{Found: false}
	missResp, err := NewRevenueSettlementGetLogic(context.Background(), commerceRevenueSvc(miss)).
		RevenueSettlementGet(&types.ParamRevenueSettlementGet{SettlementNo: "st_missing"})
	if err != nil {
		t.Fatalf("found=false 不该被报成错误: %v", err)
	}
	if missResp.Data.Found {
		t.Fatalf("found 位被网关翻成 true 了: %+v", missResp.Data)
	}
	if missResp.Data.Settlement != (types.RevenueSettlementItem{}) {
		t.Fatalf("没有结算单时不得渲染出任何字段（连 mid 都不能猜）: %+v", missResp.Data.Settlement)
	}
	if missResp.Data.Items == nil || len(missResp.Data.Items) != 0 {
		t.Fatalf("空分项要投影成 []: %#v", missResp.Data.Items)
	}
}

// TestCommerceRevenueRuleGet_HistoryNotFoundNotFabricated：历史版本还原不出来时服务回
// found=false（宁可说没有，也不能把现单价当历史单价），网关逐字转达。
func TestCommerceRevenueRuleGet_HistoryNotFoundNotFabricated(t *testing.T) {
	fake := newReplyFake()
	fake.ruleGet = &creatorrevenuerpc.GetRevenueRuleReply{Found: false}
	resp, err := NewRevenueRuleGetLogic(context.Background(), commerceRevenueSvc(fake)).
		RevenueRuleGet(&types.ParamRevenueRuleGet{RuleId: 5001, Version: 2})
	if err != nil {
		t.Fatalf("还原不出历史版本不是错误: %v", err)
	}
	if resp.Data.Found {
		t.Fatalf("found=false 被翻成 true 了（等于伪造历史单价）: %+v", resp.Data)
	}
	if resp.Data.Rule != (types.RevenueRuleItem{}) {
		t.Fatalf("没有历史版本时不得渲染现单价冒充: %+v", resp.Data.Rule)
	}
	if fake.ruleGetReq.GetVersion() != 2 {
		t.Fatalf("version 要原样下传: %+v", fake.ruleGetReq)
	}
}

// --- 7. 幂等重放、危险位与部分结果都是结论，不是错误 ---

// TestCommerceRevenueEnrollmentState_ReplayIsSuccessWithFirstResult：duplicated=true
// 是「这次没改动、首次结论在这里」，网关照常回 code=0 并转达首次状态行。
func TestCommerceRevenueEnrollmentState_ReplayIsSuccessWithFirstResult(t *testing.T) {
	fake := newReplyFake()
	fake.enrollState = &creatorrevenuerpc.SetEnrollmentStateReply{
		Duplicated: true, Enrollment: commerceRevenueFullEnrollment(),
	}
	resp, err := NewRevenueEnrollmentStateLogic(commerceRevenueSession(), commerceRevenueSvc(fake)).
		RevenueEnrollmentState(commerceRevenueGoodEnrollmentState())
	if err != nil {
		t.Fatalf("暂停重放是成功结论: %v", err)
	}
	if !resp.Data.Duplicated || resp.Data.Enrollment.State != 3 || resp.Data.Enrollment.AgreedRuleVersion != 4 {
		t.Fatalf("首次结论被丢了: %+v", resp.Data)
	}

	// 当前态不匹配由服务判并逐字透出，网关不改目标态再打一次。
	bad := newReplyFake()
	bad.err = errRevenueEnrollmentStateTransition
	if _, err := NewRevenueEnrollmentStateLogic(commerceRevenueSession(), commerceRevenueSvc(bad)).
		RevenueEnrollmentState(commerceRevenueGoodEnrollmentState()); !errors.Is(err, errRevenueEnrollmentStateTransition) {
		t.Fatalf("非法迁移必须由服务拒: %v", err)
	}
	if bad.calls != 1 {
		t.Fatalf("被拒的处置不得重试（calls=%d）", bad.calls)
	}
}

// TestCommerceRevenueSettlementGenerate_DangerSlotAndTruncatedPassThrough 锁危险位与截断位：
//   - force_void_confirmed=true 原样下传，网关既不默认关、不代为打开、也不顺带调
//     ConfirmSettlement「把新单冻上」（算完了和认了是两个权限点）；
//   - 它必须带 reason 这条**条件必填**由服务判（ErrForceVoidReasonRequired），
//     admin.api 把 reason 标成 optional，网关不预先拒空；
//   - duplicated=true（已有未作废单）是成功结论；truncated=true 必须转达（完整结果去
//     /settlement/list 读），网关不本地补齐也不隐瞒。
func TestCommerceRevenueSettlementGenerate_DangerSlotAndTruncatedPassThrough(t *testing.T) {
	ctx := commerceRevenueSession()

	force := newReplyFake()
	g := commerceRevenueGoodGenerate()
	g.ForceVoidConfirmed = true
	g.Reason = ""
	if _, err := NewRevenueSettlementGenerateLogic(ctx, commerceRevenueSvc(force)).RevenueSettlementGenerate(g); err != nil {
		t.Fatalf("条件必填的 reason 该交给服务判: %v", err)
	}
	if !force.generateReq.GetForceVoidConfirmed() {
		t.Fatalf("危险位被网关关掉了: %+v", force.generateReq)
	}
	if force.generateReq.GetReason() != "" {
		t.Fatalf("网关不得替调用方编一个作废理由: %+v", force.generateReq)
	}

	forceReason := newReplyFake()
	g2 := commerceRevenueGoodGenerate()
	g2.ForceVoidConfirmed = true
	g2.Reason = "算错单价，本期重算（复核记录 CR-2026-09-30）"
	if _, err := NewRevenueSettlementGenerateLogic(ctx, commerceRevenueSvc(forceReason)).RevenueSettlementGenerate(g2); err != nil {
		t.Fatalf("带理由的强制重算: %v", err)
	}
	if forceReason.generateReq.GetReason() != g2.Reason {
		t.Fatalf("reason 要原样下传: %+v", forceReason.generateReq)
	}

	noReason := newReplyFake()
	noReason.err = errRevenueForceVoidReasonRequired
	g3 := commerceRevenueGoodGenerate()
	g3.ForceVoidConfirmed = true
	if _, err := NewRevenueSettlementGenerateLogic(ctx, commerceRevenueSvc(noReason)).RevenueSettlementGenerate(g3); !errors.Is(err, errRevenueForceVoidReasonRequired) {
		t.Fatalf("服务的条件必填拒绝要逐字透出: %v", err)
	}

	// mid=0 = 全量出单，网关不代填 mid、不本地循环；未来周期由服务拒。
	fake := newReplyFake()
	resp, err := NewRevenueSettlementGenerateLogic(ctx, commerceRevenueSvc(fake)).
		RevenueSettlementGenerate(commerceRevenueGoodGenerate())
	if err != nil {
		t.Fatalf("settlement/generate: %v", err)
	}
	if fake.generateReq.GetMid() != 0 {
		t.Fatalf("mid=0 是全量哨兵，网关不得代填: %+v", fake.generateReq)
	}
	if resp.Data.Generated != 2 || !resp.Data.Truncated {
		t.Fatalf("generated/truncated 要照抄服务回显: %+v", resp.Data)
	}
	if len(resp.Data.Settlements) != 1 {
		t.Fatalf("服务只回前若干条时网关不得本地补齐: %+v", resp.Data.Settlements)
	}

	future := newReplyFake()
	future.err = errRevenueFuturePeriod
	if _, err := NewRevenueSettlementGenerateLogic(ctx, commerceRevenueSvc(future)).
		RevenueSettlementGenerate(&types.ParamRevenueSettlementGenerate{
			Period: "209912", Operator: 9001, IdempotencyKey: "k-generate-2",
		}); !errors.Is(err, errRevenueFuturePeriod) {
		t.Fatalf("未来周期由服务判并逐字透出: %v", err)
	}

	// 幂等键冲突：原样上抛，绝不换个 request_id 再出一次单（那就是重复出单）。
	replayed := newReplyFake()
	replayed.err = errRevenueRequestReplayed
	if _, err := NewRevenueSettlementGenerateLogic(ctx, commerceRevenueSvc(replayed)).
		RevenueSettlementGenerate(commerceRevenueGoodGenerate()); !errors.Is(err, errRevenueRequestReplayed) {
		t.Fatalf("幂等冲突要原样透出: %v", err)
	}
	if replayed.calls != 1 {
		t.Fatalf("写路由不得自动重放（calls=%d）", replayed.calls)
	}
}

// TestCommerceRevenueSettlementConfirm_BatchAndPartialResult 锁「认账」语义：
//   - settlement_nos 逐位原样下传：不去重、不 Trim、不裁剪；空列表与超 MaxConfirmBatch(200)
//     都由服务拒（admin.api 明写空列表由服务拒绝），网关不预拒；
//   - confirmed=1/failed_nos=[1 张] 的部分结果是**正常回复**，不合并成「全部成功」；
//   - confirmed=0 也不是错误（一张都没冻上），网关不加错也不报成功台账。
func TestCommerceRevenueSettlementConfirm_BatchAndPartialResult(t *testing.T) {
	ctx := commerceRevenueSession()

	fake := newReplyFake()
	nos := []string{"st_202608_10001", "  ", "st_202608_10001"}
	resp, err := NewRevenueSettlementConfirmLogic(ctx, commerceRevenueSvc(fake)).
		RevenueSettlementConfirm(&types.ParamRevenueSettlementConfirm{
			SettlementNos: nos, Reason: "复核通过", Operator: 9001, IdempotencyKey: "k-confirm-2",
		})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(fake.confirmReq.GetSettlementNos()) != 3 || fake.confirmReq.GetSettlementNos()[1] != "  " {
		t.Fatalf("单号列表被网关去重/Trim 过了（格式归服务判）: %v", fake.confirmReq.GetSettlementNos())
	}
	if resp.Data.Confirmed != 1 || len(resp.Data.FailedNos) != 1 || resp.Data.FailedNos[0] != "st_202608_10002" {
		t.Fatalf("confirmed 与 failed_nos 要原样回，不合并成全部成功: %+v", resp.Data)
	}
	if resp.Code != 0 || resp.Message != "ok" {
		t.Fatalf("部分确认仍是成功回复（带结论）: %+v", resp)
	}

	empty := newReplyFake()
	empty.err = errRevenueSettlementNosRequired
	if _, err := NewRevenueSettlementConfirmLogic(ctx, commerceRevenueSvc(empty)).
		RevenueSettlementConfirm(&types.ParamRevenueSettlementConfirm{
			Reason: "r", Operator: 9001, IdempotencyKey: "k-confirm-3",
		}); !errors.Is(err, errRevenueSettlementNosRequired) {
		t.Fatalf("空列表由服务拒（.api 明写），网关不得预拒也不得回 0 成功: %v", err)
	}
	if len(empty.confirmReq.GetSettlementNos()) != 0 {
		t.Fatalf("网关不得替调用方造单号: %v", empty.confirmReq.GetSettlementNos())
	}

	tooMany := newReplyFake()
	tooMany.err = errRevenueBatchTooLarge
	nos2 := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		nos2 = append(nos2, fmt.Sprintf("st_202608_%05d", i))
	}
	if _, err := NewRevenueSettlementConfirmLogic(ctx, commerceRevenueSvc(tooMany)).
		RevenueSettlementConfirm(&types.ParamRevenueSettlementConfirm{
			SettlementNos: nos2, Reason: "r", Operator: 9001, IdempotencyKey: "k-confirm-4",
		}); !errors.Is(err, errRevenueBatchTooLarge) {
		t.Fatalf("超批量上限由服务**拒绝**而不是裁剪: %v", err)
	}
	if len(tooMany.confirmReq.GetSettlementNos()) != 201 {
		t.Fatalf("网关不得自己夹一刀（夹了运营以为剩下的都冻上了）: %d", len(tooMany.confirmReq.GetSettlementNos()))
	}
}

// --- 8. 投影（reply → API，逐字段）---

func TestCommerceRevenueProjection_FieldByField(t *testing.T) {
	rule, enrollment, metric, settlement, item := commerceRevenueFullRule(), commerceRevenueFullEnrollment(),
		commerceRevenueFullMetric(), commerceRevenueFullSettlement(), commerceRevenueFullSettlementItem()
	cases := []struct {
		name  string
		diffs []string
	}{
		{"RevenueRuleInfo", revenueRuleDiff(revenueRuleToAPI(rule), rule)},
		{"RevenueRuleInfo(nil)", revenueRuleDiff(revenueRuleToAPI(nil), &creatorrevenuerpc.RevenueRuleInfo{})},
		{"EnrollmentInfo", revenueEnrollmentDiff(revenueEnrollmentToAPI(enrollment), enrollment)},
		{"EnrollmentInfo(nil)", revenueEnrollmentDiff(revenueEnrollmentToAPI(nil), &creatorrevenuerpc.EnrollmentInfo{})},
		{"RevenueMetricInfo", revenueMetricDiff(revenueMetricToAPI(metric), metric)},
		{"RevenueMetricInfo(nil)", revenueMetricDiff(revenueMetricToAPI(nil), &creatorrevenuerpc.RevenueMetricInfo{})},
		{"SettlementInfo", revenueSettlementDiff(revenueSettlementToAPI(settlement), settlement)},
		{"SettlementInfo(nil)", revenueSettlementDiff(revenueSettlementToAPI(nil), &creatorrevenuerpc.SettlementInfo{})},
		{"SettlementItem", revenueSettlementDetailDiff(revenueSettlementItemToAPI(item), item)},
		{"SettlementItem(nil)", revenueSettlementDetailDiff(revenueSettlementItemToAPI(nil), &creatorrevenuerpc.SettlementItem{})},
	}
	for _, c := range cases {
		if len(c.diffs) > 0 {
			t.Fatalf("%s 投影丢字段/改字段: %v", c.name, c.diffs)
		}
	}
	if got := revenueRulesToAPI([]*creatorrevenuerpc.RevenueRuleInfo{nil, rule}); len(got) != 2 ||
		got[0] != (types.RevenueRuleItem{}) || got[1].Version != 4 {
		t.Fatalf("nil 规则行要给零值而不是丢掉或 panic，列表与单行投影要一致: %+v", got)
	}
	if got := revenueEnrollmentsToAPI([]*creatorrevenuerpc.EnrollmentInfo{nil, enrollment}); len(got) != 2 ||
		got[0].Mid != 0 || got[1].Remark == "" {
		t.Fatalf("nil 参与行要给零值而不是 panic: %+v", got)
	}
	if got := revenueMetricsToAPI([]*creatorrevenuerpc.RevenueMetricInfo{nil, metric}); len(got) != 2 ||
		got[0].MetricId != 0 || got[1].RuleVersion != 4 {
		t.Fatalf("nil 台账行要给零值而不是 panic: %+v", got)
	}
	if got := revenueSettlementsToAPI([]*creatorrevenuerpc.SettlementInfo{nil, settlement}); len(got) != 2 ||
		got[0].SettlementNo != "" || got[1].ConfirmedBy == "" {
		t.Fatalf("nil 结算行要给零值而不是 panic: %+v", got)
	}
	if got := revenueRulesToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空规则列表要投影成 []: %#v", got)
	}
	if got := revenueEnrollmentsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空参与列表要投影成 []: %#v", got)
	}
	if got := revenueMetricsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空计量列表要投影成 []: %#v", got)
	}
	if got := revenueSettlementsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空结算列表要投影成 []: %#v", got)
	}
	if got := revenueSettlementItemsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空分项列表要投影成 []: %#v", got)
	}
}

// TestCommerceRevenueProjection_NilReplyDoesNotPanic 覆盖「下游回了 nil 但没报错」：
// protobuf 的 Get* 访问器是 nil 安全的，投影必须落到空集合/零值而不是 panic。
// 这条只保证不崩——真实服务不会回 (nil, nil)，回了网关也不加业务判断去补数。
func TestCommerceRevenueProjection_NilReplyDoesNotPanic(t *testing.T) {
	ctx := commerceRevenueSession()
	svcCtx := commerceRevenueSvc(&commerceRevenueFake{})
	for _, route := range commerceRevenueRoutes {
		if err := route.run(ctx, svcCtx); err != nil {
			t.Fatalf("%s: nil reply 不该变成错误: %v", route.name, err)
		}
	}
}

// --- 9. 门槛助手自身的不可能形状口径 ---

func TestCommerceRevenueOperatorRenderedOnceAndLengthCapped(t *testing.T) {
	operator, err := revenueOperator(commerceRevenueSession(), "revenueRuleUpsert", 9001)
	if err != nil {
		t.Fatalf("revenueOperator: %v", err)
	}
	if operator != "gateway/admin:77" {
		t.Fatalf("operator 必须是会话渲染值，实际 %q", operator)
	}
	if _, err := revenueOperator(commerceRevenueSession(), "revenueRuleUpsert", 77); err != nil {
		t.Fatalf("会话与表单一致时不该报错: %v", err)
	}
	// int64 的 admin_id 渲染出来最长是 "gateway/admin:" (14) + 19 位 = 33 字符，
	// 永远碰不到 64 的上限。这条断言不是走形式：它把「上限对 int64 恒不触发」这件事钉住，
	// 一旦有人改前缀或改成拼字符串别的字段（例如把 route 名也塞进 operator）而越宽，
	// 这里就会先响，而不是等到服务报 text exceeds column limit。
	maxID := middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: math.MaxInt64})
	operator, err = revenueOperator(maxID, "revenueRuleUpsert", math.MaxInt64)
	if err != nil {
		t.Fatalf("MaxInt64 渲染出的 operator 仍在列宽内，不该拒: %v", err)
	}
	if want := fmt.Sprintf("gateway/admin:%d", int64(math.MaxInt64)); operator != want {
		t.Fatalf("operator=%q want %q", operator, want)
	}
	if n := utf8.RuneCountInString(operator); n > revenueMaxOperatorLength {
		t.Fatalf("渲染出的 operator %d 字符，越过列宽 %d", n, revenueMaxOperatorLength)
	}
	// 会话里的 admin_id 非正数：中间件的 AdminFromContext 本身就把 AdminID<=0 判成「没有身份」
	// （gateway/admin/internal/middleware/adminpermissionmiddleware.go:386），所以这里到不了
	// requireOperatorID 那一层，返回的是「没有会话」哨兵。断言写成这样是为了记下真实分层：
	// 非正数主体绝不会变成 operator=gateway/admin:0 发下去。
	for _, bad := range []int64{0, -1} {
		noID := middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: bad})
		if _, err := revenueOperator(noID, "revenueRuleUpsert", bad); !errors.Is(err, errRevenueSessionRequired) {
			t.Fatalf("admin_id=%d 不能成为 operator，应返回无会话哨兵，实际 %v", bad, err)
		}
	}
	// 表单自称的 operator 非正数也拒：它是「谁在操作」的唯一线索，得先是个主体。
	if _, err := revenueOperator(commerceRevenueSession(), "revenueRuleUpsert", 0); err == nil ||
		!strings.Contains(err.Error(), "operator") {
		t.Fatalf("表单 operator=0 要拒，实际 %v", err)
	}
	// 没有会话（context 里没塞身份）一律拒，不看表单值。
	if _, err := revenueOperator(context.Background(), "revenueRuleUpsert", 77); !errors.Is(err, errRevenueSessionRequired) {
		t.Fatalf("无会话要返回 errRevenueSessionRequired，实际 %v", err)
	}
}

// --- 投影 diff 助手：字段名直接进失败消息，避免「少投影一位」只报两句不等的字符串 ---

func revenueEq(diffs *[]string, name string, got, want any) {
	if got != want {
		*diffs = append(*diffs, fmt.Sprintf("%s: got %#v want %#v", name, got, want))
	}
}

func revenueRuleDiff(got types.RevenueRuleItem, want *creatorrevenuerpc.RevenueRuleInfo) []string {
	var diffs []string
	revenueEq(&diffs, "rule_id", got.RuleId, want.GetRuleId())
	revenueEq(&diffs, "rule_code", got.RuleCode, want.GetRuleCode())
	revenueEq(&diffs, "source_type", got.SourceType, int32(want.GetSourceType()))
	revenueEq(&diffs, "name", got.Name, want.GetName())
	revenueEq(&diffs, "description", got.Description, want.GetDescription())
	revenueEq(&diffs, "unit_price_per_1000_minor", got.UnitPricePer1000Minor, want.GetUnitPricePer_1000Minor())
	revenueEq(&diffs, "currency", got.Currency, want.GetCurrency())
	revenueEq(&diffs, "unit", got.Unit, want.GetUnit())
	revenueEq(&diffs, "min_quantity", got.MinQuantity, want.GetMinQuantity())
	revenueEq(&diffs, "monthly_cap_minor", got.MonthlyCapMinor, want.GetMonthlyCapMinor())
	revenueEq(&diffs, "state", got.State, int32(want.GetState()))
	revenueEq(&diffs, "effective_from", got.EffectiveFrom, want.GetEffectiveFrom())
	revenueEq(&diffs, "version", got.Version, want.GetVersion())
	revenueEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	revenueEq(&diffs, "mtime", got.Mtime, want.GetMtime())
	revenueEq(&diffs, "created_by", got.CreatedBy, want.GetCreatedBy())
	revenueEq(&diffs, "updated_by", got.UpdatedBy, want.GetUpdatedBy())
	return diffs
}

func revenueEnrollmentDiff(got types.RevenueEnrollmentItem, want *creatorrevenuerpc.EnrollmentInfo) []string {
	var diffs []string
	revenueEq(&diffs, "mid", got.Mid, want.GetMid())
	revenueEq(&diffs, "state", got.State, int32(want.GetState()))
	revenueEq(&diffs, "agreed_rule_version", got.AgreedRuleVersion, want.GetAgreedRuleVersion())
	revenueEq(&diffs, "enrolled_at", got.EnrolledAt, want.GetEnrolledAt())
	revenueEq(&diffs, "left_at", got.LeftAt, want.GetLeftAt())
	revenueEq(&diffs, "updated_at", got.UpdatedAt, want.GetUpdatedAt())
	revenueEq(&diffs, "operator", got.Operator, want.GetOperator())
	revenueEq(&diffs, "remark", got.Remark, want.GetRemark())
	return diffs
}

func revenueMetricDiff(got types.RevenueMetricItem, want *creatorrevenuerpc.RevenueMetricInfo) []string {
	var diffs []string
	revenueEq(&diffs, "metric_id", got.MetricId, want.GetMetricId())
	revenueEq(&diffs, "period", got.Period, want.GetPeriod())
	revenueEq(&diffs, "mid", got.Mid, want.GetMid())
	revenueEq(&diffs, "aid", got.Aid, want.GetAid())
	revenueEq(&diffs, "source_type", got.SourceType, int32(want.GetSourceType()))
	revenueEq(&diffs, "rule_code", got.RuleCode, want.GetRuleCode())
	revenueEq(&diffs, "rule_version", got.RuleVersion, want.GetRuleVersion())
	revenueEq(&diffs, "quantity", got.Quantity, want.GetQuantity())
	revenueEq(&diffs, "unit", got.Unit, want.GetUnit())
	revenueEq(&diffs, "amount_minor", got.AmountMinor, want.GetAmountMinor())
	revenueEq(&diffs, "capped_amount_minor", got.CappedAmountMinor, want.GetCappedAmountMinor())
	revenueEq(&diffs, "source_detail", got.SourceDetail, want.GetSourceDetail())
	revenueEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	revenueEq(&diffs, "mtime", got.Mtime, want.GetMtime())
	return diffs
}

func revenueSettlementDiff(got types.RevenueSettlementItem, want *creatorrevenuerpc.SettlementInfo) []string {
	var diffs []string
	revenueEq(&diffs, "settlement_no", got.SettlementNo, want.GetSettlementNo())
	revenueEq(&diffs, "period", got.Period, want.GetPeriod())
	revenueEq(&diffs, "mid", got.Mid, want.GetMid())
	revenueEq(&diffs, "amount_minor", got.AmountMinor, want.GetAmountMinor())
	revenueEq(&diffs, "cap_applied_minor", got.CapAppliedMinor, want.GetCapAppliedMinor())
	revenueEq(&diffs, "currency", got.Currency, want.GetCurrency())
	revenueEq(&diffs, "metric_count", got.MetricCount, want.GetMetricCount())
	revenueEq(&diffs, "state", got.State, int32(want.GetState()))
	revenueEq(&diffs, "payout_state", got.PayoutState, int32(want.GetPayoutState()))
	revenueEq(&diffs, "confirmed_at", got.ConfirmedAt, want.GetConfirmedAt())
	revenueEq(&diffs, "confirmed_by", got.ConfirmedBy, want.GetConfirmedBy())
	revenueEq(&diffs, "void_reason", got.VoidReason, want.GetVoidReason())
	revenueEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	revenueEq(&diffs, "mtime", got.Mtime, want.GetMtime())
	return diffs
}

func revenueSettlementDetailDiff(got types.RevenueSettlementDetailItem, want *creatorrevenuerpc.SettlementItem) []string {
	var diffs []string
	revenueEq(&diffs, "source_type", got.SourceType, int32(want.GetSourceType()))
	revenueEq(&diffs, "rule_code", got.RuleCode, want.GetRuleCode())
	revenueEq(&diffs, "quantity", got.Quantity, want.GetQuantity())
	revenueEq(&diffs, "amount_minor", got.AmountMinor, want.GetAmountMinor())
	return diffs
}
