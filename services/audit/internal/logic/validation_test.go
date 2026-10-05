package logic

// 入参校验与脱敏底线。
//
// 审计写入是「一次性的证据」：写坏了不能改、写漏了不能补，所以校验必须发生在
// 碰库之前，而拒绝原因必须是具体哨兵错误（调用方按错误码决定是重试还是改入参）。
// 本文件钉住三件事：
//  1. 必填与长度上限逐字段生效，上限取的是 model 里的列宽常量（改列不改校验会红）；
//  2. 明文敏感信息（口令/token/手机号/邮箱/证件号/来源 IP/设备号）既进不了库，
//     也进不了投影回参——库里本来就没有可放明文的列；
//  3. 校验失败时数据面一次都没被调用（没有半截写入，也没有「空结果 + nil 错误」）。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go-video/services/audit/internal/repository"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// saltOf 取夹具里生效的哈希盐，避免测试自己再抄一份字面量。
func (f *fixture) saltOf() string { return string(f.opts.IpHashSalt) }

// stringFields 返回结构体所有导出字符串字段（用于「原文不得出现在任何落库/回参字段」断言）。
func stringFields(v any) map[string]string {
	out := map[string]string{}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return out
		}
		rv = rv.Elem()
	}
	tp := rv.Type()
	for i := 0; i < tp.NumField(); i++ {
		fl := tp.Field(i)
		if !fl.IsExported() || fl.Type.Kind() != reflect.String {
			continue
		}
		out[fl.Name] = rv.Field(i).String()
	}
	return out
}

func assertNoRaw(t *testing.T, label string, fields map[string]string, raw ...string) {
	t.Helper()
	for name, val := range fields {
		for _, secret := range raw {
			if secret == "" || val == "" {
				continue
			}
			if strings.Contains(val, secret) {
				t.Fatalf("%s.%s 含明文敏感原文 %q（值 %q）", label, name, secret, val)
			}
		}
	}
}

// --- CallContext ---

