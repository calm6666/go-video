package logic

// realnamedetaillogic_test.go 覆盖 RealnameDetail 与 RealnameStrippedInfo
// （repository/realname.go:184 与 :235，两者都坐在同一个 realnameInfo 读层上）。
//
// 这两个方法对「明文证件号」的处理正好相反，是实名簇断言价值最高的一对：
//   - RealnameDetail 把解密后的明文**当作返回值**端出去（契约里有 card 字段，
//     且它只做性别解析与手持照拼接），所以这里的重点是解密链与降级：
//     解不开时 Card 为空、Gender 回落 unknown，而不是报错；
//   - RealnameStrippedInfo 的存在意义就是**不含姓名与证件号**，所以除了字段口径，
//     还要把 reply 的字符串形态整体扫一遍明文；它另外暴露一个事实：
//     内部把 realnameInfo 读了两遍（:236 与 :240），缓存故障时就是两次 SELECT。
//
// 手持照链路只在「渠道=主站 且 申请单本身也是通过态 且 hand_img>0」时才走到底，
// 三个条件各测一刀，顺序里少一步多一步都会红。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx/logtest"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// putPassRealname 布一条已通过的真实形态存量行：证件号按生产口径 RSA 加密后入 card 列。
func putPassRealname(t *testing.T, e *env, mid int64, plain string, cardType int8) {
	t.Helper()
	e.st.realname.put(&model.RealnameInfo{
		Mid: mid, Realname: "张三", Country: model.RealnameCountryChina,
		CardType: cardType, Card: encryptCard(t, plain), CardMD5: "md5_" + itoa(mid),
		Status: model.RealnameApplyStatusPass,
	})
}

// cardWithDOB 拼一张 18 位身份证：6 位地址 + 8 位生日 + 3 位顺序码 + 1 位校验位。
// 顺序码末位（第 17 位）固定 '1' → 性别男，便于只变动生日这一维做成年边界用例。
func cardWithDOB(t *testing.T, dob time.Time) string {
	t.Helper()
	return "110101" + dob.Format("20060102") + "1211"
}

func TestRealnameDetailNotAppliedReturnsSentinelWithoutTouchingApply(t *testing.T) {
	const mid = int64(71001)
	e := newEnv(t)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "未申请", err)
	wantEQ(t, "未申请", "status=未通过", reply.GetStatus(), int32(model.RealnameStatusFalse))
	wantEQ(t, "未申请", "realname", reply.GetRealname(), "")
	wantEQ(t, "未申请", "card", reply.GetCard(), "")
	wantEQ(t, "未申请", "gender 兜底", reply.GetGender(), "unknown")
	wantEQ(t, "未申请", "hand_img", reply.GetHandImg(), "")
	// status=未申请 时在解析性别之前就返回了：CardType 恰好也是 0（身份证），
	// 若早退被删掉，parseIdentity("") 会失败并把这条变成「解密/解析」噪音——次序即断言。
	wantOps(t, "未申请序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71001",
		"realname.FindOne:71001",
		"cache.SetJSON:realname_info_71001/3600",
	})
	wantNoOpsWith(t, "未申请", e.ops(0), "apply.")
}

func TestRealnameDetailPassResolvesGenderAndHandPhoto(t *testing.T) {
	const mid = int64(71002)
	e := newEnv(t)
	putPassRealname(t, e, mid, cardMaleAdult, model.RealnameCardTypeIdentity)
	// 手持照：申请单需 Pass 且 hand_img>0；图片行存的是 token 路径，出口换成 CDN URL。
	e.st.apply.rows = append(e.st.apply.rows, &model.RealnameApply{
		Mid: mid, Status: model.RealnameApplyStatusPass, HandIMG: 7, CardNum: encryptCard(t, cardMaleAdult),
	})
	e.st.applyImg.rows[7] = &model.RealnameApplyImage{ID: 7, IMGData: "/idenfiles/handtok.txt"}

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "已通过", err)
	wantEQ(t, "已通过", "status=通过", reply.GetStatus(), int32(model.RealnameStatusTrue))
	wantEQ(t, "已通过", "realname", reply.GetRealname(), "张三")
	wantEQ(t, "已通过", "解密后的证件号", reply.GetCard(), cardMaleAdult)
	wantEQ(t, "已通过", "card_type", reply.GetCardType(), int32(model.RealnameCardTypeIdentity))
	wantEQ(t, "已通过", "性别由第 17 位奇偶推出", reply.GetGender(), "male")
	wantEQ(t, "已通过", "手持照 CDN URL", reply.GetHandImg(), "https://cdn.example.com/idenfiles/handtok.txt")
	wantOps(t, "已通过序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71002",
		"realname.FindOne:71002",
		"cache.SetJSON:realname_info_71002/3600",
		"apply.FindOne:71002",
		"applyImg.FindOne:7",
	})
}

