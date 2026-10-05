package logic

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	featurestorerpc "go-video/services/feature-store/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 feature-store 的口径（AGENTS.md §5/§7）：
//   - rpc→types 投影逐字段不丢（found/reused/resolved_version/degradation/cursor_entity_id/
//     last_error/remaining 这些排障与审计证据都在）；
//   - operator 只能由会话渲染成 gateway/admin:<admin_id>，八条受保护入口拿不到会话即 fail-closed；
//   - 写入口的形状门槛（幂等键非空、必填枚举位不为 UNSPECIFIED、显式版本为正、主体引用齐备、
//     回填区间不倒置、limit 不越 int32）一律在调用下游之前失败；
//   - 契约里的 0 值哨兵（ACTIVE 版本指针 / 不限过滤 / 全量扫描 / 不做乐观校验 / 当前时间 /
//     服务上限批大小）原样下传，网关不替调用方挑值；
//   - 未配置 FeatureStoreRPC 时十三口一律报错，绝不回空目录——那会把「下游没接」读成
//     「这个环境一个特征都没注册」。
//
// 「值类型与维度是否自洽」「TTL/默认值是否可用」「不可变字段是否冲突」「版本能否上线」
// 「切换的乐观基线是否命中」「pn/ps 上限」「before 是否晚于服务时钟」
// 全是 services/feature-store 的领域规则（model.ValidateFeatureDefinition / ValidEntityID /
// ValidatePageSize / Privacy.OperatorPrefixes），网关不复算 —— 这里断言的是
// 「入参原样交给下游 + 下游结论原样回传」。打桩方式与 spm/cron/audit/recommend 测试一致：
// 内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库。
// 未覆盖的 WriteFeatures/GetFeature/BatchGetFeatures 一旦被调用会直接 panic（nil 接口方法），
// 这正是「三条刻意不接的路由没有任何代码路径」的可执行证明。

var errFsFakeDownstream = errors.New("feature-store downstream unavailable")

// fsAdminFake 记录每次调用的入参，并按预置值返回响应或错误。
type fsAdminFake struct {
	featurestorerpc.FeatureStoreClient

	err   error
	calls int

	registerReq    *featurestorerpc.RegisterFeatureReq
	registerReply  *featurestorerpc.RegisterFeatureReply
	stateReq       *featurestorerpc.UpdateFeatureStateReq
	stateReply     *featurestorerpc.UpdateFeatureStateReply
	privacyReq     *featurestorerpc.UpdateFeaturePrivacyReq
	privacyReply   *featurestorerpc.UpdateFeaturePrivacyReply
	defGetReq      *featurestorerpc.GetFeatureDefinitionReq
	defGetReply    *featurestorerpc.GetFeatureDefinitionReply
	defListReq     *featurestorerpc.ListFeatureDefinitionsReq
	defListReply   *featurestorerpc.ListFeatureDefinitionsReply
	switchReq      *featurestorerpc.SwitchFeatureVersionReq
	switchReply    *featurestorerpc.SwitchFeatureVersionReply
	switchListReq  *featurestorerpc.ListVersionSwitchesReq
	switchListResp *featurestorerpc.ListVersionSwitchesReply

	submitReq    *featurestorerpc.SubmitBackfillJobReq
	submitReply  *featurestorerpc.SubmitBackfillJobReply
	jobGetReq    *featurestorerpc.GetBackfillJobReq
	jobGetReply  *featurestorerpc.GetBackfillJobReply
	jobListReq   *featurestorerpc.ListBackfillJobsReq
	jobListReply *featurestorerpc.ListBackfillJobsReply

	purgeReq    *featurestorerpc.PurgeExpiredReq
	purgeReply  *featurestorerpc.PurgeExpiredReply
	eraseReq    *featurestorerpc.EraseEntityFeaturesReq
	eraseReply  *featurestorerpc.EraseEntityFeaturesReply
	entityReq   *featurestorerpc.ListEntityFeaturesReq
	entityReply *featurestorerpc.ListEntityFeaturesReply
}

func (f *fsAdminFake) RegisterFeature(_ context.Context, in *featurestorerpc.RegisterFeatureReq,
	_ ...grpc.CallOption) (*featurestorerpc.RegisterFeatureReply, error) {
	f.calls++
	f.registerReq = in
	return f.registerReply, f.err
}

func (f *fsAdminFake) UpdateFeatureState(_ context.Context, in *featurestorerpc.UpdateFeatureStateReq,
	_ ...grpc.CallOption) (*featurestorerpc.UpdateFeatureStateReply, error) {
	f.calls++
	f.stateReq = in
	return f.stateReply, f.err
}

func (f *fsAdminFake) UpdateFeaturePrivacy(_ context.Context, in *featurestorerpc.UpdateFeaturePrivacyReq,
	_ ...grpc.CallOption) (*featurestorerpc.UpdateFeaturePrivacyReply, error) {
	f.calls++
	f.privacyReq = in
	return f.privacyReply, f.err
}

func (f *fsAdminFake) GetFeatureDefinition(_ context.Context, in *featurestorerpc.GetFeatureDefinitionReq,
	_ ...grpc.CallOption) (*featurestorerpc.GetFeatureDefinitionReply, error) {
	f.calls++
	f.defGetReq = in
	return f.defGetReply, f.err
}

func (f *fsAdminFake) ListFeatureDefinitions(_ context.Context, in *featurestorerpc.ListFeatureDefinitionsReq,
	_ ...grpc.CallOption) (*featurestorerpc.ListFeatureDefinitionsReply, error) {
	f.calls++
	f.defListReq = in
	return f.defListReply, f.err
}

func (f *fsAdminFake) SwitchFeatureVersion(_ context.Context, in *featurestorerpc.SwitchFeatureVersionReq,
	_ ...grpc.CallOption) (*featurestorerpc.SwitchFeatureVersionReply, error) {
	f.calls++
	f.switchReq = in
	return f.switchReply, f.err
}

func (f *fsAdminFake) ListVersionSwitches(_ context.Context, in *featurestorerpc.ListVersionSwitchesReq,
	_ ...grpc.CallOption) (*featurestorerpc.ListVersionSwitchesReply, error) {
	f.calls++
	f.switchListReq = in
	return f.switchListResp, f.err
}

func (f *fsAdminFake) SubmitBackfillJob(_ context.Context, in *featurestorerpc.SubmitBackfillJobReq,
	_ ...grpc.CallOption) (*featurestorerpc.SubmitBackfillJobReply, error) {
	f.calls++
	f.submitReq = in
	return f.submitReply, f.err
}

func (f *fsAdminFake) GetBackfillJob(_ context.Context, in *featurestorerpc.GetBackfillJobReq,
	_ ...grpc.CallOption) (*featurestorerpc.GetBackfillJobReply, error) {
	f.calls++
	f.jobGetReq = in
	return f.jobGetReply, f.err
}