func TestValidateCallContextTable(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	base := func() *rpc.CallContext {
		return &rpc.CallContext{CallerService: "ops-config", OperatorId: 7, TraceId: "t-1", RequestId: "r-1"}
	}
	cases := []struct {
		name     string
		cc       *rpc.CallContext
		write    bool
		wantErr  error
		wantOKOn bool
	}{
		{name: "空请求", cc: nil, write: true, wantErr: model.ErrRequestRequired},
		{name: "缺 caller_service", cc: &rpc.CallContext{RequestId: "r"}, write: true, wantErr: model.ErrCallerRequired},
		{name: "caller 全空格", cc: &rpc.CallContext{CallerService: "   "}, write: true, wantErr: model.ErrCallerRequired},
		{name: "写接口缺 request_id", cc: func() *rpc.CallContext {
			c := base()
			c.RequestId = ""
			return c
		}(), write: true, wantErr: model.ErrRequestIDRequired},
		{name: "读接口允许无 request_id", cc: func() *rpc.CallContext {
			c := base()
			c.RequestId = ""
			return c
		}(), wantOKOn: true},
		{name: "caller 超列宽", cc: &rpc.CallContext{CallerService: long(model.MaxCallerNameBytes + 1), RequestId: "r"},
			write: true, wantErr: model.ErrFieldTooLong},
		{name: "trace_id 超列宽", cc: &rpc.CallContext{CallerService: "x", TraceId: long(model.MaxTraceIDBytes + 1)},
			wantErr: model.ErrFieldTooLong},
		{name: "request_id 超列宽", cc: &rpc.CallContext{CallerService: "x", RequestId: long(model.MaxRequestIDBytes + 1)},
			wantErr: model.ErrFieldTooLong},
		{name: "operator_id 为负", cc: &rpc.CallContext{CallerService: "x", OperatorId: -1}, wantErr: model.ErrActorIDInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCallContext(tc.cc, tc.write)
			if tc.wantOKOn {
				if err != nil {
					t.Fatalf("应通过，实得 %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
		})
	}
}

// 边界：刚好等于列宽必须放过。放过/拒绝的分界若差一，写入会在 1406 才炸，
// 而那时链头已推进、条目却没落库。
func TestCallContextAcceptsExactColumnWidths(t *testing.T) {
	cc := &rpc.CallContext{
		CallerService: strings.Repeat("c", model.MaxCallerNameBytes),
		TraceId:       strings.Repeat("t", model.MaxTraceIDBytes),
		RequestId:     strings.Repeat("r", model.MaxRequestIDBytes),
	}
	if err := validateCallContext(cc, true); err != nil {
		t.Fatalf("按列宽上限给值应放过：%v", err)
	}
}

// AppendAudit 的 CallContext 校验必须在取数据面之前完成。
func TestAppendAuditRejectsBeforeTouchingData(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		in      *rpc.AppendAuditReq
		wantErr error
	}{
		{"空请求", nil, model.ErrRequestRequired},
		{"无 ctx", &rpc.AppendAuditReq{Entry: validDraft("ev-1")}, model.ErrRequestRequired},
		{"无 entry", &rpc.AppendAuditReq{Ctx: testCallContext("req-1")}, model.ErrDraftRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewAppendAuditLogic(ctx, svcCtx())
			got, err := l.AppendAudit(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if got != nil {
				t.Fatalf("拒绝时不得回响应对象：%+v", got)
			}
		})
	}
	if len(f.entries.inserted) != 0 || f.countAll() != 0 || len(f.chains.ensured) != 0 {
		t.Fatalf("非法请求不得碰数据面：inserted=%v rows=%d ensured=%v",
			f.entries.inserted, f.countAll(), f.chains.ensured)
	}
}

// --- 条目装配 ---

func TestPrepareEntryRejectsEveryInvalidField(t *testing.T) {
	f := newFixture(t)
	d := f.deps
	cc := testCallContext("req-1")
	long := func(n int) string { return strings.Repeat("x", n) }

	cases := []struct {
		name    string
		mutate  func(*rpc.AuditEntryDraft)
		wantErr error
	}{
		{"空 event_id", func(e *rpc.AuditEntryDraft) { e.EventId = "   " }, model.ErrEventIDRequired},
		{"event_id 超列宽", func(e *rpc.AuditEntryDraft) {
			e.EventId = long(model.MaxEventIDBytes + 1)
		}, model.ErrFieldTooLong},
		{"schema_version 非当前版本", func(e *rpc.AuditEntryDraft) { e.SchemaVersion = 2 }, model.ErrSchemaVersionUnsupported},
		{"actor_type 未指定", func(e *rpc.AuditEntryDraft) { e.ActorType = rpc.ActorType_ACTOR_TYPE_UNSPECIFIED },
			model.ErrActorUnspecified},
		{"actor_type 契约外取值", func(e *rpc.AuditEntryDraft) { e.ActorType = rpc.ActorType(99) },
			model.ErrActorUnspecified},
		{"admin 无 actor_id", func(e *rpc.AuditEntryDraft) { e.ActorId = 0 }, model.ErrActorUnspecified},
		{"actor_id 为负", func(e *rpc.AuditEntryDraft) { e.ActorId = -1 }, model.ErrActorIDInvalid},
		{"action 无点号", func(e *rpc.AuditEntryDraft) { e.Action = "approve" }, model.ErrActionInvalid},
		{"action 为空", func(e *rpc.AuditEntryDraft) { e.Action = "" }, model.ErrActionInvalid},
		// action 的字符集正则本身就把长度限在 64，因此超长表现为格式非法而不是列宽超限：
		// 这条断言把「两条校验哪条先命中」钉死，避免有人以为还有第二道长度防线。
		{"action 超长", func(e *rpc.AuditEntryDraft) { e.Action = "a." + long(model.MaxActionBytes) },
			model.ErrActionInvalid},
		{"action_domain 大写", func(e *rpc.AuditEntryDraft) { e.ActionDomain = "MEDIA" }, model.ErrActionDomainInvalid},
		{"action_domain 含点号", func(e *rpc.AuditEntryDraft) {
			e.ActionDomain = "media.approve"
			e.Action = "media.approve"
		}, model.ErrActionDomainInvalid},
		{"action_domain 超长", func(e *rpc.AuditEntryDraft) {
			e.ActionDomain = long(model.MaxActionDomainBytes + 1)
		}, model.ErrActionDomainInvalid},
		{"action_domain 为空", func(e *rpc.AuditEntryDraft) { e.ActionDomain = "" },
			model.ErrActionDomainInvalid},
		{"result 未指定", func(e *rpc.AuditEntryDraft) { e.Result = rpc.AuditResult_AUDIT_RESULT_UNSPECIFIED },
			model.ErrResultRequired},
		{"result 契约外取值", func(e *rpc.AuditEntryDraft) { e.Result = rpc.AuditResult(9) }, model.ErrResultRequired},
		{"source_app 未指定", func(e *rpc.AuditEntryDraft) { e.SourceApp = rpc.SourceApp_SOURCE_APP_UNSPECIFIED },
			model.ErrSourceAppRequired},
		{"只有 target_id", func(e *rpc.AuditEntryDraft) {
			e.TargetType = ""
			e.TargetId = "work-1"
		}, model.ErrActionRequired},
		{"target_type 超长", func(e *rpc.AuditEntryDraft) {
			e.TargetType = long(model.MaxTargetTypeBytes + 1)
		}, model.ErrFieldTooLong},
		{"target_id 超长", func(e *rpc.AuditEntryDraft) { e.TargetId = long(model.MaxTargetIDBytes + 1) },
			model.ErrFieldTooLong},
		{"摘要格式非白名单", func(e *rpc.AuditEntryDraft) { e.BeforeDigest = "state=1234" }, model.ErrDigestInvalid},
		{"摘要放原值", func(e *rpc.AuditEntryDraft) { e.AfterDigest = "state=approved" }, model.ErrDigestInvalid},
		{"摘要超列宽", func(e *rpc.AuditEntryDraft) {
			e.AfterDigest = strings.Repeat("a", 64) + ";" + strings.Repeat("b", 64) + ";" +
				strings.Repeat("c", 64) + ";" + strings.Repeat("d", 64)
		}, model.ErrFieldTooLong},
		{"reason 超配置上限", func(e *rpc.AuditEntryDraft) { e.Reason = long(501) }, model.ErrFieldTooLong},
		{"reason 含手机号", func(e *rpc.AuditEntryDraft) { e.Reason = "联系 13800138000 复核" }, model.ErrDigestLooksPII},
		{"reason 含口令", func(e *rpc.AuditEntryDraft) { e.Reason = "重置 password=P@ssw0rd" }, model.ErrDigestLooksPII},
		{"reason 含邮箱", func(e *rpc.AuditEntryDraft) { e.Reason = "发到 ops@example.com" }, model.ErrDigestLooksPII},
		{"reason 含证件号", func(e *rpc.AuditEntryDraft) { e.Reason = "证件 110101199003072316" }, model.ErrDigestLooksPII},
		{"actor_name 含 token", func(e *rpc.AuditEntryDraft) { e.ActorName = "bot token:abcdef" },
			model.ErrDigestLooksPII},
		{"occurred_at 超前容忍窗口", func(e *rpc.AuditEntryDraft) {
			e.OccurredAt = testNow + model.MaxFutureOccurredSkewSeconds + 1
		}, model.ErrOccurredAtFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			draft := validDraft("ev-" + tc.name)
			tc.mutate(draft)
			_, err := d.prepareEntry(cc, draft)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
		})
	}
}