func TestRealnameDetailFemaleCardResolvesFemale(t *testing.T) {
	// 同一套链路的另一侧：第 17 位偶数 → female。只测 male 会让「拿错下标」
	// （例如误用 id[17]）一路绿着。
	const mid = int64(71003)
	e := newEnv(t)
	putPassRealname(t, e, mid, cardFemaleMinor, model.RealnameCardTypeIdentity)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "女性证件号", err)
	wantEQ(t, "女性证件号", "性别", reply.GetGender(), "female")
	wantEQ(t, "女性证件号", "channel 非主站故不查申请单", len(e.st.apply.rows), 0)
	wantOps(t, "女性证件号序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71003",
		"realname.FindOne:71003",
		"cache.SetJSON:realname_info_71003/3600",
		"apply.FindOne:71003",
	})
}

func TestRealnameDetail15DigitCardUsesLegacyBranch(t *testing.T) {
	// 15 位老式号走 parseIdentity 的 "19" 前缀分支：生日 1990-03-07、性别取末位。
	const mid = int64(71004)
	e := newEnv(t)
	putPassRealname(t, e, mid, cardIDCard15, model.RealnameCardTypeIdentity)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "15 位身份证", err)
	wantEQ(t, "15 位身份证", "明文回读", reply.GetCard(), cardIDCard15)
	wantEQ(t, "15 位身份证", "性别", reply.GetGender(), "male")
}

func TestRealnameDetailNonIdentityCardTypeKeepsGenderUnknown(t *testing.T) {
	// 护照/其他类型即使号串形似身份证也不解析性别（RealnameDetail 只在 CardType=0 时解析）。
	// 这里故意存一个「按身份证规则能解出 male」的号，验证的是类型闸门而不是解析能力。
	const mid = int64(71005)
	e := newEnv(t)
	putPassRealname(t, e, mid, cardMaleAdult, 1)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "证件类型非身份证", err)
	wantEQ(t, "证件类型非身份证", "明文证件号照旧返回", reply.GetCard(), cardMaleAdult)
	wantEQ(t, "证件类型非身份证", "gender 保持 unknown", reply.GetGender(), "unknown")
}

func TestRealnameDetailUndecryptableCardDegradesToEmptyNoPII(t *testing.T) {
	// 存量行的 card 列不是合法密文（被截断/被人工改写）：CardDecrypt 失败只记日志，
	// payload.RealCard 留空 → 出口是「已通过但证件号为空、性别 unknown」，不报错。
	const mid = int64(71006)
	e := newEnv(t)
	e.st.realname.put(&model.RealnameInfo{
		Mid: mid, Realname: "张三", CardType: model.RealnameCardTypeIdentity,
		Card: "这不是base64密文!!!", Status: model.RealnameApplyStatusPass, CardMD5: "md5bad",
	})
	logs := logtest.NewCollector(t)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "密文损坏", err)
	wantEQ(t, "密文损坏", "status 仍是通过", reply.GetStatus(), int32(model.RealnameStatusTrue))
	wantEQ(t, "密文损坏", "card 为空", reply.GetCard(), "")
	wantEQ(t, "密文损坏", "gender", reply.GetGender(), "unknown")
	if !strings.Contains(logs.String(), "decrypt card") {
		t.Fatalf("期望捕获到解密失败的日志，实际=%q", logs.String())
	}
	wantNoPII(t, "密文损坏", []string{"这不是base64密文!!!"}, map[string]string{
		"logx": logs.String(), "reply": reply.String(),
	})
	// 解不开就不该再查申请单以外的东西，且**不得把坏值当空值回填**之外多做任何写。
	wantOps(t, "密文损坏序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71006",
		"realname.FindOne:71006",
		"cache.SetJSON:realname_info_71006/3600",
		"apply.FindOne:71006",
	})
}

