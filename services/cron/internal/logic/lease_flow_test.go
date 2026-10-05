package logic

// 租约与栅栏：本文件在无 MySQL 的前提下验证 cron 并发正确性的**裁决逻辑**。
//
// 覆盖的是「logic 拿到 model 的返回值之后判断得对不对」：
//   - 同一计划时刻重复领取必须幂等（ALREADY_CLAIMED / ALREADY_OWNED），绝不产生第二行；
//   - 只有过期租约才能被接管，接管必须递增 fence_token，旧实例随后一律 ErrLeaseLost；
//   - 并发上限命中要留下 SKIPPED 行而不是静默丢计划点；
//   - 重试号只能由服务端产生（attempt 必须紧跟 RETRYING 行且退避已到期）；
//   - 结果与游标同事务：游标 CAS 冲突时执行记录必须整体回滚。
//
// 未覆盖并明说（见交付报告）：MySQL 真实行锁 / uniq 键的并发行为、以及
// AcquireLease 经 Registry 的 happy path（注册表无法从包外注入 handler）。

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"go-video/services/cron/internal/registry"
	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

func TestMain(m *testing.M) {
	// 被测路径会刻意打错误日志（栅栏丢失、事务回滚），关掉输出只看断言结果。
	logx.Disable()
	os.Exit(m.Run())
}

const testTaskKey = "report.daily_scan"

func enabledDef(db *fakeDB) *model.TaskDefinition {
	return seedDefinition(db, &model.TaskDefinition{
		TaskKey:          testTaskKey,
		Name:             "日报扫描",
		Handler:          "report.daily",
		TaskGroup:        "report",
		ScheduleType:     model.ScheduleTypeManual,
		State:            model.TaskStateEnabled,
		Timezone:         "UTC",
		MaxAttempts:      3,
		RetryBaseSeconds: 30,
		RetryMaxSeconds:  600,
		LeaseTTLSeconds:  120,
		ConcurrencyLimit: 1,
		Version:          1,
	})
}

func acquireReq(owner string, plannedAt int64) *rpc.AcquireLeaseReq {
	return &rpc.AcquireLeaseReq{
		TaskKey: testTaskKey, PlannedAt: plannedAt, Attempt: 1,
		Owner: owner, TriggerType: rpc.TriggerType_TRIGGER_TYPE_SCHEDULED,
	}
}

// claim 直接驱动事务内核 claimInTx：入口 AcquireLease 还包含「读定义 + 注册表校验」，
// 那部分由 TestAcquireLease* 系列单独覆盖（注册表目前无法从包外注入 handler，见交付报告）。
func claim(t *testing.T, svcCtx *svc.ServiceContext, in *rpc.AcquireLeaseReq,
	def *model.TaskDefinition) (*rpc.AcquireLeaseReply, error) {
	t.Helper()
	l := NewAcquireLeaseLogic(context.Background(), svcCtx)
	res := &acquireResult{}
	err := l.claimInTx(context.Background(), nil, in, def,
		model.TriggerTypeScheduled, int64(def.LeaseTTLSeconds), strings.TrimSpace(in.Scope), res)
	if err != nil {
		return nil, err
	}
	return res.reply, nil
}

// --- 入口校验：必须在碰库之前拒绝 ---

func TestAcquireLeaseRejectsBeforeTouchingDatabase(t *testing.T) {
	long := func(n int) string { return strings.Repeat("k", n) }
	cases := []struct {
		name    string
		req     *rpc.AcquireLeaseReq
		wantErr error
	}{
		{"空请求", nil, model.ErrTaskKeyEmpty},
		{"无 task_key", &rpc.AcquireLeaseReq{PlannedAt: 1, Owner: "a"}, model.ErrTaskKeyEmpty},
		{"无 owner", &rpc.AcquireLeaseReq{TaskKey: testTaskKey, PlannedAt: 1}, model.ErrLeaseOwnerRequired},
		{"无计划时刻", &rpc.AcquireLeaseReq{TaskKey: testTaskKey, Owner: "a"}, model.ErrPlannedAtRequired},
		{"负 attempt", &rpc.AcquireLeaseReq{TaskKey: testTaskKey, Owner: "a", PlannedAt: 1, Attempt: -1},
			model.ErrAttemptNotRetryable},
		{"task_key 超列宽", &rpc.AcquireLeaseReq{TaskKey: long(65), Owner: "a", PlannedAt: 1}, model.ErrParamsTooLarge},
		// scope 单看没超自己的列宽（VARCHAR(64)），但拼上 task_key 会超出 lease_key VARCHAR(128)。
		{"scope 组合后超列宽", &rpc.AcquireLeaseReq{TaskKey: long(64), Owner: "a", PlannedAt: 1,
			Scope: long(64)}, model.ErrParamsTooLarge},
		{"owner 超 operator 列宽", &rpc.AcquireLeaseReq{TaskKey: testTaskKey, Owner: long(65),
			PlannedAt: 1}, model.ErrParamsTooLarge},
		{"trace_id 超列宽", &rpc.AcquireLeaseReq{TaskKey: testTaskKey, Owner: "a", PlannedAt: 1,
			TraceId: long(65)}, model.ErrParamsTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			svcCtx.Registry = registry.New()
			enabledDef(db)

			_, err := NewAcquireLeaseLogic(context.Background(), svcCtx).AcquireLease(tc.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("AcquireLease() err = %v，期望 %v", err, tc.wantErr)
			}
			if db.txRuns != 0 || len(db.runs) != 0 || len(db.leases) != 0 {
				t.Errorf("非法请求必须在进事务前拒绝：txRuns=%d runs=%d leases=%d",
					db.txRuns, len(db.runs), len(db.leases))
			}
		})
	}
}

