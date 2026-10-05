package logic

// reportdanmaku_test.go 覆盖 ReportDanmaku：用户举报弹幕的写侧入口——
// 参数守卫 → 弹幕存在性 → 自举报门禁 → 组装 model.Report → Repository.ReportDanmaku
// →（model 内部）预查 uniq_dmid_reporter / INSERT / 撞索引后回查。
//
// 四条被测不变量（AGENTS.md §5「只写本服务自有表、不直连 moderation 库」、§8「举报不改变内容可见性」）：
//  1. 守卫（dmid / reporter_mid）必须发生在任何依赖调用之前，且 dmid 优先；
//  2. 举报只落 danmaku_report 一张表：不改弹幕状态/池、不写 op_log、不开事务、不动缓存；
//  3. 同一 (dmid, reporter_mid) 只留一行——重放既不二次写入也不产生第二条；
//  4. 下游失败一律原始上抛（本方法没有可降级分支，也没有把错误洗成成功应答）。
//
// 本轮另外钉住五条现状（见 README「已知缺口」8~12）：举报不留任何审计轨迹、
// 重复举报既不重开也不更新说明、reason/content 与频次都没有门禁、
// 无状态门禁导致指向已删除弹幕的悬空举报、举报者身份完全来自请求体。
// 相应注释写在对应用例上，收严后那些断言必须变红。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

const (
	repOid      = int64(1001)
	repAid      = int64(500)
	repAuthor   = int64(7001) // 被举报弹幕的作者
	repUser     = int64(7002) // 举报者
	repStranger = int64(8888) // 与弹幕毫无关系的第三个 mid（冒充用）
	repSegNo    = int32(2)
	repReason   = int32(3) // 举报原因码：本服务不解释，只透传
	repIdemKey  = "idem-report-1"
	// reportBaseID 是替身给 danmaku_report 分配的自增主键起点（见 fakes_test.go 纪律延伸）。
	reportBaseID = int64(901)
)

var errRequery = errors.New("inject: 撞索引后的回查也挂了")

// --- 期望序列片段（与替身记轨迹的口径逐字一致） ---

// findTargetOp 是 model.ReportModel.Insert 内部那次「预查唯一键锚点」，
// 以及撞索引之后的那次回查（两条记同一个标签，靠序列里的位置区分）。
func findTargetOp(dmid, reporterMid int64) string {
	return fmt.Sprintf("report.FindByTarget:%d/%d", dmid, reporterMid)
}

func insertReportOp(dmid, reporterMid int64) string {
	return fmt.Sprintf("report.Insert:%d/%d", dmid, reporterMid)
}

// --- 布景与调用 ---

func reportDanmaku(t *testing.T, e *env, in *rpc.ReportDanmakuReq) (*rpc.ReportDanmakuReply, error) {
	t.Helper()
	return NewReportDanmakuLogic(context.Background(), e.svcCtx).ReportDanmaku(in)
}

// repSeedDanmaku 布一条属于 repAuthor 的弹幕（首行 dmid 恒为 101），返回落库行副本。
func repSeedDanmaku(t *testing.T, e *env, state, pool int32) *model.Danmaku {
	t.Helper()
	return repSeedDanmakuBy(t, e, repAuthor, state, pool)
}

// repSeedDanmakuBy 同上，但作者可指定（自举报门禁只看作者列）。
// 幂等键按当前行数递增：替身照搬了 danmaku.uniq_idempotency，一个 env 里布多条弹幕必须各用一键。
func repSeedDanmakuBy(t *testing.T, e *env, author int64, state, pool int32) *model.Danmaku {
	t.Helper()
	return seedDanmaku(t, e.st, &model.Danmaku{
		Oid: repOid, Aid: repAid, Mid: author, ProgressMs: 12_500,
		Mode: int32(rpc.DanmakuMode_MODE_SCROLL), Fontsize: 25, Color: 0x00FFCC,
		Content: "等着被人举报的那条弹幕", State: state, Pool: pool, SegNo: repSegNo,
		IdempotencyKey: fmt.Sprintf("%s-%d", repIdemKey, len(e.st.danmaku.rows)+1),
		TraceId:        "trace-seed", Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	})
}

