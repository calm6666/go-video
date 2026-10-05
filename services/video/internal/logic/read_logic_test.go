package logic

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

// TestGetSubmissionRejectsInvalidAid 锁 aid 守卫先于触库。
func TestGetSubmissionRejectsInvalidAid(t *testing.T) {
	for _, aid := range []int64{0, -1, -101} {
		st := newStore()
		from := st.log().snapshot()
		got, err := NewGetSubmissionLogic(context.Background(), st.svcCtx).
			GetSubmission(&rpc.SubmissionReq{Aid: aid, Mid: 7})
		wantErrIs(t, "aid 非法", err, model.ErrInvalidAid)
		if got != nil {
			t.Errorf("aid=%d 拒绝时仍返回 reply %+v", aid, got)
		}
		wantNoCallAfter(t, "aid 非法", st.log(), from)
	}
}

// TestGetSubmissionReturnsProjectedRow 锁详情读只发一条 SELECT，
// 并且 model 行到 rpc.Submission 的 10 个字段逐个对上（字段绑错列在这里会红）。
func TestGetSubmissionReturnsProjectedRow(t *testing.T) {
	st := newStore()
	seed := st.seedDraft(101, 7, model.StateReadyForReview)

	got, err := NewGetSubmissionLogic(context.Background(), st.svcCtx).
		GetSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantNoErr(t, "查详情", err)
	wantSeq(t, "详情轨迹", st.log(), 0, "video_submission.FindOne:101")

	s := got.GetSubmission()
	wantEq(t, "投影", "aid", s.GetAid(), seed.Aid)
	wantEq(t, "投影", "mid", s.GetMid(), seed.Mid)
	wantEq(t, "投影", "title", s.GetTitle(), seed.Title)
	wantEq(t, "投影", "desc", s.GetDesc(), seed.Desc)
	wantEq(t, "投影", "cover", s.GetCover(), seed.Cover)
	wantEq(t, "投影", "typeid", s.GetTypeid(), seed.Typeid)
	wantEq(t, "投影", "tag", s.GetTag(), seed.Tag)
	wantEq(t, "投影", "state", s.GetState(), rpc.SubmissionState_STATE_READY_FOR_REVIEW)
	wantEq(t, "投影", "ctime", s.GetCtime(), seed.Ctime)
	wantEq(t, "投影", "mtime", s.GetMtime(), seed.Mtime)
}

// TestGetSubmissionMissingRowIsNotFound 锁「model 用 (nil,nil) 表示无行」被 logic 翻成
// ErrSubmissionNotFound——把 nil 直接投影出去会变成 nil reply + nil error。
func TestGetSubmissionMissingRowIsNotFound(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)

	got, err := NewGetSubmissionLogic(context.Background(), st.svcCtx).
		GetSubmission(&rpc.SubmissionReq{Aid: 999, Mid: 7})
	wantErrIs(t, "稿件不存在", err, model.ErrSubmissionNotFound)
	if got != nil {
		t.Errorf("稿件不存在仍返回 reply %+v", got)
	}
	wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:999")
}

// TestGetSubmissionPropagatesQueryError 锁 DB 故障不被降级成「稿件不存在」。
// 这两条语义在网关侧对应不同响应（5xx vs 404），混在一起就等于把故障藏起来。
func TestGetSubmissionPropagatesQueryError(t *testing.T) {
	st := newStore()
	st.fail("submission.FindOne")

	_, err := NewGetSubmissionLogic(context.Background(), st.svcCtx).
		GetSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantErrIs(t, "查询故障", err, errBoom)
	if errors.Is(err, model.ErrSubmissionNotFound) {
		t.Errorf("DB 故障被降级成「稿件不存在」：%v", err)
	}
}

// TestGetSubmissionDoesNotReadCache 锁当前真实形态：详情读**不查缓存**。
// repository.GetSubmission 直接回源 MySQL，README 里「稿件详情走 Redis 短 TTL 缓存」
// 只是设计意图；本用例把「一次读 = 一次 SELECT、零次缓存访问」钉住，
// 将来真接缓存时这条会红，逼着改文档与失效策略。
func TestGetSubmissionDoesNotReadCache(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StatePublished)

	_, err := NewGetSubmissionLogic(context.Background(), st.svcCtx).
		GetSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantNoErr(t, "查详情", err)
	wantSeq(t, "详情只读 DB", st.log(), 0, "video_submission.FindOne:101")
	wantCount(t, "详情不该碰缓存", st.log(), "cache.", 0)
}

