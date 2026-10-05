package logic

// groups_test.go 覆盖 UpGroups（全量分组，整表 JSON 缓存）与 UpGroupMids（分组成员分页）。
//
// 钉住的结论：
//   - 两条路径都是「缓存命中就不回库、回源后按代码选的 TTL 回填」（分组 300s、成员 60s）；
//   - 缓存里的脏 payload **不会**静默降级回库：错误带 `unmarshal cache` 归属并原样上抛，
//     一次 SQL 都不发（否则脏数据能永久掩盖 DB 侧的真实形态）；
//   - 空表/空页也要被缓下来（防击穿），且「空」在应答里是 `[]` 而不是 nil；
//   - total 与 mids 是两件事：越界页返回空 mids 但保留真实 total，调用方据此判断还有多少页；
//   - 分页键 (group_id,pn,ps) 各自独立缓存，翻页不会互相命中；
//   - 缺陷 D5：本服务不写 up_special，分组成员改表后最长 60s 才可见（没有任何失效入口）。
//
// 缺陷登记见 fakes_test.go 文件头（本文件复现 D5、D6）。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go-video/services/creator/model"
	"go-video/services/creator/rpc"
)

const (
	gid        = int64(7)
	gidKeyPage = "up:gm:7:1:1000" // fakeKeyGroupMem(gid, 1, 1000)
	groupsKey  = "up:groups"
)

// --- UpGroups ---

func TestUpGroupsMissThenBackfillThenHit(t *testing.T) {
	st := newStore()
	st.seedGroup(&model.UpGroup{ID: 1, Name: "高能联盟", Tag: "high", ShortTag: "高",
		FontColor: "#ffffff", BgColor: "#000000", Note: "运营手工组"})

	sc := st.svcCtx()
	got, err := NewUpGroupsLogic(context.Background(), sc).UpGroups(&rpc.NoArgReq{})
	wantNoErr(t, "首读", err)
	wantEQ(t, "分组数", "map 大小", len(got.GetUpGroups()), 1)

	g1 := got.GetUpGroups()[1]
	if g1 == nil {
		t.Fatalf("id=1 的分组是 nil，调用方取字段会 panic")
	}
	// 投影逐字段核对：漏一个字段前端就是空白徽章，而缓存回填会把这个错误形态固化 300s。
	wantEQ(t, "id", "ID", g1.GetId(), int64(1))
	wantEQ(t, "name", "Name", g1.GetName(), "高能联盟")
	wantEQ(t, "tag", "Tag", g1.GetTag(), "high")
	wantEQ(t, "short_tag", "ShortTag", g1.GetShortTag(), "高")
	wantEQ(t, "font_color", "FontColor", g1.GetFontColor(), "#ffffff")
	wantEQ(t, "bg_color", "BgColor", g1.GetBgColor(), "#000000")
	wantEQ(t, "note", "Note", g1.GetNote(), "运营手工组")

	wantSeq(t, "miss→全表扫→回填", st.log, 0,
		"cache.GetGroups:"+groupsKey,
		"up_group.All",
		"cache.SetGroups:"+groupsKey)
	wantEQ(t, "回填 TTL 是代码选的 300s", "ttl", st.cacheTTL(t, groupsKey), fakeTTLGroups)
	// 缓存里躺的是 map[int64]*model.UpGroup 的 JSON（model 没有 json tag，字段名即 Go 名）。
	if raw := st.cacheRaw(t, groupsKey); !strings.Contains(raw, `"ID":1`) || !strings.Contains(raw, `"Name":"高能联盟"`) {
		t.Errorf("回填的 payload 形态走样：%s", raw)
	}

	before := st.log.snapshot()
	again, err := NewUpGroupsLogic(context.Background(), sc).UpGroups(&rpc.NoArgReq{})
	wantNoErr(t, "二读", err)
	wantEQ(t, "二读与首读同形", "应答", again.String(), got.String())
	wantSeq(t, "二读只碰缓存", st.log, before, "cache.GetGroups:"+groupsKey)
}

