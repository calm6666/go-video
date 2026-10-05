package logic

// 写侧任务定义单测：RegisterTask 与 UpdateTask。
//
// 这两个方法唯一的正确性来源是「同一事务里的两条写必须一起成败」与
// 「乐观锁命中不了就一行都不改」，所以本文件的断言全部围绕三件事：
//   1. 校验必须在开事务之前完成（txRuns/defs/audits 三个零值同时成立）；
//   2. 唯一键命中即幂等返回，绝不覆盖线上调度（含「定义不同也照旧幂等」这一现状）；
//   3. 审计写失败 / CAS 未命中都必须整体回滚，库里不能留下半条结果。
//
// 已知缺陷按 README 编号钉在本文件末尾的 *Sentinel* 用例里（B/C/D/F/G），
// 它们锁定的是现状而非期望，改生产代码不在本轮范围。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/cron/internal/registry"
	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"
)

// --- 本文件共用的小工具 ---

// intervalProto 一条最小可用的 INTERVAL 任务定义（注册语境）。
func intervalProto(taskKey string) *rpc.TaskDefinition {
	return &rpc.TaskDefinition{
		TaskKey: taskKey, Name: "日报扫描", Handler: "report.daily",
		ScheduleType: rpc.ScheduleType_SCHEDULE_TYPE_INTERVAL, IntervalSeconds: 3600,
		Timezone: "UTC", TimeoutSeconds: 600, MaxAttempts: 1, LeaseTtlSeconds: 120,
		MisfirePolicy: rpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ONCE_NOW,
	}
}

func registerReq(def *rpc.TaskDefinition) *rpc.RegisterTaskReq {
	return &rpc.RegisterTaskReq{
		Definition: def, IdempotencyKey: "req-" + def.TaskKey,
		Operator: "ops.alice", TraceId: "trace-reg",
	}
}

func updateReq(taskKey string, version int64, def *rpc.TaskDefinition) *rpc.UpdateTaskReq {
	return &rpc.UpdateTaskReq{
		TaskKey: taskKey, Definition: def, ExpectedVersion: version,
		Operator: "ops.bob", TraceId: "trace-upd",
	}
}

// storedDef 直接取内存里的**活行**（同包访问），用于断言「库里到底留下了什么」。
func storedDef(t *testing.T, db *fakeDB, taskKey string) *model.TaskDefinition {
	t.Helper()
	row, ok := db.defs[taskKey]
	if !ok {
		t.Fatalf("库里没有 %s（现有 %d 行）", taskKey, len(db.defs))
	}
	return row
}

// assertNoWrites 断言「拒绝发生在碰库之前」：没开事务、没写定义、没写审计。
func assertNoWrites(t *testing.T, db *fakeDB, label string) {
	t.Helper()
	if db.txRuns != 0 || len(db.defs) != 0 || len(db.audits) != 0 {
		t.Errorf("%s：拒绝不该留下任何痕迹 txRuns=%d defs=%d audits=%d",
			label, db.txRuns, len(db.defs), len(db.audits))
	}
}

// assertDelta 断言 got 落在「now + want 秒」的 ±2 秒窗口内（fakeNow 与 ServerTime 同源）。
func assertDelta(t *testing.T, got, want int64, label string) {
	t.Helper()
	if diff := absInt64(got - (fakeNow() + want)); diff > 2 {
		t.Errorf("%s = %d，期望约 now+%d（容差 2s）", label, got, want)
	}
}

// detailFields 解析审计 detail（auditDetail 产出的是键升序的合法 JSON 对象）。
func detailFields(t *testing.T, a *model.TaskAudit) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(a.Detail), &m); err != nil {
		t.Fatalf("审计 detail 不是合法 JSON：%q (%v)", a.Detail, err)
	}
	return m
}

// assertDetail 逐项核对 detail 里**列出的**键（不要求键集恰好相等）。
func assertDetail(t *testing.T, a *model.TaskAudit, want map[string]any) {
	t.Helper()
	got := detailFields(t, a)
	for k, w := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("审计 detail 缺字段 %q：%+v", k, got)
			continue
		}
		if fmt.Sprint(v) != fmt.Sprint(w) {
			t.Errorf("审计 detail[%q] = %v，期望 %v", k, v, w)
		}
	}
}

// onlyAudit 要求本次操作恰好留下一条审计，并返回它。
func onlyAudit(t *testing.T, db *fakeDB, action string) *model.TaskAudit {
	t.Helper()
	if len(db.audits) != 1 {
		t.Fatalf("审计应为 1 条，得到 %d：%+v", len(db.audits), db.audits)
	}
	a := db.audits[0]
	if a.Action != action {
		t.Fatalf("审计动作 = %q，期望 %q", a.Action, action)
	}
	return a
}

// --- RegisterTask ---

