package logic

// fakes_test.go 是 live-room logic 用例的内存替身集合。
//
// 覆盖范围（2026-09-23 按「测试文件里是否出现 NewXxxLogic(」逐个核过：21 个方法**全部**
// 有构造器级用例）：读侧 7 个（GetRoom / ListRooms / ListAnchors / ListAreas /
// ListRoomBans / ListSessions / GetSession）+ 写侧 14 个（CreateRoom / StartLive / EndLive /
// BanRoom / LiftBan / CloseRoom / UpdateRoomInfo / UpdateRoomSetting / UpsertArea /
// PrepareLive / MutateAnchor / ReportStreamState / AttachReplay / ApplyRoomModerationResult）。
// 「有构造器级用例」只等于「一次调用打了哪几条 SQL、失败留在库里什么形态」被锁住，
// **不**等于所有分支都锁住——口径与各文件未覆盖分支见 services/live-room/README.md
// 「测试覆盖」小节与「已知缺口」。
//
// 写侧替身在读侧四条纪律之外再加一条：
//  5. **事务不回滚**：`fakeConn.TransactCtx` 直接调 `fn`，中途失败时前面已写的行**留在库里**
//     （真实 MySQL 会回滚）。因此失败用例一律断言「失败后库里到底还剩什么」，
//     绝不写「应当已回滚」这种假结论——这正是本文件能暴露「副作用跨语句分裂」的地方。
//
// 为什么不需要注入缝：本服务**没有 internal/repository**，logic 直接读
// `svc.ServiceContext` 上导出的 8 个 model 接口字段与 DB/Cache/下游 client，
// 所以手工装配一个 ServiceContext 就是完整的替换点（不需要生产代码留 seam）。
// 假件直接 implement `services/live-room/model/*.go` 的 8 个接口，
// 不 import 任何 `model.NewXxxModel(conn)` 构造函数、不连 MySQL/Redis。
//
// 四条替身纪律：
//  1. **读取返回值拷贝**：每个 Find*/List* 返回的都是行副本，logic 若就地改字段
//     不会污染库存行——否则「有没有真的落库」「投影读的是哪一份数据」这类断言会被
//     共享指针掩盖。
//  2. **写侧像真实 SQL 一样写**：`Insert` 自己分配主键（表内 max+1，模拟 AUTO_INCREMENT），
//     只写生产语句里出现的列，**生产 SQL 不写的列一律不自动补**
//     （例：`live_room_ban.insert` 恒把 `lift_operator_mid/lift_reason/lifted_at` 写成 0/""，
//     `live_room_setting.upsert` 更新分支保留 `ctime`，`UpdateProfile` 不碰 `state_version`）。
//     未实现的写方法仍返回 `errUnexpectedDependency`（不返回假成功），
//     所以「本次声称没走某条 SQL」是会被 callLog 抓到的。
//     布数据也必须布成「生产写得出来的行」，否则直接 panic（见各 seedXxx）。
//  3. **有序 callLog**（`<表>.<方法>[:<键>]`）：既数次数，也断顺序。读侧要紧的结论
//     全部是顺序与口径类的（List 先于 Count 且两次同一个 query、owner_mid 路径必须
//     走绑定表取 id 再回表且不得再走 Rooms.List、守卫拒绝后一次依赖调用都不许发生）。
//     写侧的键**只放调用方可见的入参**（room_id/ban_id/状态三元组/dedup_key/reason），
//     由替身分配的自增主键不进键（`live_room.InsertTx`、`live_area.Insert`、
//     `live_room_anchor.BindTx:m101`、`live_room_state_log.InsertTx:t1:0->1:create_room`），
//     新号的正确性一律用「读回库里那一行」来断言，不写进期望序列。
//  4. **seed 静默**：布数据不写 callLog，所以序列断言里出现的每一项都是被测代码的调用。
//
// 假件的 SQL 语义逐条对齐 `model/*.go` 的真实语句（含 WHERE 片段、ORDER BY、
// LIMIT/OFFSET、以及 model 自己的入参守卫），文件内每处都标了来源行号。
// **刻意不对齐的地方只有两处，且都是缺陷**：`live_room.List` / `live_room.ListByRoomIDs` /
// `live_room_anchor.List` 三条 SQL 根本没有 OFFSET 子句，因此 `RoomListQuery.Offset` /
// `AnchorListQuery.Offset` 传进去也不生效——假件照抄这个事实（不替生产补上 OFFSET），
// 用例按当前真实行为断言并把缺陷登记在 README「已知缺口」。
//
// 如实声明无法覆盖的路径：
//   - `ServiceContext.Cache` 是具体类型 `*redis.Redis`，没有接口缝，纯内存用例只能构造
//     `Cache == nil` 的形态。因此 GetRoom 的详情缓存命中/回填、ListAreas 的列表缓存命中/回填，
//     以及写侧用例里 `invalidateRoomCache` / `invalidateAreaListCache` 的**失效是否真的发生**
//     都没有覆盖；覆盖的是「缓存未配置必须回源 MySQL 且写入照常成功」这条降级支路
//     （正是 AGENTS.md §5「缓存缺失只回源」的口径）。本轮**没有**为此加新缝：
//     加一个 `Cache` 接口会改动生产装配路径与 svc 结构体，收益不抵风险。
//   - `Creator` / `RiskControl` 在已有用例的写侧 3 个方法（CreateRoom / StartLive / EndLive）里
//     都不该被触达，本文件保持二者 nil
//     （真被触达会 nil panic，用例即红）。`Moderation` 是 Go 接口
//     `moderationrpc.ModerationOrchestratorClient`，所以用 `st.wireModeration()` 注入假客户端
//     同样不需要生产改动；不接线时它是 nil，正是「未接入 moderation」的真实形态。
//   - 真 SQL 的列名、索引命中、`COUNT(*)` 的实际值由 `model/migration_parity_test.go`
//     静态校验，本文件不重复证明。
//   - `RowsAffected` 在假件里按「WHERE 命中的行数」实现。go-sql-driver 默认
//     （DSN 未开 `clientFoundRows`，本服务 DSN 里没有该参数）返回的是** changed rows**，
//     所以真实 MySQL 上「把 ban_until 写成它已有的同一个值」这类空更新会得到 0 行命中，
//     被判成 ErrConcurrentUpdate。这条差异假件不模拟，已登记在 README「已知缺口」。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"google.golang.org/grpc"

	creatorrpc "go-video/services/creator/rpc"
	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"
	riskrpc "go-video/services/risk-control/rpc"
)

// --- 断言小工具（本包共享；comparable 之外的形态另走对应函数） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantStringsEQ 比较字符串切片（wantEQ 受 comparable 约束，切片只能另走这条）。
func wantStringsEQ(t *testing.T, label, field string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantInt64sEQ 比较 id 序列（列表类断言的主要形态：谁在第几位）。
func wantInt64sEQ(t *testing.T, label, field string, got, want []int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantDeepEQ 比较查询条件结构体（「List 与 Count 同口径」只能整体比，字段太多且
// 逐个比会漏掉以后新增的过滤维度）。
func wantDeepEQ[T any](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %v", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
	}
}

// wantErrContains 断言错误消息里的片段：logic 大量使用 `fmt.Errorf("%w: 细节", 哨兵)`，
// 哨兵本身不足以锁定「是哪个分支报的错」。
func wantErrContains(t *testing.T, label string, err error, frag string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want 含 %q", label, frag)
	}
	if !strings.Contains(err.Error(), frag) {
		t.Fatalf("%s：错误 = %v, want 含 %q", label, err, frag)
	}
}

// wantCount 断言某前缀的调用次数（seed 静默，所以数出来的都是被测代码的调用）。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整轨迹 [%s]）",
			label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// wantMethodCount 按方法名精确计数（op 去掉 `:键` 后整体相等）。
// 锁「没走某条 SQL」必须用这条：`live_room.List` 是 `live_room.ListByOwner` /
// `live_room.ListByRoomIDs` 的前缀，用 wantCount 会把断言写成永真或误伤。
func wantMethodCount(t *testing.T, label string, log *callLog, method string, want int) {
	t.Helper()
	if got := log.countMethod(method); got != want {
		t.Errorf("%s：%s 调用次数 = %d, want %d（完整轨迹 [%s]）",
			label, method, got, want, strings.Join(log.ops, " → "))
	}
}

// wantNoCallAfter 断言 from 之后没有任何依赖调用：入参守卫必须发生在触库之前。
func wantNoCallAfter(t *testing.T, label string, log *callLog, from int) {
	t.Helper()
	if ops := log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

// wantSeq 断言 from 之后的完整调用序列（顺序本身就是要锁的结论时用这条）。
// want 为空表示「此后一次依赖调用都不许发生」。
func wantSeq(t *testing.T, label string, log *callLog, from int, want ...string) {
	t.Helper()
	got := log.opsFrom(from)
	if !slices.Equal(got, want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label,
			strings.Join(got, " → "), strings.Join(want, " → "))
	}
}

// wantOpSeen 断言某条调用在轨迹里出现过（不比顺序、不看次数）。
// 只用于「这条 SQL 到底跑没跑」；锁顺序用 wantSeq，锁数量用 wantCount。
func wantOpSeen(t *testing.T, label string, log *callLog, want string) {
	t.Helper()
	if !slices.Contains(log.ops, want) {
		t.Fatalf("%s：轨迹里没有 %q（完整轨迹 [%s]）", label, want, strings.Join(log.ops, " → "))
	}
}

// wantLastOp 断言轨迹的最后一条：失败点之后不得再碰任何依赖。
func wantLastOp(t *testing.T, label string, log *callLog, want string) {
	t.Helper()
	if n := len(log.ops); n == 0 || log.ops[n-1] != want {
		t.Fatalf("%s：最后一条依赖调用 = %q, want %q（完整轨迹 [%s]）", label,
			log.opsLast(), want, strings.Join(log.ops, " → "))
	}
}

// --- 投影结果的 id 序列提取 ---

func roomIDs(infos []*rpc.RoomInfo) []int64 {
	out := make([]int64, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.GetRoomId())
	}
	return out
}

func sessionIDs(infos []*rpc.SessionInfo) []int64 {
	out := make([]int64, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.GetSessionId())
	}
	return out
}

func banIDs(infos []*rpc.RoomBanInfo) []int64 {
	out := make([]int64, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.GetBanId())
	}
	return out
}

func areaIDs(infos []*rpc.AreaInfo) []int64 {
	out := make([]int64, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.GetAreaId())
	}
	return out
}

func anchorIDs(infos []*rpc.AnchorInfo) []int64 {
	out := make([]int64, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.GetId())
	}
	return out
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

// countMethod 按「方法名」精确计数（op 去掉 `:键` 之后的部分）。
// 不能用 countPrefix：`live_room.List` 是 `live_room.ListByOwner` /
// `live_room.ListByRoomIDs` 的前缀，锁「没走某条 SQL」时前缀匹配会把断言变成假通过。
func (c *callLog) countMethod(method string) int {
	n := 0
	for _, o := range c.ops {
		if strings.SplitN(o, ":", 2)[0] == method {
			n++
		}
	}
	return n
}

func (c *callLog) snapshot() int             { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string { return c.ops[from:] }

func (c *callLog) opsLast() string {
	if len(c.ops) == 0 {
		return "<empty>"
	}
	return c.ops[len(c.ops)-1]
}

// --- 错误注入（按方法粒度，一个方法一个 error，不用全局 err） ---

// raceHooks 用来模拟「读到行之后、写之前，别的入口把这一行改了」这类并发，
// 不需要真起 goroutine：每个键是一次性钩子，在对应方法**执行之前**触发一次。
// 键与 callLog 的键同形（不含生成主键的用 `<表>.<方法>`，带参数的照抄）。
// 用法约定：想让钩子落在「某条写语句之后」，就把键挂到那条语句之后的调用上
// （例如挂在事务里的审计日志 InsertTx 上，等价于「状态改完后又被并发推进」）。
type raceHooks struct{ before map[string]func() }

func (r *raceHooks) arm(method string, fn func()) {
	if r.before == nil {
		r.before = map[string]func(){}
	}
	r.before[method] = fn
}

// fire 触发并摘掉钩子（一次性）；没布钩子时什么都不做。
func (r *raceHooks) fire(method string) {
	if r == nil || r.before == nil {
		return
	}
	if fn, ok := r.before[method]; ok {
		delete(r.before, method)
		fn()
	}
}

// assertFired 断言所有布下的钩子都被触发过：钩子没触发说明被测代码
// 根本没走到那条 SQL，用例的并发前提就成了空转（这类自证式失效必须抓出来）。
func (r *raceHooks) assertFired(t *testing.T, label string) {
	t.Helper()
	if len(r.before) != 0 {
		keys := make([]string, 0, len(r.before))
		for k := range r.before {
			keys = append(keys, k)
		}
		t.Errorf("%s：%s 这些并发钩子从未被触发（对应 SQL 没执行）%v", label, "raceBefore", keys)
	}
}

type faultInjector struct {
	who  string
	log  *callLog
	by   map[string]error
	race *raceHooks
}

func newFault(who string, log *callLog, race *raceHooks) faultInjector {
	return faultInjector{who: who, log: log, race: race}
}

// failWith 让某个方法返回指定错误（模拟 DB/驱动失败）。method 用接口里的方法名。
func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

// dbErr 返回注入的错误；未注入时返回 nil。log 记录已由调用方在入口完成。
func (f *faultInjector) dbErr(method string) error { return f.by[method] }

// fireRace 在真正写数据之前触发并发钩子（守卫之后、SQL 之前，与真实时序一致）。
func (f *faultInjector) fireRace(method string) { f.race.fire(method) }

// unexpected 是「本用例不允许触达的依赖调用」的统一出口。
// 选择返回错误而不是 panic：logic 会原样抛出，用例失败信息里直接有方法名，
// 同时这次调用仍进了 callLog，序列断言也抓得到它。
func (f *faultInjector) unexpected(method string) error {
	f.log.add("%s.%s:UNEXPECTED", f.who, method)
	return fmt.Errorf("%w: %s.%s 未在替身里实现（该路径本不该被触达，或请在 fakes_test.go 补真实 SQL 语义）",
		errUnexpectedDependency, f.who, method)
}

var errUnexpectedDependency = errors.New("liveroom/logic/test: unexpected dependency call")

// --- 事务连接替身 ---

// txSession 只是「拿到了事务会话」的凭证；读侧路径不应出现事务。
type txSession struct{ sqlx.Session }

// fakeConn 只实现 TransactCtx（模板抄 services/playback/internal/logic/fakes_test.go:609-626）。
// 其余方法落在嵌入的 nil 接口上会直接 panic，等价于「logic 单测路径上不允许出现任何
// 绕过 model 的直连 SQL」——真出现了就是越界，让它炸出来，不填成成功。
type fakeConn struct {
	sqlx.SqlConn
	log          *callLog
	transactions int
	rolledBack   int
}

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	f.transactions++
	f.log.add("db.TransactCtx")
	if err := fn(ctx, &txSession{}); err != nil {
		f.rolledBack++
		return err
	}
	return nil
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// wantNoDirectSQL 断言读侧没有绕过 model 直接起事务/连库。
func wantNoDirectSQL(t *testing.T, label string, conn *fakeConn) {
	t.Helper()
	if conn.transactions != 0 {
		t.Errorf("%s：读路径起了 %d 次事务，读侧必须只经 model 的只读方法", label, conn.transactions)
	}
}

// wantTxCount 断言事务次数（写侧：一次处置应当是一个事务；
// UpdateRoomSetting 这类「没有事务」的写路径要用它把事实钉住，而不是让它默默过去）。
func wantTxCount(t *testing.T, label string, conn *fakeConn, want int) {
	t.Helper()
	if conn.transactions != want {
		t.Errorf("%s：事务次数 = %d, want %d（轨迹 [%s]）",
			label, conn.transactions, want, strings.Join(conn.log.ops, " → "))
	}
}

// --- model 层的公共常量与工具 ---

// fakeDefaultListLimit 与 model.defaultListLimit 同值（model/live_room.go:22）：
// 调用方忘传 limit 时 SQL 侧的兜底行数。跨包不可见，这里按来源行号复刻。
const fakeDefaultListLimit int32 = 50

// fakeAreaNameMaxRunes 与 model.AreaNameMaxRunes 同值（model/live_area.go:25，硬编码 32）。
// 复制这个常量的目的就是暴露它：model 不看 config.LiveRoom.AreaNameMaxLength，
// 两处一旦漂移，logic 放行、model 拒绝（用例 TestUpsertAreaConfigWidthDrift 锁住）。
const fakeAreaNameMaxRunes = 32

// nextFakePK 模拟 AUTO_INCREMENT：表内已有号的最大值 +1。
// 分配结果不进 callLog 的键（新键的用例一律读回那一行来断言）。
func nextFakePK[T any](rows []T, idOf func(T) int64) int64 {
	var hi int64
	for _, r := range rows {
		if v := idOf(r); v > hi {
			hi = v
		}
	}
	return hi + 1
}

// fixClock 把 model 的取时入口钉死在 at 秒（model.SetClockForTest 是生产代码里
// 已有的测试专用注入点，见 model/now.go:16，本轮不新增任何缝）。
// 只有钉死时钟，`ctime/mtime/ban_until/start_at/end_at/lifted_at` 这类由 SQL 侧
// now() 产生的值才能进精确断言。
func fixClock(t *testing.T, at int64) {
	t.Helper()
	restore := model.SetClockForTest(func() time.Time { return time.Unix(at, 0) })
	t.Cleanup(restore)
}

func copyRoom(r *model.LiveRoom) *model.LiveRoom {
	cp := *r
	return &cp
}

func copyRooms(rows []*model.LiveRoom) []*model.LiveRoom {
	out := make([]*model.LiveRoom, 0, len(rows))
	for _, r := range rows {
		out = append(out, copyRoom(r))
	}
	return out
}

// roomMatches 复刻 roomListWhere（model/live_room.go:555-575）：
// 恒带 `state > UNSPECIFIED`，OwnerMid/AreaID 为 0 表示不过滤，State != UNSPECIFIED 才加等值条件。
func roomMatches(q model.RoomListQuery, r *model.LiveRoom) bool {
	if r.State <= model.RoomStateUnspecified {
		return false
	}
	if q.OwnerMid > 0 && r.OwnerMid != q.OwnerMid {
		return false
	}
	if q.AreaID > 0 && r.AreaID != q.AreaID {
		return false
	}
	if q.State != model.RoomStateUnspecified && r.State != q.State {
		return false
	}
	return true
}

// sortRooms 复刻 roomListOrder（model/live_room.go:579-589）。
func sortRooms(rows []*model.LiveRoom, order int32) {
	slices.SortStableFunc(rows, func(a, b *model.LiveRoom) int {
		switch order {
		case model.RoomOrderLivingFirst:
			if d := boolDesc(a.State == model.RoomStateLiving, b.State == model.RoomStateLiving); d != 0 {
				return d
			}
			return int64Desc(a.RoomID, b.RoomID)
		case model.RoomOrderCtimeDesc:
			if d := int64Desc(a.Ctime, b.Ctime); d != 0 {
				return d
			}
			return int64Desc(a.RoomID, b.RoomID)
		default:
			return int64Desc(a.RoomID, b.RoomID)
		}
	})
}

// int64Desc / boolDesc 把「倒序优先」的比较写成 sort 的 -1/0/1 约定。
func int64Desc(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	default:
		return 0
	}
}

