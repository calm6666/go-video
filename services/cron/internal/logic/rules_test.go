package logic

// 纯规则层单测：状态机裁决、枚举投影、游标与文本上限。
//
// 这些函数是 cron 的全部「策略」所在：调度循环（第二轮）只是把它们串起来。
// 因此它们必须能在无库、无 gRPC、无 Redis、无 sleep 的条件下被表驱动地钉死。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-video/services/cron/internal/config"
	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"
)

// --- 1. 执行状态机 ---

func TestReportDecisionMatrix(t *testing.T) {
	const now = int64(1_700_000_000)
	cases := []struct {
		name        string
		from        int32
		report      rpc.ReportState
		attempt     int32
		maxAttempts int32
		wantState   int32
		wantRetryAt int64
		wantAttempt int32
		wantErr     error
	}{
		{name: "跑成功", from: model.RunStateRunning, report: rpc.ReportState_REPORT_STATE_SUCCEEDED,
			attempt: 1, maxAttempts: 3, wantState: model.RunStateSucceeded},
		{name: "失败且还有重试机会→退避", from: model.RunStateRunning, report: rpc.ReportState_REPORT_STATE_FAILED,
			attempt: 1, maxAttempts: 3, wantState: model.RunStateRetrying,
			wantRetryAt: now + 30, wantAttempt: 2},
		{name: "失败且重试耗尽→终态", from: model.RunStateRunning, report: rpc.ReportState_REPORT_STATE_FAILED,
			attempt: 3, maxAttempts: 3, wantState: model.RunStateFailed},
		{name: "max_attempts 缺省即单次", from: model.RunStateRunning, report: rpc.ReportState_REPORT_STATE_FAILED,
			attempt: 1, maxAttempts: 0, wantState: model.RunStateFailed},
		{name: "超时", from: model.RunStateRunning, report: rpc.ReportState_REPORT_STATE_TIMEOUT,
			attempt: 1, maxAttempts: 3, wantState: model.RunStateTimeout},
		{name: "取消", from: model.RunStateRunning, report: rpc.ReportState_REPORT_STATE_CANCELED,
			attempt: 1, maxAttempts: 3, wantState: model.RunStateCanceled},
		{name: "跳过", from: model.RunStateRunning, report: rpc.ReportState_REPORT_STATE_SKIPPED,
			attempt: 1, maxAttempts: 3, wantState: model.RunStateSkipped},
		{name: "退避中的行可被下一号承接为 RUNNING", from: model.RunStateRetrying,
			report: rpc.ReportState_REPORT_STATE_SUCCEEDED, attempt: 2, maxAttempts: 3,
			wantState: model.RunStateRetrying, wantErr: model.ErrStateTransition},
		// PENDING 还没 Start，直接报成功等于「没跑却记成功」，必须拒。
		{name: "PENDING 不能直接成功", from: model.RunStatePending, report: rpc.ReportState_REPORT_STATE_SUCCEEDED,
			attempt: 1, maxAttempts: 3, wantErr: model.ErrStateTransition},
		{name: "PENDING 可以判失败", from: model.RunStatePending, report: rpc.ReportState_REPORT_STATE_FAILED,
			attempt: 1, maxAttempts: 1, wantState: model.RunStateFailed},
		// 终态永不被改回：这是「历史轨迹不可篡改」的唯一保证。
		{name: "SUCCEEDED 再报失败", from: model.RunStateSucceeded, report: rpc.ReportState_REPORT_STATE_FAILED,
			attempt: 1, maxAttempts: 3, wantErr: model.ErrStateTransition},
		{name: "FAILED 再报成功", from: model.RunStateFailed, report: rpc.ReportState_REPORT_STATE_SUCCEEDED,
			attempt: 1, maxAttempts: 3, wantErr: model.ErrStateTransition},
		{name: "SKIPPED 再报跳过", from: model.RunStateSkipped, report: rpc.ReportState_REPORT_STATE_SKIPPED,
			attempt: 1, maxAttempts: 3, wantErr: model.ErrStateTransition},
		{name: "未指明的上报状态", from: model.RunStateRunning, report: rpc.ReportState_REPORT_STATE_UNSPECIFIED,
			attempt: 1, maxAttempts: 3, wantErr: model.ErrInvalidFinalState},
		{name: "契约里没有的上报状态", from: model.RunStateRunning, report: rpc.ReportState(99),
			attempt: 1, maxAttempts: 3, wantErr: model.ErrInvalidFinalState},
		{name: "来源状态为 0（脏数据）", from: 0, report: rpc.ReportState_REPORT_STATE_SUCCEEDED,
			attempt: 1, maxAttempts: 3, wantErr: model.ErrStateTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			to, retryAt, nextAttempt, err := reportDecision(tc.from, tc.report, tc.attempt,
				tc.maxAttempts, now, 30, 600)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if to != 0 || retryAt != 0 || nextAttempt != 0 {
					t.Errorf("报错时不得给出结论：to=%d retryAt=%d nextAttempt=%d", to, retryAt, nextAttempt)
				}
				return
			}
			if to != tc.wantState {
				t.Errorf("toState = %s，期望 %s", model.RunStateName(to), model.RunStateName(tc.wantState))
			}
			if retryAt != tc.wantRetryAt {
				t.Errorf("nextRetryAt = %d，期望 %d", retryAt, tc.wantRetryAt)
			}
			if nextAttempt != tc.wantAttempt {
				t.Errorf("nextAttempt = %d，期望 %d", nextAttempt, tc.wantAttempt)
			}
		})
	}
}