func TestRealnameDetailParseFailureLeaksPlaintextCardIntoInfoLog(t *testing.T) {
	// 缺陷复现（README 已知缺口）：库里的 card 是**合法密文**，解出来却是长度不等于
	// 15/18 的号串（审核上游回写、或 = 旁部落库后被人工补一位都能造出这行）。
	// parseIdentity 只在 len 分支的 default 里把原串格式化进错误（realname.go:518），
	// RealnameDetail 又把该 err 原样写进 INFO 日志（:202）→ 明文证件号进日志。
	// 注意必须是**长度不合法**：18 位含字母的号（cardMalformed）走的是 Atoi 分支，
	// 错误里只带 "0x" 两个字，泄不出全号——所以本用例刻意用 19 位号。
	const mid = int64(71007)
	e := newEnv(t)
	leaky := cardMaleAdult + "9" // 19 位：isIDCard 必拒，但读侧不做格式校验
	putPassRealname(t, e, mid, leaky, model.RealnameCardTypeIdentity)
	logs := logtest.NewCollector(t)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "畸形号仍能查到", err)
	wantEQ(t, "畸形号仍能查到", "gender 兜底 unknown", reply.GetGender(), "unknown")
	wantEQ(t, "畸形号仍能查到", "card 原样吐出明文", reply.GetCard(), leaky)
	if !strings.Contains(logs.String(), "parse identity") {
		t.Fatalf("没捕获到性别解析日志，通道可能失效：%q", logs.String())
	}
	if !strings.Contains(logs.String(), leaky) {
		t.Errorf("期望现状是明文证件号被写进日志（缺陷），实际日志=%q", logs.String())
	}
}

func TestRealnameDetailHandPhotoLookupsShortCircuit(t *testing.T) {
	// 手持照的三重闸门各测一刀：渠道、申请单状态、hand_img 值。
	// 每格都断言「图片查询到底有没有发生」，只断言 URL 会漏掉多查/少查。
	cases := []struct {
		name      string
		channel   int8
		applyRow  *model.RealnameApply
		imgRow    *model.RealnameApplyImage
		wantImgOp bool
		wantURL   string
	}{
		{"支付宝渠道不查申请单", model.RealnameChannelAlipay, nil, nil, false, ""},
		{"申请单不存在", model.RealnameChannelMain, nil, nil, false, ""},
		{"申请单还是审核中", model.RealnameChannelMain,
			&model.RealnameApply{Mid: 71008, Status: model.RealnameApplyStatusPending, HandIMG: 9}, nil, false, ""},
		{"hand_img 为 0", model.RealnameChannelMain,
			&model.RealnameApply{Mid: 71008, Status: model.RealnameApplyStatusPass}, nil, false, ""},
		{"图片行缺失则留空", model.RealnameChannelMain,
			&model.RealnameApply{Mid: 71008, Status: model.RealnameApplyStatusPass, HandIMG: 9}, nil, true, ""},
		{"齐备才拼 URL", model.RealnameChannelMain,
			&model.RealnameApply{Mid: 71008, Status: model.RealnameApplyStatusPass, HandIMG: 9},
			&model.RealnameApplyImage{ID: 9, IMGData: "/idenfiles/ok.txt"}, true,
			"https://cdn.example.com/idenfiles/ok.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const mid = int64(71008)
			e := newEnv(t)
			e.st.realname.put(&model.RealnameInfo{
				Mid: mid, Realname: "张三", Channel: tc.channel, CardType: model.RealnameCardTypeIdentity,
				Card: encryptCard(t, cardMaleAdult), Status: model.RealnameApplyStatusPass, CardMD5: "md5c",
			})
			if tc.applyRow != nil {
				e.st.apply.rows = append(e.st.apply.rows, tc.applyRow)
			}
			if tc.imgRow != nil {
				e.st.applyImg.rows[tc.imgRow.ID] = tc.imgRow
			}

			l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
			reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "hand_img", reply.GetHandImg(), tc.wantURL)
			wantEQ(t, tc.name, "是否发生图片查询",
				e.st.log.countPrefix("applyImg.FindOne:") > 0, tc.wantImgOp)
			if tc.channel == model.RealnameChannelAlipay {
				wantNoOpsWith(t, tc.name, e.ops(0), "apply.FindOne:")
			}
		})
	}
}