func boolDesc(a, b bool) int {
	switch {
	case a && !b:
		return -1
	case !a && b:
		return 1
	default:
		return 0
	}
}

// limitRows 复刻 `LIMIT ?`（q.Limit <= 0 时 model 用 defaultListLimit）。
func limitRows[T any](rows []T, limit int32) []T {
	if limit <= 0 {
		limit = fakeDefaultListLimit
	}
	if int32(len(rows)) <= limit {
		return rows
	}
	return rows[:limit]
}

// offsetRows 复刻 `LIMIT ? OFFSET ?`。是否调用它**取决于该表真实 SQL 有没有 OFFSET 子句**，
// 见各 fake 的注释（live_room / live_room_anchor 没有，live_room_ban / live_area 有）。
func offsetRows[T any](rows []T, offset int32) []T {
	if offset <= 0 {
		return rows
	}
	if int(offset) >= len(rows) {
		return nil
	}
	return rows[offset:]
}

// --- 替身集合 + ServiceContext 装配 ---

// store 汇总 8 个 model 替身、连接替身、共享调用轨迹与下游假件。
type store struct {
	log  *callLog
	conn *fakeConn
	race *raceHooks

	rooms     *fakeRoomModel
	settings  *fakeSettingModel
	anchors   *fakeAnchorModel
	sessions  *fakeSessionModel
	bans      *fakeBanModel
	areas     *fakeAreaModel
	stateLogs *fakeStateLogModel
	idem      *fakeIdempotencyModel

	// mod 是 moderation 假客户端；只有在调用 st.wireModeration() 之后才装进 ServiceContext，
	// 未接线时 svcCtx.Moderation 是 nil —— 正是「本环境没接 moderation」的真实形态。
	mod             *fakeModerationClient
	moderationWired bool

	// creator / risk 同理：PrepareLive 的两个下游检查项分别读它们。
	// 不接线时是 nil，logic 走「未配置」降级分支（ErrCreatorNotConfigured /
	// ErrRiskControlNotConfigured），这正是契约里「未接入下游 = 不可评估」的形态。
	creator      *fakeCreatorClient
	creatorWired bool
	risk         *fakeRiskClient
	riskWired    bool
}

func newStore() *store {
	log := &callLog{}
	race := &raceHooks{}
	return &store{
		log:       log,
		conn:      &fakeConn{log: log},
		race:      race,
		rooms:     newFakeRoomModel(log, race),
		settings:  newFakeSettingModel(log, race),
		anchors:   newFakeAnchorModel(log, race),
		sessions:  newFakeSessionModel(log, race),
		bans:      newFakeBanModel(log, race),
		areas:     newFakeAreaModel(log, race),
		stateLogs: newFakeStateLogModel(log, race),
		idem:      newFakeIdempotencyModel(log, race),
		mod:       newFakeModerationClient(log),
		creator:   newFakeCreatorClient(log),
		risk:      newFakeRiskClient(log),
	}
}

// wireModeration 把假 moderation 客户端装进 ServiceContext（返回 store 自身便于链式）。
// 不写 callLog：它是装配动作而不是被测代码的调用。
func (st *store) wireModeration() *store {
	st.moderationWired = true
	return st
}

// wireCreator / wireRisk 同上：装配动作不写 callLog，
// 未接线时对应字段保持 nil，logic 的「下游未配置」降级分支才是可测形态。
func (st *store) wireCreator() *store {
	st.creatorWired = true
	return st
}

func (st *store) wireRisk() *store {
	st.riskWired = true
	return st
}

// raceBefore 布一个一次性并发钩子：在第 method 这次调用执行之前改库，模拟别的入口插队。
// 钩子若始终没触发，checkRaces 会让用例失败（前提没成立＝断言在空转）。
func (st *store) raceBefore(method string, fn func()) {
	st.race.arm(method, fn)
}

// checkRaces 在用例末尾调用，确认布下的并发钩子都被触发过。
func (st *store) checkRaces(t *testing.T) {
	t.Helper()
	st.race.assertFired(t, t.Name())
}

// liveRoomConfForTest 与 etc/live-room.yaml 的默认值逐字对齐
// （yaml 与 config tag 的一致性由 internal/config 的用例锁定）。
// MaxRoomsPerOwner 取 config tag 的 default=1；CacheRedis 留空 → Cache 字段为 nil。
func testLiveRoomConf() config.LiveRoomConf {
	return config.LiveRoomConf{
		DefaultListPageSize:     20,
		MaxListPageSize:         100,
		MaxAreaPageSize:         200,
		TitleMaxLength:          80,
		AreaNameMaxLength:       32,
		MaxRoomsPerOwner:        1,
		ModerationBusiness:      "live",
		RoomCacheTTLSeconds:     10,
		AreaListCacheTTLSeconds: 300,
	}
}

// testLiveRoomConfDo 在默认值上改个别开关，避免每个用例重抄整段配置。
func testLiveRoomConfDo(edit func(*config.LiveRoomConf)) config.LiveRoomConf {
	c := testLiveRoomConf()
	edit(&c)
	return c
}

// svcCtx 装配一个「下游 client 全 nil、Cache 全 nil」的上下文：
// 这正是「本环境未接入 creator / risk-control / moderation / Redis」的形态，
// 读侧 7 个方法在这种形态下必须照常工作（触达 nil client 会 nil panic，用例即红）。
func (st *store) svcCtx() *svc.ServiceContext {
	return st.svcCtxWith(testLiveRoomConf())
}

func (st *store) svcCtxWith(c config.LiveRoomConf) *svc.ServiceContext {
	sc := &svc.ServiceContext{
		Config:      config.Config{LiveRoom: c},
		DB:          st.conn,
		Cache:       nil,
		Rooms:       st.rooms,
		Settings:    st.settings,
		Anchors:     st.anchors,
		Sessions:    st.sessions,
		Bans:        st.bans,
		Areas:       st.areas,
		StateLogs:   st.stateLogs,
		Idempotency: st.idem,
		Creator:     nil,
		RiskControl: nil,
		Moderation:  nil,
	}
	if st.moderationWired {
		sc.Moderation = st.mod
	}
	if st.creatorWired {
		sc.Creator = st.creator
	}
	if st.riskWired {
		sc.RiskControl = st.risk
	}
	return sc
}

// --- 读回库存（静默：不经 model 接口、不写 callLog） ---

func (st *store) roomAt(t *testing.T, roomID int64) *model.LiveRoom {
	t.Helper()
	for _, r := range st.rooms.rows {
		if r.RoomID == roomID {
			return copyRoom(r)
		}
	}
	t.Fatalf("读回房间 %d：库里没有这一行（现有 %d 行）", roomID, len(st.rooms.rows))
	return nil
}

func (st *store) settingAt(t *testing.T, roomID int64) *model.LiveRoomSetting {
	t.Helper()
	for _, s := range st.settings.rows {
		if s.RoomID == roomID {
			cp := *s
			return &cp
		}
	}
	t.Fatalf("读回房间 %d 的配置行：库里没有", roomID)
	return nil
}

func (st *store) anchorRow(t *testing.T, roomID, mid int64, role int32) *model.LiveRoomAnchor {
	t.Helper()
	for _, a := range st.anchors.rows {
		if a.RoomID == roomID && a.Mid == mid && a.Role == role {
			cp := *a
			return &cp
		}
	}
	t.Fatalf("读回绑定行 room=%d mid=%d role=%d：库里没有（现有 %d 行）", roomID, mid, role, len(st.anchors.rows))
	return nil
}

func (st *store) sessionAt(t *testing.T, sessionID int64) *model.LiveSession {
	t.Helper()
	for _, s := range st.sessions.rows {
		if s.SessionID == sessionID {
			cp := *s
			return &cp
		}
	}
	t.Fatalf("读回场次 %d：库里没有", sessionID)
	return nil
}

func (st *store) banAt(t *testing.T, banID int64) *model.LiveRoomBan {
	t.Helper()
	for _, b := range st.bans.rows {
		if b.BanID == banID {
			cp := *b
			return &cp
		}
	}
	t.Fatalf("读回禁播记录 %d：库里没有（现有 %d 行）", banID, len(st.bans.rows))
	return nil
}

func (st *store) areaAt(t *testing.T, areaID int64) *model.LiveArea {
	t.Helper()
	for _, a := range st.areas.rows {
		if a.AreaID == areaID {
			cp := *a
			return &cp
		}
	}
	t.Fatalf("读回分区 %d：库里没有", areaID)
	return nil
}

// logsOf 读回某房间的全部审计行（log_id 升序＝写入顺序）。
func (st *store) logsOf(roomID int64) []*model.LiveRoomStateLog {
	var out []*model.LiveRoomStateLog
	for _, l := range st.stateLogs.rows {
		if l.RoomID == roomID {
			cp := *l
			out = append(out, &cp)
		}
	}
	slices.SortStableFunc(out, func(a, b *model.LiveRoomStateLog) int {
		return int(a.LogID - b.LogID)
	})
	return out
}

// idemAt 读回去重键那一行；不存在返回 nil（用例据此断言「键有没有被烧掉」）。
func (st *store) idemAt(dedupKey string) *model.LiveRoomIdempotency {
	for _, r := range st.idem.rows {
		if r.DedupKey == dedupKey {
			cp := *r
			return &cp
		}
	}
	return nil
}

// storeCounts 是「八张表各有多少行」的静默快照。
// 失败用例靠它断言**残留形态**（假件不回滚，所以中途失败到底留下几行是可观察的事实），
// 而不是写「应当已回滚」这种本文件证明不了的结论。
type storeCounts struct {
	rooms, settings, anchors, sessions, bans, areas, logs, idem int
}

func (st *store) counts() storeCounts {
	return storeCounts{
		rooms:    len(st.rooms.rows),
		settings: len(st.settings.rows),
		anchors:  len(st.anchors.rows),
		sessions: len(st.sessions.rows),
		bans:     len(st.bans.rows),
		areas:    len(st.areas.rows),
		logs:     len(st.stateLogs.rows),
		idem:     len(st.idem.rows),
	}
}

// wantKeyUnburned 断言某个 request_id 没有被 Claim 消费掉（守卫拒绝必须留得住重试资格）。
func wantKeyUnburned(t *testing.T, label, key string, st *store) {
	t.Helper()
	if rec := st.idemAt(key); rec != nil {
		t.Errorf("%s：request_id %q 已被 Claim 消费（kind=%d rpc=%q result_json=%q），守卫拒绝不该烧键",
			label, key, rec.Kind, rec.Rpc, rec.ResultJSON)
	}
}

// wantKeyBurned 断言键已登记且**没回填结果**——这是「重放拿不到结果」的形态
// （README 已知缺口里 request_id 被提前消费的那类缺陷）。
func wantKeyBurnedNoResult(t *testing.T, label, key string, st *store) {
	t.Helper()
	rec := st.idemAt(key)
	if rec == nil {
		t.Fatalf("%s：request_id %q 未登记（Claim 没执行？）", label, key)
	}
	if rec.ResultJSON != "" {
		t.Fatalf("%s：request_id %q 已回填结果 %q，本例要的是未回填形态", label, key, rec.ResultJSON)
	}
}

// --- seed（静默：不写 callLog，且必须布出生产 SQL 写得出来的行） ---

func (st *store) seedRoom(r *model.LiveRoom) *model.LiveRoom {
	switch {
	case r.RoomID <= 0:
		panic("seedRoom: room_id 必须显式给（live_room.room_id 是 AUTO_INCREMENT，布数据要模拟已分配的号）")
	case r.OwnerMid <= 0:
		panic("seedRoom: owner_mid 必须 > 0（CreateRoom 校验后才落库）")
	case r.Title == "":
		panic("seedRoom: title 非空（checkTitle 保证）")
	case !model.ValidRoomState(r.State):
		panic(fmt.Sprintf("seedRoom: state=%d 生产写不出来", r.State))
	case !model.ValidVerifyState(r.VerifyState):
		panic(fmt.Sprintf("seedRoom: verify_state=%d 生产写不出来", r.VerifyState))
	case !model.ValidPlatform(r.Platform):
		panic(fmt.Sprintf("seedRoom: platform=%d 生产写不出来（normalizePlatform 恒给 1..4）", r.Platform))
	case r.Ctime <= 0 || r.Mtime <= 0:
		panic("seedRoom: ctime/mtime 由 model.insert 写入，布数据不得留 0")
	}
	for _, exist := range st.rooms.rows {
		if exist.RoomID == r.RoomID {
			panic(fmt.Sprintf("seedRoom: room_id=%d 重复（PRIMARY KEY(room_id)）", r.RoomID))
		}
	}
	cp := copyRoom(r)
	st.rooms.rows = append(st.rooms.rows, cp)
	return cp
}

func (st *store) seedSetting(s *model.LiveRoomSetting) *model.LiveRoomSetting {
	switch {
	case s.RoomID <= 0:
		panic("seedSetting: room_id 是主键，必须显式给")
	case !model.ValidLiveType(s.LiveType):
		panic(fmt.Sprintf("seedSetting: live_type=%d 生产写不出来", s.LiveType))
	case s.Ctime <= 0 || s.Mtime <= 0:
		panic("seedSetting: ctime/mtime 由 Upsert 写入，布数据不得留 0")
	}
	for _, flag := range []*int32{&s.DanmakuEnabled, &s.ReplyEnabled, &s.RecordEnabled, &s.LinkmicEnabled} {
		if *flag != model.BoolToInt32(true) && *flag != model.BoolToInt32(false) {
			panic(fmt.Sprintf("seedSetting: 布尔列取值 %d 非法", *flag))
		}
	}
	for _, exist := range st.settings.rows {
		if exist.RoomID == s.RoomID {
			panic(fmt.Sprintf("seedSetting: room_id=%d 重复（PRIMARY KEY(room_id) 同时是 Upsert 冲突键）", s.RoomID))
		}
	}
	cp := *s
	st.settings.rows = append(st.settings.rows, &cp)
	return &cp
}

