package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/spm/model"
	"go-video/services/spm/rpc"
)

// 本文件钉住 README「作业与租约语义」：作业提交是 uniq_request_id 幂等的排队动作，
// 同键改内容必须冲突报错，重算入口必须显式钉口径版本，且提交本身不落任何指标。

func seedJob(t *testing.T, d *deps, requestID string,
	mut func(*model.AggregationJob)) *model.AggregationJob {
	t.Helper()
	d.db.nextJob++
	row := &model.AggregationJob{
		ID: d.db.nextJob, JobType: model.JobTypeRealtime, State: model.JobStatePending,
		SubjectType: model.SubjectTypeAid, SubjectID: 101, MetricKey: "play_cnt",
		MetricVersion: 1, WindowType: model.WindowType5Min,
		WindowStartFrom: alignedStart5Min(-2), WindowStartTo: alignedStart5Min(0),
		WindowsTotal: 3, RequestID: requestID, Operator: "tester", Reason: "预置",
		Ctime: nowForTest(), Mtime: nowForTest(),
	}
	if mut != nil {
		mut(row)
	}
	d.db.jobs[row.RequestID] = row
	return row
}

func callSubmitJob(t *testing.T, d *deps,
	in *rpc.SubmitAggregationJobReq) (*rpc.SubmitAggregationJobReply, error) {
	t.Helper()
	return NewSubmitAggregationJobLogic(context.Background(), d.ctx).SubmitAggregationJob(in)
}

func callRecompute(t *testing.T, d *deps,
	in *rpc.RecomputeMetricsReq) (*rpc.RecomputeMetricsReply, error) {
	t.Helper()
	return NewRecomputeMetricsLogic(context.Background(), d.ctx).RecomputeMetrics(in)
}

func callGetJob(t *testing.T, d *deps, in *rpc.GetAggregationJobReq) (*rpc.GetAggregationJobReply, error) {
	t.Helper()
	return NewGetAggregationJobLogic(context.Background(), d.ctx).GetAggregationJob(in)
}

func callListJobs(t *testing.T, d *deps,
	in *rpc.ListAggregationJobsReq) (*rpc.ListAggregationJobsReply, error) {
	t.Helper()
	return NewListAggregationJobsLogic(context.Background(), d.ctx).ListAggregationJobs(in)
}

func recomputeReq(metricKey string, version int32, from, to int64,
	reqID string) *rpc.RecomputeMetricsReq {
	return &rpc.RecomputeMetricsReq{
		SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, SubjectId: 101, MetricKey: metricKey,
		MetricVersion: version, WindowType: rpc.WindowType_WINDOW_TYPE_5_MIN,
		WindowStartFrom: from, WindowStartTo: to, RequestId: reqID, Operator: "sre",
	}
}

func submitReq(jobType rpc.JobType, windowType rpc.WindowType,
	metricKey string, version int32) *rpc.SubmitAggregationJobReq {
	return &rpc.SubmitAggregationJobReq{
		JobType: jobType, SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, SubjectId: 101,
		MetricKey: metricKey, MetricVersion: version, WindowType: windowType,
		WindowStartFrom: alignedStart5Min(-2), WindowStartTo: alignedStart5Min(0),
		RequestId: "req-job-" + strings.ReplaceAll(metricKey, " ", "_"), Operator: "sre",
		Reason: "补算上一段窗口",
	}
}

