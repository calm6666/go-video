package logic

// update_submission_logic_test.go 钉 UpdateSubmission 的真实口径：
//   - 可改字段只有 title/desc/cover/typeid/tag 五个（生产 SQL 见
//     model/submissionmodel.go:176-191），其余列（mid/state/ctime/aid）不进入 SET 列表；
//   - 「未传＝保持原值」由 logic 的五个 if 实现（updatesubmissionlogic.go:49-68），
//     因此传空串**清不掉**任何字段；
//   - 顺序是「读 → UPDATE → 失效缓存 → 复读」，应答来自复读而不是内存拼装；
//   - RowsAffected==0 → ErrSubmissionNotFound，含真实 MySQL 的 changed-rows 差异。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

// updateSuccessSeq 是更新成功的完整依赖序列（由实现反推：
// logic 首读 → repository.UpdateSubmissionFields → 其内部 cache.DelSubmission → logic 复读）。
var updateSuccessSeq = []string{
	"video_submission.FindOne:101",
	"video_submission.UpdateFields:101",
	"cache.DelSubmission:101",
	"video_submission.FindOne:101",
}

func updateCall(st *store, in *rpc.UpdateSubmissionReq) (*rpc.SubmissionReply, error) {
	return NewUpdateSubmissionLogic(context.Background(), st.svcCtx).UpdateSubmission(in)
}

// TestUpdateSubmissionGuardsRejectBeforeAnyDependency 锁 aid → mid 的守卫顺序，
// 并锁拒绝时一次 SQL/缓存调用都不发生、库里那一行原封不动。
func TestUpdateSubmissionGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.UpdateSubmissionReq
		want error
	}{
		{"aid=0", &rpc.UpdateSubmissionReq{Aid: 0, Mid: 7, Title: "x"}, model.ErrInvalidAid},
		{"aid<0", &rpc.UpdateSubmissionReq{Aid: -1, Mid: 7, Title: "x"}, model.ErrInvalidAid},
		{"mid=0", &rpc.UpdateSubmissionReq{Aid: 101, Mid: 0, Title: "x"}, model.ErrInvalidMid},
		{"mid<0", &rpc.UpdateSubmissionReq{Aid: 101, Mid: -3, Title: "x"}, model.ErrInvalidMid},
		{"两个都非法时先报 aid（守卫顺序）", &rpc.UpdateSubmissionReq{Aid: 0, Mid: 0, Title: "x"}, model.ErrInvalidAid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedDraft(101, 7, model.StateDraft)
			from := st.log().snapshot()

			got, err := updateCall(st, tc.in)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			wantNoCallAfter(t, tc.name, st.log(), from)

			stored := st.sub(101)
			if stored == nil {
				t.Fatal("守卫拒绝后稿件行不见了")
			}
			wantEq(t, tc.name, "title", stored.Title, "t-101")
			wantEq(t, tc.name, "desc", stored.Desc, "d-101")
			wantEq(t, tc.name, "cover", stored.Cover, "c-101")
			wantEq(t, tc.name, "typeid", stored.Typeid, int32(11))
			wantEq(t, tc.name, "tag", stored.Tag, "g-101")
			wantEq(t, tc.name, "mtime 未刷新", stored.Mtime, staleTime)
		})
	}
}

