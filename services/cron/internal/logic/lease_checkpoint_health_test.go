package logic

// 读侧「租约 / 游标 / 健康度」单测：GetLease、ListLeases、GetCheckpoint、
// ListCheckpoints、GetSchedulerHealth。
//
// 这五个方法决定运维能不能回答「谁在跑这个任务、还能跑多久」「增量跑到哪了」
// 「这个分组是不是积压了」，因此钉的是：
//   1. 缺失语义用 found=false 表达（与 GetTaskRun 的 ErrRunNotFound 恰好相反），
//      而「已释放的租约」与「从未 claim 过」必须能在回包里区分开；
//   2. lease_key 的拼接口径（task_key + "/" + scope）与两侧空白清理；
//   3. 游标分页的复合游标（task_key \x1f scope_key）非法即报错，绝不退化成首页；
//   4. only_expired 的判定用服务端时钟，且 expire_at=0（已释放）既不算持有也不算过期；
//   5. 健康度是分组的聚合视图：聚合口径、分组过滤、扫描上限、时钟回显各有各自的坑。
//
// 与另两个读侧文件同一手法：用 model 实参轨迹证明「拒绝发生在触库之前」，
// 用 *Sentinel* 用例钉住当前不成立但文档声称成立的语义。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go-video/services/cron/model"
	"go-video/services/cron/rpc"
)

// groupFacts 把一行的健康度压成稳定文本，用于逐字段比对（proto 消息直接 %+v 会带内部状态）。
func groupFacts(g *rpc.GroupHealth) string {
	return fmt.Sprintf("group=%s enabled=%d paused=%d due=%d running=%d retrying=%d failed1h=%d expired=%d oldest=%d",
		g.GetTaskGroup(), g.GetEnabledTasks(), g.GetPausedTasks(), g.GetDueBacklog(), g.GetRunning(),
		g.GetRetrying(), g.GetFailedLastHour(), g.GetExpiredLeases(), g.GetOldestDuePlannedAt())
}

// healthFacts 是整个回包的稳定摘要。
func healthFacts(reply *rpc.GetSchedulerHealthReply) string {
	out := fmt.Sprintf("server_time=%d groups=[", reply.GetServerTime())
	for i, g := range reply.GetGroups() {
		if i > 0 {
			out += " | "
		}
		out += groupFacts(g)
	}
	return out + "]"
}

// --- GetCheckpoint ---

func TestGetCheckpointRejectsBlankKeyAndReportsMissingCursorAsNotFound(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCursor(db, "index.rebuild", "shard-3", 88, "v-2026")

	logic := NewGetCheckpointLogic(context.Background(), svcCtx)
	for _, tc := range []struct {
		name string
		in   *rpc.GetCheckpointReq
	}{
		{"nil 请求", nil},
		{"缺 task_key", &rpc.GetCheckpointReq{}},
		{"只有空白", &rpc.GetCheckpointReq{TaskKey: " \t ", ScopeKey: "shard-3"}},
	} {
		reply, err := logic.GetCheckpoint(tc.in)
		if !errors.Is(err, model.ErrTaskKeyEmpty) {
			t.Errorf("%s: err = %v，期望 ErrTaskKeyEmpty", tc.name, err)
		}
		if reply != nil {
			t.Errorf("%s: 拒绝时不得回游标：%+v", tc.name, reply)
		}
	}
	if len(db.readCalls) != 0 {
		t.Errorf("空白 task_key 不得触库：%v", db.readCalls)
	}

	// 「从未推进过游标」是增量任务的正常起点，不是错误。
	missing, err := logic.GetCheckpoint(&rpc.GetCheckpointReq{TaskKey: "index.rebuild", ScopeKey: "shard-9"})
	if err != nil {
		t.Fatalf("查无游标不该报错：%v", err)
	}
	if missing.GetFound() {
		t.Errorf("不存在的 scope 必须 found=false：%+v", missing)
	}
	if missing.GetCheckpoint() != nil {
		t.Errorf("found=false 时不得带游标对象：%+v", missing.GetCheckpoint())
	}
	// 判别性对照：换成分组里真实存在的 scope 就必须读到。
	found, err := logic.GetCheckpoint(&rpc.GetCheckpointReq{TaskKey: "index.rebuild", ScopeKey: "shard-3"})
	if err != nil || !found.GetFound() || found.GetCheckpoint().GetValue() != 88 {
		t.Fatalf("对照用例应命中游标：%+v / %v", found, err)
	}
}

