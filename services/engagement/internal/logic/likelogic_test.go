package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

const (
	likeBiz     = "archive"
	likeOrigin  = int64(1)
	likeMessage = int64(101)
	likeMid     = int64(7)
	likeUpMid   = int64(88)
)

// likeOps 是「一次真正改变状态的点赞」的完整调用序列（顺序即结论）。
// Repository.Like 的口径：先查旧状态 → 写关系行 → 增计数 → 影子计数器 → 回读真值。
func likeOps(ld, dd int64) []string {
	return []string{
		"like.FindStates:archive:7:101",
		"like.Upsert:archive:7:101",
		"stat.Incr:archive:1:101:" + itoa(ld) + "/" + itoa(dd),
		"cache.IncrLike:lc:archive:1:101:" + itoa(ld),
		"cache.IncrDislike:dc:archive:1:101:" + itoa(dd),
		"stat.FindOne:archive:1:101",
	}
}

// likeReadOps 是「状态没变，幂等短路」的调用序列：只查状态和计数，一次写都没有。
func likeReadOps() []string {
	return []string{"like.FindStates:archive:7:101", "stat.FindOne:archive:1:101"}
}

// TestLikeRejectsGuardsBeforeTouchingDeps business → mid → message_id，三条都在触库之前。
// origin_id 不参与守卫（UGC 场景可以是 0），所以这里不设它的用例。
func TestLikeRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.LikeReq
		want error
	}{
		{"business 空", &rpc.LikeReq{Business: "", Mid: likeMid, MessageId: likeMessage, Action: rpc.Action_ACTION_LIKE}, model.ErrInvalidBusiness},
		{"mid 为 0", &rpc.LikeReq{Business: likeBiz, Mid: 0, MessageId: likeMessage, Action: rpc.Action_ACTION_LIKE}, model.ErrInvalidMid},
		{"mid 为负", &rpc.LikeReq{Business: likeBiz, Mid: -7, MessageId: likeMessage, Action: rpc.Action_ACTION_LIKE}, model.ErrInvalidMid},
		{"message_id 为 0", &rpc.LikeReq{Business: likeBiz, Mid: likeMid, MessageId: 0, Action: rpc.Action_ACTION_LIKE}, model.ErrInvalidMessage},
		{"message_id 为负", &rpc.LikeReq{Business: likeBiz, Mid: likeMid, MessageId: -101, Action: rpc.Action_ACTION_LIKE}, model.ErrInvalidMessage},
		{"business 优先于 mid", &rpc.LikeReq{Business: "", Mid: 0}, model.ErrInvalidBusiness},
		{"mid 优先于 message_id", &rpc.LikeReq{Business: likeBiz, Mid: 0, MessageId: 0}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedStat(st, likeBiz, likeOrigin, likeMessage, 9, 1, 0, 0)
			before := st.log.snapshot()
			got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestActionToStateCoversUnknownAction rpc.Action → state 的映射表。
// 未知枚举值（例如新客户端传了 ACTION_CANCEL_SHARE）必须落到 0=取消，
// 不能 panic，也不能被当成点赞加分。
func TestActionToStateCoversUnknownAction(t *testing.T) {
	cases := []struct {
		action rpc.Action
		want   int32
	}{
		{rpc.Action_ACTION_UNSPECIFIED, model.LikeStateCancel},
		{rpc.Action_ACTION_LIKE, model.LikeStateLike},
		{rpc.Action_ACTION_CANCEL_LIKE, model.LikeStateCancel},
		{rpc.Action_ACTION_DISLIKE, model.LikeStateDislike},
		{rpc.Action_ACTION_CANCEL_DISLIKE, model.LikeStateCancel},
		{rpc.Action(99), model.LikeStateCancel},
		{rpc.Action(-1), model.LikeStateCancel},
	}
	for _, tc := range cases {
		wantEQ(t, "actionToState("+tc.action.String()+")", "state", actionToState(tc.action), tc.want)
	}
}

// TestLikeFirstLikeWritesRelationAndCount 正常路径逐字段投影：
// 关系行、计数行、影子计数器、响应四者都要对上。
func TestLikeFirstLikeWritesRelationAndCount(t *testing.T) {
	st := newStore()

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, UpMid: likeUpMid, OriginId: likeOrigin,
		MessageId: likeMessage, Action: rpc.Action_ACTION_LIKE,
	})
	wantNoErr(t, "首次点赞", err)
	wantOps(t, "首次点赞", st.log.opsFrom(0), likeOps(1, 0))

	wantEQ(t, "首次点赞", "OriginId 回显", got.OriginId, likeOrigin)
	wantEQ(t, "首次点赞", "MessageId 回显", got.MessageId, likeMessage)
	wantEQ(t, "首次点赞", "LikeNumber", got.LikeNumber, int64(1))
	wantEQ(t, "首次点赞", "DislikeNumber", got.DislikeNumber, int64(0))

	like := st.like.get(likeBiz, likeMid, likeMessage)
	if like == nil {
		t.Fatalf("thumbup_like 未落库")
	}
	wantEQ(t, "关系行", "Business", like.Business, likeBiz)
	wantEQ(t, "关系行", "Mid", like.Mid, likeMid)
	wantEQ(t, "关系行", "UpMid", like.UpMid, likeUpMid)
	wantEQ(t, "关系行", "OriginID", like.OriginID, likeOrigin)
	wantEQ(t, "关系行", "MessageID", like.MessageID, likeMessage)
	wantEQ(t, "关系行", "State", like.State, int32(model.LikeStateLike))
	wantEQ(t, "关系行", "主键自增分配", like.ID, int64(1))

	stat := st.stat.get(likeBiz, likeOrigin, likeMessage)
	if stat == nil {
		t.Fatalf("thumbup_stat 未落库")
	}
	wantEQ(t, "计数行", "LikeNumber", stat.LikeNumber, int64(1))
	wantEQ(t, "计数行", "DislikeNumber", stat.DislikeNumber, int64(0))
	wantEQ(t, "计数行", "LikeChange（点赞不碰运营修正位）", stat.LikeChange, int64(0))
	wantEQ(t, "计数行", "DislikeChange", stat.DislikeChange, int64(0))
	isRecentUnix(t, "计数行", "Ctime", stat.Ctime)

	wantEQ(t, "影子计数器", "Redis 点赞计数", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(1))
	wantEQ(t, "影子计数器", "Redis 点踩计数", st.cache.dislikeCounter(likeBiz, likeOrigin, likeMessage), int64(0))
}

