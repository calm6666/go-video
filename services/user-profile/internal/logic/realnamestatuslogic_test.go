package logic

// realnamestatuslogic_test.go 覆盖 RealnameStatus 与 RealnameApplyStatus
// （repository/realname.go:133 与 :145）——实名簇里最轻的两个读，
// 但两者坐在**同一个缓存条目**（keyRealname）上，所以它们的用例共同回答三件事：
//   1. 状态口径：RealnameStatus 把「审核中/驳回/未申请」三态压成 0，
//      只有 Pass 才是 1；RealnameApplyStatus 才给得出四态与驳回原因；
//   2. 共享缓存：任一方法的回填都会被另一个方法命中（缓存里躺的是整个载荷，
//      含 real_card 明文与 reason），所以「谁先读谁写缓存」必须钉死；
//   3. 降级面：缓存故障只多吃一次 SELECT，读库失败才是整体报错。
//
// 用例里所有序列都从 0 起（newEnv/newStore 已把构造期轨迹清空）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// putStatusRow 布一条 realname_info 存量行（不带证件号密文，纯状态用例用）。
func putStatusRow(e *env, mid int64, status int8, reason string) {
	e.st.realname.put(&model.RealnameInfo{
		Mid: mid, Realname: "张三", Channel: model.RealnameChannelMain,
		Country: model.RealnameCountryChina, CardType: model.RealnameCardTypeIdentity,
		Status: status, Reason: reason, CardMD5: "md5_" + itoa(mid),
	})
}

func TestRealnameStatusMapsOnlyPassToTrue(t *testing.T) {
	// 流程状态 → 实名状态的真值表。审核中(0)/驳回(2)/库里存着的未申请(3)
	// 全部落到 0——只测 1/0 两点会让「把 status==0 当成通过」这类写反一路绿。
	cases := []struct {
		name   string
		status int8
		want   int32
	}{
		{"审核中算未通过", model.RealnameApplyStatusPending, model.RealnameStatusFalse},
		{"通过算通过", model.RealnameApplyStatusPass, model.RealnameStatusTrue},
		{"驳回算未通过", model.RealnameApplyStatusBack, model.RealnameStatusFalse},
		{"库里存成未申请也算未通过", model.RealnameApplyStatusNone, model.RealnameStatusFalse},
		{"越界状态值一律算未通过", 9, model.RealnameStatusFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const mid = int64(72001)
			e := newEnv(t)
			putStatusRow(e, mid, tc.status, "")

			l := NewRealnameStatusLogic(context.Background(), e.svcCtx)
			reply, err := l.RealnameStatus(&rpc.MemberMidReq{Mid: mid})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "realname_status", reply.GetRealnameStatus(), int32(tc.want))
			wantOps(t, tc.name, e.ops(0), []string{
				"cache.GetJSON:realname_info_72001",
				"realname.FindOne:72001",
				"cache.SetJSON:realname_info_72001/3600",
			})
		})
	}
}

func TestRealnameStatusMissingRowBackfillsNotAppliedSentinel(t *testing.T) {
	const mid = int64(72002)
	e := newEnv(t)

	l := NewRealnameStatusLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "无实名记录", err)
	wantEQ(t, "无实名记录", "realname_status", reply.GetRealnameStatus(), int32(model.RealnameStatusFalse))
	wantOps(t, "无实名记录序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_72002",
		"realname.FindOne:72002",
		"cache.SetJSON:realname_info_72002/3600",
	})
	// 库里没有行也要回填 status=3 的哨兵：此后 1 小时内的状态查询全不打库，
	// 实名申请成功后必须靠 delRealnameCache 才能翻案（见 realnameapplylogic_test.go）。
	cached, ok := cachedRealname(t, e, mid)
	wantEQ(t, "无实名记录", "哨兵已回填", ok, true)
	wantEQ(t, "无实名记录", "哨兵 status=未申请", int(cached.Status), model.RealnameApplyStatusNone)
	wantEQ(t, "无实名记录", "哨兵 mid", cached.Mid, mid)
	wantEQ(t, "无实名记录", "哨兵 realname 为空", cached.Realname, "")
}

func TestRealnameStatusSecondCallWithinTTLNeverReadsDB(t *testing.T) {
	const mid = int64(72003)
	e := newEnv(t)
	putStatusRow(e, mid, model.RealnameApplyStatusPass, "")
	l := NewRealnameStatusLogic(context.Background(), e.svcCtx)

	if _, err := l.RealnameStatus(&rpc.MemberMidReq{Mid: mid}); err != nil {
		t.Fatalf("第一次查询：%v", err)
	}
	if _, err := l.RealnameStatus(&rpc.MemberMidReq{Mid: mid}); err != nil {
		t.Fatalf("第二次查询：%v", err)
	}
	wantOps(t, "两次查询序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_72003",
		"realname.FindOne:72003",
		"cache.SetJSON:realname_info_72003/3600",
		"cache.GetJSON:realname_info_72003",
	})
	wantEQ(t, "两次查询", "SELECT 次数", e.st.log.countPrefix("realname.FindOne:"), 1)
}

