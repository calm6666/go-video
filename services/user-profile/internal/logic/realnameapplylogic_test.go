package logic

// realnameapplylogic_test.go 覆盖 RealnameApply（repository/realname.go:381）。
//
// 这个方法是全服务唯一会**写入证件号**的入口，所以断言面按「明文证件号绝不允许
// 出现在返回值 / 日志 / 落库列里」逐条铺：
//   - 落库列：realname_apply.card_num 必须是能用同一把 PEM 解回原号的密文；
//   - Redis：申请链路写过的每个 key（realname_info 回填 + 验证码）的**历史载荷**里
//     不得出现明文——用 blobOf 而非终态，因为成功路径末尾会把 key 删掉；
//   - 日志：logic 层与 repository 层的 Errorf 不得回显证件号（logtest 收口，
//     并在同一用例里先断言「确实捕获到了那行日志」，避免捕获通道失效造成假绿）；
//   - 返回值：EmptyReply 无业务字段，仍扫一次字符串形态兜底。
//
// 次序本身也是断言（纪律 3）：验证码 → 读实名信息 → 查重 → **才**落图片 → 加密 → 写申请单。
// 图片落库排在格式/加密校验之前，所以那两类错误会留下孤儿图片行；这个 blast radius
// 用 countPrefix / 行数显式钉住，而不是只断言错误码。

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/logx/logtest"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// applyReq 组一个申请入参（三张证件照默认都给 token，个别用例再单独清零）。
func applyReq(mid int64, code int, cardType int32, card string) *rpc.RealnameApplyReq {
	return &rpc.RealnameApplyReq{
		Mid: mid, CaptureCode: int64(code), Realname: "张三",
		CardType: cardType, CardCode: card, Country: model.RealnameCountryChina,
		HandImgToken: "handtok", FrontImgToken: "fronttok", BackImgToken: "backtok",
	}
}

// envWithCryptor 换掉注入缝里的密钥对（装配与 newRawStore 完全一致，只换 Cryptor）。
// 用途是让「加密/解密失败」分支由**密钥**触发，而不是由坏 PEM 伪装成业务分支。
func envWithCryptor(t *testing.T, pub, priv string) *env {
	t.Helper()
	e := newEnv(t)
	st := e.st
	st.repo = repository.NewWithDeps(st.cache, st.conn,
		st.base, st.exp, st.flag, st.moral, st.official, st.officialDoc, st.addit,
		st.monitor, st.review, st.realname, st.apply, st.applyImg, st.logs, st.outbox,
		repository.Options{
			IMGURLTemplate: "https://cdn.example.com/idenfiles/%s.txt",
			Cryptor:        repository.NewCardCryptor(pub, priv),
		})
	st.log.reset() // 新 Repository 的构造期快照装载不算被测调用（纪律 4）
	e.svcCtx.Repository = st.repo
	return e
}