func TestRealnameDetailApplyLookupFailurePropagates(t *testing.T) {
	// 申请单读失败是**错误**（不是降级）：整个详情接口失败，但已回填的缓存不回收。
	const mid = int64(71009)
	e := newEnv(t)
	putPassRealname(t, e, mid, cardMaleAdult, model.RealnameCardTypeIdentity)
	boom := errors.New("boom: realname_apply 读不到")
	e.st.apply.failWith("FindOne", boom)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantErrIs(t, "申请单读失败", err, boom)
	wantEQ(t, "申请单读失败", "reply 为 nil", reply == nil, true)
	wantOps(t, "申请单读失败序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71009",
		"realname.FindOne:71009",
		"cache.SetJSON:realname_info_71009/3600",
		"apply.FindOne:71009",
	})
}

func TestRealnameDetailCacheHitTrustsRealCardWithoutDecrypting(t *testing.T) {
	// 命中时**不再解密**：把缓存里的 card 列布成一堆垃圾、只把 real_card 布成合法号，
	// 性别仍能解出 → 证明读的是 real_card。库里再放一个不同的号，走回源就红。
	const mid = int64(71010)
	e := newEnv(t)
	e.st.cache.warmJSON(keyRealname(mid), realnameCacheView{
		Cached: true, Mid: mid, Realname: "张三", CardType: model.RealnameCardTypeIdentity,
		Country: model.RealnameCountryChina, Card: "GARBAGE-NOT-CIPHERTEXT",
		RealCard: cardFemaleMinor, Status: model.RealnameApplyStatusPass, Channel: model.RealnameChannelAlipay,
	})
	e.st.realname.put(&model.RealnameInfo{Mid: mid, Realname: "李四", Status: model.RealnameApplyStatusPass})

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "命中缓存", err)
	wantEQ(t, "命中缓存", "realname 来自缓存", reply.GetRealname(), "张三")
	wantEQ(t, "命中缓存", "card 来自 real_card", reply.GetCard(), cardFemaleMinor)
	wantEQ(t, "命中缓存", "gender", reply.GetGender(), "female")
	wantNoPII(t, "命中缓存", []string{"GARBAGE-NOT-CIPHERTEXT"}, map[string]string{"reply": reply.String()})
	wantOps(t, "命中缓存序列", e.ops(0), []string{"cache.GetJSON:realname_info_71010"})
}

func TestRealnameDetailCacheFaultDegradesButDoesNotBackfill(t *testing.T) {
	// 缓存读故障被吞成 miss（realname.go:86）→ 回源、照常返回，但 cacheOK=false 时**不回填**。
	// 与 Moral/BaseInfo 的降级口径对照：这里连错误都不上抛，只多打一次库。
	const mid = int64(71011)
	e := newEnv(t)
	boom := errors.New("boom: redis 不可用")
	e.st.cache.failWith("GetJSON", boom)
	putPassRealname(t, e, mid, cardMaleAdult, model.RealnameCardTypeIdentity)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "缓存故障降级", err)
	wantEQ(t, "缓存故障降级", "证件号仍解出", reply.GetCard(), cardMaleAdult)
	wantOps(t, "缓存故障降级序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71011",
		"realname.FindOne:71011",
		"apply.FindOne:71011",
	})
	wantNoOpsWith(t, "缓存故障降级", e.ops(0), "cache.SetJSON")
}

func TestRealnameDetailDBFailurePropagates(t *testing.T) {
	const mid = int64(71012)
	e := newEnv(t)
	boom := errors.New("boom: realname_info 读不到")
	e.st.realname.failWith("FindOne", boom)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantErrIs(t, "读库失败", err, boom)
	wantEQ(t, "读库失败", "reply 为 nil", reply == nil, true)
	wantOps(t, "读库失败序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71012",
		"realname.FindOne:71012",
	})
	wantNoOpsWith(t, "读库失败不得回填", e.ops(0), "cache.SetJSON")
}

