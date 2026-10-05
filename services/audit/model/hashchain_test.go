package model

import (
	"strings"
	"testing"
)

// entry 造一条参与哈希字段齐全的条目，供哈希测试复用。
func entry(chainKey string, seq int64, prev string) *AuditEntry {
	return &AuditEntry{
		EventID:       "ops-config.req-1:publish",
		SchemaVersion: SchemaVersion,
		ChainKey:      chainKey,
		Seq:           seq,
		ActorType:     ActorAdmin,
		ActorID:       7,
		ActorName:     "content_ops",
		Action:        "ops_config.publish",
		ActionDomain:  "ops_config",
		TargetType:    "ops_config:release",
		TargetID:      "42@global?v=3",
		Result:        ResultOK,
		BeforeDigest:  "value=0a1b2c3d4e5f6071",
		AfterDigest:   "value=1f2e3d4c5b6a7988",
		Reason:        "首页改版灰度放量",
		SourceApp:     SourceAdminWeb,
		IPHash:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeviceHash:    "",
		TraceID:       "trace-abc",
		OccurredAt:    1777000000,
		PrevHash:      prev,
	}
}

func TestChainKeyUsesUTCDate(t *testing.T) {
	// 同一个时刻换成本地时区就会跨日，链名必须与部署机时区无关。
	got := ChainKey("ops_config", 1777000000)
	if !strings.HasPrefix(got, "ops_config/") {
		t.Fatalf("ChainKey 前缀应为动作域: %q", got)
	}
	if !strings.HasSuffix(got, "/"+DateKey(1777000000)) {
		t.Fatalf("ChainKey 后缀应为 UTC 自然日: %q", got)
	}
	if strings.ContainsAny(got, " ") {
		t.Errorf("ChainKey 不能含空格: %q", got)
	}
}

func TestGenesisHashIsChainScoped(t *testing.T) {
	a := GenesisHash("ops_config/2026-09-20")
	b := GenesisHash("admin/2026-09-20")
	if a == b {
		t.Fatal("不同链的创世摘要必须不同，否则整段条目可在链间平移而仍自洽")
	}
	if len(a) != 64 || strings.ToLower(a) != a {
		t.Errorf("创世摘要必须是 64 位小写十六进制: %q", a)
	}
}

func TestComputeEntryHashIsDeterministic(t *testing.T) {
	e := entry("ops_config/2026-09-20", 1, GenesisHash("ops_config/2026-09-20"))
	first := ComputeEntryHash(e)
	second := ComputeEntryHash(entry("ops_config/2026-09-20", 1, GenesisHash("ops_config/2026-09-20")))
	if first != second {
		t.Fatal("同一输入必须得到同一摘要，否则无法重放校验")
	}
	if len(first) != 64 {
		t.Errorf("entry_hash 长度应为 64: %q", first)
	}
}

// TestEntryHashCoversEveryHashedField 逐字段扰动：任何参与哈希的字段变化都必须改变摘要。
// 这是哈希链契约的核心——漏掉一个字段就等于那个字段可以被人无声改写。
func TestEntryHashCoversEveryHashedField(t *testing.T) {
	base := entry("ops_config/2026-09-20", 5, "prev-value")
	baseHash := ComputeEntryHash(base)

	mutations := map[string]func(*AuditEntry){
		"schema_version": func(e *AuditEntry) { e.SchemaVersion = 2 },
		"chain_key":      func(e *AuditEntry) { e.ChainKey = "ops_config/2026-09-21" },
		"seq":            func(e *AuditEntry) { e.Seq = 6 },
		"event_id":       func(e *AuditEntry) { e.EventID = "other" },
		"actor_type":     func(e *AuditEntry) { e.ActorType = ActorSystem },
		"actor_id":       func(e *AuditEntry) { e.ActorID = 8 },
		"action":         func(e *AuditEntry) { e.Action = "ops_config.rollback" },
		"target_type":    func(e *AuditEntry) { e.TargetType = "ops_config:item" },
		"target_id":      func(e *AuditEntry) { e.TargetID = "43" },
		"result":         func(e *AuditEntry) { e.Result = ResultDenied },
		"before_digest":  func(e *AuditEntry) { e.BeforeDigest = "" },
		"after_digest":   func(e *AuditEntry) { e.AfterDigest = "value=0000000000000000" },
		"reason":         func(e *AuditEntry) { e.Reason = "改了原因" },
		"source_app":     func(e *AuditEntry) { e.SourceApp = SourceAndroid },
		"ip_hash":        func(e *AuditEntry) { e.IPHash = strings.Repeat("b", ShortHashLen) },
		"device_hash":    func(e *AuditEntry) { e.DeviceHash = strings.Repeat("c", ShortHashLen) },
		"trace_id":       func(e *AuditEntry) { e.TraceID = "trace-xyz" },
		"occurred_at":    func(e *AuditEntry) { e.OccurredAt = base.OccurredAt + 1 },
		"prev_hash":      func(e *AuditEntry) { e.PrevHash = "another-prev" },
	}
	for name, mutate := range mutations {
		cp := *base
		mutate(&cp)
		if ComputeEntryHash(&cp) == baseHash {
			t.Errorf("字段 %s 的变化没有被 entry_hash 覆盖", name)
		}
	}
}

