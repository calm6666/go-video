// 本文件是 private-message logic 包的测试替身（内存库 + 事务探针 + 日志探针）。
//
// 它存在的前提是本服务的可测性设计：model 全是接口、事务入口只有 sqlx.SqlConn.TransactCtx、
// 参与者授权只有 requireMembership 一处。于是这些真正的风险点都能在不连 MySQL、
// 不起 Redis 的前提下被证明：
//   - 越权调用不留副作用（写了哪些行、读了哪些行都可数）；
//   - 发送的「消息行 + 会话指针 + 未读投影」是否落在同一个事务里；
//   - 事务失败后是否留下幽灵行。
//
// 关键手法：
//   - 六个 fake 各自复刻真 model 的 SQL 语义（唯一键回放、CAS 的 WHERE、GREATEST 下界、
//     RowsAffected 的「实际改变行数」口径），而不是「一律返回成功」：
//     替身比被测宽松 = 测试通过但生产出错。
//   - 表用**值类型 map** 存行，读写都拷一份，因此 logic 拿到的指针指向的是副本，
//     「回滚后行没被改」才是可证的。
//   - 没有实现的路径一律 panic（不是返回零值），避免用例悄悄走到假成功。
//
// 一处诚实的限制：svc.ServiceContext.Cache 是具体类型 *redis.Redis，进程内无法替换成假实现，
// 因此「缓存键被不被碰」不能直接观测。本套替身的做法是：
//   - 绝大多数用例把 Cache 置 nil，helper（cacheGetJSON/cacheSetJSON/incrCounter）在
//     Cache==nil 时直接短路，这本身就是 README 记的那条 fail-closed 分支
//     （计数器取不到即拒发）；键格式与命名空间改由 unread_test.go 的
//     TestUnreadCacheKeyIsNamespaced 按纯函数钉住；
//   - 需要区分「没配缓存」与「配了但挂了」时，用 deadCache() 指向一个必然拒绝连接的
//     回环地址（127.0.0.1:1）并配 shortCtx() 截止点，不会挂住测试；
//   - 缓存「命中并回放」这一分支至今不可测：包里没有进程内 redis server，
//     引入 miniredis 属于新增第三方依赖，本轮不做（unread_test.go 里以注释钉住这条边界）。

package logic

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go-video/common/ratelimit"
	"go-video/services/private-message/internal/config"
	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func bg() context.Context { return context.Background() }

// wantErr 断言错误链里含目标哨兵。契约用 errors.Is 表达，不用文案，
// 否则改一句提示语就会「测试失败但其实没错」。
func wantErr(t *testing.T, err error, target error, label string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s：期望错误 %v，实得 %v", label, target, err)
	}
}

// wantFail 除了哨兵之外还要求错误非 nil：nil 错误 + 零值响应就是「伪造成功」，
// 是本域最需要被测试挡住的一类通过（未配置门禁当成门禁通过）。
func wantFail(t *testing.T, err error, target error, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：期望失败 %v，实得成功（伪造成功是最坏的一类通过）", label, target)
	}
	wantErr(t, err, target, label)
}

func wantOK[T any](t *testing.T, got T, err error, label string) T {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：意外失败：%v", label, err)
	}
	return got
}

// --- 内存库 ---

type store struct {
	convs    map[int64]model.Conversation
	members  map[string]model.ConversationMember
	msgs     map[int64]model.Message
	settings map[int64]model.UserSetting
	reports  map[int64]model.Report
	wlogs    map[int64]model.WithdrawLog

	nextID int64

	// calls 记录每个 model 方法被调了几次：越权调用必须「一次内容读都没发生」。
	calls map[string]int
	// rowsLoaded 记录每个读接口「真正物化进内存的行数」（不是调用次数）。
	// 两者必须分开：「消息不存在」这类被拒调用天然要查一次才知道不存在，
	// 但它一行都没取到，用调用次数当证据会逼测试放过读取、或用行数当证据才会漏掉真缺陷。
	rowsLoaded map[string]int
	// txs 是 TransactCtx 次数：发送路径的契约是「恰好一个事务」。
	txs int
	// sessionWrites / plainWrites 记录带 session 参数的写方法是在事务内还是事务外被调用。
	// 「消息行、会话指针、未读投影分两个事务提交」这类缺陷只能这样被钉住：
	// 光看最终数据状态是对的，事务边界错了要等到崩溃窗口才暴露。
	sessionWrites []string
	plainWrites   []string
	// failTx 注入事务内某一步失败（模拟 MySQL 抖动 / 死锁）。
	// 读方法同样支持：logic 的「依赖错误原样上抛」只有在能主动制造下游错误时才测得出来。
	failTx map[string]error
	// hideSettingRow 让 Settings.FindByMid 恒回 (nil,nil)：
	// 用于造「Upsert 写成功了但回查不到」这种只有真库才会出现的现场
	// （内存表写完必然在，缺了这一钩子那条防御分支就永远测不到）。
	hideSettingRow bool
}

func newStore() *store {
	return &store{
		convs:      map[int64]model.Conversation{},
		members:    map[string]model.ConversationMember{},
		msgs:       map[int64]model.Message{},
		settings:   map[int64]model.UserSetting{},
		reports:    map[int64]model.Report{},
		wlogs:      map[int64]model.WithdrawLog{},
		nextID:     1000,
		calls:      map[string]int{},
		rowsLoaded: map[string]int{},
		failTx:     map[string]error{},
	}
}

func memberKey(conversationID, mid int64) string { return fmt.Sprintf("%d|%d", conversationID, mid) }

func (st *store) tick(name string) { st.calls[name]++ }

// loadRows 记账一次读取真正取到的行数（0 也要记，表示「查过但没查到」）。
func (st *store) loadRows(name string, n int) { st.rowsLoaded[name] += n }

// call 计数并回 err，让写方法的错误分支也能一行返回。
func (st *store) call(name string, err error) error {
	st.tick(name)
	return err
}

func (st *store) id() int64 { st.nextID++; return st.nextID }

// recordWrite 记录一次「带 session 语义」的写落在哪一侧。
func (st *store) recordWrite(name string, session sqlx.Session) {
	if session != nil {
		st.sessionWrites = append(st.sessionWrites, name)
		return
	}
	st.plainWrites = append(st.plainWrites, name)
}

func (st *store) failIf(name string) error { return st.failTx[name] }

type snapshot struct {
	convs    map[int64]model.Conversation
	members  map[string]model.ConversationMember
	msgs     map[int64]model.Message
	settings map[int64]model.UserSetting
	reports  map[int64]model.Report
	wlogs    map[int64]model.WithdrawLog
	nextID   int64
}

func (st *store) snapshot() snapshot {
	snap := snapshot{
		convs:    make(map[int64]model.Conversation, len(st.convs)),
		members:  make(map[string]model.ConversationMember, len(st.members)),
		msgs:     make(map[int64]model.Message, len(st.msgs)),
		settings: make(map[int64]model.UserSetting, len(st.settings)),
		reports:  make(map[int64]model.Report, len(st.reports)),
		wlogs:    make(map[int64]model.WithdrawLog, len(st.wlogs)),
		nextID:   st.nextID,
	}
	for k, v := range st.convs {
		snap.convs[k] = v
	}
	for k, v := range st.members {
		snap.members[k] = v
	}
	for k, v := range st.msgs {
		snap.msgs[k] = cloneMessage(v)
	}
	for k, v := range st.settings {
		snap.settings[k] = v
	}
	for k, v := range st.reports {
		snap.reports[k] = v
	}
	for k, v := range st.wlogs {
		snap.wlogs[k] = v
	}
	return snap
}

