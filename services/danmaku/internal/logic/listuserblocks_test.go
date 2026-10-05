package logic

import (
	"context"
	"fmt"
	"testing"

	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

// 本文件覆盖 ListUserBlocks（查询某人的屏蔽清单）。
//
// 这是读接口，但有两条别的读接口没有的边界，必须钉住：
//  1. 行的归属：danmaku_user_block 的唯一键是 uniq_mid_target(mid, blocked_mid, keyword)，
//     查询必须以 mid 为硬过滤，甲的清单里绝不能出现乙的行（否则等于把别人的隐私偏好整包端走）；
//  2. total 与列表口径**故意不一致**（model/danmaku_user_block.go:129-132）：
//     total 是「筛选条件下的全部行（含 state=0 已解除）」，列表只返回 state=1 的行。
//     于是存在一个真实可达的形态：total>0 却一页都翻不出来。本文件把它钉成期望值，
//     将来若要统一口径，改这里即可暴露影响面。
//
// 另外钉两条旁路不变量：分页参数**原样下传**（夹紧只发生在 model，两层不许各自造默认值）；
// 本方法纯读，不许碰 dm:ub:* 与发送侧任何缓存（清掉只会把线上视图打空再回填旧数据）。

const ubOther = int64(7010) // 另一个用户，用于钉隔离

// --- 轨迹期望 ---

func ubListOp(mid int64, blockType, pn, ps int32) string {
	return fmt.Sprintf("userblock.List:%d/%d/%d/%d", mid, blockType, pn, ps)
}

// --- 布景 ---

func callListUserBlocks(t *testing.T, e *env, in *rpc.ListUserBlocksReq) (*rpc.ListUserBlocksReply, error) {
	t.Helper()
	return NewListUserBlocksLogic(context.Background(), e.svcCtx).ListUserBlocks(in)
}

// seedUBMix 布四类行（自增主键按写入顺序 701/702/703/704）：
// 本人屏蔽用户（生效）、本人屏蔽关键词（生效）、本人已解除项、另一个用户的生效项。
// 返回库里那四行的权威副本，顺序与下列注释一致。
func seedUBMix(t *testing.T, e *env) []*model.UserBlock {
	t.Helper()
	rows := []*model.UserBlock{
		seedUserBlock(t, e.st, &model.UserBlock{Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: ubTargetA, State: model.UserBlockOn}),
		seedUserBlock(t, e.st, &model.UserBlock{Mid: ubSelf, Type: model.UserBlockKeyword, Keyword: "剧透警告", State: model.UserBlockOn}),
		seedUserBlock(t, e.st, &model.UserBlock{Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: ubTargetB, State: model.UserBlockOff}),
		seedUserBlock(t, e.st, &model.UserBlock{Mid: ubOther, Type: model.UserBlockMid, BlockedMid: ubTargetA, State: model.UserBlockOn}),
	}
	// ctime/mtime 落在不同秒：否则「投影把 ctime/mtime 串列」这类缺陷测不出来。
	ubBackdate(t, e, ubSelf, ubTargetA, "", 1700000001, 1700000101)
	ubBackdate(t, e, ubSelf, 0, "剧透警告", 1700000002, 1700000102)
	ubBackdate(t, e, ubSelf, ubTargetB, "", 1700000003, 1700000103)
	ubBackdate(t, e, ubOther, ubTargetA, "", 1700000004, 1700000104)
	return rows
}

// seedUBOnMids 给本人布 n 条生效的「屏蔽用户」项，blocked_mid 依次 8001..800n，
// 主键 701..700+n；List 按 id DESC 返回，于是期望顺序 = 逆序。
func seedUBOnMids(t *testing.T, e *env, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		row := seedUserBlock(t, e.st, &model.UserBlock{
			Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: int64(8001 + i), State: model.UserBlockOn,
		})
		ids = append(ids, row.ID)
	}
	return ids
}

// --- 守卫 ---