func TestAcquireLeaseNotRunnableWhenHandlerMissing(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Registry = registry.New() // 本进程没有任何 handler 实现
	enabledDef(db)

	reply, err := NewAcquireLeaseLogic(context.Background(), svcCtx).AcquireLease(
		acquireReq("inst-a", fakeNow()))
	if err != nil {
		t.Fatalf("NOT_RUNNABLE 不是错误，得是结论：%v", err)
	}
	if reply.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_NOT_RUNNABLE {
		t.Errorf("outcome = %v，期望 NOT_RUNNABLE", reply.GetOutcome())
	}
	if !strings.Contains(reply.GetMessage(), "report.daily") {
		t.Errorf("拒绝原因必须点出缺哪个 handler，得到 %q", reply.GetMessage())
	}
	if reply.GetRunId() != 0 || reply.GetFenceToken() != 0 {
		t.Errorf("未取得锁却回填了 run_id/fence：%+v", reply)
	}
	if len(db.runs) != 0 {
		t.Errorf("不可跑的任务不得留下执行记录，得到 %d 行", len(db.runs))
	}
}

func TestAcquireLeaseMissingDefinitionIsNotRunnable(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Registry = registry.New()

	reply, err := NewAcquireLeaseLogic(context.Background(), svcCtx).AcquireLease(
		acquireReq("inst-a", fakeNow()))
	if err != nil || reply.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_NOT_RUNNABLE {
		t.Fatalf("reply=%+v err=%v，期望 NOT_RUNNABLE 且无错误", reply, err)
	}
	if !strings.Contains(reply.GetMessage(), "没有任务定义") {
		t.Errorf("消息要说清「调度事实源缺失」，得到 %q", reply.GetMessage())
	}
	if len(db.runs) != 0 {
		t.Errorf("没有定义就不该有执行记录，得到 %d 行", len(db.runs))
	}
}

// --- 领取内核 ---

func TestClaimFirstAttemptAcquiresLeaseAndAdvancesPointer(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	planned := (fakeNow() / 300) * 300
	def := seedDefinition(db, &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "日报扫描", Handler: "report.daily", TaskGroup: "report",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 300, NextFireAt: planned,
		State: model.TaskStateEnabled, Timezone: "UTC", MaxAttempts: 3,
		RetryBaseSeconds: 30, RetryMaxSeconds: 600, LeaseTTLSeconds: 120, ConcurrencyLimit: 1, Version: 1,
	})

	reply, err := claim(t, svcCtx, acquireReq("inst-a", planned), def)
	if err != nil {
		t.Fatalf("claimInTx: %v", err)
	}
	if reply.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ACQUIRED {
		t.Fatalf("outcome = %v，期望 ACQUIRED（message=%q）", reply.GetOutcome(), reply.GetMessage())
	}
	if reply.GetFenceToken() != 1 {
		t.Errorf("首次取得的 fence_token 必须从 1 起，得到 %d", reply.GetFenceToken())
	}
	if reply.GetLeaseExpireAt() <= fakeNow() {
		t.Errorf("lease_expire_at=%d 必须晚于现在", reply.GetLeaseExpireAt())
	}

	run := db.runs[reply.GetRunId()]
	if run.State != model.RunStateRunning || run.LeaseOwner != "inst-a" || run.FenceToken != 1 {
		t.Errorf("执行记录未按「已持有」落库：%+v", run)
	}
	if run.LeaseExpireAt != reply.GetLeaseExpireAt() {
		t.Errorf("两处过期时间必须一致：run=%d lease=%d", run.LeaseExpireAt, reply.GetLeaseExpireAt())
	}
	lease := db.leases[testTaskKey]
	if lease == nil || lease.RunID != run.ID || lease.TakeoverCount != 0 {
		t.Errorf("任务级租约装配错误：%+v", lease)
	}
	// 领取的正是当前计划点 → 指针必须前进到下一个 5 分钟边界，且记录 last_fire_at。
	if got := db.defs[testTaskKey]; got.LastFireAt != planned || got.NextFireAt != planned+300 {
		t.Errorf("调度指针未正确推进：last=%d next=%d，期望 last=%d next=%d",
			got.LastFireAt, got.NextFireAt, planned, planned+300)
	}
}

func TestClaimSamePlanPointIsIdempotent(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	def := enabledDef(db)
	planned := fakeNow() - 60

	first, err := claim(t, svcCtx, acquireReq("inst-a", planned), def)
	if err != nil {
		t.Fatalf("首次领取：%v", err)
	}
	// 同一实例重入：必须拿回原栅栏令牌继续心跳，而不是第二个 run。
	again, err := claim(t, svcCtx, acquireReq("inst-a", planned), def)
	if err != nil {
		t.Fatalf("同实例重入：%v", err)
	}
	if again.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_CLAIMED {
		t.Errorf("同实例重入 outcome = %v，期望 ALREADY_CLAIMED", again.GetOutcome())
	}
	if again.GetRunId() != first.GetRunId() || again.GetFenceToken() != first.GetFenceToken() {
		t.Errorf("重入必须回到同一行同一栅栏：%+v vs %+v", again, first)
	}
	// 他人持有且未过期：拒绝，且绝不改动库里的持有关系。
	other, err := claim(t, svcCtx, acquireReq("inst-b", planned), def)
	if err != nil {
		t.Fatalf("他人领取：%v", err)
	}
	if other.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED {
		t.Errorf("他人持有未过期 outcome = %v，期望 ALREADY_OWNED", other.GetOutcome())
	}
	if len(db.runs) != 1 {
		t.Errorf("同一计划时刻只允许一行，得到 %d 行", len(db.runs))
	}
	if run := db.runs[first.GetRunId()]; run.LeaseOwner != "inst-a" || run.FenceToken != 1 {
		t.Errorf("被拒的领取改写了持有关系：%+v", run)
	}
	if lease := db.leases[testTaskKey]; lease.FenceToken != 1 || lease.TakeoverCount != 0 {
		t.Errorf("未发生抢占却动了栅栏：%+v", lease)
	}
}

