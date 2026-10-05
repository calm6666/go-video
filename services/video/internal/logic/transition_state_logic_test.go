package logic

// transition_state_logic_test.go 钉 TransitionState 的真实口径：
//   - 合法转换表只有一个来源：statemachine.go 的 legalTransitions（本文件一律由它反推期望值，
//     不手抄枚举顺序）；§8 的「禁止抄近路写 PUBLISHED」就是这张表没有跨级入边；
//   - 入参守卫只有两条：aid>0、target!=STATE_UNSPECIFIED；越界枚举值（99/-1）不被守卫拦下，
//     而是在读库之后被状态机拒（与 ListByState 同一口径，见 README 已知缺口 6）；
//   - 一次推进 = 一个 TransactCtx 内「二次校验读 → 状态 UPDATE → 审计 INSERT」，
//     随后失效稿件详情缓存，最后复读拼应答；
//   - 并发窗口只在「事务内二次校验之前」闭合：UPDATE 没有 state 的 CAS 条件，
//     校验后被插队属于丢失更新且检测不到（TestTransitionStateLostUpdateIsNotDetected）。
//
// 本仓 fake 不回滚（fakes_test.go 纪律 5），失败用例一律断言「库里实际还剩什么」。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

func transitionCall(st *store, in *rpc.TransitionReq) (*rpc.SubmissionReply, error) {
	return NewTransitionStateLogic(context.Background(), st.svcCtx).TransitionState(in)
}

// transitionSeq 是「推进成功」的完整依赖序列（由实现反推：
// logic 首读 → repository.TransitionState 开事务 → 事务内二次校验读（不带 session，
// 与首读是同一条 FindOne SQL）→ UpdateState(session) → audit Insert(session) →
// 可见性转换再加一次 video_outbox.Insert(session) → logic 失效缓存 → logic 复读）。
// 事件行的有无取自本包自己的判定表 wantEventAction，不调用 repository 的实现。
func transitionSeq(aid int64, from, to int32) []string {
	ops := []string{
		fmt.Sprintf("video_submission.FindOne:%d", aid),
		"db.TransactCtx",
		fmt.Sprintf("video_submission.FindOne:%d", aid),
		fmt.Sprintf("video_submission.UpdateState:%d/%d", aid, to),
		fmt.Sprintf("video_audit_log.Insert:%d:%d->%d", aid, from, to),
	}
	if wantEventAction(from, to) != "" {
		ops = append(ops, fmt.Sprintf("video_outbox.Insert:%d", aid))
	}
	ops = append(ops,
		fmt.Sprintf("cache.DelSubmission:%d", aid),
		fmt.Sprintf("video_submission.FindOne:%d", aid),
	)
	return ops
}

var allSubmissionStates = []int32{
	model.StateDraft, model.StateUploading, model.StateUploaded, model.StateScanning,
	model.StateTranscoding, model.StateReadyForReview, model.StateRejected, model.StateAppeal,
	model.StateApproved, model.StateScheduled, model.StatePublished, model.StateOffline,
	model.StateExpired, model.StateDeleted,
}

// TestTransitionStateGuardsRejectBeforeAnyDependency 锁守卫顺序 aid → target，
// 以及 target=STATE_UNSPECIFIED 必须在触库之前被拒（否则「未指定」会先打一条 SELECT）。
func TestTransitionStateGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.TransitionReq
		want error
	}{
		{"aid=0", &rpc.TransitionReq{Aid: 0, Target: rpc.SubmissionState_STATE_UPLOADING}, model.ErrInvalidAid},
		{"aid<0", &rpc.TransitionReq{Aid: -1, Target: rpc.SubmissionState_STATE_UPLOADING}, model.ErrInvalidAid},
		{"target=UNSPECIFIED", &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UNSPECIFIED}, model.ErrInvalidTargetState},
		{"target=0 的显式零值", &rpc.TransitionReq{Aid: 101}, model.ErrInvalidTargetState},
		{"两个都非法时先报 aid（守卫顺序）", &rpc.TransitionReq{Aid: 0, Target: rpc.SubmissionState_STATE_UNSPECIFIED}, model.ErrInvalidAid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedDraft(101, 7, model.StateDraft)
			from := st.log().snapshot()

			got, err := transitionCall(st, tc.in)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			wantNoCallAfter(t, tc.name, st.log(), from)
			wantEq(t, tc.name, "state", st.sub(101).State, model.StateDraft)
			wantEq(t, tc.name, "审计行数", st.auditCount(), 0)
		})
	}
}

