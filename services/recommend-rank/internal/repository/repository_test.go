package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/services/recommend-rank/model"
)

// TestStubDownstreamNeverFakesSuccess 是本服务「禁止伪成功」红线的可执行证明：
// 四条下游（特征 / 行为 / 安全 / 运营参数）在未接线时必须显式失败，
// 一旦有人把它们改成「返回空集合 + nil error」，线上就会出现
// 「所有候选特征缺失但排序看起来正常」的静默错乱，这个测试当场就红。
func TestStubDownstreamNeverFakesSuccess(t *testing.T) {
	ctx := context.Background()
	d := NewStubDownstream()

	if _, err := d.Features.ItemFeatures(ctx, "home.feed", "fc_v1", []int64{1, 2}); !errors.Is(err, model.ErrNotImplemented) {
		t.Errorf("ItemFeatures 应返回 ErrNotImplemented, got %v", err)
	}
	if _, err := d.Features.UserFeatures(ctx, 10086, "fc_v1"); !errors.Is(err, model.ErrNotImplemented) {
		t.Errorf("UserFeatures 应返回 ErrNotImplemented, got %v", err)
	}
	if _, err := d.Behaviors.ContentQuality(ctx, []int64{1}); !errors.Is(err, model.ErrNotImplemented) {
		t.Errorf("ContentQuality 应返回 ErrNotImplemented, got %v", err)
	}
	if _, err := d.Behaviors.UserInterests(ctx, 10086); !errors.Is(err, model.ErrNotImplemented) {
		t.Errorf("UserInterests 应返回 ErrNotImplemented, got %v", err)
	}
	if _, err := d.Safety.VisibleAids(ctx, []int64{1}); !errors.Is(err, model.ErrNotImplemented) {
		t.Errorf("VisibleAids 应返回 ErrNotImplemented（读不到结论不能默认放行）, got %v", err)
	}
	if _, err := d.OpsConfigs.Resolve(ctx, "home.feed"); !errors.Is(err, model.ErrNotImplemented) {
		t.Errorf("Resolve 应返回 ErrNotImplemented, got %v", err)
	}
}

// TestInterventionHasNoResultTamperingFields 钉住运营干预的结构边界：
// 只允许参数（开关/频控窗口/打散间隔/配置代次），不允许任何 aid 级字段。
// AGENTS.md §7 禁止「手工修改推荐结果」，新增字段前必须先改契约评审。
func TestInterventionHasNoResultTamperingFields(t *testing.T) {
	banned := []string{"aid", "top", "boost", "weight", "ad", "pay", "coin", "vip", "member", "revenue", "order"}
	typ := reflect.TypeOf(Intervention{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, bad := range banned {
			if strings.Contains(name, bad) {
				t.Errorf("Intervention 字段 %s 命中禁用词 %q：运营只能改参数，不能改结果，也不允许商业化字段",
					typ.Field(i).Name, bad)
			}
		}
	}
	if typ.NumField() == 0 {
		t.Fatal("Intervention 不应为空结构，否则配置代次无从记录")
	}
}

// TestCacheKeysAreNamespaced 检查缓存 key 口径：
// 含服务前缀与版本段，且不同 model_key / 不同哈希盐不会撞 key。
func TestCacheKeysAreNamespaced(t *testing.T) {
	cases := map[string]string{
		"rtcfg":    RuntimeConfigKey("home_feed_multi_gate"),
		"active":   ActiveModelKey("home_feed_multi_gate"),
		"decision": DecisionPointerKey("req-1"),
		"assign":   AssignmentKey("exp", model.SubjectMid, "10086", "seed-a"),
		"assign2":  AssignmentKey("exp", model.SubjectMid, "10086", "seed-b"),
		"runexp":   RunningExperimentsKey(1_700_000_000),
		"runexp2":  RunningExperimentsKey(1_700_000_060),
	}
	for name, key := range cases {
		if !strings.HasPrefix(key, "rk:v1:") {
			t.Errorf("%s key 缺少服务/版本前缀: %s", name, key)
		}
		if strings.Contains(key, " ") || key == "" {
			t.Errorf("%s key 非法: %q", name, key)
		}
	}
	if cases["assign"] == cases["assign2"] {
		t.Error("换哈希盐后必须产生不同 key，否则旧分组会被回灌")
	}
	if cases["runexp"] == cases["runexp2"] {
		t.Error("RUNNING 变体快照按分钟分片，否则新增实验要依赖人工清 key")
	}
	if cases["rtcfg"] == cases["active"] {
		t.Error("运行时配置与 ACTIVE 快照不能共用一个 key（失效范围不同）")
	}
}

// TestDisabledCacheIsHarmless 未配置 Redis（rds=nil）时缓存层必须整体降级为「未命中」，
// 既不 panic 也不谎报命中 —— 本服务的单测与回放环境正是依赖这条才能不连真实 Redis。
func TestDisabledCacheIsHarmless(t *testing.T) {
	c := NewCache(nil)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Errorf("关闭缓存不应报故障: %v", err)
	}
	var dst map[string]int64
	if c.GetSnapshot(ctx, RuntimeConfigKey("m"), &dst) {
		t.Error("关闭缓存后 GetSnapshot 必须返回未命中")
	}
	c.SetSnapshot(ctx, RuntimeConfigKey("m"), map[string]int64{"a": 1}, 60) // 不应 panic
	if c.DecisionPointer(ctx, "req-1") != "" {
		t.Error("关闭缓存后不应伪造回放指针")
	}
	c.SetDecisionPointer(ctx, "req-1", "d-1", 60) // 不应 panic
	c.DelKeys(ctx, ActiveModelKey("m"))           // 不应 panic
	c.InvalidateModelConfig(ctx, "m")             // 不应 panic
}