func TestGetCheckpointTrimsTaskKeyButKeepsScopeVerbatim(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCursor(db, "media.scan", "region=cn", 7, "")
	logic := NewGetCheckpointLogic(context.Background(), svcCtx)

	// task_key 两侧空白必须清掉，否则带着空格去撞 uniq_task_scope 会「查无此人」。
	got, err := logic.GetCheckpoint(&rpc.GetCheckpointReq{TaskKey: "  media.scan\t", ScopeKey: "region=cn"})
	if err != nil || !got.GetFound() {
		t.Fatalf("task_key 应被清理后再查库：%+v / %v", got, err)
	}
	// scope_key 与写入侧（checkpointFromProto 不 trim scope_key）同一口径：原样匹配。
	if scope, err := logic.GetCheckpoint(&rpc.GetCheckpointReq{
		TaskKey: "media.scan", ScopeKey: " region=cn "}); err != nil || scope.GetFound() {
		t.Errorf("scope_key 必须与落库值原样匹配（不 trim），found=%t err=%v", scope.GetFound(), err)
	}
	if got, want := fmt.Sprint(db.readCalls[len(db.readCalls)-2:]),
		"[checkpoint:media.scan/region=cn checkpoint:media.scan/ region=cn ]"; got != want {
		t.Errorf("实参轨迹不对：%s", got)
	}

	// 默认游标（scope 为空串）是独立的一行，不能被当成「查无」。
	seedCursor(db, "media.scan", "", 1, "alias-default")
	def, err := logic.GetCheckpoint(&rpc.GetCheckpointReq{TaskKey: "media.scan"})
	if err != nil || !def.GetFound() || def.GetCheckpoint().GetValueStr() != "alias-default" {
		t.Errorf("空 scope 是合法的默认游标行：%+v / %v", def, err)
	}
}

func TestGetCheckpointProjectsEveryColumn(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedCursor(db, "report.job", "shard-1", 4242, "pt=2026-09-01")
	db.checkpoints[checkpointKey("report.job", "shard-1")].Version = 9
	db.checkpoints[checkpointKey("report.job", "shard-1")].Operator = "erin"
	db.checkpoints[checkpointKey("report.job", "shard-1")].Ctime = now - 100
	db.checkpoints[checkpointKey("report.job", "shard-1")].Mtime = now - 5

	reply, err := NewGetCheckpointLogic(context.Background(), svcCtx).
		GetCheckpoint(&rpc.GetCheckpointReq{TaskKey: "report.job", ScopeKey: "shard-1"})
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	got := reply.GetCheckpoint()
	src := db.checkpoints[checkpointKey("report.job", "shard-1")]
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"task_key", got.GetTaskKey(), src.TaskKey},
		{"scope_key", got.GetScopeKey(), src.ScopeKey},
		{"value", got.GetValue(), src.Value},
		{"value_str", got.GetValueStr(), src.ValueStr},
		{"version", got.GetVersion(), src.Version},
		{"operator", got.GetOperator(), src.Operator},
		{"ctime", got.GetCtime(), src.Ctime},
		{"mtime", got.GetMtime(), src.Mtime},
	} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("列 %s：got=%v want=%v", c.name, c.got, c.want)
		}
	}
	if n, want := exportedFieldCount(rpc.Checkpoint{}), 8; n != want {
		t.Errorf("rpc.Checkpoint 导出字段数 = %d，本用例覆盖 %d 个；proto 加列必须补断言", n, want)
	}
	if db.txRuns != 0 || len(db.audits) != 0 {
		t.Errorf("GetCheckpoint 不该有写副作用：txRuns=%d audits=%d", db.txRuns, len(db.audits))
	}
}

// --- ListCheckpoints ---

func TestListCheckpointsRejectsBadPagingBeforeTouchingModel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCursor(db, "a.job", "s1", 1, "")
	logic := NewListCheckpointsLogic(context.Background(), svcCtx)

	if _, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{PageSize: 101}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("page_size 越界应报 ErrInvalidPageLimit，得到 %v", err)
	}
	for _, bad := range []string{"no-separator", "-1", "\x1fshard-only", "  \x1f "} {
		if _, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{Cursor: bad}); !errors.Is(err, model.ErrInvalidCursor) {
			t.Errorf("坏复合游标 %q: err = %v，期望 ErrInvalidCursor", bad, err)
		}
	}
	if len(db.cpCalls) != 0 {
		t.Errorf("非法分页/游标不得触库：%+v", db.cpCalls)
	}
	// 顺序：page_size 先于 cursor（坏 page_size + 坏游标时报分页）。
	if _, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{
		PageSize: 999, Cursor: "nope"}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("page_size 必须最先被拒，得到 %v", err)
	}
	// 空白游标是「从头开始」；task_key 段有值的游标合法。
	if _, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{Cursor: "  "}); err != nil {
		t.Errorf("空白游标应按首页处理，得到 %v", err)
	}
	if _, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{Cursor: "a.job\x1f"}); err != nil {
		t.Errorf("scope 为空的复合游标应合法，得到 %v", err)
	}
	if _, err := logic.ListCheckpoints(nil); err != nil {
		t.Errorf("nil 请求应按默认处理，得到 %v", err)
	}
}