// TestReportDecisionBackoffConverges 钉住「退避既指数增长又有天花板」。
// 没有上限的退避会让 max_retry_seconds 形同虚设；上限丢失则一次故障能锁死任务一整天。
func TestReportDecisionBackoffConverges(t *testing.T) {
	const now = int64(1_700_000_000)
	t.Run("按 max_retry_seconds 收敛", func(t *testing.T) {
		want := []int64{30, 60, 120, 240, 300, 300} // 基数 30，上限 300
		for attempt, delta := range want {
			to, retryAt, next, err := reportDecision(model.RunStateRunning,
				rpc.ReportState_REPORT_STATE_FAILED, int32(attempt+1), 100, now, 30, 300)
			if err != nil {
				t.Fatalf("attempt=%d: %v", attempt+1, err)
			}
			if to != model.RunStateRetrying || next != int32(attempt+2) {
				t.Errorf("attempt=%d 应继续退避：%d/%d", attempt+1, to, next)
			}
			if got := retryAt - now; got != delta {
				t.Errorf("attempt=%d 退避 %ds，期望 %ds", attempt+1, got, delta)
			}
		}
	})
	t.Run("未声明上限时落到绝对天花板", func(t *testing.T) {
		_, retryAt, _, err := reportDecision(model.RunStateRunning,
			rpc.ReportState_REPORT_STATE_FAILED, 40, 100, now, 60, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := retryAt - now; got != 86400*7 {
			t.Errorf("退避 %ds，期望硬天花板 %ds", got, 86400*7)
		}
	})
	t.Run("非退避结论绝不带 next_retry_at", func(t *testing.T) {
		for _, st := range []rpc.ReportState{rpc.ReportState_REPORT_STATE_SUCCEEDED,
			rpc.ReportState_REPORT_STATE_TIMEOUT, rpc.ReportState_REPORT_STATE_CANCELED,
			rpc.ReportState_REPORT_STATE_SKIPPED} {
			_, retryAt, next, err := reportDecision(model.RunStateRunning, st, 1, 3, now, 30, 600)
			if err != nil {
				t.Fatalf("%v: %v", st, err)
			}
			if retryAt != 0 || next != 0 {
				t.Errorf("%v 却给出了退避窗口/新重试号：%d/%d", st, retryAt, next)
			}
		}
	})
}

func TestClassifyExistingRun(t *testing.T) {
	const now = int64(1_700_000_000)
	run := func(state int32, owner string, expire int64) *model.TaskRun {
		return &model.TaskRun{ID: 7, TaskKey: testTaskKey, State: state, LeaseOwner: owner,
			FenceToken: 3, LeaseExpireAt: expire}
	}
	cases := []struct {
		name        string
		existing    *model.TaskRun
		wantOutcome rpc.LeaseOutcome
		wantMsg     bool // 是否必须带人读原因
	}{
		{"空行交给上层再判", nil, rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, false},
		{"自己重入拿回原栅栏", run(model.RunStateRunning, "a", now+60),
			rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_CLAIMED, true},
		{"他人持有未过期", run(model.RunStateRunning, "b", now+60),
			rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED, true},
		{"他人持有已过期→可接管", run(model.RunStateRunning, "b", now-1),
			rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, false},
		{"已终结不重复跑", run(model.RunStateSucceeded, "a", 0),
			rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_CLAIMED, true},
		{"失败终态也不重复跑", run(model.RunStateFailed, "b", now-1),
			rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_CLAIMED, true},
		{"退避中未到期", run(model.RunStateRetrying, "b", now+30),
			rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED, true},
		{"退避中但租约时间已清→等新 attempt", run(model.RunStateRetrying, "b", 0),
			rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, false},
		{"PENDING 无人持有可直接接管", run(model.RunStatePending, "", 0),
			rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, false},
		{"PENDING 被他人 claim 未过期", run(model.RunStatePending, "b", now+30),
			rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED, true},
		{"PENDING 被他人 claim 已过期", run(model.RunStatePending, "b", now-30),
			rpc.LeaseOutcome_LEASE_OUTCOME_UNSPECIFIED, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, msg := classifyExistingRun(tc.existing, "a", now)
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %v，期望 %v", outcome, tc.wantOutcome)
			}
			if tc.wantMsg && msg == "" {
				t.Error("拒绝/幂等结论必须带可排障的原因文本")
			}
			if !tc.wantMsg && msg != "" {
				t.Errorf("可以再试的分支不该带文案（会被当成拒绝）：%q", msg)
			}
		})
	}
}

// --- 2. 枚举转换：0 值与越界值 ---

