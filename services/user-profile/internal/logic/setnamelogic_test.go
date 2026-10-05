package logic

// setnamelogic_test.go 覆盖 SetName（改昵称）。
//
// 链路事实：logic 只把 in.Mid / in.Name 交给 Repository.SetName，
// Repository.setBaseTx 在**一个事务**里做「单列 UPSERT + Outbox 事件」，提交成功后
// 才失效 bs_<mid>。因此本文件钉的四件事是：
//  1. 事务边界与事件同事务（AGENTS.md §5）——事件行必须拿得到 tx 会话；
//  2. 单列 UPSERT 不许顺手改别的列（一次改名把头像/签名清掉是线上事故）；
//  3. 失效范围：只删 bs_<mid>，且删失败时同步结果仍是成功（行为哨兵，见缺陷）；
//  4. 校验缺失：mid、长度（DDL `name` VARCHAR(64)）、空白串、remote_ip 一律不校验/不使用。
//
// 缺的校验不虚构，一律钉成现状并在 wantSetNameNoGuard 里登记 TODO(缺陷)。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// wantSetNameNoGuard 钉住「SetName 没有任何前置守卫」这件事本身：
// 每种畸形入参都必须看到**完整的写入序列**（一次 UPSERT + 一次事件 + 一次失效），
// 而不是轨迹为空——轨迹为空才说明守卫存在。
//
// TODO(缺陷) 应在此处拒绝而目前不拒绝的输入：
//   - mid <= 0：改一个不存在的账号，会凭空造出一行 user_base（幻影资料）；
//   - 长度 > 64：DDL `name` VARCHAR(64)，本层不裁剪也不报错，超长只能等 MySQL 抛 1406；
//   - 全空白 / 含换行的昵称：昵称合规性（敏感词、格式）在本服务无任何检查，
//     审核结论 owner 是 moderation-orchestrator（AGENTS.md §7/§5），这里既不入审核也不拦截；
//   - remote_ip：契约里有、logic 里完全不用，昵称变更不留任何来源 IP 审计。
func TestSetNameHasNoPreconditionGuard(t *testing.T) {
	cases := []struct {
		label string
		mid   int64
		name  string
		want  string
	}{
		{"mid=0 照样写", 0, "零号用户", "零号用户"},
		{"mid 负数照样写", -7, "负号用户", "负号用户"},
		{"空串按清空处理不拒绝", 30010, "", ""},
		{"全空格不拒绝", 30011, "   ", "   "},
		{"超出 DDL VARCHAR(64) 的 65 字符不裁剪", 30012, strings.Repeat("昵", 65), strings.Repeat("昵", 65)},
		{"300 字符不裁剪", 30013, strings.Repeat("x", 300), strings.Repeat("x", 300)},
		{"换行与制表符不拒绝", 30014, "第一行\n\t第二行", "第一行\n\t第二行"},
		{"SQL 注入形态的字符串原样入库", 30015, "a'); DROP TABLE user_base; --", "a'); DROP TABLE user_base; --"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			l := NewSetNameLogic(context.Background(), e.svcCtx)
			reply, err := l.SetName(&rpc.UpdateUnameReq{Mid: tc.mid, Name: tc.name, RemoteIp: "10.0.0.9"})
			wantNoErr(t, tc.label, err)
			if reply == nil {
				t.Fatalf("%s：成功时 reply 必须非 nil（*EmptyReply 是空消息，nil 会让客户端解引用失败）", tc.label)
			}
			wantSetBaseWritten(t, tc.label, e, tc.mid, "SetName", repository.ActUpdateUname, 1)
			row := e.st.base.get(tc.mid)
			if row == nil {
				t.Fatalf("%s：user_base 里没有落库", tc.label)
			}
			wantEQ(t, tc.label, "落库 name 逐字未改", row.Name, tc.want)
		})
	}
}