// repReq 组一条正常举报请求（reason/content 都带值）。
func repReq(dmid, reporterMid int64) *rpc.ReportDanmakuReq {
	return &rpc.ReportDanmakuReq{
		Dmid: dmid, ReporterMid: reporterMid, Reason: repReason,
		Content: "含人身攻击", TraceId: "trace-report",
	}
}

// repRow 读举报表那一行（副本，断言用）。
func repRow(t *testing.T, e *env, reportID int64) *model.Report {
	t.Helper()
	row := e.st.report.get(reportID)
	if row == nil {
		t.Fatalf("前置条件不成立：举报 %d 不在库里", reportID)
	}
	return row
}

// repWantRow 断言「举报之外什么都没动」：弹幕行原样、op_log 零行、事务零次、缓存零次。
// 这条断言是 §5/§8 边界的直接体现——举报只是记录事实，不替审核域提前执法。
func repWantRow(t *testing.T, label string, e *env, dmid int64, state, pool int32) {
	t.Helper()
	row := e.st.danmaku.get(dmid)
	if row == nil {
		t.Fatalf("%s：弹幕 %d 竟然不在库里了", label, dmid)
	}
	wantEQ(t, label, "弹幕 state", row.State, state)
	wantEQ(t, label, "弹幕 pool", row.Pool, pool)
	wantEQ(t, label, "弹幕作者", row.Mid, repAuthor)
	wantEQ(t, label, "op_log 行数", len(e.st.opLog.rows), 0)
	wantEQ(t, label, "事务次数", e.st.conn.transactions, 0)
	wantCount(t, label, e.st.log, "cache.", 0)
	wantCount(t, label, e.st.log, "segment.", 0)
	wantCount(t, label, e.st.log, "danmaku.Transition", 0)
	wantCount(t, label, e.st.log, "danmaku.Update", 0)
}

// --- 守卫 ---

// TestReportDanmakuRejectsInvalidRequests 两个守卫都发生在触库之前。
// dmid 非法优先于 reporter_mid 非法（reportdanmakulogic.go:34 在 :37 之前）。
func TestReportDanmakuRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ReportDanmakuReq
		want error
	}{
		{"dmid 为 0", &rpc.ReportDanmakuReq{ReporterMid: repUser}, model.ErrInvalidDmid},
		{"dmid 为负", &rpc.ReportDanmakuReq{Dmid: -7, ReporterMid: repUser}, model.ErrInvalidDmid},
		{"dmid 非法优先于 reporter 非法", &rpc.ReportDanmakuReq{Dmid: 0}, model.ErrInvalidDmid},
		{"reporter_mid 为 0（未登录也进不来）", &rpc.ReportDanmakuReq{Dmid: 101}, model.ErrInvalidMid},
		{"reporter_mid 为负", &rpc.ReportDanmakuReq{Dmid: 101, ReporterMid: -1}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			// 先布一行真实弹幕：dmid=101 那两条用例要证明「守卫排在读主表之前」
			repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
			before := e.st.log.snapshot()

			reply, err := reportDanmaku(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法请求仍被受理 %+v", tc.name, reply)
			}
			// 守卫排在读主表之前：连一次 danmaku.FindOne 都不许有
			wantNoCall(t, tc.name, e.st, before)
			wantEQ(t, tc.name, "举报行数", len(e.st.report.rows), 0)
		})
	}
}

// --- 被举报对象存在性 ---

// TestReportDanmakuTargetMustExist 举报必须落到一条真实弹幕上：
// 不存在 ⇒ ErrDanmakuNotFound；读失败 ⇒ 原始错误上抛。两条路径都不许留下半行举报。
// 注意这里的判别点：这是本方法唯一的「对象门禁」，弹幕处于什么状态它不管（见
// TestReportDanmakuAcceptsAnyDanmakuState）。
func TestReportDanmakuTargetMustExist(t *testing.T) {
	t.Run("弹幕不存在", func(t *testing.T) {
		e := newEnv(t)
		seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)

		reply, err := reportDanmaku(t, e, repReq(seeded.Dmid+50, repUser))
		wantErrIs(t, "举报不存在的弹幕", err, model.ErrDanmakuNotFound)
		if reply != nil {
			t.Errorf("举报不存在的弹幕仍返回 %+v", reply)
		}
		wantOps(t, "举报不存在的弹幕", e.ops(), []string{findOneOp(seeded.Dmid + 50)})
		wantEQ(t, "举报不存在的弹幕", "举报行数", len(e.st.report.rows), 0)
	})

	t.Run("弹幕读失败", func(t *testing.T) {
		e := newEnv(t)
		seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
		e.st.danmaku.failWith("FindOne", errDB)

		reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repUser))
		wantErrIs(t, "弹幕读失败", err, errDB)
		if reply != nil {
			t.Errorf("弹幕读失败仍返回 %+v", reply)
		}
		wantOps(t, "弹幕读失败", e.ops(), []string{findOneOp(seeded.Dmid)})
		wantEQ(t, "弹幕读失败", "举报行数", len(e.st.report.rows), 0)
	})
}

