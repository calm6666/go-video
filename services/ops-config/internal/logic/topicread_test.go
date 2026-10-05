// 本文件钉住专题的两个读接口：GetTopic（端上按 slug 寻址、后台按 ID 操作）与
// ListTopics（后台分页 + online_only 视图）。
//
// 三条真正的风险：
//  1. 两个定位符必须落在**两个独立缓存键**上，写侧两份都要删，
//     否则「后台改了、端上还是旧的」；
//  2. 条目（排期）永远不许进缓存，只有主记录进 —— 换内容是运营最高频动作；
//  3. 下架专题与「专题不存在」必须是不同答案：前者仍回定义（端上要能显示「已下架」），
//     但 ttl=0 禁止任何缓存。
package logic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

var errTopicStoreDown = errors.New("read ops_topic: no connection")

func (e *testEnv) getTopic(t *testing.T, req *rpc.GetTopicReq) (*rpc.GetTopicReply, error) {
	t.Helper()
	return NewGetTopicLogic(bg(), e.svc).GetTopic(req)
}

func (e *testEnv) listTopics(t *testing.T, req *rpc.ListTopicsReq) (*rpc.ListTopicsReply, error) {
	t.Helper()
	return NewListTopicsLogic(bg(), e.svc).ListTopics(req)
}

func topicSlugs(items []*rpc.Topic) []string {
	out := make([]string, 0, len(items))
	for _, row := range items {
		out = append(out, row.GetSlug())
	}
	return out
}

func topicItemPositions(items []*rpc.TopicItem) []int32 {
	out := make([]int32, 0, len(items))
	for _, it := range items {
		out = append(out, it.GetPosition())
	}
	return out
}

// sequentialItems 造 n 条 position 从 1 连续的条目（model 要求专题条目不得有空洞）。
func sequentialItems(n int, state int32) []*model.TopicItem {
	out := make([]*model.TopicItem, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, &model.TopicItem{
			ItemType: model.ItemTypeUGCVideo, ItemID: fmt.Sprintf("t%03d", i),
			Position: int32(i), State: state,
		})
	}
	return out
}

func TestGetTopicRequiresALocatorBeforeTouchingStorage(t *testing.T) {
	e := newTestEnv(t)
	topic := e.seedTopic(t, &model.Topic{Slug: "tp-loc", Title: "定位符", State: model.StateOn})
	before := e.effects()

	for _, req := range []*rpc.GetTopicReq{{}, {Slug: "   "}, {TopicId: 0}, {TopicId: -1, Slug: "  "}} {
		reply, err := e.getTopic(t, req)
		wantFail(t, err, model.ErrTopicNotFound, fmt.Sprintf("无定位符 %+v", req))
		if reply != nil {
			t.Fatalf("%+v：出错还回了响应体", req)
		}
	}
	if got := e.call("Topic.FindByID"); got != 0 {
		t.Fatalf("定位条件缺失却查了库：%d 次", got)
	}
	if got := e.call("Topic.FindBySlug"); got != 0 {
		t.Fatalf("定位条件缺失却查了库：%d 次", got)
	}
	e.requireSameEffects(t, before, "无定位符零副作用")

	// 两者都给以 topic_id 为准：同一请求回两份视图总有不一致的时刻。
	// 这里用「slug 指向另一个专题」来钉：如果实现读的是 slug，就会返回别的专题。
	other := e.seedTopic(t, &model.Topic{Slug: "tp-other", Title: "另一个", State: model.StateOn})
	if other.TopicID == topic.TopicID {
		t.Fatal("种子失效：两个专题同 ID")
	}
	reply, err := e.getTopic(t, &rpc.GetTopicReq{TopicId: topic.TopicID, Slug: "tp-other"})
	got := wantOK(t, reply, err, "id 与 slug 同时给")
	if got.GetTopic().GetTopicId() != topic.TopicID || got.GetTopic().GetSlug() != "tp-loc" {
		t.Fatalf("两者都给时没以 topic_id 为准：%+v", got.GetTopic())
	}
	if e.call("Topic.FindBySlug") != 0 {
		t.Fatal("以 id 为准却仍按 slug 查了一次（多一次回源，且两份视图可能不一致）")
	}
}