// TestOptionsDefaultsAreUsedByRepository 确认仓库层的上限来自注入的 Options，
// 而不是散落在方法里的字面量（ListAssignments 会按 AssignmentScanLimit 截断）。
func TestOptionsDefaultsAreUsedByRepository(t *testing.T) {
	repo := &Repository{opt: Options{AssignmentScanLimit: 50, BucketCount: model.DefaultBucketCount}}
	if got := repo.Options().AssignmentScanLimit; got != 50 {
		t.Errorf("Options 未透传: %d", got)
	}
	if repo.BucketCount() != model.DefaultBucketCount {
		t.Errorf("BucketCount = %d", repo.BucketCount())
	}
	if got := repo.ResolveModelKey(""); got != "" {
		t.Errorf("未配置默认模型名时不应凭空造一个: %q", got)
	}
	if got := repo.ResolveModelKey("explicit"); got != "explicit" {
		t.Errorf("显式 model_key 应原样返回: %q", got)
	}
}

// TestArchiveGuardsWithoutDB 只验证不访问数据库的前置校验分支
// （保留期为 0 表示「不清理」，超过批量上限直接拒绝），这些判断必须在触库之前完成。
func TestArchiveGuardsWithoutDB(t *testing.T) {
	noRetention := &Repository{opt: Options{DecisionRetentionDays: 0, ArchiveBatchSize: 10}}
	ids, err := noRetention.PurgeExpiredDecisionLogs(context.Background())
	if err != nil || ids != nil {
		t.Errorf("保留期为 0 应完全跳过清理: ids=%v err=%v", ids, err)
	}
	if _, err := noRetention.PurgeExpiredAssignments(context.Background()); err != nil {
		t.Errorf("分桶保留期为 0 应跳过: %v", err)
	}
	if _, err := noRetention.DeleteArchivedDecisionLogs(context.Background(), make([]int64, 11)); err == nil {
		t.Error("超过归档批量上限必须拒绝，避免一次大事务锁住决策摘要表")
	}
	if n, err := noRetention.DeleteArchivedDecisionLogs(context.Background(), nil); err != nil || n != 0 {
		t.Errorf("空批次应直接返回 0: n=%d err=%v", n, err)
	}
}