// TestTransitionStateRejectsOutOfRangeEnumAfterReading 钉「枚举越界」的真实落点：
// stateFromRPC 是无条件 int32 强转（convert.go:14-16），守卫只在调用点上判 0
// （transitionstatelogic.go:35-38），99/-1 会先打一条 SELECT，
// 再被状态机拒成 ErrInvalidStateTransition 而不是 ErrInvalidTargetState。
// 与 ListByState 的同类口径一致（read_logic_test.go 的 TestListByStateAcceptsUnknownEnumValue）：
// 两边都是「读已经发生、才被业务层拒」，差别只在白查的是一条稿件 SELECT 还是一次 COUNT。
func TestTransitionStateRejectsOutOfRangeEnumAfterReading(t *testing.T) {
	for _, target := range []rpc.SubmissionState{99, -1, 15} {
		st := newStore()
		st.seedDraft(101, 7, model.StatePublished)

		got, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: target})
		wantErrIs(t, fmt.Sprintf("target=%d", target), err, model.ErrInvalidStateTransition)
		if errors.Is(err, model.ErrInvalidTargetState) {
			t.Errorf("target=%d 被守卫拦下了，与实现不符（守卫只判 0）", target)
		}
		if got != nil {
			t.Errorf("target=%d 拒绝时仍返回 reply %+v", target, got)
		}
		wantSeq(t, "越界枚举轨迹", st.log(), 0, "video_submission.FindOne:101")
		wantEq(t, "越界枚举", "state", st.sub(101).State, model.StatePublished)
		wantCount(t, "越界枚举", st.log(), "cache.", 0)
	}
}

// TestTransitionStateAcceptsExactlyTheDeclaredMatrix 用 statemachine.go 的矩阵本身生成期望：
// 对 14×14 每一对 (from,to) 跑一次 RPC，成功当且仅当 to ∈ legalTransitions[from]。
// 于是「矩阵加一条边」「logic 改成不查矩阵」「fake 少记一次调用」都会在这里红，
// 而不是靠用例里手抄的一串枚举。
func TestTransitionStateAcceptsExactlyTheDeclaredMatrix(t *testing.T) {
	for _, from := range allSubmissionStates {
		for _, to := range allSubmissionStates {
			from, to := from, to
			t.Run(fmt.Sprintf("%d->%d", from, to), func(t *testing.T) {
				st := newStore()
				st.seedDraft(101, 7, from)

				got, err := transitionCall(st, &rpc.TransitionReq{
					Aid: 101, Target: stateToRPC(to), Operator: "matrix", Reason: "r",
				})
				wantOK := slices.Contains(legalTransitions[from], to)
				if !wantOK {
					wantErrIs(t, "矩阵外转换", err, model.ErrInvalidStateTransition)
					if got != nil {
						t.Errorf("%d->%d 被拒仍返回 reply %+v", from, to, got)
					}
					wantSeq(t, "矩阵外转换轨迹", st.log(), 0, "video_submission.FindOne:101")
					wantEq(t, "矩阵外转换", "state 不变", st.sub(101).State, from)
					wantEq(t, "矩阵外转换", "mtime 不变", st.sub(101).Mtime, staleTime)
					wantEq(t, "矩阵外转换", "事务次数", st.txCount(), 0)
					wantEq(t, "矩阵外转换", "审计行数", st.auditCount(), 0)
					wantCount(t, "矩阵外转换", st.log(), "cache.", 0)
					return
				}

				wantNoErr(t, "矩阵内转换", err)
				wantSeq(t, "矩阵内转换轨迹", st.log(), 0, transitionSeq(101, from, to)...)
				wantEq(t, "矩阵内转换", "库里 state", st.sub(101).State, to)
				wantEq(t, "矩阵内转换", "事务次数", st.txCount(), 1)
				wantEq(t, "矩阵内转换", "审计行数", st.auditCount(), 1)
				wantEq(t, "矩阵内转换", "应答 state", got.GetSubmission().GetState(), stateToRPC(to))
			})
		}
	}
}