func TestGetTopicMissesAreNotCachedButOfflineTopicsAreFoundWithZeroTTL(t *testing.T) {
	e := newTestEnv(t)
	e.seedTopic(t, &model.Topic{Slug: "tp-off", Title: "已撤下", State: model.StateOff})

	miss, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-nope"})
	got := wantOK(t, miss, err, "不存在的专题")
	if got.GetFound() || got.GetTopic() != nil {
		t.Fatalf("不存在却回了专题：%+v", got)
	}
	// 「不存在/过期入口」不该占 gRPC 错误码，但要有短 TTL 让入口较快自愈。
	if got.GetTtl() != itemPointerTTLSeconds {
		t.Fatalf("未命中建议 TTL 应是 %d：%d", itemPointerTTLSeconds, got.GetTtl())
	}
	if _, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-nope"}); err != nil {
		t.Fatalf("第二次读取：%v", err)
	}
	if n := e.call("Topic.FindBySlug"); n != 2 {
		t.Fatalf("未命中被缓存了（回源 %d 次，期望 2 次）", n)
	}
	for _, k := range e.cache.sets {
		t.Fatalf("未命中回填了投影键：%s", k)
	}

	// 下架专题：定义照回（端上要能显示「已下架」而不是 404），但 ttl 必须是 0。
	off, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-off"})
	offline := wantOK(t, off, err, "下架专题")
	if !offline.GetFound() || offline.GetTopic().GetState() != model.StateOff {
		t.Fatalf("下架专题应回定义 + state=2：%+v", offline.GetTopic())
	}
	if offline.GetTtl() != 0 {
		t.Fatalf("下架专题禁止缓存，实得 ttl=%d（撤下的入口被继续缓存一个 TTL，点进去就是空页）",
			offline.GetTtl())
	}
	// 口径不一致处（与 ResolveSlot 对照）：停投坑位回 found=false，下架专题回 found=true。
	// 语义上各自成立——位置可以凭空消失，专题链接却是历史入口——
	// 但两侧都必须在 README 里写清，否则调用方会按其中一侧的直觉去接另一侧。
	if e.call("Slot.FindByCode") != 0 {
		t.Fatal("本用例不该读坑位表")
	}
}

func TestGetTopicCachesOnlyTheDefinitionUnderTwoIndependentKeys(t *testing.T) {
	e := newTestEnv(t)
	topic := e.seedTopic(t, &model.Topic{Slug: "tp-keys", Title: "两个键", State: model.StateOn})
	e.seedTopicItems(t, topic.TopicID, sequentialItems(2, model.StateOn))
	idKey := e.limits.topicKey("id:" + fmt.Sprint(topic.TopicID))
	slugKey := e.limits.topicKey("slug:tp-keys")
	requireOwnKeys(t, []string{idKey, slugKey}, testKeyPrefix, "专题投影键")
	if idKey == slugKey {
		t.Fatal("两个定位符共用一个键，删一份就会漏一份")
	}

	bySlug, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-keys", WithItems: true})
	first := wantOK(t, bySlug, err, "按 slug 读")
	if len(first.GetItems()) != 2 {
		t.Fatalf("with_items 没回条目：%v", topicItemPositions(first.GetItems()))
	}
	if e.cache.data[slugKey] == "" {
		t.Fatal("slug 键没回填")
	}
	if e.cache.data[idKey] != "" {
		t.Fatal("按 slug 读却写了 id 键（键构造与定位符不一致）")
	}
	if n := e.cache.setTTLs[slugKey]; n != e.limits.topicTTL {
		t.Fatalf("投影 TTL 应是 OpsTopic.ListTTLSeconds(%d)：%d", e.limits.topicTTL, n)
	}
	// 条目绝不进缓存：缓存值里连内容 ID 的一个字节都不许出现（AGENTS.md §5）。
	raw := e.cache.data[slugKey]
	if strings.Contains(raw, "t001") {
		t.Fatalf("专题条目被写进了投影：%s", raw)
	}
	var row model.Topic
	if err := json.Unmarshal([]byte(raw), &row); err != nil || row.TopicID != topic.TopicID {
		t.Fatalf("投影值不是专题主记录：%v raw=%s", err, raw)
	}

	// 第二个定位符是独立的键：首轮仍要回源，之后各自命中。
	byID, err := e.getTopic(t, &rpc.GetTopicReq{TopicId: topic.TopicID})
	second := wantOK(t, byID, err, "按 id 读")
	if second.GetTopic().GetSlug() != "tp-keys" {
		t.Fatalf("按 id 读到的不是同一个专题：%+v", second.GetTopic())
	}
	if n := e.call("Topic.FindByID"); n != 1 {
		t.Fatalf("按 id 首轮没回源：%d", n)
	}
	if e.call("Topic.FindBySlug") != 1 {
		t.Fatalf("slug 首轮回源次数不符：%d", e.call("Topic.FindBySlug"))
	}
	reads := e.call("TopicItem.ListByTopic")
	if _, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-keys", WithItems: true}); err != nil {
		t.Fatalf("第三轮：%v", err)
	}
	if e.call("Topic.FindBySlug") != 1 {
		t.Fatal("slug 键没命中，白回源了一次")
	}
	if e.call("TopicItem.ListByTopic") != reads+1 {
		t.Fatalf("条目被缓存了（换内容会一个 TTL 不可见）")
	}
}