func (st *store) seedAnchor(a *model.LiveRoomAnchor) *model.LiveRoomAnchor {
	switch {
	case a.ID <= 0:
		panic("seedAnchor: id 是 AUTO_INCREMENT 主键，布数据必须显式给")
	case a.RoomID <= 0 || a.Mid <= 0:
		panic("seedAnchor: room_id / mid 必须 > 0（bind 前已校验）")
	case !model.ValidAnchorRole(a.Role):
		panic(fmt.Sprintf("seedAnchor: role=%d 生产写不出来", a.Role))
	case a.State != model.BindStateEnabled && a.State != model.BindStateDisabled:
		panic(fmt.Sprintf("seedAnchor: state=%d 非法", a.State))
	case a.Ctime <= 0 || a.Mtime <= 0:
		panic("seedAnchor: ctime/mtime 由 bind 写入，布数据不得留 0")
	}
	// uniq_active_owner 的语义：只有「生效房主」行写 owner_room_id = room_id，其余必须 NULL。
	if a.Role == model.AnchorRoleOwner && a.State == model.BindStateEnabled {
		if !a.OwnerRoomID.Valid || a.OwnerRoomID.Int64 != a.RoomID {
			panic(fmt.Sprintf("seedAnchor: 生效房主行的 owner_room_id 必须是 %d（model/live_room_anchor.go:115-119）", a.RoomID))
		}
	} else if a.OwnerRoomID.Valid {
		panic("seedAnchor: 非「生效房主」行的 owner_room_id 必须是 NULL，否则 uniq_active_owner 早冲突了")
	}
	for _, exist := range st.anchors.rows {
		if exist.ID == a.ID {
			panic(fmt.Sprintf("seedAnchor: id=%d 重复", a.ID))
		}
		if exist.RoomID == a.RoomID && exist.Mid == a.Mid && exist.Role == a.Role {
			panic("seedAnchor: 撞上 uniq_room_mid_role(room_id,mid,role)，bind 是覆盖不是插新行")
		}
		if a.Role == model.AnchorRoleOwner && a.State == model.BindStateEnabled &&
			exist.Role == model.AnchorRoleOwner && exist.State == model.BindStateEnabled &&
			exist.RoomID == a.RoomID {
			panic("seedAnchor: 一个房间只能有一条生效房主行（uniq_active_owner）")
		}
	}
	cp := *a
	st.anchors.rows = append(st.anchors.rows, &cp)
	return &cp
}

func (st *store) seedSession(s *model.LiveSession) *model.LiveSession {
	switch {
	case s.SessionID <= 0:
		panic("seedSession: session_id 是 AUTO_INCREMENT 主键，布数据必须显式给")
	case s.RoomID <= 0 || s.Mid <= 0:
		panic("seedSession: room_id / mid 必须 > 0（insert 前已校验，model/live_session.go）")
	case !model.ValidSessionState(s.State):
		panic(fmt.Sprintf("seedSession: state=%d 生产写不出来", s.State))
	case !model.ValidReplayState(s.ReplayState):
		panic(fmt.Sprintf("seedSession: replay_state=%d 生产写不出来", s.ReplayState))
	case s.Ctime <= 0 || s.Mtime <= 0:
		panic("seedSession: ctime/mtime 由 insert 写入，布数据不得留 0")
	case model.SessionStateIsTerminal(s.State) && s.EndedAt <= 0:
		panic("seedSession: 终态场次的 ended_at 由 Transition 的同条 UPDATE 写入，不能留 0")
	case !model.SessionStateIsTerminal(s.State) && s.EndedAt != 0:
		panic("seedSession: 非终态场次不能有 ended_at")
	}
	for _, exist := range st.sessions.rows {
		if exist.SessionID == s.SessionID {
			panic(fmt.Sprintf("seedSession: session_id=%d 重复", s.SessionID))
		}
	}
	cp := *s
	st.sessions.rows = append(st.sessions.rows, &cp)
	return &cp
}

func (st *store) seedBan(b *model.LiveRoomBan) *model.LiveRoomBan {
	switch {
	case b.BanID <= 0:
		panic("seedBan: ban_id 是 AUTO_INCREMENT 主键，布数据必须显式给")
	case b.RoomID <= 0:
		panic("seedBan: room_id 必须 > 0")
	case b.OperatorMid <= 0:
		panic("seedBan: operator_mid 必须 > 0（insert 前的守卫）")
	case !model.ValidBanState(b.State):
		panic(fmt.Sprintf("seedBan: state=%d 生产写不出来", b.State))
	case b.Ctime <= 0:
		panic("seedBan: ctime 由 insert 写入（本表无 mtime，append-only）")
	}
	switch b.BanType {
	case model.BanTypePermanent:
		if b.EndAt != 0 {
			panic("seedBan: 永久禁播的 end_at 恒为 0（model/live_room_ban.go:insert）")
		}
	case model.BanTypeTemporary:
		if b.StartAt <= 0 || b.EndAt <= b.StartAt {
			panic("seedBan: 临时禁播必须 end_at > start_at > 0")
		}
	default:
		panic(fmt.Sprintf("seedBan: ban_type=%d 生产写不出来", b.BanType))
	}
	if b.State == model.BanStateLifted && (b.LiftOperatorMid <= 0 || b.LiftedAt <= 0) {
		panic("seedBan: 已解除记录必须有 lift_operator_mid / lifted_at（Lift 写入）")
	}
	for _, exist := range st.bans.rows {
		if exist.BanID == b.BanID {
			panic(fmt.Sprintf("seedBan: ban_id=%d 重复", b.BanID))
		}
	}
	cp := *b
	st.bans.rows = append(st.bans.rows, &cp)
	return &cp
}

func (st *store) seedArea(a *model.LiveArea) *model.LiveArea {
	switch {
	case a.AreaID <= 0:
		panic("seedArea: area_id 是 AUTO_INCREMENT 主键，布数据必须显式给")
	case strings.TrimSpace(a.AreaName) == "":
		panic("seedArea: area_name 非空（Insert 守卫）")
	case a.State != model.AreaStateEnabled && a.State != model.AreaStateDisabled:
		panic(fmt.Sprintf("seedArea: state=%d 生产写不出来", a.State))
	case a.OperatorMid <= 0:
		panic("seedArea: operator_mid 必须 > 0（Insert 守卫）")
	case a.Ctime <= 0 || a.Mtime <= 0:
		panic("seedArea: ctime/mtime 由 Insert/Update 写入")
	}
	for _, exist := range st.areas.rows {
		if exist.AreaID == a.AreaID {
			panic(fmt.Sprintf("seedArea: area_id=%d 重复", a.AreaID))
		}
		if exist.AreaName == a.AreaName {
			panic(fmt.Sprintf("seedArea: area_name=%q 撞上 uniq_area_name", a.AreaName))
		}
	}
	cp := *a
	st.areas.rows = append(st.areas.rows, &cp)
	return &cp
}

// seedIdem 直接布一个「已被某次执行 Claim 过」的去重键，用来测重放路径：
// resultJSON 传空串表示**首次执行还没回填结果**（真实存在于「Claim 成功但业务失败」之后）。
// 静默不写 callLog，所以序列断言里出现的 Claim/Find 都是被测代码的调用。
func (st *store) seedIdem(key, rpcName, resultJSON string) *model.LiveRoomIdempotency {
	if key == "" {
		panic("seedIdem: dedup_key 非空（Claim 守卫）")
	}
	for _, r := range st.idem.rows {
		if r.DedupKey == key {
			panic(fmt.Sprintf("seedIdem: dedup_key=%q 重复（uniq_dedup_key）", key))
		}
	}
	row := &model.LiveRoomIdempotency{
		DedupKey:   key,
		Kind:       model.IdempotencyKindRequest,
		Rpc:        rpcName,
		ResultJSON: resultJSON,
		Ctime:      7000,
		Mtime:      7000,
	}
	row.ID = nextFakePK(st.idem.rows, func(x *model.LiveRoomIdempotency) int64 { return x.ID })
	st.idem.rows = append(st.idem.rows, row)
	return row
}

// --- live_room 替身 ---

type fakeRoomModel struct {
	faultInjector
	rows []*model.LiveRoom

	// 捕获查询条件：List 与 Count 必须收到**同一个** RoomListQuery（total 与页内容同口径）。
	listQueries      []model.RoomListQuery
	countQueries     []model.RoomListQuery
	listByIDsQueries []roomIDsCall
	// countOwnerCalls / countAreaCalls 捕获上限与占用校验的 states 集合：
	// 「计入哪些状态」是策略（FINISHED 算不算额度、FINISHED 算不算占用），只能读回参数来断言。
	countOwnerCalls []countStatesCall
	countAreaCalls  []countStatesCall
}

type roomIDsCall struct {
	ids []int64
	q   model.RoomListQuery
}

type countStatesCall struct {
	value  int64
	states []int32
}

func newFakeRoomModel(log *callLog, race *raceHooks) *fakeRoomModel {
	return &fakeRoomModel{faultInjector: newFault("live_room", log, race)}
}

// Insert 复刻 model/live_room.go:144-180。
// 真实 SQL 写入全部 16 列（除主键），并且：
//   - ctime 仅在调用方给 0 时补 now，**mtime 恒被覆盖成 now**（生产代码 `r.Mtime = now`
//     无条件执行，且它改的就是调用方传进来的那个结构体——假件照抄这个副作用）；
//   - 不做任何业务校验（没有 title/state 守卫），所以 logic 的校验必须在触库前完成；
//   - room_id 是 AUTO_INCREMENT，由本方法分配。
func (f *fakeRoomModel) Insert(_ context.Context, r *model.LiveRoom) (int64, error) {
	f.log.add("live_room.Insert")
	if err := f.dbErr("Insert"); err != nil {
		return 0, fmt.Errorf("live_room Insert: %w", err)
	}
	return f.insert(r)
}

func (f *fakeRoomModel) InsertTx(_ context.Context, session sqlx.Session, r *model.LiveRoom) (int64, error) {
	if session == nil {
		// 真实 InsertTx 在 session==nil 时**退化为连接版本**（model/live_room.go:150-155）：
		// 键里就会露出 live_room.Insert，事务内的写被降级成事务外，序列断言会当场抓到。
		return f.Insert(context.Background(), r)
	}
	f.log.add("live_room.InsertTx")
	if err := f.dbErr("InsertTx"); err != nil {
		return 0, fmt.Errorf("live_room Insert: %w", err)
	}
	return f.insert(r)
}

func (f *fakeRoomModel) insert(r *model.LiveRoom) (int64, error) {
	now := model.NowUnix()
	if r.Ctime == 0 {
		r.Ctime = now
	}
	r.Mtime = now
	cp := copyRoom(r)
	cp.RoomID = nextFakePK(f.rows, func(x *model.LiveRoom) int64 { return x.RoomID })
	f.rows = append(f.rows, cp)
	return cp.RoomID, nil
}

// FindOne 复刻 model/live_room.go:182-192：无记录返回 (nil, nil)，不校验 room_id。
func (f *fakeRoomModel) FindOne(_ context.Context, roomID int64) (*model.LiveRoom, error) {
	f.log.add("live_room.FindOne:%d", roomID)
	f.fireRace("live_room.FindOne")
	if err := f.dbErr("FindOne"); err != nil {
		return nil, err
	}
	for _, r := range f.rows {
		if r.RoomID == roomID {
			return copyRoom(r), nil
		}
	}
	return nil, nil
}

// ListByOwner 复刻 model/live_room.go:194-211：ownerMid<=0 直接 ErrInvalidMid；
// 排除 FINISHED；room_id 倒序；limit<=0 用兜底值。键里带上 limit，
// 好让用例能钉住「GetRoom 只要一条」。
func (f *fakeRoomModel) ListByOwner(_ context.Context, ownerMid int64, limit int32) ([]*model.LiveRoom, error) {
	f.log.add("live_room.ListByOwner:%d/%d", ownerMid, limit)
	if err := f.dbErr("ListByOwner"); err != nil {
		return nil, err
	}
	if ownerMid <= 0 {
		return nil, model.ErrInvalidMid
	}
	var rows []*model.LiveRoom
	for _, r := range f.rows {
		if r.OwnerMid == ownerMid && r.State != model.RoomStateFinished {
			rows = append(rows, copyRoom(r))
		}
	}
	sortRooms(rows, model.RoomOrderIDDesc)
	return limitRows(rows, limit), nil
}

// List 复刻 model/live_room.go:213-230。
// 注意：**真实 SQL 只有 LIMIT，没有 OFFSET**，所以 q.Offset 传进来也不生效——
// 这是缺陷（logic 已算出 offset 并塞进 q，翻页拿到的还是同一页），假件照抄不补。
func (f *fakeRoomModel) List(_ context.Context, q model.RoomListQuery) ([]*model.LiveRoom, error) {
	f.log.add("live_room.List")
	f.listQueries = append(f.listQueries, q)
	if err := f.dbErr("List"); err != nil {
		return nil, err
	}
	rows := f.filter(q, nil)
	sortRooms(rows, q.Order)
	return limitRows(rows, q.Limit), nil
}

// Count 与 List 共用同一个过滤构造器（model/live_room.go:233-244）。同样没有 OFFSET。
func (f *fakeRoomModel) Count(_ context.Context, q model.RoomListQuery) (int64, error) {
	f.log.add("live_room.Count")
	f.countQueries = append(f.countQueries, q)
	if err := f.dbErr("Count"); err != nil {
		return 0, err
	}
	return int64(len(f.filter(q, nil))), nil
}

// CountByOwner 复刻 model/live_room.go:246-251 + countByStates:
// ownerMid<=0 先 ErrInvalidMid；states 为空表示不限状态（真 SQL 不拼 IN 子句）。
// 捕获 states 集合供「FINISHED 是否计入额度」这类策略断言。
func (f *fakeRoomModel) CountByOwner(_ context.Context, ownerMid int64, states []int32) (int64, error) {
	f.log.add("live_room.CountByOwner:%d", ownerMid)
	f.countOwnerCalls = append(f.countOwnerCalls, countStatesCall{value: ownerMid, states: slices.Clone(states)})
	if err := f.dbErr("CountByOwner"); err != nil {
		return 0, err
	}
	if ownerMid <= 0 {
		return 0, model.ErrInvalidMid
	}
	return f.countBy("owner_mid", ownerMid, states), nil
}

// CountByArea 复刻 model/live_room.go:253-258：areaID<=0 先 ErrInvalidAreaID。
func (f *fakeRoomModel) CountByArea(_ context.Context, areaID int64, states []int32) (int64, error) {
	f.log.add("live_room.CountByArea:%d", areaID)
	f.countAreaCalls = append(f.countAreaCalls, countStatesCall{value: areaID, states: slices.Clone(states)})
	if err := f.dbErr("CountByArea"); err != nil {
		return 0, err
	}
	if areaID <= 0 {
		return 0, model.ErrInvalidAreaID
	}
	return f.countBy("area_id", areaID, states), nil
}

// countBy 复刻 countByStates（model/live_room.go:261-281）。
func (f *fakeRoomModel) countBy(column string, value int64, states []int32) int64 {
	var n int64
	for _, r := range f.rows {
		if column == "owner_mid" && r.OwnerMid != value {
			continue
		}
		if column == "area_id" && r.AreaID != value {
			continue
		}
		if len(states) > 0 && !slices.Contains(states, r.State) {
			continue
		}
		n++
	}
	return n
}

func (f *fakeRoomModel) Transition(_ context.Context, roomID int64, from, to, expectVersion int32,
	patch model.RoomPatch) (bool, error) {
	return f.transition("live_room.Transition", "Transition", roomID, from, to, expectVersion, patch)
}

func (f *fakeRoomModel) TransitionTx(_ context.Context, session sqlx.Session, roomID int64,
	from, to, expectVersion int32, patch model.RoomPatch) (bool, error) {
	if session == nil {
		// 与真实实现一致：退化为连接版本（键里露出非 Tx 的那条）。
		return f.Transition(context.Background(), roomID, from, to, expectVersion, patch)
	}
	return f.transition("live_room.TransitionTx", "TransitionTx", roomID, from, to, expectVersion, patch)
}

// transition 复刻 model/live_room.go:283-317：
//  1. `CanRoomTransition(from,to)` 是**发 SQL 之前**的守卫，非法边直接 ErrInvalidRoomTransition；
//  2. WHERE `room_id AND state=from [AND state_version=expect]`，expectVersion<=0 不校验版本；
//  3. 命中才写 state=to、state_version+1、mtime=now，并把 patch 里的列并进**同一条** UPDATE。
func (f *fakeRoomModel) transition(key, method string, roomID int64, from, to, expectVersion int32,
	patch model.RoomPatch) (bool, error) {
	if !model.CanRoomTransition(from, to) {
		f.log.add("%s:%d:%d->%d:GUARD", key, roomID, from, to)
		return false, model.ErrInvalidRoomTransition
	}
	f.log.add("%s:%d:%d->%d/v%d", key, roomID, from, to, expectVersion)
	f.fireRace(fmt.Sprintf("%s:%d:%d->%d", key, roomID, from, to))
	if err := f.dbErr(method); err != nil {
		return false, fmt.Errorf("live_room Transition: %w", err)
	}
	row := f.match(roomID, from, expectVersion)
	if row == nil {
		return false, nil
	}
	now := model.NowUnix()
	row.State = to
	row.StateVersion++
	row.Mtime = now
	applyRoomPatch(row, patch)
	return true, nil
}

// match 按 CAS 条件找行；找不到返回 nil（调用方据此回 false）。
func (f *fakeRoomModel) match(roomID int64, from, expectVersion int32) *model.LiveRoom {
	for _, r := range f.rows {
		if r.RoomID != roomID || r.State != from {
			continue
		}
		if expectVersion > 0 && r.StateVersion != expectVersion {
			continue
		}
		return r
	}
	return nil
}