// --- 自举报 ---

// TestReportDanmakuSelfReportIsForbidden 自举报无意义：门禁比的是「被举报弹幕的作者列」
// 与请求里的 reporter_mid（reportdanmakulogic.go:48），拒绝必须发生在写举报表之前。
// 对照行是「作者的另一个号之外的人」——只差一个 mid 就放行，说明判定确实落在这一列上。
func TestReportDanmakuSelfReportIsForbidden(t *testing.T) {
	cases := []struct {
		name     string
		reporter int64
		wantErr  error
	}{
		{"作者举报自己的弹幕", repAuthor, model.ErrForbidden},
		{"他人举报（放行）", repUser, nil},
		{"第三人与弹幕毫无关系（放行）", repStranger, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)

			reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, tc.reporter))

			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				if reply != nil {
					t.Errorf("%s：仍返回 %+v", tc.name, reply)
				}
				// 只读了一次主表就回绝，举报表一行没写、留痕零条
				wantOps(t, tc.name, e.ops(), []string{findOneOp(seeded.Dmid)})
				wantEQ(t, tc.name, "举报行数", len(e.st.report.rows), 0)
				return
			}
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "举报行数", len(e.st.report.rows), 1)
		})
	}
}

// --- 正常路径逐字段 ---

// TestReportDanmakuPersistsEveryField 正常路径的完整投影：应答两列、落库那一行的每一列、
// 以及「除举报表之外零副作用」。ctime 用调用前后的时间夹住（logic 自己盖的戳，
// 见 reportdanmakulogic.go:60），不拿弹幕种子里那个 1_700_000_000 冒充。
func TestReportDanmakuPersistsEveryField(t *testing.T) {
	e := newEnv(t)
	seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
	seedSegment(t, e.st, repOid, repSegNo, 4)
	e.st.cache.warmSegmentCount(repOid, repSegNo, 4)

	before := time.Now().Unix()
	reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repUser))
	after := time.Now().Unix()
	wantNoErr(t, "正常举报", err)
	if reply == nil {
		t.Fatal("正常举报：reply = nil")
	}

	// 应答逐列：只有 report_id 与 duplicated 两列有值
	wantEQ(t, "正常举报", "report_id", reply.GetReportId(), reportBaseID)
	wantEQ(t, "正常举报", "duplicated", reply.GetDuplicated(), false)

	row := repRow(t, e, reply.GetReportId())
	wantEQ(t, "正常举报", "dmid", row.Dmid, seeded.Dmid)
	wantEQ(t, "正常举报", "reporter_mid", row.ReporterMid, repUser)
	wantEQ(t, "正常举报", "reason", row.Reason, repReason)
	wantEQ(t, "正常举报", "content", row.Content, "含人身攻击")
	wantEQ(t, "正常举报", "trace_id", row.TraceID, "trace-report")
	// 新举报一律排到待处理队列（请求里没有 state 可填）
	wantEQ(t, "正常举报", "state", row.State, model.ReportPending)
	if row.Ctime < before || row.Ctime > after {
		t.Errorf("正常举报：ctime = %d, want 落在 [%d,%d]（logic 现场盖戳，不是种子值 1700000000）", row.Ctime, before, after)
	}

	// 副作用序列：读弹幕 →（model 内）预查唯一键 → INSERT；到此为止
	wantOps(t, "正常举报", e.ops(), []string{
		findOneOp(seeded.Dmid),
		findTargetOp(seeded.Dmid, repUser),
		insertReportOp(seeded.Dmid, repUser),
	})
	repWantRow(t, "正常举报", e, seeded.Dmid, model.StateNormal, model.PoolNormal)
	// 段计数与段列表缓存纹丝不动：举报不影响下发
	wantEQ(t, "正常举报", "段表计数", e.st.segment.countAt(repOid, repSegNo), int32(4))
	wantEQ(t, "正常举报", "缓存段计数", cacheCount(e, repOid, repSegNo), int32(4))
	// 也没有任何鉴权/风控/审核下游调用（本方法的依赖只有 danmaku 读 + report 写）
	wantCount(t, "正常举报", e.st.log, "moderation.", 0)
	wantCount(t, "正常举报", e.st.log, "limiter.", 0)
	wantCount(t, "正常举报", e.st.log, "oplog.", 0)
}