// TestTransitionStatePublishChainHasNoShortcut 钉 §8 的落地形态两件事：
//  1. DRAFT→…→PUBLISHED 这条 8 步链路每一步都合法，且每步恰好一个事务 + 一条审计，
//     审计链首尾相接（from 等于上一步的 to）；
//  2. 任何跨级跳到 PUBLISHED 都被拒——包括 APPROVED→PUBLISHED 这种「看起来只差一步」。
func TestTransitionStatePublishChainHasNoShortcut(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)

	chain := []int32{
		model.StateUploading, model.StateUploaded, model.StateScanning, model.StateTranscoding,
		model.StateReadyForReview, model.StateApproved, model.StateScheduled, model.StatePublished,
	}
	prev := model.StateDraft // 首步的 from 由 seed 决定
	for i, to := range chain {
		got, err := transitionCall(st, &rpc.TransitionReq{
			Aid: 101, Target: stateToRPC(to), Operator: fmt.Sprintf("step-%d", i), Reason: "链路",
		})
		wantNoErr(t, fmt.Sprintf("链路第 %d 步 %d->%d", i+1, prev, to), err)
		wantEq(t, "链路", "库里 state", st.sub(101).State, to)
		wantEq(t, "链路", "事务累计", st.txCount(), i+1)
		wantEq(t, "链路", "审计累计", st.auditCount(), i+1)
		wantEq(t, "链路", "应答 state", got.GetSubmission().GetState(), stateToRPC(to))
		prev = to
	}

	rows := st.auditRows(101)
	if len(rows) != len(chain) {
		t.Fatalf("审计行数 = %d, want %d", len(rows), len(chain))
	}
	wantFrom := model.StateDraft
	for i, r := range rows {
		wantEq(t, fmt.Sprintf("审计第 %d 条", i+1), "from_state", r.FromState, wantFrom)
		wantEq(t, fmt.Sprintf("审计第 %d 条", i+1), "to_state", r.ToState, chain[i])
		wantEq(t, fmt.Sprintf("审计第 %d 条", i+1), "operator", r.Operator, fmt.Sprintf("step-%d", i))
		wantEq(t, fmt.Sprintf("审计第 %d 条", i+1), "reason", r.Reason, "链路")
		wantFrom = chain[i]
	}
	// 每步都失效一次详情缓存，且顺序都在事务之后（9 次，不重不漏）。
	if len(st.cache.dels) != len(chain) {
		t.Errorf("缓存失效次数 = %d, want %d（%v）", len(st.cache.dels), len(chain), st.cache.dels)
	}
	for _, aid := range st.cache.dels {
		wantEq(t, "缓存失效", "aid", aid, int64(101))
	}

	// 抄近路：稿件已经 PUBLISHED，任何「跨级回到中段」或「直接再发一次」都被拒。
	for _, to := range []int32{model.StatePublished, model.StateApproved, model.StateReadyForReview, model.StateDraft} {
		before := st.log().snapshot()
		_, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: stateToRPC(to), Operator: "抄近路"})
		wantErrIs(t, fmt.Sprintf("PUBLISHED->%d", to), err, model.ErrInvalidStateTransition)
		wantSeq(t, "抄近路轨迹", st.log(), before, "video_submission.FindOne:101")
		wantEq(t, "抄近路", "state 仍是 PUBLISHED", st.sub(101).State, model.StatePublished)
		wantEq(t, "抄近路", "审计不增加", st.auditCount(), len(chain))
	}
}