// TestLikeLeavesCtimeZero 缺陷 #2 的现象固化：
// logic 构造 ThumbupLike 时不给 Ctime/Mtime，model 的 INSERT 又是逐字取入参，
// 所以 thumbup_like.ctime 恒为 0。后果：UserLikes 的 ORDER BY ctime DESC 完全失效、
// HasLike 返回的 time 恒为 0。修复（logic 补 now）后本用例会红，届时改成 isRecentUnix。
func TestLikeLeavesCtimeZero(t *testing.T) {
	st := newStore()
	_, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_LIKE,
	})
	wantNoErr(t, "点赞", err)

	like := st.like.get(likeBiz, likeMid, likeMessage)
	wantEQ(t, "缺陷 #2", "thumbup_like.Ctime", like.Ctime, int64(0))
	wantEQ(t, "缺陷 #2", "thumbup_like.Mtime", like.Mtime, int64(0))

	// 同一次操作里计数行的 ctime 却是有值的（model.Incr 自己取 now）——
	// 两表行为不一致，说明这不是刻意的业务约定而是漏赋值。
	stat := st.stat.get(likeBiz, likeOrigin, likeMessage)
	if stat.Ctime == 0 {
		t.Errorf("thumbup_stat.Ctime = 0, want 非零（Incr 的 SQL 自己取 now）")
	}
}