// TestListUserBlocksRejectsInvalidRequests 守卫表：非法入参必须在碰任何依赖之前被拒。
// mid<=0 尤其要紧：本方法没有 operator 概念，mid 就是归属者，放行 0 会把「无主行」当成一份合法清单。
func TestListUserBlocksRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListUserBlocksReq
		want error
	}{
		{"mid 缺失", &rpc.ListUserBlocksReq{Type: rpc.UserBlockType_USER_BLOCK_MID, Pn: 1, Ps: 20}, model.ErrInvalidMid},
		{"mid 为负", &rpc.ListUserBlocksReq{Mid: -7, Pn: 1, Ps: 20}, model.ErrInvalidMid},
		{"mid 非法优先于类型非法", &rpc.ListUserBlocksReq{Mid: 0, Type: rpc.UserBlockType(6)}, model.ErrInvalidMid},
		{"未知类型取值", &rpc.ListUserBlocksReq{Mid: ubSelf, Type: rpc.UserBlockType(6)}, model.ErrInvalidUserBlock},
		{"未知类型取值（负数）", &rpc.ListUserBlocksReq{Mid: ubSelf, Type: rpc.UserBlockType(-1)}, model.ErrInvalidUserBlock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedUBMix(t, e)
			e.st.cache.warmUserBlocks(ubSelf, &model.UserBlock{Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: ubTargetA, State: model.UserBlockOn})
			before := e.st.log.snapshot()

			reply, err := callListUserBlocks(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法请求仍返回 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			_, ok := e.st.cache.userBlockSet(ubSelf)
			wantEQ(t, tc.name, "dm:ub:<mid> 未被清", ok, true)
		})
	}
}

// --- 正常路径 ---

// TestListUserBlocksProjectsEveryField 逐字段钉死 model→rpc 投影（8 个字段都不许漏/串），
// 同时钉住顺序（id DESC）、total 口径（含已解除）与「只读」这条约束。
func TestListUserBlocksProjectsEveryField(t *testing.T) {
	e := newEnv(t)
	seedUBMix(t, e)

	reply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubSelf, Pn: 1, Ps: 10})
	wantNoErr(t, "本人清单首页", err)
	if reply == nil {
		t.Fatal("本人清单首页：回复 = nil, want 非空")
	}

	wantEQ(t, "本人清单首页", "条数（已解除项不出现）", len(reply.Blocks), 2)
	wantEQ(t, "本人清单首页", "total（含已解除项）", reply.Total, int32(3))
	wantInt64sEQ(t, "本人清单首页", []int64{reply.Blocks[0].Id, reply.Blocks[1].Id}, []int64{702, 701})

	// 期望值一律回读库里的行，而不是拿布景时的旧副本自证。
	want := []*model.UserBlock{ubRow(t, e, ubSelf, 0, "剧透警告"), ubRow(t, e, ubSelf, ubTargetA, "")}
	for i, got := range reply.Blocks {
		label := fmt.Sprintf("本人清单第 %d 行", i+1)
		wantEQ(t, label, "id", got.Id, want[i].ID)
		wantEQ(t, label, "mid", got.Mid, want[i].Mid)
		wantEQ(t, label, "type", got.Type, want[i].Type)
		wantEQ(t, label, "blocked_mid", got.BlockedMid, want[i].BlockedMid)
		wantEQ(t, label, "keyword", got.Keyword, want[i].Keyword)
		wantEQ(t, label, "state", got.State, want[i].State)
		wantEQ(t, label, "ctime", got.Ctime, want[i].Ctime)
		wantEQ(t, label, "mtime", got.Mtime, want[i].Mtime)
	}
	// 关键词项的 blocked_mid 必须是 0 占位：唯一键里 0/'' 是占位值，串了就会和「屏蔽用户」项撞键。
	wantEQ(t, "本人清单第 1 行", "关键词项 blocked_mid 为 0 占位", reply.Blocks[0].BlockedMid, int64(0))
	wantEQ(t, "本人清单第 2 行", "用户项 keyword 为空串占位", reply.Blocks[1].Keyword, "")

	wantOps(t, "本人清单首页", e.ops(), []string{ubListOp(ubSelf, 0, 1, 10)})
	// 纯读旁路：一个 cache.* 都不许出现，也不许走读接口的 ListEnabled（那是 ListDanmaku 的过滤路径）。
	wantCount(t, "本人清单首页", e.st.log, "cache.", 0)
	wantCount(t, "本人清单首页", e.st.log, "userblock.ListEnabled", 0)
	wantCount(t, "本人清单首页", e.st.log, "danmaku.", 0)
}

