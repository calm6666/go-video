package logic

// 读侧任务定义单测：GetTask 与 ListTasks。
//
// 这两个方法是运营后台与调度排障的眼睛，必须在无 MySQL 的条件下钉死四件事：
//   1. 入参非法（空白 task_key、越界 page_size、坏游标）必须在**碰库之前**拒绝，
//      否则一次拼错的深分页就是一次全表扫描（用 listCalls 轨迹证明零调用）；
//   2. 「不存在」必须是 ErrTaskNotFound 且不带 definition，绝不回空对象假装「查到了但没配」；
//   3. 投影逐列搬运（reflect 守卫保证「加列必然要补断言」）；
//   4. 游标分页的排序、has_more 与 total 三个概念各自独立，不能互相冒充。
//
// 未覆盖（登记在 README）：model 返回 error 的分支（fake 只会返回内存里的行，
// 造不出真库错误），以及 Cache *redis.Redis 相关分支（本仓无 miniredis 且禁止加依赖）。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/services/cron/model"
	"go-video/services/cron/rpc"
)

// seedTask 铺一条只关心过滤维度的任务定义（静默写入，不产生任何调用轨迹）。
func seedTask(db *fakeDB, id int64, taskKey string, state int32, group, handler string) *model.TaskDefinition {
	return seedDefinition(db, &model.TaskDefinition{
		ID: id, TaskKey: taskKey, Name: taskKey, Handler: handler, TaskGroup: group,
		ScheduleType: model.ScheduleTypeManual, State: state, Version: 1,
	})
}

// pageTaskKeys 把一页结果压成 task_key 序列，顺序即 model 返回顺序。
func pageTaskKeys(reply *rpc.ListTasksReply) []string {
	out := make([]string, 0, len(reply.GetList()))
	for _, d := range reply.GetList() {
		out = append(out, d.TaskKey)
	}
	return out
}

// equalStrings 逐位比较字符串切片（长度与顺序都必须一致）。
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestGetTaskRejectsBlankKeyWithoutTouchingDatabase(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	enabledDef(db)

	for _, tc := range []struct {
		name string
		in   *rpc.GetTaskReq
	}{
		{"nil 请求", nil},
		{"缺 task_key", &rpc.GetTaskReq{}},
		{"只有空白", &rpc.GetTaskReq{TaskKey: " \t "}},
	} {
		reply, err := NewGetTaskLogic(context.Background(), svcCtx).GetTask(tc.in)
		if !errors.Is(err, model.ErrTaskKeyEmpty) {
			t.Errorf("%s: err = %v，期望 ErrTaskKeyEmpty", tc.name, err)
		}
		if reply != nil {
			t.Errorf("%s: 拒绝时不得给出定义：%+v", tc.name, reply)
		}
	}
	// 判别性对照：同一上下文、同一数据，清掉空白后的合法 key 必须查到。
	ok, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: testTaskKey})
	if err != nil || ok.GetDefinition().GetTaskKey() != testTaskKey {
		t.Fatalf("对照用例应成功，得到 %+v / %v", ok, err)
	}
	// 读路径不开事务、不写审计。
	if db.txRuns != 0 || len(db.audits) != 0 {
		t.Errorf("GetTask 不该有写副作用：txRuns=%d audits=%d", db.txRuns, len(db.audits))
	}
}

func TestGetTaskTrimsKeyAndFailsLoudlyOnMissingRow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")
	enabledDef(db)

	// 两侧空白必须清掉后再查库：带着空格去查唯一键，真实存在的任务会「查无此人」。
	reply, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: "  " + testTaskKey + "  "})
	if err != nil {
		t.Fatalf("带空白的合法 key 应被清掉后命中：%v", err)
	}
	if reply.GetDefinition().GetTaskKey() != testTaskKey {
		t.Errorf("回包里 task_key 必须是清理后的值，得到 %q", reply.GetDefinition().GetTaskKey())
	}

	missing, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: "report.never_registered"})
	if !errors.Is(err, model.ErrTaskNotFound) {
		t.Errorf("err = %v，期望 ErrTaskNotFound", err)
	}
	if missing != nil {
		t.Errorf("不存在时不得回空 definition 假装「查到了但没配」：%+v", missing)
	}
}