func TestEnumConvertersRejectUnspecifiedAndUnknown(t *testing.T) {
	t.Run("schedule_type 必须显式声明", func(t *testing.T) {
		if _, err := scheduleTypeValue(rpc.ScheduleType_SCHEDULE_TYPE_UNSPECIFIED); !errors.Is(err, model.ErrInvalidSchedule) {
			t.Fatalf("err = %v，期望 ErrInvalidSchedule", err)
		}
		for _, tc := range []struct {
			in   rpc.ScheduleType
			want int32
		}{
			{rpc.ScheduleType_SCHEDULE_TYPE_CRON, model.ScheduleTypeCron},
			{rpc.ScheduleType_SCHEDULE_TYPE_INTERVAL, model.ScheduleTypeInterval},
			{rpc.ScheduleType_SCHEDULE_TYPE_MANUAL, model.ScheduleTypeManual},
			{rpc.ScheduleType(77), 0},
		} {
			got, err := scheduleTypeValue(tc.in)
			if tc.want == 0 {
				if err == nil {
					t.Errorf("schedule_type=%d 应被拒绝", tc.in)
				}
				continue
			}
			if err != nil || got != tc.want {
				t.Errorf("schedule_type=%d -> %d/%v", tc.in, got, err)
			}
		}
	})

	t.Run("misfire 缺省即 FIRE_ONCE_NOW，越界拒", func(t *testing.T) {
		if got, err := misfirePolicyValue(rpc.MisfirePolicy_MISFIRE_POLICY_UNSPECIFIED); err != nil ||
			got != model.MisfirePolicyFireOnceNow {
			t.Errorf("缺省策略 = %d/%v，期望 FIRE_ONCE_NOW", got, err)
		}
		if _, err := misfirePolicyValue(rpc.MisfirePolicy(9)); !errors.Is(err, model.ErrInvalidSchedule) {
			t.Errorf("err = %v，期望 ErrInvalidSchedule", err)
		}
	})

	t.Run("task_state 缺省用调用方默认值", func(t *testing.T) {
		got, err := taskStateValue(rpc.TaskState_TASK_STATE_UNSPECIFIED, model.TaskStatePaused)
		if err != nil || got != model.TaskStatePaused {
			t.Errorf("got=%d err=%v，期望回落到 PAUSED", got, err)
		}
		if _, err := taskStateValue(rpc.TaskState_TASK_STATE_UNSPECIFIED, model.TaskStateUnspecified); err != nil {
			t.Errorf("0 在注册语境由上层兜成 ENABLED，不该在这里报错：%v", err)
		}
		for _, tc := range []struct {
			in   rpc.TaskState
			want int32
		}{
			{rpc.TaskState_TASK_STATE_ENABLED, model.TaskStateEnabled},
			{rpc.TaskState_TASK_STATE_PAUSED, model.TaskStatePaused},
			{rpc.TaskState_TASK_STATE_DISABLED, model.TaskStateDisabled},
		} {
			if got, err := taskStateValue(tc.in, 0); err != nil || got != tc.want {
				t.Errorf("task_state=%d -> %d/%v", tc.in, got, err)
			}
		}
		if _, err := taskStateValue(rpc.TaskState(42), model.TaskStateEnabled); err == nil {
			t.Error("未知 task_state 必须报错，绝不能兜成默认值")
		}
	})

	t.Run("release 的 final_state 只允许两种", func(t *testing.T) {
		for _, ok := range []rpc.RunState{rpc.RunState_RUN_STATE_SKIPPED, rpc.RunState_RUN_STATE_CANCELED} {
			if _, err := runStateValue(ok); err != nil {
				t.Errorf("%v 应合法：%v", ok, err)
			}
		}
		for _, bad := range []rpc.RunState{rpc.RunState_RUN_STATE_UNSPECIFIED, rpc.RunState_RUN_STATE_PENDING,
			rpc.RunState_RUN_STATE_RUNNING, rpc.RunState_RUN_STATE_RETRYING, rpc.RunState_RUN_STATE_SUCCEEDED,
			rpc.RunState_RUN_STATE_FAILED, rpc.RunState_RUN_STATE_TIMEOUT, rpc.RunState(88)} {
			if _, err := runStateValue(bad); !errors.Is(err, model.ErrInvalidFinalState) {
				t.Errorf("final_state=%v err = %v，期望 ErrInvalidFinalState", bad, err)
			}
		}
	})

	t.Run("trigger_type 缺省按调用方语境", func(t *testing.T) {
		if got, err := triggerTypeValue(rpc.TriggerType_TRIGGER_TYPE_UNSPECIFIED, model.TriggerTypeManual); err != nil ||
			got != model.TriggerTypeManual {
			t.Errorf("got=%d err=%v，期望回落到 MANUAL", got, err)
		}
		for _, tc := range []struct {
			in   rpc.TriggerType
			want int32
		}{
			{rpc.TriggerType_TRIGGER_TYPE_SCHEDULED, model.TriggerTypeScheduled},
			{rpc.TriggerType_TRIGGER_TYPE_MANUAL, model.TriggerTypeManual},
			{rpc.TriggerType_TRIGGER_TYPE_RETRY, model.TriggerTypeRetry},
			{rpc.TriggerType_TRIGGER_TYPE_REPLAY, model.TriggerTypeReplay},
		} {
			if got, err := triggerTypeValue(tc.in, 0); err != nil || got != tc.want {
				t.Errorf("trigger_type=%d -> %d/%v", tc.in, got, err)
			}
		}
		if _, err := triggerTypeValue(rpc.TriggerType(66), model.TriggerTypeScheduled); err == nil {
			t.Error("未知 trigger_type 必须报错")
		}
	})
}

// --- 3. 游标与分页 ---

func TestCursorCodecRejectsGarbage(t *testing.T) {
	t.Run("id 游标", func(t *testing.T) {
		if got, err := decodeIDCursor(""); err != nil || got != 0 {
			t.Errorf("空游标应是「从头」：%d/%v", got, err)
		}
		if got, err := decodeIDCursor(" 42 "); err != nil || got != 42 {
			t.Errorf("带空白的合法游标应被接受：%d/%v", got, err)
		}
		for _, bad := range []string{"abc", "0", "-7", "12.5", "99999999999999999999"} {
			if _, err := decodeIDCursor(bad); !errors.Is(err, model.ErrInvalidCursor) {
				t.Errorf("decodeIDCursor(%q) err = %v，期望 ErrInvalidCursor", bad, err)
			}
		}
		if encodeIDCursor(0) != "" || encodeIDCursor(-1) != "" || encodeIDCursor(8) != "8" {
			t.Error("encodeIDCursor 的「无下一页」必须是空串")
		}
	})

	t.Run("复合游标", func(t *testing.T) {
		if got, err := decodeCompositeCursor(""); err != nil || got != "" {
			t.Errorf("空复合游标应是「从头」：%q/%v", got, err)
		}
		ok := model.CompositeCursor("report.daily", "shard=3")
		got, err := decodeCompositeCursor(ok)
		if err != nil || got != ok {
			t.Errorf("合法复合游标必须原样透传：%q/%v", got, err)
		}
		for _, bad := range []string{"没有分隔符", "\x1fshard=1", "   \x1f  "} {
			if _, err := decodeCompositeCursor(bad); !errors.Is(err, model.ErrInvalidCursor) {
				t.Errorf("decodeCompositeCursor(%q) err = %v，期望 ErrInvalidCursor", bad, err)
			}
		}
	})
}

