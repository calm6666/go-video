package logic

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

// 本文件覆盖 BlockWord（运营侧屏蔽词新增/停用/删除）。
//
// 屏蔽词的领域定位（deploy/migrations/danmaku/000002 + model/danmaku_blockword.go）：
// 它是**发送侧词库**，作用域只有「全局」和「分区」两档，没有「某个用户的词库」这一层
// （用户个人屏蔽归 danmaku_user_block / UserBlock）。因此本域要钉的是：
//  1. 匿名写必须被拒（operator_mid > 0），且 operator 列要如实记下**最近一次**操作者；
//  2. 词条以 word 为唯一键（uniq_word）：重复 ADD 走 upsert 不产生新行，停用后重新启用不换 ID；
//  3. 停用是软状态（state=0，行保留便于审计），删除是物理删行——两者对调用方的回复必须可辨；
//  4. 词库改动必须失效 Redis 词库缓存，且失效范围要覆盖所有受影响的作用域视图。
//     第 4 条正是本方法当前的缺口所在（见 TestBlockWordGlobalChangeLeavesPartitionCachesStale）。

const (
	bwOperator = int64(9001) // 运营/管理员
	bwOid      = int64(1001) // 分区词所属内容
	bwOtherOid = int64(2002) // 另一个分区
)

// --- 轨迹期望 ---

func bwUpsertOp(word string) string  { return "blockword.Upsert:" + word }
func bwFindOneOp(word string) string { return "blockword.FindOne:" + word }
func bwDisableOp(word string) string { return "blockword.Disable:" + word }
func bwDeleteOp(word string) string  { return "blockword.Delete:" + word }

// bwDelCacheOp 复刻 repository.invalidateBlockWordCache → Cache.DelBlockWords 的键集合：
// 恒删 dm:bw:0，oid>0 时再删 dm:bw:<oid>（repository/cache.go:312-319）。
func bwDelCacheOp(oid int64) string {
	keys := fmt.Sprintf(keyBlockWord, 0)
	if oid > 0 {
		keys += "," + fmt.Sprintf(keyBlockWord, oid)
	}
	return "cache.DelBW:" + keys
}

// --- 布景与调用 ---

func callBlockWord(t *testing.T, e *env, in *rpc.BlockWordReq) (*rpc.BlockWordReply, error) {
	t.Helper()
	return NewBlockWordLogic(context.Background(), e.svcCtx).BlockWord(in)
}

func bwAdd(word string, scope rpc.BlockWordScope, oid int64) *rpc.BlockWordReq {
	return &rpc.BlockWordReq{
		Action: rpc.BlockWordAction_BLOCK_WORD_ADD, Word: word,
		Scope: scope, Oid: oid, OperatorMid: bwOperator, TraceId: "trace-bw",
	}
}

func bwAction(action rpc.BlockWordAction, word string, scope rpc.BlockWordScope, oid int64) *rpc.BlockWordReq {
	return &rpc.BlockWordReq{
		Action: action, Word: word, Scope: scope, Oid: oid,
		OperatorMid: bwOperator, TraceId: "trace-bw",
	}
}

// bwRow 读库里那一行（副本，断言用）。词不存在即 fatal：调用方前提没成立。
func bwRow(t *testing.T, e *env, word string) *model.BlockWord {
	t.Helper()
	row, ok := e.st.blockWord.rows[word]
	if !ok {
		t.Fatalf("前置条件不成立：屏蔽词 %q 不在库里", word)
	}
	cp := *row
	return &cp
}

func bwHas(e *env, word string) bool { _, ok := e.st.blockWord.rows[word]; return ok }

// bwBackdate 把库里那行的 ctime/mtime 改成过去的定值（静默改，不记轨迹）。
// 不改的话「mtime 被刷新」这条断言会因为 Unix 秒粒度在同一秒内执行而随机变红。
func bwBackdate(e *env, word string, ctime, mtime int64) {
	row, ok := e.st.blockWord.rows[word]
	if !ok {
		panic("布景前置条件不成立：屏蔽词 " + word + " 不在库里")
	}
	row.Ctime, row.Mtime = ctime, mtime
}

// --- 守卫 ---