func TestListCheckpointsPagesWithCompositeCursorAscending(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	// 故意乱序铺：输出必须按 (task_key, scope_key) 升序，游标是「严格大于」。
	seedCursor(db, "b.job", "s2", 2, "")
	seedCursor(db, "a.job", "s2", 2, "")
	seedCursor(db, "b.job", "s1", 2, "")
	seedCursor(db, "a.job", "s1", 2, "")
	logic := NewListCheckpointsLogic(context.Background(), svcCtx)

	first, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{PageSize: 3})
	if err != nil {
		t.Fatalf("第一页：%v", err)
	}
	if got, want := fmt.Sprint(cpKeys(first)), "[a.job/s1 a.job/s2 b.job/s1]"; got != want {
		t.Fatalf("第一页 = %s，期望 %s", got, want)
	}
	if first.GetNextCursor() != model.CompositeCursor("b.job", "s1") || !first.GetHasMore() {
		t.Errorf("满页必须给出复合游标 (task_key, scope_key)：cursor=%q has_more=%t",
			first.GetNextCursor(), first.GetHasMore())
	}
	if first.GetTotal() != 4 {
		t.Errorf("total 是全量行数，得到 %d", first.GetTotal())
	}

	second, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{
		PageSize: 3, Cursor: first.GetNextCursor()})
	if err != nil {
		t.Fatalf("第二页：%v", err)
	}
	// 游标行本身不得重复出现（严格大于，不是大于等于）。
	if got, want := fmt.Sprint(cpKeys(second)), "[b.job/s2]"; got != want {
		t.Errorf("第二页 = %s，期望 %s", got, want)
	}
	if second.GetHasMore() || second.GetNextCursor() != "" {
		t.Errorf("不满页不得给出游标：cursor=%q has_more=%t",
			second.GetNextCursor(), second.GetHasMore())
	}
	if second.GetTotal() != 4 {
		t.Errorf("total 不随游标变化，得到 %d", second.GetTotal())
	}
	call := db.cpCalls[len(db.cpCalls)-1]
	if call.cursorKey != model.CompositeCursor("b.job", "s1") || call.limit != 3 || call.taskKey != "" {
		t.Errorf("游标与分页实参必须原样下推给 model：%+v", call)
	}

	// task_key 过滤：total 与当页同一套条件。
	one, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{TaskKey: "a.job"})
	if err != nil {
		t.Fatalf("按任务过滤：%v", err)
	}
	if got, want := fmt.Sprint(cpKeys(one)), "[a.job/s1 a.job/s2]"; got != want {
		t.Errorf("a.job 集合 = %s，期望 %s", got, want)
	}
	if one.GetTotal() != 2 {
		t.Errorf("total 要跟着 task_key 收敛，得到 %d", one.GetTotal())
	}
	// 页大小缺省走 config.Task.DefaultPageSize。
	if _, err := logic.ListCheckpoints(&rpc.ListCheckpointsReq{}); err != nil {
		t.Fatalf("默认分页：%v", err)
	}
	if got := db.cpCalls[len(db.cpCalls)-1].limit; got != 20 {
		t.Errorf("page_size=0 应兜到 20，得到 %d", got)
	}
}

func cpKeys(reply *rpc.ListCheckpointsReply) []string {
	out := make([]string, 0, len(reply.GetList()))
	for _, c := range reply.GetList() {
		out = append(out, c.GetTaskKey()+"/"+c.GetScopeKey())
	}
	return out
}

// --- GetLease ---

func TestGetLeaseDistinguishesNeverClaimedFromReleased(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	logic := NewGetLeaseLogic(context.Background(), svcCtx)

	for _, tc := range []struct {
		name string
		in   *rpc.GetLeaseReq
	}{
		{"nil 请求", nil},
		{"缺 task_key", &rpc.GetLeaseReq{}},
		{"只有空白", &rpc.GetLeaseReq{TaskKey: " \t ", Scope: "s1"}},
	} {
		reply, err := logic.GetLease(tc.in)
		if !errors.Is(err, model.ErrTaskKeyEmpty) {
			t.Errorf("%s: err = %v，期望 ErrTaskKeyEmpty", tc.name, err)
		}
		if reply != nil {
			t.Errorf("%s: 拒绝时不得回租约：%+v", tc.name, reply)
		}
	}
	if len(db.readCalls) != 0 {
		t.Errorf("空白 task_key 不得触库：%v", db.readCalls)
	}

	// 从未 claim 过：found=false（与 GetTaskRun 的 ErrRunNotFound 是两种不同语义）。
	never, err := logic.GetLease(&rpc.GetLeaseReq{TaskKey: "never.claimed"})
	if err != nil {
		t.Fatalf("查无租约不该报错：%v", err)
	}
	if never.GetFound() || never.GetLease() != nil {
		t.Errorf("从未 claim 过必须 found=false 且不带对象：%+v", never)
	}

	// 已释放的租约是「有这一行、只是没人持有」：found=true + owner 空 + expire_at=0。
	released := seedLease(db, "released.task", "released.task", "", 5, 0, 0)
	got, err := logic.GetLease(&rpc.GetLeaseReq{TaskKey: "released.task"})
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if !got.GetFound() {
		t.Fatal("已释放的租约必须 found=true（它带着 fence_token=5，与从未 claim 完全不同）")
	}
	if got.GetLease().GetOwner() != "" || got.GetLease().GetExpireAt() != 0 {
		t.Errorf("已释放租约不得带持有者/到期时间：%+v", got.GetLease())
	}
	if got.GetLease().GetFenceToken() != released.FenceToken {
		t.Errorf("fence_token 必须透出（它永不回退）：got=%d want=%d",
			got.GetLease().GetFenceToken(), released.FenceToken)
	}
	// takeover_count 是「实例频繁崩溃」的唯一线索，必须原样透出。
	if got.GetLease().GetTakeoverCount() != released.TakeoverCount {
		t.Errorf("takeover_count 未透出：%+v", got.GetLease())
	}
}