// exportedFieldCount 数一个结构体的导出字段（protoimpl 的内部字段是私有的，天然排除）。
func exportedFieldCount(v any) int {
	typ := reflect.TypeOf(v)
	n := 0
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).PkgPath == "" {
			n++
		}
	}
	return n
}

// TestGetTaskProjectsEveryColumn 逐列核对 cron_task_definition → rpc.TaskDefinition 投影。
//
// 每个列都取互不相同的值：任意两列写串（例如把 lease_ttl_seconds 填进
// timeout_seconds）都会当场红。末尾的 reflect 守卫保证「表加了列、
// 投影漏了列」或「投影加了列、测试没跟上」都无法悄悄通过。
func TestGetTaskProjectsEveryColumn(t *testing.T) {
	row := &model.TaskDefinition{
		ID: 777, TaskKey: "rights.expire_scan", Name: "版权到期扫描", Handler: "rights.expire",
		TaskGroup: "rights", ScheduleType: model.ScheduleTypeCron, CronExpr: "0 3 * * *",
		IntervalSeconds: 3600, Timezone: "Asia/Shanghai", TimeoutSeconds: 601, MaxAttempts: 5,
		RetryBaseSeconds: 61, RetryMaxSeconds: 660, ConcurrencyLimit: 4, LeaseTTLSeconds: 121,
		MisfirePolicy: model.MisfirePolicyFireAll, MisfireBackfillLim: 7,
		Params: `{"days":30}`, SecretRefs: "TOKEN_A,TOKEN_B", State: model.TaskStatePaused,
		NextFireAt: 1_700_000_001, LastFireAt: 1_700_000_002, LastSuccessAt: 1_700_000_003,
		LastError: "downstream timeout", Version: 9, Owner: "rights-team", Operator: "ops.alice",
		Ctime: 1_700_000_000, Mtime: 1_700_000_005,
	}
	svcCtx, db := newTestSvc(t)
	seedDefinition(db, row)

	got, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: row.TaskKey})
	if err != nil {
		t.Fatal(err)
	}
	info := got.GetDefinition()
	if info == nil {
		t.Fatal("回包里没有 definition")
	}

	pairs := []struct {
		column string
		from   func(*model.TaskDefinition) any
		to     func(*rpc.TaskDefinition) any
	}{
		{"id → task_id", func(d *model.TaskDefinition) any { return d.ID }, func(g *rpc.TaskDefinition) any { return g.TaskId }},
		{"task_key", func(d *model.TaskDefinition) any { return d.TaskKey }, func(g *rpc.TaskDefinition) any { return g.TaskKey }},
		{"name", func(d *model.TaskDefinition) any { return d.Name }, func(g *rpc.TaskDefinition) any { return g.Name }},
		{"handler", func(d *model.TaskDefinition) any { return d.Handler }, func(g *rpc.TaskDefinition) any { return g.Handler }},
		{"task_group", func(d *model.TaskDefinition) any { return d.TaskGroup }, func(g *rpc.TaskDefinition) any { return g.TaskGroup }},
		{"schedule_type", func(d *model.TaskDefinition) any { return d.ScheduleType }, func(g *rpc.TaskDefinition) any { return int32(g.ScheduleType) }},
		{"cron_expr", func(d *model.TaskDefinition) any { return d.CronExpr }, func(g *rpc.TaskDefinition) any { return g.CronExpr }},
		{"interval_seconds", func(d *model.TaskDefinition) any { return d.IntervalSeconds }, func(g *rpc.TaskDefinition) any { return g.IntervalSeconds }},
		{"timezone", func(d *model.TaskDefinition) any { return d.Timezone }, func(g *rpc.TaskDefinition) any { return g.Timezone }},
		{"timeout_seconds", func(d *model.TaskDefinition) any { return d.TimeoutSeconds }, func(g *rpc.TaskDefinition) any { return g.TimeoutSeconds }},
		{"max_attempts", func(d *model.TaskDefinition) any { return d.MaxAttempts }, func(g *rpc.TaskDefinition) any { return g.MaxAttempts }},
		{"retry_base_seconds", func(d *model.TaskDefinition) any { return d.RetryBaseSeconds }, func(g *rpc.TaskDefinition) any { return g.RetryBaseSeconds }},
		{"retry_max_seconds", func(d *model.TaskDefinition) any { return d.RetryMaxSeconds }, func(g *rpc.TaskDefinition) any { return g.RetryMaxSeconds }},
		{"concurrency_limit", func(d *model.TaskDefinition) any { return d.ConcurrencyLimit }, func(g *rpc.TaskDefinition) any { return g.ConcurrencyLimit }},
		{"lease_ttl_seconds", func(d *model.TaskDefinition) any { return d.LeaseTTLSeconds }, func(g *rpc.TaskDefinition) any { return g.LeaseTtlSeconds }},
		{"misfire_policy", func(d *model.TaskDefinition) any { return d.MisfirePolicy }, func(g *rpc.TaskDefinition) any { return int32(g.MisfirePolicy) }},
		{"misfire_backfill_limit", func(d *model.TaskDefinition) any { return d.MisfireBackfillLim }, func(g *rpc.TaskDefinition) any { return g.MisfireBackfillLimit }},
		{"params", func(d *model.TaskDefinition) any { return d.Params }, func(g *rpc.TaskDefinition) any { return g.Params }},
		{"secret_refs", func(d *model.TaskDefinition) any { return d.SecretRefs }, func(g *rpc.TaskDefinition) any { return g.SecretRefs }},
		{"state", func(d *model.TaskDefinition) any { return d.State }, func(g *rpc.TaskDefinition) any { return int32(g.State) }},
		{"next_fire_at", func(d *model.TaskDefinition) any { return d.NextFireAt }, func(g *rpc.TaskDefinition) any { return g.NextFireAt }},
		{"last_fire_at", func(d *model.TaskDefinition) any { return d.LastFireAt }, func(g *rpc.TaskDefinition) any { return g.LastFireAt }},
		{"last_success_at", func(d *model.TaskDefinition) any { return d.LastSuccessAt }, func(g *rpc.TaskDefinition) any { return g.LastSuccessAt }},
		{"last_error", func(d *model.TaskDefinition) any { return d.LastError }, func(g *rpc.TaskDefinition) any { return g.LastError }},
		{"version", func(d *model.TaskDefinition) any { return d.Version }, func(g *rpc.TaskDefinition) any { return g.Version }},
		{"owner", func(d *model.TaskDefinition) any { return d.Owner }, func(g *rpc.TaskDefinition) any { return g.Owner }},
		{"operator", func(d *model.TaskDefinition) any { return d.Operator }, func(g *rpc.TaskDefinition) any { return g.Operator }},
		{"ctime", func(d *model.TaskDefinition) any { return d.Ctime }, func(g *rpc.TaskDefinition) any { return g.Ctime }},
		{"mtime", func(d *model.TaskDefinition) any { return d.Mtime }, func(g *rpc.TaskDefinition) any { return g.Mtime }},
	}
	for _, p := range pairs {
		if want, gotVal := p.from(row), p.to(info); want != gotVal {
			t.Errorf("%s：库里是 %v，投影成 %v", p.column, want, gotVal)
		}
	}
	if n := exportedFieldCount(model.TaskDefinition{}); n != len(pairs) {
		t.Errorf("cron_task_definition 有 %d 列，本用例只核对 %d 列", n, len(pairs))
	}
	if n := exportedFieldCount(rpc.TaskDefinition{}); n != len(pairs) {
		t.Errorf("rpc.TaskDefinition 有 %d 个字段，本用例只核对 %d 个", n, len(pairs))
	}

	// 数值枚举必须与 model 常量同义（不是「恰好数字相同」的巧合）。
	if info.ScheduleType != rpc.ScheduleType_SCHEDULE_TYPE_CRON ||
		info.State != rpc.TaskState_TASK_STATE_PAUSED ||
		info.MisfirePolicy != rpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ALL {
		t.Errorf("枚举投影错位：%+v", info)
	}
}