// TestRecomputeMetricsOnlyQueuesAVersionPinnedJob 重算入口：必须显式口径版本，
// 只派生 PENDING 作业，不写任何指标行（真正的重算由执行器以 source=RECOMPUTE 回写）。
func TestRecomputeMetricsOnlyQueuesAVersionPinnedJob(t *testing.T) {
	d := newDeps(t)
	const key = "play_cnt"
	mustActiveDef(t, d, key, 2, "1,3")
	from, to := alignedStart5Min(-3), alignedStart5Min(0)

	reply, err := callRecompute(t, d, recomputeReq(key, 2, from, to, "req-rc-1"))
	if err != nil {
		t.Fatalf("重算提交失败: %v", err)
	}
	if reply.JobId <= 0 || reply.Reused {
		t.Fatalf("job_id/reused=%d/%v，期望正数/false", reply.JobId, reply.Reused)
	}
	if reply.WindowsPlanned != 4 {
		t.Fatalf("windows_planned=%d，期望 4（[from,to] 含端点）", reply.WindowsPlanned)
	}
	job := d.db.jobs["req-rc-1"]
	if job.ID != reply.JobId {
		t.Fatalf("回带 job_id=%d 与库里的 %d 不一致", reply.JobId, job.ID)
	}
	if job.JobType != model.JobTypeRecompute || job.State != model.JobStatePending {
		t.Fatalf("作业类型/状态=%d/%d，期望 RECOMPUTE/PENDING", job.JobType, job.State)
	}
	if job.MetricVersion != 2 || job.WindowStartFrom != from || job.WindowStartTo != to ||
		job.WindowsTotal != 4 || job.WindowsDone != 0 || job.WindowsFailed != 0 {
		t.Fatalf("作业内容与请求不符: %+v", job)
	}
	if job.Operator != "sre" || job.RequestID != "req-rc-1" {
		t.Fatalf("留痕列缺失: %+v", job)
	}
	// 本方法是「排队」而不是「执行」。
	if d.wins.upsertCalls != 0 || len(d.db.windows) != 0 || d.db.txRuns != 0 {
		t.Fatalf("重算提交动了指标投影：upsert=%d 行数=%d 事务=%d",
			d.wins.upsertCalls, len(d.db.windows), d.db.txRuns)
	}
}

