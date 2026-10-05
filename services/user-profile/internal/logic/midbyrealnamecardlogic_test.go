package logic

// midbyrealnamecardlogic_test.go 覆盖 MidByRealnameCard（repository/realname.go:297）：
// 审核/风控侧「拿一串证件号换 mid」的批量反查。
//
// 这个方法是实名簇里唯一**不读缓存**、也唯一把明文证件号当入参的读口，
// 于是它的断言面全在哈希与映射方向上：
//   - 哈希三元组：cardMD5 的键是 salt + 小写号串 + 证件类型 + 国家，
//     少拼一维就会「张冠李戴」或「查不到」；盐未导出，断言侧无法复现，
//     因此下面用 md5Oracle 让**生产实现自己**给出哈希再回塞进行里（oracle 只用于
//     「哪个入参算出哪把哈希」，映射方向/状态过滤/大小写/去重都与 oracle 无关）；
//   - 出口键：契约要求 code_to_mid 的键是**调用方给的原号串**，不是哈希；
//   - PII：号串是明文入参，所以本方法真正要防的是「明文被顺手写进 Redis 或日志」；
//   - 批次：一次请求 = 一条 SELECT，实现里没有分批、没有上限，靠用例钉住现状。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// md5Oracle 用生产实现算出各号串的哈希，并把这轮探测产生的轨迹**抹掉**
// （纪律 4：布景不得留在被测序列里）。返回 入参号串 → 哈希。
func md5Oracle(t *testing.T, e *env, country, cardType int32, codes ...string) map[string]string {
	t.Helper()
	from := e.st.log.snapshot()
	if _, err := NewMidByRealnameCardLogic(context.Background(), e.svcCtx).
		MidByRealnameCard(&rpc.MidByRealnameCardsReq{CardCode: codes, Country: country, CardType: cardType}); err != nil {
		t.Fatalf("取哈希 oracle 失败：%v", err)
	}
	ops := e.ops(from)
	const p = "realname.FindMidsByCardMD5s:"
	if len(ops) != 1 || !strings.HasPrefix(ops[0], p) {
		t.Fatalf("取哈希 oracle 的轨迹异常：%v", ops)
	}
	hashes := strings.Split(strings.TrimPrefix(ops[0], p), ",")
	if len(hashes) != len(codes) {
		t.Fatalf("oracle 哈希数 = %d, want %d", len(hashes), len(codes))
	}
	out := make(map[string]string, len(codes))
	for i, code := range codes {
		out[code] = hashes[i]
	}
	e.st.log.ops = e.st.log.ops[:from] // 探测归探测，别污染用例序列
	return out
}

// putRealnameByCardMD5 把一把哈希挂到某个 mid 的 realname_info 行上（模拟已实名用户）。
func putRealnameByCardMD5(e *env, mid int64, md5v string, status int8) {
	e.st.realname.put(&model.RealnameInfo{
		Mid: mid, Realname: "张三", Status: status, CardMD5: md5v,
		Country: model.RealnameCountryChina, CardType: model.RealnameCardTypeIdentity,
	})
}

func queryMids(t *testing.T, e *env, codes []string, country, cardType int32) (map[string]int64, error) {
	t.Helper()
	reply, err := NewMidByRealnameCardLogic(context.Background(), e.svcCtx).
		MidByRealnameCard(&rpc.MidByRealnameCardsReq{CardCode: codes, Country: country, CardType: cardType})
	if err != nil {
		return nil, err
	}
	return reply.GetCodeToMid(), nil
}