func TestRealnameApplyHappyPathStoresOnlyCiphertext(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantNoErr(t, "正常申请", err)
	wantEQ(t, "正常申请", "reply 非空", reply != nil, true)
	wantEQ(t, "正常申请", "EmptyReply 无字段", reply.String(), "")

	// 次序：验证码（错误次数 + 本体）→ 读实名（miss 回源 + 回填「未申请」哨兵）
	// → 查重 → 三张图片 → 申请单 → 删验证码 + 删实名缓存。
	// card_md5 由未导出盐算出、断言侧不可复现，只断前缀（非确定值不进精确序列）。
	wantOpsLoose(t, "正常申请调用序列", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70001",
		"cache.GetInt:realname_cap_code_70001",
		"cache.GetJSON:realname_info_70001",
		"realname.FindOne:70001",
		"cache.SetJSON:realname_info_70001/3600",
		"realname.FindOneByCardMD5:~",
		"applyImg.Insert:1",
		"applyImg.Insert:2",
		"applyImg.Insert:3",
		"apply.Insert:70001/1",
		"cache.Del:realname_cap_code_70001",
		"cache.Del:realname_info_70001",
	})

	row := e.st.apply.latest(mid)
	if row == nil {
		t.Fatal("申请单没有落库")
	}
	wantEQ(t, "正常申请", "status=审核中", int(row.Status), model.RealnameApplyStatusPending)
	wantEQ(t, "正常申请", "realname", row.Realname, "张三")
	wantEQ(t, "正常申请", "card_type", int(row.CardType), model.RealnameCardTypeIdentity)
	wantEQ(t, "正常申请", "country", int(row.Country), model.RealnameCountryChina)
	// 三张图片的自增 ID 必须由 LastInsertId 回写进申请单三列
	// （替身若只改内部副本，这里就是唯一能发现「图片 ID 丢了」的地方）。
	wantEQ(t, "正常申请", "hand_img", row.HandIMG, int64(1))
	wantEQ(t, "正常申请", "front_img", row.FrontIMG, int64(2))
	wantEQ(t, "正常申请", "back_img", row.BackIMG, int64(3))

	// 落库列：不得是明文，且必须是**能解回原证件号**的真密文（不是随机串、不是 base64(明文)）。
	if row.CardNum == cardMaleAdult {
		t.Fatalf("card_num 直接存了明文证件号：%s", row.CardNum)
	}
	if row.CardNum == base64.StdEncoding.EncodeToString([]byte(cardMaleAdult)) {
		t.Errorf("card_num 只是 base64(明文)，不是加密：%s", row.CardNum)
	}
	plain, derr := decryptCard(row.CardNum)
	wantNoErr(t, "card_num 可解密", derr)
	wantEQ(t, "card_num 解密回读", "证件号", plain, cardMaleAdult)
	wantEQ(t, "card_md5", "哈希长度", len(row.CardMD5), 32)
	if strings.Contains(row.CardMD5, cardMaleAdult) {
		t.Errorf("card_md5 列里出现了明文：%s", row.CardMD5)
	}

	// 图片列只存 token 路径，不得夹带证件号或密文。
	imgBlobs := map[string]string{}
	for id := int64(1); id <= 3; id++ {
		imgBlobs["img_data_"+itoa(id)] = e.st.applyImg.rows[id].IMGData
	}
	wantEQ(t, "图片落库口径", "第 1 张 img_data", e.st.applyImg.rows[1].IMGData, "/idenfiles/handtok.txt")
	wantNoPII(t, "证件照行", []string{cardMaleAdult, row.CardNum}, imgBlobs)

	// Redis：申请链路对 realname_info 只写过「未申请」哨兵，随后被删。
	// 判 PII 必须看 blobOf（历史写入），终态已被 Del、看不出明文有没有进过 Redis。
	if e.st.cache.jsonOf(keyRealname(mid), &realnameCacheView{}) {
		t.Error("申请成功后实名缓存没有被失效")
	}
	wantNoPII(t, "申请链路写过的缓存载荷", []string{cardMaleAdult}, map[string]string{
		"realname_info 历史载荷": e.st.cache.blobOf(keyRealname(mid)),
		"验证码历史载荷":            e.st.cache.blobOf(keyCapCode(mid)),
	})
	wantNoPII(t, "返回值", []string{cardMaleAdult}, map[string]string{"reply": reply.String()})
}

func TestRealnameApplyWrongCaptureResetsAndIncrementsErrTimes(t *testing.T) {
	const mid = int64(70002)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	// 错误次数 miss（生产把 miss 记成 -1）→ 先 SetInt(0) 再 Incr，且首次要补 TTL。
	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	reply, err := l.RealnameApply(applyReq(mid, 654321, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantErrIs(t, "验证码错误", err, repository.ErrRealnameCaptureErr)
	wantEQ(t, "验证码错误", "reply 为 nil", reply == nil, true)

	wantOps(t, "验证码错误的副作用序列", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70002",
		"cache.GetInt:realname_cap_code_70002",
		"cache.SetInt:realname_cap_err_times_70002/86400",
		"cache.Incr:realname_cap_err_times_70002",
		"cache.GetInt:realname_cap_err_times_70002",
		"cache.Expire:realname_cap_err_times_70002/86400",
	})
	v, ok := e.st.cache.intOf(keyCapErr(mid))
	wantEQ(t, "错误次数", "累加后的值", v, int64(1))
	wantEQ(t, "错误次数", "key 存在", ok, true)

	// 业务写侧一步都不能发生：没读库、没落图片、没写申请单、也没失效缓存。
	wantNoOpsWith(t, "验证码错误不得触库", e.ops(0), "realname.")
	wantNoOpsWith(t, "验证码错误不得落图片", e.ops(0), "applyImg.")
	wantNoOpsWith(t, "验证码错误不得写申请单", e.ops(0), "apply.")
	wantNoOpsWith(t, "验证码错误不得删验证码", e.ops(0), "cache.Del")
	wantEQ(t, "验证码错误", "申请单行数", len(e.st.apply.rows), 0)
	// 验证码本身必须留着（错一次不该逼用户重新领取）。
	if _, ok := e.st.cache.intOf(keyCapCode(mid)); !ok {
		t.Error("验证码被错误尝试删掉了")
	}
}

func TestRealnameApplySecondWrongCaptureSkipsResetAndTTL(t *testing.T) {
	// 同一分支的另一半：errTimes 已有值（>=0）时不再 SetInt(0)，
	// 且 Incr 后值 >1 时**不再补 Expire**——24h 窗口从首次失败起算、不随失败滑动。
	const mid = int64(70003)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	e.st.cache.warmInt(keyCapErr(mid), 2)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 1, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantErrIs(t, "第二次错验证码", err, repository.ErrRealnameCaptureErr)
	wantOps(t, "第二次错验证码序列", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70003",
		"cache.GetInt:realname_cap_code_70003",
		"cache.Incr:realname_cap_err_times_70003",
		"cache.GetInt:realname_cap_err_times_70003",
	})
	v, _ := e.st.cache.intOf(keyCapErr(mid))
	wantEQ(t, "第二次错验证码", "错误次数", v, int64(3))
}