func (f *fsAdminFake) ListBackfillJobs(_ context.Context, in *featurestorerpc.ListBackfillJobsReq,
	_ ...grpc.CallOption) (*featurestorerpc.ListBackfillJobsReply, error) {
	f.calls++
	f.jobListReq = in
	return f.jobListReply, f.err
}

func (f *fsAdminFake) PurgeExpired(_ context.Context, in *featurestorerpc.PurgeExpiredReq,
	_ ...grpc.CallOption) (*featurestorerpc.PurgeExpiredReply, error) {
	f.calls++
	f.purgeReq = in
	return f.purgeReply, f.err
}

func (f *fsAdminFake) EraseEntityFeatures(_ context.Context, in *featurestorerpc.EraseEntityFeaturesReq,
	_ ...grpc.CallOption) (*featurestorerpc.EraseEntityFeaturesReply, error) {
	f.calls++
	f.eraseReq = in
	return f.eraseReply, f.err
}

func (f *fsAdminFake) ListEntityFeatures(_ context.Context, in *featurestorerpc.ListEntityFeaturesReq,
	_ ...grpc.CallOption) (*featurestorerpc.ListEntityFeaturesReply, error) {
	f.calls++
	f.entityReq = in
	return f.entityReply, f.err
}

// --- 测试夹具 ---

func fsAdminSvc(fake featurestorerpc.FeatureStoreClient) *svc.ServiceContext {
	return &svc.ServiceContext{FeatureStore: fake}
}

// fsAdminSessionCtx 模拟 AdminPermission 中间件已解析出会话身份的请求上下文。
func fsAdminSessionCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77,
		Roles:   []string{"feature_operator"},
	})
}

// fsNoSessionCtx 是「没挂上会话身份」的请求上下文：受保护入口必须 fail-closed。
func fsNoSessionCtx() context.Context {
	return context.Background()
}

func fsFixtureDefinitionRPC() *featurestorerpc.FeatureDefinition {
	return &featurestorerpc.FeatureDefinition{
		FeatureKey:    "u_play_finish_rate_7d",
		Version:       3,
		Name:          "7 日完播率",
		ValueType:     featurestorerpc.FeatureValueType_FEATURE_VALUE_TYPE_DOUBLE,
		EntityScope:   featurestorerpc.EntityScope_ENTITY_SCOPE_MID,
		Source:        featurestorerpc.FeatureSource_FEATURE_SOURCE_SPM_METRIC,
		PrivacyLevel:  featurestorerpc.PrivacyLevel_PRIVACY_LEVEL_USER_PROFILE,
		WindowSeconds: 604800,
		TtlSeconds:    86400,
		DefaultValue:  "0.0",
		Dimension:     0,
		State:         featurestorerpc.FeatureState_FEATURE_STATE_ACTIVE,
		Description:   "近 7 日 sum(有效播放时长)/sum(视频时长)",
		ChangeNote:    "v3 引入拖拽回看",
		CreatedBy:     "gateway/admin:77",
		Ctime:         1690000000,
		Mtime:         1699999999,
	}
}

func fsFixtureEntryRPC() *featurestorerpc.FeatureEntry {
	return &featurestorerpc.FeatureEntry{
		Feature: &featurestorerpc.FeatureRef{
			FeatureKey: "u_recent_click_aids",
			Version:    0,
		},
		Entity: &featurestorerpc.EntityRef{
			EntityScope: featurestorerpc.EntityScope_ENTITY_SCOPE_DEVICE,
			EntityId:    "ab12CD34ef56",
		},
		Value: &featurestorerpc.FeatureValue{
			ValueType:  featurestorerpc.FeatureValueType_FEATURE_VALUE_TYPE_INT64_LIST,
			Int64List:  []int64{10086, 10087},
			DoubleList: []float64{0.5},
		},
		ResolvedVersion: 2,
		Degradation:     featurestorerpc.FeatureDegradation_FEATURE_DEGRADATION_PREVIOUS_VERSION,
		EventTime:       1700000000,
		ExpireAt:        1700086400,
		SourceMetricKey: "finish_rate@v3",
		TtlSeconds:      86400,
	}
}

func fsFixtureJobRPC() *featurestorerpc.BackfillJob {
	return &featurestorerpc.BackfillJob{
		JobId:          4001,
		FeatureKey:     "u_play_finish_rate_7d",
		Version:        3,
		EntityScope:    featurestorerpc.EntityScope_ENTITY_SCOPE_MID,
		Source:         featurestorerpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
		State:          featurestorerpc.BackfillState_BACKFILL_STATE_RUNNING,
		WindowFrom:     1699000000,
		WindowTo:       1700000000,
		EntitiesTotal:  12000,
		EntitiesDone:   7000,
		EntitiesFailed: 1,
		CursorEntityId: 889900,
		AutoSwitch:     true,
		RequestId:      "fs-backfill-2026-09-22-01",
		Operator:       "gateway/admin:77",
		Reason:         "故障单 OPS-312：09-18 快照缺失回填",
		LastError:      "entity 889901 sample missing",
		Ctime:          1700000001,
		Mtime:          1700000106,
	}
}

func fsFixtureSwitchRecordRPC() *featurestorerpc.ListVersionSwitchesReply_SwitchRecord {
	return &featurestorerpc.ListVersionSwitchesReply_SwitchRecord{
		SwitchId:    9001,
		FeatureKey:  "u_play_finish_rate_7d",
		FromVersion: 2,
		ToVersion:   3,
		Operator:    "gateway/admin:77",
		Reason:      "离线评估 OPS-330",
		RequestId:   "fs-switch-2026-09-22-01",
		Ctime:       1700000200,
	}
}

// --- 投影 ---