func TestRealnameStatusTrustsStaleCacheOverDBRow(t *testing.T) {
	// 缓存里写着通过、库里已经改成驳回：返回值仍是「通过」。
	// 这不是 bug 复现而是**失效契约**——任何改 realname_info 的写路径都必须删缓存，
	// 否则最长 1 小时对外口径不变（RealnameApply 只删了 cap_code/info 两张键，
	// 见 realnameapplylogic_test.go 里的 cache.Del 序列）。
	const mid = int64(72004)
	e := newEnv(t)
	e.st.cache.warmJSON(keyRealname(mid), realnameCacheView{
		Cached: true, Mid: mid, Status: model.RealnameApplyStatusPass,
	})
	putStatusRow(e, mid, model.RealnameApplyStatusBack, "证件照片模糊")

	l := NewRealnameStatusLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "陈旧缓存", err)
	wantEQ(t, "陈旧缓存", "仍回答通过", reply.GetRealnameStatus(), int32(model.RealnameStatusTrue))
	wantOps(t, "陈旧缓存序列", e.ops(0), []string{"cache.GetJSON:realname_info_72004"})
	wantNoOpsWith(t, "陈旧缓存", e.ops(0), "realname.FindOne:")
}

func TestRealnameStatusStatusOnlyCallStillPutsPlaintextCardIntoCache(t *testing.T) {
	// 行为哨兵（README 已知缺口）：调用方只要一个 0/1，但回填的是**整个载荷**，
	// 含 Pass 行被顺手解密出来的 real_card（明文证件号）。
	// 也就是说：每一次缓存 miss 的状态查询都会把明文写进 Redis，TTL 1 小时。
	// 修好后本段必须改成 wantNoPII(real_card 不出现在 SetJSON 历史里)。
	const mid = int64(72005)
	e := newEnv(t)
	e.st.realname.put(&model.RealnameInfo{
		Mid: mid, Realname: "张三", Status: model.RealnameApplyStatusPass,
		CardType: model.RealnameCardTypeIdentity, Country: model.RealnameCountryChina,
		Card: encryptCard(t, cardMaleAdult), CardMD5: "md5_status",
	})

	l := NewRealnameStatusLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "状态查询", err)
	wantEQ(t, "状态查询", "realname_status", reply.GetRealnameStatus(), int32(model.RealnameStatusTrue))

	cached, ok := cachedRealname(t, e, mid)
	wantEQ(t, "状态查询", "回填发生", ok, true)
	wantEQ(t, "状态查询", "载荷里的 card 是密文", cached.Card, e.st.realname.get(mid).Card)
	wantEQ(t, "状态查询", "现状：载荷里另附明文 real_card", cached.RealCard, cardMaleAdult)
	// 不看终态、看写历史：证明明文确实**进过** Redis 字节流（而不是只在内存结构里）。
	if blob := e.st.cache.blobOf(keyRealname(mid)); !strings.Contains(blob, cardMaleAdult) {
		t.Fatalf("期望现状是明文证件号被写进缓存载荷（缺陷），实际写入=%q", blob)
	}
	// 返回值本身不含明文：调用方只拿到一个 0/1。
	wantNoPII(t, "状态查询", []string{cardMaleAdult, "张三"}, map[string]string{"reply": reply.String()})
}

func TestRealnameStatusPropagatesReadErrorWithoutBackfill(t *testing.T) {
	const mid = int64(72006)
	e := newEnv(t)
	boom := errors.New("boom: realname_info 读不到")
	e.st.realname.failWith("FindOne", boom)

	l := NewRealnameStatusLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStatus(&rpc.MemberMidReq{Mid: mid})
	wantErrIs(t, "读库失败", err, boom)
	wantEQ(t, "读库失败", "reply 为 nil", reply == nil, true)
	wantOps(t, "读库失败序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_72006",
		"realname.FindOne:72006",
	})
	wantNoOpsWith(t, "读库失败", e.ops(0), "cache.SetJSON")
}

