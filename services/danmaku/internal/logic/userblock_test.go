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

// 本文件覆盖 UserBlock（用户个人屏蔽：屏蔽某个人 / 屏蔽某关键词，以及解除）。
//
// 领域边界（deploy/migrations/danmaku/000002 + model/danmaku_user_block.go）：
// 用户屏蔽**只在读取侧生效**（ListDanmaku 按请求者的 mid 过滤），不改弹幕主表状态，
// 所以「解除屏蔽」不需要回填历史。与屏蔽词的根本区别是归属：
// 屏蔽词是运营维护的全域词库，用户屏蔽是**每人自己的一份视图**。因此本方法要钉死：
//  1. 行的归属列 mid 恒等于请求者本人——不能替别人建屏蔽，也不能清别人的名单；
//  2. 缓存失效只能打在**本人**的 dm:ub:<mid> 上（打到被屏蔽者身上既无意义又是越权）；
//  3. uniq_mid_target 让屏蔽/解除都是 upsert：同目标重复提交只更新 state，不换主键、不新增行；
//  4. type 与目标列必须自洽：type=1 只填 blocked_mid、keyword 恒空串，
//     type=2 只填 keyword、blocked_mid 恒 0——唯一键里 0/'' 是占位值，写脏了就会串键。

const (
	ubSelf    = int64(7001) // 操作者本人
	ubTargetA = int64(7002) // 被屏蔽用户 A
	ubTargetB = int64(7003) // 被屏蔽用户 B
)

// --- 轨迹期望 ---

func ubUpsertOp(mid, blockedMid int64, keyword string) string {
	return "userblock.Upsert:" + ubKey(mid, blockedMid, keyword)
}

// ubDelCacheOp 复刻 repository.UpsertUserBlock 的失效动作：只删本人那一份视图缓存。
func ubDelCacheOp(mid int64) string { return "cache.DelUB:" + fmt.Sprintf(keyUserBlock, mid) }

// --- 布景与调用 ---

func callUserBlock(t *testing.T, e *env, in *rpc.UserBlockReq) (*rpc.EmptyReply, error) {
	t.Helper()
	return NewUserBlockLogic(context.Background(), e.svcCtx).UserBlock(in)
}

func ubBlockMid(mid, blocked int64) *rpc.UserBlockReq {
	return &rpc.UserBlockReq{Mid: mid, Type: rpc.UserBlockType_USER_BLOCK_MID, BlockedMid: blocked, TraceId: "trace-ub"}
}

func ubBlockKeyword(mid int64, kw string) *rpc.UserBlockReq {
	return &rpc.UserBlockReq{Mid: mid, Type: rpc.UserBlockType_USER_BLOCK_KEYWORD, Keyword: kw, TraceId: "trace-ub"}
}

func ubUnblock(in *rpc.UserBlockReq) *rpc.UserBlockReq {
	in.Unblock = true
	return in
}

// ubRow 读库里那一行（副本，断言用）；按唯一键定位，行不在即 fatal。
func ubRow(t *testing.T, e *env, mid, blockedMid int64, keyword string) *model.UserBlock {
	t.Helper()
	row, ok := e.st.userBlock.rows[ubKey(mid, blockedMid, keyword)]
	if !ok {
		t.Fatalf("前置条件不成立：屏蔽项 %s 不在库里", ubKey(mid, blockedMid, keyword))
	}
	cp := *row
	return &cp
}

// ubBackdate 把某行的 ctime/mtime 改成过去的定值（静默改，不记轨迹），
// 否则同秒执行时「mtime 被刷新」的断言会因时钟粒度随机变红。
func ubBackdate(t *testing.T, e *env, mid, blockedMid int64, keyword string, ctime, mtime int64) {
	t.Helper()
	row, ok := e.st.userBlock.rows[ubKey(mid, blockedMid, keyword)]
	if !ok {
		t.Fatalf("布景前置条件不成立：屏蔽项 %s 不在库里", ubKey(mid, blockedMid, keyword))
	}
	row.Ctime, row.Mtime = ctime, mtime
}

// --- 守卫 ---