// TestBlockWordRejectsInvalidRequests 守卫表：所有非法入参都必须在碰任何依赖之前被拒。
// 屏蔽词是运营写接口，一次非法请求都不该产生「先查一遍词表再决定认不认」的读放大，
// 更不该顺手把词库缓存清掉。
func TestBlockWordRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.BlockWordReq
		want error
	}{
		{
			name: "operator 为 0（匿名写）",
			in:   &rpc.BlockWordReq{Action: rpc.BlockWordAction_BLOCK_WORD_ADD, Word: "广告", Scope: rpc.BlockWordScope_SCOPE_GLOBAL},
			want: model.ErrOperatorRequired,
		},
		{
			name: "operator 为负",
			in:   &rpc.BlockWordReq{Action: rpc.BlockWordAction_BLOCK_WORD_ADD, Word: "广告", Scope: rpc.BlockWordScope_SCOPE_GLOBAL, OperatorMid: -1},
			want: model.ErrOperatorRequired,
		},
		{
			name: "operator 非法优先于词非法",
			in:   &rpc.BlockWordReq{Action: rpc.BlockWordAction_BLOCK_WORD_ADD, Word: "  ", Scope: rpc.BlockWordScope_SCOPE_UNSPECIFIED},
			want: model.ErrOperatorRequired,
		},
		{
			name: "空词",
			in:   bwAdd("", rpc.BlockWordScope_SCOPE_GLOBAL, 0),
			want: model.ErrInvalidBlockWord,
		},
		{
			name: "纯空白词（TrimSpace 后为空）",
			in:   bwAdd("   \t ", rpc.BlockWordScope_SCOPE_GLOBAL, 0),
			want: model.ErrInvalidBlockWord,
		},
		{
			name: "词长 65 字符（超 word 列宽 VARCHAR(64)）",
			in:   bwAdd(strings.Repeat("词", 65), rpc.BlockWordScope_SCOPE_GLOBAL, 0),
			want: model.ErrInvalidBlockWord,
		},
		{
			name: "作用域未指定",
			in:   bwAdd("广告", rpc.BlockWordScope_SCOPE_UNSPECIFIED, 0),
			want: model.ErrInvalidBlockWord,
		},
		{
			name: "未知作用域取值",
			in:   bwAdd("广告", rpc.BlockWordScope(7), 0),
			want: model.ErrInvalidBlockWord,
		},
		{
			name: "分区词不带 oid",
			in:   bwAdd("广告", rpc.BlockWordScope_SCOPE_OID, 0),
			want: model.ErrInvalidOid,
		},
		{
			name: "分区词 oid 为负",
			in:   bwAdd("广告", rpc.BlockWordScope_SCOPE_OID, -3),
			want: model.ErrInvalidOid,
		},
		{
			name: "作用域非法优先于动作非法",
			in:   bwAction(rpc.BlockWordAction_BLOCK_WORD_UNSPECIFIED, "广告", rpc.BlockWordScope_SCOPE_UNSPECIFIED, 0),
			want: model.ErrInvalidBlockWord,
		},
		{
			name: "动作未指定",
			in:   bwAction(rpc.BlockWordAction_BLOCK_WORD_UNSPECIFIED, "广告", rpc.BlockWordScope_SCOPE_GLOBAL, 0),
			want: model.ErrInvalidBlockWord,
		},
		{
			name: "未知动作取值",
			in:   bwAction(rpc.BlockWordAction(9), "广告", rpc.BlockWordScope_SCOPE_GLOBAL, 0),
			want: model.ErrInvalidBlockWord,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmBlockWords(0, "旧全局词")
			e.st.cache.warmBlockWords(bwOid, "旧分区词")
			before := e.st.log.snapshot()

			reply, err := callBlockWord(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法请求仍返回 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			// 守卫阶段不许把词库缓存清掉（否则畸形请求成了打缓存的工具）
			_, g0 := e.st.cache.blockWordSet(0)
			_, gOid := e.st.cache.blockWordSet(bwOid)
			wantEQ(t, tc.name, "dm:bw:0 仍在", g0, true)
			wantEQ(t, tc.name, "dm:bw:<oid> 仍在", gOid, true)
			wantEQ(t, tc.name, "词表行数", len(e.st.blockWord.rows), 0)
		})
	}
}