func TestRegisterTaskRejectsBeforeTouchingDatabase(t *testing.T) {
	cases := []struct {
		name     string
		specific func(*svc.ServiceContext) // 注册表装配（留空即用空注册表）
		nilReq   bool                      // 整个请求为 nil（go-zero handler 可能传下来）
		mutate   func(*rpc.RegisterTaskReq)
		wantErr  error
		wantText string
	}{
		{name: "nil 请求", nilReq: true, wantErr: model.ErrTaskKeyEmpty},
		{name: "缺 definition", mutate: func(r *rpc.RegisterTaskReq) { r.Definition = nil },
			wantErr: model.ErrTaskKeyEmpty},
		{name: "缺幂等键", mutate: func(r *rpc.RegisterTaskReq) { r.IdempotencyKey = "  " },
			wantErr: model.ErrIdempotencyKeyEmpty},
		{name: "task_key 空白", mutate: func(r *rpc.RegisterTaskReq) { r.Definition.TaskKey = "   " },
			wantErr: model.ErrTaskKeyEmpty},
		{name: "调度方式未指定", mutate: func(r *rpc.RegisterTaskReq) {
			r.Definition.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_UNSPECIFIED
		}, wantErr: model.ErrInvalidSchedule},
		{name: "INTERVAL 缺周期", mutate: func(r *rpc.RegisterTaskReq) { r.Definition.IntervalSeconds = 0 },
			wantErr: model.ErrInvalidSchedule},
		{name: "CRON 缺表达式", mutate: func(r *rpc.RegisterTaskReq) {
			r.Definition.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_CRON
			r.Definition.CronExpr = ""
		}, wantErr: model.ErrInvalidSchedule},
		{name: "CRON 表达式字段数不对", mutate: func(r *rpc.RegisterTaskReq) {
			r.Definition.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_CRON
			r.Definition.CronExpr = "0 3 * *"
			r.Definition.IntervalSeconds = 0
		}, wantErr: model.ErrInvalidSchedule},
		{name: "时区不存在", mutate: func(r *rpc.RegisterTaskReq) {
			r.Definition.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_CRON
			r.Definition.CronExpr = "0 3 * * *"
			r.Definition.IntervalSeconds = 0
			r.Definition.Timezone = "Mars/Valles_Maris"
		}, wantErr: model.ErrInvalidSchedule},
		{name: "params 超上限", mutate: func(r *rpc.RegisterTaskReq) {
			r.Definition.Params = strings.Repeat("x", model.MaxParamsBytes+1)
		}, wantErr: model.ErrParamsTooLarge},
		{name: "开重试但不给退避基数", mutate: func(r *rpc.RegisterTaskReq) {
			r.Definition.MaxAttempts = 3
			r.Definition.RetryBaseSeconds = 0
		}, wantErr: model.ErrInvalidRetryPolicy},
		{name: "handler 未注册", mutate: func(r *rpc.RegisterTaskReq) { r.Definition.Handler = "nope.gone" },
			wantErr: model.ErrHandlerNotRegistered},
		{name: "注册即声明停用", mutate: func(r *rpc.RegisterTaskReq) {
			r.Definition.State = rpc.TaskState_TASK_STATE_DISABLED
		}, wantErr: model.ErrStateTransition},
		{name: "非法状态枚举", mutate: func(r *rpc.RegisterTaskReq) {
			r.Definition.State = rpc.TaskState(9)
		}, wantText: "invalid task state"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			registerOK(t, svcCtx, "report.daily")
			if tc.specific != nil {
				tc.specific(svcCtx)
			}
			var in *rpc.RegisterTaskReq
			if !tc.nilReq {
				in = registerReq(intervalProto("report.daily_scan"))
				if tc.mutate != nil {
					tc.mutate(in)
				}
			}
			reply, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(in)
			switch {
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			case tc.wantErr == nil && (err == nil || !strings.Contains(err.Error(), tc.wantText)):
				t.Fatalf("err = %v，期望文本包含 %q", err, tc.wantText)
			}
			if reply != nil {
				t.Errorf("拒绝时不得回定义：%+v", reply)
			}
			assertNoWrites(t, db, tc.name)
		})
	}
	// 对照项：同一份数据、注册表里有 handler 时必然成功，
	// 说明上面的红全都来自入参/契约本身，而不是布景坏了。
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(intervalProto("report.daily_scan"))); err != nil {
		t.Fatalf("对照用例应注册成功：%v", err)
	}
	if len(db.defs) != 1 {
		t.Errorf("对照用例应落下 1 行，得到 %d", len(db.defs))
	}
}

func TestRegisterTaskCreatesDefinitionWithServerDefaults(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")

	before := fakeNow()
	reply, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(intervalProto(testTaskKey)))
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Created {
		t.Errorf("首次注册 created = false，dedupe=%q", reply.DedupeReason)
	}
	row := storedDef(t, db, testTaskKey)

	if row.Version != 1 {
		t.Errorf("首版 version = %d，期望 1", row.Version)
	}
	if row.State != model.TaskStateEnabled {
		t.Errorf("未声明状态时默认 ENABLED，得到 %s", model.TaskStateName(row.State))
	}
	if row.Name != "日报扫描" || row.Handler != "report.daily" {
		t.Errorf("可改字段没取请求值：%+v", row)
	}
	if row.IntervalSeconds != 3600 || row.TimeoutSeconds != 600 || row.LeaseTTLSeconds != 120 {
		t.Errorf("调度参数未按请求落库：%+v", row)
	}
	if row.MaxAttempts != 1 || row.ConcurrencyLimit != 1 || row.MisfireBackfillLim != 5 {
		t.Errorf("未声明的并发/补齐上限没兜底：%+v", row)
	}
	if row.Operator != "ops.alice" {
		t.Errorf("operator = %q，期望取请求值", row.Operator)
	}
	if row.MisfirePolicy != model.MisfirePolicyFireOnceNow {
		t.Errorf("显式声明的 misfire 策略丢了：%d", row.MisfirePolicy)
	}
	// 调度指针由服务端现算，绝不接受调用方传值。
	assertDelta(t, row.NextFireAt, 3600, "INTERVAL 首个计划点")
	if row.NextFireAt <= before {
		t.Errorf("首个计划点必须晚于请求时刻：%d vs %d", row.NextFireAt, before)
	}

	audit := onlyAudit(t, db, model.AuditActionRegister)
	if audit.TaskKey != testTaskKey || audit.ToState != "enabled" || audit.FromState != "" {
		t.Errorf("注册审计的状态文本不对：%+v", audit)
	}
	if audit.Operator != "ops.alice" || audit.TraceID != "trace-reg" {
		t.Errorf("注册审计没留下可追溯身份：%+v", audit)
	}
	assertDetail(t, audit, map[string]any{
		"handler": "report.daily", "schedule": `interval(3600s, tz=UTC)`,
		"timeout_seconds": float64(600), "max_attempts": float64(1),
		"concurrency_limit": float64(1), "lease_ttl_seconds": float64(120),
		"misfire_policy": float64(model.MisfirePolicyFireOnceNow),
		"params_bytes":   float64(0), "idempotency_key": "req-" + testTaskKey,
		"next_fire_at": float64(row.NextFireAt), // 审计里的指针必须与库里一致
	})
}

// TestRegisterTaskCallerSuppliedPointerIsIgnored 证明 next_fire_at/version/state
// 三个「调用方可能塞值」的字段一律由服务端决定。
func TestRegisterTaskCallerSuppliedPointerIsIgnored(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")

	in := registerReq(intervalProto(testTaskKey))
	in.Definition.NextFireAt = 123    // 伪造计划点
	in.Definition.Version = 77        // 伪造乐观锁版本
	in.Definition.LastFireAt = 456    // 伪造「已经跑过」
	in.Definition.LastSuccessAt = 789 // 伪造成功时间
	in.Definition.LastError = "已修复"   // 伪造历史错误

	if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(in); err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	assertDelta(t, row.NextFireAt, 3600, "next_fire_at 由服务端现算")
	if row.Version != 1 {
		t.Errorf("version = %d，注册时只能是 1", row.Version)
	}
	if row.LastFireAt != 0 || row.LastSuccessAt != 0 || row.LastError != "" {
		t.Errorf("执行历史字段被调用方写进了新定义：%+v", row)
	}
}