func TestClaimTakeoverOnlyAfterExpiryAndBumpsFence(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	def := enabledDef(db)
	planned := fakeNow() - 600

	first, err := claim(t, svcCtx, acquireReq("inst-a", planned), def)
	if err != nil {
		t.Fatalf("首次领取：%v", err)
	}
	runID := first.GetRunId()

	// 未过期就抢：拿不到锁，且旧实例的栅栏不受影响。
	if _, err := claim(t, svcCtx, acquireReq("inst-b", planned), def); err != nil {
		t.Fatalf("未过期抢占：%v", err)
	}
	if run := db.runs[runID]; run.LeaseOwner != "inst-a" {
		t.Fatalf("未过期的租约被顶掉了：%+v", run)
	}

	// 模拟持有者崩溃：两处过期时间同时落到过去。
	expire := fakeNow() - 1
	db.runs[runID].LeaseExpireAt = expire
	db.leases[testTaskKey].ExpireAt = expire

	second, err := claim(t, svcCtx, acquireReq("inst-b", planned), def)
	if err != nil {
		t.Fatalf("过期后接管：%v", err)
	}
	if second.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ACQUIRED || second.GetRunId() != runID {
		t.Fatalf("接管必须就地复用同一行并给出 ACQUIRED：%+v", second)
	}
	if second.GetFenceToken() != 2 {
		t.Errorf("接管后 fence_token 应为 2（单调递增不回退），得到 %d", second.GetFenceToken())
	}
	if lease := db.leases[testTaskKey]; lease.TakeoverCount != 1 || lease.OwnerInstance != "inst-b" {
		t.Errorf("接管计数/持有者不符：%+v", lease)
	}

	// 决定性证据：旧实例带着旧栅栏上报，必须被栅栏挡下。
	_, err = NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
		&rpc.ReportTaskResultReq{RunId: runID, Owner: "inst-a", FenceToken: 1,
			State: rpc.ReportState_REPORT_STATE_SUCCEEDED})
	if !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("被顶掉的旧实例上报 err = %v，期望 ErrLeaseLost", err)
	}
	if run := db.runs[runID]; run.State != model.RunStateRunning {
		t.Errorf("旧实例的上报不该改动执行状态：%+v", run)
	}
	// 新实例带着新栅栏上报才有效。
	reply, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
		&rpc.ReportTaskResultReq{RunId: runID, Owner: "inst-b", FenceToken: 2,
			State: rpc.ReportState_REPORT_STATE_SUCCEEDED})
	if err != nil {
		t.Fatalf("接管者上报：%v", err)
	}
	if reply.GetRun().GetState() != rpc.RunState_RUN_STATE_SUCCEEDED {
		t.Errorf("上报后状态 = %v，期望 SUCCEEDED", reply.GetRun().GetState())
	}
}

func TestClaimConcurrencyLimitRecordsSkippedRun(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	def := enabledDef(db)
	def.ConcurrencyLimit = 1
	// 已经有一个在跑的执行（不同计划时刻）。
	seedRunning(db, 900, testTaskKey, "inst-busy", 1, fakeNow()+3600)

	reply, err := claim(t, svcCtx, acquireReq("inst-a", fakeNow()), def)
	if err != nil {
		t.Fatalf("并发闸门不该报错：%v", err)
	}
	if reply.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_CONCURRENCY_LIMIT {
		t.Fatalf("outcome = %v，期望 CONCURRENCY_LIMIT", reply.GetOutcome())
	}
	if !strings.Contains(reply.GetMessage(), "并发上限") {
		t.Errorf("消息要能回答为什么没跑，得到 %q", reply.GetMessage())
	}
	run := db.runs[reply.GetRunId()]
	if run.State != model.RunStateSkipped {
		t.Errorf("被闸门挡下的计划点必须留下 SKIPPED 行（不许静默丢弃），得到 %s",
			model.RunStateName(run.State))
	}
	if run.LastError == "" {
		t.Error("SKIPPED 行必须带原因")
	}
	if _, ok := db.leases[testTaskKey]; ok {
		t.Error("并发闸门命中时不得抢下任务级租约")
	}
}

func TestClaimRetryAttemptMustBeIssuedByServer(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	def := enabledDef(db)
	planned := fakeNow() - 900

	t.Run("上一号不是 RETRYING 直接拒", func(t *testing.T) {
		req := acquireReq("inst-a", planned)
		req.Attempt = 2
		if _, err := claim(t, svcCtx, req, def); !errors.Is(err, model.ErrAttemptNotRetryable) {
			t.Fatalf("err = %v，期望 ErrAttemptNotRetryable", err)
		}
		if len(db.runs) != 0 {
			t.Errorf("被拒的重试不该建行，得到 %d 行", len(db.runs))
		}
	})

	t.Run("退避未到期拒", func(t *testing.T) {
		seedRunning(db, 910, testTaskKey, "inst-a", 1, 0)
		prev := db.runs[910] // 直接改库里的行：seedRunning 的返回值是快照，改它没有效果
		prev.State = model.RunStateRetrying
		prev.TriggerType = model.TriggerTypeRetry
		prev.PlannedAt = planned
		prev.NextRetryAt = fakeNow() + 600
		req := acquireReq("inst-b", planned)
		req.Attempt = 2
		if _, err := claim(t, svcCtx, req, def); !errors.Is(err, model.ErrRetryNotDue) {
			t.Fatalf("err = %v，期望 ErrRetryNotDue", err)
		}
	})

	t.Run("退避到期则新 attempt 行按 RETRY 来源领取", func(t *testing.T) {
		db.runs[910].NextRetryAt = fakeNow() - 1
		req := acquireReq("inst-b", planned)
		req.Attempt = 2
		reply, err := claim(t, svcCtx, req, def)
		if err != nil {
			t.Fatalf("退避到期后应可领取：%v", err)
		}
		if reply.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ACQUIRED {
			t.Fatalf("outcome = %v，期望 ACQUIRED", reply.GetOutcome())
		}
		run := db.runs[reply.GetRunId()]
		if run.Attempt != 2 || run.TriggerType != model.TriggerTypeRetry {
			t.Errorf("新 attempt 行不符：%+v", run)
		}
		if run.FenceToken != 1 {
			t.Errorf("这是一次全新的 attempt（新行），栅栏应从 1 起，得到 %d", run.FenceToken)
		}
		if prev := db.runs[910]; prev.State != model.RunStateRetrying {
			t.Errorf("上一号 RETRYING 行必须原样保留（历史轨迹不可篡改）：%+v", prev)
		}
	})
}