// TestBlockWordAddStores64CharBoundaryAtLimit 钉住长度门禁的边界：
// 64 个字符放行、65 个字符拒绝（与 danmaku_blockword.word VARCHAR(64) 同口径，
// 且按 rune 计数，中文一个字符算 1 而不是 3 字节）。
func TestBlockWordAddStores64CharBoundaryAtLimit(t *testing.T) {
	cases := []struct {
		name    string
		word    string
		wantErr error
	}{
		{"恰好 64 个汉字", strings.Repeat("词", 64), nil},
		{"恰好 64 个 ASCII", strings.Repeat("a", 64), nil},
		{"65 个汉字", strings.Repeat("词", 65), model.ErrInvalidBlockWord},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, err := callBlockWord(t, e, bwAdd(tc.word, rpc.BlockWordScope_SCOPE_GLOBAL, 0))
			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				wantEQ(t, tc.name, "被拒时不入库", bwHas(e, tc.word), false)
				return
			}
			wantNoErr(t, tc.name, err)
			row := bwRow(t, e, tc.word)
			wantEQ(t, tc.name, "rune 数", len([]rune(row.Word)), 64)
		})
	}
}

// --- 新增 ---

// TestBlockWordAddGlobalWordPersistsRowAndInvalidatesCache 新增全局词的正常路径：
// 逐字段钉死落库行 + 回复 + 缓存失效范围。
func TestBlockWordAddGlobalWordPersistsRowAndInvalidatesCache(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmBlockWords(0, "陈旧全局词")
	e.st.cache.warmBlockWords(bwOid, "陈旧分区词")
	calledAt := time.Now().Unix()

	reply, err := callBlockWord(t, e, bwAdd("  垃圾广告  ", rpc.BlockWordScope_SCOPE_GLOBAL, bwOtherOid))
	wantNoErr(t, "新增全局词", err)
	if reply == nil {
		t.Fatal("新增全局词：回复 = nil, want 非空")
	}

	// 回复：新行主键 + 生效态。注意 in.Oid 在 GLOBAL 分支被丢弃（不是本分区词）。
	wantEQ(t, "新增全局词", "word_id", reply.WordId, int64(501))
	wantEQ(t, "新增全局词", "state", reply.State, model.BlockWordEnabled)

	// 落库行逐字段
	row := bwRow(t, e, "垃圾广告")
	wantEQ(t, "新增全局词", "word_id", row.WordID, int64(501))
	wantEQ(t, "新增全局词", "word（TrimSpace 后入库）", row.Word, "垃圾广告")
	wantEQ(t, "新增全局词", "scope", row.Scope, model.ScopeGlobal)
	wantEQ(t, "新增全局词", "oid（全局词恒 0，不采信入参）", row.Oid, int64(0))
	wantEQ(t, "新增全局词", "state", row.State, model.BlockWordEnabled)
	wantEQ(t, "新增全局词", "operator", row.Operator, bwOperator)
	if row.Ctime < calledAt || row.Ctime > calledAt+2 {
		t.Errorf("新增全局词：ctime = %d, want ≈ %d", row.Ctime, calledAt)
	}
	wantEQ(t, "新增全局词", "mtime == ctime（新建）", row.Mtime, row.Ctime)
	wantEQ(t, "新增全局词", "词表行数", len(e.st.blockWord.rows), 1)
	// 前后空白被裁掉了，不能留下第二个键
	wantEQ(t, "新增全局词", "带空格的原始词没有另开一行", bwHas(e, "  垃圾广告  "), false)

	wantOps(t, "新增全局词", e.ops(), []string{bwUpsertOp("垃圾广告"), bwDelCacheOp(0)})
}

// TestBlockWordAddPartitionWordInvalidatesBothViews 新增分区词：
// 分区视图与全局视图都要失效（发送侧词库是「全局 + 本分区」两段拼出来的，
// 见 repository.BlockWordFilter → loadBlockWords(0) + loadBlockWords(oid)）。
func TestBlockWordAddPartitionWordInvalidatesBothViews(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmBlockWords(0, "全局词")
	e.st.cache.warmBlockWords(bwOid, "旧分区词")

	reply, err := callBlockWord(t, e, bwAdd("分区黑话", rpc.BlockWordScope_SCOPE_OID, bwOid))
	wantNoErr(t, "新增分区词", err)
	wantEQ(t, "新增分区词", "word_id", reply.WordId, int64(501))
	wantEQ(t, "新增分区词", "state", reply.State, model.BlockWordEnabled)

	row := bwRow(t, e, "分区黑话")
	wantEQ(t, "新增分区词", "scope", row.Scope, model.ScopeOid)
	wantEQ(t, "新增分区词", "oid", row.Oid, bwOid)

	wantOps(t, "新增分区词", e.ops(), []string{bwUpsertOp("分区黑话"), bwDelCacheOp(bwOid)})
	_, g0 := e.st.cache.blockWordSet(0)
	_, gOid := e.st.cache.blockWordSet(bwOid)
	wantEQ(t, "新增分区词", "dm:bw:0 已失效", g0, false)
	wantEQ(t, "新增分区词", "dm:bw:<oid> 已失效", gOid, false)
	// 其它分区的视图不该被牵连（只在本次改动的作用域内失效）
	e.st.cache.warmBlockWords(bwOtherOid, "别人的分区词")
	_, gOther := e.st.cache.blockWordSet(bwOtherOid)
	wantEQ(t, "新增分区词", "别的分区缓存不动", gOther, true)
}