// TestUserBlockRejectsInvalidRequests 守卫表：所有非法入参都必须在碰任何依赖之前被拒。
// 尤其要钉「匿名 mid」这条：本方法没有任何 operator 字段，mid 既是调用者也是归属者，
// mid 缺失时绝不能落到 (0, x, ”) 这种无主行上。
func TestUserBlockRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.UserBlockReq
		want error
	}{
		{"mid 缺失", &rpc.UserBlockReq{Type: rpc.UserBlockType_USER_BLOCK_MID, BlockedMid: ubTargetA}, model.ErrInvalidMid},
		{"mid 为负", &rpc.UserBlockReq{Mid: -1, Type: rpc.UserBlockType_USER_BLOCK_MID, BlockedMid: ubTargetA}, model.ErrInvalidMid},
		{"mid 非法优先于目标非法", &rpc.UserBlockReq{Mid: 0, Type: rpc.UserBlockType_USER_BLOCK_KEYWORD}, model.ErrInvalidMid},
		{"屏蔽用户但不带 blocked_mid", ubBlockMid(ubSelf, 0), model.ErrInvalidUserBlock},
		{"屏蔽用户带负 blocked_mid", ubBlockMid(ubSelf, -9), model.ErrInvalidUserBlock},
		{"关键词为空串", ubBlockKeyword(ubSelf, ""), model.ErrInvalidUserBlock},
		{"关键词只有空白（TrimSpace 后为空）", ubBlockKeyword(ubSelf, " \t "), model.ErrInvalidUserBlock},
		{"关键词 65 字符（超 keyword 列宽 VARCHAR(64)）", ubBlockKeyword(ubSelf, strings.Repeat("词", 65)), model.ErrInvalidUserBlock},
		{"类型未指定", &rpc.UserBlockReq{Mid: ubSelf, BlockedMid: ubTargetA}, model.ErrInvalidUserBlock},
		{"未知类型取值", &rpc.UserBlockReq{Mid: ubSelf, Type: rpc.UserBlockType(6)}, model.ErrInvalidUserBlock},
		{"解除一个非法目标", ubUnblock(ubBlockMid(ubSelf, 0)), model.ErrInvalidUserBlock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmUserBlocks(ubSelf, &model.UserBlock{Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: ubTargetA, State: model.UserBlockOn})
			before := e.st.log.snapshot()

			reply, err := callUserBlock(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法请求仍返回 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			wantEQ(t, tc.name, "屏蔽表没被写入", len(e.st.userBlock.rows), 0)
			// 守卫失败不许把本人的屏蔽视图缓存清掉
			_, ok := e.st.cache.userBlockSet(ubSelf)
			wantEQ(t, tc.name, "dm:ub:<self> 未被清", ok, true)
		})
	}
}

// TestUserBlockKeywordAtLengthBoundary 关键词长度门禁边界：64 字符放行、65 字符拒绝，
// 且入库的是 TrimSpace 之后的原文（列宽按字符计，中文 1 个算 1 个）。
func TestUserBlockKeywordAtLengthBoundary(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		want    string
		wantErr error
	}{
		{"恰好 64 个汉字", strings.Repeat("词", 64), strings.Repeat("词", 64), nil},
		{"65 个汉字", strings.Repeat("词", 65), "", model.ErrInvalidUserBlock},
		{"前后空白被裁掉", "  刷屏关键词  ", "刷屏关键词", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, err := callUserBlock(t, e, ubBlockKeyword(ubSelf, tc.keyword))
			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				wantEQ(t, tc.name, "被拒时不落库", len(e.st.userBlock.rows), 0)
				return
			}
			wantNoErr(t, tc.name, err)
			row := ubRow(t, e, ubSelf, 0, tc.want)
			wantEQ(t, tc.name, "keyword 入库值", row.Keyword, tc.want)
			wantEQ(t, tc.name, "带空白的原词没另开一行", len(e.st.userBlock.rows), 1)
		})
	}
}

// --- 归属与自屏蔽 ---

// TestUserBlockSelfIsRejected 不能屏蔽自己：ErrForbidden，且零副作用。
func TestUserBlockSelfIsRejected(t *testing.T) {
	e := newEnv(t)
	before := e.st.log.snapshot()

	reply, err := callUserBlock(t, e, ubBlockMid(ubSelf, ubSelf))

	wantErrIs(t, "屏蔽自己", err, model.ErrForbidden)
	if reply != nil {
		t.Errorf("屏蔽自己仍返回 %+v", reply)
	}
	wantNoCall(t, "屏蔽自己", e.st, before)
	wantEQ(t, "屏蔽自己", "屏蔽表为空", len(e.st.userBlock.rows), 0)
}