// TestRecomputeMetricsIsIdempotentAndRejectsContentReuse 同一 request_id 重复投递回放首次作业，
// 换内容再投必须冲突：否则调用方以为新的重算已排队，实际库里还是老那份。
func TestRecomputeMetricsIsIdempotentAndRejectsContentReuse(t *testing.T) {
	d := newDeps(t)
	const key = "play_cnt"
	mustActiveDef(t, d, key, 1, "1")
	from, to := alignedStart5Min(-3), alignedStart5Min(0)

	first, err := callRecompute(t, d, recomputeReq(key, 1, from, to, "req-rc-2"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := callRecompute(t, d, recomputeReq(key, 1, from, to, "req-rc-2"))
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if !again.Reused || again.JobId != first.JobId || again.WindowsPlanned != first.WindowsPlanned {
		t.Fatalf("重放应回放首次作业：%+v vs %+v", again, first)
	}
	// 去重靠库里的 uniq_request_id，不是进程内缓存：两次投递都要走到 model 边界，
	// 而库里只能有一行。
	if d.jobs.insertCalls != 2 || len(d.db.jobs) != 1 {
		t.Fatalf("重放路径不对：insert=%d 作业数=%d", d.jobs.insertCalls, len(d.db.jobs))
	}
	if d.jobs.insertSeen[0].ID != 0 {
		t.Fatalf("真库不回填主键，假件被改坏了：%+v", d.jobs.insertSeen[0])
	}

	t.Run("同键改区间", func(t *testing.T) {
		_, err := callRecompute(t, d, recomputeReq(key, 1, alignedStart5Min(-9), to, "req-rc-2"))
		requireErrIs(t, err, model.ErrRequestIdConflict, "幂等键复用")
		if len(d.db.jobs) != 1 {
			t.Fatalf("冲突路径留下了 %d 个作业", len(d.db.jobs))
		}
		if got := d.db.jobs["req-rc-2"].WindowStartFrom; got != from {
			t.Fatalf("冲突路径改写了已有作业的区间: %d", got)
		}
	})
	t.Run("同键改主体", func(t *testing.T) {
		in := recomputeReq(key, 1, from, to, "req-rc-2")
		in.SubjectId = 999
		_, err := callRecompute(t, d, in)
		requireErrIs(t, err, model.ErrRequestIdConflict, "主体不同也是不同请求")
	})
	t.Run("同键改留痕不算冲突", func(t *testing.T) {
		in := recomputeReq(key, 1, from, to, "req-rc-2")
		in.Operator = "another-sre"
		reply, err := callRecompute(t, d, in)
		if err != nil {
			t.Fatalf("operator 不是作业的业务身份: %v", err)
		}
		if !reply.Reused || reply.JobId != first.JobId {
			t.Fatalf("应回放首次作业: %+v", reply)
		}
	})

	t.Run("插成功却读不到时绝不回 job_id=0", func(t *testing.T) {
		d2 := newDeps(t)
		mustActiveDef(t, d2, key, 1, "1")
		d2.jobs.readBackNil = true
		_, err := callRecompute(t, d2, recomputeReq(key, 1, from, to, "req-rc-3"))
		requireErrIs(t, err, model.ErrJobNotFound, "回读为空")
	})
	t.Run("库故障原样上抛", func(t *testing.T) {
		d2 := newDeps(t)
		mustActiveDef(t, d2, key, 1, "1")
		d2.jobs.errOnInsert = true
		_, err := callRecompute(t, d2, recomputeReq(key, 1, from, to, "req-rc-4"))
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		d2.jobs.errOnInsert = false
		d2.jobs.errOnFind = true
		_, err = callRecompute(t, d2, recomputeReq(key, 1, from, to, "req-rc-5"))
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
	})
}

// TestRecomputeMetricsRejectsUnsafeRequests 重算入参边界：不接受 ACTIVE 指针、
// 不接受非 ACTIVE 口径、区间与跨度越界一律拒绝而不是截断执行。
func TestRecomputeMetricsRejectsUnsafeRequests(t *testing.T) {
	d := newDeps(t)
	const key = "play_cnt"
	mustActiveDef(t, d, key, 1, "1")
	seedDef(t, d, "draft_key", 1, model.DefinitionStateDraft)
	seedDef(t, d, "retired_key", 1, model.DefinitionStateRetired)
	from := alignedStart5Min(-3)

	cases := []struct {
		why  string
		want error
		mut  func(*rpc.RecomputeMetricsReq)
	}{
		{"不接受 ACTIVE 指针", model.ErrMetricVersionRequired,
			func(in *rpc.RecomputeMetricsReq) { in.MetricVersion = 0 }},
		{"负版本", model.ErrMetricVersionRequired,
			func(in *rpc.RecomputeMetricsReq) { in.MetricVersion = -1 }},
		{"缺操作人", model.ErrOperatorRequired,
			func(in *rpc.RecomputeMetricsReq) { in.Operator = " " }},
		{"缺幂等键", model.ErrRequestIdRequired,
			func(in *rpc.RecomputeMetricsReq) { in.RequestId = "" }},
		{"未登记口径", model.ErrMetricDefinitionNotFound,
			func(in *rpc.RecomputeMetricsReq) { in.MetricKey = "ghost" }},
		{"草稿口径不可重算", model.ErrMetricNotActive,
			func(in *rpc.RecomputeMetricsReq) { in.MetricKey = "draft_key" }},
		{"退役口径不可重算", model.ErrMetricNotActive,
			func(in *rpc.RecomputeMetricsReq) { in.MetricKey = "retired_key" }},
		{"粒度未登记", model.ErrInvalidWindow,
			func(in *rpc.RecomputeMetricsReq) { in.WindowType = rpc.WindowType_WINDOW_TYPE_DAY }},
		{"粒度 UNSPECIFIED", model.ErrInvalidWindow,
			func(in *rpc.RecomputeMetricsReq) {
				in.WindowType = rpc.WindowType_WINDOW_TYPE_UNSPECIFIED
			}},
		{"缺起点", model.ErrInvalidWindow,
			func(in *rpc.RecomputeMetricsReq) { in.WindowStartFrom = 0 }},
		{"区间倒挂", model.ErrInvalidWindow, func(in *rpc.RecomputeMetricsReq) {
			in.WindowStartFrom, in.WindowStartTo = alignedStart5Min(0), alignedStart5Min(-6)
		}},
		{"主体未指定", model.ErrInvalidSubject,
			func(in *rpc.RecomputeMetricsReq) { in.SubjectId = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			in := recomputeReq(key, 1, from, alignedStart5Min(0), "req-rc-bad-"+tc.why)
			tc.mut(in)
			_, err := callRecompute(t, d, in)
			requireErrIs(t, err, tc.want, tc.why)
		})
	}
	t.Run("跨度超上限", func(t *testing.T) {
		in := recomputeReq(key, 1,
			alignedStart5Min(-int64(d.ctx.Config.Spm.MaxWindowsPerJob)-2),
			alignedStart5Min(0), "req-rc-huge")
		_, err := callRecompute(t, d, in)
		requireErrIs(t, err, model.ErrWindowRangeTooLarge, "拆半执行会让同一 request_id 进度不可解释")
		if !strings.Contains(err.Error(), "拆分提交") {
			t.Fatalf("错误应给出可执行指引: %v", err)
		}
	})
	t.Run("写侧限流", func(t *testing.T) {
		d.write.denies = 1
		_, err := callRecompute(t, d, recomputeReq(key, 1, from, alignedStart5Min(0), "req-rc-rl"))
		requireErrIs(t, err, model.ErrRateLimited, "无令牌")
	})
	if len(d.db.jobs) != 0 {
		t.Fatalf("以上拒绝路径排队了 %d 个作业", len(d.db.jobs))
	}
}

// TestSubmitAggregationJobTypeGuards 作业类型白名单：RECOMPUTE 只能由 RecomputeMetrics 派生，
// REALTIME 只有 5 分钟/小时两档，回填必须钉口径版本。
func TestSubmitAggregationJobTypeGuards(t *testing.T) {
	d := newDeps(t)
	const key = "play_cnt"
	mustActiveDef(t, d, key, 3, "1,2,3")

	t.Run("RECOMPUTE 必须走专用入口", func(t *testing.T) {
		in := submitReq(rpc.JobType_JOB_TYPE_RECOMPUTE, rpc.WindowType_WINDOW_TYPE_DAY, key, 3)
		_, err := callSubmitJob(t, d, in)
		requireErrIs(t, err, model.ErrInvalidJobType, "重算入口分离")
	})
	for _, jt := range []rpc.JobType{rpc.JobType_JOB_TYPE_UNSPECIFIED, rpc.JobType(9)} {
		t.Run("非法作业类型", func(t *testing.T) {
			_, err := callSubmitJob(t, d, submitReq(jt, rpc.WindowType_WINDOW_TYPE_5_MIN, key, 0))
			requireErrIs(t, err, model.ErrInvalidJobType, "job_type 白名单")
		})
	}
	t.Run("REALTIME 只支持 5 分钟与小时", func(t *testing.T) {
		for _, wt := range []rpc.WindowType{rpc.WindowType_WINDOW_TYPE_DAY,
			rpc.WindowType_WINDOW_TYPE_WEEK, rpc.WindowType_WINDOW_TYPE_TOTAL} {
			_, err := callSubmitJob(t, d, submitReq(rpc.JobType_JOB_TYPE_REALTIME, wt, "", 0))
			requireErrIs(t, err, model.ErrInvalidWindow, "实时档位")
		}
	})
	t.Run("回填必须显式口径版本", func(t *testing.T) {
		in := submitReq(rpc.JobType_JOB_TYPE_OFFLINE_BACKFILL, rpc.WindowType_WINDOW_TYPE_DAY,
			key, 0)
		_, err := callSubmitJob(t, d, in)
		requireErrIs(t, err, model.ErrMetricVersionRequired, "回填钉版本")
	})
	t.Run("回填必须给口径键", func(t *testing.T) {
		in := submitReq(rpc.JobType_JOB_TYPE_OFFLINE_BACKFILL, rpc.WindowType_WINDOW_TYPE_DAY,
			"", 0)
		_, err := callSubmitJob(t, d, in)
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "逐个口径钉版本")
	})
	t.Run("不给口径键就不许给版本", func(t *testing.T) {
		in := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_5_MIN, "", 3)
		_, err := callSubmitJob(t, d, in)
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "版本没有归属")
	})
	t.Run("粒度未登记在口径里", func(t *testing.T) {
		in := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_WEEK, key, 3)
		_, err := callSubmitJob(t, d, in)
		requireErrIs(t, err, model.ErrInvalidWindow, "supported_windows")
	})
	t.Run("主体形态矛盾", func(t *testing.T) {
		in := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_5_MIN, key, 0)
		in.SubjectType, in.SubjectId = rpc.SubjectType_SUBJECT_TYPE_UNSPECIFIED, 101
		_, err := callSubmitJob(t, d, in)
		requireErrIs(t, err, model.ErrInvalidSubject, "subject_type=0 时主键必须为 0")
	})
	for _, mut := range []struct {
		why  string
		want error
		fn   func(*rpc.SubmitAggregationJobReq)
	}{
		{"缺理由", model.ErrReasonRequired, func(in *rpc.SubmitAggregationJobReq) {
			in.Reason = "  "
		}},
		{"缺操作人", model.ErrOperatorRequired, func(in *rpc.SubmitAggregationJobReq) {
			in.Operator = ""
		}},
		{"缺幂等键", model.ErrRequestIdRequired, func(in *rpc.SubmitAggregationJobReq) {
			in.RequestId = ""
		}},
	} {
		t.Run(mut.why, func(t *testing.T) {
			in := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_5_MIN,
				key, 0)
			mut.fn(in)
			_, err := callSubmitJob(t, d, in)
			requireErrIs(t, err, mut.want, mut.why)
		})
	}
	if len(d.db.jobs) != 0 {
		t.Fatalf("以上拒绝路径排队了 %d 个作业", len(d.db.jobs))
	}
}