func TestRealnameStatusCacheFaultDegradesSilentlyWithoutBackfill(t *testing.T) {
	// 与 GetBit 口径相反：GetJSON 的故障被吞成 miss（realname.go:86），
	// 状态照常返回，但 cacheOK=false 时不回填——故障期每次查询都打库。
	const mid = int64(72007)
	e := newEnv(t)
	e.st.cache.failWith("GetJSON", errors.New("boom: redis 不可用"))
	putStatusRow(e, mid, model.RealnameApplyStatusPass, "")

	l := NewRealnameStatusLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "缓存故障", err)
	wantEQ(t, "缓存故障", "仍回答通过", reply.GetRealnameStatus(), int32(model.RealnameStatusTrue))
	wantOps(t, "缓存故障序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_72007",
		"realname.FindOne:72007",
	})
	wantNoOpsWith(t, "缓存故障", e.ops(0), "cache.SetJSON")
}

func TestRealnameApplyStatusReturnsFlowStatusAndReason(t *testing.T) {
	// 与 RealnameStatus 的分野在这里：四态原样端出，且驳回原因只有这条路能拿到。
	cases := []struct {
		name       string
		status     int8
		reason     string
		wantStatus int32
		wantRemark string
	}{
		{"审核中", model.RealnameApplyStatusPending, "", 0, ""},
		{"通过", model.RealnameApplyStatusPass, "", 1, ""},
		{"驳回带原因", model.RealnameApplyStatusBack, "证件照片模糊", 2, "证件照片模糊"},
		{"库里存成未申请", model.RealnameApplyStatusNone, "", 3, ""},
		{"通过但残留旧原因", model.RealnameApplyStatusPass, "上一轮驳回原因", 1, "上一轮驳回原因"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const mid = int64(72008)
			e := newEnv(t)
			putStatusRow(e, mid, tc.status, tc.reason)

			l := NewRealnameApplyStatusLogic(context.Background(), e.svcCtx)
			reply, err := l.RealnameApplyStatus(&rpc.MemberMidReq{Mid: mid})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "status", reply.GetStatus(), tc.wantStatus)
			wantEQ(t, tc.name, "remark", reply.GetRemark(), tc.wantRemark)
			wantOps(t, tc.name, e.ops(0), []string{
				"cache.GetJSON:realname_info_72008",
				"realname.FindOne:72008",
				"cache.SetJSON:realname_info_72008/3600",
			})
		})
	}
}

func TestRealnameApplyStatusReasonRoundTripsThroughCache(t *testing.T) {
	// reason 必须在缓存载荷里活下来：否则第二次查询会「原因丢失」，
	// 前端刷新一次驳回理由就变空。
	const mid = int64(72009)
	e := newEnv(t)
	putStatusRow(e, mid, model.RealnameApplyStatusBack, "身份证号码与姓名不一致")
	l := NewRealnameApplyStatusLogic(context.Background(), e.svcCtx)

	first, err := l.RealnameApplyStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "第一次", err)
	second, err := l.RealnameApplyStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "第二次", err)
	wantEQ(t, "缓存回读", "status 一致", second.GetStatus(), first.GetStatus())
	wantEQ(t, "缓存回读", "remark 一致", second.GetRemark(), "身份证号码与姓名不一致")
	wantOps(t, "缓存回读序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_72009",
		"realname.FindOne:72009",
		"cache.SetJSON:realname_info_72009/3600",
		"cache.GetJSON:realname_info_72009",
	})
	cached, ok := cachedRealname(t, e, mid)
	wantEQ(t, "缓存回读", "回填发生", ok, true)
	wantEQ(t, "缓存回读", "载荷里的 reason", cached.Reason, "身份证号码与姓名不一致")
	wantEQ(t, "缓存回读", "载荷里的 status", int(cached.Status), model.RealnameApplyStatusBack)
}

func TestRealnameApplyStatusPassesThroughUnknownStatusCode(t *testing.T) {
	// 读侧没有状态白名单：库里躺着一个不认识的码（上游/人工写入）就原样吐给调用方。
	// 钉住现状，是为了让「加校验」这个决定将来由测试逼出来而不是靠记忆。
	const mid = int64(72010)
	e := newEnv(t)
	putStatusRow(e, mid, 9, "未知状态")

	l := NewRealnameApplyStatusLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameApplyStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "越界状态", err)
	wantEQ(t, "越界状态", "status 原样透传", reply.GetStatus(), int32(9))
	wantEQ(t, "越界状态", "remark 原样透传", reply.GetRemark(), "未知状态")
}