// TestTransitionStateDirectPublishFromMidStatesIsRejected 单独钉「未审核稿件被直接发布」：
// 上传/转码/审核中的稿件都不存在到 PUBLISHED 的边，媒体回调即使想越权也推不动。
func TestTransitionStateDirectPublishFromMidStatesIsRejected(t *testing.T) {
	for _, from := range []int32{
		model.StateDraft, model.StateUploading, model.StateUploaded,
		model.StateScanning, model.StateTranscoding, model.StateReadyForReview, model.StateApproved,
	} {
		st := newStore()
		st.seedDraft(101, 7, from)

		_, err := transitionCall(st, &rpc.TransitionReq{
			Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED, Operator: "media-worker", Reason: "转码完成",
		})
		wantErrIs(t, fmt.Sprintf("%d 直接发布", from), err, model.ErrInvalidStateTransition)
		wantSeq(t, "越权发布轨迹", st.log(), 0, "video_submission.FindOne:101")
		wantEq(t, "越权发布", "state 不变", st.sub(101).State, from)
		wantEq(t, "越权发布", "事务次数", st.txCount(), 0)
		wantEq(t, "越权发布", "审计行数", st.auditCount(), 0)
	}
}

// TestTransitionStateReplyComesFromRereadAndOnlyMetaColumnsUntouched 钉成功路径的落库面：
//   - 只有 state 与 mtime 变（UPDATE ... SET state=?, mtime=? WHERE aid=?），
//     title/desc/cover/typeid/tag/mid/ctime 都保持原值；
//   - 应答的 mtime 等于库里的新 mtime 而不是首读的 staleTime，证明 reply 来自复读；
//   - 版次表一个字节都不写（发布不等于产媒资）。
func TestTransitionStateReplyComesFromRereadAndOnlyMetaColumnsUntouched(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateScheduled)
	st.seedVersion(&model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", State: model.StatePublished, Ctime: 100})

	got, err := transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED, Operator: "cron:publish", Reason: "到点发布",
	})
	wantNoErr(t, "定时发布", err)

	stored := st.sub(101)
	wantEq(t, "发布", "state", stored.State, model.StatePublished)
	wantEq(t, "发布", "title", stored.Title, "t-101")
	wantEq(t, "发布", "desc", stored.Desc, "d-101")
	wantEq(t, "发布", "cover", stored.Cover, "c-101")
	wantEq(t, "发布", "typeid", stored.Typeid, int32(11))
	wantEq(t, "发布", "tag", stored.Tag, "g-101")
	wantEq(t, "发布", "mid", stored.Mid, int64(7))
	wantEq(t, "发布", "ctime", stored.Ctime, staleTime)
	if stored.Mtime <= staleTime {
		t.Errorf("mtime = %d，状态 UPDATE 没刷新 mtime", stored.Mtime)
	}
	wantEq(t, "应答来自复读", "应答 mtime", got.GetSubmission().GetMtime(), stored.Mtime)
	wantEq(t, "应答来自复读", "应答 state", got.GetSubmission().GetState(), rpc.SubmissionState_STATE_PUBLISHED)
	wantEq(t, "应答投影", "应答 aid", got.GetSubmission().GetAid(), int64(101))

	wantSeq(t, "发布轨迹", st.log(), 0, transitionSeq(101, model.StateScheduled, model.StatePublished)...)
	wantCount(t, "发布不该写版次表", st.log(), "video_version.", 0)
	wantEq(t, "发布", "版次行数不变", len(st.vers.rows), 1)
	if len(st.cache.dels) != 1 || st.cache.dels[0] != 101 {
		t.Errorf("应只失效 aid=101 的详情缓存，实得 %v", st.cache.dels)
	}
}