func TestUpGroupsEmptyTableIsCachedToo(t *testing.T) {
	st := newStore()

	got, err := NewUpGroupsLogic(context.Background(), st.svcCtx()).UpGroups(&rpc.NoArgReq{})
	wantNoErr(t, "空表", err)
	if got == nil {
		t.Fatalf("应答 = nil, want 非 nil 空应答")
	}
	// 空表时 out 是 make 出来的空 map（upgroupslogic.go:34）：应答里是 {} 而不是 null，
	// 调用方 range 前不必判 nil。
	if out := got.GetUpGroups(); out == nil {
		t.Errorf("up_groups = nil, want 空 map")
	} else {
		wantEQ(t, "分组数", "map 大小", len(out), 0)
	}
	wantSeq(t, "空表也回填", st.log, 0,
		"cache.GetGroups:"+groupsKey,
		"up_group.All",
		"cache.SetGroups:"+groupsKey)
	wantEQ(t, "空对象形态", "raw", st.cacheRaw(t, groupsKey), "{}")

	before := st.log.snapshot()
	if _, err := NewUpGroupsLogic(context.Background(), st.svcCtx()).
		UpGroups(&rpc.NoArgReq{}); err != nil {
		t.Fatalf("二读：%v", err)
	}
	// `{}` 对 GetGroups 来说是**命中**（cache.go:197-206 只对 redis.Nil 返回 miss），
	// 所以空表不会被每次请求都全表扫一遍。
	wantSeq(t, "空标记即命中", st.log, before, "cache.GetGroups:"+groupsKey)
}

// TestUpGroupsNoInvalidationPath pin 缺陷 D5 的分组列表侧：本服务没有任何写 up_group 的 RPC，
// 缓存里那份快照在 TTL（300s）内就是唯一事实；「运营面加了新组」用静默 seed 表示。
func TestUpGroupsStaleSnapshotSurvivesUntilTTL(t *testing.T) {
	st := newStore()
	st.seedGroup(&model.UpGroup{ID: 1, Name: "高能联盟"})
	sc := st.svcCtx()

	first, err := NewUpGroupsLogic(context.Background(), sc).UpGroups(&rpc.NoArgReq{})
	wantNoErr(t, "首读", err)
	wantEQ(t, "首读分组数", "map 大小", len(first.GetUpGroups()), 1)

	st.seedGroup(&model.UpGroup{ID: 2, Name: "知名UP"}) // 库里现在有两个组
	before := st.log.snapshot()
	second, err := NewUpGroupsLogic(context.Background(), sc).UpGroups(&rpc.NoArgReq{})
	wantNoErr(t, "二读", err)
	wantEQ(t, "pin 当前行为：二读仍是旧快照", "map 大小", len(second.GetUpGroups()), 1)
	wantSeq(t, "旧快照不回库", st.log, before, "cache.GetGroups:"+groupsKey)
	wantEQ(t, "滞后上限就是 TTL", "ttl", st.cacheTTL(t, groupsKey), fakeTTLGroups)
	wantEQ(t, "库侧真实行数（对照）", "up_group 行数", len(st.group.rows), 2)
}

func TestUpGroupsCorruptCacheFailsLoud(t *testing.T) {
	for _, raw := range []string{"not-json", `["a"]`, `{"1":{"ID":`} {
		t.Run(raw, func(t *testing.T) {
			st := newStore()
			st.seedGroup(&model.UpGroup{ID: 1, Name: "高能联盟"})
			st.warmGroups(raw)

			got, err := NewUpGroupsLogic(context.Background(), st.svcCtx()).UpGroups(&rpc.NoArgReq{})
			// 错误带 `unmarshal cache` 归属：一眼能分辨「缓存脏了」和「库挂了」；
			// errors.Unwrap 非 nil → 证明是 `%w` 包装（repository.go:139），根因没被改写掉。
			wantErrContains(t, "脏 payload 必须报错", err, "UpGroups unmarshal cache")
			if errors.Unwrap(err) == nil {
				t.Errorf("根因错误丢了（未用 %%w 包装）：%v", err)
			}
			if got != nil {
				t.Errorf("应答 = %#v, want nil", got)
			}
			// 不降级回库：否则这条脏 key 永远没人发现。
			wantSeq(t, "轨迹止于缓存", st.log, 0, "cache.GetGroups:"+groupsKey)
		})
	}
}

func TestUpGroupsCacheFailureIsNotDowngraded(t *testing.T) {
	st := newStore()
	st.seedGroup(&model.UpGroup{ID: 1, Name: "高能联盟"})
	st.cache.failWith("GetGroups", errFakeRedis)

	got, err := NewUpGroupsLogic(context.Background(), st.svcCtx()).UpGroups(&rpc.NoArgReq{})
	wantErrIs(t, "缓存故障原样透传", err, errFakeRedis)
	if got != nil {
		t.Errorf("应答 = %#v, want nil（不能降级成全量分组）", got)
	}
	wantSeq(t, "缓存故障时不回库", st.log, 0, "cache.GetGroups:"+groupsKey)
}

