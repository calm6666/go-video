package logic

// addpropertyreviewlogic_test.go 覆盖 AddPropertyReview（logic/addpropertyreviewlogic.go:31
// → repository/official.go:135）。
//
// 这条链路的价值结论有三类，逐条钉：
//  1. **old 与 new 的存储形态不对称**：头像的旧值经 facePath（util.go:71）归一成 URL path
//     （域名与 query 全丢），新值却是调用方传什么就存什么（含 `?auth_key=` 这类短期签名参数），
//     于是同一行的两列口径不同、且带令牌的 URL 永久留在 user_property_review.new。
//  2. **守卫的位置与形状**：extra 的 JSON 闸门发生在触库之前（一次依赖都不许碰），
//     property 白名单却坐在 base.FindOne **之后**；State/Property 由 int32 直接截成 int8，
//     所以 259 会被当成「昵称」、300 会被当成 44 档状态。
//  3. **失败口径分三段**：base 读失败与 Add 失败如实上抛；InMonitor 与 Archive 的失败被
//     logx.Errorf 吞掉、接口仍返回成功——归档失败就是「同一属性攒出两条待审」，
//     监控读失败就是「受监控用户的变更被记成非监控」，两者都不会让调用方重试。
//
// 重复提交按现状钉：第二次会把第一次的待审行归档（state 0→3）并追加新行，
// 表上没有唯一键（deploy/migrations/user-profile/000007_create_user_property_review.sql:51-53
// 只有 PRIMARY KEY(id) 与 idx_mid_property），所以行数只增不减；若提交的是非 0 状态，
// Archive 的 `WHERE state = 0` 根本命中不到，重复提交就是重复的待审行。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

const (
	// 明显的假值：老域名的带头像短期签名的 URL，query 段用来验「旧值有没有被归一」。
	reviewSeedFaceURL = "https://i0.hdslb.com/i/0/seedface.jpg?auth_key=1700000000-0-0-seedtoken"
	// 假值：提交方给的新头像，仍是带签名参数的完整 URL。
	reviewNewFaceURL = "https://cdn.example.com/i/1/newface.jpg?auth_key=1800000000-0-0-newtoken"
	// 假手机号：塞进签名列，用于看它会不会被任何一层脱敏（现状：原样落库）。
	reviewFakeTel = "13800000000"
)

func reviewReq(mid int64, property, state int32, newV, extra string) *rpc.AddPropertyReviewReq {
	return &rpc.AddPropertyReviewReq{Mid: mid, New: newV, State: state, Property: property, Extra: extra}
}

// reviewRows 回读全部审核行（值拷贝，插入顺序）。
func reviewRows(e *env) []*model.UserPropertyReview { return e.st.review.all() }

func TestAddPropertyReviewOldValuePerProperty(t *testing.T) {
	const mid = int64(41001)
	cases := []struct {
		label    string
		property int32
		wantOld  string
		newV     string
	}{
		{"头像：旧值只留 path", model.ReviewPropertyFace, "/i/0/seedface.jpg", reviewNewFaceURL},
		{"签名：旧值整列", model.ReviewPropertySign, "老签名", "新签名" + reviewFakeTel},
		{"昵称：旧值整列", model.ReviewPropertyName, "老昵称", "新昵称"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			e.st.base.put(&model.UserBase{Mid: mid, Name: "老昵称", Sign: "老签名", Face: reviewSeedFaceURL})
			e.st.monitor.put(&model.UserMonitor{Mid: mid, Operator: "运营小A"})

			_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
				AddPropertyReview(reviewReq(mid, tc.property, model.ReviewStateWait, tc.newV, ""))
			wantNoErr(t, tc.label, err)

			// 读旧值 → 查监控 → 归档同属性待审 → 追加新行（少一步、多一步都红）。
			wantOps(t, tc.label+"：调用序列", e.ops(0), []string{
				"base.FindOne:" + itoa(mid),
				"monitor.InMonitor:" + itoa(mid),
				"review.Archive:" + itoa(mid) + "/" + itoa(int64(tc.property)),
				"review.Add:" + itoa(mid) + "/" + itoa(int64(tc.property)) + "/1",
			})
			rows := reviewRows(e)
			wantEQ(t, tc.label, "行数", len(rows), 1)
			row := rows[0]
			wantEQ(t, tc.label, "mid", row.Mid, mid)
			wantEQ(t, tc.label, "property", int(row.Property), int(tc.property))
			wantEQ(t, tc.label, "state 原样取入参（服务端不强制待审核）", int(row.State), model.ReviewStateWait)
			wantEQ(t, tc.label, "old 来自当前资料", row.Old, tc.wantOld)
			wantEQ(t, tc.label, "new 原样入库", row.New, tc.newV)
			wantEQ(t, tc.label, "is_monitor 取实时名单", row.IsMonitor, true)
			wantEQ(t, tc.label, "空 extra 被补成 {}", row.Extra, "{}")
			// 归档实参是 `<mid>/<property>/<operator>/<remark>`：两个空串正是现状（不保留既有操作人）。
			wantEQ(t, tc.label, "Archive 的 operator/remark 都是空串",
				e.st.review.calls[0], itoa(mid)+"/"+itoa(int64(tc.property))+"//")
		})
	}
}

