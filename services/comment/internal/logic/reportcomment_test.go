package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/comment/model"
	rpc "go-video/services/comment/rpc"
)

// rcValidReq 是一条合法举报。
func rcValidReq() *rpc.ReportCommentReq {
	return &rpc.ReportCommentReq{
		Rpid: 501, ReporterMid: 7007, Reason: 3,
		Content: "刷屏广告，请看第 12 楼", TraceId: "trace-rpt-1",
	}
}

// TestReportCommentGuards 参数守卫表：rpid / reporter_mid 非法必须在触库前拒绝。
func TestReportCommentGuards(t *testing.T) {
	cases := []struct {
		label   string
		mutate  func(*rpc.ReportCommentReq)
		wantIs  error
		wantMsg string
	}{
		{"rpid 为 0", func(r *rpc.ReportCommentReq) { r.Rpid = 0 }, nil, "comment: invalid rpid"},
		{"rpid 为负", func(r *rpc.ReportCommentReq) { r.Rpid = -4 }, nil, "comment: invalid rpid"},
		{"reporter_mid 为 0", func(r *rpc.ReportCommentReq) { r.ReporterMid = 0 }, model.ErrInvalidMid, ""},
		{"reporter_mid 为负", func(r *rpc.ReportCommentReq) { r.ReporterMid = -2 }, model.ErrInvalidMid, ""},
		{
			"两者同时非法时按 rpid 报",
			func(r *rpc.ReportCommentReq) { r.Rpid, r.ReporterMid = 0, 0 },
			nil, "comment: invalid rpid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()
			req := rcValidReq()
			tc.mutate(req)

			reply, err := NewReportCommentLogic(context.Background(), e.svcCtx).ReportComment(req)

			if tc.wantIs != nil {
				wantErrIs(t, tc.label, err, tc.wantIs)
			} else {
				wantErrMessage(t, tc.label, err, tc.wantMsg)
			}
			if reply != nil {
				t.Errorf("%s：拒绝后仍返回了 %+v", tc.label, reply)
			}
			wantNoCall(t, tc.label, e.st, before)
			wantEQ(t, tc.label, "举报行数", e.st.reports.countRows(), 0)
		})
	}
}

// TestReportCommentWritesAuditRowOnly 正常路径：只落一行 comment_report，
// 字段逐项来自请求（trace_id 必须透传，moderation 靠它关联回调），
// 且**一次都不碰评论表和缓存**——举报本身不改评论状态（AGENTS.md §8 审核结论才推进状态）。
func TestReportCommentWritesAuditRowOnly(t *testing.T) {
	e := newEnv(t)
	seed := distinctComment(501, 9001)
	seed.Tp = 1
	seedComment(t, e.st, seed)
	e.st.cache.warmList(9001, 1, "hot", 1, 20, "列表缓存")
	e.st.cache.warmStats(9001, 1, 7, 3)

	reply, err := NewReportCommentLogic(context.Background(), e.svcCtx).ReportComment(rcValidReq())
	wantNoErr(t, "ReportComment", err)
	if reply == nil {
		t.Fatalf("举报成功却返回了 nil")
	}

	row := e.st.reports.only(t)
	wantEQ(t, "举报行", "report_id", row.ReportID, int64(701))
	wantEQ(t, "举报行", "rpid", row.Rpid, int64(501))
	wantEQ(t, "举报行", "reporter_mid", row.ReporterMid, int64(7007))
	wantEQ(t, "举报行", "reason", row.Reason, int32(3))
	wantEQ(t, "举报行", "content", row.Content, "刷屏广告，请看第 12 楼")
	wantEQ(t, "举报行", "trace_id 透传", row.TraceID, "trace-rpt-1")
	assertAround(t, "举报行", "ctime", row.Ctime, nowUnix(), 2)

	// 评论状态与缓存一律不动。
	wantEQ(t, "举报后", "被举报评论 state 未变", e.st.comments.state(501), stateNormal)
	wantEQ(t, "举报后", "列表缓存未失效", e.st.cache.listPayload(9001, 1, "hot", 1, 20), "列表缓存")
	if _, ok := e.st.cache.statsOf(9001, 1); !ok {
		t.Errorf("举报不该失效计数缓存")
	}
	wantStringsEQ(t, "举报后", "只发生了举报写入", pickOps(e.st.log, "comment.", "cache."), nil)
	wantOps(t, "ReportComment 链路", e.st.log.ops, []string{"report.Insert:501/7007"})
}

// TestReportCommentReplayDuplicatesRow 现状钉桩（README 已知缺口 #4）：
// comment_report 上没有 (rpid, reporter_mid) 唯一约束，请求也没有幂等键，
// 因此同一人重复举报会重复落行、并让待审队列被同一条评论灌满。
func TestReportCommentReplayDuplicatesRow(t *testing.T) {
	e := newEnv(t)
	rc := NewReportCommentLogic(context.Background(), e.svcCtx)

	for i := 0; i < 3; i++ {
		_, err := rc.ReportComment(rcValidReq())
		wantNoErr(t, "重复举报", err)
	}

	wantEQ(t, "重复举报", "落库行数", e.st.reports.countRows(), 3)
	wantCount(t, "重复举报", e.st.log, "report.Insert", 3)
}

// TestReportCommentHasNoReasonOrLengthGuard 缺守卫的现状（README 已知缺口 #6）：
// reason 超出 comment_report.reason TINYINT 范围、content 超出 VARCHAR(500) 都直接进 SQL。
func TestReportCommentHasNoReasonOrLengthGuard(t *testing.T) {
	e := newEnv(t)
	req := rcValidReq()
	req.Reason = 999999
	req.Content = strings.Repeat("举", 300) // 900 字节，超过 VARCHAR(500)

	_, err := NewReportCommentLogic(context.Background(), e.svcCtx).ReportComment(req)
	wantNoErr(t, "越界入参", err)

	row := e.st.reports.only(t)
	wantEQ(t, "越界入参", "reason 原样进 SQL", row.Reason, int32(999999))
	wantEQ(t, "越界入参", "content 长度原样进 SQL", len(row.Content), 900)
}

// TestReportCommentPropagatesStoreFailure 写入失败必须报错且不留下任何行（不伪造成功）。
func TestReportCommentPropagatesStoreFailure(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("comment: mysql is gone")
	e.st.reports.failWith("Insert", boom)

	reply, err := NewReportCommentLogic(context.Background(), e.svcCtx).ReportComment(rcValidReq())

	wantErrIs(t, "举报写失败", err, boom)
	if reply != nil {
		t.Errorf("举报写失败却返回了 %+v", reply)
	}
	wantEQ(t, "举报写失败", "落库行数", e.st.reports.countRows(), 0)
	wantOps(t, "举报写失败", e.st.log.ops, []string{"report.Insert:501/7007"})
}

// TestReportCommentEmptyReplyCarriesNoID 契约核对：EmptyReply 没有 report_id 字段，
// 因此调用方无法据返回值去重/关联待审工单（README 已知缺口 #16，此处只钉现状）。
func TestReportCommentEmptyReplyCarriesNoID(t *testing.T) {
	e := newEnv(t)

	reply, err := NewReportCommentLogic(context.Background(), e.svcCtx).ReportComment(rcValidReq())
	wantNoErr(t, "举报成功", err)

	wantEQ(t, "举报回复", "字段数", reply.ProtoReflect().Descriptor().Fields().Len(), 0)
}