// TestTransitionStateAuditRowRecordsCallerSuppliedIdentity 钉审计的「原样入库」：
// operator/reason 由调用方自报，logic 既不校验也不加工——包括空串。
// 这是把现状 pin 成哨兵：AGENTS.md §8 要求删除/下架保留审计证据，
// 但当前证据链里的「谁」完全由调用方填空，见 README 已知缺口 5。
// 判别性：非空 operator 会照写入库（前一条链路用例已证），空串同样照写。
func TestTransitionStateAuditRowRecordsCallerSuppliedIdentity(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	before := time.Now().Unix()

	_, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
	wantNoErr(t, "operator/reason 都为空", err)

	rows := st.auditRows(101)
	if len(rows) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(rows))
	}
	wantEq(t, "审计", "aid", rows[0].Aid, int64(101))
	wantEq(t, "审计", "from_state", rows[0].FromState, model.StateDraft)
	wantEq(t, "审计", "to_state", rows[0].ToState, model.StateUploading)
	wantEq(t, "审计", "operator 原样（可为空串）", rows[0].Operator, "")
	wantEq(t, "审计", "reason 原样", rows[0].Reason, "")
	if rows[0].Ctime < before {
		t.Errorf("审计 ctime = %d, want >= %d", rows[0].Ctime, before)
	}
}

// TestUnauthenticatedPublishChainReachesPublishedWhileDeleteIsOwnerChecked 是 README
// 已知缺口 5 的可达性证明，也是本文件唯一一条「跨 RPC 对照」用例：
//   - 同一进程、同一 store，只调用 TransitionState 且 **一个 operator 都不填**，
//     就能把 aid=101 从 DRAFT 一路推到 PUBLISHED（8 步全合法，矩阵不拦空身份）；
//     到达 PUBLISHED 后再推一次 PUBLISHED→OFFLINE/DELETED 同样成功，
//     于是「陌生 aid 的稿件被下架/删除」也是这条无身份链路的终点；
//   - 对照：同一行上 DeleteSubmission 用别人的 mid 会被 ErrNotOwner 拒。
//     两侧必须同时成立，用例才有判别性——若哪天 TransitionState 也补上属主校验，
//     第一段就会失败；若删除侧的校验被移除，第二段就会失败。
//     只测其中一侧都退化成永真断言。
func TestUnauthenticatedPublishChainReachesPublishedWhileDeleteIsOwnerChecked(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 4242, model.StateDraft)

	// AGENTS.md §8 的主干链路，逐步推进时 operator 全为空串。
	for _, to := range []int32{
		model.StateUploading, model.StateUploaded, model.StateScanning, model.StateTranscoding,
		model.StateReadyForReview, model.StateApproved, model.StateScheduled, model.StatePublished,
	} {
		_, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: stateToRPC(to)})
		wantNoErr(t, fmt.Sprintf("空身份推进到 %d", to), err)
	}
	wantEq(t, "无身份链路", "库里 state", st.sub(101).State, model.StatePublished)
	wantEq(t, "无身份链路", "mid 始终是稿件主人", st.sub(101).Mid, int64(4242))

	// 8 步审计全部留痕，且「谁做的」这一列全空。
	rows := st.auditRows(101)
	if len(rows) != 8 {
		t.Fatalf("审计行数 = %d, want 8", len(rows))
	}
	for i, r := range rows {
		wantEq(t, fmt.Sprintf("审计第 %d 条", i+1), "operator 为空串", r.Operator, "")
		wantEq(t, fmt.Sprintf("审计第 %d 条", i+1), "reason 为空串", r.Reason, "")
	}

	// 已到 PUBLISHED 的稿件仍可被同一空身份链路下架；DELETED 由下面的对照侧走。
	_, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_OFFLINE})
	wantNoErr(t, "空身份下架", err)
	wantEq(t, "空身份下架", "库里 state", st.sub(101).State, model.StateOffline)
	_, err = transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_DELETED})
	wantNoErr(t, "空身份删除", err)
	wantEq(t, "空身份删除", "库里 state", st.sub(101).State, model.StateDeleted)
	wantEq(t, "空身份链路", "审计累计", st.auditCount(), 10)

	// 对照侧：删除走自己的 mid 校验，陌生调用方一行都推不动。
	// 先复位到 PUBLISHED，保证两侧撞的是同一条 PUBLISHED→DELETED 边。
	st.setState(101, model.StatePublished)
	before := st.log().snapshot()
	got, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 1})
	wantErrIs(t, "非属主删除", err, model.ErrNotOwner)
	if got != nil {
		t.Errorf("非属主删除仍返回 reply %+v", got)
	}
	wantSeq(t, "非属主删除轨迹", st.log(), before, "video_submission.FindOne:101")
	wantEq(t, "非属主删除", "state 不变", st.sub(101).State, model.StatePublished)
	wantEq(t, "非属主删除", "审计不增加", st.auditCount(), 10)

	// 判别性另一半：同一 aid 换成属主 mid 就成功，证明上一条的拒绝来自 mid 比对
	// 而不是「稿件已 DELETED 的幂等短路」或缓存/状态矩阵。
	_, err = deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 4242})
	wantNoErr(t, "属主删除", err)
	wantEq(t, "属主删除", "库里 state", st.sub(101).State, model.StateDeleted)
	wantEq(t, "属主删除", "审计 operator 由 logic 拼出", st.auditRows(101)[10].Operator, "owner:4242")
}

