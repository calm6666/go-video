package logic

// setnickupdatedlogic_test.go 覆盖 SetNickUpdated（标记「已首次修改昵称」）。
//
// 这条链路是这批里最短的一个：logic → Repository.SetNickUpdated →
// flagModel.SetAttr(mid, model.NickUpdated)，落到 model 层是一句
// `INSERT INTO user_flag (mid, flag) VALUES (?, ?) ON DUPLICATE KEY UPDATE flag = flag | ?`。
// 由此产生的、必须被钉住的现状：
//  1. 只有一步 DB 调用：**没有事务、没有 Outbox 事件、没有任何缓存失效**
//     （user_flag 没有缓存，读侧 NickUpdated 每次直查 DB，所以不失效是安全的；
//     但「标志位变更」这件事在事件流里完全不可见，下游无法据此收敛行为）；
//  2. 位或写入天然幂等，且**只能置位不能复位**；
//  3. 没有任何前置守卫，也没有「必须先改过昵称才允许置位」的因果校验。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

const nickAttr = int64(model.NickUpdated) // 标志位 = 1

// TestSetNickUpdatedHasNoPreconditionGuard 钉「mid 不校验、重复调用不拒绝」。
// 每一步都必须真的落到 flag.SetAttr（轨迹非空），否则就是「有守卫」。
//
// TODO(缺陷)：
//   - mid <= 0：user_flag 会以 mid=0 / mid<0 为主键补一行（真实 MySQL 的
//     `mid` BIGINT UNSIGNED 会拒负数，本层不拦），而 user_base 里没有这个人；
//   - 与 SetName 之间没有因果关系：从没改过昵称也能置位，改过昵称（SetName）
//     却不会自动置位——两件事完全靠调用方自觉，置位时机不可信；
//   - 无调用者身份约束，用户可以自己将「首次改名」奖励/流程标记置为已完成。
func TestSetNickUpdatedHasNoPreconditionGuard(t *testing.T) {
	cases := []struct {
		label string
		mid   int64
	}{
		{"mid=0 照样置位", 0},
		{"mid 负数照样置位", -1},
		{"正常 mid", 33001},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			l := NewSetNickUpdatedLogic(context.Background(), e.svcCtx)
			reply, err := l.SetNickUpdated(&rpc.MemberMidReq{Mid: tc.mid, RemoteIp: "10.0.0.3"})
			wantNoErr(t, tc.label, err)
			if reply == nil {
				t.Fatalf("%s：成功时 reply 必须非 nil", tc.label)
			}
			wantOps(t, tc.label, e.ops(0), []string{"flag.SetAttr:" + itoa(tc.mid) + "/" + itoa(nickAttr)})
		})
	}
}

// TestSetNickUpdatedIsIdempotentAndMonotonic 幂等 + 只置位不复位。
// 重放三次必须：值仍是 1、库里仍只有一位、且**每次都真的发生写**（没有「已置位就跳过」的短路）。
func TestSetNickUpdatedIsIdempotentAndMonotonic(t *testing.T) {
	e := newEnv(t)
	l := NewSetNickUpdatedLogic(context.Background(), e.svcCtx)

	wantSeq := make([]string, 0, 3)
	for i := 1; i <= 3; i++ {
		_, err := l.SetNickUpdated(&rpc.MemberMidReq{Mid: 33100})
		wantNoErr(t, "重复置位", err)
		wantSeq = append(wantSeq, "flag.SetAttr:33100/1")
		wantOps(t, "重放第 3 次仍真写（无短路）", e.ops(0), wantSeq)
		wantEQ(t, "重复置位", "flag 值仍是 1（位或幂等）", e.st.flag.value(33100), uint(model.NickUpdated))
	}

	// 已有一位不影响新位：先把别的标志位（模型里目前只有 1，这里用 2 模拟未来新增位）布进去，
	// SetNickUpdated 必须只 OR 上 1，不能覆盖掉 2。
	e2 := newEnv(t)
	e2.st.flag.put(33101, 2)
	_, err := NewSetNickUpdatedLogic(context.Background(), e2.svcCtx).SetNickUpdated(&rpc.MemberMidReq{Mid: 33101})
	wantNoErr(t, "与其它标志位共存", err)
	wantEQ(t, "与其它标志位共存", "flag = 2|1 = 3（不覆盖其它位）", e2.st.flag.value(33101), uint(3))

	// 反向：本方法绝不能清位。
	if e2.st.flag.value(33101)&uint(model.NickUpdated) == 0 {
		t.Error("与其它标志位共存：NickUpdated 位丢了")
	}
}

