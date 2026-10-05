package logic

// pin_logic_test.go 锁置顶集合（feed_pin 表 + feed:pin:{mid} Set）的写侧语义：
//   - PinFeed 的读校验→DB→缓存三步顺序，以及**每步失败时到底留下了什么**；
//   - 归属校验只存在于 PinFeed（UnpinFeed 不查主表，钉住「取消置顶不需要动态还在」）；
//   - 重复置顶走 ON DUPLICATE（不报错、不加行），model.ErrPinAlreadyExists 是死代码；
//   - 并发窗口：FindOne 与 feed_pin.Add 之间动态被删除 → 留下孤儿置顶行（缺陷 D19）；
//   - 域结论：**置顶集合在本服务的 8 个 rpc 里没有任何读路径**（缺陷 D18）——
//     PullFeed/ListUserFeed 都不查 feed:pin，rpc.FeedItem 也没有置顶标记字段，
//     Repository.ListPins 只被自己引用（已用 Grep 确认全仓无调用点）。

import (
	"context"
	"testing"

	"go-video/services/feed/model"
	"go-video/services/feed/rpc"
)

func TestPinFeedWritesDbBeforeCache(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)

	st.log.reset()
	reply, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID})
	wantNoErr(t, "置顶自己的动态", err)
	wantEQ(t, "应答", "reply", reply != nil, true)
	wantOps(t, "先校验、再落库、最后写缓存", st.log.ops, []string{
		"feed_outbox.FindOne:" + itoa(row.ID),
		"feed_pin.Add:mid=101:fid=" + itoa(row.ID),
		"cache.AddPin:mid=101:fid=" + itoa(row.ID),
	})
	pinState, ok := st.pin.stateOf(author, row.ID)
	wantEQ(t, "DB 置顶行生效", "state", pinState, model.PinStateNormal)
	wantEQ(t, "DB 置顶行存在", "found", ok, true)
	wantInt64sEQ(t, "缓存集合与 DB 一致", "pins", st.cache.pinMembers(author), []int64{row.ID})
}

// TestPinFeedOwnershipAndStateRejections 钉住 repository.go:467 的三个拒绝分支：
// 别人的动态、已删除的动态、主表根本没有的 ID，都必须是 ErrFeedNotFound，
// 且**不得留下任何写副作用**（守卫在 feed_pin.Add 之前）。
func TestPinFeedOwnershipAndStateRejections(t *testing.T) {
	other := int64(202)
	deletedOwner := int64(101)

	st := newStore()
	mine := st.outbox.seed(deletedOwner, 5001, 1_000)
	gone := st.outbox.seed(deletedOwner, 5002, 900)
	st.outbox.seed(other, 5101, 800) // 别人空间里确实有内容，越权目标却是我的动态
	// 已删除的那行走生产写路径布防，免得形态不真实。
	if _, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: deletedOwner, FeedId: gone.ID}); err != nil {
		t.Fatalf("布防删除失败：%v", err)
	}
	ghost := st.outbox.lastID() + 900

	cases := []struct {
		name string
		mid  int64
		fid  int64
		why  string
	}{
		{"别人的动态", other, mine.ID, "越权置顶他人动态"},
		{"已删除的动态", deletedOwner, gone.ID, "复活已删除动态"},
		{"不存在的动态", deletedOwner, ghost, "凭空置顶"},
	}
	for _, tc := range cases {
		from := st.log.snapshot()
		reply, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
			&rpc.PinFeedReq{Mid: tc.mid, FeedId: tc.fid})
		wantErrIs(t, tc.why, err, model.ErrFeedNotFound)
		if reply != nil {
			t.Errorf("%s：出错时不应返回应答：%+v", tc.name, reply)
		}
		ops := st.log.opsFrom(from)
		wantOps(t, tc.name+"只允许一次主表读取", ops, []string{"feed_outbox.FindOne:" + itoa(tc.fid)})
		wantInt64sEQ(t, tc.name+"没有写进置顶集合", "pins", st.cache.pinMembers(tc.mid), nil)
		if _, ok := st.pin.stateOf(tc.mid, tc.fid); ok {
			t.Errorf("%s：feed_pin 里出现了残留行", tc.name)
		}
	}
	// 拒绝别人的动态不能顺手把自己/对方的既有置顶改掉。
	wantInt64sEQ(t, "被越权的那条动态仍未置顶", "pins", st.cache.pinMembers(deletedOwner), nil)
}