// TestUpdateSubmissionOnlyWritesTheFiveMetaColumns 逐字段钉「哪些能改、未传不动」：
// 每个用例只传一个字段，另外四个必须保持 seed 值（回读库里那一行比对，不看返回值）。
// 最后一例是「一个都不传」——现状它会照发一条 UPDATE 把原值写回去，并且**清不掉** desc，
// 这条与 TestUpdateSubmissionNoOpUpdateDivergence 一起构成空更新口径的边界。
func TestUpdateSubmissionOnlyWritesTheFiveMetaColumns(t *testing.T) {
	cases := []struct {
		name       string
		patch      *rpc.UpdateSubmissionReq
		wantTitle  string
		wantDesc   string
		wantCover  string
		wantTypeid int32
		wantTag    string
	}{
		{
			name:      "只改 title",
			patch:     &rpc.UpdateSubmissionReq{Title: "新标题"},
			wantTitle: "新标题", wantDesc: "d-101", wantCover: "c-101", wantTypeid: 11, wantTag: "g-101",
		},
		{
			name:      "只改 desc",
			patch:     &rpc.UpdateSubmissionReq{Desc: "新简介"},
			wantTitle: "t-101", wantDesc: "新简介", wantCover: "c-101", wantTypeid: 11, wantTag: "g-101",
		},
		{
			name:      "只改 cover",
			patch:     &rpc.UpdateSubmissionReq{Cover: "https://c/2.jpg"},
			wantTitle: "t-101", wantDesc: "d-101", wantCover: "https://c/2.jpg", wantTypeid: 11, wantTag: "g-101",
		},
		{
			name:      "只改 typeid",
			patch:     &rpc.UpdateSubmissionReq{Typeid: 12},
			wantTitle: "t-101", wantDesc: "d-101", wantCover: "c-101", wantTypeid: 12, wantTag: "g-101",
		},
		{
			name:      "只改 tag",
			patch:     &rpc.UpdateSubmissionReq{Tag: "x,y"},
			wantTitle: "t-101", wantDesc: "d-101", wantCover: "c-101", wantTypeid: 11, wantTag: "x,y",
		},
		{
			name: "五个全改",
			patch: &rpc.UpdateSubmissionReq{
				Title: "T2", Desc: "D2", Cover: "C2", Typeid: 99, Tag: "G2",
			},
			wantTitle: "T2", wantDesc: "D2", wantCover: "C2", wantTypeid: 99, wantTag: "G2",
		},
		{
			name:      "desc 传空串＝保持原值（清空做不到）",
			patch:     &rpc.UpdateSubmissionReq{Desc: ""},
			wantTitle: "t-101", wantDesc: "d-101", wantCover: "c-101", wantTypeid: 11, wantTag: "g-101",
		},
		{
			name:      "一个都不传＝整行原值回写",
			patch:     &rpc.UpdateSubmissionReq{},
			wantTitle: "t-101", wantDesc: "d-101", wantCover: "c-101", wantTypeid: 11, wantTag: "g-101",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedDraft(101, 7, model.StateDraft)

			in := tc.patch
			in.Aid, in.Mid = 101, 7
			got, err := updateCall(st, in)
			wantNoErr(t, tc.name, err)

			// 库里那一行才是结论：五个可写列 + 只有 mtime 被 SQL 刷新。
			stored := st.sub(101)
			if stored == nil {
				t.Fatal("更新后稿件行不见了")
			}
			wantEq(t, tc.name, "title", stored.Title, tc.wantTitle)
			wantEq(t, tc.name, "desc", stored.Desc, tc.wantDesc)
			wantEq(t, tc.name, "cover", stored.Cover, tc.wantCover)
			wantEq(t, tc.name, "typeid", stored.Typeid, tc.wantTypeid)
			wantEq(t, tc.name, "tag", stored.Tag, tc.wantTag)
			wantEq(t, tc.name, "aid", stored.Aid, int64(101))
			wantEq(t, tc.name, "mid", stored.Mid, int64(7))
			wantEq(t, tc.name, "state 不进 SET 列表", stored.State, model.StateDraft)
			wantEq(t, tc.name, "ctime 不进 SET 列表", stored.Ctime, staleTime)
			if stored.Mtime <= staleTime {
				t.Errorf("%s：mtime = %d，生产 SQL 的 SET mtime = nowUnix() 没生效", tc.name, stored.Mtime)
			}

			// 一次更新＝一条 UPDATE 语句（不是按字段拆成多条），且顺序完整。
			wantSeq(t, tc.name, st.log(), 0, updateSuccessSeq...)
			// 不越权碰别的表。
			wantCount(t, tc.name, st.log(), "video_version.", 0)
			wantCount(t, tc.name, st.log(), "video_audit_log.", 0)

			// 应答与库一致（不是把入参回显）。
			s := got.GetSubmission()
			wantEq(t, tc.name, "应答 title", s.GetTitle(), stored.Title)
			wantEq(t, tc.name, "应答 desc", s.GetDesc(), stored.Desc)
			wantEq(t, tc.name, "应答 typeid", s.GetTypeid(), stored.Typeid)
			wantEq(t, tc.name, "应答 state", s.GetState(), rpc.SubmissionState_STATE_DRAFT)
		})
	}
}