// TestTransitionStateLostUpdateBeforeTxCheckIsDetected 是并发窗口的「内侧」：
// 别的入口在 logic 首读之后、事务内二次校验之前把稿件推走 → repository 比对比对
// fromState → ErrInvalidStateTransition，一行都不写。
func TestTransitionStateLostUpdateBeforeTxCheckIsDetected(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.before("db.TransactCtx", func() { st.setState(101, model.StateUploaded) })

	got, err := transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING, Operator: "worker-a",
	})
	wantErrIs(t, "校验前被插队", err, model.ErrInvalidStateTransition)
	if got != nil {
		t.Errorf("拒绝时仍返回 reply %+v", got)
	}
	wantSeq(t, "轨迹", st.log(), 0,
		"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101")
	wantEq(t, "插队", "state 保持插队后的值", st.sub(101).State, model.StateUploaded)
	wantEq(t, "插队", "审计行数", st.auditCount(), 0)
	wantCount(t, "插队", st.log(), "cache.", 0)
	st.s.checkHooks(t)
}

// TestTransitionStateLostUpdateIsNotDetected 钉住 fakes_test.go 头部声明的第二处
// 「替身与真库都改不掉」的并发缺陷：repository.TransitionState 的二次校验用
// r.subMd.FindOne（**不带 session**，repository.go:137），而 UPDATE 语句是
// `SET state=?, mtime=? WHERE aid=?`（submissionmodel.go:194-196）——没有
// `AND state = ?` 的 CAS 条件。于是在「二次校验之后、UPDATE 之前」这个窗口里
// 别人的写入会被无条件覆盖，而且整调用还报成功。
//
// 场景：稿件在 APPROVED；运营 A 提交 APPROVED→SCHEDULED；审核 B 在校验之后
// 刚落 REJECTED。结果库里是 SCHEDULED（即将被发布），审计里只有 A 那一条
// 「APPROVED→SCHEDULED」，B 的驳回痕迹彻底丢失。
// 判别性对照：同一改动挪到校验之前（TestTransitionStateLostUpdateBeforeTxCheckIsDetected）
// 就会被检出并拒绝。修法（UPDATE 带 state 条件、或校验读走 session）见 README 已知缺口 4。
func TestTransitionStateLostUpdateIsNotDetected(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateApproved)
	// 钩子挂在「事务内 UPDATE 之前」：此刻二次校验的 FindOne 已经过去了。
	st.before("submission.UpdateState", func() { st.setState(101, model.StateRejected) })

	got, err := transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_SCHEDULED, Operator: "ops-a", Reason: "排期",
	})
	wantNoErr(t, "丢失更新被判成功", err)
	wantSeq(t, "轨迹", st.log(), 0, transitionSeq(101, model.StateApproved, model.StateScheduled)...)

	stored := st.sub(101)
	wantEq(t, "丢失更新", "库里 state 覆盖掉了刚落的驳回", stored.State, model.StateScheduled)
	wantEq(t, "丢失更新", "应答 state", got.GetSubmission().GetState(), rpc.SubmissionState_STATE_SCHEDULED)

	rows := st.auditRows(101)
	if len(rows) != 1 {
		t.Fatalf("审计行数 = %d, want 1（审核 B 的写入走的是别的入口，本用例不该替它补行）", len(rows))
	}
	wantEq(t, "丢失更新", "审计 from 是过期的 APPROVED 而不是实际的 REJECTED", rows[0].FromState, model.StateApproved)
	wantEq(t, "丢失更新", "审计 to", rows[0].ToState, model.StateScheduled)
	wantCount(t, "丢失更新", st.log(), "video_submission.UpdateState", 1)
	st.s.checkHooks(t)
}

