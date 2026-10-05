package logic

import (
	"context"
	"testing"
	"time"

	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

// TestCreateSubmissionGuardsRejectBeforeAnyDependency 锁「入参守卫先于任何依赖」：
// 守卫顺序 mid → title → typeid，且拒绝后一次 SQL/缓存调用都不许发生
// （投稿接口在网关侧是高频入口，非法请求不许打到 MySQL）。
func TestCreateSubmissionGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.CreateSubmissionReq
		want error
	}{
		{"mid=0", &rpc.CreateSubmissionReq{Mid: 0, Title: "t", Typeid: 1}, model.ErrInvalidMid},
		{"mid<0", &rpc.CreateSubmissionReq{Mid: -1, Title: "t", Typeid: 1}, model.ErrInvalidMid},
		{
			"title 空",
			&rpc.CreateSubmissionReq{Mid: 7, Title: "", Typeid: 1},
			model.ErrInvalidTitle,
		},
		{"typeid=0", &rpc.CreateSubmissionReq{Mid: 7, Title: "t", Typeid: 0}, model.ErrInvalidTypeid},
		{"typeid<0", &rpc.CreateSubmissionReq{Mid: 7, Title: "t", Typeid: -3}, model.ErrInvalidTypeid},
		{
			"三个都非法时按 mid 先报（守卫顺序）",
			&rpc.CreateSubmissionReq{Mid: 0, Title: "", Typeid: 0},
			model.ErrInvalidMid,
		},
		{
			"mid 合法、title 与 typeid 都非法时按 title 报",
			&rpc.CreateSubmissionReq{Mid: 7, Title: "", Typeid: 0},
			model.ErrInvalidTitle,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			from := st.log().snapshot()
			got, err := NewCreateSubmissionLogic(context.Background(), st.svcCtx).CreateSubmission(tc.in)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			wantNoCallAfter(t, tc.name, st.log(), from)
			if len(st.subs.rows) != 0 {
				t.Errorf("%s：拒绝后库里出现 %d 行稿件", tc.name, len(st.subs.rows))
			}
		})
	}
}

// TestCreateSubmissionWritesDraftAndBackfillsAid 锁投稿创建的四条事实：
//  1. 只发一条 INSERT，state 恒为 DRAFT（调用方无法通过入参指定初始状态）；
//  2. 自增主键 aid 既回写进库里那一行、也出现在应答里（曾经丢过 LastInsertId 的服务不是一次）；
//  3. ctime/mtime 取同一个 now，且都是当前时间而不是零值；
//  4. 创建草稿不写审计、不起事务、不碰 video_version/缓存。
func TestCreateSubmissionWritesDraftAndBackfillsAid(t *testing.T) {
	st := newStore()
	before := time.Now().Unix()
	in := &rpc.CreateSubmissionReq{Mid: 7, Title: "标题", Desc: "简介", Cover: "https://c/1.jpg", Typeid: 11, Tag: "a,b", Ip: "1.2.3.4"}

	got, err := NewCreateSubmissionLogic(context.Background(), st.svcCtx).CreateSubmission(in)
	wantNoErr(t, "创建稿件", err)
	wantSeq(t, "创建轨迹", st.log(), 0, "video_submission.Insert:m7/s1")

	s := got.GetSubmission()
	if s == nil {
		t.Fatal("应答里没有 submission")
	}
	stored := st.sub(s.GetAid())
	if stored == nil {
		t.Fatalf("应答 aid=%d 在库里不存在：主键没真的落库", s.GetAid())
	}
	wantEq(t, "落库", "aid", stored.Aid, s.GetAid())
	wantEq(t, "落库", "mid", stored.Mid, int64(7))
	wantEq(t, "落库", "title", stored.Title, "标题")
	wantEq(t, "落库", "desc", stored.Desc, "简介")
	wantEq(t, "落库", "cover", stored.Cover, "https://c/1.jpg")
	wantEq(t, "落库", "typeid", stored.Typeid, int32(11))
	wantEq(t, "落库", "tag", stored.Tag, "a,b")
	wantEq(t, "落库", "state", stored.State, model.StateDraft)
	wantEq(t, "投影", "state", s.GetState(), rpc.SubmissionState_STATE_DRAFT)

	if stored.Ctime < before || stored.Mtime < before {
		t.Errorf("ctime=%d mtime=%d 都应当 >= %d（未取当前时间）", stored.Ctime, stored.Mtime, before)
	}
	wantEq(t, "同一次 now", "ctime==mtime", stored.Ctime == stored.Mtime, true)
	wantEq(t, "投影", "ctime", s.GetCtime(), stored.Ctime)
	wantEq(t, "投影", "mtime", s.GetMtime(), stored.Mtime)

	// aid 由自增分配：库里已有的最大 aid + 1（此处从 1 开始）。
	wantEq(t, "首号", "aid", stored.Aid, int64(1))
}

// TestCreateSubmissionSecondAidDoesNotReuseFirst 锁连续两次投稿拿到不同 aid
// （若 logic 复用同一个 model 实例并覆盖入参，这里会立刻红）。
func TestCreateSubmissionSecondAidDoesNotReuseFirst(t *testing.T) {
	st := newStore()
	l := NewCreateSubmissionLogic(context.Background(), st.svcCtx)
	first, err := l.CreateSubmission(&rpc.CreateSubmissionReq{Mid: 7, Title: "a", Typeid: 1})
	wantNoErr(t, "第一次投稿", err)
	second, err := l.CreateSubmission(&rpc.CreateSubmissionReq{Mid: 7, Title: "b", Typeid: 1})
	wantNoErr(t, "第二次投稿", err)
	if first.GetSubmission().GetAid() == second.GetSubmission().GetAid() {
		t.Fatalf("两次投稿 aid 相同 = %d", first.GetSubmission().GetAid())
	}
	wantSeq(t, "两次投稿轨迹", st.log(), 0, "video_submission.Insert:m7/s1", "video_submission.Insert:m7/s1")
	wantEq(t, "行数", "video_submission", len(st.subs.rows), 2)
}

// TestCreateSubmissionPropagatesInsertError 锁「INSERT 失败原样透传」：
// 既不换成别的哨兵，也不返回半只稿件（aid=0 的成功应答是最坏结果）。
func TestCreateSubmissionPropagatesInsertError(t *testing.T) {
	st := newStore()
	st.fail("submission.Insert")

	got, err := NewCreateSubmissionLogic(context.Background(), st.svcCtx).
		CreateSubmission(&rpc.CreateSubmissionReq{Mid: 7, Title: "t", Typeid: 1})
	wantErrIs(t, "INSERT 故障", err, errBoom)
	if got != nil {
		t.Errorf("INSERT 故障仍返回 reply %+v（不能返回半只稿件）", got)
	}
	wantEq(t, "失败后", "行数", len(st.subs.rows), 0)
	wantSeq(t, "失败轨迹", st.log(), 0, "video_submission.Insert:m7/s1")
}