// TestRegisterTaskPointerPerScheduleType 三种调度方式各自的首个计划点。
// 这里只断言「指针的性质」（是否命中声明的时区/是否在未来/手动任务恒 0），
// 不在测试里重算 cron——那会造出第二套调度语义。
func TestRegisterTaskPointerPerScheduleType(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")

	cron := intervalProto(testTaskKey)
	cron.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_CRON
	cron.CronExpr = "0 3 * * *"
	cron.IntervalSeconds = 0
	reply, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(registerReq(cron))
	if err != nil {
		t.Fatal(err)
	}
	next := reply.GetDefinition().GetNextFireAt()
	if at := time.Unix(next, 0).UTC(); at.Hour() != 3 || at.Minute() != 0 || at.Second() != 0 {
		t.Errorf("UTC 的 0 3 * * * 应落在 03:00:00Z，得到 %s", at)
	}
	if delta := next - fakeNow(); delta <= 0 || delta > 86400+60 {
		t.Errorf("首个计划点应在未来一天内，得到 now+%d", delta)
	}

	// 换时区后同一表达式指向不同的 UTC 瞬间：证明 timezone 真的参与了计算。
	cron2 := intervalProto("report.daily_sh")
	cron2.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_CRON
	cron2.CronExpr = "0 3 * * *"
	cron2.IntervalSeconds = 0
	cron2.Timezone = "Asia/Shanghai"
	if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(cron2)); err != nil {
		t.Fatal(err)
	}
	sh := storedDef(t, db, "report.daily_sh").NextFireAt
	if at := time.Unix(sh, 0).UTC(); at.Hour() != 19 || at.Minute() != 0 {
		t.Errorf("03:00 Asia/Shanghai 应是前一天 19:00Z，得到 %s", at)
	}

	manual := intervalProto("report.manual")
	manual.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_MANUAL
	manual.CronExpr = ""
	manual.IntervalSeconds = 0
	if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(manual)); err != nil {
		t.Fatal(err)
	}
	if got := storedDef(t, db, "report.manual").NextFireAt; got != 0 {
		t.Errorf("手动任务的计划点 = %d，期望 0（不参与到期扫描）", got)
	}

	// 注册成 PAUSED 时仍带着算好的计划点：ListDue 靠 state=ENABLED 拦住它，
	// 所以不影响调度，但「暂停=指针归零」这条不变式在这里不成立（README 已登记）。
	paused := intervalProto("report.paused")
	paused.State = rpc.TaskState_TASK_STATE_PAUSED
	if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(paused)); err != nil {
		t.Fatal(err)
	}
	prow := storedDef(t, db, "report.paused")
	if prow.State != model.TaskStatePaused || prow.NextFireAt == 0 {
		t.Errorf("当前行为已变，请同步 README：state=%s next_fire_at=%d",
			model.TaskStateName(prow.State), prow.NextFireAt)
	}
}

func TestRegisterTaskAppliesRegistryTimeoutAndBoundaries(t *testing.T) {
	t.Run("timeout 由注册表兜底", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		registerHandler(t, svcCtx, registry.Spec{Name: "report.daily", SuggestedTimeout: 90})
		in := registerReq(intervalProto(testTaskKey))
		in.Definition.TimeoutSeconds = 0
		if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(in); err != nil {
			t.Fatal(err)
		}
		if got := storedDef(t, db, testTaskKey).TimeoutSeconds; got != 90 {
			t.Errorf("timeout_seconds = %d，期望注册表建议值 90", got)
		}

		// 判别性对照：DB 显式给了值就不该被建议值覆盖。
		svcCtx2, db2 := newTestSvc(t)
		registerHandler(t, svcCtx2, registry.Spec{Name: "report.daily", SuggestedTimeout: 90})
		if _, err := NewRegisterTaskLogic(context.Background(), svcCtx2).
			RegisterTask(registerReq(intervalProto(testTaskKey))); err != nil {
			t.Fatal(err)
		}
		if got := storedDef(t, db2, testTaskKey).TimeoutSeconds; got != 600 {
			t.Errorf("显式 600 被改成了 %d", got)
		}
	})

	t.Run("两边都没有 timeout 即注册表冲突", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		registerOK(t, svcCtx, "report.daily") // Spec.SuggestedTimeout = 0
		in := registerReq(intervalProto(testTaskKey))
		in.Definition.TimeoutSeconds = 0
		_, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(in)
		if !errors.Is(err, registry.ErrSpecConflict) {
			t.Fatalf("err = %v，期望 ErrSpecConflict", err)
		}
		assertNoWrites(t, db, "缺 timeout")
	})

	t.Run("代码边界逐条拦住", func(t *testing.T) {
		cases := []struct {
			name   string
			spec   registry.Spec
			mutate func(*rpc.TaskDefinition)
		}{
			{"SerialOnly 只许串行", registry.Spec{Name: "report.daily", SerialOnly: true},
				func(d *rpc.TaskDefinition) { d.ConcurrencyLimit = 2 }},
			{"max_attempts 上限", registry.Spec{Name: "report.daily", SuggestedMaxAttempts: 3},
				func(d *rpc.TaskDefinition) {
					d.MaxAttempts = 4
					d.RetryBaseSeconds = 30
				}},
			{"租约 TTL 下限", registry.Spec{Name: "report.daily", MinLeaseTTLSeconds: 600},
				func(d *rpc.TaskDefinition) { d.LeaseTtlSeconds = 120 }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				svcCtx, db := newTestSvc(t)
				registerHandler(t, svcCtx, tc.spec)
				in := registerReq(intervalProto(testTaskKey))
				tc.mutate(in.Definition)
				_, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(in)
				if !errors.Is(err, registry.ErrSpecConflict) {
					t.Fatalf("err = %v，期望 ErrSpecConflict", err)
				}
				assertNoWrites(t, db, tc.name)

				// 判别性对照：同一 Spec 下不越界的配置必须放行，
				// 否则「永远报错」也能骗过上面的断言。
				svcCtx2, db2 := newTestSvc(t)
				registerHandler(t, svcCtx2, tc.spec)
				ok := intervalProto(testTaskKey)
				if tc.spec.SerialOnly {
					ok.ConcurrencyLimit = 1
				}
				if tc.spec.SuggestedMaxAttempts > 0 {
					ok.MaxAttempts = tc.spec.SuggestedMaxAttempts
					ok.RetryBaseSeconds = 30
				}
				if tc.spec.MinLeaseTTLSeconds > 0 {
					ok.LeaseTtlSeconds = int32(tc.spec.MinLeaseTTLSeconds)
					ok.TimeoutSeconds = int32(tc.spec.MinLeaseTTLSeconds)
				}
				if _, err := NewRegisterTaskLogic(context.Background(), svcCtx2).
					RegisterTask(registerReq(ok)); err != nil {
					t.Fatalf("界内配置被拒：%v", err)
				}
				if len(db2.defs) != 1 {
					t.Errorf("对照用例应落 1 行，得到 %d", len(db2.defs))
				}
			})
		}
	})
}