func TestMidByRealnameCardMapsHashedCodeBackToOriginalCode(t *testing.T) {
	e := newEnv(t)
	codes := []string{cardMaleAdult, cardFemaleMinor, "123456789012345678"}
	oracle := md5Oracle(t, e, model.RealnameCountryChina, model.RealnameCardTypeIdentity, codes...)
	putRealnameByCardMD5(e, 81001, oracle[cardMaleAdult], model.RealnameApplyStatusPass)
	putRealnameByCardMD5(e, 81002, oracle[cardFemaleMinor], model.RealnameApplyStatusPass)

	got, err := queryMids(t, e, codes, model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "批量反查", err)
	wantEQ(t, "批量反查", "命中数", len(got), 2)
	// 出口键必须是调用方原样给的号串：若实现把哈希当键吐出去，这里立刻红。
	wantEQ(t, "批量反查", "A → mid", got[cardMaleAdult], int64(81001))
	wantEQ(t, "批量反查", "B → mid", got[cardFemaleMinor], int64(81002))
	if _, ok := got[oracle[cardMaleAdult]]; ok {
		t.Errorf("反查结果里出现了以哈希为键的条目：%v", got)
	}
	if _, ok := got["123456789012345678"]; ok {
		t.Errorf("未注册的号串不该出现在结果里：%v", got)
	}
	// 一次请求一条 SELECT，且**完全不碰实名缓存**（反查不能依赖 realname_info_<mid>）。
	wantOps(t, "批量反查序列", e.ops(0), []string{
		"realname.FindMidsByCardMD5s:" + oracle[cardMaleAdult] + "," +
			oracle[cardFemaleMinor] + "," + oracle["123456789012345678"],
	})
	wantNoOpsWith(t, "批量反查", e.ops(0), "cache.")
	wantNoOpsWith(t, "批量反查", e.ops(0), "realname.FindOne:")
}

func TestMidByRealnameCardIsCaseInsensitiveOnTheWholeNumber(t *testing.T) {
	// cardMD5 对号串做了 ToLower：同一个号的两种写法必须落到同一把哈希。
	// 身份证末位可以是 x/X，审核侧大小写混用是常态，这里以带校验位的号为样本。
	const lower = "11010119900307121x"
	const upper = "11010119900307121X"
	e := newEnv(t)
	oracle := md5Oracle(t, e, model.RealnameCountryChina, model.RealnameCardTypeIdentity, lower)
	if oracle[lower] == "" {
		t.Fatal("oracle 返回了空哈希")
	}
	putRealnameByCardMD5(e, 81003, oracle[lower], model.RealnameApplyStatusPass)

	got, err := queryMids(t, e, []string{upper}, model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "大写末位", err)
	wantEQ(t, "大写末位", "用大写写法命中同一 mid", got[upper], int64(81003))

	// 两种写法同时查：各自成一个键、指向同一个 mid（结果按入参原样给回，不做归一）。
	got2, err := queryMids(t, e, []string{lower, upper}, model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "大小写同查", err)
	wantEQ(t, "大小写同查", "两个键", len(got2), 2)
	wantEQ(t, "大小写同查", "小写键", got2[lower], int64(81003))
	wantEQ(t, "大小写同查", "大写键", got2[upper], int64(81003))
}

func TestMidByRealnameCardHashBindsCardTypeAndCountry(t *testing.T) {
	// 哈希三元组里少拼任何一维都会「跨类型误命中」。这里以「同号码 + 不同国家」
	// 与「同号码 + 不同类型」各测一刀：注册在 (身份证, 中国) 的号，
	// 用 (身份证, 其他国家) 或 (护照, 中国) 去反查都必须查不到。
	e := newEnv(t)
	oracle := md5Oracle(t, e, model.RealnameCountryChina, model.RealnameCardTypeIdentity, cardMaleAdult)
	putRealnameByCardMD5(e, 81004, oracle[cardMaleAdult], model.RealnameApplyStatusPass)

	otherType, err := queryMids(t, e, []string{cardMaleAdult}, model.RealnameCountryChina, 1)
	wantNoErr(t, "换证件类型", err)
	wantEQ(t, "换证件类型", "不得命中", len(otherType), 0)

	otherCountry, err := queryMids(t, e, []string{cardMaleAdult}, 1, model.RealnameCardTypeIdentity)
	wantNoErr(t, "换国家", err)
	wantEQ(t, "换国家", "不得命中", len(otherCountry), 0)

	// 反证：三元组对齐时确实命中，排除「断言永远为真」。
	same, err := queryMids(t, e, []string{cardMaleAdult}, model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "同三元组", err)
	wantEQ(t, "同三元组", "命中", same[cardMaleAdult], int64(81004))

	// 三次请求 = 三条不同的哈希列表，说明维度真的进了哈希而不只是没被查。
	wantEQ(t, "三次请求", "SELECT 次数", e.st.log.countPrefix("realname.FindMidsByCardMD5s:"), 3)
	hashes := map[string]bool{}
	for _, op := range e.ops(0) {
		hashes[strings.TrimPrefix(op, "realname.FindMidsByCardMD5s:")] = true
	}
	wantEQ(t, "三次请求", "三种三元组算出三把不同哈希", len(hashes), 3)
}

