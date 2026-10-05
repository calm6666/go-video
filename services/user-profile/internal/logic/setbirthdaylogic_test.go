package logic

// setbirthdaylogic_test.go 覆盖 SetBirthday（设置生日）。
//
// 与 SetName/SetSign 同一条 setBaseTx 链路，所以共用不变量（事务/事件/失效范围）
// 直接走 wantSetBaseWritten，本文件只钉 birthday 这一个值域的特殊性：
//  1. DDL 是 `birthday` BIGINT（**有符号**，默认 -28800），负值合法且被当作哨兵用，
//     所以「0 是不是清空」「越界值」这两件事必须钉住现状；
//  2. 与 SetBase 的对照：SetBase 里 Birthday==0 会被归一成 DefaultTime(-28800)，
//     而 SetBirthday **完全没有这层归一**，同一个语义在两个写入口口径不一致；
//  3. 生日在业务上通常是「只允许设置一次」的字段，本服务没有任何一次性检查；
//  4. BaseInfo 把 birthday 原样吐给网关，越界值会一路穿透到前端渲染。

import (
	"context"
	"errors"
	"math"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// TestBirthdayGuardTableIsPinnedAsNoGuard 钉「SetBirthday 无任何前置守卫」。
// 每个畸形值都必须看到**完整写入序列**（轨迹非空）+ 原样落库，
// 轨迹为空才说明守卫存在——那样本用例就该红，用来防「有人悄悄加了校验却让注释过期」。
//
// TODO(缺陷) 应拒未拒：
//   - mid <= 0：给不存在的账号补一行 user_base（幻影资料）；
//   - 未来日期 / 公元 1900 年前：生日的合理性区间不检查，下游按生日推年龄的功能会拿到负数年龄；
//   - 0 与 -1 等哨兵值：与「未设置」的 DefaultTime(-28800) 语义冲突，本层不区分；
//   - 一次性：重复调用可无限次改写，没有「已设置就不许再改」的约束；
//   - remote_ip 契约里有但完全不用，生日变更零审计。
func TestBirthdayGuardTableIsPinnedAsNoGuard(t *testing.T) {
	cases := []struct {
		label string
		mid   int64
		want  int64
	}{
		{"mid=0 照样写", 0, 946684800},
		{"mid 负数照样写", -3, 946684800},
		{"0 被当成 1970-01-01 而不是未设置", 31001, 0},
		{"负 1 照样写", 31002, -1},
		{"未来日期不拒绝", 31003, 4102444800},
		{"公元 1900 年前不拒绝", 31004, -2524611600},
		{"int64 上限不裁剪（真实 MySQL 会按 BIGINT 存下）", 31005, math.MaxInt64},
		{"int64 下限不裁剪", 31006, math.MinInt64},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			l := NewSetBirthdayLogic(context.Background(), e.svcCtx)
			reply, err := l.SetBirthday(&rpc.UpdateBirthdayReq{Mid: tc.mid, Birthday: tc.want, RemoteIp: "10.0.0.1"})
			wantNoErr(t, tc.label, err)
			if reply == nil {
				t.Fatalf("%s：成功时 reply 必须非 nil", tc.label)
			}
			wantSetBaseWritten(t, tc.label, e, tc.mid, "SetBirthday", repository.ActUpdatePersonInfo, 1)
			row := e.st.base.get(tc.mid)
			if row == nil {
				t.Fatalf("%s：user_base 没有落库", tc.label)
			}
			wantEQ(t, tc.label, "birthday 原样入库", row.Birthday, tc.want)
		})
	}
}

// TestBirthdayZeroIsNotNormalizedUnset 单独把「0 不被归一」这件事钉成一个用例，
// 并和 SetBase 的口径放在一起对照（对照用例行才是这条的价值所在）。
func TestBirthdayZeroIsNotNormalizedUnset(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 31010, Name: "小美", Birthday: model.DefaultTime})

	l := NewSetBirthdayLogic(context.Background(), e.svcCtx)
	_, err := l.SetBirthday(&rpc.UpdateBirthdayReq{Mid: 31010, Birthday: 0})
	wantNoErr(t, "生日写 0", err)
	wantSetBaseWritten(t, "生日写 0", e, 31010, "SetBirthday", repository.ActUpdatePersonInfo, 1)
	wantEQ(t, "生日写 0", "birthday 存成 0（SetBirthday 不做 0→-28800 归一）", e.st.base.get(31010).Birthday, int64(0))

	// 对照：同一份「生日=0」的意图走 Repository.SetBase 会被归一，两个写入口对 0 的
	// 口径不一致。注意这里只能直接调 Repository——**SetBase 没有任何 logic 入口**
	// （35 个 RPC 方法里没有 SetBase，见 README 已知缺口：它是从 logic 层看不见的死代码）。
	e.st.log.reset()
	wantNoErr(t, "对照 SetBase 写 0", e.st.repo.SetBase(context.Background(), &model.UserBase{Mid: 31010, Birthday: 0}))
	wantEQ(t, "对照 SetBase 写 0", "SetBase 把 0 归一成 DefaultTime", e.st.base.get(31010).Birthday, int64(model.DefaultTime))

	// 穿透到读侧：0 会被原样吐给网关（不是「未设置」哨兵）。
	e.st.log.reset()
	bl := NewBaseLogic(context.Background(), e.svcCtx)
	baseAfter, err := bl.Base(&rpc.MemberMidReq{Mid: 31010})
	wantNoErr(t, "读回生日", err)
	wantEQ(t, "读回生日", "SetBase 之后生日是归一值", baseAfter.GetBirthday(), int64(model.DefaultTime))
}