// TestBlockWordGlobalChangeLeavesPartitionCachesStale 钉住现状缺陷：
// dm:bw:<oid> 里存的是 ListEnabled(oid) 的结果，即「全局词 + 本分区词」拼出来的完整词库
// （model/danmaku_blockword.go:125-144），所以**全局词也躺在每个分区的缓存里**。
// 但 invalidateBlockWordCache 对全局改动只删 dm:bw:0（repository/cache.go:312-319），
// 各分区缓存要等 BlockWordCacheTTLSeconds（etc 缺省 300s）过期才会回源。
// 后果：新增一条全局辱骂词后各分区最长 300s 继续漏拦；停用/删除后最长 300s 继续误拦。
// 缺陷登记进 README 已知缺口；修法落地时本用例应改为断言分区键也被删。
func TestBlockWordGlobalChangeLeavesPartitionCachesStale(t *testing.T) {
	e := newEnv(t)
	seedBlockWord(t, e.st, &model.BlockWord{Word: "全局脏话", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: bwOperator})
	// 各分区缓存里都含这个全局词（ListEnabled(oid) 返回全局 + 本分区）
	e.st.cache.warmBlockWords(0, "全局脏话")
	e.st.cache.warmBlockWords(bwOid, "全局脏话", "本分区词")
	e.st.cache.warmBlockWords(bwOtherOid, "全局脏话", "别家分区词")

	reply, err := callBlockWord(t, e, bwAction(rpc.BlockWordAction_BLOCK_WORD_DISABLE, "全局脏话", rpc.BlockWordScope_SCOPE_GLOBAL, 0))
	wantNoErr(t, "停用全局词", err)
	wantEQ(t, "停用全局词", "state", reply.State, model.BlockWordDisabled)

	wantOps(t, "停用全局词", e.ops(), []string{bwFindOneOp("全局脏话"), bwDisableOp("全局脏话"), bwDelCacheOp(0)})
	_, g0 := e.st.cache.blockWordSet(0)
	wantEQ(t, "停用全局词", "dm:bw:0 已失效", g0, false)
	stale, okOid := e.st.cache.blockWordSet(bwOid)
	wantEQ(t, "停用全局词", "分区缓存键仍在", okOid, true)
	// 下面这条断言本身就是缺陷证据：分区视图里那个已停用的全局词还在。
	wantStringsEQ(t, "停用全局词", "分区缓存残留已停用的全局词", stale, []string{"全局脏话", "本分区词"})
	otherStale, _ := e.st.cache.blockWordSet(bwOtherOid)
	wantStringsEQ(t, "停用全局词", "别家分区同样残留", otherStale, []string{"全局脏话", "别家分区词"})
}