func TestSetNameUpdatesOnlyTheNameColumn(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{
		Mid: 30001, Name: "旧昵称", Sex: 2, Face: "https://cdn.example.com/old.png",
		Sign: "旧签名", Rank: 12345, Birthday: 946684800,
	})
	// 缓存里放一份**更旧**的资料：写完之后必须读不到它（失效真生效）。
	e.st.cache.warmJSON(keyBase(30001), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
		Mid: 30001, Name: "缓存里的旧昵称", Sex: 1, Face: "cached.png", Sign: "缓存签名", Rank: 1, Birthday: 1,
	}})

	l := NewSetNameLogic(context.Background(), e.svcCtx)
	reply, err := l.SetName(&rpc.UpdateUnameReq{Mid: 30001, Name: "新昵称", RemoteIp: "10.1.1.1"})
	wantNoErr(t, "改昵称", err)
	if reply == nil {
		t.Fatal("改昵称：成功时 reply 必须非 nil")
	}
	wantSetBaseWritten(t, "改昵称", e, 30001, "SetName", repository.ActUpdateUname, 1)

	row := e.st.base.get(30001)
	if row == nil {
		t.Fatal("改昵称：user_base 行丢了")
	}
	wantEQ(t, "改昵称", "name 改成新值", row.Name, "新昵称")
	wantEQ(t, "改昵称", "sex 不许被顺手改", row.Sex, int64(2))
	wantEQ(t, "改昵称", "face 不许被顺手改", row.Face, "https://cdn.example.com/old.png")
	wantEQ(t, "改昵称", "sign 不许被顺手改", row.Sign, "旧签名")
	wantEQ(t, "改昵称", "rank 不许被顺手改", row.Rank, int64(12345))
	wantEQ(t, "改昵称", "birthday 不许被顺手改", row.Birthday, int64(946684800))

	// 写后读：缓存必须已失效，Base 走回源并把新昵称读出、按 TTL=3600 回填。
	e.st.log.reset()
	bl := NewBaseLogic(context.Background(), e.svcCtx)
	base, err := bl.Base(&rpc.MemberMidReq{Mid: 30001})
	wantNoErr(t, "改昵称后读回", err)
	wantOps(t, "改昵称后读回", e.ops(0), []string{
		"cache.GetJSON:bs_30001",
		"base.FindOne:30001",
		"cache.SetJSON:bs_30001/3600",
	})
	wantEQ(t, "改昵称后读回", "name", base.GetName(), "新昵称")
	wantEQ(t, "改昵称后读回", "mid", base.GetMid(), int64(30001))
}

func TestSetNameEmptyValueClearsColumn(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 30002, Name: "要被清掉的昵称", Sex: 1, Face: "f", Sign: "s", Rank: 8888, Birthday: 100})

	l := NewSetNameLogic(context.Background(), e.svcCtx)
	_, err := l.SetName(&rpc.UpdateUnameReq{Mid: 30002, Name: ""})
	wantNoErr(t, "空昵称", err)
	wantSetBaseWritten(t, "空昵称", e, 30002, "SetName", repository.ActUpdateUname, 1)
	row := e.st.base.get(30002)
	if row == nil {
		t.Fatal("空昵称：行没了")
	}
	wantEQ(t, "空昵称", "name 被清空（不是拒绝也不是保留旧值）", row.Name, "")
	wantEQ(t, "空昵称", "rank 保留", row.Rank, int64(8888))
}

