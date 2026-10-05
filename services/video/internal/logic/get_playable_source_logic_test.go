package logic

// get_playable_source_logic_test.go 钉 GetPlayableSource 的真实口径：
//   - 只有 state == PUBLISHED 的稿件才会去查 video_version，
//     SCHEDULED（定时发布未到时）与其它任何状态都在首读之后直接拒绝（§8）；
//   - 全程只读：一个事务、一条写 SQL、一次缓存失效都不许出现；
//   - 可播放版次 = 「ORDER BY version DESC 之后第一个 asset_id 非空的版次」
//     （repository.go:175-186），所以最新几个版次还没登记媒资时会回退到旧版次；
//   - 「没有可播放版次」在仓储层是 (nil, nil)，由 logic 判成 ErrSubmissionNotPlayable，
//     而不是返回一个 version 为 nil 的成功应答；
//   - 稿件详情缓存**从不被读取**（当前全仓也没有任何写缓存的代码），每次播放解析都回源。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

func playableCall(st *store, in *rpc.PlayableSourceReq) (*rpc.PlayableSourceReply, error) {
	return NewGetPlayableSourceLogic(context.Background(), st.svcCtx).GetPlayableSource(in)
}

// publishedWithAssets 布一只已发布稿件 + 若干版次（静默路径，不写 callLog）。
func publishedWithAssets(st *store, versions ...*model.VideoVersion) {
	st.seedDraft(101, 7, model.StatePublished)
	for _, v := range versions {
		st.seedVersion(v)
	}
}

func snapshotSubs(st *store) []model.VideoSubmission {
	out := make([]model.VideoSubmission, 0, len(st.subs.rows))
	for _, r := range st.subs.rows {
		out = append(out, *r)
	}
	return out
}

func snapshotVers(st *store) []model.VideoVersion {
	out := make([]model.VideoVersion, 0, len(st.vers.rows))
	for _, r := range st.vers.rows {
		out = append(out, *r)
	}
	return out
}

// TestGetPlayableSourceRejectsInvalidAidBeforeAnyDependency 锁入参守卫先于触库。
func TestGetPlayableSourceRejectsInvalidAidBeforeAnyDependency(t *testing.T) {
	for _, aid := range []int64{0, -1, -101} {
		st := newStore()
		publishedWithAssets(st, &model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", Ctime: 100})
		from := st.log().snapshot()

		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: aid, Mid: 7})
		wantErrIs(t, "aid 非法", err, model.ErrInvalidAid)
		if got != nil {
			t.Errorf("aid=%d 拒绝时仍返回 reply %+v", aid, got)
		}
		wantNoCallAfter(t, "aid 非法", st.log(), from)
	}
}

// TestGetPlayableSourceMissingRowIsNotFound 锁「稿件不存在」不被混进「不可播放」：
// 前者是数据缺失，后者是合法状态结论，网关据此给客户端不同文案。
func TestGetPlayableSourceMissingRowIsNotFound(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StatePublished)

	got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 999})
	wantErrIs(t, "稿件不存在", err, model.ErrSubmissionNotFound)
	if errors.Is(err, model.ErrSubmissionNotPlayable) {
		t.Errorf("「不存在」被混进「不可播放」：%v", err)
	}
	if got != nil {
		t.Errorf("稿件不存在仍返回 reply %+v", got)
	}
	wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:999")
	wantCount(t, "稿件不存在", st.log(), "video_version.", 0)
}

// TestGetPlayableSourceRefusesToTouchVersionTableBeforePublished 钉 §8 的读侧闸门：
// 除 PUBLISHED 外的每个状态都必须「只读稿件、不查版次」，
// 其中 SCHEDULED（定时发布）是最要紧的一条——媒资早就登记好了，
// 只要不查版次表就放不了流。判别性：PUBLISHED 时同样入参一定会去查版次。
func TestGetPlayableSourceRefusesToTouchVersionTableBeforePublished(t *testing.T) {
	for _, state := range allSubmissionStates {
		if state == model.StatePublished {
			continue // 对照见下面的 t.Run
		}
		st := newStore()
		st.seedDraft(101, 7, state)
		st.seedVersion(&model.VideoVersion{Aid: 101, Version: 3, AssetID: "a-3", Ctime: 100})

		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantErrIs(t, "未发布拒答", err, model.ErrSubmissionNotPlayable)
		if got != nil {
			t.Errorf("state=%d 拒绝时仍返回 reply %+v", state, got)
		}
		wantSeq(t, "未发布轨迹", st.log(), 0, "video_submission.FindOne:101")
		wantCount(t, "未发布不许碰版次表", st.log(), "video_version.", 0)
	}

	t.Run("对照：PUBLISHED 才会去查版次表", func(t *testing.T) {
		st := newStore()
		publishedWithAssets(st, &model.VideoVersion{Aid: 101, Version: 3, AssetID: "a-3", Ctime: 100})

		_, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantNoErr(t, "已发布", err)
		wantSeq(t, "已发布轨迹", st.log(), 0,
			"video_submission.FindOne:101", "video_version.ListByAid:101")
	})
}