func TestPinFeedFindOneErrorPassesThrough(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	st.fault().failWith("feed_outbox.FindOne", errInjected)

	st.log.reset()
	_, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID})
	wantErrIs(t, "主表读取错误原样透传", err, errInjected)
	wantOps(t, "读校验失败后不得写置顶表/集合", st.log.ops, []string{
		"feed_outbox.FindOne:" + itoa(row.ID),
	})
}

// TestPinFeedDbFailureSkipsCache 置顶落库失败时缓存必须保持干净：
// 这一步顺序（DB 先、缓存后）是对的，用例把它钉住，防止反向重构。
func TestPinFeedDbFailureSkipsCache(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	st.fault().failWith("feed_pin.Add", errInjected)

	_, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID})
	wantErrIs(t, "置顶落库错误透传", err, errInjected)
	wantOps(t, "落库失败即止", st.log.ops, []string{
		"feed_outbox.FindOne:" + itoa(row.ID),
		"feed_pin.Add:mid=101:fid=" + itoa(row.ID),
	})
	wantInt64sEQ(t, "缓存里不该有置顶成员", "pins", st.cache.pinMembers(author), nil)
}

// TestPinFeedCacheFailureAlreadyCommittedDb 反向窗口：feed_pin 已提交、SADD 失败，
// 方法把错误返回给调用方，但库里那行置顶已经生效（repository.go:476 直接 return AddPin 的错误）。
// 后果：调用方看到失败、置顶实际存在；由于 ON DUPLICATE 幂等，重试可以自愈——
// 这条用例锁的是「重试前 DB 已有一行 normal」这个中间态。
func TestPinFeedCacheFailureAlreadyCommittedDb(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	st.fault().failWith("cache.AddPin", errCacheDown)

	_, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID})
	wantErrIs(t, "缓存写失败要报错（不静默）", err, errCacheDown)
	state, ok := st.pin.stateOf(author, row.ID)
	wantEQ(t, "DB 已经落定", "found", ok, true)
	wantEQ(t, "DB 已经落定", "state", state, model.PinStateNormal)
	wantInt64sEQ(t, "缓存还没有成员", "pins", st.cache.pinMembers(author), nil)

	// 重试自愈：SADD 成功后两侧一致，且 feed_pin 仍是 1 行。
	st.fault().failWith("cache.AddPin", nil)
	st.log.reset()
	_, err = NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID})
	wantNoErr(t, "重试置顶", err)
	wantOps(t, "重试仍走 ON DUPLICATE（不新增行）", st.log.ops, []string{
		"feed_outbox.FindOne:" + itoa(row.ID),
		"feed_pin.Add:mid=101:fid=" + itoa(row.ID),
		"cache.AddPin:mid=101:fid=" + itoa(row.ID),
	})
	wantEQ(t, "重试后置顶行仍只有 1 条", "normal pin rows", st.pin.normalCount(), 1)
	wantInt64sEQ(t, "重试后两侧一致", "pins", st.cache.pinMembers(author), []int64{row.ID})
}

// TestPinFeedRepeatIsSilentUpsert 重复置顶既不报错也不加行；
// 反面证据：model.ErrPinAlreadyExists 全仓没有被返回（已用 Grep 确认），
// 所以「已置顶」这个语义在服务里不存在。
func TestPinFeedRepeatIsSilentUpsert(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	svcCtx := st.svcCtx()

	for i := 1; i <= 3; i++ {
		st.log.reset()
		_, err := NewPinFeedLogic(context.Background(), svcCtx).PinFeed(
			&rpc.PinFeedReq{Mid: author, FeedId: row.ID})
		wantNoErr(t, "第 N 次置顶都应成功", err)
		wantCountIn(t, "每次都只做一次归属校验", st.log.ops, "feed_outbox.FindOne", 1)
		wantCountIn(t, "每次都只做一次置顶写入", st.log.ops, "feed_pin.Add", 1)
	}
	wantEQ(t, "3 次置顶在库里还是 1 行", "normal pin rows", st.pin.normalCount(), 1)
	wantInt64sEQ(t, "集合里也只有 1 个成员", "pins", st.cache.pinMembers(author), []int64{row.ID})
}