// TestRegisterTaskIsIdempotentOnUniqueKey 重复注册（含「定义不同」的重复注册）
// 只能读回线上行，绝不覆盖，也不产生第二条审计。
func TestRegisterTaskIsIdempotentOnUniqueKey(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")

	first, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(intervalProto(testTaskKey)))
	if err != nil || !first.Created {
		t.Fatalf("首次注册：%+v / %v", first, err)
	}
	before := *storedDef(t, db, testTaskKey)

	// 第二次：换了名字、换了周期——契约上这属于「改配置」，必须走 UpdateTask。
	second := intervalProto(testTaskKey)
	second.Name = "被忽略的新名"
	second.IntervalSeconds = 60
	second.TimeoutSeconds = 30
	reply, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(second))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Created {
		t.Error("唯一键命中却报告 created=true")
	}
	if !strings.Contains(reply.DedupeReason, "uniq_task_key") ||
		!strings.Contains(reply.DedupeReason, "UpdateTask") {
		t.Errorf("去重说明必须指出命中哪个键、下一步该调什么： %q", reply.DedupeReason)
	}
	if reply.GetDefinition().GetName() != before.Name {
		t.Errorf("线上定义被覆盖：name=%q（原 %q）", reply.GetDefinition().GetName(), before.Name)
	}
	after := *storedDef(t, db, testTaskKey)
	if after != before {
		t.Errorf("重复注册改动了库里行：\n前 %+v\n后 %+v", before, after)
	}
	// 库里始终只有那一条注册审计。
	onlyAudit(t, db, model.AuditActionRegister)
}

func TestRegisterTaskRollsBackWhenAuditFails(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	db.auditErr = errors.New("audit table is gone")

	_, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(intervalProto(testTaskKey)))
	if err == nil || !strings.Contains(err.Error(), "audit table is gone") {
		t.Fatalf("err = %v，期望审计写失败原样冒出来", err)
	}
	if len(db.defs) != 0 {
		t.Errorf("审计失败后留下了 %d 条「有调度却没痕迹」的定义", len(db.defs))
	}
	if db.txRuns != 1 || db.txRollups != 1 {
		t.Errorf("事务应回滚一次：runs=%d rollups=%d", db.txRuns, db.txRollups)
	}
}

// TestRegisterTaskFailsLoudlyWhenDuplicateRowUnreadable 唯一键说「已有这行」、
// 按 key 却读不回来时，绝不能回一个零值定义冒充幂等成功。
func TestRegisterTaskFailsLoudlyWhenDuplicateRowUnreadable(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	enabledDef(db)
	db.unreadable[testTaskKey] = true

	reply, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(intervalProto(testTaskKey)))
	if !errors.Is(err, model.ErrTaskNotFound) {
		t.Fatalf("err = %v，期望 ErrTaskNotFound", err)
	}
	if reply != nil {
		t.Errorf("读不回时必须显式失败，不得回 %+v", reply)
	}
	if db.txRuns != 1 {
		t.Errorf("这次确实进了事务并撞上了库不一致：txRuns=%d", db.txRuns)
	}
	if len(db.audits) != 0 {
		t.Errorf("未落库的注册不该有审计：%+v", db.audits)
	}
}

// TestRegisterTaskDropsAutoIncrementIdSentinel 哨兵用例（不改生产代码，只钉住现状）。
//
// TODO(缺陷 B)：model/tx.go 的 Insert 从不读 LastInsertId，所以 RegisterTask 回包里的
// task_id 恒为 0，而 UpdateTask/Pause/Resume/Disable 都会回读到真主键。
// 后台拿注册响应做「刚建的任务 id」时会得到 0。见 README「已知缺陷」。
func TestRegisterTaskDropsAutoIncrementIdSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")

	reply, err := NewRegisterTaskLogic(context.Background(), svcCtx).
		RegisterTask(registerReq(intervalProto(testTaskKey)))
	if err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.ID == 0 {
		t.Fatal("假库没发主键，本用例的判别性不成立")
	}
	info := reply.GetDefinition()
	if info == nil {
		t.Fatal("注册成功却回了空定义")
	}
	if info.TaskId != 0 {
		t.Errorf("现状已改变（task_id=%d，库里 %d）：请删除本哨兵并同步 README", info.TaskId, row.ID)
	}

	// 判别性对照：同一个任务再读一次，task_id 就是真值——
	// 说明丢主键只发生在注册这一次响应里，而不是「本服务没有主键」。
	got, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: testTaskKey})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDefinition().GetTaskId() != row.ID {
		t.Errorf("GetTask 主键 = %d，期望库里的 %d", got.GetDefinition().GetTaskId(), row.ID)
	}
}

// TestRegisterTaskSkipsServerSideDefinitionDefaultsSentinel 哨兵用例。
//
// TODO(缺陷 C)：registertasklogic.go:44 用 full=false 转义请求，于是
// definitionFromProto 的注册语境（helpers.go:280-316）整段成为死代码：
//   - task_group 不补 "default"、timezone 不补 config 默认值；
//   - misfire_policy 未声明时落成 UNSPECIFIED(0) 而不是 FIRE_ONCE_NOW；
//   - lease_ttl_seconds 的绝对上限（model.MaxLeaseTTLSeconds）没跑，
//     于是荒谬的 TTL 能注册进来，等到 AcquireLease 才在 svc.LeaseTTL 处炸。
//
// 下面钉住前两条的可观测后果；misfire=0 让 ResumeTask 必然失败的链条见
// task_state_transition_test.go 的同名哨兵。
func TestRegisterTaskSkipsServerSideDefinitionDefaultsSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")

	in := registerReq(intervalProto(testTaskKey))
	in.Definition.TaskGroup = ""
	in.Definition.Timezone = ""
	in.Definition.MisfirePolicy = rpc.MisfirePolicy_MISFIRE_POLICY_UNSPECIFIED
	if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(in); err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.TaskGroup != "" {
		t.Errorf("现状已改变（task_group=%q）：请删除本哨兵并同步 README", row.TaskGroup)
	}
	if row.Timezone != "" {
		t.Errorf("现状已改变（timezone=%q）：请删除本哨兵", row.Timezone)
	}
	if row.MisfirePolicy != model.MisfirePolicyUnspecified {
		t.Errorf("现状已改变（misfire_policy=%d）：请删除本哨兵", row.MisfirePolicy)
	}
	// 同一份请求走「更新语境 + 服务端默认」的对照：默认值本该长成什么样。
	defaults := defaultsOf(svcCtx)
	withDefaults, err := defaults.definitionFromProto(in.Definition, true)
	if err != nil {
		t.Fatal(err)
	}
	if withDefaults.TaskGroup != defaultTaskGroupName || withDefaults.Timezone != "UTC" ||
		withDefaults.MisfirePolicy != model.MisfirePolicyFireOnceNow {
		t.Fatalf("注册语境本该补齐：%+v", withDefaults)
	}

	// 审计文本里也能看到时区是空的：schedule_summary 会写成 tz=。
	audit := onlyAudit(t, db, model.AuditActionRegister)
	assertDetail(t, audit, map[string]any{"task_group": "", "schedule": `interval(3600s, tz=)`})
}