func TestGetTopicInvalidationIsObservableAsRealDeletes(t *testing.T) {
	e := newTestEnv(t)
	topic := e.seedTopic(t, &model.Topic{Slug: "tp-live", Title: "旧标题", State: model.StateOn, Version: 4})
	idKey := e.limits.topicKey("id:" + fmt.Sprint(topic.TopicID))
	oldKey := e.limits.topicKey("slug:tp-live")

	// 先把投影填满：让「删除」这一步真的删掉东西，而不是删空气。
	if _, err := e.getTopic(t, &rpc.GetTopicReq{TopicId: topic.TopicID}); err != nil {
		t.Fatalf("预热按 id 读：%v", err)
	}
	if _, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-live"}); err != nil {
		t.Fatalf("预热按 slug 读：%v", err)
	}
	if e.cache.data[idKey] == "" || e.cache.data[oldKey] == "" {
		t.Fatalf("预热后投影没建立：id=%q slug=%q", e.cache.data[idKey], e.cache.data[oldKey])
	}
	baseline := e.effects()

	// 改标题（不改 slug）：两份键都要删，且去重后不多删。
	if _, err := NewSaveTopicLogic(bg(), e.svc).SaveTopic(&rpc.SaveTopicReq{
		Ctx: callCtx("tp-rename-1"), TopicId: topic.TopicID, Title: "新标题",
		State: model.StateOn, ExpectVersion: 4, Reason: "改标题",
	}); err != nil {
		t.Fatalf("SaveTopic 改标题：%v", err)
	}
	requireDeletedKeys(t, e.deletedKeysSince(baseline), []string{idKey, oldKey}, "同 slug 更新删的键")
	if e.cache.data[idKey] != "" || e.cache.data[oldKey] != "" {
		t.Fatal("删了键却还读得到旧值（替身失效或键构造不一致）")
	}
	// 下一次读必须回源并看到新值：这才叫失效，只 DEL 不验证等于没验证。
	after, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-live"})
	fresh := wantOK(t, after, err, "改后按 slug 读")
	if fresh.GetTopic().GetTitle() != "新标题" {
		t.Fatalf("读到旧投影：%+v", fresh.GetTopic())
	}
	if e.call("Topic.FindBySlug") != 2 {
		t.Fatalf("改后没回源：%d 次", e.call("Topic.FindBySlug"))
	}

	// 改 slug：新旧四个键里，两份 slug 键 + 一份 id 键都必须删干净。
	// 漏删旧 slug 键 = 旧入口继续返回一个「已经不叫这个名」的专题，
	// 而这类入口往往是分享出去的链接，没人会去清缓存。
	renameBase := e.effects()
	if _, err := NewSaveTopicLogic(bg(), e.svc).SaveTopic(&rpc.SaveTopicReq{
		Ctx: callCtx("tp-rename-2"), TopicId: topic.TopicID, Slug: "tp-moved", Title: "新标题",
		State: model.StateOn, ExpectVersion: 5, Reason: "换句柄",
	}); err != nil {
		t.Fatalf("SaveTopic 改 slug：%v", err)
	}
	newKey := e.limits.topicKey("slug:tp-moved")
	requireDeletedKeys(t, e.deletedKeysSince(renameBase), []string{idKey, oldKey, newKey}, "换 slug 删的键")
	// 只读接口不写审计：判定窗口必须收在这两次 GetTopic 上（前面每次 SaveTopic 各写过一条，
	// 拿全用例的绝对值断言等于把写侧的审计记到读侧头上）。
	// 删除也只可能落在这三个键上——多删一个就是越界（AGENTS.md §5）。
	readAudit := e.effects().audits
	if moved, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-moved"}); err != nil ||
		!moved.GetFound() || moved.GetTopic().GetSlug() != "tp-moved" {
		t.Fatalf("新 slug 读不到：%+v err=%v", moved.GetTopic(), err)
	}
	if gone, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-live"}); err != nil || gone.GetFound() {
		t.Fatalf("旧 slug 还能解析出一个专题（历史入口没断干净）：%+v err=%v", gone.GetTopic(), err)
	}
	if n := e.effects().audits - readAudit; n != 0 {
		t.Fatalf("只读的 GetTopic 写了审计：%d 条", n)
	}
	requireOwnKeys(t, e.cache.dels, testKeyPrefix, "专题投影删除键")
	touched := map[string]bool{}
	for _, k := range e.cache.dels {
		touched[k] = true
	}
	if len(touched) != 3 || !touched[idKey] || !touched[oldKey] || !touched[newKey] {
		t.Fatalf("被删过的键集合不符：%v", sortedKeys(touched))
	}
}