// TestPinFeedAfterUnpinResurrectsSameRow 取消置顶只是把 state 打成 1（软状态），
// 再次置顶必须复用同一行而不是插入第二行——否则 feed_pin 的 uniq(mid,feed_id) 会冲突。
func TestPinFeedAfterUnpinResurrectsSameRow(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	svcCtx := st.svcCtx()

	if _, err := NewPinFeedLogic(context.Background(), svcCtx).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID}); err != nil {
		t.Fatalf("首次置顶失败：%v", err)
	}
	if _, err := NewUnpinFeedLogic(context.Background(), svcCtx).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: row.ID}); err != nil {
		t.Fatalf("取消置顶失败：%v", err)
	}
	state, ok := st.pin.stateOf(author, row.ID)
	wantEQ(t, "取消置顶是软状态", "found", ok, true)
	wantEQ(t, "取消置顶是软状态", "state", state, model.PinStateDeleted)

	st.log.reset()
	if _, err := NewPinFeedLogic(context.Background(), svcCtx).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID}); err != nil {
		t.Fatalf("重新置顶失败：%v", err)
	}
	state, _ = st.pin.stateOf(author, row.ID)
	wantEQ(t, "重新置顶把同一行翻回 normal", "state", state, model.PinStateNormal)
	wantEQ(t, "重新置顶不产生第二行", "normal pin rows", st.pin.normalCount(), 1)
	wantInt64sEQ(t, "缓存成员回来", "pins", st.cache.pinMembers(author), []int64{row.ID})
}

// TestPinFeedRaceWithDeleteLeavesOrphanPin 并发窗口（缺陷 D19）：
// PinFeed 的归属校验（feed_outbox.FindOne）与 feed_pin.Add 之间没有事务，
// 期间 DeleteFeed 把动态软删除，置顶行照样写入，成为指向已删除动态的孤儿。
// repository.go:463-476（读校验、写库、写缓存三步无事务；NewWithDeps 注释也承认无 TransactCtx 路径）。
func TestPinFeedRaceWithDeleteLeavesOrphanPin(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	// 模拟另一个实例在本进程两次调用之间把这条动态软删除（等价于 DeleteFeed 第 1 步）。
	st.raceBefore("feed_pin.Add", func() {
		st.outbox.rows[row.ID].State = model.FeedStateDeleted
	})

	_, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID})
	wantNoErr(t, "并发删除后置顶仍然成功（孤儿行）", err)
	st.checkRaces(t)

	pinState, ok := st.pin.stateOf(author, row.ID)
	wantEQ(t, "孤儿置顶行存在", "found", ok, true)
	wantEQ(t, "孤儿置顶行状态", "state", pinState, model.PinStateNormal)
	wantInt64sEQ(t, "缓存集合里也有它", "pins", st.cache.pinMembers(author), []int64{row.ID})
	feedState, _ := st.outbox.stateOf(row.ID)
	wantEQ(t, "主表已是删除态", "feed state", feedState, model.FeedStateDeleted)
}

// TestPinFeedHasNoCountCap 置顶条数在 model 与 repository 两层都没有任何上限
// （feed_pin 只有 uniq(mid,feed_id)，没有 count 校验）：钉住现状，
// 一旦将来加了上限，这个用例会红并提醒改契约（proto 里也没有 max pin 字段）。
func TestPinFeedHasNoCountCap(t *testing.T) {
	st := newStore()
	author := int64(101)
	var ids []int64
	for i := 0; i < 6; i++ {
		row := st.outbox.seed(author, int64(5001+i), int64(1_000-i))
		if _, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
			&rpc.PinFeedReq{Mid: author, FeedId: row.ID}); err != nil {
			t.Fatalf("第 %d 条置顶失败：%v", i+1, err)
		}
		ids = append(ids, row.ID)
	}
	wantEQ(t, "6 条全部入库", "normal pin rows", st.pin.normalCount(), 6)
	// 缓存集合成员按 ID 倒序读回（与 Redis SMEMBERS 的无序性无关，这里只比集合内容）。
	wantEQ(t, "6 条全部进集合", "pin members", len(st.cache.pinMembers(author)), 6)
	for _, id := range ids {
		found := false
		for _, m := range st.cache.pinMembers(author) {
			if m == id {
				found = true
			}
		}
		if !found {
			t.Errorf("置顶成员 %d 不在集合 %v 里", id, st.cache.pinMembers(author))
		}
	}
}