func TestGetLeaseBuildsKeyFromTaskAndScope(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedLease(db, "shard.task/shard-7", "shard.task", "worker-3", 2, fakeNow()+60, 501)
	seedLease(db, "plain.task", "plain.task", "worker-1", 9, fakeNow()+60, 502)
	logic := NewGetLeaseLogic(context.Background(), svcCtx)

	// 分片任务的租约键是 task_key + "/" + scope；两侧空白都要清掉。
	scoped, err := logic.GetLease(&rpc.GetLeaseReq{TaskKey: " shard.task ", Scope: " shard-7 "})
	if err != nil {
		t.Fatalf("带 scope 的租约查询：%v", err)
	}
	if !scoped.GetFound() || scoped.GetLease().GetLeaseKey() != "shard.task/shard-7" {
		t.Fatalf("lease_key 拼接口径不对：%+v", scoped.GetLease())
	}
	if scoped.GetLease().GetOwner() != "worker-3" {
		t.Errorf("持有者未透出：%+v", scoped.GetLease())
	}
	// scope 留空即任务级全局键（等于 task_key 本身）。
	plain, err := logic.GetLease(&rpc.GetLeaseReq{TaskKey: "plain.task"})
	if err != nil || !plain.GetFound() || plain.GetLease().GetLeaseKey() != "plain.task" {
		t.Fatalf("任务级租约键不对：%+v / %v", plain.GetLease(), err)
	}
	// 判别性对照：同一 task_key 换个 scope 就是另一把锁，查不到即 found=false。
	other, err := logic.GetLease(&rpc.GetLeaseReq{TaskKey: "shard.task", Scope: "shard-8"})
	if err != nil || other.GetFound() {
		t.Errorf("不同 scope 必须是独立租约：%+v / %v", other, err)
	}
	if got, want := fmt.Sprint(db.readCalls),
		"[lease:shard.task/shard-7 lease:plain.task lease:shard.task/shard-8]"; got != want {
		t.Errorf("键拼接轨迹 = %s，期望 %s", got, want)
	}
}

func TestGetLeaseProjectsEveryColumn(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedLease(db, "conv.task/s1", "conv.task", "worker-7", 41, now+30, 900)
	row := db.leases["conv.task/s1"]
	row.TakeoverCount = 3
	row.AcquiredAt = now - 120
	row.Ctime = now - 300

	reply, err := NewGetLeaseLogic(context.Background(), svcCtx).
		GetLease(&rpc.GetLeaseReq{TaskKey: "conv.task", Scope: "s1"})
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	got := reply.GetLease()
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"lease_key", got.GetLeaseKey(), row.LeaseKey},
		{"owner", got.GetOwner(), row.OwnerInstance},
		{"fence_token", got.GetFenceToken(), row.FenceToken},
		{"expire_at", got.GetExpireAt(), row.ExpireAt},
		{"acquired_at", got.GetAcquiredAt(), row.AcquiredAt},
		{"takeover_count", got.GetTakeoverCount(), row.TakeoverCount},
	} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("列 %s：got=%v want=%v", c.name, c.got, c.want)
		}
	}
	// cron_task_lease 还有 task_key / scope / run_id / renewed_at / ctime / mtime 六列
	// 没有透出：LeaseInfo 只到 6 个字段，「这把锁关联哪次执行」只能回表查（README 契约缺口）。
	if n, want := exportedFieldCount(rpc.LeaseInfo{}), 6; n != want {
		t.Errorf("rpc.LeaseInfo 导出字段数 = %d，本用例覆盖 %d 个；proto 加列必须补断言", n, want)
	}
}

// --- ListLeases ---

func leaseKeysOf(reply *rpc.ListLeasesReply) []string {
	out := make([]string, 0, len(reply.GetList()))
	for _, l := range reply.GetList() {
		out = append(out, l.GetLeaseKey())
	}
	return out
}

func TestListLeasesRejectsBadPagingAndUsesServerClock(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedLease(db, "l.task", "l.task", "w1", 1, fakeNow()+60, 0)
	logic := NewListLeasesLogic(context.Background(), svcCtx)

	if _, err := logic.ListLeases(&rpc.ListLeasesReq{PageSize: 101}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("page_size 越界应报 ErrInvalidPageLimit，得到 %v", err)
	}
	if _, err := logic.ListLeases(&rpc.ListLeasesReq{Cursor: "not-a-number"}); !errors.Is(err, model.ErrInvalidCursor) {
		t.Errorf("坏游标应报 ErrInvalidCursor，得到 %v", err)
	}
	if _, err := logic.ListLeases(&rpc.ListLeasesReq{
		PageSize: 999, Cursor: "x"}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("page_size 必须最先被拒，得到 %v", err)
	}
	if len(db.leaseCalls) != 0 {
		t.Errorf("非法分页/游标不得触库：%+v", db.leaseCalls)
	}

	before := fakeNow()
	if _, err := logic.ListLeases(nil); err != nil {
		t.Fatalf("nil 请求应按默认处理：%v", err)
	}
	call := db.leaseCalls[len(db.leaseCalls)-1]
	if call.now < before || call.now > fakeNow() {
		t.Errorf("now=0 必须取服务端时钟（否则各副本对「能否抢占」判定不一致）：%+v", call)
	}
	// 显式 now 原样使用；<=0 一律回落服务端时钟。
	if _, err := logic.ListLeases(&rpc.ListLeasesReq{Now: -5}); err != nil {
		t.Fatalf("负 now 应按未提供处理：%v", err)
	}
	if got := db.leaseCalls[len(db.leaseCalls)-1].now; got < before {
		t.Errorf("负 now 未回落服务端时钟：%d", got)
	}
	if _, err := logic.ListLeases(&rpc.ListLeasesReq{Now: 123456}); err != nil {
		t.Fatalf("显式 now：%v", err)
	}
	if got := db.leaseCalls[len(db.leaseCalls)-1].now; got != 123456 {
		t.Errorf("显式 now 必须原样下推，得到 %d", got)
	}
}

