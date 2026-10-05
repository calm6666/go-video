package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

// 本文件覆盖 PostDanmaku：参数守卫 → 令牌桶 → 幂等重放 → 分钟窗口 → 屏蔽词 → 落库 → 送审。
// 处理顺序本身就是被测契约（AGENTS.md §8 审核门禁 + §5 写接口幂等），
// 所以每个用例都断言完整有序调用序列，而不只是次数。

const (
	postOid  = int64(1001)
	postAid  = int64(500)
	postMid  = int64(7001)
	postTask = int64(9001)
	// postSeg 是 progress_ms=12_345 在 6 秒分段下的分段号（12_345 / 6000 = 2）。
	postSeg = int32(2)
)

var (
	errLimiter    = errors.New("inject: 令牌桶耗尽")
	errModeration = errors.New("inject: moderation-orchestrator 不可用")
	errCache      = errors.New("inject: redis down")
)

// postReq 组一条合法发送请求；cliKey 是客户端幂等键。
func postReq(cliKey string) *rpc.PostDanmakuReq {
	return &rpc.PostDanmakuReq{
		Oid: postOid, Aid: postAid, Mid: postMid, ProgressMs: 12_345,
		Mode: rpc.DanmakuMode_MODE_TOP, Fontsize: 30, Color: 0x112233,
		Content: "第一轮测试弹幕", IdempotencyKey: cliKey, TraceId: "trace-post",
	}
}

func postDanmaku(t *testing.T, e *env, in *rpc.PostDanmakuReq) (*rpc.PostDanmakuReply, error) {
	t.Helper()
	return NewPostDanmakuLogic(context.Background(), e.svcCtx).PostDanmaku(in)
}

// --- 期望序列片段（格式与替身记轨迹的口径逐字一致） ---

func midRateOp(mid, bucket int64) string {
	return "cache.IncrMid:" + fmt.Sprintf(keyRateMid, mid, bucket)
}
func oidRateOp(oid, bucket int64) string {
	return "cache.IncrOid:" + fmt.Sprintf(keyRateOid, oid, bucket)
}
func findKeyOp(key string) string   { return "danmaku.FindByKey:" + key }
func insertKeyOp(key string) string { return "danmaku.Insert:" + key }

// bwLoadOps 是词库「读缓存 miss → 回源 DB → 回填」三连。
// 缓存命中时只有第一条；缓存读写失败降级时仍是三条（写失败只记日志、不中断）。
func bwLoadOps(scopeOID int64, nWords int) []string {
	key := fmt.Sprintf(keyBlockWord, scopeOID)
	return []string{
		"cache.GetBW:" + key,
		fmt.Sprintf("blockword.ListEnabled:%d", scopeOID),
		fmt.Sprintf("cache.SetBW:%s=%d", key, nWords),
	}
}

// postGatesOps 是「守卫 + 令牌桶 + 幂等预查 + 分钟窗口 + 双门禁词库」的公共前缀。
func postGatesOps(bucket int64, key string) []string {
	ops := []string{"limiter.Allow", findKeyOp(key), midRateOp(postMid, bucket), oidRateOp(postOid, bucket)}
	ops = append(ops, bwLoadOps(0, 0)...)
	ops = append(ops, bwLoadOps(postOid, 0)...)
	return ops
}

// postInsertOps 再往后到「主表写入成功」（含 CreateDanmaku 内的第二次幂等回查）。
func postInsertOps(bucket int64, key string) []string {
	return append(postGatesOps(bucket, key), findKeyOp(key), insertKeyOp(key))
}

// TestPostDanmakuRejectsInvalidRequests 参数守卫必须发生在任何依赖调用之前：
// 一条非法弹幕不许消耗令牌桶、不许读缓存、不许碰 MySQL。
func TestPostDanmakuRejectsInvalidRequests(t *testing.T) {
	mut := func(fn func(in *rpc.PostDanmakuReq)) *rpc.PostDanmakuReq {
		in := &rpc.PostDanmakuReq{Oid: postOid, Mid: postMid, Content: "合法正文", IdempotencyKey: "cli-g"}
		fn(in)
		return in
	}
	cases := []struct {
		name string
		in   *rpc.PostDanmakuReq
		want error
	}{
		{"oid 为 0", &rpc.PostDanmakuReq{Mid: postMid, Content: "x", IdempotencyKey: "k"}, model.ErrInvalidOid},
		{"oid 为负", &rpc.PostDanmakuReq{Oid: -1, Mid: postMid, Content: "x", IdempotencyKey: "k"}, model.ErrInvalidOid},
		{"mid 非正（弹幕不支持游客发送）", &rpc.PostDanmakuReq{Oid: postOid, Content: "x", IdempotencyKey: "k"}, model.ErrInvalidMid},
		{"progress_ms 为负", mut(func(in *rpc.PostDanmakuReq) { in.ProgressMs = -1 }), model.ErrInvalidProgress},
		{"正文为空", mut(func(in *rpc.PostDanmakuReq) { in.Content = "" }), model.ErrContentEmpty},
		{"正文 101 rune 超长", mut(func(in *rpc.PostDanmakuReq) { in.Content = strings.Repeat("a", 101) }), model.ErrContentTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()

			reply, err := postDanmaku(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法请求仍被受理 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// TestPostDanmakuContentBoundaryByRunes 长度上限按 rune 判定、边界含等号：
// 100 rune 必须通过，101 rune 必须拒绝；40 个汉字（120 字节）不能被按字节误杀。
func TestPostDanmakuContentBoundaryByRunes(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr error
	}{
		{"恰好 100 个 ASCII 通过", strings.Repeat("a", 100), nil},
		{"40 个汉字（120 字节）通过", strings.Repeat("弹幕", 20), nil},
		{"51 个汉字（153 字节）通过", strings.Repeat("弹", 51), nil},
		{"101 个 ASCII 拒绝", strings.Repeat("a", 101), model.ErrContentTooLong},
		{"51 个汉字 + 50 个 ASCII 拒绝", strings.Repeat("弹", 51) + strings.Repeat("a", 50), model.ErrContentTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.mod.taskID = postTask
			in := postReq("cli-len")
			in.Content = tc.content

			reply, err := postDanmaku(t, e, in)

			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				wantNoCall(t, tc.name, e.st, 0)
				return
			}
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "落库正文", e.st.danmaku.only().Content, tc.content)
			wantEQ(t, tc.name, "dmid", reply.Dmid, int64(101))
		})
	}
}