// TestSubmitAggregationJobResolvesActiveVersion 实时作业允许不钉口径（本轮全部 ACTIVE 口径），
// 钉了就把 version=0 解析成当时的 ACTIVE 版本再落库：作业行必须记下确定的版本。
func TestSubmitAggregationJobResolvesActiveVersion(t *testing.T) {
	d := newDeps(t)
	const key = "play_cnt"
	mustActiveDef(t, d, key, 3, "1,2")

	wide := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_5_MIN, "", 0)
	wide.RequestId = "req-all-metrics"
	reply, err := callSubmitJob(t, d, wide)
	if err != nil {
		t.Fatalf("实时全量作业被拒: %v", err)
	}
	if reply.Job.MetricKey != "" || reply.Job.MetricVersion != 0 {
		t.Fatalf("全量作业不该带口径: %+v", reply.Job)
	}
	if reply.Job.SubjectType != rpc.SubjectType_SUBJECT_TYPE_AID || reply.Job.SubjectId != 101 {
		t.Fatalf("主体投影不对: %+v", reply.Job)
	}
	if reply.Job.WindowsTotal != 3 || reply.Job.State != rpc.JobState_JOB_STATE_PENDING {
		t.Fatalf("作业内容不对: %+v", reply.Job)
	}

	pinned := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_HOUR, key, 0)
	pinned.RequestId = "req-active"
	pinned.WindowStartFrom, pinned.WindowStartTo = hourStart(-5), hourStart(-1)
	if _, err := callSubmitJob(t, d, pinned); err != nil {
		t.Fatal(err)
	}
	got := d.db.jobs["req-active"]
	if got.MetricVersion != 3 {
		t.Fatalf("version=0 未解析成 ACTIVE 版本 %d: %+v", 3, got)
	}
	if got.WindowStartFrom != hourStart(-5) || got.WindowStartTo != hourStart(-1) ||
		got.WindowsTotal != 5 {
		t.Fatalf("小时级区间未按左边界规整: %+v", got)
	}

	// to=0 由服务端按「现在」补，调用方省略终点是常态。
	openEnded := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_HOUR, key, 0)
	openEnded.RequestId = "req-open-ended"
	openEnded.WindowStartFrom, openEnded.WindowStartTo = hourStart(-6), 0
	if _, err := callSubmitJob(t, d, openEnded); err != nil {
		t.Fatal(err)
	}
	latest := d.db.jobs["req-open-ended"]
	if latest.WindowStartTo != hourStart(0) || latest.WindowStartTo <= latest.WindowStartFrom ||
		latest.WindowsTotal != 7 {
		t.Fatalf("终点兜底不对: %+v", latest)
	}

	unscoped := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_5_MIN, "", 0)
	unscoped.SubjectType, unscoped.SubjectId = rpc.SubjectType_SUBJECT_TYPE_UNSPECIFIED, 0
	unscoped.RequestId = "req-unscoped"
	if _, err := callSubmitJob(t, d, unscoped); err != nil {
		t.Fatalf("(0,0) 全部主体应受理: %v", err)
	}
}