func TestPrepareEntryAcceptsLegalShapes(t *testing.T) {
	f := newFixture(t)
	d := f.deps
	cc := testCallContext("req-ok")

	// 摘要三种白名单形态都要放过：空、整对象 64hex、字段=16hex 列表。
	for i, dg := range []string{"", strings.Repeat("a", 64), "state=0a1b2c3d4e5f6071;reason=1f2e3d4c5b6a7988"} {
		draft := validDraft(fmt.Sprintf("ev-ok-%d", i))
		draft.BeforeDigest = dg
		draft.AfterDigest = dg
		row, err := d.prepareEntry(cc, draft)
		if err != nil {
			t.Fatalf("第 %d 种摘要 %q 应放过：%v", i, dg, err)
		}
		if row.SchemaVersion != model.SchemaVersion || row.ChainKey != model.ChainKey("media", testNow) {
			t.Fatalf("装配结果异常：%+v", row)
		}
	}
	// actor_type=SYSTEM/UNKNOWN 允许 actor_id=0（定时任务与登录失败没有主体 ID）。
	for _, at := range []rpc.ActorType{rpc.ActorType_ACTOR_TYPE_SYSTEM, rpc.ActorType_ACTOR_TYPE_UNKNOWN} {
		draft := validDraft("ev-actor-" + at.String())
		draft.ActorType = at
		draft.ActorId = 0
		if _, err := d.prepareEntry(cc, draft); err != nil {
			t.Fatalf("%v 允许 actor_id=0：%v", at, err)
		}
	}
	// target_type 单独给出是合法维度（按类型排查）。
	draft := validDraft("ev-target-only-type")
	draft.TargetType = "video:submission"
	draft.TargetId = ""
	if _, err := d.prepareEntry(cc, draft); err != nil {
		t.Fatalf("只有 target_type 应放过：%v", err)
	}
	// occurred_at=0 由服务端按「现在」补齐，不能落成 0 让条目掉出所有时间窗口。
	draft = validDraft("ev-no-occurred")
	draft.OccurredAt = 0
	row, err := d.prepareEntry(cc, draft)
	if err != nil {
		t.Fatalf("缺 occurred_at 应补默认值：%v", err)
	}
	if row.OccurredAt != testNow || row.Ctime != testNow {
		t.Fatalf("默认时间应取注入时钟：occurred_at=%d ctime=%d", row.OccurredAt, row.Ctime)
	}
	// 超前但在容忍窗口内要放过（时钟偏差是常态）。
	draft = validDraft("ev-skew")
	draft.OccurredAt = testNow + model.MaxFutureOccurredSkewSeconds
	if _, err := d.prepareEntry(cc, draft); err != nil {
		t.Fatalf("容忍边界应放过：%v", err)
	}
}

