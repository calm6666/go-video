package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/inbox/internal/config"
	"go-video/services/inbox/model"
)

// 本文件只用内存假件验证 repository 的编排逻辑（事务配对、快照增量与重算的一致性、
// 缓存失效时机、分页与限额），不连接 MySQL/Redis/Kafka。
// 假件必须忠实复现 deploy/migrations/inbox/*.sql 的约束语义：
// uniq(idempotency_key)、uniq(mid,msg_id)、PRIMARY KEY(mid,category)、GREATEST 兜底，
// 且错误必须真实返回，避免「永不失败的假实现」（AGENTS.md §9）。

// --- 内存数据表：复现三张表的唯一键与状态语义 ---

type memStore struct {
	nextMsgID int64
	nextRowID int64
	msgs      map[int64]*model.InboxMessage
	byKey     map[string]int64
	rows      []*model.InboxUserMessage
	stat      map[int64]map[int32]int64
	statKnown map[int64]bool
	// fail 非空时，匹配的写操作返回错误（模拟依赖不可用，不得被吞掉）。
	fail string
}

func newMemStore() *memStore {
	return &memStore{
		msgs:      make(map[int64]*model.InboxMessage),
		byKey:     make(map[string]int64),
		stat:      make(map[int64]map[int32]int64),
		statKnown: make(map[int64]bool),
	}
}

func (s *memStore) maybeFail(op string) error {
	if s.fail != "" && s.fail == op {
		return fmt.Errorf("inbox/repository/test: injected failure at %s", op)
	}
	return nil
}

// --- msgModel 实现 model.InboxMessageModel ---

type msgModel struct{ s *memStore }

func (m *msgModel) InsertIdempotent(
	_ context.Context, _ sqlx.Session, msg *model.InboxMessage,
) (int64, bool, error) {
	if err := m.s.maybeFail("insert_message"); err != nil {
		return 0, false, err
	}
	if msg.IdempotencyKey == "" {
		return 0, false, model.ErrInvalidIdempotencyKey
	}
	if id, ok := m.s.byKey[msg.IdempotencyKey]; ok {
		return id, false, nil // uniq_idempotency_key：命中幂等键不写新行
	}
	m.s.nextMsgID++
	msg.MsgID = m.s.nextMsgID
	msg.State = model.MessageStateNormal
	if msg.Ctime == 0 {
		msg.Ctime = 1700000000
	}
	copied := *msg
	m.s.msgs[msg.MsgID] = &copied
	m.s.byKey[msg.IdempotencyKey] = msg.MsgID
	return msg.MsgID, true, nil
}

func (m *msgModel) FindOne(_ context.Context, msgID int64) (*model.InboxMessage, error) {
	if msg, ok := m.s.msgs[msgID]; ok {
		copied := *msg
		return &copied, nil
	}
	return nil, nil
}

func (m *msgModel) Withdraw(_ context.Context, msgID int64) error {
	if msg, ok := m.s.msgs[msgID]; ok {
		msg.State = model.MessageStateWithdrawn
	}
	return nil
}

// --- userModel 实现 model.InboxUserMessageModel ---

type userModel struct{ s *memStore }

func (m *userModel) InsertIdempotent(
	_ context.Context, _ sqlx.Session, um *model.InboxUserMessage,
) (bool, error) {
	if err := m.s.maybeFail("insert_user_message"); err != nil {
		return false, err
	}
	for _, row := range m.s.rows {
		if row.Mid == um.Mid && row.MsgID == um.MsgID {
			return false, nil // uniq_mid_msg
		}
	}
	m.s.nextRowID++
	if um.Ctime == 0 {
		um.Ctime = 1700000000
	}
	um.ID = m.s.nextRowID
	copied := *um
	m.s.rows = append(m.s.rows, &copied)
	return true, nil
}