// TestSubmitAggregationJobIsIdempotent 同一 request_id 回带首次作业，不重复排队。
func TestSubmitAggregationJobIsIdempotent(t *testing.T) {
	d := newDeps(t)
	const key = "play_cnt"
	mustActiveDef(t, d, key, 1, "1,2,3")
	in := submitReq(rpc.JobType_JOB_TYPE_OFFLINE_BACKFILL, rpc.WindowType_WINDOW_TYPE_DAY, key, 1)
	in.WindowStartFrom, in.WindowStartTo = dayStart(-3), dayStart(-1)
	in.RequestId = "req-idem"

	first, err := callSubmitJob(t, d, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused || first.JobId != first.Job.JobId {
		t.Fatalf("首次提交响应不对: %+v", first)
	}
	again, err := callSubmitJob(t, d, in)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if !again.Reused || again.JobId != first.JobId {
		t.Fatalf("重放应回首次作业：%+v vs %+v", again, first)
	}
	// 去重靠库里的 uniq_request_id，不是进程内缓存：两次投递都要走到 model 边界，
	// 而库里只能有一行。
	if d.jobs.insertCalls != 2 || len(d.db.jobs) != 1 {
		t.Fatalf("重放路径不对：insert=%d 作业数=%d", d.jobs.insertCalls, len(d.db.jobs))
	}

	t.Run("同键改粒度算冲突", func(t *testing.T) {
		other := submitReq(rpc.JobType_JOB_TYPE_OFFLINE_BACKFILL, rpc.WindowType_WINDOW_TYPE_HOUR,
			key, 1)
		other.RequestId = "req-idem"
		other.WindowStartFrom, other.WindowStartTo = dayStart(-3), dayStart(-1)
		_, err := callSubmitJob(t, d, other)
		requireErrIs(t, err, model.ErrRequestIdConflict, "区间/维度变了")
	})
	t.Run("回读为空", func(t *testing.T) {
		d2 := newDeps(t)
		mustActiveDef(t, d2, key, 1, "1,2")
		d2.jobs.created = boolPtr(true)
		d2.jobs.readBackNil = true
		req := submitReq(rpc.JobType_JOB_TYPE_REALTIME, rpc.WindowType_WINDOW_TYPE_5_MIN, key, 1)
		req.RequestId = "req-lost"
		_, err := callSubmitJob(t, d2, req)
		requireErrIs(t, err, model.ErrJobNotFound, "不能回 job_id=0")
	})
}

func boolPtr(v bool) *bool { return &v }

// dayStart 返回相对今天第 n 天的天级左边界（与 model.DayStartUnix 同源）。
func dayStart(n int64) int64 {
	return model.DayStartUnix(timeNow().Unix()) + n*dayUnix
}

// hourStart 返回相对「本小时左边界」的第 n 个小时起点。
func hourStart(n int64) int64 {
	return model.AlignWindow(timeNow().Unix(), model.WindowTypeHour) + n*hourUnix
}

// TestGetAggregationJobLookups 双入口查询：任一入口都能定位；不命中回 found=false
// （cron 轮询终态是常态，不能靠错误码轮询）。
func TestGetAggregationJobLookups(t *testing.T) {
	d := newDeps(t)
	created := seedJob(t, d, "req-find", nil)

	t.Run("按 request_id（D-2：当前实现查不到）", func(t *testing.T) {
		// 已知缺陷 D-2（阻断性）：只给 request_id 时 job_id 恒为 0，而 default 分支要求
		// row.ID == in.GetJobId() 才算命中，于是任何真实作业（id >= 1）都被判「不命中」。
		// 本用例钉住现状并留作 tripwire：修复后应断言 found=true 且回带该作业。
		reply, err := callGetJob(t, d, &rpc.GetAggregationJobReq{RequestId: "req-find"})
		if err != nil {
			t.Fatalf("查不到不该报错: %v", err)
		}
		if reply.Found {
			t.Fatalf("D-2 已修复？请同步本断言为「按幂等键查到作业」: %+v", reply)
		}
	})
	t.Run("按 job_id", func(t *testing.T) {
		reply, err := callGetJob(t, d, &rpc.GetAggregationJobReq{JobId: created.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !reply.Found || reply.Job.RequestId != "req-find" || reply.Job.JobId != created.ID {
			t.Fatalf("按主键没查到：%+v", reply)
		}
	})
	t.Run("两个都给且一致", func(t *testing.T) {
		reply, err := callGetJob(t, d, &rpc.GetAggregationJobReq{
			JobId: created.ID, RequestId: "req-find"})
		if err != nil {
			t.Fatal(err)
		}
		if !reply.Found {
			t.Fatal("一致的定位条件不该查不到")
		}
	})
	t.Run("两个都不给", func(t *testing.T) {
		_, err := callGetJob(t, d, &rpc.GetAggregationJobReq{})
		requireErrIs(t, err, model.ErrRequestIdRequired, "无定位条件")
	})
	t.Run("不存在", func(t *testing.T) {
		reply, err := callGetJob(t, d, &rpc.GetAggregationJobReq{RequestId: "ghost"})
		if err != nil {
			t.Fatalf("查不到不该报错: %v", err)
		}
		if reply.Found || reply.Job != nil {
			t.Fatalf("found=%v 且回了作业 %+v", reply.Found, reply.Job)
		}
		reply, err = callGetJob(t, d, &rpc.GetAggregationJobReq{JobId: 9999})
		if err != nil || reply.Found {
			t.Fatalf("按不存在的主键: %v %+v", err, reply)
		}
	})
	// 已知缺陷 D-2：注释与实现规划都要求「job_id 与 request_id 指向不同作业时按不命中处理」，
	// 但 switch 的 case in.GetJobId() > 0 在两个都给时先命中，那段交叉核对是死代码。
	// 下面的断言钉住当前可观测行为（以 job_id 为准、request_id 被忽略），修 D-2 时应改为
	// 「found=false」并断言 FindByRequestID 被调用过。
	t.Run("两键矛盾时当前实现以 job_id 为准", func(t *testing.T) {
		seedJob(t, d, "req-other", nil)
		reply, err := callGetJob(t, d, &rpc.GetAggregationJobReq{
			JobId: created.ID, RequestId: "req-other"})
		if err != nil {
			t.Fatal(err)
		}
		if !reply.Found || reply.Job.JobId != created.ID || reply.Job.RequestId != "req-find" {
			t.Fatalf("当前实现回的是 job_id 那一行（D-2）: %+v", reply)
		}
	})
	t.Run("负 job_id 加有效幂等键查不到", func(t *testing.T) {
		reply, err := callGetJob(t, d, &rpc.GetAggregationJobReq{JobId: -1, RequestId: "req-find"})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Found {
			t.Fatalf("D-2 的连带后果：负数主键会压掉 request_id 查询: %+v", reply)
		}
	})
	t.Run("库故障与限流", func(t *testing.T) {
		d2 := newDeps(t)
		d2.jobs.errOnFind = true
		_, err := callGetJob(t, d2, &rpc.GetAggregationJobReq{JobId: 1})
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		d2.read.denies = 1
		_, err = callGetJob(t, d2, &rpc.GetAggregationJobReq{JobId: 1})
		requireErrIs(t, err, model.ErrRateLimited, "无令牌")
	})
}

// TestListAggregationJobsFiltersAndBounds 作业清单：过滤条件逐列下传，
// 非法过滤值拒绝而不是当「不限」，越界页只回 total。
func TestListAggregationJobsFiltersAndBounds(t *testing.T) {
	d := newDeps(t)
	seedJob(t, d, "req-list", nil)
	total := int64(30)
	d.jobs.listTotal = &total

	t.Run("过滤条件与分页下传", func(t *testing.T) {
		d.jobs.seenFts = nil
		_, err := callListJobs(t, d, &rpc.ListAggregationJobsReq{
			JobType: rpc.JobType_JOB_TYPE_RECOMPUTE,
			State:   rpc.JobState_JOB_STATE_RUNNING, Since: 1234, Pn: 4, Ps: 5})
		if err != nil {
			t.Fatal(err)
		}
		want := model.JobFilter{JobType: model.JobTypeRecompute, State: model.JobStateRunning,
			Since: 1234, Offset: 15, Limit: 5}
		if len(d.jobs.seenFts) != 2 || d.jobs.seenFts[0] != want || d.jobs.seenFts[1] != want {
			t.Fatalf("下传条件=%v，期望两次都是 %+v", d.jobs.seenFts, want)
		}
	})
	t.Run("非法类型与状态拒绝", func(t *testing.T) {
		_, err := callListJobs(t, d, &rpc.ListAggregationJobsReq{JobType: rpc.JobType(8)})
		requireErrIs(t, err, model.ErrInvalidJobType, "越界 job_type")
		_, err = callListJobs(t, d, &rpc.ListAggregationJobsReq{State: rpc.JobState(9)})
		requireErrIs(t, err, model.ErrInvalidJobState, "越界 state")
		_, err = callListJobs(t, d, &rpc.ListAggregationJobsReq{Since: -1})
		requireErrIs(t, err, model.ErrInvalidJobState, "since 为负")
		_, err = callListJobs(t, d, &rpc.ListAggregationJobsReq{Ps: d.ctx.Config.Spm.MaxPageSize + 1})
		requireErrIs(t, err, model.ErrPsTooLarge, "ps 越界必须拒绝而不是 clamp")
	})
	t.Run("越界页只回 total", func(t *testing.T) {
		d.jobs.seenFts = nil
		d.jobs.listCalls = 0
		reply, err := callListJobs(t, d, &rpc.ListAggregationJobsReq{Pn: 20, Ps: 10})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Total != total {
			t.Fatalf("total=%d", reply.Total)
		}
		if len(reply.Jobs) != 0 || d.jobs.listCalls != 1 {
			t.Fatalf("越界页仍查了列表：%+v", reply)
		}
	})
	t.Run("深翻页保护", func(t *testing.T) {
		d.jobs.listCalls = 0
		if _, err := callListJobs(t, d, &rpc.ListAggregationJobsReq{Pn: 100_001, Ps: 10}); err != nil {
			t.Fatal(err)
		}
		if d.jobs.listCalls != 1 {
			t.Fatalf("OFFSET 已达 %d 量级仍查列表", int64(100000*10))
		}
	})
	t.Run("库故障上抛", func(t *testing.T) {
		d.jobs.errOnList = true
		_, err := callListJobs(t, d, &rpc.ListAggregationJobsReq{})
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		d.jobs.errOnList = false
	})
}