// TestGetTaskStillReadsWhenHandlerMissing 固定读侧边界：
// 「DB 有定义、本进程没实现」是部署缺口，只进日志与调度侧裁决，不改变查询结果。
// 否则运维在后台会连「有这条任务」都看不到，反而更难定位。
func TestGetTaskStillReadsWhenHandlerMissing(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)
	if svcCtx.Registry.Has("report.daily") {
		t.Fatal("本用例的前提是注册表里没有 report.daily")
	}

	before, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: testTaskKey})
	if err != nil {
		t.Fatalf("未注册 handler 仍要能读到定义：%v", err)
	}

	registerOK(t, svcCtx, "report.daily")
	after, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: testTaskKey})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.GetDefinition(), after.GetDefinition()) {
		t.Errorf("注册表状态不得改变读侧投影：\n未注册 %+v\n已注册 %+v", before.GetDefinition(), after.GetDefinition())
	}
}

func TestListTasksRejectsBadPagingBeforeTouchingModel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	enabledDef(db)

	cases := []struct {
		name    string
		in      *rpc.ListTasksReq
		wantErr error
	}{
		{"page_size 超上限", &rpc.ListTasksReq{PageSize: 101}, model.ErrInvalidPageLimit},
		{"游标不是数字", &rpc.ListTasksReq{Cursor: "abc"}, model.ErrInvalidCursor},
		{"游标是 0", &rpc.ListTasksReq{Cursor: "0"}, model.ErrInvalidCursor},
		{"游标是负数", &rpc.ListTasksReq{Cursor: "-7"}, model.ErrInvalidCursor},
	}
	for _, tc := range cases {
		reply, err := NewListTasksLogic(context.Background(), svcCtx).ListTasks(tc.in)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v，期望 %v", tc.name, err, tc.wantErr)
		}
		if reply != nil {
			t.Errorf("%s: 拒绝时不得回半页数据：%+v", tc.name, reply)
		}
	}
	// 全部拒绝都必须在入口完成：一次分页查询都不许发出去。
	if len(db.listCalls) != 0 {
		t.Errorf("非法分页参数导致 %d 次游标查询：%+v", len(db.listCalls), db.listCalls)
	}
	if db.txRuns != 0 {
		t.Errorf("列表读侧不该开事务：%d", db.txRuns)
	}
	// 对照项（放在零调用断言之后，避免把布景调用算进轨迹）：
	// nil 请求按「默认第一页」处理，绝不 panic 也不报错。
	if reply, err := NewListTasksLogic(context.Background(), svcCtx).ListTasks(nil); err != nil ||
		len(reply.GetList()) != 1 {
		t.Errorf("nil 请求应退化为默认第一页：%+v / %v", reply, err)
	}
}