// TestPostDanmakuRequiresClientKeyBeforeTouchingData 缺幂等键必须拒绝。
// 该守卫排在令牌桶之后（实现顺序，用序列锁定），但数据侧不许留下任何痕迹。
func TestPostDanmakuRequiresClientKeyBeforeTouchingData(t *testing.T) {
	cases := []struct {
		name string
		mut  func(in *rpc.PostDanmakuReq)
	}{
		{"idempotency_key 与 client_msg_id 都为空", func(in *rpc.PostDanmakuReq) { in.IdempotencyKey = "" }},
		{"键超过 64 字符（落库列宽）", func(in *rpc.PostDanmakuReq) { in.IdempotencyKey = strings.Repeat("k", 65) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			in := postReq("cli-x")
			tc.mut(in)

			reply, err := postDanmaku(t, e, in)

			wantErrIs(t, tc.name, err, model.ErrIdempotencyKeyRequired)
			if reply != nil {
				t.Errorf("%s：仍返回了弹幕 %+v", tc.name, reply)
			}
			wantOps(t, tc.name, e.ops(), []string{"limiter.Allow", "limiter.Done:0"})
			wantCount(t, tc.name, e.st.log, "danmaku.", 0)
			wantCount(t, tc.name, e.st.log, "cache.", 0)
		})
	}
}

// TestPostDanmakuFallsBackToClientMsgID client_msg_id 是幂等键的缺省来源；
// 键空间由 (oid, mid, 客户端键) 决定——换 oid 或换 mid 都必须得到不同的落库键。
func TestPostDanmakuFallsBackToClientMsgID(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	in := postReq("")
	in.ClientMsgId = "cm-1"

	_, err := postDanmaku(t, e, in)

	wantNoErr(t, "退化到 client_msg_id", err)
	wantEQ(t, "退化到 client_msg_id", "idempotency_key",
		e.st.danmaku.only().IdempotencyKey, goldenIdem(postOid, postMid, "cm-1"))
	if goldenIdem(postOid, postMid, "cm-1") == goldenIdem(postOid, postMid+1, "cm-1") {
		t.Error("幂等键未绑定 mid，不同用户会互相顶掉")
	}
	if goldenIdem(postOid, postMid, "cm-1") == goldenIdem(postOid+1, postMid, "cm-1") {
		t.Error("幂等键未绑定 oid，不同内容会互相顶掉")
	}
}

// TestPostDanmakuHappyPathPersistsEveryField 是逐字段投影用例 + 完整有序序列：
// 回复与落库行的每一列都要对上，不许只断言非空。
func TestPostDanmakuHappyPathPersistsEveryField(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	bucket := e.st.cache.freezeWindows()
	key := goldenIdem(postOid, postMid, "cli-1")
	calledAt := time.Now().Unix()

	reply, err := postDanmaku(t, e, postReq("cli-1"))
	wantNoErr(t, "合法发送", err)

	wantEQ(t, "回复", "dmid", reply.Dmid, int64(101))
	wantEQ(t, "回复", "state", reply.State, model.StatePending)
	wantEQ(t, "回复", "pool", reply.Pool, model.PoolReview)
	wantEQ(t, "回复", "seg_no", reply.SegNo, postSeg)
	wantEQ(t, "回复", "replayed", reply.Replayed, false)
	wantEQ(t, "回复", "moderation_task_id", reply.ModerationTaskId, postTask)
	if reply.Ctime < calledAt || reply.Ctime > calledAt+2 {
		t.Errorf("回复 ctime = %d, want ≈ %d", reply.Ctime, calledAt)
	}

	row := e.st.danmaku.only()
	wantEQ(t, "落库", "oid", row.Oid, postOid)
	wantEQ(t, "落库", "aid", row.Aid, postAid)
	wantEQ(t, "落库", "mid", row.Mid, postMid)
	wantEQ(t, "落库", "progress_ms", row.ProgressMs, int64(12_345))
	wantEQ(t, "落库", "mode", row.Mode, int32(rpc.DanmakuMode_MODE_TOP))
	wantEQ(t, "落库", "fontsize", row.Fontsize, int32(30))
	wantEQ(t, "落库", "color", row.Color, int32(0x112233))
	wantEQ(t, "落库", "content", row.Content, "第一轮测试弹幕")
	// AGENTS.md §8：机审门禁开启时，未命中屏蔽词的弹幕只能进待审/审核池。
	wantEQ(t, "落库", "state", row.State, model.StatePending)
	wantEQ(t, "落库", "pool", row.Pool, model.PoolReview)
	wantEQ(t, "落库", "seg_no", row.SegNo, postSeg)
	wantEQ(t, "落库", "idempotency_key", row.IdempotencyKey, key)
	wantEQ(t, "落库", "trace_id", row.TraceId, "trace-post")
	wantEQ(t, "落库", "回填的 moderation_task_id", row.ModerationTaskId, postTask)
	wantEQ(t, "落库", "mtime 与 ctime 同源", row.Mtime, row.Ctime)

	wantOps(t, "合法发送", e.ops(), append(postInsertOps(bucket, key),
		"moderation.Submit:101/7001/danmaku-publish", "danmaku.SetTask:101/9001", "limiter.Done:0"))
	// 待审弹幕不得进入下发包：段计数与段缓存一次都不许碰
	wantCount(t, "合法发送", e.st.log, "segment.Incr", 0)
	wantCount(t, "合法发送", e.st.log, "cache.IncrCnt", 0)
	wantCount(t, "合法发送", e.st.log, "cache.DelSeg", 0)
	wantEQ(t, "合法发送", "段表无行", e.st.segment.countAt(postOid, postSeg), int32(-1))
}