// TestUserBlockWritesOnlyCallersOwnRow 归属不变量（本方法最要紧的一条）：
// 行的 mid 恒等于请求者，A 屏蔽 B 不会在 B 的名下产生任何一行，
// 也**不会失效 B 的缓存视图**——B 的弹幕只是从 A 的读结果里消失，B 自己什么都没变。
func TestUserBlockWritesOnlyCallersOwnRow(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmUserBlocks(ubSelf, &model.UserBlock{Mid: ubSelf, State: model.UserBlockOn})
	e.st.cache.warmUserBlocks(ubTargetA, &model.UserBlock{Mid: ubTargetA, State: model.UserBlockOn})
	calledAt := time.Now().Unix()

	reply, err := callUserBlock(t, e, ubBlockMid(ubSelf, ubTargetA))
	wantNoErr(t, "A 屏蔽 B", err)
	if reply == nil {
		t.Fatal("A 屏蔽 B：回复 = nil, want 非空 EmptyReply")
	}

	wantEQ(t, "A 屏蔽 B", "只落一行", len(e.st.userBlock.rows), 1)
	row := ubRow(t, e, ubSelf, ubTargetA, "")
	wantEQ(t, "A 屏蔽 B", "id", row.ID, int64(701))
	wantEQ(t, "A 屏蔽 B", "mid 是请求者本人", row.Mid, ubSelf)
	wantEQ(t, "A 屏蔽 B", "type", row.Type, model.UserBlockMid)
	wantEQ(t, "A 屏蔽 B", "blocked_mid", row.BlockedMid, ubTargetA)
	wantEQ(t, "A 屏蔽 B", "keyword 占位为空串（唯一键要求）", row.Keyword, "")
	wantEQ(t, "A 屏蔽 B", "state 生效", row.State, model.UserBlockOn)
	if row.Ctime < calledAt || row.Ctime > calledAt+2 {
		t.Errorf("A 屏蔽 B：ctime = %d, want ≈ %d", row.Ctime, calledAt)
	}
	wantEQ(t, "A 屏蔽 B", "mtime == ctime（新建）", row.Mtime, row.Ctime)
	// 被屏蔽者名下没有多出一行
	wantEQ(t, "A 屏蔽 B", "B 的名下没有行", ubExists(e, ubTargetA, ubSelf, ""), false)

	wantOps(t, "A 屏蔽 B", e.ops(), []string{ubUpsertOp(ubSelf, ubTargetA, ""), ubDelCacheOp(ubSelf)})
	_, selfHit := e.st.cache.userBlockSet(ubSelf)
	wantEQ(t, "A 屏蔽 B", "本人缓存已失效", selfHit, false)
	_, targetHit := e.st.cache.userBlockSet(ubTargetA)
	wantEQ(t, "A 屏蔽 B", "被屏蔽者缓存不动（越权失效才是 bug）", targetHit, true)
	// 用户屏蔽不是弹幕状态迁移：不许写 op_log、不许开事务、不许动主表
	wantCount(t, "A 屏蔽 B", e.st.log, "oplog.", 0)
	wantCount(t, "A 屏蔽 B", e.st.log, "tx.", 0)
	wantCount(t, "A 屏蔽 B", e.st.log, "danmaku.", 0)
	wantEQ(t, "A 屏蔽 B", "弹幕主表没被碰", e.st.danmaku.countRows(), 0)
}

// ubExists 判断某个唯一键在不在库里。
func ubExists(e *env, mid, blockedMid int64, keyword string) bool {
	_, ok := e.st.userBlock.rows[ubKey(mid, blockedMid, keyword)]
	return ok
}

// TestUserBlockKeywordDropsForeignBlockedMid type=2 时 blocked_mid 必须被归一成 0：
// 若把请求里夹带的别人 mid 落进唯一键，同一关键词在不同请求下会裂成多行、
// 读侧过滤（按 keyword 匹配）也会莫名其妙。钉住「带脏字段也归一」。
func TestUserBlockKeywordDropsForeignBlockedMid(t *testing.T) {
	cases := []struct {
		name  string
		in    *rpc.UserBlockReq
		dirty int64
	}{
		{"关键词请求夹带别人的 blocked_mid", ubBlockKeyword(ubSelf, "剧透"), ubTargetA},
		{"关键词请求夹带自己的 blocked_mid", ubBlockKeyword(ubSelf, "剧透"), ubSelf},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.in.BlockedMid = tc.dirty

			_, err := callUserBlock(t, e, tc.in)

			if tc.dirty == ubSelf {
				// ⚠ 现状缺陷：自屏蔽门禁在类型归一**之前**执行，
				// 于是一条纯关键词屏蔽只因为客户端多塞了 blocked_mid=自己就被判成 ErrForbidden。
				wantErrIs(t, tc.name, err, model.ErrForbidden)
				wantEQ(t, tc.name, "被误拒时不落库", len(e.st.userBlock.rows), 0)
				return
			}
			wantNoErr(t, tc.name, err)
			row := ubRow(t, e, ubSelf, 0, "剧透")
			wantEQ(t, tc.name, "blocked_mid 被归一成 0", row.BlockedMid, int64(0))
			wantEQ(t, tc.name, "keyword", row.Keyword, "剧透")
			wantEQ(t, tc.name, "type", row.Type, model.UserBlockKeyword)
			wantEQ(t, tc.name, "脏字段没裂出新行", len(e.st.userBlock.rows), 1)
			wantOps(t, tc.name, e.ops(), []string{ubUpsertOp(ubSelf, 0, "剧透"), ubDelCacheOp(ubSelf)})
		})
	}
}