func TestListLeasesOnlyExpiredFilterAndOrdering(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	held := seedLease(db, "held.task", "held.task", "w1", 1, now+300, 0)
	expired := seedLease(db, "expired.task", "expired.task", "w2", 2, now-30, 0)
	edge := seedLease(db, "edge.task", "edge.task", "w3", 3, now, 0)
	idle := seedLease(db, "idle.task", "idle.task", "", 4, 0, 0) // 已释放

	logic := NewListLeasesLogic(context.Background(), svcCtx)
	all, err := logic.ListLeases(&rpc.ListLeasesReq{Now: now, PageSize: 10})
	if err != nil {
		t.Fatalf("全量列表：%v", err)
	}
	// 排序按 expire_at 升序：先看最快要过期的；已释放的（0）排在最前但既不持有也不算过期。
	want := fmt.Sprint([]string{idle.LeaseKey, expired.LeaseKey, edge.LeaseKey, held.LeaseKey})
	if got := fmt.Sprint(leaseKeysOf(all)); got != want {
		t.Errorf("expire_at 升序 = %s，期望 %s", got, want)
	}
	if all.GetTotal() != 4 {
		t.Errorf("total = %d，期望 4", all.GetTotal())
	}

	only, err := logic.ListLeases(&rpc.ListLeasesReq{Now: now, OnlyExpired: true, PageSize: 10})
	if err != nil {
		t.Fatalf("只看过期：%v", err)
	}
	// 过期判定是 expire_at > 0 AND expire_at <= now：边界（恰等于 now）算过期，
	// 已释放（expire_at=0）与未过期都不算。
	want = fmt.Sprint([]string{expired.LeaseKey, edge.LeaseKey})
	if got := fmt.Sprint(leaseKeysOf(only)); got != want {
		t.Errorf("过期集合 = %s，期望 %s", got, want)
	}
	if only.GetTotal() != 2 {
		t.Errorf("total 必须与 only_expired 同一口径，得到 %d", only.GetTotal())
	}
	if call := db.leaseCalls[len(db.leaseCalls)-1]; !call.onlyExpired || call.now != now {
		t.Errorf("only_expired 与 now 必须下推：%+v", call)
	}
	// task_key 过滤与 only_expired 正交。
	one, err := logic.ListLeases(&rpc.ListLeasesReq{TaskKey: "held.task", Now: now, OnlyExpired: true})
	if err != nil {
		t.Fatalf("按任务过滤过期租约：%v", err)
	}
	if len(one.GetList()) != 0 || one.GetTotal() != 0 || one.GetHasMore() {
		t.Errorf("未过期的任务在 only_expired 下应为空：%+v", one)
	}
}