func TestRealnameApplyLocksOutAfterTooManyErrors(t *testing.T) {
	const mid = int64(70004)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	e.st.cache.warmInt(keyCapErr(mid), 4) // >3 即锁

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantErrIs(t, "错误次数超限", err, repository.ErrRealnameCaptureErrTooMany)
	// 锁定时**连验证码都不读**（不做比对），但会先把验证码删掉，逼用户重新领取。
	wantOps(t, "锁定序列", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70004",
		"cache.Del:realname_cap_code_70004",
	})
	// 行为哨兵（README 已知缺口）：只删验证码、不清错误次数，而错误计数 TTL 是 24h。
	// 用户除非成功发一次新验证码（而发码路径同样不重置它），否则 24h 内没有解锁路径。
	v, ok := e.st.cache.intOf(keyCapErr(mid))
	wantEQ(t, "锁定后", "错误次数没有被重置", v, int64(4))
	wantEQ(t, "锁定后", "错误次数 key 仍在", ok, true)
	wantEQ(t, "锁定后", "申请单行数", len(e.st.apply.rows), 0)
}

func TestRealnameApplyMissingCaptureIsInvalid(t *testing.T) {
	const mid = int64(70005)
	e := newEnv(t) // 不领取验证码

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 0, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantErrIs(t, "未领取验证码", err, repository.ErrRealnameCaptureInvalid)
	wantOps(t, "未领取验证码序列", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70005",
		"cache.GetInt:realname_cap_code_70005",
	})
}

func TestRealnameApplyCacheHitSkipsDBButStillGatesStatus(t *testing.T) {
	// 命中 realname_info 缓存时**完全不读库**，门禁用的是缓存里的 status。
	// 这条既证明读穿被跳过，也钉住一个后果：缓存被写成 status=3 就能绕开
	// 「审核中不得重复提交」的门禁（key 是裸的 realname_info_<mid>，无签名）。
	const mid = int64(70006)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	e.st.cache.warmJSON(keyRealname(mid), realnameCacheView{
		Cached: true, Mid: mid, Status: model.RealnameApplyStatusNone,
	})
	e.st.realname.put(&model.RealnameInfo{Mid: mid, Status: model.RealnameApplyStatusPass})

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantNoErr(t, "缓存说没申请过就放行", err)
	wantOpsLoose(t, "命中缓存时的库调用", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70006",
		"cache.GetInt:realname_cap_code_70006",
		"cache.GetJSON:realname_info_70006",
		"realname.FindOneByCardMD5:~",
		"applyImg.Insert:1",
		"applyImg.Insert:2",
		"applyImg.Insert:3",
		"apply.Insert:70006/1",
		"cache.Del:realname_cap_code_70006",
		"cache.Del:realname_info_70006",
	})
	wantNoOpsWith(t, "命中缓存", e.ops(0), "realname.FindOne:")
	wantNoOpsWith(t, "命中缓存不得回填", e.ops(0), "cache.SetJSON")
}