// TestUpdateSubmissionReplyComesFromReread 锁应答里的 mtime 是**落库后**的那个值：
// 首读拿到的 mtime 恒为 staleTime，若 logic 用内存里的 sub 拼应答就会在这里红。
// 同时钉「失效缓存发生在 UPDATE 之后、复读之前」这个顺序——缓存先失效、后落库
// 会留下「缓存已空但库里还是旧值」的窗口反过来（旧值又被复读填不回来，但顺序错位
// 本身就是客户端读到旧版的窗口）。
func TestUpdateSubmissionReplyComesFromReread(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)

	got, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "复读值"})
	wantNoErr(t, "更新", err)
	stored := st.sub(101)
	wantEq(t, "应答来源", "应答 mtime", got.GetSubmission().GetMtime(), stored.Mtime)
	if got.GetSubmission().GetMtime() == staleTime {
		t.Errorf("应答 mtime 仍是 seed 的 %d，说明 reply 来自首读而不是复读", staleTime)
	}
	wantSeq(t, "落库→失效→复读", st.log(), 0, updateSuccessSeq...)
	if len(st.cache.dels) != 1 || st.cache.dels[0] != 101 {
		t.Errorf("失效的稿件详情键应只作用在 aid=101，实得 %v", st.cache.dels)
	}
}

// TestUpdateSubmissionOwnershipAndStateGuards 锁三条拒绝路径都「只读不写」：
// 稿件不存在、非属主、非 DRAFT；并钉住守卫顺序 mid 先于 state
// （别人的已发布稿件报 ErrNotOwner 而不是 ErrSubmissionNotDraft，否则等于向陌生
// mid 泄露「这只稿件存在且已发布」）。
func TestUpdateSubmissionOwnershipAndStateGuards(t *testing.T) {
	t.Run("稿件不存在", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)

		got, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 999, Mid: 7, Title: "x"})
		wantErrIs(t, "稿件不存在", err, model.ErrSubmissionNotFound)
		if got != nil {
			t.Errorf("稿件不存在仍返回 reply %+v", got)
		}
		wantSeq(t, "稿件不存在轨迹", st.log(), 0, "video_submission.FindOne:999")
	})

	t.Run("非属主", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)

		_, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 8, Title: "x"})
		wantErrIs(t, "非属主", err, model.ErrNotOwner)
		wantSeq(t, "非属主轨迹", st.log(), 0, "video_submission.FindOne:101")
		wantEq(t, "非属主", "落库标题", st.sub(101).Title, "t-101")
	})

	t.Run("陌生 mid 读别人已发布稿件时先报 not owner", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StatePublished)

		_, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 8, Title: "x"})
		wantErrIs(t, "守卫顺序", err, model.ErrNotOwner)
		if errors.Is(err, model.ErrSubmissionNotDraft) {
			t.Errorf("守卫顺序写反了：%v", err)
		}
	})

	t.Run("除 DRAFT 外一律拒改", func(t *testing.T) {
		allStates := []int32{
			model.StateDraft, model.StateUploading, model.StateUploaded, model.StateScanning,
			model.StateTranscoding, model.StateReadyForReview, model.StateRejected, model.StateAppeal,
			model.StateApproved, model.StateScheduled, model.StatePublished, model.StateOffline,
			model.StateExpired, model.StateDeleted,
		}
		for _, state := range allStates {
			if state == model.StateDraft {
				continue // DRAFT 可改已由 TestUpdateSubmissionOnlyWritesTheFiveMetaColumns 钉住
			}
			st := newStore()
			st.seedDraft(101, 7, state)

			_, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "改一下"})
			wantErrIs(t, "非 DRAFT 拒改", err, model.ErrSubmissionNotDraft)
			wantSeq(t, "非 DRAFT 轨迹", st.log(), 0, "video_submission.FindOne:101")
			wantEq(t, "非 DRAFT 拒改", "标题未变", st.sub(101).Title, "t-101")
			wantEq(t, "非 DRAFT 拒改", "mtime 未刷新", st.sub(101).Mtime, staleTime)
			wantEq(t, "非 DRAFT 拒改", "state 未被顺带推进", st.sub(101).State, state)
		}
	})
}