// TestListLeasesCursorSkipsRowsSentinel 钉住租约分页的一处漏读 + 重复：
// model 侧是 `ORDER BY expire_at ASC, id ASC` 但游标条件只有 `id > cursor`，
// 一旦 expire_at 的顺序与 id 的顺序不一致（续租随时在改 expire_at，必然发生），
// 翻页就会既漏掉 id 较小的行、又把 id 较大的行再给一遍。
// 修好那天（游标改成 (expire_at, id) 复合，或与 ListCheckpoints 同一套编码）本用例会红。
func TestListLeasesCursorSkipsRowsSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	// id 递增但过期程度递减：expire_at 升序的结果与 id 升序正好相反。
	s1 := seedLease(db, "s1.task", "s1.task", "w1", 1, now-10, 0)
	s2 := seedLease(db, "s2.task", "s2.task", "w2", 1, now-20, 0)
	s3 := seedLease(db, "s3.task", "s3.task", "w3", 1, now-30, 0)
	logic := NewListLeasesLogic(context.Background(), svcCtx)

	first, err := logic.ListLeases(&rpc.ListLeasesReq{Now: now, OnlyExpired: true, PageSize: 2})
	if err != nil {
		t.Fatalf("第一页：%v", err)
	}
	if got, want := fmt.Sprint(leaseKeysOf(first)),
		fmt.Sprint([]string{s3.LeaseKey, s2.LeaseKey}); got != want {
		t.Fatalf("第一页 = %s，期望 %s", got, want)
	}
	if first.GetNextCursor() != fmt.Sprint(s2.ID) || !first.GetHasMore() {
		t.Fatalf("游标必须是本页最后一行的 id：%+v", first)
	}
	if first.GetTotal() != 3 {
		t.Fatalf("total=%d，用例前提（三行都已过期）不成立", first.GetTotal())
	}

	second, err := logic.ListLeases(&rpc.ListLeasesReq{
		Now: now, OnlyExpired: true, PageSize: 2, Cursor: first.GetNextCursor()})
	if err != nil {
		t.Fatalf("第二页：%v", err)
	}
	// 期望写成切片形态与 leaseKeysOf 的 fmt.Sprint 同口径；漏掉中括号会让这条
	// 永远红（"[s3.task]" != "s3.task"），掩盖它想钉的重复读。
	if got, want := fmt.Sprint(leaseKeysOf(second)), fmt.Sprint([]string{s3.LeaseKey}); got != want {
		t.Errorf("当前现实：第二页把第一页最后一行又给了一遍（got=%s，期望 %s）", got, want)
	}
	seen := map[string]bool{}
	for _, k := range append(leaseKeysOf(first), leaseKeysOf(second)...) {
		seen[k] = true
	}
	if seen[s1.LeaseKey] {
		t.Error("翻完两页就读到了 s1：游标漏读缺口已修复，请改断言并撤销 README 缺口")
	}
	if len(seen) != 2 {
		t.Errorf("两页应覆盖到的行数已变（当前现实是 2/3）：%v", seen)
	}
	// 再翻一页也不会补回来：游标走到 s3 的 id 就没有更大的行了，
	// 而 s1（id 更小、但更晚过期）对分页调用方永久不可见。
	third, err := logic.ListLeases(&rpc.ListLeasesReq{
		Now: now, OnlyExpired: true, PageSize: 2, Cursor: fmt.Sprint(s3.ID)})
	if err != nil {
		t.Fatalf("第三页：%v", err)
	}
	if len(third.GetList()) != 0 || third.GetHasMore() {
		t.Errorf("游标推到最大 id 后应为末页：%+v", leaseKeysOf(third))
	}
}

// --- GetSchedulerHealth ---

// seedHealthFixture 铺一个「两个分组、各类事实各来一点」的最小现场，返回铺完后的当前秒。
func seedHealthFixture(t *testing.T, db *fakeDB) int64 {
	t.Helper()
	now := fakeNow()
	def := func(id int64, key, handler, group string, state int32, nextFireAt int64) {
		seedDefinition(db, &model.TaskDefinition{
			ID: id, TaskKey: key, Name: key, Handler: handler, TaskGroup: group,
			ScheduleType: model.ScheduleTypeManual, State: state, TimeoutSeconds: 60,
			MaxAttempts: 1, ConcurrencyLimit: 1, LeaseTTLSeconds: 300, Version: 1,
			NextFireAt: nextFireAt,
		})
	}
	def(1, "a.one", "h.alpha", "alpha", model.TaskStateEnabled, now-30)
	def(2, "a.two", "h.alpha", "alpha", model.TaskStateEnabled, now-10)
	def(3, "a.three", "h.alpha", "alpha", model.TaskStatePaused, 0)
	def(4, "b.one", "h.beta", "beta", model.TaskStateDisabled, 0)
	// 执行侧：一小时内 running / retrying / failed 各一条，一条 failed 在窗口外。
	seedRun(db, &model.TaskRun{ID: 11, TaskKey: "a.one", PlannedAt: now - 60, Attempt: 1,
		State: model.RunStateRunning, Ctime: now - 5})
	seedRun(db, &model.TaskRun{ID: 12, TaskKey: "a.one", PlannedAt: now - 61, Attempt: 2,
		State: model.RunStateRetrying, Ctime: now - 6})
	seedRun(db, &model.TaskRun{ID: 13, TaskKey: "a.two", PlannedAt: now - 62, Attempt: 1,
		State: model.RunStateFailed, Ctime: now - 7})
	seedRun(db, &model.TaskRun{ID: 14, TaskKey: "a.two", PlannedAt: now - 7000, Attempt: 1,
		State: model.RunStateFailed, Ctime: now - 7000})
	// 定义已被删掉的孤儿执行：JOIN 之后不得计入任何分组。
	seedRun(db, &model.TaskRun{ID: 15, TaskKey: "gone.task", PlannedAt: now - 63, Attempt: 1,
		State: model.RunStateFailed, Ctime: now - 8})
	// 租约：过期且有主 1 条；过期但无主（已释放）与未过期各 1 条都不算「失联遗留」。
	seedLease(db, "a.one", "a.one", "w-dead", 3, now-5, 11)
	seedLease(db, "a.two", "a.two", "", 3, 0, 0)
	seedLease(db, "b.one", "b.one", "w-alive", 1, now+300, 0)
	return now
}