// TestGetSubmissionIsNotOwnerScoped 把缺陷 pin 成哨兵：GetSubmission 完全不看的
// SubmissionReq.Mid，任何调用方都能拿到别人**未发布**稿件的标题/简介/封面/标签。
// 期望的修法（详情按 state 分可见性，或 DRAFT/REJECTED 仅属主与运营可读）
// 需要产品与网关共同定，不在本轮擅改，故只锁住现状：改名或加校验都会让本用例红。
func TestGetSubmissionIsNotOwnerScoped(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)

	got, err := NewGetSubmissionLogic(context.Background(), st.svcCtx).
		GetSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 888888})
	wantNoErr(t, "陌生 mid 读别人草稿", err)
	wantEq(t, "陌生 mid 读别人草稿", "title", got.GetSubmission().GetTitle(), "t-101")
	wantEq(t, "陌生 mid 读别人草稿", "desc", got.GetSubmission().GetDesc(), "d-101")
	wantEq(t, "轨迹", "调用次数", len(st.log().opsFrom(0)), 1)
}

// --- 分页查询 ---

// TestListSubmissionsRejectsBadPageSizeBeforeAnyDependency 锁 ps 守卫先于触库，
// 并把「ps=0 被拒」这件事 pin 住：model 本来会把 0 兜底成 20，logic 却直接报错，
// 两层口径不一致，客户端传 ps=0 拿不到默认页（缺陷见 README）。
func TestListSubmissionsRejectsBadPageSizeBeforeAnyDependency(t *testing.T) {
	for _, ps := range []int32{0, -1, 51, 1000} {
		st := newStore()
		from := st.log().snapshot()
		got, err := NewListSubmissionsLogic(context.Background(), st.svcCtx).
			ListSubmissions(&rpc.ListReq{Mid: 7, Ps: ps})
		wantErrIs(t, "ps 非法", err, model.ErrPsTooLarge)
		if got != nil {
			t.Errorf("ps=%d 拒绝时仍返回 reply %+v", ps, got)
		}
		wantNoCallAfter(t, "ps 非法", st.log(), from)
	}
}

// TestListSubmissionsFiltersAndOrders 锁列表口径三件事：
//  1. COUNT 先于 SELECT，且两次用同一个 (mid,typeid,pn,ps) 口径——
//     否则「total 与实际页内容来自不同条件」这种分页错乱无法被发现；
//  2. DELETED 稿件被 `state <> 14` 排除；
//  3. 结果按 aid 倒序，total 是命中总数而不是本页行数。
func TestListSubmissionsFiltersAndOrders(t *testing.T) {
	st := newStore()
	st.seedDraft(1, 7, model.StatePublished)
	st.seedDraft(2, 7, model.StateDraft)
	st.seedDraft(3, 7, model.StateDeleted) // 被 state<>DELETED 排除
	st.seedDraft(4, 8, model.StatePublished)
	st.seedDraft(5, 7, model.StateRejected)

	got, err := NewListSubmissionsLogic(context.Background(), st.svcCtx).
		ListSubmissions(&rpc.ListReq{Mid: 7, Pn: 1, Ps: 2})
	wantNoErr(t, "列表", err)
	wantSeq(t, "列表轨迹", st.log(), 0,
		"video_submission.List.count:7/0/1/2", "video_submission.List.rows:7/0/1/2")
	wantEq(t, "total", "命中总数", got.GetTotal(), int32(3))
	ids := submissionIDs(got.GetSubmissions())
	if !slices.Equal(ids, []int64{5, 2}) {
		t.Errorf("第一页 aid = %v, want [5 2]（aid 倒序 + LIMIT 2）", ids)
	}
}

// TestListSubmissionsSecondPage 锁翻页取 OFFSET：第二页拿到剩下的那条，不重复不遗漏。
func TestListSubmissionsSecondPage(t *testing.T) {
	st := newStore()
	for aid := int64(1); aid <= 5; aid++ {
		st.seedDraft(aid, 7, model.StatePublished)
	}

	got, err := NewListSubmissionsLogic(context.Background(), st.svcCtx).
		ListSubmissions(&rpc.ListReq{Mid: 7, Pn: 3, Ps: 2})
	wantNoErr(t, "第三页", err)
	wantEq(t, "total", "命中总数", got.GetTotal(), int32(5))
	ids := submissionIDs(got.GetSubmissions())
	if !slices.Equal(ids, []int64{1}) {
		t.Errorf("第三页 aid = %v, want [1]", ids)
	}
}