func TestPageSizeConvergesOrRejects(t *testing.T) {
	svcCtx, _ := newTestSvc(t)

	if got, err := svcCtx.PageSize(0); err != nil || got != 20 {
		t.Errorf("未指定时应取服务端默认：got=%d err=%v", got, err)
	}
	if got, err := svcCtx.PageSize(50); err != nil || got != 50 {
		t.Errorf("界内取值不该被改动：got=%d err=%v", got, err)
	}
	// 越界必须报错而不是静默夹到上限：静默放大等于替调用方决定成本。
	if _, err := svcCtx.PageSize(101); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("err = %v，期望 ErrInvalidPageLimit", err)
	}
	if _, err := svcCtx.PageSize(-1); err != nil {
		t.Errorf("负数按「未指定」处理成默认值，得到 %v", err)
	}

	t.Run("配置写得比 model 上限还大时以 model 为准", func(t *testing.T) {
		svcCtx.Config.Task.MaxPageSize = 999999
		defer func() { svcCtx.Config.Task.MaxPageSize = 100 }()
		if got, err := svcCtx.PageSize(model.MaxPageSize); err != nil || got != model.MaxPageSize {
			t.Errorf("got=%d err=%v，期望取满 model 上限 %d", got, err, model.MaxPageSize)
		}
		if _, err := svcCtx.PageSize(model.MaxPageSize + 1); !errors.Is(err, model.ErrInvalidPageLimit) {
			t.Errorf("err = %v，期望 ErrInvalidPageLimit（配置的越界值不能突破代码硬上限）", err)
		}
	})
}

// --- 4. 文本上限与必填校验 ---

func TestCheckTextLimitBoundaries(t *testing.T) {
	if err := checkTextLimit("params", strings.Repeat("a", 100), 100); err != nil {
		t.Errorf("恰好等于上限应放过：%v", err)
	}
	if err := checkTextLimit("params", strings.Repeat("a", 101), 100); !errors.Is(err, model.ErrParamsTooLarge) {
		t.Errorf("err = %v，期望 ErrParamsTooLarge", err)
	}
	// 中文按字节算：列宽是 utf8mb4 的字符数，字节数只会更保守，不会放过超长值。
	if err := checkTextLimit("params", strings.Repeat("字", 40), 100); !errors.Is(err, model.ErrParamsTooLarge) {
		t.Errorf("err = %v，期望按字节判定超限（120 字节 > 100）", err)
	}
	if err := requireIdempotencyKey("   "); !errors.Is(err, model.ErrIdempotencyKeyEmpty) {
		t.Errorf("err = %v，期望 ErrIdempotencyKeyEmpty", err)
	}
	if err := requireReason(""); !errors.Is(err, model.ErrReasonRequired) {
		t.Errorf("err = %v，期望 ErrReasonRequired", err)
	}
	if err := summaryLimit(0); err != model.MaxResultSummaryBytes {
		t.Errorf("配置缺省时 summaryLimit = %d，期望夹到列宽 %d", err, model.MaxResultSummaryBytes)
	}
	if err := summaryLimit(1 << 20); err != model.MaxResultSummaryBytes {
		t.Errorf("配置写得比列宽还大时 = %d，期望 %d", err, model.MaxResultSummaryBytes)
	}
	if err := summaryLimit(500); err != 500 {
		t.Errorf("界内配置应被尊重，得到 %d", err)
	}
}

// TestLeaseKeyLimitCoversComposition 是 lease_key 组合溢出的回归用例：
// task_key 与 scope 各自都没超自己的列宽，拼起来却超出 lease_key VARCHAR(128)。
func TestLeaseKeyLimitCoversComposition(t *testing.T) {
	if err := checkLeaseKeyLimit(strings.Repeat("k", model.MaxTaskKeyBytes),
		strings.Repeat("s", model.MaxScopeBytes)); err != nil {
		t.Errorf("最坏合法组合应被放过：%v", err)
	}
	if err := checkLeaseKeyLimit(strings.Repeat("k", model.MaxTaskKeyBytes),
		strings.Repeat("s", model.MaxScopeBytes+1)); !errors.Is(err, model.ErrParamsTooLarge) {
		t.Errorf("err = %v，期望 ErrParamsTooLarge", err)
	}
	if err := checkLeaseKeyLimit("", ""); err != nil {
		t.Errorf("空 scope 表示任务级全局锁，不该报错：%v", err)
	}
}