func (st *store) restore(s snapshot) {
	st.convs, st.members, st.settings, st.reports, st.wlogs = s.convs, s.members, s.settings, s.reports, s.wlogs
	st.nextID = s.nextID
	st.msgs = make(map[int64]model.Message, len(s.msgs))
	for k, v := range s.msgs {
		st.msgs[k] = cloneMessage(v)
	}
}

// cloneMessage 深拷贝密文列：[]byte 是整张表里唯一的引用类型，
// 不拷就等于把 live 底层数组交给 logic，「改了也不影响库」的断言会失效。
func cloneMessage(v model.Message) model.Message {
	if v.ContentCipher != nil {
		cp := make([]byte, len(v.ContentCipher))
		copy(cp, v.ContentCipher)
		v.ContentCipher = cp
	}
	return v
}

// --- 种子数据 ---

func (st *store) putConv(c model.Conversation) model.Conversation {
	if c.ConversationID == 0 {
		c.ConversationID = st.id()
	}
	if c.State == 0 {
		c.State = model.ConversationStateNormal
	}
	if c.PairKey == "" {
		c.PairKey = model.PairKey(c.UserA, c.UserB)
	}
	st.convs[c.ConversationID] = c
	return c
}

func (st *store) putMember(m model.ConversationMember) model.ConversationMember {
	key := memberKey(m.ConversationID, m.Mid)
	if _, ok := st.members[key]; !ok {
		m.ID = st.id()
	}
	st.members[key] = m
	return m
}

func (st *store) putMsg(m model.Message) model.Message {
	if m.MsgID == 0 {
		m.MsgID = st.id()
	}
	st.msgs[m.MsgID] = cloneMessage(m)
	return st.msgs[m.MsgID]
}

func (st *store) putSetting(s model.UserSetting) model.UserSetting {
	st.settings[s.Mid] = s
	return s
}

func (st *store) putReport(r model.Report) model.Report {
	if r.ReportID == 0 {
		r.ReportID = st.id()
	}
	st.reports[r.ReportID] = r
	return r
}

func (st *store) convRow(id int64) (model.Conversation, bool) {
	c, ok := st.convs[id]
	return c, ok
}

func (st *store) memberRow(conversationID, mid int64) (model.ConversationMember, bool) {
	m, ok := st.members[memberKey(conversationID, mid)]
	return m, ok
}

func (st *store) msgRow(id int64) (model.Message, bool) {
	m, ok := st.msgs[id]
	if !ok {
		return m, false
	}
	return cloneMessage(m), true
}

// --- 六个 model 替身 ---

type fakeConversations struct{ st *store }
type fakeMembers struct{ st *store }
type fakeMessages struct{ st *store }
type fakeSettings struct{ st *store }
type fakeReports struct {
	st *store
	// listByCursorArgs 记下 ListByCursor 的实参序列（见 listByCursorArg）。
	// 只有 Reports 这一个替身需要记实参：举报台账是本页唯一的「过滤条件全靠入参」的读接口，
	// 四个位置（state/target_mid/cursor/ps）搬错一位，投影仍是一页像样的行，只有实参能证伪。
	listByCursorArgs []listByCursorArg
}
type fakeWithdrawLogs struct{ st *store }

// listByCursorArg 是 model.ReportModel.ListByCursor 的一次实参快照。
type listByCursorArg struct {
	state, targetMid, cursorID int64
	ps                         int32
}

var (
	_ model.ConversationModel       = (*fakeConversations)(nil)
	_ model.ConversationMemberModel = (*fakeMembers)(nil)
	_ model.MessageModel            = (*fakeMessages)(nil)
	_ model.UserSettingModel        = (*fakeSettings)(nil)
	_ model.ReportModel             = (*fakeReports)(nil)
	_ model.WithdrawLogModel        = (*fakeWithdrawLogs)(nil)
)

func (f *fakeConversations) FindOrCreate(ctx context.Context, pairKey string, userA, userB int64) (*model.Conversation, bool, error) {
	return f.FindOrCreateInTx(ctx, nil, pairKey, userA, userB)
}

// FindOrCreateInTx 复刻 ON DUPLICATE KEY UPDATE conversation_id = conversation_id：
// 命中唯一键时 affected=0 ⇒ created=false，且永远回查 pair_key 而不是 LastInsertId。
func (f *fakeConversations) FindOrCreateInTx(ctx context.Context, session sqlx.Session,
	pairKey string, userA, userB int64) (*model.Conversation, bool, error) {
	f.st.tick("Conversations.FindOrCreateInTx")
	f.st.recordWrite("Conversations.FindOrCreateInTx", session)
	if err := f.st.failIf("Conversations.FindOrCreateInTx"); err != nil {
		return nil, false, err
	}
	for _, c := range f.st.convs {
		if c.PairKey == pairKey {
			cp := c
			return &cp, false, nil
		}
	}
	now := timeNowUnix()
	row := model.Conversation{
		ConversationID: f.st.id(), PairKey: pairKey, UserA: userA, UserB: userB,
		State: model.ConversationStateNormal, CreatedAt: now, UpdatedAt: now,
	}
	f.st.convs[row.ConversationID] = row
	cp := row
	return &cp, true, nil
}

func (f *fakeConversations) FindByID(ctx context.Context, conversationID int64) (*model.Conversation, error) {
	f.st.tick("Conversations.FindByID")
	c, ok := f.st.convRow(conversationID)
	if !ok {
		return nil, nil
	}
	cp := c
	return &cp, nil
}