func TestRealnameApplyStatusCacheHitSkipsDBAndTrustsPayload(t *testing.T) {
	const mid = int64(72011)
	e := newEnv(t)
	e.st.cache.warmJSON(keyRealname(mid), realnameCacheView{
		Cached: true, Mid: mid, Status: model.RealnameApplyStatusPending, Reason: "缓存里的原因",
	})
	putStatusRow(e, mid, model.RealnameApplyStatusPass, "")

	l := NewRealnameApplyStatusLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameApplyStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "命中缓存", err)
	wantEQ(t, "命中缓存", "status 来自缓存", reply.GetStatus(), int32(model.RealnameApplyStatusPending))
	wantEQ(t, "命中缓存", "remark 来自缓存", reply.GetRemark(), "缓存里的原因")
	wantOps(t, "命中缓存序列", e.ops(0), []string{"cache.GetJSON:realname_info_72011"})
}

func TestRealnameApplyStatusPropagatesReadError(t *testing.T) {
	const mid = int64(72012)
	e := newEnv(t)
	boom := errors.New("boom: realname_info 读不到")
	e.st.realname.failWith("FindOne", boom)

	l := NewRealnameApplyStatusLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameApplyStatus(&rpc.MemberMidReq{Mid: mid})
	wantErrIs(t, "读库失败", err, boom)
	wantEQ(t, "读库失败", "reply 为 nil", reply == nil, true)
	wantNoOpsWith(t, "读库失败", e.ops(0), "cache.SetJSON")
}

func TestRealnameStatusAndApplyStatusShareOneCacheEntry(t *testing.T) {
	// 两个方法（以及 Detail/Stripped/Adult）共用 realname_info_<mid> 一整个条目：
	// 先查状态、再查申请进度时，第二次必须命中第一次的回填。
	// 这条也解释了为什么 RealnameApply 成功后要删这一个 key 而不是「各自方法的 key」。
	const mid = int64(72013)
	e := newEnv(t)
	putStatusRow(e, mid, model.RealnameApplyStatusPass, "")

	status, err := NewRealnameStatusLogic(context.Background(), e.svcCtx).
		RealnameStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "状态", err)
	wantEQ(t, "状态", "realname_status", status.GetRealnameStatus(), int32(model.RealnameStatusTrue))

	apply, err := NewRealnameApplyStatusLogic(context.Background(), e.svcCtx).
		RealnameApplyStatus(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "申请进度", err)
	wantEQ(t, "申请进度", "status", apply.GetStatus(), int32(model.RealnameApplyStatusPass))

	detail, err := NewRealnameDetailLogic(context.Background(), e.svcCtx).
		RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "详情", err)
	wantEQ(t, "详情", "status", detail.GetStatus(), int32(model.RealnameStatusTrue))

	wantOps(t, "三方法共用一条缓存", e.ops(0), []string{
		"cache.GetJSON:realname_info_72013",
		"realname.FindOne:72013",
		"cache.SetJSON:realname_info_72013/3600",
		"cache.GetJSON:realname_info_72013",
		"cache.GetJSON:realname_info_72013",
		// Detail 命中缓存后仍会因渠道=主站去查申请单（此刻库里没有行）。
		"apply.FindOne:72013",
	})
	wantEQ(t, "三方法共用一条缓存", "SELECT 次数",
		e.st.log.countPrefix("realname.FindOne:"), 1)
	wantEQ(t, "三方法共用一条缓存", "回填次数",
		e.st.log.countPrefix("cache.SetJSON:realname_info_"), 1)
}

func TestRealnameStatusClusterDoesNotGuardNonPositiveMid(t *testing.T) {
	// 状态两个方法都没有 mid<=0 守卫（对照 AddMoral 等写侧的 mid<=0 报错）：
	// 非法入参照常查 realname_info_0 / realname_info_-1，并把哨兵写进那个 key。
	cases := []struct {
		name string
		mid  int64
		key  string
	}{
		{"mid=0", 0, "realname_info_0"},
		{"mid 为负", -7, "realname_info_-7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)

			status, err := NewRealnameStatusLogic(context.Background(), e.svcCtx).
				RealnameStatus(&rpc.MemberMidReq{Mid: tc.mid})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "realname_status", status.GetRealnameStatus(), int32(model.RealnameStatusFalse))

			apply, err := NewRealnameApplyStatusLogic(context.Background(), e.svcCtx).
				RealnameApplyStatus(&rpc.MemberMidReq{Mid: tc.mid})
			wantNoErr(t, tc.name+" 申请进度", err)
			wantEQ(t, tc.name+" 申请进度", "status=未申请", apply.GetStatus(), int32(model.RealnameApplyStatusNone))

			wantOps(t, tc.name+" 序列", e.ops(0), []string{
				"cache.GetJSON:" + tc.key,
				"realname.FindOne:" + itoa(tc.mid),
				"cache.SetJSON:" + tc.key + "/3600",
				"cache.GetJSON:" + tc.key,
			})
		})
	}
}