func TestRealnameApplyRejectsPendingAndPassButAllowsRejected(t *testing.T) {
	// 流程状态门禁（realname.go:390）：0 审核中 / 1 通过 都拒；2 驳回 / 3 未申请 放行。
	// 「驳回可重提」是这条规则唯一的产品意义，三种状态都要跑，不能只测一侧。
	for _, tc := range []struct {
		name        string
		status      int8
		wantAlready bool
	}{
		{"审核中", model.RealnameApplyStatusPending, true},
		{"已通过", model.RealnameApplyStatusPass, true},
		{"已驳回", model.RealnameApplyStatusBack, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const mid = int64(70007)
			e := newEnv(t)
			e.st.cache.warmInt(keyCapCode(mid), 123456)
			e.st.realname.put(&model.RealnameInfo{
				Mid: mid, Realname: "张三", Status: tc.status, Reason: "证件模糊",
				Card: encryptCard(t, cardMaleAdult), CardMD5: "seedmd5" + itoa(int64(tc.status)),
			})

			l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
			_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardFemaleMinor))
			if !tc.wantAlready {
				wantNoErr(t, tc.name+"应放行", err)
				wantEQ(t, tc.name+"放行", "申请单行数", len(e.st.apply.rows), 1)
				wantEQ(t, tc.name+"放行", "图片行数", len(e.st.applyImg.rows), 3)
				return
			}
			wantErrIs(t, tc.name+"应拒绝", err, repository.ErrRealnameApplyAlready)
			// 拒绝前只到「读实名 + 回填」这一步，查重都没开始。
			wantOps(t, tc.name+"拒绝序列", e.ops(0), []string{
				"cache.GetInt:realname_cap_err_times_70007",
				"cache.GetInt:realname_cap_code_70007",
				"cache.GetJSON:realname_info_70007",
				"realname.FindOne:70007",
				"cache.SetJSON:realname_info_70007/3600",
			})
			wantNoOpsWith(t, tc.name+"拒绝", e.ops(0), "applyImg.")
			wantNoOpsWith(t, tc.name+"拒绝", e.ops(0), "apply.")
			wantEQ(t, tc.name+"拒绝", "存量行没有被改写", e.st.realname.get(mid).CardMD5, "seedmd5"+itoa(int64(tc.status)))
		})
	}
}

func TestRealnameApplyReadThroughPutsPlaintextCardIntoRedis(t *testing.T) {
	// 读穿已通过的存量行时，realnameInfo 会用私钥解密并把明文塞进 real_card 再回填。
	// 本用例走通这条链并钉住三件事：
	//  1. 载荷里 card 是密文、real_card 是明文（**缺陷**：明文进缓存，见 README 已知缺口）；
	//  2. 被拒路径不删缓存 → 这份明文载荷会在 3600s TTL 内一直留在 Redis；
	//  3. 申请失败不会把新证件号写进任何地方。
	const mid = int64(70008)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	cipher := encryptCard(t, cardMaleAdult)
	e.st.realname.put(&model.RealnameInfo{
		Mid: mid, Realname: "张三", Status: model.RealnameApplyStatusPass,
		Card: cipher, CardMD5: "md5pass", Country: model.RealnameCountryChina,
		CardType: model.RealnameCardTypeIdentity,
	})

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardFemaleMinor))
	wantErrIs(t, "已通过不得重复申请", err, repository.ErrRealnameApplyAlready)

	cached, ok := cachedRealname(t, e, mid)
	wantEQ(t, "读穿回填", "缓存有值", ok, true)
	wantEQ(t, "读穿回填", "cached 哨兵", cached.Cached, true)
	wantEQ(t, "读穿回填", "card 字段是密文", cached.Card, cipher)
	wantEQ(t, "读穿回填", "status", int(cached.Status), model.RealnameApplyStatusPass)
	wantEQ(t, "读穿回填", "real_card 是解密结果", cached.RealCard, cardMaleAdult)
	// 行为哨兵：修好后（载荷剔除 RealCard 或对载荷单独加密）本段必须改成 wantNoPII。
	if !strings.Contains(e.st.cache.blobOf(keyRealname(mid)), cardMaleAdult) {
		t.Errorf("期望缓存载荷含明文证件号（现状），实际=%s", e.st.cache.blobOf(keyRealname(mid)))
	}
	wantNoOpsWith(t, "被拒路径不得失效缓存", e.ops(0), "cache.Del")
	wantEQ(t, "被拒路径", "新证件号没落任何库", len(e.st.apply.rows), 0)
}