func TestBirthdayOnlyTouchesTheBirthdayColumn(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{
		Mid: 31020, Name: "旧昵称", Sex: 2, Face: "https://cdn.example.com/a.png",
		Sign: "旧签名", Rank: 12345, Birthday: 1,
	})
	e.st.cache.warmJSON(keyBase(31020), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
		Mid: 31020, Name: "缓存昵称", Birthday: 1,
	}})

	l := NewSetBirthdayLogic(context.Background(), e.svcCtx)
	_, err := l.SetBirthday(&rpc.UpdateBirthdayReq{Mid: 31020, Birthday: 946684800, RemoteIp: "203.0.113.7"})
	wantNoErr(t, "改生日", err)
	wantSetBaseWritten(t, "改生日", e, 31020, "SetBirthday", repository.ActUpdatePersonInfo, 1)

	row := e.st.base.get(31020)
	wantEQ(t, "改生日", "birthday", row.Birthday, int64(946684800))
	wantEQ(t, "改生日", "name 不许被顺手改", row.Name, "旧昵称")
	wantEQ(t, "改生日", "sex 不许被顺手改", row.Sex, int64(2))
	wantEQ(t, "改生日", "face 不许被顺手改", row.Face, "https://cdn.example.com/a.png")
	wantEQ(t, "改生日", "sign 不许被顺手改", row.Sign, "旧签名")
	wantEQ(t, "改生日", "rank 不许被顺手改", row.Rank, int64(12345))

	// 写后读：bs_ 已失效 → 回源 → 按 TTL 3600 回填，且读到的就是新生日。
	e.st.log.reset()
	bl := NewBaseLogic(context.Background(), e.svcCtx)
	base, err := bl.Base(&rpc.MemberMidReq{Mid: 31020})
	wantNoErr(t, "改生日后读回", err)
	wantOps(t, "改生日后读回", e.ops(0), []string{
		"cache.GetJSON:bs_31020",
		"base.FindOne:31020",
		"cache.SetJSON:bs_31020/3600",
	})
	wantEQ(t, "改生日后读回", "birthday", base.GetBirthday(), int64(946684800))
}

func TestBirthdayCreatesMissingRowWithDefaults(t *testing.T) {
	e := newEnv(t)
	l := NewSetBirthdayLogic(context.Background(), e.svcCtx)
	_, err := l.SetBirthday(&rpc.UpdateBirthdayReq{Mid: 31030, Birthday: 1000000000})
	wantNoErr(t, "给不存在的 mid 设生日", err)
	wantSetBaseWritten(t, "给不存在的 mid 设生日", e, 31030, "SetBirthday", repository.ActUpdatePersonInfo, 1)

	row := e.st.base.get(31030)
	if row == nil {
		t.Fatal("给不存在的 mid 设生日：没有补建行")
	}
	wantEQ(t, "补建行", "birthday", row.Birthday, int64(1000000000))
	wantEQ(t, "补建行", "name 空", row.Name, "")
	wantEQ(t, "补建行", "rank 取 DDL 默认 5000", row.Rank, int64(model.DefaultRank))
	wantEQ(t, "补建行", "sex 取 DDL 默认 0", row.Sex, int64(0))
}

