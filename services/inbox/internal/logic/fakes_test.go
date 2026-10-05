package logic

// fakes_test.go 是 inbox logic 测试的内存替身集合。
//
// 为什么需要注入缝：inbox 的 ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(fakeConn, 内存 cache, 内存 msgMd/userMd/statMd/offsetMd/dlqMd)
// 组装**真实的 Repository**，只把它的 6 个依赖换成替身——这样「幂等投递、明细与快照同事务、
// Redis 加速副本的回源/回填/失效时机、游标翻页」整条编排都在被测路径上，
// 而不是把 Repository 也 mock 掉（否则 logic 测试什么都证明不了）。
// 见 internal/repository/cache.go 的 UnreadCache 注释。
//
// 四条替身纪律（catalog / rights / playback 三轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic 里的写回不得污染库存行，否则「有没有真的落库」
//     这类断言会被共享指针掩盖。
//  2. 副作用按**顺序**记录（callLog，格式 %s.%s:%s），断言序列而不是只断言次数：inbox 要紧的
//     结论是「计数变了有没有失效缓存」「重复标记已读有没有二次扣减」「写事务里是不是所有语句
//     都拿到同一个事务会话（op 后缀 tx / conn 就是证据）」。
//  3. 错误注入按方法粒度（faultInjector.failWith("ListByMid", err)），不用一个全局 err；
//     替身自己不复刻 model 的错误包装口径时会返回 errWrap，保证生产侧 errors.Is 仍然命中。
//  4. 布数据走替身的**静默写入路径**（seedMessage / seedRow / seedStat / cache.warm → put 而非
//     公开的 InsertIdempotent / ReplaceByMid）：布景不算被测调用，因此轨迹断言可以直接从 0 开始数。
//     如果哪天改成用公开方法布景，就必须先在布景之后取 before := st.log.snapshot()。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（uniq(idempotency_key)、uniq(mid,msg_id)、
//     INSERT IGNORE 的 affected 口径、GREATEST 兜底、ORDER BY ctime DESC,id DESC LIMIT），
//     不证明 SQL 文本与列名本身，那部分由 deploy/migrations/inbox/*.sql 与集成环境负责；
//   - 事务原子性由 MySQL 保证，替身用「事务内写入立即可见 + 登记逆操作 + 出错逆序回滚」模拟，
//     目的是断言「哪些写入被放进了同一个事务」，不是验证 MySQL 的隔离级别；
//   - Redis 的 HGETALL/HMSET/DEL 由 fakeCache 按同语义复刻，不验证 TTL 编码与真实连接故障
//     （UnreadCache.Get 没有 error 返回值，缓存故障在接口上就表现为 miss，即降级读）；
//   - 消费侧依赖（ConsumerOffsetModel / DeadLetterModel）不在 logic 路径上，替身一旦被调用
//     就返回 errUnusedDependency 并记轨迹，避免「永不失败的替身」。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/inbox/internal/config"
	"go-video/services/inbox/internal/repository"
	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
)

// --- 断言小工具（与 catalog / rights / playback 同口径） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want errors.Is(..., %v)", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(..., %v)", label, err, want)
	}
}

func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
	}
}

// wantNoCall 断言 from 之后没有发生任何依赖调用（守卫必须发生在触库/触缓存之前）。
func wantNoCall(t *testing.T, label string, st *store, from int) {
	t.Helper()
	if ops := st.log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

func wantOps(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label, strings.Join(got, " → "), strings.Join(want, " → "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s：第 %d 次调用 = %s, want %s（完整序列 [%s]",
				label, i+1, got[i], want[i], strings.Join(got, " → "))
		}
	}
}

// wantOpsPrefix 逐位比对调用序列，但每一位只要求前缀匹配：
// 用于「同一类语句带随机主键/可变参数列表」的场合（自增 msg_id、idsTag 的具体顺序），
// 钉死的是第几步、哪类语句，而不是它的不确定参数。
func wantOpsPrefix(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：调用序列 = [%s], want %d 项 [%s]", label, strings.Join(got, " → "), len(want), strings.Join(want, " → "))
	}
	for i, prefix := range want {
		if !strings.HasPrefix(got[i], prefix) {
			t.Errorf("%s：第 %d 次调用 = %s, want 前缀 %s（完整序列 [%s]）",
				label, i+1, got[i], prefix, strings.Join(got, " → "))
		}
	}
}

// wantAbsent 断言某类调用没有发生过（缓存不得在写失败时被回填、事务不得开第二次等）。
func wantAbsent(t *testing.T, label string, log *callLog, prefix string) {
	t.Helper()
	wantAbsentFrom(t, label, log, 0, prefix)
}