// TestPostDanmakuNormalizesDisplayFields 展示参数的归一规则：越界回落默认值而不是报错，
// 缺省 aid 时按 oid 投影，保证归档列非空。
func TestPostDanmakuNormalizesDisplayFields(t *testing.T) {
	cases := []struct {
		name                          string
		mode                          rpc.DanmakuMode
		fontsize, color, aid, wantAid int64
		wantMode, wantFont, wantColor int32
	}{
		{"未指定模式/字号/颜色 → 滚动 + 25 + 白", rpc.DanmakuMode_MODE_UNSPECIFIED, 0, 0, 777, 777, int32(rpc.DanmakuMode_MODE_SCROLL), 25, 0xFFFFFF},
		{"字号过小回落默认", rpc.DanmakuMode_MODE_TOP, 5, 0x112233, 777, 777, int32(rpc.DanmakuMode_MODE_TOP), 25, 0x112233},
		{"字号过大回落默认", rpc.DanmakuMode_MODE_TOP, 65, 0x112233, 777, 777, int32(rpc.DanmakuMode_MODE_TOP), 25, 0x112233},
		{"字号上界 64 保留", rpc.DanmakuMode_MODE_TOP, 64, 0x112233, 777, 777, int32(rpc.DanmakuMode_MODE_TOP), 64, 0x112233},
		{"字号下界 10 保留", rpc.DanmakuMode_MODE_TOP, 10, 0x112233, 777, 777, int32(rpc.DanmakuMode_MODE_TOP), 10, 0x112233},
		{"颜色超出 24bit 回落白", rpc.DanmakuMode_MODE_TOP, 30, 0x1_000000, 777, 777, int32(rpc.DanmakuMode_MODE_TOP), 30, 0xFFFFFF},
		{"负颜色回落白", rpc.DanmakuMode_MODE_TOP, 30, -1, 777, 777, int32(rpc.DanmakuMode_MODE_TOP), 30, 0xFFFFFF},
		{"未知模式回落滚动", rpc.DanmakuMode_MODE_ADVANCED + 1, 30, 0x112233, 777, 777, int32(rpc.DanmakuMode_MODE_SCROLL), 30, 0x112233},
		{"aid 缺省按 oid 投影", rpc.DanmakuMode_MODE_SCROLL, 30, 0x00FF00, 0, postOid, int32(rpc.DanmakuMode_MODE_SCROLL), 30, 0x00FF00},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.mod.taskID = postTask
			in := postReq("cli-display")
			in.Mode, in.Fontsize, in.Color, in.Aid = tc.mode, int32(tc.fontsize), int32(tc.color), tc.aid

			_, err := postDanmaku(t, e, in)

			wantNoErr(t, tc.name, err)
			row := e.st.danmaku.only()
			wantEQ(t, tc.name, "mode", row.Mode, tc.wantMode)
			wantEQ(t, tc.name, "fontsize", row.Fontsize, tc.wantFont)
			wantEQ(t, tc.name, "color", row.Color, tc.wantColor)
			wantEQ(t, tc.name, "aid", row.Aid, tc.wantAid)
		})
	}
}

// TestPostDanmakuBlockWordHitFoldsIntoBlockPool 命中屏蔽词后的可见性：
// 折叠 + 屏蔽池（既不进普通池也不进审核池下发包），正文原文入库，送审原因区别于普通送审。
func TestPostDanmakuBlockWordHitFoldsIntoBlockPool(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	bucket := e.st.cache.freezeWindows()
	seedBlockWord(t, e.st, &model.BlockWord{Word: "广告", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})
	key := goldenIdem(postOid, postMid, "cli-hit")
	in := postReq("cli-hit")
	in.Content = "点击这里有广告哦"

	reply, err := postDanmaku(t, e, in)
	wantNoErr(t, "命中屏蔽词", err)

	wantEQ(t, "命中屏蔽词", "state", reply.State, model.StateFolded)
	wantEQ(t, "命中屏蔽词", "pool", reply.Pool, model.PoolBlock)
	wantEQ(t, "命中屏蔽词", "moderation_task_id", reply.ModerationTaskId, postTask)
	row := e.st.danmaku.only()
	wantEQ(t, "命中屏蔽词落库", "content", row.Content, "点击这里有广告哦") // 只判定不改写用户输入
	wantEQ(t, "命中屏蔽词落库", "state", row.State, model.StateFolded)
	wantEQ(t, "命中屏蔽词落库", "pool", row.Pool, model.PoolBlock)
	wantEQ(t, "命中屏蔽词", "送审原因", e.mod.calls[0], "101/7001/danmaku-blockword")
	wantCount(t, "命中屏蔽词", e.st.log, "segment.Incr", 0)
	wantCount(t, "命中屏蔽词", e.st.log, "cache.IncrCnt", 0)

	// 与 happy path 的差别：两次词库回源都带上全局词（ListEnabled(oid) 的 SQL 口径是
	// 「全局 ∪ 本分区」，故 dm:bw:0 与 dm:bw:1001 各含 1 条），状态/池与送审原因不同。
	wantOps(t, "命中屏蔽词", e.ops(), append(func() []string {
		ops := []string{"limiter.Allow", findKeyOp(key), midRateOp(postMid, bucket), oidRateOp(postOid, bucket)}
		ops = append(ops, bwLoadOps(0, 1)...)
		ops = append(ops, bwLoadOps(postOid, 1)...)
		return append(ops, findKeyOp(key), insertKeyOp(key))
	}(), "moderation.Submit:101/7001/danmaku-blockword", "danmaku.SetTask:101/9001", "limiter.Done:0"))
}