// --- 幂等 ---

// TestReportDanmakuRepeatKeepsFirstRecord 同一人重复举报只留一行（uniq_dmid_reporter）。
// 现状哨兵（README 缺口 10）：命中已有行就原样回显——既不重开处理状态，也不更新
// reason/content（model/danmaku_report.go:68-70 直接 return old.ReportID），
// 于是「被驳回后再举报」永远进不了队列，用户看到的仍是第一次那份说明。
// 收严（重开队列或至少记录追加说明）后本用例的 state/说明断言必须变红。
func TestReportDanmakuRepeatKeepsFirstRecord(t *testing.T) {
	t.Run("待处理态重复举报", func(t *testing.T) {
		e := newEnv(t)
		seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
		first, err := reportDanmaku(t, e, repReq(seeded.Dmid, repUser))
		wantNoErr(t, "首次举报", err)

		before := e.st.log.snapshot()
		second := repReq(seeded.Dmid, repUser)
		second.Reason = 9
		second.Content = "换了个原因再来一次"
		reply, err := reportDanmaku(t, e, second)
		wantNoErr(t, "重复举报", err)

		wantEQ(t, "重复举报", "report_id 仍是第一次那个", reply.GetReportId(), first.GetReportId())
		wantEQ(t, "重复举报", "duplicated", reply.GetDuplicated(), true)
		// 序列里没有 report.Insert：预查命中就不写（也不会白占一个自增 ID）
		wantOps(t, "重复举报", e.st.log.opsFrom(before),
			[]string{findOneOp(seeded.Dmid), findTargetOp(seeded.Dmid, repUser)})
		wantEQ(t, "重复举报", "举报总行数", len(e.st.report.rows), 1)

		row := repRow(t, e, first.GetReportId())
		wantEQ(t, "重复举报", "reason 没被第二次改掉", row.Reason, repReason)
		wantEQ(t, "重复举报", "content 没被第二次改掉", row.Content, "含人身攻击")
		wantEQ(t, "重复举报", "state 仍是待处理", row.State, model.ReportPending)
	})

	t.Run("已驳回的举报无法重新排队", func(t *testing.T) {
		e := newEnv(t)
		seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
		seedReport(t, e.st, &model.Report{
			Dmid: seeded.Dmid, ReporterMid: repUser, Reason: 1, Content: "第一次的说明",
			State: model.ReportDismissed, TraceID: "trace-old", Ctime: 1_700_000_000,
		})
		seedSegment(t, e.st, repOid, repSegNo, 4)

		reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repUser))
		wantNoErr(t, "驳回后再举报", err)
		wantEQ(t, "驳回后再举报", "duplicated", reply.GetDuplicated(), true)
		wantEQ(t, "驳回后再举报", "回显的是被驳回那一行", reply.GetReportId(), reportBaseID)
		row := repRow(t, e, reply.GetReportId())
		wantEQ(t, "驳回后再举报", "没有回到待处理队列", row.State, model.ReportDismissed)
		// 队列里没有新增：moderation 侧 ListPending 永远看不到这次举报
		pending, total, err := e.st.report.ListPending(context.Background(), 1, 20)
		wantNoErr(t, "驳回后再举报", err)
		wantEQ(t, "驳回后再举报", "待处理条数", len(pending), 0)
		wantEQ(t, "驳回后再举报", "待处理总数", total, int32(0))
		repWantRow(t, "驳回后再举报", e, seeded.Dmid, model.StateNormal, model.PoolNormal)
	})
}