func TestRealnameApplyRejectsCardBoundToAnotherMid(t *testing.T) {
	// 查重的键是 card_md5（含未导出盐）。用例不重算哈希，而是拿**同一次申请落库的
	// card_md5** 去布别人的存量行：期望值与实现同源，但「同证不同人」这条判定
	// 仍是被测行为，不是自证。
	const owner, intruder = int64(70009), int64(70010)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(owner), 111111)
	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(owner, 111111, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantNoErr(t, "首个申请", err)
	cardMD5 := e.st.apply.latest(owner).CardMD5

	e2 := newEnv(t)
	e2.st.cache.warmInt(keyCapCode(intruder), 111111)
	e2.st.realname.put(&model.RealnameInfo{
		Mid: owner, Status: model.RealnameApplyStatusPass, CardMD5: cardMD5,
	})
	l2 := NewRealnameApplyLogic(context.Background(), e2.svcCtx)
	_, err = l2.RealnameApply(applyReq(intruder, 111111, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantErrIs(t, "证件已被他人绑定", err, repository.ErrRealnameCardBindAlready)
	wantOpsLoose(t, "查重命中序列", e2.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70010",
		"cache.GetInt:realname_cap_code_70010",
		"cache.GetJSON:realname_info_70010",
		"realname.FindOne:70010",
		"cache.SetJSON:realname_info_70010/3600",
		"realname.FindOneByCardMD5:" + cardMD5,
	})
	wantEQ(t, "证件已被他人绑定", "申请单行数", len(e2.st.apply.rows), 0)
	wantEQ(t, "证件已被他人绑定", "图片行数", len(e2.st.applyImg.rows), 0)

	// 哈希键含 country：换个国家声明同一证件号就不再判重复——钉住键设计，
	// 免得有人把 cardMD5 改成只哈希证件号（那会让上面的绕过失效、也改变查重语义）。
	e3 := newEnv(t)
	e3.st.cache.warmInt(keyCapCode(intruder), 111111)
	e3.st.realname.put(&model.RealnameInfo{
		Mid: owner, Status: model.RealnameApplyStatusPass, CardMD5: cardMD5,
	})
	req := applyReq(intruder, 111111, model.RealnameCardTypeIdentity, cardMaleAdult)
	req.Country = 1
	_, err = NewRealnameApplyLogic(context.Background(), e3.svcCtx).RealnameApply(req)
	wantNoErr(t, "country 不同则不判重复", err)
	wantEQ(t, "country 不同则不判重复", "申请单行数", len(e3.st.apply.rows), 1)
}

func TestRealnameApplyFormatCheckRunsAfterImagesLeavingOrphans(t *testing.T) {
	const mid = int64(70011)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMalformed))
	wantErrIs(t, "身份证格式错误", err, repository.ErrRealnameCardNumErr)
	wantOpsLoose(t, "格式错误序列", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70011",
		"cache.GetInt:realname_cap_code_70011",
		"cache.GetJSON:realname_info_70011",
		"realname.FindOne:70011",
		"cache.SetJSON:realname_info_70011/3600",
		"realname.FindOneByCardMD5:~",
		"applyImg.Insert:1",
		"applyImg.Insert:2",
		"applyImg.Insert:3",
	})
	// blast radius：三张证件照已落库且没有事务回滚（申请单那条 INSERT 才是链路终点）。
	wantEQ(t, "格式错误的副作用", "孤儿图片行数", len(e.st.applyImg.rows), 3)
	wantEQ(t, "格式错误的副作用", "申请单行数", len(e.st.apply.rows), 0)
	// 验证码留着：用户改对号后可以立刻重试，不必重新领取。
	wantNoOpsWith(t, "格式错误", e.ops(0), "cache.Del")
}