// TestPostDanmakuBlockWordPaddedVariantStillHits 屏蔽词归一化在发送侧生效：
// 零宽/标点填充与全半角变体不能绕过词库（policy.Normalize 的口径）。
func TestPostDanmakuBlockWordPaddedVariantStillHits(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"标点填充", "广·告"},
		{"零宽字符插入", "广​告"},
		{"大小写变体", "SPAM"},
		{"全角变体", "ｓｐａｍ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.mod.taskID = postTask
			seedBlockWord(t, e.st, &model.BlockWord{Word: "广告", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})
			seedBlockWord(t, e.st, &model.BlockWord{Word: "spam", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})
			in := postReq("cli-variant")
			in.Content = tc.content

			reply, err := postDanmaku(t, e, in)

			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "state", reply.State, model.StateFolded)
			wantEQ(t, tc.name, "pool", reply.Pool, model.PoolBlock)
		})
	}
}

// TestPostDanmakuBlockWordScopeOnlyAppliesToItsOid 分区词只对所属 oid 生效，全局词对所有 oid 生效，
// 停用词不参与判定。
func TestPostDanmakuBlockWordScopeOnlyAppliesToItsOid(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	seedBlockWord(t, e.st, &model.BlockWord{Word: "私信", Scope: model.ScopeOid, Oid: postOid, State: model.BlockWordEnabled, Operator: 99})
	seedBlockWord(t, e.st, &model.BlockWord{Word: "外挂", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})
	seedBlockWord(t, e.st, &model.BlockWord{Word: "已被停用", Scope: model.ScopeGlobal, State: model.BlockWordDisabled, Operator: 99})

	hit := postReq("cli-scope-hit")
	hit.Content = "私信我"
	reply, err := postDanmaku(t, e, hit)
	wantNoErr(t, "分区词命中本分区", err)
	wantEQ(t, "分区词命中本分区", "state", reply.State, model.StateFolded)

	miss := postReq("cli-scope-miss")
	miss.Oid = 2002 // 换分区
	miss.Content = "私信我"
	reply, err = postDanmaku(t, e, miss)
	wantNoErr(t, "分区词跨分区不命中", err)
	wantEQ(t, "分区词跨分区不命中", "state", reply.State, model.StatePending)
	wantEQ(t, "分区词跨分区不命中", "pool", reply.Pool, model.PoolReview)

	off := postReq("cli-scope-off")
	off.Content = "已被停用的写法"
	reply, err = postDanmaku(t, e, off)
	wantNoErr(t, "停用词不参与判定", err)
	wantEQ(t, "停用词不参与判定", "state", reply.State, model.StatePending)

	global := postReq("cli-scope-global")
	global.Oid = 3003
	global.Content = "低价外挂"
	reply, err = postDanmaku(t, e, global)
	wantNoErr(t, "全局词对任意分区生效", err)
	wantEQ(t, "全局词对任意分区生效", "state", reply.State, model.StateFolded)
}

// TestPostDanmakuBlockWordCacheIsReusedAcrossSends 词库命中缓存后不再回源 DB：
// 第二条弹幕只读两次缓存，不查库（发送 QPS 的主要放大点）。
func TestPostDanmakuBlockWordCacheIsReusedAcrossSends(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	seedBlockWord(t, e.st, &model.BlockWord{Word: "广告", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})

	for i := range 3 {
		_, err := postDanmaku(t, e, postReq(fmt.Sprintf("cli-cache-%d", i)))
		wantNoErr(t, fmt.Sprintf("第 %d 条", i+1), err)
	}
	wantCount(t, "词库缓存复用", e.st.log, "blockword.ListEnabled", 2) // 仅第一条回源（全局 + 分区）
	wantCount(t, "词库缓存复用", e.st.log, "cache.GetBW", 6)
	words, hit := e.st.cache.blockWordSet(0)
	wantEQ(t, "词库缓存复用", "缓存命中", hit, true)
	wantStringsEQ(t, "词库缓存复用", "全局词", words, []string{"广告"})
}

// TestPostDanmakuIdempotentReplayDoesNotDoubleInsertOrConsumeQuota 发送幂等口径：
// 同键重试直接返回原 dmid，不二次写、不二次送审、不消耗分钟窗口配额。
func TestPostDanmakuIdempotentReplayDoesNotDoubleInsertOrConsumeQuota(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	bucket := e.st.cache.freezeWindows()
	key := goldenIdem(postOid, postMid, "cli-replay")
	seeded := seedDanmaku(t, e.st, &model.Danmaku{
		Oid: postOid, Aid: postAid, Mid: postMid, ProgressMs: 12_345,
		Mode: int32(rpc.DanmakuMode_MODE_TOP), Fontsize: 30, Color: 0x112233,
		Content: "已存在的那条", State: model.StatePending, Pool: model.PoolReview, SegNo: postSeg,
		IdempotencyKey: key, ModerationTaskId: 777, TraceId: "trace-seed",
		Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	})

	reply, err := postDanmaku(t, e, postReq("cli-replay"))
	wantNoErr(t, "幂等重放", err)

	wantEQ(t, "幂等重放", "dmid", reply.Dmid, seeded.Dmid)
	wantEQ(t, "幂等重放", "replayed", reply.Replayed, true)
	wantEQ(t, "幂等重放", "state", reply.State, model.StatePending)
	wantEQ(t, "幂等重放", "pool", reply.Pool, model.PoolReview)
	wantEQ(t, "幂等重放", "seg_no", reply.SegNo, postSeg)
	wantEQ(t, "幂等重放", "ctime（返回原值）", reply.Ctime, int64(1_700_000_000))
	wantEQ(t, "幂等重放", "moderation_task_id（返回原值）", reply.ModerationTaskId, int64(777))

	wantOps(t, "幂等重放", e.ops(), []string{"limiter.Allow", findKeyOp(key), "limiter.Done:0"})
	wantEQ(t, "幂等重放", "danmaku 行数", int64(e.st.danmaku.countRows()), 1)
	wantCount(t, "幂等重放", e.st.log, "danmaku.Insert", 0)
	wantCount(t, "幂等重放", e.st.log, "moderation.Submit", 0)
	wantEQ(t, "幂等重放", "mid 窗口未被消耗", e.st.cache.window(keyRateMid, postMid, bucket), int32(0))
	wantEQ(t, "幂等重放", "oid 窗口未被消耗", e.st.cache.window(keyRateOid, postOid, bucket), int32(0))
}