func TestPrepareEntryTruncatesActorNameButNeverReason(t *testing.T) {
	f := newFixture(t)
	d := f.deps
	// actor_name 不参与哈希（改名不追改历史条目），允许按 rune 截断；
	// reason 参与哈希，截断会让「库里的文本」与「算摘要用的文本」不一致，只能拒写。
	draft := validDraft("ev-long-name")
	draft.ActorName = strings.Repeat("名", model.MaxActorNameBytes+20)
	row, err := d.prepareEntry(testCallContext("req-2"), draft)
	if err != nil {
		t.Fatalf("actor_name 超长应截断：%v", err)
	}
	if len([]rune(row.ActorName)) != model.MaxActorNameBytes {
		t.Fatalf("actor_name 未按 rune 截断：%d", len([]rune(row.ActorName)))
	}
	if !utf8Valid(row.ActorName) {
		t.Fatal("截断产生了非法 UTF-8")
	}
	draft = validDraft("ev-long-reason")
	draft.Reason = strings.Repeat("由", 501)
	if _, err := d.prepareEntry(testCallContext("req-2"), draft); !errors.Is(err, model.ErrFieldTooLong) {
		t.Fatalf("reason 超长必须拒写而不是截断：%v", err)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// --- limits 归一 ---

func TestLimitHelpersConvergeBrokenConfig(t *testing.T) {
	d := deps{}
	d.opts = repository.Options{}
	if got := d.reasonLimit(); got != model.MaxReasonBytes {
		t.Fatalf("MaxReasonLen=0 应收敛到列宽 %d，实得 %d", model.MaxReasonBytes, got)
	}
	d.opts.MaxReasonLen = 999999
	if got := d.reasonLimit(); got != model.MaxReasonBytes {
		t.Fatalf("配置超过列宽必须夹回列宽，实得 %d", got)
	}
	d.opts.MaxReasonLen = 100
	if got := d.reasonLimit(); got != 100 {
		t.Fatalf("配置小于列宽时应生效，实得 %d", got)
	}

	d.opts = repository.Options{ExportBatchRows: 0}
	if got := d.scanLimit(0); got < 1 {
		t.Fatalf("scanLimit 不得给出 0 批次：%d", got)
	}
	d.opts.ExportBatchRows = 1 << 20
	if got := d.scanLimit(0); got != model.MaxScanBatchRows {
		t.Fatalf("单批行数必须夹到 model 硬上限 %d，实得 %d", model.MaxScanBatchRows, got)
	}
	if got := d.scanLimit(10); got != 10 {
		t.Fatalf("调用方要求的更小批次应生效，实得 %d", got)
	}
	// verify/archive 的上限同样必须兜到 >=1：给 0 等于「一次读 0 行」的假成功。
	for _, tc := range []struct {
		name string
		got  int32
		want int32
	}{
		{"verifyLimit 生效配置", deps{opts: repository.Options{MaxVerifyEntries: 500}}.verifyLimit(0), 500},
		{"verifyLimit 缺省", deps{opts: repository.Options{MaxVerifyEntries: 0}}.verifyLimit(0), 1},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s = %d，期望 %d", tc.name, tc.got, tc.want)
		}
	}
	if got := (deps{opts: repository.Options{ArchiveMaxEntries: -5}}).archiveLimit(); got != 1 {
		t.Fatalf("archiveLimit 缺省兜底应为 1，实得 %d", got)
	}
}

// --- 来源标识：只存加盐短哈希 ---

func TestPlainTextSourceIdentifiersNeverPersisted(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	const rawIP, rawDev = "203.0.113.9", "harmony-device-8f21c"
	cc := testCallContext("req-src")
	cc.Ip = rawIP
	draft := validDraft("ev-src")
	draft.DeviceId = rawDev
	// draft.ip 也在契约里：调用方把原文放在这里时同样不允许落到任何列。
	draft.Ip = "198.51.100.7"

	l := NewAppendAuditLogic(context.Background(), svcCtx())
	reply, err := l.AppendAudit(&rpc.AppendAuditReq{Ctx: cc, Entry: draft})
	if err != nil {
		t.Fatalf("合法写入失败：%v", err)
	}
	row := f.entries.byEvent["ev-src"]
	if row == nil {
		t.Fatal("条目未落库")
	}
	wantIP := model.ShortHash(f.saltOf(), rawIP)
	wantDev := model.ShortHash(f.saltOf(), rawDev)
	if row.IPHash != wantIP || len(row.IPHash) != model.ShortHashLen {
		t.Fatalf("ip_hash 应为加盐短哈希：%q", row.IPHash)
	}
	if row.DeviceHash != wantDev {
		t.Fatalf("device_hash 应为加盐短哈希：%q", row.DeviceHash)
	}
	if row.IPHash == wantDev {
		t.Fatal("IP 与设备标识必须散列成不同值")
	}
	secrets := []string{rawIP, rawDev, draft.Ip}
	assertNoRaw(t, "落库行", stringFields(row), secrets...)
	assertNoRaw(t, "回参条目", stringFields(reply.GetEntry()), secrets...)
}

// 库里不存在放明文的列、投影里也不存在：新增字段时这条会挡住「顺手把 IP 带出去」。
func TestNoPlaintextSourceColumnsOrViewFieldsExist(t *testing.T) {
	banned := []string{"ip", "useragent", "sourceip", "deviceid", "password", "token", "idcard", "phone"}
	for _, typ := range []any{model.AuditEntry{}, rpc.AuditEntryView{}} {
		tp := reflect.TypeOf(typ)
		for i := 0; i < tp.NumField(); i++ {
			name := strings.ToLower(tp.Field(i).Name)
			for _, b := range banned {
				if name == b {
					t.Fatalf("%T 出现了明文敏感列 %s：该表只能存加盐哈希", typ, tp.Field(i).Name)
				}
			}
		}
	}
}

func TestHashSaltMissingRefusesEveryWrite(t *testing.T) {
	for _, withIP := range []bool{true, false} {
		t.Run(fmt.Sprintf("带IP=%v", withIP), func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			f.withOpts(func(o *repository.Options) { o.IpHashSalt = nil })
			cc := testCallContext("req-nosalt")
			if withIP {
				cc.Ip = "203.0.113.9"
			}
			_, err := NewAppendAuditLogic(context.Background(), svcCtx()).
				AppendAudit(&rpc.AppendAuditReq{Ctx: cc, Entry: validDraft("ev-nosalt")})
			// 缺盐时宁可拒写：退化成裸 SHA-256(IP) 等于把弱哈希当不可逆摘要。
			if !errors.Is(err, model.ErrHashSaltMissing) {
				t.Fatalf("err = %v，期望 ErrHashSaltMissing", err)
			}
			if f.countAll() != 0 || len(f.chains.ensured) != 0 {
				t.Fatalf("拒写时不得留下任何痕迹：rows=%d ensured=%v", f.countAll(), f.chains.ensured)
			}
		})
	}
}