// TestFsProjectionKeepsEveryField 逐结构体断言每个字段：取互不相同的非零值，
// 漏投影、错列、把 ttl_seconds 对错列、或把未知枚举压成 0 都会让这里失败。
func TestFsProjectionKeepsEveryField(t *testing.T) {
	def := fsDefinitionToAPI(fsFixtureDefinitionRPC())
	if def.FeatureKey != "u_play_finish_rate_7d" || def.Version != 3 || def.Name != "7 日完播率" ||
		def.ValueType != int32(featurestorerpc.FeatureValueType_FEATURE_VALUE_TYPE_DOUBLE) ||
		def.EntityScope != int32(featurestorerpc.EntityScope_ENTITY_SCOPE_MID) ||
		def.Source != int32(featurestorerpc.FeatureSource_FEATURE_SOURCE_SPM_METRIC) ||
		def.PrivacyLevel != int32(featurestorerpc.PrivacyLevel_PRIVACY_LEVEL_USER_PROFILE) ||
		def.WindowSeconds != 604800 || def.TTLSeconds != 86400 || def.DefaultValue != "0.0" ||
		def.Dimension != 0 ||
		def.State != int32(featurestorerpc.FeatureState_FEATURE_STATE_ACTIVE) ||
		def.Description == "" || def.ChangeNote == "" || def.CreatedBy != "gateway/admin:77" ||
		def.Ctime != 1690000000 || def.Mtime != 1699999999 {
		t.Fatalf("特征定义投影丢字段（含 ttl_seconds→TTLSeconds 的列名映射）：%+v", def)
	}

	entry := fsEntryToAPI(fsFixtureEntryRPC())
	if entry.FeatureKey != "u_recent_click_aids" || entry.FeatureVersion != 0 ||
		entry.EntityScope != int32(featurestorerpc.EntityScope_ENTITY_SCOPE_DEVICE) ||
		entry.EntityId != "ab12CD34ef56" || entry.ResolvedVersion != 2 ||
		entry.Degradation != int32(featurestorerpc.FeatureDegradation_FEATURE_DEGRADATION_PREVIOUS_VERSION) ||
		entry.EventTime != 1700000000 || entry.ExpireAt != 1700086400 ||
		entry.SourceMetricKey != "finish_rate@v3" || entry.TTLSeconds != 86400 {
		t.Fatalf("特征值条目投影丢字段（resolved_version/degradation 是降级证据）：%+v", entry)
	}
	if entry.Value.ValueType != int32(featurestorerpc.FeatureValueType_FEATURE_VALUE_TYPE_INT64_LIST) ||
		len(entry.Value.Int64List) != 2 || entry.Value.Int64List[1] != 10087 ||
		len(entry.Value.DoubleList) != 1 || entry.Value.DoubleList[0] != 0.5 {
		t.Fatalf("特征值列表位未原样搬运：%+v", entry.Value)
	}

	job := fsJobToAPI(fsFixtureJobRPC())
	if job.JobId != 4001 || job.FeatureKey != "u_play_finish_rate_7d" || job.Version != 3 ||
		job.EntityScope != int32(featurestorerpc.EntityScope_ENTITY_SCOPE_MID) ||
		job.Source != int32(featurestorerpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL) ||
		job.State != int32(featurestorerpc.BackfillState_BACKFILL_STATE_RUNNING) ||
		job.WindowFrom != 1699000000 || job.WindowTo != 1700000000 ||
		job.EntitiesTotal != 12000 || job.EntitiesDone != 7000 || job.EntitiesFailed != 1 ||
		job.CursorEntityId != 889900 || !job.AutoSwitch || job.RequestId == "" ||
		job.Operator != "gateway/admin:77" || job.Reason == "" || job.LastError == "" ||
		job.Ctime != 1700000001 || job.Mtime != 1700000106 || job.FinishedAt != 0 {
		t.Fatalf("回填作业投影丢字段（断点游标/失败数/last_error 是排障证据）：%+v", job)
	}

	rec := fsSwitchRecordsToAPI([]*featurestorerpc.ListVersionSwitchesReply_SwitchRecord{fsFixtureSwitchRecordRPC()})
	if len(rec) != 1 || rec[0].SwitchId != 9001 || rec[0].FeatureKey != "u_play_finish_rate_7d" ||
		rec[0].FromVersion != 2 || rec[0].ToVersion != 3 || rec[0].Operator != "gateway/admin:77" ||
		rec[0].Reason == "" || rec[0].RequestId == "" || rec[0].Ctime != 1700000200 {
		t.Fatalf("切换审计投影丢字段：%+v", rec)
	}
}

// TestFsDefinitionForRPCLeavesServerOwnedFieldsEmpty 锁住注册表单的组装：
// state/created_by/ctime/mtime 四位不由网关填——注册一律以 DRAFT 入库，
// 经办人与库时钟只能由服务渲染（网关自报经办人等于伪造审计主体）。
func TestFsDefinitionForRPCLeavesServerOwnedFieldsEmpty(t *testing.T) {
	got := fsDefinitionForRPC(types.FsFeatureDefinitionInput{
		FeatureKey:    "u_play_finish_rate_7d",
		Version:       3,
		Name:          "7 日完播率",
		ValueType:     2,
		EntityScope:   1,
		Source:        1,
		PrivacyLevel:  4,
		WindowSeconds: 604800,
		TTLSeconds:    86400,
		DefaultValue:  "0.0",
		Dimension:     0,
		Description:   "近 7 日完播率",
		ChangeNote:    "v3",
	})
	if got.State != featurestorerpc.FeatureState_FEATURE_STATE_UNSPECIFIED ||
		got.CreatedBy != "" || got.Ctime != 0 || got.Mtime != 0 {
		t.Fatalf("网关填了 state/created_by/ctime/mtime 之一，等于替服务做上线决定：%+v", got)
	}
	if got.TtlSeconds != 86400 || got.WindowSeconds != 604800 || got.DefaultValue != "0.0" {
		t.Fatalf("数值位未原样下传：%+v", got)
	}
	if got.ValueType != featurestorerpc.FeatureValueType(2) || got.EntityScope != featurestorerpc.EntityScope(1) ||
		got.Source != featurestorerpc.FeatureSource(1) || got.PrivacyLevel != featurestorerpc.PrivacyLevel(4) {
		t.Fatalf("枚举位未逐位转换：%+v", got)
	}
}

// TestFsProjectionsReturnEmptySliceNotNil 锁住「列表回 [] 而不是 null」。
func TestFsProjectionsReturnEmptySliceNotNil(t *testing.T) {
	cases := map[string]any{
		"defs":    fsDefinitionsToAPI(nil),
		"entries": fsEntriesToAPI(nil),
		"switch":  fsSwitchRecordsToAPI(nil),
		"jobs":    fsJobsToAPI(nil),
	}
	for name, v := range cases {
		if reflect.ValueOf(v).IsNil() {
			t.Errorf("%s 投影返回 nil，后台会收到 null 而不是 []", name)
		}
	}
	// nil 结构体不 panic：返回零值行，让后台看到「空」而不是 500。
	if got := fsDefinitionToAPI(nil); got.FeatureKey != "" {
		t.Errorf("nil 定义应投影成零值：%+v", got)
	}
	if got := fsEntryToAPI(nil); got.Degradation != 0 || got.Value.Int64List == nil {
		t.Errorf("nil 条目应投影成零值且列表非 nil：%+v", got)
	}
	if got := fsJobToAPI(nil); got.JobId != 0 {
		t.Errorf("nil 作业应投影成零值：%+v", got)
	}
	// 值投影的两个列表位即使源为 nil 也必须是非 nil 切片。
	v := fsFeatureValueToAPI(nil)
	if v.Int64List == nil || v.DoubleList == nil {
		t.Errorf("nil 特征值的列表位应回 []：%#v", v)
	}
}

// --- 身份与门槛 ---