func TestRealnameApplyTrailingEqualsBypassesCheckAndStoresPlaintext(t *testing.T) {
	// realname.go:419 `if !strings.HasSuffix(arg.CardCode, "=")`：以 "=" 结尾就当作
	// 「上游已加密」，**同时**跳过身份证格式校验与 RSA 加密，原样入库。
	// 后果：card_num 列里就是明文证件号，且畸形号也能进（README 已知缺口的复现证据）。
	const mid = int64(70012)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult+"="))
	wantNoErr(t, "以 = 结尾的证件号", err)
	row := e.st.apply.latest(mid)
	if row == nil {
		t.Fatal("申请单没有落库")
	}
	wantEQ(t, "= 结尾绕过加密", "card_num 原样等于入参", row.CardNum, cardMaleAdult+"=")
	if _, derr := decryptCard(row.CardNum); derr == nil {
		t.Error("期望 card_num 解不开（它不是密文），实际解出了内容")
	}
	// 同一旁路让格式非法的号也能入库（isIDCard 被一起跳过）。
	_, err = l.RealnameApply(applyReq(mid+1, 123456, model.RealnameCardTypeIdentity, cardMalformed+"="))
	wantErrIs(t, "mid+1 没领验证码所以止步于门禁", err, repository.ErrRealnameCaptureInvalid)
	wantEQ(t, "第二张单没进去", "申请单行数", len(e.st.apply.rows), 1)

	// 明文只出现在 card_num 这一列；其余列与载荷仍然干净。
	wantNoPII(t, "= 旁路后的其他列", []string{cardMaleAdult}, map[string]string{
		"card_md5": row.CardMD5, "img": e.st.applyImg.rows[1].IMGData,
	})
}

func TestRealnameApplyOversizedCardFailsEncryptWithoutEchoingIt(t *testing.T) {
	// CardType!=0 时不做格式校验，直接进 RSA。RSA-2048 + PKCS1v15 明文上限 245 字节，
	// 超长必然报错；关键是错误链与日志都不得把超长证件号回显出来。
	const mid = int64(70013)
	long := strings.Repeat("9", 300)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	logs := logtest.NewCollector(t)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, 1, long))
	if err == nil {
		t.Fatal("超长证件号期望报错，实际成功")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("期望 RSA 长度错误，实际 %v", err)
	}
	// 先证明捕获通道在工作，否则「日志里没有明文」会白过。
	if !strings.Contains(logs.String(), "RealnameApply") {
		t.Fatalf("日志捕获通道失效，没收到 logic 层那行：%q", logs.String())
	}
	wantNoPII(t, "错误与日志", []string{long[:40], long}, map[string]string{
		"error": err.Error(), "logx": logs.String(),
	})
	wantEQ(t, "超长证件号", "申请单行数", len(e.st.apply.rows), 0)
	wantEQ(t, "超长证件号", "孤儿图片行数", len(e.st.applyImg.rows), 3)
}

func TestRealnameApplyEncryptFailurePropagatesAndLeaksNoKeyMaterial(t *testing.T) {
	// CardEncrypt 的失败分支只能由**密钥**触发（crypto.go:31 pem.Decode 失败）。
	// 这里刻意用一把空公钥 PEM 作坏夹具——不是因为生产会配错，而是为了走到那条分支；
	// 全包其余实名用例都用运行时生成的真 PEM（见 fakes_test.go 的 mustTestCardKeyPEM），
	// 免得「加解密失败」被误读成业务分支结论。
	// 断言：错误如实上抛（不被吞）、图片成为孤儿、错误文本与日志都不带证件号。
	const mid = int64(70014)
	e := envWithCryptor(t, "", testPrivPEM)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	logs := logtest.NewCollector(t)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantEQ(t, "坏公钥", "错误文本", err.Error(), "user-profile/crypto: invalid public key pem")
	wantNoPII(t, "加密失败分支", []string{cardMaleAdult}, map[string]string{
		"error": err.Error(), "logx": logs.String(),
	})
	wantEQ(t, "加密失败分支", "申请单行数", len(e.st.apply.rows), 0)
	wantEQ(t, "加密失败分支", "孤儿图片行数", len(e.st.applyImg.rows), 3)
	// 失败路径同样不失效验证码。
	wantNoOpsWith(t, "加密失败分支", e.ops(0), "cache.Del")
}