func TestUpGroupsDBFailureWrapped(t *testing.T) {
	st := newStore()
	st.group.failWith("All", errFakeDB)

	got, err := NewUpGroupsLogic(context.Background(), st.svcCtx()).UpGroups(&rpc.NoArgReq{})
	wantErrIs(t, "DB 失败透传", err, errFakeDB)
	wantErrContains(t, "错误归属", err, "UpGroups All")
	if got != nil {
		t.Errorf("应答 = %#v, want nil", got)
	}
	// 故障不能进缓存，否则一次抖动被钉成 300s 的空分组列表。
	wantSeq(t, "失败后不得回填", st.log, 0,
		"cache.GetGroups:"+groupsKey,
		"up_group.All")
	if st.cacheHas(groupsKey) {
		t.Errorf("DB 失败却写了缓存")
	}
}

func TestUpGroupsBackfillFailureStillAnswers(t *testing.T) {
	st := newStore()
	st.seedGroup(&model.UpGroup{ID: 1, Name: "高能联盟"})
	st.cache.failWith("SetGroups", errFakeRedis)

	got, err := NewUpGroupsLogic(context.Background(), st.svcCtx()).UpGroups(&rpc.NoArgReq{})
	wantNoErr(t, "回填失败被忽略（repository.go:151 是 `_ =`）", err)
	wantEQ(t, "应答仍来自 DB", "map 大小", len(got.GetUpGroups()), 1)
	if st.cacheHas(groupsKey) {
		t.Errorf("SetGroups 失败了却留下值，会让后续读走半成品")
	}
}

// --- UpGroupMids ---

func TestUpGroupMidsGuardsRejectBeforeAnyDependency(t *testing.T) {
	for _, bad := range []int64{0, -3} {
		t.Run(fakeKeyGroupMem(bad, 1, 1000), func(t *testing.T) {
			st := newStore()
			st.seedSpecial(11, gid) // 分组 7 里确实有成员：非法 gid 也不能被「查到了」带过去
			before := st.log.snapshot()

			got, err := NewUpGroupMidsLogic(context.Background(), st.svcCtx()).
				UpGroupMids(&rpc.UpGroupMidsReq{GroupId: bad, Pn: 1, Ps: 1000})
			wantErrIs(t, "非法 group_id", err, errInvalidGroupID)
			if got != nil {
				t.Errorf("应答 = %#v, want nil", got)
			}
			wantNoCallAfter(t, "非法 group_id", st.log, before)
			wantEQ(t, "库存未被牵连", "up_special 行数", len(st.special.rows), 1)
		})
	}
}