// TestReportDanmakuIsolatedByReporterAndTarget 唯一键是 (dmid, reporter_mid) 而不是 dmid：
// 同一条弹幕被两个人举报 ⇒ 两行；同一个人举报两条弹幕 ⇒ 两行。
// 这条断言同时钉住「举报不聚合」：moderation 侧看到的是一条弹幕 N 条独立举报。
func TestReportDanmakuIsolatedByReporterAndTarget(t *testing.T) {
	e := newEnv(t)
	a := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
	b := repSeedDanmakuBy(t, e, repAuthor+40, model.StateNormal, model.PoolNormal)

	first, err := reportDanmaku(t, e, repReq(a.Dmid, repUser))
	wantNoErr(t, "甲举报弹幕 A", err)
	second, err := reportDanmaku(t, e, repReq(a.Dmid, repUser+1))
	wantNoErr(t, "乙举报同一条弹幕 A", err)
	third, err := reportDanmaku(t, e, repReq(b.Dmid, repUser))
	wantNoErr(t, "甲举报弹幕 B", err)

	wantEQ(t, "三条举报互不相同", "第一条 ID", first.GetReportId(), reportBaseID)
	wantEQ(t, "三条举报互不相同", "第二条 ID", second.GetReportId(), reportBaseID+1)
	wantEQ(t, "三条举报互不相同", "第三条 ID", third.GetReportId(), reportBaseID+2)
	for _, r := range []*rpc.ReportDanmakuReply{first, second, third} {
		wantEQ(t, "三条举报互不相同", "duplicated", r.GetDuplicated(), false)
	}
	wantEQ(t, "三条举报互不相同", "举报行数", len(e.st.report.rows), 3)
	wantEQ(t, "三条举报互不相同", "同一条弹幕上的举报数",
		countIn(e.ops(), fmt.Sprintf("report.Insert:%d/", a.Dmid)), 2)

	// 每个人看到的仍是自己那一行，不会被别人的说明覆盖
	wantEQ(t, "举报互相隔离", "甲在 A 上的举报人", repRow(t, e, first.GetReportId()).ReporterMid, repUser)
	wantEQ(t, "举报互相隔离", "乙在 A 上的举报人", repRow(t, e, second.GetReportId()).ReporterMid, repUser+1)
}

// --- 并发 ---

// TestReportDanmakuConcurrentDuplicateFallsBackToExistingRow 并发下 uniq_dmid_reporter 兜底：
// 预查 miss ⇒ 执行 INSERT ⇒ 撞唯一索引 ⇒ model 回查一次并按重放返回（model/danmaku_report.go:76-82）。
// 现状要点：回查命中后落库的是对手那一行，本次请求带的 reason/content 被就地丢弃，
// 应答 duplicated=true——调用方无从知道「自己那份说明没进去」。
func TestReportDanmakuConcurrentDuplicateFallsBackToExistingRow(t *testing.T) {
	t.Run("回查命中 ⇒ 按重放返回，丢本次的说明", func(t *testing.T) {
		e := newEnv(t)
		seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
		e.st.report.armConcurrentReplay(&model.Report{
			Dmid: seeded.Dmid, ReporterMid: repUser, Reason: 77, Content: "并发对手写下的说明",
			State: model.ReportPending, TraceID: "trace-other", Ctime: 1_700_000_000,
		})

		reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repUser))
		wantNoErr(t, "并发重复举报", err)
		wantEQ(t, "并发重复举报", "report_id 是对手那行", reply.GetReportId(), reportBaseID)
		wantEQ(t, "并发重复举报", "duplicated", reply.GetDuplicated(), true)
		wantOps(t, "并发重复举报", e.ops(), []string{
			findOneOp(seeded.Dmid), findTargetOp(seeded.Dmid, repUser),
			insertReportOp(seeded.Dmid, repUser), findTargetOp(seeded.Dmid, repUser),
		})
		wantEQ(t, "并发重复举报", "只有一行", len(e.st.report.rows), 1)
		row := repRow(t, e, reply.GetReportId())
		wantEQ(t, "并发重复举报", "入库的是对手的原因码", row.Reason, int32(77))
		wantEQ(t, "并发重复举报", "本次的 content 被丢弃", row.Content, "并发对手写下的说明")
	})

	t.Run("回查自己也失败 ⇒ 原始错误上抛，举报彻底丢失", func(t *testing.T) {
		e := newEnv(t)
		seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
		e.st.report.armConcurrentReplay(&model.Report{
			Dmid: seeded.Dmid, ReporterMid: repUser, Reason: 77, Content: "并发对手写下的说明",
			State: model.ReportPending, TraceID: "trace-other", Ctime: 1_700_000_000,
		})
		e.st.report.failWith("FindByTargetAfterDup", errRequery)

		reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repUser))
		wantErrIs(t, "撞索引后回查失败", err, errRequery)
		if reply != nil {
			t.Errorf("撞索引后回查失败仍返回 %+v", reply)
		}
		// 现状：回查的原始错误没有被包装成 "danmaku_report Insert"（model/danmaku_report.go:78 直接 return），
		// 上抛的错误里连「哪张表、哪个弹幕」都没有。
		wantErrContains(t, "撞索引后回查失败（现状）", err, "inject: 撞索引后的回查也挂了")
		if strings.Contains(err.Error(), "danmaku_report Insert") {
			t.Errorf("错误被包装成了带表名的 %v，与现状（原样 return）不符", err)
		}
		wantOps(t, "撞索引后回查失败", e.ops(), []string{
			findOneOp(seeded.Dmid), findTargetOp(seeded.Dmid, repUser),
			insertReportOp(seeded.Dmid, repUser), findTargetOp(seeded.Dmid, repUser),
		})
		// 库里只有对手那一行：本次举报既没落库也没被重放认定
		wantEQ(t, "撞索引后回查失败", "举报行数", len(e.st.report.rows), 1)
	})
}