// TestListSubmissionsPastLastPageKeepsTotal 锁「越界页」的真实形态：
// COUNT 仍发、SELECT 返回空页，但 total 照给（客户端据此判断还能不能翻）。
func TestListSubmissionsPastLastPageKeepsTotal(t *testing.T) {
	st := newStore()
	st.seedDraft(1, 7, model.StatePublished)

	got, err := NewListSubmissionsLogic(context.Background(), st.svcCtx).
		ListSubmissions(&rpc.ListReq{Mid: 7, Pn: 9, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEq(t, "total", "命中总数", got.GetTotal(), int32(1))
	wantEq(t, "越界页", "本页行数", len(got.GetSubmissions()), 0)
	wantSeq(t, "越界页轨迹", st.log(), 0,
		"video_submission.List.count:7/0/9/2", "video_submission.List.rows:7/0/9/2")
}

// TestListSubmissionsEmptyDoesNotSelectRows 锁 total==0 时 model 短路，
// 不再发第二条 SELECT（空用户主页是高频路径）。
func TestListSubmissionsEmptyDoesNotSelectRows(t *testing.T) {
	st := newStore()
	st.seedDraft(1, 7, model.StatePublished)

	got, err := NewListSubmissionsLogic(context.Background(), st.svcCtx).
		ListSubmissions(&rpc.ListReq{Mid: 99, Pn: 1, Ps: 20})
	wantNoErr(t, "空列表", err)
	wantEq(t, "空列表", "total", got.GetTotal(), int32(0))
	if got.GetSubmissions() == nil {
		t.Error("Submissions = nil，want 非 nil 空切片（logic 用 make 兜住，客户端不该见到 null）")
	}
	wantEq(t, "空列表", "行数", len(got.GetSubmissions()), 0)
	wantSeq(t, "空列表轨迹", st.log(), 0, "video_submission.List.count:99/0/1/20")
}

// TestListSubmissionsTypeidFilterAndErrorPassthrough 锁 typeid 过滤同样进 COUNT/SELECT 口径，
// 并锁 SELECT 故障原样透传（不能被吞成空列表）。
func TestListSubmissionsTypeidFilterAndErrorPassthrough(t *testing.T) {
	st := newStore()
	st.seedSub(&model.VideoSubmission{Aid: 1, Mid: 7, Typeid: 11, State: model.StatePublished, Ctime: 100, Mtime: 100})
	st.seedSub(&model.VideoSubmission{Aid: 2, Mid: 7, Typeid: 12, State: model.StatePublished, Ctime: 100, Mtime: 100})

	got, err := NewListSubmissionsLogic(context.Background(), st.svcCtx).
		ListSubmissions(&rpc.ListReq{Mid: 7, Typeid: 12, Pn: 1, Ps: 20})
	wantNoErr(t, "分区过滤", err)
	wantSeq(t, "分区过滤轨迹", st.log(), 0,
		"video_submission.List.count:7/12/1/20", "video_submission.List.rows:7/12/1/20")
	wantEq(t, "分区过滤", "total", got.GetTotal(), int32(1))
	ids := submissionIDs(got.GetSubmissions())
	if !slices.Equal(ids, []int64{2}) {
		t.Errorf("分区过滤结果 = %v, want [2]", ids)
	}

	// COUNT 故障：整个调用必须失败，并且不再发 SELECT。
	st2 := newStore()
	st2.seedDraft(1, 7, model.StatePublished)
	st2.fail("submission.List.count")
	_, err = NewListSubmissionsLogic(context.Background(), st2.svcCtx).
		ListSubmissions(&rpc.ListReq{Mid: 7, Pn: 1, Ps: 20})
	wantErrIs(t, "COUNT 故障", err, errBoom)
	wantSeq(t, "COUNT 故障轨迹", st2.log(), 0, "video_submission.List.count:7/0/1/20")

	// 取行故障：logic 已经拿到 total，但整调用仍必须失败而不是返回「total 对、列表空」。
	st3 := newStore()
	st3.seedDraft(1, 7, model.StatePublished)
	st3.fail("submission.List.rows")
	_, err = NewListSubmissionsLogic(context.Background(), st3.svcCtx).
		ListSubmissions(&rpc.ListReq{Mid: 7, Pn: 1, Ps: 20})
	wantErrIs(t, "SELECT 故障", err, errBoom)
	wantSeq(t, "SELECT 故障轨迹", st3.log(), 0,
		"video_submission.List.count:7/0/1/20", "video_submission.List.rows:7/0/1/20")
}

// --- 按状态查询 ---

// TestListByStateGuardOrder 锁守卫顺序：ps 先判、state 后判。
// 两者都非法时报 ErrPsTooLarge——把顺序写反会让「ps 超限」变成「状态非法」，
// 运营后台的报错文案与埋点都会错位。
func TestListByStateGuardOrder(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListByStateReq
		want error
	}{
		{"ps=0", &rpc.ListByStateReq{State: rpc.SubmissionState_STATE_PUBLISHED, Pn: 1, Ps: 0}, model.ErrPsTooLarge},
		{"ps=51", &rpc.ListByStateReq{State: rpc.SubmissionState_STATE_PUBLISHED, Pn: 1, Ps: 51}, model.ErrPsTooLarge},
		{"ps 与 state 同时非法时先报 ps", &rpc.ListByStateReq{State: rpc.SubmissionState_STATE_UNSPECIFIED, Ps: 99}, model.ErrPsTooLarge},
		{"state=UNSPECIFIED", &rpc.ListByStateReq{State: rpc.SubmissionState_STATE_UNSPECIFIED, Pn: 1, Ps: 20}, model.ErrInvalidTargetState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			from := st.log().snapshot()
			got, err := NewListByStateLogic(context.Background(), st.svcCtx).ListByState(tc.in)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			wantNoCallAfter(t, tc.name, st.log(), from)
		})
	}
}