// --- 批量写入 ---

func TestBatchAppendRejectsWholeBatchBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()

	t.Run("空数组", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		_, err := NewBatchAppendAuditLogic(ctx, svcCtx()).
			BatchAppendAudit(&rpc.BatchAppendAuditReq{Ctx: testCallContext("req-b0")})
		if !errors.Is(err, model.ErrBatchEmpty) {
			t.Fatalf("err = %v，期望 ErrBatchEmpty（空数组多半是上游 bug）", err)
		}
		if f.countAll() != 0 || len(f.chains.ensured) != 0 {
			t.Fatalf("空批次不得碰数据面：rows=%d ensured=%v", f.countAll(), f.chains.ensured)
		}
	})

	t.Run("超过单次上限", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.withOpts(func(o *repository.Options) { o.MaxBatchSize = 2 })
		drafts := []*rpc.AuditEntryDraft{validDraft("b-1"), validDraft("b-2"), validDraft("b-3")}
		_, err := NewBatchAppendAuditLogic(ctx, svcCtx()).
			BatchAppendAudit(&rpc.BatchAppendAuditReq{Ctx: testCallContext("req-b1"), Entries: drafts})
		if !errors.Is(err, model.ErrBatchTooLarge) {
			t.Fatalf("err = %v，期望 ErrBatchTooLarge", err)
		}
		if len(f.entries.inserted) != 0 {
			t.Fatalf("超限批次不得写入：%v", f.entries.inserted)
		}
	})

	t.Run("一条非法整批拒绝", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		bad := validDraft("b-bad")
		bad.Result = rpc.AuditResult_AUDIT_RESULT_UNSPECIFIED
		in := &rpc.BatchAppendAuditReq{Ctx: testCallContext("req-b2"),
			Entries: []*rpc.AuditEntryDraft{validDraft("b-ok-1"), bad, validDraft("b-ok-2")}}
		_, err := NewBatchAppendAuditLogic(ctx, svcCtx()).BatchAppendAudit(in)
		if !errors.Is(err, model.ErrResultRequired) {
			t.Fatalf("err = %v，期望 ErrResultRequired", err)
		}
		// 必须点出下标，否则调用方只能对着一批同构条目逐条盲猜。
		if !strings.Contains(err.Error(), "entries[1]") {
			t.Fatalf("错误未指出坏条目下标：%v", err)
		}
		if len(f.entries.inserted) != 0 || f.countAll() != 0 {
			t.Fatalf("半批成功等于让上游以为都写入了：inserted=%v", f.entries.inserted)
		}
	})

	t.Run("批次内 event_id 自撞", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		dup := validDraft("b-dup")
		in := &rpc.BatchAppendAuditReq{Ctx: testCallContext("req-b3"),
			Entries: []*rpc.AuditEntryDraft{validDraft("b-dup"), dup}}
		_, err := NewBatchAppendAuditLogic(ctx, svcCtx()).BatchAppendAudit(in)
		if !errors.Is(err, model.ErrDuplicateEventID) {
			t.Fatalf("err = %v，期望 ErrDuplicateEventID", err)
		}
		if !strings.Contains(err.Error(), "entries[1] 与 entries[0]") {
			t.Fatalf("必须点出冲突的两条下标：%v", err)
		}
	})
}

