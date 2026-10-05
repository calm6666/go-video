package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

// delReq 给一条「所有守卫都能过」的基准单条删除请求。
func delReq() *rpc.DeleteSearchHistoryReq {
	return &rpc.DeleteSearchHistoryReq{
		Mid: 88, Keyword: "开源软件", Confirm: true, RequestId: "req-del-1", TraceId: "trace-del-1",
	}
}

func doDeleteHistory(t *testing.T, st *store, in *rpc.DeleteSearchHistoryReq) (*rpc.DeleteSearchHistoryReply, error) {
	t.Helper()
	return NewDeleteSearchHistoryLogic(context.Background(), st.svcCtx()).DeleteSearchHistory(in)
}

// TestDeleteSearchHistoryRejectsInvalidRequestsBeforeAnyDependency 守卫表：
// 删除既要求「确认」也要求「定位得到那个词」——空/超长/纯控制字符的关键词一旦放行，
// 要么删不掉（用户以为已擦除，实际还在），要么按错误条件扩大删除面。全部在打库前拒。
func TestDeleteSearchHistoryRejectsInvalidRequestsBeforeAnyDependency(t *testing.T) {
	longKeyword := strings.Repeat("词", repository.MaxKeywordRunes+1) // 规范化后仍超过 MaxKeywordRunes 兜底
	ok := delReq()

	cases := []struct {
		name  string
		req   *rpc.DeleteSearchHistoryReq
		want  error
		cause string
	}{
		{"空请求", nil, model.ErrInvalidMid, "in==nil 与非法 mid 同码"},
		{"游客 mid=0", bad(ok, func(r *rpc.DeleteSearchHistoryReq) { r.Mid = 0 }), model.ErrInvalidMid, "无归属即无从删除"},
		{"负 mid", bad(ok, func(r *rpc.DeleteSearchHistoryReq) { r.Mid = -88 }), model.ErrInvalidMid, "负 mid 不是用户"},
		{"未二次确认", bad(ok, func(r *rpc.DeleteSearchHistoryReq) { r.Confirm = false }), model.ErrConfirmRequired,
			"confirm=false 不产生任何写入（网关 UI 必须显式二次确认）"},
		{"空关键词", bad(ok, func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "" }), model.ErrInvalidKeyword, "关键词必填"},
		{"纯空白关键词", bad(ok, func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = " \t\n " }), model.ErrInvalidKeyword,
			"规范化后为空：放行会按空哈希误删"},
		{"纯控制字符关键词", bad(ok, func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "\x01\x02\x7f" }), model.ErrInvalidKeyword,
			"清洗后为空，不允许把控制字符当定位条件"},
		{"超过配置上限的关键词", bad(ok, func(r *rpc.DeleteSearchHistoryReq) {
			r.Keyword = strings.Repeat("词", 65) // cfg.KeywordMaxLen=64
		}), model.ErrInvalidKeyword, "配置上限先于兜底上限生效"},
		{"超过兜底上限的关键词", bad(ok, func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = longKeyword }),
			model.ErrInvalidKeyword, "keyword 列 varchar(128)，再长必被截断定位"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "android", nowMinus(300), nowMinus(60))

			reply, err := doDeleteHistory(t, st, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：被拒的请求不应返回响应体，实际 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, st, 0)
			if row, exists := st.history.get(88, "开源软件"); !exists {
				t.Errorf("%s：守卫拒绝后本人历史消失：%v", tc.name, row)
			}
		})
	}
}