func TestGetTopicItemLimitMatrix(t *testing.T) {
	e := newTestEnv(t)
	topic := e.seedTopic(t, &model.Topic{Slug: "tp-items", Title: "带条目", State: model.StateOn})
	e.seedTopicItems(t, topic.TopicID, sequentialItems(120, model.StateOn))

	// 不给 with_items：一条都不回，也不该去查条目表。
	bare, err := e.getTopic(t, &rpc.GetTopicReq{TopicId: topic.TopicID})
	none := wantOK(t, bare, err, "不带条目")
	if len(none.GetItems()) != 0 {
		t.Fatalf("没要条目却回了 %d 条", len(none.GetItems()))
	}
	if e.call("TopicItem.ListByTopic") != 0 {
		t.Fatal("没要条目却查了条目表")
	}

	// 缺省上限 = OpsTopic.DefaultItemLimit(100)：专题再大也只回一屏，
	// 端上要更多必须显式点名（否则一次列表页拉取能拖垮整条链路）。
	cases := []struct {
		name  string
		limit int32
		want  int
	}{
		{"不给 item_limit 用缺省", 0, 100},
		{"点名 20 条", 20, 20},
		{"点名 120 条（全量）", 120, 120},
		{"点名 9999 夹到 OpsTopic.MaxItems(500)，实际只有 120 条", 9999, 120},
		{"负数当缺省处理", -5, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.getTopic(t, &rpc.GetTopicReq{TopicId: topic.TopicID,
				WithItems: true, ItemLimit: tc.limit})
			got := wantOK(t, reply, err, tc.name)
			if len(got.GetItems()) != tc.want {
				t.Fatalf("%s：条目数不符 got=%d want=%d", tc.name, len(got.GetItems()), tc.want)
			}
			// 条目按 position 连续返回，端上直接按序渲染。
			positions := topicItemPositions(got.GetItems())
			for i, p := range positions {
				if p != int32(i+1) {
					t.Fatalf("%s：第 %d 条的 position 是 %d（顺序断了）", tc.name, i+1, p)
				}
			}
			if got.GetTopic().GetTopicId() != topic.TopicID || got.GetTtl() != 60 {
				t.Fatalf("%s：主记录或 TTL 被条目参数带偏了：%+v ttl=%d",
					tc.name, got.GetTopic(), got.GetTtl())
			}
		})
	}
}

func TestGetTopicItemLimitCannotExceedConfiguredMax(t *testing.T) {
	// 配置收紧到 30：调用方点名 50/9999 也拿不到，但缺省值不吃这条夹取。
	e := newTestEnvWith(t, func(c *config.Config) { c.OpsTopic.MaxItems = 30 })
	topic := e.seedTopic(t, &model.Topic{Slug: "tp-tight", Title: "收紧", State: model.StateOn})
	e.seedTopicItems(t, topic.TopicID, sequentialItems(120, model.StateOn))

	cases := []struct {
		name  string
		limit int32
		want  int
	}{
		// 缺省值只受硬上限（model.MaxTopicItems）约束，不受本服务 OpsTopic.MaxItems 约束：
		// gettopiclogic.go:78-84 的夹取只作用在「调用方点名的 item_limit」上。
		// 收紧 MaxItems 之后库里仍可能有更多条目（条目是收紧前写的），
		// 此时缺省读仍会回 100 条。这与坑位读路径的 capacity 夹取不同口径，
		// 本用例把它钉成事实，改动时不会被「顺手统一一下」悄悄换掉。
		{"缺省 item_limit 不被 OpsTopic.MaxItems 夹取", 0, 100},
		{"点名 10 条", 10, 10},
		{"点名 50 条夹到 MaxItems=30", 50, 30},
		{"点名 9999 夹到 MaxItems=30", 9999, 30},
	}
	for _, tc := range cases {
		reply, err := e.getTopic(t, &rpc.GetTopicReq{TopicId: topic.TopicID,
			WithItems: true, ItemLimit: tc.limit})
		got := wantOK(t, reply, err, tc.name)
		if len(got.GetItems()) != tc.want {
			t.Fatalf("%s：got=%d want=%d", tc.name, len(got.GetItems()), tc.want)
		}
	}
}

func TestGetTopicOnlyReturnsEffectiveItems(t *testing.T) {
	e := newTestEnv(t)
	topic := e.seedTopic(t, &model.Topic{Slug: "tp-state", Title: "条目状态", State: model.StateOn})
	e.seedTopicItems(t, topic.TopicID, []*model.TopicItem{
		{ItemType: model.ItemTypeUGCVideo, ItemID: "on-a", Position: 1, State: model.StateOn},
		{ItemType: model.ItemTypeUGCVideo, ItemID: "off-b", Position: 2, State: model.StateOff},
		{ItemType: model.ItemTypePGCSeason, ItemID: "on-c", Position: 3, State: model.StateOn},
	})
	reply, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-state", WithItems: true})
	got := wantOK(t, reply, err, "条目状态过滤")
	if len(got.GetItems()) != 2 {
		t.Fatalf("下架条目被回给端上了：%v", topicItemPositions(got.GetItems()))
	}
	for _, it := range got.GetItems() {
		if it.GetState() != model.StateOn {
			t.Fatalf("回了一条非生效条目：%+v", it)
		}
	}
	if got.GetItems()[0].GetItemId() != "on-a" || got.GetItems()[1].GetItemId() != "on-c" {
		t.Fatalf("条目内容不符：%+v", got.GetItems())
	}
	// 条目只回引用（item_type + item_id）：标题/封面属于内容服务，复制过来是第二份真相。
	if got.GetItems()[0].GetItemType() != model.ItemTypeUGCVideo ||
		got.GetItems()[0].GetItemId() != "on-a" {
		t.Fatalf("条目引用投影不符：%+v", got.GetItems()[0])
	}
}