// TestUpGroupMidsNormalizesPageInPlace 同时钉两件事：
//  1. 归一化后的 pn/ps 才是真正生效的页（缓存键与 SQL 参数都用它）；
//  2. 缺陷 D6：归一化结果被**写回入参消息**，调用方复用同一个 request 对象时页码会被悄悄改。
func TestUpGroupMidsNormalizesPageInPlace(t *testing.T) {
	cases := []struct {
		name     string
		pn, ps   int32
		wantPn   int32
		wantPs   int32
		wantKey  string
		wantMids int // 归一化后的页是否落在有数据的那一页（1 个成员）
	}{
		{name: "pn=0 → 1", pn: 0, ps: 20, wantPn: 1, wantPs: 20, wantKey: "up:gm:7:1:20", wantMids: 1},
		{name: "pn 负数 → 1", pn: -5, ps: 20, wantPn: 1, wantPs: 20, wantKey: "up:gm:7:1:20", wantMids: 1},
		// pn=2 配 ps=1000 就越界了：这里要的是「页参数原样生效」，成员数正好为 0 反证它没被改成第 1 页。
		{name: "ps=0 → 1000", pn: 2, ps: 0, wantPn: 2, wantPs: 1000, wantKey: "up:gm:7:2:1000", wantMids: 0},
		{name: "ps 负数 → 1000", pn: 1, ps: -1, wantPn: 1, wantPs: 1000, wantKey: "up:gm:7:1:1000", wantMids: 1},
		{name: "ps=2000 → 1000（单页上限）", pn: 1, ps: 2000, wantPn: 1, wantPs: 1000, wantKey: "up:gm:7:1:1000", wantMids: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedSpecial(11, gid)

			in := &rpc.UpGroupMidsReq{GroupId: gid, Pn: tc.pn, Ps: tc.ps}
			got, err := NewUpGroupMidsLogic(context.Background(), st.svcCtx()).UpGroupMids(in)
			wantNoErr(t, t.Name(), err)
			wantEQ(t, "生效页的成员数", "mids 长度", len(got.GetMids()), tc.wantMids)
			wantEQ(t, "total 与页参数无关", "Total", got.GetTotal(), int32(1))

			// 缓存键与 SQL 参数都必须是归一化后的页，否则「同一页两份缓存」会长期不一致。
			wantSeq(t, "轨迹", st.log, 0,
				"cache.GetGroupMids:"+tc.wantKey,
				"up_special.FindMidsByGroup:7/"+strconv.Itoa(int(tc.wantPn))+"/"+strconv.Itoa(int(tc.wantPs)),
				"cache.SetGroupMids:"+tc.wantKey)
			wantEQ(t, "model 收到的 ps（logic 的 1000 优先于 model 自己的 20 兜底）",
				"ps", st.groupMem.midsCalls[0].ps, tc.wantPs)

			// D6：入参对象本身被改了。
			wantEQ(t, "pin D6：in.Pn 被就地改写", "Pn", in.Pn, tc.wantPn)
			wantEQ(t, "pin D6：in.Ps 被就地改写", "Ps", in.Ps, tc.wantPs)
		})
	}
}

func TestUpGroupMidsMissThenBackfillThenHit(t *testing.T) {
	st := newStore()
	st.seedSpecial(11, gid)
	st.seedSpecial(9, gid) // 故意乱序布：ORDER BY mid 必须把它排到前面
	st.seedSpecial(13, 99) // 别的分组，不得串进来
	sc := st.svcCtx()

	in := &rpc.UpGroupMidsReq{GroupId: gid, Pn: 1, Ps: 1000}
	got, err := NewUpGroupMidsLogic(context.Background(), sc).UpGroupMids(in)
	wantNoErr(t, "首读", err)
	wantInt64sEQ(t, "成员按 mid 升序", "mids", got.GetMids(), []int64{9, 11})
	wantEQ(t, "total", "Total", got.GetTotal(), int32(2))
	wantSeq(t, "miss→回源→回填", st.log, 0,
		"cache.GetGroupMids:"+gidKeyPage,
		"up_special.FindMidsByGroup:7/1/1000",
		"cache.SetGroupMids:"+gidKeyPage)
	wantEQ(t, "payload 是 mids+total 的对象", "raw",
		st.cacheRaw(t, gidKeyPage), `{"mids":[9,11],"total":2}`)
	wantEQ(t, "成员缓存 TTL 是 60s", "ttl", st.cacheTTL(t, gidKeyPage), fakeTTLGroupMem)

	before := st.log.snapshot()
	again, err := NewUpGroupMidsLogic(context.Background(), sc).UpGroupMids(in)
	wantNoErr(t, "二读", err)
	wantInt64sEQ(t, "二读来自缓存", "mids", again.GetMids(), []int64{9, 11})
	wantEQ(t, "二读 total 也来自缓存", "Total", again.GetTotal(), int32(2))
	wantSeq(t, "二读只碰缓存", st.log, before, "cache.GetGroupMids:"+gidKeyPage)
}

// TestUpGroupMidsPagesHaveSeparateKeys 锁「翻页不复用上一页缓存」：
// 每个 (gid,pn,ps) 一条 key，所以深翻页会各自回源一次（也是缺陷 D5 的口径来源）。
func TestUpGroupMidsPagesHaveSeparateKeys(t *testing.T) {
	st := newStore()
	for _, mid := range []int64{1, 2, 3} {
		st.seedSpecial(mid, gid)
	}
	sc := st.svcCtx()

	first, err := NewUpGroupMidsLogic(context.Background(), sc).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 1, Ps: 2})
	wantNoErr(t, "第 1 页", err)
	wantInt64sEQ(t, "第 1 页", "mids", first.GetMids(), []int64{1, 2})
	wantEQ(t, "第 1 页 total", "Total", first.GetTotal(), int32(3))

	second, err := NewUpGroupMidsLogic(context.Background(), sc).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 2, Ps: 2})
	wantNoErr(t, "第 2 页", err)
	wantInt64sEQ(t, "第 2 页", "mids", second.GetMids(), []int64{3})
	wantEQ(t, "第 2 页 total 不变", "Total", second.GetTotal(), int32(3))

	wantSeq(t, "两页各自回源", st.log, 0,
		"cache.GetGroupMids:up:gm:7:1:2",
		"up_special.FindMidsByGroup:7/1/2",
		"cache.SetGroupMids:up:gm:7:1:2",
		"cache.GetGroupMids:up:gm:7:2:2",
		"up_special.FindMidsByGroup:7/2/2",
		"cache.SetGroupMids:up:gm:7:2:2")
}