// TestRegisterTaskAcceptsAbsurdLeaseTtlSentinel 哨兵用例（缺陷 C 的第二段）。
//
// TODO(缺陷 C)：lease_ttl_seconds=99999999（远超 model.MaxLeaseTTLSeconds=86400）
// 能注册成功，但同一行在 AcquireLease 里必然报 ErrInvalidLeaseTTL：
// 「注册通过 = 任务可跑」这条契约不成立，故障推迟到第一次执行才暴露。
func TestRegisterTaskAcceptsAbsurdLeaseTtlSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")

	in := registerReq(intervalProto(testTaskKey))
	in.Definition.LeaseTtlSeconds = model.MaxLeaseTTLSeconds + 1
	if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(in); err != nil {
		t.Fatalf("注册被拒（现状已改变，请删除本哨兵）：%v", err)
	}
	if got := storedDef(t, db, testTaskKey).LeaseTTLSeconds; got != model.MaxLeaseTTLSeconds+1 {
		t.Errorf("库里的 TTL = %d，与请求不符", got)
	}

	// 判别性对照：走注册语境（full=true）的同一条定义会被同一条上限拦住。
	defaults := defaultsOf(svcCtx)
	if _, err := defaults.definitionFromProto(in.Definition, true); !errors.Is(err, model.ErrInvalidLeaseTTL) {
		t.Errorf("full=true 语境 err = %v，期望 ErrInvalidLeaseTTL（上限校验本身是存在的）", err)
	}

	// 判别性对照 2：真正跑起来时这个 TTL 必然失败（svc.LeaseTTL 是唯一的收敛口径）。
	if _, err := svcCtx.LeaseTTL(int32(model.MaxLeaseTTLSeconds + 1)); !errors.Is(err, model.ErrInvalidLeaseTTL) {
		t.Errorf("svc.LeaseTTL err = %v，期望 ErrInvalidLeaseTTL", err)
	}
}

// --- UpdateTask ---

func TestUpdateTaskRejectsBeforeReadingRow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily", TaskGroup: "report",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 1,
		NextFireAt: 1_700_000_000, Params: `{"a":1}`,
	})
	before := *storedDef(t, db, testTaskKey)

	cases := []struct {
		name    string
		in      *rpc.UpdateTaskReq
		wantErr error
	}{
		{"nil 请求", nil, model.ErrTaskKeyEmpty},
		{"缺 definition", &rpc.UpdateTaskReq{TaskKey: testTaskKey, ExpectedVersion: 1},
			model.ErrTaskKeyEmpty},
		{"task_key 空白", updateReq("   ", 1, intervalProto(testTaskKey)), model.ErrTaskKeyEmpty},
		{"没给 expected_version", updateReq(testTaskKey, 0, intervalProto(testTaskKey)),
			model.ErrVersionConflict},
		{"expected_version 为负", updateReq(testTaskKey, -5, intervalProto(testTaskKey)),
			model.ErrVersionConflict},
		{"路径与体内的 key 不一致", updateReq(testTaskKey, 1, intervalProto("other.task")),
			model.ErrTaskKeyEmpty},
		{"params 超上限", updateReq(testTaskKey, 1, func() *rpc.TaskDefinition {
			d := intervalProto(testTaskKey)
			d.Params = strings.Repeat("x", model.MaxParamsBytes+1)
			return d
		}()), model.ErrParamsTooLarge},
		{"改成没注册的 handler", updateReq(testTaskKey, 1, func() *rpc.TaskDefinition {
			d := intervalProto(testTaskKey)
			d.Handler = "nope.gone"
			return d
		}()), model.ErrHandlerNotRegistered},
		{"改成非法调度", updateReq(testTaskKey, 1, func() *rpc.TaskDefinition {
			d := intervalProto(testTaskKey)
			d.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_CRON
			d.CronExpr = ""
			return d
		}()), model.ErrInvalidSchedule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := NewUpdateTaskLogic(context.Background(), svcCtx).UpdateTask(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if reply != nil && reply.Definition != nil {
				t.Errorf("拒绝时不得回定义：%+v", reply)
			}
		})
	}
	if after := *storedDef(t, db, testTaskKey); after != before {
		t.Errorf("被拒的更新改动了库里行：\n前 %+v\n后 %+v", before, after)
	}
	if len(db.audits) != 0 || db.txRuns != 0 {
		t.Errorf("拒绝不该有事务/审计痕迹：txRuns=%d audits=%d", db.txRuns, len(db.audits))
	}

	// 任务不存在时同样不能进事务（读定义在事务外，见 updatetasklogic.go:60）。
	_, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq("report.never_registered", 1, intervalProto("report.never_registered")))
	if !errors.Is(err, model.ErrTaskNotFound) {
		t.Errorf("err = %v，期望 ErrTaskNotFound", err)
	}
	if db.txRuns != 0 {
		t.Errorf("目标不存在时不该开事务：%d", db.txRuns)
	}
}

func TestUpdateTaskBumpsVersionAndAuditsChange(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily", TaskGroup: "report",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 1,
		NextFireAt: 1_700_000_000, Operator: "ops.alice",
	})

	patch := intervalProto(testTaskKey)
	patch.Name = "新名"
	patch.TimeoutSeconds = 900
	reply, err := NewUpdateTaskLogic(context.Background(), svcCtx).UpdateTask(updateReq(testTaskKey, 1, patch))
	if err != nil {
		t.Fatal(err)
	}
	// 回读而非回快照：version 必须是库里的新值。
	if reply.GetDefinition().GetVersion() != 2 {
		t.Errorf("回包 version = %d，期望 2（提交后回读）", reply.GetDefinition().GetVersion())
	}
	if reply.GetDefinition().GetName() != "新名" || reply.GetDefinition().GetTimeoutSeconds() != 900 {
		t.Errorf("回包没反映改动：%+v", reply.GetDefinition())
	}
	row := storedDef(t, db, testTaskKey)
	if row.Version != 2 || row.Name != "新名" || row.TimeoutSeconds != 900 {
		t.Errorf("库里行不对：%+v", row)
	}
	if row.NextFireAt != 1_700_000_000 {
		t.Errorf("只改超时不该动调度指针，得到 %d", row.NextFireAt)
	}
	if row.State != model.TaskStateEnabled {
		t.Errorf("UpdateTask 不该碰状态：%s", model.TaskStateName(row.State))
	}

	audit := onlyAudit(t, db, model.AuditActionUpdate)
	if audit.FromState != "enabled" || audit.ToState != "enabled" {
		t.Errorf("更新审计的状态文本应保持不变：%+v", audit)
	}
	if audit.TraceID != "trace-upd" {
		t.Errorf("审计丢了 trace_id：%+v", audit)
	}
	assertDetail(t, audit, map[string]any{
		"expected_version": float64(1), "schedule_changed": false,
		"prev_schedule": "interval(3600s, tz=UTC)", "schedule": "interval(3600s, tz=UTC)",
		"handler_changed": false, "params_bytes": float64(0), "operator": "ops.bob",
	})
	// 指针没动，就不该在审计里编造一个 next_fire_at 变更。
	got := detailFields(t, audit)
	if _, ok := got["next_fire_at"]; ok {
		t.Errorf("指针未变动却写了 next_fire_at：%+v", got)
	}
	if _, ok := got["prev_next_fire_at"]; ok {
		t.Errorf("指针未变动却写了 prev_next_fire_at：%+v", got)
	}
}