// TestUserBlockMidTypeDropsKeyword type=1 时 keyword 必须落空串，理由同上
// （uniq_mid_target 里 ” 是占位值，落进脏值会让「屏蔽某人」与「屏蔽某人+某词」混成两条语义）。
func TestUserBlockMidTypeDropsKeyword(t *testing.T) {
	e := newEnv(t)
	in := ubBlockMid(ubSelf, ubTargetA)
	in.Keyword = "顺手带的关键词"

	_, err := callUserBlock(t, e, in)

	wantNoErr(t, "屏蔽用户时夹带关键词", err)
	row := ubRow(t, e, ubSelf, ubTargetA, "")
	wantEQ(t, "屏蔽用户时夹带关键词", "keyword 落空串", row.Keyword, "")
	wantEQ(t, "屏蔽用户时夹带关键词", "type", row.Type, model.UserBlockMid)
	wantEQ(t, "屏蔽用户时夹带关键词", "只有一行", len(e.st.userBlock.rows), 1)
}

// --- upsert 幂等与解除 ---

// TestUserBlockRepeatIsIdempotentOnUniqueKey 同一目标重复屏蔽：
// 不换主键、不新增行、state 保持生效、ctime 不动、mtime 刷新。
func TestUserBlockRepeatIsIdempotentOnUniqueKey(t *testing.T) {
	e := newEnv(t)
	first := seedUserBlock(t, e.st, &model.UserBlock{Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: ubTargetA, State: model.UserBlockOn})
	ubBackdate(t, e, ubSelf, ubTargetA, "", 1_600_000_000, 1_600_000_001)
	before := ubRow(t, e, ubSelf, ubTargetA, "")
	calledAt := time.Now().Unix()

	_, err := callUserBlock(t, e, ubBlockMid(ubSelf, ubTargetA))
	wantNoErr(t, "重复屏蔽同一目标", err)

	row := ubRow(t, e, ubSelf, ubTargetA, "")
	wantEQ(t, "重复屏蔽同一目标", "行数仍为 1", len(e.st.userBlock.rows), 1)
	wantEQ(t, "重复屏蔽同一目标", "主键不变", row.ID, first.ID)
	wantEQ(t, "重复屏蔽同一目标", "state", row.State, model.UserBlockOn)
	wantEQ(t, "重复屏蔽同一目标", "type", row.Type, model.UserBlockMid)
	wantEQ(t, "重复屏蔽同一目标", "ctime 不被改写", row.Ctime, before.Ctime)
	if row.Mtime <= before.Mtime || row.Mtime < calledAt {
		t.Errorf("重复屏蔽同一目标：mtime = %d, want 刷到当前时间（原值 %d）", row.Mtime, before.Mtime)
	}
	wantEQ(t, "重复屏蔽同一目标", "自增号没被白吃掉", e.st.userBlock.next, first.ID)
	wantOps(t, "重复屏蔽同一目标", e.ops(), []string{ubUpsertOp(ubSelf, ubTargetA, ""), ubDelCacheOp(ubSelf)})
}