// TestListUserBlocksTypeFilterMapping 类型过滤映射表：UNSPECIFIED 不过滤，其余按枚举值下传。
// 这里是 int32(in.Type) 的裸转换，所以「rpc 枚举值 == model 常量」这件事必须被钉住，
// 任一侧改号就会静默查错类型而不是编译失败。
func TestListUserBlocksTypeFilterMapping(t *testing.T) {
	cases := []struct {
		name      string
		typ       rpc.UserBlockType
		wantType  int32
		wantWords []int64
		wantTypes []int32
	}{
		// 不过滤时返回项带着各自的 type（不是 0）：0 只是「不加这个条件」，不是行上的值。
		{"不过滤", rpc.UserBlockType_USER_BLOCK_UNSPECIFIED, 0, []int64{702, 701},
			[]int32{model.UserBlockKeyword, model.UserBlockMid}},
		{"只看屏蔽用户", rpc.UserBlockType_USER_BLOCK_MID, model.UserBlockMid, []int64{701},
			[]int32{model.UserBlockMid}},
		{"只看屏蔽关键词", rpc.UserBlockType_USER_BLOCK_KEYWORD, model.UserBlockKeyword, []int64{702},
			[]int32{model.UserBlockKeyword}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedUBMix(t, e)

			reply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubSelf, Type: tc.typ, Pn: 1, Ps: 10})
			wantNoErr(t, tc.name, err)
			got := make([]int64, 0, len(reply.Blocks))
			for _, b := range reply.Blocks {
				got = append(got, b.Id)
			}
			wantInt64sEQ(t, tc.name, got, tc.wantWords)
			if len(reply.Blocks) != len(tc.wantTypes) {
				t.Fatalf("%s：返回项数 = %d, want %d", tc.name, len(reply.Blocks), len(tc.wantTypes))
			}
			for i, b := range reply.Blocks {
				wantEQ(t, tc.name, fmt.Sprintf("第 %d 行的 type", i+1), b.Type, tc.wantTypes[i])
			}
			// 屏蔽用户项 filtered：total 也跟着 type 走（3 条本人项里 2 条是 type=1）。
			wantOps(t, tc.name, e.ops(), []string{ubListOp(ubSelf, tc.wantType, 1, 10)})
		})
	}
}

// TestListUserBlocksTotalCountsUnblockedRows 钉住 total 与列表的口径分裂：
// 全部项都已解除时，total 仍是条数、列表却一页都翻不出来。
// 前端若按 total 渲染「共 N 条」并分页，就会翻到空页——缺陷登记进 README 已知缺口。
func TestListUserBlocksTotalCountsUnblockedRows(t *testing.T) {
	cases := []struct {
		name      string
		off, on   int
		wantTotal int32
		wantRows  int
	}{
		{"只有已解除项", 2, 0, 2, 0},
		{"生效与已解除混合（解除项被藏起来）", 2, 1, 3, 1},
		{"只有生效项", 0, 2, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			for i := 0; i < tc.off; i++ {
				seedUserBlock(t, e.st, &model.UserBlock{
					Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: int64(8101 + i), State: model.UserBlockOff,
				})
			}
			for i := 0; i < tc.on; i++ {
				seedUserBlock(t, e.st, &model.UserBlock{
					Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: int64(8201 + i), State: model.UserBlockOn,
				})
			}

			reply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubSelf, Pn: 1, Ps: 10})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "total（含已解除）", reply.Total, tc.wantTotal)
			wantEQ(t, tc.name, "列表条数（只含生效）", len(reply.Blocks), tc.wantRows)
			if reply.Blocks == nil {
				t.Errorf("%s：Blocks = nil, want 非 nil 空切片", tc.name)
			}
			for _, b := range reply.Blocks {
				wantEQ(t, tc.name, "返回项必须都是生效态", b.State, model.UserBlockOn)
			}
			wantOps(t, tc.name, e.ops(), []string{ubListOp(ubSelf, 0, 1, 10)})
		})
	}
}