// TestAddPropertyReviewNewFaceKeepsSignedQuery 钉存储形态的不对称：
// old 列被 facePath 剥掉域名与 query，new 列把带 auth_key 的完整 URL 原样存下。
// 现状哨兵（README 已知缺口）：把 new 也归一成 path（或落库前剥掉 query）后本用例必须变红。
func TestAddPropertyReviewNewFaceKeepsSignedQuery(t *testing.T) {
	const mid = int64(41002)
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: mid, Face: reviewSeedFaceURL})

	_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
		AddPropertyReview(reviewReq(mid, model.ReviewPropertyFace, model.ReviewStateWait, reviewNewFaceURL, ""))
	wantNoErr(t, "头像两列口径", err)
	row := reviewRows(e)[0]
	wantEQ(t, "头像两列口径", "old 只剩 path", row.Old, "/i/0/seedface.jpg")
	wantEQ(t, "头像两列口径", "new 连签名参数一起入库", row.New, reviewNewFaceURL)
	if !strings.Contains(row.New, "auth_key=") {
		t.Errorf("现状哨兵：new 列应仍含短期签名参数，实际=%q", row.New)
	}
	if strings.Contains(row.Old, "auth_key=") {
		t.Errorf("old 列不该含签名参数（facePath 应剥掉 query），实际=%q", row.Old)
	}
}

// TestAddPropertyReviewStateAndPropertyTruncateToInt8 钉住 int32→int8 的静默截断：
// 逻辑层 addpropertyreviewlogic.go:43-44 直接 int8(in.State)/int8(in.Property)，
// 所以越界值不会被拒，而是**换成另一个合法值**——property=259 被判成「昵称」并顺利过白名单。
func TestAddPropertyReviewStateAndPropertyTruncateToInt8(t *testing.T) {
	cases := []struct {
		label      string
		property   int32
		state      int32
		wantProp   int8
		wantState  int8
		wantOldCol string // 截断后按哪个属性取旧值
	}{
		{"property=259 被当成昵称(3)", 259, model.ReviewStateWait, model.ReviewPropertyName, 0, "老昵称"},
		{"property=257 被当成头像(1)", 257, model.ReviewStateWait, model.ReviewPropertyFace, 0, "/i/0/seedface.jpg"},
		{"state=300 被当成 44", model.ReviewPropertySign, 300, model.ReviewPropertySign, 44, "老签名"},
		{"state=256 被当成待审核(0)", model.ReviewPropertySign, 256, model.ReviewPropertySign, 0, "老签名"},
		{"state=-1 原样交给驱动（列是 TINYINT UNSIGNED）", model.ReviewPropertySign, -1, model.ReviewPropertySign, -1, "老签名"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			const mid = int64(41003)
			e := newEnv(t)
			e.st.base.put(&model.UserBase{Mid: mid, Name: "老昵称", Sign: "老签名", Face: reviewSeedFaceURL})

			_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
				AddPropertyReview(reviewReq(mid, tc.property, tc.state, "新值", ""))
			wantNoErr(t, tc.label, err)
			row := reviewRows(e)[0]
			wantEQ(t, tc.label, "property 落库值", row.Property, tc.wantProp)
			wantEQ(t, tc.label, "state 落库值", row.State, tc.wantState)
			wantEQ(t, tc.label, "旧值按截断后的属性取列", row.Old, tc.wantOldCol)
		})
	}
}