func TestSetNameCreatesMissingRowWithDDLDefaults(t *testing.T) {
	// TODO(缺陷) 无「用户是否存在」校验：给一个 account 侧不存在的 mid 改名，
	// 单列 INSERT ... ON DUPLICATE 会凭空补出一行 user_base，此后 BaseInfo 不再把它
	// 判成「查无此人」（mid != 0），Member 也不再返回 ErrMemberNotExist。
	e := newEnv(t)
	l := NewSetNameLogic(context.Background(), e.svcCtx)
	_, err := l.SetName(&rpc.UpdateUnameReq{Mid: 30003, Name: "幽灵用户"})
	wantNoErr(t, "给不存在的 mid 改名", err)
	wantSetBaseWritten(t, "给不存在的 mid 改名", e, 30003, "SetName", repository.ActUpdateUname, 1)

	row := e.st.base.get(30003)
	if row == nil {
		t.Fatal("给不存在的 mid 改名：没有补建行")
	}
	wantEQ(t, "补建行", "mid", row.Mid, int64(30003))
	wantEQ(t, "补建行", "name", row.Name, "幽灵用户")
	wantEQ(t, "补建行", "sex 取 DDL 默认 0", row.Sex, int64(0))
	wantEQ(t, "补建行", "face 取 DDL 默认空", row.Face, "")
	wantEQ(t, "补建行", "sign 取 DDL 默认空", row.Sign, "")
	wantEQ(t, "补建行", "rank 取 DDL 默认 5000", row.Rank, int64(model.DefaultRank))
	wantEQ(t, "补建行", "birthday 取 DDL 默认 -28800", row.Birthday, int64(model.DefaultTime))

	// 直接后果：这个不存在的用户现在能被 Member 查出来了。
	e.st.log.reset()
	ml := NewMemberLogic(context.Background(), e.svcCtx)
	m, err := ml.Member(&rpc.MemberMidReq{Mid: 30003})
	wantNoErr(t, "补建行后的 Member", err)
	wantEQ(t, "补建行后的 Member", "mid 不再是 0 哨兵", m.GetBaseInfo().GetMid(), int64(30003))
}

func TestSetNameDownstreamFailures(t *testing.T) {
	t.Run("单列写失败：错误传出、不发事件不失效缓存", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30004, Name: "旧昵称", Rank: 7000, Birthday: 1})
		boom := errors.New("Error 1406: Data too long for column 'name'")
		e.st.cache.warmJSON(keyBase(30004), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30004, Name: "旧昵称"}})
		e.st.base.failWith("SetName", boom)

		l := NewSetNameLogic(context.Background(), e.svcCtx)
		reply, err := l.SetName(&rpc.UpdateUnameReq{Mid: 30004, Name: "超长昵称"})
		wantErrIs(t, "单列写失败", err, boom)
		if reply != nil {
			t.Errorf("单列写失败：reply = %+v, want nil", reply)
		}
		ops := e.ops(0)
		wantOps(t, "单列写失败", ops, []string{"base.SetName:30004"})
		wantNoOpsWith(t, "单列写失败", ops, "outbox.Insert")
		wantNoOpsWith(t, "单列写失败", ops, "cache.Del")
		wantEQ(t, "单列写失败", "事务次数", e.st.conn.transactions, 1)
		wantEQ(t, "单列写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "单列写失败", "库存昵称保持旧值", e.st.base.get(30004).Name, "旧昵称")
		if _, ok := e.st.cache.jsons[keyBase(30004)]; !ok {
			t.Error("单列写失败：缓存被顺手删了（失败不该扩大影响面）")
		}
	})

	t.Run("Outbox 写失败：错误传出且不失效缓存（事件与写同事务，一起回滚）", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30005, Name: "旧昵称"})
		boom := errors.New("insert into member_outbox: boom")
		e.st.outbox.failWith("Insert", boom)

		l := NewSetNameLogic(context.Background(), e.svcCtx)
		reply, err := l.SetName(&rpc.UpdateUnameReq{Mid: 30005, Name: "新昵称"})
		wantErrIs(t, "Outbox 写失败", err, boom)
		if reply != nil {
			t.Errorf("Outbox 写失败：reply = %+v, want nil", reply)
		}
		ops := e.ops(0)
		wantOps(t, "Outbox 写失败", ops, []string{"base.SetName:30005", "outbox.Insert:user.profile.updated/30005/1"})
		wantNoOpsWith(t, "Outbox 写失败", ops, "cache.Del")
		wantEQ(t, "Outbox 写失败", "事务回滚", e.st.conn.rolledBack, 1)
		// 说明：内存替身无法像 InnoDB 那样撤销已执行的单列写，所以这里断言的是
		// 「缓存没被失效 + 错误如实传出」这两个可观察事实；真库的原子性由 TransactCtx 保证。
		wantEQ(t, "Outbox 写失败", "事件行数（回滚后不留行）", e.st.outbox.count(), 0)
	})

	t.Run("缓存失效失败被吞：接口仍返回成功但缓存留在旧值", func(t *testing.T) {
		// 行为哨兵（与 A 批「ReportWrite 的 error 被丢弃」同类）：
		// setBaseTx 对 delBaseCache 只 logx.Errorf，DB 已提交所以**不能**报错，
		// 但结果是昵称改成功了、account 侧与本地 bs_ 缓存在 TTL（3600s）内继续吐旧昵称。
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30006, Name: "旧昵称"})
		e.st.cache.warmJSON(keyBase(30006), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30006, Name: "缓存旧昵称"}})
		boom := errors.New("del bs_30006: redis down")
		e.st.cache.failWith("Del", boom)

		l := NewSetNameLogic(context.Background(), e.svcCtx)
		reply, err := l.SetName(&rpc.UpdateUnameReq{Mid: 30006, Name: "新昵称"})
		wantNoErr(t, "失效失败", err)
		if reply == nil {
			t.Fatal("失效失败：成功时 reply 必须非 nil")
		}
		wantOps(t, "失效失败", e.ops(0), []string{
			"base.SetName:30006",
			"outbox.Insert:user.profile.updated/30006/1",
			"cache.Del:bs_30006",
		})
		wantEQ(t, "失效失败", "库里已是新值", e.st.base.get(30006).Name, "新昵称")
		var stale baseCachePayload
		if !e.st.cache.jsonOf(keyBase(30006), &stale) {
			t.Fatal("失效失败：缓存竟然没了（说明 Del 的错误被替身当成成功执行）")
		}
		wantEQ(t, "失效失败", "缓存仍是旧昵称（脏读窗口 = TTL）", stale.Name, "缓存旧昵称")

		// 而且下一次读会直接命中脏缓存、不再回源：脏读是持续性的，不是单次。
		e.st.log.reset()
		bl := NewBaseLogic(context.Background(), e.svcCtx)
		base, err := bl.Base(&rpc.MemberMidReq{Mid: 30006})
		wantNoErr(t, "失效失败后读回", err)
		wantOps(t, "失效失败后读回", e.ops(0), []string{"cache.GetJSON:bs_30006"})
		wantEQ(t, "失效失败后读回", "吐出的还是旧昵称", base.GetName(), "缓存旧昵称")
	})
}