// TestGetPlayableSourceReturnsLatestVersionWithAsset 钉成功路径的投影与「取最新」：
// 版次 1/2 都带媒资时返回 version=2（ORDER BY version DESC 的第一行），
// 五个字段逐一对齐库里的行（字段绑错列、拿首读稿件字段冒充版次字段都会红）。
func TestGetPlayableSourceReturnsLatestVersionWithAsset(t *testing.T) {
	st := newStore()
	publishedWithAssets(st,
		&model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", State: model.StatePublished, Ctime: 111},
		&model.VideoVersion{Aid: 101, Version: 2, AssetID: "a-2", State: model.StatePublished, Ctime: 222},
	)

	got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7, Ip: "10.0.0.7"})
	wantNoErr(t, "取播放来源", err)
	wantEq(t, "投影", "aid", got.GetAid(), int64(101))
	wantEq(t, "投影", "submission_state", got.GetSubmissionState(), rpc.SubmissionState_STATE_PUBLISHED)

	v := got.GetVersion()
	if v == nil {
		t.Fatal("version = nil，want 最新可播放版次")
	}
	wantEq(t, "版次投影", "aid", v.GetAid(), int64(101))
	wantEq(t, "版次投影", "version", v.GetVersion(), int64(2))
	wantEq(t, "版次投影", "asset_id", v.GetAssetId(), "a-2")
	wantEq(t, "版次投影", "state", v.GetState(), rpc.SubmissionState_STATE_PUBLISHED)
	wantEq(t, "版次投影", "ctime", v.GetCtime(), int64(222))
	wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:101", "video_version.ListByAid:101")
}

// TestGetPlayableSourceFallsBackToNewestVersionThatHasAsset 钉「回退」口径：
// 重新转码时新版本还没登记 asset_id，播放必须继续用旧版次而不是失败。
// 判别性：如果实现是「只看最大 version」，这里会拿到 version=3 的空 asset_id 或报不可播放。
func TestGetPlayableSourceFallsBackToNewestVersionThatHasAsset(t *testing.T) {
	st := newStore()
	publishedWithAssets(st,
		&model.VideoVersion{Aid: 101, Version: 1, AssetID: "", Ctime: 100},
		&model.VideoVersion{Aid: 101, Version: 2, AssetID: "a-2", Ctime: 200},
		&model.VideoVersion{Aid: 101, Version: 3, AssetID: "", Ctime: 300},
		&model.VideoVersion{Aid: 101, Version: 4, AssetID: "", Ctime: 400},
	)

	got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101})
	wantNoErr(t, "回退旧版次", err)
	wantEq(t, "回退旧版次", "version", got.GetVersion().GetVersion(), int64(2))
	wantEq(t, "回退旧版次", "asset_id", got.GetVersion().GetAssetId(), "a-2")
	wantEq(t, "回退旧版次", "ctime", got.GetVersion().GetCtime(), int64(200))
	// 版次表只查一次（回退是内存里挑，不是逐版次回查）。
	wantCount(t, "回退", st.log(), "video_version.ListByAid", 1)
}