// TestLikeRepeatedLikeIsIdempotent 本服务最要紧的不变量：
// 同一用户对同一内容重复点赞，第二次起**不再写关系行、不再加计数**，
// 只回读当前计数（对着 uniq_business_mid_message 的唯一键断言）。
func TestLikeRepeatedLikeIsIdempotent(t *testing.T) {
	st := newStore()
	l := NewLikeLogic(context.Background(), newTestSvc(st))
	in := &rpc.LikeReq{Business: likeBiz, Mid: likeMid, UpMid: likeUpMid, OriginId: likeOrigin,
		MessageId: likeMessage, Action: rpc.Action_ACTION_LIKE}

	first, err := l.Like(in)
	wantNoErr(t, "第一次点赞", err)
	second, err := l.Like(in)
	wantNoErr(t, "第二次点赞", err)
	third, err := l.Like(in)
	wantNoErr(t, "第三次点赞", err)

	wantOps(t, "重复点赞", st.log.opsFrom(0), append(append(likeOps(1, 0), likeReadOps()...), likeReadOps()...))
	wantEQ(t, "重复点赞", "首次 LikeNumber", first.LikeNumber, int64(1))
	wantEQ(t, "重复点赞", "第二次不叠加", second.LikeNumber, int64(1))
	wantEQ(t, "重复点赞", "第三次不叠加", third.LikeNumber, int64(1))
	wantEQ(t, "重复点赞", "关系行数（唯一键只有一行）", len(st.like.rows), 1)
	wantEQ(t, "重复点赞", "State 仍是已点赞", st.like.get(likeBiz, likeMid, likeMessage).State, int32(model.LikeStateLike))
	wantEQ(t, "重复点赞", "计数行 LikeNumber", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeNumber, int64(1))
	wantCount(t, "重复点赞", st.log, "like.Upsert:", 1)
	wantCount(t, "重复点赞", st.log, "stat.Incr:", 1)
	wantEQ(t, "重复点赞", "影子计数器只加过一次", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(1))
}

// TestLikeCancelAfterLikeThenLikeAgainNetsOne 点赞 → 取消 → 再点赞的净值必须是 1。
// 三个分支的 delta 方向（+1 / -1 / +1）和每步的调用序列都逐步核对。
func TestLikeCancelAfterLikeThenLikeAgainNetsOne(t *testing.T) {
	st := newStore()
	l := NewLikeLogic(context.Background(), newTestSvc(st))
	base := &rpc.LikeReq{Business: likeBiz, Mid: likeMid, UpMid: likeUpMid, OriginId: likeOrigin, MessageId: likeMessage}

	a, err := l.Like(&rpc.LikeReq{Business: base.Business, Mid: base.Mid, UpMid: base.UpMid,
		OriginId: base.OriginId, MessageId: base.MessageId, Action: rpc.Action_ACTION_LIKE})
	wantNoErr(t, "点赞", err)
	b, err := l.Like(&rpc.LikeReq{Business: base.Business, Mid: base.Mid, UpMid: base.UpMid,
		OriginId: base.OriginId, MessageId: base.MessageId, Action: rpc.Action_ACTION_CANCEL_LIKE})
	wantNoErr(t, "取消", err)
	c, err := l.Like(&rpc.LikeReq{Business: base.Business, Mid: base.Mid, UpMid: base.UpMid,
		OriginId: base.OriginId, MessageId: base.MessageId, Action: rpc.Action_ACTION_LIKE})
	wantNoErr(t, "再点赞", err)

	wantOps(t, "点赞/取消/再点赞", st.log.opsFrom(0),
		append(append(likeOps(1, 0), likeOps(-1, 0)...), likeOps(1, 0)...))
	wantEQ(t, "净值", "点赞后", a.LikeNumber, int64(1))
	wantEQ(t, "净值", "取消后", b.LikeNumber, int64(0))
	wantEQ(t, "净值", "再点赞后", c.LikeNumber, int64(1))
	wantEQ(t, "净值", "关系行数（取消是软改状态，不删行）", len(st.like.rows), 1)
	wantEQ(t, "净值", "最终 State", st.like.get(likeBiz, likeMid, likeMessage).State, int32(model.LikeStateLike))
	wantEQ(t, "净值", "最终计数", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeNumber, int64(1))
	wantEQ(t, "净值", "影子计数器净值", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(1))
}