// applyRoomPatch 把 RoomPatch 的非 nil 列写进行内（model/live_room.go:patch.setClauses 的等价）。
func applyRoomPatch(row *model.LiveRoom, patch model.RoomPatch) {
	if patch.VerifyState != nil {
		row.VerifyState = *patch.VerifyState
	}
	if patch.RejectReason != nil {
		row.RejectReason = *patch.RejectReason
	}
	if patch.BanUntil != nil {
		row.BanUntil = *patch.BanUntil
	}
	if patch.ActiveSessionID != nil {
		row.ActiveSessionID = *patch.ActiveSessionID
	}
	if patch.ActiveStreamID != nil {
		row.ActiveStreamID = *patch.ActiveStreamID
	}
	if patch.ModerationTaskID != nil {
		row.ModerationTaskID = *patch.ModerationTaskID
	}
}

// UpdateProfile 复刻 model/live_room.go:319-370。
// 关键事实（都是用例要锁的）：
//   - allowStates 为空**先**返回 ErrRoomStateNotEditable，不发 SQL；
//   - 空串/0 表示该列不改；verify_state 非 UNSPECIFIED 时顺带清 reject_reason；
//   - 这条 UPDATE 里**没有 state_version**：改资料不推进状态版本，
//     所以「改完资料 state_version 还是原值」是契约，不是巧合；
//   - WHERE `room_id AND state IN (allowStates)`，命中 0 行返回 false。
func (f *fakeRoomModel) UpdateProfile(_ context.Context, roomID int64, title, cover string, areaID int64,
	verifyState int32, moderationTaskID int64, allowStates []int32) (bool, error) {
	f.log.add("live_room.UpdateProfile:%d:v%d/t%d", roomID, verifyState, moderationTaskID)
	f.fireRace("live_room.UpdateProfile")
	if len(allowStates) == 0 {
		return false, model.ErrRoomStateNotEditable
	}
	if err := f.dbErr("UpdateProfile"); err != nil {
		return false, fmt.Errorf("live_room UpdateProfile: %w", err)
	}
	var row *model.LiveRoom
	for _, r := range f.rows {
		if r.RoomID == roomID && slices.Contains(allowStates, r.State) {
			row = r
			break
		}
	}
	if row == nil {
		return false, nil
	}
	if title != "" {
		row.Title = title
	}
	if cover != "" {
		row.Cover = cover
	}
	if areaID > 0 {
		row.AreaID = areaID
	}
	if verifyState != model.VerifyStateUnspecified {
		row.VerifyState = verifyState
		row.RejectReason = ""
	}
	if moderationTaskID > 0 {
		row.ModerationTaskID = moderationTaskID
	}
	row.Mtime = model.NowUnix()
	return true, nil
}