func TestRealnameStrippedInfoCarriesNoNameAndNoCard(t *testing.T) {
	const mid = int64(71013)
	e := newEnv(t)
	putPassRealname(t, e, mid, cardMaleAdult, model.RealnameCardTypeIdentity)

	l := NewRealnameStrippedInfoLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStrippedInfo(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "脱敏信息", err)
	wantEQ(t, "脱敏信息", "mid", reply.GetMid(), mid)
	wantEQ(t, "脱敏信息", "status", reply.GetStatus(), int32(model.RealnameApplyStatusPass))
	wantEQ(t, "脱敏信息", "channel", reply.GetChannel(), int32(model.RealnameChannelMain))
	wantEQ(t, "脱敏信息", "country", reply.GetCountry(), int32(model.RealnameCountryChina))
	wantEQ(t, "脱敏信息", "card_type", reply.GetCardType(), int32(model.RealnameCardTypeIdentity))
	wantEQ(t, "脱敏信息", "成年", int(reply.GetAdultType()), model.RealnameAdultTypeTrue)
	wantNoPII(t, "脱敏信息", []string{cardMaleAdult, "张三", "md5_71013"}, map[string]string{
		"reply": reply.String(),
	})

	// 现状：内部把 realnameInfo 读了两遍（:236 与 :240）。第二遍靠第一遍的回填才没打库。
	wantOps(t, "脱敏信息序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71013",
		"realname.FindOne:71013",
		"cache.SetJSON:realname_info_71013/3600",
		"cache.GetJSON:realname_info_71013",
	})
}

func TestRealnameStrippedInfoSecondReadHitsDBAgainWhenCacheFaults(t *testing.T) {
	// 上条用例的对照：缓存故障时第一遍不回填，于是第二遍 realnameInfo **真的再打一次库**。
	// 一次脱敏查询 = 2 次 SELECT（README 已知缺口）。
	const mid = int64(71014)
	e := newEnv(t)
	e.st.cache.failWith("GetJSON", errors.New("boom: redis 不可用"))
	putPassRealname(t, e, mid, cardMaleAdult, model.RealnameCardTypeIdentity)

	l := NewRealnameStrippedInfoLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStrippedInfo(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "缓存故障下的脱敏查询", err)
	wantEQ(t, "缓存故障下的脱敏查询", "成年判定仍来自第二次解密", int(reply.GetAdultType()), model.RealnameAdultTypeTrue)
	wantOps(t, "缓存故障下的脱敏查询序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71014",
		"realname.FindOne:71014",
		"cache.GetJSON:realname_info_71014",
		"realname.FindOne:71014",
	})
	wantEQ(t, "缓存故障下的脱敏查询", "SELECT 次数",
		e.st.log.countPrefix("realname.FindOne:"), 2)
}

func TestRealnameStrippedInfoAdultFailureDegradesOnlyAdultType(t *testing.T) {
	// 第二次读（RealnameAdult→realnameInfo）失败时被 Errorf 吞掉，adult 落 Unknown，
	// 其余字段照常返回——爆炸半径只有一列。第一次读失败才是整体报错（见下一条）。
	// 缓存被置成常故障，否则第一遍的回填会让第二遍命中缓存、根本打不到第二次 SELECT，
	// 「第 2 次读失败」这个分支就永远执行不到（这也顺带钉住：命中缓存时成年判定不再读库）。
	const mid = int64(71015)
	e := newEnv(t)
	putPassRealname(t, e, mid, cardMaleAdult, model.RealnameCardTypeIdentity)
	e.st.cache.failWith("GetJSON", errors.New("boom: redis 不可用"))
	boom := errors.New("boom: 第二次读库失败")
	e.st.realname.failOn("FindOne", 2, boom)

	l := NewRealnameStrippedInfoLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStrippedInfo(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "成年判定降级", err)
	wantEQ(t, "成年判定降级", "adult=未知", int(reply.GetAdultType()), model.RealnameAdultTypeUnknown)
	wantEQ(t, "成年判定降级", "status 仍是通过", reply.GetStatus(), int32(model.RealnameApplyStatusPass))
	wantEQ(t, "成年判定降级", "mid 仍来自第一遍", reply.GetMid(), mid)
	wantNoPII(t, "成年判定降级", []string{cardMaleAdult}, map[string]string{"reply": reply.String()})
	wantOps(t, "成年判定降级序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71015",
		"realname.FindOne:71015",
		"cache.GetJSON:realname_info_71015",
		"realname.FindOne:71015",
	})
}