func TestBatchAppendReportsAcceptedAndReusedSeparately(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	cc := testCallContext("req-b4")
	logic := NewBatchAppendAuditLogic(ctx, svcCtx())

	in := &rpc.BatchAppendAuditReq{Ctx: cc, Entries: []*rpc.AuditEntryDraft{validDraft("bb-1"), validDraft("bb-2")}}
	reply, err := logic.BatchAppendAudit(in)
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetAccepted() != 2 || reply.GetReused() != 0 {
		t.Fatalf("首次提交应 accepted=2 reused=0，实得 %+v", reply)
	}
	if len(reply.GetEntries()) != 2 || reply.GetEntries()[0].GetEventId() != "bb-1" {
		t.Fatalf("回参必须与入参同序：%+v", reply.GetEntries())
	}

	// 重放：一条已存在、一条新写。
	reply2, err := logic.BatchAppendAudit(&rpc.BatchAppendAuditReq{Ctx: cc,
		Entries: []*rpc.AuditEntryDraft{validDraft("bb-1"), validDraft("bb-3")}})
	if err != nil {
		t.Fatal(err)
	}
	if reply2.GetAccepted() != 1 || reply2.GetReused() != 1 {
		t.Fatalf("accepted=%d reused=%d，期望 1/1", reply2.GetAccepted(), reply2.GetReused())
	}
	if got := reply2.GetEntries()[0].GetEntryId(); got != reply.GetEntries()[0].GetEntryId() {
		t.Fatalf("reused 条目必须原样回既有行：%d != %d", got, reply.GetEntries()[0].GetEntryId())
	}
	if f.countAll() != 3 {
		t.Fatalf("行数=%d，期望 3", f.countAll())
	}
}