func (f *fakeConversations) FindByPairKey(ctx context.Context, pairKey string) (*model.Conversation, error) {
	f.st.tick("Conversations.FindByPairKey")
	if pairKey == "" {
		return nil, model.ErrConversationNotFound
	}
	for _, c := range f.st.convs {
		if c.PairKey == pairKey {
			cp := c
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeConversations) FindByIDs(ctx context.Context, ids []int64) (map[int64]*model.Conversation, error) {
	f.st.tick("Conversations.FindByIDs")
	if err := f.st.failIf("Conversations.FindByIDs"); err != nil {
		return nil, err
	}
	out := make(map[int64]*model.Conversation, len(ids))
	for _, id := range ids {
		if c, ok := f.st.convRow(id); ok {
			cp := c
			out[id] = &cp
		}
	}
	return out, nil
}

// AllocateSeq 复刻「session 必填」这条硬约束：行锁是 seq 唯一的分配者，
// 事务外分配出来的 seq 没有任何东西保证不被第二个发送者同时拿到。
func (f *fakeConversations) AllocateSeq(ctx context.Context, session sqlx.Session, conversationID int64) (int64, error) {
	f.st.tick("Conversations.AllocateSeq")
	if session == nil {
		return 0, errors.New("pm_conversation AllocateSeq: session required (must run in transaction)")
	}
	c, ok := f.st.convRow(conversationID)
	if !ok {
		return 0, model.ErrConversationNotFound
	}
	c.LastSeq++
	c.UpdatedAt = timeNowUnix()
	f.st.convs[conversationID] = c
	return c.LastSeq, nil
}

func (f *fakeConversations) TouchLastMessage(ctx context.Context, session sqlx.Session,
	conversationID, msgID, seq, msgTime int64) error {
	f.st.tick("Conversations.TouchLastMessage")
	f.st.recordWrite("Conversations.TouchLastMessage", session)
	if err := f.st.failIf("Conversations.TouchLastMessage"); err != nil {
		return err
	}
	c, ok := f.st.convRow(conversationID)
	if !ok {
		return nil
	}
	if c.LastSeq >= seq { // 只前进不后退
		return nil
	}
	c.LastSeq, c.LastMsgID, c.LastMsgTime, c.UpdatedAt = seq, msgID, msgTime, timeNowUnix()
	f.st.convs[conversationID] = c
	return nil
}

func (f *fakeConversations) UpdateState(ctx context.Context, conversationID int64, state int32) (*model.Conversation, error) {
	f.st.tick("Conversations.UpdateState")
	c, ok := f.st.convRow(conversationID)
	if !ok {
		return nil, nil
	}
	c.State = state
	f.st.convs[conversationID] = c
	cp := c
	return &cp, nil
}

func (f *fakeMembers) Ensure(ctx context.Context, session sqlx.Session, rows []*model.ConversationMember) error {
	f.st.tick("Members.Ensure")
	f.st.recordWrite("Members.Ensure", session)
	if err := f.st.failIf("Members.Ensure"); err != nil {
		return err
	}
	for _, r := range rows {
		if r == nil {
			return errors.New("pm_conversation_member Ensure: nil row")
		}
		key := memberKey(r.ConversationID, r.Mid)
		if cur, ok := f.st.members[key]; ok {
			// ON DUPLICATE KEY UPDATE updated_at = VALUES(updated_at)：其余列一律不动。
			cur.UpdatedAt = timeNowUnix()
			f.st.members[key] = cur
			continue
		}
		row := *r
		row.ID = f.st.id()
		row.CreatedAt = timeNowUnix()
		row.UpdatedAt = timeNowUnix()
		f.st.members[key] = row
	}
	return nil
}

func (f *fakeMembers) Find(ctx context.Context, conversationID, mid int64) (*model.ConversationMember, error) {
	f.st.tick("Members.Find")
	m, ok := f.st.memberRow(conversationID, mid)
	if !ok {
		return nil, nil
	}
	cp := m
	return &cp, nil
}

func (f *fakeMembers) FindByPeer(ctx context.Context, mid, peerMid int64) (*model.ConversationMember, error) {
	f.st.tick("Members.FindByPeer")
	for _, m := range f.st.members {
		if m.Mid == mid && m.PeerMid == peerMid {
			cp := m
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeMembers) ListByMid(ctx context.Context, opt model.ListMembersOptions) ([]*model.ConversationMember, error) {
	f.st.tick("Members.ListByMid")
	if err := f.st.failIf("Members.ListByMid"); err != nil {
		return nil, err
	}
	if opt.PageSize <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []model.ConversationMember
	for _, m := range f.st.members {
		if m.Mid != opt.Mid {
			continue
		}
		if !opt.IncludeHidden && m.HideState != model.HideStateNormal {
			continue
		}
		if opt.OnlyUnread && m.UnreadCount <= 0 {
			continue
		}
		if opt.CursorTime > 0 && !(m.LastMsgTime < opt.CursorTime ||
			(m.LastMsgTime == opt.CursorTime && m.ID < opt.CursorID)) {
			continue
		}
		rows = append(rows, m)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].LastMsgTime != rows[j].LastMsgTime {
			return rows[i].LastMsgTime > rows[j].LastMsgTime
		}
		return rows[i].ID > rows[j].ID
	})
	if int32(len(rows)) > opt.PageSize {
		rows = rows[:opt.PageSize]
	}
	out := make([]*model.ConversationMember, 0, len(rows))
	for _, r := range rows {
		cp := r
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeMembers) ListPeersByMid(ctx context.Context, mid int64, peerMids []int64) (map[int64]*model.ConversationMember, error) {
	f.st.tick("Members.ListPeersByMid")
	out := map[int64]*model.ConversationMember{}
	want := map[int64]bool{}
	for _, p := range peerMids {
		want[p] = true
	}
	for _, m := range f.st.members {
		if m.Mid == mid && want[m.PeerMid] {
			cp := m
			out[m.PeerMid] = &cp
		}
	}
	return out, nil
}

func (f *fakeMembers) ListByConversation(ctx context.Context, conversationID int64) ([]*model.ConversationMember, error) {
	f.st.tick("Members.ListByConversation")
	var out []*model.ConversationMember
	for _, m := range f.st.members {
		if m.ConversationID == conversationID {
			cp := m
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mid < out[j].Mid })
	return out, nil
}

// ApplyIncoming 复刻两条 UPDATE 的 WHERE last_seq < ? 与 GREATEST 下界。
func (f *fakeMembers) ApplyIncoming(ctx context.Context, session sqlx.Session,
	conversationID, senderMid, receiverMid, msgID, seq int64, msgType int32, preview string, msgTime int64) error {
	f.st.tick("Members.ApplyIncoming")
	f.st.recordWrite("Members.ApplyIncoming", session)
	if err := f.st.failIf("Members.ApplyIncoming"); err != nil {
		return err
	}
	// 接收方：投影前移 + 未读 +1（UPDATE 语义：缺行不新增，静默丢失）。
	if key := memberKey(conversationID, receiverMid); true {
		if m, ok := f.st.members[key]; ok && m.LastSeq < seq {
			m.LastSeq, m.LastMsgID, m.LastMsgType, m.LastPreview = seq, msgID, msgType, preview
			m.LastMsgTime, m.HideState = msgTime, model.HideStateNormal
			if m.UnreadCount+1 > 1 {
				m.UnreadCount++
			} else {
				m.UnreadCount = 1
			}
			m.UpdatedAt = timeNowUnix()
			f.st.members[key] = m
		}
	}
	if key := memberKey(conversationID, senderMid); true {
		if m, ok := f.st.members[key]; ok && m.LastSeq < seq {
			m.LastSeq, m.LastMsgID, m.LastMsgType, m.LastPreview = seq, msgID, msgType, preview
			m.LastMsgTime, m.ReadSeq, m.UnreadCount, m.HideState = msgTime, seq, 0, model.HideStateNormal
			m.UpdatedAt = timeNowUnix()
			f.st.members[key] = m
		}
	}
	return nil
}

func (f *fakeMembers) MarkRead(ctx context.Context, conversationID, mid, readSeq int64) (bool, *model.ConversationMember, error) {
	f.st.tick("Members.MarkRead")
	if err := f.st.failIf("Members.MarkRead"); err != nil {
		return false, nil, err
	}
	key := memberKey(conversationID, mid)
	m, ok := f.st.memberRow(conversationID, mid)
	applied := false
	if ok && m.ReadSeq < readSeq { // WHERE read_seq < ?
		m.ReadSeq, m.UnreadCount, m.UpdatedAt = readSeq, 0, timeNowUnix()
		f.st.members[key] = m
		applied = true
	}
	if !ok {
		return false, nil, nil
	}
	cp := m
	return applied, &cp, nil
}

// SetHidden 复刻「UPDATE ... WHERE (conversation_id, mid)」：
// 无此成员行即拒绝（越权在这一条 UPDATE 内被证明）。
// 命中成员行但值没变（同秒重复隐藏，真库 RowsAffected=0，见 model 注释）按幂等成功处理，
// 这一条依赖 model 侧的修复；model 未修复前它会返回 ErrNotConversationMember，
// hide_conversation_idempotent 用例即钉住这个差别。
func (f *fakeMembers) SetHidden(ctx context.Context, conversationID, mid int64, hidden bool) error {
	f.st.tick("Members.SetHidden")
	state := model.HideStateNormal
	if hidden {
		state = model.HideStateHidden
	}
	m, ok := f.st.memberRow(conversationID, mid)
	if !ok {
		return model.ErrNotConversationMember
	}
	m.HideState, m.UpdatedAt = state, timeNowUnix()
	f.st.members[memberKey(conversationID, mid)] = m
	return nil
}

func (f *fakeMembers) DecrementUnreadIfUnread(ctx context.Context, session sqlx.Session, conversationID, seq int64) error {
	f.st.tick("Members.DecrementUnreadIfUnread")
	f.st.recordWrite("Members.DecrementUnreadIfUnread", session)
	if err := f.st.failIf("Members.DecrementUnreadIfUnread"); err != nil {
		return err
	}
	for key, m := range f.st.members {
		if m.ConversationID != conversationID {
			continue
		}
		if m.ReadSeq < seq && m.UnreadCount > 0 {
			m.UnreadCount--
			if m.UnreadCount < 0 {
				m.UnreadCount = 0
			}
			m.UpdatedAt = timeNowUnix()
			f.st.members[key] = m
		}
	}
	return nil
}

func (f *fakeMembers) RefreshPreview(ctx context.Context, session sqlx.Session, conversationID, msgID int64, preview string) error {
	f.st.tick("Members.RefreshPreview")
	f.st.recordWrite("Members.RefreshPreview", session)
	if err := f.st.failIf("Members.RefreshPreview"); err != nil {
		return err
	}
	for key, m := range f.st.members {
		if m.ConversationID == conversationID && m.LastMsgID == msgID {
			m.LastPreview, m.UpdatedAt = preview, timeNowUnix()
			f.st.members[key] = m
		}
	}
	return nil
}

func (f *fakeMembers) RebuildProjection(ctx context.Context, conversationID, mid int64, p model.MemberProjection) error {
	f.st.tick("Members.RebuildProjection")
	key := memberKey(conversationID, mid)
	m, ok := f.st.members[key]
	if !ok {
		return nil
	}
	m.ReadSeq, m.UnreadCount, m.LastSeq, m.LastMsgID = p.ReadSeq, p.UnreadCount, p.LastSeq, p.LastMsgID
	m.LastMsgType, m.LastPreview, m.LastMsgTime, m.UpdatedAt = p.LastMsgType, p.LastPreview, p.LastMsgTime, timeNowUnix()
	f.st.members[key] = m
	return nil
}

func (f *fakeMembers) SumUnread(ctx context.Context, mid int64, includeHidden bool) (int64, int64, error) {
	f.st.tick("Members.SumUnread")
	if err := f.st.failIf("Members.SumUnread"); err != nil {
		return 0, 0, err
	}
	var total, conversations int64
	for _, m := range f.st.members {
		if m.Mid != mid || m.UnreadCount <= 0 {
			continue
		}
		if !includeHidden && m.HideState != model.HideStateNormal {
			continue
		}
		total += m.UnreadCount
		conversations++
	}
	return total, conversations, nil
}

func (f *fakeMessages) Insert(ctx context.Context, session sqlx.Session, msg *model.Message) (int64, bool, error) {
	f.st.tick("Messages.Insert")
	f.st.recordWrite("Messages.Insert", session)
	if err := f.st.failIf("Messages.Insert"); err != nil {
		return 0, false, err
	}
	if msg.ConversationID <= 0 || msg.Seq <= 0 {
		return 0, false, model.ErrConversationNotFound
	}
	if msg.ClientMsgID == "" {
		return 0, false, model.ErrClientMsgIDRequired
	}
	// uniq_sender_client：幂等重放回已有 ID。
	for _, m := range f.st.msgs {
		if m.SenderMid == msg.SenderMid && m.ClientMsgID == msg.ClientMsgID {
			return m.MsgID, false, nil
		}
	}
	// uniq_conv_seq：同会话同 seq 只能有一行。
	for _, m := range f.st.msgs {
		if m.ConversationID == msg.ConversationID && m.Seq == msg.Seq {
			return 0, false, errors.New("pm_message Insert: Error 1062: Duplicate entry for key 'uniq_conv_seq'")
		}
	}
	row := cloneMessage(*msg)
	row.MsgID = f.st.id()
	row.ContentPurged = model.ContentPurgedNo
	if row.Ctime == 0 {
		row.Ctime = timeNowUnix()
	}
	row.Mtime = timeNowUnix()
	f.st.msgs[row.MsgID] = row
	return row.MsgID, true, nil
}

func (f *fakeMessages) FindByID(ctx context.Context, msgID int64) (*model.Message, error) {
	f.st.tick("Messages.FindByID")
	m, ok := f.st.msgRow(msgID)
	if !ok {
		f.st.loadRows("Messages.FindByID", 0)
		return nil, nil
	}
	f.st.loadRows("Messages.FindByID", 1)
	return &m, nil
}

func (f *fakeMessages) FindByClientMsgID(ctx context.Context, senderMid int64, clientMsgID string) (*model.Message, error) {
	f.st.tick("Messages.FindByClientMsgID")
	if clientMsgID == "" {
		return nil, model.ErrClientMsgIDRequired
	}
	for _, m := range f.st.msgs {
		if m.SenderMid == senderMid && m.ClientMsgID == clientMsgID {
			cp := cloneMessage(m)
			f.st.loadRows("Messages.FindByClientMsgID", 1)
			return &cp, nil
		}
	}
	f.st.loadRows("Messages.FindByClientMsgID", 0)
	return nil, nil
}

func (f *fakeMessages) FindByIDs(ctx context.Context, ids []int64) (map[int64]*model.Message, error) {
	f.st.tick("Messages.FindByIDs")
	out := map[int64]*model.Message{}
	for _, id := range ids {
		if m, ok := f.st.msgRow(id); ok {
			cp := m
			out[id] = &cp
		}
	}
	f.st.loadRows("Messages.FindByIDs", len(out))
	return out, nil
}

func (f *fakeMessages) ListBeforeSeq(ctx context.Context, conversationID, cursorSeq int64, ps int32) ([]*model.Message, error) {
	f.st.tick("Messages.ListBeforeSeq")
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []model.Message
	for _, m := range f.st.msgs {
		if m.ConversationID != conversationID {
			continue
		}
		if cursorSeq > 0 && m.Seq >= cursorSeq {
			continue
		}
		rows = append(rows, m)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq > rows[j].Seq })
	if int32(len(rows)) > ps {
		rows = rows[:ps]
	}
	out := make([]*model.Message, 0, len(rows))
	for _, r := range rows {
		cp := cloneMessage(r)
		out = append(out, &cp)
	}
	f.st.loadRows("Messages.ListBeforeSeq", len(out))
	return out, nil
}

func (f *fakeMessages) LastVisible(ctx context.Context, conversationID int64) (*model.Message, error) {
	f.st.tick("Messages.LastVisible")
	var best *model.Message
	for _, m := range f.st.msgs {
		if m.ConversationID != conversationID || m.State == model.MsgStateRejected {
			continue
		}
		cp := cloneMessage(m)
		if best == nil || cp.Seq > best.Seq {
			best = &cp
		}
	}
	if best != nil {
		f.st.loadRows("Messages.LastVisible", 1)
	}
	return best, nil
}

func (f *fakeMessages) CountVisibleAfterSeq(ctx context.Context, conversationID, seq int64) (int64, error) {
	f.st.tick("Messages.CountVisibleAfterSeq")
	var n int64
	for _, m := range f.st.msgs {
		if m.ConversationID == conversationID && m.Seq > seq && m.State == model.MsgStateNormal {
			n++
		}
	}
	return n, nil
}

func (f *fakeMessages) HasAnyFromSender(ctx context.Context, conversationID, senderMid int64) (bool, error) {
	f.st.tick("Messages.HasAnyFromSender")
	if conversationID <= 0 {
		return false, model.ErrConversationNotFound
	}
	if senderMid <= 0 {
		return false, model.ErrInvalidMid
	}
	for _, m := range f.st.msgs {
		if m.ConversationID == conversationID && m.SenderMid == senderMid {
			return true, nil
		}
	}
	return false, nil
}

// BindAuditEvent 只作用于 PENDING_REVIEW 行；同值重写时真库 RowsAffected 可能是 0，
// 这里按「命中态即 applied」复刻（真库里 audit_event_id 变了必然改变行数）。
func (f *fakeMessages) BindAuditEvent(ctx context.Context, session sqlx.Session, msgID, taskID int64, eventID string) (bool, error) {
	f.st.tick("Messages.BindAuditEvent")
	if msgID <= 0 {
		return false, model.ErrMessageNotFound
	}
	if eventID == "" {
		return false, model.ErrIdempotencyKeyRequired
	}
	m, ok := f.st.msgRow(msgID)
	if !ok || m.State != model.MsgStatePendingReview {
		return false, nil
	}
	m.AuditTaskID, m.AuditEventID, m.Mtime = taskID, eventID, timeNowUnix()
	f.st.msgs[msgID] = m
	return true, nil
}

func (f *fakeMessages) MarkState(ctx context.Context, session sqlx.Session, msgID int64, from []int32,
	to int32, withdrawTime int64, auditEventID string) (bool, error) {
	f.st.tick("Messages.MarkState")
	f.st.recordWrite("Messages.MarkState", session)
	if err := f.st.failIf("Messages.MarkState"); err != nil {
		return false, err
	}
	if len(from) == 0 {
		return false, model.ErrInvalidStateTransition
	}
	m, ok := f.st.msgRow(msgID)
	if !ok {
		return false, model.ErrMessageNotFound
	}
	allowed := false
	for _, s := range from {
		if s == m.State {
			allowed = true
		}
	}
	if !allowed {
		return false, nil // 重复投递/非法迁移：交由 logic 归因
	}
	if withdrawTime > 0 {
		m.WithdrawTime = withdrawTime
	}
	if auditEventID != "" {
		m.AuditEventID = auditEventID
	}
	m.State, m.Mtime = to, timeNowUnix()
	f.st.msgs[msgID] = m
	return true, nil
}

func (f *fakeMessages) BindAuditTask(ctx context.Context, session sqlx.Session, msgID, taskID int64) error {
	f.st.tick("Messages.BindAuditTask")
	f.st.recordWrite("Messages.BindAuditTask", session)
	m, ok := f.st.msgRow(msgID)
	if !ok || m.AuditTaskID != 0 { // WHERE audit_task_id = 0
		return nil
	}
	m.AuditTaskID, m.Mtime = taskID, timeNowUnix()
	f.st.msgs[msgID] = m
	return nil
}

func (f *fakeMessages) ListPurgeCandidates(ctx context.Context, beforeTime int64, limit int32) ([]int64, error) {
	f.st.tick("Messages.ListPurgeCandidates")
	if limit <= 0 {
		return nil, model.ErrInvalidPage
	}
	type row struct {
		id    int64
		ctime int64
	}
	var cand []row
	for _, m := range f.st.msgs {
		if m.Ctime < beforeTime && m.ContentPurged == model.ContentPurgedNo {
			cand = append(cand, row{m.MsgID, m.Ctime})
		}
	}
	sort.Slice(cand, func(i, j int) bool {
		if cand[i].ctime != cand[j].ctime {
			return cand[i].ctime < cand[j].ctime
		}
		return cand[i].id < cand[j].id
	})
	if int32(len(cand)) > limit {
		cand = cand[:limit]
	}
	ids := make([]int64, 0, len(cand))
	for _, c := range cand {
		ids = append(ids, c.id)
	}
	return ids, nil
}

func (f *fakeMessages) CountPurgeCandidates(ctx context.Context, beforeTime int64) (int64, error) {
	f.st.tick("Messages.CountPurgeCandidates")
	var n int64
	for _, m := range f.st.msgs {
		if m.Ctime < beforeTime && m.ContentPurged == model.ContentPurgedNo {
			n++
		}
	}
	return n, nil
}

// PurgeByIDs 复刻 WHERE content_purged = 0：重跑不产生第二次写入，
// 且只清正文与媒体引用，state/preview/审计列一律保留。
func (f *fakeMessages) PurgeByIDs(ctx context.Context, ids []int64) (int64, error) {
	f.st.tick("Messages.PurgeByIDs")
	if err := f.st.failIf("Messages.PurgeByIDs"); err != nil {
		return 0, err
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var n int64
	for id, m := range f.st.msgs {
		if !want[id] || m.ContentPurged != model.ContentPurgedNo {
			continue
		}
		m.ContentCipher, m.KeyVersion, m.ContentPurged, m.MediaRef = nil, 0, model.ContentPurgedYes, ""
		m.Mtime = timeNowUnix()
		f.st.msgs[id] = m
		n++
	}
	return n, nil
}

func (f *fakeSettings) FindByMid(ctx context.Context, mid int64) (*model.UserSetting, error) {
	f.st.tick("Settings.FindByMid")
	if err := f.st.failIf("Settings.FindByMid"); err != nil {
		return nil, err
	}
	s, ok := f.st.settings[mid]
	if !ok || f.st.hideSettingRow {
		return nil, nil
	}
	cp := s
	return &cp, nil
}

func (f *fakeSettings) FindByMids(ctx context.Context, mids []int64) (map[int64]*model.UserSetting, error) {
	f.st.tick("Settings.FindByMids")
	out := map[int64]*model.UserSetting{}
	for _, mid := range mids {
		if s, ok := f.st.settings[mid]; ok {
			cp := s
			out[mid] = &cp
		}
	}
	return out, nil
}

func (f *fakeSettings) Upsert(ctx context.Context, s *model.UserSetting) error {
	f.st.tick("Settings.Upsert")
	if err := f.st.failIf("Settings.Upsert"); err != nil {
		return err
	}
	if s == nil || s.Mid <= 0 {
		return model.ErrInvalidMid
	}
	if !model.ValidAllowFrom(s.AllowFrom) {
		return model.ErrInvalidAllowFrom
	}
	now := timeNowUnix()
	if cur, ok := f.st.settings[s.Mid]; ok {
		// ON DUPLICATE KEY UPDATE：created_at 不动。
		row := *s
		row.CreatedAt, row.UpdatedAt = cur.CreatedAt, now
		f.st.settings[s.Mid] = row
		return nil
	}
	row := *s
	row.CreatedAt, row.UpdatedAt = now, now
	f.st.settings[s.Mid] = row
	return nil
}

func (f *fakeReports) Insert(ctx context.Context, r *model.Report) (int64, bool, error) {
	f.st.tick("Reports.Insert")
	if err := f.st.failIf("Reports.Insert"); err != nil {
		return 0, false, err
	}
	if r.MsgID <= 0 || r.ReporterMid <= 0 {
		return 0, false, model.ErrInvalidMid
	}
	for _, cur := range f.st.reports { // uniq_msg_reporter
		if cur.MsgID == r.MsgID && cur.ReporterMid == r.ReporterMid {
			return cur.ReportID, false, nil
		}
	}
	now := timeNowUnix()
	row := *r
	row.ReportID = f.st.id()
	row.State = model.ReportStatePending
	row.Handler, row.HandleNote = 0, ""
	row.HandleIdempotencyKey = sqlNullString("", false)
	row.Ctime, row.Mtime = now, now
	f.st.reports[row.ReportID] = row
	return row.ReportID, true, nil
}

func (f *fakeReports) FindByID(ctx context.Context, reportID int64) (*model.Report, error) {
	f.st.tick("Reports.FindByID")
	r, ok := f.st.reports[reportID]
	if !ok {
		return nil, nil
	}
	cp := r
	return &cp, nil
}

func (f *fakeReports) FindByHandleKey(ctx context.Context, key string) (*model.Report, error) {
	f.st.tick("Reports.FindByHandleKey")
	if key == "" {
		return nil, model.ErrIdempotencyKeyRequired
	}
	for _, r := range f.st.reports {
		if r.HandleIdempotencyKey.Valid && r.HandleIdempotencyKey.String == key {
			cp := r
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeReports) ListPending(ctx context.Context, ps int32) ([]*model.Report, error) {
	f.st.tick("Reports.ListPending")
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var out []*model.Report
	for _, r := range f.st.reports {
		if r.State == model.ReportStatePending {
			cp := r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReportID < out[j].ReportID })
	if int32(len(out)) > ps {
		out = out[:ps]
	}
	return out, nil
}

func (f *fakeReports) ListByCursor(ctx context.Context, state, targetMid, cursorID int64, ps int32) ([]*model.Report, error) {
	f.st.tick("Reports.ListByCursor")
	f.listByCursorArgs = append(f.listByCursorArgs, listByCursorArg{
		state: state, targetMid: targetMid, cursorID: cursorID, ps: ps,
	})
	if err := f.st.failIf("Reports.ListByCursor"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var out []*model.Report
	for _, r := range f.st.reports {
		if state > 0 && r.State != int32(state) {
			continue
		}
		if targetMid > 0 && r.TargetMid != targetMid {
			continue
		}
		if cursorID > 0 && r.ReportID >= cursorID {
			continue
		}
		cp := r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReportID > out[j].ReportID })
	if int32(len(out)) > ps {
		out = out[:ps]
	}
	return out, nil
}

func (f *fakeReports) markHandled(reportID int64, state int32, handler int64, note, key string, requery bool) error {
	f.st.tick("Reports.MarkHandled")
	r, ok := f.st.reports[reportID]
	if ok && r.State == model.ReportStatePending {
		r.State, r.Handler, r.HandleNote = state, handler, note
		r.HandleIdempotencyKey, r.Mtime = sqlNullString(key, true), timeNowUnix()
		f.st.reports[reportID] = r
		return nil
	}
	if requery && !ok {
		return model.ErrReportNotFound
	}
	return model.ErrConcurrentUpdate
}

func (f *fakeReports) MarkHandled(ctx context.Context, reportID int64, state int32, handler int64, note, key string) error {
	if key == "" {
		f.st.tick("Reports.MarkHandled")
		return model.ErrIdempotencyKeyRequired
	}
	return f.markHandled(reportID, state, handler, note, key, true)
}

// MarkHandledInTx 与 MarkHandled 的唯一差别：事务内不回查（回查走的是连接而非事务快照），
// 命中 0 行一律 ErrConcurrentUpdate，由 logic 在事务外按幂等键回放。
func (f *fakeReports) MarkHandledInTx(ctx context.Context, session sqlx.Session, reportID int64,
	state int32, handler int64, note, key string) error {
	f.st.recordWrite("Reports.MarkHandledInTx", session)
	if err := f.st.failIf("Reports.MarkHandledInTx"); err != nil {
		return err
	}
	if key == "" {
		f.st.tick("Reports.MarkHandledInTx")
		return model.ErrIdempotencyKeyRequired
	}
	return f.markHandled(reportID, state, handler, note, key, false)
}

func (f *fakeReports) BindAuditTask(ctx context.Context, reportID, taskID int64) error {
	f.st.tick("Reports.BindAuditTask")
	r, ok := f.st.reports[reportID]
	if !ok || r.AuditTaskID != 0 {
		return nil
	}
	r.AuditTaskID, r.Mtime = taskID, timeNowUnix()
	f.st.reports[reportID] = r
	return nil
}

func (f *fakeWithdrawLogs) Insert(ctx context.Context, session sqlx.Session, l *model.WithdrawLog) error {
	f.st.tick("WithdrawLogs.Insert")
	f.st.recordWrite("WithdrawLogs.Insert", session)
	if err := f.st.failIf("WithdrawLogs.Insert"); err != nil {
		return err
	}
	if l == nil || l.MsgID <= 0 {
		return model.ErrMessageNotFound
	}
	if !model.ValidWithdrawSource(l.Source) {
		return model.ErrInvalidStateTransition
	}
	row := *l
	row.LogID = f.st.id()
	if row.Ctime == 0 {
		row.Ctime = timeNowUnix()
	}
	f.st.wlogs[row.LogID] = row
	l.LogID = row.LogID
	return nil
}

func (f *fakeWithdrawLogs) ListByMsgID(ctx context.Context, msgID int64) ([]*model.WithdrawLog, error) {
	f.st.tick("WithdrawLogs.ListByMsgID")
	var out []*model.WithdrawLog
	for _, l := range f.st.wlogs {
		if l.MsgID == msgID {
			cp := l
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LogID < out[j].LogID })
	return out, nil
}

func (f *fakeWithdrawLogs) ListByOperator(ctx context.Context, operatorMid int64, ps int32) ([]*model.WithdrawLog, error) {
	f.st.tick("WithdrawLogs.ListByOperator")
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var out []*model.WithdrawLog
	for _, l := range f.st.wlogs {
		if l.OperatorMid == operatorMid {
			cp := l
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LogID > out[j].LogID })
	if int32(len(out)) > ps {
		out = out[:ps]
	}
	return out, nil
}

// --- 事务与连接 ---

type fakeConn struct {
	sqlx.SqlConn
	st *store
}

func (c *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.st.txs++
	snap := c.st.snapshot()
	if err := fn(ctx, &fakeSession{}); err != nil {
		c.st.restore(snap)
		return err
	}
	return nil
}

// fakeSession 只作为「我在事务里」的凭证存在：本替身直接操作内存表，
// 但 model 的 AllocateSeq 等约定必须能区分事务内外。
type fakeSession struct{ sqlx.Session }

// --- 限流 ---

type fakeLimiter struct {
	err   error
	calls int
	ops   []ratelimit.Op
}

func (l *fakeLimiter) Allow(ctx context.Context) (func(ratelimit.Op), error) {
	l.calls++
	if l.err != nil {
		return func(ratelimit.Op) {}, l.err
	}
	return func(op ratelimit.Op) { l.ops = append(l.ops, op) }, nil
}

// --- 日志 ---

type fakeLogWriter struct {
	mu   sync.Mutex
	text []string
}

func (w *fakeLogWriter) Alert(v any)                          { w.add("severe", v) }
func (w *fakeLogWriter) Close() error                         { return nil }
func (w *fakeLogWriter) Debug(v any, fields ...logx.LogField) { w.add("debug", v, fields...) }
func (w *fakeLogWriter) Error(v any, fields ...logx.LogField) { w.add("error", v, fields...) }
func (w *fakeLogWriter) Info(v any, fields ...logx.LogField)  { w.add("info", v, fields...) }
func (w *fakeLogWriter) Severe(v any)                         { w.add("severe", v) }
func (w *fakeLogWriter) Slow(v any, fields ...logx.LogField)  { w.add("slow", v, fields...) }
func (w *fakeLogWriter) Stack(v any)                          { w.add("error", v) }
func (w *fakeLogWriter) Stat(v any, fields ...logx.LogField)  { w.add("stat", v, fields...) }

// fieldsText 把结构化字段并成 `key=value` 一起留痕：logx 把 Infow 的字段作为可变参数
// 传给每个 writer，忽略它们就等于让按字段值做的断言全部失效（正向永假、隐私反向永真）。
func fieldsText(fields []logx.LogField) string {
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, " %s=%v", f.Key, f.Value)
	}
	return b.String()
}

func (w *fakeLogWriter) add(kind string, v any, fields ...logx.LogField) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.text = append(w.text, kind+" "+fmt.Sprint(v)+fieldsText(fields))
}

func (w *fakeLogWriter) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.text...)
}