// TestListTasksPassesPagingAndFiltersToModel 核对 logic 交给 model 的实参：
// limit 取自 svcCtx.PageSize（跟随配置，不是写死的 model.DefaultPageSize），
// cursor/state/group/handler 原样透传。
func TestListTasksPassesPagingAndFiltersToModel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedTask(db, 1, "a.one", model.TaskStateEnabled, "g1", "h.one")

	svcCtx.Config.Task.DefaultPageSize = 7
	if _, err := NewListTasksLogic(context.Background(), svcCtx).ListTasks(&rpc.ListTasksReq{}); err != nil {
		t.Fatal(err)
	}
	if len(db.listCalls) != 1 {
		t.Fatalf("应有 1 次游标查询，得到 %+v", db.listCalls)
	}
	if got := db.listCalls[0]; got.limit != 7 || got.cursorID != 0 || got.state != 0 ||
		got.group != "" || got.handler != "" {
		t.Errorf("未指定分页时的实参不对：%+v（limit 应取配置的默认 7）", got)
	}

	// 判别性对照：显式 page_size 覆盖默认值；显式过滤条件必须原样落到 SQL 实参。
	svcCtx.Config.Task.DefaultPageSize = 20
	if _, err := NewListTasksLogic(context.Background(), svcCtx).ListTasks(&rpc.ListTasksReq{
		PageSize: 3, Cursor: "42", State: rpc.TaskState_TASK_STATE_PAUSED,
		TaskGroup: "g1", Handler: "h.one",
	}); err != nil {
		t.Fatal(err)
	}
	want := listCall{state: model.TaskStatePaused, group: "g1", handler: "h.one", cursorID: 42, limit: 3}
	if got := db.listCalls[1]; got != want {
		t.Errorf("实参 = %+v，期望 %+v", got, want)
	}

	// 上限内的最大页要原样放行（越界项已在上一用例拒绝）。
	if _, err := NewListTasksLogic(context.Background(), svcCtx).
		ListTasks(&rpc.ListTasksReq{PageSize: 100}); err != nil {
		t.Fatalf("page_size=100（= config.MaxPageSize）应被放行：%v", err)
	}
	if got := db.listCalls[2]; got.limit != 100 {
		t.Errorf("limit = %d，期望 100", got.limit)
	}
}