func TestGetTopicCacheDamageNeverChangesTheVerdict(t *testing.T) {
	e := newTestEnv(t)
	topic := e.seedTopic(t, &model.Topic{Slug: "tp-broken", Title: "坏投影", State: model.StateOn})
	e.seedTopicItems(t, topic.TopicID, sequentialItems(3, model.StateOn))
	key := e.limits.topicKey("slug:tp-broken")

	base, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-broken", WithItems: true})
	want := wantOK(t, base, err, "基准")
	logs := captureLogs(t)

	cases := []struct {
		name   string
		setup  func()
		logSub string
	}{
		{"Redis 读故障", func() { e.cache.getErr = errTopicStoreDown }, "读 " + key + " 失败"},
		{"投影值是坏 JSON", func() { e.cache.seed(key, "{\"topic_id\":") }, "解析 " + key + " 失败"},
		{"回填失败", func() { e.cache.setErr = errTopicStoreDown }, "回填 " + key + " 失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			reads := e.call("Topic.FindBySlug")
			reply, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-broken", WithItems: true})
			got := wantOK(t, reply, err, tc.name)
			if got.GetFound() != want.GetFound() || got.GetTtl() != want.GetTtl() ||
				got.GetTopic().GetTitle() != want.GetTopic().GetTitle() ||
				len(got.GetItems()) != len(want.GetItems()) {
				t.Fatalf("%s：投影故障改变了判定：%+v ttl=%d items=%d", tc.name, got.GetTopic(),
					got.GetTtl(), len(got.GetItems()))
			}
			if !strings.Contains(logs.joined(), tc.logSub) {
				t.Fatalf("%s：故障没留下 Error 日志（缺 %q）\n%s", tc.name, tc.logSub, logs.joined())
			}
			// 这一次确实回源了：否则「判定不变」可能只是命中了坏值。
			if e.call("Topic.FindBySlug") != reads+1 {
				t.Fatalf("%s：没真的回源（%d → %d）", tc.name, reads, e.call("Topic.FindBySlug"))
			}
			e.cache.getErr, e.cache.setErr = nil, nil
			delete(e.cache.data, key)
		})
	}
}

func TestGetTopicPropagatesStorageErrorsVerbatim(t *testing.T) {
	e := newTestEnv(t)
	topic := e.seedTopic(t, &model.Topic{Slug: "tp-db", Title: "库故障", State: model.StateOn})
	e.seedTopicItems(t, topic.TopicID, sequentialItems(2, model.StateOn))

	// 主记录读失败：必须是错误，不能退成 found=false（那等于「这个专题不存在」）。
	e.db.readErrs["Topic.FindBySlug"] = errTopicStoreDown
	reply, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-db"})
	wantFail(t, err, errTopicStoreDown, "Topic.FindBySlug 故障")
	if reply != nil {
		t.Fatalf("依赖故障却回了响应体：%+v", reply)
	}
	if len(e.cache.sets) != 0 {
		t.Fatalf("读失败仍回填了投影：%v", e.cache.sets)
	}
	delete(e.db.readErrs, "Topic.FindBySlug")

	// 条目读失败：专题主记录读到了也不能降级成「空专题」。
	// 这是最坏的一类伪装：运营会以为条目被清空了，实际只是库抖了一下。
	e.db.readErrs["TopicItem.ListByTopic"] = errTopicStoreDown
	second, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-db", WithItems: true})
	wantFail(t, err, errTopicStoreDown, "TopicItem.ListByTopic 故障")
	if second != nil {
		t.Fatalf("条目读失败却回了 found=%v items=%d", second.GetFound(), len(second.GetItems()))
	}
	if errors.Is(err, model.ErrTopicNotFound) {
		t.Fatalf("依赖错误被改写成了「专题不存在」：%v", err)
	}
	delete(e.db.readErrs, "TopicItem.ListByTopic")
	ok, err := e.getTopic(t, &rpc.GetTopicReq{Slug: "tp-db", WithItems: true})
	back := wantOK(t, ok, err, "恢复后")
	if len(back.GetItems()) != 2 {
		t.Fatalf("恢复后条目数不符：%v", topicItemPositions(back.GetItems()))
	}
}