func TestUnpinFeedSequenceAndEmptySetLosesKey(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	if _, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID}); err != nil {
		t.Fatalf("布防置顶失败：%v", err)
	}

	st.log.reset()
	reply, err := NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: row.ID})
	wantNoErr(t, "取消置顶", err)
	wantEQ(t, "应答", "reply", reply != nil, true)
	wantOps(t, "先删库再删集合，不回查主表", st.log.ops, []string{
		"feed_pin.Del:mid=101:fid=" + itoa(row.ID),
		"cache.RemPin:mid=101:fid=" + itoa(row.ID),
	})
	state, _ := st.pin.stateOf(author, row.ID)
	wantEQ(t, "置顶行转软删除", "state", state, model.PinStateDeleted)
	wantInt64sEQ(t, "SREM 删空即键消失", "pins", st.cache.pinMembers(author), nil)
}

// TestUnpinFeedNotFoundLeavesCacheUntouched 未置顶（或从未置顶）→ ErrPinNotFound，
// 且缓存一步都不执行（repository.go:481-484 先 DB 后缓存的顺序保证）。
func TestUnpinFeedNotFoundLeavesCacheUntouched(t *testing.T) {
	st := newStore()
	author := int64(101)
	mine := st.outbox.seed(author, 5001, 1_000)
	pinned := st.outbox.seed(author, 5002, 900)
	if _, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: pinned.ID}); err != nil {
		t.Fatalf("布防置顶失败：%v", err)
	}

	from := st.log.snapshot()
	_, err := NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: mine.ID})
	wantErrIs(t, "未置顶应返回 ErrPinNotFound", err, model.ErrPinNotFound)
	wantOps(t, "只有一次 Del，无 RemPin", st.log.ops[from:], []string{
		"feed_pin.Del:mid=101:fid=" + itoa(mine.ID),
	})
	wantInt64sEQ(t, "既有置顶集合不受影响", "pins", st.cache.pinMembers(author), []int64{pinned.ID})

	// 二次取消：同一条已软删除的行再次取消也是 ErrPinNotFound（WHERE state=0）。
	_, err = NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: pinned.ID})
	wantNoErr(t, "第一次取消", err)
	st.log.reset()
	_, err = NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: pinned.ID})
	wantErrIs(t, "重复取消", err, model.ErrPinNotFound)
	wantOps(t, "重复取消不触碰缓存", st.log.ops, []string{
		"feed_pin.Del:mid=101:fid=" + itoa(pinned.ID),
	})
}

// TestUnpinFeedDoesNotCheckFeedExistence UnpinFeed 不读主表：
// 即使动态已被删除、甚至 ID 属于别人，取消置顶只看 feed_pin 的 (mid, feed_id)。
// 这是正确的（清理入口不该被主表状态卡住），但它同时说明
// 孤儿置顶行（缺陷 D19）只能靠 UnpinFeed 清掉，DeleteFeed 自己不清理（缺陷 D17）。
func TestUnpinFeedDoesNotCheckFeedExistence(t *testing.T) {
	st := newStore()
	author := int64(101)
	other := int64(202)
	victim := st.outbox.seed(other, 5001, 1_000) // 动态其实属于别人
	st.cache.seedPin(author, victim.ID)
	st.pin.seed(author, victim.ID)

	st.log.reset()
	_, err := NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: victim.ID})
	wantNoErr(t, "取消不属于自己的动态的置顶也成功", err)
	wantOps(t, "零次主表读取", st.log.ops, []string{
		"feed_pin.Del:mid=101:fid=" + itoa(victim.ID),
		"cache.RemPin:mid=101:fid=" + itoa(victim.ID),
	})
}