// TestPostDanmakuReplayResubmitsWhenPreviousSubmitFailed 上次送审失败（task_id=0）时，
// 重试必须补送并回填，而不是永远停在「待审且无任务」。
func TestPostDanmakuReplayResubmitsWhenPreviousSubmitFailed(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	key := goldenIdem(postOid, postMid, "cli-retry")
	seeded := seedDanmaku(t, e.st, &model.Danmaku{
		Oid: postOid, Aid: postAid, Mid: postMid, ProgressMs: 12_345,
		Mode: int32(rpc.DanmakuMode_MODE_TOP), Fontsize: 30, Color: 0x112233,
		Content: "送审失败的那条", State: model.StateFolded, Pool: model.PoolBlock, SegNo: postSeg,
		IdempotencyKey: key, TraceId: "trace-seed", Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	})

	reply, err := postDanmaku(t, e, postReq("cli-retry"))
	wantNoErr(t, "补送审", err)

	wantEQ(t, "补送审", "dmid", reply.Dmid, seeded.Dmid)
	wantEQ(t, "补送审", "replayed", reply.Replayed, true)
	wantEQ(t, "补送审", "moderation_task_id", reply.ModerationTaskId, postTask)
	wantEQ(t, "补送审", "库里 task 已回填", e.st.danmaku.get(seeded.Dmid).ModerationTaskId, postTask)
	// 屏蔽池的重送审沿用 danmaku-blockword 原因（按落库池判定，不按本次内容重算）
	wantOps(t, "补送审", e.ops(), []string{
		"limiter.Allow", findKeyOp(key),
		"moderation.Submit:101/7001/danmaku-blockword", "danmaku.SetTask:101/9001",
		"limiter.Done:0",
	})
}

// TestPostDanmakuConcurrentDuplicateReliesOnUniqueIndex 并发重复投递：
// 预查 miss，INSERT 撞 uniq_idempotency，回查命中后按重放返回——不得二次插入、不得二次加计数。
func TestPostDanmakuConcurrentDuplicateReliesOnUniqueIndex(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	bucket := e.st.cache.freezeWindows()
	key := goldenIdem(postOid, postMid, "cli-race")
	// 让下一次 Insert 模拟「另一个请求抢先落地同一幂等键」：返回 1062，但行已在库里。
	e.st.danmaku.pendingReveal = &model.Danmaku{
		Oid: postOid, Aid: postAid, Mid: postMid, ProgressMs: 12_345,
		Mode: int32(rpc.DanmakuMode_MODE_TOP), Fontsize: 30, Color: 0x112233,
		Content: "第一轮测试弹幕", State: model.StatePending, Pool: model.PoolReview, SegNo: postSeg,
		IdempotencyKey: key, TraceId: "trace-post", Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	}

	reply, err := postDanmaku(t, e, postReq("cli-race"))
	wantNoErr(t, "并发重复投递", err)

	wantEQ(t, "并发重复投递", "dmid 取自抢先那一行", reply.Dmid, int64(101))
	wantEQ(t, "并发重复投递", "replayed", reply.Replayed, true)
	wantEQ(t, "并发重复投递", "ctime 取自抢先那一行", reply.Ctime, int64(1_700_000_000))
	wantEQ(t, "并发重复投递", "danmaku 行数", int64(e.st.danmaku.countRows()), 1)
	wantCount(t, "并发重复投递", e.st.log, "danmaku.Insert", 1)
	wantCount(t, "并发重复投递", e.st.log, "segment.Incr", 0)

	wantOps(t, "并发重复投递", e.ops(), append(postGatesOps(bucket, key),
		findKeyOp(key), // CreateDanmaku 预查
		insertKeyOp(key),
		findKeyOp(key), // CreateDanmaku 撞唯一键后回查
		findKeyOp(key), // logic 又回查一次（缺陷 ⑧：同一次发送共回查 4 次）
		"moderation.Submit:101/7001/danmaku-publish", "danmaku.SetTask:101/9001", "limiter.Done:0"))
}

// TestPostDanmakuMachineReviewBypassPublishesDirectly 机审门禁关闭时直接进普通池，
// 并派生段计数、失效段列表缓存；此时一行都不该送审。这条旁路只应由配置打开（README 缺口）。
func TestPostDanmakuMachineReviewBypassPublishesDirectly(t *testing.T) {
	cfg := defaultConf()
	cfg.MachineReviewEnabled = false
	e := newEnvConf(t, cfg)
	bucket := e.st.cache.freezeWindows()
	key := goldenIdem(postOid, postMid, "cli-open")

	reply, err := postDanmaku(t, e, postReq("cli-open"))
	wantNoErr(t, "机审旁路", err)

	wantEQ(t, "机审旁路", "state", reply.State, model.StateNormal)
	wantEQ(t, "机审旁路", "pool", reply.Pool, model.PoolNormal)
	wantEQ(t, "机审旁路", "moderation_task_id", reply.ModerationTaskId, int64(0))
	row := e.st.danmaku.only()
	wantEQ(t, "机审旁路落库", "state", row.State, model.StateNormal)
	wantEQ(t, "机审旁路落库", "pool", row.Pool, model.PoolNormal)

	wantCount(t, "机审旁路", e.st.log, "moderation.Submit", 0)
	wantOps(t, "机审旁路", e.ops(), append(postInsertOps(bucket, key),
		// 主行落库后立即派生：先 DB 段计数、再缓存计数、最后失效段列表
		"segment.Incr:1001/2/+1", "cache.IncrCnt:dm:cnt:1001:2=+1", "cache.DelSeg:dm:seg:1001:2",
		"limiter.Done:0"))
	wantEQ(t, "机审旁路", "段表计数", e.st.segment.countAt(postOid, postSeg), int32(1))
	wantEQ(t, "机审旁路", "缓存段计数", func() int32 { v, _ := e.st.cache.count(postOid, postSeg); return v }(), int32(1))
}

