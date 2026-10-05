package logic

// fakes_test.go 是 operation logic 测试的内存替身集合
// （覆盖鉴权、账号/RBAC、菜单与运营配置、审计查询与任务编排的方法）。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造 repository.New 会连真 Redis + 真 MySQL，测试无处塞替身。
// 因此本包用例统一用 repository.NewWithDeps(真实 Cache + 内存 Store, 内存 conn,
// 内存 model, 下游) 组装**真实的 Repository + 真实的 Cache**，只把它最外层的依赖换成替身——
// 这样「口令校验、防爆破锁定、会话签发与缓存回填、权限快照与版本失效、审计脱敏、
// 角色并集判定」整条判定链都在被测路径上，而不是把 Repository 一起 mock 掉。
//
// 五条替身纪律（account / risk-control 各轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：repository 里对返回值的写回不得污染库存行，
//     否则「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 写入按真实 SQL 的口径复刻主键、列集与唯一约束（deploy/migrations/operation/*.sql）：
//     op_admin_user 的 Insert 是 `ON DUPLICATE KEY UPDATE mtime = mtime` + RowsAffected==0
//     判定重名 → model.ErrAdminExists（不是 driver 1062）；op_role 的 Insert 是普通 INSERT，
//     撞 uniq_name 才是 1062；op_role_permission / op_admin_role 主键即去重键，
//     但 model 层只对 op_admin_role 入参去重（role.go:239-247），
//     op_role_permission 不去重（role.go:197-205）→ 重复权限点会撞主键并整体回滚。
//     缺了这些口径，「重名/重复授权」这类真实故障会被替身悄悄吞掉。
//  3. 副作用按**顺序**记录（callLog），断言完整序列而不是只数次数：本服务要紧的结论是
//     「先读缓存还是先回源、吊销有没有走到缓存、审计是不是最后一步」，只数次数会漏掉次序错误。
//     条目格式 `<表>.<方法>:<关键参数>`；随机值（token）与自增主键由**响应体/读回**拿回来
//     再拼进期望序列，因此序列仍是精确比对；只有时间戳一类不可复现的值走 wantCount/前缀断言。
//  4. 布数据走替身的**静默写入路径**（put*/seed* → 直接塞 map，不写 callLog）：
//     布景不算被测调用，轨迹断言可以直接从 0 开始数。
//  5. 九张表（含 op_menu / op_config）现已全部接线：菜单与运营配置方法本身要读这两张表。
//     「未触达表留 nil 当探针」这条纪律因此只剩一层兜底——fakeConn 里嵌的是 nil
//     sqlx.SqlConn，任何绕过 model 直接发 SQL 的写法（ExecCtx/QueryRowCtx）仍会立刻 panic，
//     等价于「logic 单测路径上不允许出现真实 SQL」。
//     op_menu / op_admin_task / op_admin_task_step 与 video/catalog/rights/moderation 四个
//     下游客户端已随各自方法接入，不再属于「留 nil 当探针」的那一批。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（WHERE、JOIN 条件、ORDER BY、LIMIT/OFFSET、唯一键、
//     utf8mb4_unicode_ci 的大小写不敏感等值匹配），不证明 SQL 文本与列名本身；
//     model/*.go 的列名由 services/operation/model 的迁移同步测试守。
//     列表类替身会把**拼出来的 WHERE 文本与绑定实参**记进轨迹，因此
//     「筛选条件到底有没有进 SQL、按什么顺序进」是用例断言的一部分，而不只是「返回了几行」。
//   - 乐观并发（`WHERE state = ?` 的 CAS）用「下一步会输」的静默开关复刻，
//     验证的是**实现拿到 false 之后的处置**（是否回读、是否猜测结果、是否留痕），
//     不等价于真实 MySQL 的行锁/隔离级别行为。
//   - 缓存只复刻 Store 层的字符串读写，不等价于真实 Redis 的 TTL 到期（用例只断言下发给
//     Setex 的 ttl 数值，不模拟时间流逝后的自动失效）。
//   - PBKDF2 迭代数使用生产默认值 210000（hashAdminPasswordWithIterations 非导出，
//     测试不降强度），因此本包用例会真实付出 PBKDF2 成本；这是有意的：
//     「登录必须走真散列比对」本身就是被测结论。
//   - gRPC 框架层（server/interceptor、真实 MySQL 事务隔离、Redis 集群行为）不在纯单测可达范围，
//     本包只测到 logic 出入口。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/operation/internal/config"
	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/model"
	"go-video/services/operation/rpc"
)

func TestMain(m *testing.M) {
	// 被测路径上的 logx.Errorf/Infof 只用于人工排查，不进断言；关闭以免污染测试输出。
	logx.Disable()
	m.Run()
}

// 领域错误别名：logic 层把 repository/model 的错误原样外传（这是被测行为之一），
// 用例用 errors.Is 比对哨兵；起别名只是为了让断言里不重复 model. 前缀。
var (
	ErrAdminNotFound       = model.ErrAdminNotFound
	ErrAdminExists         = model.ErrAdminExists
	ErrAdminDisabled       = model.ErrAdminDisabled
	ErrAdminLocked         = model.ErrAdminLocked
	ErrAdminPasswordWrong  = model.ErrAdminPasswordWrong
	ErrAdminPasswordEmpty  = model.ErrAdminPasswordEmpty
	ErrAdminPasswordWeak   = model.ErrAdminPasswordWeak
	ErrAdminSelfDisable    = model.ErrAdminSelfDisable
	ErrSecondFactorMissing = model.ErrSecondFactorRequired
	ErrSecondFactorWrong   = model.ErrSecondFactorWrong
	ErrSessionInvalid      = model.ErrSessionInvalid
	ErrTokenSecretMissing  = model.ErrTokenSecretMissing
	ErrRoleNotFound        = model.ErrRoleNotFound
	ErrRoleExists          = model.ErrRoleExists
	ErrPermissionInvalid   = model.ErrPermissionInvalid
	ErrPermissionNotFound  = model.ErrPermissionNotFound
	ErrPermissionExists    = model.ErrPermissionExists
	ErrMenuNotFound        = model.ErrMenuNotFound
	ErrMenuParentSelf      = model.ErrMenuParentSelf
	ErrConfigNotFound      = model.ErrConfigNotFound
	ErrConfigExists        = model.ErrConfigExists
	ErrConfigKeyEmpty      = model.ErrConfigKeyEmpty
	ErrConfigValueInvalid  = model.ErrConfigValueInvalid
	ErrConfigVersionConf   = model.ErrConfigVersionConflict
	ErrInvalidOperator     = model.ErrInvalidOperator
	ErrDownstreamUnavail   = model.ErrDownstreamUnavailable
	ErrRoleHasMembers      = model.ErrRoleHasMembers
	ErrTaskNotFound        = model.ErrTaskNotFound
	ErrTaskExists          = model.ErrTaskExists
	ErrTaskBadTransition   = model.ErrTaskBadTransition
	ErrTaskEmptySteps      = model.ErrTaskEmptySteps
	ErrTaskTooManySteps    = model.ErrTaskTooManySteps
	ErrTaskParamsNotJSON   = model.ErrTaskParamsNotJSON
	ErrTaskTypeUnknown     = model.ErrTaskTypeUnknown
)

// 缓存 key 字面量（与 repository/cache.go 的常量一一对应）。
// 刻意在测试里重写而不是引用：key 命名空间漂移（op:rbac: 变成别的、
// 会话 key 与 account 的用户会话 key 混用）必须让用例变红。
const (
	keyRBACVersion  = "op:rbac:ver"
	keySessionPrefx = "op:sess:"
	keyMenuVersion  = "op:menu:ver"
	// missPlaceholder 与 repository/cache.go:29 同内容：不存在的配置 key 写入的哨兵。
	// 用例断言「哨兵到底有没有挡住建库」时必须能认出它。
	missPlaceholder = `{"miss":1}`
	// missGuardTTL 空值哨兵 TTL（repository/cache.go:28 defaultMissTTL）。
	missGuardTTL = 30
)

func rbacSnapshotKey(version, adminID int64) string {
	return fmt.Sprintf("op:rbac:%d:%d", version, adminID)
}

func sessionCacheKey(token string) string { return keySessionPrefx + token }

// cfgCacheKey 运营配置缓存 key（<scope>:<cfg_key>）。
// 刻意在测试里重写而非引用 repository.configCacheKey：cfg_key 有没有被归一、
// scope 与 key 的位置有没有写反，都是用例要钉住的结论。
func cfgCacheKey(scope, cfgKey string) string { return fmt.Sprintf("op:cfg:%s:%s", scope, cfgKey) }

// menuTreeKey 全量菜单树缓存 key（按菜单版本号分槽）。
func menuTreeKey(ver int64) string { return fmt.Sprintf("op:menu:%d", ver) }

// ============================================================================
// 断言小工具（本包共享）
// ============================================================================

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %v", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

// wantErr 断言确实报错了（错误内容不固定时用这条；不许用它掩盖「本该报错却成功」）。
func wantErr(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want 非 nil", label)
	}
}

// wantNoErr 断言没有错误（「不存在」「无权限」在本服务是结论不是错误时必须走这条）。
func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
	}
}

// wantOK 断言「无错误且应答非空」，并把应答原样交回给后续断言。
// 必须同时查 err 和 nil：go-zero 的 logic 允许 (nil, nil) 这种形态，
// 只断言 err==nil 会让「成功但空应答」溜过去，用例随后解引用才炸——
// 那时报错位置在断言处，读起来像测试写错了而不是实现有 bug。
// 约束取 comparable 而不是 any：nil 判定要能用 ==，应答都是指针/整数这类可比值。
func wantOK[T comparable](t *testing.T, got T, err error, label string) T {
	t.Helper()
	wantNoErr(t, label, err)
	var zero T
	if got == zero {
		t.Fatalf("%s：应答为 %v，但用例接下来要读它的具体字段", label, got)
	}
	return got
}

// wantOps 按顺序精确比较完整调用序列（长度不等直接 Fatal，避免「少一步」被忽略）。
func wantOps(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label, strings.Join(got, " → "), strings.Join(want, " → "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s：第 %d 次调用 = %s, want %s（完整序列 [%s]）",
				label, i+1, got[i], want[i], strings.Join(got, " → "))
		}
	}
}

// wantCount 断言以 prefix 开头的调用次数（用于值不可复现的 time/ts 一类步骤）。
func wantCount(t *testing.T, label string, ops []string, prefix string, want int) {
	t.Helper()
	got := countPrefix(ops, prefix)
	if got != want {
		t.Errorf("%s：%s 出现 %d 次, want %d（完整序列 [%s]）", label, prefix, got, want, strings.Join(ops, " → "))
	}
}

// wantNoOpsWith 断言序列里没有任何一项包含 needle（「这一步压根没发生」）。
func wantNoOpsWith(t *testing.T, label string, ops []string, needle string) {
	t.Helper()
	for _, o := range ops {
		if strings.Contains(o, needle) {
			t.Errorf("%s：出现了不该发生的调用 %s（完整序列 %v）", label, o, ops)
		}
	}
}

// wantZeroOps 断言守卫拒绝后**零**依赖调用（本轮所有写接口用例的地基断言）。
func wantZeroOps(t *testing.T, label string, ops []string) {
	t.Helper()
	if len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍有依赖调用 %v", label, ops)
	}
}

// wantTSWindow 断言一个 Unix 秒落在 [from, to] 内（time.Now() 产生的值不能自比、
// 也不能只断言 >0：0 与 20 年前都必须红）。
func wantTSWindow(t *testing.T, label, field string, got, from, to int64) {
	t.Helper()
	if got < from || got > to {
		t.Errorf("%s：%s = %d, want 落在 [%d, %d]", label, field, got, from, to)
	}
}

// wantNotContains 断言一段文本里没有 needle（口令/散列/手机号不外泄这条硬规则的落地方式）。
func wantNotContains(t *testing.T, label, text, needle string) {
	t.Helper()
	if needle == "" {
		t.Fatalf("%s：needle 为空，断言无意义", label)
	}
	if strings.Contains(text, needle) {
		t.Errorf("%s：文本里出现了 %q —— %s", label, needle, text)
	}
}

// wantContains 断言一段文本里确有 needle（与 wantNotContains 配对，防止拿空断言蒙混）。
func wantContains(t *testing.T, label, text, needle string) {
	t.Helper()
	if needle == "" {
		t.Fatalf("%s：needle 为空，断言无意义", label)
	}
	if !strings.Contains(text, needle) {
		t.Errorf("%s：文本里没有 %q —— %s", label, needle, text)
	}
}