// --- 状态门禁 ---

// TestReportDanmakuAcceptsAnyDanmakuState 钉住现状（README 缺口 9）：本方法只判「弹幕存不存在」，
// 不判状态/池，所以已删除（3）、已驳回（4）、审核中（1）乃至枚举外的脏状态照样能举报成功，
// 并且写入发生在事务之外、不改弹幕任何一列 ⇒ 审核队列里会出现指向不可见内容的悬空举报。
// 对照：不存在的弹幕会被挡下（见 TestReportDanmakuTargetMustExist），说明这里缺的不是存在性而是状态门禁。
func TestReportDanmakuAcceptsAnyDanmakuState(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		pool  int32
	}{
		{"普通池可见弹幕", model.StateNormal, model.PoolNormal},
		{"审核中（本人可见）", model.StatePending, model.PoolReview},
		{"已折叠", model.StateFolded, model.PoolBlock},
		{"作者已删除的弹幕仍可被举报", model.StateDeleted, model.PoolBlock},
		{"已驳回的弹幕仍可被举报", model.StateRejected, model.PoolBlock},
		{"枚举外的脏状态", 9, model.PoolNormal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := repSeedDanmaku(t, e, tc.state, tc.pool)

			reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repUser))
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "举报照样落库", reply.GetDuplicated(), false)
			wantEQ(t, tc.name, "举报行数", len(e.st.report.rows), 1)
			// 弹幕那一行原封不动：既不隐藏也不折叠，也不留痕
			repWantRow(t, tc.name, e, seeded.Dmid, tc.state, tc.pool)
		})
	}
}

// --- 入参门禁 ---

// TestReportDanmakuContentAndReasonAreNotValidated 钉住现状（README 缺口 11）：
// 本方法对 reason/content 一个字节都不校验——原样进 model.Report 再进 SQL。
// 列宽是 content VARCHAR(500)、trace_id VARCHAR(64)（deploy/migrations/danmaku/
// 000002_create_danmaku_moderation_tables.sql:71、:73），PostDanmaku 有 MaxContentLength
// 门禁而这里没有，超长处只在 MySQL 严格模式下报错，报出来就是一个 500。
// 判别对照：同样是「字符串入参」，弹幕正文超长会被 ErrInvalidContent 挡下（见 postdanmaku_test.go），
// 这里 5001 字直接落库。
func TestReportDanmakuContentAndReasonAreNotValidated(t *testing.T) {
	long := strings.Repeat("举", 5001)
	cases := []struct {
		name    string
		reason  int32
		content string
	}{
		{"原因码 0（未选原因也能提交）", 0, "没有原因"},
		{"原因码为负", -1, "负原因码"},
		{"原因码取 int32 上界", 2147483647, "越界原因码"},
		{"说明为空", repReason, ""},
		{"说明超出列宽 5001 字（对照：弹幕正文超 100 字会被拒）", repReason, long},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
			in := repReq(seeded.Dmid, repUser)
			in.Reason = tc.reason
			in.Content = tc.content

			reply, err := reportDanmaku(t, e, in)
			wantNoErr(t, tc.name, err)
			row := repRow(t, e, reply.GetReportId())
			wantEQ(t, tc.name, "reason 原样入库", row.Reason, tc.reason)
			wantEQ(t, tc.name, "content 一字未改、一字未裁", row.Content, tc.content)
		})
	}
}