func TestRealnameApplyImageInsertFailureAbortsAtTheFailingImage(t *testing.T) {
	const mid = int64(70015)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	boom := errors.New("boom: realname_apply_img 写不进去")
	e.st.applyImg.failOn("Insert", 2, boom) // 只让第二张失败

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantErrIs(t, "第二张图片写失败", err, boom)
	wantOpsLoose(t, "图片失败即中止", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70015",
		"cache.GetInt:realname_cap_code_70015",
		"cache.GetJSON:realname_info_70015",
		"realname.FindOne:70015",
		"cache.SetJSON:realname_info_70015/3600",
		"realname.FindOneByCardMD5:~",
		"applyImg.Insert:1",
		"applyImg.Insert:2",
	})
	// 第一张已落库、第三张不再尝试、申请单不写、验证码不删（与格式错误分支同口径）。
	wantEQ(t, "图片失败即中止", "图片行数", len(e.st.applyImg.rows), 1)
	wantEQ(t, "图片失败即中止", "申请单行数", len(e.st.apply.rows), 0)
	wantNoOpsWith(t, "图片失败即中止", e.ops(0), "cache.Del")
	// 失败的那次 Insert 没有回写 ID（真实 model 在 err 分支不读 LastInsertId）。
	wantEQ(t, "图片失败即中止", "第 2 行不存在", e.st.applyImg.rows[2] == nil, true)
}

func TestRealnameApplyInsertFailureKeepsSentinelCacheAndCapture(t *testing.T) {
	const mid = int64(70016)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	boom := errors.New("boom: realname_apply 写不进去")
	e.st.apply.failWith("Insert", boom)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantErrIs(t, "申请单写失败", err, boom)
	wantEQ(t, "申请单写失败", "图片仍落 3 行", len(e.st.applyImg.rows), 3)
	wantEQ(t, "申请单写失败", "最后一步是 apply.Insert",
		e.ops(0)[len(e.ops(0))-1], "apply.Insert:70016/1")
	// 失败时既不失效缓存也不删验证码：那条「未申请」哨兵继续活 3600s，
	// 用户随后查状态仍会读到「未申请」（行为哨兵，见 README 已知缺口）。
	wantNoOpsWith(t, "申请单写失败", e.ops(0), "cache.Del")
}

func TestRealnameApplyEmptyTokensSkipImageRows(t *testing.T) {
	// 生产的判空方式很特别：拼完前后缀后再与 "/idenfiles/.txt" 比对。
	// 所以「token 为空」才跳过，任何非空 token（含 "."、"x"）都会落一行。
	const mid = int64(70017)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	req := applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult)
	req.HandImgToken, req.FrontImgToken, req.BackImgToken = "", "", ""

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(req)
	wantNoErr(t, "三个 token 都为空", err)
	wantNoOpsWith(t, "token 为空则不落图片行", e.ops(0), "applyImg.")
	row := e.st.apply.latest(mid)
	if row == nil {
		t.Fatal("申请单没有落库")
	}
	wantEQ(t, "token 为空", "hand_img", row.HandIMG, int64(0))
	wantEQ(t, "token 为空", "front_img", row.FrontIMG, int64(0))
	wantEQ(t, "token 为空", "back_img", row.BackIMG, int64(0))
	wantEQ(t, "token 为空", "图片行数", len(e.st.applyImg.rows), 0)
}

func TestRealnameApplyPartialTokensAllocateOnlyPresentImages(t *testing.T) {
	// 只有 hand 有值 → 只插 1 行，front/back 列保持 0；
	// 图片 ID 只在真正插入时自增，因此「跳过」不会把后面的 ID 顶偏。
	const mid = int64(70018)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	req := applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult)
	req.FrontImgToken, req.BackImgToken = "", ""

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(req)
	wantNoErr(t, "只有手持照", err)
	wantOpsLoose(t, "只有手持照", e.ops(0), []string{
		"cache.GetInt:realname_cap_err_times_70018",
		"cache.GetInt:realname_cap_code_70018",
		"cache.GetJSON:realname_info_70018",
		"realname.FindOne:70018",
		"cache.SetJSON:realname_info_70018/3600",
		"realname.FindOneByCardMD5:~",
		"applyImg.Insert:1",
		"apply.Insert:70018/1",
		"cache.Del:realname_cap_code_70018",
		"cache.Del:realname_info_70018",
	})
	row := e.st.apply.latest(mid)
	wantEQ(t, "只有手持照", "hand_img", row.HandIMG, int64(1))
	wantEQ(t, "只有手持照", "front_img", row.FrontIMG, int64(0))
	wantEQ(t, "只有手持照", "back_img", row.BackIMG, int64(0))
}