// TestUpdateTaskMergeSemantics 逐字段核对「空串/0 表示不改」这条 proto3 下的约定，
// 以及 params/secret_refs 允许显式清空的例外。
func TestUpdateTaskMergeSemantics(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily", TaskGroup: "report",
		ScheduleType: model.ScheduleTypeCron, CronExpr: "0 3 * * *", Timezone: "Asia/Shanghai",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 3, RetryBaseSeconds: 60,
		RetryMaxSeconds: 600, ConcurrencyLimit: 2, LeaseTTLSeconds: 300,
		MisfirePolicy: model.MisfirePolicyFireAll, MisfireBackfillLim: 9, Version: 4,
		NextFireAt: 1_700_000_000, Owner: "report-team", Params: `{"a":1}`, SecretRefs: "TOKEN_A",
	})

	// 只声明「换个名字 + 清空参数」，其余全空。
	patch := &rpc.TaskDefinition{
		TaskKey:      testTaskKey,                         // 更新语境也必填，见 TestUpdateTaskMustResendTaskKeySentinel
		ScheduleType: rpc.ScheduleType_SCHEDULE_TYPE_CRON, // 现状：必须重发调度方式（见缺陷 F 哨兵）
		Name:         "新名",
	}
	if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 4, patch)); err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.Name != "新名" {
		t.Errorf("name 没改成：%q", row.Name)
	}
	if row.Handler != "report.daily" || row.TaskGroup != "report" || row.Owner != "report-team" ||
		row.Timezone != "Asia/Shanghai" || row.CronExpr != "0 3 * * *" {
		t.Errorf("漏传的文本字段被清空了（会把线上任务改成死定义）：%+v", row)
	}
	if row.TimeoutSeconds != 600 || row.MaxAttempts != 3 || row.RetryBaseSeconds != 60 ||
		row.RetryMaxSeconds != 600 || row.ConcurrencyLimit != 2 || row.LeaseTTLSeconds != 300 ||
		row.MisfirePolicy != model.MisfirePolicyFireAll || row.MisfireBackfillLim != 9 {
		t.Errorf("数值 0 应表示「不改」：%+v", row)
	}
	// params / secret_refs 是唯一允许显式清空的两个字段。
	if row.Params != "" || row.SecretRefs != "" {
		t.Errorf("params/secret_refs 应被显式清空，得到 %q/%q", row.Params, row.SecretRefs)
	}
	// 未声明 misfire 策略时不得降级（0 表示不改）。
	patch2 := &rpc.TaskDefinition{TaskKey: testTaskKey, ScheduleType: rpc.ScheduleType_SCHEDULE_TYPE_CRON}
	if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 5, patch2)); err != nil {
		t.Fatal(err)
	}
	if got := storedDef(t, db, testTaskKey).MisfirePolicy; got != model.MisfirePolicyFireAll {
		t.Errorf("misfire 策略被降级成 %d", got)
	}
}

// TestUpdateTaskPointerOnlyForEnabledScheduleChanges 调度指针的三条边界：
// 改了计划且启用中才重算；暂停中的指针由 ResumeTask 负责；切到手动即归零。
func TestUpdateTaskPointerOnlyForEnabledScheduleChanges(t *testing.T) {
	seed := func(t *testing.T) (*svc.ServiceContext, *fakeDB) {
		t.Helper()
		svcCtx, db := newTestSvc(t)
		registerOK(t, svcCtx, "report.daily")
		seedDefinition(db, &model.TaskDefinition{
			TaskKey: testTaskKey, Name: "日报", Handler: "report.daily", TaskGroup: "report",
			ScheduleType: model.ScheduleTypeCron, CronExpr: "0 3 * * *", Timezone: "UTC",
			State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
			LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow,
			Version: 1, NextFireAt: 1_700_000_000,
		})
		return svcCtx, db
	}
	cronPatch := func() *rpc.TaskDefinition {
		return &rpc.TaskDefinition{
			TaskKey: testTaskKey, ScheduleType: rpc.ScheduleType_SCHEDULE_TYPE_CRON,
			CronExpr: "30 4 * * *", Timezone: "UTC", TimeoutSeconds: 600, LeaseTtlSeconds: 120,
		}
	}

	t.Run("启用中改计划即重算", func(t *testing.T) {
		svcCtx, db := seed(t)
		if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
			UpdateTask(updateReq(testTaskKey, 1, cronPatch())); err != nil {
			t.Fatal(err)
		}
		row := storedDef(t, db, testTaskKey)
		if row.NextFireAt == 1_700_000_000 {
			t.Fatal("改了 cron_expr 却没重算指针，旧计划点会误触发一次执行")
		}
		if at := time.Unix(row.NextFireAt, 0).UTC(); at.Hour() != 4 || at.Minute() != 30 {
			t.Errorf("新指针应指向 04:30Z，得到 %s", at)
		}
		audit := onlyAudit(t, db, model.AuditActionUpdate)
		d := detailFields(t, audit)
		if d["schedule_changed"] != true {
			t.Errorf("审计应标出计划变了：%+v", d)
		}
		if d["prev_next_fire_at"].(float64) != 1_700_000_000 ||
			d["next_fire_at"].(float64) != float64(row.NextFireAt) {
			t.Errorf("审计丢了新旧指针：%+v", d)
		}
		if d["prev_schedule"] != `cron("0 3 * * *", tz=UTC)` ||
			d["schedule"] != `cron("30 4 * * *", tz=UTC)` {
			t.Errorf("审计的新旧计划文本不对：%+v", d)
		}
	})

	t.Run("暂停中改计划不动指针", func(t *testing.T) {
		svcCtx, db := seed(t)
		storedDef(t, db, testTaskKey).State = model.TaskStatePaused
		if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
			UpdateTask(updateReq(testTaskKey, 1, cronPatch())); err != nil {
			t.Fatal(err)
		}
		row := storedDef(t, db, testTaskKey)
		if row.NextFireAt != 1_700_000_000 {
			t.Errorf("暂停中的指针应由 ResumeTask 决定，这里被改成了 %d", row.NextFireAt)
		}
		if row.CronExpr != "30 4 * * *" {
			t.Errorf("计划字段本身仍要落库，得到 %q", row.CronExpr)
		}
	})

	t.Run("同计划只改超时不动指针", func(t *testing.T) {
		svcCtx, db := seed(t)
		patch := cronPatch()
		patch.CronExpr = "0 3 * * *" // 与库里一致
		patch.TimeoutSeconds = 900
		if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
			UpdateTask(updateReq(testTaskKey, 1, patch)); err != nil {
			t.Fatal(err)
		}
		row := storedDef(t, db, testTaskKey)
		if row.NextFireAt != 1_700_000_000 {
			t.Errorf("计划没变就不该重算指针，得到 %d", row.NextFireAt)
		}
		if row.TimeoutSeconds != 900 {
			t.Errorf("超时没生效：%d", row.TimeoutSeconds)
		}
		onlyAudit(t, db, model.AuditActionUpdate)
	})

	t.Run("切到手动即归零", func(t *testing.T) {
		svcCtx, db := seed(t)
		patch := &rpc.TaskDefinition{TaskKey: testTaskKey, ScheduleType: rpc.ScheduleType_SCHEDULE_TYPE_MANUAL}
		if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
			UpdateTask(updateReq(testTaskKey, 1, patch)); err != nil {
			t.Fatal(err)
		}
		row := storedDef(t, db, testTaskKey)
		if row.NextFireAt != 0 {
			t.Errorf("手动任务的计划点 = %d，期望 0", row.NextFireAt)
		}
		if row.ScheduleType != model.ScheduleTypeManual || row.CronExpr != "" {
			t.Errorf("换调度方式时旧表达式必须让位：%+v", row)
		}
	})
}