// TestTransitionStateTxStageFailuresLeaveKnownResidue 钉事务三段失败后的库内残留
// （fake 不回滚，见 fakes_test.go 纪律 5；真实 MySQL 会回滚，这里因此不断言「已回滚」）：
// 三段失败都必须原样报错，并且 logic 不再往下走（不失效缓存、不复读、应答为 nil）。
func TestTransitionStateTxStageFailuresLeaveKnownResidue(t *testing.T) {
	t.Run("开启事务就失败", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("db.TransactCtx")

		got, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
		wantErrIs(t, "事务故障", err, errBoom)
		if got != nil {
			t.Errorf("事务失败仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:101", "db.TransactCtx")
		wantEq(t, "事务故障", "state", st.sub(101).State, model.StateDraft)
		wantEq(t, "事务故障", "审计行数", st.auditCount(), 0)
		wantCount(t, "事务故障", st.log(), "cache.", 0)
	})

	t.Run("状态 UPDATE 失败", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("submission.UpdateState")

		_, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
		wantErrIs(t, "UPDATE 故障", err, errBoom)
		wantSeq(t, "轨迹", st.log(), 0,
			"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101",
			"video_submission.UpdateState:101/2")
		wantEq(t, "UPDATE 故障", "state", st.sub(101).State, model.StateDraft)
		wantEq(t, "UPDATE 故障", "mtime", st.sub(101).Mtime, staleTime)
		wantEq(t, "UPDATE 故障", "审计行数", st.auditCount(), 0)
		wantCount(t, "UPDATE 故障", st.log(), "cache.", 0)
	})

	t.Run("审计 INSERT 失败时状态 UPDATE 已经跑过", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("audit.Insert")

		got, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
		wantErrIs(t, "审计故障", err, errBoom)
		if got != nil {
			t.Errorf("审计失败仍返回 reply %+v（丢审计不能算推进成功）", got)
		}
		wantSeq(t, "轨迹", st.log(), 0,
			"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101",
			"video_submission.UpdateState:101/2", "video_audit_log.Insert:101:1->2")
		// 替身不回滚：state 已被改成 UPLOADING。这条断言的作用是证明语句顺序是
		// 「先 UPDATE 状态、后写审计」；生产上两者同事务，任一条失败都整体回滚。
		wantEq(t, "审计故障（替身语义，生产由同事务回滚）", "state", st.sub(101).State, model.StateUploading)
		wantEq(t, "审计故障", "审计行数", st.auditCount(), 0)
		wantCount(t, "审计故障后不该再动缓存", st.log(), "cache.", 0)
	})

	t.Run("事务内二次校验读失败", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		// 钩子挂在首读之前装不上（会连带打断 logic 首读），改挂在开事务之前：
		// 此时首读已经过去，只有事务内的 FindOne 会失败。
		st.before("db.TransactCtx", func() { st.s.fail("submission.FindOne", errBoom) })

		_, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
		wantErrIs(t, "校验读故障", err, errBoom)
		wantSeq(t, "轨迹", st.log(), 0,
			"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101")
		wantEq(t, "校验读故障", "state", st.sub(101).State, model.StateDraft)
		wantEq(t, "校验读故障", "审计行数", st.auditCount(), 0)
		st.s.checkHooks(t)
	})
}