func TestSetNameDoesNotRecordCallerIP(t *testing.T) {
	// 契约里的 remote_ip 一路走到 logic 就被丢掉：既不进事件、也不进任何日志。
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 30007, Name: "旧昵称"})

	l := NewSetNameLogic(context.Background(), e.svcCtx)
	_, err := l.SetName(&rpc.UpdateUnameReq{Mid: 30007, Name: "新昵称", RemoteIp: "203.0.113.9"})
	wantNoErr(t, "昵称变更审计", err)
	wantSetBaseWritten(t, "昵称变更审计", e, 30007, "SetName", repository.ActUpdateUname, 1)

	_, pl := decodeOutboxView(t, "昵称变更审计", e.st.outbox.row(0))
	wantEQ(t, "昵称变更审计", "payload 键数量（只有 mid/action）", len(pl), 2)
	if _, ok := pl["ip"]; ok {
		t.Errorf("昵称变更审计：payload 里出现了 ip = %v，与本结论不符", pl["ip"])
	}
	if _, ok := pl["remote_ip"]; ok {
		t.Errorf("昵称变更审计：payload 里出现了 remote_ip = %v，与本结论不符", pl["remote_ip"])
	}
	if _, ok := pl["operator"]; ok {
		t.Errorf("昵称变更审计：payload 里出现了 operator = %v，与本结论不符", pl["operator"])
	}
	wantEQ(t, "昵称变更审计", "member_log 行数（昵称变更不写变更日志）", e.st.logs.count(), 0)
}

func TestSetNameRejectsNilRequestIsNotGuarded(t *testing.T) {
	// in 为 nil 时 logic 直接 panic（in.Mid 解引用）。钉成现状：RPC 层不会传 nil，
	// 但这不是「有守卫」，而是「没有防御」；真正的 nil 处理在 go-zero 的 unmarshal 侧。
	e := newEnv(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("SetName(nil) 期望 panic（现状），却返回了正常值")
		}
	}()
	_, _ = NewSetNameLogic(context.Background(), e.svcCtx).SetName((*rpc.UpdateUnameReq)(nil))
}