// TestLikeCancelWithoutAnyRecordIsSilentNoop 取消一个从没点过的对象：
// 库里没有行 → 旧状态按 0 处理 → 与新状态相同 → 一次写都不发生，且不报错。
// 计数行也不存在时返回 (0,0)，这是「查无计数」而不是失败。
func TestLikeCancelWithoutAnyRecordIsSilentNoop(t *testing.T) {
	st := newStore()

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_CANCEL_LIKE,
	})
	wantNoErr(t, "取消不存在的点赞", err)
	wantEQ(t, "取消不存在的点赞", "OriginId 回显", got.OriginId, likeOrigin)
	wantEQ(t, "取消不存在的点赞", "MessageId 回显", got.MessageId, likeMessage)
	wantEQ(t, "取消不存在的点赞", "LikeNumber", got.LikeNumber, int64(0))
	wantEQ(t, "取消不存在的点赞", "DislikeNumber", got.DislikeNumber, int64(0))
	wantOps(t, "取消不存在的点赞", st.log.opsFrom(0), likeReadOps())
	wantEQ(t, "取消不存在的点赞", "没有插入关系行", len(st.like.rows), 0)
	wantEQ(t, "取消不存在的点赞", "没有插入计数行", len(st.stat.rows), 0)
	wantCount(t, "取消不存在的点赞", st.log, "like.Upsert:", 0)
	wantCount(t, "取消不存在的点赞", st.log, "stat.Incr:", 0)
	wantCount(t, "取消不存在的点赞", st.log, "cache.", 0)
}

// TestLikeNoopReadReturnsExistingCounts 幂等短路分支的回读值必须来自计数行本身：
// 别的用户已经把点赞数点到 9，本次「重复取消」要把这个真值带回去，而不是 0。
func TestLikeNoopReadReturnsExistingCounts(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 9, 3, 1, 0)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_CANCEL_LIKE,
	})
	wantNoErr(t, "重复取消", err)
	wantEQ(t, "重复取消", "LikeNumber 取计数行真值", got.LikeNumber, int64(9))
	wantEQ(t, "重复取消", "DislikeNumber", got.DislikeNumber, int64(3))
	wantOps(t, "重复取消", st.log.opsFrom(0), likeReadOps())
	wantEQ(t, "重复取消", "计数行未被改动", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeNumber, int64(9))
	wantEQ(t, "重复取消", "运营修正位未被改动", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeChange, int64(1))
}

// TestLikeSwitchFromLikeToDislikeMovesTheSingleVote 点赞→点踩：同一用户只有一张票，
// 所以 like-1 且 dislike+1（不是各自独立累加）。
func TestLikeSwitchFromLikeToDislikeMovesTheSingleVote(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, UpMid: likeUpMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_DISLIKE,
	})
	wantNoErr(t, "点赞改点踩", err)
	wantOps(t, "点赞改点踩", st.log.opsFrom(0), likeOps(-1, 1))
	wantEQ(t, "点赞改点踩", "LikeNumber", got.LikeNumber, int64(4))
	wantEQ(t, "点赞改点踩", "DislikeNumber", got.DislikeNumber, int64(3))
	wantEQ(t, "点赞改点踩", "State", st.like.get(likeBiz, likeMid, likeMessage).State, int32(model.LikeStateDislike))
	wantEQ(t, "点赞改点踩", "一张票不会凭空变两张",
		st.stat.get(likeBiz, likeOrigin, likeMessage).LikeNumber+st.stat.get(likeBiz, likeOrigin, likeMessage).DislikeNumber, int64(7))
}