// ClearActiveSession 复刻 model/live_room.go:369-382：
// `SET active_session_id=0, active_stream_id=”, mtime=? WHERE room_id=? AND active_session_id=?`。
// 两处细节是用例要锁的：这条 UPDATE **不推进 state_version**（挂机位不是状态迁移），
// 且 WHERE 带 session_id 守卫 —— 已经切到别的活跃会话时它返回 false，不改行。
func (f *fakeRoomModel) ClearActiveSession(_ context.Context, roomID, sessionID int64) (bool, error) {
	f.log.add("live_room.ClearActiveSession:%d/s%d", roomID, sessionID)
	f.fireRace("live_room.ClearActiveSession")
	if err := f.dbErr("ClearActiveSession"); err != nil {
		return false, fmt.Errorf("live_room ClearActiveSession: %w", err)
	}
	for _, r := range f.rows {
		if r.RoomID == roomID && r.ActiveSessionID == sessionID {
			r.ActiveSessionID = 0
			r.ActiveStreamID = ""
			r.Mtime = model.NowUnix()
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRoomModel) ListBansToExpire(context.Context, int64, int32) ([]*model.LiveRoom, error) {
	return nil, f.unexpected("ListBansToExpire")
}

func (f *fakeRoomModel) SetVerifyResult(ctx context.Context, roomID int64, verifyState int32,
	rejectReason string, allowStates []int32) (bool, error) {
	return f.setVerifyResult("live_room.SetVerifyResult", "SetVerifyResult",
		roomID, verifyState, rejectReason, allowStates)
}

func (f *fakeRoomModel) SetVerifyResultTx(_ context.Context, session sqlx.Session, roomID int64,
	verifyState int32, rejectReason string, allowStates []int32) (bool, error) {
	if session == nil {
		return f.SetVerifyResult(context.Background(), roomID, verifyState, rejectReason, allowStates)
	}
	return f.setVerifyResult("live_room.SetVerifyResultTx", "SetVerifyResultTx",
		roomID, verifyState, rejectReason, allowStates)
}

// setVerifyResult 复刻 model/live_room.go:403-444。三条守卫（roomID>0、
// ValidVerifyState、allowStates 非空）都在 SQL 之前；与 setBanUntil 的**关键差异**是
// model 在这里不逐个校验 allowStates 的取值，假件照抄（不替生产补校验）。
// 命中条件 `room_id AND state IN (allowStates)`，写 verify_state/reject_reason/mtime
// 三列，**不推进 state_version**（结论回写不是业务状态迁移）。
func (f *fakeRoomModel) setVerifyResult(key, method string, roomID int64, verifyState int32,
	rejectReason string, allowStates []int32) (bool, error) {
	f.log.add("%s:%d:%d", key, roomID, verifyState)
	f.fireRace(key)
	if roomID <= 0 {
		return false, model.ErrInvalidRoomID
	}
	if !model.ValidVerifyState(verifyState) {
		return false, model.ErrInvalidVerifyTransition
	}
	if len(allowStates) == 0 {
		return false, model.ErrRoomStateNotEditable
	}
	if err := f.dbErr(method); err != nil {
		return false, fmt.Errorf("live_room SetVerifyResult: %w", err)
	}
	for _, r := range f.rows {
		if r.RoomID == roomID && slices.Contains(allowStates, r.State) {
			r.VerifyState = verifyState
			r.RejectReason = rejectReason
			r.Mtime = model.NowUnix()
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRoomModel) SetBanUntil(ctx context.Context, roomID, banUntil int64, allowStates []int32) (bool, error) {
	return f.setBanUntil("live_room.SetBanUntil", "SetBanUntil", roomID, banUntil, allowStates)
}

func (f *fakeRoomModel) SetBanUntilTx(_ context.Context, session sqlx.Session, roomID, banUntil int64,
	allowStates []int32) (bool, error) {
	if session == nil {
		return f.SetBanUntil(context.Background(), roomID, banUntil, allowStates)
	}
	return f.setBanUntil("live_room.SetBanUntilTx", "SetBanUntilTx", roomID, banUntil, allowStates)
}

// setBanUntil 复刻 model/live_room.go:452-486：三条守卫（roomID / banUntil>=0 /
// allowStates 非空且逐个是合法房间状态）都在 SQL 之前，命中条件 `room_id AND state IN (...)`。
func (f *fakeRoomModel) setBanUntil(key, method string, roomID, banUntil int64, allowStates []int32) (bool, error) {
	f.log.add("%s:%d:%d", key, roomID, banUntil)
	f.fireRace(key)
	if roomID <= 0 {
		return false, model.ErrInvalidRoomID
	}
	if banUntil < 0 {
		return false, model.ErrBanDurationRequired
	}
	if len(allowStates) == 0 {
		return false, model.ErrRoomStateNotEditable
	}
	for _, s := range allowStates {
		if !model.ValidRoomState(s) {
			return false, model.ErrInvalidRoomTransition
		}
	}
	if err := f.dbErr(method); err != nil {
		return false, fmt.Errorf("live_room SetBanUntil: %w", err)
	}
	for _, r := range f.rows {
		if r.RoomID == roomID && slices.Contains(allowStates, r.State) {
			r.BanUntil = banUntil
			r.Mtime = model.NowUnix()
			return true, nil
		}
	}
	return false, nil
}

// ListByRoomIDs 复刻 model/live_room.go:494-521：ids 为空返回空页（不是全表）；
// 过滤与排序复用 RoomListQuery；同样只有 LIMIT，没有 OFFSET。
func (f *fakeRoomModel) ListByRoomIDs(_ context.Context, ids []int64, q model.RoomListQuery) ([]*model.LiveRoom, error) {
	f.log.add("live_room.ListByRoomIDs:n=%d", len(ids))
	f.listByIDsQueries = append(f.listByIDsQueries, roomIDsCall{ids: slices.Clone(ids), q: q})
	if err := f.dbErr("ListByRoomIDs"); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows := f.filter(q, ids)
	sortRooms(rows, q.Order)
	return limitRows(rows, q.Limit), nil
}

// filter 将 rows 收敛到 q 的 WHERE 口径；idSet 非 nil 时再加 room_id IN (...)。
func (f *fakeRoomModel) filter(q model.RoomListQuery, idSet []int64) []*model.LiveRoom {
	var rows []*model.LiveRoom
	for _, r := range f.rows {
		if !roomMatches(q, r) {
			continue
		}
		if idSet != nil && !slices.Contains(idSet, r.RoomID) {
			continue
		}
		rows = append(rows, copyRoom(r))
	}
	return rows
}

var _ model.LiveRoomModel = (*fakeRoomModel)(nil)

// --- live_room_setting 替身 ---

type fakeSettingModel struct {
	faultInjector
	rows []*model.LiveRoomSetting
}

func newFakeSettingModel(log *callLog, race *raceHooks) *fakeSettingModel {
	return &fakeSettingModel{faultInjector: newFault("live_room_setting", log, race)}
}

// Upsert / UpsertTx 复刻 model/live_room_setting.go:68-110：
// `INSERT ... ON DUPLICATE KEY UPDATE` 冲突键是 PRIMARY KEY(room_id)，
// 更新分支写 danmaku/reply/record/linkmic/live_type/min_client_version_code/mtime，
// **不写 ctime**（首次创建时间必须留住，这是本方法最容易被写错的一条）。
// 三条守卫（room_id / live_type / min_client_version_code>=0）都在 SQL 之前。
func (f *fakeSettingModel) Upsert(_ context.Context, s *model.LiveRoomSetting) error {
	f.log.add("live_room_setting.Upsert:%d", s.RoomID)
	return f.upsert("Upsert", s)
}

func (f *fakeSettingModel) UpsertTx(_ context.Context, session sqlx.Session, s *model.LiveRoomSetting) error {
	if session == nil {
		return f.Upsert(context.Background(), s)
	}
	f.log.add("live_room_setting.UpsertTx:%d", s.RoomID)
	return f.upsert("UpsertTx", s)
}

func (f *fakeSettingModel) upsert(method string, s *model.LiveRoomSetting) error {
	f.fireRace("live_room_setting." + method)
	if s.RoomID <= 0 {
		return model.ErrInvalidRoomID
	}
	if !model.ValidLiveType(s.LiveType) {
		return model.ErrSettingInvalid
	}
	if s.MinClientVersionCode < 0 {
		return model.ErrSettingInvalid
	}
	if err := f.dbErr(method); err != nil {
		return fmt.Errorf("live_room_setting Upsert: %w", err)
	}
	now := model.NowUnix()
	for _, row := range f.rows {
		if row.RoomID != s.RoomID {
			continue
		}
		row.DanmakuEnabled = s.DanmakuEnabled
		row.ReplyEnabled = s.ReplyEnabled
		row.RecordEnabled = s.RecordEnabled
		row.LinkmicEnabled = s.LinkmicEnabled
		row.LiveType = s.LiveType
		row.MinClientVersionCode = s.MinClientVersionCode
		row.Mtime = now // ctime 保持原值
		s.Mtime = now
		return nil
	}
	cp := *s
	if cp.Ctime == 0 {
		cp.Ctime = now
	}
	cp.Mtime = now
	f.rows = append(f.rows, &cp)
	s.Ctime, s.Mtime = cp.Ctime, cp.Mtime
	return nil
}

// FindOne 复刻 model/live_room_setting.go:112-125：roomID<=0 先 ErrInvalidRoomID，
// 无记录返回 (nil, nil)——
// 「房间没写过配置」不是错误，由 logic 套服务端默认。
func (f *fakeSettingModel) FindOne(_ context.Context, roomID int64) (*model.LiveRoomSetting, error) {
	f.log.add("live_room_setting.FindOne:%d", roomID)
	if err := f.dbErr("FindOne"); err != nil {
		return nil, err
	}
	if roomID <= 0 {
		return nil, model.ErrInvalidRoomID
	}
	for _, s := range f.rows {
		if s.RoomID == roomID {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

// RecordEnabledFor 复刻 model/live_room_setting.go:127-136。真实实现内部调 FindOne，
// 但这里记**自己的键**（同 FindActiveByRoom 的理由：免得用例把「logic 调了哪个方法」
// 读成「调了另一个方法」）。无配置行时真实实现返回 ErrNoSettingRow —— 与 FindOne 的
// (nil,nil) 不同，这个差别正是用例要锁的。
func (f *fakeSettingModel) RecordEnabledFor(_ context.Context, roomID int64) (bool, error) {
	f.log.add("live_room_setting.RecordEnabledFor:%d", roomID)
	f.fireRace("live_room_setting.RecordEnabledFor")
	if err := f.dbErr("RecordEnabledFor"); err != nil {
		return false, fmt.Errorf("live_room_setting RecordEnabledFor: %w", err)
	}
	if roomID <= 0 {
		return false, model.ErrInvalidRoomID
	}
	for _, s := range f.rows {
		if s.RoomID == roomID {
			return model.Int32ToBool(s.RecordEnabled), nil
		}
	}
	return false, model.ErrNoSettingRow
}

var _ model.LiveRoomSettingModel = (*fakeSettingModel)(nil)

// --- live_room_anchor 替身 ---

type fakeAnchorModel struct {
	faultInjector
	rows []*model.LiveRoomAnchor

	listQueries  []model.AnchorListQuery
	countQueries []model.AnchorListQuery
}

func newFakeAnchorModel(log *callLog, race *raceHooks) *fakeAnchorModel {
	return &fakeAnchorModel{faultInjector: newFault("live_room_anchor", log, race)}
}

// Bind / BindTx 复刻 model/live_room_anchor.go:99-142：
// `INSERT ... ON DUPLICATE KEY UPDATE state/owner_room_id/mtime`，冲突键有两个
// （uniq_room_mid_role 与 uniq_active_owner），语义完全不同：
//   - 撞 uniq_room_mid_role：覆盖同一行的 state/owner_room_id/mtime，RowsAffected=2 →
//     真实代码 `aff != 1` 时**返回 0**（不返回自增号），假件照抄；
//   - 撞 uniq_active_owner（同房间已有别的生效房主）：驱动报 1062 → 翻译成 ErrDuplicateOwner；
//   - owner_room_id 占位列只在 role=OWNER 时写值，其余写 NULL（解绑要能释放占位）。
func (f *fakeAnchorModel) Bind(_ context.Context, roomID, mid int64, role int32) (int64, error) {
	f.log.add("live_room_anchor.Bind:m%d", mid)
	return f.bind("Bind", roomID, mid, role)
}

func (f *fakeAnchorModel) BindTx(_ context.Context, session sqlx.Session, roomID, mid int64, role int32) (int64, error) {
	if session == nil {
		return f.Bind(context.Background(), roomID, mid, role)
	}
	f.log.add("live_room_anchor.BindTx:m%d", mid)
	return f.bind("BindTx", roomID, mid, role)
}

func (f *fakeAnchorModel) bind(method string, roomID, mid int64, role int32) (int64, error) {
	f.fireRace("live_room_anchor." + method)
	if roomID <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	if mid <= 0 {
		return 0, model.ErrInvalidMid
	}
	if !model.ValidAnchorRole(role) {
		return 0, model.ErrAnchorRoleInvalid
	}
	if err := f.dbErr(method); err != nil {
		return 0, fmt.Errorf("live_room_anchor Bind: %w", err)
	}
	now := model.NowUnix()
	var owner sql.NullInt64
	if role == model.AnchorRoleOwner {
		owner = sql.NullInt64{Int64: roomID, Valid: true}
	}
	// uniq_active_owner：别的 mid 已持有该房间的生效房主 → 1062。
	if role == model.AnchorRoleOwner {
		for _, a := range f.rows {
			if a.RoomID == roomID && a.Role == model.AnchorRoleOwner && a.State == model.BindStateEnabled &&
				a.Mid != mid {
				return 0, model.ErrDuplicateOwner
			}
		}
	}
	for _, a := range f.rows {
		if a.RoomID == roomID && a.Mid == mid && a.Role == role {
			a.State = model.BindStateEnabled
			a.OwnerRoomID = owner
			a.Mtime = now
			return 0, nil // ON DUPLICATE 更新分支：aff!=1 → 真实实现返回 0
		}
	}
	row := &model.LiveRoomAnchor{
		ID:          nextFakePK(f.rows, func(x *model.LiveRoomAnchor) int64 { return x.ID }),
		RoomID:      roomID,
		Mid:         mid,
		Role:        role,
		State:       model.BindStateEnabled,
		OwnerRoomID: owner,
		Ctime:       now,
		Mtime:       now,
	}
	f.rows = append(f.rows, row)
	return row.ID, nil
}

// FindOwner 复刻 model/live_room_anchor.go:144-155：`room_id AND role=1 AND state=1 LIMIT 1`，
// 无记录返回 (nil, nil)。
func (f *fakeAnchorModel) FindOwner(_ context.Context, roomID int64) (*model.LiveRoomAnchor, error) {
	f.log.add("live_room_anchor.FindOwner:%d", roomID)
	f.fireRace("live_room_anchor.FindOwner")
	if err := f.dbErr("FindOwner"); err != nil {
		return nil, err
	}
	var found *model.LiveRoomAnchor
	for _, a := range f.rows {
		if a.RoomID != roomID || a.Role != model.AnchorRoleOwner || a.State != model.BindStateEnabled {
			continue
		}
		if found == nil || a.ID < found.ID {
			cp := *a
			found = &cp
		}
	}
	return found, nil
}

// Find 复刻 model/live_room_anchor.go:157-169：按 (room_id, mid, role) 精确一行，无记录 (nil, nil)。
func (f *fakeAnchorModel) Find(_ context.Context, roomID, mid int64, role int32) (*model.LiveRoomAnchor, error) {
	f.log.add("live_room_anchor.Find:%d/%d", roomID, mid)
	if err := f.dbErr("Find"); err != nil {
		return nil, err
	}
	for _, a := range f.rows {
		if a.RoomID == roomID && a.Mid == mid && a.Role == role {
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}

// IsEnabled 复刻 model/live_room_anchor.go:171-187：
// roomID/mid 任一 <=0 先 ErrInvalidMid；`state=1` 的行里 role 升序取第一条（房主优先）。
func (f *fakeAnchorModel) IsEnabled(_ context.Context, roomID, mid int64) (bool, int32, error) {
	f.log.add("live_room_anchor.IsEnabled:%d/%d", roomID, mid)
	f.fireRace("live_room_anchor.IsEnabled")
	if err := f.dbErr("IsEnabled"); err != nil {
		return false, 0, err
	}
	if roomID <= 0 || mid <= 0 {
		return false, 0, model.ErrInvalidMid
	}
	best := int32(0)
	for _, a := range f.rows {
		if a.RoomID != roomID || a.Mid != mid || a.State != model.BindStateEnabled {
			continue
		}
		if best == 0 || a.Role < best {
			best = a.Role
		}
	}
	if best == 0 {
		return false, 0, nil
	}
	return true, best, nil
}

// List 复刻 model/live_room_anchor.go:185-205：RoomID<=0 先报 ErrInvalidRoomID；
// ORDER BY role ASC, id ASC；**真实 SQL 没有 OFFSET**（AnchorListQuery.Offset 不生效，缺陷）。
func (f *fakeAnchorModel) List(_ context.Context, q model.AnchorListQuery) ([]*model.LiveRoomAnchor, error) {
	f.log.add("live_room_anchor.List:%d", q.RoomID)
	f.listQueries = append(f.listQueries, q)
	if err := f.dbErr("List"); err != nil {
		return nil, err
	}
	if q.RoomID <= 0 {
		return nil, model.ErrInvalidRoomID
	}
	rows := f.filter(q)
	slices.SortStableFunc(rows, func(a, b *model.LiveRoomAnchor) int {
		if a.Role != b.Role {
			return int(a.Role) - int(b.Role)
		}
		// model/live_room_anchor.go:194 是 `ORDER BY role ASC, id ASC`（同角色按 id 升序，
		// 即先绑定的在前）；这里必须照抄 ASC，写成 DESC 会把「谁在前」的断言建立在假 SQL 上。
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	return limitRows(rows, q.Limit), nil
}

// Count 与 List 共用 anchorWhere（model/live_room_anchor.go:344-360）。
func (f *fakeAnchorModel) Count(_ context.Context, q model.AnchorListQuery) (int64, error) {
	f.log.add("live_room_anchor.Count:%d", q.RoomID)
	f.countQueries = append(f.countQueries, q)
	if err := f.dbErr("Count"); err != nil {
		return 0, err
	}
	if q.RoomID <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	return int64(len(f.filter(q))), nil
}

func (f *fakeAnchorModel) filter(q model.AnchorListQuery) []*model.LiveRoomAnchor {
	var rows []*model.LiveRoomAnchor
	for _, a := range f.rows {
		if a.RoomID != q.RoomID {
			continue
		}
		if q.Role != model.AnchorRoleUnspecified && a.Role != q.Role {
			continue
		}
		if q.OnlyEnabled && a.State != model.BindStateEnabled {
			continue
		}
		cp := *a
		rows = append(rows, &cp)
	}
	return rows
}

// CountActiveRoomsByMid 复刻 model/live_room_anchor.go:222-240：mid<=0 先 ErrInvalidMid；
// 口径是 `COUNT(DISTINCT room_id) WHERE mid=? AND state=生效 [AND role=?]`。
// **DISTINCT 是与 ListRoomsByMid 的分水岭**（后者 SELECT room_id 不带 DISTINCT，
// 见本文件 ListRoomsByMid 注释）：同一 mid 在同一房间占两个角色时，
// 这里算 1 个房间、那里回两个相同 room_id，两道配额因此不是同一个数。
func (f *fakeAnchorModel) CountActiveRoomsByMid(_ context.Context, mid int64, role int32) (int64, error) {
	f.log.add("live_room_anchor.CountActiveRoomsByMid:%d/r%d", mid, role)
	if err := f.dbErr("CountActiveRoomsByMid"); err != nil {
		return 0, err
	}
	if mid <= 0 {
		return 0, model.ErrInvalidMid
	}
	seen := map[int64]struct{}{}
	for _, a := range f.rows {
		if a.Mid != mid || a.State != model.BindStateEnabled {
			continue
		}
		if role != model.AnchorRoleUnspecified && a.Role != role {
			continue
		}
		seen[a.RoomID] = struct{}{}
	}
	return int64(len(seen)), nil
}

// Unbind 复刻 model/live_room_anchor.go:242-270：三条守卫（roomID/mid/role）都在 SQL 之前；
// UPDATE **无条件排除 role=OWNER**（换房主只能走 TransferOwner），把命中的每一条
// 非房主生效行都置为 state=停用、owner_room_id=NULL，回 RowsAffected（=命中行数）。
// role=Unspecified(0) 时不加 role 条件，即「解绑该 mid 在本房间的全部非房主角色」，
// 所以命中行数可以 >1 —— 真实 MySQL（未开 clientFoundRows）下 1→2 是变化更新，
// 行数与假件一致。
func (f *fakeAnchorModel) Unbind(_ context.Context, roomID, mid int64, role int32) (int64, error) {
	f.log.add("live_room_anchor.Unbind:%d/%d/r%d", roomID, mid, role)
	f.fireRace("live_room_anchor.Unbind")
	if roomID <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	if mid <= 0 {
		return 0, model.ErrInvalidMid
	}
	if role != model.AnchorRoleUnspecified && !model.ValidAnchorRole(role) {
		return 0, model.ErrAnchorRoleInvalid
	}
	if err := f.dbErr("Unbind"); err != nil {
		return 0, fmt.Errorf("live_room_anchor Unbind: %w", err)
	}
	var aff int64
	now := model.NowUnix()
	for _, a := range f.rows {
		if a.RoomID != roomID || a.Mid != mid || a.State != model.BindStateEnabled ||
			a.Role == model.AnchorRoleOwner {
			continue
		}
		if role != model.AnchorRoleUnspecified && a.Role != role {
			continue
		}
		a.State = model.BindStateDisabled
		a.OwnerRoomID = sql.NullInt64{}
		a.Mtime = now
		aff++
	}
	return aff, nil
}

func (f *fakeAnchorModel) TransferOwner(context.Context, int64, int64, int64) (bool, error) {
	return false, f.unexpected("TransferOwner")
}

// ListRoomsByMid 复刻 model/live_room_anchor.go:317-341：
// mid<=0 先报 ErrInvalidMid；state=生效；role=Unspecified 不过滤；room_id 倒序 + LIMIT。
// 真实 SQL 是 `SELECT room_id`（**不是 DISTINCT**），所以同一 mid 在同一房间占两个角色时
// 会返回两个相同 room_id —— 假件照抄，用例锁住由此产生的 total 口径问题。
func (f *fakeAnchorModel) ListRoomsByMid(_ context.Context, mid int64, role int32, limit int32) ([]int64, error) {
	f.log.add("live_room_anchor.ListRoomsByMid:%d", mid)
	if err := f.dbErr("ListRoomsByMid"); err != nil {
		return nil, err
	}
	if mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	var ids []int64
	for _, a := range f.rows {
		if a.Mid != mid || a.State != model.BindStateEnabled {
			continue
		}
		if role != model.AnchorRoleUnspecified && a.Role != role {
			continue
		}
		ids = append(ids, a.RoomID)
	}
	slices.SortStableFunc(ids, func(a, b int64) int { return int64Desc(a, b) })
	return limitRows(ids, limit), nil
}

var _ model.LiveRoomAnchorModel = (*fakeAnchorModel)(nil)

// --- live_session 替身 ---

type fakeSessionModel struct {
	faultInjector
	rows []*model.LiveSession

	listQueries []model.SessionListModel
}

func newFakeSessionModel(log *callLog, race *raceHooks) *fakeSessionModel {
	return &fakeSessionModel{faultInjector: newFault("live_session", log, race)}
}

// Insert / InsertTx 复刻 model/live_session.go:125-166（场次建档的唯一 SQL 实现）。
// 真实 INSERT 写入除主键外的全部 19 列，并且：
//   - 两条守卫（room_id / mid）都在 SQL 之前；
//   - ctime 仅在调用方给 0 时补 now，mtime 恒被覆盖成 now（照抄生产对入参结构体的副作用）；
//   - **不补任何业务列**：started_at / last_stream_seq / moderation_task_id 给什么写什么，
//     所以「StartLive 是否真的按 PENDING + seq=0 建档」只能读回那一行来断言。
//
// 键里带 room_id（调用方可见），新分配的 session_id 不进键。
func (f *fakeSessionModel) Insert(_ context.Context, s *model.LiveSession) (int64, error) {
	f.log.add("live_session.Insert:r%d", s.RoomID)
	return f.insert("Insert", s)
}

func (f *fakeSessionModel) InsertTx(_ context.Context, session sqlx.Session, s *model.LiveSession) (int64, error) {
	if session == nil {
		// 真实实现 session==nil 时退化为连接版本（model/live_session.go:129-134）。
		return f.Insert(context.Background(), s)
	}
	f.log.add("live_session.InsertTx:r%d", s.RoomID)
	return f.insert("InsertTx", s)
}

func (f *fakeSessionModel) insert(method string, s *model.LiveSession) (int64, error) {
	f.fireRace("live_session." + method)
	if s.RoomID <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	if s.Mid <= 0 {
		return 0, model.ErrInvalidMid
	}
	if err := f.dbErr(method); err != nil {
		return 0, fmt.Errorf("live_session Insert: %w", err)
	}
	now := model.NowUnix()
	if s.Ctime == 0 {
		s.Ctime = now
	}
	s.Mtime = now
	cp := *s
	cp.SessionID = nextFakePK(f.rows, func(x *model.LiveSession) int64 { return x.SessionID })
	f.rows = append(f.rows, &cp)
	s.SessionID = cp.SessionID
	return cp.SessionID, nil
}

func (f *fakeSessionModel) FindOne(_ context.Context, sessionID int64) (*model.LiveSession, error) {
	f.log.add("live_session.FindOne:%d", sessionID)
	if err := f.dbErr("FindOne"); err != nil {
		return nil, err
	}
	for _, s := range f.rows {
		if s.SessionID == sessionID {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

// FindActiveByRoom 在真实 model 里就是 ListActiveByRoom(roomID, 1) 取首条
// （model/live_session.go）。这里独立实现同语义并记自己的键，
// 免得用例把「logic 调了哪个方法」读成「调了另一个方法」。
func (f *fakeSessionModel) FindActiveByRoom(_ context.Context, roomID int64) (*model.LiveSession, error) {
	f.log.add("live_session.FindActiveByRoom:%d", roomID)
	f.fireRace("live_session.FindActiveByRoom")
	if err := f.dbErr("FindActiveByRoom"); err != nil {
		return nil, err
	}
	var found *model.LiveSession
	for _, s := range f.rows {
		if s.RoomID != roomID || !slices.Contains(model.ActiveSessionStates, s.State) {
			continue
		}
		if found == nil || s.SessionID > found.SessionID {
			cp := *s
			found = &cp
		}
	}
	return found, nil
}

// ListActiveByRoom 复刻 model/live_session.go：state IN (PENDING, LIVING)、session_id 倒序、LIMIT。
func (f *fakeSessionModel) ListActiveByRoom(_ context.Context, roomID int64, limit int32) ([]*model.LiveSession, error) {
	f.log.add("live_session.ListActiveByRoom:%d", roomID)
	f.fireRace("live_session.ListActiveByRoom")
	if err := f.dbErr("ListActiveByRoom"); err != nil {
		return nil, err
	}
	var rows []*model.LiveSession
	for _, s := range f.rows {
		if s.RoomID != roomID || !slices.Contains(model.ActiveSessionStates, s.State) {
			continue
		}
		cp := *s
		rows = append(rows, &cp)
	}
	slices.SortStableFunc(rows, func(a, b *model.LiveSession) int { return int64Desc(a.SessionID, b.SessionID) })
	return limitRows(rows, limit), nil
}

// FindLatest 复刻 model/live_session.go:roomID<=0 先 ErrInvalidRoomID；offset<0 归 0；
// session_id 倒序取第 offset+1 条；越界返回 (nil, nil)。
func (f *fakeSessionModel) FindLatest(_ context.Context, roomID int64, offset int32) (*model.LiveSession, error) {
	f.log.add("live_session.FindLatest:%d/%d", roomID, offset)
	if err := f.dbErr("FindLatest"); err != nil {
		return nil, err
	}
	if roomID <= 0 {
		return nil, model.ErrInvalidRoomID
	}
	if offset < 0 {
		offset = 0
	}
	var rows []*model.LiveSession
	for _, s := range f.rows {
		if s.RoomID == roomID {
			cp := *s
			rows = append(rows, &cp)
		}
	}
	slices.SortStableFunc(rows, func(a, b *model.LiveSession) int { return int64Desc(a.SessionID, b.SessionID) })
	if int(offset) >= len(rows) {
		return nil, nil
	}
	return rows[offset], nil
}

// List 复刻 model/live_session.go 的游标分页：RoomID<=0 先 ErrInvalidRoomID，
// mid>0 / state!=UNSPECIFIED / before>0 才加条件，session_id 倒序 + LIMIT（无 OFFSET）。
func (f *fakeSessionModel) List(_ context.Context, q model.SessionListModel) ([]*model.LiveSession, error) {
	f.log.add("live_session.List:%d", q.RoomID)
	f.listQueries = append(f.listQueries, q)
	if err := f.dbErr("List"); err != nil {
		return nil, err
	}
	if q.RoomID <= 0 {
		return nil, model.ErrInvalidRoomID
	}
	var rows []*model.LiveSession
	for _, s := range f.rows {
		if s.RoomID != q.RoomID {
			continue
		}
		if q.Mid > 0 && s.Mid != q.Mid {
			continue
		}
		if q.State != model.SessionStateUnspecified && s.State != q.State {
			continue
		}
		if q.BeforeSessionID > 0 && s.SessionID >= q.BeforeSessionID {
			continue
		}
		cp := *s
		rows = append(rows, &cp)
	}
	slices.SortStableFunc(rows, func(a, b *model.LiveSession) int { return int64Desc(a.SessionID, b.SessionID) })
	return limitRows(rows, q.Limit), nil
}

// Transition / TransitionTx 复刻 model/live_session.go:284-323。
// 四条真实语义都是断言面：
//  1. `CanSessionTransition(from,to)` 在 SQL 之前，非法边返回 ErrInvalidSessionTransition；
//  2. 进终态必须带非 UNSPECIFIED 的 end_reason，否则 ErrEndReasonInvalid（禁播/关闭各有专列原因）；
//  3. 终态的 duration 由 SQL 侧算 `GREATEST(ended_at - started_at, 0)`，不是调用方给的；
//  4. →LIVING 用 `started_at = GREATEST(started_at, now)`，断流重连不得刷新开播时间。
func (f *fakeSessionModel) Transition(_ context.Context, sessionID int64, from, to, endReason int32,
	endedAt int64) (bool, error) {
	return f.transition("live_session.Transition", "Transition", sessionID, from, to, endReason, endedAt)
}

func (f *fakeSessionModel) TransitionTx(_ context.Context, session sqlx.Session, sessionID int64,
	from, to, endReason int32, endedAt int64) (bool, error) {
	if session == nil {
		return f.Transition(context.Background(), sessionID, from, to, endReason, endedAt)
	}
	return f.transition("live_session.TransitionTx", "TransitionTx", sessionID, from, to, endReason, endedAt)
}

func (f *fakeSessionModel) transition(key, method string, sessionID int64, from, to, endReason int32,
	endedAt int64) (bool, error) {
	// 两条守卫都在 SQL 之前（model/live_session.go:285-296），失败时只留 :GUARD 轨迹。
	if !model.CanSessionTransition(from, to) {
		f.log.add("%s:%d:%d->%d:GUARD", key, sessionID, from, to)
		return false, model.ErrInvalidSessionTransition
	}
	if model.SessionStateIsTerminal(to) && endReason == model.EndReasonUnspecified {
		f.log.add("%s:%d:%d->%d:GUARD", key, sessionID, from, to)
		return false, model.ErrEndReasonInvalid
	}
	f.log.add("%s:%d:%d->%d/r%d", key, sessionID, from, to, endReason)
	f.fireRace(key)
	if err := f.dbErr(method); err != nil {
		return false, fmt.Errorf("live_session Transition: %w", err)
	}
	now := model.NowUnix()
	for _, s := range f.rows {
		if s.SessionID != sessionID || s.State != from {
			continue
		}
		s.State = to
		s.Mtime = now
		switch {
		case model.SessionStateIsTerminal(to):
			if endedAt <= 0 {
				endedAt = now
			}
			s.EndedAt = endedAt
			s.EndReason = endReason
			d := endedAt - s.StartedAt
			if d < 0 {
				d = 0
			}
			s.DurationSeconds = d
		case to == model.SessionStateLiving:
			if s.StartedAt < now {
				s.StartedAt = now
			}
		}
		return true, nil
	}
	return false, nil
}

// SetStreamID 复刻 model/live_session.go:325-340：空 streamID 先 ErrStreamRefMismatch（不发 SQL），
// SQL 的 WHERE 带 `stream_id = ”` —— 只在原值为空时补登，重连事件不得让流引用漂流。
func (f *fakeSessionModel) SetStreamID(_ context.Context, sessionID int64, streamID string) (bool, error) {
	f.log.add("live_session.SetStreamID:%d:%s", sessionID, streamID)
	f.fireRace("live_session.SetStreamID")
	if err := f.dbErr("SetStreamID"); err != nil {
		return false, fmt.Errorf("live_session SetStreamID: %w", err)
	}
	if streamID == "" {
		return false, model.ErrStreamRefMismatch
	}
	for _, s := range f.rows {
		if s.SessionID == sessionID && s.StreamID == "" {
			s.StreamID = streamID
			s.Mtime = model.NowUnix()
			return true, nil
		}
	}
	return false, nil
}

// AdvanceStreamSeq / AdvanceStreamSeqTx 复刻 model/live_session.go:342-388 的 seq 守卫迁移：
//  1. CanSessionTransition 与 seq<=0 都在 SQL 之前；
//  2. 进终态必须带非 UNSPECIFIED end_reason（同 Transition），endedAt<=0 时补 now；
//  3. WHERE `session_id AND state=from AND last_stream_seq < seq`，命中才写 state/seq/mtime
//     以及终态列（duration 仍由 SQL 侧按 started_at 算）。
func (f *fakeSessionModel) AdvanceStreamSeq(_ context.Context, sessionID, seq int64,
	from, to, endReason int32, endedAt int64) (bool, error) {
	return f.advanceStreamSeq("live_session.AdvanceStreamSeq", "AdvanceStreamSeq",
		sessionID, seq, from, to, endReason, endedAt)
}

func (f *fakeSessionModel) AdvanceStreamSeqTx(_ context.Context, session sqlx.Session, sessionID, seq int64,
	from, to, endReason int32, endedAt int64) (bool, error) {
	if session == nil {
		return f.AdvanceStreamSeq(context.Background(), sessionID, seq, from, to, endReason, endedAt)
	}
	return f.advanceStreamSeq("live_session.AdvanceStreamSeqTx", "AdvanceStreamSeqTx",
		sessionID, seq, from, to, endReason, endedAt)
}

func (f *fakeSessionModel) advanceStreamSeq(key, method string, sessionID, seq int64,
	from, to, endReason int32, endedAt int64) (bool, error) {
	if !model.CanSessionTransition(from, to) {
		f.log.add("%s:%d:%d->%d:GUARD", key, sessionID, from, to)
		return false, model.ErrInvalidSessionTransition
	}
	if seq <= 0 {
		f.log.add("%s:%d:%d->%d:GUARD", key, sessionID, from, to)
		return false, model.ErrStreamSeqStale
	}
	if model.SessionStateIsTerminal(to) && endReason == model.EndReasonUnspecified {
		f.log.add("%s:%d:%d->%d:GUARD", key, sessionID, from, to)
		return false, model.ErrEndReasonInvalid
	}
	f.log.add("%s:%d:%d->%d/seq%d", key, sessionID, from, to, seq)
	f.fireRace(key)
	if err := f.dbErr(method); err != nil {
		return false, fmt.Errorf("live_session AdvanceStreamSeq: %w", err)
	}
	now := model.NowUnix()
	for _, s := range f.rows {
		if s.SessionID != sessionID || s.State != from || s.LastStreamSeq >= seq {
			continue
		}
		s.LastStreamSeq = seq
		s.State = to
		s.Mtime = now
		if model.SessionStateIsTerminal(to) {
			if endedAt <= 0 {
				endedAt = now
			}
			s.EndedAt = endedAt
			s.EndReason = endReason
			d := endedAt - s.StartedAt
			if d < 0 {
				d = 0
			}
			s.DurationSeconds = d
		}
		return true, nil
	}
	return false, nil
}

// BumpStreamSeqTx 复刻 model/live_session.go:391-414：只推 seq、**不改 state**，
// 守卫是 sessionID/seq 两条前置错误，WHERE 同样是 last_stream_seq < seq。
func (f *fakeSessionModel) BumpStreamSeqTx(_ context.Context, session sqlx.Session,
	sessionID, seq int64) (bool, error) {
	f.log.add("live_session.BumpStreamSeqTx:%d/seq%d", sessionID, seq)
	f.fireRace("live_session.BumpStreamSeqTx")
	if sessionID <= 0 {
		return false, model.ErrInvalidSessionID
	}
	if seq <= 0 {
		return false, model.ErrStreamSeqStale
	}
	if err := f.dbErr("BumpStreamSeqTx"); err != nil {
		return false, fmt.Errorf("live_session BumpStreamSeq: %w", err)
	}
	for _, s := range f.rows {
		if s.SessionID == sessionID && s.LastStreamSeq < seq {
			s.LastStreamSeq = seq
			s.Mtime = model.NowUnix()
			return true, nil
		}
	}
	return false, nil
}

// AttachReplay 复刻 model/live_session.go:416-462：三条守卫（sessionID/roomID/
// CanReplayTransition(from,to)）都在 SQL 之前，非法边直接 ErrInvalidReplayTransition；
// SET 里 replay_state/mtime 恒写，三个引用列**只在 >0 时写**（回放是逐步补全的：
// 先 record_id、再 asset_id、最后 aid），WHERE 同时带
// `room_id + state IN (ENDED,TERMINATED) + replay_state = from` ——
// 归属与终态由 SQL 守，不靠「先读再判」，所以跨房间串改在并发下穿不过去。
func (f *fakeSessionModel) AttachReplay(_ context.Context, sessionID, roomID int64,
	fromReplay, toReplay int32, recordID, recordAssetID, recordAid int64) (bool, error) {
	f.log.add("live_session.AttachReplay:%d:%d->%d", sessionID, fromReplay, toReplay)
	f.fireRace("live_session.AttachReplay")
	if sessionID <= 0 {
		return false, model.ErrInvalidSessionID
	}
	if roomID <= 0 {
		return false, model.ErrInvalidRoomID
	}
	if !model.CanReplayTransition(fromReplay, toReplay) {
		return false, model.ErrInvalidReplayTransition
	}
	if err := f.dbErr("AttachReplay"); err != nil {
		return false, fmt.Errorf("live_session AttachReplay: %w", err)
	}
	for _, s := range f.rows {
		if s.SessionID != sessionID || s.RoomID != roomID ||
			!model.SessionStateIsTerminal(s.State) || s.ReplayState != fromReplay {
			continue
		}
		s.ReplayState = toReplay
		if recordID > 0 {
			s.RecordID = recordID
		}
		if recordAssetID > 0 {
			s.RecordAssetID = recordAssetID
		}
		if recordAid > 0 {
			s.RecordAid = recordAid
		}
		s.Mtime = model.NowUnix()
		return true, nil
	}
	return false, nil
}

func (f *fakeSessionModel) SetModerationTaskID(context.Context, int64, int64) error {
	return f.unexpected("SetModerationTaskID")
}

func (f *fakeSessionModel) CountActive(context.Context) (int64, error) {
	return 0, f.unexpected("CountActive")
}

var _ model.LiveSessionModel = (*fakeSessionModel)(nil)

// --- live_room_ban 替身 ---

type fakeBanModel struct {
	faultInjector
	rows []*model.LiveRoomBan

	listQueries  []model.BanListQuery
	countQueries []model.BanListQuery
}

func newFakeBanModel(log *callLog, race *raceHooks) *fakeBanModel {
	return &fakeBanModel{faultInjector: newFault("live_room_ban", log, race)}
}

// Insert / InsertTx 复刻 model/live_room_ban.go:91-142。守卫顺序照抄：
// room_id → operator_mid → （永久：end_at 必须 0；临时：start_at 缺则补 now 且 end_at>start_at）。
// 写入分支的关键事实：state=0 归一为 Active、ctime=0 补 now、
// 且 `lift_operator_mid/lift_reason/lifted_at` 三列是**字面量 0/”/0**（新记录不可能带解除信息）。
func (f *fakeBanModel) Insert(_ context.Context, b *model.LiveRoomBan) (int64, error) {
	f.log.add("live_room_ban.Insert")
	return f.insert("Insert", b)
}

func (f *fakeBanModel) InsertTx(_ context.Context, session sqlx.Session, b *model.LiveRoomBan) (int64, error) {
	if session == nil {
		return f.Insert(context.Background(), b)
	}
	f.log.add("live_room_ban.InsertTx")
	return f.insert("InsertTx", b)
}

func (f *fakeBanModel) insert(method string, b *model.LiveRoomBan) (int64, error) {
	f.fireRace("live_room_ban." + method)
	if b.RoomID <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	if b.OperatorMid <= 0 {
		return 0, model.ErrOperatorRequired
	}
	now := model.NowUnix()
	switch b.BanType {
	case model.BanTypePermanent:
		if b.EndAt != 0 {
			return 0, model.ErrBanTypeInvalid
		}
	case model.BanTypeTemporary:
		if b.StartAt <= 0 {
			b.StartAt = now
		}
		if b.EndAt <= b.StartAt {
			return 0, model.ErrBanDurationRequired
		}
	default:
		return 0, model.ErrBanTypeInvalid
	}
	if err := f.dbErr(method); err != nil {
		return 0, fmt.Errorf("live_room_ban Insert: %w", err)
	}
	if b.State == 0 {
		b.State = model.BanStateActive
	}
	if b.Ctime == 0 {
		b.Ctime = now
	}
	cp := *b
	cp.BanID = nextFakePK(f.rows, func(x *model.LiveRoomBan) int64 { return x.BanID })
	// 生产 INSERT 的列清单里这三列是常量，等价于强制清零。
	cp.LiftOperatorMid = 0
	cp.LiftReason = ""
	cp.LiftedAt = 0
	f.rows = append(f.rows, &cp)
	b.BanID = cp.BanID
	return cp.BanID, nil
}

// FindOne 复刻 model/live_room_ban.go:144-154：不存在返回 (nil, nil)。
func (f *fakeBanModel) FindOne(_ context.Context, banID int64) (*model.LiveRoomBan, error) {
	f.log.add("live_room_ban.FindOne:%d", banID)
	f.fireRace("live_room_ban.FindOne")
	if err := f.dbErr("FindOne"); err != nil {
		return nil, err
	}
	for _, b := range f.rows {
		if b.BanID == banID {
			cp := *b
			return &cp, nil
		}
	}
	return nil, nil
}

// FindActiveByRoom 复刻 model/live_room_ban.go:156-168：
// `state=1 AND (end_at=0 OR end_at>now) ORDER BY ban_id DESC LIMIT 1`
// —— 已到期的临时禁播**不算生效记录**，永久禁播（end_at=0）永远算。
func (f *fakeBanModel) FindActiveByRoom(_ context.Context, roomID, now int64) (*model.LiveRoomBan, error) {
	f.log.add("live_room_ban.FindActiveByRoom:%d@%d", roomID, now)
	f.fireRace("live_room_ban.FindActiveByRoom")
	if err := f.dbErr("FindActiveByRoom"); err != nil {
		return nil, err
	}
	var found *model.LiveRoomBan
	for _, b := range f.rows {
		if b.RoomID != roomID || b.State != model.BanStateActive {
			continue
		}
		if b.EndAt != 0 && b.EndAt <= now {
			continue
		}
		if found == nil || b.BanID > found.BanID {
			cp := *b
			found = &cp
		}
	}
	return found, nil
}

// HasActiveByMid 复刻 model/live_room_ban.go:170-184：mid<=0 先 ErrInvalidMid（不发 SQL），
// 条件与 FindActiveByRoom 同构（state=1 且 永久或 end_at>now），只是按 mid 找、命中即返回。
func (f *fakeBanModel) HasActiveByMid(_ context.Context, mid, now int64) (bool, error) {
	f.log.add("live_room_ban.HasActiveByMid:%d@%d", mid, now)
	f.fireRace("live_room_ban.HasActiveByMid")
	if err := f.dbErr("HasActiveByMid"); err != nil {
		return false, fmt.Errorf("live_room_ban HasActiveByMid: %w", err)
	}
	if mid <= 0 {
		return false, model.ErrInvalidMid
	}
	for _, b := range f.rows {
		if b.Mid != mid || b.State != model.BanStateActive {
			continue
		}
		if b.EndAt != 0 && b.EndAt <= now {
			continue
		}
		return true, nil
	}
	return false, nil
}

// List 复刻 model/live_room_ban.go 的 List：ban_id 倒序，**带 LIMIT ? OFFSET ?**
// （这张表的 SQL 与 live_room / live_room_anchor 不同，offset 真的生效）。
func (f *fakeBanModel) List(_ context.Context, q model.BanListQuery) ([]*model.LiveRoomBan, error) {
	f.log.add("live_room_ban.List")
	f.listQueries = append(f.listQueries, q)
	if err := f.dbErr("List"); err != nil {
		return nil, err
	}
	rows := f.filter(q)
	slices.SortStableFunc(rows, func(a, b *model.LiveRoomBan) int { return int64Desc(a.BanID, b.BanID) })
	rows = offsetRows(rows, q.Offset)
	return limitRows(rows, q.Limit), nil
}

// Count 与 List 共用 banWhere（model/live_room_ban.go:banWhere）。
func (f *fakeBanModel) Count(_ context.Context, q model.BanListQuery) (int64, error) {
	f.log.add("live_room_ban.Count")
	f.countQueries = append(f.countQueries, q)
	if err := f.dbErr("Count"); err != nil {
		return 0, err
	}
	return int64(len(f.filter(q))), nil
}

func (f *fakeBanModel) filter(q model.BanListQuery) []*model.LiveRoomBan {
	var rows []*model.LiveRoomBan
	for _, b := range f.rows {
		if b.BanID <= 0 {
			continue
		}
		if q.RoomID > 0 && b.RoomID != q.RoomID {
			continue
		}
		if q.Mid > 0 && b.Mid != q.Mid {
			continue
		}
		if q.State != 0 && b.State != q.State {
			continue
		}
		cp := *b
		rows = append(rows, &cp)
	}
	return rows
}

// Lift / LiftTx 复刻 model/live_room_ban.go:217-253。
// 守卫顺序照抄：ban_id<=0 先 ErrBanNotFound（**不是** ErrInvalidRoomID，
// 少这一条就会把「解除不存在号」的错报成另一个哨兵）、lift_operator_mid<=0 先 ErrOperatorRequired、
// lifted_at<=0 才补 now。WHERE `ban_id AND state=1`，所以重复解除命中 0 行 → 回 false。
func (f *fakeBanModel) Lift(_ context.Context, banID, liftOperatorMid int64, reason string,
	liftedAt int64) (bool, error) {
	return f.lift("Lift", banID, liftOperatorMid, reason, liftedAt)
}

func (f *fakeBanModel) LiftTx(_ context.Context, session sqlx.Session, banID, liftOperatorMid int64,
	reason string, liftedAt int64) (bool, error) {
	if session == nil {
		return f.Lift(context.Background(), banID, liftOperatorMid, reason, liftedAt)
	}
	return f.lift("LiftTx", banID, liftOperatorMid, reason, liftedAt)
}

func (f *fakeBanModel) lift(method string, banID, liftOperatorMid int64, reason string,
	liftedAt int64) (bool, error) {
	f.log.add("live_room_ban.%s:%d", method, banID)
	f.fireRace("live_room_ban." + method)
	if banID <= 0 {
		return false, model.ErrBanNotFound
	}
	if liftOperatorMid <= 0 {
		return false, model.ErrOperatorRequired
	}
	if err := f.dbErr(method); err != nil {
		return false, fmt.Errorf("live_room_ban Lift: %w", err)
	}
	at := liftedAt
	if at <= 0 {
		at = model.NowUnix()
	}
	for _, b := range f.rows {
		if b.BanID == banID && b.State == model.BanStateActive {
			b.State = model.BanStateLifted
			b.LiftOperatorMid = liftOperatorMid
			b.LiftReason = reason
			b.LiftedAt = at
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeBanModel) ExpireDue(context.Context, int64, int32) (int64, error) {
	return 0, f.unexpected("ExpireDue")
}

func (f *fakeBanModel) ListDue(context.Context, int64, int32) ([]*model.LiveRoomBan, error) {
	return nil, f.unexpected("ListDue")
}

var _ model.LiveRoomBanModel = (*fakeBanModel)(nil)

// --- live_area 替身 ---

type fakeAreaModel struct {
	faultInjector
	rows []*model.LiveArea

	listQueries  []model.AreaListQuery
	countQueries []model.AreaListQuery
}

func newFakeAreaModel(log *callLog, race *raceHooks) *fakeAreaModel {
	return &fakeAreaModel{faultInjector: newFault("live_area", log, race)}
}

// Insert 复刻 model/live_area.go:84-113。注意三条生产侧事实：
//   - 名称长度上限是**硬编码** model.AreaNameMaxRunes（=fakeAreaNameMaxRunes），
//     不看 config.LiveRoom.AreaNameMaxLength —— 两处宽度漂移由
//     TestUpsertAreaConfigWidthDrift 锁定；
//   - 只写 7 列，area_id 由 AUTO_INCREMENT 分配；
//   - ctime 仅在调用方给 0 时补 now，mtime 恒被覆盖成 now（改的是调用方结构体）。
//
// uniq_area_name 冲突在真实驱动里是 1062，这里按同一条语义直接返回 ErrAreaNameConflict。
func (f *fakeAreaModel) Insert(_ context.Context, a *model.LiveArea) (int64, error) {
	f.log.add("live_area.Insert")
	f.fireRace("live_area.Insert")
	if strings.TrimSpace(a.AreaName) == "" || runeLen(a.AreaName) > fakeAreaNameMaxRunes {
		return 0, model.ErrAreaNameInvalid
	}
	if a.OperatorMid <= 0 {
		return 0, model.ErrOperatorRequired
	}
	if a.State != model.AreaStateEnabled && a.State != model.AreaStateDisabled {
		return 0, model.ErrAreaStateInvalid
	}
	if err := f.dbErr("Insert"); err != nil {
		if errors.Is(err, errFakeUniqueAreaName) {
			return 0, model.ErrAreaNameConflict
		}
		return 0, fmt.Errorf("live_area Insert: %w", err)
	}
	if exist := f.byName(a.AreaName); exist != nil {
		return 0, model.ErrAreaNameConflict
	}
	now := model.NowUnix()
	if a.Ctime == 0 {
		a.Ctime = now
	}
	a.Mtime = now
	cp := *a
	cp.AreaID = nextFakePK(f.rows, func(x *model.LiveArea) int64 { return x.AreaID })
	f.rows = append(f.rows, &cp)
	a.AreaID = cp.AreaID
	return cp.AreaID, nil
}

// Update 复刻 model/live_area.go:115-152：`state/sort/operator_mid/mtime` 恒写，
// area_name 只在非空时写，parent_area_id 只在 **>=0** 时写（0 是合法的一级分区值）。
// WHERE 只有 area_id，所以「把同一套值再写一遍」在假件里算命中（返回 true），
// 而真实 MySQL（未开 clientFoundRows）会给 changed=0 → false：该差异见 README 已知缺口。
func (f *fakeAreaModel) Update(_ context.Context, a *model.LiveArea) (bool, error) {
	f.log.add("live_area.Update:%d", a.AreaID)
	f.fireRace("live_area.Update")
	if a.AreaID <= 0 {
		return false, model.ErrInvalidAreaID
	}
	if a.OperatorMid <= 0 {
		return false, model.ErrOperatorRequired
	}
	if a.AreaName != "" && (strings.TrimSpace(a.AreaName) == "" || runeLen(a.AreaName) > fakeAreaNameMaxRunes) {
		return false, model.ErrAreaNameInvalid
	}
	if a.State != model.AreaStateEnabled && a.State != model.AreaStateDisabled {
		return false, model.ErrAreaStateInvalid
	}
	if err := f.dbErr("Update"); err != nil {
		if errors.Is(err, errFakeUniqueAreaName) {
			return false, model.ErrAreaNameConflict
		}
		return false, fmt.Errorf("live_area Update: %w", err)
	}
	if a.AreaName != "" {
		if exist := f.byName(a.AreaName); exist != nil && exist.AreaID != a.AreaID {
			return false, model.ErrAreaNameConflict
		}
	}
	row := f.byID(a.AreaID)
	if row == nil {
		return false, nil
	}
	row.State = a.State
	row.Sort = a.Sort
	row.OperatorMid = a.OperatorMid
	row.Mtime = model.NowUnix()
	if a.AreaName != "" {
		row.AreaName = a.AreaName
	}
	if a.ParentAreaID >= 0 {
		row.ParentAreaID = a.ParentAreaID
	}
	return true, nil
}

// FindOne 复刻 model/live_area.go:155-166：不存在返回 (nil, nil)，不校验 area_id。
func (f *fakeAreaModel) FindOne(_ context.Context, areaID int64) (*model.LiveArea, error) {
	f.log.add("live_area.FindOne:%d", areaID)
	f.fireRace("live_area.FindOne")
	if err := f.dbErr("FindOne"); err != nil {
		return nil, err
	}
	if row := f.byID(areaID); row != nil {
		cp := *row
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeAreaModel) byID(areaID int64) *model.LiveArea {
	for _, a := range f.rows {
		if a.AreaID == areaID {
			return a
		}
	}
	return nil
}

func (f *fakeAreaModel) byName(name string) *model.LiveArea {
	for _, a := range f.rows {
		if a.AreaName == name {
			return a
		}
	}
	return nil
}

// errFakeUniqueAreaName 让用例能把「驱动报 1062」这一形态单独注入，
// 与假件自己做的 uniq_area_name 预检区分开（生产代码两条路径都翻译成同一个哨兵）。
var errFakeUniqueAreaName = errors.New("test: duplicate entry uniq_area_name")

func (f *fakeAreaModel) FindByName(context.Context, string) (*model.LiveArea, error) {
	return nil, f.unexpected("FindByName")
}

// List 复刻 model/live_area.go 的 List：areaWhere 里 ParentAreaID / State **>= 0 才加条件**
// （-1 才是「不过滤」，0 是合法取值），ORDER parent ASC, sort ASC, area_id ASC，带 OFFSET。
func (f *fakeAreaModel) List(_ context.Context, q model.AreaListQuery) ([]*model.LiveArea, error) {
	f.log.add("live_area.List")
	f.listQueries = append(f.listQueries, q)
	if err := f.dbErr("List"); err != nil {
		return nil, err
	}
	rows := f.filter(q)
	slices.SortStableFunc(rows, func(a, b *model.LiveArea) int {
		if a.ParentAreaID != b.ParentAreaID {
			return int(a.ParentAreaID - b.ParentAreaID)
		}
		if a.Sort != b.Sort {
			return int(a.Sort) - int(b.Sort)
		}
		return int(a.AreaID - b.AreaID)
	})
	rows = offsetRows(rows, q.Offset)
	return limitRows(rows, q.Limit), nil
}

func (f *fakeAreaModel) Count(_ context.Context, q model.AreaListQuery) (int64, error) {
	f.log.add("live_area.Count")
	f.countQueries = append(f.countQueries, q)
	if err := f.dbErr("Count"); err != nil {
		return 0, err
	}
	return int64(len(f.filter(q))), nil
}

func (f *fakeAreaModel) filter(q model.AreaListQuery) []*model.LiveArea {
	var rows []*model.LiveArea
	for _, a := range f.rows {
		if a.AreaID <= 0 {
			continue
		}
		if q.ParentAreaID >= 0 && a.ParentAreaID != q.ParentAreaID {
			continue
		}
		if q.State >= 0 && a.State != q.State {
			continue
		}
		cp := *a
		rows = append(rows, &cp)
	}
	return rows
}

// IsUsable 复刻 model/live_area.go:225-240：area_id<=0 先 ErrInvalidAreaID，
// 之后是单条 `COUNT(*) ... WHERE area_id=? AND state=1`（停用行算「不可用」）。
func (f *fakeAreaModel) IsUsable(_ context.Context, areaID int64) (bool, error) {
	f.log.add("live_area.IsUsable:%d", areaID)
	if err := f.dbErr("IsUsable"); err != nil {
		return false, err
	}
	if areaID <= 0 {
		return false, model.ErrInvalidAreaID
	}
	row := f.byID(areaID)
	return row != nil && row.State == model.AreaStateEnabled, nil
}

// LevelOf 复刻 model/live_area.go:242-265。真实实现**没有自己的 SQL**：
// 它就是 1~2 次 FindOne（先看自己，父非 0 时再看父），所以这里先记一条
// live_area.LevelOf:<id> 作为「logic 走了哪个入口」的标记，再委托给 FindOne
// （它按真实 SQL 记账）。三条返回口径都是用例要锁的：
//   - 自己不存在 → ErrAreaNotFound；
//   - 父行不存在（脏引用）→ ErrAreaParentInvalid；
//   - 父自己在第 2 层（三级嵌套）→ (0, parent, ErrAreaParentInvalid)，**父行照样回传**。
func (f *fakeAreaModel) LevelOf(ctx context.Context, areaID int64) (int, *model.LiveArea, error) {
	f.log.add("live_area.LevelOf:%d", areaID)
	if err := f.dbErr("LevelOf"); err != nil {
		return 0, nil, err
	}
	area, err := f.FindOne(ctx, areaID)
	if err != nil {
		return 0, nil, err
	}
	if area == nil {
		return 0, nil, model.ErrAreaNotFound
	}
	if area.ParentAreaID == 0 {
		return 1, area, nil
	}
	parent, err := f.FindOne(ctx, area.ParentAreaID)
	if err != nil {
		return 0, nil, err
	}
	if parent == nil {
		return 0, nil, model.ErrAreaParentInvalid
	}
	if parent.ParentAreaID != 0 {
		return 0, parent, model.ErrAreaParentInvalid
	}
	return 2, parent, nil
}

// CountChildren 复刻 model/live_area.go:267-281：parentAreaID<=0 先 ErrInvalidAreaID；
// `COUNT(*) WHERE parent_area_id = ?` **不看 state**（停用的子分区也算占用）。
func (f *fakeAreaModel) CountChildren(_ context.Context, parentAreaID int64) (int64, error) {
	f.log.add("live_area.CountChildren:%d", parentAreaID)
	if err := f.dbErr("CountChildren"); err != nil {
		return 0, err
	}
	if parentAreaID <= 0 {
		return 0, model.ErrInvalidAreaID
	}
	var n int64
	for _, a := range f.rows {
		if a.AreaID > 0 && a.ParentAreaID == parentAreaID {
			n++
		}
	}
	return n, nil
}

var _ model.LiveAreaModel = (*fakeAreaModel)(nil)

// --- live_room_state_log / live_room_idempotency 替身 ---
// 读侧 7 个方法都不触达它们；写侧批次 1 里 StateLogs 与 Idempotency 是断言主角。

type fakeStateLogModel struct {
	faultInjector
	rows []*model.LiveRoomStateLog
}

func newFakeStateLogModel(log *callLog, race *raceHooks) *fakeStateLogModel {
	return &fakeStateLogModel{faultInjector: newFault("live_room_state_log", log, race)}
}

// Insert / InsertTx 复刻 model/live_room_state_log.go:73-110。
// 守卫只有两条（room_id、state_type），都在 SQL 之前；**没有任何「迁移是否合法」的判定**
// —— 审计表是 append-only 的事实记录，写什么由调用方负责，所以「状态没改但日志写了」
// 这类分裂只能靠用例读回来暴露（见各写方法的失败残留断言）。
// 键里带 state_type / from->to / reason：这三个字段合起来才是「哪一次迁移」。
func (f *fakeStateLogModel) Insert(_ context.Context, l *model.LiveRoomStateLog) (int64, error) {
	return f.insert("Insert", "live_room_state_log.Insert", l)
}

func (f *fakeStateLogModel) InsertTx(_ context.Context, session sqlx.Session,
	l *model.LiveRoomStateLog) (int64, error) {
	if session == nil {
		return f.Insert(context.Background(), l)
	}
	return f.insert("InsertTx", "live_room_state_log.InsertTx", l)
}

func (f *fakeStateLogModel) insert(method, key string, l *model.LiveRoomStateLog) (int64, error) {
	f.log.add("%s:t%d:%d->%d:%s", key, l.StateType, l.FromState, l.ToState, l.Reason)
	f.fireRace("live_room_state_log." + method)
	if l.RoomID <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	switch l.StateType {
	case model.LogTypeRoomState, model.LogTypeVerifyState, model.LogTypeSessionState, model.LogTypeReplayState:
	default:
		return 0, model.ErrStateTypeInvalid
	}
	if err := f.dbErr(method); err != nil {
		return 0, fmt.Errorf("live_room_state_log Insert: %w", err)
	}
	if l.Ctime == 0 {
		l.Ctime = model.NowUnix()
	}
	cp := *l
	cp.LogID = nextFakePK(f.rows, func(x *model.LiveRoomStateLog) int64 { return x.LogID })
	f.rows = append(f.rows, &cp)
	l.LogID = cp.LogID
	return cp.LogID, nil
}

func (f *fakeStateLogModel) ListByRoom(context.Context, int64, int32, int32) ([]*model.LiveRoomStateLog, error) {
	return nil, f.unexpected("ListByRoom")
}

func (f *fakeStateLogModel) ListByTimeRange(context.Context, int64, int64, string,
	int32) ([]*model.LiveRoomStateLog, error) {
	return nil, f.unexpected("ListByTimeRange")
}

var _ model.LiveRoomStateLogModel = (*fakeStateLogModel)(nil)

type fakeIdempotencyModel struct {
	faultInjector
	rows []*model.LiveRoomIdempotency
}

func newFakeIdempotencyModel(log *callLog, race *raceHooks) *fakeIdempotencyModel {
	return &fakeIdempotencyModel{faultInjector: newFault("live_room_idempotency", log, race)}
}

// Claim 复刻 model/live_room_idempotency.go:63-90 的
// `INSERT ... ON DUPLICATE KEY UPDATE id = id`：
//   - 两条守卫（空键 / kind）都在 SQL 之前；
//   - **键已存在时那一行一点都不变**（`id=id` 是无变化更新），只回 false；
//     这一点至关重要：logic 里 `Claim` 传进来的 RoomID/SessionID/TraceID 都是当次入参，
//     重放时若把新值写进去，「首次受理时的归因」就被后来人改掉了；
//   - 真插入才分配 id，返回 true（MySQL 对插入行 aff=1、对已存在行 aff=0）。
func (f *fakeIdempotencyModel) Claim(_ context.Context, rec *model.LiveRoomIdempotency) (bool, error) {
	f.log.add("live_room_idempotency.Claim:%s", rec.DedupKey)
	f.fireRace("live_room_idempotency.Claim")
	if rec.DedupKey == "" {
		return false, model.ErrRequestIDRequired
	}
	if rec.Kind != model.IdempotencyKindRequest && rec.Kind != model.IdempotencyKindEvent {
		return false, model.ErrIdempotencyKindInvalid
	}
	if err := f.dbErr("Claim"); err != nil {
		return false, fmt.Errorf("live_room_idempotency Claim: %w", err)
	}
	now := model.NowUnix()
	if rec.Ctime == 0 {
		rec.Ctime = now
	}
	rec.Mtime = now
	if exist := f.byKey(rec.DedupKey); exist != nil {
		_ = exist // ON DUPLICATE KEY UPDATE id=id：既有行保持不动
		return false, nil
	}
	cp := *rec
	cp.ID = nextFakePK(f.rows, func(x *model.LiveRoomIdempotency) int64 { return x.ID })
	f.rows = append(f.rows, &cp)
	rec.ID = cp.ID
	return true, nil
}

// Find 复刻 model/live_room_idempotency.go:92-105：空键 ErrRequestIDRequired，
// 不存在返回 (nil, nil)（logic 的 dedupRecord 把它翻译成 ErrIdempotencyResultMissing）。
func (f *fakeIdempotencyModel) Find(_ context.Context, dedupKey string) (*model.LiveRoomIdempotency, error) {
	f.log.add("live_room_idempotency.Find:%s", dedupKey)
	if err := f.dbErr("Find"); err != nil {
		return nil, err
	}
	if dedupKey == "" {
		return nil, model.ErrRequestIDRequired
	}
	if rec := f.byKey(dedupKey); rec != nil {
		cp := *rec
		return &cp, nil
	}
	return nil, nil
}

// SaveResult 复刻 model/live_room_idempotency.go:107-122：
// `WHERE dedup_key = ? AND result_json = ”` —— 只回填**还没回填过**的那一次，
// 后到的重放不覆盖既有事实（返回 false，logic 侧 saveDedupResult 只记日志不报错）。
func (f *fakeIdempotencyModel) SaveResult(_ context.Context, dedupKey, resultJSON string) (bool, error) {
	f.log.add("live_room_idempotency.SaveResult:%s", dedupKey)
	if err := f.dbErr("SaveResult"); err != nil {
		return false, fmt.Errorf("live_room_idempotency SaveResult: %w", err)
	}
	if dedupKey == "" {
		return false, model.ErrRequestIDRequired
	}
	rec := f.byKey(dedupKey)
	if rec == nil || rec.ResultJSON != "" {
		return false, nil
	}
	rec.ResultJSON = resultJSON
	rec.Mtime = model.NowUnix()
	return true, nil
}

// byKey 直接扫库存行（不写 callLog：它不是一条 SQL，只是假件的索引）。
func (f *fakeIdempotencyModel) byKey(dedupKey string) *model.LiveRoomIdempotency {
	for _, r := range f.rows {
		if r.DedupKey == dedupKey {
			return r
		}
	}
	return nil
}

func (f *fakeIdempotencyModel) DeleteExpired(context.Context, int64, int64) (int64, error) {
	return 0, f.unexpected("DeleteExpired")
}

var _ model.LiveRoomIdempotencyModel = (*fakeIdempotencyModel)(nil)

// --- 布数据用的最小合法行构造器 ---

// baseRoom 给一行「生产写得出来」的房间；用例只覆盖自己关心的字段。
func baseRoom(roomID, ownerMid int64, state int32) *model.LiveRoom {
	return &model.LiveRoom{
		RoomID:       roomID,
		OwnerMid:     ownerMid,
		Title:        fmt.Sprintf("房间 %d", roomID),
		AreaID:       7001,
		State:        state,
		VerifyState:  model.VerifyStatePassed,
		StateVersion: 1,
		Platform:     model.PlatformAndroid,
		Ctime:        1000 + roomID,
		Mtime:        1000 + roomID,
	}
}

func baseSession(sessionID, roomID, mid int64, state int32) *model.LiveSession {
	s := &model.LiveSession{
		SessionID:      sessionID,
		RoomID:         roomID,
		Mid:            mid,
		State:          state,
		TitleSnapshot:  fmt.Sprintf("场次 %d 标题", sessionID),
		AreaIDSnapshot: 7001,
		ReplayState:    model.ReplayStateNone,
		Ctime:          2000 + sessionID,
		Mtime:          2000 + sessionID,
	}
	if model.SessionStateIsTerminal(state) {
		s.StartedAt = 2000 + sessionID
		s.EndedAt = 3000 + sessionID
		s.DurationSeconds = 1000
		s.EndReason = model.EndReasonAnchorStop
	}
	return s
}

// baseSetting 给一行「CreateRoom 的 Upsert 写得出来」的配置行：
// 取值照抄 helpers.go:defaultSettingRow（弹幕/回复开，录制/连麦关，视频直播），
// ctime/mtime 由 Upsert 用 now 写入，所以布数据必须给非 0 值。
func baseSetting(roomID int64, now int64) *model.LiveRoomSetting {
	return &model.LiveRoomSetting{
		RoomID:         roomID,
		DanmakuEnabled: model.BoolToInt32(true),
		ReplyEnabled:   model.BoolToInt32(true),
		RecordEnabled:  model.BoolToInt32(false),
		LinkmicEnabled: model.BoolToInt32(false),
		LiveType:       model.LiveTypeVideo,
		Ctime:          now,
		Mtime:          now,
	}
}

func baseAnchor(id, roomID, mid int64, role int32) *model.LiveRoomAnchor {
	a := &model.LiveRoomAnchor{
		ID:     id,
		RoomID: roomID,
		Mid:    mid,
		Role:   role,
		State:  model.BindStateEnabled,
		Ctime:  4000 + id,
		Mtime:  4000 + id,
	}
	if role == model.AnchorRoleOwner {
		a.OwnerRoomID = sql.NullInt64{Int64: roomID, Valid: true}
	}
	return a
}

func baseBan(banID, roomID int64, state int32) *model.LiveRoomBan {
	b := &model.LiveRoomBan{
		BanID:       banID,
		RoomID:      roomID,
		Mid:         900 + roomID,
		BanType:     model.BanTypePermanent,
		Reason:      "违规内容",
		StartAt:     4000 + banID,
		State:       state,
		OperatorMid: 555,
		Ctime:       5000 + banID,
	}
	if state == model.BanStateLifted {
		// 对齐 model/live_room_ban.go:lift 实际写入的三列，其它列由 insert 恒写成 0/""。
		b.LiftOperatorMid = 777
		b.LiftReason = "申诉通过"
		b.LiftedAt = b.StartAt + 600
	}
	return b
}

func baseArea(areaID int64, parent int64, state int32) *model.LiveArea {
	return &model.LiveArea{
		AreaID:       areaID,
		AreaName:     fmt.Sprintf("分区-%d", areaID),
		ParentAreaID: parent,
		Sort:         int32(areaID % 100),
		State:        state,
		OperatorMid:  555,
		Ctime:        6000 + areaID,
		Mtime:        6000 + areaID,
	}
}

// --- moderation 下游假件 ---

// fakeModerationClient 实现 moderationrpc.ModerationOrchestratorClient（7 个方法的 Go 接口），
// 所以注入它**不需要生产代码留任何缝**：ServiceContext.Moderation 就是这个接口类型。
// 写侧批次 1 只走 SubmitForReview；其余 6 个方法一律 unexpected（真被触达就是越界，
// 调用会进 callLog 并返回错误，序列断言当场抓到）。
type fakeModerationClient struct {
	log  *callLog
	err  error
	task int64

	// submits 捕获每次提交的入参：Business / Reason / SubmissionId 都是策略断言的对象。
	submits []*moderationrpc.SubmitReq
}

func newFakeModerationClient(log *callLog) *fakeModerationClient {
	return &fakeModerationClient{log: log, task: 9001}
}

// failWith 让 SubmitForReview 返回指定错误（模拟下游不可用）。
func (f *fakeModerationClient) failWith(err error) { f.err = err }

// taskReturns 设定回传的 task_id；0 表示「下游回了一个空任务」这一真实可能形态
// （submitProfileReview 把它判成 ErrDownstreamUnavailable）。
func (f *fakeModerationClient) taskReturns(id int64) { f.task = id }

func (f *fakeModerationClient) SubmitForReview(_ context.Context, in *moderationrpc.SubmitReq,
	_ ...grpc.CallOption) (*moderationrpc.TaskReply, error) {
	f.log.add("moderation.SubmitForReview:%d", in.GetSubmissionId())
	f.submits = append(f.submits, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.task == 0 {
		// 下游成功应答但没有任务：这就是「空任务」分支的形态。
		return &moderationrpc.TaskReply{}, nil
	}
	return &moderationrpc.TaskReply{Task: &moderationrpc.Task{
		TaskId:       f.task,
		SubmissionId: in.GetSubmissionId(),
		ContentType:  in.GetContentType(),
		Mid:          in.GetMid(),
		UpMid:        in.GetUpMid(),
		Business:     in.GetBusiness(),
		Reason:       in.GetReason(),
		State:        moderationrpc.TaskState_TASK_STATE_PENDING,
	}}, nil
}

// submitCount 读回提交次数（静默：不写 callLog）。
func (f *fakeModerationClient) submitCount() int { return len(f.submits) }

func (f *fakeModerationClient) GetTask(context.Context, *moderationrpc.TaskReq,
	...grpc.CallOption) (*moderationrpc.TaskReply, error) {
	f.log.add("moderation.GetTask:UNEXPECTED")
	return nil, fmt.Errorf("%w: moderation.GetTask", errUnexpectedDependency)
}

func (f *fakeModerationClient) GetResult(context.Context, *moderationrpc.ResultReq,
	...grpc.CallOption) (*moderationrpc.ResultReply, error) {
	f.log.add("moderation.GetResult:UNEXPECTED")
	return nil, fmt.Errorf("%w: moderation.GetResult", errUnexpectedDependency)
}

func (f *fakeModerationClient) ListTasks(context.Context, *moderationrpc.ListTasksReq,
	...grpc.CallOption) (*moderationrpc.TasksReply, error) {
	f.log.add("moderation.ListTasks:UNEXPECTED")
	return nil, fmt.Errorf("%w: moderation.ListTasks", errUnexpectedDependency)
}

func (f *fakeModerationClient) SubmitAppeal(context.Context, *moderationrpc.AppealReq,
	...grpc.CallOption) (*moderationrpc.AppealReply, error) {
	f.log.add("moderation.SubmitAppeal:UNEXPECTED")
	return nil, fmt.Errorf("%w: moderation.SubmitAppeal", errUnexpectedDependency)
}

func (f *fakeModerationClient) ProcessAppeal(context.Context, *moderationrpc.ProcessAppealReq,
	...grpc.CallOption) (*moderationrpc.AppealReply, error) {
	f.log.add("moderation.ProcessAppeal:UNEXPECTED")
	return nil, fmt.Errorf("%w: moderation.ProcessAppeal", errUnexpectedDependency)
}

func (f *fakeModerationClient) SubmitWorkerResult(context.Context, *moderationrpc.WorkerResultReq,
	...grpc.CallOption) (*moderationrpc.EmptyReply, error) {
	f.log.add("moderation.SubmitWorkerResult:UNEXPECTED")
	return nil, fmt.Errorf("%w: moderation.SubmitWorkerResult", errUnexpectedDependency)
}

var _ moderationrpc.ModerationOrchestratorClient = (*fakeModerationClient)(nil)

// --- creator / risk-control 下游假件 ---
//
// 两条纪律与 fakeModerationClient 一致：
//   - 只有 PrepareLive 真正读到的方法（UpAttr / CheckAction）有实现，
//     其余方法一律 `:UNEXPECTED` + errUnexpectedDependency——「本路径声称没碰下游」
//     会被 callLog 抓到，假成功不可能；
//   - 入参整体留存（ups/reqs），策略断言读的是**发出去的请求**，不是替身的返回值。

type fakeCreatorClient struct {
	log *callLog
	err error
	// isAuthor 是 UpAttrReply.is_author 的回值：1 有直播资格、0 没有（契约注释口径）。
	isAuthor int32
	ups      []*creatorrpc.UpAttrReq
}

func newFakeCreatorClient(log *callLog) *fakeCreatorClient {
	return &fakeCreatorClient{log: log, isAuthor: model.BoolToInt32(true)}
}

// failWith 让 UpAttr 返回指定错误（模拟 creator 不可用 → logic 记 degraded 检查项）。
func (f *fakeCreatorClient) failWith(err error) { f.err = err }

// authorReturns 设定回传的 is_author；model.BoolToInt32(true) 才是「有资格」。
func (f *fakeCreatorClient) authorReturns(isAuthor int32) { f.isAuthor = isAuthor }

func (f *fakeCreatorClient) UpAttr(_ context.Context, in *creatorrpc.UpAttrReq,
	_ ...grpc.CallOption) (*creatorrpc.UpAttrReply, error) {
	f.log.add("creator.UpAttr:%d/f%d", in.GetMid(), in.GetFrom())
	f.ups = append(f.ups, in)
	if f.err != nil {
		return nil, f.err
	}
	return &creatorrpc.UpAttrReply{IsAuthor: f.isAuthor}, nil
}

// upCount 读回 UpAttr 调用次数（静默：不写 callLog）。
func (f *fakeCreatorClient) upCount() int { return len(f.ups) }

func (f *fakeCreatorClient) unexpected(method string) error {
	f.log.add("creator.%s:UNEXPECTED", method)
	return fmt.Errorf("%w: creator.%s", errUnexpectedDependency, method)
}

func (f *fakeCreatorClient) UpSpecial(context.Context, *creatorrpc.UpSpecialReq,
	...grpc.CallOption) (*creatorrpc.UpSpecialReply, error) {
	return nil, f.unexpected("UpSpecial")
}

func (f *fakeCreatorClient) UpsSpecial(context.Context, *creatorrpc.UpsSpecialReq,
	...grpc.CallOption) (*creatorrpc.UpsSpecialReply, error) {
	return nil, f.unexpected("UpsSpecial")
}

func (f *fakeCreatorClient) UpGroups(context.Context, *creatorrpc.NoArgReq,
	...grpc.CallOption) (*creatorrpc.UpGroupsReply, error) {
	return nil, f.unexpected("UpGroups")
}

func (f *fakeCreatorClient) UpGroupMids(context.Context, *creatorrpc.UpGroupMidsReq,
	...grpc.CallOption) (*creatorrpc.UpGroupMidsReply, error) {
	return nil, f.unexpected("UpGroupMids")
}

func (f *fakeCreatorClient) SetUpSwitch(context.Context, *creatorrpc.UpSwitchReq,
	...grpc.CallOption) (*creatorrpc.EmptyReply, error) {
	return nil, f.unexpected("SetUpSwitch")
}

func (f *fakeCreatorClient) UpSwitch(context.Context, *creatorrpc.UpSwitchReq,
	...grpc.CallOption) (*creatorrpc.UpSwitchReply, error) {
	return nil, f.unexpected("UpSwitch")
}

func (f *fakeCreatorClient) GetHighAllyUps(context.Context, *creatorrpc.HighAllyUpsReq,
	...grpc.CallOption) (*creatorrpc.HighAllyUpsReply, error) {
	return nil, f.unexpected("GetHighAllyUps")
}

var _ creatorrpc.CreatorClient = (*fakeCreatorClient)(nil)

type fakeRiskClient struct {
	log *callLog
	err error
	// reply 非 nil 时原样回传（含 nil 语义：用例可以显式要求「下游回了空应答」）。
	// 为 nil 时回默认的 ALLOW。
	reply *riskrpc.CheckActionReply
	reqs  []*riskrpc.CheckActionReq
}

func newFakeRiskClient(log *callLog) *fakeRiskClient {
	return &fakeRiskClient{log: log}
}

// failWith 让 CheckAction 返回指定错误（risk 侧不可用 → logic 记 degraded 检查项）。
func (f *fakeRiskClient) failWith(err error) { f.err = err }

// returns 设定 CheckAction 的回值。
func (f *fakeRiskClient) returns(reply *riskrpc.CheckActionReply) { f.reply = reply }

// decision 只改裁决，其余字段保持默认（多数用例只关心放行/拒绝）。
func (f *fakeRiskClient) decision(d riskrpc.Decision) {
	f.reply = &riskrpc.CheckActionReply{Decision: d}
}

func (f *fakeRiskClient) CheckAction(_ context.Context, in *riskrpc.CheckActionReq,
	_ ...grpc.CallOption) (*riskrpc.CheckActionReply, error) {
	f.log.add("risk.CheckAction:%d/a%d", in.GetMid(), in.GetAction())
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.reply != nil {
		return f.reply, nil
	}
	return &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_ALLOW, RequestId: in.GetRequestId()}, nil
}

// reqCount 读回 CheckAction 调用次数（静默：不写 callLog）。
func (f *fakeRiskClient) reqCount() int { return len(f.reqs) }

func (f *fakeRiskClient) unexpected(method string) error {
	f.log.add("risk.%s:UNEXPECTED", method)
	return fmt.Errorf("%w: risk.%s", errUnexpectedDependency, method)
}

func (f *fakeRiskClient) ReportAction(context.Context, *riskrpc.ReportActionReq,
	...grpc.CallOption) (*riskrpc.ReportActionReply, error) {
	return nil, f.unexpected("ReportAction")
}

func (f *fakeRiskClient) GetDeviceProfile(context.Context, *riskrpc.GetDeviceProfileReq,
	...grpc.CallOption) (*riskrpc.GetDeviceProfileReply, error) {
	return nil, f.unexpected("GetDeviceProfile")
}

func (f *fakeRiskClient) UpsertDeviceProfile(context.Context, *riskrpc.UpsertDeviceProfileReq,
	...grpc.CallOption) (*riskrpc.UpsertDeviceProfileReply, error) {
	return nil, f.unexpected("UpsertDeviceProfile")
}

func (f *fakeRiskClient) ApplyPunishment(context.Context, *riskrpc.ApplyPunishmentReq,
	...grpc.CallOption) (*riskrpc.ApplyPunishmentReply, error) {
	return nil, f.unexpected("ApplyPunishment")
}

func (f *fakeRiskClient) LiftPunishment(context.Context, *riskrpc.LiftPunishmentReq,
	...grpc.CallOption) (*riskrpc.LiftPunishmentReply, error) {
	return nil, f.unexpected("LiftPunishment")
}

func (f *fakeRiskClient) ListPunishments(context.Context, *riskrpc.ListPunishmentsReq,
	...grpc.CallOption) (*riskrpc.ListPunishmentsReply, error) {
	return nil, f.unexpected("ListPunishments")
}

func (f *fakeRiskClient) UpsertRule(context.Context, *riskrpc.UpsertRuleReq,
	...grpc.CallOption) (*riskrpc.UpsertRuleReply, error) {
	return nil, f.unexpected("UpsertRule")
}

func (f *fakeRiskClient) ListRules(context.Context, *riskrpc.ListRulesReq,
	...grpc.CallOption) (*riskrpc.ListRulesReply, error) {
	return nil, f.unexpected("ListRules")
}

func (f *fakeRiskClient) UpsertListEntry(context.Context, *riskrpc.UpsertListEntryReq,
	...grpc.CallOption) (*riskrpc.UpsertListEntryReply, error) {
	return nil, f.unexpected("UpsertListEntry")
}

func (f *fakeRiskClient) GetListEntries(context.Context, *riskrpc.GetListEntriesReq,
	...grpc.CallOption) (*riskrpc.GetListEntriesReply, error) {
	return nil, f.unexpected("GetListEntries")
}

var _ riskrpc.RiskControlClient = (*fakeRiskClient)(nil)