// TestPostDanmakuNeverFakesSubmittedWhenModerationMissing 未配置审核客户端时必须显式失败
// （AGENTS.md §8），行以「待审/审核池」落库但不伪造 task_id、不返回成功。
func TestPostDanmakuNeverFakesSubmittedWhenModerationMissing(t *testing.T) {
	e := newEnv(t)
	e.svcCtx.Moderation = nil // 对应生产里未配置 moderation RPC
	bucket := e.st.cache.freezeWindows()
	key := goldenIdem(postOid, postMid, "cli-nomod")

	reply, err := postDanmaku(t, e, postReq("cli-nomod"))

	wantErrIs(t, "未配置机审", err, model.ErrModerationNotConfigured)
	if reply != nil {
		t.Errorf("未配置机审仍返回结果 %+v", reply)
	}
	row := e.st.danmaku.only()
	wantEQ(t, "未配置机审", "落库 state", row.State, model.StatePending)
	wantEQ(t, "未配置机审", "落库 pool", row.Pool, model.PoolReview)
	wantEQ(t, "未配置机审", "落库 moderation_task_id", row.ModerationTaskId, int64(0))
	wantCount(t, "未配置机审", e.st.log, "moderation.Submit", 0)
	wantOps(t, "未配置机审", e.ops(), append(postInsertOps(bucket, key), "limiter.Done:0"))
}

// TestPostDanmakuPropagatesDownstreamFailures 每个依赖各注入一次故障：
// 错误原样上抛、没有半成品数据、没有伪成功回复，且确实炸在预期的那一步。
func TestPostDanmakuPropagatesDownstreamFailures(t *testing.T) {
	cases := []struct {
		name string
		arm  func(e *env)
		want error
		// seq 是从入口到故障点的完整有序序列
		seq func(bucket int64, key string) []string
		// rows / submitted 断言故障后的副作用边界
		rows      int
		submitted int
	}{
		{
			name: "令牌桶不可用统一收敛为限流错误", arm: func(e *env) { e.lim.err = errLimiter },
			want: model.ErrRateLimited, rows: 0, submitted: 0,
			seq: func(_ int64, _ string) []string { return []string{"limiter.Allow"} },
		},
		{
			name: "幂等键回查失败", arm: func(e *env) { e.st.danmaku.failWith("FindByIdempotencyKey", errDB) },
			want: errDB, rows: 0, submitted: 0,
			seq: func(_ int64, key string) []string {
				return []string{"limiter.Allow", findKeyOp(key), "limiter.Done:0"}
			},
		},
		{
			name: "mid 分钟窗口写入失败", arm: func(e *env) { e.st.cache.failWith("IncrMidWindow", errDB) },
			want: errDB, rows: 0, submitted: 0,
			seq: func(bucket int64, key string) []string {
				return []string{"limiter.Allow", findKeyOp(key), midRateOp(postMid, bucket), "limiter.Done:0"}
			},
		},
		{
			name: "oid 分钟窗口写入失败", arm: func(e *env) { e.st.cache.failWith("IncrOidWindow", errDB) },
			want: errDB, rows: 0, submitted: 0,
			seq: func(bucket int64, key string) []string {
				return []string{"limiter.Allow", findKeyOp(key), midRateOp(postMid, bucket), oidRateOp(postOid, bucket), "limiter.Done:0"}
			},
		},
		{
			name: "屏蔽词库回源失败必须显式拒绝而非默认放行", arm: func(e *env) { e.st.blockWord.failWith("ListEnabled", errDB) },
			want: errDB, rows: 0, submitted: 0,
			seq: func(bucket int64, key string) []string {
				return append([]string{"limiter.Allow", findKeyOp(key), midRateOp(postMid, bucket), oidRateOp(postOid, bucket)},
					"cache.GetBW:dm:bw:0", "blockword.ListEnabled:0", "limiter.Done:0")
			},
		},
		{
			name: "弹幕落库失败", arm: func(e *env) { e.st.danmaku.failWith("Insert", errDB) },
			want: errDB, rows: 0, submitted: 0,
			seq: func(bucket int64, key string) []string {
				// 撞库失败后 CreateDanmaku 还会回查一次唯一键
				return append(postGatesOps(bucket, key),
					findKeyOp(key), insertKeyOp(key), findKeyOp(key), "limiter.Done:0")
			},
		},
		{
			name: "机审下游失败", arm: func(e *env) { e.mod.err = errModeration },
			want: errModeration, rows: 1, submitted: 1,
			seq: func(bucket int64, key string) []string {
				return append(postInsertOps(bucket, key), "moderation.Submit:101/7001/danmaku-publish", "limiter.Done:0")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.mod.taskID = postTask // 除注入的故障外，送审本身可用
			bucket := e.st.cache.freezeWindows()
			tc.arm(e)

			reply, err := postDanmaku(t, e, postReq("cli-f1"))

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：故障仍返回结果 %+v", tc.name, reply)
			}
			wantOps(t, tc.name, e.ops(), tc.seq(bucket, goldenIdem(postOid, postMid, "cli-f1")))
			wantEQ(t, tc.name, "danmaku 行数", int64(e.st.danmaku.countRows()), int64(tc.rows))
			wantCount(t, tc.name, e.st.log, "moderation.Submit", tc.submitted)
			wantEQ(t, tc.name, "段表未被写过", e.st.segment.countAt(postOid, postSeg), int32(-1))
			wantEQ(t, tc.name, "op_log 未被写过", len(e.st.opLog.rows), 0)
		})
	}
}