// TestReportDanmakuAppliesNoFrequencyOrRiskControl 钉住现状（README 缺口 11）：
// 同一个用户连续举报 5 条弹幕全部成功、5 行独立记录，全程没有调用限流器、
// 没有写风控/审核下游，也没有任何「同一人短时间举报数」的上限。
// 判别对照：唯一挡住重复的只有 uniq_dmid_reporter（同一弹幕同一人），换弹幕就畅通无阻。
func TestReportDanmakuAppliesNoFrequencyOrRiskControl(t *testing.T) {
	e := newEnv(t)
	seeded := make([]int64, 0, 6)
	for i := 0; i < 5; i++ {
		d := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
		seeded = append(seeded, d.Dmid)
	}

	for i, dmid := range seeded {
		reply, err := reportDanmaku(t, e, repReq(dmid, repUser))
		wantNoErr(t, "同一人连发举报", err)
		wantEQ(t, "同一人连发举报", "duplicated", reply.GetDuplicated(), false)
		wantEQ(t, "同一人连发举报", "report_id 逐条递增", reply.GetReportId(), reportBaseID+int64(i))
	}
	// 第 6 次落在第一条弹幕上：这时才被唯一索引挡下
	reply, err := reportDanmaku(t, e, repReq(seeded[0], repUser))
	wantNoErr(t, "同一人重复举报同一条", err)
	wantEQ(t, "同一人重复举报同一条", "duplicated", reply.GetDuplicated(), true)

	wantEQ(t, "无频次门禁", "举报行数", len(e.st.report.rows), 5)
	wantEQ(t, "无频次门禁", "insert 次数", countIn(e.ops(), "report.Insert"), 5)
	wantCount(t, "无频次门禁", e.st.log, "limiter.", 0)
	wantCount(t, "无频次门禁", e.st.log, "moderation.", 0)
	wantCount(t, "无频次门禁", e.st.log, "oplog.", 0)
	wantCount(t, "无频次门禁", e.st.log, "cache.", 0)
}

// --- 身份边界 ---

// TestReportDanmakuTrustsRequestSuppliedReporterMid 钉住现状（README 缺口 12）：
// 举报者身份完全来自请求体，本方法既不核对登录态也不看 ctx（reportdanmakulogic.go:37 只判 >0），
// 而 danmaku 的 gRPC 服务没有任何鉴权拦截器（danmaku.v1.go:28-34、
// internal/server/danmakuserver.go:45-48），网关又原样转发客户端填的 reporter_mid
// （gateway/app/internal/logic/reportdanmakulogic.go:40）。
// 两条可判别后果：① 任意 mid 都能被写进 reporter_mid（冒充他人举报、事后无法追责）；
// ② 作者只要把 reporter_mid 填成别人的号，就能绕过自举报门禁举报自己的弹幕。
// 与既有 TestListUserBlocksTrustsRequestSuppliedMid 同源，本轮一并登记。
func TestReportDanmakuTrustsRequestSuppliedReporterMid(t *testing.T) {
	t.Run("冒充任意 mid 举报", func(t *testing.T) {
		e := newEnv(t)
		seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)

		reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repStranger))
		wantNoErr(t, "冒充举报", err)
		row := repRow(t, e, reply.GetReportId())
		wantEQ(t, "冒充举报", "reporter_mid 就是请求里填的那个", row.ReporterMid, repStranger)
		wantEQ(t, "冒充举报", "被举报的作者列不受影响", e.st.danmaku.get(seeded.Dmid).Mid, repAuthor)
	})

	t.Run("作者换个 reporter_mid 就能举报自己的弹幕", func(t *testing.T) {
		e := newEnv(t)
		seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)

		// 先按作者真实 mid 举报：被 ErrForbidden 挡下
		_, err := reportDanmaku(t, e, repReq(seeded.Dmid, repAuthor))
		wantErrIs(t, "作者举报自己的弹幕", err, model.ErrForbidden)
		wantEQ(t, "作者举报自己的弹幕", "举报行数", len(e.st.report.rows), 0)

		// 同一个调用方只改请求体里的 reporter_mid ⇒ 放行
		reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repStranger))
		wantNoErr(t, "换 reporter_mid 绕过自举报门禁", err)
		wantEQ(t, "换 reporter_mid 绕过自举报门禁", "举报已落库", len(e.st.report.rows), 1)
		wantEQ(t, "换 reporter_mid 绕过自举报门禁", "入库身份是被伪造的那个",
			repRow(t, e, reply.GetReportId()).ReporterMid, repStranger)
	})
}