// wantHex32 断言不可逆短哈希的形态（32 个小写 hex）：空串与明文都必须红。
func wantHex32(t *testing.T, label, field, v string) {
	t.Helper()
	if len(v) != 32 {
		t.Fatalf("%s：%s = %q, want 32 个 hex 字符", label, field, v)
	}
	for _, r := range v {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("%s：%s 含非 hex 字符 %q, want 全小写 hex", label, field, r)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// tsFlag 把「是否下发了时间戳」编码成可精确比对的轨迹字段：
// 真实值不可复现（time.Now()），但「这一条迁移到底有没有带 started_at/finished_at」
// 是本服务要紧的结论（终态没带 finished_at 就是那条缺陷），因此用 */0 记形状、
// 落库值另行 wantTSWindow 断言区间。
func tsFlag(n int64) string {
	if n > 0 {
		return "*"
	}
	return "0"
}

func countPrefix(ops []string, prefix string) int {
	n := 0
	for _, o := range ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

// joinInts 把 ID 列表拼成轨迹里的一个字段（顺序即回源顺序，可断言）。
func joinInts(ids []int64) string {
	ps := make([]string, 0, len(ids))
	for _, id := range ids {
		ps = append(ps, itoa(id))
	}
	return strings.Join(ps, ",")
}

// errText 取错误文本（nil 安全）。用于断言「内部错误原文外传给了调用方」这类
// 只能看报文才能定性的行为。
func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// ============================================================================
// 调用轨迹与错误注入
// ============================================================================

type callLog struct {
	mu  sync.Mutex
	ops []string
}

func (c *callLog) add(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ops...)
}

func (c *callLog) opsFrom(from int) []string {
	all := c.snapshot()
	if from > len(all) {
		from = len(all)
	}
	return all[from:]
}

func (c *callLog) reset() {
	c.mu.Lock()
	c.ops = nil
	c.mu.Unlock()
}

type faultInjector struct {
	mu  sync.Mutex
	by  map[string]error
	nth map[string]*nthFault
}

// nthFault 让某个方法的第 n 次调用失败，前 n-1 次照常成功。
type nthFault struct {
	left int
	err  error
}

func (f *faultInjector) failWith(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

// failOn 把故障点精确放在第 n 次调用上（n 从 1 起）。
// 「第一条绑定成功、第二条失败」这类事务边界只有它能证明：
// 实现到底是整批回滚还是逐条提交，看第 n 次发作后的库存行数就知道。
func (f *faultInjector) failOn(method string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nth == nil {
		f.nth = map[string]*nthFault{}
	}
	f.nth[method] = &nthFault{left: n - 1, err: err}
}

func (f *faultInjector) fail(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if nf, ok := f.nth[method]; ok {
		if nf.left > 0 {
			nf.left--
		} else {
			return nf.err
		}
	}
	return f.by[method]
}

// dupErr 复刻 MySQL 1062 唯一键冲突的报文形态（真实驱动会把 key 名与冲突值带回）。
func dupErr(key, value string) error {
	return fmt.Errorf("Error 1062: Duplicate entry '%s' for key '%s'", value, key)
}

// ============================================================================
// Redis Store 替身（repository.Store）
// ============================================================================

// fakeStore 是内存版 Redis 读写面：真实 *repository.Cache 仍留在被测路径上，
// 因此 key 派生、JSON 编解码、空值哨兵与版本号失效都被覆盖。
type fakeStore struct {
	faultInjector
	log *callLog

	mu   sync.Mutex
	vals map[string]string
	ttls map[string]int
}

func newFakeStore(log *callLog) *fakeStore {
	return &fakeStore{log: log, vals: map[string]string{}, ttls: map[string]int{}}
}

func (f *fakeStore) Get(_ context.Context, key string) (string, bool, error) {
	f.log.add("cache.Get:%s", key)
	if err := f.fail("Get"); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vals[key]
	if !ok {
		return "", false, nil // 与 redisStore.Get 一致：miss → ("", false, nil)
	}
	return v, true, nil
}

func (f *fakeStore) Setex(_ context.Context, key, val string, ttl int) error {
	f.log.add("cache.Setex:%s/%d", key, ttl)
	if err := f.fail("Setex"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ttl <= 0 {
		ttl = 30 // redisStore.Setex 的 defaultMissTTL 兜底
	}
	f.vals[key] = val
	f.ttls[key] = ttl
	return nil
}

func (f *fakeStore) Delete(_ context.Context, keys ...string) error {
	f.log.add("cache.Del:%s", strings.Join(keys, ","))
	if err := f.fail("Del"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		delete(f.vals, k)
		delete(f.ttls, k)
	}
	return nil
}

func (f *fakeStore) Incr(_ context.Context, key string, ttl int) (int64, error) {
	f.log.add("cache.Incr:%s/%d", key, ttl)
	if err := f.fail("Incr"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, _ := strconv.ParseInt(f.vals[key], 10, 64)
	n++
	f.vals[key] = strconv.FormatInt(n, 10)
	f.ttls[key] = ttl
	return n, nil
}

// --- 静默布景 / 回读（纪律 4：不写轨迹） ---

// warmJSON 静默把任意值写成某个 key 的缓存值（用于把缓存布成「命中」分支）。
func (f *fakeStore) warmJSON(key string, v any) {
	bs, err := json.Marshal(v)
	if err != nil {
		panic("布景序列化失败：" + err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals[key] = string(bs)
	f.ttls[key] = 60
}

// rawOf 回读某个 key 的原始值（生产自己写进去的序列化结果，测试不重造格式）。
func (f *fakeStore) rawOf(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vals[key]
	return v, ok
}

// ttlOf 回读某个 key 当前生效的 TTL 秒数。
func (f *fakeStore) ttlOf(key string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.ttls[key]
	return v, ok
}

// forget 静默让某个 key 变成「未命中」（不写轨迹）：用于把缓存退回 DB 口径，
// 从而单独驱动「回源 → 回填」这条分支。
func (f *fakeStore) forget(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vals, key)
	delete(f.ttls, key)
}

// hasKey 静默判断某个 key 是否仍在缓存里（「作废是靠换版本槽而不是删旧槽」这类断言用）。
func (f *fakeStore) hasKey(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.vals[key]
	return ok
}

// keys 回读当前全部 key（排序后返回，便于「缓存里只剩这些」式断言）。
func (f *fakeStore) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.vals))
	for k := range f.vals {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// allText 把整个缓存内容拼成一段文本，用于「口令/散列/手机号绝不进缓存」的兜底断言。
func (f *fakeStore) allText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	for k, v := range f.vals {
		fmt.Fprintf(&sb, "%s=%s ", k, v)
	}
	return sb.String()
}

// rewriteState 静默把某个权限快照里的 state 改掉（不写轨迹）：
// 用来复刻「快照还在缓存窗口内、账号状态已变」这条缓存与库不一致的真实场景。
func (f *fakeStore) rewriteState(t *testing.T, key string, state int32) {
	t.Helper()
	raw, ok := f.rawOf(key)
	if !ok {
		t.Fatalf("缓存里没有 %s，无法布「快照仍命中」的景（现有 key：%v）", key, f.keys())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("快照不是合法 JSON：%v", err)
	}
	m["state"] = float64(state)
	bs, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("快照重序列化失败：%v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals[key] = string(bs)
}

// ============================================================================
// 事务连接替身
// ============================================================================

// fakeConn 只实现本服务用到的 TransactCtx（角色/权限绑定是事务写）。
// 其它方法落在嵌入的 nil 接口上会直接 panic，等价于
// 「logic 单测路径上不允许出现真实 SQL」——真出现了就是越界，让它炸出来。
type fakeConn struct {
	sqlx.SqlConn
	mu         sync.Mutex
	transactOK int
	rolledBack int
}

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	// 本轮 8 个方法的替身 model 直接按语义改内存，不消费 Session；
	// 仍然记录事务开/回滚次数，供「绑定失败必须整体回滚」的断言使用。
	f.mu.Lock()
	f.transactOK++
	f.mu.Unlock()
	err := fn(ctx, &panicSession{})
	if err != nil {
		f.mu.Lock()
		f.rolledBack++
		f.mu.Unlock()
	}
	return err
}

func (f *fakeConn) stats() (opened, rolledBack int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transactOK, f.rolledBack
}

// panicSession 是「替身路径上不该出现真实 SQL」的响铃：
// 任何 ExecCtx/QueryRowCtx 调用都会 panic，而不是静默返回空结果。
type panicSession struct{ sqlx.Session }

var (
	_ sqlx.SqlConn     = (*fakeConn)(nil)
	_ repository.Store = (*fakeStore)(nil)
)

// ============================================================================
// op_admin_user 表
// ============================================================================

type fakeAdminUserModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   map[int64]*model.AdminUser
	order  []int64
	nextID int64
}

func newFakeAdminUserModel(log *callLog) *fakeAdminUserModel {
	return &fakeAdminUserModel{log: log, rows: map[int64]*model.AdminUser{}}
}

// Insert 复刻 model/admin_user.go:86-115：ctime 为 0 补 now、mtime 一律刷新、
// 回填自增 admin_id 到入参指针，并且用 ON DUPLICATE KEY + RowsAffected==0
// 把「重名」判成 model.ErrAdminExists（不依赖 driver 错误码）。
func (f *fakeAdminUserModel) Insert(_ context.Context, u *model.AdminUser) (int64, error) {
	f.log.add("admin_user.Insert:%s", u.Username)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if strings.EqualFold(f.rows[id].Username, u.Username) {
			return 0, model.ErrAdminExists // 真实 SQL：aff==0 → ErrAdminExists
		}
	}
	if u.AdminID != 0 {
		if _, ok := f.rows[u.AdminID]; ok {
			return 0, dupErr("op_admin_user.PRIMARY", itoa(u.AdminID))
		}
	}
	now := nowUnix()
	cp := *u
	if cp.Ctime == 0 {
		cp.Ctime = now
	}
	cp.Mtime = now
	f.nextID++
	cp.AdminID = f.nextID
	f.rows[cp.AdminID] = &cp
	f.order = append(f.order, cp.AdminID)
	u.AdminID, u.Ctime, u.Mtime = cp.AdminID, cp.Ctime, cp.Mtime
	return cp.AdminID, nil
}

func (f *fakeAdminUserModel) FindOne(_ context.Context, adminID int64) (*model.AdminUser, error) {
	f.log.add("admin_user.FindOne:%d", adminID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	return f.copyBy(func(r *model.AdminUser) bool { return r.AdminID == adminID }), nil
}

func (f *fakeAdminUserModel) FindByUsername(_ context.Context, username string) (*model.AdminUser, error) {
	f.log.add("admin_user.FindByUsername:%s", username)
	if err := f.fail("FindByUsername"); err != nil {
		return nil, err
	}
	// username 列是 utf8mb4_unicode_ci：大小写不敏感，按主键升序取第一条（无 ORDER BY）。
	return f.copyBy(func(r *model.AdminUser) bool { return strings.EqualFold(r.Username, username) }), nil
}

func (f *fakeAdminUserModel) FindMany(_ context.Context, adminIDs []int64) (map[int64]*model.AdminUser, error) {
	f.log.add("admin_user.FindMany:%s", joinInts(adminIDs))
	if err := f.fail("FindMany"); err != nil {
		return nil, err
	}
	out := map[int64]*model.AdminUser{}
	for _, id := range adminIDs {
		if row := f.copyBy(func(r *model.AdminUser) bool { return r.AdminID == id }); row != nil {
			out[id] = row
		}
	}
	return out, nil
}

func (f *fakeAdminUserModel) copyBy(pred func(*model.AdminUser) bool) *model.AdminUser {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if row := f.rows[id]; pred(row) {
			cp := *row
			return &cp
		}
	}
	return nil // model：sql.ErrNoRows → (nil, nil)
}

// profileCols 复刻 UpdateProfile 拼 SET 子句的顺序与条件（model/admin_user.go:173-209）。
func profileCols(patch *model.AdminUserPatch) string {
	cols := []string{"mtime"}
	if patch.Remark != nil {
		cols = append(cols, "remark")
	}
	if patch.TwoFactorTarget != nil {
		cols = append(cols, "two_factor_target")
	}
	if patch.State != nil {
		cols = append(cols, "state")
		if *patch.State == model.AdminStateNormal {
			cols = append(cols, "fail_count", "locked_until")
		}
	}
	cols = append(cols, "operator")
	return strings.Join(cols, ",")
}

// UpdateProfile 按补丁更新；行不存在时 UPDATE 影响 0 行、不报错（返回 false）。
func (f *fakeAdminUserModel) UpdateProfile(_ context.Context, adminID int64, patch *model.AdminUserPatch) (bool, error) {
	f.log.add("admin_user.UpdateProfile:%d/%s", adminID, profileCols(patch))
	if err := f.fail("UpdateProfile"); err != nil {
		return false, err
	}
	if patch == nil {
		return false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[adminID]
	if !ok {
		return false, nil
	}
	if patch.Remark != nil {
		row.Remark = *patch.Remark
	}
	if patch.TwoFactorTarget != nil {
		row.TwoFactorTarget = *patch.TwoFactorTarget
	}
	if patch.State != nil {
		row.State = *patch.State
		if *patch.State == model.AdminStateNormal {
			row.FailCount, row.LockedUntil = 0, 0
		}
	}
	row.Operator = patch.Operator
	row.Mtime = nowUnix()
	return true, nil
}

// UpdatePassword 复刻 model/admin_user.go:211-216：散列 + 算法 + operator + mtime，
// 并清零失败计数与锁定（改密即解锁）。
func (f *fakeAdminUserModel) UpdatePassword(_ context.Context, adminID int64, hash, algo string, operator int64) error {
	f.log.add("admin_user.UpdatePassword:%d", adminID)
	if err := f.fail("UpdatePassword"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[adminID]; ok {
		row.PasswordHash, row.PwdAlgo, row.Operator = hash, algo, operator
		row.FailCount, row.LockedUntil = 0, 0
		row.Mtime = nowUnix()
	}
	return nil
}

func (f *fakeAdminUserModel) UpdateLoginGuard(_ context.Context, adminID int64, failCount int32, lockedUntil int64, state int32) error {
	f.log.add("admin_user.UpdateLoginGuard:%d/%d/%d/%d", adminID, failCount, lockedUntil, state)
	if err := f.fail("UpdateLoginGuard"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[adminID]; ok {
		row.FailCount, row.LockedUntil, row.State = failCount, lockedUntil, state
		row.Mtime = nowUnix()
	}
	return nil
}

// TouchLogin 复刻 model/admin_user.go:225-230：写最近登录时间并 state=1、清零计数与锁定。
func (f *fakeAdminUserModel) TouchLogin(_ context.Context, adminID int64, at int64) error {
	f.log.add("admin_user.TouchLogin:%d", adminID)
	if err := f.fail("TouchLogin"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[adminID]; ok {
		row.LastLoginAt = at
		row.FailCount, row.LockedUntil = 0, 0
		row.State = model.AdminStateNormal
		row.Mtime = nowUnix()
	}
	return nil
}

// List 复刻 model/admin_user.go:232-265：state>0 过滤、username LIKE %kw%、
// ORDER BY admin_id DESC、COUNT 为 0 时不发第二条查询。
func (f *fakeAdminUserModel) List(_ context.Context, state int32, keyword string, pn, ps int32) ([]*model.AdminUser, int64, error) {
	f.log.add("admin_user.List:%d/%s/%d/%d", state, keyword, pn, ps)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*model.AdminUser
	for _, id := range f.order {
		row := f.rows[id]
		if state > 0 && row.State != state {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(row.Username), strings.ToLower(keyword)) {
			continue
		}
		hits = append(hits, row)
	}
	total := int64(len(hits))
	if total == 0 {
		return nil, 0, nil
	}
	slices.Reverse(hits) // admin_id DESC
	start := int((pn - 1) * ps)
	if start >= len(hits) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(hits) {
		end = len(hits)
	}
	out := make([]*model.AdminUser, 0, end-start)
	for _, row := range hits[start:end] {
		cp := *row
		out = append(out, &cp)
	}
	return out, total, nil
}

func (f *fakeAdminUserModel) CountByUsername(_ context.Context, username string) (int64, error) {
	f.log.add("admin_user.CountByUsername:%s", username)
	if err := f.fail("CountByUsername"); err != nil {
		return 0, err
	}
	n := int64(0)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if strings.EqualFold(row.Username, username) {
			n++
		}
	}
	return n, nil
}

// --- 静默布景 / 回读 ---

func (f *fakeAdminUserModel) put(u *model.AdminUser) *model.AdminUser {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *u
	if cp.AdminID == 0 {
		cp.AdminID = f.nextID
	}
	if cp.AdminID > f.nextID {
		f.nextID = cp.AdminID
	}
	if _, exists := f.rows[cp.AdminID]; !exists {
		f.order = append(f.order, cp.AdminID)
		slices.Sort(f.order)
	}
	f.rows[cp.AdminID] = &cp
	return &cp
}

// mutate 静默改库存行本身（不写轨迹、不新增行）：用于把账号布成
// 「已禁用 / 已锁定 / 散列被写坏」这类无法通过正常写路径得到的状态。
func (f *fakeAdminUserModel) mutate(t *testing.T, adminID int64, fn func(*model.AdminUser)) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[adminID]
	if !ok {
		t.Fatalf("op_admin_user 里没有 admin_id=%d 的行，无法布景", adminID)
	}
	fn(row)
}

func (f *fakeAdminUserModel) get(adminID int64) *model.AdminUser {
	return f.copyBy(func(r *model.AdminUser) bool { return r.AdminID == adminID })
}

func (f *fakeAdminUserModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// ============================================================================
// op_admin_session 表
// ============================================================================

type fakeSessionModel struct {
	faultInjector
	log   *callLog
	mu    sync.Mutex
	rows  map[string]*model.AdminSession
	order []string
}

func newFakeSessionModel(log *callLog) *fakeSessionModel {
	return &fakeSessionModel{log: log, rows: map[string]*model.AdminSession{}}
}

// Insert 复刻 model/admin_session.go:54-65：token 即主键（无自增），ctime 为 0 补 now；
// 重复签发同一 token 撞 PRIMARY。
func (f *fakeSessionModel) Insert(_ context.Context, s *model.AdminSession) error {
	f.log.add("admin_session.Insert:%d", s.AdminID)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[s.Token]; ok {
		return dupErr("op_admin_session.PRIMARY", s.Token)
	}
	cp := *s
	if cp.Ctime == 0 {
		cp.Ctime = nowUnix()
	}
	f.rows[cp.Token] = &cp
	f.order = append(f.order, cp.Token)
	s.Ctime = cp.Ctime
	return nil
}

func (f *fakeSessionModel) FindByToken(_ context.Context, token string) (*model.AdminSession, error) {
	f.log.add("admin_session.FindByToken:%s", token)
	if err := f.fail("FindByToken"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[token]
	if !ok {
		return nil, nil // model：sql.ErrNoRows → (nil, nil)
	}
	cp := *row
	return &cp, nil
}

// Revoke 复刻 model/admin_session.go:79-82：SET state=2 WHERE token = ?（不带 state 条件）。
func (f *fakeSessionModel) Revoke(_ context.Context, token string) error {
	f.log.add("admin_session.Revoke:%s", token)
	if err := f.fail("Revoke"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[token]; ok {
		row.State = model.SessionStateRevoked
	}
	return nil
}

// RevokeAllByAdmin 复刻 model/admin_session.go:84-88：只吊销 state=1 的行。
func (f *fakeSessionModel) RevokeAllByAdmin(_ context.Context, adminID int64) error {
	f.log.add("admin_session.RevokeAllByAdmin:%d", adminID)
	if err := f.fail("RevokeAllByAdmin"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tok := range f.order {
		if row := f.rows[tok]; row.AdminID == adminID && row.State == model.SessionStateActive {
			row.State = model.SessionStateRevoked
		}
	}
	return nil
}

func (f *fakeSessionModel) PurgeExpired(_ context.Context, before int64) (int64, error) {
	f.log.add("admin_session.PurgeExpired:%d", before)
	if err := f.fail("PurgeExpired"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, tok := range append([]string(nil), f.order...) {
		if f.rows[tok].Expires < before {
			delete(f.rows, tok)
			f.order = slices.DeleteFunc(f.order, func(x string) bool { return x == tok })
			n++
		}
	}
	return n, nil
}

func (f *fakeSessionModel) put(s *model.AdminSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *s
	f.rows[cp.Token] = &cp
	f.order = append(f.order, cp.Token)
}

func (f *fakeSessionModel) get(token string) *model.AdminSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[token]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// delete 静默删行（不写轨迹）：用于把「缓存里有会话、库里没有」这种不一致布出来。
func (f *fakeSessionModel) delete(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, token)
	f.order = slices.DeleteFunc(f.order, func(x string) bool { return x == token })
}

func (f *fakeSessionModel) activeFor(adminID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, row := range f.rows {
		if row.AdminID == adminID && row.State == model.SessionStateActive {
			n++
		}
	}
	return n
}

func (f *fakeSessionModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// ============================================================================
// op_role / op_role_permission / op_admin_role（model.RoleModel 一张嘴管三张表）
// ============================================================================

type fakeRoleModel struct {
	faultInjector
	log  *callLog
	mu   sync.Mutex
	role map[int64]*model.Role
	// perms: role_id → permission_id 列表（op_role_permission）
	perms map[int64][]int64
	// bindings: admin_id → role_id 列表（op_admin_role）
	bindings map[int64][]int64
	order    []int64
	nextID   int64
	// permRow 静默回读权限点行（组装时接到 fakePermissionModel）。
	// 真实 LoadAdminGrants 是 LEFT JOIN op_permission，判定读的 Resource/Action
	// 来自那张表；替身若只回 PermissionID，RBAC 判定会永远 deny，
	// 通配符与超管边界这些结论就成了假绿灯，所以这里必须补齐整行。
	permRow func(permissionID int64) *model.Permission
}

func newFakeRoleModel(log *callLog) *fakeRoleModel {
	return &fakeRoleModel{log: log, role: map[int64]*model.Role{}, perms: map[int64][]int64{}, bindings: map[int64][]int64{}}
}

// Insert 复刻 model/role.go:100-118：普通 INSERT（撞 uniq_name 才是 1062）、
// ctime 为 0 补 now、mtime 刷新、回填自增 role_id。
func (f *fakeRoleModel) Insert(_ context.Context, r *model.Role) (int64, error) {
	f.log.add("role.Insert:%s", r.Name)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if strings.EqualFold(f.role[id].Name, r.Name) {
			return 0, dupErr("op_role.uniq_name", r.Name)
		}
	}
	now := nowUnix()
	cp := *r
	if cp.Ctime == 0 {
		cp.Ctime = now
	}
	cp.Mtime = now
	f.nextID++
	cp.RoleID = f.nextID
	f.role[cp.RoleID] = &cp
	f.order = append(f.order, cp.RoleID)
	r.RoleID, r.Ctime, r.Mtime = cp.RoleID, cp.Ctime, cp.Mtime
	return cp.RoleID, nil
}

func (f *fakeRoleModel) FindOne(_ context.Context, roleID int64) (*model.Role, error) {
	f.log.add("role.FindOne:%d", roleID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.role[roleID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeRoleModel) FindByName(_ context.Context, name string) (*model.Role, error) {
	f.log.add("role.FindByName:%s", name)
	if err := f.fail("FindByName"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if strings.EqualFold(f.role[id].Name, name) {
			cp := *f.role[id]
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeRoleModel) List(_ context.Context, state int32, keyword string, pn, ps int32) ([]*model.Role, int64, error) {
	f.log.add("role.List:%d/%s/%d/%d", state, keyword, pn, ps)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*model.Role
	for _, id := range f.order { // role_id ASC
		row := f.role[id]
		if state > 0 && row.State != state {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(row.Name), strings.ToLower(keyword)) &&
			!strings.Contains(strings.ToLower(row.Title), strings.ToLower(keyword)) {
			continue
		}
		cp := *row
		hits = append(hits, &cp)
	}
	total := int64(len(hits))
	if total == 0 {
		return nil, 0, nil
	}
	start := int((pn - 1) * ps)
	if start >= len(hits) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(hits) {
		end = len(hits)
	}
	return hits[start:end], total, nil
}

// Delete 复刻 model/role.go:177-190：事务内删两张绑定 + 角色行。
func (f *fakeRoleModel) Delete(_ context.Context, roleID int64) error {
	f.log.add("role.Delete:%d", roleID)
	if err := f.fail("Delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.role, roleID)
	delete(f.perms, roleID)
	f.order = slices.DeleteFunc(f.order, func(x int64) bool { return x == roleID })
	for adminID, ids := range f.bindings {
		f.bindings[adminID] = slices.DeleteFunc(append([]int64(nil), ids...), func(x int64) bool { return x == roleID })
	}
	return nil
}

// GrantPermissions 复刻 model/role.go:192-209：事务内 DELETE + 逐条 INSERT。
// **入参不去重**，因此重复 permission_id 撞主键 (role_id, permission_id) → 整批回滚，
// 旧绑定必须仍在（替身按事务口径先检查再改）。
func (f *fakeRoleModel) GrantPermissions(_ context.Context, roleID int64, permissionIDs []int64) error {
	f.log.add("role_permission.GrantPermissions:%d/%s", roleID, joinInts(permissionIDs))
	if err := f.fail("GrantPermissions"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[int64]bool{}
	for _, pid := range permissionIDs {
		if pid <= 0 {
			return errors.New("op_role_permission: invalid permission_id")
		}
		if seen[pid] {
			return dupErr("op_role_permission.PRIMARY", fmt.Sprintf("%d-%d", roleID, pid))
		}
		seen[pid] = true
	}
	f.perms[roleID] = append([]int64(nil), permissionIDs...)
	slices.Sort(f.perms[roleID])
	return nil
}

func (f *fakeRoleModel) ListPermissionIDs(_ context.Context, roleID int64) ([]int64, error) {
	f.log.add("role_permission.ListPermissionIDs:%d", roleID)
	if err := f.fail("ListPermissionIDs"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.perms[roleID]...), nil
}

func (f *fakeRoleModel) CountMembers(_ context.Context, roleID int64) (int64, error) {
	f.log.add("admin_role.CountMembers:%d", roleID)
	if err := f.fail("CountMembers"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, ids := range f.bindings {
		for _, id := range ids {
			if id == roleID {
				n++
			}
		}
	}
	return n, nil
}

// ReplaceAdminRoles 复刻 model/role.go:234-256：事务内 DELETE + 逐条 INSERT，
// 入参**去重**（与 GrantPermissions 不对称，是真实差异）。非法 id 直接失败且不落任何变更。
func (f *fakeRoleModel) ReplaceAdminRoles(_ context.Context, adminID int64, roleIDs []int64) error {
	f.log.add("admin_role.ReplaceAdminRoles:%d/%s", adminID, joinInts(roleIDs))
	if err := f.fail("ReplaceAdminRoles"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[int64]bool{}
	next := make([]int64, 0, len(roleIDs))
	for _, rid := range roleIDs {
		if rid <= 0 {
			return errors.New("op_admin_role: invalid role_id")
		}
		if seen[rid] {
			continue
		}
		seen[rid] = true
		next = append(next, rid)
	}
	slices.Sort(next)
	f.bindings[adminID] = next
	return nil
}

func (f *fakeRoleModel) ListRoleIDsByAdmin(_ context.Context, adminID int64) ([]int64, error) {
	f.log.add("admin_role.ListRoleIDsByAdmin:%d", adminID)
	if err := f.fail("ListRoleIDsByAdmin"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.bindings[adminID]...), nil
}

func (f *fakeRoleModel) ListAdminsByRole(_ context.Context, roleID int64) ([]int64, error) {
	f.log.add("admin_role.ListAdminsByRole:%d", roleID)
	if err := f.fail("ListAdminsByRole"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for adminID, ids := range f.bindings {
		for _, id := range ids {
			if id == roleID {
				out = append(out, adminID)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// ListRoleBindings 复刻 model/role.go:282-301：INNER JOIN op_role，
// 因此角色行被删掉时绑定不再出现；ORDER BY admin_id ASC, role_id ASC。
func (f *fakeRoleModel) ListRoleBindings(_ context.Context, adminIDs []int64) ([]*model.AdminRolePair, error) {
	f.log.add("admin_role.ListRoleBindings:%s", joinInts(adminIDs))
	if err := f.fail("ListRoleBindings"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[int64]bool{}
	for _, id := range adminIDs {
		want[id] = true
	}
	var out []*model.AdminRolePair
	ids := mapsKeys(want) // 替身按 admin_id 升序遍历，与 ORDER BY admin_id ASC 对齐
	slices.Sort(ids)
	for _, adminID := range ids {
		for _, roleID := range f.bindings[adminID] { // 替身保证 bindings 已升序
			role, ok := f.role[roleID]
			if !ok {
				continue // INNER JOIN：无角色行则不出现在结果里
			}
			out = append(out, &model.AdminRolePair{AdminID: adminID, RoleID: roleID, RoleName: role.Name})
		}
	}
	return out, nil
}

// LoadAdminGrants 复刻 model/role.go:320-370：JOIN op_role ON r.state = 1
// （停用角色整体不参与判定）、LEFT JOIN 权限点（未绑权限的角色也返回，Permissions 为空），
// ORDER BY r.role_id ASC, p.permission_id ASC。
func (f *fakeRoleModel) LoadAdminGrants(_ context.Context, adminID int64) ([]*model.RoleGrant, error) {
	f.log.add("role.LoadAdminGrants:%d", adminID)
	if err := f.fail("LoadAdminGrants"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.RoleGrant
	for _, roleID := range f.bindings[adminID] {
		role, ok := f.role[roleID]
		if !ok || role.State != model.StateEnable {
			continue
		}
		g := &model.RoleGrant{Role: &model.Role{
			RoleID: role.RoleID, Name: role.Name, Title: role.Title,
			State: role.State, Operator: role.Operator, Ctime: role.Ctime, Mtime: role.Mtime,
		}}
		for _, pid := range f.perms[roleID] {
			// 真实 SQL 是 LEFT JOIN op_permission + IFNULL(...,0)：绑定悬空
			// （权限点行被删）时该列读成 0，不进 Permissions。替身同口径。
			row := f.permRow(pid)
			if row == nil {
				continue
			}
			cp := *row
			g.Permissions = append(g.Permissions, &cp)
		}
		out = append(out, g)
	}
	return out, nil
}

// --- 静默布景 / 回读 ---

func (f *fakeRoleModel) putRole(r *model.Role) *model.Role {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *r
	if cp.RoleID == 0 {
		cp.RoleID = f.nextID
	}
	if cp.RoleID > f.nextID {
		f.nextID = cp.RoleID
	}
	if _, exists := f.role[cp.RoleID]; !exists {
		f.order = append(f.order, cp.RoleID)
		slices.Sort(f.order)
	}
	f.role[cp.RoleID] = &cp
	return &cp
}

func (f *fakeRoleModel) putBindings(adminID int64, roleIDs []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := append([]int64(nil), roleIDs...)
	slices.Sort(next)
	f.bindings[adminID] = next
}

func (f *fakeRoleModel) grant(roleID int64, permissionIDs []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := append([]int64(nil), permissionIDs...)
	slices.Sort(next)
	f.perms[roleID] = next
}

func (f *fakeRoleModel) rolesOf(adminID int64) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.bindings[adminID]...)
}

func (f *fakeRoleModel) permsOf(roleID int64) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.perms[roleID]...)
}

func (f *fakeRoleModel) roleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.role)
}

// mapsKeys 是小工具：ListRoleBindings 需要按 admin_id 升序遍历命中集合。
func mapsKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ============================================================================
// op_permission 表
// ============================================================================

type fakePermissionModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   map[int64]*model.Permission
	order  []int64
	nextID int64
}

func newFakePermissionModel(log *callLog) *fakePermissionModel {
	return &fakePermissionModel{log: log, rows: map[int64]*model.Permission{}}
}

func (f *fakePermissionModel) Insert(_ context.Context, p *model.Permission) (int64, error) {
	f.log.add("permission.Insert:%s#%s", p.Resource, p.Action)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		row := f.rows[id]
		if row.Resource == p.Resource && row.Action == p.Action {
			return 0, dupErr("op_permission.uniq_resource_action", p.Resource+"-"+p.Action)
		}
	}
	cp := *p
	if cp.Ctime == 0 {
		cp.Ctime = nowUnix()
	}
	f.nextID++
	cp.PermissionID = f.nextID
	f.rows[cp.PermissionID] = &cp
	f.order = append(f.order, cp.PermissionID)
	// model/permission.go:56-69 会把 ctime 兜底值与自增主键一起写回**入参指针**，
	// repository 正是靠这一点把 ctime 回传给调用方（role.go:294-301）；
	// 少了这次写回，「应答里的 ctime 与库里是否一致」这条结论就测不出来。
	p.PermissionID, p.Ctime = cp.PermissionID, cp.Ctime
	return cp.PermissionID, nil
}

func (f *fakePermissionModel) FindOne(_ context.Context, permissionID int64) (*model.Permission, error) {
	f.log.add("permission.FindOne:%d", permissionID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[permissionID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakePermissionModel) FindByResourceAction(_ context.Context, resource, action string) (*model.Permission, error) {
	f.log.add("permission.FindByResourceAction:%s#%s", resource, action)
	if err := f.fail("FindByResourceAction"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		row := f.rows[id]
		if row.Resource == resource && row.Action == action {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakePermissionModel) FindMany(_ context.Context, ids []int64) (map[int64]*model.Permission, error) {
	f.log.add("permission.FindMany:%s", joinInts(ids))
	if err := f.fail("FindMany"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]*model.Permission{}
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			cp := *row
			out[id] = &cp
		}
	}
	return out, nil
}

// List 复刻 model/permission.go:120-147：COUNT 先行（为 0 时不发第二条查询）、
// domain 空则不过滤、ORDER BY domain,resource,action、LIMIT/OFFSET。
// WHERE 文本与绑定实参一起进轨迹，因此「域筛选到底进没进 SQL」是可断言的。
func (f *fakePermissionModel) List(_ context.Context, domain string, pn, ps int32) ([]*model.Permission, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]string, 0, 1)
	if domain != "" {
		where += " AND domain = ?"
		args = append(args, domain)
	}
	f.log.add("permission.List:%s[%s]/%d/%d", where, strings.Join(args, ","), pn, ps)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*model.Permission
	for _, id := range f.order {
		row := f.rows[id]
		// op_permission 整表 utf8mb4_unicode_ci：domain 等值比较大小写不敏感。
		// 不做 EqualFold 会假装出一个「大写筛选查不到」的假缺陷。
		if domain != "" && !strings.EqualFold(row.Domain, domain) {
			continue
		}
		cp := *row
		hits = append(hits, &cp)
	}
	slices.SortFunc(hits, func(a, b *model.Permission) int {
		if c := strings.Compare(a.Domain, b.Domain); c != 0 {
			return c
		}
		if c := strings.Compare(a.Resource, b.Resource); c != 0 {
			return c
		}
		return strings.Compare(a.Action, b.Action)
	})
	total := int64(len(hits))
	if total == 0 {
		return nil, 0, nil
	}
	start := int((pn - 1) * ps)
	if start >= len(hits) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(hits) {
		end = len(hits)
	}
	return hits[start:end], total, nil
}

func (f *fakePermissionModel) put(p *model.Permission) *model.Permission {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *p
	if cp.PermissionID == 0 {
		cp.PermissionID = f.nextID
	}
	if cp.PermissionID > f.nextID {
		f.nextID = cp.PermissionID
	}
	if _, exists := f.rows[cp.PermissionID]; !exists {
		f.order = append(f.order, cp.PermissionID)
		slices.Sort(f.order)
	}
	f.rows[cp.PermissionID] = &cp
	return &cp
}

// rowOf 静默按 permission_id 回读整行（不写轨迹），供 LoadAdminGrants 补全
// LEFT JOIN op_permission 才有的 Resource/Action。
func (f *fakePermissionModel) rowOf(permissionID int64) *model.Permission {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[permissionID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// idOf 静默按 (resource, action) 回读 permission_id（布景去重用，不写轨迹）：
// op_permission 有 uniq_resource_action，布景重复插同一权限点必须复用已有行。
func (f *fakePermissionModel) idOf(resource, action string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		row := f.rows[id]
		if row.Resource == resource && row.Action == action {
			return id, true
		}
	}
	return 0, false
}

// count 静默回读行数（「失败后库里到底留没留下东西」这类断言用）。
func (f *fakePermissionModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// ============================================================================
// op_menu 表
// ============================================================================

// fakeMenuModel 复刻 model/menu.go 的 defaultMenuModel。
// 两条要紧的口径：
//  1. Insert 会把 ctime 兜底值、mtime 与自增 menu_id **写回入参指针**（model/menu.go:64-83），
//     所以 repository.SaveMenu 的新建路径能回传真实时间戳；
//  2. Update 只 SET 那八列并刷新 mtime，**不回写任何东西**（model/menu.go:85-94），
//     而 repository/menu.go:215-232 返回的是它自己 new 出来的 node ——
//     于是更新路径的应答 ctime/mtime 恒为 0。这条缺陷正是靠这两点差异才钉得住。
type fakeMenuModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   map[int64]*model.Menu
	order  []int64
	nextID int64
}

func newFakeMenuModel(log *callLog) *fakeMenuModel {
	return &fakeMenuModel{log: log, rows: map[int64]*model.Menu{}}
}

func (f *fakeMenuModel) Insert(_ context.Context, node *model.Menu) (int64, error) {
	f.log.add("menu.Insert:%d/%s", node.ParentID, node.Name)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if node.MenuID != 0 {
		if _, ok := f.rows[node.MenuID]; ok {
			return 0, dupErr("op_menu.PRIMARY", itoa(node.MenuID))
		}
	}
	now := nowUnix()
	cp := *node
	if cp.Ctime == 0 {
		cp.Ctime = now
	}
	cp.Mtime = now
	f.nextID++
	cp.MenuID = f.nextID
	f.rows[cp.MenuID] = &cp
	f.order = append(f.order, cp.MenuID)
	node.MenuID, node.Ctime, node.Mtime = cp.MenuID, cp.Ctime, cp.Mtime
	return cp.MenuID, nil
}

// Update 按 menu_id 覆盖八列并刷新 mtime；ctime 不动。
func (f *fakeMenuModel) Update(_ context.Context, node *model.Menu) error {
	f.log.add("menu.Update:%d/%s/%s/%d", node.MenuID, node.Name, node.RequiredPermission, node.State)
	if err := f.fail("Update"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[node.MenuID]
	if !ok {
		return nil // UPDATE 影响 0 行不报错（存在性由 repository 的 FindOne 预检负责）
	}
	row.ParentID, row.Name, row.Path, row.Icon = node.ParentID, node.Name, node.Path, node.Icon
	row.Sort, row.RequiredPermission, row.State = node.Sort, node.RequiredPermission, node.State
	row.Mtime = nowUnix()
	return nil
}

func (f *fakeMenuModel) FindOne(_ context.Context, menuID int64) (*model.Menu, error) {
	f.log.add("menu.FindOne:%d", menuID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[menuID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// ListAll 复刻 model/menu.go:108-118：无过滤、ORDER BY parent_id ASC, sort ASC, menu_id ASC，
// 含隐藏节点（可见性过滤在 repository 内存里做）。
func (f *fakeMenuModel) ListAll(_ context.Context) ([]*model.Menu, error) {
	f.log.add("menu.ListAll")
	if err := f.fail("ListAll"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := append([]int64(nil), f.order...)
	slices.SortFunc(ids, func(a, b int64) int {
		x, y := f.rows[a], f.rows[b]
		if x.ParentID != y.ParentID {
			return int(x.ParentID - y.ParentID)
		}
		if x.Sort != y.Sort {
			return int(x.Sort) - int(y.Sort)
		}
		return int(x.MenuID - y.MenuID)
	})
	out := make([]*model.Menu, 0, len(ids))
	for _, id := range ids {
		cp := *f.rows[id]
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeMenuModel) CountChildren(_ context.Context, menuID int64) (int64, error) {
	f.log.add("menu.CountChildren:%d", menuID)
	if err := f.fail("CountChildren"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, id := range f.order {
		if f.rows[id].ParentID == menuID {
			n++
		}
	}
	return n, nil
}

// --- 静默布景 / 回读 ---

// put 静默插一个节点：menu_id 未给则取 nextID+1（显式给定时保持给定值，
// 便于布出「parent_id → sort」这种排序场景）。
func (f *fakeMenuModel) put(node *model.Menu) *model.Menu {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *node
	if cp.MenuID == 0 {
		f.nextID++
		cp.MenuID = f.nextID
	}
	if cp.MenuID > f.nextID {
		f.nextID = cp.MenuID
	}
	if _, exists := f.rows[cp.MenuID]; !exists {
		f.order = append(f.order, cp.MenuID)
	}
	f.rows[cp.MenuID] = &cp
	return &cp
}

func (f *fakeMenuModel) get(menuID int64) *model.Menu {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[menuID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeMenuModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// ============================================================================
// op_config 表
// ============================================================================

// fakeOpsConfigModel 复刻 model/config.go 的 defaultOpsConfigModel。
// 三条必须复刻到位的口径：
//  1. Insert 是 `ON DUPLICATE KEY UPDATE mtime = mtime` + RowsAffected==0 →
//     model.ErrConfigExists（model/config.go:79-94），不依赖 driver 错误码；
//     repository 再把它翻译成「已存在，请带 expect_version」的乐观锁冲突。
//  2. UpdateWithVersion 的 SET 带 `AND version = ?`（model/config.go:115-135），
//     未命中时 (nil,false,nil) 且不报错；命中时**返回的是入参结构体的拷贝**
//     并只把 version 置为 next —— 所以应答里的 mtime 是更新前的旧值（列缺陷）。
//  3. uniq_cfg_key_scope 落在 utf8mb4_unicode_ci 表上：cfg_key 与 scope 的等值
//     比较**大小写不敏感**，而 repository 只把 scope 归一为小写、cfg_key 保留原文，
//     缓存 key 又按原文派生 ⇒ 「DB 一行 / 缓存两槽」的分裂必须能在替身里复现。
type fakeOpsConfigModel struct {
	faultInjector
	log      *callLog
	mu       sync.Mutex
	rows     map[int64]*model.OpsConfig
	order    []int64
	nextID   int64
	lostNext int // 接下来 N 次 UpdateWithVersion 的 CAS 会输给别的推进者
}

func newFakeOpsConfigModel(log *callLog) *fakeOpsConfigModel {
	return &fakeOpsConfigModel{log: log, rows: map[int64]*model.OpsConfig{}}
}

// matchLocked 调用方持锁：按 (cfg_key, scope) 的 ci 等值找行。
func (f *fakeOpsConfigModel) matchLocked(cfgKey, scope string) *model.OpsConfig {
	for _, id := range f.order {
		row := f.rows[id]
		if strings.EqualFold(row.CfgKey, cfgKey) && strings.EqualFold(row.Scope, scope) {
			return row
		}
	}
	return nil
}

func (f *fakeOpsConfigModel) Insert(_ context.Context, c *model.OpsConfig) (int64, error) {
	f.log.add("ops_config.Insert:%s/%s", c.CfgKey, c.Scope)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.matchLocked(c.CfgKey, c.Scope); row != nil {
		return 0, model.ErrConfigExists // ON DUPLICATE KEY UPDATE mtime=mtime → aff 0
	}
	if c.ID != 0 {
		if _, ok := f.rows[c.ID]; ok {
			return 0, dupErr("op_config.PRIMARY", itoa(c.ID))
		}
	}
	now := nowUnix()
	cp := *c
	if cp.Ctime == 0 {
		cp.Ctime = now
	}
	cp.Mtime = now
	if cp.Version == 0 {
		cp.Version = 1
	}
	f.nextID++
	cp.ID = f.nextID
	f.rows[cp.ID] = &cp
	f.order = append(f.order, cp.ID)
	c.ID, c.Ctime, c.Mtime, c.Version = cp.ID, cp.Ctime, cp.Mtime, cp.Version
	return cp.ID, nil
}

func (f *fakeOpsConfigModel) FindOne(_ context.Context, cfgKey, scope string) (*model.OpsConfig, error) {
	f.log.add("ops_config.FindOne:%s/%s", cfgKey, scope)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.matchLocked(cfgKey, scope)
	if row == nil {
		return nil, nil // sql.ErrNoRows → (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeOpsConfigModel) UpdateWithVersion(_ context.Context, c *model.OpsConfig, expect int64) (*model.OpsConfig, bool, error) {
	f.log.add("ops_config.UpdateWithVersion:%s/%s/expect=%d", c.CfgKey, c.Scope, expect)
	if err := f.fail("UpdateWithVersion"); err != nil {
		return nil, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lostNext > 0 {
		f.lostNext-- // 读后写之间被抢先推进
		return nil, false, nil
	}
	row := f.matchLocked(c.CfgKey, c.Scope)
	if row == nil || row.Version != expect {
		return nil, false, nil // WHERE version = ? 未命中：影响 0 行，不报错
	}
	row.CfgValue, row.ValueType, row.State = c.CfgValue, c.ValueType, c.State
	row.Operator, row.Remark = c.Operator, c.Remark
	row.Version, row.Mtime = expect+1, nowUnix()
	out := *c // 与 model/config.go:133 一致：回的是入参拷贝，mtime 未跟新
	out.Version = expect + 1
	return &out, true, nil
}

// loseNextUpdate 静默声明「接下来一次 CAS 会输给别的推进者」（不写轨迹）。
func (f *fakeOpsConfigModel) loseNextUpdate() {
	f.mu.Lock()
	f.lostNext++
	f.mu.Unlock()
}

// List 复刻 model/config.go:138-165（本期无 logic 入口走它，实现只为满足接口并留痕）。
func (f *fakeOpsConfigModel) List(_ context.Context, scope string, pn, ps int32) ([]*model.OpsConfig, int64, error) {
	where := "WHERE 1 = 1"
	var args []string
	if scope != "" {
		where += " AND scope = ?"
		args = append(args, scope)
	}
	f.log.add("ops_config.List:%s[%s]/%d/%d", where, strings.Join(args, ","), pn, ps)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*model.OpsConfig
	for _, id := range f.order {
		row := f.rows[id]
		if scope != "" && !strings.EqualFold(row.Scope, scope) {
			continue
		}
		cp := *row
		hits = append(hits, &cp)
	}
	slices.SortFunc(hits, func(a, b *model.OpsConfig) int {
		if c := strings.Compare(a.CfgKey, b.CfgKey); c != 0 {
			return c
		}
		return strings.Compare(a.Scope, b.Scope)
	})
	total := int64(len(hits))
	if total == 0 {
		return nil, 0, nil
	}
	start := int((pn - 1) * ps)
	if start >= len(hits) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(hits) {
		end = len(hits)
	}
	return hits[start:end], total, nil
}

// --- 静默布景 / 回读 ---

// put 静默写一行（同 (cfg_key, scope) 覆盖，不写轨迹）。
func (f *fakeOpsConfigModel) put(c *model.OpsConfig) *model.OpsConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *c
	if row := f.matchLocked(cp.CfgKey, cp.Scope); row != nil {
		cp.ID = row.ID
		f.rows[cp.ID] = &cp
		return &cp
	}
	if cp.ID == 0 {
		f.nextID++
		cp.ID = f.nextID
	}
	if cp.ID > f.nextID {
		f.nextID = cp.ID
	}
	f.rows[cp.ID] = &cp
	f.order = append(f.order, cp.ID)
	return &cp
}

func (f *fakeOpsConfigModel) get(cfgKey, scope string) *model.OpsConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.matchLocked(cfgKey, scope)
	if row == nil {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeOpsConfigModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// ============================================================================
// op_audit_index 表
// ============================================================================

type fakeAuditModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   []*model.AuditIndex
	nextID int64
	// userRow 是 List 的 `LEFT JOIN op_admin_user u ON u.admin_id = a.admin_id`
	// （model/audit.go:137-140）：username **不是** op_audit_index 的列，
	// 写入时 newAuditIndex 根本不填它，只有查侧 JOIN 才拿得到。
	// 替身若省掉这一步，「审计列表显示不出是谁做的」这类缺陷就会被静默掩盖。
	userRow func(adminID int64) *model.AdminUser
}

func newFakeAuditModel(log *callLog) *fakeAuditModel {
	return &fakeAuditModel{log: log}
}

// Insert 复刻 model/audit.go:77-94：ctime 为 0 补 now、回填自增 id、写回入参指针。
func (f *fakeAuditModel) Insert(_ context.Context, a *model.AuditIndex) (int64, error) {
	f.log.add("audit_index.Insert:%s/%s", a.Action, a.Result)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *a
	if cp.Ctime == 0 {
		cp.Ctime = nowUnix()
	}
	cp.Username = "" // 表里没有这一列：即使调用方塞了也不落库（读侧只能靠 JOIN）
	f.nextID++
	cp.ID = f.nextID
	f.rows = append(f.rows, &cp)
	a.ID, a.Ctime = cp.ID, cp.Ctime
	return cp.ID, nil
}

// auditWhere 按 model/audit.go:96-122 的**顺序**拼 WHERE，并同步收集绑定实参。
// 顺序即结论：换条件顺序、漏传一个筛选字段都会让整段轨迹断言变红。
func auditWhere(flt model.AuditFilter) (string, []string) {
	where := "WHERE 1 = 1"
	var args []string
	if flt.AdminID > 0 {
		where += " AND a.admin_id = ?"
		args = append(args, itoa(flt.AdminID))
	}
	if flt.Action != "" {
		where += " AND a.action = ?"
		args = append(args, flt.Action)
	}
	if flt.ResourceType != "" {
		where += " AND a.resource_type = ?"
		args = append(args, flt.ResourceType)
	}
	if flt.ResourceID != "" {
		where += " AND a.resource_id = ?"
		args = append(args, flt.ResourceID)
	}
	if flt.StartAt > 0 {
		where += " AND a.ctime >= ?" // 含下界
		args = append(args, itoa(flt.StartAt))
	}
	if flt.EndAt > 0 {
		where += " AND a.ctime < ?" // 不含上界
		args = append(args, itoa(flt.EndAt))
	}
	return where, args
}

// List 复刻 model/audit.go:96-148：COUNT 先行（total==0 时不发第二条查询）、
// 六个筛选条件按上述顺序进 WHERE、LEFT JOIN 取 username、ORDER BY a.id DESC、LIMIT/OFFSET。
func (f *fakeAuditModel) List(_ context.Context, flt model.AuditFilter) ([]*model.AuditIndex, int64, error) {
	where, args := auditWhere(flt)
	f.log.add("audit_index.List:%s[%s]/%d/%d", where, strings.Join(args, ","), flt.Pn, flt.Ps)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*model.AuditIndex
	for _, row := range f.rows {
		if flt.AdminID > 0 && row.AdminID != flt.AdminID {
			continue
		}
		if flt.Action != "" && row.Action != flt.Action {
			continue
		}
		if flt.ResourceType != "" && row.ResourceType != flt.ResourceType {
			continue
		}
		if flt.ResourceID != "" && row.ResourceID != flt.ResourceID {
			continue
		}
		if flt.StartAt > 0 && row.Ctime < flt.StartAt {
			continue
		}
		if flt.EndAt > 0 && row.Ctime >= flt.EndAt {
			continue
		}
		cp := *row
		hits = append(hits, &cp)
	}
	total := int64(len(hits))
	if total == 0 {
		return nil, 0, nil // COUNT 为 0：第二条带 JOIN 的查询不发
	}
	slices.Reverse(hits) // ORDER BY a.id DESC
	start := int((flt.Pn - 1) * flt.Ps)
	if start >= len(hits) {
		return nil, total, nil
	}
	end := start + int(flt.Ps)
	if end > len(hits) {
		end = len(hits)
	}
	out := hits[start:end]
	for _, row := range out {
		row.Username = "" // IFNULL(u.username, '')：账号不在了就是空串
		if u := f.userRow(row.AdminID); u != nil {
			row.Username = u.Username
		}
	}
	return out, total, nil
}

func (f *fakeAuditModel) all() []*model.AuditIndex {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*model.AuditIndex, 0, len(f.rows))
	for _, row := range f.rows {
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// only 回读唯一一行（不唯一直接 fatal，避免用例静默读错行）。
func (f *fakeAuditModel) only(t *testing.T) *model.AuditIndex {
	t.Helper()
	rows := f.all()
	if len(rows) != 1 {
		t.Fatalf("期望 op_audit_index 只有 1 行，实际 %d 行：%v", len(rows), actionsOf(rows))
	}
	return rows[0]
}

// actionsOf 把审计行摘要成「action/result」列表，便于失败时读。
func actionsOf(rows []*model.AuditIndex) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Action+"/"+r.Result)
	}
	return out
}

// ============================================================================
// op_admin_task 表
// ============================================================================

// fakeAdminTaskModel 复刻 model/task.go 的 defaultAdminTaskModel。
// 两条必须复刻到位的口径：
//  1. Insert 是 `ON DUPLICATE KEY UPDATE mtime = mtime` + RowsAffected==0 → model.ErrTaskExists
//     （model/task.go:136-150），即幂等判定不依赖 driver 错误码；
//     uniq_request_id 与主键同走这一条，request_id 列是 utf8mb4_unicode_ci → 大小写不敏感。
//  2. TransitionState 带 `WHERE task_id = ? AND state = ?`（model/task.go:195-204），
//     条件不满足时影响 0 行、返回 false 且**不报错** —— 乐观并发的全部结论都压在这一列上，
//     因此这里额外提供 loseNextTransition(from) 静默开关来复刻「被另一个推进者抢先」。
type fakeAdminTaskModel struct {
	faultInjector
	log      *callLog
	mu       sync.Mutex
	rows     map[int64]*model.AdminTask
	order    []int64
	nextID   int64
	lostFrom map[string]int // from 状态 → 接下来 N 次 CAS 必须失败（并发窗口）
}

func newFakeAdminTaskModel(log *callLog) *fakeAdminTaskModel {
	return &fakeAdminTaskModel{log: log, rows: map[int64]*model.AdminTask{}, lostFrom: map[string]int{}}
}

func (f *fakeAdminTaskModel) Insert(_ context.Context, task *model.AdminTask) (int64, error) {
	f.log.add("admin_task.Insert:%s", task.RequestID)
	if err := f.fail("Insert"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := nowUnix()
	if task.TaskID != 0 {
		if _, ok := f.rows[task.TaskID]; ok {
			return 0, model.ErrTaskExists // ON DUPLICATE KEY UPDATE mtime=mtime → aff 0
		}
	}
	for _, id := range f.order {
		if strings.EqualFold(f.rows[id].RequestID, task.RequestID) {
			return 0, model.ErrTaskExists
		}
	}
	cp := *task
	if cp.Ctime == 0 {
		cp.Ctime = now
	}
	cp.Mtime = now
	if cp.State == "" {
		cp.State = model.TaskStatePending
	}
	f.nextID++
	cp.TaskID = f.nextID
	f.rows[cp.TaskID] = &cp
	f.order = append(f.order, cp.TaskID)
	task.TaskID, task.Ctime, task.Mtime, task.State = cp.TaskID, cp.Ctime, cp.Mtime, cp.State
	return cp.TaskID, nil
}

func (f *fakeAdminTaskModel) FindOne(_ context.Context, taskID int64) (*model.AdminTask, error) {
	f.log.add("admin_task.FindOne:%d", taskID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	return f.copyBy(func(r *model.AdminTask) bool { return r.TaskID == taskID }), nil
}

func (f *fakeAdminTaskModel) FindByRequestID(_ context.Context, requestID string) (*model.AdminTask, error) {
	f.log.add("admin_task.FindByRequestID:%s", requestID)
	if err := f.fail("FindByRequestID"); err != nil {
		return nil, err
	}
	// uniq_request_id 是 utf8mb4_unicode_ci：等值查询与唯一性判定同样大小写不敏感。
	return f.copyBy(func(r *model.AdminTask) bool {
		return strings.EqualFold(r.RequestID, requestID)
	}), nil
}

func (f *fakeAdminTaskModel) copyBy(pred func(*model.AdminTask) bool) *model.AdminTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if row := f.rows[id]; pred(row) {
			cp := *row
			return &cp
		}
	}
	return nil // model：sql.ErrNoRows → (nil, nil)
}

// TransitionState 只在库中 state == from 时写入；startedAt/finishedAt <= 0 时**不动该列**
// （model/task.go:183-205 的 SET 子句是条件拼接的，这是「取消的任务 finished_at 仍为 0」
// 这条缺陷的直接成因，见 README）。
func (f *fakeAdminTaskModel) TransitionState(_ context.Context, taskID int64, from, to string, startedAt, finishedAt int64) (bool, error) {
	// 时间戳实参不可复现，因此轨迹里只记「有没有下发」（*/0）：
	// 落库值本身由用例读回行后用 wantTSWindow 断言，两件事各自精确、序列仍可整段比对。
	f.log.add("admin_task.TransitionState:%d/%s->%s/%s/%s", taskID, from, to, tsFlag(startedAt), tsFlag(finishedAt))
	if err := f.fail("TransitionState"); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lostFrom[from] > 0 {
		f.lostFrom[from]-- // 并发窗口：另一个推进者已把 state 改掉
		return false, nil
	}
	row, ok := f.rows[taskID]
	if !ok || row.State != from {
		return false, nil
	}
	row.State = to
	row.Mtime = nowUnix()
	if startedAt > 0 {
		row.StartedAt = startedAt
	}
	if finishedAt > 0 {
		row.FinishedAt = finishedAt
	}
	return true, nil
}

func (f *fakeAdminTaskModel) AddCounters(_ context.Context, taskID int64, succeededDelta, failedDelta int32) error {
	f.log.add("admin_task.AddCounters:%d/%d/%d", taskID, succeededDelta, failedDelta)
	if err := f.fail("AddCounters"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[taskID]; ok { // SQL 侧自增，行不存在时影响 0 行、不报错
		row.Succeeded += succeededDelta
		row.Failed += failedDelta
		row.Progress += succeededDelta + failedDelta
		row.Mtime = nowUnix()
	}
	return nil
}

// List 复刻 model/task.go:214-248：state/taskType 空则不过滤、operator>0 才过滤、
// COUNT 为 0 时不发第二条查询、ORDER BY task_id DESC、LIMIT/OFFSET。
func (f *fakeAdminTaskModel) List(_ context.Context, state, taskType string, operator int64, pn, ps int32) ([]*model.AdminTask, int64, error) {
	f.log.add("admin_task.List:%s/%s/%d/%d/%d", state, taskType, operator, pn, ps)
	if err := f.fail("List"); err != nil {
		return nil, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*model.AdminTask
	for _, id := range f.order {
		row := f.rows[id]
		if state != "" && row.State != state {
			continue
		}
		if taskType != "" && row.TaskType != taskType {
			continue
		}
		if operator > 0 && row.Operator != operator {
			continue
		}
		cp := *row
		hits = append(hits, &cp)
	}
	total := int64(len(hits))
	if total == 0 {
		return nil, 0, nil
	}
	slices.Reverse(hits) // task_id DESC
	start := int((pn - 1) * ps)
	if start >= len(hits) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(hits) {
		end = len(hits)
	}
	return hits[start:end], total, nil
}

// --- 静默布景 ---

func (f *fakeAdminTaskModel) put(task *model.AdminTask) *model.AdminTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *task
	if cp.TaskID == 0 {
		cp.TaskID = f.nextID
	}
	if cp.TaskID > f.nextID {
		f.nextID = cp.TaskID
	}
	if cp.State == "" {
		cp.State = model.TaskStatePending
	}
	if _, exists := f.rows[cp.TaskID]; !exists {
		f.order = append(f.order, cp.TaskID)
		slices.Sort(f.order)
	}
	f.rows[cp.TaskID] = &cp
	return &cp
}

func (f *fakeAdminTaskModel) get(taskID int64) *model.AdminTask {
	return f.copyBy(func(r *model.AdminTask) bool { return r.TaskID == taskID })
}

// loseNextTransition 静默声明「接下来 N 次 from=<from> 的 CAS 会输给另一个推进者」，
// 用于测乐观并发分支（不写轨迹，等价于对端事务先提交了）。
func (f *fakeAdminTaskModel) loseNextTransition(from string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lostFrom[from] = n
}

// ============================================================================
// op_admin_task_step 表
// ============================================================================

// fakeAdminTaskStepModel 复刻 model/task.go 的 defaultAdminTaskStepModel。
// InsertBatch 是单事务（model/task.go:264-288）：任一步失败必须整批不落，
// 否则「任务已建但步骤半截」这种假状态会被替身悄悄放行。
// uniq_task_step(task_id, step_no) 是普通 1062（这里直接返回错误文本，调用方只断言报错）。
type fakeAdminTaskStepModel struct {
	faultInjector
	log       *callLog
	mu        sync.Mutex
	rows      map[int64]*model.AdminTaskStep
	order     []int64
	nextID    int64
	lostClaim map[int64]int // stepID → 接下来 N 次 pending→running 抢占会输
}

func newFakeAdminTaskStepModel(log *callLog) *fakeAdminTaskStepModel {
	return &fakeAdminTaskStepModel{log: log, rows: map[int64]*model.AdminTaskStep{}, lostClaim: map[int64]int{}}
}

func (f *fakeAdminTaskStepModel) InsertBatch(_ context.Context, steps []*model.AdminTaskStep) error {
	f.log.add("admin_task_step.InsertBatch:%d", len(steps))
	if err := f.fail("InsertBatch"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// 事务口径：先整体校验，再整体落库（任何一条撞唯一键都当作整批回滚）。
	seen := make(map[[2]int64]bool, len(steps))
	for _, s := range steps {
		key := [2]int64{s.TaskID, int64(s.StepNo)}
		if seen[key] {
			return dupErr("op_admin_task_step.uniq_task_step", fmt.Sprintf("%d-%d", s.TaskID, s.StepNo))
		}
		seen[key] = true
		for _, id := range f.order {
			row := f.rows[id]
			if row.TaskID == s.TaskID && row.StepNo == s.StepNo {
				return dupErr("op_admin_task_step.uniq_task_step", fmt.Sprintf("%d-%d", s.TaskID, s.StepNo))
			}
		}
	}
	now := nowUnix()
	for _, s := range steps {
		cp := *s
		if cp.Ctime == 0 {
			cp.Ctime = now
		}
		cp.Mtime = now
		if cp.State == "" {
			cp.State = model.StepStatePending
		}
		f.nextID++
		cp.ID = f.nextID
		f.rows[cp.ID] = &cp
		f.order = append(f.order, cp.ID)
		s.ID, s.Ctime, s.Mtime, s.State = cp.ID, cp.Ctime, cp.Mtime, cp.State
	}
	return nil
}

// snapshot 调用方持锁。
func (f *fakeAdminTaskStepModel) snapshotLocked(pred func(*model.AdminTaskStep) bool) []*model.AdminTaskStep {
	var out []*model.AdminTaskStep
	for _, id := range f.order { // id 即插入顺序 = step_no 顺序
		if row := f.rows[id]; pred(row) {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out
}

func (f *fakeAdminTaskStepModel) ListByTask(_ context.Context, taskID int64) ([]*model.AdminTaskStep, error) {
	f.log.add("admin_task_step.ListByTask:%d", taskID)
	if err := f.fail("ListByTask"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotLocked(func(r *model.AdminTaskStep) bool { return r.TaskID == taskID }), nil
}

func (f *fakeAdminTaskStepModel) ListExecutable(_ context.Context, taskID int64, limit int32) ([]*model.AdminTaskStep, error) {
	f.log.add("admin_task_step.ListExecutable:%d/%d", taskID, limit)
	if err := f.fail("ListExecutable"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100 // model/task.go:303-305 的兜底
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.snapshotLocked(func(r *model.AdminTaskStep) bool {
		return r.TaskID == taskID && r.State == model.StepStatePending
	})
	if int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkStep 只在库中 state == from 时写回结果。
func (f *fakeAdminTaskStepModel) MarkStep(_ context.Context, stepID int64, from, to, result, errMsg string) (bool, error) {
	f.log.add("admin_task_step.MarkStep:%d/%s->%s", stepID, from, to)
	if err := f.fail("MarkStep"); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if from == model.StepStatePending && f.lostClaim[stepID] > 0 {
		f.lostClaim[stepID]-- // 另一个推进者已抢走这一步
		return false, nil
	}
	row, ok := f.rows[stepID]
	if !ok || row.State != from {
		return false, nil
	}
	row.State = to
	row.Result = result
	row.ErrMsg = errMsg
	row.Mtime = nowUnix()
	return true, nil
}

func (f *fakeAdminTaskStepModel) CancelPending(_ context.Context, taskID int64) (int64, error) {
	f.log.add("admin_task_step.CancelPending:%d", taskID)
	if err := f.fail("CancelPending"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, id := range f.order {
		if row := f.rows[id]; row.TaskID == taskID && row.State == model.StepStatePending {
			row.State = model.StepStateCanceled
			row.Mtime = nowUnix()
			n++
		}
	}
	return n, nil
}

func (f *fakeAdminTaskStepModel) CountByTask(_ context.Context, taskID int64) (map[string]int32, error) {
	f.log.add("admin_task_step.CountByTask:%d", taskID)
	if err := f.fail("CountByTask"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int32{}
	for _, id := range f.order {
		if row := f.rows[id]; row.TaskID == taskID {
			out[row.State]++
		}
	}
	return out, nil
}

// --- 静默布景 / 回读 ---

func (f *fakeAdminTaskStepModel) put(s *model.AdminTaskStep) *model.AdminTaskStep {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *s
	if cp.ID == 0 {
		cp.ID = f.nextID
	}
	if cp.State == "" {
		cp.State = model.StepStatePending
	}
	f.rows[cp.ID] = &cp
	f.order = append(f.order, cp.ID)
	return &cp
}

// stepsOf 静默回读某任务的全部步骤（step_no 升序），不写轨迹。
func (f *fakeAdminTaskStepModel) stepsOf(taskID int64) []*model.AdminTaskStep {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.snapshotLocked(func(r *model.AdminTaskStep) bool { return r.TaskID == taskID })
	slices.SortFunc(out, func(a, b *model.AdminTaskStep) int { return int(a.StepNo) - int(b.StepNo) })
	return out
}

// loseNextClaim 静默声明「这一步接下来会被别的推进者抢走」（pending→running CAS 失败）。
func (f *fakeAdminTaskStepModel) loseNextClaim(stepNo int32, taskID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if row := f.rows[id]; row.TaskID == taskID && row.StepNo == stepNo {
			f.lostClaim[id]++
			return
		}
	}
}

// ============================================================================
// 下游 client 替身
// ============================================================================

// fakeAccountGateway 是 account 二次校验通道的替身。
// 默认行为：带 check 则调用它，否则返回 errDownstreamWired —— 也就是说
// 「没打算测二次校验的用例一旦走到二阶校验」会立刻显式失败，而不是静默通过。
type fakeAccountGateway struct {
	faultInjector
	log   *callLog
	check func(target, code string) error
}

func (f *fakeAccountGateway) CheckSecondFactor(_ context.Context, target, code string) error {
	f.log.add("account.CheckSecondFactor:%s/%s", target, code)
	if err := f.fail("CheckSecondFactor"); err != nil {
		return err
	}
	if f.check != nil {
		return f.check(target, code)
	}
	return errSecondFactorNotWired
}

// errSecondFactorNotWired 是「用例没配二阶校验替身却走到了这一步」的响铃。
var errSecondFactorNotWired = errors.New("operation/logic test: 二次校验替身未配置 check")

// ---------------------------------------------------------------------------
// 任务推进的四个下游动作替身（repository/downstream.go:25-47）
//
// 共同约定：
//   - 轨迹里带上实参，因此「调了哪个下游、调了几次、按什么顺序」都能被整段序列断言钉住；
//   - 默认成功，失败用 faultInjector 的 failWith/failOn 注入 —— 后者可以精确到
//     「第 n 个步骤失败」，从而证明 RunAdminTask 是「逐步骤独立、失败不中断整批」；
//   - 缺客户端（Downstream 字段为 nil）不是错误而是显式行为：requireXxx 会返回
//     model.ErrDownstreamUnavailable，步骤被标 failed 而不是静默成功，所以
//     「未部署」这条分支同样要用例覆盖（storeOpt withoutTaskDownstreams）。
// ---------------------------------------------------------------------------

// fakeVideoGateway 是 video 侧运营动作替身（稿件下架）。
type fakeVideoGateway struct {
	faultInjector
	log *callLog
}

func (f *fakeVideoGateway) OfflineSubmission(_ context.Context, aid int64, operator, reason, ip string) error {
	f.log.add("video.OfflineSubmission:%d/%s/%s/%s", aid, operator, reason, ip)
	return f.fail("OfflineSubmission")
}

// fakeCatalogGateway 是 catalog 侧运营动作替身（剧集下架）。
type fakeCatalogGateway struct {
	faultInjector
	log *callLog
}

func (f *fakeCatalogGateway) OfflineEpisode(_ context.Context, episodeID int64) error {
	f.log.add("catalog.OfflineEpisode:%d", episodeID)
	return f.fail("OfflineEpisode")
}

// fakeRightsGateway 是 rights 侧运营动作替身（版权窗口立即失效）。
type fakeRightsGateway struct {
	faultInjector
	log *callLog
}

func (f *fakeRightsGateway) ExpireWindow(_ context.Context, windowID int64, ip string) error {
	f.log.add("rights.ExpireWindow:%d/%s", windowID, ip)
	return f.fail("ExpireWindow")
}

// fakeModerationGateway 是 moderation-orchestrator 侧运营动作替身（维持驳回申诉）。
type fakeModerationGateway struct {
	faultInjector
	log *callLog
}

func (f *fakeModerationGateway) RejectAppeal(_ context.Context, appealID, handlerAdminID int64, reason, ip string) error {
	f.log.add("moderation.RejectAppeal:%d/%d/%s/%s", appealID, handlerAdminID, reason, ip)
	return f.fail("RejectAppeal")
}

// ============================================================================
// 装配
// ============================================================================

// defaultTokenTTL 后台会话有效期秒数（与 repository.SessionConf.ttl() 的缺省值一致，
// 但用例显式传值，避免依赖实现里的兜底常量）。
const defaultTokenTTL = 7200

type store struct {
	log     *callLog
	cache   *fakeStore
	conn    *fakeConn
	admin   *fakeAdminUserModel
	session *fakeSessionModel
	role    *fakeRoleModel
	perm    *fakePermissionModel
	menu    *fakeMenuModel
	config  *fakeOpsConfigModel
	task    *fakeAdminTaskModel
	step    *fakeAdminTaskStepModel
	audit   *fakeAuditModel
	account *fakeAccountGateway
	video   *fakeVideoGateway
	catalog *fakeCatalogGateway
	rights  *fakeRightsGateway
	moder   *fakeModerationGateway

	tokenSecret []byte
	tokenTTL    int64
	issuer      string
	maxFail     int32
	lockMinutes int64
	permTTL     int
	configTTL   int
	menuTTL     int
	runSteps    int32
	repo        *repository.Repository
}

// storeOpt 在 Repository 组装之前改装配参数（只能换非 model 依赖）。
type storeOpt func(*store)

// withoutTokenSecret 复刻「AdminSession.TokenSecretRef 指向的环境变量没注入」的部署缺陷。
func withoutTokenSecret() storeOpt { return func(st *store) { st.tokenSecret = nil } }

// withLogin 覆盖防爆破阈值（<=0 时 repository 会补自己的缺省值）。
func withLogin(maxFail int32, lockMinutes int64) storeOpt {
	return func(st *store) { st.maxFail, st.lockMinutes = maxFail, lockMinutes }
}

// withSecondFactorChecker 配置二次校验通道的判定口径。
func withSecondFactorChecker(check func(target, code string) error) storeOpt {
	return func(st *store) { st.account.check = check }
}

// withoutAccountDownstream 复刻「AccountRPC 未配置」：二阶校验必须 fail-closed。
func withoutAccountDownstream() storeOpt { return func(st *store) { st.account = nil } }

// withRunSteps 覆盖 RunAdminTask 单次推进的默认步数（<=0 时 repository 补 100，
// 见 repository/repository.go:152-157），用于测 maxSteps 的钳制口径。
func withRunSteps(n int32) storeOpt { return func(st *store) { st.runSteps = n } }

// withoutTaskDownstreams 复刻「video/catalog/rights/moderation 四个客户端都没配」：
// 步骤必须落 failed，而不是静默成功（repository/downstream.go:175-201）。
func withoutTaskDownstreams() storeOpt {
	return func(st *store) { st.video, st.catalog, st.rights, st.moder = nil, nil, nil, nil }
}

// withCacheTTLs 显式下发三类缓存的 TTL。应答里回传的 ttl 字段必须等于配置值，
// 不能靠 repository.normalizeOptions 的缺省常量（否则实现把 300 改成 30 也不会红）。
func withCacheTTLs(permTTL, configTTL, menuTTL int) storeOpt {
	return func(st *store) { st.permTTL, st.configTTL, st.menuTTL = permTTL, configTTL, menuTTL }
}

func newRawStore(opts ...storeOpt) *store {
	lg := &callLog{}
	st := &store{
		log:         lg,
		cache:       newFakeStore(lg),
		conn:        &fakeConn{},
		admin:       newFakeAdminUserModel(lg),
		session:     newFakeSessionModel(lg),
		role:        newFakeRoleModel(lg),
		perm:        newFakePermissionModel(lg),
		menu:        newFakeMenuModel(lg),
		config:      newFakeOpsConfigModel(lg),
		task:        newFakeAdminTaskModel(lg),
		step:        newFakeAdminTaskStepModel(lg),
		audit:       newFakeAuditModel(lg),
		account:     &fakeAccountGateway{log: lg},
		video:       &fakeVideoGateway{log: lg},
		catalog:     &fakeCatalogGateway{log: lg},
		rights:      &fakeRightsGateway{log: lg},
		moder:       &fakeModerationGateway{log: lg},
		tokenSecret: []byte("unit-test-admin-token-secret"),
		tokenTTL:    defaultTokenTTL,
		issuer:      "op",
		maxFail:     5,
		lockMinutes: 15,
		permTTL:     60,
	}
	for _, opt := range opts {
		opt(st)
	}
	// LoadAdminGrants 的权限点整行由 op_permission 提供（LEFT JOIN），替身用静默
	// 回读补齐 Resource/Action，装配期接线不算被测调用（纪律 4）。
	st.role.permRow = st.perm.rowOf
	// ListAuditIndex 的 username 由 `LEFT JOIN op_admin_user` 提供（不是审计行的列），
	// 同样走静默回读：账号被删时读成空串，与生产 SQL 的 IFNULL 同口径。
	st.audit.userRow = st.admin.get
	ds := &repository.Downstream{}
	if st.account != nil {
		ds.Account = st.account
	}
	if st.video != nil {
		ds.Video = st.video
	}
	if st.catalog != nil {
		ds.Catalog = st.catalog
	}
	if st.rights != nil {
		ds.Rights = st.rights
	}
	if st.moder != nil {
		ds.Moderation = st.moder
	}
	st.repo = repository.NewWithDeps(
		repository.NewCacheWithStore(st.cache), st.conn, ds,
		repository.Models{
			AdminUser:  st.admin,
			Session:    st.session,
			Role:       st.role,
			Permission: st.perm,
			Menu:       st.menu,
			Config:     st.config,
			Task:       st.task,
			Step:       st.step,
			Audit:      st.audit,
		},
		repository.SessionConf{TokenTTL: st.tokenTTL, Issuer: st.issuer, Secret: st.tokenSecret},
		repository.Options{
			PermissionTTL: st.permTTL, ConfigTTL: st.configTTL, MenuTTL: st.menuTTL,
			MaxFail: st.maxFail, LockMinutes: st.lockMinutes, RunSteps: st.runSteps,
		},
	)
	return st
}

// newStore 组装真实 Repository（依赖为替身）并清空装配期轨迹（纪律 4）。
func newStore(opts ...storeOpt) *store {
	st := newRawStore(opts...)
	st.log.reset()
	return st
}

// env 是一次用例的完整运行时：真实 Repository（依赖为替身）+ ServiceContext。
type env struct {
	t      *testing.T
	st     *store
	svcCtx *svc.ServiceContext
}

func newEnv(t *testing.T, opts ...storeOpt) *env {
	t.Helper()
	st := newStore(opts...)
	return &env{t: t, st: st, svcCtx: &svc.ServiceContext{Config: config.Config{}, Repository: st.repo}}
}

// ops 取 from 之后的调用序列（一般传 0，布景已静默）。
func (e *env) ops(from int) []string { return e.st.log.opsFrom(from) }

// nowUnix 用例开头取一次，用于时间窗断言（跨秒抖动由 ±5s 容差吸收）。
func nowUnix() int64 { return time.Now().Unix() }

// ============================================================================
// 静默布景（不写轨迹）
// ============================================================================

// testPassword 满足 repository.MinPasswordLength=10 的真实强度要求。
const testPassword = "Adm1n-Passw0rd-2026"

// seedAdmin 布一个「账号名 + PBKDF2 散列」齐备的管理员，返回 admin_id。
// 走 put 静默路径，因此不算被测调用。散列由生产函数生成（不降迭代数）。
func seedAdmin(t *testing.T, st *store, username, password string, mut func(*model.AdminUser)) int64 {
	t.Helper()
	hash, algo, err := repository.HashAdminPassword(password)
	wantNoErr(t, "测试侧口令散列生成", err)
	row := &model.AdminUser{
		Username:     username,
		PasswordHash: hash,
		PwdAlgo:      algo,
		State:        model.AdminStateNormal,
		Ctime:        nowUnix(),
		Mtime:        nowUnix(),
	}
	if mut != nil {
		mut(row)
	}
	return st.admin.put(row).AdminID
}

// seedMenu 静默布一个 op_menu 节点，返回 menu_id。
// required 传 "" 表示「登录即可见」，传 "resource#action" 表示需该权限点。
// 显式给定 menu_id（在 mut 里）可以布出「parent_id → sort」的排序场景。
func seedMenu(t *testing.T, st *store, parentID int64, name, required string, mut func(*model.Menu)) int64 {
	t.Helper()
	row := &model.Menu{
		ParentID:           parentID,
		Name:               name,
		RequiredPermission: required,
		State:              model.StateEnable,
		Ctime:              nowUnix(),
		Mtime:              nowUnix(),
	}
	if mut != nil {
		mut(row)
	}
	return st.menu.put(row).MenuID
}

// seedConfig 静默布一行 op_config，返回行 ID。
// version 显式给 1（与 model/config.go:76-78 的兜底一致）：乐观锁用例要能直接
// 拿它当 expect_version，而不是靠替身猜。
func seedConfig(t *testing.T, st *store, cfgKey, scope, value string, mut func(*model.OpsConfig)) int64 {
	t.Helper()
	row := &model.OpsConfig{
		CfgKey:    cfgKey,
		CfgValue:  value,
		ValueType: repository.ValueTypeString,
		Scope:     scope,
		Version:   1,
		State:     model.StateEnable,
		Ctime:     nowUnix(),
		Mtime:     nowUnix(),
	}
	if mut != nil {
		mut(row)
	}
	return st.config.put(row).ID
}

// seedAdminRow 静默布一个账号行，**不做 PBKDF2 散列**：用于本条用例根本不走登录
// 的场景（菜单、运营配置、审计列表都只读 admin_id / state / username 三列），
// 避免为布景白付 210000 次迭代的代价。需要真实登录链路的用例仍用 seedAdmin。
// 散列列留空串：若哪个被测路径真的去校验口令，它会直接失败而不是被假散列蒙过。
func seedAdminRow(t *testing.T, st *store, username string, mut func(*model.AdminUser)) int64 {
	t.Helper()
	row := &model.AdminUser{
		Username: username, State: model.AdminStateNormal, Ctime: nowUnix(), Mtime: nowUnix(),
	}
	if mut != nil {
		mut(row)
	}
	return st.admin.put(row).AdminID
}

// seedPerm 布一个权限点，返回 permission_id。
func seedPerm(t *testing.T, st *store, resource, action string) int64 {
	t.Helper()
	return st.perm.put(&model.Permission{
		Resource: resource, Action: action,
		Domain: strings.SplitN(resource, ":", 2)[0], Ctime: nowUnix(),
	}).PermissionID
}

// seedRole 布一个启用角色（state 由调用方覆盖），返回 role_id。
func seedRole(t *testing.T, st *store, name string, mut func(*model.Role)) int64 {
	t.Helper()
	row := &model.Role{Name: name, Title: name, State: model.StateEnable, Ctime: nowUnix(), Mtime: nowUnix()}
	if mut != nil {
		mut(row)
	}
	return st.role.putRole(row).RoleID
}

// seedGrant 布「权限点 → 角色 → 管理员」整条链，返回 role_id。
// resources 形如 "video:submission#read"，与 gateway/admin 的权限点写法一致。
func seedGrant(t *testing.T, st *store, adminID int64, roleName string, resources ...string) int64 {
	t.Helper()
	roleID := seedRole(t, st, roleName, nil)
	ids := make([]int64, 0, len(resources))
	for _, res := range resources {
		resource, action, ok := strings.Cut(res, "#")
		if !ok {
			t.Fatalf("布景权限点写法错误（缺 #）：%q", res)
		}
		pid, exists := st.perm.idOf(resource, action)
		if !exists {
			pid = seedPerm(t, st, resource, action)
		}
		ids = append(ids, pid)
	}
	st.role.grant(roleID, ids)
	st.role.putBindings(adminID, append(st.role.rolesOf(adminID), roleID))
	return roleID
}

// ---------------------------------------------------------------------------
// 任务布景
// ---------------------------------------------------------------------------

// stepTargetType 任务类型 → 步骤目标类型。这里**故意照抄一份** repository/task.go:28-33
// 的映射，而不是从生产代码取：两者一旦漂移（例如新增了任务类型却漏改 target_type），
// 布景出来的步骤就会被 SubmitAdminTask 的 target_type 校验拒掉，用例立刻红。
var stepTargetType = map[string]string{
	model.TaskTypeBatchOfflineSubmission: "submission",
	model.TaskTypeBatchOfflineEpisode:    "episode",
	model.TaskTypeBatchExpireWindow:      "rights_window",
	model.TaskTypeBatchRejectAppeal:      "moderation_appeal",
}

// targetTypeOf 取任务类型对应的目标类型；未知类型直接 fatal（布景写错不配静默通过）。
func targetTypeOf(t *testing.T, taskType string) string {
	t.Helper()
	tt, ok := stepTargetType[taskType]
	if !ok {
		t.Fatalf("布景用了未知任务类型 %q（与 repository/task.go:28-33 的映射漂移了）", taskType)
	}
	return tt
}

// seedTask 静默布一行 op_admin_task（不落步骤、不写审计），返回 task_id。
// 用于「推进/取消/查询一个已存在的任务」这类不需要重新提交的用例。
func seedTask(t *testing.T, st *store, taskType, requestID string, mut func(*model.AdminTask)) int64 {
	t.Helper()
	row := &model.AdminTask{
		TaskType:  taskType,
		Params:    "{}",
		State:     model.TaskStatePending,
		RequestID: requestID,
		Ctime:     nowUnix(),
		Mtime:     nowUnix(),
	}
	if mut != nil {
		mut(row)
	}
	return st.task.put(row).TaskID
}

// seedTaskAndSteps 静默布「任务 + N 条步骤」，步骤按 step_no=1..N 顺序 pending，
// 返回 task_id。targetIDs 是字符串目标 ID（与表列一致）。
func seedTaskAndSteps(t *testing.T, st *store, taskType, requestID string, targetIDs ...string) int64 {
	t.Helper()
	tt := targetTypeOf(t, taskType)
	id := seedTask(t, st, taskType, requestID, func(row *model.AdminTask) {
		row.Total = int32(len(targetIDs))
		row.Operator = seedOperatorID
	})
	for i, tid := range targetIDs {
		st.step.put(&model.AdminTaskStep{
			TaskID: id, StepNo: int32(i + 1), TargetType: tt, TargetID: tid,
			State: model.StepStatePending, Ctime: nowUnix(), Mtime: nowUnix(),
		})
	}
	return id
}

// seedOperatorID 是布景任务默认挂的提交人，与 opCtx(9001) 保持一致。
const seedOperatorID int64 = 9001

// ============================================================================
// 测试用视图与调用器
// ============================================================================

// opCtx 是合法的操作者上下文（operator_id>0）。写接口的 logic 层守卫只看它。
func opCtx(operator int64) *rpc.OpContext {
	return &rpc.OpContext{
		OperatorId:   operator,
		OperatorName: "operator_root",
		Ip:           "203.0.113.9",
		UserAgent:    "Mozilla/5.0 (Windows NT 10.0) AdminConsole/1.0",
		TraceId:      "trace-op-1",
		RequestId:    "req-op-1",
	}
}

func loginCall(t *testing.T, e *env, in *rpc.AdminLoginReq) (*rpc.AdminLoginReply, error) {
	t.Helper()
	return NewAdminLoginLogic(context.Background(), e.svcCtx).AdminLogin(in)
}

func verifyCall(t *testing.T, e *env, in *rpc.VerifyAdminPermissionReq) (*rpc.VerifyAdminPermissionReply, error) {
	t.Helper()
	return NewVerifyAdminPermissionLogic(context.Background(), e.svcCtx).VerifyAdminPermission(in)
}

func createAdminCall(t *testing.T, e *env, in *rpc.CreateAdminUserReq) (*rpc.CreateAdminUserReply, error) {
	t.Helper()
	return NewCreateAdminUserLogic(context.Background(), e.svcCtx).CreateAdminUser(in)
}

func updateAdminCall(t *testing.T, e *env, in *rpc.UpdateAdminUserReq) (*rpc.UpdateAdminUserReply, error) {
	t.Helper()
	return NewUpdateAdminUserLogic(context.Background(), e.svcCtx).UpdateAdminUser(in)
}

func disableAdminCall(t *testing.T, e *env, in *rpc.DisableAdminUserReq) (*rpc.DisableAdminUserReply, error) {
	t.Helper()
	return NewDisableAdminUserLogic(context.Background(), e.svcCtx).DisableAdminUser(in)
}

func listAdminCall(t *testing.T, e *env, in *rpc.ListAdminUsersReq) (*rpc.ListAdminUsersReply, error) {
	t.Helper()
	return NewListAdminUsersLogic(context.Background(), e.svcCtx).ListAdminUsers(in)
}

func createRoleCall(t *testing.T, e *env, in *rpc.CreateRoleReq) (*rpc.CreateRoleReply, error) {
	t.Helper()
	return NewCreateRoleLogic(context.Background(), e.svcCtx).CreateRole(in)
}

func assignRolesCall(t *testing.T, e *env, in *rpc.AssignRolesReq) (*rpc.AssignRolesReply, error) {
	t.Helper()
	return NewAssignRolesLogic(context.Background(), e.svcCtx).AssignRoles(in)
}

func submitTaskCall(t *testing.T, e *env, in *rpc.SubmitAdminTaskReq) (*rpc.SubmitAdminTaskReply, error) {
	t.Helper()
	return NewSubmitAdminTaskLogic(context.Background(), e.svcCtx).SubmitAdminTask(in)
}

func runTaskCall(t *testing.T, e *env, in *rpc.RunAdminTaskReq) (*rpc.RunAdminTaskReply, error) {
	t.Helper()
	return NewRunAdminTaskLogic(context.Background(), e.svcCtx).RunAdminTask(in)
}

func cancelTaskCall(t *testing.T, e *env, in *rpc.CancelAdminTaskReq) (*rpc.CancelAdminTaskReply, error) {
	t.Helper()
	return NewCancelAdminTaskLogic(context.Background(), e.svcCtx).CancelAdminTask(in)
}

func getTaskCall(t *testing.T, e *env, in *rpc.GetAdminTaskReq) (*rpc.GetAdminTaskReply, error) {
	t.Helper()
	return NewGetAdminTaskLogic(context.Background(), e.svcCtx).GetAdminTask(in)
}

func listTasksCall(t *testing.T, e *env, in *rpc.ListAdminTasksReq) (*rpc.ListAdminTasksReply, error) {
	t.Helper()
	return NewListAdminTasksLogic(context.Background(), e.svcCtx).ListAdminTasks(in)
}

func listRolesCall(t *testing.T, e *env, in *rpc.ListRolesReq) (*rpc.ListRolesReply, error) {
	t.Helper()
	return NewListRolesLogic(context.Background(), e.svcCtx).ListRoles(in)
}

func deleteRoleCall(t *testing.T, e *env, in *rpc.DeleteRoleReq) (*rpc.EmptyReply, error) {
	t.Helper()
	return NewDeleteRoleLogic(context.Background(), e.svcCtx).DeleteRole(in)
}

func createPermCall(t *testing.T, e *env, in *rpc.CreatePermissionReq) (*rpc.CreatePermissionReply, error) {
	t.Helper()
	return NewCreatePermissionLogic(context.Background(), e.svcCtx).CreatePermission(in)
}

func listPermsCall(t *testing.T, e *env, in *rpc.ListPermissionsReq) (*rpc.ListPermissionsReply, error) {
	t.Helper()
	return NewListPermissionsLogic(context.Background(), e.svcCtx).ListPermissions(in)
}

func saveMenuCall(t *testing.T, e *env, in *rpc.SaveMenuReq) (*rpc.SaveMenuReply, error) {
	t.Helper()
	return NewSaveMenuLogic(context.Background(), e.svcCtx).SaveMenu(in)
}

func getMenuCall(t *testing.T, e *env, in *rpc.GetMenuReq) (*rpc.GetMenuReply, error) {
	t.Helper()
	return NewGetMenuLogic(context.Background(), e.svcCtx).GetMenu(in)
}

func saveCfgCall(t *testing.T, e *env, in *rpc.SaveOpsConfigReq) (*rpc.SaveOpsConfigReply, error) {
	t.Helper()
	return NewSaveOpsConfigLogic(context.Background(), e.svcCtx).SaveOpsConfig(in)
}

func getCfgCall(t *testing.T, e *env, in *rpc.GetOpsConfigReq) (*rpc.GetOpsConfigReply, error) {
	t.Helper()
	return NewGetOpsConfigLogic(context.Background(), e.svcCtx).GetOpsConfig(in)
}

func listAuditCall(t *testing.T, e *env, in *rpc.ListAuditIndexReq) (*rpc.ListAuditIndexReply, error) {
	t.Helper()
	return NewListAuditIndexLogic(context.Background(), e.svcCtx).ListAuditIndex(in)
}

// loginAs 走真实 AdminLogin 拿一个后台会话（顺带把 op:sess: 缓存灌上），用于
// 「先登录、再校验权限/改配置」这类链式用例，避免手工布 token 时绕过 HMAC 校验。
// 作为布景步骤，它结束后会把轨迹清零（纪律 4：布数据必须静默）；
// 需要断言登录本身序列的用例请直接调 loginCall。
func loginAs(t *testing.T, e *env, username, password string) *rpc.AdminLoginReply {
	t.Helper()
	reply, err := loginCall(t, e, &rpc.AdminLoginReq{
		Username: username, Password: password,
		Ip: "203.0.113.9", UserAgent: "Mozilla/5.0 AdminConsole", TraceId: "trace-seed", RequestId: "req-seed",
	})
	wantNoErr(t, "登录布景 "+username, err)
	if reply == nil {
		t.Fatalf("登录布景返回 nil reply")
	}
	e.st.log.reset()
	return reply
}

// 编译期确认替身满足 model / repository 接口（签名漂移在编译期即暴露）。
var (
	_ model.AdminUserModel         = (*fakeAdminUserModel)(nil)
	_ model.AdminSessionModel      = (*fakeSessionModel)(nil)
	_ model.RoleModel              = (*fakeRoleModel)(nil)
	_ model.PermissionModel        = (*fakePermissionModel)(nil)
	_ model.MenuModel              = (*fakeMenuModel)(nil)
	_ model.OpsConfigModel         = (*fakeOpsConfigModel)(nil)
	_ model.AdminTaskModel         = (*fakeAdminTaskModel)(nil)
	_ model.AdminTaskStepModel     = (*fakeAdminTaskStepModel)(nil)
	_ model.AuditIndexModel        = (*fakeAuditModel)(nil)
	_ repository.AccountGateway    = (*fakeAccountGateway)(nil)
	_ repository.VideoGateway      = (*fakeVideoGateway)(nil)
	_ repository.CatalogGateway    = (*fakeCatalogGateway)(nil)
	_ repository.RightsGateway     = (*fakeRightsGateway)(nil)
	_ repository.ModerationGateway = (*fakeModerationGateway)(nil)
)