// TestListUserBlocksNoRowsAtAll 库里一条都没有时不许报错，也不许凭空造 total。
func TestListUserBlocksNoRowsAtAll(t *testing.T) {
	e := newEnv(t)

	reply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubSelf, Pn: 1, Ps: 20})
	wantNoErr(t, "空清单", err)
	if reply == nil {
		t.Fatal("空清单：回复 = nil, want 非空")
	}
	if reply.Blocks == nil {
		t.Error("空清单：Blocks = nil, want 非 nil 空切片")
	}
	wantEQ(t, "空清单", "条数", len(reply.Blocks), 0)
	wantEQ(t, "空清单", "total", reply.Total, int32(0))
	wantOps(t, "空清单", e.ops(), []string{ubListOp(ubSelf, 0, 1, 20)})
}

// TestListUserBlocksPaginationIsPassedThroughUnchanged pn/ps 必须原样下传：
// 夹紧（ps<1||ps>100→20、pn<1→1）只发生在 model 里，两层各造一套默认值页码就会漂。
func TestListUserBlocksPaginationIsPassedThroughUnchanged(t *testing.T) {
	e := newEnv(t)
	seedUBOnMids(t, e, 5) // id 701..705，List 按 DESC ⇒ 705,704,703,702,701

	cases := []struct {
		name   string
		pn, ps int32
		want   []int64
	}{
		{"第 1 页每页 2 条", 1, 2, []int64{705, 704}},
		{"第 2 页每页 2 条", 2, 2, []int64{703, 702}},
		{"第 3 页只剩 1 条", 3, 2, []int64{701}},
		{"越界页码返回空页", 4, 2, nil},
		{"页码 0（model 夹成 1）", 0, 2, []int64{705, 704}},
		{"负页码（model 夹成 1）", -5, 2, []int64{705, 704}},
		{"每页 0（model 夹成 20）", 1, 0, []int64{705, 704, 703, 702, 701}},
		{"每页 500（超上限，model 夹成 20）", 1, 500, []int64{705, 704, 703, 702, 701}},
		{"每页 100（上限内，原样生效）", 1, 100, []int64{705, 704, 703, 702, 701}},
		{"每页负数（model 夹成 20）", 1, -3, []int64{705, 704, 703, 702, 701}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 十个子用例共用一份布景（分页只读，行不变），所以轨迹要按片段取，
			// 否则第二个子用例就会把前一个的 List 也算进期望序列。
			from := e.st.log.snapshot()
			reply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubSelf, Pn: tc.pn, Ps: tc.ps})
			wantNoErr(t, tc.name, err)
			got := make([]int64, 0, len(reply.Blocks))
			for _, b := range reply.Blocks {
				got = append(got, b.Id)
			}
			wantInt64sEQ(t, tc.name, got, tc.want)
			wantEQ(t, tc.name, "total 不受分页影响", reply.Total, int32(5))
			// 下传的仍是原始 pn/ps。
			wantOps(t, tc.name, e.st.log.opsFrom(from), []string{ubListOp(ubSelf, 0, tc.pn, tc.ps)})
		})
	}
}

// --- 归属与隔离 ---

