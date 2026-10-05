package logic

// setfacelogic_test.go 覆盖 SetFace（改头像）。
// 链路与 SetName 相同，本文件钉 face 特有三条：
//  1. 动作位是 updateFace（account 侧据此决定失效哪份缓存，写错就是错缓存）；
//  2. 头像 URL **一个字符都不校验**：非 https、data:、javascript:、带签名参数的
//     OSS 长期地址全部照写。AGENTS.md §6 要求客户端拿不到对象存储长期地址，
//     而本服务既不接受上传票据也不校验来源，这条防线完全在网关与前端；
//     属性审核（AddPropertyReview，C 批）才是唯一的事后把关，且只在用户被监控时启用。
//  3. 读侧没有默认头像兜底：model.URLNoFace 常量存在但全仓无引用，
//     face 为空时 Base 就返回空串（钉成现状 + TODO(缺陷)）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

func TestFaceGuardTableIsPinnedAsNoGuard(t *testing.T) {
	// TODO(缺陷) 期望被拒但现状全放行的输入：mid<=0、非 http(s) 协议、超长（DDL face VARCHAR(255)）、空串。
	cases := []struct {
		label string
		mid   int64
		face  string
	}{
		{"mid=0 照样写", 0, "https://cdn.example.com/zero.png"},
		{"http 明文不拒绝", 30201, "http://insecure.example.com/a.png"},
		{"javascript 伪协议不拒绝", 30202, "javascript:alert(1)"},
		{"data URI 不拒绝", 30203, "data:image/png;base64,AAAA"},
		{"裸字符串（根本不是 URL）不拒绝", 30204, "not-a-url"},
		{"带 OSS 签名参数的长期地址不拒绝", 30205, "https://bucket.oss-cn-hangzhou.aliyuncs.com/k?OSSAccessKeyId=AK&Signature=s"},
		{"空串是清空而不是拒绝", 30206, ""},
		{"正好 255 字符（DDL 上限）不拒绝", 30207, "https://cdn.example.com/" + strings.Repeat("p", 234)},
		{"超出 255 的 1000 字符也不裁剪", 30208, "https://cdn.example.com/" + strings.Repeat("q", 1000)},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			_, err := NewSetFaceLogic(context.Background(), e.svcCtx).SetFace(&rpc.UpdateFaceReq{Mid: tc.mid, Face: tc.face, RemoteIp: "10.0.0.7"})
			wantNoErr(t, tc.label, err)
			wantSetBaseWritten(t, tc.label, e, tc.mid, "SetFace", repository.ActUpdateFace, 1)
			row := e.st.base.get(tc.mid)
			if row == nil {
				t.Fatalf("%s：没有落库", tc.label)
			}
			wantEQ(t, tc.label, "face 逐字入库（不裁剪不改写）", row.Face, tc.face)
			wantEQ(t, tc.label, "face 长度原样", len(row.Face), len(tc.face))
		})
	}
}

func TestFaceOnlyTouchesTheFaceColumn(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{
		Mid: 30210, Name: "保留的名字", Sex: 2, Face: "old-face.png",
		Sign: "保留的签名", Rank: 13579, Birthday: 900000000,
	})
	_, err := NewSetFaceLogic(context.Background(), e.svcCtx).SetFace(&rpc.UpdateFaceReq{Mid: 30210, Face: "new-face.png"})
	wantNoErr(t, "改头像", err)
	wantSetBaseWritten(t, "改头像", e, 30210, "SetFace", repository.ActUpdateFace, 1)

	row := e.st.base.get(30210)
	wantEQ(t, "改头像", "face", row.Face, "new-face.png")
	wantEQ(t, "改头像", "name 保留", row.Name, "保留的名字")
	wantEQ(t, "改头像", "sex 保留", row.Sex, int64(2))
	wantEQ(t, "改头像", "sign 保留", row.Sign, "保留的签名")
	wantEQ(t, "改头像", "rank 保留", row.Rank, int64(13579))
	wantEQ(t, "改头像", "birthday 保留", row.Birthday, int64(900000000))
}

func TestFaceClearedAndReadBackHasNoDefaultAvatar(t *testing.T) {
	// TODO(缺陷) model.URLNoFace 定义了「未设置头像时的默认地址」，但全仓无引用：
	// 清空头像后 Base 返回 face=""，客户端必须自己兜底，否则个人空间会出现空头像。
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 30220, Name: "n", Face: "to-be-cleared.png", Rank: 5000, Birthday: 1})

	_, err := NewSetFaceLogic(context.Background(), e.svcCtx).SetFace(&rpc.UpdateFaceReq{Mid: 30220, Face: ""})
	wantNoErr(t, "清空头像", err)
	wantSetBaseWritten(t, "清空头像", e, 30220, "SetFace", repository.ActUpdateFace, 1)

	e.st.log.reset()
	base, err := NewBaseLogic(context.Background(), e.svcCtx).Base(&rpc.MemberMidReq{Mid: 30220})
	wantNoErr(t, "清空头像后读回", err)
	wantOps(t, "清空头像后读回", e.ops(0), []string{
		"cache.GetJSON:bs_30220", "base.FindOne:30220", "cache.SetJSON:bs_30220/3600",
	})
	wantEQ(t, "清空头像后读回", "face 是空串而不是默认头像", base.GetFace(), "")
	wantEQ(t, "清空头像后读回", "回填进缓存的同样是空串", func() string {
		var p baseCachePayload
		if !e.st.cache.jsonOf(keyBase(30220), &p) {
			t.Fatal("清空头像后读回：没有回填缓存")
		}
		return p.Face
	}(), "")
}