// TestSetNickUpdatedReadBack 写后读：NickUpdated 必须立刻读到 true，
// 并且读路径只有 flag.HasAttr 一步（证明 user_flag 确实没有缓存层，
// 也就解释了为什么本方法不需要失效任何 key）。
func TestSetNickUpdatedReadBack(t *testing.T) {
	e := newEnv(t)
	wl := NewNickUpdatedLogic(context.Background(), e.svcCtx)

	e.st.log.reset()
	before, err := wl.NickUpdated(&rpc.MemberMidReq{Mid: 33200})
	wantNoErr(t, "置位前查询", err)
	wantOps(t, "置位前查询", e.ops(0), []string{"flag.HasAttr:33200/1"})
	if before.GetNickUpdated() {
		t.Fatal("置位前查询：还没置位就返回 true 了")
	}

	_, err = NewSetNickUpdatedLogic(context.Background(), e.svcCtx).SetNickUpdated(&rpc.MemberMidReq{Mid: 33200})
	wantNoErr(t, "置位", err)

	e.st.log.reset()
	after, err := wl.NickUpdated(&rpc.MemberMidReq{Mid: 33200})
	wantNoErr(t, "置位后查询", err)
	wantOps(t, "置位后查询（读路径直查 DB，无缓存）", e.ops(0), []string{"flag.HasAttr:33200/1"})
	if !after.GetNickUpdated() {
		t.Fatal("置位后查询：写进去读不出来")
	}
}

// TestSetNickUpdatedEmitsNoEventAndInvalidatesNoCache 钉「副作用面为零」这件事：
// 资料变更家族（SetRank/SetName/…）都发 user.profile.updated，本方法一条都不发，
// 也不删 bs_/exp_/moral_ 中任何一个 key。若将来给 user_flag 加了缓存，本用例会红，
// 提醒补失效——这条断言的价值是「变化会被发现」，不是「现状多好」。
func TestSetNickUpdatedEmitsNoEventAndInvalidatesNoCache(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 33300, Name: "小张", Rank: 5000})
	e.st.cache.warmJSON(keyBase(33300), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 33300, Name: "小张"}})
	e.st.cache.warmInt(keyExp(33300), 12345)
	e.st.cache.warmJSON(keyMoral(33300), model.UserMoral{Mid: 33300, Moral: model.DefaultMoral})

	l := NewSetNickUpdatedLogic(context.Background(), e.svcCtx)
	_, err := l.SetNickUpdated(&rpc.MemberMidReq{Mid: 33300, RemoteIp: "203.0.113.1"})
	wantNoErr(t, "置位无副作用", err)

	wantOps(t, "置位无副作用", e.ops(0), []string{"flag.SetAttr:33300/1"})
	wantEQ(t, "置位无副作用", "事务次数", e.st.conn.transactions, 0)
	wantEQ(t, "置位无副作用", "Outbox 行数（标志位变更不发事件）", e.st.outbox.count(), 0)
	wantEQ(t, "置位无副作用", "member_log 行数", e.st.logs.count(), 0)
	wantEQ(t, "置位无副作用", "cache.Del 次数", e.st.log.countPrefix("cache.Del:"), 0)
	if _, ok := e.st.cache.jsons[keyBase(33300)]; !ok {
		t.Error("置位无副作用：bs_33300 被删了，与「本方法不失效缓存」的结论不符")
	}
	if _, ok := e.st.cache.strs[keyExp(33300)]; !ok {
		t.Error("置位无副作用：exp_33300 被删了，与结论不符")
	}
}

func TestSetNickUpdatedDownstreamFailure(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("Error 1406: Data too long / disk full on user_flag")
	e.st.flag.failWith("SetAttr", boom)

	l := NewSetNickUpdatedLogic(context.Background(), e.svcCtx)
	reply, err := l.SetNickUpdated(&rpc.MemberMidReq{Mid: 33400})
	wantErrIs(t, "写失败", err, boom)
	if reply != nil {
		t.Errorf("写失败：reply = %+v, want nil（失败必须假成功不了）", reply)
	}
	wantOps(t, "写失败", e.ops(0), []string{"flag.SetAttr:33400/1"})
	wantEQ(t, "写失败", "标志位未被写入", e.st.flag.value(33400), uint(0))

	// 读侧仍是 false：没有「写失败但读成功」的假象。
	e.st.log.reset()
	n, err := NewNickUpdatedLogic(context.Background(), e.svcCtx).NickUpdated(&rpc.MemberMidReq{Mid: 33400})
	wantNoErr(t, "写失败后读", err)
	if n.GetNickUpdated() {
		t.Error("写失败后读：返回了 true（假成功）")
	}
}