func TestListTopicsRejectsIllegalInputsBeforeTouchingStorage(t *testing.T) {
	e := newTestEnv(t)
	e.seedTopic(t, &model.Topic{Slug: "lt-one", Title: "一号", State: model.StateOn})
	before := e.effects()

	cases := []struct {
		name string
		req  *rpc.ListTopicsReq
		want error
	}{
		{"pn 为负", &rpc.ListTopicsReq{Pn: -1}, model.ErrInvalidPage},
		{"ps 为负", &rpc.ListTopicsReq{Ps: -1}, model.ErrInvalidPage},
		{"state 第四个值", &rpc.ListTopicsReq{State: 3}, model.ErrRuleStateInvalid},
		{"state 为负", &rpc.ListTopicsReq{State: -2}, model.ErrRuleStateInvalid},
		// 关键词只进 LIKE：超长多半是把整段文本粘进了搜索框，不是「查不到」。
		{"关键词超长", &rpc.ListTopicsReq{Keyword: strings.Repeat("词", 65)}, model.ErrBatchTooLarge},
		// 引用 ID 是 catalog 的主键：负数不可能是任何分区，
		// 放过去只会得到一个永远空手的筛选（而不是 ErrItemRefRequired）。
		{"zone_id 为负", &rpc.ListTopicsReq{ZoneId: -1}, model.ErrItemRefRequired},
		{"tag_id 为负", &rpc.ListTopicsReq{TagId: -7}, model.ErrItemRefRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listTopics(t, tc.req)
			wantFail(t, err, tc.want, tc.name)
			if reply != nil {
				t.Fatalf("%s：出错还回了响应体", tc.name)
			}
			if got := e.call("Topic.List"); got != 0 {
				t.Fatalf("%s：入参校验阶段就查了库：%d 次", tc.name, got)
			}
		})
	}
	// 64  rune 的关键词在界内（长度是字符数而不是字节数：中文搜索框最容易被字节数误伤）。
	if _, err := e.listTopics(t, &rpc.ListTopicsReq{Keyword: strings.Repeat("词", 64)}); err != nil {
		t.Fatalf("64 个汉字的关键词被拒了：%v", err)
	}
	e.requireSameEffects(t, before, "非法 ListTopics 入参零副作用")
}

func TestListTopicsOrdersPagesAndFiltersByStateWindow(t *testing.T) {
	e := newTestEnv(t)
	now := model.NowUnix()
	// 种子顺序就是 topic_id 顺序：a 必须排在 b 之前，「同 sort 按 id ASC 兜底」才可读。
	e.seedTopic(t, &model.Topic{Slug: "lt-sort-a", Title: "同序 a", State: model.StateOn, Sort: 10})
	e.seedTopic(t, &model.Topic{Slug: "lt-sort-b", Title: "同序 b", State: model.StateOn, Sort: 10})
	e.seedTopic(t, &model.Topic{Slug: "lt-first", Title: "最前", State: model.StateOn, Sort: 1})
	e.seedTopic(t, &model.Topic{Slug: "lt-off", Title: "下架", State: model.StateOff, Sort: 2})
	e.seedTopic(t, &model.Topic{Slug: "lt-future", Title: "未开始", State: model.StateOn,
		Sort: 3, StartAt: now + 3600, EndAt: now + 7200})
	e.seedTopic(t, &model.Topic{Slug: "lt-past", Title: "已结束", State: model.StateOn,
		Sort: 4, StartAt: now - 7200, EndAt: now - 3600})

	all, err := e.listTopics(t, &rpc.ListTopicsReq{Ps: 100})
	rows := wantOK(t, all, err, "全量")
	// 排序固定 sort ASC, topic_id ASC：没有第二个 tiebreaker 时翻页会重复或漏行。
	// 默认列表不筛状态、也不看生效窗口（下架/未开始/过期都要在后台列得出来），
	// 所以次序只由 sort 决定：1/2/3/4/10/10。
	if got := strings.Join(topicSlugs(rows.GetItems()), ","); got !=
		"lt-first,lt-off,lt-future,lt-past,lt-sort-a,lt-sort-b" {
		t.Fatalf("排序不是 sort ASC, topic_id ASC：%s", got)
	}
	if rows.GetTotal() != 6 {
		t.Fatalf("total 不是筛选后全量：%d", rows.GetTotal())
	}
	// 同 sort 的两条按 topic_id 兜底：种子顺序即 id 顺序，lt-sort-a 的 ID 更小。
	if rows.GetItems()[4].GetSlug() != "lt-sort-a" || rows.GetItems()[5].GetSlug() != "lt-sort-b" {
		t.Fatalf("同 sort 没按 topic_id 兜底：%v", topicSlugs(rows.GetItems()))
	}

	// online_only：只要「上架且此刻在窗口内」。
	online, err := e.listTopics(t, &rpc.ListTopicsReq{OnlineOnly: true, Ps: 100})
	live := wantOK(t, online, err, "online_only")
	if got := strings.Join(topicSlugs(live.GetItems()), ","); got != "lt-first,lt-sort-a,lt-sort-b" {
		t.Fatalf("online_only 结果不符：%s（未开始/已结束/下架都不该出现）", got)
	}
	// 不给 online_only：窗口不参与判定，未开始与已结束的专题都在（后台要能看到它们）。
	if len(rows.GetItems()) != 6 {
		t.Fatalf("非 online_only 却做了窗口判定：%v", topicSlugs(rows.GetItems()))
	}
	// online_only 与 state=OFF 同时给：两个条件相交为空，而不是「后一个覆盖前一个」。
	both, err := e.listTopics(t, &rpc.ListTopicsReq{OnlineOnly: true, State: model.StateOff, Ps: 100})
	conflict := wantOK(t, both, err, "online_only + state=OFF")
	if len(conflict.GetItems()) != 0 || conflict.GetTotal() != 0 {
		t.Fatalf("互斥条件应回空集：%v", topicSlugs(conflict.GetItems()))
	}
	// 只看下架。
	off, err := e.listTopics(t, &rpc.ListTopicsReq{State: model.StateOff})
	offs := wantOK(t, off, err, "state=OFF")
	if strings.Join(topicSlugs(offs.GetItems()), ",") != "lt-off" {
		t.Fatalf("state=OFF 结果不符：%v", topicSlugs(offs.GetItems()))
	}

	// 翻页：两页拼起来必须等于全量。
	p1, err := e.listTopics(t, &rpc.ListTopicsReq{Pn: 1, Ps: 4})
	first := wantOK(t, p1, err, "第一页")
	p2, err := e.listTopics(t, &rpc.ListTopicsReq{Pn: 2, Ps: 4})
	second := wantOK(t, p2, err, "第二页")
	if len(first.GetItems()) != 4 || len(second.GetItems()) != 2 {
		t.Fatalf("页大小没被执行：%d / %d", len(first.GetItems()), len(second.GetItems()))
	}
	// 翻页拼接必须与上面那份全量逐条同序（次序本身已在「全量」处钉住，这里不再抄一遍字面量）。
	joined := append(topicSlugs(first.GetItems()), topicSlugs(second.GetItems())...)
	if strings.Join(joined, ",") != strings.Join(topicSlugs(rows.GetItems()), ",") {
		t.Fatalf("翻页拼接与全量不一致：\n  got =%v\n want=%v", joined, topicSlugs(rows.GetItems()))
	}
	// ps 越界夹到上限、缺省回落到上限（配置 Query.MaxPageSize=100）。
	big, err := e.listTopics(t, &rpc.ListTopicsReq{Ps: 900})
	clamped := wantOK(t, big, err, "ps 越界")
	if len(clamped.GetItems()) != 6 {
		t.Fatalf("全量 6 条，夹取不该改变本页条数：%d", len(clamped.GetItems()))
	}
	// 只读：零写、零缓存。
	if len(e.cache.sets) != 0 || len(e.cache.gets) != 0 || len(e.audit.reqs) != 0 {
		t.Fatalf("ListTopics 动了缓存或审计：sets=%v gets=%v audit=%d",
			e.cache.sets, e.cache.gets, len(e.audit.reqs))
	}
}