func TestClaimRetryingRowCannotBeTakenOverInPlace(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	def := enabledDef(db)
	planned := fakeNow() - 300
	seedRunning(db, 920, testTaskKey, "inst-a", 3, 0)
	db.runs[920].State = model.RunStateRetrying
	db.runs[920].PlannedAt = planned
	db.runs[920].NextRetryAt = fakeNow() - 1

	reply, err := claim(t, svcCtx, acquireReq("inst-b", planned), def)
	if !errors.Is(err, model.ErrAttemptNotRetryable) {
		t.Fatalf("err = %v（reply=%+v），期望 ErrAttemptNotRetryable", err, reply)
	}
	if reply != nil {
		t.Error("报错时不该带结论")
	}
	if got := db.runs[920]; got.State != model.RunStateRetrying || got.FenceToken != 3 {
		t.Errorf("退避中的行被就地改写了：%+v", got)
	}
}

func TestClaimPreemptionDisabledKeepsExpiredHolder(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Config.Lease.PreemptionEnabled = false
	def := enabledDef(db)
	planned := fakeNow() - 600

	first, err := claim(t, svcCtx, acquireReq("inst-a", planned), def)
	if err != nil {
		t.Fatalf("首次领取：%v", err)
	}
	db.runs[first.GetRunId()].LeaseExpireAt = fakeNow() - 1
	db.leases[testTaskKey].ExpireAt = fakeNow() - 1

	reply, err := claim(t, svcCtx, acquireReq("inst-b", planned), def)
	if err != nil {
		t.Fatalf("关闭抢占是结论不是错误：%v", err)
	}
	if reply.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED {
		t.Fatalf("outcome = %v，期望 ALREADY_OWNED", reply.GetOutcome())
	}
	if !strings.Contains(reply.GetMessage(), model.ErrPreemptionDisabled.Error()) {
		t.Errorf("消息必须说明是「排障窗口关闭了抢占」，得到 %q", reply.GetMessage())
	}
	if run := db.runs[first.GetRunId()]; run.LeaseOwner != "inst-a" || run.FenceToken != 1 {
		t.Errorf("关闭抢占后仍被改写：%+v", run)
	}
}

// TestClaimScopedTasksTakeIndependentLeases 校验「分片各锁各的」。
//
// 注意 cron_task_run 的幂等键是 (task_key, planned_at, attempt)，不含 scope（契约缺口，
// 见 README）：两个分片不能共用同一计划时刻，所以本用例用相邻计划时刻领取，
// 断言的是租约这一层——键按 task_key/scope 拆分，且同分片内互斥不串锁。
func TestClaimScopedTasksTakeIndependentLeases(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	def := enabledDef(db)
	// 关掉并发闸门：CountRunning 也按 task_key 统计而不含 scope。
	// 本用例只验「分片级租约各锁各的」这一层，分片与并发闸门的交互属契约缺口（见 README）。
	def.ConcurrencyLimit = 0
	planned := fakeNow() - 600

	a, err := claim(t, svcCtx, scopedReq("inst-a", planned, "shard=1"), def)
	if err != nil || a.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ACQUIRED {
		t.Fatalf("分片 1 领取：%+v %v", a, err)
	}
	b, err := claim(t, svcCtx, scopedReq("inst-b", planned+1, "shard=2"), def)
	if err != nil || b.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ACQUIRED {
		t.Fatalf("分片 2 应独立取得锁：%+v %v", b, err)
	}
	if _, ok := db.leases[testTaskKey+"/shard=1"]; !ok {
		t.Errorf("lease_key 必须是 task_key/scope 组合，得到 %v", keysOf(db.leases))
	}
	if _, ok := db.leases[testTaskKey+"/shard=2"]; !ok {
		t.Errorf("两个分片必须各有自己的租约行，得到 %v", keysOf(db.leases))
	}
	if a.GetRunId() == b.GetRunId() {
		t.Error("不同分片共用了一次执行")
	}
	if a.GetFenceToken() != 1 || b.GetFenceToken() != 1 {
		t.Errorf("两个分片的栅栏应各自从 1 起：%d/%d", a.GetFenceToken(), b.GetFenceToken())
	}
	// 同分片内互斥仍然成立：另一个实例带着同一计划时刻来抢，只能被拒。
	c, err := claim(t, svcCtx, scopedReq("inst-c", planned, "shard=1"), def)
	if err != nil {
		t.Fatalf("同分片抢占：%v", err)
	}
	if c.GetOutcome() != rpc.LeaseOutcome_LEASE_OUTCOME_ALREADY_OWNED {
		t.Errorf("outcome = %v，期望 ALREADY_OWNED（分片锁不许串）", c.GetOutcome())
	}
}