// TestUpdateTaskVersionConflictLeavesNoTrace 「别人先提交了新版本」时：
// 乐观锁未命中 → 一行都没改、没有审计、没有指针，只回 ErrVersionConflict。
func TestUpdateTaskVersionConflictLeavesNoTrace(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily", TaskGroup: "report",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 7,
		NextFireAt: 1_700_000_000,
	})
	before := *storedDef(t, db, testTaskKey)
	db.casMissOnUpdate = true // 并发里有别的写入者先提交了新版本

	patch := intervalProto(testTaskKey)
	patch.Name = "抢不过的名字"
	patch.TimeoutSeconds = 900
	reply, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 7, patch))
	if !errors.Is(err, model.ErrVersionConflict) {
		t.Fatalf("err = %v，期望 ErrVersionConflict", err)
	}
	if reply != nil {
		t.Errorf("冲突时不得回定义：%+v", reply)
	}
	if after := *storedDef(t, db, testTaskKey); after != before {
		t.Errorf("CAS 未命中却改写了行：\n前 %+v\n后 %+v", before, after)
	}
	if len(db.audits) != 0 {
		t.Errorf("冲突不该留审计：%+v", db.audits)
	}
	if db.txRuns != 1 || db.txRollups != 1 {
		t.Errorf("事务应回滚一次：runs=%d rollups=%d", db.txRuns, db.txRollups)
	}
}

func TestUpdateTaskRollsBackWhenAuditFails(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 1,
	})
	db.auditErr = errors.New("audit insert rejected")

	_, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 1, intervalProto(testTaskKey)))
	if err == nil || !strings.Contains(err.Error(), "audit insert rejected") {
		t.Fatalf("err = %v，期望原样冒出的审计错误", err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.Version != 1 || row.Name != "旧名" {
		t.Errorf("定义改了、痕迹没留下（半条结果）：%+v", row)
	}
	if db.txRollups != 1 {
		t.Errorf("事务应回滚一次，得到 %d", db.txRollups)
	}
}

// TestUpdateTaskStateAndKeyAreServerSide 后台不能借 UpdateTask 改状态或改键。
func TestUpdateTaskStateAndKeyAreServerSide(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateDisabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 1,
		NextFireAt: 1_700_000_000,
	})

	patch := intervalProto(testTaskKey)
	patch.State = rpc.TaskState_TASK_STATE_ENABLED // 想「顺手」把停用任务改回来
	patch.Version = 99                             // 想自己推进乐观锁
	if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 1, patch)); err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.State != model.TaskStateDisabled {
		t.Errorf("UpdateTask 越权改了状态：%s", model.TaskStateName(row.State))
	}
	if row.TaskKey != testTaskKey {
		t.Errorf("task_key 被改成了 %q", row.TaskKey)
	}
	if _, ok := db.defs["other.key"]; ok {
		t.Error("更新写到了另一个任务上")
	}
	if len(db.defs) != 1 {
		t.Errorf("库里多了任务定义（现有 %d 行）", len(db.defs))
	}
	// 状态没变，审计里的 from/to 文本必须仍是 disabled。
	audit := onlyAudit(t, db, model.AuditActionUpdate)
	if audit.FromState != "disabled" || audit.ToState != "disabled" {
		t.Errorf("审计状态文本：%+v", audit)
	}
}