// TestListByStateFiltersByExactState 锁「按状态查询」只发那一条状态口径的 SQL，
// 并且 total 与页内容同一状态。
func TestListByStateFiltersByExactState(t *testing.T) {
	st := newStore()
	st.seedDraft(1, 7, model.StateReadyForReview)
	st.seedDraft(2, 7, model.StatePublished)
	st.seedDraft(3, 7, model.StateReadyForReview)

	got, err := NewListByStateLogic(context.Background(), st.svcCtx).
		ListByState(&rpc.ListByStateReq{State: rpc.SubmissionState_STATE_READY_FOR_REVIEW, Pn: 1, Ps: 20})
	wantNoErr(t, "按状态查", err)
	wantSeq(t, "按状态查轨迹", st.log(), 0,
		"video_submission.ListByState.count:6/1/20", "video_submission.ListByState.rows:6/1/20")
	wantEq(t, "按状态查", "total", got.GetTotal(), int32(2))
	ids := submissionIDs(got.GetSubmissions())
	if !slices.Equal(ids, []int64{3, 1}) {
		t.Errorf("按状态查结果 = %v, want [3 1]", ids)
	}
	for _, s := range got.GetSubmissions() {
		wantEq(t, "按状态查", "state", s.GetState(), rpc.SubmissionState_STATE_READY_FOR_REVIEW)
	}
}

// TestListByStateAcceptsUnknownEnumValue 把缺陷 pin 成哨兵：
// stateFromRPC 只判 0，不判 > STATE_DELETED 的取值，于是 state=99 会被当成
// 「查一个不存在的状态」打到 MySQL 并返回空列表，而不是报 ErrInvalidTargetState。
// 现状无数据风险（只会白查一次），但枚举越界应当被拒；修法要定「拒 or 归一」，
// 与网关枚举白名单一起看，本轮不改。
func TestListByStateAcceptsUnknownEnumValue(t *testing.T) {
	st := newStore()
	st.seedDraft(1, 7, model.StatePublished)

	got, err := NewListByStateLogic(context.Background(), st.svcCtx).
		ListByState(&rpc.ListByStateReq{State: rpc.SubmissionState(99), Pn: 1, Ps: 20})
	wantNoErr(t, "越界枚举", err)
	wantEq(t, "越界枚举", "total", got.GetTotal(), int32(0))
	wantSeq(t, "越界枚举轨迹", st.log(), 0, "video_submission.ListByState.count:99/1/20")
}

// TestListByStatePropagatesQueryError 锁 COUNT 故障原样透传且不越权取行。
func TestListByStatePropagatesQueryError(t *testing.T) {
	st := newStore()
	st.seedDraft(1, 7, model.StatePublished)
	st.fail("submission.ListByState.count")

	_, err := NewListByStateLogic(context.Background(), st.svcCtx).
		ListByState(&rpc.ListByStateReq{State: rpc.SubmissionState_STATE_PUBLISHED, Pn: 1, Ps: 20})
	wantErrIs(t, "COUNT 故障", err, errBoom)
	wantSeq(t, "COUNT 故障轨迹", st.log(), 0, "video_submission.ListByState.count:11/1/20")
}
