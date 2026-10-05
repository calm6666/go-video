package model

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestStreamStateNumberingPinned 锁死流状态编号。
// 1/2/3/4 同时是 live-room ReportStreamStateReq.stream_state 的取值和
// live.state.v1 事件 payload 的一部分，重排会静默破坏跨服务投影。
func TestStreamStateNumberingPinned(t *testing.T) {
	cases := map[string]struct {
		got  int32
		want int32
	}{
		"IDLE":        {StreamStateIdle, 1},
		"PUBLISHING":  {StreamStatePublishing, 2},
		"INTERRUPTED": {StreamStateInterrupted, 3},
		"STOPPED":     {StreamStateStopped, 4},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s 状态编号被改动：got=%d want=%d（会破坏 live-room 的 seq 守卫）", name, c.got, c.want)
		}
	}
}

// TestCanTransitionStreamState 全矩阵遍历：4×4 组合逐字判定，
// 顺带验证未知取值一律拒绝（新版本协议误用不会静默推进状态机）。
func TestCanTransitionStreamState(t *testing.T) {
	all := []int32{StreamStateIdle, StreamStatePublishing, StreamStateInterrupted, StreamStateStopped}
	want := map[[2]int32]bool{
		{StreamStateIdle, StreamStatePublishing}:        true,
		{StreamStateIdle, StreamStateInterrupted}:       false, // 未推过流谈不上断流
		{StreamStateIdle, StreamStateStopped}:           true,
		{StreamStatePublishing, StreamStateIdle}:        false, // 不允许退回建档
		{StreamStatePublishing, StreamStateInterrupted}: true,
		{StreamStatePublishing, StreamStateStopped}:     true,
		{StreamStateInterrupted, StreamStatePublishing}: true, // 宽限期内重连
		{StreamStateInterrupted, StreamStateIdle}:       false,
		{StreamStateInterrupted, StreamStateStopped}:    true,
		{StreamStateStopped, StreamStateIdle}:           false, // 终态无出边
		{StreamStateStopped, StreamStatePublishing}:     false,
		{StreamStateStopped, StreamStateInterrupted}:    false,
	}
	for _, from := range all {
		for _, to := range all {
			got := CanTransitionStreamState(from, to)
			if from == to {
				// 同态是幂等 no-op：不得占 seq、不得产生事件。
				if got {
					t.Errorf("同态 %d→%d 被判为合法迁移", from, to)
				}
				continue
			}
			key := [2]int32{from, to}
			exp, ok := want[key]
			if !ok {
				t.Fatalf("用例未覆盖 %d→%d", from, to)
			}
			if got != exp {
				t.Errorf("CanTransitionStreamState(%d,%d)=%v want=%v", from, to, got, exp)
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Fatalf("存在未遍历的组合：%v", want)
	}
	for _, bad := range []int32{0, 5, -1, 99} {
		if CanTransitionStreamState(bad, StreamStatePublishing) || CanTransitionStreamState(StreamStateIdle, bad) {
			t.Errorf("未知状态取值 %d 必须被拒绝", bad)
		}
	}
}

// TestTerminalAndActiveStates 校验终态与「占配额」集合互斥且覆盖完整。
func TestTerminalAndActiveStates(t *testing.T) {
	all := []int32{StreamStateIdle, StreamStatePublishing, StreamStateInterrupted, StreamStateStopped}
	active := map[int32]bool{}
	for _, s := range ActiveStreamStates() {
		if !ValidStreamState(s) {
			t.Fatalf("ActiveStreamStates 含非法取值 %d", s)
		}
		if TerminalStreamState(s) {
			t.Errorf("状态 %d 同时是活跃与终态", s)
		}
		active[s] = true
	}
	if len(active) != len(ActiveStreamStates()) {
		t.Error("ActiveStreamStates 含重复取值")
	}
	seenStopped := false
	for _, s := range all {
		if TerminalStreamState(s) == active[s] {
			t.Errorf("状态 %d 既不在活跃集合也不是终态", s)
		}
		if s == StreamStateStopped {
			seenStopped = true
			if active[s] {
				t.Error("STOPPED 不得计入非终态，否则密钥配额与节点配额永不释放")
			}
		}
	}
	if !seenStopped {
		t.Fatal("缺少 STOPPED 用例")
	}
}

// TestProtocolMaskEnumRoundTrip 枚举（1/2/3）与位图（1/2/4）不得混用：
// 正反映射必须互逆，且组合位图/未知枚举一律拒绝而不是按 RTMP 兜底。
func TestProtocolMaskEnumRoundTrip(t *testing.T) {
	for _, protocol := range []int32{1, 2, 3} {
		mask, ok := ProtocolMask(protocol)
		if !ok {
			t.Fatalf("ProtocolMask(%d) 失败", protocol)
		}
		if mask&(mask-1) != 0 {
			t.Errorf("ProtocolMask(%d)=%d 不是单个位", protocol, mask)
		}
		back, ok := ProtocolEnum(mask)
		if !ok || back != protocol {
			t.Errorf("反查不一致：ProtocolEnum(%d)=%d,%v want=%d", mask, back, ok, protocol)
		}
	}
	for _, bad := range []int32{0, 4, -1} {
		if mask, ok := ProtocolMask(bad); ok || mask != 0 {
			t.Errorf("ProtocolMask(%d) 应拒绝，got=%d,%v", bad, mask, ok)
		}
	}
	for _, combo := range []uint32{0, 3, 5, 6, 7, 8} {
		if v, ok := ProtocolEnum(combo); ok || v != 0 {
			t.Errorf("ProtocolEnum(%d) 应拒绝组合值/0，got=%d,%v", combo, v, ok)
		}
	}
	for _, protocol := range []int32{1, 2, 3, 0, 7} {
		if got := ValidProtocol(protocol); got != (protocol >= 1 && protocol <= 3) {
			t.Errorf("ValidProtocol(%d)=%v", protocol, got)
		}
	}
}

// TestAuthAllowedKeyState 只有 ACTIVE/ROTATING 可鉴权，且 ROTATING 的到期由 grace_until 决定。
func TestAuthAllowedKeyState(t *testing.T) {
	for state, want := range map[int32]bool{
		KeyStateActive:   true,
		KeyStateRotating: true,
		KeyStateRetired:  false,
		KeyStateExpired:  false,
		KeyStateRevoked:  false,
		0:                false,
	} {
		if got := AuthAllowedKeyState(state); got != want {
			t.Errorf("AuthAllowedKeyState(%d)=%v want=%v", state, got, want)
		}
	}
	if KeyStateRevoked == KeyStateRetired {
		t.Fatal("REVOKED 与 RETIRED 不得共用取值：吊销不可恢复、退役可重新签发")
	}
}

// TestOutboxStateNumberingMatchesProto outbox 取值与 rpc.OutboxState 枚举逐字对齐，0 保留给 UNSPECIFIED。
func TestOutboxStateNumberingMatchesProto(t *testing.T) {
	seen := map[int32]bool{OutboxStatePending: true, OutboxStatePublished: true, OutboxStateFailed: true}
	if len(seen) != 3 {
		t.Fatal("Outbox 状态取值重复")
	}
	for _, s := range []int32{OutboxStatePending, OutboxStatePublished, OutboxStateFailed} {
		if s <= 0 {
			t.Errorf("Outbox 状态 %d 不得为 0/负数（0 留给 UNSPECIFIED，不落库）", s)
		}
	}
	if CallbackResultPending != 0 {
		t.Error("回调判定初值必须是 0（尚未判定）")
	}
}

// TestClampLimit 每个列表查询都恒有 LIMIT：这里锁死默认值与上限夹取。
func TestClampLimit(t *testing.T) {
	cases := []struct{ limit, max, want int32 }{
		{0, 50, 10},
		{-1, 100, 20},
		{10, 0, 0}, // max 未配置时不放大成无界
		{200, 50, 50},
		{50, 50, 50},
		{49, 50, 49},
	}
	for _, c := range cases {
		if got := clampLimit(c.limit, c.max); got != c.want {
			t.Errorf("clampLimit(%d,%d)=%d want=%d", c.limit, c.max, got, c.want)
		}
	}
}

// TestTruncateRuneSafe 截断只在 rune 边界停止，避免写出非法 UTF-8 导致整行插入失败。
func TestTruncateRuneSafe(t *testing.T) {
	src := "推流密钥abc"
	for _, max := range []int{0, 1, 2, 3, 6, 7, 8, 9, 100} {
		got := truncate(src, max)
		if len(got) > max && max <= len(src) {
			t.Errorf("truncate(%q,%d)=%q 超出字节上限", src, max, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q,%d)=%q 产生了半个 UTF-8 序列", src, max, got)
		}
	}
	if got := truncate("ascii", 10); got != "ascii" {
		t.Errorf("未超长时不应改动，got=%q", got)
	}
}

// TestStatePlaceholders 非终态 IN 片段与参数个数必须一致，否则 SQL 直接报错。
func TestStatePlaceholders(t *testing.T) {
	ph, args := statePlaceholders()
	if got, want := strings.Count(ph, "?"), len(args); got != want {
		t.Fatalf("占位符与参数不匹配：%s / %v", ph, args)
	}
	if strings.HasSuffix(ph, ",") {
		t.Errorf("IN 片段结尾多余逗号：%s", ph)
	}
}

// TestIsDuplicateErr 幂等重放判定：唯一索引冲突识别为「已存在」，其他错误不得被吞掉。
func TestIsDuplicateErr(t *testing.T) {
	boom := errors.New("boom")
	cases := map[error]bool{
		nil:  false,
		boom: false,
		errors.New("Error 1062 (23000): Duplicate entry 'x' for key 'uniq_report_id'"): true,
		errors.New("Duplicate entry 'abc' for key 'PRIMARY'"):                          true,
		ErrConcurrentUpdate: false,
	}
	for err, want := range cases {
		if got := IsDuplicate(err); got != want {
			t.Errorf("IsDuplicate(%v)=%v want=%v", err, got, want)
		}
	}
	if !isNoRows(sql.ErrNoRows) {
		t.Error("isNoRows 应识别 sql.ErrNoRows")
	}
	if isNoRows(boom) {
		t.Error("isNoRows 不得把普通错误当无记录")
	}
}

// TestNullableString 空串必须写成 NULL：唯一索引允许多个 NULL，
// 否则可选幂等列（report_id 等）第二次插入就会撞唯一键。
func TestNullableString(t *testing.T) {
	if got := nullableString(""); got != nil {
		t.Errorf("nullableString(\"\")=%v want nil", got)
	}
	if got := nullableString("evt_01"); got != "evt_01" {
		t.Errorf("nullableString(\"evt_01\")=%v", got)
	}
}

// TestSentinelErrorsDistinct 哨兵错误必须各自唯一：
// logic 与调用方靠 errors.Is 区分「重放」「并发冲突」「配额不足」，不能靠文案。
func TestSentinelErrorsDistinct(t *testing.T) {
	all := []error{
		ErrNotImplemented, ErrIdempotencyKeyRequired, ErrInvalidRoomId, ErrInvalidMid, ErrOperatorRequired,
		ErrInvalidKeyId, ErrInvalidStreamId, ErrInvalidProtocol, ErrInvalidTtl, ErrStreamKeyNotFound,
		ErrStreamKeyRevoked, ErrStreamKeyExpired, ErrStreamKeyNotUsable, ErrPublishDenied,
		ErrKeyRefUnresolvable, ErrStreamQuotaExceeded, ErrStreamNotFound, ErrInvalidStreamState,
		ErrInvalidStateTransition,
		ErrConcurrentUpdate, ErrSeqConflict, ErrTerminalStream, ErrNodeNotFound, ErrNodeNotAssignable,
		ErrNoAvailableNode, ErrNodeProtocolMismatch, ErrAssignmentNotFound, ErrLeaseNotHeld,
		ErrNoFailedEvents, ErrCallbackDomainUnbound, ErrCallbackSignatureMismatch,
		ErrCallbackTimestampSkew, ErrCallbackReplayed, ErrCallbackSignerUnavailable,
		ErrCdnNotConfigured, ErrInvalidSampleMetrics, ErrPageSizeInvalid, ErrReasonTooLong,
		ErrReasonRequired, ErrCacheUnavailable,
	}
	seen := map[string]error{}
	for _, err := range all {
		if err == nil {
			t.Fatal("存在未初始化的哨兵错误")
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "live-ingest: ") {
			t.Errorf("哨兵错误文案需带服务前缀以便定位：%q", msg)
		}
		if strings.ContainsAny(msg, "0123456789") && !strings.Contains(msg, "1062") {
			t.Errorf("错误文案不得内嵌变量值（日志聚合基数爆炸）：%q", msg)
		}
		// 文案只能描述「哪类失败」，不得内嵌列名之外的密钥材料字段（明文、哈希、Vault 引用、签名原文）。
		for _, forbidden := range []string{"key_hash", "plaintext", "secret_ref", "signature_hash", "raw_params"} {
			if strings.Contains(strings.ToLower(msg), forbidden) {
				t.Errorf("错误文案不得提及密钥/签名材料：%q", msg)
			}
		}
		if prev, ok := seen[msg]; ok {
			t.Errorf("哨兵错误文案重复：%q 同时属于 %v 与 %v", msg, prev, err)
		}
		seen[msg] = err
	}
	if !errors.Is(ErrInvalidStateTransition, ErrInvalidStateTransition) {
		t.Fatal("哨兵错误必须可被 errors.Is 识别")
	}
	if errors.Is(ErrConcurrentUpdate, ErrSeqConflict) {
		t.Fatal("并发冲突与 seq 冲突必须是不同错误")
	}
}

// TestTransitionMatrixMatchesDoc 保证文档里那张迁移表与代码常量不脱节。
func TestTransitionMatrixMatchesDoc(t *testing.T) {
	fromDoc := map[int32][]int32{
		StreamStateIdle:        {StreamStatePublishing, StreamStateStopped},
		StreamStatePublishing:  {StreamStateInterrupted, StreamStateStopped},
		StreamStateInterrupted: {StreamStatePublishing, StreamStateStopped},
		StreamStateStopped:     nil,
	}
	for from, wants := range fromDoc {
		var got []int32
		for _, to := range []int32{StreamStateIdle, StreamStatePublishing, StreamStateInterrupted, StreamStateStopped} {
			if CanTransitionStreamState(from, to) {
				got = append(got, to)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(wants) {
			t.Errorf("状态 %d 的出边 got=%v want=%v（README 迁移表需同步）", from, got, wants)
		}
	}
}