// TestUpdateSubmissionLostRowIsNotFound 钉 model 的 RowsAffected==0 口径：
// logic 首读时行还在，UPDATE 之前被别的入口删掉 → 生产 SQL 命中 0 行 →
// ErrSubmissionNotFound（updatesubmissionlogic.go:69-72 原样透传）。
// 判别性在边界两侧：行在时成功（前一用例），行没了必须 not found，
// 且缓存失效与复读都不许发生。
func TestUpdateSubmissionLostRowIsNotFound(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.before("submission.UpdateFields", func() { st.dropSub(101) })

	got, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "x"})
	wantErrIs(t, "更新时行已被删除", err, model.ErrSubmissionNotFound)
	if got != nil {
		t.Errorf("not found 仍返回 reply %+v", got)
	}
	wantSeq(t, "轨迹", st.log(), 0,
		"video_submission.FindOne:101", "video_submission.UpdateFields:101")
	wantCount(t, "更新失败不该失效缓存", st.log(), "cache.", 0)
	if st.sub(101) != nil {
		t.Error("行又被写回来了")
	}
	st.s.checkHooks(t)
}

// TestUpdateSubmissionNoOpUpdateDivergence 钉住本仓 fake 与真实 MySQL 的那处差异：
// etc/video.v1.yaml:14 的 DSN 未开 clientFoundRows，所以真实 MySQL 的
// RowsAffected 是 **changed rows**：把 title 更新成它已有的值命中 0 行，
// model/submissionmodel.go:187-189 就判 ErrSubmissionNotFound——
// 属主改自己的草稿、只是这次什么都没动，在真库里会拿到「稿件不存在」。
// 替身默认按「WHERE 命中行数」实现（等价于开了 clientFoundRows），
// markChangedRows() 打开真实行为；同一条入参在两种口径下结论相反。
func TestUpdateSubmissionNoOpUpdateDivergence(t *testing.T) {
	run := func(changedRowsOnly bool, patch *rpc.UpdateSubmissionReq) (*rpc.SubmissionReply, error, *store) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		if changedRowsOnly {
			st.markChangedRows()
		}
		patch.Aid, patch.Mid = 101, 7
		got, err := updateCall(st, patch)
		return got, err, st
	}
	emptyPatch := func() *rpc.UpdateSubmissionReq { return &rpc.UpdateSubmissionReq{} }
	realPatch := func() *rpc.UpdateSubmissionReq {
		return &rpc.UpdateSubmissionReq{Title: "真的有改动"}
	}

	// 边界 A：默认口径（命中行数）——空更新「成功」，但仍会刷 mtime、仍会失效缓存。
	got, err, st := run(false, emptyPatch())
	wantNoErr(t, "替身口径下的空更新", err)
	wantSeq(t, "替身口径轨迹", st.log(), 0, updateSuccessSeq...)
	wantEq(t, "替身口径", "mtime 仍被刷新", st.sub(101).Mtime > staleTime, true)
	wantEq(t, "替身口径", "应答标题", got.GetSubmission().GetTitle(), "t-101")

	// 边界 B：真实 MySQL 口径（changed rows）——同一份入参被判 not found。
	got, err, st = run(true, emptyPatch())
	wantErrIs(t, "真实 MySQL 的空更新", err, model.ErrSubmissionNotFound)
	if got != nil {
		t.Errorf("真库空更新返回了 reply %+v", got)
	}
	stored := st.sub(101)
	if stored == nil {
		t.Fatal("行本身是被真删了，用例前提不成立")
	}
	wantEq(t, "真库空更新", "库里标题照旧", stored.Title, "t-101")
	wantEq(t, "真库空更新", "库里 mtime 照旧", stored.Mtime, staleTime)
	wantSeq(t, "真库空更新轨迹", st.log(), 0,
		"video_submission.FindOne:101", "video_submission.UpdateFields:101")
	wantCount(t, "真库空更新不该失效缓存", st.log(), "cache.", 0)

	// 对照组：同一开关下「真有改动」仍然成功——红/绿只由「有没有真的改到值」决定。
	got, err, st = run(true, realPatch())
	wantNoErr(t, "真实 MySQL 下的有效更新", err)
	wantEq(t, "真实 MySQL 下的有效更新", "库里标题", st.sub(101).Title, "真的有改动")
	wantSeq(t, "有效更新轨迹", st.log(), 0, updateSuccessSeq...)
	wantEq(t, "真实 MySQL 下的有效更新", "应答标题", got.GetSubmission().GetTitle(), "真的有改动")
}