// TestUpGroupMidsEmptyPageKeepsTotalAndAnswersNonNilNil 钉越界页的两层归一化：
// model 交回 (nil, total)，repository 缓存里写 `[]`（repository.go:180-182）但**返回给 logic 的仍是 nil**，
// 由 logic 的 nil→[] 兜底（upgroupmidslogic.go:48-50）保证应答里是空数组而不是 null。
func TestUpGroupMidsEmptyPageKeepsTotalAndAnswersNonNil(t *testing.T) {
	st := newStore()
	st.seedSpecial(1, gid)
	st.seedSpecial(2, gid)
	sc := st.svcCtx()

	got, err := NewUpGroupMidsLogic(context.Background(), sc).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 9, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "空 mids 必须是 []（不是 nil）", "mids 是否非 nil", got.GetMids() != nil, true)
	wantEQ(t, "空 mids 长度", "mids 长度", len(got.GetMids()), 0)
	wantEQ(t, "越界页仍保留真实 total", "Total", got.GetTotal(), int32(2))
	wantEQ(t, "缓存里落成空数组", "raw",
		st.cacheRaw(t, "up:gm:7:9:2"), `{"mids":[],"total":2}`)

	before := st.log.snapshot()
	again, err := NewUpGroupMidsLogic(context.Background(), sc).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 9, Ps: 2})
	wantNoErr(t, "二读越界页", err)
	wantEQ(t, "缓存路径也不交出 nil", "mids 是否非 nil", again.GetMids() != nil, true)
	wantSeq(t, "空页负缓存生效", st.log, before, "cache.GetGroupMids:up:gm:7:9:2")
}

// TestUpGroupMidsUnknownGroupIsCachedAsEmpty 分组压根不存在：total=0、mids=[]，同样被缓下来。
func TestUpGroupMidsUnknownGroupIsCachedAsEmpty(t *testing.T) {
	st := newStore()
	st.seedSpecial(11, gid) // 只有分组 7 有成员

	got, err := NewUpGroupMidsLogic(context.Background(), st.svcCtx()).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: 404, Pn: 1, Ps: 10})
	wantNoErr(t, "不存在的分组", err)
	wantEQ(t, "mids 长度", "mids 长度", len(got.GetMids()), 0)
	wantEQ(t, "total", "Total", got.GetTotal(), int32(0))
	wantSeq(t, "回源一次并回填空页", st.log, 0,
		"cache.GetGroupMids:up:gm:404:1:10",
		"up_special.FindMidsByGroup:404/1/10",
		"cache.SetGroupMids:up:gm:404:1:10")
	wantEQ(t, "payload", "raw", st.cacheRaw(t, "up:gm:404:1:10"), `{"mids":[],"total":0}`)
}

// TestUpGroupMidsMemberAddedAfterSnapshotIsInvisible pin 缺陷 D5：
// 运营面把新成员塞进分组 7 之后，读侧在 60s 内仍看不到——因为本服务没有任何失效入口
// （没有写 up_special 的 RPC，cache.go 也没有 DelGroupMids）。
func TestUpGroupMidsMemberAddedAfterSnapshotIsInvisible(t *testing.T) {
	st := newStore()
	st.seedSpecial(11, gid)
	sc := st.svcCtx()

	first, err := NewUpGroupMidsLogic(context.Background(), sc).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 1, Ps: 1000})
	wantNoErr(t, "首读", err)
	wantInt64sEQ(t, "首读成员", "mids", first.GetMids(), []int64{11})

	st.seedSpecial(12, gid) // 改表：新成员 12 加入分组 7（静默，不写 callLog）
	before := st.log.snapshot()
	second, err := NewUpGroupMidsLogic(context.Background(), sc).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 1, Ps: 1000})
	wantNoErr(t, "二读", err)
	wantInt64sEQ(t, "pin 当前行为：新成员不可见", "mids", second.GetMids(), []int64{11})
	wantEQ(t, "pin：连 total 也是旧的", "Total", second.GetTotal(), int32(1))
	wantSeq(t, "旧快照不回库", st.log, before, "cache.GetGroupMids:"+gidKeyPage)
	wantEQ(t, "滞后上限就是 TTL", "ttl", st.cacheTTL(t, gidKeyPage), fakeTTLGroupMem)
	wantEQ(t, "库侧真实成员行（对照）", "up_special 行数", len(st.special.rows), 2)
}