func scopedReq(owner string, plannedAt int64, scope string) *rpc.AcquireLeaseReq {
	req := acquireReq(owner, plannedAt)
	req.Scope = scope
	return req
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- 续租 / 释放 / 上报 ---

func TestRenewLeaseExtendsBothOrFailsLoudly(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)
	run := seedRunning(db, 1001, testTaskKey, "inst-a", 7, fakeNow()+10)
	seedLease(db, testTaskKey, testTaskKey, "inst-a", 7, fakeNow()+10, run.ID)

	reply, err := NewRenewLeaseLogic(context.Background(), svcCtx).RenewLease(
		&rpc.RenewLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 7, TtlSeconds: 300})
	if err != nil {
		t.Fatalf("续租：%v", err)
	}
	if reply.GetLeaseExpireAt() <= fakeNow()+200 {
		t.Errorf("续租后的过期时间没有真的推后：%d", reply.GetLeaseExpireAt())
	}
	// 两处过期时间必须一起续（差 1 秒以内是取整时钟，不是逻辑漏洞）。
	if diff := absInt64(db.runs[run.ID].LeaseExpireAt - db.leases[testTaskKey].ExpireAt); diff > 1 {
		t.Errorf("两处过期时间不一致：run=%d lease=%d",
			db.runs[run.ID].LeaseExpireAt, db.leases[testTaskKey].ExpireAt)
	}
	afterFirst := db.runs[run.ID].LeaseExpireAt

	// 租约已被接管（栅栏与持有者都变了）：续租必须失败，且不许只续上 run 那一半。
	db.leases[testTaskKey].FenceToken = 8
	db.leases[testTaskKey].OwnerInstance = "inst-b"
	_, err = NewRenewLeaseLogic(context.Background(), svcCtx).RenewLease(
		&rpc.RenewLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 7, TtlSeconds: 300})
	if !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("栅栏不一致 err = %v，期望 ErrLeaseLost", err)
	}
	if db.runs[run.ID].LeaseExpireAt != afterFirst {
		t.Errorf("lease 侧 CAS 未命中却把 run 侧续上了（半续租是最坏结果）：%d vs %d",
			db.runs[run.ID].LeaseExpireAt, afterFirst)
	}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestRenewLeaseRejectsBadRequests(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db) // resolveTTL 在没给 ttl_seconds 时要读任务定义
	run := seedRunning(db, 1002, testTaskKey, "inst-a", 3, fakeNow()+60)

	cases := []struct {
		name    string
		req     *rpc.RenewLeaseReq
		wantErr error
	}{
		{"无 run_id", &rpc.RenewLeaseReq{Owner: "inst-a", FenceToken: 3}, model.ErrRunNotFound},
		{"无 owner", &rpc.RenewLeaseReq{RunId: run.ID, FenceToken: 3}, model.ErrLeaseOwnerRequired},
		{"栅栏非正", &rpc.RenewLeaseReq{RunId: run.ID, Owner: "inst-a"}, model.ErrLeaseNotHeld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRenewLeaseLogic(context.Background(), svcCtx).RenewLease(tc.req); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
		})
	}

	t.Run("没有租约行就不承认持有锁", func(t *testing.T) {
		// run_id 反查不到 cron_task_lease：说明这把锁不是走 AcquireLease 拿的。
		if _, err := NewRenewLeaseLogic(context.Background(), svcCtx).RenewLease(
			&rpc.RenewLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 3}); !errors.Is(err, model.ErrLeaseNotHeld) {
			t.Fatalf("err = %v，期望 ErrLeaseNotHeld", err)
		}
	})

	t.Run("TTL 上界越界即拒，下界夹紧", func(t *testing.T) {
		seedLease(db, testTaskKey, testTaskKey, "inst-a", 3, fakeNow()+60, run.ID)
		// 超过 MaxTTLSeconds：心跳绝不能把租约撑成无限期。
		_, err := NewRenewLeaseLogic(context.Background(), svcCtx).RenewLease(
			&rpc.RenewLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 3, TtlSeconds: 90000})
		if !errors.Is(err, model.ErrInvalidLeaseTTL) {
			t.Fatalf("err = %v，期望 ErrInvalidLeaseTTL", err)
		}
		// 低于 MinTTLSeconds：夹紧到下限而不是报错（心跳间隔本就小于下限）。
		reply, err := NewRenewLeaseLogic(context.Background(), svcCtx).RenewLease(
			&rpc.RenewLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 3, TtlSeconds: 10})
		if err != nil {
			t.Fatalf("低于下限的 TTL 应被夹紧而非报错：%v", err)
		}
		if window := reply.GetLeaseExpireAt() - fakeNow(); window < 28 || window > 32 {
			t.Errorf("夹紧后的 TTL = %ds，期望约 %ds（MinTTLSeconds）", window, svcCtx.Config.Lease.MinTTLSeconds)
		}
	})
}