// TestAddPropertyReviewUnknownPropertyGuardRunsAfterDBRead
// property 白名单在 official.go:141-156 的 switch default 里，位置在 base.FindOne 之后：
// 拒绝本身是对的（ErrRequestErr），但**守卫没有发生在触库之前**（与 extra 闸门不对称）。
func TestAddPropertyReviewUnknownPropertyGuardRunsAfterDBRead(t *testing.T) {
	for _, property := range []int32{0, 4, -1, 100} {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 41004, Name: "老昵称"})

		reply, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(41004, property, model.ReviewStateWait, "新值", ""))
		wantErrIs(t, "未知属性被拒", err, repository.ErrRequestErr)
		wantEQ(t, "未知属性被拒", "reply 为 nil", reply == nil, true)
		wantOps(t, "未知属性序列（已经打过一次库）", e.ops(0), []string{"base.FindOne:41004"})
		wantNoOpsWith(t, "未知属性不得归档", e.ops(0), "review.Archive")
		wantNoOpsWith(t, "未知属性不得追加", e.ops(0), "review.Add")
		wantNoOpsWith(t, "未知属性不得查监控", e.ops(0), "monitor.InMonitor")
		wantEQ(t, "未知属性被拒", "审核行数", len(reviewRows(e)), 0)
	}
}

// TestAddPropertyReviewExtraGuardIsBeforeAnyDependency
// extra 的 JSON 闸门在 logic 层（addpropertyreviewlogic.go:33-39），是这批里唯一
// 「拒绝时一次依赖都不碰」的守卫；错误是 encoding/json 的原生错误，
// **没有**被换成 ErrRequestErr（调用方拿到的是 "invalid character ..." 这类文本）。
func TestAddPropertyReviewExtraGuardIsBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		label     string
		extra     string
		wantErr   bool
		wantStore string // 放行时落库的 extra（现状：原样存，不重排、不补全）
	}{
		{"空串被补成 {}", "", false, "{}"},
		{"合法对象原样存", `{"from":"app"}`, false, `{"from":"app"}`},
		{"带空白的合法 JSON 不被规整", "{ \"a\" : 1 }", false, "{ \"a\" : 1 }"},
		{"null 过闸后原样入库", "null", false, "null"},
		{"数组被拒", `[1,2]`, true, ""},
		{"标量被拒", `"abc"`, true, ""},
		{"残缺 JSON 被拒", `{"a":`, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			const mid = int64(41005)
			e := newEnv(t)
			e.st.base.put(&model.UserBase{Mid: mid, Sign: "老签名"})

			_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
				AddPropertyReview(reviewReq(mid, model.ReviewPropertySign, model.ReviewStateWait, "新签名", tc.extra))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s：期望报错，实际放行", tc.label)
				}
				if errors.Is(err, repository.ErrRequestErr) {
					t.Errorf("%s：错误被换成了 ErrRequestErr，实际=%v", tc.label, err)
				}
				var syn *json.SyntaxError
				var typ *json.UnmarshalTypeError
				if !errors.As(err, &syn) && !errors.As(err, &typ) {
					t.Errorf("%s：错误种类 = %T(%v), want json.SyntaxError/UnmarshalTypeError", tc.label, err, err)
				}
				wantNoCall(t, tc.label, e.st, 0) // 关键：守卫先于一切依赖
				wantEQ(t, tc.label, "审核行数", len(reviewRows(e)), 0)
				return
			}
			wantNoErr(t, tc.label, err)
			wantEQ(t, tc.label, "落库 extra", reviewRows(e)[0].Extra, tc.wantStore)
		})
	}
}

// TestAddPropertyReviewMissingBaseAndNonPositiveMid
// base 行不存在时 old 取空串、照样建审核行；mid<=0 无守卫（与读侧缺口 1 同口径）。
func TestAddPropertyReviewMissingBaseAndNonPositiveMid(t *testing.T) {
	t.Run("资料行不存在：old 为空但仍建审核行", func(t *testing.T) {
		e := newEnv(t) // 不布 base
		_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(41006, model.ReviewPropertyFace, model.ReviewStateWait, "任意", ""))
		wantNoErr(t, "资料不存在", err)
		row := reviewRows(e)[0]
		wantEQ(t, "资料不存在", "old 为空串", row.Old, "")
		wantEQ(t, "资料不存在", "new 不做任何校验", row.New, "任意")
		wantEQ(t, "资料不存在", "is_monitor=false", row.IsMonitor, false)
	})

	t.Run("new 为空串也照收（无必填校验）", func(t *testing.T) {
		e := newEnv(t)
		_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(41007, model.ReviewPropertySign, model.ReviewStateWait, "", ""))
		wantNoErr(t, "空新值", err)
		wantEQ(t, "空新值", "new 列", reviewRows(e)[0].New, "")
	})

	t.Run("mid=0 无守卫：照样建给不存在的账号", func(t *testing.T) {
		e := newEnv(t)
		_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(0, model.ReviewPropertyName, model.ReviewStateWait, "新昵称", ""))
		wantNoErr(t, "mid=0", err)
		wantOps(t, "mid=0 序列", e.ops(0), []string{
			"base.FindOne:0", "monitor.InMonitor:0", "review.Archive:0/3", "review.Add:0/3/1",
		})
		wantEQ(t, "mid=0", "审核行 mid", reviewRows(e)[0].Mid, int64(0))
	})
}