func TestGetSchedulerHealthAggregatesPerGroup(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := seedHealthFixture(t, db)

	reply, err := NewGetSchedulerHealthLogic(context.Background(), svcCtx).
		GetSchedulerHealth(&rpc.GetSchedulerHealthReq{Now: now})
	if err != nil {
		t.Fatalf("GetSchedulerHealth: %v", err)
	}
	// 分组按名字升序稳定输出，巡检脚本可以直接 diff。
	want := fmt.Sprintf(
		"server_time=%d groups=[group=alpha enabled=2 paused=1 due=2 running=1 retrying=1 "+
			"failed1h=1 expired=1 oldest=%d | group=beta enabled=0 paused=0 due=0 running=0 "+
			"retrying=0 failed1h=0 expired=0 oldest=0]", now, now-30)
	if got := healthFacts(reply); got != want {
		t.Errorf("健康度聚合结果不对：\n got=%s\nwant=%s", got, want)
	}
	if reply.GetVersion() == "" {
		t.Errorf("version 必须带上，便于排障定位镜像")
	}
	if db.txRuns != 0 || len(db.audits) != 0 {
		t.Errorf("健康度是只读的：txRuns=%d audits=%d", db.txRuns, len(db.audits))
	}
	// 只有 DISABLED 任务的分组也要出现在清单里（全零），
	// 否则「这个分组整个消失了」会被读成「这个分组没配过任务」。
	if len(reply.GetGroups()) != 2 {
		t.Fatalf("分组数 = %d，期望 2：%s", len(reply.GetGroups()), healthFacts(reply))
	}
	// 一小时窗口是真的按 ctime 收敛的：窗口内 1 条 failed，窗口外那条不算。
	// 判别性对照：把窗口起点推到 7000 秒前之后两条都该在（用 now 前移模拟）。
	older, err := NewGetSchedulerHealthLogic(context.Background(), svcCtx).
		GetSchedulerHealth(&rpc.GetSchedulerHealthReq{Now: now + 7000})
	if err != nil {
		t.Fatalf("换窗口基准时刻：%v", err)
	}
	for _, g := range older.GetGroups() {
		if g.GetTaskGroup() == "alpha" && g.GetFailedLastHour() != 0 {
			t.Errorf("以 now+7000 为基准时，两条 failed 都在 3600 秒窗口之外，得到 %d",
				g.GetFailedLastHour())
		}
	}
}

func TestGetSchedulerHealthGroupFilterAndClock(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := seedHealthFixture(t, db)
	logic := NewGetSchedulerHealthLogic(context.Background(), svcCtx)

	// 分组过滤：执行/租约/定义三类聚合只留该分组，到期扫描在 SQL 里就过滤掉了。
	alpha, err := logic.GetSchedulerHealth(&rpc.GetSchedulerHealthReq{Now: now, TaskGroup: "alpha"})
	if err != nil {
		t.Fatalf("按分组过滤：%v", err)
	}
	want := fmt.Sprintf(
		"server_time=%d groups=[group=alpha enabled=2 paused=1 due=2 running=1 retrying=1 "+
			"failed1h=1 expired=1 oldest=%d]", now, now-30)
	if got := healthFacts(alpha); got != want {
		t.Errorf("过滤后的健康度 = %s，期望 %s", got, want)
	}
	// 查一个不存在的分组：必须回空清单，而不是「全量清单 + 假装过滤过」。
	ghost, err := logic.GetSchedulerHealth(&rpc.GetSchedulerHealthReq{Now: now, TaskGroup: "no.such.group"})
	if err != nil {
		t.Fatalf("查不存在分组：%v", err)
	}
	if len(ghost.GetGroups()) != 0 {
		t.Errorf("不存在的分组应回空清单：%s", healthFacts(ghost))
	}

	// now=0 时取服务端时钟，并把它用在所有子查询上。
	before := fakeNow()
	auto, err := logic.GetSchedulerHealth(&rpc.GetSchedulerHealthReq{})
	if err != nil {
		t.Fatalf("now=0：%v", err)
	}
	if auto.GetServerTime() < before || auto.GetServerTime() > fakeNow() {
		t.Errorf("now=0 必须落到服务端时钟，得到 %d", auto.GetServerTime())
	}
	if due := db.dueCalls[len(db.dueCalls)-1]; due.now != auto.GetServerTime() || due.lookahead != 0 {
		t.Errorf("到期积压必须用同一个服务端时刻、且不带提前量：%+v", due)
	}
	if run := db.runCalls; len(run) != 0 {
		t.Errorf("健康度不得走执行记录的分页查询：%+v", run)
	}
}

