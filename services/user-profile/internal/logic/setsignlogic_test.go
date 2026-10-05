package logic

// setsignlogic_test.go 覆盖 SetSign（改签名）。链路与 SetName 相同，
// 本文件钉 sign 特有口径：
//  1. 动作位 updatePersonInfo（与 sex/birthday 同组），不是 updateUname/updateFace；
//  2. 签名是最典型的富文本注入面：控制字符、HTML、Markdown、零宽字符、超长（DDL
//     `sign` VARCHAR(255)）全部**原样入库**，本服务不裁剪、不转义、不入审核；
//  3. 空串 = 清空签名（不是拒绝，也不是「保留旧值」）。
//
// 与 SetFace 一样，这里也顺带钉住「写侧与审核解耦」：AGENTS.md §5 规定审核结论
// owner 是 moderation-orchestrator，本服务不得自行判定违规；现状是本服务连「提交审核」
// 都没做（AddPropertyReview 只在网关侧被显式调用），所以 SetSign 之后签名直接对外可见。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

func TestSignGuardTableIsPinnedAsNoGuard(t *testing.T) {
	// TODO(缺陷) 期望被拒但现状全放行：mid<=0、超长、控制字符、富文本。
	long := strings.Repeat("签名", 200) // 400 个汉字，UTF-8 下 1200 字节，远超 VARCHAR(255)
	cases := []struct {
		label string
		mid   int64
		sign  string
	}{
		{"mid=0 照样写", 0, "零号签名"},
		{"mid 负数照样写", -9, "负号签名"},
		{"空串清空不拒绝", 30301, ""},
		{"纯空格不拒绝", 30302, "     "},
		{"HTML 片段不转义", 30303, `<script>alert(1)</script>`},
		{"Markdown / @ 提及不处理", 30304, "[链接](https://evil.example) @someone"},
		{"控制字符（NUL/换行/退格）原样入库", 30305, "a\x00b\nc\bd"},
		{"零宽与双向覆盖字符不剔除", 30306, "‮‎abc‍"},
		{"emoji 与变体选择符按字节存", 30307, "🎉️ 更新"},
		{"1200 字节超长不裁剪", 30308, long},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			_, err := NewSetSignLogic(context.Background(), e.svcCtx).SetSign(&rpc.UpdateSignReq{Mid: tc.mid, Sign: tc.sign, RemoteIp: "10.0.0.6"})
			wantNoErr(t, tc.label, err)
			wantSetBaseWritten(t, tc.label, e, tc.mid, "SetSign", repository.ActUpdatePersonInfo, 1)
			row := e.st.base.get(tc.mid)
			if row == nil {
				t.Fatalf("%s：没有落库", tc.label)
			}
			wantEQ(t, tc.label, "sign 逐字入库", row.Sign, tc.sign)
			wantEQ(t, tc.label, "sign 字节长度未裁剪", len(row.Sign), len(tc.sign))
		})
	}
}

func TestSignOnlyTouchesTheSignColumn(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{
		Mid: 30310, Name: "名字不动", Sex: 1, Face: "face 不动",
		Sign: "旧签名", Rank: 24680, Birthday: 800000000,
	})
	_, err := NewSetSignLogic(context.Background(), e.svcCtx).SetSign(&rpc.UpdateSignReq{Mid: 30310, Sign: "新签名"})
	wantNoErr(t, "改签名", err)
	wantSetBaseWritten(t, "改签名", e, 30310, "SetSign", repository.ActUpdatePersonInfo, 1)

	row := e.st.base.get(30310)
	wantEQ(t, "改签名", "sign", row.Sign, "新签名")
	wantEQ(t, "改签名", "name 保留", row.Name, "名字不动")
	wantEQ(t, "改签名", "sex 保留", row.Sex, int64(1))
	wantEQ(t, "改签名", "face 保留", row.Face, "face 不动")
	wantEQ(t, "改签名", "rank 保留", row.Rank, int64(24680))
	wantEQ(t, "改签名", "birthday 保留", row.Birthday, int64(800000000))

	e.st.log.reset()
	base, err := NewBaseLogic(context.Background(), e.svcCtx).Base(&rpc.MemberMidReq{Mid: 30310})
	wantNoErr(t, "改签名后读回", err)
	wantOps(t, "改签名后读回", e.ops(0), []string{
		"cache.GetJSON:bs_30310", "base.FindOne:30310", "cache.SetJSON:bs_30310/3600",
	})
	wantEQ(t, "改签名后读回", "sign", base.GetSign(), "新签名")
}