// TestAddPropertyReviewRepeatSubmission 重复提交：新行永远追加，旧待审行被归档成 state=3。
// 归档同时把 operator/remark 写成空串（official.go:161 传的就是两个空串）。
func TestAddPropertyReviewRepeatSubmission(t *testing.T) {
	const mid = int64(41008)
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: mid, Name: "老昵称"})
	l := NewAddPropertyReviewLogic(context.Background(), e.svcCtx)

	_, err := l.AddPropertyReview(reviewReq(mid, model.ReviewPropertyName, model.ReviewStateWait, "第一次", ""))
	wantNoErr(t, "第一次提交", err)
	e.st.base.put(&model.UserBase{Mid: mid, Name: "第一次"}) // 旧值随资料推进
	_, err = l.AddPropertyReview(reviewReq(mid, model.ReviewPropertyName, model.ReviewStateWait, "第二次", ""))
	wantNoErr(t, "第二次提交", err)

	rows := reviewRows(e)
	wantEQ(t, "重复提交", "两行都在（表无唯一键，靠归档区分有效行）", len(rows), 2)
	wantEQ(t, "重复提交", "第 1 行被归档", int(rows[0].State), model.ReviewStateArchived)
	wantEQ(t, "重复提交", "第 1 行 old 是提交时的资料", rows[0].Old, "老昵称")
	wantEQ(t, "重复提交", "第 2 行 old 取到第一次的新值", rows[1].Old, "第一次")
	wantEQ(t, "重复提交", "第 2 行仍待审核", int(rows[1].State), model.ReviewStateWait)
	wantEQ(t, "重复提交", "有效待审只有 1 行", countPendingReviews(rows, mid, model.ReviewPropertyName), 1)
	wantOps(t, "重复提交序列", e.ops(0), []string{
		"base.FindOne:41008", "monitor.InMonitor:41008", "review.Archive:41008/3", "review.Add:41008/3/1",
		"base.FindOne:41008", "monitor.InMonitor:41008", "review.Archive:41008/3", "review.Add:41008/3/2",
	})
}

// TestAddPropertyReviewNonPendingStateNeverArchived 提交非 0 状态（如 10 自动审核中）时，
// Archive 的 `WHERE state = 0` 命不中刚建的那行，于是重复提交会**攒出多条同态记录**，
// 而接口一路返回成功——本用例把「第二条没被归档」钉成现状。
func TestAddPropertyReviewNonPendingStateNeverArchived(t *testing.T) {
	const mid = int64(41009)
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: mid, Sign: "老签名"})
	l := NewAddPropertyReviewLogic(context.Background(), e.svcCtx)

	for i := 0; i < 3; i++ {
		if _, err := l.AddPropertyReview(reviewReq(mid, model.ReviewPropertySign, model.ReviewStateQueuing, "新签名", "")); err != nil {
			t.Fatalf("第 %d 次提交：%v", i+1, err)
		}
	}
	rows := reviewRows(e)
	wantEQ(t, "非待审状态重复提交", "行数", len(rows), 3)
	wantEQ(t, "非待审状态重复提交", "state=10 的行数（全部留存，无人归档）",
		countWithState(rows, model.ReviewStateQueuing), 3)
	wantEQ(t, "非待审状态重复提交", "归档次数（每次都调了，但都命中 0 行）",
		e.st.log.countPrefix("review.Archive:"), 3)
}

func countPendingReviews(rows []*model.UserPropertyReview, mid int64, property int8) int {
	n := 0
	for _, r := range rows {
		if r.Mid == mid && r.Property == property && r.State == model.ReviewStateWait {
			n++
		}
	}
	return n
}

func countWithState(rows []*model.UserPropertyReview, state int8) int {
	n := 0
	for _, r := range rows {
		if r.State == state {
			n++
		}
	}
	return n
}