// TestUserBlockUnblockFlipsStateAndKeepsRowAsAnchor 解除屏蔽：
// 行必须留着（state=0 是幂等锚点），只翻 state；解除后本人缓存照旧失效。
func TestUserBlockUnblockFlipsStateAndKeepsRowAsAnchor(t *testing.T) {
	e := newEnv(t)
	seeded := seedUserBlock(t, e.st, &model.UserBlock{Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: ubTargetA, State: model.UserBlockOn})
	e.st.cache.warmUserBlocks(ubSelf, &model.UserBlock{Mid: ubSelf, BlockedMid: ubTargetA, State: model.UserBlockOn})

	reply, err := callUserBlock(t, e, ubUnblock(ubBlockMid(ubSelf, ubTargetA)))
	wantNoErr(t, "解除屏蔽", err)
	if reply == nil {
		t.Fatal("解除屏蔽：回复 = nil, want 非空 EmptyReply")
	}

	row := ubRow(t, e, ubSelf, ubTargetA, "")
	wantEQ(t, "解除屏蔽", "行留着（幂等锚点）", len(e.st.userBlock.rows), 1)
	wantEQ(t, "解除屏蔽", "state 翻成已解除", row.State, model.UserBlockOff)
	wantEQ(t, "解除屏蔽", "主键不变", row.ID, seeded.ID)
	wantEQ(t, "解除屏蔽", "归属仍是本人", row.Mid, ubSelf)
	wantEQ(t, "解除屏蔽", "目标不变", row.BlockedMid, ubTargetA)
	wantOps(t, "解除屏蔽", e.ops(), []string{ubUpsertOp(ubSelf, ubTargetA, ""), ubDelCacheOp(ubSelf)})
	_, hit := e.st.cache.userBlockSet(ubSelf)
	wantEQ(t, "解除屏蔽", "本人缓存已失效", hit, false)
}

// TestUserBlockUnblockOfUnknownTargetCreatesOffRow 钉住现状：
// 解除一条从没屏蔽过的目标不会报「不存在」，而是**新写一行 state=0**。
// 这是 uniq_mid_target upsert 设计的直接后果（迁移注释：保留行做幂等锚点），
// 副作用是 ListUserBlocks 的 total 会把这类「从没生效过」的幽灵行也算进去
// （见 listuserblocks_test.go 的 total/列表口径用例）。
func TestUserBlockUnblockOfUnknownTargetCreatesOffRow(t *testing.T) {
	e := newEnv(t)

	_, err := callUserBlock(t, e, ubUnblock(ubBlockKeyword(ubSelf, "从没屏蔽过的词")))

	wantNoErr(t, "解除未屏蔽过的关键词", err)
	row := ubRow(t, e, ubSelf, 0, "从没屏蔽过的词")
	wantEQ(t, "解除未屏蔽过的关键词", "凭空多了一行", len(e.st.userBlock.rows), 1)
	wantEQ(t, "解除未屏蔽过的关键词", "state 是已解除", row.State, model.UserBlockOff)
	wantEQ(t, "解除未屏蔽过的关键词", "归属仍是本人", row.Mid, ubSelf)
	wantOps(t, "解除未屏蔽过的关键词", e.ops(), []string{ubUpsertOp(ubSelf, 0, "从没屏蔽过的词"), ubDelCacheOp(ubSelf)})
}

// TestUserBlockTargetsDoNotCollide 同一个人的三种目标必须各占一行、互不覆盖：
// 唯一键裂错的话，屏蔽 B 会把屏蔽关键词的记录吃掉。
func TestUserBlockTargetsDoNotCollide(t *testing.T) {
	e := newEnv(t)

	for _, in := range []*rpc.UserBlockReq{
		ubBlockMid(ubSelf, ubTargetA),
		ubBlockMid(ubSelf, ubTargetB),
		ubBlockKeyword(ubSelf, "剧透"),
	} {
		_, err := callUserBlock(t, e, in)
		wantNoErr(t, "多目标写入", err)
	}

	wantEQ(t, "多目标写入", "三行独立", len(e.st.userBlock.rows), 3)
	wantEQ(t, "多目标写入", "A 行 state", ubRow(t, e, ubSelf, ubTargetA, "").State, model.UserBlockOn)
	wantEQ(t, "多目标写入", "B 行 state", ubRow(t, e, ubSelf, ubTargetB, "").State, model.UserBlockOn)
	wantEQ(t, "多目标写入", "关键词行 state", ubRow(t, e, ubSelf, 0, "剧透").State, model.UserBlockOn)
	wantInt64sEQ(t, "多目标写入", []int64{
		ubRow(t, e, ubSelf, ubTargetA, "").ID,
		ubRow(t, e, ubSelf, ubTargetB, "").ID,
		ubRow(t, e, ubSelf, 0, "剧透").ID,
	}, []int64{701, 702, 703})

	wantOps(t, "多目标写入", e.ops(), []string{
		ubUpsertOp(ubSelf, ubTargetA, ""), ubDelCacheOp(ubSelf),
		ubUpsertOp(ubSelf, ubTargetB, ""), ubDelCacheOp(ubSelf),
		ubUpsertOp(ubSelf, 0, "剧透"), ubDelCacheOp(ubSelf),
	})
}

