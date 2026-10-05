package logic

// setsexlogic_test.go 覆盖 SetSex（改性别）。链路与 SetName 同一条（setBaseTx：
// 事务内单列 UPSERT + user.profile.updated 事件 → 提交后失效 bs_<mid>），
// 本文件只钉 sex 特有的口径：
//  1. 动作位是 updatePersonInfo（不是 updateUname）——account 侧按 action 决定失效哪份缓存；
//  2. sex 的取值域只有 DDL 注释里的 {0,1,2}，**本层完全不校验**：99、-1 照写；
//     注意列是 TINYINT UNSIGNED，-1 在真库是 1264 越界，替身按饱和存下，
//     所以这里的断言是「值被原样交给 SQL」而不是「值合法」；
//  3. 0 是「保密」这个**合法结论**，不是「未传」——所以 0 必须真的写进去，
//     不能因为 Go 零值而被跳过（跳过会让用户永远改不回保密）。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

func TestSexGuardTableIsPinnedAsNoGuard(t *testing.T) {
	// TODO(缺陷) 期望被拒但现状全放行的输入：mid<=0、sex 不在 {0,1,2}。
	cases := []struct {
		label string
		mid   int64
		sex   int64
	}{
		{"mid=0 照样写", 0, 1},
		{"mid 负数照样写", -5, 2},
		{"sex=0（保密）是结论不是缺省", 30101, 0},
		{"sex=1", 30102, 1},
		{"sex=2", 30103, 2},
		{"sex=3 越界枚举不拒绝", 30104, 3},
		{"sex=99 不拒绝", 30105, 99},
		{"sex 负数不拒绝（真库由 UNSIGNED 列报错）", 30106, -1},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			if tc.mid > 0 {
				e.st.base.put(&model.UserBase{Mid: tc.mid, Name: "名字", Sex: 2, Rank: 5000, Birthday: 100})
			}
			l := NewSetSexLogic(context.Background(), e.svcCtx)
			reply, err := l.SetSex(&rpc.UpdateSexReq{Mid: tc.mid, Sex: tc.sex, RemoteIp: "10.0.0.8"})
			wantNoErr(t, tc.label, err)
			if reply == nil {
				t.Fatal(tc.label + "：成功时 reply 必须非 nil")
			}
			wantSetBaseWritten(t, tc.label, e, tc.mid, "SetSex", repository.ActUpdatePersonInfo, 1)
			row := e.st.base.get(tc.mid)
			if row == nil {
				t.Fatalf("%s：没有落库", tc.label)
			}
			wantEQ(t, tc.label, "交给 SQL 的 sex 原值", row.Sex, tc.sex)
			if tc.mid > 0 {
				wantEQ(t, tc.label, "name 不许被顺手改", row.Name, "名字")
			}
		})
	}
}

func TestSexZeroOverridesPreviousValue(t *testing.T) {
	// 最容易写错的一条：如果实现按「sex != 0 才写」做条件跳过，用户改不回「保密」。
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 30110, Name: "男青年", Sex: 1, Face: "face.png", Sign: "s", Rank: 7000, Birthday: 555})

	l := NewSetSexLogic(context.Background(), e.svcCtx)
	_, err := l.SetSex(&rpc.UpdateSexReq{Mid: 30110, Sex: 0})
	wantNoErr(t, "改回保密", err)
	wantSetBaseWritten(t, "改回保密", e, 30110, "SetSex", repository.ActUpdatePersonInfo, 1)

	row := e.st.base.get(30110)
	wantEQ(t, "改回保密", "sex 从 1 变 0", row.Sex, int64(0))
	wantEQ(t, "改回保密", "face 保留", row.Face, "face.png")
	wantEQ(t, "改回保密", "sign 保留", row.Sign, "s")
	wantEQ(t, "改回保密", "rank 保留", row.Rank, int64(7000))
	wantEQ(t, "改回保密", "birthday 保留", row.Birthday, int64(555))
	wantEQ(t, "改回保密", "name 保留", row.Name, "男青年")

	e.st.log.reset()
	base, err := NewBaseLogic(context.Background(), e.svcCtx).Base(&rpc.MemberMidReq{Mid: 30110})
	wantNoErr(t, "改回保密后读回", err)
	wantOps(t, "改回保密后读回", e.ops(0), []string{
		"cache.GetJSON:bs_30110", "base.FindOne:30110", "cache.SetJSON:bs_30110/3600",
	})
	wantEQ(t, "改回保密后读回", "sex 读出 0", base.GetSex(), int64(0))
	wantEQ(t, "改回保密后读回", "name 读出旧值（未受影响）", base.GetName(), "男青年")
}