func TestFaceDownstreamFailures(t *testing.T) {
	t.Run("face 列写失败", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30230, Face: "old.png"})
		boom := errors.New("Error 1406: Data too long for column 'face'")
		e.st.base.failWith("SetFace", boom)

		reply, err := NewSetFaceLogic(context.Background(), e.svcCtx).SetFace(&rpc.UpdateFaceReq{Mid: 30230, Face: "超长"})
		wantErrIs(t, "face 列写失败", err, boom)
		if reply != nil {
			t.Errorf("face 列写失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "face 列写失败", e.ops(0), []string{"base.SetFace:30230"})
		wantEQ(t, "face 列写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "face 列写失败", "事件行数", e.st.outbox.count(), 0)
		wantEQ(t, "face 列写失败", "库存 face 未变", e.st.base.get(30230).Face, "old.png")
	})

	t.Run("事件写失败不失效缓存", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30231, Face: "old.png"})
		e.st.cache.warmJSON(keyBase(30231), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30231, Face: "old.png"}})
		boom := errors.New("insert into member_outbox: boom")
		e.st.outbox.failWith("Insert", boom)

		reply, err := NewSetFaceLogic(context.Background(), e.svcCtx).SetFace(&rpc.UpdateFaceReq{Mid: 30231, Face: "new.png"})
		wantErrIs(t, "事件写失败", err, boom)
		if reply != nil {
			t.Errorf("事件写失败：reply = %+v, want nil", reply)
		}
		wantNoOpsWith(t, "事件写失败", e.ops(0), "cache.Del")
		wantEQ(t, "事件写失败", "缓存原样保留", e.st.log.countPrefix("cache.Del:"), 0)
	})

	t.Run("失效失败被吞成成功", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 30232, Face: "old.png"})
		e.st.cache.warmJSON(keyBase(30232), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30232, Face: "缓存旧头像"}})
		e.st.cache.failWith("Del", errors.New("del bs_30232: down"))

		_, err := NewSetFaceLogic(context.Background(), e.svcCtx).SetFace(&rpc.UpdateFaceReq{Mid: 30232, Face: "new.png"})
		wantNoErr(t, "失效失败仍算成功", err)
		var stale baseCachePayload
		wantEQ(t, "失效失败仍算成功", "缓存还在", e.st.cache.jsonOf(keyBase(30232), &stale), true)
		wantEQ(t, "失效失败仍算成功", "缓存里仍是旧头像", stale.Face, "缓存旧头像")
		wantEQ(t, "失效失败仍算成功", "库里已是新头像", e.st.base.get(30232).Face, "new.png")
	})
}

func TestFaceEmitsExactlyOneUpdateFaceEvent(t *testing.T) {
	// 两次改头像 = 两条事件、两个不同的 event_id（消费者按 event_id 幂等）。
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 30240, Face: "a.png"})
	l := NewSetFaceLogic(context.Background(), e.svcCtx)

	_, err := l.SetFace(&rpc.UpdateFaceReq{Mid: 30240, Face: "b.png"})
	wantNoErr(t, "第一次改头像", err)
	firstID := e.st.outbox.row(0).EventID
	e.st.log.reset()
	_, err = l.SetFace(&rpc.UpdateFaceReq{Mid: 30240, Face: "c.png"})
	wantNoErr(t, "第二次改头像", err)
	wantOps(t, "第二次改头像", e.ops(0), []string{
		"base.SetFace:30240",
		"outbox.Insert:user.profile.updated/30240/2",
		"cache.Del:bs_30240",
	})
	wantEQ(t, "第二次改头像", "事件行数", e.st.outbox.count(), 2)
	wantEQ(t, "第二次改头像", "第二次事务数（每次写一个事务）", e.st.conn.transactions, 2)
	second := e.st.outbox.row(1)
	if second.EventID == firstID {
		t.Errorf("第二次改头像：两条事件 event_id 相同（%s），消费者会去重掉第二次失效", second.EventID)
	}
	wantEQ(t, "第二次改头像", "第二条事件 action", func() string {
		_, pl := decodeOutboxView(t, "第二次改头像", second)
		return plStr(t, "第二次改头像", pl, "action")
	}(), repository.ActUpdateFace)
}