func TestDefinitionFromProtoLimitsAndDefaults(t *testing.T) {
	defaults := ServiceDefaults{
		Timezone: "UTC", MaxParamsBytes: model.MaxParamsBytes, DefaultLeaseTTL: 300,
		DefaultBackfillLimit: defaultMisfireBackfillLimit, DefaultTaskGroup: defaultTaskGroupName,
	}
	valid := func() *rpc.TaskDefinition {
		return &rpc.TaskDefinition{
			TaskKey: "rights.expire_scan", Name: "版权到期扫描", Handler: "rights.expire",
			ScheduleType: rpc.ScheduleType_SCHEDULE_TYPE_CRON, CronExpr: "*/10 * * * *",
			MaxAttempts: 3, RetryBaseSeconds: 60, RetryMaxSeconds: 600, ConcurrencyLimit: 1,
			TimeoutSeconds: 600,
		}
	}

	t.Run("注册语境补齐默认值", func(t *testing.T) {
		d, err := defaults.definitionFromProto(valid(), true)
		if err != nil {
			t.Fatalf("注册：%v", err)
		}
		if d.TaskGroup != defaultTaskGroupName || d.Timezone != "UTC" {
			t.Errorf("默认分组/时区未补齐：%+v", d)
		}
		if d.MisfirePolicy != model.MisfirePolicyFireOnceNow || d.MisfireBackfillLim != 5 {
			t.Errorf("misfire 默认值不符：%+v", d)
		}
		if d.State != model.TaskStateEnabled || d.Version != 0 {
			t.Errorf("注册语境状态应为 ENABLED，version 由库里算：%+v", d)
		}
		if d.MaxAttempts != 3 || d.LeaseTTLSeconds != 300 {
			t.Errorf("未声明的 lease TTL 应兜成服务端默认：%+v", d)
		}
	})

	t.Run("更新语境不得改写未声明字段", func(t *testing.T) {
		in := valid()
		in.MisfirePolicy = rpc.MisfirePolicy_MISFIRE_POLICY_UNSPECIFIED
		in.TaskGroup = ""
		d, err := defaults.definitionFromProto(in, false)
		if err != nil {
			t.Fatalf("更新语境：%v", err)
		}
		// 这是「暂停中的任务被 UpdateTask 悄悄改成 FIRE_ONCE_NOW」那类事故的防线。
		if d.MisfirePolicy != model.MisfirePolicyUnspecified {
			t.Errorf("未声明策略必须落 UNSPECIFIED(0) 表示不改，得到 %d", d.MisfirePolicy)
		}
		if d.TaskGroup != "" {
			t.Errorf("更新语境留空必须表示不改，得到 %q", d.TaskGroup)
		}
		if d.Name != "版权到期扫描" {
			t.Errorf("可改字段仍要取请求值：%q", d.Name)
		}
	})

	// 逐列回归：以前只有 params 有门槛，其余字段超长会一路撞到 INSERT 才炸。
	t.Run("逐列长度上限", func(t *testing.T) {
		cases := []struct {
			field string
			set   func(*rpc.TaskDefinition, string)
			max   int
		}{
			{"task_key", func(d *rpc.TaskDefinition, v string) { d.TaskKey = v }, model.MaxTaskKeyBytes},
			{"name", func(d *rpc.TaskDefinition, v string) { d.Name = v }, model.MaxTaskNameBytes},
			{"handler", func(d *rpc.TaskDefinition, v string) { d.Handler = v }, model.MaxHandlerBytes},
			{"task_group", func(d *rpc.TaskDefinition, v string) { d.TaskGroup = v }, model.MaxTaskGroupBytes},
			{"cron_expr", func(d *rpc.TaskDefinition, v string) { d.CronExpr = v }, model.MaxCronExprBytes},
			{"timezone", func(d *rpc.TaskDefinition, v string) { d.Timezone = v }, model.MaxTimezoneBytes},
			{"owner", func(d *rpc.TaskDefinition, v string) { d.Owner = v }, model.MaxOwnerBytes},
			// 曾经的缺陷：secret_refs 用 MaxParamsBytes(4096) 校验，而列宽只有 512。
			{"secret_refs", func(d *rpc.TaskDefinition, v string) { d.SecretRefs = v }, model.MaxSecretRefsBytes},
			{"params", func(d *rpc.TaskDefinition, v string) { d.Params = v }, model.MaxParamsBytes},
		}
		for _, tc := range cases {
			t.Run(tc.field, func(t *testing.T) {
				in := valid()
				tc.set(in, strings.Repeat("x", tc.max+1))
				if _, err := defaults.definitionFromProto(in, true); !errors.Is(err, model.ErrParamsTooLarge) {
					t.Fatalf("%s 超 %d 字节 err = %v，期望 ErrParamsTooLarge", tc.field, tc.max, err)
				}
				in2 := valid()
				tc.set(in2, strings.Repeat("x", tc.max))
				if _, err := defaults.definitionFromProto(in2, true); err != nil {
					t.Errorf("%s 恰好 %d 字节应被放过：%v", tc.field, tc.max, err)
				}
			})
		}
	})

	t.Run("空 task_key 与未知枚举", func(t *testing.T) {
		in := valid()
		in.TaskKey = "   "
		if _, err := defaults.definitionFromProto(in, true); !errors.Is(err, model.ErrTaskKeyEmpty) {
			t.Errorf("err = %v，期望 ErrTaskKeyEmpty", err)
		}
		in = valid()
		in.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_UNSPECIFIED
		if _, err := defaults.definitionFromProto(in, true); !errors.Is(err, model.ErrInvalidSchedule) {
			t.Errorf("err = %v，期望 ErrInvalidSchedule", err)
		}
		if _, err := defaults.definitionFromProto(nil, true); err == nil {
			t.Error("nil 定义必须报错")
		}
	})

	// model.ValidateTaskDefinition 负责「能不能调度」的结构自洽性。
	// 注意：cron_expr 的**语法**解析不在本层（全仓没有 cron parser 依赖，见最终报告），
	// 这里只钉住本层真实执行的规则，不用「顺手少几条断言」换绿灯。
	t.Run("结构自洽性交给 model", func(t *testing.T) {
		in := valid()
		in.CronExpr = ""
		if _, err := defaults.definitionFromProto(in, true); !errors.Is(err, model.ErrInvalidSchedule) {
			t.Errorf("CRON 调度缺表达式 err = %v，期望 ErrInvalidSchedule", err)
		}
		in = valid()
		in.ScheduleType = rpc.ScheduleType_SCHEDULE_TYPE_INTERVAL
		in.CronExpr = ""
		in.IntervalSeconds = 0
		if _, err := defaults.definitionFromProto(in, true); !errors.Is(err, model.ErrInvalidSchedule) {
			t.Errorf("INTERVAL 调度缺周期 err = %v，期望 ErrInvalidSchedule", err)
		}
		in = valid()
		in.LeaseTtlSeconds = model.MaxLeaseTTLSeconds + 1
		if _, err := defaults.definitionFromProto(in, true); !errors.Is(err, model.ErrInvalidLeaseTTL) {
			t.Errorf("err = %v，期望 ErrInvalidLeaseTTL", err)
		}
		in = valid()
		in.TimeoutSeconds = 0
		if _, err := defaults.definitionFromProto(in, true); !errors.Is(err, model.ErrInvalidLeaseTTL) {
			t.Errorf("timeout=0 err = %v，期望 ErrInvalidLeaseTTL", err)
		}
		in = valid()
		in.MaxAttempts = 2
		in.RetryBaseSeconds = 0
		if _, err := defaults.definitionFromProto(in, true); !errors.Is(err, model.ErrInvalidRetryPolicy) {
			t.Errorf("开重试却不给退避基数 err = %v，期望 ErrInvalidRetryPolicy", err)
		}
		in = valid()
		in.ConcurrencyLimit = -1
		if _, err := defaults.definitionFromProto(in, true); err != nil {
			t.Errorf("负数并发应兜底，err = %v", err)
		}
	})

	// 未声明的调度参数由 applyDefinitionDefaults 兜底：把 0 直接写进库会让任务永远跑不起来。
	t.Run("注册语境兜底非正参数", func(t *testing.T) {
		in := valid()
		in.ConcurrencyLimit = 0
		in.MaxAttempts = 0
		in.Name = ""
		d, err := defaults.definitionFromProto(in, true)
		if err != nil {
			t.Fatalf("未声明字段应兜底而不是报错：%v", err)
		}
		if d.ConcurrencyLimit != 1 || d.MaxAttempts != 1 {
			t.Errorf("并发/重试次数未兜成 1：%+v", d)
		}
		if d.Name != in.TaskKey {
			t.Errorf("缺 name 应退化为 task_key，得到 %q", d.Name)
		}
		if d.RetryBaseSeconds != in.RetryBaseSeconds {
			t.Errorf("兜底不该改写调用方显式给出的退避基数：%+v", d)
		}
	})
}