func TestSexCreatesMissingRowWithDefaults(t *testing.T) {
	e := newEnv(t)
	l := NewSetSexLogic(context.Background(), e.svcCtx)
	_, err := l.SetSex(&rpc.UpdateSexReq{Mid: 30120, Sex: 2})
	wantNoErr(t, "给不存在的 mid 改性别", err)
	wantSetBaseWritten(t, "给不存在的 mid 改性别", e, 30120, "SetSex", repository.ActUpdatePersonInfo, 1)
	row := e.st.base.get(30120)
	if row == nil {
		t.Fatal("给不存在的 mid 改性别：没有补建行")
	}
	wantEQ(t, "补建行", "sex", row.Sex, int64(2))
	wantEQ(t, "补建行", "name 默认空", row.Name, "")
	wantEQ(t, "补建行", "rank 默认 5000", row.Rank, int64(model.DefaultRank))
	wantEQ(t, "补建行", "birthday 默认 -28800", row.Birthday, int64(model.DefaultTime))
}

func TestSexDownstreamFailures(t *testing.T) {
	t.Run("sex 列写失败", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30130, Sex: 1})
		boom := errors.New("Error 1264: Out of range value for column 'sex'")
		e.st.base.failWith("SetSex", boom)

		reply, err := NewSetSexLogic(context.Background(), e.svcCtx).SetSex(&rpc.UpdateSexReq{Mid: 30130, Sex: -1})
		wantErrIs(t, "sex 列写失败", err, boom)
		if reply != nil {
			t.Errorf("sex 列写失败：reply = %+v, want nil", reply)
		}
		ops := e.ops(0)
		wantOps(t, "sex 列写失败", ops, []string{"base.SetSex:30130"})
		wantNoOpsWith(t, "sex 列写失败", ops, "outbox.Insert")
		wantNoOpsWith(t, "sex 列写失败", ops, "cache.Del")
		wantEQ(t, "sex 列写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "sex 列写失败", "库存 sex 未变", e.st.base.get(30130).Sex, int64(1))
	})

	t.Run("事件写失败连带事务回滚且不失效缓存", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30131, Sex: 1})
		boom := errors.New("insert into member_outbox: boom")
		e.st.outbox.failWith("Insert", boom)

		reply, err := NewSetSexLogic(context.Background(), e.svcCtx).SetSex(&rpc.UpdateSexReq{Mid: 30131, Sex: 2})
		wantErrIs(t, "事件写失败", err, boom)
		if reply != nil {
			t.Errorf("事件写失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "事件写失败", e.ops(0), []string{"base.SetSex:30131", "outbox.Insert:user.profile.updated/30131/1"})
		wantEQ(t, "事件写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "事件写失败", "事件行数", e.st.outbox.count(), 0)
	})

	t.Run("缓存失效失败被吞成成功", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30132, Sex: 1})
		e.st.cache.warmJSON(keyBase(30132), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30132, Sex: 1}})
		e.st.cache.failWith("Del", errors.New("del bs_30132: redis down"))

		reply, err := NewSetSexLogic(context.Background(), e.svcCtx).SetSex(&rpc.UpdateSexReq{Mid: 30132, Sex: 2})
		wantNoErr(t, "失效失败仍算成功", err)
		if reply == nil {
			t.Fatal("失效失败仍算成功：reply 不应为 nil")
		}
		wantEQ(t, "失效失败仍算成功", "库里已是 2", e.st.base.get(30132).Sex, int64(2))
		var stale baseCachePayload
		wantEQ(t, "失效失败仍算成功", "缓存仍是 1（脏读窗口 = TTL 3600s）", e.st.cache.jsonOf(keyBase(30132), &stale), true)
		wantEQ(t, "失效失败仍算成功", "缓存 sex", stale.Sex, int64(1))
	})
}

func TestSexUsesPersonInfoActionAndCarriesNoAuditFields(t *testing.T) {
	e := newEnv(t)
	l := NewSetSexLogic(context.Background(), e.svcCtx)
	_, err := l.SetSex(&rpc.UpdateSexReq{Mid: 30140, Sex: 2, RemoteIp: "203.0.113.10"})
	wantNoErr(t, "性别变更事件", err)
	wantSetBaseWritten(t, "性别变更事件", e, 30140, "SetSex", repository.ActUpdatePersonInfo, 1)
	_, pl := decodeOutboxView(t, "性别变更事件", e.st.outbox.row(0))
	wantEQ(t, "性别变更事件", "payload 键数量（只有 mid/action）", len(pl), 2)
	if _, ok := pl["sex"]; ok {
		t.Errorf("性别变更事件：事件里携带了 sex = %v，与本结论不符（事件只是失效信号，不传资料）", pl["sex"])
	}
	if _, ok := pl["ip"]; ok {
		t.Errorf("性别变更事件：事件里携带了 ip = %v，与本结论不符（remote_ip 被丢弃）", pl["ip"])
	}
	wantEQ(t, "性别变更事件", "不写 member_log", e.st.logs.count(), 0)
}