// TestListTasksOrdersByIdAndStopsAtExhaustion 钉住游标分页的排序与 has_more 语义。
//
// 关键在于「满页必给游标」：model 只有在返回行数 < limit 时才确认没有下一页，
// 所以恰好翻到末尾的那一页仍会报 has_more=true，调用方要多翻一次拿到空页。
// 这是既有语义（total 才是真值），本用例把它钉住，避免日后被「顺手优化」成
// 少报下一页而让客户端漏数据。
func TestListTasksOrdersByIdAndStopsAtExhaustion(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	// 故意按 id 倒序铺数据：内存 map 的遍历序与 id 序相反，
	// 只有 model 的 ORDER BY id ASC 生效才能得到 a1..a4。
	seedTask(db, 4, "a4.task", model.TaskStateEnabled, "g", "h.four")
	seedTask(db, 3, "a3.task", model.TaskStateEnabled, "g", "h.three")
	seedTask(db, 2, "a2.task", model.TaskStateEnabled, "g", "h.two")
	seedTask(db, 1, "a1.task", model.TaskStateEnabled, "g", "h.one")

	l := NewListTasksLogic(context.Background(), svcCtx)
	page1, err := l.ListTasks(&rpc.ListTasksReq{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if keys := pageTaskKeys(page1); !equalStrings(keys, []string{"a1.task", "a2.task"}) {
		t.Fatalf("第一页顺序 = %v，期望按 id 升序 [a1.task a2.task]", keys)
	}
	if page1.NextCursor != "2" || !page1.HasMore || page1.Total != 4 {
		t.Errorf("第一页游标语义不符：cursor=%q hasMore=%v total=%d",
			page1.NextCursor, page1.HasMore, page1.Total)
	}

	page2, err := l.ListTasks(&rpc.ListTasksReq{PageSize: 2, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if keys := pageTaskKeys(page2); !equalStrings(keys, []string{"a3.task", "a4.task"}) {
		t.Fatalf("第二页 = %v", keys)
	}
	if page2.NextCursor != "4" || !page2.HasMore {
		t.Errorf("满页必须给游标（哪怕已到尾）：cursor=%q hasMore=%v", page2.NextCursor, page2.HasMore)
	}
	if page2.Total != 4 {
		t.Errorf("total 恒为过滤后的总行数，得到 %d", page2.Total)
	}

	page3, err := l.ListTasks(&rpc.ListTasksReq{PageSize: 2, Cursor: page2.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page3.List) != 0 || page3.NextCursor != "" || page3.HasMore || page3.Total != 4 {
		t.Errorf("尾后一页应为空且无游标：%+v", page3)
	}
	// 三页翻完，行集不重不漏。
	seen := map[string]bool{}
	for _, r := range [][]string{pageTaskKeys(page1), pageTaskKeys(page2), pageTaskKeys(page3)} {
		for _, k := range r {
			if seen[k] {
				t.Errorf("%s 被翻到两次：游标分页不重不漏被破坏", k)
			}
			seen[k] = true
		}
	}
	if len(seen) != 4 {
		t.Errorf("三页共看到 %d 条，期望 4", len(seen))
	}
}

func TestListTasksFiltersByStateGroupAndHandler(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedTask(db, 1, "e.report.a", model.TaskStateEnabled, "report", "h.a")
	seedTask(db, 2, "p.report.b", model.TaskStatePaused, "report", "h.b")
	seedTask(db, 3, "d.report.c", model.TaskStateDisabled, "report", "h.b")
	seedTask(db, 4, "e.other.d", model.TaskStateEnabled, "other", "h.b")

	l := NewListTasksLogic(context.Background(), svcCtx)
	cases := []struct {
		name string
		in   *rpc.ListTasksReq
		want []string
	}{
		{"全部状态", &rpc.ListTasksReq{}, []string{"e.report.a", "p.report.b", "d.report.c", "e.other.d"}},
		{"只要启用", &rpc.ListTasksReq{State: rpc.TaskState_TASK_STATE_ENABLED},
			[]string{"e.report.a", "e.other.d"}},
		{"只要暂停", &rpc.ListTasksReq{State: rpc.TaskState_TASK_STATE_PAUSED}, []string{"p.report.b"}},
		{"只要停用", &rpc.ListTasksReq{State: rpc.TaskState_TASK_STATE_DISABLED}, []string{"d.report.c"}},
		{"按分组", &rpc.ListTasksReq{TaskGroup: "report"},
			[]string{"e.report.a", "p.report.b", "d.report.c"}},
		{"按 handler", &rpc.ListTasksReq{Handler: "h.b"},
			[]string{"p.report.b", "d.report.c", "e.other.d"}},
		{"分组+状态+handler 三条件与", &rpc.ListTasksReq{
			State: rpc.TaskState_TASK_STATE_PAUSED, TaskGroup: "report", Handler: "h.b"},
			[]string{"p.report.b"}},
		{"无匹配", &rpc.ListTasksReq{TaskGroup: "nope"}, nil},
		// 契约里没有「非法状态」这一说：0 是全部，非 0 走等值过滤，凑不出来的值只会查空。
		{"表里没有的状态值查空", &rpc.ListTasksReq{State: rpc.TaskState(7)}, nil},
	}
	for _, tc := range cases {
		reply, err := l.ListTasks(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got := pageTaskKeys(reply)
		if !equalStrings(got, tc.want) {
			t.Errorf("%s：得到 %v，期望 %v", tc.name, got, tc.want)
		}
		if reply.Total != int64(len(tc.want)) {
			t.Errorf("%s：total = %d，期望 %d", tc.name, reply.Total, len(tc.want))
		}
	}
}

// TestListTasksTotalCountsAllRowsNotJustPage 证明 total 来自 CountByFilter 而不是
// 当页长度：否则「page_size=2 时 total 变成 2」会让后台的分页条数每秒都在骗人。
func TestListTasksTotalCountsAllRowsNotJustPage(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedTask(db, 1, "t.one", model.TaskStateEnabled, "g", "h.a")
	seedTask(db, 2, "t.two", model.TaskStateEnabled, "g", "h.a")
	seedTask(db, 3, "t.three", model.TaskStateEnabled, "g", "h.a")

	reply, err := NewListTasksLogic(context.Background(), svcCtx).
		ListTasks(&rpc.ListTasksReq{PageSize: 2, TaskGroup: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.List) != 2 {
		t.Fatalf("当页应只有 2 行，得到 %d", len(reply.List))
	}
	if reply.Total != 3 {
		t.Errorf("total = %d，期望过滤后的 3（不是当页 2）", reply.Total)
	}
}

// TestListTasksProjectsSameColumnsAsGetTask 两个读接口的投影必须同源同值：
// 列表页与详情页对同一任务给出不同数字，运维就会按错误的那份做决策。
func TestListTasksProjectsSameColumnsAsGetTask(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedDefinition(db, &model.TaskDefinition{
		ID: 9, TaskKey: "report.weekly", Name: "周报", Handler: "h.weekly", TaskGroup: "report",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "Asia/Shanghai",
		State: model.TaskStateEnabled, NextFireAt: 1_700_000_009, Version: 11,
	})

	l := NewListTasksLogic(context.Background(), svcCtx)
	listed, err := l.ListTasks(&rpc.ListTasksReq{Handler: "h.weekly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.List) != 1 {
		t.Fatalf("过滤后应有 1 行，得到 %d", len(listed.List))
	}
	got, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: "report.weekly"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listed.List[0], got.Definition) {
		t.Errorf("两个读接口的投影不一致：\n列表 %+v\n详情 %+v", listed.List[0], got.Definition)
	}
	if listed.List[0].TaskId != 9 {
		t.Errorf("列表投影丢了主键 task_id=%d", listed.List[0].TaskId)
	}
}