// TestBlockWordAddIsIdempotentOnUniqueWordKey uniq_word 口径：
// 同一词重复 ADD 不产生新行、不换主键、ctime 不动、mtime 刷新、operator 记最新；
// 已停用的词被 ADD 重新启用（迁移注释明写的语义）。
func TestBlockWordAddIsIdempotentOnUniqueWordKey(t *testing.T) {
	e := newEnv(t)
	seeded := seedBlockWord(t, e.st, &model.BlockWord{
		Word: "刷屏词", Scope: model.ScopeOid, Oid: bwOid, State: model.BlockWordDisabled, Operator: 8001,
	})
	bwBackdate(e, "刷屏词", 1_600_000_000, 1_600_000_001)
	before := *e.st.blockWord.rows["刷屏词"]

	reply, err := callBlockWord(t, e, bwAdd("刷屏词", rpc.BlockWordScope_SCOPE_OID, bwOid))
	wantNoErr(t, "重复 ADD", err)
	wantEQ(t, "重复 ADD", "复用原 word_id", reply.WordId, seeded.WordID)
	wantEQ(t, "重复 ADD", "state 被重新启用", reply.State, model.BlockWordEnabled)

	row := bwRow(t, e, "刷屏词")
	wantEQ(t, "重复 ADD", "行数仍为 1", len(e.st.blockWord.rows), 1)
	wantEQ(t, "重复 ADD", "word_id 不变", row.WordID, seeded.WordID)
	wantEQ(t, "重复 ADD", "state", row.State, model.BlockWordEnabled)
	wantEQ(t, "重复 ADD", "scope", row.Scope, model.ScopeOid)
	wantEQ(t, "重复 ADD", "oid", row.Oid, bwOid)
	wantEQ(t, "重复 ADD", "operator 记成最新操作者", row.Operator, bwOperator)
	wantEQ(t, "重复 ADD", "ctime 是原创建时间", row.Ctime, before.Ctime)
	if row.Mtime <= before.Mtime {
		t.Errorf("重复 ADD：mtime = %d, want 刷到当前时间（原值 %d）", row.Mtime, before.Mtime)
	}
	wantEQ(t, "重复 ADD", "自增号没被白吃掉（没有新行）", e.st.blockWord.next, seeded.WordID)
}

// TestBlockWordAddOnExistingGlobalWordDemotesIt 钉住现状缺陷：
// uniq_word 只按 word 建唯一键，所以「给某分区加一个与全局词同名的词」
// 会把那条**全局**词就地降级成分区词（scope/oid 被覆盖），全局词库当场少一条。
// 停用/删除别的分区的词同理（见下一个用例）。
func TestBlockWordAddOnExistingGlobalWordDemotesIt(t *testing.T) {
	e := newEnv(t)
	global := seedBlockWord(t, e.st, &model.BlockWord{
		Word: "跨域词", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 8001,
	})

	reply, err := callBlockWord(t, e, bwAdd("跨域词", rpc.BlockWordScope_SCOPE_OID, bwOid))
	wantNoErr(t, "全局词被分区 ADD 改写", err)
	wantEQ(t, "全局词被分区 ADD 改写", "回复的是同一条目", reply.WordId, global.WordID)

	row := bwRow(t, e, "跨域词")
	wantEQ(t, "全局词被分区 ADD 改写", "scope 被改成分区", row.Scope, model.ScopeOid)
	wantEQ(t, "全局词被分区 ADD 改写", "oid 被改成请求分区", row.Oid, bwOid)
	wantEQ(t, "全局词被分区 ADD 改写", "词表只剩 1 行", len(e.st.blockWord.rows), 1)

	// 现状：全局视图回源后已经取不到这个词了（降级是即刻生效的，不受缓存失效影响）
	e.st.log.ops = e.st.log.ops[:0]
	enabled, err := e.st.blockWord.ListEnabled(context.Background(), 0)
	wantNoErr(t, "全局词库回源", err)
	wantEQ(t, "全局词库回源", "全局视图条数", len(enabled), 0)
}

// --- 停用 ---

// TestBlockWordDisableKeepsRowAsAuditEvidence 停用是软状态：
// 行必须留着，只改 state/operator/mtime，word/scope/oid/ctime 一个都不许动。
func TestBlockWordDisableKeepsRowAsAuditEvidence(t *testing.T) {
	e := newEnv(t)
	seeded := seedBlockWord(t, e.st, &model.BlockWord{
		Word: "要停用的词", Scope: model.ScopeOid, Oid: bwOid, State: model.BlockWordEnabled, Operator: 8001,
	})
	bwBackdate(e, "要停用的词", 1_600_000_000, 1_600_000_001)
	before := *e.st.blockWord.rows["要停用的词"]
	e.st.cache.warmBlockWords(0, "x")
	e.st.cache.warmBlockWords(bwOid, "x")
	calledAt := time.Now().Unix()

	reply, err := callBlockWord(t, e, bwAction(rpc.BlockWordAction_BLOCK_WORD_DISABLE, "要停用的词", rpc.BlockWordScope_SCOPE_GLOBAL, 0))
	wantNoErr(t, "停用词条", err)
	wantEQ(t, "停用词条", "word_id", reply.WordId, seeded.WordID)
	wantEQ(t, "停用词条", "state", reply.State, model.BlockWordDisabled)

	row := bwRow(t, e, "要停用的词")
	wantEQ(t, "停用词条", "行还在（软停用）", len(e.st.blockWord.rows), 1)
	wantEQ(t, "停用词条", "word_id", row.WordID, seeded.WordID)
	wantEQ(t, "停用词条", "word", row.Word, "要停用的词")
	wantEQ(t, "停用词条", "state", row.State, model.BlockWordDisabled)
	wantEQ(t, "停用词条", "operator 记成执行停用的人", row.Operator, bwOperator)
	wantEQ(t, "停用词条", "scope 不被请求参数改写", row.Scope, model.ScopeOid)
	wantEQ(t, "停用词条", "oid 不被请求参数改写", row.Oid, bwOid)
	wantEQ(t, "停用词条", "ctime", row.Ctime, before.Ctime)
	if row.Mtime <= before.Mtime || row.Mtime < calledAt {
		t.Errorf("停用词条：mtime = %d, want 刷到当前时间（原值 %d）", row.Mtime, before.Mtime)
	}
	// 失效范围按**库里那一行的 oid**，不是请求里的 oid（请求里传的是 GLOBAL/oid=0）
	wantOps(t, "停用词条", e.ops(), []string{bwFindOneOp("要停用的词"), bwDisableOp("要停用的词"), bwDelCacheOp(bwOid)})
}