// TestFsProtectedRoutesRequireSession 八条受保护入口（7 写 + 1 定向个人导出读）
// 在没有会话身份时必须拒绝且不打下游。
func TestFsProtectedRoutesRequireSession(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context, s *svc.ServiceContext) error
	}{
		{"entityFeatureList", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewFsEntityFeatureListLogic(ctx, s).FsEntityFeatureList(&types.ParamFsEntityFeatureList{
				EntityScope: 1, EntityId: "10086", Pn: 1, Ps: 20,
			})
			return err
		}},
		{"definitionRegister", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewFsDefinitionRegisterLogic(ctx, s).FsDefinitionRegister(&types.ParamFsDefinitionRegister{
				Definition:     types.FsFeatureDefinitionInput{FeatureKey: "k", Version: 1, ValueType: 1, EntityScope: 1, Source: 1, PrivacyLevel: 1, Description: "d"},
				IdempotencyKey: "k-1",
			})
			return err
		}},
		{"definitionState", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewFsDefinitionStateLogic(ctx, s).FsDefinitionState(&types.ParamFsDefinitionState{
				FeatureKey: "k", Version: 2, State: 2, Reason: "上线 v2", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"definitionPrivacy", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewFsDefinitionPrivacyLogic(ctx, s).FsDefinitionPrivacy(&types.ParamFsDefinitionPrivacy{
				FeatureKey: "k", Version: 2, PrivacyLevel: 2, Reason: "降档工单", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"versionSwitch", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewFsVersionSwitchLogic(ctx, s).FsVersionSwitch(&types.ParamFsVersionSwitch{
				FeatureKey: "k", FromVersion: 2, ToVersion: 3, Reason: "评估结论", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"backfillSubmit", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewFsBackfillSubmitLogic(ctx, s).FsBackfillSubmit(&types.ParamFsBackfillSubmit{
				FeatureKey: "k", Version: 3, EntityScope: 1, Source: 4, WindowFrom: 100,
				Reason: "回填", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"entityFeatureErase", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewFsEntityFeatureEraseLogic(ctx, s).FsEntityFeatureErase(&types.ParamFsEntityFeatureErase{
				EntityScope: 1, EntityId: "10086", Reason: "工单 66", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"retentionPurge", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewFsRetentionPurgeLogic(ctx, s).FsRetentionPurge(&types.ParamFsRetentionPurge{
				IdempotencyKey: "k-1",
			})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fsAdminFake{}
			if err := tc.call(fsNoSessionCtx(), fsAdminSvc(fake)); !errors.Is(err, errFsSessionRequired) {
				t.Fatalf("无会话应返回 errFsSessionRequired，实际 %v", err)
			}
			if fake.calls != 0 {
				t.Fatalf("无会话却打了下游 %d 次", fake.calls)
			}
			if err := tc.call(fsAdminSessionCtx(), fsAdminSvc(fake)); err != nil {
				t.Fatalf("有会话时不该失败：%v", err)
			}
			if fake.calls != 1 {
				t.Fatalf("有会话时应打一次下游，实际 %d 次", fake.calls)
			}
		})
	}
}

// TestFsProtectedRoutesRenderOperatorFromSession 受保护入口的 operator 必须是会话渲染出的
// gateway/admin:<admin_id>，且幂等键原样落到 request_id（改一个字符等于换一次执行权）。
func TestFsProtectedRoutesRenderOperatorFromSession(t *testing.T) {
	fake := &fsAdminFake{
		registerReply: &featurestorerpc.RegisterFeatureReply{},
		stateReply:    &featurestorerpc.UpdateFeatureStateReply{},
		switchReply:   &featurestorerpc.SwitchFeatureVersionReply{},
		submitReply:   &featurestorerpc.SubmitBackfillJobReply{},
		eraseReply:    &featurestorerpc.EraseEntityFeaturesReply{},
		purgeReply:    &featurestorerpc.PurgeExpiredReply{},
	}
	s := fsAdminSvc(fake)
	ctx := fsAdminSessionCtx()

	if _, err := NewFsDefinitionRegisterLogic(ctx, s).FsDefinitionRegister(&types.ParamFsDefinitionRegister{
		Definition:     types.FsFeatureDefinitionInput{FeatureKey: " 带空格的键 ", Version: 1, ValueType: 1, EntityScope: 1, Source: 1, PrivacyLevel: 1, Description: "d"},
		IdempotencyKey: "  idem-1  ",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.registerReq.GetOperator() != "gateway/admin:77" {
		t.Fatalf("operator 未由会话渲染：%q", fake.registerReq.GetOperator())
	}
	// 幂等键与 feature_key 都按原值下传（服务自己 trim）：网关悄悄 trim 会让「带空格的键」
	// 与运营看到的那一条不是同一个值。
	if fake.registerReq.GetRequestId() != "  idem-1  " {
		t.Fatalf("幂等键被改写：%q", fake.registerReq.GetRequestId())
	}
	if fake.registerReq.GetDefinition().GetFeatureKey() != " 带空格的键 " {
		t.Fatalf("feature_key 被改写：%q", fake.registerReq.GetDefinition().GetFeatureKey())
	}

	if _, err := NewFsDefinitionStateLogic(ctx, s).FsDefinitionState(&types.ParamFsDefinitionState{
		FeatureKey: "k", Version: 2, State: 3, Reason: "下线", IdempotencyKey: "idem-2",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.stateReq.GetOperator() != "gateway/admin:77" || fake.stateReq.GetRequestId() != "idem-2" ||
		fake.stateReq.GetReason() != "下线" ||
		fake.stateReq.GetState() != featurestorerpc.FeatureState_FEATURE_STATE_RETIRED {
		t.Fatalf("状态入口入参不完整下传：%+v", fake.stateReq)
	}

	if _, err := NewFsVersionSwitchLogic(ctx, s).FsVersionSwitch(&types.ParamFsVersionSwitch{
		FeatureKey: "k", FromVersion: 2, ToVersion: 3, ExpectedFromVersion: 2,
		Reason: "OPS-330", IdempotencyKey: "idem-3",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.switchReq.GetOperator() != "gateway/admin:77" || fake.switchReq.GetExpectedFromVersion() != 2 ||
		fake.switchReq.GetFromVersion() != 2 || fake.switchReq.GetToVersion() != 3 ||
		fake.switchReq.GetReason() != "OPS-330" {
		t.Fatalf("切换入口入参不完整下传：%+v", fake.switchReq)
	}

	if _, err := NewFsEntityFeatureEraseLogic(ctx, s).FsEntityFeatureErase(&types.ParamFsEntityFeatureErase{
		EntityScope: 4, EntityId: "AB12cd34", MinPrivacyLevel: 3, Reason: "工单 66", IdempotencyKey: "idem-4",
	}); err != nil {
		t.Fatal(err)
	}
	// 哈希摘要大小写敏感：网关不得 trim 或 lower。
	if fake.eraseReq.GetEntity().GetEntityId() != "AB12cd34" {
		t.Fatalf("entity_id 被改写：%q", fake.eraseReq.GetEntity().GetEntityId())
	}
	if fake.eraseReq.GetOperator() != "gateway/admin:77" ||
		fake.eraseReq.GetMinPrivacyLevel() != featurestorerpc.PrivacyLevel_PRIVACY_LEVEL_PSEUDONYMOUS {
		t.Fatalf("擦除入口入参不完整下传：%+v", fake.eraseReq)
	}

	if _, err := NewFsRetentionPurgeLogic(ctx, s).FsRetentionPurge(&types.ParamFsRetentionPurge{
		Limit: 500, Before: 1700000000, IdempotencyKey: "idem-5",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.purgeReq.GetOperator() != "gateway/admin:77" || fake.purgeReq.GetLimit() != 500 ||
		fake.purgeReq.GetBefore() != 1700000000 || fake.purgeReq.GetRequestId() != "idem-5" {
		t.Fatalf("清理入口入参不完整下传：%+v", fake.purgeReq)
	}

	if _, err := NewFsBackfillSubmitLogic(ctx, s).FsBackfillSubmit(&types.ParamFsBackfillSubmit{
		FeatureKey: "k", Version: 3, EntityScope: 1, Source: 4, WindowFrom: 100, WindowTo: 200,
		EntityIds: []string{"1", "2"}, Reason: "回填", IdempotencyKey: "idem-6",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.submitReq.GetOperator() != "gateway/admin:77" || len(fake.submitReq.GetEntityIds()) != 2 ||
		fake.submitReq.GetReason() != "回填" {
		t.Fatalf("回填入口入参不完整下传：%+v", fake.submitReq)
	}
}

// TestFsGateRejectsBeforeDownstream 形状门槛必须在打下游之前失败：
// 每条都断言 fake.calls==0，否则「门槛生效」与「下游恰好报错」在测试里分不开。
func TestFsGateRejectsBeforeDownstream(t *testing.T) {
	ctx := fsAdminSessionCtx()
	cases := []struct {
		name string
		call func(s *svc.ServiceContext) error
	}{
		{"注册缺幂等键", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionRegisterLogic(ctx, s).FsDefinitionRegister(&types.ParamFsDefinitionRegister{
				Definition: types.FsFeatureDefinitionInput{FeatureKey: "k", Version: 1, ValueType: 1, EntityScope: 1, Source: 1, PrivacyLevel: 1},
			})
			return err
		}},
		{"注册版本为0", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionRegisterLogic(ctx, s).FsDefinitionRegister(&types.ParamFsDefinitionRegister{
				Definition:     types.FsFeatureDefinitionInput{FeatureKey: "k", Version: 0, ValueType: 1, EntityScope: 1, Source: 1, PrivacyLevel: 1},
				IdempotencyKey: "k-1",
			})
			return err
		}},
		{"注册隐私级别未声明", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionRegisterLogic(ctx, s).FsDefinitionRegister(&types.ParamFsDefinitionRegister{
				Definition:     types.FsFeatureDefinitionInput{FeatureKey: "k", Version: 1, ValueType: 1, EntityScope: 1, Source: 1, PrivacyLevel: 0},
				IdempotencyKey: "k-1",
			})
			return err
		}},
		{"注册值类型未声明", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionRegisterLogic(ctx, s).FsDefinitionRegister(&types.ParamFsDefinitionRegister{
				Definition:     types.FsFeatureDefinitionInput{FeatureKey: "k", Version: 1, ValueType: 0, EntityScope: 1, Source: 1, PrivacyLevel: 1},
				IdempotencyKey: "k-1",
			})
			return err
		}},
		{"注册窗口为负", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionRegisterLogic(ctx, s).FsDefinitionRegister(&types.ParamFsDefinitionRegister{
				Definition:     types.FsFeatureDefinitionInput{FeatureKey: "k", Version: 1, ValueType: 1, EntityScope: 1, Source: 1, PrivacyLevel: 1, WindowSeconds: -1},
				IdempotencyKey: "k-1",
			})
			return err
		}},
		{"状态迁移目标未选", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionStateLogic(ctx, s).FsDefinitionState(&types.ParamFsDefinitionState{
				FeatureKey: "k", Version: 2, State: 0, Reason: "r", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"状态迁移版本用指针哨兵", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionStateLogic(ctx, s).FsDefinitionState(&types.ParamFsDefinitionState{
				FeatureKey: "k", Version: 0, State: 2, Reason: "r", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"状态迁移缺理由", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionStateLogic(ctx, s).FsDefinitionState(&types.ParamFsDefinitionState{
				FeatureKey: "k", Version: 2, State: 2, Reason: "   ", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"隐私调整级别未声明", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionPrivacyLogic(ctx, s).FsDefinitionPrivacy(&types.ParamFsDefinitionPrivacy{
				FeatureKey: "k", Version: 2, PrivacyLevel: 0, Reason: "r", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"切换目标版本为0", func(s *svc.ServiceContext) error {
			_, err := NewFsVersionSwitchLogic(ctx, s).FsVersionSwitch(&types.ParamFsVersionSwitch{
				FeatureKey: "k", FromVersion: 2, ToVersion: 0, Reason: "r", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"切换缺理由", func(s *svc.ServiceContext) error {
			_, err := NewFsVersionSwitchLogic(ctx, s).FsVersionSwitch(&types.ParamFsVersionSwitch{
				FeatureKey: "k", FromVersion: 2, ToVersion: 3, Reason: "", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"回填区间倒置", func(s *svc.ServiceContext) error {
			_, err := NewFsBackfillSubmitLogic(ctx, s).FsBackfillSubmit(&types.ParamFsBackfillSubmit{
				FeatureKey: "k", Version: 3, EntityScope: 1, Source: 4, WindowFrom: 200, WindowTo: 100,
				Reason: "r", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"回填来源未声明", func(s *svc.ServiceContext) error {
			_, err := NewFsBackfillSubmitLogic(ctx, s).FsBackfillSubmit(&types.ParamFsBackfillSubmit{
				FeatureKey: "k", Version: 3, EntityScope: 1, Source: 0, WindowFrom: 100,
				Reason: "r", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"擦除缺主体标识", func(s *svc.ServiceContext) error {
			_, err := NewFsEntityFeatureEraseLogic(ctx, s).FsEntityFeatureErase(&types.ParamFsEntityFeatureErase{
				EntityScope: 1, EntityId: "  ", Reason: "工单", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"擦除主体维度未声明", func(s *svc.ServiceContext) error {
			_, err := NewFsEntityFeatureEraseLogic(ctx, s).FsEntityFeatureErase(&types.ParamFsEntityFeatureErase{
				EntityScope: 0, EntityId: "10086", Reason: "工单", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"擦除缺工单理由", func(s *svc.ServiceContext) error {
			_, err := NewFsEntityFeatureEraseLogic(ctx, s).FsEntityFeatureErase(&types.ParamFsEntityFeatureErase{
				EntityScope: 1, EntityId: "10086", Reason: "", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"清理批大小越 int32", func(s *svc.ServiceContext) error {
			_, err := NewFsRetentionPurgeLogic(ctx, s).FsRetentionPurge(&types.ParamFsRetentionPurge{
				Limit: math.MaxInt32 + 1, IdempotencyKey: "k-1",
			})
			return err
		}},
		{"清理批大小为负", func(s *svc.ServiceContext) error {
			_, err := NewFsRetentionPurgeLogic(ctx, s).FsRetentionPurge(&types.ParamFsRetentionPurge{
				Limit: -1, IdempotencyKey: "k-1",
			})
			return err
		}},
		{"清理缺幂等键", func(s *svc.ServiceContext) error {
			_, err := NewFsRetentionPurgeLogic(ctx, s).FsRetentionPurge(&types.ParamFsRetentionPurge{})
			return err
		}},
		{"导出页大小缺省", func(s *svc.ServiceContext) error {
			_, err := NewFsEntityFeatureListLogic(ctx, s).FsEntityFeatureList(&types.ParamFsEntityFeatureList{
				EntityScope: 1, EntityId: "10086", Pn: 1, Ps: 0,
			})
			return err
		}},
		{"单定义读缺 key", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionGetLogic(context.Background(), s).FsDefinitionGet(&types.ParamFsDefinitionGet{Version: 2})
			return err
		}},
		{"定义列表过滤位为负", func(s *svc.ServiceContext) error {
			_, err := NewFsDefinitionListLogic(context.Background(), s).FsDefinitionList(&types.ParamFsDefinitionList{
				EntityScope: -1, Pn: 1, Ps: 20,
			})
			return err
		}},
		{"切换流水页码为0", func(s *svc.ServiceContext) error {
			_, err := NewFsVersionSwitchListLogic(context.Background(), s).FsVersionSwitchList(&types.ParamFsVersionSwitchList{
				Pn: 0, Ps: 20,
			})
			return err
		}},
		{"作业读没有寻址位", func(s *svc.ServiceContext) error {
			_, err := NewFsBackfillGetLogic(context.Background(), s).FsBackfillGet(&types.ParamFsBackfillGet{})
			return err
		}},
		{"作业读只有空白 request_id", func(s *svc.ServiceContext) error {
			_, err := NewFsBackfillGetLogic(context.Background(), s).FsBackfillGet(&types.ParamFsBackfillGet{RequestId: "  "})
			return err
		}},
		{"作业列表 since 为负", func(s *svc.ServiceContext) error {
			_, err := NewFsBackfillListLogic(context.Background(), s).FsBackfillList(&types.ParamFsBackfillList{
				Since: -1, Pn: 1, Ps: 20,
			})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fsAdminFake{}
			err := tc.call(fsAdminSvc(fake))
			if err == nil {
				t.Fatalf("门槛未生效：请求被放行到下游")
			}
			if !strings.HasPrefix(err.Error(), "gateway/admin:") {
				t.Fatalf("错误必须是网关自己的门槛错误（不能是下游错误冒名）：%v", err)
			}
			if fake.calls != 0 {
				t.Fatalf("门槛失败却打了下游 %d 次", fake.calls)
			}
		})
	}
}

// TestFsSentinelsPassThroughUnchanged 契约里的 0 值哨兵必须原样下传：
// 换成「具体值」等于替调用方做了一个它没做的决定。
func TestFsSentinelsPassThroughUnchanged(t *testing.T) {
	fake := &fsAdminFake{
		defGetReply:    &featurestorerpc.GetFeatureDefinitionReply{},
		defListReply:   &featurestorerpc.ListFeatureDefinitionsReply{},
		switchListResp: &featurestorerpc.ListVersionSwitchesReply{},
		jobGetReply:    &featurestorerpc.GetBackfillJobReply{},
		jobListReply:   &featurestorerpc.ListBackfillJobsReply{},
		entityReply:    &featurestorerpc.ListEntityFeaturesReply{},
		switchReply:    &featurestorerpc.SwitchFeatureVersionReply{},
		submitReply:    &featurestorerpc.SubmitBackfillJobReply{},
		purgeReply:     &featurestorerpc.PurgeExpiredReply{},
		eraseReply:     &featurestorerpc.EraseEntityFeaturesReply{},
	}
	s := fsAdminSvc(fake)
	ctx := fsAdminSessionCtx()

	if _, err := NewFsDefinitionGetLogic(context.Background(), s).FsDefinitionGet(&types.ParamFsDefinitionGet{
		FeatureKey: "k",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.defGetReq.GetVersion() != 0 {
		t.Fatalf("未给版本时应下传 0（ACTIVE 指针哨兵），实际 %d", fake.defGetReq.GetVersion())
	}

	if _, err := NewFsDefinitionListLogic(context.Background(), s).FsDefinitionList(&types.ParamFsDefinitionList{
		Pn: 1, Ps: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if fake.defListReq.GetEntityScope() != featurestorerpc.EntityScope_ENTITY_SCOPE_UNSPECIFIED ||
		fake.defListReq.GetSource() != featurestorerpc.FeatureSource_FEATURE_SOURCE_UNSPECIFIED ||
		fake.defListReq.GetState() != featurestorerpc.FeatureState_FEATURE_STATE_UNSPECIFIED ||
		fake.defListReq.GetMaxPrivacyLevel() != featurestorerpc.PrivacyLevel_PRIVACY_LEVEL_UNSPECIFIED {
		t.Fatalf("目录读的「不限」过滤位被改写：%+v", fake.defListReq)
	}

	if _, err := NewFsVersionSwitchListLogic(context.Background(), s).FsVersionSwitchList(&types.ParamFsVersionSwitchList{
		Pn: 1, Ps: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if fake.switchListReq.GetSince() != 0 || fake.switchListReq.GetFeatureKey() != "" {
		t.Fatalf("切换审计的「不限」哨兵被改写：%+v", fake.switchListReq)
	}

	if _, err := NewFsBackfillGetLogic(context.Background(), s).FsBackfillGet(&types.ParamFsBackfillGet{
		RequestId: "req-1",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.jobGetReq.GetJobId() != 0 || fake.jobGetReq.GetRequestId() != "req-1" {
		t.Fatalf("作业读的二选一主体被改写：%+v", fake.jobGetReq)
	}

	if _, err := NewFsBackfillListLogic(context.Background(), s).FsBackfillList(&types.ParamFsBackfillList{
		Pn: 1, Ps: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if fake.jobListReq.GetState() != featurestorerpc.BackfillState_BACKFILL_STATE_UNSPECIFIED ||
		fake.jobListReq.GetSince() != 0 {
		t.Fatalf("作业列表的「不限」哨兵被改写：%+v", fake.jobListReq)
	}

	if _, err := NewFsEntityFeatureListLogic(ctx, s).FsEntityFeatureList(&types.ParamFsEntityFeatureList{
		EntityScope: 4, EntityId: "ab12", Pn: 1, Ps: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if fake.entityReq.GetMinPrivacyLevel() != featurestorerpc.PrivacyLevel_PRIVACY_LEVEL_UNSPECIFIED {
		t.Fatalf("导出的「不限级别」哨兵被改写：%+v", fake.entityReq)
	}

	if _, err := NewFsVersionSwitchLogic(ctx, s).FsVersionSwitch(&types.ParamFsVersionSwitch{
		FeatureKey: "k", FromVersion: 2, ToVersion: 3, Reason: "r", IdempotencyKey: "k-1",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.switchReq.GetExpectedFromVersion() != 0 {
		t.Fatalf("expected_from_version=0（不校验）被改写：%+v", fake.switchReq)
	}

	if _, err := NewFsBackfillSubmitLogic(ctx, s).FsBackfillSubmit(&types.ParamFsBackfillSubmit{
		FeatureKey: "k", Version: 3, EntityScope: 1, Source: 4, WindowFrom: 100,
		Reason: "r", IdempotencyKey: "k-1",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.submitReq.GetWindowTo() != 0 || fake.submitReq.GetFromVersion() != 0 ||
		len(fake.submitReq.GetEntityIds()) != 0 {
		t.Fatalf("回填的 window_to/from_version/entity_ids 哨兵被改写：%+v", fake.submitReq)
	}

	if _, err := NewFsRetentionPurgeLogic(ctx, s).FsRetentionPurge(&types.ParamFsRetentionPurge{
		IdempotencyKey: "k-1",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.purgeReq.GetLimit() != 0 || fake.purgeReq.GetBefore() != 0 {
		t.Fatalf("清理的 limit/before 哨兵被改写：%+v", fake.purgeReq)
	}

	if _, err := NewFsEntityFeatureEraseLogic(ctx, s).FsEntityFeatureErase(&types.ParamFsEntityFeatureErase{
		EntityScope: 1, EntityId: "10086", Reason: "工单", IdempotencyKey: "k-1",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.eraseReq.GetMinPrivacyLevel() != featurestorerpc.PrivacyLevel_PRIVACY_LEVEL_UNSPECIFIED {
		t.Fatalf("擦除的「全部个体特征」哨兵被改写：%+v", fake.eraseReq)
	}
}

// TestFsOpenReadRoutesDoNotRequireSession 五处目录/流水读不要求会话身份：
// 挂判定会把 operation 变成排障页的读放大瓶颈（与 spm 读面同口径）。
func TestFsOpenReadRoutesDoNotRequireSession(t *testing.T) {
	fake := &fsAdminFake{
		defGetReply:    &featurestorerpc.GetFeatureDefinitionReply{},
		defListReply:   &featurestorerpc.ListFeatureDefinitionsReply{},
		switchListResp: &featurestorerpc.ListVersionSwitchesReply{},
		jobListReply:   &featurestorerpc.ListBackfillJobsReply{},
	}
	noSession := fsNoSessionCtx()
	s := fsAdminSvc(fake)

	if _, err := NewFsDefinitionGetLogic(noSession, s).FsDefinitionGet(&types.ParamFsDefinitionGet{FeatureKey: "k"}); err != nil {
		t.Fatalf("单定义读不该要求会话身份：%v", err)
	}
	if _, err := NewFsDefinitionListLogic(noSession, s).FsDefinitionList(&types.ParamFsDefinitionList{Pn: 1, Ps: 20}); err != nil {
		t.Fatalf("定义目录读不该要求会话身份：%v", err)
	}
	if _, err := NewFsVersionSwitchListLogic(noSession, s).FsVersionSwitchList(&types.ParamFsVersionSwitchList{Pn: 1, Ps: 20}); err != nil {
		t.Fatalf("切换审计读不该要求会话身份：%v", err)
	}
	if _, err := NewFsBackfillListLogic(noSession, s).FsBackfillList(&types.ParamFsBackfillList{Pn: 1, Ps: 20}); err != nil {
		t.Fatalf("作业列表读不该要求会话身份：%v", err)
	}
	if fake.defListReq.GetPs() != 20 || fake.entityReq != nil {
		t.Fatalf("只读入口入参异常：%+v", fake.defListReq)
	}
}

// TestFsListRoutesReturnEmptySliceNotNil 下游回空列表时后台必须收到 []（长度 0 非 nil），
// 并且 found/total 这类「服务结论」必须回传而不是被折叠。
func TestFsListRoutesReturnEmptySliceNotNil(t *testing.T) {
	fake := &fsAdminFake{
		defGetReply:  &featurestorerpc.GetFeatureDefinitionReply{Found: false},
		defListReply: &featurestorerpc.ListFeatureDefinitionsReply{},
		jobGetReply:  &featurestorerpc.GetBackfillJobReply{Found: false},
		jobListReply: &featurestorerpc.ListBackfillJobsReply{Total: 0},
	}
	ctx := context.Background()
	s := fsAdminSvc(fake)

	defs, err := NewFsDefinitionListLogic(ctx, s).FsDefinitionList(&types.ParamFsDefinitionList{Pn: 1, Ps: 20})
	if err != nil {
		t.Fatal(err)
	}
	if defs.Data.Definitions == nil {
		t.Fatal("空特征目录应回 []，否则后台收到 null")
	}
	switches, err := NewFsVersionSwitchListLogic(ctx, s).FsVersionSwitchList(
		&types.ParamFsVersionSwitchList{Pn: 1, Ps: 20})
	if err != nil {
		t.Fatal(err)
	}
	if switches.Data.Items == nil {
		t.Fatal("空切换审计应回 []")
	}
	jobs, err := NewFsBackfillListLogic(ctx, s).FsBackfillList(&types.ParamFsBackfillList{Pn: 1, Ps: 20})
	if err != nil {
		t.Fatal(err)
	}
	if jobs.Data.Jobs == nil {
		t.Fatal("空作业列表应回 []")
	}
	def, err := NewFsDefinitionGetLogic(ctx, s).FsDefinitionGet(&types.ParamFsDefinitionGet{FeatureKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if def.Data.Found {
		t.Fatal("found=false 被网关改成 true")
	}
	job, err := NewFsBackfillGetLogic(ctx, s).FsBackfillGet(&types.ParamFsBackfillGet{JobId: 1})
	if err != nil {
		t.Fatal(err)
	}
	if job.Data.Found {
		t.Fatal("作业 found=false 被网关改成 true")
	}
	if fake.entityReply != nil {
		t.Fatal("只读路由不该触发定向导出")
	}
}

// TestFsEntityExportKeepsServiceConvergence 定向导出：服务把超出可导出级别的条目收敛成空集时，
// 网关不得「补一次不带级别的读」把范围放宽，也不得把空集报成错误。
func TestFsEntityExportKeepsServiceConvergence(t *testing.T) {
	fake := &fsAdminFake{entityReply: &featurestorerpc.ListEntityFeaturesReply{Total: 12}}
	if _, err := NewFsEntityFeatureListLogic(fsAdminSessionCtx(), fsAdminSvc(fake)).FsEntityFeatureList(
		&types.ParamFsEntityFeatureList{EntityScope: 1, EntityId: "10086", MinPrivacyLevel: 4, Pn: 1, Ps: 20}); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Fatalf("被服务收敛后网关又放宽重读了一次（下游调用 %d 次）", fake.calls)
	}
	if fake.entityReq.GetMinPrivacyLevel() != featurestorerpc.PrivacyLevel_PRIVACY_LEVEL_USER_PROFILE {
		t.Fatalf("调用方给的级别被改写：%+v", fake.entityReq)
	}
}

// TestFsDownstreamErrorPropagatesVerbatim 下游错误原样上抛：不吞、不改写成空结果、不伪造 found。
func TestFsDownstreamErrorPropagatesVerbatim(t *testing.T) {
	fake := &fsAdminFake{
		err:           errFsFakeDownstream,
		defGetReply:   &featurestorerpc.GetFeatureDefinitionReply{Found: true},
		defListReply:  &featurestorerpc.ListFeatureDefinitionsReply{},
		purgeReply:    &featurestorerpc.PurgeExpiredReply{Purged: 9},
		registerReply: &featurestorerpc.RegisterFeatureReply{Created: true},
	}
	s := fsAdminSvc(fake)

	if _, err := NewFsDefinitionGetLogic(context.Background(), s).FsDefinitionGet(
		&types.ParamFsDefinitionGet{FeatureKey: "k"}); !errors.Is(err, errFsFakeDownstream) {
		t.Fatalf("下游错误被改写：%v", err)
	}
	if _, err := NewFsDefinitionListLogic(context.Background(), s).FsDefinitionList(
		&types.ParamFsDefinitionList{Pn: 1, Ps: 20}); !errors.Is(err, errFsFakeDownstream) {
		t.Fatalf("目录读的错误被改写：%v", err)
	}
	resp, err := NewFsRetentionPurgeLogic(fsAdminSessionCtx(), s).FsRetentionPurge(
		&types.ParamFsRetentionPurge{IdempotencyKey: "k-1"})
	if err == nil {
		t.Fatalf("清理失败却回了成功响应：%+v", resp)
	}
	if !errors.Is(err, errFsFakeDownstream) {
		t.Fatalf("清理错误被改写：%v", err)
	}
	if resp != nil {
		t.Fatalf("失败时不应带回 purged=9 的响应：%+v", resp)
	}
	if _, err := NewFsDefinitionRegisterLogic(fsAdminSessionCtx(), s).FsDefinitionRegister(
		&types.ParamFsDefinitionRegister{
			Definition:     types.FsFeatureDefinitionInput{FeatureKey: "k", Version: 1, ValueType: 1, EntityScope: 1, Source: 1, PrivacyLevel: 1},
			IdempotencyKey: "k-1",
		}); !errors.Is(err, errFsFakeDownstream) {
		t.Fatalf("注册错误被改写：%v", err)
	}
}

// TestFsRoutesFailClosedWhenClientNotConfigured 未配置 FeatureStoreRPC 时十三口一律报错：
// 不回空目录（会把「下游没接」读成「一个特征都没注册」），也不伪造 created/switched=true。
func TestFsRoutesFailClosedWhenClientNotConfigured(t *testing.T) {
	s := &svc.ServiceContext{}
	ctx := fsAdminSessionCtx()
	cases := map[string]func() error{
		"definitionGet": func() error {
			_, err := NewFsDefinitionGetLogic(ctx, s).FsDefinitionGet(&types.ParamFsDefinitionGet{FeatureKey: "k"})
			return err
		},
		"definitionList": func() error {
			_, err := NewFsDefinitionListLogic(ctx, s).FsDefinitionList(&types.ParamFsDefinitionList{Pn: 1, Ps: 20})
			return err
		},
		"versionSwitchList": func() error {
			_, err := NewFsVersionSwitchListLogic(ctx, s).FsVersionSwitchList(&types.ParamFsVersionSwitchList{Pn: 1, Ps: 20})
			return err
		},
		"backfillGet": func() error {
			_, err := NewFsBackfillGetLogic(ctx, s).FsBackfillGet(&types.ParamFsBackfillGet{JobId: 1})
			return err
		},
		"backfillList": func() error {
			_, err := NewFsBackfillListLogic(ctx, s).FsBackfillList(&types.ParamFsBackfillList{Pn: 1, Ps: 20})
			return err
		},
		"entityFeatureList": func() error {
			_, err := NewFsEntityFeatureListLogic(ctx, s).FsEntityFeatureList(&types.ParamFsEntityFeatureList{EntityScope: 1, EntityId: "1", Pn: 1, Ps: 20})
			return err
		},
		"definitionRegister": func() error {
			_, err := NewFsDefinitionRegisterLogic(ctx, s).FsDefinitionRegister(&types.ParamFsDefinitionRegister{
				Definition: types.FsFeatureDefinitionInput{FeatureKey: "k", Version: 1, ValueType: 1, EntityScope: 1, Source: 1, PrivacyLevel: 1},
			})
			return err
		},
		"definitionState": func() error {
			_, err := NewFsDefinitionStateLogic(ctx, s).FsDefinitionState(&types.ParamFsDefinitionState{FeatureKey: "k", Version: 1, State: 2, Reason: "r"})
			return err
		},
		"definitionPrivacy": func() error {
			_, err := NewFsDefinitionPrivacyLogic(ctx, s).FsDefinitionPrivacy(&types.ParamFsDefinitionPrivacy{FeatureKey: "k", Version: 1, PrivacyLevel: 2, Reason: "r"})
			return err
		},
		"versionSwitch": func() error {
			_, err := NewFsVersionSwitchLogic(ctx, s).FsVersionSwitch(&types.ParamFsVersionSwitch{FeatureKey: "k", FromVersion: 1, ToVersion: 2, Reason: "r"})
			return err
		},
		"backfillSubmit": func() error {
			_, err := NewFsBackfillSubmitLogic(ctx, s).FsBackfillSubmit(&types.ParamFsBackfillSubmit{FeatureKey: "k", Version: 1, EntityScope: 1, Source: 4, WindowFrom: 1, Reason: "r"})
			return err
		},
		"entityFeatureErase": func() error {
			_, err := NewFsEntityFeatureEraseLogic(ctx, s).FsEntityFeatureErase(&types.ParamFsEntityFeatureErase{EntityScope: 1, EntityId: "1", Reason: "r"})
			return err
		},
		"retentionPurge": func() error {
			_, err := NewFsRetentionPurgeLogic(ctx, s).FsRetentionPurge(&types.ParamFsRetentionPurge{})
			return err
		},
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, errFeatureStoreNotConfigured) {
			t.Errorf("%s 在未配置下游时应返回 errFeatureStoreNotConfigured，实际 %v", name, err)
		}
	}
}

// TestFsPurgeLimitOutOfRangeIsNotSilentlyTruncated limit 表单是 int64、rpc 字段是 int32：
// 越界的数截断后可能落到 0/负数，而 0 恰好是「清到服务端上限」的哨兵——
// 一次打字失误就会变成「清一批上限」，所以这里必须拒而不是夹。
func TestFsPurgeLimitOutOfRangeIsNotSilentlyTruncated(t *testing.T) {
	if err := fsInt32Range("limit", math.MaxInt32); err != nil {
		t.Fatalf("int32 上界本身是合法值：%v", err)
	}
	if err := fsInt32Range("limit", math.MaxInt32+1); err == nil {
		t.Fatal("越界值未被拒绝，会被静默截断")
	}
}