func TestCheckpointFromProtoFieldLimits(t *testing.T) {
	t.Run("逐列长度", func(t *testing.T) {
		cases := []struct {
			field string
			set   func(*rpc.Checkpoint, string)
			max   int
		}{
			{"scope_key", func(c *rpc.Checkpoint, v string) { c.ScopeKey = v }, model.MaxScopeKeyBytes},
			// 曾经的缺陷：value_str 用 MaxParamsBytes(4096) 校验，而列宽只有 255。
			{"value_str", func(c *rpc.Checkpoint, v string) { c.ValueStr = v }, model.MaxValueStrBytes},
			{"operator", func(c *rpc.Checkpoint, v string) { c.Operator = v }, model.MaxOperatorBytes},
		}
		for _, tc := range cases {
			t.Run(tc.field, func(t *testing.T) {
				in := &rpc.Checkpoint{TaskKey: testTaskKey}
				tc.set(in, strings.Repeat("x", tc.max+1))
				if _, err := checkpointFromProto(in); !errors.Is(err, model.ErrParamsTooLarge) {
					t.Fatalf("err = %v，期望 ErrParamsTooLarge", err)
				}
			})
		}
	})
	t.Run("task_key 必填", func(t *testing.T) {
		if _, err := checkpointFromProto(nil); !errors.Is(err, model.ErrTaskKeyEmpty) {
			t.Errorf("err = %v，期望 ErrTaskKeyEmpty", err)
		}
		if _, err := checkpointFromProto(&rpc.Checkpoint{TaskKey: " "}); !errors.Is(err, model.ErrTaskKeyEmpty) {
			t.Errorf("err = %v，期望 ErrTaskKeyEmpty", err)
		}
		c, err := checkpointFromProto(&rpc.Checkpoint{TaskKey: " report.daily ", Value: 7})
		if err != nil || c.TaskKey != "report.daily" {
			t.Errorf("两侧空白应被清掉：%+v %v", c, err)
		}
	})
}

func TestMergeDefinitionKeepsServerSideFields(t *testing.T) {
	base := &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "旧名", Handler: "report.daily", TaskGroup: "report",
		ScheduleType: model.ScheduleTypeCron, CronExpr: "0 3 * * *", Timezone: "UTC",
		MaxAttempts: 3, ConcurrencyLimit: 2, RetryBaseSeconds: 60, RetryMaxSeconds: 600,
		MisfirePolicy: model.MisfirePolicyFireAll, MisfireBackfillLim: 5, State: model.TaskStatePaused,
		Version: 7, NextFireAt: 123, Params: `{"a":1}`, SecretRefs: "TOKEN_A",
	}

	t.Run("漏传字段不得清空既有配置", func(t *testing.T) {
		merged := mergeDefinition(base, &model.TaskDefinition{TaskKey: "被忽略"})
		if merged.Handler != "report.daily" || merged.Name != "旧名" || merged.TaskGroup != "report" {
			t.Errorf("空串必须表示「不改」：%+v", merged)
		}
		if merged.CronExpr != "0 3 * * *" || merged.MaxAttempts != 3 || merged.ConcurrencyLimit != 2 {
			t.Errorf("数值 0 表示「不改」：%+v", merged)
		}
		if merged.MisfirePolicy != model.MisfirePolicyFireAll {
			t.Error("未声明的 misfire 策略绝不降级")
		}
		// params/secret_refs 是显式允许清空的例外。
		if merged.Params != "" || merged.SecretRefs != "" {
			t.Errorf("params/secret_refs 空串表示清空，得到 %q/%q", merged.Params, merged.SecretRefs)
		}
		// 状态与幂等身份永远以服务端为准。
		if merged.State != model.TaskStatePaused || merged.Version != 7 || merged.NextFireAt != 123 {
			t.Errorf("后台不得借 UpdateTask 改状态/版本/指针：%+v", merged)
		}
		if merged.TaskKey != testTaskKey {
			t.Errorf("task_key 不可改，得到 %q", merged.TaskKey)
		}
	})

	t.Run("显式改小退避参数生效", func(t *testing.T) {
		merged := mergeDefinition(base, &model.TaskDefinition{
			RetryBaseSeconds: 10, RetryMaxSeconds: 30, TimeoutSeconds: 60,
		})
		if merged.RetryBaseSeconds != 10 || merged.RetryMaxSeconds != 30 || merged.TimeoutSeconds != 60 {
			t.Errorf("界内新值未生效：%+v", merged)
		}
	})

	t.Run("换调度方式时旧表达式让位", func(t *testing.T) {
		merged := mergeDefinition(base, &model.TaskDefinition{
			ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 300,
		})
		if merged.ScheduleType != model.ScheduleTypeInterval || merged.IntervalSeconds != 300 {
			t.Fatalf("调度方式未切换：%+v", merged)
		}
		if merged.CronExpr != "" {
			t.Errorf("切到 interval 后必须丢掉 cron_expr，得到 %q", merged.CronExpr)
		}
	})

	t.Run("同调度方式下 cron_expr 空表示不改", func(t *testing.T) {
		merged := mergeDefinition(base, &model.TaskDefinition{
			ScheduleType: model.ScheduleTypeCron, Name: "新名",
		})
		if merged.CronExpr != "0 3 * * *" || merged.Name != "新名" {
			t.Errorf("%+v", merged)
		}
	})
}