// TestBlockWordDisableIgnoresRequestedScope 钉住现状缺陷：
// DISABLE/DELETE 只用 word 定位词条（uniq_word），in.Scope/in.Oid 被完全忽略，
// 所以带「本分区」作用域的请求可以停用/删除别家分区、甚至全局的词条，
// 而本服务侧没有任何分区归属校验（gateway RBAC 只管到接口级）。
func TestBlockWordDisableIgnoresRequestedScope(t *testing.T) {
	e := newEnv(t)
	other := seedBlockWord(t, e.st, &model.BlockWord{
		Word: "别家分区词", Scope: model.ScopeOid, Oid: bwOtherOid, State: model.BlockWordEnabled, Operator: 8001,
	})

	// 请求声称自己在管 bwOid 这个分区，却把 bwOtherOid 的词条停用了
	reply, err := callBlockWord(t, e, bwAction(rpc.BlockWordAction_BLOCK_WORD_DISABLE, "别家分区词", rpc.BlockWordScope_SCOPE_OID, bwOid))
	wantNoErr(t, "跨分区停用", err)
	wantEQ(t, "跨分区停用", "word_id", reply.WordId, other.WordID)
	wantEQ(t, "跨分区停用", "state", reply.State, model.BlockWordDisabled)
	wantEQ(t, "跨分区停用", "别家词条确实被改了", bwRow(t, e, "别家分区词").State, model.BlockWordDisabled)
	// 失效的也是**被查那行的 oid**（别家分区），而不是请求声称的分区——
	// 于是本分区缓存里那份同名旧词视图反倒没被清（若之前被 loadBlockWords(bwOid) 回填过）。
	wantOps(t, "跨分区停用", e.ops(), []string{bwFindOneOp("别家分区词"), bwDisableOp("别家分区词"), bwDelCacheOp(bwOtherOid)})
	wantEQ(t, "跨分区停用", "没有权限/归属类错误", reply.State, model.BlockWordDisabled)
}

// TestBlockWordDisableAndDeleteMissingWord 停用/删除不存在的词条：
// 只读一次词表就得报 ErrBlockWordNotFound，不许写、不许清缓存、更不许「顺手加一条停用记录」。
func TestBlockWordDisableAndDeleteMissingWord(t *testing.T) {
	cases := []struct {
		name   string
		action rpc.BlockWordAction
	}{
		{"停用不存在的词", rpc.BlockWordAction_BLOCK_WORD_DISABLE},
		{"删除不存在的词", rpc.BlockWordAction_BLOCK_WORD_DELETE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmBlockWords(0, "旧词")

			reply, err := callBlockWord(t, e, bwAction(tc.action, "从来没加过的词", rpc.BlockWordScope_SCOPE_GLOBAL, 0))

			wantErrIs(t, tc.name, err, model.ErrBlockWordNotFound)
			if reply != nil {
				t.Errorf("%s：仍返回 %+v", tc.name, reply)
			}
			wantOps(t, tc.name, e.ops(), []string{bwFindOneOp("从来没加过的词")})
			wantEQ(t, tc.name, "词表没被凭空写入", len(e.st.blockWord.rows), 0)
			_, ok := e.st.cache.blockWordSet(0)
			wantEQ(t, tc.name, "缓存没被清", ok, true)
			wantEQ(t, tc.name, "Disable 未被调用", countIn(e.ops(), "blockword.Disable"), 0)
			wantEQ(t, tc.name, "Delete 未被调用", countIn(e.ops(), "blockword.Delete"), 0)
		})
	}
}