// TestUserBlockDifferentOwnersAreIsolated 两个用户各屏蔽同一目标：两行归属不同、互不覆盖，
// 且每次都只失效自己那份缓存（不存在「替别人解除屏蔽」的通道）。
func TestUserBlockDifferentOwnersAreIsolated(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmUserBlocks(ubSelf, &model.UserBlock{Mid: ubSelf})
	e.st.cache.warmUserBlocks(ubTargetB, &model.UserBlock{Mid: ubTargetB})

	_, err := callUserBlock(t, e, ubBlockMid(ubSelf, ubTargetA))
	wantNoErr(t, "用户甲屏蔽", err)
	_, err = callUserBlock(t, e, ubBlockMid(ubTargetB, ubTargetA))
	wantNoErr(t, "用户乙屏蔽同一人", err)

	wantEQ(t, "双用户隔离", "两行各自归属", len(e.st.userBlock.rows), 2)
	wantEQ(t, "双用户隔离", "甲行的 mid", ubRow(t, e, ubSelf, ubTargetA, "").Mid, ubSelf)
	wantEQ(t, "双用户隔离", "乙行的 mid", ubRow(t, e, ubTargetB, ubTargetA, "").Mid, ubTargetB)
	wantOps(t, "双用户隔离", e.ops(), []string{
		ubUpsertOp(ubSelf, ubTargetA, ""), ubDelCacheOp(ubSelf),
		ubUpsertOp(ubTargetB, ubTargetA, ""), ubDelCacheOp(ubTargetB),
	})
}

// --- 下游故障 ---

// TestUserBlockPropagatesDownstreamFailures 写库失败必须上抛且不失效缓存；
// 只有缓存侧失败允许降级（TTL 兜底），且已提交的行不许被回滚掉。
func TestUserBlockPropagatesDownstreamFailures(t *testing.T) {
	cases := []struct {
		name    string
		arm     func(e *env)
		wantErr error
		// wantRow 是期望的库里行数
		wantRow int
		wantSt  int32
		// wantOps 是该用例的完整轨迹：写库失败时不得有失效步
		wantOps []string
	}{
		{
			name:    "写库失败",
			arm:     func(e *env) { e.st.userBlock.failWith("Upsert", errDB) },
			wantErr: errDB,
			wantRow: 0,
			wantOps: []string{ubUpsertOp(ubSelf, ubTargetA, "")},
		},
		{
			name:    "缓存失效失败只记日志",
			arm:     func(e *env) { e.st.cache.failWith("DelUserBlocks", errCache) },
			wantErr: nil,
			wantRow: 1,
			wantSt:  model.UserBlockOn,
			wantOps: []string{ubUpsertOp(ubSelf, ubTargetA, ""), ubDelCacheOp(ubSelf)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmUserBlocks(ubSelf, &model.UserBlock{Mid: ubSelf, State: model.UserBlockOn})
			tc.arm(e)

			reply, err := callUserBlock(t, e, ubBlockMid(ubSelf, ubTargetA))

			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				if reply != nil {
					t.Errorf("%s：故障仍返回 %+v", tc.name, reply)
				}
				// 写失败不许失效缓存：否则一次失败的写把线上视图打成空，
				// 下一次读还会把**旧库内容**回填成新数据。
				wantCount(t, tc.name, e.st.log, "cache.", 0)
				stored, ok := e.st.cache.userBlockSet(ubSelf)
				wantEQ(t, tc.name, "本人缓存原样还在", ok, true)
				wantEQ(t, tc.name, "缓存内容未被改动", len(stored), 1)
			} else {
				wantNoErr(t, tc.name, err)
				if reply == nil {
					t.Fatalf("%s：可降级故障把屏蔽打成了失败", tc.name)
				}
			}
			wantOps(t, tc.name, e.ops(), tc.wantOps)
			wantEQ(t, tc.name, "屏蔽表行数", len(e.st.userBlock.rows), tc.wantRow)
			if tc.wantErr == nil {
				wantEQ(t, tc.name, "行 state", ubRow(t, e, ubSelf, ubTargetA, "").State, tc.wantSt)
			}
		})
	}
}