// TestPostDanmakuModerationFailureKeepsRowUnpublished 送审失败时行已经落库，
// 但必须处于「不会被下发」的待审/审核池，task_id 仍为 0，交由重试补送。
func TestPostDanmakuModerationFailureKeepsRowUnpublished(t *testing.T) {
	e := newEnv(t)
	e.mod.err = errModeration

	reply, err := postDanmaku(t, e, postReq("cli-modfail"))

	wantErrIs(t, "送审失败", err, errModeration)
	if reply != nil {
		t.Errorf("送审失败仍返回结果 %+v", reply)
	}
	row := e.st.danmaku.only()
	wantEQ(t, "送审失败", "state", row.State, model.StatePending)
	wantEQ(t, "送审失败", "pool", row.Pool, model.PoolReview)
	wantEQ(t, "送审失败", "moderation_task_id", row.ModerationTaskId, int64(0))
	wantCount(t, "送审失败", e.st.log, "danmaku.SetTask", 0)
}

// TestPostDanmakuStillRepliesWhenTaskIDBackfillFails 回填 task_id 失败只记日志：
// 任务已在下游创建，整体报错会让客户端重复送审。这里锁定「不报错」这一取舍，
// 同时把「回复里的 task 与库里不一致」的窗口显式记录下来（README 缺口）。
func TestPostDanmakuStillRepliesWhenTaskIDBackfillFails(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	e.st.danmaku.failWith("SetModerationTaskID", errDB)

	reply, err := postDanmaku(t, e, postReq("cli-backfill"))

	wantNoErr(t, "回填失败不影响发送", err)
	wantEQ(t, "回填失败", "回复里的 task", reply.ModerationTaskId, postTask)
	wantEQ(t, "回填失败", "库里的 task", e.st.danmaku.only().ModerationTaskId, int64(0))
}

// TestPostDanmakuDegradesToDbWhenBlockWordCacheBroken 词库缓存读写失败时降级直读 DB：
// 不阻断发送，也不允许「读不到词库就默认放行」。
func TestPostDanmakuDegradesToDbWhenBlockWordCacheBroken(t *testing.T) {
	e := newEnv(t)
	e.mod.taskID = postTask
	e.st.cache.failWith("GetBlockWords", errCache)
	e.st.cache.failWith("SetBlockWords", errCache)
	seedBlockWord(t, e.st, &model.BlockWord{Word: "广告", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})
	in := postReq("cli-degrade")
	in.Content = "这里有广告"

	reply, err := postDanmaku(t, e, in)

	wantNoErr(t, "缓存故障降级读库", err)
	wantEQ(t, "缓存故障降级读库", "state", reply.State, model.StateFolded)
	wantEQ(t, "缓存故障降级读库", "pool", reply.Pool, model.PoolBlock)
	wantCount(t, "缓存故障降级", e.st.log, "cache.GetBW", 2)
	wantCount(t, "缓存故障降级", e.st.log, "blockword.ListEnabled", 2)
	wantCount(t, "缓存故障降级", e.st.log, "cache.SetBW", 2)
}

// TestPostDanmakuRateLimitHitsMidDimension 同一用户在同一分钟窗口内反复发同一内容：
// 内容本身不判重（每行都落库），到阈值才由分钟窗口拒绝；被拒的那次仍然把窗口计数推了上去。
func TestPostDanmakuRateLimitHitsMidDimension(t *testing.T) {
	cfg := defaultConf()
	cfg.MaxPerUserPerMinute = 3
	e := newEnvConf(t, cfg)
	e.mod.taskID = postTask
	bucket := e.st.cache.freezeWindows()

	for i := range 3 {
		in := postReq(fmt.Sprintf("cli-burst-%d", i))
		in.Content = "刷屏的同一条内容" // 内容完全相同，只换幂等键
		reply, err := postDanmaku(t, e, in)
		wantNoErr(t, fmt.Sprintf("窗口内第 %d 条", i+1), err)
		wantEQ(t, fmt.Sprintf("窗口内第 %d 条", i+1), "state", reply.State, model.StatePending)
	}
	wantEQ(t, "同内容不做内容级判重", "落库行数", int64(e.st.danmaku.countRows()), 3)

	before := e.st.log.snapshot()
	reply, err := postDanmaku(t, e, postReq("cli-burst-3"))
	wantErrIs(t, "第 4 条超阈值", err, model.ErrRateLimited)
	wantErrContains(t, "第 4 条超阈值", err, ": mid")
	if reply != nil {
		t.Errorf("被限流的请求仍返回结果 %+v", reply)
	}
	tail := e.st.log.opsFrom(before)
	wantOps(t, "第 4 条超阈值", tail, []string{
		"limiter.Allow",
		findKeyOp(goldenIdem(postOid, postMid, "cli-burst-3")),
		midRateOp(postMid, bucket),
		oidRateOp(postOid, bucket),
		"limiter.Done:0",
	})
	wantEQ(t, "第 4 条超阈值", "本次未落库", countIn(tail, "danmaku.Insert"), 0)
	wantEQ(t, "第 4 条超阈值", "本次未读词库", countIn(tail, "cache.GetBW"), 0)
	wantEQ(t, "同内容不做内容级判重", "拒绝后行数不变", int64(e.st.danmaku.countRows()), 3)
	// 被拒请求也已消耗窗口计数（固定窗口 INCR 先于判定）——真实口径，用来自查
	// 「客户端狂刷会不会把自己锁在门外」：桶随分钟滚动，key 自带分钟维度。
	wantEQ(t, "第 4 条超阈值", "mid 窗口累计", e.st.cache.window(keyRateMid, postMid, bucket), 4)
	wantEQ(t, "第 4 条超阈值", "oid 窗口累计", e.st.cache.window(keyRateOid, postOid, bucket), 4)
}