// TestGetSchedulerHealthEchoesCallerClockSentinel 钉住两条契约缺口：
//  1. reply.server_time 本意是「调度端据此校正本地时钟」，但这里回显的就是调用方
//     传进来的 now；传 1 就回 1，等于把漂移的时钟又塞回给调用方；
//  2. 到期积压的扫描上限是 model.MaxPageSize(200)，超过部分只显示 200，
//     调用方无从知道「200」是恰好积压 200 还是已经积压 20000。
func TestGetSchedulerHealthEchoesCallerClockSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedHealthFixture(t, db)

	reply, err := NewGetSchedulerHealthLogic(context.Background(), svcCtx).
		GetSchedulerHealth(&rpc.GetSchedulerHealthReq{Now: 1})
	if err != nil {
		t.Fatalf("GetSchedulerHealth(now=1): %v", err)
	}
	if reply.GetServerTime() != 1 {
		t.Errorf("当前现实：server_time 原样回显调用方时钟，得到 %d；修复后请改成服务端时钟并撤销 README 缺口",
			reply.GetServerTime())
	}
	// 判别性对照：ListDueTasks 的 server_time 是服务端现读的，不会被回显污染。
	due, err := NewListDueTasksLogic(context.Background(), svcCtx).ListDueTasks(&rpc.ListDueTasksReq{Now: 1})
	if err != nil {
		t.Fatalf("ListDueTasks(now=1): %v", err)
	}
	if due.GetServerTime() <= 1 {
		t.Errorf("对照失败：ListDueTasks 也回显了调用方时钟，得到 %d", due.GetServerTime())
	}

	// 积压被扫描上限截断：201 个到期任务只报 200。
	base := fakeNow()
	for i := int64(0); i < 201; i++ {
		seedDefinition(db, &model.TaskDefinition{
			ID: 1000 + i, TaskKey: fmt.Sprintf("flood.%03d", i), Handler: "h.flood",
			TaskGroup: "flood", ScheduleType: model.ScheduleTypeManual,
			State: model.TaskStateEnabled, TimeoutSeconds: 60, MaxAttempts: 1,
			ConcurrencyLimit: 1, LeaseTTLSeconds: 300, Version: 1, NextFireAt: base - i})
	}
	flood, err := NewGetSchedulerHealthLogic(context.Background(), svcCtx).
		GetSchedulerHealth(&rpc.GetSchedulerHealthReq{Now: base, TaskGroup: "flood"})
	if err != nil {
		t.Fatalf("积压扫描：%v", err)
	}
	if len(flood.GetGroups()) != 1 {
		t.Fatalf("应有 flood 分组：%s", healthFacts(flood))
	}
	g := flood.GetGroups()[0]
	if g.GetEnabledTasks() != 201 {
		t.Fatalf("分组内任务数应为 201（用例前提），得到 %d", g.GetEnabledTasks())
	}
	if g.GetDueBacklog() != model.MaxPageSize {
		t.Errorf("当前现实：due_backlog 被截断在 %d（真实积压 201），修复后请显式给出「已达上限」标记",
			g.GetDueBacklog())
	}
	if got := db.dueCalls[len(db.dueCalls)-1]; got.limit != dueBacklogScanLimit {
		t.Errorf("积压扫描必须带上限，得到 %+v", got)
	}
}

// TestGetSchedulerHealthHidesMissingHandlersSentinel 钉住 README 与实现的落差：
// 文档说健康度包含「本进程可跑的 handler 清单」，但 rpc.GetSchedulerHealthReply
// 里根本没有承载它的字段，于是「DB 注册了任务、这个进程一个 handler 都没装」
// 与「全部装齐」两种状态的回包一字不差 —— 缺口只进 ERROR 日志，巡检脚本看不见。
func TestGetSchedulerHealthHidesMissingHandlersSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := seedHealthFixture(t, db)

	bare, err := NewGetSchedulerHealthLogic(context.Background(), svcCtx).
		GetSchedulerHealth(&rpc.GetSchedulerHealthReq{Now: now})
	if err != nil {
		t.Fatalf("未装 handler 的健康度：%v", err)
	}
	if svcCtx.Registry.Len() != 0 {
		t.Fatalf("用例前提：本进程注册表为空，得到 %d", svcCtx.Registry.Len())
	}

	// 同一份数据、装齐 handler 的另一个进程。
	fullSvc, dbFull := newTestSvc(t)
	registerOK(t, fullSvc, "h.alpha")
	registerOK(t, fullSvc, "h.beta")
	seedHealthFixture(t, dbFull)
	loaded, err := NewGetSchedulerHealthLogic(context.Background(), fullSvc).
		GetSchedulerHealth(&rpc.GetSchedulerHealthReq{Now: now})
	if err != nil {
		t.Fatalf("装齐 handler 的健康度：%v", err)
	}
	if fullSvc.Registry.Len() == 0 {
		t.Fatal("对照用例必须真的装上 handler")
	}

	// 两个进程能跑的任务天差地别，回包却一模一样。
	if healthFacts(bare) != healthFacts(loaded) {
		t.Errorf("handler 缺口开始出现在回包里了（说明已修复）：\n bare=%s\nloaded=%s",
			healthFacts(bare), healthFacts(loaded))
	}
	for _, f := range []string{"Handler", "Handlers", "MissingHandlers", "RunnableHandlers", "RegisteredHandlers"} {
		if _, has := reflect.TypeOf(rpc.GetSchedulerHealthReply{}).FieldByName(f); has {
			t.Errorf("回包出现了 %s 字段：请把本用例改成断言该字段，并撤销 README 缺口", f)
		}
	}
	// 数据必须真的等价，否则上面的「一字不差」是通过两个现场都铺错达成的。
	if fmt.Sprint(db.listCalls) != fmt.Sprint(dbFull.listCalls) {
		t.Errorf("两个现场的分组扫描不一致：%v vs %v", db.listCalls, dbFull.listCalls)
	}
	if len(dbFull.listCalls) == 0 {
		t.Error("对照现场必须真的扫过启用任务（否则回包相同只是因为什么都没读）")
	}
}