func TestReleaseLeaseOnlyEndsWithSkippedOrCanceled(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	run := seedRunning(db, 1003, testTaskKey, "inst-a", 4, fakeNow()+300)
	seedLease(db, testTaskKey, testTaskKey, "inst-a", 4, fakeNow()+300, run.ID)

	t.Run("final_state 只能是不跑/取消", func(t *testing.T) {
		for _, st := range []rpc.RunState{
			rpc.RunState_RUN_STATE_SUCCEEDED, rpc.RunState_RUN_STATE_FAILED,
			rpc.RunState_RUN_STATE_TIMEOUT, rpc.RunState_RUN_STATE_UNSPECIFIED,
		} {
			_, err := NewReleaseLeaseLogic(context.Background(), svcCtx).ReleaseLease(
				&rpc.ReleaseLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 4, FinalState: st, Reason: "x"})
			if !errors.Is(err, model.ErrInvalidFinalState) {
				t.Errorf("final_state=%v err = %v，期望 ErrInvalidFinalState", st, err)
			}
		}
	})

	t.Run("必须给原因", func(t *testing.T) {
		_, err := NewReleaseLeaseLogic(context.Background(), svcCtx).ReleaseLease(
			&rpc.ReleaseLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 4,
				FinalState: rpc.RunState_RUN_STATE_SKIPPED})
		if !errors.Is(err, model.ErrReasonRequired) {
			t.Fatalf("err = %v，期望 ErrReasonRequired", err)
		}
		if run := db.runs[1003]; run.State != model.RunStateRunning {
			t.Errorf("被拒的释放改了状态：%+v", run)
		}
	})

	t.Run("栅栏不一致即放弃收尾", func(t *testing.T) {
		_, err := NewReleaseLeaseLogic(context.Background(), svcCtx).ReleaseLease(
			&rpc.ReleaseLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 3,
				FinalState: rpc.RunState_RUN_STATE_CANCELED, Reason: "运维取消"})
		if !errors.Is(err, model.ErrLeaseLost) {
			t.Fatalf("err = %v，期望 ErrLeaseLost", err)
		}
	})

	t.Run("正常释放：状态与租约一起落地", func(t *testing.T) {
		if _, err := NewReleaseLeaseLogic(context.Background(), svcCtx).ReleaseLease(
			&rpc.ReleaseLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 4,
				FinalState: rpc.RunState_RUN_STATE_CANCELED, Reason: "运维取消"}); err != nil {
			t.Fatalf("释放：%v", err)
		}
		row := db.runs[1003]
		if row.State != model.RunStateCanceled || row.LastError != "运维取消" {
			t.Errorf("终态/原因未落地：%+v", row)
		}
		if row.LeaseExpireAt != 0 {
			t.Errorf("终态必须交回租约时间，得到 %d", row.LeaseExpireAt)
		}
		if row.DurationMs < 20_000 {
			t.Errorf("耗时由服务端算（started_at 起），得到 %d", row.DurationMs)
		}
		lease := db.leases[testTaskKey]
		if lease.OwnerInstance != "" || lease.ExpireAt != 0 || lease.RunID != 0 {
			t.Errorf("租约未真正交回：%+v", lease)
		}
		if lease.FenceToken != 4 {
			t.Errorf("释放绝不回退栅栏令牌，得到 %d", lease.FenceToken)
		}
	})

	t.Run("幂等重入什么都不改", func(t *testing.T) {
		before := *db.leases[testTaskKey]
		if _, err := NewReleaseLeaseLogic(context.Background(), svcCtx).ReleaseLease(
			&rpc.ReleaseLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 4,
				FinalState: rpc.RunState_RUN_STATE_CANCELED, Reason: "重复点击"}); err != nil {
			t.Fatalf("幂等重入应静默成功：%v", err)
		}
		after := *db.leases[testTaskKey]
		if after != before {
			t.Errorf("幂等重入改动了租约：before=%+v after=%+v", before, after)
		}
	})

	t.Run("换成别的终态属于改写历史", func(t *testing.T) {
		_, err := NewReleaseLeaseLogic(context.Background(), svcCtx).ReleaseLease(
			&rpc.ReleaseLeaseReq{RunId: run.ID, Owner: "inst-a", FenceToken: 4,
				FinalState: rpc.RunState_RUN_STATE_SKIPPED, Reason: "改主意"})
		if !errors.Is(err, model.ErrRunAlreadyFinished) {
			t.Fatalf("err = %v，期望 ErrRunAlreadyFinished", err)
		}
	})
}

func TestReportTaskResultRequiresMatchingOwnerAndFence(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)
	run := seedRunning(db, 1004, testTaskKey, "inst-a", 5, fakeNow()+300)

	cases := []struct {
		name    string
		req     *rpc.ReportTaskResultReq
		wantErr error
	}{
		{"栅栏不一致（已被接管）", &rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a",
			FenceToken: 4, State: rpc.ReportState_REPORT_STATE_SUCCEEDED}, model.ErrLeaseLost},
		{"栅栏对但持有者不对", &rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-x",
			FenceToken: 5, State: rpc.ReportState_REPORT_STATE_SUCCEEDED}, model.ErrLeaseNotHeld},
		{"栅栏非正", &rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a",
			State: rpc.ReportState_REPORT_STATE_SUCCEEDED}, model.ErrLeaseNotHeld},
		{"run 不存在", &rpc.ReportTaskResultReq{RunId: 77, Owner: "inst-a", FenceToken: 5,
			State: rpc.ReportState_REPORT_STATE_SUCCEEDED}, model.ErrRunNotFound},
		{"未知上报状态", &rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 5,
			State: rpc.ReportState_REPORT_STATE_UNSPECIFIED}, model.ErrInvalidFinalState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(tc.req); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if got := db.runs[run.ID]; got.State != model.RunStateRunning {
				t.Fatalf("被拒的上报改动了执行状态：%+v", got)
			}
		})
	}
}