// TestAddPropertyReviewSwallowedFailures 三段失败口径逐条钉：
// base.FindOne 与 review.Add 的错误原样上抛；monitor.InMonitor 与 review.Archive 被吞。
func TestAddPropertyReviewSwallowedFailures(t *testing.T) {
	const mid = int64(41010)

	t.Run("base.FindOne 失败：原样上抛、不归档不追加", func(t *testing.T) {
		e := newEnv(t)
		boom := errors.New("boom: user_base 读不到")
		e.st.base.failWith("FindOne", boom)
		reply, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(mid, model.ReviewPropertyFace, model.ReviewStateWait, "x", ""))
		wantErrIs(t, "资料读失败", err, boom)
		wantEQ(t, "资料读失败", "reply 为 nil", reply == nil, true)
		wantOps(t, "资料读失败", e.ops(0), []string{"base.FindOne:41010"})
		wantEQ(t, "资料读失败", "审核行数", len(reviewRows(e)), 0)
	})

	t.Run("review.Add 失败：原样上抛（归档已经生效，半截状态）", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: mid, Sign: "老签名"})
		if _, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(mid, model.ReviewPropertySign, model.ReviewStateWait, "旧的新值", "")); err != nil {
			t.Fatalf("布第一行：%v", err)
		}
		e.st.log.reset()
		boom := errors.New("boom: user_property_review 写不进去")
		e.st.review.failWith("Add", boom)
		_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(mid, model.ReviewPropertySign, model.ReviewStateWait, "新的新值", ""))
		wantErrIs(t, "审核写失败", err, boom)
		wantOps(t, "审核写失败", e.ops(0), []string{
			"base.FindOne:41010", "monitor.InMonitor:41010", "review.Archive:41010/2", "review.Add:41010/2/2",
		})
		rows := reviewRows(e)
		wantEQ(t, "审核写失败", "只剩 1 行", len(rows), 1)
		wantEQ(t, "审核写失败", "已有的待审行已被归档（新值丢了、旧值也查不到待审）",
			int(rows[0].State), model.ReviewStateArchived)
	})

	t.Run("monitor.InMonitor 失败：被吞成「非监控」，接口仍成功", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: mid, Sign: "老签名"})
		e.st.monitor.put(&model.UserMonitor{Mid: mid}) // 明明在名单里
		e.st.monitor.failWith("InMonitor", errors.New("boom: user_monitor 读不到"))
		reply, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(mid, model.ReviewPropertySign, model.ReviewStateWait, "新签名", ""))
		wantNoErr(t, "监控读失败被吞", err)
		wantEQ(t, "监控读失败被吞", "reply 非空", reply != nil, true)
		row := reviewRows(e)[0]
		wantEQ(t, "监控读失败被吞", "is_monitor 记成 false（受监控用户被降级）", row.IsMonitor, false)
		wantOps(t, "监控读失败被吞", e.ops(0), []string{
			"base.FindOne:41010", "monitor.InMonitor:41010", "review.Archive:41010/2", "review.Add:41010/2/1",
		})
	})

	t.Run("review.Archive 失败：被吞，同属性攒出两条待审", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: mid, Sign: "老签名"})
		if _, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(mid, model.ReviewPropertySign, model.ReviewStateWait, "第一次", "")); err != nil {
			t.Fatalf("布第一行：%v", err)
		}
		e.st.log.reset()
		e.st.review.failWith("Archive", errors.New("boom: 归档语句超时"))
		_, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
			AddPropertyReview(reviewReq(mid, model.ReviewPropertySign, model.ReviewStateWait, "第二次", ""))
		wantNoErr(t, "归档失败被吞", err)
		rows := reviewRows(e)
		wantEQ(t, "归档失败被吞", "行数", len(rows), 2)
		wantEQ(t, "归档失败被吞", "两条都还是待审核",
			countPendingReviews(rows, mid, model.ReviewPropertySign), 2)
	})
}

// TestAddPropertyReviewSoftDeletedMonitorCountsAsFree
// is_monitor 走 InMonitor 的 `is_deleted = 0` 口径：已被移出名单的用户记 false。
// 这条与上一条合起来证明 is_monitor 不是「历史上曾被监控过」而是「此刻在名单里」。
func TestAddPropertyReviewSoftDeletedMonitorCountsAsFree(t *testing.T) {
	const mid = int64(41011)
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: mid, Name: "老昵称"})
	e.st.monitor.put(&model.UserMonitor{Mid: mid, IsDeleted: 1})

	if _, err := NewAddPropertyReviewLogic(context.Background(), e.svcCtx).
		AddPropertyReview(reviewReq(mid, model.ReviewPropertyName, model.ReviewStateWait, "新昵称", "")); err != nil {
		t.Fatalf("提交：%v", err)
	}
	wantEQ(t, "软删除名单", "is_monitor=false", reviewRows(e)[0].IsMonitor, false)
	wantEQ(t, "软删除名单", "查询次数", e.st.log.countPrefix("monitor.InMonitor:"), 1)
}