func TestListTopicsRefFiltersAndKeyword(t *testing.T) {
	e := newTestEnv(t)
	e.seedTopic(t, &model.Topic{Slug: "rf-both", Title: "分区 1 与 12", State: model.StateOn,
		ZoneIDs: ",1,12,", TagIDs: ",77,"})
	e.seedTopic(t, &model.Topic{Slug: "rf-twelve", Title: "只有 12", State: model.StateOn, ZoneIDs: ",12,"})
	e.seedTopic(t, &model.Topic{Slug: "rf-none", Title: "不挂分区", State: model.StateOn})

	// 引用 ID 的存储形态是 ",1,12,"（前后都带逗号）：查 zone_id=1 绝不能命中只挂了 12 的专题。
	// 这一条同时保护读侧与写侧——少了逗号包裹，LIKE '%,1,%' 就会把 1 与 12/112 混成一谈。
	// 三条种子的 sort 都是 0，所以次序完全由 tiebreaker topic_id ASC = 种子顺序决定：
	// rf-both(1) → rf-twelve(2) → rf-none(3)。写期望时按这个顺序抄，别按 slug 字典序。
	for _, tc := range []struct {
		name      string
		zone, tag int64
		want      string
	}{
		{"zone_id=1", 1, 0, "rf-both"},
		{"zone_id=12", 12, 0, "rf-both,rf-twelve"},
		{"zone_id=112（不存在）", 112, 0, ""},
		{"zone_id=0＝不过滤", 0, 0, "rf-both,rf-twelve,rf-none"},
		{"tag_id=77", 0, 77, "rf-both"},
		{"tag_id=7（前缀陷阱）", 0, 7, ""},
		{"zone 与 tag 同时给（交集）", 12, 77, "rf-both"},
		{"zone 与 tag 无交集", 12, 7, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listTopics(t, &rpc.ListTopicsReq{ZoneId: tc.zone, TagId: tc.tag, Ps: 100})
			got := wantOK(t, reply, err, tc.name)
			if s := strings.Join(topicSlugs(got.GetItems()), ","); s != tc.want {
				t.Fatalf("%s：结果不符\n  got =%s\n want=%s", tc.name, s, tc.want)
			}
		})
	}

	// 关键词命中 slug 或标题任一处，并且两端空白被裁掉。
	for _, tc := range []struct {
		name string
		kw   string
		want string
	}{
		{"命中 slug 片段", "twelve", "rf-twelve"},
		{"命中标题（中文）", "分区 1 与", "rf-both"},
		{"带空白的关键词", "  不挂分区  ", "rf-none"},
		{"同时命中三条（slug 或标题任一即可）", "rf-", "rf-both,rf-twelve,rf-none"},
		{"通配符按字面量处理（model 侧 escapeLike，见 ops_topic.go:326）", "%", ""},
		{"查不到的词", "不存在", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listTopics(t, &rpc.ListTopicsReq{Keyword: tc.kw, Ps: 100})
			got := wantOK(t, reply, err, tc.name)
			if s := strings.Join(topicSlugs(got.GetItems()), ","); s != tc.want {
				t.Fatalf("%s：关键词结果不符\n  got =%s\n want=%s", tc.name, s, tc.want)
			}
		})
	}

	// 投影只回引用 ID，绝不回分区名/标签名（AGENTS.md §5：本服务不复制 catalog 主资料）。
	one, err := e.listTopics(t, &rpc.ListTopicsReq{ZoneId: 1, Ps: 100})
	row := wantOK(t, one, err, "引用投影")
	item := row.GetItems()[0]
	if len(item.GetZoneIds()) != 2 || item.GetZoneIds()[0] != 1 || item.GetZoneIds()[1] != 12 {
		t.Fatalf("zone_ids 没还原成数组：%v", item.GetZoneIds())
	}
	if len(item.GetTagIds()) != 1 || item.GetTagIds()[0] != 77 {
		t.Fatalf("tag_ids 没还原成数组：%v", item.GetTagIds())
	}
	// 不挂分区的专题回空数组而不是 [0]：一个 0 元素会被调用方当成「有效分区 ID」。
	none, err := e.listTopics(t, &rpc.ListTopicsReq{Keyword: "不挂", Ps: 100})
	part := wantOK(t, none, err, "不挂分区")
	if len(part.GetItems()[0].GetZoneIds()) != 0 {
		t.Fatalf("不挂分区的专题回了非空数组：%v", part.GetItems()[0].GetZoneIds())
	}
}

