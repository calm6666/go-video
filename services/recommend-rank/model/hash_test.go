package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
)

// digest64 生成合法的 64 位 sha256 hex 摘要（测试夹具）。
func digest64(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// itoa 是十进制字符串助手（分桶主体标识用）。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestDigestAidsIsOrderSensitive(t *testing.T) {
	// 顺序敏感是「结果摘要」有意义的前提：[1,2] 与 [2,1] 必须是不同摘要。
	a := DigestAids([]int64{1, 2})
	b := DigestAids([]int64{2, 1})
	if a == b {
		t.Fatal("DigestAids 对顺序不敏感，无法证明排序结果")
	}
	if len(a) != 64 || !IsSha256Hex(a) {
		t.Fatalf("摘要必须是 64 位 sha256 hex, got %q", a)
	}
	if got := DigestAids([]int64{1, 2}); got != a {
		t.Fatal("同一序列必须得到同一摘要（幂等回放的口径）")
	}
	// 分隔符防碰撞：[12] 与 [1,2] 不能算出同一个值。
	if DigestAids([]int64{12}) == DigestAids([]int64{1, 2}) {
		t.Fatal("aid 序列缺少分隔符会产生碰撞")
	}
}

func TestJoinAndSplitAidsRoundTrip(t *testing.T) {
	caids := []int64{7, 0, -3, 11}
	joined := JoinAids(caids)
	if joined != "7,0,-3,11" {
		t.Fatalf("JoinAids = %q", joined)
	}
	got := SplitAids(joined)
	if len(got) != len(caids) {
		t.Fatalf("SplitAids 往返丢项: %v", got)
	}
	for i := range caids {
		if got[i] != caids[i] {
			t.Fatalf("SplitAids[%d]=%d want %d", i, got[i], caids[i])
		}
	}
	if SplitAids("") != nil || JoinAids(nil) != "" {
		t.Fatal("空序列必须往返成空值，不能写入 0 这个假 aid")
	}
	if got := SplitAids("9,bad,, "); len(got) != 1 || got[0] != 9 {
		t.Fatalf("脏数据应被跳过而不是中断解析: %v", got)
	}
}

func TestBucketOfIsStableAndInRange(t *testing.T) {
	const count = DefaultBucketCount
	first := BucketOf(SubjectMid, "10086", "home_feed_exp", "seed-a", count)
	if first < 0 || first >= count {
		t.Fatalf("桶号越界: %d", first)
	}
	for i := 0; i < 20; i++ {
		if got := BucketOf(SubjectMid, "10086", "home_feed_exp", "seed-a", count); got != first {
			t.Fatalf("同一主体的桶号不稳定: %d != %d", got, first)
		}
	}
	// 换盐必须整体重分桶（否则「实验换算法」无法重新开始）。
	if BucketOf(SubjectMid, "10086", "home_feed_exp", "seed-b", count) == first {
		t.Log("换盐后桶号相同属于小概率事件，不作为失败条件")
	}
	if BucketOf(SubjectMid, "10087", "home_feed_exp", "seed-a", count) == first &&
		BucketOf(SubjectMid, "10088", "home_feed_exp", "seed-a", count) == first {
		t.Fatal("不同主体的桶号高度一致，哈希可能没有参与主体标识")
	}
	// bucket_count 非法时按默认口径，而不是返回 0 或 panic。
	if got := BucketOf(SubjectDevice, digest64("d"), "exp", "seed", 0); got < 0 || got >= count {
		t.Fatalf("bucket_count=0 时应按默认 1000 归一: %d", got)
	}
	// 分布粗检：1000 个主体不应全部落进 10% 的桶区间，否则分流会严重不均。
	inRange := 0
	for i := 0; i < 1000; i++ {
		b := BucketOf(SubjectMid, itoa(int64(i)+1), "home_feed_exp", "seed-a", count)
		if b < count/10 {
			inRange++
		}
	}
	if inRange < 50 || inRange > 250 {
		t.Fatalf("分桶分布明显不均（期望约 100，实际 %d），检查哈希实现", inRange)
	}
}