// TestLikeCancelDislikeOnlyDeductsDislike 取消点踩只扣点踩位。
func TestLikeCancelDislikeOnlyDeductsDislike(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateDislike, 1_700_000_500)
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_CANCEL_DISLIKE,
	})
	wantNoErr(t, "取消点踩", err)
	wantOps(t, "取消点踩", st.log.opsFrom(0), likeOps(0, -1))
	wantEQ(t, "取消点踩", "LikeNumber 不变", got.LikeNumber, int64(5))
	wantEQ(t, "取消点踩", "DislikeNumber 减一", got.DislikeNumber, int64(1))
	wantEQ(t, "取消点踩", "State", st.like.get(likeBiz, likeMid, likeMessage).State, int32(model.LikeStateCancel))
}

// TestLikeTrustsStoredStateNotClientIntent 状态机以库内 state 为准：
// 用户库里是「已点赞」，却发了 ACTION_CANCEL_DISLIKE（映射到 state=0），
// 结果应当扣掉那张点赞票，而不是把不存在的点踩票减一（那会把 dislike 扣成负数）。
func TestLikeTrustsStoredStateNotClientIntent(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_CANCEL_DISLIKE,
	})
	wantNoErr(t, "错报动作", err)
	wantOps(t, "错报动作", st.log.opsFrom(0), likeOps(-1, 0))
	wantEQ(t, "错报动作", "LikeNumber", got.LikeNumber, int64(4))
	wantEQ(t, "错报动作", "DislikeNumber 未被扣成负", got.DislikeNumber, int64(2))
}

// TestLikeUnknownActionBehavesAsCancel 未知动作 → state 0 → 等价于取消。
// 这条锁的是「新客户端的未知枚举不会给内容加分」，属于计数的防御边界。
func TestLikeUnknownActionBehavesAsCancel(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action(77),
	})
	wantNoErr(t, "未知动作", err)
	wantOps(t, "未知动作", st.log.opsFrom(0), likeOps(-1, 0))
	wantEQ(t, "未知动作", "LikeNumber", got.LikeNumber, int64(4))
}

// TestLikeCancelWithoutStatRowGoesNegative 缺陷 #8 的现象固化：
// 关系行在、计数行不在（历史数据/回刷丢失）时取消点赞，
// Incr 的 SQL 走 INSERT 分支，直接写入 like_number = -1（没有 GREATEST(0, …) 兜底）。
// 修复后本用例应改红。
func TestLikeCancelWithoutStatRowGoesNegative(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_CANCEL_LIKE,
	})
	wantNoErr(t, "缺计数行的取消", err)
	wantOps(t, "缺计数行的取消", st.log.opsFrom(0), likeOps(-1, 0))
	stat := st.stat.get(likeBiz, likeOrigin, likeMessage)
	if stat == nil {
		t.Fatalf("Incr 应插入计数行")
	}
	wantEQ(t, "缺陷 #8", "LikeNumber 变成负数", stat.LikeNumber, int64(-1))
	wantEQ(t, "缺陷 #8", "响应原样透出负数", got.LikeNumber, int64(-1))
}

// TestLikeReturnedNumberIsDbOnly Repository.Like 的注释写「DB 值 + Redis 计数器」，
// 实现只返回 DB 值（Redis 只是影子计数器）。这条断言锁实现口径，并登记注释与代码不一致（缺陷 #7）。
// 如果哪天真按注释实现成相加，这里会红，届时必须同时处理「回刷后双计」的问题。
func TestLikeReturnedNumberIsDbOnly(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 10, 0, 0, 0)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_LIKE,
	})
	wantNoErr(t, "DB 有基数", err)
	wantEQ(t, "返回口径", "LikeNumber = DB 值（10+1），不是 DB+Redis 相加的 12", got.LikeNumber, int64(11))
	wantEQ(t, "返回口径", "影子计数器单独累计", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(1))
}