func (m *userModel) List(_ context.Context, f model.ListFilter) ([]*model.MessageRow, error) {
	var out []*model.MessageRow
	for _, row := range m.s.rows {
		msg, ok := m.s.msgs[row.MsgID]
		if !ok || row.Mid != f.Mid {
			continue
		}
		if row.DelState != model.DelStateNormal || msg.State != model.MessageStateNormal {
			continue
		}
		if model.ValidCategory(f.Category) && row.Category != f.Category {
			continue
		}
		if f.UnreadOnly && row.ReadState != model.ReadStateUnread {
			continue
		}
		if f.CursorTime > 0 && !(row.Ctime < f.CursorTime ||
			(row.Ctime == f.CursorTime && row.ID < f.CursorID)) {
			continue
		}
		out = append(out, &model.MessageRow{
			ID: row.ID, MsgID: row.MsgID, Mid: row.Mid, Category: row.Category,
			MsgType: msg.MsgType, Title: msg.Title, Content: msg.Content,
			SenderMid: msg.SenderMid, BizType: msg.BizType, BizID: msg.BizID,
			Extra: msg.Extra, ReadState: row.ReadState, DelState: row.DelState, Ctime: row.Ctime,
		})
	}
	// 与真实 SQL 一致：ORDER BY ctime DESC, id DESC
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && outLess(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if f.Limit > 0 && len(out) > int(f.Limit) {
		out = out[:f.Limit]
	}
	return out, nil
}

func outLess(a, b *model.MessageRow) bool {
	if a.Ctime != b.Ctime {
		return a.Ctime > b.Ctime
	}
	return a.ID > b.ID
}

func (m *userModel) MarkReadBatch(
	_ context.Context, _ sqlx.Session, mid int64, msgIDs []int64,
) (int64, error) {
	if err := m.s.maybeFail("mark_read"); err != nil {
		return 0, err
	}
	var changed int64
	for _, row := range m.s.rows {
		if row.Mid != mid || row.ReadState != model.ReadStateUnread || row.DelState != model.DelStateNormal {
			continue
		}
		if containsID(msgIDs, row.MsgID) {
			row.ReadState = model.ReadStateRead
			changed++
		}
	}
	return changed, nil
}

func (m *userModel) MarkAllReadBatch(
	_ context.Context, _ sqlx.Session, mid int64, category int32,
) (int64, error) {
	var changed int64
	for _, row := range m.s.rows {
		if row.Mid != mid || row.ReadState != model.ReadStateUnread || row.DelState != model.DelStateNormal {
			continue
		}
		if model.ValidCategory(category) && row.Category != category {
			continue
		}
		row.ReadState = model.ReadStateRead
		changed++
	}
	return changed, nil
}

func (m *userModel) SoftDeleteBatch(
	_ context.Context, _ sqlx.Session, mid int64, msgIDs []int64,
) (int64, error) {
	var changed int64
	for _, row := range m.s.rows {
		if row.Mid != mid || row.DelState != model.DelStateNormal || !containsID(msgIDs, row.MsgID) {
			continue
		}
		row.DelState = model.DelStateDeleted
		row.ReadState = model.ReadStateRead
		changed++
	}
	return changed, nil
}

func (m *userModel) CountOwned(_ context.Context, mid int64, msgIDs []int64) (int64, error) {
	var n int64
	for _, row := range m.s.rows {
		if row.Mid == mid && containsID(msgIDs, row.MsgID) {
			n++
		}
	}
	return n, nil
}

func (m *userModel) CountUnreadByCategory(_ context.Context, mid int64) ([]model.CategoryCount, error) {
	counts := make(map[int32]int64)
	for _, row := range m.s.rows {
		if row.Mid != mid || row.ReadState != model.ReadStateUnread || row.DelState != model.DelStateNormal {
			continue
		}
		counts[row.Category]++
	}
	out := make([]model.CategoryCount, 0, len(counts))
	for _, category := range model.AllCategories() {
		if n, ok := counts[category]; ok {
			out = append(out, model.CategoryCount{Category: category, Unread: n})
		}
	}
	return out, nil
}

func containsID(ids []int64, v int64) bool {
	for _, id := range ids {
		if id == v {
			return true
		}
	}
	return false
}

// --- statModel 实现 model.UnreadStatModel ---

type statModel struct{ s *memStore }

func (m *statModel) ReplaceByMid(
	_ context.Context, _ sqlx.Session, mid int64, counts map[int32]int64,
) error {
	if err := m.s.maybeFail("replace_stat"); err != nil {
		return err
	}
	snapshot := make(map[int32]int64, len(model.AllCategories()))
	for _, category := range model.AllCategories() {
		snapshot[category] = counts[category] // 与真实 SQL 一致：四个分类全写，缺失记 0
	}
	m.s.stat[mid] = snapshot
	m.s.statKnown[mid] = true
	return nil
}

func (m *statModel) IncrBy(
	_ context.Context, _ sqlx.Session, mid int64, category int32, delta int64,
) error {
	if delta == 0 {
		return nil
	}
	if err := m.s.maybeFail("incr_stat"); err != nil {
		return err
	}
	snapshot, ok := m.s.stat[mid]
	if !ok {
		snapshot = make(map[int32]int64)
		m.s.stat[mid] = snapshot
		m.s.statKnown[mid] = true
	}
	// GREATEST(unread + ?, 0)：异常写入也不会产生负数快照。
	snapshot[category] = maxInt64(snapshot[category]+delta, 0)
	return nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (m *statModel) ListByMid(_ context.Context, mid int64) ([]model.UnreadStat, error) {
	if !m.s.statKnown[mid] {
		return nil, nil // 无记录返回空切片：读路径据此回落到明细表重算
	}
	out := make([]model.UnreadStat, 0, len(m.s.stat[mid]))
	for _, category := range model.AllCategories() {
		out = append(out, model.UnreadStat{Mid: mid, Category: category, Unread: m.s.stat[mid][category]})
	}
	return out, nil
}

func (m *statModel) DeleteByMid(_ context.Context, mid int64) error {
	delete(m.s.stat, mid)
	delete(m.s.statKnown, mid)
	return nil
}

// --- 不本文件覆盖路径的 model：被调用即说明测试范围变了，明确报错而不是静默通过 ---

type offsetModel struct{ s *memStore }

func (m *offsetModel) Claim(context.Context, *model.ConsumerOffset, int64) (model.ClaimOutcome, *model.ConsumerOffset, error) {
	return model.ClaimDeferred, nil, errors.New("offsetModel.Claim not exercised by these tests")
}
func (m *offsetModel) MarkSucceeded(context.Context, string) error { return nil }
func (m *offsetModel) MarkRetry(context.Context, string, int64, string, string) error {
	return nil
}
func (m *offsetModel) MarkDeadLetter(context.Context, string, string, string) error { return nil }
func (m *offsetModel) FindOne(context.Context, string) (*model.ConsumerOffset, error) {
	return nil, nil
}
func (m *offsetModel) ListOverdueRetry(context.Context, int64, int32) ([]*model.ConsumerOffset, error) {
	return nil, nil
}

type dlqModel struct{ s *memStore }

func (m *dlqModel) InsertIdempotent(context.Context, *model.DeadLetter) (bool, error) {
	return false, nil
}
func (m *dlqModel) ListOpen(context.Context, int32) ([]*model.DeadLetter, error) { return nil, nil }
func (m *dlqModel) MarkState(context.Context, int64, string) error               { return nil }

// --- 事务连接与缓存假件 ---

// fakeConn 只实现本文件用到的 TransactCtx：内存假件不区分事务内/外，
// 但 Repository 的「明细 + 快照同事务」编排必须被真实走到。
// 其余方法由嵌入的 nil 接口兜底，一旦有测试越界使用会立刻 panic。
type fakeConn struct {
	sqlx.SqlConn
}

func (fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	return fn(ctx, fakeSession{})
}

type fakeSession struct{ sqlx.Session }

type fakeCache struct {
	data        map[int64]map[int32]int64
	invalidated []int64
	gets, sets  int
	hits        int
	failSet     bool
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: make(map[int64]map[int32]int64)}
}

func (c *fakeCache) Get(_ context.Context, mid int64) (map[int32]int64, bool) {
	c.gets++
	snapshot, ok := c.data[mid]
	if ok {
		c.hits++
	}
	return snapshot, ok
}

func (c *fakeCache) Set(_ context.Context, mid int64, snapshot map[int32]int64) {
	if c.failSet {
		return // Redis 写失败不得影响读路径
	}
	c.sets++
	copied := make(map[int32]int64, len(snapshot))
	for k, v := range snapshot {
		copied[k] = v
	}
	c.data[mid] = copied
}

func (c *fakeCache) Invalidate(_ context.Context, mids ...int64) {
	for _, mid := range mids {
		delete(c.data, mid)
		c.invalidated = append(c.invalidated, mid)
	}
}

func (c *fakeCache) Ping(context.Context) error { return nil }

// --- 装配辅助 ---

func testConf() config.InboxConf {
	return config.InboxConf{PageSize: 20, MaxPageSize: 50, MaxRecipients: 5, UnreadCacheSeconds: 1800, MaxContentBytes: 4096}
}

func newTestRepo(t *testing.T) (*Repository, *memStore, *fakeCache) {
	t.Helper()
	store := newMemStore()
	cache := newFakeCache()
	repo := newRepository(fakeConn{}, cache, testConf(),
		&msgModel{s: store}, &userModel{s: store}, &statModel{s: store},
		&offsetModel{s: store}, &dlqModel{s: store})
	return repo, store, cache
}

func sysMsg(key, title string) *model.InboxMessage {
	return &model.InboxMessage{
		Category: model.CategorySystem, MsgType: model.MsgTypeText,
		Title: title, Content: title + " 正文", IdempotencyKey: key,
	}
}

func unreadTotal(t *testing.T, repo *Repository, mid int64, force bool) *UnreadSnapshot {
	t.Helper()
	snapshot, err := repo.GetUnread(context.Background(), mid, force)
	if err != nil {
		t.Fatalf("GetUnread(mid=%d, force=%v) 失败: %v", mid, force, err)
	}
	return snapshot
}

// --- 投递与幂等 ---

func TestDeliverWritesOneRowPerRecipientAndIncrementsUnread(t *testing.T) {
	repo, store, cache := newTestRepo(t)
	ctx := context.Background()

	res, err := repo.Deliver(ctx, sysMsg("sys:1", "系统维护"), []int64{11, 12, 11, 0, -3, 13})
	if err != nil {
		t.Fatalf("Deliver 失败: %v", err)
	}
	if res.MsgID == 0 || res.Delivered != 3 || res.Deduplicated {
		t.Fatalf("期望 1 条消息 3 个收件人，得到 %+v", res)
	}
	if len(store.rows) != 3 {
		t.Fatalf("收件明细应去重后 3 行，得到 %d 行", len(store.rows))
	}
	for _, mid := range []int64{11, 12, 13} {
		if got := store.stat[mid][model.CategorySystem]; got != 1 {
			t.Fatalf("mid=%d 未读快照期望 1，得到 %d", mid, got)
		}
	}
	// 提交后只失效缓存，不做增量写缓存：避免事务回滚留下脏计数。
	if len(cache.invalidated) != 3 {
		t.Fatalf("期望失效 3 个用户的快照缓存，实际 %v", cache.invalidated)
	}
	if len(cache.data) != 0 {
		t.Fatal("投递路径不得预热 Redis 缓存")
	}
}

func TestDeliverSameIdempotencyKeyIsNoop(t *testing.T) {
	repo, store, _ := newTestRepo(t)
	ctx := context.Background()
	msg := sysMsg("sys:dup", "重复投递")

	first, err := repo.Deliver(ctx, msg, []int64{21, 22})
	if err != nil {
		t.Fatalf("首次 Deliver 失败: %v", err)
	}
	second, err := repo.Deliver(ctx, sysMsg("sys:dup", "重复投递"), []int64{21, 22})
	if err != nil {
		t.Fatalf("重复 Deliver 失败: %v", err)
	}
	if !second.Deduplicated || second.Delivered != 0 {
		t.Fatalf("重复投递应命中幂等键且不新增收件行，得到 %+v", second)
	}
	if second.MsgID != first.MsgID {
		t.Fatalf("幂等命中应返回首条 msg_id=%d，得到 %d", first.MsgID, second.MsgID)
	}
	if len(store.rows) != 2 {
		t.Fatalf("收件明细应仍为 2 行，得到 %d 行", len(store.rows))
	}
	for _, mid := range []int64{21, 22} {
		if got := store.stat[mid][model.CategorySystem]; got != 1 {
			t.Fatalf("重复投递后 mid=%d 未读仍应为 1，得到 %d", mid, got)
		}
	}
}

func TestDeliverRejectsInvalidInput(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()

	if _, err := repo.Deliver(ctx, sysMsg("sys:nokey", "x"), nil); !errors.Is(err, model.ErrEmptyRecipients) {
		t.Fatalf("空收件人期望 ErrEmptyRecipients，得到 %v", err)
	}
	if _, err := repo.Deliver(ctx, sysMsg("", "x"), []int64{1}); !errors.Is(err, model.ErrInvalidIdempotencyKey) {
		t.Fatalf("空幂等键期望 ErrInvalidIdempotencyKey，得到 %v", err)
	}
	blank := sysMsg("sys:blank", "")
	blank.Title, blank.Content = "", ""
	if _, err := repo.Deliver(ctx, blank, []int64{1}); !errors.Is(err, model.ErrEmptyContent) {
		t.Fatalf("空标题与正文期望 ErrEmptyContent，得到 %v", err)
	}
	if _, err := repo.Deliver(ctx, sysMsg("sys:many", "x"), []int64{1, 2, 3, 4, 5, 6}); !errors.Is(err, model.ErrTooManyRecipients) {
		t.Fatalf("超过 MaxRecipients 期望 ErrTooManyRecipients，得到 %v", err)
	}
	if _, err := repo.Deliver(ctx, sysMsg("sys:badcat", "x"), []int64{1}); err != nil {
		t.Fatalf("默认分类应可用，得到 %v", err)
	}
	wrong := sysMsg("sys:wrongcat", "x")
	wrong.Category = 9
	if _, err := repo.Deliver(ctx, wrong, []int64{1}); !errors.Is(err, model.ErrInvalidCategory) {
		t.Fatalf("非法分类期望 ErrInvalidCategory，得到 %v", err)
	}
}

func TestDeliverErrorPropagatesWithoutCacheInvalidate(t *testing.T) {
	repo, store, cache := newTestRepo(t)
	store.fail = "incr_stat"

	if _, err := repo.Deliver(context.Background(), sysMsg("sys:fail", "计数写失败"), []int64{31}); err == nil {
		t.Fatal("依赖失败必须返回错误，不能静默成功")
	}
	if len(cache.invalidated) != 0 {
		t.Fatalf("事务未提交时不得失效缓存，实际 %v", cache.invalidated)
	}
}

// --- 未读计数：增量快照与明细重算必须一致 ---

func TestUnreadSnapshotAlwaysMatchesRecompute(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()
	const mid = int64(41)

	// 三种分类各投一条，然后逐步已读/删除，每一步都比对快照与重算结果。
	for i, category := range []int32{model.CategorySystem, model.CategoryEngagement, model.CategoryLive} {
		msg := sysMsg(fmt.Sprintf("mix:%d", i), "混合分类")
		msg.Category = category
		if _, err := repo.Deliver(ctx, msg, []int64{mid}); err != nil {
			t.Fatalf("Deliver category=%d 失败: %v", category, err)
		}
	}

	assertConsistent := func(step string) {
		t.Helper()
		snapshot := unreadTotal(t, repo, mid, false)
		recomputed := unreadTotal(t, repo, mid, true)
		if snapshot.Total != recomputed.Total {
			t.Fatalf("%s：快照总数 %d 与明细重算 %d 不一致", step, snapshot.Total, recomputed.Total)
		}
		for _, category := range model.AllCategories() {
			if snapshot.ByCategory[category] != recomputed.ByCategory[category] {
				t.Fatalf("%s：分类 %d 快照 %d 与重算 %d 不一致",
					step, category, snapshot.ByCategory[category], recomputed.ByCategory[category])
			}
		}
	}
	assertConsistent("全部未读")
	if got := unreadTotal(t, repo, mid, false); got.Total != 3 {
		t.Fatalf("期望未读 3，得到 %d", got.Total)
	}

	msgIDs := []int64{1, 2}
	if _, err := repo.MarkReadBatch(ctx, mid, msgIDs); err != nil {
		t.Fatalf("MarkReadBatch 失败: %v", err)
	}
	assertConsistent("标记两条已读后")
	if got := unreadTotal(t, repo, mid, false); got.Total != 1 {
		t.Fatalf("期望未读 1，得到 %d", got.Total)
	}

	// 重复标记已读必须零副作用（不重复扣减快照）。
	if changed, err := repo.MarkReadBatch(ctx, mid, msgIDs); err != nil || changed != 0 {
		t.Fatalf("重复标记已读期望 changed=0，得到 changed=%d err=%v", changed, err)
	}
	assertConsistent("重复标记已读后")

	if _, err := repo.MarkAllRead(ctx, mid, model.CategoryAll); err != nil {
		t.Fatalf("MarkAllRead 失败: %v", err)
	}
	assertConsistent("全部已读后")
	if got := unreadTotal(t, repo, mid, false); got.Total != 0 {
		t.Fatalf("期望未读 0，得到 %d", got.Total)
	}
}

func TestDeleteDecrementsUnreadAndStaysListedOut(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()
	const mid = int64(51)

	for i := 0; i < 3; i++ {
		if _, err := repo.Deliver(ctx, sysMsg(fmt.Sprintf("del:%d", i), "删除用例"), []int64{mid}); err != nil {
			t.Fatalf("Deliver 失败: %v", err)
		}
	}
	if got := unreadTotal(t, repo, mid, false); got.Total != 3 {
		t.Fatalf("期望未读 3，得到 %d", got.Total)
	}
	changed, exists, err := repo.DeleteMessages(ctx, mid, []int64{1, 2})
	if err != nil || !exists || changed != 2 {
		t.Fatalf("DeleteMessages 期望 (2,true)，得到 (%d,%v,%v)", changed, exists, err)
	}
	snapshot := unreadTotal(t, repo, mid, false)
	if snapshot.Total != 1 {
		t.Fatalf("删除后期望未读 1，得到 %d", snapshot.Total)
	}
	if snapshot.Source != SourceStat {
		t.Fatalf("删除后应回源 DB 快照，得到 source=%s", snapshot.Source)
	}
	rows, next, hasMore, err := repo.ListMessages(ctx, mid, model.CategoryAll, "", 10, false)
	if err != nil {
		t.Fatalf("ListMessages 失败: %v", err)
	}
	if len(rows) != 1 || next != "" || hasMore {
		t.Fatalf("删除后列表期望 1 条无游标，得到 %d 条 next=%q hasMore=%v", len(rows), next, hasMore)
	}
	// 再删一次同一批：幂等，且未读不被扣成负数。
	if changed, _, err = repo.DeleteMessages(ctx, mid, []int64{1, 2}); err != nil || changed != 0 {
		t.Fatalf("重复删除期望 changed=0，得到 changed=%d err=%v", changed, err)
	}
	if got := unreadTotal(t, repo, mid, true); got.Total != 1 {
		t.Fatalf("重复删除后未读应仍为 1，得到 %d", got.Total)
	}
}

func TestDeleteOtherUsersMessageIsRejected(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()
	if _, err := repo.Deliver(ctx, sysMsg("own:1", "归属校验"), []int64{61}); err != nil {
		t.Fatalf("Deliver 失败: %v", err)
	}
	changed, exists, err := repo.DeleteMessages(ctx, 62, []int64{1})
	if err != nil {
		t.Fatalf("DeleteMessages 失败: %v", err)
	}
	if changed != 0 || exists {
		t.Fatalf("越权删除期望 (0,false)，得到 (%d,%v)", changed, exists)
	}
	if got := unreadTotal(t, repo, 61, false); got.Total != 1 {
		t.Fatalf("越权尝试不得改变他人未读，得到 %d", got.Total)
	}
}

func TestRecomputeRepairsDriftedSnapshot(t *testing.T) {
	repo, store, cache := newTestRepo(t)
	const mid = int64(71)
	if _, err := repo.Deliver(context.Background(), sysMsg("drift:1", "计数漂移"), []int64{mid}); err != nil {
		t.Fatalf("Deliver 失败: %v", err)
	}
	// 直接篡改快照表：模拟历史 bug 或外部写入造成的漂移。
	store.stat[mid][model.CategorySystem] = 99
	delete(cache.data, mid)

	dirty := unreadTotal(t, repo, mid, false)
	if dirty.Source != SourceStat || dirty.Total != 99 {
		t.Fatalf("非强制读应先命中脏快照，得到 %+v", dirty)
	}
	fixed := unreadTotal(t, repo, mid, true)
	if fixed.Source != SourceRecompute || fixed.Total != 1 {
		t.Fatalf("强制重算期望 total=1，得到 %+v", fixed)
	}
	if store.stat[mid][model.CategorySystem] != 1 {
		t.Fatalf("重算必须回写快照表，得到 %d", store.stat[mid][model.CategorySystem])
	}
	// 重算后普通读也回到真值，并回填了缓存。
	after := unreadTotal(t, repo, mid, false)
	if after.Source != SourceCache || after.Total != 1 {
		t.Fatalf("重算后期望命中缓存 1，得到 %+v", after)
	}
}

func TestGetUnreadCacheLayers(t *testing.T) {
	repo, _, cache := newTestRepo(t)
	ctx := context.Background()
	const mid = int64(81)
	if _, err := repo.Deliver(ctx, sysMsg("cache:1", "缓存层级"), []int64{mid}); err != nil {
		t.Fatalf("Deliver 失败: %v", err)
	}
	if first := unreadTotal(t, repo, mid, false); first.Source != SourceStat {
		t.Fatalf("首次读期望回源快照，得到 %s", first.Source)
	}
	if second := unreadTotal(t, repo, mid, false); second.Source != SourceCache {
		t.Fatalf("第二次读期望命中缓存，得到 %s", second.Source)
	}
	if _, err := repo.MarkAllRead(ctx, mid, model.CategoryAll); err != nil {
		t.Fatalf("MarkAllRead 失败: %v", err)
	}
	third := unreadTotal(t, repo, mid, false)
	if third.Source != SourceStat || third.Total != 0 {
		t.Fatalf("写后期望缓存被失效并读到 0，得到 %+v", third)
	}
	if len(cache.invalidated) == 0 {
		t.Fatal("写路径必须失效受影响用户")
	}
	// mid 非法时不得静默返回空快照。
	if _, err := repo.GetUnread(ctx, 0, false); !errors.Is(err, model.ErrInvalidMid) {
		t.Fatalf("mid=0 期望 ErrInvalidMid，得到 %v", err)
	}
}

func TestGetUnreadWithoutStatRowsFallsBackToRecompute(t *testing.T) {
	repo, store, _ := newTestRepo(t)
	const mid = int64(91)
	// 手工造出「有明细、无快照」的状态：模拟快照表被清理任务清空的场景。
	store.nextMsgID++
	store.msgs[1] = &model.InboxMessage{MsgID: 1, Category: model.CategoryEngagement,
		IdempotencyKey: "orphan:1", Ctime: 1700000000, State: model.MessageStateNormal}
	store.byKey["orphan:1"] = 1
	store.rows = append(store.rows, &model.InboxUserMessage{
		ID: 1, Mid: mid, MsgID: 1, Category: model.CategoryEngagement,
		ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: 1700000000,
	})
	got := unreadTotal(t, repo, mid, false)
	if got.Source != SourceRecompute || got.Total != 1 {
		t.Fatalf("无快照行时应重算得 1，得到 %+v", got)
	}
	if !store.statKnown[mid] {
		t.Fatal("重算结果必须回写快照表")
	}
}