// TestDeleteSearchHistoryErasesTheNormalizedWordOfThatUserOnly 正常路径逐字段投影 + 三条作用域不变量：
//  1. 待删词按查询链路同一套规则规范化后再取哈希 —— 库里存的是规范化词，
//     不规范化就定位不到，接口会「成功返回 deleted=0」而用户的数据其实还在；
//  2. 删除按 (mid, keyword_hash) 精确定位，同关键词的他人记录一行都不能少；
//  3. 只删被点名的那个词，同用户的其它历史保持原样。
func TestDeleteSearchHistoryErasesTheNormalizedWordOfThatUserOnly(t *testing.T) {
	st := newStore(t, testConfig())
	st.history.seedRow(88, "开源 软件", model.HistoryStateNormal, "android", nowMinus(900), nowMinus(100))
	st.history.seedRow(88, "番剧推荐", model.HistoryStateNormal, "ios", nowMinus(800), nowMinus(90))
	foreigner := st.history.seedRow(99, "开源 软件", model.HistoryStateNormal, "desktop", nowMinus(700), nowMinus(80))
	other, existed := st.history.get(88, "番剧推荐")
	if !existed {
		t.Fatalf("布景失败：同用户的另一行不存在")
	}

	// 客户端送来的是带两端空白/重复空格/控制字符的原始词，必须落到同一行上。
	req := bad(delReq(), func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "  开源   软件\x07 " })
	reply, err := doDeleteHistory(t, st, req)
	wantNoErr(t, "删除单条历史", err)
	wantEQ(t, "删除单条历史", "Deleted", reply.Deleted, int64(1))
	wantEQ(t, "删除单条历史", "HardDeleted（物理删除，无软删保留）", reply.HardDeleted, true)
	// 顺序：只有一次删除，键里带的是规范化后的词，且作用域限定在 mid=88。
	wantOps(t, "删除单条历史调用序列", st.log.ops, []string{"history.DeleteKeyword:88/开源 软件"})

	if _, exists := st.history.get(88, "开源 软件"); exists {
		t.Errorf("删除单条历史：本人的行仍在，擦除未生效")
	}
	row, exists := st.history.get(99, "开源 软件")
	if !exists {
		t.Fatalf("删除单条历史：同关键词的他人记录被删（越权擦除）")
	}
	wantEQ(t, "他人记录不受影响", "Mid", row.Mid, int64(99))
	wantEQ(t, "他人记录不受影响", "Platform", row.Platform, "desktop")
	wantEQ(t, "他人记录不受影响", "Ctime", row.Ctime, foreigner.Ctime)
	wantEQ(t, "他人记录不受影响", "Mtime", row.Mtime, foreigner.Mtime)
	wantEQ(t, "他人记录不受影响", "KeywordHash", row.KeywordHash, model.KeywordHash("开源 软件"))
	if after, ok := st.history.get(88, "番剧推荐"); !ok {
		t.Errorf("删除单条历史：同用户的另一行被连带删除")
	} else {
		wantEQ(t, "未点名的行保持原样", "Id", after.Id, other.Id)
		wantEQ(t, "未点名的行保持原样", "Ctime", after.Ctime, other.Ctime)
		wantEQ(t, "未点名的行保持原样", "Mtime", after.Mtime, other.Mtime)
		wantEQ(t, "未点名的行保持原样", "State", after.State, model.HistoryStateNormal)
	}
	wantEQ(t, "删除单条历史", "本人剩余行数", st.history.countBy(88), 1)
}

// TestDeleteSearchHistoryIsIdempotentStateAgnosticAndNeverGated 四条语义：
//   - 幂等：重复删除同一词返回 deleted=0 且仍算成功（不把「已经删过」当错误）；
//   - 不分状态：运营标记/待清理行同样能被擦除（DeleteKeyword 不带 state 条件）；
//   - 查无此词：deleted=0 是成功而不是 ErrHistoryNotFound（对外不泄露「你有没有搜过」）；
//   - 不受 Search.HistoryEnabled 限制：擦除能力必须始终可用。
func TestDeleteSearchHistoryIsIdempotentStateAgnosticAndNeverGated(t *testing.T) {
	cfg := testConfig()
	cfg.Search.HistoryEnabled = false
	st := newStore(t, cfg)
	st.history.seedRow(88, "番剧 推荐", model.HistoryStateFlagged, "android", nowMinus(900), nowMinus(100))
	st.history.seedRow(88, "深夜电台", model.HistoryStateTombstone, "ios", nowMinus(800), nowMinus(90))

	first, err := doDeleteHistory(t, st, bad(delReq(), func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "番剧 推荐" }))
	wantNoErr(t, "删除被运营标记的行", err)
	wantEQ(t, "删除被运营标记的行", "Deleted", first.Deleted, int64(1))
	wantEQ(t, "删除被运营标记的行", "HardDeleted", first.HardDeleted, true)

	second, err := doDeleteHistory(t, st, bad(delReq(), func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "番剧 推荐" }))
	wantNoErr(t, "重复删除", err)
	wantEQ(t, "重复删除", "Deleted", second.Deleted, int64(0))
	wantEQ(t, "重复删除", "HardDeleted", second.HardDeleted, true)

	third, err := doDeleteHistory(t, st, bad(delReq(), func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "从未搜过的词" }))
	wantNoErr(t, "查无此词", err)
	wantEQ(t, "查无此词", "Deleted", third.Deleted, int64(0))

	forth, err := doDeleteHistory(t, st, bad(delReq(), func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "深夜电台" }))
	wantNoErr(t, "删除待清理行", err)
	wantEQ(t, "删除待清理行", "Deleted", forth.Deleted, int64(1))

	wantOps(t, "四次删除的调用序列", st.log.ops, []string{
		"history.DeleteKeyword:88/番剧 推荐",
		"history.DeleteKeyword:88/番剧 推荐",
		"history.DeleteKeyword:88/从未搜过的词",
		"history.DeleteKeyword:88/深夜电台",
	})
	wantEQ(t, "四次删除后", "本人剩余行数", st.history.countBy(88), 0)
}