// TestListUserBlocksRowsAreIsolatedByMid 甲的清单不含乙的一行，反之亦然。
// 唯一键前缀是 mid，所以只要过滤条件漏掉 mid，整张表就会串成一份清单——这里钉死不漏。
func TestListUserBlocksRowsAreIsolatedByMid(t *testing.T) {
	e := newEnv(t)
	seedUBMix(t, e)

	selfReply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubSelf, Pn: 1, Ps: 10})
	wantNoErr(t, "本人清单", err)
	selfIDs := make([]int64, 0, len(selfReply.Blocks))
	for _, b := range selfReply.Blocks {
		wantEQ(t, "本人清单", "返回项 mid 恒为本人", b.Mid, ubSelf)
		selfIDs = append(selfIDs, b.Id)
	}
	wantInt64sEQ(t, "本人清单", selfIDs, []int64{702, 701})
	wantEQ(t, "本人清单", "total 不含他人行", selfReply.Total, int32(3))

	otherReply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubOther, Pn: 1, Ps: 10})
	wantNoErr(t, "他人清单", err)
	otherIDs := make([]int64, 0, len(otherReply.Blocks))
	for _, b := range otherReply.Blocks {
		wantEQ(t, "他人清单", "返回项 mid 恒为该用户", b.Mid, ubOther)
		otherIDs = append(otherIDs, b.Id)
	}
	wantInt64sEQ(t, "他人清单", otherIDs, []int64{704})
	wantEQ(t, "他人清单", "total 与本人清单互不污染", otherReply.Total, int32(1))

	// 两次查询之间没有任何交叉写入或缓存动作
	wantOps(t, "隔离核对", e.ops(), []string{ubListOp(ubSelf, 0, 1, 10), ubListOp(ubOther, 0, 1, 10)})
	wantCount(t, "隔离核对", e.st.log, "cache.", 0)
}

// TestListUserBlocksTrustsRequestSuppliedMid 钉住现状缺陷：
// 服务侧唯一的归属校验就是「按 in.Mid 过滤」，它并不核对 in.Mid 是否就是登录用户。
// listuserblockslogic.go:28 的注释写着「由 gateway 校验登录 mid 与 in.Mid 一致」，
// 但 gateway/app 的两个入口（/danmaku/user_block 与 /danmaku/user_blocks）都直接透传
// 请求参数里的 mid，且该路由组没有任何鉴权中间件 ⇒ 换一个 mid 就能读出他人的屏蔽清单。
// 修法落地（服务侧收会话或网关补校验）时，本用例应改为断言越权被拒。
func TestListUserBlocksTrustsRequestSuppliedMid(t *testing.T) {
	e := newEnv(t)
	seedUBMix(t, e)
	e.st.cache.warmUserBlocks(ubSelf, &model.UserBlock{Mid: ubSelf, Type: model.UserBlockMid, BlockedMid: ubTargetA, State: model.UserBlockOn})

	// 以「本人」为被测调用者，却问别人的清单：现状是照单全给。
	reply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubOther, Pn: 1, Ps: 10})
	wantNoErr(t, "拿他人 mid 查询（现状）", err)
	wantEQ(t, "拿他人 mid 查询（现状）", "越权读到的条数", len(reply.Blocks), 1)
	wantEQ(t, "拿他人 mid 查询（现状）", "越权读到的行 mid", reply.Blocks[0].Mid, ubOther)
	wantEQ(t, "拿他人 mid 查询（现状）", "越权读到的被屏蔽对象", reply.Blocks[0].BlockedMid, ubTargetA)
	wantOps(t, "拿他人 mid 查询（现状）", e.ops(), []string{ubListOp(ubOther, 0, 1, 10)})
	// 越权读还顺带不能把本人的缓存清掉——现状是整条链路不碰缓存，所以这一点是安全的。
	_, ok := e.st.cache.userBlockSet(ubSelf)
	wantEQ(t, "拿他人 mid 查询（现状）", "本人缓存原样还在", ok, true)
}

// --- 下游故障 ---

// TestListUserBlocksPropagatesListFailure 查库失败必须原样上抛且不得返回半个清单：
// 把「查库失败」渲染成「你一条都没屏蔽」会让用户重复提交屏蔽，也会让运营误判。
func TestListUserBlocksPropagatesListFailure(t *testing.T) {
	e := newEnv(t)
	seedUBMix(t, e)
	e.st.userBlock.failWith("List", errDB)

	reply, err := callListUserBlocks(t, e, &rpc.ListUserBlocksReq{Mid: ubSelf, Pn: 1, Ps: 10})

	wantErrIs(t, "清单查询失败", err, errDB)
	if reply != nil {
		t.Errorf("清单查询失败仍返回 %+v", reply)
	}
	wantOps(t, "清单查询失败", e.ops(), []string{ubListOp(ubSelf, 0, 1, 10)})
	wantCount(t, "清单查询失败", e.st.log, "cache.", 0)
}