// TestUpdateTaskIgnoresExpectedVersionSentinel 哨兵用例。
//
// TODO(缺陷 A)：updatetasklogic.go:44 只判 expected_version 是否给出，
// 第 120 行的 CAS 用的是第 60 行在事务外读到的 before.Version，
// 于是调用方传的 expected_version 完全不参与并发裁决：
// 两个后台各拿同一份快照改不同字段，后写的会静默覆盖先写的（丢更新），
// 而「expected_version 不匹配」这条冲突只能由真库的行版本变化触发。
func TestUpdateTaskIgnoresExpectedVersionSentinel(t *testing.T) {
	t.Run("现状：声称的 expected_version 不参与裁决", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		registerOK(t, svcCtx, "report.daily")
		seedDefinition(db, &model.TaskDefinition{
			TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily",
			ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
			State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
			LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 3,
		})
		patch := intervalProto(testTaskKey)
		patch.TimeoutSeconds = 900
		// 库里是 version=3，调用方声称 999 —— 按契约必须冲突，现状是照改。
		reply, err := NewUpdateTaskLogic(context.Background(), svcCtx).
			UpdateTask(updateReq(testTaskKey, 999, patch))
		if errors.Is(err, model.ErrVersionConflict) {
			t.Error("现状已改变（expected_version=999 被拒）：请删除本哨兵并同步 README")
			return
		}
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if got := storedDef(t, db, testTaskKey); got.TimeoutSeconds != 900 || got.Version != 4 {
			t.Errorf("库里行与预期不符：%+v", got)
		}
		if reply.GetDefinition().GetVersion() != 4 {
			t.Errorf("回包 version = %d，期望 4", reply.GetDefinition().GetVersion())
		}
		t.Log("缺陷 A 仍在：事务外读到的 before.Version 才是 CAS 条件，调用方的 expected_version 不参与裁决")
	})

	// 判别性对照：真正的并发赢家（库里版本被他人推进）会被现有 CAS 拦住。
	t.Run("对照：库版本被抢先推进时 CAS 有效", func(t *testing.T) {
		svcCtx2, db2 := newTestSvc(t)
		registerOK(t, svcCtx2, "report.daily")
		seedDefinition(db2, &model.TaskDefinition{
			TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily",
			ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
			State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
			LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 3,
		})
		db2.casMissOnUpdate = true
		patch := intervalProto(testTaskKey)
		patch.TimeoutSeconds = 900
		_, err := NewUpdateTaskLogic(context.Background(), svcCtx2).
			UpdateTask(updateReq(testTaskKey, 3, patch))
		if !errors.Is(err, model.ErrVersionConflict) {
			t.Errorf("err = %v，期望 ErrVersionConflict（CAS 本身是有效的）", err)
		}
		if got := storedDef(t, db2, testTaskKey); got.TimeoutSeconds != 600 || got.Version != 3 {
			t.Errorf("CAS 未命中却留下了半条结果：%+v", got)
		}
	})
}

// TestUpdateTaskCannotClearOperatorSentinel 哨兵用例。
//
// TODO(缺陷 G)：cron_task_definition.operator 的注释是「最近一次变更操作人」，
// UpdateMutable 也确实带 operator = ?，但 updatetasklogic.go 只把 operatorOf 的结果
// 写进审计，从未赋给 merged.Operator，于是这一列永远停在最后一次注册/状态迁移的人。
// 排障时按定义行看「谁改的」会指错人。
func TestUpdateTaskCannotClearOperatorSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow,
		Version: 1, Operator: "ops.alice",
	})

	if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 1, intervalProto(testTaskKey))); err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.Operator != "ops.bob" {
		t.Logf("缺陷 G 仍在：库里 operator=%q，审计 operator=ops.bob", row.Operator)
	} else {
		t.Errorf("现状已改变（定义行 operator=%q）：请删除本哨兵并同步 README", row.Operator)
	}
	audit := onlyAudit(t, db, model.AuditActionUpdate)
	if audit.Operator != "ops.bob" {
		t.Errorf("审计里的操作人丢了：%+v", audit)
	}
	if reply, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: testTaskKey}); err != nil ||
		reply.GetDefinition().GetOperator() != row.Operator {
		t.Errorf("读侧与库里不一致：%+v / %v", reply.GetDefinition(), err)
	}
}

// TestUpdateTaskMustResendScheduleTypeSentinel 哨兵用例。
//
// TODO(缺陷 F)：mergeDefinition 里 `patch.ScheduleType != model.ScheduleTypeUnspecified`
// 这个守卫永远为真——definitionFromProto 连 full=false 也会拒绝 UNSPECIFIED。
// 后果：任何一次 UpdateTask 都必须重发 schedule_type，
// 「只改一个数值、不碰调度」这种部分更新在契约上做不到。
func TestUpdateTaskMustResendScheduleTypeSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 1,
		NextFireAt: 1_700_000_000,
	})

	patch := &rpc.TaskDefinition{TaskKey: testTaskKey, Name: "只改名字"} // 不声明 schedule_type
	_, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 1, patch))
	if !errors.Is(err, model.ErrInvalidSchedule) {
		t.Errorf("err = %v，期望 ErrInvalidSchedule（当前无法做与调度无关的部分更新）", err)
	}
	if row := storedDef(t, db, testTaskKey); row.Name != "旧名" || row.Version != 1 {
		t.Errorf("被拒的更新改了库里行：%+v", row)
	}

	// 判别性对照：重发同一个 schedule_type 就能改，且调度事实由 merge 保住。
	full := &rpc.TaskDefinition{
		TaskKey: testTaskKey, Name: "只改名字", ScheduleType: rpc.ScheduleType_SCHEDULE_TYPE_INTERVAL,
	}
	if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 1, full)); err != nil {
		t.Fatalf("重发 schedule_type 后应成功：%v", err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.Name != "只改名字" || row.IntervalSeconds != 3600 || row.ScheduleType != model.ScheduleTypeInterval {
		t.Errorf("merge 没保住调度事实：%+v", row)
	}
}

// TestUpdateTaskMustResendTaskKeySentinel 哨兵用例。
//
// TODO(缺陷 H)：updatetasklogic.go:52-57 的注释承诺「请求体里的 definition.task_key
// 允许省略」，但同一函数在第 48 行就调 definitionFromProto，后者（helpers.go:252）
// 对空 task_key 一律回 ErrTaskKeyEmpty。于是「省略」这条路根本走不通，
// 第 54 行的 body != "" 守卫恒真，注释与实现互相矛盾。
func TestUpdateTaskMustResendTaskKeySentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow, Version: 1,
	})

	// 路径参数是权威的，体内省略同一个 key —— 注释说这样合法。
	omit := intervalProto("")
	if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 1, omit)); !errors.Is(err, model.ErrTaskKeyEmpty) {
		if err == nil {
			t.Errorf("现状已改变（省略 definition.task_key 被接受）：请删除本哨兵并同步 README")
		} else {
			t.Errorf("err = %v，期望 ErrTaskKeyEmpty（哨兵钉的就是这条拒绝）", err)
		}
	} else {
		t.Log("缺陷 H 仍在：注释承诺「definition.task_key 允许省略」，实现却先报 ErrTaskKeyEmpty")
	}
	if row := *storedDef(t, db, testTaskKey); row.Version != 1 || row.Name != "旧名" {
		t.Errorf("被拒的更新改了库里行：%+v", row)
	}
	if len(db.audits) != 0 || db.txRuns != 0 {
		t.Errorf("拒绝不该有事务/审计痕迹：txRuns=%d audits=%d", db.txRuns, len(db.audits))
	}

	// 判别性对照：同一个 key 显式重发就通过，说明上面失败的不是「路径参数没给」。
	if _, err := NewUpdateTaskLogic(context.Background(), svcCtx).
		UpdateTask(updateReq(testTaskKey, 1, intervalProto(testTaskKey))); err != nil {
		t.Fatalf("重发一致 task_key 后应成功：%v", err)
	}
}