func TestScaleBucket(t *testing.T) {
	if got := ScaleBucket(999, DefaultBucketCount, DefaultBucketCount); got != 999 {
		t.Errorf("同空间不应改变桶号: %d", got)
	}
	if got := ScaleBucket(999, DefaultBucketCount, 100); got != 99 {
		t.Errorf("缩小空间应落在上界内: %d", got)
	}
	if got := ScaleBucket(500, DefaultBucketCount, 0); got != 500 {
		t.Errorf("目标空间非法时应原样返回: %d", got)
	}
	if got := ScaleBucket(-1, DefaultBucketCount, 100); got != 0 {
		t.Errorf("负桶号应被夹到 0: %d", got)
	}
}

func TestNewDecisionID(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := NewDecisionID("req-1", int64(i))
		if len(id) != 32 {
			t.Fatalf("decision_id 长度应为 32: %q", id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("decision_id 重复: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestModelStateTransitions(t *testing.T) {
	allowed := [][2]int32{
		{ModelStateDraft, ModelStateReady},
		{ModelStateReady, ModelStateActive},
		{ModelStateReady, ModelStateRetired},
		{ModelStateActive, ModelStateRetired},
		{ModelStateDraft, ModelStateRetired},
	}
	for _, pair := range allowed {
		if !CanTransitionModelState(pair[0], pair[1]) {
			t.Errorf("应允许 %d -> %d", pair[0], pair[1])
		}
	}
	denied := [][2]int32{
		{ModelStateRetired, ModelStateActive}, // 禁止「复活」：回滚请用其他版本
		{ModelStateActive, ModelStateReady},
		{ModelStateActive, ModelStateActive},
		{ModelStateActive, ModelStateDraft},
		{0, ModelStateActive},
		{ModelStateReady, 99},
	}
	for _, pair := range denied {
		if CanTransitionModelState(pair[0], pair[1]) {
			t.Errorf("应禁止 %d -> %d", pair[0], pair[1])
		}
	}
}

func TestExpStateTransitions(t *testing.T) {
	if !CanTransitionExpState(ExpStateRunning, ExpStatePaused) || !CanTransitionExpState(ExpStatePaused, ExpStateRunning) {
		t.Fatal("RUNNING ⇄ PAUSED 必须双向允许（暂停不应丢失实验）")
	}
	if CanTransitionExpState(ExpStateStopped, ExpStateRunning) {
		t.Fatal("STOPPED 是终态，不允许回退")
	}
	if CanTransitionExpState(ExpStateDraft, ExpStatePaused) {
		t.Fatal("草稿不能直接暂停（未经过 RUNNING 的实验没有分流记录）")
	}
	if !ValidExpState(ExpStateStopped) || ValidExpState(0) || ValidExpState(5) {
		t.Fatal("实验状态枚举边界判定错误")
	}
}

func TestValidateSubjectIDRejectsRawDeviceID(t *testing.T) {
	if err := ValidateSubjectID(SubjectMid, "10086"); err != nil {
		t.Errorf("合法 mid 被拒: %v", err)
	}
	if err := ValidateSubjectID(SubjectMid, "0"); !errors.Is(err, ErrSubjectIDRequired) {
		t.Errorf("mid=0 应视为缺主体标识, got %v", err)
	}
	if err := ValidateSubjectID(SubjectMid, "abc"); !errors.Is(err, ErrSubjectIDRequired) {
		t.Errorf("非十进制 mid 应被拒, got %v", err)
	}
	if err := ValidateSubjectID(SubjectDevice, digest64("device")); err != nil {
		t.Errorf("合法设备摘要被拒: %v", err)
	}
	if err := ValidateSubjectID(SubjectDevice, "HUAWEI-VID-123"); !errors.Is(err, ErrRawDeviceID) {
		t.Errorf("明文设备号必须以 ErrRawDeviceID 拒绝, got %v", err)
	}
	if err := ValidateSubjectID(0, digest64("x")); !errors.Is(err, ErrInvalidSubjectType) {
		t.Errorf("未知主体类型应被拒, got %v", err)
	}
}

func TestValidBucketRange(t *testing.T) {
	ok := []struct{ s, e, c int32 }{{0, 500, 1000}, {500, 1000, 1000}, {999, 1000, 1000}}
	for _, v := range ok {
		if !ValidBucketRange(v.s, v.e, v.c) {
			t.Errorf("区间 [%d,%d)/%d 应合法", v.s, v.e, v.c)
		}
	}
	bad := []struct{ s, e, c int32 }{{0, 0, 1000}, {500, 500, 1000}, {600, 500, 1000}, {0, 1001, 1000}, {-1, 10, 1000}}
	for _, v := range bad {
		if ValidBucketRange(v.s, v.e, v.c) {
			t.Errorf("区间 [%d,%d)/%d 应非法", v.s, v.e, v.c)
		}
	}
}

func TestSupportedObjectivesExcludesMonetization(t *testing.T) {
	// AGENTS.md §7：目标集合只有内容与行为质量指标。
	// 这条断言是给未来改代码的人看的：新增任何付费/广告/会员目标都会被它挡下。
	for _, banned := range []string{"pred_ad_revenue", "pred_pay", "pred_vip", "pred_coin", "ad_slot"} {
		if ValidObjective(banned) {
			t.Errorf("目标 %s 属于商业化范畴，不得进入排序目标集合", banned)
		}
	}
	for _, want := range []string{ObjectiveClick, ObjectiveFinish, ObjectiveInteract, ObjectiveNegative} {
		if !ValidObjective(want) {
			t.Errorf("目标 %s 应受控可用", want)
		}
	}
	if len(SupportedObjectives()) != 4 {
		t.Fatalf("目标数量变化（%d）需同步 README 与 rpc 注释", len(SupportedObjectives()))
	}
}

func TestJoinFeatureKeysIsDedupedAndSorted(t *testing.T) {
	if got := JoinFeatureKeys([]string{"b", "a", " b ", "", "a"}); got != "a,b" {
		t.Fatalf("JoinFeatureKeys = %q，期望升序去重去空", got)
	}
	// 配置指纹依赖稳定顺序：同样的清单必须永远算出同一串，否则 keys_digest 会假性变化。
	if JoinFeatureKeys([]string{"x", "y"}) != JoinFeatureKeys([]string{"y", "x"}) {
		t.Fatal("特征清单顺序未归一，keys_digest 无法作为配置指纹")
	}
}

func TestValidPolicyAndFallbackEnums(t *testing.T) {
	for _, p := range []string{MissingPolicyDefault, MissingPolicyDropSource, MissingPolicyReject} {
		if !ValidMissingPolicy(p) {
			t.Errorf("缺失策略 %s 应受控", p)
		}
	}
	if ValidMissingPolicy("ignore") || ValidMissingPolicy("") {
		t.Error("未登记的缺失策略必须被拒（空串会让特征缺失时无规则可循）")
	}
	if !ValidDegradeReason(DegradeReasonNone) || ValidDegradeReason("unknown_reason") {
		t.Error("降级原因受控集合判定错误")
	}
	if !ValidFallback(FallbackRecallOrder) || ValidFallback("random_order") {
		t.Error("兜底策略受控集合判定错误")
	}
	if ControlVariant != "control" {
		t.Errorf("默认变体名变更会破坏历史实验数据的可解释性: %q", ControlVariant)
	}
}

func TestExperimentAggregateIDAndJoinWhere(t *testing.T) {
	if got := ExperimentAggregateID("exp", "v1"); got != "exp/v1" {
		t.Errorf("ExperimentAggregateID = %q", got)
	}
	if got := joinWhere(nil); got != "" {
		t.Errorf("joinWhere(nil) = %q，应为空串（不能拼出悬空 WHERE）", got)
	}
	if got := joinWhere([]string{"a = ?", "b = ?"}); got != " WHERE a = ? AND b = ?" {
		t.Errorf("joinWhere = %q", got)
	}
	if got := inPlaceholders(3); got != "?,?,?" {
		t.Errorf("inPlaceholders = %q", got)
	}
}

func TestIsDuplicateErr(t *testing.T) {
	if !isDuplicateErr(errors.New("Error 1062: Duplicate entry 'x' for key 'uniq_request'")) {
		t.Fatal("唯一键冲突必须被识别，否则幂等命中会漏成 500")
	}
	if isDuplicateErr(nil) || isDuplicateErr(errors.New("connection refused")) {
		t.Fatal("非冲突错误不得被误判为幂等命中")
	}
}