func TestBirthdayDownstreamFailures(t *testing.T) {
	boomDB := errors.New("Error 1264: Out of range value for column 'birthday'")
	t.Run("单列写失败：不发事件、不失效缓存", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 31040, Birthday: 1})
		e.st.cache.warmJSON(keyBase(31040), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 31040, Birthday: 1}})
		e.st.base.failWith("SetBirthday", boomDB)

		l := NewSetBirthdayLogic(context.Background(), e.svcCtx)
		reply, err := l.SetBirthday(&rpc.UpdateBirthdayReq{Mid: 31040, Birthday: 999})
		wantErrIs(t, "单列写失败", err, boomDB)
		if reply != nil {
			t.Errorf("单列写失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "单列写失败", e.ops(0), []string{"base.SetBirthday:31040"})
		wantEQ(t, "单列写失败", "事务次数", e.st.conn.transactions, 1)
		wantEQ(t, "单列写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "单列写失败", "事件行数", e.st.outbox.count(), 0)
		if _, ok := e.st.cache.jsons[keyBase(31040)]; !ok {
			t.Error("单列写失败：缓存被顺手删了（失败不该扩大影响面）")
		}
	})

	t.Run("Outbox 写失败：错误传出且不失效缓存", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 31041, Birthday: 1})
		boom := errors.New("insert into member_outbox: boom")
		e.st.outbox.failWith("Insert", boom)

		l := NewSetBirthdayLogic(context.Background(), e.svcCtx)
		_, err := l.SetBirthday(&rpc.UpdateBirthdayReq{Mid: 31041, Birthday: 999})
		wantErrIs(t, "Outbox 写失败", err, boom)
		wantOps(t, "Outbox 写失败", e.ops(0), []string{
			"base.SetBirthday:31041",
			"outbox.Insert:user.profile.updated/31041/1",
		})
		wantEQ(t, "Outbox 写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "Outbox 写失败", "事件行数（同事务回滚不留行）", e.st.outbox.count(), 0)
	})

	t.Run("缓存失效失败被吞：接口成功但脏缓存继续吐旧生日", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 31042, Birthday: 1})
		e.st.cache.warmJSON(keyBase(31042), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 31042, Birthday: 1}})
		e.st.cache.failWith("Del", errors.New("del bs_31042: redis down"))

		l := NewSetBirthdayLogic(context.Background(), e.svcCtx)
		reply, err := l.SetBirthday(&rpc.UpdateBirthdayReq{Mid: 31042, Birthday: 946684800})
		wantNoErr(t, "失效失败", err)
		if reply == nil {
			t.Fatal("失效失败：成功时 reply 必须非 nil")
		}
		wantOps(t, "失效失败", e.ops(0), []string{
			"base.SetBirthday:31042",
			"outbox.Insert:user.profile.updated/31042/1",
			"cache.Del:bs_31042",
		})
		wantEQ(t, "失效失败", "库里已是新值", e.st.base.get(31042).Birthday, int64(946684800))
		var stale baseCachePayload
		if !e.st.cache.jsonOf(keyBase(31042), &stale) {
			t.Fatal("失效失败：缓存竟然没了（说明 Del 的错误被替身当成成功执行）")
		}
		wantEQ(t, "失效失败", "缓存仍是旧生日", stale.Birthday, int64(1))

		e.st.log.reset()
		bl := NewBaseLogic(context.Background(), e.svcCtx)
		base, err := bl.Base(&rpc.MemberMidReq{Mid: 31042})
		wantNoErr(t, "失效失败后读回", err)
		wantOps(t, "失效失败后读回", e.ops(0), []string{"cache.GetJSON:bs_31042"})
		wantEQ(t, "失效失败后读回", "脏读是持续性的", base.GetBirthday(), int64(1))
	})
}

// TestBirthdayCarriesPersonInfoActionAndNoAuditFields 断言事件里的 action 是
// updatePersonInfo（不是 updateUname/updateFace），且 payload 只有 {mid, action}。
func TestBirthdayCarriesPersonInfoActionAndNoAuditFields(t *testing.T) {
	e := newEnv(t)
	l := NewSetBirthdayLogic(context.Background(), e.svcCtx)
	_, err := l.SetBirthday(&rpc.UpdateBirthdayReq{Mid: 31050, Birthday: 946684800, RemoteIp: "203.0.113.50"})
	wantNoErr(t, "生日事件", err)
	wantSetBaseWritten(t, "生日事件", e, 31050, "SetBirthday", repository.ActUpdatePersonInfo, 1)

	_, pl := decodeOutboxView(t, "生日事件", e.st.outbox.row(0))
	wantEQ(t, "生日事件", "payload 键数量", len(pl), 2)
	if _, ok := pl["birthday"]; ok {
		t.Errorf("生日事件：payload 泄漏了生日值 = %v（PII 不应进事件，AGENTS.md §6）", pl["birthday"])
	}
	if _, ok := pl["ip"]; ok {
		t.Errorf("生日事件：payload 出现了 ip = %v", pl["ip"])
	}
	wantEQ(t, "生日事件", "member_log 行数（生日变更不留服务端审计日志）", e.st.logs.count(), 0)
}