func TestSignCreatesMissingRow(t *testing.T) {
	e := newEnv(t)
	_, err := NewSetSignLogic(context.Background(), e.svcCtx).SetSign(&rpc.UpdateSignReq{Mid: 30320, Sign: "凭空有签名"})
	wantNoErr(t, "给不存在的 mid 改签名", err)
	wantSetBaseWritten(t, "给不存在的 mid 改签名", e, 30320, "SetSign", repository.ActUpdatePersonInfo, 1)
	row := e.st.base.get(30320)
	if row == nil {
		t.Fatal("给不存在的 mid 改签名：没有补建行")
	}
	wantEQ(t, "补建行", "sign", row.Sign, "凭空有签名")
	wantEQ(t, "补建行", "rank 取 DDL 默认", row.Rank, int64(model.DefaultRank))
	wantEQ(t, "补建行", "birthday 取 DDL 默认", row.Birthday, int64(model.DefaultTime))
	wantEQ(t, "补建行", "name 取 DDL 默认空", row.Name, "")
}

func TestSignDownstreamFailures(t *testing.T) {
	t.Run("sign 列写失败", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30330, Sign: "旧签名"})
		boom := errors.New("Error 1406: Data too long for column 'sign'")
		e.st.base.failWith("SetSign", boom)

		reply, err := NewSetSignLogic(context.Background(), e.svcCtx).SetSign(&rpc.UpdateSignReq{Mid: 30330, Sign: "超长超长"})
		wantErrIs(t, "sign 列写失败", err, boom)
		if reply != nil {
			t.Errorf("sign 列写失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "sign 列写失败", e.ops(0), []string{"base.SetSign:30330"})
		wantNoOpsWith(t, "sign 列写失败", e.ops(0), "outbox.Insert")
		wantEQ(t, "sign 列写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "sign 列写失败", "库存签名未变", e.st.base.get(30330).Sign, "旧签名")
	})

	t.Run("事件写失败", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30331, Sign: "旧签名"})
		boom := errors.New("insert into member_outbox: boom")
		e.st.outbox.failWith("Insert", boom)

		reply, err := NewSetSignLogic(context.Background(), e.svcCtx).SetSign(&rpc.UpdateSignReq{Mid: 30331, Sign: "新签名"})
		wantErrIs(t, "事件写失败", err, boom)
		if reply != nil {
			t.Errorf("事件写失败：reply = %+v, want nil", reply)
		}
		wantNoOpsWith(t, "事件写失败", e.ops(0), "cache.Del")
		wantEQ(t, "事件写失败", "缓存没被动过", e.st.log.countPrefix("cache."), 0)
	})

	t.Run("失效失败被吞成成功", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30332, Sign: "旧签名"})
		e.st.cache.warmJSON(keyBase(30332), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30332, Sign: "缓存旧签名"}})
		e.st.cache.failWith("Del", errors.New("del bs_30332: down"))

		_, err := NewSetSignLogic(context.Background(), e.svcCtx).SetSign(&rpc.UpdateSignReq{Mid: 30332, Sign: "新签名"})
		wantNoErr(t, "失效失败仍算成功", err)
		var stale baseCachePayload
		wantEQ(t, "失效失败仍算成功", "缓存仍在", e.st.cache.jsonOf(keyBase(30332), &stale), true)
		wantEQ(t, "失效失败仍算成功", "缓存签名是旧值", stale.Sign, "缓存旧签名")
	})
}

func TestSignWriteDoesNotInvalidateOtherUsersCache(t *testing.T) {
	// 失效范围只覆盖被改的那个 key：别人的 bs_ 与自己的 exp_/moral_ 都不许被顺手删。
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 30340, Sign: "旧"})
	e.st.base.put(&model.UserBase{Mid: 30341, Sign: "邻居的签名"})
	e.st.cache.warmJSON(keyBase(30340), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30340}})
	e.st.cache.warmJSON(keyBase(30341), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30341}})
	e.st.cache.warmInt(keyExp(30340), 100)
	e.st.cache.warmJSON(keyMoral(30340), model.UserMoral{Mid: 30340, Moral: 7000})

	_, err := NewSetSignLogic(context.Background(), e.svcCtx).SetSign(&rpc.UpdateSignReq{Mid: 30340, Sign: "新"})
	wantNoErr(t, "失效范围", err)
	wantSetBaseWritten(t, "失效范围", e, 30340, "SetSign", repository.ActUpdatePersonInfo, 1)
	if _, ok := e.st.cache.jsons[keyBase(30341)]; !ok {
		t.Error("失效范围：邻居的 bs_30341 被误删")
	}
	if _, ok := e.st.cache.strs[keyExp(30340)]; !ok {
		t.Error("失效范围：自己的 exp_30340 被误删")
	}
	if _, ok := e.st.cache.jsons[keyMoral(30340)]; !ok {
		t.Error("失效范围：自己的 moral_30340 被误删")
	}
}