func TestRealnameStrippedInfoPropagatesFirstReadFailure(t *testing.T) {
	const mid = int64(71016)
	e := newEnv(t)
	boom := errors.New("boom: realname_info 读不到")
	e.st.realname.failWith("FindOne", boom)

	l := NewRealnameStrippedInfoLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStrippedInfo(&rpc.MemberMidReq{Mid: mid})
	wantErrIs(t, "第一遍读失败", err, boom)
	wantEQ(t, "第一遍读失败", "reply 为 nil", reply == nil, true)
	wantOps(t, "第一遍读失败序列", e.ops(0), []string{
		"cache.GetJSON:realname_info_71016",
		"realname.FindOne:71016",
	})
}

func TestRealnameStrippedInfoAdultTypeMatrix(t *testing.T) {
	// RealnameAdult 的四条回落各有独立成因，逐条给行（repository/realname.go:256）。
	// 时间敏感项用 time.Now() 反推生日，与生产同一个时钟源，跨零点抖动的口径见
	// fakes_test.go 头部声明。
	now := time.Now()
	cases := []struct {
		name     string
		status   int8
		cardType int8
		country  int16
		plain    string
		want     int
	}{
		{"未通过一律未知", model.RealnameApplyStatusPending, model.RealnameCardTypeIdentity,
			model.RealnameCountryChina, cardMaleAdult, model.RealnameAdultTypeUnknown},
		{"证件类型非身份证则未知", model.RealnameApplyStatusPass, 1,
			model.RealnameCountryChina, cardMaleAdult, model.RealnameAdultTypeUnknown},
		{"国家非中国则未知", model.RealnameApplyStatusPass, model.RealnameCardTypeIdentity,
			1, cardMaleAdult, model.RealnameAdultTypeUnknown},
		{"解不出生日则未知", model.RealnameApplyStatusPass, model.RealnameCardTypeIdentity,
			model.RealnameCountryChina, cardMalformed, model.RealnameAdultTypeUnknown},
		{"出生在未来则判未成年", model.RealnameApplyStatusPass, model.RealnameCardTypeIdentity,
			model.RealnameCountryChina, cardWithDOB(t, now.AddDate(1, 0, 0)), model.RealnameAdultTypeFalse},
		{"刚满 18 岁当天即成年", model.RealnameApplyStatusPass, model.RealnameCardTypeIdentity,
			model.RealnameCountryChina, cardWithDOB(t, now.AddDate(-18, 0, 0)), model.RealnameAdultTypeTrue},
		{"差一天满 18 判未成年", model.RealnameApplyStatusPass, model.RealnameCardTypeIdentity,
			model.RealnameCountryChina, cardWithDOB(t, now.AddDate(-18, 0, 1)), model.RealnameAdultTypeFalse},
		{"15 位号按 19xx 解释后成年", model.RealnameApplyStatusPass, model.RealnameCardTypeIdentity,
			model.RealnameCountryChina, cardIDCard15, model.RealnameAdultTypeTrue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const mid = int64(71017)
			e := newEnv(t)
			e.st.realname.put(&model.RealnameInfo{
				Mid: mid, Realname: "张三", Status: tc.status, CardType: tc.cardType, Country: tc.country,
				Card: encryptCard(t, tc.plain), CardMD5: "md5m",
			})
			l := NewRealnameStrippedInfoLogic(context.Background(), e.svcCtx)
			reply, err := l.RealnameStrippedInfo(&rpc.MemberMidReq{Mid: mid})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "adult_type", int(reply.GetAdultType()), tc.want)
			wantNoPII(t, tc.name, []string{tc.plain}, map[string]string{"reply": reply.String()})
		})
	}
}

