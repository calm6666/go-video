package repository

// 本文件测 social-graph 适配器（socialgraph_client.go）的**映射、分页、切片与错误外传**。
// 替身内嵌 socialgraph.SocialGraphClient 接口：适配器一旦调用到没建模的方法，
// panic 直接报在方法名上，等价「适配器偷偷依赖了没被测的 RPC」。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"google.golang.org/grpc"

	socialgraph "go-video/services/social-graph/rpc"
)

// 编译期钉子：适配器必须满足 repository.SocialGraphClient（生产注入的那条缝）。
var _ SocialGraphClient = (*socialGraphClient)(nil)

type sgCall struct {
	method string
	mid    int64
	owner  int64
	pn     int32
	ps     int32
	owners []int64
	mids   []int64
}

type sgFake struct {
	socialgraph.SocialGraphClient
	calls []sgCall

	following     bool
	followingErr  error
	followedSet   map[int64]bool
	batchErr      error
	followingPage []*socialgraph.FollowingReply
	blackPage     []*socialgraph.BlacksReply
	stat          *socialgraph.StatReply
	statErr       error
	richAttrs     map[int64]int32
	richErr       error
}

func (f *sgFake) rec(c sgCall) { f.calls = append(f.calls, c) }

func (f *sgFake) IsFollowing(_ context.Context, in *socialgraph.RelationReq, _ ...grpc.CallOption) (*socialgraph.RelationReply, error) {
	f.rec(sgCall{method: "IsFollowing", mid: in.GetMid(), owner: in.GetOwner()})
	if f.followingErr != nil {
		return nil, f.followingErr
	}
	return &socialgraph.RelationReply{Following: f.following}, nil
}

func (f *sgFake) IsFollowedBatch(_ context.Context, in *socialgraph.RelationsReq, _ ...grpc.CallOption) (*socialgraph.RelationsReply, error) {
	f.rec(sgCall{method: "IsFollowedBatch", mid: in.GetMid(), owners: append([]int64(nil), in.GetOwners()...)})
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make(map[int64]bool, len(in.GetOwners()))
	for _, o := range in.GetOwners() {
		out[o] = f.followedSet[o]
	}
	return &socialgraph.RelationsReply{Following: out}, nil
}

func (f *sgFake) ListFollowing(_ context.Context, in *socialgraph.ListReq, _ ...grpc.CallOption) (*socialgraph.FollowingReply, error) {
	f.rec(sgCall{method: "ListFollowing", mid: in.GetMid(), pn: in.GetPn(), ps: in.GetPs()})
	if in.GetPs() > 50 {
		return nil, errors.New("ps too large")
	}
	idx := int(in.GetPn()) - 1
	if idx < 0 || idx >= len(f.followingPage) {
		return nil, fmt.Errorf("替身没有第 %d 页", in.GetPn())
	}
	return f.followingPage[idx], nil
}

func (f *sgFake) ListBlacks(_ context.Context, in *socialgraph.ListReq, _ ...grpc.CallOption) (*socialgraph.BlacksReply, error) {
	f.rec(sgCall{method: "ListBlacks", mid: in.GetMid(), pn: in.GetPn(), ps: in.GetPs()})
	idx := int(in.GetPn()) - 1
	if idx < 0 || idx >= len(f.blackPage) {
		return nil, fmt.Errorf("替身没有第 %d 页", in.GetPn())
	}
	return f.blackPage[idx], nil
}

func (f *sgFake) Stat(_ context.Context, in *socialgraph.MidReq, _ ...grpc.CallOption) (*socialgraph.StatReply, error) {
	f.rec(sgCall{method: "Stat", mid: in.GetMid()})
	if f.statErr != nil {
		return nil, f.statErr
	}
	return f.stat, nil
}

// RichRelations 替身复刻下游口径：请求里的每个 mid 都会得到一个键，
// 无关系的是显式 0（social-graph repository.RichRelations 预填 false/0 后只把命中位或上去）。
func (f *sgFake) RichRelations(_ context.Context, in *socialgraph.RichRelationsReq, _ ...grpc.CallOption) (*socialgraph.RichRelationsReply, error) {
	f.rec(sgCall{method: "RichRelations", owner: in.GetOwner(), mids: append([]int64(nil), in.GetMids()...)})
	if f.richErr != nil {
		return nil, f.richErr
	}
	out := make(map[int64]int32, len(in.GetMids()))
	for _, m := range in.GetMids() {
		out[m] = f.richAttrs[m]
	}
	return &socialgraph.RichRelationsReply{Attrs: out}, nil
}