func (w *fakeLogWriter) joined() string { return strings.Join(w.all(), "\n") }

// captureLogs 挂一份内存日志接收器：本服务的隐私契约里有一类只能从日志证明——
// 「日志只带主键，永不带正文」。用 AddWriter 而非 SetWriter，默认输出保留。
func captureLogs(t *testing.T) *fakeLogWriter {
	t.Helper()
	w := &fakeLogWriter{}
	logx.AddWriter(w)
	return w
}

// --- 测试脚手架 ---

// testDataKey 是一眼可辨认的测试密钥材料（32 字节 AES-256）。
// 它不是「生产密钥的占位符」：本文件所有断言都不依赖它的具体字节，
// 只有长度与「必须与 pepper 同时存在」被用到。
func testDataKey() string { return base64OfBytes(strings.Repeat("k", 32)) }

func newTestConfig() config.Config {
	var c config.Config
	c.PrivateMessage = config.PrivateMessageConf{
		PageSize:                20,
		MaxPageSize:             50,
		MaxTextLength:           2000,
		PreviewRunes:            30,
		DefaultAllowFrom:        model.AllowFromFollowed,
		RejectStrangerByDefault: true,
		KeywordFilterEnabled:    true,
		MachineReviewEnabled:    false,
		// 0 = 该项本地限额不设限（quotaUnlimited 语义）：本套用例默认不依赖 Redis，
		// 需要验证 fail-closed 的用例显式把它调回正数（见 TestSendFailsClosedWhenCounterUnavailable）。
		MaxPerUserPerMinute:            0,
		MaxStrangerConversationsPerDay: 0,
		WithdrawWindowSeconds:          120,
		MessageRetentionDays:           180,
		PurgeBatchSize:                 500,
		PostQps:                        400,
		PostBurst:                      100,
	}
	c.Cipher = config.CipherConf{KeyVersion: 3, DataKeyBase64: testDataKey(), HashPepper: "unit-test-pepper"}
	return c
}