// TestLikeDoesNotTouchOtherDomainTables AGENTS.md §5 的所有权边界在本缝上的表达：
// Repository 的依赖面只有 thumbup_* + 本域缓存，点赞不得波及收藏/分享表。
func TestLikeDoesNotTouchOtherDomainTables(t *testing.T) {
	st := newStore()
	seedFavItem(st, likeMid, likeMessage, 33, 2, 11, 0)
	seedFolder(st, 33, likeMid, "只看番", 1, model.FolderStateNormal, 1)
	seedShare(st, likeMessage, likeMid, 2, 20260901)

	_, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_LIKE,
	})
	wantNoErr(t, "点赞", err)
	wantCount(t, "越权检查", st.log, "favItem.", 0)
	wantCount(t, "越权检查", st.log, "folder.", 0)
	wantCount(t, "越权检查", st.log, "share.", 0)
	wantEQ(t, "越权检查", "收藏项未被波及", st.favItem.get(likeMid, likeMessage, 2).State, int32(0))
	wantEQ(t, "越权检查", "收藏夹未被波及", st.folder.get(33).State, int32(model.FolderStateNormal))
	wantEQ(t, "越权检查", "分享行数未被波及", st.share.rowCount(), 1)
	wantCount(t, "越权检查", st.log, "cache.DelFolders", 0)
	wantCount(t, "越权检查", st.log, "cache.SetIsFavored", 0)
}

// TestLikePropagatesFindStatesFailure 第一步就失败：一行都不能落，缓存也不动。
func TestLikePropagatesFindStatesFailure(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)
	st.like.failWith("FindStates", errBoom)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_LIKE,
	})
	wantFail(t, "FindStates 失败", got, err, errBoom)
	wantOps(t, "FindStates 失败后的调用", st.log.opsFrom(0), []string{"like.FindStates:archive:7:101"})
	wantEQ(t, "FindStates 失败", "未写关系行", len(st.like.rows), 0)
	wantEQ(t, "FindStates 失败", "计数未被改动", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeNumber, int64(5))
}

// TestLikePropagatesUpsertFailure 关系行写失败时不得先动计数（顺序本身就是防漂移的一半）。
func TestLikePropagatesUpsertFailure(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)
	st.like.failWith("Upsert", errBoom)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_LIKE,
	})
	wantFail(t, "Upsert 失败", got, err, errBoom)
	wantOps(t, "Upsert 失败后的调用", st.log.opsFrom(0), []string{
		"like.FindStates:archive:7:101", "like.Upsert:archive:7:101",
	})
	wantEQ(t, "Upsert 失败", "未插入关系行", len(st.like.rows), 0)
	wantEQ(t, "Upsert 失败", "计数未被改动", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeNumber, int64(5))
	wantCount(t, "Upsert 失败", st.log, "stat.Incr", 0)
	wantCount(t, "Upsert 失败", st.log, "cache.", 0)
}

// TestLikeStatIncrFailureLeavesRelationRow 缺陷 #1 的现象固化：
// 「关系行」和「计数」是两次独立写入、不在同一事务里，所以 Incr 失败时
// thumbup_like 已经变成已点赞，但 thumbup_stat 一个字节都没写。
// 更糟的是它**不可自愈**：用户再点一次会命中幂等短路（旧状态==新状态），
// 只回读计数行，返回 0，计数就永远停在 0。修复（加事务或补偿）后本用例会红。
func TestLikeStatIncrFailureLeavesRelationRow(t *testing.T) {
	st := newStore()
	l := NewLikeLogic(context.Background(), newTestSvc(st))
	in := &rpc.LikeReq{Business: likeBiz, Mid: likeMid, OriginId: likeOrigin,
		MessageId: likeMessage, Action: rpc.Action_ACTION_LIKE}
	st.stat.failWith("Incr", errBoom)

	got, err := l.Like(in)
	wantFail(t, "Incr 失败", got, err, errBoom)
	wantOps(t, "Incr 失败后的调用", st.log.opsFrom(0), []string{
		"like.FindStates:archive:7:101", "like.Upsert:archive:7:101", "stat.Incr:archive:1:101:1/0",
	})
	wantEQ(t, "缺陷 #1", "关系行已落地（半截数据）", st.like.get(likeBiz, likeMid, likeMessage).State, int32(model.LikeStateLike))
	wantEQ(t, "缺陷 #1", "计数行没被创建", len(st.stat.rows), 0)
	wantCount(t, "缺陷 #1", st.log, "cache.", 0)

	// 用户重试：命中幂等短路，不补计数，返回 0 —— 点赞「成功」了但计数永久丢失。
	st.stat.faultInjector = faultInjector{}
	failedCalls := st.log.snapshot()
	retry, err := l.Like(in)
	wantNoErr(t, "重试", err)
	wantEQ(t, "缺陷 #1", "重试也不补计数", retry.LikeNumber, int64(0))
	wantOps(t, "重试的调用", st.log.opsFrom(failedCalls), likeReadOps())
}