func TestReportTaskResultTerminalIsIdempotent(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)
	run := seedRunning(db, 1005, testTaskKey, "inst-a", 2, fakeNow()+300)
	// 先正常跑完一次。
	if _, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
		&rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 2,
			State: rpc.ReportState_REPORT_STATE_SUCCEEDED, ResultSummary: "scanned=10"}); err != nil {
		t.Fatalf("首次上报：%v", err)
	}
	finished := *db.runs[run.ID]

	reply, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
		&rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 2,
			State: rpc.ReportState_REPORT_STATE_FAILED, ErrorMessage: "重放带来的失败"})
	if err != nil {
		t.Fatalf("重复上报必须幂等成功：%v", err)
	}
	if reply.GetFirstReported() {
		t.Error("已是终态的行不该再算首次上报")
	}
	if reply.GetRun().GetState() != rpc.RunState_RUN_STATE_SUCCEEDED {
		t.Errorf("终态被第二次上报改写了：%+v", reply.GetRun())
	}
	if after := *db.runs[run.ID]; after.State != finished.State || after.ResultSummary != finished.ResultSummary {
		t.Errorf("历史轨迹被覆盖：before=%+v after=%+v", finished, after)
	}
}

func TestReportTaskResultBackoffAndExhaustion(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	def := enabledDef(db) // max_attempts=3, base=30, max=600
	run := seedRunning(db, 1006, testTaskKey, "inst-a", 1, fakeNow()+300)

	reply, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
		&rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 1,
			State: rpc.ReportState_REPORT_STATE_FAILED, ErrorMessage: "下游超时"})
	if err != nil {
		t.Fatalf("失败上报：%v", err)
	}
	row := db.runs[run.ID]
	if row.State != model.RunStateRetrying {
		t.Fatalf("还有重试机会时该进 RETRYING，得到 %s", model.RunStateName(row.State))
	}
	if reply.GetNextAttempt() != 2 {
		t.Errorf("next_attempt = %d，期望 2", reply.GetNextAttempt())
	}
	// 退避窗口由服务端算：base=30，第 1 次失败后约 30s。
	window := reply.GetNextRetryAt() - fakeNow()
	if window < 25 || window > 35 {
		t.Errorf("退避窗口 = %ds，期望约 %ds（base=%d）", window, def.RetryBaseSeconds, def.RetryBaseSeconds)
	}
	if row.NextRetryAt != reply.GetNextRetryAt() {
		t.Errorf("响应与库里的 next_retry_at 不一致：%d vs %d", reply.GetNextRetryAt(), row.NextRetryAt)
	}
	if row.LeaseExpireAt == 0 {
		t.Error("RETRYING 不是终态，不该把租约时间清零")
	}

	// 最后一次尝试仍失败：直接 FAILED 终态，并把任务级最近失败回写定义。
	db.runs[run.ID].State = model.RunStateRunning
	db.runs[run.ID].Attempt = 3
	if _, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
		&rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 1,
			State: rpc.ReportState_REPORT_STATE_FAILED, ErrorMessage: "三连失败"}); err != nil {
		t.Fatalf("最后一次失败上报：%v", err)
	}
	if got := db.runs[run.ID]; got.State != model.RunStateFailed || got.NextRetryAt != 0 {
		t.Errorf("重试耗尽后应落 FAILED 且不再排退避：%+v", got)
	}
	if got := db.defs[testTaskKey]; got.LastError != "三连失败" || got.LastSuccessAt != 0 {
		t.Errorf("任务级最近结果未回写：%+v", got)
	}
}

func TestReportTaskResultAdvancesCheckpointAtomically(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)
	run := seedRunning(db, 1007, testTaskKey, "inst-a", 1, fakeNow()+300)
	db.checkpoints[checkpointKey(testTaskKey, "")] = &model.TaskCheckpoint{
		ID: 1, TaskKey: testTaskKey, Value: 40, Version: 3, Ctime: fakeNow() - 60, Mtime: fakeNow() - 60,
	}

	t.Run("游标版本冲突时结果上报必须回滚", func(t *testing.T) {
		_, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
			&rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 1,
				State: rpc.ReportState_REPORT_STATE_SUCCEEDED,
				Checkpoint: &rpc.Checkpoint{TaskKey: testTaskKey, Value: 99,
					Version: 2}}) // 期望版本 2，库里是 3
		if !errors.Is(err, model.ErrCheckpointConflict) {
			t.Fatalf("err = %v，期望 ErrCheckpointConflict", err)
		}
		if got := db.runs[run.ID]; got.State != model.RunStateRunning {
			t.Errorf("游标冲突却把执行记成终态了（重放会漏数据）：%+v", got)
		}
		if got := db.checkpoints[checkpointKey(testTaskKey, "")]; got.Value != 40 || got.Version != 3 {
			t.Errorf("游标被半路改写：%+v", got)
		}
	})

	t.Run("版本正确时一起前进", func(t *testing.T) {
		reply, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
			&rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 1,
				State:      rpc.ReportState_REPORT_STATE_SUCCEEDED,
				Checkpoint: &rpc.Checkpoint{TaskKey: testTaskKey, Value: 99, Version: 3}})
		if err != nil {
			t.Fatalf("上报：%v", err)
		}
		if !reply.GetCheckpointAdvanced() || !reply.GetFirstReported() {
			t.Errorf("reply = %+v，期望 first_reported 与 checkpoint_advanced 同时为真", reply)
		}
		got := db.checkpoints[checkpointKey(testTaskKey, "")]
		if got.Value != 99 || got.Version != 4 {
			t.Errorf("游标未前进：%+v", got)
		}
		if run := db.runs[run.ID]; run.State != model.RunStateSucceeded {
			t.Errorf("执行未落终态：%+v", run)
		}
	})
}