// env 是一套「内存库 + 六个 model 替身 + 事务探针」的测试环境。
// 下游客户端由 downstream_test.go 的 withDownstream 之类助手指派，默认全为 nil
// （未配置即 fail-closed，是 README 写死的口径，也是大部分读取路径的真实形态）。
type env struct {
	t       *testing.T
	svc     *svc.ServiceContext
	st      *store
	logs    *fakeLogWriter
	limiter *fakeLimiter
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if err := newTestConfig().Validate(); err != nil {
		t.Fatalf("测试配置本身不合法，后续断言全部失真：%v", err)
	}
	st := newStore()
	lim := &fakeLimiter{}
	return &env{
		t:  t,
		st: st,
		svc: &svc.ServiceContext{
			Config:        newTestConfig(),
			DB:            &fakeConn{st: st},
			Conversations: &fakeConversations{st: st},
			Members:       &fakeMembers{st: st},
			Messages:      &fakeMessages{st: st},
			Settings:      &fakeSettings{st: st},
			Reports:       &fakeReports{st: st},
			WithdrawLogs:  &fakeWithdrawLogs{st: st},
			WriteLimiter:  lim,
		},
		logs:    captureLogs(t),
		limiter: lim,
	}
}

// txMark / txsSince 把「这次调用开了几个事务」的观测窗口收在被测调用上：
// st.txs 是整个用例的累计量，种子写入自己就开过事务，
// 拿绝对值断言等于把脚手架的写入算到被测代码头上。
func (e *env) txMark() int           { return e.st.txs }
func (e *env) txsSince(mark int) int { return e.st.txs - mark }