// TestLikeFinalReadFailureStillConverges 最后一步回读失败：两步写入都已完成，
// 所以调用方看到的是错误，但重试会走幂等短路拿到正确计数（不双计）。
func TestLikeFinalReadFailureStillConverges(t *testing.T) {
	st := newStore()
	l := NewLikeLogic(context.Background(), newTestSvc(st))
	in := &rpc.LikeReq{Business: likeBiz, Mid: likeMid, OriginId: likeOrigin,
		MessageId: likeMessage, Action: rpc.Action_ACTION_LIKE}
	st.stat.failWith("FindOne", errBoom)

	got, err := l.Like(in)
	wantFail(t, "回读失败", got, err, errBoom)
	wantOps(t, "回读失败后的调用", st.log.opsFrom(0), []string{
		"like.FindStates:archive:7:101", "like.Upsert:archive:7:101", "stat.Incr:archive:1:101:1/0",
		"cache.IncrLike:lc:archive:1:101:1", "cache.IncrDislike:dc:archive:1:101:0", "stat.FindOne:archive:1:101",
	})
	wantEQ(t, "回读失败", "计数已加过一次", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeNumber, int64(1))

	st.stat.faultInjector = faultInjector{}
	retry, err := l.Like(in)
	wantNoErr(t, "重试", err)
	wantEQ(t, "重试", "收敛到 1（不双计）", retry.LikeNumber, int64(1))
	wantCount(t, "重试", st.log, "stat.Incr:", 1)
}

// TestLikePropagatesNoopReadFailure 幂等短路分支的计数读失败也必须透出：
// 客户端不能把「读不到计数」当成就 0。
func TestLikePropagatesNoopReadFailure(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)
	st.stat.failWith("FindOne", errBoom)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_LIKE,
	})
	wantFail(t, "短路读失败", got, err, errBoom)
	wantOps(t, "短路读失败后的调用", st.log.opsFrom(0), likeReadOps())
	wantCount(t, "短路读失败", st.log, "stat.Incr:", 0)
	wantCount(t, "短路读失败", st.log, "cache.", 0)
}

// TestLikeSwallowsCacheCounterFailure Redis 影子计数器写失败被丢弃（`_ =`）：
// 返回成功、DB 计数正确。这是可用性取舍（计数以 DB 为准），
// 但 Redis 计数器会比 DB 少一跳，定时回刷的差值口径要能容忍（缺陷 #5 同源）。
func TestLikeSwallowsCacheCounterFailure(t *testing.T) {
	st := newStore()
	st.cache.failWith("IncrLikeCount", errBoom)
	st.cache.failWith("IncrDislikeCount", errBoom)

	got, err := NewLikeLogic(context.Background(), newTestSvc(st)).Like(&rpc.LikeReq{
		Business: likeBiz, Mid: likeMid, OriginId: likeOrigin, MessageId: likeMessage,
		Action: rpc.Action_ACTION_LIKE,
	})
	wantNoErr(t, "计数器写失败", err)
	wantEQ(t, "计数器写失败", "LikeNumber 以 DB 为准", got.LikeNumber, int64(1))
	wantOps(t, "计数器写失败", st.log.opsFrom(0), likeOps(1, 0))
	wantEQ(t, "计数器写失败", "Redis 计数器没写上", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(0))
}