func TestReportTaskResultReleasesTaskLeaseOnTerminal(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)
	run := seedRunning(db, 1008, testTaskKey, "inst-a", 6, fakeNow()+300)
	seedLease(db, testTaskKey, testTaskKey, "inst-a", 6, fakeNow()+300, run.ID)

	if _, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
		&rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 6,
			State: rpc.ReportState_REPORT_STATE_SUCCEEDED}); err != nil {
		t.Fatalf("上报：%v", err)
	}
	lease := db.leases[testTaskKey]
	if lease.OwnerInstance != "" || lease.ExpireAt != 0 {
		t.Errorf("终态后必须交回任务级租约（否则下个计划点白等一个 TTL）：%+v", lease)
	}
	if lease.FenceToken != 6 {
		t.Errorf("交回不回退栅栏，得到 %d", lease.FenceToken)
	}
	if got := db.defs[testTaskKey]; got.LastSuccessAt <= 0 {
		t.Errorf("成功时间未回写任务定义：%+v", got)
	}
}

func TestReportTaskResultRejectsOversizedSummary(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)
	run := seedRunning(db, 1009, testTaskKey, "inst-a", 1, fakeNow()+300)

	// 配置缺省（0）时也不能「不校验」：必须夹到列宽上限。
	svcCtx.Config.Task.MaxResultSummaryBytes = 0
	_, err := NewReportTaskResultLogic(context.Background(), svcCtx).ReportTaskResult(
		&rpc.ReportTaskResultReq{RunId: run.ID, Owner: "inst-a", FenceToken: 1,
			State:         rpc.ReportState_REPORT_STATE_SUCCEEDED,
			ResultSummary: strings.Repeat("字", model.MaxResultSummaryBytes)})
	if !errors.Is(err, model.ErrParamsTooLarge) {
		t.Fatalf("err = %v，期望 ErrParamsTooLarge（结果摘要只有 VARCHAR(1024)）", err)
	}
	if got := db.runs[run.ID]; got.State != model.RunStateRunning {
		t.Errorf("超限上报不该落库：%+v", got)
	}
}

// --- 游标独立推进（CAS） ---

func TestSaveCheckpointCasSemantics(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)
	base := func(version int64) *rpc.SaveCheckpointReq {
		return &rpc.SaveCheckpointReq{
			Checkpoint:      &rpc.Checkpoint{TaskKey: testTaskKey, Value: 100, Version: version},
			ExpectedVersion: version,
			Operator:        "inst-a",
			IdempotencyKey:  "req-1",
		}
	}

	t.Run("缺幂等键即拒", func(t *testing.T) {
		req := base(0)
		req.IdempotencyKey = "  "
		if _, err := NewSaveCheckpointLogic(context.Background(), svcCtx).SaveCheckpoint(req); !errors.Is(err, model.ErrIdempotencyKeyEmpty) {
			t.Fatalf("err = %v，期望 ErrIdempotencyKeyEmpty", err)
		}
	})

	t.Run("首写要求行尚不存在", func(t *testing.T) {
		reply, err := NewSaveCheckpointLogic(context.Background(), svcCtx).SaveCheckpoint(base(0))
		if err != nil {
			t.Fatalf("首写：%v", err)
		}
		if !reply.GetAdvanced() || reply.GetCheckpoint().GetVersion() != 1 {
			t.Errorf("首写后 version 必须是服务端算出的 1，得到 %+v", reply.GetCheckpoint())
		}
	})

	t.Run("并发首写第二个赢家必须冲突", func(t *testing.T) {
		_, err := NewSaveCheckpointLogic(context.Background(), svcCtx).SaveCheckpoint(base(0))
		if !errors.Is(err, model.ErrCheckpointConflict) {
			t.Fatalf("err = %v，期望 ErrCheckpointConflict", err)
		}
	})

	t.Run("版本对不上就绝不覆盖", func(t *testing.T) {
		_, err := NewSaveCheckpointLogic(context.Background(), svcCtx).SaveCheckpoint(base(9))
		if !errors.Is(err, model.ErrCheckpointConflict) {
			t.Fatalf("err = %v，期望 ErrCheckpointConflict", err)
		}
	})

	t.Run("两个版本字段自相矛盾即拒", func(t *testing.T) {
		req := base(1)
		req.Checkpoint.Version = 2
		if _, err := NewSaveCheckpointLogic(context.Background(), svcCtx).SaveCheckpoint(req); !errors.Is(err, model.ErrCheckpointConflict) {
			t.Fatalf("err = %v，期望 ErrCheckpointConflict", err)
		}
	})

	t.Run("游标必须挂在已注册任务上", func(t *testing.T) {
		req := base(1)
		req.Checkpoint.TaskKey = "nope.not_registered"
		if _, err := NewSaveCheckpointLogic(context.Background(), svcCtx).SaveCheckpoint(req); !errors.Is(err, model.ErrTaskNotFound) {
			t.Fatalf("err = %v，期望 ErrTaskNotFound", err)
		}
	})

	t.Run("value_str 超列宽即拒", func(t *testing.T) {
		req := base(1)
		req.Checkpoint.ValueStr = strings.Repeat("a", model.MaxValueStrBytes+1)
		if _, err := NewSaveCheckpointLogic(context.Background(), svcCtx).SaveCheckpoint(req); !errors.Is(err, model.ErrParamsTooLarge) {
			t.Fatalf("err = %v，期望 ErrParamsTooLarge", err)
		}
	})

	t.Run("scope_key 允许空表示默认游标", func(t *testing.T) {
		req := base(1)
		req.Checkpoint.ValueStr = "alias-v2"
		if _, err := NewSaveCheckpointLogic(context.Background(), svcCtx).SaveCheckpoint(req); err != nil {
			t.Fatalf("带期望版本的一次前进：%v", err)
		}
		got := db.checkpoints[checkpointKey(testTaskKey, "")]
		if got.ValueStr != "alias-v2" || got.Version != 2 {
			t.Errorf("游标未按时前进：%+v", got)
		}
	})
}