func (e *env) calls(name string) int { return e.st.calls[name] }

// rows 是「这次调用写了多少行」的探针快照。
type rows struct {
	convs, members, msgs, settings, reports, wlogs int
}

func (e *env) snapshotRows() rows {
	return rows{len(e.st.convs), len(e.st.members), len(e.st.msgs), len(e.st.settings), len(e.st.reports), len(e.st.wlogs)}
}

func (r rows) String() string {
	return fmt.Sprintf("conv=%d member=%d msg=%d setting=%d report=%d withdraw_log=%d",
		r.convs, r.members, r.msgs, r.settings, r.reports, r.wlogs)
}

// requireSameRows 断言一次调用没留下任何行级痕迹：入参被拒、不是成员、门禁挡住，
// 三种情形都要求「库里什么都没变」。
func (e *env) requireSameRows(t *testing.T, before rows, label string) {
	t.Helper()
	if got := e.snapshotRows(); got != before {
		t.Fatalf("%s：产生了行级副作用\n  前：%s\n  后：%s", label, before, got)
	}
}

// --- 小工具 ---

func base64OfBytes(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// deadCache 回一个「配置了但必然连不上」的 *redis.Redis：
// 用它区分「没配缓存（Cache==nil，helper 直接短路）」与「配了但挂了（走到真实调用再失败）」，
// 后者才是线上 Redis 抖动时读取路径的真实形态。
//
// 端口 1 在本机不会被监听，connect 立刻被拒；用例仍应配 shortCtx() 收敛总时长。
// 这个客户端不持有本地监听资源，也不需要连接池（go-zero 该版本无 Close 方法），
// 因此不注册 t.Cleanup。
func deadCache(t *testing.T) *redis.Redis {
	t.Helper()
	r, err := redis.NewRedis(redis.RedisConf{Host: deadCacheAddr, Type: "node", NonBlock: true})
	if err != nil {
		t.Fatalf("构造 dead redis 客户端失败：%v", err)
	}
	return r
}

const deadCacheAddr = "127.0.0.1:1"

// shortCtx 给「必然失败的下游调用」一个明确的截止点：
// go-zero 的 redis 客户端不暴露重试/超时选项，没有截止点时一条挂掉的缓存操作
// 可能把用例拖到分钟级；300ms 足够本地 connect 被拒两次。
func shortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg(), 300*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

// handleCalls 汇总某个数据句柄被调用的方法次数（"Members" → Members.* 全部调用数）。
// self / operator 口径的入口用它钉住「只许碰自己那一张表」：
// 多出一条跨表读取（例如偏好查询顺手去扫会话）会立刻让计数非零。
func (st *store) handleCalls(handle string) int {
	prefix := handle + "."
	var n int
	for k, v := range st.calls {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}

func (e *env) handleCalls(handle string) int { return e.st.handleCalls(handle) }

// touchedHandles 回「本次用例实际碰过哪些数据句柄」，用于失败信息：
// 只说「越界了」不给证据，评审时无法判断是新增面还是替身记错了账。
func (st *store) touchedHandles() []string {
	seen := map[string]bool{}
	var out []string
	for k, v := range st.calls {
		if v <= 0 {
			continue
		}
		handle := strings.SplitN(k, ".", 2)[0]
		if seen[handle] {
			continue
		}
		seen[handle] = true
		out = append(out, handle)
	}
	sort.Strings(out)
	return out
}

func (e *env) touchedHandles() []string { return e.st.touchedHandles() }

// callMark 是「按调用窗口记账」的快照点。
//
// 为什么不能直接用 handleCalls 的绝对值：种子数据本身就会写会话/成员/消息三张表，
// 用绝对值断言「这次调用只读了成员表」必然因为脚手架的历史记账而失败，
// 于是只能测出「整个用例碰过哪些表」——那正是最弱的一句话。
// 有了快照差分，才能钉住「守卫之后到查库之前这一步不多读一张表」这类真问题。
type callMark struct {
	st     *store
	before map[string]int
}

func (e *env) markCalls() callMark {
	return callMark{st: e.st, before: e.st.callSnapshot()}
}

// callsOf 回某个具体方法（"Members.SumUnread"）在标记之后的调用次数。
func (m callMark) callsOf(name string) int { return m.st.calls[name] - m.before[name] }

// handleCallsOf 回某个句柄（"Members"）在标记之后全部方法的调用次数之和。
func (m callMark) handleCallsOf(handle string) int {
	prefix := handle + "."
	var n int
	for k, v := range m.st.calls {
		if strings.HasPrefix(k, prefix) {
			n -= m.before[k]
			n += v
		}
	}
	return n
}

// touchedSince 回标记之后被碰过的句柄名（已排序），用于失败信息给证据。
func (m callMark) touchedSince() []string {
	seen := map[string]bool{}
	var out []string
	for k, v := range m.st.calls {
		if v <= m.before[k] {
			continue
		}
		handle := strings.SplitN(k, ".", 2)[0]
		if seen[handle] {
			continue
		}
		seen[handle] = true
		out = append(out, handle)
	}
	sort.Strings(out)
	return out
}

func (st *store) callSnapshot() map[string]int {
	out := make(map[string]int, len(st.calls))
	for k, v := range st.calls {
		out[k] = v
	}
	return out
}

// modelHandles 是本服务全部六个数据句柄（与 svc.ServiceContext 上的六个 model 字段一一对应）。
var modelHandles = []string{"Conversations", "Members", "Messages", "Settings", "Reports", "WithdrawLogs"}

// errInjected 是「依赖层抖动」的注入哨兵：与 model 包里任何业务哨兵都不同，
// 用它才能测出「logic 是否原样上抛依赖错误」——若实现把任意错误都折叠成某个业务码，
// 断言业务哨兵会照样通过，而断言 errInjected 一定会红。
var errInjected = errors.New("private-message/test: injected dependency failure")

// requireOnlyHandleRead 断言「标记之后的这次调用只读了 want 这一个句柄」。
// self / operator 域入口的越权面扩张都是「顺手多读了一张表」，用窗口内的调用次数钉住
// 比读代码可靠：多一条跨表读就会让另一个句柄的窗口计数变成非零。
func requireOnlyHandleRead(t *testing.T, m callMark, want string, label string) {
	t.Helper()
	if m.handleCallsOf(want) == 0 {
		t.Fatalf("%s：没有走 %s 这张表，等于没测到真实数据来源（实际碰过 %v）", label, want, m.touchedSince())
	}
	for _, h := range modelHandles {
		if h == want {
			continue
		}
		if n := m.handleCallsOf(h); n != 0 {
			t.Fatalf("%s：越界访问了 %s.*（%d 次），本方法只允许读 %s.*；本次实际碰过 %v", label, h, n, want, m.touchedSince())
		}
	}
}

// requireHandlesRead 断言「标记之后这次调用恰好只碰了 want 里列出的那几个句柄」。
// 用于列表这类「按设计就要读两张表」的入口：requireOnlyHandleRead 太严，
// 但放任「碰了哪张表都行」又等于没测——精确集合是本服务授权口径的最小可证单位。
func requireHandlesRead(t *testing.T, m callMark, want []string, label string) {
	t.Helper()
	inWant := map[string]bool{}
	for _, h := range want {
		inWant[h] = true
		if m.handleCallsOf(h) == 0 {
			t.Fatalf("%s：没有走 %s 这张表，等于没测到真实数据来源（实际碰过 %v）", label, h, m.touchedSince())
		}
	}
	for _, h := range modelHandles {
		if inWant[h] {
			continue
		}
		if n := m.handleCallsOf(h); n != 0 {
			t.Fatalf("%s：越界访问了 %s.*（%d 次），本方法允许读的集合是 %v；本次实际碰过 %v", label, h, n, want, m.touchedSince())
		}
	}
}

// sqlNullString 复刻 pm_report.handle_idempotency_key 的 NULL/值 两态。
func sqlNullString(v string, valid bool) sql.NullString {
	return sql.NullString{String: v, Valid: valid}
}