func TestMidByRealnameCardOnlyCountsPendingAndPassBindings(t *testing.T) {
	// model/realname.go 的反查语句带 `AND status IN (0, 1)`：
	// 驳回(2)/未申请(3) 的历史绑定不参与反查，否则被驳回的号还能被风控捞出 mid。
	cases := []struct {
		name   string
		status int8
		mid    int64
		hit    bool
	}{
		{"审核中", model.RealnameApplyStatusPending, 82001, true},
		{"通过", model.RealnameApplyStatusPass, 82002, true},
		{"驳回", model.RealnameApplyStatusBack, 82003, false},
		{"未申请", model.RealnameApplyStatusNone, 82004, false},
		{"越界状态", 9, 82005, false},
	}
	e := newEnv(t)
	codes := make([]string, 0, len(cases))
	for i := range cases {
		// 每个状态配一个不同号串，避免同哈希在 map 里互相覆盖。
		codes = append(codes, cardMaleAdult[:15]+string(rune('A'+i)))
	}
	oracle := md5Oracle(t, e, model.RealnameCountryChina, model.RealnameCardTypeIdentity, codes...)
	for i, tc := range cases {
		putRealnameByCardMD5(e, tc.mid, oracle[codes[i]], tc.status)
	}

	got, err := queryMids(t, e, codes, model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "状态过滤", err)
	wantEQ(t, "状态过滤", "只有审核中/通过被捞回", len(got), 2)
	for i, tc := range cases {
		mid, ok := got[codes[i]]
		wantEQ(t, tc.name, "是否命中", ok, tc.hit)
		if tc.hit {
			wantEQ(t, tc.name, "mid", mid, tc.mid)
		}
	}
}

func TestMidByRealnameCardSendsDuplicateCodesWithoutDedup(t *testing.T) {
	// 入参去重没做：同一个号给两次，IN 列表里就是两把一样的哈希（多占位符、多带宽），
	// 但出口 map 仍只有一个键。钉住现状=把「将来加分批/去重」变成有测试约束的改动。
	e := newEnv(t)
	oracle := md5Oracle(t, e, model.RealnameCountryChina, model.RealnameCardTypeIdentity, cardMaleAdult)
	putRealnameByCardMD5(e, 81005, oracle[cardMaleAdult], model.RealnameApplyStatusPass)

	got, err := queryMids(t, e, []string{cardMaleAdult, cardMaleAdult},
		model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "重复入参", err)
	wantEQ(t, "重复入参", "结果去重后一条", len(got), 1)
	wantEQ(t, "重复入参", "mid", got[cardMaleAdult], int64(81005))
	wantOps(t, "重复入参序列", e.ops(0), []string{
		"realname.FindMidsByCardMD5s:" + oracle[cardMaleAdult] + "," + oracle[cardMaleAdult],
	})
}

func TestMidByRealnameCardEmptyAndNilCodesSkipSQL(t *testing.T) {
	cases := []struct {
		name  string
		codes []string
	}{
		{"nil 列表", nil},
		{"空列表", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			got, err := queryMids(t, e, tc.codes, model.RealnameCountryChina, model.RealnameCardTypeIdentity)
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "结果为空", len(got), 0)
			// model 的 `if len(args)==0 { return 空 }` 让这一步连 SQL 都不发（纪律 2 口径）。
			wantOps(t, tc.name+"序列", e.ops(0), []string{})
		})
	}
}

func TestMidByRealnameCardWhitespaceIsPartOfTheHash(t *testing.T) {
	// 只做 ToLower、不做 Trim：带空格的号串算出另一把哈希，于是查不到。
	// 网关侧若忘了 trim，反查会静默返回空 map（而不是报错），这条用例就是那个口径的证据。
	e := newEnv(t)
	oracle := md5Oracle(t, e, model.RealnameCountryChina, model.RealnameCardTypeIdentity,
		cardMaleAdult, " "+cardMaleAdult, cardMaleAdult+" ")
	putRealnameByCardMD5(e, 81006, oracle[cardMaleAdult], model.RealnameApplyStatusPass)

	wantEQ(t, "空格进哈希", "前后空格各算一把新哈希",
		len(map[string]bool{oracle[cardMaleAdult]: true, oracle[" "+cardMaleAdult]: true, oracle[cardMaleAdult+" "]: true}), 3)

	got, err := queryMids(t, e, []string{" " + cardMaleAdult, cardMaleAdult + " "},
		model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "带空格查询", err)
	wantEQ(t, "带空格查询", "一条都不命中", len(got), 0)
}