func TestRealnameApplyReplayAccumulatesPendingRows(t *testing.T) {
	// 幂等/重放现状：门禁只看 realname_info（审核结果表），从不看 realname_apply，
	// 而 realname_apply 只有 idx_mid 普通索引（000008_create_realname.sql，无唯一键）。
	// 于是审核完成前重复提交会攒出多张 pending 单。
	// 行为哨兵（README 已知缺口）：改成也查 realname_apply 或加唯一键后，本用例必须变红。
	const mid = int64(70019)
	e := newEnv(t)
	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)

	e.st.cache.warmInt(keyCapCode(mid), 123456)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantNoErr(t, "第一次提交", err)

	// 上一轮成功后验证码被删；重新领取（静默布景）再提交一次。
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	_, err = l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantNoErr(t, "第二次提交也放行（现状）", err)

	wantEQ(t, "重放", "pending 申请单行数", len(e.st.apply.rows), 2)
	wantEQ(t, "重放", "图片行数", len(e.st.applyImg.rows), 6)
	first, second := e.st.apply.rows[0], e.st.apply.rows[1]
	wantEQ(t, "重放", "两张单同证（无幂等键）", second.CardMD5, first.CardMD5)
	wantEQ(t, "重放", "自增 ID 递增", second.ID > first.ID, true)
}

func TestRealnameApplyDoesNotGuardNonPositiveMid(t *testing.T) {
	// 实名簇没有 mid<=0 守卫（README 已知缺口 1 的写侧版本）：mid=0 能走完整个流程，
	// 落库行 mid=0，Redis key 是裸的 realname_cap_code_0。
	const mid = int64(0)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantNoErr(t, "mid=0 提交", err)
	row := e.st.apply.latest(mid)
	if row == nil {
		t.Fatal("mid=0 的申请单没有落库")
	}
	wantEQ(t, "mid=0 提交", "申请单 mid", row.Mid, int64(0))
	wantOpsLoose(t, "mid=0 的 key 形态", e.ops(0)[:2], []string{
		"cache.GetInt:realname_cap_err_times_0",
		"cache.GetInt:realname_cap_code_0",
	})
}

func TestRealnameApplyCTimeSetByRepositoryNeverReachesTheRow(t *testing.T) {
	// 仓库构造 apply 时写了 CTime=now，但生产 SQL 的列集里没有 ctime
	// （DDL 默认 0）→ 落库行 ctime 恒为 0，审核队列拿不到提交时刻。
	// 替身按同一列集裁剪（纪律 2），所以这条判的是生产 SQL，不是替身行为。
	const mid = int64(70021)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)

	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, cardMaleAdult))
	wantNoErr(t, "ctime 断言", err)
	row := e.st.apply.latest(mid)
	wantEQ(t, "生产 INSERT 未写 ctime", "ctime", row.CTime, int64(0))
	wantEQ(t, "生产 INSERT 未写 mtime", "mtime", row.MTime, int64(0))
	wantEQ(t, "img INSERT 未写 ctime", "图片行 ctime", e.st.applyImg.rows[1].CTime, int64(0))
	wantEQ(t, "operator 列未写", "operator", row.Operator, "")
}

func TestRealnameApplyCardTypeVariantsSkipIdentityCheck(t *testing.T) {
	// 格式校验只对 CardType=身份证(0) 生效：其它类型直接进 RSA。
	// 边界在于「护照号 10 位」不该被身份证正则误伤，也不该被放行成明文。
	const mid = int64(70022)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	l := NewRealnameApplyLogic(context.Background(), e.svcCtx)

	_, err := l.RealnameApply(applyReq(mid, 123456, 1, "E12345678"))
	wantNoErr(t, "护照类型跳过身份证格式校验", err)
	plain, derr := decryptCard(e.st.apply.latest(mid).CardNum)
	wantNoErr(t, "护照号解密", derr)
	wantEQ(t, "护照号回读", "card_num", plain, "E12345678")

	// 同一串按身份证类型提交则被拒（正则不因长度而放宽）。
	e.st.cache.warmInt(keyCapCode(mid), 123456)
	_, err = l.RealnameApply(applyReq(mid, 123456, model.RealnameCardTypeIdentity, "E12345678"))
	wantErrIs(t, "身份证类型下同一串非法", err, repository.ErrRealnameCardNumErr)
	wantEQ(t, "身份证类型下同一串非法", "申请单仍只有 1 行", len(e.st.apply.rows), 1)
}