func sgItems(from, to int64) []*socialgraph.RelationItem {
	items := make([]*socialgraph.RelationItem, 0, to-from)
	for id := from; id < to; id++ {
		items = append(items, &socialgraph.RelationItem{Mid: id})
	}
	return items
}

func sgPageCount(t *testing.T, calls []sgCall, method string) int {
	t.Helper()
	n := 0
	for _, c := range calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func TestSGRelationMapsIsFollowingAndPropagatesError(t *testing.T) {
	f := &sgFake{following: true}
	got, err := newSocialGraphClient(f).Relation(context.Background(), 11, 22)
	if err != nil || !got {
		t.Fatalf("映射错：got=%v err=%v", got, err)
	}
	if len(f.calls) != 1 || f.calls[0].method != "IsFollowing" ||
		f.calls[0].mid != 11 || f.calls[0].owner != 22 {
		t.Fatalf("请求字段没按契约传： %+v", f.calls)
	}

	wantErr := errors.New("social-graph down")
	f2 := &sgFake{followingErr: wantErr}
	got2, err2 := newSocialGraphClient(f2).Relation(context.Background(), 11, 22)
	if !errors.Is(err2, wantErr) {
		t.Fatalf("下游错误必须原样外传而不是降级成 false：err=%v", err2)
	}
	if got2 {
		t.Fatalf("出错时不得回 true：%v", got2)
	}
}

func TestSGRelationsChunksOverHundredOwners(t *testing.T) {
	owners := make([]int64, 0, 230)
	for i := int64(1); i <= 230; i++ {
		owners = append(owners, i)
	}
	f := &sgFake{followedSet: map[int64]bool{5: true, 105: true, 230: true}}
	got, err := newSocialGraphClient(f).Relations(context.Background(), 9, owners)
	if err != nil {
		t.Fatal(err)
	}
	// 230 个 owner → 100/100/30 三片；上游对 >100 直接 ErrTooManyOwners，不切片就是必失败。
	if len(f.calls) != 3 {
		t.Fatalf("应切成 3 次批量调用：%+v", f.calls)
	}
	for i, c := range f.calls {
		want := 100
		if i == 2 {
			want = 30
		}
		if len(c.owners) != want {
			t.Fatalf("第 %d 片大小=%d 期望 %d", i, len(c.owners), want)
		}
	}
	// 未出现在下游 map 里的 owner 必须是「未关注」而不是「缺键」——调用方按 map[owner] 直读。
	if len(got) != 230 {
		t.Fatalf("返回 map 必须覆盖全部 owner：%d", len(got))
	}
	for _, id := range []int64{5, 105, 230} {
		if !got[id] {
			t.Fatalf("%d 应判为已关注", id)
		}
	}
	if got[6] {
		t.Fatalf("6 不该被关注却被点亮")
	}

	empty, err := newSocialGraphClient(&sgFake{}).Relations(context.Background(), 9, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空 owners 不应打下游也不应报错：res=%v err=%v", empty, err)
	}
}

func TestSGRelationsFailsWholeBatchOnOneChunk(t *testing.T) {
	owners := make([]int64, 0, 150)
	for i := int64(1); i <= 150; i++ {
		owners = append(owners, i)
	}
	wantErr := errors.New("second chunk down")
	f := &sgFake{followedSet: map[int64]bool{1: true}, batchErr: wantErr}
	// 两片都撞同一个替身错误：任一片失败必须整体报错，不能回半张表。
	got, err := newSocialGraphClient(f).Relations(context.Background(), 9, owners)
	if !errors.Is(err, wantErr) {
		t.Fatalf("半张关系表比报错更危险，必须整体失败：err=%v", err)
	}
	if got != nil {
		t.Fatalf("失败时不得返回部分结果：%v", got)
	}
}

func TestSGAttentionsPagesUntilTotalAndStopsOnEmptyPage(t *testing.T) {
	f := &sgFake{followingPage: []*socialgraph.FollowingReply{
		{Total: 120, Items: sgItems(1, 51)},
		{Total: 120, Items: sgItems(51, 101)},
		{Total: 120, Items: sgItems(101, 121)},
	}}
	got, err := newSocialGraphClient(f).Attentions(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 120 {
		t.Fatalf("必须拉完 120 条：%d", len(got))
	}
	if got[0] != 1 || got[119] != 120 {
		t.Fatalf("顺序丢了（下游按 ctime DESC）：%v..%v", got[0], got[119])
	}
	// 分页参数必须是 pn=1..3、ps=50（ps>50 上游直接拒答）。
	for i, c := range f.calls {
		if c.method != "ListFollowing" || c.pn != int32(i+1) || c.ps != sgPageSizeMax || c.mid != 7 {
			t.Fatalf("第 %d 页参数不对： %+v", i, c)
		}
	}

	// total 谎报时以空页为止，不能无限翻页。
	f2 := &sgFake{followingPage: []*socialgraph.FollowingReply{
		{Total: 500, Items: sgItems(1, 51)},
		{Total: 500, Items: sgItems(51, 101)},
		{Total: 500, Items: nil},
	}}
	got2, err := newSocialGraphClient(f2).Attentions(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 100 || sgPageCount(t, f2.calls, "ListFollowing") != 3 {
		t.Fatalf("空页必须终止翻页：items=%d calls=%d", len(got2), sgPageCount(t, f2.calls, "ListFollowing"))
	}
}

func TestSGAttentionsStopsAtPageCapInsteadOfUnboundedFanout(t *testing.T) {
	pages := make([]*socialgraph.FollowingReply, 0, sgListPageCap+2)
	for pn := 0; pn < sgListPageCap+2; pn++ {
		pages = append(pages, &socialgraph.FollowingReply{
			Total: 1 << 20, Items: sgItems(int64(pn*50)+1, int64(pn*50)+51)})
	}
	f := &sgFake{followingPage: pages}
	_, err := newSocialGraphClient(f).Attentions(context.Background(), 7)
	if err == nil {
		t.Fatal("超过分页上限必须报错，不能把一次账号聚合变成无界扇出")
	}
	if got := sgPageCount(t, f.calls, "ListFollowing"); got != sgListPageCap {
		t.Fatalf("翻到上限即停：calls=%d want=%d", got, sgListPageCap)
	}
}

func TestSGBlacksReturnsSet(t *testing.T) {
	f := &sgFake{blackPage: []*socialgraph.BlacksReply{
		{Total: 3, Items: sgItems(100, 103)},
	}}
	got, err := newSocialGraphClient(f).Blacks(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[100] || !got[101] || !got[102] || got[103] {
		t.Fatalf("黑名单 set 不对：%v", got)
	}
	if sgPageCount(t, f.calls, "ListBlacks") != 1 {
		t.Fatalf("total 已满足就不该再翻页：%+v", f.calls)
	}
}

func TestSGStatMapsBothCounters(t *testing.T) {
	f := &sgFake{stat: &socialgraph.StatReply{Following: 41, Follower: 777, Whisper: 0}}
	st, err := newSocialGraphClient(f).Stat(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if st.Following != 41 || st.Follower != 777 {
		t.Fatalf("计数映射错：%+v", st)
	}

	if _, err := newSocialGraphClient(&sgFake{statErr: errors.New("redis down")}).Stat(context.Background(), 7); err == nil {
		t.Fatal("Stat 出错必须外传，交由调用方决定降级")
	}
}

// TestSGRichRelationsPassesAttrsThroughUnchanged 钉住适配器的「透传」口径：
// 位掩码逐键原样搬运，含下游显式给出的 0（未关系）——把 0 当「无键」丢掉，
// 调用方就会拿到一张缺键的表并按 attr 缺失误判。
func TestSGRichRelationsPassesAttrsThroughUnchanged(t *testing.T) {
	f := &sgFake{richAttrs: map[int64]int32{
		11: int32(socialgraph.RelationAttr_RELATION_ATTR_FOLLOWING),
		12: int32(socialgraph.RelationAttr_RELATION_ATTR_MUTUAL),
		13: int32(socialgraph.RelationAttr_RELATION_ATTR_SPECIAL),
		// 14 与 15 下游回 0：无关系是显式事实，不是缺键。
	}}
	got, err := newSocialGraphClient(f).RichRelations(context.Background(), 7, []int64{11, 12, 13, 14, 15})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].method != "RichRelations" {
		t.Fatalf("5 个 mid 应在 100 以内，只发一次批量调用：%+v", f.calls)
	}
	if f.calls[0].owner != 7 {
		t.Fatalf("owner 没按契约传：%+v", f.calls[0])
	}
	if !slices.Equal(f.calls[0].mids, []int64{11, 12, 13, 14, 15}) {
		t.Fatalf("mids 原样透传（顺序与内容都不该被适配器改动）：%v", f.calls[0].mids)
	}
	wantAttrs := map[int64]int32{11: 1, 12: 3, 13: 8, 14: 0, 15: 0}
	if len(got) != len(wantAttrs) {
		t.Fatalf("返回 map 必须覆盖全部 mid：%v", got)
	}
	for mid, want := range wantAttrs {
		if got[mid] != want {
			t.Fatalf("mid=%d attr=%d 期望 %d（掩码位不得被适配器重编）", mid, got[mid], want)
		}
	}
}

func TestSGRichRelationsChunksOverHundredMids(t *testing.T) {
	mids := make([]int64, 0, 230)
	richAttrs := make(map[int64]int32, 230)
	for i := int64(1); i <= 230; i++ {
		mids = append(mids, i)
		richAttrs[i] = 0
	}
	richAttrs[5] = 1
	richAttrs[105] = 2
	richAttrs[230] = 12 // FOLLOWING|BLACKED|SPECIAL 的合法组合位
	f := &sgFake{richAttrs: richAttrs}
	got, err := newSocialGraphClient(f).RichRelations(context.Background(), 9, mids)
	if err != nil {
		t.Fatal(err)
	}
	// 230 个 mid → 100/100/30 三片；上游对 >100 直接 ErrTooManyMids，不切片就是必失败。
	if len(f.calls) != 3 {
		t.Fatalf("应切成 3 次批量调用：%+v", f.calls)
	}
	cursor := 0
	for i, c := range f.calls {
		want := 100
		if i == 2 {
			want = 30
		}
		if len(c.mids) != want {
			t.Fatalf("第 %d 片大小=%d 期望 %d", i, len(c.mids), want)
		}
		if !slices.Equal(c.mids, mids[cursor:cursor+want]) {
			t.Fatalf("第 %d 片不是连续切片：%v", i, c.mids)
		}
		cursor += want
	}
	if len(got) != 230 {
		t.Fatalf("合并后必须覆盖全部 mid：%d", len(got))
	}
	for mid, want := range map[int64]int32{5: 1, 105: 2, 230: 12} {
		if got[mid] != want {
			t.Fatalf("跨片合并丢了 mid=%d 的位：%d 期望 %d", mid, got[mid], want)
		}
	}
	if got[6] != 0 {
		t.Fatalf("6 无关系却拿到 %d", got[6])
	}
}

func TestSGRichRelationsFailsWholeBatchOnOneChunk(t *testing.T) {
	mids := make([]int64, 0, 150)
	for i := int64(1); i <= 150; i++ {
		mids = append(mids, i)
	}
	wantErr := errors.New("second chunk down")
	f := &sgFake{richAttrs: map[int64]int32{1: 1}, richErr: wantErr}
	// 两片都撞同一个替身错误：任一片失败必须整体报错，不能回半张掩码表。
	got, err := newSocialGraphClient(f).RichRelations(context.Background(), 9, mids)
	if !errors.Is(err, wantErr) {
		t.Fatalf("半张关系表比报错更危险，必须整体失败：err=%v", err)
	}
	if got != nil {
		t.Fatalf("失败时不得返回部分结果：%v", got)
	}
}

func TestSGRichRelationsEmptyMidsMakesNoCall(t *testing.T) {
	f := &sgFake{richAttrs: map[int64]int32{1: 1}}
	got, err := newSocialGraphClient(f).RichRelations(context.Background(), 9, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("空 mids 应回空 map：%v", got)
	}
	if len(f.calls) != 0 {
		t.Fatalf("空 mids 不该打下游：%+v", f.calls)
	}
}