func TestListTopicsAndGetTopicAgreeOnTheSameConfig(t *testing.T) {
	e := newTestEnv(t)
	now := model.NowUnix()
	seed := []struct {
		slug           string
		state          int32
		startAt, endAt int64
	}{
		{"ax-live", model.StateOn, 0, 0},
		{"ax-future", model.StateOn, now + 3600, now + 7200},
		{"ax-past", model.StateOn, now - 7200, now - 3600},
		{"ax-offline", model.StateOff, 0, 0},
	}
	for _, s := range seed {
		e.seedTopic(t, &model.Topic{Slug: s.slug, Title: s.slug, State: s.state,
			StartAt: s.startAt, EndAt: s.endAt, Sort: 5})
	}
	listed, err := e.listTopics(t, &rpc.ListTopicsReq{OnlineOnly: true, Ps: 100})
	rows := wantOK(t, listed, err, "online_only 视图")
	inList := map[string]bool{}
	for _, slug := range topicSlugs(rows.GetItems()) {
		inList[slug] = true
	}
	for _, s := range seed {
		reply, err := e.getTopic(t, &rpc.GetTopicReq{Slug: s.slug})
		got := wantOK(t, reply, err, "GetTopic "+s.slug)
		if !got.GetFound() {
			t.Fatalf("%s：库里有的专题解析成「不存在」（两侧不一致）", s.slug)
		}
		onlineNow := s.state == model.StateOn &&
			(s.startAt == 0 || now >= s.startAt) && (s.endAt == 0 || now < s.endAt)
		if inList[s.slug] != onlineNow {
			t.Fatalf("%s：online_only 视图与专题状态不符（列表=%v 应=%v）", s.slug, inList[s.slug], onlineNow)
		}
		// ttl>0 与「可以被端上缓存」必须与 online_only 视图同口径。
		if (got.GetTtl() > 0) != (s.state == model.StateOn) {
			t.Fatalf("%s：state=%d 却回了 ttl=%d", s.slug, s.state, got.GetTtl())
		}
	}
	if len(inList) != 1 || !inList["ax-live"] {
		t.Fatalf("online_only 只应有 ax-live：%v", inList)
	}
}

func TestListTopicsPropagatesStorageErrorVerbatim(t *testing.T) {
	e := newTestEnv(t)
	e.seedTopic(t, &model.Topic{Slug: "lt-db", Title: "库故障", State: model.StateOn})
	before := e.effects()

	e.db.readErrs["Topic.List"] = errTopicStoreDown
	reply, err := e.listTopics(t, &rpc.ListTopicsReq{Ps: 100})
	wantFail(t, err, errTopicStoreDown, "Topic.List 故障")
	if reply != nil {
		t.Fatalf("依赖故障却回了响应体（空列表是伪装）：%+v", reply)
	}
	delete(e.db.readErrs, "Topic.List")
	ok, err := e.listTopics(t, &rpc.ListTopicsReq{Ps: 100})
	back := wantOK(t, ok, err, "恢复后")
	if len(back.GetItems()) != 1 {
		t.Fatalf("恢复后读不到：%v", topicSlugs(back.GetItems()))
	}
	e.requireSameEffects(t, before, "ListTopics 只读零副作用")
}