func TestPlanResumeFireAtPerMisfirePolicy(t *testing.T) {
	const now = int64(1_700_000_000)
	defaults := ServiceDefaults{Timezone: "UTC"}
	interval := func(policy int32, lastFire int64) *model.TaskDefinition {
		return &model.TaskDefinition{
			TaskKey: testTaskKey, ScheduleType: model.ScheduleTypeInterval,
			IntervalSeconds: 3600, MisfirePolicy: policy, LastFireAt: lastFire,
		}
	}

	t.Run("暂停期跨过一个点：FIRE_ONCE_NOW 合并成一次", func(t *testing.T) {
		got, err := planResumeFireAt(interval(model.MisfirePolicyFireOnceNow, now-7200), defaults, now)
		if err != nil || got != now {
			t.Errorf("got=%d err=%v，期望立即补跑一次", got, err)
		}
	})
	t.Run("FIRE_ALL 指向最早未跑点", func(t *testing.T) {
		got, err := planResumeFireAt(interval(model.MisfirePolicyFireAll, now-7200), defaults, now)
		if err != nil || got != now-3600 {
			t.Errorf("got=%d err=%v，期望指向最早未跑点", got, err)
		}
	})
	t.Run("SKIP_TO_NEXT 丢弃过期点", func(t *testing.T) {
		got, err := planResumeFireAt(interval(model.MisfirePolicySkipToNext, now-7200), defaults, now)
		if err != nil || got != now+3600 {
			t.Errorf("got=%d err=%v，期望 now 之后的第一个点", got, err)
		}
	})
	t.Run("计划点仍在未来则按原计划", func(t *testing.T) {
		for _, policy := range []int32{model.MisfirePolicyFireAll, model.MisfirePolicyFireOnceNow} {
			got, err := planResumeFireAt(interval(policy, now-60), defaults, now)
			if err != nil || got != now+3540 {
				t.Errorf("policy=%d got=%d err=%v，期望不被 misfire 策略改写", policy, got, err)
			}
		}
	})
	t.Run("从未跑过用当前时刻回溯", func(t *testing.T) {
		got, err := planResumeFireAt(interval(model.MisfirePolicyFireAll, 0), defaults, now)
		if err != nil || got != now+3599 {
			t.Errorf("got=%d err=%v，期望 anchor 取 now-1", got, err)
		}
	})
	t.Run("手动任务没有指针", func(t *testing.T) {
		got, err := planResumeFireAt(&model.TaskDefinition{TaskKey: testTaskKey,
			ScheduleType: model.ScheduleTypeManual}, defaults, now)
		if err != nil || got != 0 {
			t.Errorf("got=%d err=%v，期望 0", got, err)
		}
	})
	t.Run("脏 misfire 值必须报错", func(t *testing.T) {
		if _, err := planResumeFireAt(interval(9, now-7200), defaults, now); !errors.Is(err, model.ErrInvalidSchedule) {
			t.Errorf("err = %v，期望 ErrInvalidSchedule", err)
		}
		if _, err := planResumeFireAt(nil, defaults, now); !errors.Is(err, model.ErrTaskNotFound) {
			t.Errorf("err = %v，期望 ErrTaskNotFound", err)
		}
	})
}

// --- 5. 其余装配期小规则 ---

func TestDefaultsOfClampsBrokenConfig(t *testing.T) {
	svcCtx, _ := newTestSvc(t)
	svcCtx.Config = config.Config{} // 全零：模拟「配置里忘了写」
	got := defaultsOf(svcCtx)
	if got.MaxParamsBytes != model.MaxParamsBytes {
		t.Errorf("params 上限 = %d，期望兜到 %d", got.MaxParamsBytes, model.MaxParamsBytes)
	}
	if got.Timezone != "UTC" {
		t.Errorf("时区 = %q，期望兜到 UTC", got.Timezone)
	}
	if got.DefaultLeaseTTL != model.MinLeaseTTLSeconds {
		t.Errorf("默认 TTL = %d，期望兜到下限 %d", got.DefaultLeaseTTL, model.MinLeaseTTLSeconds)
	}
	if got.DefaultTaskGroup != "default" || got.DefaultBackfillLimit != defaultMisfireBackfillLimit {
		t.Errorf("默认分组/补齐上限不符：%+v", got)
	}
	if got.PreemptionEnabled {
		t.Error("零值配置下抢占必须是关的（默认开由 unmarshal 的 default 标签负责）")
	}

	svcCtx2, _ := newTestSvc(t)
	svcCtx2.Config.Task.MaxParamsBytes = 1 << 30
	if got := defaultsOf(svcCtx2); got.MaxParamsBytes != model.MaxParamsBytes {
		t.Errorf("配置写飞时 params 上限 = %d，期望夹到 %d", got.MaxParamsBytes, model.MaxParamsBytes)
	}
	svcCtx2.Config.Task.DefaultTimezone = "Asia/Shanghai"
	if got := defaultsOf(svcCtx2); got.Timezone != "Asia/Shanghai" {
		t.Errorf("显式时区应被尊重，得到 %q", got.Timezone)
	}
}