// TestUnpinFeedCacheFailureMakesRetryImpossible DB 已删、SREM 失败 → 返回错误。
// 后果（缺陷 D20）：调用方重试时 pinMd.Del 已经是 0 行，直接 ErrPinNotFound，
// 重试路径无法再把缓存修回来，feed:pin:{mid} 里永久留一个已取消的成员。
func TestUnpinFeedCacheFailureMakesRetryImpossible(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	svcCtx := st.svcCtx()
	if _, err := NewPinFeedLogic(context.Background(), svcCtx).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: row.ID}); err != nil {
		t.Fatalf("布防置顶失败：%v", err)
	}
	st.fault().failWith("cache.RemPin", errCacheDown)

	st.log.reset()
	_, err := NewUnpinFeedLogic(context.Background(), svcCtx).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: row.ID})
	wantErrIs(t, "缓存取消失败要报错", err, errCacheDown)
	state, _ := st.pin.stateOf(author, row.ID)
	wantEQ(t, "DB 已取消", "state", state, model.PinStateDeleted)
	wantInt64sEQ(t, "缓存仍是旧成员", "pins", st.cache.pinMembers(author), []int64{row.ID})

	// 重试：无论缓存是否恢复，都拿不到「修复」的机会。
	st.fault().failWith("cache.RemPin", nil)
	st.log.reset()
	_, err = NewUnpinFeedLogic(context.Background(), svcCtx).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: row.ID})
	wantErrIs(t, "重试变成永久 not found", err, model.ErrPinNotFound)
	wantOps(t, "重试卡在 DB 一步", st.log.ops, []string{
		"feed_pin.Del:mid=101:fid=" + itoa(row.ID),
	})
	wantInt64sEQ(t, "脏成员留在集合里", "pins", st.cache.pinMembers(author), []int64{row.ID})
}

// TestPinnedSetHasNoReadPath 域结论（缺陷 D18）：置顶只对 feed_pin / feed:pin 生效，
// 8 个 rpc 里没有任何一条读路径会用到它——PullFeed 与 ListUserFeed 的调用序列里
// 连 cache.ListPins 都不出现，返回顺序仍是纯 ctime 倒序，FeedItem 里也没有置顶标记。
func TestPinnedSetHasNoReadPath(t *testing.T) {
	st := newStore()
	author := int64(101)
	follower := int64(7001)
	newest := st.outbox.seed(author, 5001, 1_000)
	oldest := st.outbox.seed(author, 5002, 500)
	st.cache.seedZSet(inboxKey(follower), newest.ID, newest.Ctime)
	st.cache.seedZSet(inboxKey(follower), oldest.ID, oldest.Ctime)
	st.cache.seedZSet(outboxKey(author), newest.ID, newest.Ctime)
	st.cache.seedZSet(outboxKey(author), oldest.ID, oldest.Ctime)
	// 把「更老」的那条置顶：若读侧应用了置顶，它必须排到最前。
	if _, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: oldest.ID}); err != nil {
		t.Fatalf("置顶失败：%v", err)
	}
	wantInt64sEQ(t, "布防：置顶已存在", "pins", st.cache.pinMembers(author), []int64{oldest.ID})

	st.log.reset()
	pull, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 20})
	wantNoErr(t, "关注流读取", err)
	wantCountIn(t, "关注流不读置顶集合", st.log.ops, "cache.ListPins", 0)
	wantInt64sEQ(t, "顺序仍是 ctime 倒序（置顶无效）", "items", feedIDs(pull.Items),
		[]int64{newest.ID, oldest.ID})

	st.log.reset()
	home, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: author, Ps: 20})
	wantNoErr(t, "主页读取", err)
	wantCountIn(t, "主页不读置顶集合", st.log.ops, "cache.ListPins", 0)
	wantInt64sEQ(t, "顺序仍是 ctime 倒序（置顶无效）", "items", feedIDs(home.Items),
		[]int64{newest.ID, oldest.ID})

	// 对照组：取消置顶后逐条重读，输出必须与置顶时一模一样——
	// 若将来读侧应用了置顶，这两组序列必然不同，用例即红。
	if _, err := NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: oldest.ID}); err != nil {
		t.Fatalf("取消置顶失败：%v", err)
	}
	afterPull, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 20})
	wantNoErr(t, "取消置顶后重读关注流", err)
	afterHome, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: author, Ps: 20})
	wantNoErr(t, "取消置顶后重读主页", err)
	wantInt64sEQ(t, "置顶与否对 PullFeed 输出无差异", "items",
		feedIDs(afterPull.Items), feedIDs(pull.Items))
	wantInt64sEQ(t, "置顶与否对 ListUserFeed 输出无差异", "items",
		feedIDs(afterHome.Items), feedIDs(home.Items))
}