func TestMidByRealnameCardModelFailurePropagates(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("boom: realname_info 批量查询失败")
	e.st.realname.failWith("FindMidsByCardMD5s", boom)

	reply, err := NewMidByRealnameCardLogic(context.Background(), e.svcCtx).
		MidByRealnameCard(&rpc.MidByRealnameCardsReq{
			CardCode: []string{cardMaleAdult}, Country: model.RealnameCountryChina,
			CardType: model.RealnameCardTypeIdentity,
		})
	wantErrIs(t, "反查失败", err, boom)
	wantEQ(t, "反查失败", "reply 为 nil", reply == nil, true)
	wantNoOpsWith(t, "反查失败", e.ops(0), "cache.")
}

func TestMidByRealnameCardIgnoresWarmedRealnameCache(t *testing.T) {
	// 反着钉一次「不读缓存」：把该 mid 的 realname_info_<mid> 缓存布成完全另一条记录，
	// 结果仍来自 DB 行——若实现改成「先查缓存再回源」，这里就会偏。
	e := newEnv(t)
	oracle := md5Oracle(t, e, model.RealnameCountryChina, model.RealnameCardTypeIdentity, cardMaleAdult)
	putRealnameByCardMD5(e, 81007, oracle[cardMaleAdult], model.RealnameApplyStatusPass)
	e.st.cache.warmJSON(keyRealname(81007), realnameCacheView{
		Cached: true, Mid: 81007, Status: model.RealnameApplyStatusNone,
	})

	got, err := queryMids(t, e, []string{cardMaleAdult}, model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "缓存不参与", err)
	wantEQ(t, "缓存不参与", "仍按 DB 行命中", got[cardMaleAdult], int64(81007))
	wantOps(t, "缓存不参与序列", e.ops(0), []string{
		"realname.FindMidsByCardMD5s:" + oracle[cardMaleAdult],
	})
}

func TestMidByRealnameCardLargeBatchIsOneQueryWithoutChunking(t *testing.T) {
	// 现状：无分批、无条数上限——200 个号 = 一条 200 个占位符的 IN。
	// 这是「行为哨兵」（README 已知缺口）：真正的风险是 max_allowed_packet 与索引退化，
	// 若以后加分批或上限，本用例的「1 次查询 + 200 把哈希」必须同步改。
	const n = 200
	e := newEnv(t)
	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		codes = append(codes, cardMaleAdult[:15]+string(rune('A'+i%26))+itoa(int64(i)))
	}
	oracle := md5Oracle(t, e, model.RealnameCountryChina, model.RealnameCardTypeIdentity, codes...)
	// 只给其中 3 个号布上绑定行，验证结果集与入参规模无关。
	for i, idx := range []int{0, 99, 199} {
		putRealnameByCardMD5(e, 83000+int64(i), oracle[codes[idx]], model.RealnameApplyStatusPass)
	}

	got, err := queryMids(t, e, codes, model.RealnameCountryChina, model.RealnameCardTypeIdentity)
	wantNoErr(t, "大批量", err)
	wantEQ(t, "大批量", "命中数与入参规模无关", len(got), 3)
	wantEQ(t, "大批量", "SELECT 次数", e.st.log.countPrefix("realname.FindMidsByCardMD5s:"), 1)
	trace := e.ops(0)[0]
	wantEQ(t, "大批量", "占位符个数 = 入参个数（无去重无分批）",
		strings.Count(trace, ",")+1, n)
	for _, idx := range []int{0, 99, 199} {
		wantEQ(t, "大批量", "命中 mid", got[codes[idx]] > 0, true)
	}
	// 明文号串不得被顺手写进 Redis（本方法一个缓存键都不该碰）。
	blobs := map[string]string{}
	for key, vals := range e.st.cache.writes {
		blobs[key] = strings.Join(vals, "\n")
	}
	wantNoPII(t, "大批量", []string{cardMaleAdult}, blobs)
}