// TestEntryHashExcludesNonHashedColumns 反向确认：不参与哈希的列改写后摘要不变。
// 记下这个性质是为了让「archived_at 可以被归档作业改写」成为设计而非漏洞。
// actor_name / action_domain 同样是查询用快照：前者是展示名（可能随账号改名变化），
// 后者的信息已经被 chain_key 承载，都不进哈希，否则会因无关演化而误报链断裂。
func TestEntryHashExcludesNonHashedColumns(t *testing.T) {
	base := entry("admin/2026-09-20", 2, "prev")
	want := ComputeEntryHash(base)

	cp := *base
	cp.EntryID = 9999
	cp.Ctime = nowUnix()
	cp.ArchivedAt = nowUnix()
	cp.RequestID = "req-2"
	cp.CallerService = "operation"
	cp.ActorName = "someone_else"
	cp.ActionDomain = "billing"
	if got := ComputeEntryHash(&cp); got != want {
		t.Errorf("entry_id/ctime/archived_at/request_id/caller_service/actor_name/action_domain 不应参与哈希: %s != %s", got, want)
	}
}

func TestShortHashIsSaltedAndTruncated(t *testing.T) {
	got := ShortHash("salt-a", "10.0.0.1")
	if len(got) != ShortHashLen {
		t.Fatalf("短哈希长度应为 %d, got %d", ShortHashLen, len(got))
	}
	if ShortHash("salt-b", "10.0.0.1") == got {
		t.Error("换盐必须得到不同摘要：裸 SHA-256(IP) 可被 2^32 枚举反查，加盐才是这条防线的全部意义")
	}
	if ShortHash("salt-a", "") != "" {
		t.Error("空来源应返回空串：未知来源不能用假哈希占位")
	}
}

func TestValidDigestWhitelist(t *testing.T) {
	ok := []string{
		"",
		strings.Repeat("a", 64),
		"state=0a1b2c3d4e5f6071",
		"state=0a1b2c3d4e5f6071;reason=1f2e3d4c5b6a7988",
	}
	for _, s := range ok {
		if !ValidDigest(s) {
			t.Errorf("应接受摘要 %q", s)
		}
	}
	bad := []string{
		"state=0a1b2c3d",               // 摘要太短
		"state=0A1B2C3D4E5F6071",       // 大写十六进制
		"phone=+8613800000000",         // 明文手机号
		"13800000000",                  // 裸值，无字段名
		"state=0a1b2c3d4e5f6071;",      // 尾随分号产生空 pair
		"state value=0a1b2c3d4e5f6071", // 字段名含空格
		"two_factor_enabled=false",     // 明文布尔：值必须是摘要，不能是原值
	}
	for _, s := range bad {
		if ValidDigest(s) {
			t.Errorf("应拒绝摘要 %q", s)
		}
	}
}

func TestLooksLikePII(t *testing.T) {
	detected := []string{
		"联系方式 13800138000",
		"user@example.com",
		"身份证 110101199003072316",
		"password= hunter2",
		"token: abcdef",
	}
	for _, s := range detected {
		if !LooksLikePII(s) {
			t.Errorf("应识别为疑似明文敏感信息: %q", s)
		}
	}
	clean := []string{
		"首页改版灰度放量",
		"版权到期下架",
		"",
		"1101011990", // 短数字（如版本号片段）不应误伤
	}
	for _, s := range clean {
		if LooksLikePII(s) {
			t.Errorf("不应误判为敏感信息: %q", s)
		}
	}
}

func TestExportStateTransition(t *testing.T) {
	if !CanExportTransition(ExportStatePending, ExportStateRunning) {
		t.Error("pending → running 必须合法")
	}
	if CanExportTransition(ExportStatePending, ExportStateSucceeded) {
		t.Error("pending 不能跳过 running 直接成功")
	}
	if CanExportTransition(ExportStateFailed, ExportStateRunning) {
		t.Error("failed 是终态，不能有出边")
	}
	if !CanExportTransition(ExportStateSucceeded, ExportStateExpired) {
		t.Error("对象到期后 succeeded 必须能流转到 expired")
	}
	if IsExportFinalState(ExportStateSucceeded) {
		t.Error("succeeded 不是终态：还需要过期收敛")
	}
}

func TestBatchStateTransition(t *testing.T) {
	if !CanBatchTransition(BatchStateVerified, BatchStatePurged) {
		t.Error("verified → purged 必须合法")
	}
	if CanBatchTransition(BatchStateWriting, BatchStatePurged) {
		t.Error("writing 不能跳过 verified 直接清热表——跳过清单校验等于销毁证据")
	}
	if CanBatchTransition(BatchStatePurged, BatchStateFailed) {
		t.Error("purged 是终态")
	}
}

func TestCanPolicyDays(t *testing.T) {
	cases := []struct {
		hot, archive, delete int32
		want                 bool
	}{
		{365, 90, 0, true},    // 永久保留
		{365, 90, 1095, true}, // 3 年后允许清理
		{90, 90, 90, true},    // 边界：三个窗口重合
		{30, 90, 0, false},    // hot < archive：还没归档就要出热表
		{365, 0, 0, false},    // archive 必须 > 0
		{365, 90, 30, false},  // delete < archive：未归档先删
		{365, 90, -1, false},  // 负数
	}
	for _, c := range cases {
		if got := CanPolicyDays(c.hot, c.archive, c.delete); got != c.want {
			t.Errorf("CanPolicyDays(%d,%d,%d) = %v, want %v", c.hot, c.archive, c.delete, got, c.want)
		}
	}
}