func TestAuditDetailIsStableAndSafe(t *testing.T) {
	if got := auditDetail(nil); got != "" {
		t.Errorf("空字段不该写出 {}，得到 %q", got)
	}
	got := auditDetail(map[string]any{
		"reason": "版权到期", "to_state": 2, "z": nil,
	})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("必须是合法 JSON：%q err=%v", got, err)
	}
	if len(parsed) != 3 || parsed["reason"] != "版权到期" {
		t.Errorf("内容不符：%+v", parsed)
	}
	// key 升序：审计文本要能直接 diff，不能每次跑出一个新顺序。
	if !strings.HasPrefix(got, `{"reason":`) || !strings.HasSuffix(got, `"z":null}`) {
		t.Errorf("字段未按稳定顺序输出：%q", got)
	}
	if strings.Contains(auditDetail(map[string]any{"secret": make(chan int)}), "chan") {
		t.Error("不可序列化字段必须退化为占位文本")
	}
}

func TestSmallHelpers(t *testing.T) {
	t.Run("durationMs 只信服务端时钟", func(t *testing.T) {
		if durationMs(nil, 100) != 0 {
			t.Error("nil 行应为 0")
		}
		if durationMs(&model.TaskRun{}, 100) != 0 {
			t.Error("未开始（PENDING）应为 0，不能算成 now")
		}
		if got := durationMs(&model.TaskRun{StartedAt: 1000}, 999); got != 0 {
			t.Errorf("时钟回拨时得到 %d，期望 0", got)
		}
		if got := durationMs(&model.TaskRun{StartedAt: 1000}, 1042); got != 42_000 {
			t.Errorf("got=%d，期望 42000ms", got)
		}
	})

	t.Run("stateAllowed", func(t *testing.T) {
		allowed := []int32{model.TaskStateEnabled}
		if !stateAllowed(allowed, model.TaskStateEnabled) {
			t.Error("集合内状态必须放行")
		}
		if stateAllowed(allowed, model.TaskStateDisabled) || stateAllowed(nil, model.TaskStateEnabled) ||
			stateAllowed(allowed, model.TaskStateUnspecified) {
			t.Error("集合外状态（含 0 值）必须拒绝")
		}
	})

	t.Run("notRunnableReply 不伪造领取成功", func(t *testing.T) {
		reply := notRunnableReply("没有定义")
		if reply.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_NOT_RUNNABLE || reply.GetMessage() != "没有定义" {
			t.Errorf("%+v", reply)
		}
		if reply.GetRunId() != 0 || reply.GetFenceToken() != 0 || reply.GetLeaseExpireAt() != 0 {
			t.Errorf("NOT_RUNNABLE 却带了租约字段：%+v", reply)
		}
	})

	t.Run("firstNonEmpty 与 triggerTypeName", func(t *testing.T) {
		if got := firstNonEmpty(" ", "", "b", "c"); got != "b" {
			t.Errorf("got=%q", got)
		}
		if got := firstNonEmpty(" ", ""); got != "" {
			t.Errorf("got=%q", got)
		}
		for _, tc := range []struct {
			v    int32
			want string
		}{
			{model.TriggerTypeScheduled, "SCHEDULED"}, {model.TriggerTypeManual, "MANUAL"},
			{model.TriggerTypeRetry, "RETRY"}, {model.TriggerTypeReplay, "REPLAY"}, {0, "UNKNOWN(0)"},
		} {
			if got := triggerTypeName(tc.v); got != tc.want {
				t.Errorf("triggerTypeName(%d) = %q，期望 %q", tc.v, got, tc.want)
			}
		}
	})

	t.Run("scheduleSummary 覆盖三种调度", func(t *testing.T) {
		cases := []struct {
			d    *model.TaskDefinition
			want string
		}{
			{&model.TaskDefinition{ScheduleType: model.ScheduleTypeCron, CronExpr: "0 3 * * *", Timezone: "UTC"},
				`cron("0 3 * * *", tz=UTC)`},
			{&model.TaskDefinition{ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 60, Timezone: "UTC"},
				"interval(60s, tz=UTC)"},
			{&model.TaskDefinition{ScheduleType: model.ScheduleTypeManual}, "manual"},
			{&model.TaskDefinition{ScheduleType: 42}, "unknown(42)"},
		}
		for _, tc := range cases {
			if got := scheduleSummary(tc.d); got != tc.want {
				t.Errorf("got=%q，期望 %q", got, tc.want)
			}
		}
	})

	t.Run("orDefaultTTL 只在未声明时兜底", func(t *testing.T) {
		if got := orDefaultTTL(0, 120); got != 120 {
			t.Errorf("got=%d", got)
		}
		if got := orDefaultTTL(60, 120); got != 60 {
			t.Errorf("got=%d", got)
		}
		if got := orDefaultTTL(-1, 120); got != 120 {
			t.Errorf("负数按未声明处理，得到 %d", got)
		}
	})

	t.Run("splitScopeFromLeaseKey", func(t *testing.T) {
		taskKey, scope := splitScopeFromLeaseKey("report.daily/shard=3")
		if taskKey != "report.daily" || scope != "shard=3" {
			t.Errorf("got=%q/%q", taskKey, scope)
		}
		if taskKey, scope := splitScopeFromLeaseKey("report.daily"); taskKey != "report.daily" || scope != "" {
			t.Errorf("无分片时 got=%q/%q", taskKey, scope)
		}
		if taskKey, scope := splitScopeFromLeaseKey(leaseKeyOf("a.b", "s=1")); taskKey != "a.b" || scope != "s=1" {
			t.Errorf("往返不符 got=%q/%q", taskKey, scope)
		}
	})
}

// 钉住测试脚手架的装配类型：newTestSvc 必须返回真实的 *svc.ServiceContext，
// 否则 defaultsOf/LeaseTTL/PageSize 这些读配置的纯函数就不在被测路径上。
var _ func(*testing.T) (*svc.ServiceContext, *fakeDB) = newTestSvc