// TestBlockWordConcurrentRemoveBeforeWrite 预查到行、真正写时那行已被并发运营物理删掉：
// UPDATE/DELETE 受影响 0 行 ⇒ 按「词条不存在」返回，不得回伪成功、不得清缓存。
func TestBlockWordConcurrentRemoveBeforeWrite(t *testing.T) {
	cases := []struct {
		name    string
		action  rpc.BlockWordAction
		wantOps []string
	}{
		{
			name:    "停用撞空",
			action:  rpc.BlockWordAction_BLOCK_WORD_DISABLE,
			wantOps: []string{bwFindOneOp("被抢删的词"), bwDisableOp("被抢删的词"), bwDelCacheOp(0)},
		},
		{
			name:    "删除撞空",
			action:  rpc.BlockWordAction_BLOCK_WORD_DELETE,
			wantOps: []string{bwFindOneOp("被抢删的词"), bwDeleteOp("被抢删的词"), bwDelCacheOp(0)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedBlockWord(t, e.st, &model.BlockWord{
				Word: "被抢删的词", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 8001,
			})
			e.st.cache.warmBlockWords(0, "被抢删的词")
			// FindOne 读到之后、写之前那行消失（另一个运营刚把它物理删了）
			e.st.blockWord.vanishBeforeWrite()

			reply, err := callBlockWord(t, e, bwAction(tc.action, "被抢删的词", rpc.BlockWordScope_SCOPE_GLOBAL, 0))

			wantErrIs(t, tc.name, err, model.ErrBlockWordNotFound)
			if reply != nil {
				t.Errorf("%s：抢空仍返回 %+v", tc.name, reply)
			}
			// 受影响 0 行必须报「不存在」，不能回伪成功。
			wantOps(t, tc.name, e.ops(), tc.wantOps)
			wantEQ(t, tc.name, "没有凭空补写", len(e.st.blockWord.rows), 0)
			wantEQ(t, tc.name, "主键索引也没有", len(e.st.blockWord.byID), 0)
			// 撞空仍然失效缓存是对的（那行确实变了），但不许回填旧词库
			_, stale := e.st.cache.blockWordSet(0)
			wantEQ(t, tc.name, "词库缓存已失效", stale, false)
		})
	}
}

// --- 删除 ---

// TestBlockWordDeleteRemovesRowButRepliesDisabled 物理删除的成功路径 + 缺陷钉桩：
// 行真的没了，但回复的 state 复用 BlockWordDisabled（0），
// 调用方无法区分「停用（行还在，可再启用）」与「已删除（行没了，word_id 也查不到）」。
func TestBlockWordDeleteRemovesRowButRepliesDisabled(t *testing.T) {
	e := newEnv(t)
	seeded := seedBlockWord(t, e.st, &model.BlockWord{
		Word: "要删掉的词", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 8001,
	})
	e.st.cache.warmBlockWords(0, "旧词")

	reply, err := callBlockWord(t, e, bwAction(rpc.BlockWordAction_BLOCK_WORD_DELETE, "要删掉的词", rpc.BlockWordScope_SCOPE_GLOBAL, 0))
	wantNoErr(t, "物理删除词条", err)
	wantEQ(t, "物理删除词条", "word_id", reply.WordId, seeded.WordID)
	// ⚠ 现状：删除回复 state=0（与停用同值）。修法落地时应换成显式「已删除」语义。
	wantEQ(t, "物理删除词条", "state（现状与停用同值）", reply.State, model.BlockWordDisabled)

	wantEQ(t, "物理删除词条", "行已物理移除", bwHas(e, "要删掉的词"), false)
	wantEQ(t, "物理删除词条", "词表行数", len(e.st.blockWord.rows), 0)
	wantEQ(t, "物理删除词条", "主键索引也移除", len(e.st.blockWord.byID), 0)
	wantOps(t, "物理删除词条", e.ops(), []string{bwFindOneOp("要删掉的词"), bwDeleteOp("要删掉的词"), bwDelCacheOp(0)})
}