// TestUpdateSubmissionPropagatesDependencyErrors 钉三段故障各自的「库里还剩什么」：
// 首读失败＝什么都没发生；UPDATE 失败＝什么都没发生；
// 复读失败＝**更新已经落库**（本仓 fake 与真库一样不会回滚已提交的 UPDATE），
// 但调用整体报错——客户端会看到失败、稿件却已经改了，这是真实的可观测后果。
func TestUpdateSubmissionPropagatesDependencyErrors(t *testing.T) {
	t.Run("首读故障", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("submission.FindOne")

		got, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "x"})
		wantErrIs(t, "首读故障", err, errBoom)
		if errors.Is(err, model.ErrSubmissionNotFound) {
			t.Errorf("DB 故障被降级成「稿件不存在」：%v", err)
		}
		if got != nil {
			t.Errorf("故障仍返回 reply %+v", got)
		}
		wantSeq(t, "首读故障轨迹", st.log(), 0, "video_submission.FindOne:101")
		wantEq(t, "首读故障", "库里标题", st.sub(101).Title, "t-101")
	})

	t.Run("UPDATE 故障", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("submission.UpdateFields")

		_, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "x"})
		wantErrIs(t, "UPDATE 故障", err, errBoom)
		wantSeq(t, "UPDATE 故障轨迹", st.log(), 0,
			"video_submission.FindOne:101", "video_submission.UpdateFields:101")
		wantEq(t, "UPDATE 故障", "库里标题", st.sub(101).Title, "t-101")
		wantCount(t, "UPDATE 故障", st.log(), "cache.", 0)
	})

	t.Run("复读故障时更新已经生效", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		// 钩子挂在 UPDATE 之前：第一次 FindOne 已经过去了，所以只影响 logic 的复读。
		st.before("submission.UpdateFields", func() { st.s.fail("submission.FindOne", errBoom) })

		got, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "已经落库"})
		wantErrIs(t, "复读故障", err, errBoom)
		if got != nil {
			t.Errorf("复读故障仍返回 reply %+v", got)
		}
		wantEq(t, "复读故障", "库里标题其实已改", st.sub(101).Title, "已经落库")
		wantSeq(t, "复读故障轨迹", st.log(), 0,
			"video_submission.FindOne:101", "video_submission.UpdateFields:101",
			"cache.DelSubmission:101", "video_submission.FindOne:101")
		// 复读确实发了 SQL，只是失败——不是 logic 自己拼了个 reply 再报错。
		wantCount(t, "复读故障", st.log(), "video_submission.UpdateFields", 1)
		st.s.checkHooks(t)
	})
}

// TestUpdateSubmissionIgnoresCacheInvalidationFailure 钉住 repository 的取舍：
// UpdateSubmissionFields 里 `_ = r.cache.DelSubmission(...)`（repository.go:125），
// 缓存失效失败既不上报、也不回滚已经落库的 UPDATE。
// 现状无害（全仓没有任何写入 video:sub:* 的代码，缓存恒空），
// 一旦接入 cache-aside 这条就是「客户端能读到旧详情 60 秒」的成因，见 README 已知缺口 2。
func TestUpdateSubmissionIgnoresCacheInvalidationFailure(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.fail("cache.DelSubmission")

	got, err := updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "照样成功"})
	wantNoErr(t, "缓存失效失败", err)
	wantEq(t, "缓存失效失败", "库里标题", st.sub(101).Title, "照样成功")
	wantEq(t, "缓存失效失败", "应答标题", got.GetSubmission().GetTitle(), "照样成功")
	wantSeq(t, "缓存失效失败轨迹", st.log(), 0, updateSuccessSeq...)
	if len(st.cache.dels) != 0 {
		t.Errorf("失效失败的 key 不该被记为已失效，实得 %v", st.cache.dels)
	}
}