// TestGetPlayableSourceWithoutAnyAssetIsNotPlayable 钉仓储的 (nil, nil) 口径
// 被 logic 判成 ErrSubmissionNotPlayable：两种情形是「一条版次都没有」和
// 「版次都在但都没媒资」。两侧都不许返回 version 为 nil 的成功应答——
// 那会让网关拿着空 asset_id 去签发播放地址。
func TestGetPlayableSourceWithoutAnyAssetIsNotPlayable(t *testing.T) {
	t.Run("没有任何版次行", func(t *testing.T) {
		st := newStore()
		publishedWithAssets(st)

		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantErrIs(t, "无版次", err, model.ErrSubmissionNotPlayable)
		if got != nil {
			t.Errorf("无版次仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:101", "video_version.ListByAid:101")
	})

	t.Run("版次都还没登记媒资", func(t *testing.T) {
		st := newStore()
		publishedWithAssets(st,
			&model.VideoVersion{Aid: 101, Version: 1, AssetID: "", State: model.StateTranscoding, Ctime: 100},
			&model.VideoVersion{Aid: 101, Version: 2, AssetID: "", State: model.StateTranscoding, Ctime: 100},
		)

		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantErrIs(t, "媒资未登记", err, model.ErrSubmissionNotPlayable)
		if got != nil {
			t.Errorf("媒资未登记仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:101", "video_version.ListByAid:101")
	})

	t.Run("对照：只要有一行带 asset_id 就成功", func(t *testing.T) {
		st := newStore()
		publishedWithAssets(st,
			&model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", Ctime: 100},
			&model.VideoVersion{Aid: 101, Version: 2, AssetID: "", Ctime: 100},
		)

		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantNoErr(t, "对照", err)
		wantEq(t, "对照", "version", got.GetVersion().GetVersion(), int64(1))
	})
}

// TestGetPlayableSourceDoesNotJudgeVersionState 把「版次 state 不参与播放判定」钉成观察哨：
// LatestPlayableVersion 只按 asset_id 非空挑选，state 原样投影给网关却不设门槛
// （repository.go:180-184、migration 000002 的 video_version.state 列）。
// 现状无受害者：全仓没有任何代码写入 video_version.state（只有 model 的 INSERT 带这列，
// 本期 8 个方法都不写版次表）。一旦开始写状态，这条就是「被废弃的版次仍可播放」的成因。
func TestGetPlayableSourceDoesNotJudgeVersionState(t *testing.T) {
	st := newStore()
	publishedWithAssets(st,
		&model.VideoVersion{Aid: 101, Version: 9, AssetID: "a-废弃", State: model.StateDeleted, Ctime: 900},
	)

	got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
	wantNoErr(t, "版次状态不设门槛", err)
	wantEq(t, "版次状态不设门槛", "version", got.GetVersion().GetVersion(), int64(9))
	wantEq(t, "版次状态不设门槛", "asset_id", got.GetVersion().GetAssetId(), "a-废弃")
	wantEq(t, "版次状态不设门槛", "state 原样投影", got.GetVersion().GetState(), rpc.SubmissionState_STATE_DELETED)
}

// TestGetPlayableSourceIsStrictlyReadOnly 钉「只读旁路」：整条调用前后
// 三张表的每一行逐字段不变（reflect.DeepEqual 比快照），事务 0 次、
// 审计 0 行、缓存 0 次访问，依赖序列恰好两条 SELECT。
// 任何「顺手把 state 写一下」的实现都会在这里红。
func TestGetPlayableSourceIsStrictlyReadOnly(t *testing.T) {
	st := newStore()
	publishedWithAssets(st,
		&model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", State: model.StatePublished, Ctime: 100},
		&model.VideoVersion{Aid: 102, Version: 7, AssetID: "a-7", State: model.StatePublished, Ctime: 100},
	)
	st.seedDraft(102, 8, model.StateDraft) // 别人的稿件，确保不被顺手改
	subsBefore := snapshotSubs(st)
	versBefore := snapshotVers(st)

	for i := 0; i < 3; i++ {
		_, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantNoErr(t, "只读调用", err)
	}

	if !reflect.DeepEqual(snapshotSubs(st), subsBefore) {
		t.Errorf("video_submission 被改动：before %+v after %+v", subsBefore, snapshotSubs(st))
	}
	if !reflect.DeepEqual(snapshotVers(st), versBefore) {
		t.Errorf("video_version 被改动：before %+v after %+v", versBefore, snapshotVers(st))
	}
	wantEq(t, "只读", "事务次数", st.txCount(), 0)
	wantEq(t, "只读", "审计行数", st.auditCount(), 0)
	wantCount(t, "只读", st.log(), "video_submission.UpdateFields", 0)
	wantCount(t, "只读", st.log(), "video_submission.UpdateState", 0)
	wantCount(t, "只读", st.log(), "video_submission.Insert", 0)
	wantCount(t, "只读", st.log(), "video_version.Insert", 0)
	wantCount(t, "只读", st.log(), "video_audit_log.Insert", 0)
	wantCount(t, "只读", st.log(), "cache.", 0)
	wantSeq(t, "只读轨迹", st.log(), 0,
		"video_submission.FindOne:101", "video_version.ListByAid:101",
		"video_submission.FindOne:101", "video_version.ListByAid:101",
		"video_submission.FindOne:101", "video_version.ListByAid:101")
}

// TestGetPlayableSourceAlwaysGoesToDatabase 钉「缓存缺失时怎么办」的真实现状：
// 缓存里永远没有稿件详情（全仓只有 DelSubmission，没有任何写入 video:sub:* 的代码），
// 所以每次播放解析都是「稿件 SELECT + 版次 SELECT」两次回源，而不是先 miss 再回填。
// README 的「稿件详情走 Redis 短 TTL 缓存」因此只在失效侧成立，见 README 已知缺口 1。
func TestGetPlayableSourceAlwaysGoesToDatabase(t *testing.T) {
	st := newStore()
	publishedWithAssets(st, &model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", Ctime: 100})

	for i := 0; i < 5; i++ {
		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantNoErr(t, "每次都回源的播放解析", err)
		wantEq(t, "每次都回源的播放解析", "asset_id", got.GetVersion().GetAssetId(), "a-1")
	}
	wantCount(t, "播放解析的回源", st.log(), "video_submission.FindOne", 5)
	wantCount(t, "播放解析的回源", st.log(), "video_version.ListByAid", 5)
	wantCount(t, "播放解析从不读缓存", st.log(), "cache.", 0)
	if len(st.cache.dels) != 0 {
		t.Errorf("只读接口不该失效缓存，实得 %v", st.cache.dels)
	}
}

// TestGetPlayableSourcePropagatesDependencyErrors 钉两段故障都不被降级成「不可播放」：
// 播放链路把 DB 故障说成「这只稿件不能看」会让客户端永久放弃重试。
func TestGetPlayableSourcePropagatesDependencyErrors(t *testing.T) {
	t.Run("稿件读故障", func(t *testing.T) {
		st := newStore()
		publishedWithAssets(st, &model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", Ctime: 100})
		st.fail("submission.FindOne")

		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantErrIs(t, "稿件读故障", err, errBoom)
		if errors.Is(err, model.ErrSubmissionNotPlayable) || errors.Is(err, model.ErrSubmissionNotFound) {
			t.Errorf("故障被降级成业务结论：%v", err)
		}
		if got != nil {
			t.Errorf("故障仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:101")
		wantCount(t, "稿件读故障后不该查版次", st.log(), "video_version.", 0)
	})

	t.Run("版次读故障", func(t *testing.T) {
		st := newStore()
		publishedWithAssets(st, &model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", Ctime: 100})
		st.fail("version.ListByAid")

		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: 7})
		wantErrIs(t, "版次读故障", err, errBoom)
		if errors.Is(err, model.ErrSubmissionNotPlayable) {
			t.Errorf("版次查询故障被降级成「不可播放」：%v", err)
		}
		if got != nil {
			t.Errorf("故障仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log(), 0,
			"video_submission.FindOne:101", "video_version.ListByAid:101")
	})
}

// TestGetPlayableSourceIgnoresRequesterMid 把现状钉成哨兵：PlayableSourceReq.Mid
// 只进日志（getplayablesourcelogic.go:43-44 的 Infof），不参与任何判定——
// 游客（mid=0）与陌生 mid 都能解析出已发布稿件的媒资引用。
// 这与 proto 注释「仅日志与排障，不做起点鉴权」一致，因此是有意的公开播放面；
// 但版权内容（§1：普通用户不得发布整片）如果也走这条链路，就需要在网关侧另加闸门。
// 判别性：稿件未发布时同样不看的 mid 会被状态闸门拦住（前一用例已证），
// 也就是说拒绝原因只有 state，没有身份。
func TestGetPlayableSourceIgnoresRequesterMid(t *testing.T) {
	for _, mid := range []int64{0, 7, 8, 999999} {
		st := newStore()
		publishedWithAssets(st, &model.VideoVersion{Aid: 101, Version: 1, AssetID: "a-1", Ctime: 100})

		got, err := playableCall(st, &rpc.PlayableSourceReq{Aid: 101, Mid: mid})
		wantNoErr(t, "请求者身份不参与判定", err)
		wantEq(t, "请求者身份不参与判定", "asset_id", got.GetVersion().GetAssetId(), "a-1")
		wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:101", "video_version.ListByAid:101")
	}
}