// --- 下游故障 ---

// TestBlockWordPropagatesDownstreamFailures 每个依赖各注入一次故障：
// 写库失败必须原样上抛、且**不得**失效缓存（否则用一次失败的写把线上词库缓存打空）；
// 只有缓存侧失败才允许降级（TTL 兜底）。
func TestBlockWordPropagatesDownstreamFailures(t *testing.T) {
	const w = "故障词"
	cases := []struct {
		name      string
		action    rpc.BlockWordAction
		arm       func(e *env)
		wantErr   error
		wantOps   []string
		wantState int32
	}{
		{
			name:      "新增撞库失败",
			action:    rpc.BlockWordAction_BLOCK_WORD_ADD,
			arm:       func(e *env) { e.st.blockWord.failWith("Upsert", errDB) },
			wantErr:   errDB,
			wantOps:   []string{bwUpsertOp(w)},
			wantState: model.BlockWordEnabled,
		},
		{
			name:      "停用时读词表失败",
			action:    rpc.BlockWordAction_BLOCK_WORD_DISABLE,
			arm:       func(e *env) { e.st.blockWord.failWith("FindOne", errDB) },
			wantErr:   errDB,
			wantOps:   []string{bwFindOneOp(w)},
			wantState: model.BlockWordEnabled,
		},
		{
			name:      "停用写失败",
			action:    rpc.BlockWordAction_BLOCK_WORD_DISABLE,
			arm:       func(e *env) { e.st.blockWord.failWith("Disable", errDB) },
			wantErr:   errDB,
			wantOps:   []string{bwFindOneOp(w), bwDisableOp(w)},
			wantState: model.BlockWordEnabled,
		},
		{
			name:      "删除时读词表失败",
			action:    rpc.BlockWordAction_BLOCK_WORD_DELETE,
			arm:       func(e *env) { e.st.blockWord.failWith("FindOne", errDB) },
			wantErr:   errDB,
			wantOps:   []string{bwFindOneOp(w)},
			wantState: model.BlockWordEnabled,
		},
		{
			name:      "删除写失败",
			action:    rpc.BlockWordAction_BLOCK_WORD_DELETE,
			arm:       func(e *env) { e.st.blockWord.failWith("Delete", errDB) },
			wantErr:   errDB,
			wantOps:   []string{bwFindOneOp(w), bwDeleteOp(w)},
			wantState: model.BlockWordEnabled,
		},
		{
			name:      "缓存失效失败只记日志",
			action:    rpc.BlockWordAction_BLOCK_WORD_DISABLE,
			arm:       func(e *env) { e.st.cache.failWith("DelBlockWords", errCache) },
			wantErr:   nil,
			wantOps:   []string{bwFindOneOp(w), bwDisableOp(w), bwDelCacheOp(0)},
			wantState: model.BlockWordDisabled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedBlockWord(t, e.st, &model.BlockWord{
				Word: w, Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 8001,
			})
			e.st.cache.warmBlockWords(0, w)
			tc.arm(e)

			reply, err := callBlockWord(t, e, bwAction(tc.action, w, rpc.BlockWordScope_SCOPE_GLOBAL, 0))

			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				if reply != nil {
					t.Errorf("%s：故障仍返回 %+v", tc.name, reply)
				}
				// 写失败时不许动缓存，也不许留半截状态：词条还是别人写进去的那个版本
				wantCount(t, tc.name, e.st.log, "cache.", 0)
				words, ok := e.st.cache.blockWordSet(0)
				wantEQ(t, tc.name, "词库缓存原样还在", ok, true)
				wantStringsEQ(t, tc.name, "词库缓存内容未变", words, []string{w})
				row := bwRow(t, e, w)
				wantEQ(t, tc.name, "词行 state 未被改", row.State, model.BlockWordEnabled)
				wantEQ(t, tc.name, "operator 未被改", row.Operator, int64(8001))
			} else {
				wantNoErr(t, tc.name, err)
				if reply == nil {
					t.Fatalf("%s：可降级故障把写操作打成了失败", tc.name)
				}
				wantEQ(t, tc.name, "词行 state", bwRow(t, e, w).State, tc.wantState)
				wantEQ(t, tc.name, "operator 记成执行者", bwRow(t, e, w).Operator, bwOperator)
			}
			wantOps(t, tc.name, e.ops(), tc.wantOps)
		})
	}
}