// TestPostDanmakuRateLimitHitsOidDimension 单个内容被多用户刷屏时按 oid 维度拒绝，
// 且各发送者的 mid 维度都没超限。
func TestPostDanmakuRateLimitHitsOidDimension(t *testing.T) {
	cfg := defaultConf()
	cfg.MaxPerOidPerMinute = 2
	e := newEnvConf(t, cfg)
	e.mod.taskID = postTask
	bucket := e.st.cache.freezeWindows()

	for i, mid := range []int64{11, 12, 13} {
		in := postReq(fmt.Sprintf("cli-oid-%d", i))
		in.Mid = mid
		_, err := postDanmaku(t, e, in)
		if i < 2 {
			wantNoErr(t, fmt.Sprintf("oid 窗口内第 %d 条", i+1), err)
			continue
		}
		wantErrIs(t, "oid 维度超限", err, model.ErrRateLimited)
		wantErrContains(t, "oid 维度超限", err, ": oid")
	}
	wantEQ(t, "oid 维度超限", "落库行数", int64(e.st.danmaku.countRows()), 2)
	wantEQ(t, "oid 维度超限", "oid 窗口累计", e.st.cache.window(keyRateOid, postOid, bucket), 3)
	wantEQ(t, "oid 维度超限", "第三位用户 mid 计数", e.st.cache.window(keyRateMid, 13, bucket), 1)
}

// TestPostDanmakuMidDimensionReportedFirst 两个维度同时超限时先报 mid：
// 用户级配额是更贴近调用方的原因，gateway 据此提示也更准确。
func TestPostDanmakuMidDimensionReportedFirst(t *testing.T) {
	cfg := defaultConf()
	cfg.MaxPerUserPerMinute = 1
	cfg.MaxPerOidPerMinute = 1
	e := newEnvConf(t, cfg)
	e.mod.taskID = postTask

	_, err := postDanmaku(t, e, postReq("cli-both-1"))
	wantNoErr(t, "第一条", err)
	_, err = postDanmaku(t, e, postReq("cli-both-2"))
	wantErrIs(t, "双维超限", err, model.ErrRateLimited)
	wantErrContains(t, "双维超限", err, ": mid")
}

// TestPostDanmakuUnlimitedWindowsNeverTrip 阈值 <= 0 表示该维度不限流（policy.CheckWindows 口径）：
// 关掉防刷屏后连续发送不该被拒，但窗口计数仍在累积（一旦重新打开会立刻触发）。
func TestPostDanmakuUnlimitedWindowsNeverTrip(t *testing.T) {
	cfg := defaultConf()
	cfg.MaxPerUserPerMinute = 0
	cfg.MaxPerOidPerMinute = 0
	e := newEnvConf(t, cfg)
	e.mod.taskID = postTask
	bucket := e.st.cache.freezeWindows()

	for i := range 5 {
		_, err := postDanmaku(t, e, postReq(fmt.Sprintf("cli-free-%d", i)))
		wantNoErr(t, fmt.Sprintf("不限流第 %d 条", i+1), err)
	}
	wantEQ(t, "不限流", "落库行数", int64(e.st.danmaku.countRows()), 5)
	wantEQ(t, "不限流", "mid 窗口仍在累积", e.st.cache.window(keyRateMid, postMid, bucket), 5)
}

// TestPostDanmakuSensitiveWordGateCanBeDisabled 关闭屏蔽词门禁后不再读词库，
// 也就不会折叠（这条链路是配置开关，显式测出来防止误改）。
func TestPostDanmakuSensitiveWordGateCanBeDisabled(t *testing.T) {
	cfg := defaultConf()
	cfg.SensitiveWordCheckEnabled = false
	e := newEnvConf(t, cfg)
	e.mod.taskID = postTask
	seedBlockWord(t, e.st, &model.BlockWord{Word: "广告", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})
	in := postReq("cli-noword")
	in.Content = "满屏广告"

	reply, err := postDanmaku(t, e, in)

	wantNoErr(t, "关闭屏蔽词门禁", err)
	wantEQ(t, "关闭屏蔽词门禁", "state", reply.State, model.StatePending)
	wantCount(t, "关闭屏蔽词门禁", e.st.log, "cache.GetBW", 0)
	wantCount(t, "关闭屏蔽词门禁", e.st.log, "blockword.", 0)
}

// TestPostDanmakuOversizedBlockWordListBypassesCache 词库条数超过缓存上限时不写缓存，
// 避免热 key 大 value；发送仍要正确判定命中。
func TestPostDanmakuOversizedBlockWordListBypassesCache(t *testing.T) {
	cfg := defaultConf()
	cfg.BlockWordCacheMaxWords = 1
	e := newEnvConf(t, cfg)
	e.mod.taskID = postTask
	seedBlockWord(t, e.st, &model.BlockWord{Word: "广告", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})
	seedBlockWord(t, e.st, &model.BlockWord{Word: "spam", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 99})
	in := postReq("cli-oversize")
	in.Content = "有广告"

	reply, err := postDanmaku(t, e, in)

	wantNoErr(t, "词库过大绕过缓存", err)
	wantEQ(t, "词库过大绕过缓存", "state", reply.State, model.StateFolded)
	wantCount(t, "词库过大绕过缓存", e.st.log, "cache.SetBW", 0)
	wantCount(t, "词库过大绕过缓存", e.st.log, "blockword.ListEnabled", 2)
}