// wantAbsentFrom 只在 from 之后的区段里计数：幂等用例的前半段本来就是合法写入，
// 全轨迹计数会把「首次投递」也算进去，从而掩盖「重放是否二次扣减」这个结论。
func wantAbsentFrom(t *testing.T, label string, log *callLog, from int, prefix string) {
	t.Helper()
	ops := log.opsFrom(from)
	n := 0
	for _, o := range ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	if n != 0 {
		t.Errorf("%s：%s* 出现 %d 次，期望 0 次（完整序列 [%s]）", label, prefix, n, strings.Join(ops, " → "))
	}
}

// wantErrContains 用于非哨兵错误（repository 用 fmt.Errorf 直接生成的约束类提示）。
func wantErrContains(t *testing.T, label string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want 包含 %q", label, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s：错误 = %v, want 包含 %q", label, err, want)
	}
}

// wantMapEQ 逐键比较分类快照（map 不能直接用 ==，也不能只断言非空）。
func wantMapEQ(t *testing.T, label, field string, got, want map[int32]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s：%s = %v, want %v（键数不同）", label, field, got, want)
		return
	}
	for category, n := range want {
		if got[category] != n {
			t.Errorf("%s：%s[%d] = %d, want %d（完整值 %v）", label, field, category, got[category], n, got)
		}
	}
}

// assertAround 断言墙钟派生值落在期望附近：model 内部用 time.Now()（不可注入），
// 因此只允许 [-slack, +slack] 的偏差。
func assertAround(t *testing.T, label, field string, got, want, slack int64) {
	t.Helper()
	if got < want-slack || got > want+slack {
		t.Errorf("%s：%s = %d, want %d±%d", label, field, got, want, slack)
	}
}