// --- 下游故障 ---

// TestReportDanmakuPropagatesDownstreamFailures 本方法没有可降级分支：三类故障都把错误
// 原样交给调用方（差别只在包装文案）：
//   - 主表读失败：原始 errDB；
//   - 预查唯一键失败：model 直接 return err（danmaku_report.go:65-67），没有表名上下文；
//   - INSERT 失败且回查仍 miss：包装成 "danmaku_report Insert: ..."（:83），错误链里保住 errDB。
//
// 三条都必须零残留：举报没落库、弹幕没被动过、缓存一次都没碰。
func TestReportDanmakuPropagatesDownstreamFailures(t *testing.T) {
	cases := []struct {
		name     string
		arm      func(e *env)
		wantErr  error
		wantText string
		// wantWrapped：错误文案里是否带 "danmaku_report Insert" 的表级上下文。
		// false 就是要钉住「上抛的是裸错误，看不出是哪张表、哪条弹幕」。
		wantWrapped bool
		wantOps     func(dmid int64) []string
	}{
		{
			name:     "主表读失败",
			arm:      func(e *env) { e.st.danmaku.failWith("FindOne", errDB) },
			wantErr:  errDB,
			wantText: "inject: db down",
			wantOps:  func(dmid int64) []string { return []string{findOneOp(dmid)} },
		},
		{
			name:     "预查唯一键失败（现状：无表名上下文）",
			arm:      func(e *env) { e.st.report.failWith("FindByTarget", errDB) },
			wantErr:  errDB,
			wantText: "inject: db down",
			wantOps: func(dmid int64) []string {
				return []string{findOneOp(dmid), findTargetOp(dmid, repUser)}
			},
		},
		{
			name:        "INSERT 失败且回查仍 miss",
			arm:         func(e *env) { e.st.report.failWith("Insert", errDB) },
			wantErr:     errDB,
			wantText:    "danmaku_report Insert: inject: db down",
			wantWrapped: true,
			wantOps: func(dmid int64) []string {
				return []string{findOneOp(dmid), findTargetOp(dmid, repUser),
					insertReportOp(dmid, repUser), findTargetOp(dmid, repUser)}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := repSeedDanmaku(t, e, model.StateNormal, model.PoolNormal)
			seedSegment(t, e.st, repOid, repSegNo, 4)
			e.st.cache.warmSegmentCount(repOid, repSegNo, 4)
			tc.arm(e)

			reply, err := reportDanmaku(t, e, repReq(seeded.Dmid, repUser))
			wantErrIs(t, tc.name, err, tc.wantErr)
			wantErrContains(t, tc.name, err, tc.wantText)
			if wrapped := strings.Contains(err.Error(), "danmaku_report Insert"); wrapped != tc.wantWrapped {
				t.Errorf("%s：错误是否带表级上下文 = %v, want %v（%v）", tc.name, wrapped, tc.wantWrapped, err)
			}
			if reply != nil {
				t.Errorf("%s：故障仍返回 %+v", tc.name, reply)
			}
			wantOps(t, tc.name, e.ops(), tc.wantOps(seeded.Dmid))
			wantEQ(t, tc.name, "举报没落库", len(e.st.report.rows), 0)
			repWantRow(t, tc.name, e, seeded.Dmid, model.StateNormal, model.PoolNormal)
			wantEQ(t, tc.name, "段表计数未动", e.st.segment.countAt(repOid, repSegNo), int32(4))
			wantEQ(t, tc.name, "缓存段计数未动", cacheCount(e, repOid, repSegNo), int32(4))
		})
	}
}