func TestRealnameStrippedInfoDoesNotGuardNonPositiveMid(t *testing.T) {
	// 实名读簇（Detail/Stripped/Status/ApplyStatus/MidByRealnameCard/TelCapture）
	// 都没有 mid<=0 守卫：mid=0 照查 realname_info_0、照回填哨兵。
	const mid = int64(0)
	e := newEnv(t)

	l := NewRealnameStrippedInfoLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameStrippedInfo(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "mid=0", err)
	wantEQ(t, "mid=0", "status=未申请", int(reply.GetStatus()), model.RealnameApplyStatusNone)
	wantOps(t, "mid=0 的 key 形态", e.ops(0), []string{
		"cache.GetJSON:realname_info_0",
		"realname.FindOne:0",
		"cache.SetJSON:realname_info_0/3600",
		"cache.GetJSON:realname_info_0",
	})
	// 哨兵带着 mid=0 进了缓存载荷：与真实用户无法区分（同 README 缺口 1 的口径）。
	cached, ok := cachedRealname(t, e, mid)
	wantEQ(t, "mid=0", "哨兵已回填", ok, true)
	wantEQ(t, "mid=0", "哨兵 status", int(cached.Status), model.RealnameApplyStatusNone)
}

func TestRealnameDetailTrailingEqualsRowCannotBeReadBack(t *testing.T) {
	// 与 = 旁路（realname.go:419）对接的一次闭环验证：那条形如 "…=" 的号被原样写进
	// 库后，读侧只能解出空——即「写入口允许一种永远读不出来的行」。
	// 断言的是读侧降级，同时把写/读两侧对同一列的口径不一致钉成一条证据。
	const mid = int64(71018)
	e := newEnv(t)
	e.st.realname.put(&model.RealnameInfo{
		Mid: mid, Realname: "张三", CardType: model.RealnameCardTypeIdentity,
		Card: cardMaleAdult + "=", Status: model.RealnameApplyStatusPass, CardMD5: "md5eq",
	})
	logs := logtest.NewCollector(t)

	l := NewRealnameDetailLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameDetail(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "明文行读不回", err)
	wantEQ(t, "明文行读不回", "card 为空", reply.GetCard(), "")
	wantEQ(t, "明文行读不回", "status 仍是已通过", reply.GetStatus(), int32(model.RealnameStatusTrue))
	// 关键反向断言：解不开时**不得把库里那串原样端出去**（否则等于把 card 列当明文透传）。
	wantNoPII(t, "明文行读不回", []string{cardMaleAdult}, map[string]string{
		"reply": reply.String(), "logx": logs.String(),
	})
}

func TestRealnameDetailAndStrippedShareOneBackfillPerRequest(t *testing.T) {
	// 两个方法各自只回填一次：Detail 读一遍、Stripped 读两遍但第二遍命中。
	// 回填载荷里 card 必须是密文——这是「缓存里躺着的是可公开的部分」的最低要求。
	const mid = int64(71019)
	e := newEnv(t)
	putPassRealname(t, e, mid, cardMaleAdult, model.RealnameCardTypeIdentity)

	if _, err := NewRealnameDetailLogic(context.Background(), e.svcCtx).RealnameDetail(&rpc.MemberMidReq{Mid: mid}); err != nil {
		t.Fatalf("详情：%v", err)
	}
	cached, ok := cachedRealname(t, e, mid)
	wantEQ(t, "详情", "回填发生", ok, true)
	wantEQ(t, "详情", "载荷里是密文列", cached.Card, e.st.realname.get(mid).Card)
	wantEQ(t, "详情", "回填次数", e.st.log.countPrefix("cache.SetJSON:realname_info_"), 1)
	// 现状：载荷同时带了 real_card（明文），见 README 已知缺口。
	wantEQ(t, "详情", "real_card 是明文", cached.RealCard, cardMaleAdult)

	e2 := newEnv(t)
	putPassRealname(t, e2, mid, cardFemaleMinor, model.RealnameCardTypeIdentity)
	if _, err := NewRealnameStrippedInfoLogic(context.Background(), e2.svcCtx).
		RealnameStrippedInfo(&rpc.MemberMidReq{Mid: mid}); err != nil {
		t.Fatalf("脱敏：%v", err)
	}
	wantEQ(t, "脱敏", "回填次数", e2.st.log.countPrefix("cache.SetJSON:realname_info_"), 1)
	wantEQ(t, "脱敏", "SELECT 次数", e2.st.log.countPrefix("realname.FindOne:"), 1)
}