func TestUpGroupMidsCorruptCacheFailsLoud(t *testing.T) {
	st := newStore()
	st.seedSpecial(11, gid)
	st.warmGroupMids(gid, 1, 1000, `{"mids":[11]`) // 半截 JSON

	got, err := NewUpGroupMidsLogic(context.Background(), st.svcCtx()).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 1, Ps: 1000})
	wantErrContains(t, "脏 payload 必须报错", err, "UpGroupMids unmarshal cache")
	if got != nil {
		t.Errorf("应答 = %#v, want nil", got)
	}
	wantSeq(t, "不降级回库", st.log, 0, "cache.GetGroupMids:"+gidKeyPage)
}

func TestUpGroupMidsCacheFailureIsNotDowngraded(t *testing.T) {
	st := newStore()
	st.seedSpecial(11, gid)
	st.cache.failWith("GetGroupMids", errFakeRedis)

	got, err := NewUpGroupMidsLogic(context.Background(), st.svcCtx()).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 1, Ps: 1000})
	wantErrIs(t, "缓存故障原样透传", err, errFakeRedis)
	if got != nil {
		t.Errorf("应答 = %#v, want nil", got)
	}
	wantSeq(t, "缓存故障时不回库", st.log, 0, "cache.GetGroupMids:"+gidKeyPage)
}

func TestUpGroupMidsDBFailureWrapped(t *testing.T) {
	st := newStore()
	st.groupMem.failWith("FindMidsByGroup", errFakeDB)

	got, err := NewUpGroupMidsLogic(context.Background(), st.svcCtx()).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 1, Ps: 1000})
	wantErrIs(t, "DB 失败透传", err, errFakeDB)
	wantErrContains(t, "错误归属", err, "UpGroupMids FindMids")
	if got != nil {
		t.Errorf("应答 = %#v, want nil", got)
	}
	wantSeq(t, "失败后不得回填", st.log, 0,
		"cache.GetGroupMids:"+gidKeyPage,
		"up_special.FindMidsByGroup:7/1/1000")
	if st.cacheHas(gidKeyPage) {
		t.Errorf("DB 失败却写了缓存，会把故障钉成 60s 的空分组")
	}
}

// TestUpGroupMidsOnlyTouchesItsOwnKeys 确认分组路径不越界读写别的缓存域
// （up:spec / up:sw / up:attr 都归别的 RPC，串了就是数据域污染，见 AGENTS.md §5）。
func TestUpGroupMidsOnlyTouchesItsOwnKeys(t *testing.T) {
	st := newStore()
	st.seedSpecial(11, gid)
	if _, err := NewUpGroupMidsLogic(context.Background(), st.svcCtx()).
		UpGroupMids(&rpc.UpGroupMidsReq{GroupId: gid, Pn: 1, Ps: 1000}); err != nil {
		t.Fatalf("UpGroupMids：%v", err)
	}
	for _, o := range st.log.ops {
		if strings.HasPrefix(o, "up_group.All") || strings.HasPrefix(o, "up_special.FindOne") ||
			strings.HasPrefix(o, "up_special.FindMany") || strings.HasPrefix(o, "cache.SetSpecial") ||
			strings.HasPrefix(o, "up_attr.") || strings.HasPrefix(o, "up_switch.") || strings.HasPrefix(o, "sign_up.") {
			t.Errorf("分组成员路径越界触达：%s", o)
		}
	}
	if keys := st.cache.keysSorted(); len(keys) != 1 || keys[0] != gidKeyPage {
		t.Errorf("缓存里留下的键 = %v, want 只有 [%s]", keys, gidKeyPage)
	}
}