// TestDeleteSearchHistoryPropagatesFailure 删除失败如实传出：
// 错误不被吞、不返回伪造的 deleted=0 成功，且行必须还在库里（否则客户端会以为已擦除）。
func TestDeleteSearchHistoryPropagatesFailure(t *testing.T) {
	st := newStore(t, testConfig())
	boom := errors.New("search_history DeleteKeyword: deadlocks found")
	st.history.failWith("DeleteKeyword", boom)
	st.history.seedRow(88, "开源 软件", model.HistoryStateNormal, "android", nowMinus(900), nowMinus(100))
	st.history.seedRow(99, "开源 软件", model.HistoryStateNormal, "desktop", nowMinus(800), nowMinus(90))

	reply, err := doDeleteHistory(t, st, bad(delReq(), func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "开源 软件" }))
	wantErrIs(t, "删除失败", err, boom)
	if reply != nil {
		t.Errorf("删除失败：不得返回成功响应（deleted=%d hard=%v）", reply.Deleted, reply.HardDeleted)
	}
	wantOps(t, "删除失败的调用序列", st.log.ops, []string{"history.DeleteKeyword:88/开源 软件"})
	if _, exists := st.history.get(88, "开源 软件"); !exists {
		t.Errorf("删除失败：行却被删掉了（半截数据）")
	}
	if _, exists := st.history.get(99, "开源 软件"); !exists {
		t.Errorf("删除失败：他人行被连带删除")
	}
}

// TestDeleteSearchHistoryErasesByExactNormalizedWordNotCaseFolded 钉住「删除定位键」：
// 擦除走 (mid, keyword_hash)，哈希取自**逐字节**的规范化词（只折叠空白、去控制字符），
// 本层不做大小写折叠，所以 "Apple" 与 "apple" 是两个不同的擦除目标，删一个不得连带删另一个。
//
// 与写入侧的键**不一致**，已登记为缺陷：迁移 SQL 里 search_history 的唯一索引是
// uniq_mid_keyword(mid, keyword)，keyword 为 VARCHAR(128) 且未指定 COLLATE
// （utf8mb4 默认在 MySQL 8 是 *_ci），真库里大小写异形会折叠成一行，
// 而删除按精确哈希定位 -> 返回 deleted=0 的「静默擦不掉」。
// 内存替身按字节精确匹配（等价 BINARY 排序规则），所以本用例只证明「本层不折叠」，
// 折叠后果无法在纯内存测试里复现，见 README「测试 / 已知缺口」。
func TestDeleteSearchHistoryErasesByExactNormalizedWordNotCaseFolded(t *testing.T) {
	st := newStore(t, testConfig())
	upperCtime, upperMtime := nowMinus(900), nowMinus(500)
	st.history.seedRow(88, "Apple", model.HistoryStateNormal, "ios", upperCtime, upperMtime)
	st.history.seedRow(88, "apple", model.HistoryStateNormal, "android", nowMinus(800), nowMinus(100))
	lowerHash := model.KeywordHash("apple")

	// 两端空白先被规范化裁掉，定位键是 hash("apple") 而不是 hash(" apple ")。
	reply, err := doDeleteHistory(t, st, bad(delReq(), func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = " apple " }))
	wantNoErr(t, "删除小写形态", err)
	wantEQ(t, "删除小写形态", "Deleted", reply.Deleted, int64(1))
	wantEQ(t, "删除小写形态", "HardDeleted", reply.HardDeleted, true)
	wantOps(t, "删除小写形态调用序列", st.log.ops, []string{"history.DeleteKeyword:88/apple"})
	if _, still := st.history.get(88, "apple"); still {
		t.Errorf("删除小写形态：精确匹配的那行没被擦掉")
	}

	kept, still := st.history.get(88, "Apple")
	if !still {
		t.Fatalf("大小写异形的大写行被连带删除（说明实现或替身做了折叠，隐私口径变了）")
	}
	wantEQ(t, "大写行不受影响", "Keyword", kept.Keyword, "Apple")
	wantEQ(t, "大写行不受影响", "Platform", kept.Platform, "ios")
	wantEQ(t, "大写行不受影响", "State", kept.State, int32(model.HistoryStateNormal))
	wantEQ(t, "大写行不受影响", "Ctime", kept.Ctime, upperCtime)
	wantEQ(t, "大写行不受影响", "Mtime", kept.Mtime, upperMtime)
	wantEQ(t, "大写行不受影响", "KeywordHash 由逐字节规范化词决定", kept.KeywordHash, model.KeywordHash("Apple"))
	if kept.KeywordHash == lowerHash {
		t.Errorf("大小写异形的两行哈希相同（%s）：sha256 折叠了大小写，删除会连带擦除他人意图的行", kept.KeywordHash)
	}

	// 第三种形态（全大写）定位不到任何行：deleted=0 且仍算成功、不报错、也不碰既有行 ——
	// 这正是 MySQL *_ci 库里「两形一行」会踩到的静默擦不掉，缺陷登记见函数注释。
	third, err := doDeleteHistory(t, st, bad(delReq(), func(r *rpc.DeleteSearchHistoryReq) { r.Keyword = "APPLE" }))
	wantNoErr(t, "删除全大写形态", err)
	wantEQ(t, "删除全大写形态", "Deleted（定位不到，不报错）", third.Deleted, int64(0))
	wantOpsAt(t, "删除全大写形态", st, 1, "history.DeleteKeyword:88/APPLE")
	if _, still := st.history.get(88, "Apple"); !still {
		t.Errorf("删除全大写形态：定位不到的删除把 Apple 那一行也删掉了")
	}
	// 剩余行只有大写那一行：小写行确实物理消失（无软删残留）。
	wantEQ(t, "两次删除后本人剩余行数", "count", int64(st.history.countBy(88)), int64(1))
}