// TestTransitionStateMissingRowAndFirstReadFailure 区分「稿件不存在」与「查不到」：
// 前者是 logic 首读拿到 (nil,nil) 后判定，后者是 DB 故障，绝不能被降级成 404。
func TestTransitionStateMissingRowAndFirstReadFailure(t *testing.T) {
	t.Run("稿件不存在", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)

		got, err := transitionCall(st, &rpc.TransitionReq{Aid: 999, Target: rpc.SubmissionState_STATE_UPLOADING})
		wantErrIs(t, "稿件不存在", err, model.ErrSubmissionNotFound)
		if got != nil {
			t.Errorf("稿件不存在仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:999")
		wantEq(t, "稿件不存在", "事务次数", st.txCount(), 0)
	})

	t.Run("首读故障不被降级成 not found", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("submission.FindOne")

		_, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
		wantErrIs(t, "首读故障", err, errBoom)
		if errors.Is(err, model.ErrSubmissionNotFound) {
			t.Errorf("DB 故障被降级成「稿件不存在」：%v", err)
		}
		wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:101")
	})

	t.Run("复读故障时推进已经生效", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		// 事务内的 FindOne 也必须成功，所以故障只能在缓存失效那一刻装上：
		// 失效之后 logic 还要复读，读就撞上 errBoom。
		st.before("cache.DelSubmission", func() { st.s.fail("submission.FindOne", errBoom) })

		got, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
		wantErrIs(t, "复读故障", err, errBoom)
		if got != nil {
			t.Errorf("复读故障仍返回 reply %+v", got)
		}
		wantEq(t, "复读故障", "库里 state 其实已推进", st.sub(101).State, model.StateUploading)
		wantEq(t, "复读故障", "审计已经落下", st.auditCount(), 1)
		wantSeq(t, "轨迹", st.log(), 0,
			"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101",
			"video_submission.UpdateState:101/2", "video_audit_log.Insert:101:1->2",
			"cache.DelSubmission:101", "video_submission.FindOne:101")
		st.s.checkHooks(t)
	})
}

// TestTransitionStateIgnoresCacheInvalidationFailure 钉 repository/logic 的取舍：
// `_ = InvalidateSubmissionCache(...)`（transitionstatelogic.go:57）——缓存失效失败
// 既不上报，也不回滚已经提交的状态推进；应答照常成功。
func TestTransitionStateIgnoresCacheInvalidationFailure(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.fail("cache.DelSubmission")

	got, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
	wantNoErr(t, "缓存失效失败", err)
	wantEq(t, "缓存失效失败", "库里 state", st.sub(101).State, model.StateUploading)
	wantEq(t, "缓存失效失败", "应答 state", got.GetSubmission().GetState(), rpc.SubmissionState_STATE_UPLOADING)
	wantSeq(t, "轨迹", st.log(), 0, transitionSeq(101, model.StateDraft, model.StateUploading)...)
	if len(st.cache.dels) != 0 {
		t.Errorf("失效失败的 key 不该被记为已失效，实得 %v", st.cache.dels)
	}
}

// TestTransitionStateIgnoresCallerIdentity 把现状钉成哨兵：TransitionReq 里
// **没有** mid/权限字段（rpc/video.proto:112-118），logic 也不做任何调用方校验
// （transitionstatelogic.go:31-62），所以任何能触达该 RPC 的进程都能以任意 operator
// 推进任何人的稿件——与同服务 Update/Delete 都有属主校验形成对照。
// 边界两侧：入参里塞什么都照走（本用例），以及合法矩阵内一定成功
// （TestTransitionStateAcceptsExactlyTheDeclaredMatrix 已逐对验证）。
// gateway/app 目前把这条 RPC 挂成了终端路由，详见 README 已知缺口 5。
func TestTransitionStateIgnoresCallerIdentity(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 4242, model.StatePublished)

	got, err := transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_DELETED, Operator: "whoever", Reason: "没有归属校验",
	})
	wantNoErr(t, "无归属校验的下架", err)
	wantEq(t, "无归属校验", "库里 state", st.sub(101).State, model.StateDeleted)
	wantEq(t, "无归属校验", "应答 mid", got.GetSubmission().GetMid(), int64(4242))
	wantSeq(t, "轨迹", st.log(), 0, transitionSeq(101, model.StatePublished, model.StateDeleted)...)
	wantEq(t, "无归属校验", "审计 operator 由调用方自报", st.auditRows(101)[0].Operator, "whoever")
}