// errWrap 让替身返回的错误在生产侧被 %w 包装后仍然 errors.Is 命中。
func errWrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("inbox/%s: %w", op, err)
}

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) countPrefix(prefix string) int {
	n := 0
	for _, o := range c.ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

func (c *callLog) snapshot() int             { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string { return c.ops[from:] }

// --- 错误注入 ---

type faultInjector struct{ by map[string]error }

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

func (f *faultInjector) fail(method string) error { return f.by[method] }

// --- 内存表：复刻 deploy/migrations/inbox/*.sql 的唯一键与状态语义 ---

type tables struct {
	log *callLog

	nextMsgID int64
	nextRowID int64
	msgs      map[int64]*model.InboxMessage
	byKey     map[string]int64
	rows      []*model.InboxUserMessage
	// stat[mid][category] = 未读快照；statMtime[mid] 是该用户四行快照的写入时间。
	stat      map[int64]map[int32]int64
	statMtime map[int64]int64

	// inTx 为真时写入登记逆操作，事务出错后逆序回滚（模拟「同一事务 all-or-nothing」）。
	inTx  bool
	undos []func()
}

func newTables(log *callLog) *tables {
	return &tables{
		log:       log,
		nextMsgID: 900,
		nextRowID: 5000,
		msgs:      map[int64]*model.InboxMessage{},
		byKey:     map[string]int64{},
		stat:      map[int64]map[int32]int64{},
		statMtime: map[int64]int64{},
	}
}

func (tb *tables) now() int64 { return time.Now().Unix() }

func (tb *tables) beginTx() {
	if tb.inTx {
		panic("inbox/logic/test: 替身不支持嵌套事务")
	}
	tb.inTx = true
	tb.undos = nil
}

func (tb *tables) commitTx() {
	tb.inTx = false
	tb.undos = nil
}

func (tb *tables) rollbackTx() {
	for i := len(tb.undos) - 1; i >= 0; i-- {
		tb.undos[i]()
	}
	tb.undos = nil
	tb.inTx = false
}

// undo 只在事务内登记；事务外的写入是自动提交的，回滚不了（与 MySQL 一致）。
func (tb *tables) undo(fn func()) {
	if tb.inTx {
		tb.undos = append(tb.undos, fn)
	}
}

// --- 静默布数据（纪律 4：不记轨迹、不查注入错误） ---

// seedMessage 布一条消息主体；MsgID 为 0 时用自增替身号。
func seedMessage(t *testing.T, st *store, msg *model.InboxMessage) *model.InboxMessage {
	t.Helper()
	cp := *msg
	if cp.MsgID == 0 {
		st.tb.nextMsgID++
		cp.MsgID = st.tb.nextMsgID
	}
	if cp.Ctime == 0 {
		cp.Ctime = st.tb.now()
	}
	if cp.IdempotencyKey == "" {
		cp.IdempotencyKey = fmt.Sprintf("seed-msg-%d", cp.MsgID)
	}
	if _, ok := st.tb.byKey[cp.IdempotencyKey]; ok {
		t.Fatalf("布景重复的 idempotency_key %q", cp.IdempotencyKey)
	}
	st.tb.msgs[cp.MsgID] = &cp
	st.tb.byKey[cp.IdempotencyKey] = cp.MsgID
	return st.getMessage(t, cp.MsgID)
}

// seedRow 布一行收件明细，并要求消息主体已存在（JOIN 语义）。
func seedRow(t *testing.T, st *store, row *model.InboxUserMessage) *model.InboxUserMessage {
	t.Helper()
	if _, ok := st.tb.msgs[row.MsgID]; !ok {
		t.Fatalf("布景收件明细引用了不存在的消息 %d", row.MsgID)
	}
	if _, dup := st.rowRef(row.Mid, row.MsgID); dup {
		t.Fatalf("布景违反 uniq(mid,msg_id)：%d/%d", row.Mid, row.MsgID)
	}
	cp := *row
	if cp.ID == 0 {
		st.tb.nextRowID++
		cp.ID = st.tb.nextRowID
	}
	if cp.Ctime == 0 {
		cp.Ctime = st.tb.now()
	}
	if cp.ReadState == 0 {
		cp.ReadState = model.ReadStateUnread
	}
	st.tb.rows = append(st.tb.rows, &cp)
	got, _ := st.rowRef(cp.Mid, cp.MsgID)
	return got
}

// seedStat 布某用户的分类快照（四行齐全，与 ReplaceByMid 口径一致）。
func seedStat(t *testing.T, st *store, mid int64, counts map[int32]int64, mtime int64) {
	t.Helper()
	snapshot := map[int32]int64{}
	for _, category := range model.AllCategories() {
		snapshot[category] = counts[category]
	}
	st.tb.stat[mid] = snapshot
	st.tb.statMtime[mid] = mtime
}

// rowRef 按 (mid,msg_id) 找库存行（返回的是库存指针，只在替身内部使用）。
func (st *store) rowRef(mid, msgID int64) (*model.InboxUserMessage, bool) {
	for _, r := range st.tb.rows {
		if r.Mid == mid && r.MsgID == msgID {
			return r, true
		}
	}
	return nil, false
}

func (st *store) getMessage(t *testing.T, msgID int64) *model.InboxMessage {
	t.Helper()
	row, ok := st.tb.msgs[msgID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (st *store) getRow(t *testing.T, mid, msgID int64) *model.InboxUserMessage {
	t.Helper()
	row, ok := st.rowRef(mid, msgID)
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// rowIDs 返回某用户当前的收件明细 msg_id（升序），用于「有没有多写一行」的断言。
func (st *store) rowIDs(mid int64) []int64 {
	out := make([]int64, 0, len(st.tb.rows))
	for _, r := range st.tb.rows {
		if r.Mid == mid {
			out = append(out, r.MsgID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// statOf 读某用户的分类快照副本；无记录返回 nil。
func (st *store) statOf(mid int64) map[int32]int64 {
	snapshot, ok := st.tb.stat[mid]
	if !ok {
		return nil
	}
	out := make(map[int32]int64, len(snapshot))
	for k, v := range snapshot {
		out[k] = v
	}
	return out
}

// --- 消息主体替身（model.InboxMessageModel） ---

type fakeMessageModel struct {
	faultInjector
	log *callLog
	tb  *tables
}

func (f *fakeMessageModel) InsertIdempotent(
	_ context.Context, session sqlx.Session, msg *model.InboxMessage,
) (int64, bool, error) {
	f.log.add("msg.InsertIdempotent:%s/%s", msg.IdempotencyKey, txScope(f.tb, session))
	if err := f.fail("InsertIdempotent"); err != nil {
		return 0, false, errWrap("msg.InsertIdempotent", err)
	}
	if msg.IdempotencyKey == "" {
		return 0, false, model.ErrInvalidIdempotencyKey
	}
	// uniq(idempotency_key)：命中已存在键时不写新行，返回首次那条的 msg_id。
	if msgID, ok := f.tb.byKey[msg.IdempotencyKey]; ok {
		if row, exist := f.tb.msgs[msgID]; exist {
			msg.MsgID, msg.Ctime, msg.State = msgID, row.Ctime, row.State
		}
		return msgID, false, nil
	}
	cp := *msg
	if cp.Ctime == 0 {
		cp.Ctime = f.tb.now()
	}
	cp.Mtime = f.tb.now()
	cp.State = model.MessageStateNormal
	f.tb.nextMsgID++
	cp.MsgID = f.tb.nextMsgID
	f.tb.msgs[cp.MsgID] = &cp
	f.tb.byKey[cp.IdempotencyKey] = cp.MsgID
	id := cp.MsgID
	key := cp.IdempotencyKey
	f.tb.undo(func() {
		delete(f.tb.msgs, id)
		delete(f.tb.byKey, key)
	})
	msg.MsgID, msg.Ctime, msg.State = id, cp.Ctime, cp.State
	return id, true, nil
}

func (f *fakeMessageModel) FindOne(_ context.Context, msgID int64) (*model.InboxMessage, error) {
	f.log.add("msg.FindOne:%d", msgID)
	if err := f.fail("FindOne"); err != nil {
		return nil, errWrap("msg.FindOne", err)
	}
	row, ok := f.tb.msgs[msgID]
	if !ok {
		return nil, nil // 与 model 一致：查无此行返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeMessageModel) Withdraw(_ context.Context, msgID int64) error {
	f.log.add("msg.Withdraw:%d", msgID)
	if err := f.fail("Withdraw"); err != nil {
		return errWrap("msg.Withdraw", err)
	}
	row, ok := f.tb.msgs[msgID]
	if !ok {
		return fmt.Errorf("inbox/logic/test: 撤回不存在的消息 %d", msgID)
	}
	prev := row.State
	row.State = model.MessageStateWithdrawn
	f.tb.undo(func() { row.State = prev })
	return nil
}

// --- 收件明细替身（model.InboxUserMessageModel） ---

type fakeUserMessageModel struct {
	faultInjector
	log *callLog
	tb  *tables
}

func (f *fakeUserMessageModel) InsertIdempotent(
	_ context.Context, session sqlx.Session, um *model.InboxUserMessage,
) (bool, error) {
	f.log.add("user.InsertIdempotent:%d/%d/%s", um.Mid, um.MsgID, txScope(f.tb, session))
	if err := f.fail("InsertIdempotent"); err != nil {
		return false, errWrap("user.InsertIdempotent", err)
	}
	for _, row := range f.tb.rows {
		if row.Mid == um.Mid && row.MsgID == um.MsgID {
			return false, nil // INSERT IGNORE 命中 uniq(mid,msg_id)：affected=0
		}
	}
	cp := *um
	if cp.Ctime == 0 {
		cp.Ctime = f.tb.now()
	}
	cp.Mtime = f.tb.now()
	if cp.ReadState == 0 {
		cp.ReadState = model.ReadStateUnread
	}
	f.tb.nextRowID++
	cp.ID = f.tb.nextRowID
	f.tb.rows = append(f.tb.rows, &cp)
	um.ID = cp.ID
	deleted := false
	f.tb.undo(func() {
		if deleted {
			return
		}
		deleted = true
		out := f.tb.rows[:0]
		for _, row := range f.tb.rows {
			if row.ID != cp.ID {
				out = append(out, row)
			}
		}
		f.tb.rows = out
	})
	return true, nil
}

func (f *fakeUserMessageModel) List(_ context.Context, filter model.ListFilter) ([]*model.MessageRow, error) {
	f.log.add("user.List:%d/%d/%v/%d/%d", filter.Mid, filter.Category, filter.UnreadOnly, filter.CursorTime, filter.Limit)
	if err := f.fail("List"); err != nil {
		return nil, errWrap("user.List", err)
	}
	if filter.Limit <= 0 {
		return nil, fmt.Errorf("inbox/logic/test: List 缺少 LIMIT（limit=%d）", filter.Limit)
	}
	type entry struct {
		row *model.InboxUserMessage
		msg *model.InboxMessage
	}
	var matched []entry
	for _, row := range f.tb.rows {
		msg, ok := f.tb.msgs[row.MsgID]
		if !ok || row.Mid != filter.Mid || row.DelState != model.DelStateNormal ||
			msg.State != model.MessageStateNormal {
			continue
		}
		if model.ValidCategory(filter.Category) && row.Category != filter.Category {
			continue
		}
		if filter.UnreadOnly && row.ReadState != model.ReadStateUnread {
			continue
		}
		if filter.CursorTime > 0 && !(row.Ctime < filter.CursorTime ||
			(row.Ctime == filter.CursorTime && row.ID < filter.CursorID)) {
			continue
		}
		matched = append(matched, entry{row: row, msg: msg})
	}
	// ORDER BY um.ctime DESC, um.id DESC
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].row.Ctime != matched[j].row.Ctime {
			return matched[i].row.Ctime > matched[j].row.Ctime
		}
		return matched[i].row.ID > matched[j].row.ID
	})
	if int32(len(matched)) > filter.Limit {
		matched = matched[:filter.Limit]
	}
	out := make([]*model.MessageRow, 0, len(matched))
	for _, e := range matched {
		out = append(out, &model.MessageRow{
			ID: e.row.ID, MsgID: e.row.MsgID, Mid: e.row.Mid, Category: e.row.Category,
			MsgType: e.msg.MsgType, Title: e.msg.Title, Content: e.msg.Content,
			SenderMid: e.msg.SenderMid, BizType: e.msg.BizType, BizID: e.msg.BizID,
			Extra: e.msg.Extra, ReadState: e.row.ReadState, DelState: e.row.DelState,
			Ctime: e.row.Ctime,
		})
	}
	return out, nil
}

func (f *fakeUserMessageModel) MarkReadBatch(
	_ context.Context, session sqlx.Session, mid int64, msgIDs []int64,
) (int64, error) {
	f.log.add("user.MarkReadBatch:%d/%s/%s", mid, idsTag(msgIDs), txScope(f.tb, session))
	if err := f.fail("MarkReadBatch"); err != nil {
		return 0, errWrap("user.MarkReadBatch", err)
	}
	return f.transition(mid, msgIDs, func(row *model.InboxUserMessage) bool {
		return row.ReadState == model.ReadStateUnread && row.DelState == model.DelStateNormal
	}, func(row *model.InboxUserMessage) { row.ReadState = model.ReadStateRead })
}

func (f *fakeUserMessageModel) MarkAllReadBatch(
	_ context.Context, session sqlx.Session, mid int64, category int32,
) (int64, error) {
	f.log.add("user.MarkAllReadBatch:%d/%d/%s", mid, category, txScope(f.tb, session))
	if err := f.fail("MarkAllReadBatch"); err != nil {
		return 0, errWrap("user.MarkAllReadBatch", err)
	}
	var changed int64
	for _, row := range f.tb.rows {
		if row.Mid != mid || row.ReadState != model.ReadStateUnread || row.DelState != model.DelStateNormal {
			continue
		}
		if model.ValidCategory(category) && row.Category != category {
			continue
		}
		prev := row.ReadState
		row.ReadState = model.ReadStateRead
		changed++
		r, p := row, prev
		f.tb.undo(func() { r.ReadState = p })
	}
	return changed, nil
}

func (f *fakeUserMessageModel) SoftDeleteBatch(
	_ context.Context, session sqlx.Session, mid int64, msgIDs []int64,
) (int64, error) {
	f.log.add("user.SoftDeleteBatch:%d/%s/%s", mid, idsTag(msgIDs), txScope(f.tb, session))
	if err := f.fail("SoftDeleteBatch"); err != nil {
		return 0, errWrap("user.SoftDeleteBatch", err)
	}
	// 删除即视为不再提醒：一并置为已读（与真实 SQL 的 SET del_state,read_state 一致）。
	return f.transition(mid, msgIDs, func(row *model.InboxUserMessage) bool {
		return row.DelState == model.DelStateNormal
	}, func(row *model.InboxUserMessage) {
		row.DelState = model.DelStateDeleted
		row.ReadState = model.ReadStateRead
	})
}

// transition 是「按 msg_id 集合推进状态」的公共实现，返回真实变更行数。
func (f *fakeUserMessageModel) transition(
	mid int64, msgIDs []int64,
	gate func(*model.InboxUserMessage) bool,
	apply func(*model.InboxUserMessage),
) (int64, error) {
	var changed int64
	for _, row := range f.tb.rows {
		if row.Mid != mid || !contains(msgIDs, row.MsgID) || !gate(row) {
			continue
		}
		before := *row
		apply(row)
		row.Mtime = f.tb.now()
		changed++
		f.tb.undo(func() {
			cp := before
			*row = cp
		})
	}
	return changed, nil
}

func (f *fakeUserMessageModel) CountOwned(_ context.Context, mid int64, msgIDs []int64) (int64, error) {
	f.log.add("user.CountOwned:%d/%s", mid, idsTag(msgIDs))
	if err := f.fail("CountOwned"); err != nil {
		return 0, errWrap("user.CountOwned", err)
	}
	var count int64
	for _, row := range f.tb.rows {
		if row.Mid == mid && contains(msgIDs, row.MsgID) {
			count++
		}
	}
	return count, nil
}

func (f *fakeUserMessageModel) CountUnreadByCategory(_ context.Context, mid int64) ([]model.CategoryCount, error) {
	f.log.add("user.CountUnreadByCategory:%d", mid)
	if err := f.fail("CountUnreadByCategory"); err != nil {
		return nil, errWrap("user.CountUnreadByCategory", err)
	}
	counts := map[int32]int64{}
	for _, row := range f.tb.rows {
		if row.Mid == mid && row.ReadState == model.ReadStateUnread && row.DelState == model.DelStateNormal {
			counts[row.Category]++
		}
	}
	// 真实 SQL 是 GROUP BY category，不出现的分类没有行；按分类升序返回保证轨迹可断言。
	out := make([]model.CategoryCount, 0, len(counts))
	for _, category := range model.AllCategories() {
		if n, ok := counts[category]; ok {
			out = append(out, model.CategoryCount{Category: category, Unread: n})
		}
	}
	return out, nil
}

// --- 未读快照替身（model.UnreadStatModel） ---

type fakeStatModel struct {
	faultInjector
	log *callLog
	tb  *tables
}

// ReplaceByMid 整体替换某用户的分类快照：四个分类都写（缺失记 0）。
func (f *fakeStatModel) ReplaceByMid(
	_ context.Context, session sqlx.Session, mid int64, counts map[int32]int64,
) error {
	f.log.add("stat.ReplaceByMid:%d/%s", mid, txScope(f.tb, session))
	if err := f.fail("ReplaceByMid"); err != nil {
		return errWrap("stat.ReplaceByMid", err)
	}
	prev, existed := f.tb.stat[mid], false
	if _, ok := f.tb.stat[mid]; ok {
		existed = true
		prev = make(map[int32]int64, len(f.tb.stat[mid]))
		for k, v := range f.tb.stat[mid] {
			prev[k] = v
		}
	}
	prevMtime := f.tb.statMtime[mid]
	snapshot := make(map[int32]int64, len(model.AllCategories()))
	for _, category := range model.AllCategories() {
		snapshot[category] = counts[category]
	}
	f.tb.stat[mid] = snapshot
	f.tb.statMtime[mid] = f.tb.now()
	f.tb.undo(func() {
		if existed {
			f.tb.stat[mid] = prev
			f.tb.statMtime[mid] = prevMtime
			return
		}
		delete(f.tb.stat, mid)
		delete(f.tb.statMtime, mid)
	})
	return nil
}

// IncrBy 复刻 GREATEST(unread+delta, 0) 与「新行取 max(delta,0)」。
func (f *fakeStatModel) IncrBy(
	_ context.Context, session sqlx.Session, mid int64, category int32, delta int64,
) error {
	f.log.add("stat.IncrBy:%d/%d/%d/%s", mid, category, delta, txScope(f.tb, session))
	if err := f.fail("IncrBy"); err != nil {
		return errWrap("stat.IncrBy", err)
	}
	if delta == 0 {
		return nil
	}
	snapshot, existed := f.tb.stat[mid]
	if !existed {
		snapshot = map[int32]int64{}
		f.tb.stat[mid] = snapshot
	}
	prev, hadCategory := snapshot[category]
	next := prev + delta
	if !hadCategory {
		next = delta
	}
	if next < 0 {
		next = 0
	}
	snapshot[category] = next
	prevMtime := f.tb.statMtime[mid]
	f.tb.statMtime[mid] = f.tb.now()
	f.tb.undo(func() {
		if !existed {
			delete(f.tb.stat, mid)
		} else if hadCategory {
			snapshot[category] = prev
		} else {
			delete(snapshot, category)
		}
		f.tb.statMtime[mid] = prevMtime
	})
	return nil
}

func (f *fakeStatModel) ListByMid(_ context.Context, mid int64) ([]model.UnreadStat, error) {
	f.log.add("stat.ListByMid:%d", mid)
	if err := f.fail("ListByMid"); err != nil {
		return nil, errWrap("stat.ListByMid", err)
	}
	snapshot, ok := f.tb.stat[mid]
	if !ok {
		return nil, nil
	}
	out := make([]model.UnreadStat, 0, len(snapshot))
	for _, category := range model.AllCategories() {
		n, has := snapshot[category]
		if !has {
			continue
		}
		out = append(out, model.UnreadStat{Mid: mid, Category: category, Unread: n, Mtime: f.tb.statMtime[mid]})
	}
	return out, nil
}

func (f *fakeStatModel) DeleteByMid(_ context.Context, mid int64) error {
	f.log.add("stat.DeleteByMid:%d", mid)
	if err := f.fail("DeleteByMid"); err != nil {
		return errWrap("stat.DeleteByMid", err)
	}
	prev, existed := f.tb.stat[mid]
	prevMtime := f.tb.statMtime[mid]
	delete(f.tb.stat, mid)
	delete(f.tb.statMtime, mid)
	f.tb.undo(func() {
		if existed {
			f.tb.stat[mid] = prev
			f.tb.statMtime[mid] = prevMtime
		}
	})
	return nil
}

// --- 消费侧替身：不在 logic 路径上，被调用即报错（不做永不失败的替身） ---

// errUnusedDependency 表示 logic 的用例走在了消费侧依赖上：这要么是生产装配变了，
// 要么是用例越界，必须红，而不是静默通过。
var errUnusedDependency = errors.New("inbox/logic/test: 消费侧依赖被 logic 路径调用")

type fakeOffsetModel struct {
	log *callLog
}

func (f *fakeOffsetModel) unusable(op string) error {
	f.log.add("offset.%s", op)
	return errWrap("offset."+op, errUnusedDependency)
}

func (f *fakeOffsetModel) Claim(context.Context, *model.ConsumerOffset, int64) (model.ClaimOutcome, *model.ConsumerOffset, error) {
	return 0, nil, f.unusable("Claim")
}

func (f *fakeOffsetModel) MarkSucceeded(context.Context, string) error {
	return f.unusable("MarkSucceeded")
}

func (f *fakeOffsetModel) MarkRetry(context.Context, string, int64, string, string) error {
	return f.unusable("MarkRetry")
}

func (f *fakeOffsetModel) MarkDeadLetter(context.Context, string, string, string) error {
	return f.unusable("MarkDeadLetter")
}

func (f *fakeOffsetModel) FindOne(context.Context, string) (*model.ConsumerOffset, error) {
	return nil, f.unusable("FindOne")
}

func (f *fakeOffsetModel) ListOverdueRetry(context.Context, int64, int32) ([]*model.ConsumerOffset, error) {
	return nil, f.unusable("ListOverdueRetry")
}

type fakeDlqModel struct {
	log *callLog
}

func (f *fakeDlqModel) unusable(op string) error {
	f.log.add("dlq.%s", op)
	return errWrap("dlq."+op, errUnusedDependency)
}

func (f *fakeDlqModel) InsertIdempotent(context.Context, *model.DeadLetter) (bool, error) {
	return false, f.unusable("InsertIdempotent")
}

func (f *fakeDlqModel) ListOpen(context.Context, int32) ([]*model.DeadLetter, error) {
	return nil, f.unusable("ListOpen")
}

func (f *fakeDlqModel) MarkState(context.Context, int64, string) error {
	return f.unusable("MarkState")
}

// --- Redis 未读加速层替身（repository.UnreadCache） ---

// fakeCache 复刻「一个 mid 一个 hash，字段是四个分类」的语义。
// Get 没有 error 返回值：Redis 故障在接口上就表现为 miss，读路径降级回源，
// 因此 unavailable 只是让布景能模拟「缓存不可用」这一事实。
type fakeCache struct {
	faultInjector
	log  *callLog
	data map[int64]map[int32]int64
	// unavailable 为真时 Get 直接 miss（记轨迹，证明降级是读回源而不是报错）。
	unavailable bool
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{log: log, data: map[int64]map[int32]int64{}}
}

func (f *fakeCache) Get(_ context.Context, mid int64) (map[int32]int64, bool) {
	f.log.add("cache.Get:%d", mid)
	if f.unavailable {
		return nil, false
	}
	snapshot, ok := f.data[mid]
	if !ok {
		return nil, false
	}
	return copySnapshot(snapshot), true
}

func (f *fakeCache) Set(_ context.Context, mid int64, snapshot map[int32]int64) {
	f.log.add("cache.Set:%d/%d", mid, model.SnapshotTotal(snapshot))
	if err := f.fail("Set"); err != nil {
		f.log.add("cache.SetFailed:%v", err)
		return // Redis 写失败不得影响读路径（真实实现只记日志）
	}
	f.data[mid] = copySnapshot(snapshot)
}

func (f *fakeCache) Invalidate(_ context.Context, mids ...int64) {
	parts := make([]string, 0, len(mids))
	for _, mid := range mids {
		delete(f.data, mid)
		parts = append(parts, fmt.Sprint(mid))
	}
	f.log.add("cache.Invalidate:%s", strings.Join(parts, "+"))
}

func (f *fakeCache) Ping(context.Context) error {
	f.log.add("cache.Ping")
	return f.fail("Ping")
}

// warm 静默预热快照（布数据，不写轨迹：见纪律 4）。
func (f *fakeCache) warm(mid int64, snapshot map[int32]int64) {
	f.data[mid] = copySnapshot(snapshot)
}

// cached 读缓存副本；miss 返回 nil。
func (f *fakeCache) cached(mid int64) map[int32]int64 {
	snapshot, ok := f.data[mid]
	if !ok {
		return nil
	}
	return copySnapshot(snapshot)
}

func copySnapshot(snapshot map[int32]int64) map[int32]int64 {
	out := make(map[int32]int64, len(snapshot))
	for k, v := range snapshot {
		out[k] = v
	}
	return out
}

// --- 事务连接替身 ---

// txSession 只是「拿到了事务会话」的凭证；替身用它区分 tx / conn 两种作用域。
type txSession struct{ sqlx.Session }

// txScope 把「语句跑在事务内还是自动提交」编码进轨迹后缀，
// 于是 wantOps 就能钉住「明细与快照同事务」这条不变量。
func txScope(tb *tables, session sqlx.Session) string {
	if session != nil {
		if !tb.inTx {
			panic("inbox/logic/test: 事务会话出现在事务外")
		}
		return "tx"
	}
	return "conn"
}

// fakeConn 只实现 inbox 用到的 TransactCtx，并把事务边界交给 tables 的回滚簿记。
// 其它方法落在嵌入的 nil 接口上会直接 panic，等价于「logic 单测路径上不允许出现任何直连 SQL」。
type fakeConn struct {
	sqlx.SqlConn
	tb  *tables
	log *callLog
}

func (c *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.log.add("conn.TransactCtx")
	c.tb.beginTx()
	err := fn(ctx, &txSession{})
	if err != nil {
		c.tb.rollbackTx()
		return err
	}
	c.tb.commitTx()
	return nil
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// --- 装配 ---

type store struct {
	log   *callLog
	tb    *tables
	cache *fakeCache
	conn  *fakeConn
	msgs  *fakeMessageModel
	users *fakeUserMessageModel
	stats *fakeStatModel
	repo  *repository.Repository
}

// testConf 与 services/inbox/etc/inbox.yaml 的量级一致，但把上限压到能被小表用例打到：
// MaxRecipients=3、MaxContentBytes=64、MaxPageSize=50、PageSize=20。
func testConf() config.InboxConf {
	return config.InboxConf{
		PageSize: 20, MaxPageSize: 50, MaxRecipients: 3,
		UnreadCacheSeconds: 1800, MaxContentBytes: 64,
	}
}

func newStore(conf config.InboxConf) *store {
	log := &callLog{}
	tb := newTables(log)
	st := &store{
		log:   log,
		tb:    tb,
		cache: newFakeCache(log),
		conn:  &fakeConn{tb: tb, log: log},
		msgs:  &fakeMessageModel{log: log, tb: tb},
		users: &fakeUserMessageModel{log: log, tb: tb},
		stats: &fakeStatModel{log: log, tb: tb},
	}
	st.repo = repository.NewWithDeps(st.conn, st.cache, conf,
		st.msgs, st.users, st.stats, &fakeOffsetModel{log: log}, &fakeDlqModel{log: log})
	return st
}

// env 是一次用例的完整运行时：真实 Repository（依赖为替身）+ ServiceContext。
// Config 其余字段留空零值即可——7 个 logic 只读 Repository。
type env struct {
	t      *testing.T
	st     *store
	svcCtx *svc.ServiceContext
}

func newEnv(t *testing.T) *env { return newEnvConf(t, testConf()) }

func newEnvConf(t *testing.T, conf config.InboxConf) *env {
	t.Helper()
	st := newStore(conf)
	return &env{t: t, st: st, svcCtx: &svc.ServiceContext{Repository: st.repo}}
}

// --- 布景小工具 ---

// idsTag 把 msg_id 列表变成可断言的稳定字符串（升序无关，按传入顺序）。
func idsTag(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprint(id))
	}
	return strings.Join(parts, "+")
}

func contains(ids []int64, v int64) bool {
	for _, id := range ids {
		if id == v {
			return true
		}
	}
	return false
}

// 测试用的固定 mid：辨识度足够高，避免与替身自增出来的 id 混淆。
const (
	midAlice = int64(91001)
	midBob   = int64(91002)
	midCarol = int64(91003)
)

func nowUnix() int64 { return time.Now().Unix() }
