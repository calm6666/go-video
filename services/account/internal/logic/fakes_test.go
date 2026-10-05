package logic

// fakes_test.go 是 account logic 测试的内存替身集合。
// 批次1（7 个会话/密码方法）：PasswordLogin / Register / Logout / TokenInfo /
// RenewToken / SetPassword / ResetPassword。
// 批次2（8 个资料聚合方法）：Info3 / Infos3 / InfosByName3 / Card3 / Profile3 /
// ProfileWithStat3 / Vip3 / Vips3 —— 为此给两个下游 client 替身加了**可选**的
// 「已接线布数据」层（withDownstream），并给 fakeCache 加了 proto 缓存的静默布景/
// 回读（warmMsg / warmRawMsg / msgBytesOf / msgAs）。**未布数据的 mid 行为与批次1
// 逐字相同**（仍返回 errDownstreamWired），因此批次1 的 7 个用例文件不受影响。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造 repository.New 会连真 Redis + 真 MySQL，还会拉起 5 秒一次的缓存延迟失效协程，
// 测试无处塞替身。因此本包用例统一用 repository.NewWithDeps(内存缓存, 内存 conn,
// 6 个内存 model, 会响铃的下游替身) 组装**真实的 Repository**，只把它的依赖换成替身——这样
// 「登录标识反查、密码哈希比对、会话签发与缓存回填、token 轮换、改密后吊销、验证码消费」
// 整条判定链都在被测路径上，而不是把 Repository 一起 mock 掉。
// 见 internal/repository/cache.go 的 Cacher 注释与 repository.go 的 NewWithDeps 注释。
//
// 四条替身纪律（user-profile / catalog / rights / playback 各轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：repository 里 `s.Token = newToken` 之类的写回不得污染库存行，
//     否则「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 写入按真实 SQL 的口径处理主键与列集：account_session / account_secret / account_login_log
//     的 Insert 会回填自增 ID（LastInsertId 语义），account_credential 的 Insert **不回填** ID
//     （真实 model 丢弃了 ExecCtx 结果）；credential 的 created_at/updated_at 直接取入参值
//     （注册路径传 0，替身也存 0，绝不"好心"补 now）；account / session / login_log 的
//     created_at/ctime 为 0 时由 model 层补 now（各自的 if 条件不同，逐条照抄）。
//     同时按 deploy/migrations/account/*.sql 的 UNIQUE KEY 复刻唯一约束：
//     uk_type_identifier(credential_type, identifier)、uk_mid_type_status(mid, secret_type, status)、
//     uk_token、uk_refresh、account.PRIMARY(mid)。缺了这些约束，「二次改密撞唯一键」这类
//     真实故障就会被替身悄悄吞掉。
//  3. 副作用按**顺序**记录（callLog），断言完整序列而不是只数次数：本服务要紧的结论是
//     「先读缓存还是先查库、签发会话时第几步回填了哪个 key、吊销有没有走到缓存」，
//     只数次数会漏掉次序错误。随机值（token/csrf/salt）由**响应体**拿回来再拼进期望序列，
//     因此序列仍是精确比对；只有时间戳一类不可复现的值走 wantCount / 前缀断言。
//  4. 布数据走替身的**静默写入路径**（seed* / put* → 直接塞 map，不写 callLog）：
//     布景不算被测调用，轨迹断言可以直接从 0 开始数。
//
// 事务口径（本服务的注入缝里最容易自欺的一处）：Register/SetPassword/ResetPassword 的
// 「事务」只覆盖拿到了 sqlx.Session 的写（account_secret），account 与 account_credential
// 的 model 接口**根本没有 tx 形参**（model/account.go:36、account_credential.go:50），
// 所以它们走 conn.ExecCtx、不随事务回滚。替身按同一口径实现：txSession 自带 undo 日志，
// 只有登记过的写会在回滚时撤销。用例 TestRegisterTransactionOnlyRollsBackSecretWrites
// 因此能证明「事务失败后账号主表与凭证行仍在库里」这条孤儿数据链路（见 README 已知缺口 1）。
//
// 批次3（关系/经验/验证码收尾 15 个方法）：AddExp3 / AddMoral3 / Relation3 / Relations3 /
// Attentions3 / Blacks3 / RichRelations3 / Cards3 / DelCache / CookieInfo / CaptureLogin /
// SendCapture / CheckCapture / CheckHistoryPassword / LoginLogs。为此在本文件加了三处
// **可选**扩展，未接线时的行为与批次1/2 逐字相同（仍返回 errDownstreamWired）：
//   - fakeUserProfileClient 的写侧接线（withDownstreamWrite）：AddExp/AddMoral 只有先「写成功」，
//     exp_moral.go 末尾那次缓存失效才会发生，否则整条失效路径根本不可达；
//   - fakeSocialGraphClient 的关系族布数据（Downstream.Relation/Attentions/Blacks/Rich）与
//     结构化入参记账（sgCall）：轨迹字符串把 nil 与 []int64{} 拼成同一个空串，
//     「参数是否原样传到下游」必须在结构化记账上断言；
//   - store 的 withoutSocialGraph()/withoutUserProfile()：把 Repository 的下游 client 换成
//     **未注入（nil 接口）**装配，用于复刻 internal/svc/servicecontext.go:37-40 的
//     「配置留空」分支——2026-10-03 接上适配器之后，social-graph 只有没配 Etcd/Target 时
//     才恒传 nil，关系族在该形态下仍回空默认值（repository/relation.go 的 nil 分支）。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（WHERE 条件、LIMIT 1 取最早 id、ORDER BY 方向、
//     唯一键），不证明 SQL 文本与列名本身；model/*.go 的列名/占位符仍无单测。
//   - 批次2 的补充口径：withDownstream 只替到 UserProfileClient / SocialGraphClient
//     这一层（即 repository 看到的接口边界），**不**替 gRPC 报文与适配器转换，
//     因此 userprofile_client.go 的 sexStr / Moral/100 / mid=0 防击穿哨兵仍不可离线覆盖；
//     替身按适配器口径把 (nil, nil) 表达为「该 mid 不在布数据 map 里」。
//   - fakeCache.GetInt 对读故障按真实实现吞成 miss（生产不返回错误），不注入错误分支。
//   - 缓存的 Info/Card/Profile/Vip 组合方法（i3_/c3_/p3_/v3_）自批次2 起进入被测路径
//     （命中/回源/回填/坏值吞成 miss 均有断言）；批次1 只按口径实现、无断言。
//     读写两侧都过一遍 proto.Marshal/Unmarshal 的**字节口径**（见 addMsg/getMsg）：
//     零值消息编码成空串，而生产把空串读成 miss——这条规则不复刻的话，
//     「Vip3 的回填其实是无效写」就会被替身的「按对象存」口径掩盖成假命中。
//   - 验证码明文（SendCapture 生成）与 gRPC 框架层（server/interceptor、真实 MySQL 事务隔离、
//     Redis TTL 到期）不在纯单测可达范围内；本包只测到 logic 出入口。
//   - 批次3 的补充口径：写侧接线只替到 UserProfileClient.AddExp/AddMoral 与
//     SocialGraphClient 关系族的**接口边界**，适配器内部把入参映射成 gRPC 报文的动作
//     （userprofile_client.go:195-232）仍不可离线覆盖；因此本批断言到「入参原样交到
//     repository 看见的那个接口」为止，operater/real_ip 是否在适配器里落地属于契约缺口，
//     只能在用例里以注释登记（见 addexp3logic_test.go）。

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"google.golang.org/protobuf/proto"

	"go-video/services/account/internal/config"
	"go-video/services/account/internal/repository"
	"go-video/services/account/internal/svc"
	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func TestMain(m *testing.M) {
	// 被测路径上的 logx.Errorf/Infof 只用于人工排查，不进断言；关闭以免污染测试输出。
	logx.Disable()
	m.Run()
}

// 领域错误别名：logic 层把 repository 的错误原样外传（这是被测行为之一），
// 用例用 errors.Is 比对哨兵；起别名只是为了让断言里不重复 repository. 前缀。
var (
	ErrLoginAccountNotExist = repository.ErrLoginAccountNotExist
	ErrLoginPasswordWrong   = repository.ErrLoginPasswordWrong
	ErrCaptureInvalid       = repository.ErrCaptureInvalid
	ErrCaptureWrong         = repository.ErrCaptureWrong
	ErrCaptureErrTooMany    = repository.ErrCaptureErrTooMany
	ErrCaptureSendTooMany   = repository.ErrCaptureSendTooMany
	ErrAccountExists        = repository.ErrAccountExists
	ErrSessionRevoked       = repository.ErrSessionRevoked
	ErrPasswordRequired     = repository.ErrPasswordRequired
)

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

// wantNoErr 断言没有错误（「不存在」在本服务是结论不是错误时必须走这条）。
func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
	}
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

// wantOpsSet 与 wantOps 同强度，但**不比较顺序**：只允许用在实现里遍历 map 的那几步
// （repository/account.go 的回填循环 `for mid, info := range raw` 走的是 Go map，
// 迭代顺序每次运行都不同，按顺序比会得到一条随机红的用例）。
// 仍然是精确比对：多一步、少一步、内容不同、重复都照样红。
func wantOpsSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	gs := append([]string(nil), got...)
	ws := append([]string(nil), want...)
	slices.Sort(gs)
	slices.Sort(ws)
	wantOps(t, label+"（忽略 map 迭代顺序）", gs, ws)
}

// wantOpsArgSet 在 wantOpsSet 之上再放宽**唯一**一维：单个调用内部逗号分隔的参数先后。
// 用在「参数列表本身来自 Go map 迭代」的那一步——例如 InfosByName3 把
// name→mid 的 map 摊成 mids 再批量回源（infosbyname3logic.go:38-41），
// `userProfile.Bases:70001,70002` 的先后每次运行都不同，wantOpsSet 仍会随机红。
// 放宽的只有同一调用内的参数顺序：步数、每步的调用名、参数**集合**照旧精确比对，
// 多一个 mid、少一个 mid、写成别的下游都照样红。
func wantOpsArgSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	wantOpsSet(t, label, sortOpArgs(got), sortOpArgs(want))
}

func sortOpArgs(ops []string) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		head, args, ok := strings.Cut(op, ":")
		if !ok {
			out[i] = op
			continue
		}
		parts := strings.Split(args, ",")
		if len(parts) < 2 {
			out[i] = op
			continue
		}
		slices.Sort(parts)
		out[i] = head + ":" + strings.Join(parts, ",")
	}
	return out
}

// wantProto 用 proto.Equal 比较两条 protobuf 消息（两侧都为 nil 视为相等）。
// 不能用 reflect.DeepEqual：pb 生成结构里有 state/sizeCache 等内部字段。
func wantProto[T proto.Message](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if !proto.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
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

// wantTSWindow 断言一个 Unix 秒落在 [from, to] 内（time.Now() 产生的值不能自比、
// 也不能只断言 >0：0 与 20 年前都必须红）。
func wantTSWindow(t *testing.T, label, field string, got, from, to int64) {
	t.Helper()
	if got < from || got > to {
		t.Errorf("%s：%s = %d, want 落在 [%d, %d]", label, field, got, from, to)
	}
}

// wantHexLen 断言随机串是指定长度的小写 hex（token/csrf/salt 的形态契约：
// 空串、短串、含非 hex 字符都必须红，因为下游网关按定长解析）。
func wantHexLen(t *testing.T, label, field, v string, n int) {
	t.Helper()
	if len(v) != n {
		t.Fatalf("%s：%s = %q, want 长度 %d", label, field, v, n)
	}
	for _, r := range v {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("%s：%s = %q, want 小写 hex（%q 不是）", label, field, v, r)
		}
	}
}

// wantNotContains 断言一段文本里没有 needle（密码/哈希不外泄这条硬规则的落地方式）。
func wantNotContains(t *testing.T, label, text, needle string) {
	t.Helper()
	if needle == "" {
		t.Fatalf("%s：needle 为空，断言无意义", label)
	}
	if strings.Contains(text, needle) {
		t.Errorf("%s：文本里出现了 %q —— %s", label, needle, text)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func countPrefix(ops []string, prefix string) int {
	n := 0
	for _, o := range ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

// joinInts 把 mid 列表拼成轨迹里的一个字段（顺序即回源顺序，可断言）。
func joinInts(mids []int64) string {
	ps := make([]string, 0, len(mids))
	for _, m := range mids {
		ps = append(ps, itoa(m))
	}
	return strings.Join(ps, ",")
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

func (c *callLog) len() int { return len(c.snapshot()) }

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
// 批量写链路必须有它：只有让「第二条」失败，才能证明实现是
// 「整批一个事务、失败即中止」而不是「逐条提交、失败跳过」。
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

// clearFaults 撤掉已注入的故障，用于「故障 → 依赖恢复 → 重试同一步」这类用例。
func (f *faultInjector) clearFaults() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by = nil
	f.nth = nil
}

// dupErr 复刻 MySQL 1062 唯一键冲突的报文形态（真实驱动会把 key 名与冲突值带回）。
func dupErr(key, value string) error {
	return fmt.Errorf("Error 1062: Duplicate entry '%s' for key '%s'", value, key)
}

// ============================================================================
// Redis 缓存替身（repository.Cacher）
// ============================================================================

type fakeCache struct {
	faultInjector
	log *callLog

	mu    sync.Mutex
	jsons map[string][]byte
	strs  map[string]string
	msgs  map[string][]byte // i3_/c3_/p3_/v3_ 的**原始字节**（等价于 Redis 的 string 值）
	ttls  map[string]int
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{log: log, jsons: map[string][]byte{}, strs: map[string]string{}, msgs: map[string][]byte{}, ttls: map[string]int{}}
}

func (f *fakeCache) Ping(context.Context) error {
	f.log.add("cache.Ping")
	return f.fail("Ping")
}

// msgKey 复刻 cache.go 的 key 派生（前缀不一致时读穿用例即红）。
func infoKey(mid int64) string    { return "i3_" + itoa(mid) }
func cardKey(mid int64) string    { return "c3_" + itoa(mid) }
func vipKey(mid int64) string     { return "v3_" + itoa(mid) }
func profileKey(mid int64) string { return "p3_" + itoa(mid) }

// 以下四个组合方法复刻 *Cache 的「故障与坏值一律吞成 miss」口径（cache.go:58-76 等），
// 因此 fail 注入也返回 (nil, nil)。本轮 7 个方法不经过它们，仅为接口完整性实现。
func (f *fakeCache) CacheInfo(_ context.Context, mid int64) (*rpc.Info, error) {
	return getMsg[*rpc.Info](f, "CacheInfo", infoKey(mid))
}
func (f *fakeCache) AddCacheInfo(_ context.Context, mid int64, v *rpc.Info) {
	addMsg(f, "AddCacheInfo", infoKey(mid), v, v == nil)
}
func (f *fakeCache) CacheCard(_ context.Context, mid int64) (*rpc.Card, error) {
	return getMsg[*rpc.Card](f, "CacheCard", cardKey(mid))
}
func (f *fakeCache) AddCacheCard(_ context.Context, mid int64, v *rpc.Card) {
	addMsg(f, "AddCacheCard", cardKey(mid), v, v == nil)
}
func (f *fakeCache) CacheProfile(_ context.Context, mid int64) (*rpc.Profile, error) {
	return getMsg[*rpc.Profile](f, "CacheProfile", profileKey(mid))
}
func (f *fakeCache) AddCacheProfile(_ context.Context, mid int64, v *rpc.Profile) {
	addMsg(f, "AddCacheProfile", profileKey(mid), v, v == nil)
}
func (f *fakeCache) CacheVip(_ context.Context, mid int64) (*rpc.VipInfo, error) {
	return getMsg[*rpc.VipInfo](f, "CacheVip", vipKey(mid))
}
func (f *fakeCache) AddCacheVip(_ context.Context, mid int64, v *rpc.VipInfo) {
	addMsg(f, "AddCacheVip", vipKey(mid), v, v == nil)
}

// decodeMsg 用**真实 protobuf 编解码**把缓存字节还原成 T，判定与生产的四段 Cache*
// 逐字同构（cache.go:113-131 是 Info 的形态，Card/Profile/Vip 一样）：
// 键不存在、值为空串（cache.go:122-124）、Unmarshal 失败（cache.go:126-129）三者都算 miss。
// 之所以存字节而不是存对象：只有走一遍 Marshal/Unmarshal，才能把
// 「零值消息编码成空串 → 读回来永远是 miss」这类真实后果留在被测路径上
// （见 vip3logic_test.go 的缺口 22）。
func decodeMsg[T proto.Message](bs []byte) (T, bool) {
	var zero T
	if len(bs) == 0 {
		return zero, false
	}
	v, ok := reflect.New(reflect.TypeOf(zero).Elem()).Interface().(T)
	if !ok {
		return zero, false
	}
	if err := proto.Unmarshal(bs, v); err != nil {
		return zero, false
	}
	return v, true
}

func getMsg[T proto.Message](f *fakeCache, method, key string) (T, error) {
	var zero T
	f.log.add("cache.%s:%s", method, key)
	if err := f.fail(method); err != nil {
		return zero, nil // 与生产一致：故障吞成 miss
	}
	f.mu.Lock()
	bs := f.msgs[key]
	f.mu.Unlock()
	got, _ := decodeMsg[T](bs)
	return got, nil
}

func addMsg(f *fakeCache, method, key string, m proto.Message, isNil bool) {
	if isNil {
		return // 生产在触 Redis 之前就 return，不产生任何调用
	}
	f.log.add("cache.%s:%s", method, key)
	if err := f.fail(method); err != nil {
		return
	}
	// 逐字复刻生产 AddCache* 的编码面（cache.go 里 Info/Card/Profile/Vip 四段同构）：
	// marshal 失败 → 连键都不写；零值消息 marshal 成**空字节串** → 键写了但值为空，
	// 而 Cache* 读到空串按 miss 处理。少了这一层，「会员查询的缓存形同虚设」
	// 这类事实就会被替身的「存对象」口径抹掉（见 vip3logic_test.go）。
	bs, err := proto.Marshal(m)
	if err != nil {
		return // 与生产同构：marshal 失败连键都不写（cache.go:138-141）
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[key] = bs
	f.ttls[key] = 3600 // cacheExpireSeconds
}

// DelCache 删除某 mid 的 4 个缓存键；按生产口径逐键删除并收集错误。
func (f *fakeCache) DelCache(_ context.Context, mid int64) []error {
	keys := []string{infoKey(mid), cardKey(mid), vipKey(mid), profileKey(mid)}
	f.log.add("cache.DelCache:%s", strings.Join(keys, ","))
	if err := f.fail("DelCache"); err != nil {
		errs := make([]error, 0, len(keys))
		for range keys {
			errs = append(errs, err)
		}
		return errs
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		delete(f.jsons, k)
		delete(f.strs, k)
		delete(f.msgs, k)
	}
	return nil
}

// GetJSON 读 JSON；miss 返回 redis.Nil（**不是** nil err），与 cache.go:GetJSON 一致。
func (f *fakeCache) GetJSON(_ context.Context, key string, v any) error {
	f.log.add("cache.GetJSON:%s", key)
	if err := f.fail("GetJSON"); err != nil {
		return err
	}
	f.mu.Lock()
	bs, ok := f.jsons[key]
	f.mu.Unlock()
	if !ok {
		return redis.Nil
	}
	return json.Unmarshal(bs, v)
}

// SetJSON 写 JSON；故障只记日志、不外抛（生产签名就没有 error）。
func (f *fakeCache) SetJSON(_ context.Context, key string, v any, ttlSeconds int) {
	f.log.add("cache.SetJSON:%s/%d", key, ttlSeconds)
	if err := f.fail("SetJSON"); err != nil {
		return
	}
	bs, err := json.Marshal(v)
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jsons[key] = bs
	f.ttls[key] = ttlSeconds
}

// GetInt 读整数；miss、非整数、读故障三者都返回 (0, false)（生产即如此）。
func (f *fakeCache) GetInt(_ context.Context, key string) (int64, bool) {
	f.log.add("cache.GetInt:%s", key)
	if err := f.fail("GetInt"); err != nil {
		return 0, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.strs[key]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// SetInt 写整数，错误如实上抛（SendCapture 会因此中止）。
func (f *fakeCache) SetInt(_ context.Context, key string, v int64, ttlSeconds int) error {
	f.log.add("cache.SetInt:%s/%d", key, ttlSeconds)
	if err := f.fail("SetInt"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.strs[key] = strconv.FormatInt(v, 10)
	f.ttls[key] = ttlSeconds
	return nil
}

// Del 删除 key；miss 不算错误（cache.go:Del 的口径）。
func (f *fakeCache) Del(_ context.Context, key string) error {
	f.log.add("cache.Del:%s", key)
	if err := f.fail("Del"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jsons, key)
	delete(f.strs, key)
	delete(f.msgs, key)
	return nil
}

// Incr 计数 +1 并返回新值。
func (f *fakeCache) Incr(_ context.Context, key string) (int64, error) {
	f.log.add("cache.Incr:%s", key)
	if err := f.fail("Incr"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, _ := strconv.ParseInt(f.strs[key], 10, 64)
	v++
	f.strs[key] = strconv.FormatInt(v, 10)
	return v, nil
}

// Expire 重设 TTL。
func (f *fakeCache) Expire(_ context.Context, key string, seconds int) error {
	f.log.add("cache.Expire:%s/%d", key, seconds)
	if err := f.fail("Expire"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ttls[key] = seconds
	return nil
}

// --- 静默布景 / 回读（纪律 4：不写轨迹） ---

func (f *fakeCache) warmJSON(key string, v any) {
	bs, err := json.Marshal(v)
	if err != nil {
		panic("布景序列化失败：" + err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jsons[key] = bs
}

func (f *fakeCache) warmInt(key string, v int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.strs[key] = strconv.FormatInt(v, 10)
}

// warmRaw 静默塞一段**非法 JSON**，用来复刻线上偶发的「缓存里是坏值」：
// 真实 GetJSON 会在 json.Unmarshal 上返回错误，Repository 按「未命中 → 回源 DB」处理。
func (f *fakeCache) warmRaw(key string, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jsons[key] = []byte(raw)
}

// warmRawStr 往**整数槽位**（strs）静默塞一段非数字原文。
// 为什么必须有它（批次3 加的注入缝）：验证码/计数 key 走的是 GetInt，
// 生产的 GetInt 对「键上是一坨非数字」按 ParseInt 失败 → (0,false) 处理（cache.go:GetInt），
// 而 warmRaw 只能污染 jsons 槽、warmInt 只能写合法整数，
// 于是「整数 key 上是坏值」这条分支在没有本缝时根本不可达。
func (f *fakeCache) warmRawStr(key string, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.strs[key] = raw
}

// warmMsg 静默塞一条 protobuf 缓存值（i3_/c3_/p3_/v3_ 命中分支的布景入口）。
// TTL 由调用方显式给出而不是写死 3600：布一个**不等于** cacheExpireSeconds 的值，
// 才能证明「回填把过期时间重新拉满」而不是布景值一直待在那儿。
// 允许塞与 key 不匹配的消息类型（例如往 v3_ 键下放 *rpc.Info），用来复刻「契约漂移/
// 跨类型值」：生产的 Cache* 只把字节 proto.Unmarshal 进目标消息，解不出来才按 miss
// 处理（cache.go:126-129）；字段号与 wire type 对得上的跨类型字节会被**当成合法命中**
// （README 缺口 23，替身用 decodeMsg 走同一套编解码，不做类型标签的近似）。
func (f *fakeCache) warmMsg(key string, m proto.Message, ttlSeconds int) {
	bs, err := proto.Marshal(m)
	if err != nil {
		return // 布景编不出来就什么都不写，让用例的「读不到」断言自己炸出来
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[key] = bs // 零值消息 → 空串：键在、值空，读回来是 miss
	f.ttls[key] = ttlSeconds
}

// warmRawMsg 静默塞一段**不经编码器**的原始字节（复刻「键上是一坨坏值」）：
// 生产读到它时 proto.Unmarshal 会失败并吞成 miss（cache.go:126-129）。
func (f *fakeCache) warmRawMsg(key string, bs []byte, ttlSeconds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[key] = bs
	f.ttls[key] = ttlSeconds
}

// msgBytesOf 静默回读某个 proto 键的**原始字节**；键不存在返回 (nil, false)。
// 长度为 0 且 ok 为真，表示「键在、值是空串」——生产读路径把它算作 miss。
func (f *fakeCache) msgBytesOf(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bs, ok := f.msgs[key]
	return bs, ok
}

// msgAs 回读某条 proto 缓存值并按生产的编解码口径判定它能否解成 T；
// 键不存在、值为空串、字节解不出来三种情况一律返回 false。
// 注意「跨类型但 wire 兼容」的字节会照样解成 true（生产也照样当真值命中，README 缺口 23）。
func msgAs[T proto.Message](f *fakeCache, key string) (T, bool) {
	f.mu.Lock()
	bs, ok := f.msgs[key]
	f.mu.Unlock()
	var zero T
	if !ok {
		return zero, false
	}
	return decodeMsg[T](bs)
}

func (f *fakeCache) intOf(key string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := strconv.ParseInt(f.strs[key], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// jsonOf 回读缓存里的 JSON；key 不存在返回 false。
func (f *fakeCache) jsonOf(key string, v any) bool {
	f.mu.Lock()
	bs, ok := f.jsons[key]
	f.mu.Unlock()
	if !ok {
		return false
	}
	return json.Unmarshal(bs, v) == nil
}

// forget 静默让某个 key 变成「未命中」（不写轨迹）：用于把缓存退回 DB 口径，
// 从而单独驱动「回源 → 回填」这条分支。
func (f *fakeCache) forget(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jsons, key)
	delete(f.strs, key)
	delete(f.msgs, key)
	delete(f.ttls, key)
}

func (f *fakeCache) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, a := f.jsons[key]
	_, b := f.strs[key]
	_, c := f.msgs[key]
	return a || b || c
}

// ttlOf 回读某个 key 当前生效的 TTL 秒数（签发/续期用例断言缓存存活窗口）。
func (f *fakeCache) ttlOf(key string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.ttls[key]
	return v, ok
}

// keys 回读当前全部 key（排序后返回，便于「缓存里只剩这些」式断言）。
func (f *fakeCache) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.jsons)+len(f.strs)+len(f.msgs)+len(f.ttls))
	seen := map[string]bool{}
	for k := range f.jsons {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range f.strs {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range f.msgs {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// allText 把整个缓存内容拼成一段文本，用于「密码/哈希绝不进缓存」的兜底断言。
func (f *fakeCache) allText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	for k, v := range f.jsons {
		fmt.Fprintf(&sb, "%s=%s ", k, v)
	}
	for k, v := range f.strs {
		fmt.Fprintf(&sb, "%s=%s ", k, v)
	}
	for k, v := range f.msgs {
		// %s 而不是 %v：proto 值是**字节**，protobuf 的 string 字段按原文躺在里面，
		// 打成数字数组会让「缓存里不许出现昵称/密码」这类兜底断言变成永远成立。
		fmt.Fprintf(&sb, "%s=%s ", k, v)
	}
	return sb.String()
}

// ============================================================================
// 事务连接替身
// ============================================================================

// txSession 是「事务会话」的凭证，自带 undo 日志：
// 只有把撤销动作登记进来的写（即真的拿到了 tx 的 model 方法）才受回滚影响，
// 与 InnoDB 的行级回滚同口径。
//
// 嵌入 nil sqlx.Session 而不是手写 10 个方法：本服务的 model 只通过
// AccountSecretModel.Insert/MarkHistory 的 tx 形参触到会话，且替身按语义直接改内存，
// 不会真的下发 Exec/QueryRow。一旦有人把 SQL 直下发到 tx 上，nil 嵌入会立刻 panic
// ——「logic 单测路径上不允许出现真实 SQL」这条边界与 fakeConn 同口径地炸出来。
type txSession struct {
	sqlx.Session
	mu   sync.Mutex
	undo []func()
}

func (s *txSession) record(u func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.undo = append(s.undo, u)
}

func (s *txSession) rollback() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.undo) - 1; i >= 0; i-- {
		s.undo[i]()
	}
	s.undo = nil
}

// fakeConn 只实现本服务用到的 TransactCtx。其它方法落在嵌入的 nil 接口上会直接 panic，
// 等价于「logic 单测路径上不允许出现任何直连 SQL」——真出现了就是越界，让它炸出来。
type fakeConn struct {
	sqlx.SqlConn
	mu           sync.Mutex
	transactions int
	rolledBack   int
}

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	f.mu.Lock()
	f.transactions++
	f.mu.Unlock()

	sess := &txSession{}
	err := fn(ctx, sess)
	if err != nil {
		f.mu.Lock()
		f.rolledBack++
		f.mu.Unlock()
		sess.rollback()
		return err
	}
	return nil
}

// txStats 回读事务计数（开启数, 回滚数）。
func (f *fakeConn) txStats() (opened, rolledBack int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transactions, f.rolledBack
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// ============================================================================
// account 表
// ============================================================================

type fakeAccountModel struct {
	faultInjector
	log  *callLog
	mu   sync.Mutex
	rows map[int64]*model.Account
}

func newFakeAccountModel(log *callLog) *fakeAccountModel {
	return &fakeAccountModel{log: log, rows: map[int64]*model.Account{}}
}

func (f *fakeAccountModel) FindOne(_ context.Context, mid int64) (*model.Account, error) {
	f.log.add("account.FindOne:%d", mid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[mid]
	if !ok {
		return nil, nil // model：sql.ErrNoRows → (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeAccountModel) FindMany(_ context.Context, mids []int64) (map[int64]*model.Account, error) {
	f.log.add("account.FindMany:%s", joinInts(mids))
	if err := f.fail("FindMany"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64]*model.Account, len(mids))
	for _, mid := range mids {
		row, ok := f.rows[mid]
		if !ok {
			continue // model：缺失的 mid 不进结果
		}
		cp := *row
		out[mid] = &cp
	}
	return out, nil
}

// Insert 复刻 model/account.go:78-87：created_at 为 0 时补 now、updated_at 一律刷新，
// 并且**会写回入参指针**；主键是 mid，重复插入撞 PRIMARY 而不是「覆盖」。
func (f *fakeAccountModel) Insert(_ context.Context, data *model.Account) error {
	f.log.add("account.Insert:%d", data.Mid)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[data.Mid]; ok {
		return dupErr("account.PRIMARY", itoa(data.Mid))
	}
	cp := *data
	f.fixTimes(&cp)
	f.rows[data.Mid] = &cp
	return nil
}

func (f *fakeAccountModel) UpdateStatus(_ context.Context, mid int64, status int32) error {
	f.log.add("account.UpdateStatus:%d/%d", mid, status)
	if err := f.fail("UpdateStatus"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[mid]; ok { // UPDATE 未命中不报错
		row.Status = status
		row.UpdatedAt = nowUnix()
	}
	return nil
}

// fixTimes 与真实 model 同样的 if 条件（先改入参再落库，故用值拷贝复刻）。
func (f *fakeAccountModel) fixTimes(data *model.Account) {
	now := nowUnix()
	if data.CreatedAt == 0 {
		data.CreatedAt = now
	}
	data.UpdatedAt = now
}

func (f *fakeAccountModel) get(mid int64) *model.Account {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[mid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeAccountModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// put 静默布一行账号主表（不写轨迹）。
func (f *fakeAccountModel) put(a *model.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *a
	f.rows[a.Mid] = &cp
}

func (f *fakeAccountModel) mids() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, 0, len(f.rows))
	for mid := range f.rows {
		out = append(out, mid)
	}
	slices.Sort(out)
	return out
}

// ============================================================================
// account_credential 表
// ============================================================================

type fakeCredentialModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   []*model.AccountCredential
	nextID int64
}

func newFakeCredentialModel(log *callLog) *fakeCredentialModel {
	return &fakeCredentialModel{log: log}
}

// FindMidsByIdentifiers 复刻 account_credential.go:87-115：
// WHERE credential_type = ? AND LOWER(identifier) IN (?) —— **不带 status 过滤**
// （DDL 注释要求过滤 status=0，SQL 里没有；见 README 已知缺口 2）。
func (f *fakeCredentialModel) FindMidsByIdentifiers(_ context.Context, credType int8, identifiers []string) (map[string]int64, error) {
	f.log.add("cred.FindMidsByIdentifiers:%d/%s", credType, strings.Join(identifiers, ","))
	if err := f.fail("FindMidsByIdentifiers"); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, want := range identifiers {
		for _, row := range f.rows {
			if row.CredentialType == credType && strings.EqualFold(row.Identifier, want) {
				out[strings.ToLower(row.Identifier)] = row.Mid
			}
		}
	}
	return out, nil
}

func (f *fakeCredentialModel) FindByMid(_ context.Context, mid int64) ([]*model.AccountCredential, error) {
	f.log.add("cred.FindByMid:%d", mid)
	if err := f.fail("FindByMid"); err != nil {
		return nil, err
	}
	return f.copies(func(r *model.AccountCredential) bool { return r.Mid == mid }), nil
}

func (f *fakeCredentialModel) FindByMids(_ context.Context, mids []int64) (map[int64][]*model.AccountCredential, error) {
	f.log.add("cred.FindByMids:%s", joinInts(mids))
	if err := f.fail("FindByMids"); err != nil {
		return nil, err
	}
	out := map[int64][]*model.AccountCredential{}
	for _, mid := range mids {
		if rows := f.copies(func(r *model.AccountCredential) bool { return r.Mid == mid }); len(rows) > 0 {
			out[mid] = rows
		}
	}
	return out, nil
}

func (f *fakeCredentialModel) copies(pred func(*model.AccountCredential) bool) []*model.AccountCredential {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.AccountCredential
	for _, row := range f.rows {
		if pred(row) {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out
}

// Insert 复刻 account_credential.go:117-121：六列全取入参值（created_at/updated_at
// 传 0 就存 0，**不补 now**），且**不回填自增 ID**（真实 model 丢弃了 ExecCtx 结果）。
// 唯一键 uk_type_identifier 与 DDL 一致（utf8mb4_unicode_ci 排序 → 大小写不敏感）。
func (f *fakeCredentialModel) Insert(_ context.Context, data *model.AccountCredential) error {
	f.log.add("cred.Insert:%d/%d/%s", data.Mid, data.CredentialType, data.Identifier)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.CredentialType == data.CredentialType && strings.EqualFold(row.Identifier, data.Identifier) {
			return dupErr("account_credential.uk_type_identifier", data.Identifier)
		}
	}
	f.nextID++
	cp := *data
	cp.ID = f.nextID // 行有自增 id，但不写回调用方指针
	f.rows = append(f.rows, &cp)
	return nil
}

// put 静默布一条凭证（不写轨迹）。
func (f *fakeCredentialModel) put(c *model.AccountCredential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *c
	cp.ID = f.nextID
	f.rows = append(f.rows, &cp)
}

// setStatus 静默改某条凭证的 status（复刻「解绑」这个运维动作，不写轨迹）。
func (f *fakeCredentialModel) setStatus(mid int64, credType int8, status int8) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Mid == mid && row.CredentialType == credType {
			row.Status = status
		}
	}
}

func (f *fakeCredentialModel) rowsFor(mid int64) []*model.AccountCredential {
	return f.copies(func(r *model.AccountCredential) bool { return r.Mid == mid })
}

func (f *fakeCredentialModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// ============================================================================
// account_secret 表（唯一受事务会话保护的 model）
// ============================================================================

type fakeSecretModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   map[int64]*model.AccountSecret
	order  []int64
	nextID int64
	// txSeen 记每次带 tx 形参的调用是否真的拿到了事务会话。
	txSeen map[string]int
	noTx   map[string]int
}

func newFakeSecretModel(log *callLog) *fakeSecretModel {
	return &fakeSecretModel{log: log, rows: map[int64]*model.AccountSecret{}, txSeen: map[string]int{}, noTx: map[string]int{}}
}

// peekID 加锁读自增序列当前值，只为轨迹里打印「本条将拿到的主键」，
// 不与并发用例（如重复登录竞态）抢同一把锁的非原子读写。
func (f *fakeSecretModel) peekID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextID
}

func (f *fakeSecretModel) FindActive(_ context.Context, mid int64, secretType int8) (*model.AccountSecret, error) {
	f.log.add("secret.FindActive:%d/%d", mid, secretType)
	if err := f.fail("FindActive"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order { // 与 SQL 一致：无 ORDER BY，取第一条命中（替身按 id 升序）
		row := f.rows[id]
		if row.Mid == mid && row.SecretType == secretType && row.Status == model.SecretStatusActive {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeSecretModel) FindAll(_ context.Context, mid int64, secretType int8) ([]*model.AccountSecret, error) {
	f.log.add("secret.FindAll:%d/%d", mid, secretType)
	if err := f.fail("FindAll"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*model.AccountSecret, 0, len(f.order))
	for i := len(f.order) - 1; i >= 0; i-- { // ORDER BY id DESC
		row := f.rows[f.order[i]]
		if row.Mid == mid && row.SecretType == secretType {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeSecretModel) markTx(method string, tx sqlx.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tx != nil {
		f.txSeen[method]++
	} else {
		f.noTx[method]++
	}
}

// Insert 复刻 secret.go:92-100：走 tx.ExecCtx、回填 LastInsertId 到入参指针，
// 并受 uk_mid_type_status 约束（secret.go 所在表的 DDL 见 000011）。
func (f *fakeSecretModel) Insert(_ context.Context, tx sqlx.Session, secret *model.AccountSecret) error {
	f.log.add("secret.Insert:%d/%d/%d", f.peekID()+1, secret.Mid, secret.Status)
	f.markTx("Insert", tx)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		row := f.rows[id]
		if row.Mid == secret.Mid && row.SecretType == secret.SecretType && row.Status == secret.Status {
			return dupErr("account_secret.uk_mid_type_status",
				fmt.Sprintf("%d-%d-%d", secret.Mid, secret.SecretType, secret.Status))
		}
	}
	f.nextID++
	cp := *secret
	cp.ID = f.nextID
	f.rows[cp.ID] = &cp
	f.order = append(f.order, cp.ID)
	secret.ID = cp.ID // 真实 model：secret.ID, err = res.LastInsertId()
	if ts, ok := tx.(*txSession); ok {
		id := cp.ID
		ts.record(f.undoInsert(id))
	}
	return nil
}

func (f *fakeSecretModel) undoInsert(id int64) func() {
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.rows, id)
		for i, x := range f.order {
			if x == id {
				f.order = append(f.order[:i], f.order[i+1:]...)
				break
			}
		}
	}
}

// MarkHistory 复刻 secret.go:102-106：UPDATE ... SET status = 1 WHERE mid = ? AND
// secret_type = ? AND status = 0。因为 DDL 把 status 放进了唯一键
// （uk_mid_type_status），**已有历史行时这条 UPDATE 本身就会撞 1062**——
// 这正是「第二次改密必然失败」的成因（README 已知缺口 3）。
func (f *fakeSecretModel) MarkHistory(_ context.Context, tx sqlx.Session, mid int64, secretType int8) error {
	f.log.add("secret.MarkHistory:%d/%d", mid, secretType)
	f.markTx("MarkHistory", tx)
	if err := f.fail("MarkHistory"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*model.AccountSecret
	hasHistory := false
	for _, id := range f.order {
		row := f.rows[id]
		if row.Mid != mid || row.SecretType != secretType {
			continue
		}
		if row.Status == model.SecretStatusActive {
			hits = append(hits, row)
		}
		if row.Status == model.SecretStatusHistory {
			hasHistory = true
		}
	}
	if len(hits) == 0 {
		return nil // UPDATE 未命中：0 行受影响，不算错误
	}
	if hasHistory {
		return dupErr("account_secret.uk_mid_type_status", fmt.Sprintf("%d-%d-1", mid, secretType))
	}
	for _, row := range hits {
		id, before := row.ID, *row
		if ts, ok := tx.(*txSession); ok {
			ts.record(f.undoRestoreSecret(id, before))
		}
		row.Status = model.SecretStatusHistory
		row.MTime = nowUnix()
	}
	return nil
}

func (f *fakeSecretModel) undoRestoreSecret(id int64, before model.AccountSecret) func() {
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		cp := before
		f.rows[id] = &cp
	}
}

func (f *fakeSecretModel) put(s *model.AccountSecret) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *s
	cp.ID = f.nextID
	f.rows[cp.ID] = &cp
	f.order = append(f.order, cp.ID)
}

// txCount 回读「带事务会话调用了几次」（加锁，供并发用例安全断言）。
func (f *fakeSecretModel) txCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.txSeen[method]
}

// rowsOf 回读某 mid 的全部密钥行，按 id 升序（值拷贝）。
func (f *fakeSecretModel) rowsOf(mid int64) []*model.AccountSecret {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.AccountSecret
	for _, id := range f.order {
		row := f.rows[id]
		if row.Mid == mid {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out
}

func (f *fakeSecretModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}

// ============================================================================
// account_session 表
// ============================================================================

type fakeSessionModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   map[int64]*model.AccountSession
	order  []int64
	nextID int64
}

func newFakeSessionModel(log *callLog) *fakeSessionModel {
	return &fakeSessionModel{log: log, rows: map[int64]*model.AccountSession{}}
}

func (f *fakeSessionModel) peekID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextID
}

func (f *fakeSessionModel) FindByToken(_ context.Context, token string) (*model.AccountSession, error) {
	f.log.add("session.FindByToken:%s", token)
	if err := f.fail("FindByToken"); err != nil {
		return nil, err
	}
	return f.firstBy(func(r *model.AccountSession) bool { return r.Token == token }), nil
}

func (f *fakeSessionModel) FindByRefresh(_ context.Context, refreshToken string) (*model.AccountSession, error) {
	f.log.add("session.FindByRefresh:%s", refreshToken)
	if err := f.fail("FindByRefresh"); err != nil {
		return nil, err
	}
	return f.firstBy(func(r *model.AccountSession) bool { return r.RefreshToken == refreshToken }), nil
}

func (f *fakeSessionModel) firstBy(pred func(*model.AccountSession) bool) *model.AccountSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order { // LIMIT 1：按主键升序取第一条
		row := f.rows[id]
		if pred(row) {
			cp := *row
			return &cp
		}
	}
	return nil
}

// Insert 复刻 session.go:103-120：ctime 为 0 补 now、mtime 一律刷新、回填自增 ID，
// 受 uk_token / uk_refresh 约束。
func (f *fakeSessionModel) Insert(_ context.Context, session *model.AccountSession) error {
	f.log.add("session.Insert:%d/%d", f.peekID()+1, session.Mid)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		row := f.rows[id]
		if row.Token == session.Token {
			return dupErr("account_session.uk_token", session.Token)
		}
		if row.RefreshToken == session.RefreshToken {
			return dupErr("account_session.uk_refresh", session.RefreshToken)
		}
	}
	now := nowUnix()
	cp := *session
	if cp.CTime == 0 {
		cp.CTime = now
	}
	cp.MTime = now
	f.nextID++
	cp.ID = f.nextID
	f.rows[cp.ID] = &cp
	f.order = append(f.order, cp.ID)
	session.ID, session.CTime, session.MTime = cp.ID, cp.CTime, cp.MTime
	return nil
}

// Revoke 复刻 session.go:122-126：WHERE token = ? AND status = 0（已吊销行不再刷新 mtime）。
func (f *fakeSessionModel) Revoke(_ context.Context, token string) error {
	f.log.add("session.Revoke:%s", token)
	if err := f.fail("Revoke"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		row := f.rows[id]
		if row.Token == token && row.Status == model.SessionStatusActive {
			row.Status = model.SessionStatusRevoked
			row.MTime = nowUnix()
		}
	}
	return nil
}

// RevokeAll 复刻 session.go:128-132：WHERE mid = ? AND status = 0。
func (f *fakeSessionModel) RevokeAll(_ context.Context, mid int64) error {
	f.log.add("session.RevokeAll:%d", mid)
	if err := f.fail("RevokeAll"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		row := f.rows[id]
		if row.Mid == mid && row.Status == model.SessionStatusActive {
			row.Status = model.SessionStatusRevoked
			row.MTime = nowUnix()
		}
	}
	return nil
}

// UpdateToken 复刻 session.go:134-138：SET token, csrf, expires, mtime WHERE id = ?
// —— 注意 **不更新 refresh_token / refresh_expires / status**（README 已知缺口 4）。
func (f *fakeSessionModel) UpdateToken(_ context.Context, id int64, token, csrf string, expires int64) error {
	f.log.add("session.UpdateToken:%d", id)
	if err := f.fail("UpdateToken"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.order { // uk_token
		if x != id && f.rows[x].Token == token {
			return dupErr("account_session.uk_token", token)
		}
	}
	row, ok := f.rows[id]
	if !ok {
		return nil // UPDATE 未命中 0 行，不报错
	}
	row.Token, row.Csrf, row.Expires, row.MTime = token, csrf, expires, nowUnix()
	return nil
}

func (f *fakeSessionModel) put(s *model.AccountSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *s
	cp.ID = f.nextID
	f.rows[cp.ID] = &cp
	f.order = append(f.order, cp.ID)
}

// get 按主键回读（值拷贝）。
func (f *fakeSessionModel) get(id int64) *model.AccountSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// byToken 按 token 回读（值拷贝）。
func (f *fakeSessionModel) byToken(token string) *model.AccountSession {
	return f.firstBy(func(r *model.AccountSession) bool { return r.Token == token })
}

// mutate 静默按 token 改库存行本身（不写轨迹、不新增行）：
// 用于把会话布成「已过期 / 已吊销」这类无法通过正常写路径得到的状态。
func (f *fakeSessionModel) mutate(token string, fn func(*model.AccountSession)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if row := f.rows[id]; row.Token == token {
			fn(row)
			return
		}
	}
	panic("account_session 里没有 token=" + token + " 的行，无法布景")
}

// rowsOf 回读某 mid 的全部会话，按 id 升序。
func (f *fakeSessionModel) rowsOf(mid int64) []*model.AccountSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.AccountSession
	for _, id := range f.order {
		row := f.rows[id]
		if row.Mid == mid {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out
}

// activeFor 回读某 mid 仍有效的会话数。
func (f *fakeSessionModel) activeFor(mid int64) int {
	n := 0
	for _, row := range f.rowsOf(mid) {
		if row.Status == model.SessionStatusActive {
			n++
		}
	}
	return n
}

func (f *fakeSessionModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}

// ============================================================================
// account_login_log / account_capture_log 表
// ============================================================================

type fakeLoginLogModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   []*model.AccountLoginLog
	nextID int64
}

func newFakeLoginLogModel(log *callLog) *fakeLoginLogModel {
	return &fakeLoginLogModel{log: log}
}

func (f *fakeLoginLogModel) peekID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextID
}

// put 静默布一行登录日志（纪律 4：不写 callLog）。
// 与 Add 的差别只有一处，也是本方法存在的理由：put **不补 now()**，
// TS/CTime 原样入库。LoginLogs 的要紧结论是「ORDER BY ctime DESC, id DESC」
// 的排序方向与同秒 tiebreak，靠 now() 布出来的行时间戳全相等，方向就不可证了。
// ID 仍按插入顺序自增（与 session/secret 的 put 同口径），因此「插入序＝id 序」。
func (f *fakeLoginLogModel) put(l *model.AccountLoginLog) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *l
	cp.ID = f.nextID
	f.rows = append(f.rows, &cp)
}

// Add 复刻 login_log.go:83-96：ts 为 0 补 now、ctime 一律刷新、回填自增 ID。
func (f *fakeLoginLogModel) Add(_ context.Context, l *model.AccountLoginLog) error {
	f.log.add("loginlog.Add:%d/%d/%d/%d", f.peekID()+1, l.Mid, l.LoginType, l.Status)
	if err := f.fail("Add"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := nowUnix()
	cp := *l
	if cp.TS == 0 {
		cp.TS = now
	}
	cp.CTime = now
	f.nextID++
	cp.ID = f.nextID
	f.rows = append(f.rows, &cp)
	l.ID, l.TS, l.CTime = cp.ID, cp.TS, cp.CTime
	return nil
}

func (f *fakeLoginLogModel) FindByMid(_ context.Context, mid int64, limit int32) ([]*model.AccountLoginLog, error) {
	f.log.add("loginlog.FindByMid:%d/%d", mid, limit)
	if err := f.fail("FindByMid"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*model.AccountLoginLog
	for _, row := range f.rows {
		if row.Mid == mid {
			cp := *row
			hits = append(hits, &cp)
		}
	}
	// 复刻 model/login_log.go:105-106 的 `ORDER BY ctime DESC, id DESC`。
	// 批次3 修正：原先这里只写 slices.Reverse(hits)（＝纯 id 倒序），
	// 于是「ctime 与插入序不同」的布景会被替身排成和生产完全不同的顺序
	// （Add 一律刷 now()，所以既有测试看不出来；LoginLogs 的排序方向用例必须靠 put 布
	//  乱 ctime，替身不跟着修就变成「替身自己错」而不是「生产错」）。
	slices.SortStableFunc(hits, func(a, b *model.AccountLoginLog) int {
		if a.CTime != b.CTime {
			if a.CTime > b.CTime {
				return -1
			}
			return 1
		}
		if a.ID != b.ID {
			if a.ID > b.ID {
				return -1
			}
			return 1
		}
		return 0
	})
	if int32(len(hits)) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// rowsOf 回读某 mid 的日志（插入顺序）。
func (f *fakeLoginLogModel) rowsOf(mid int64) []*model.AccountLoginLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.AccountLoginLog
	for _, row := range f.rows {
		if row.Mid == mid {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out
}

func (f *fakeLoginLogModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// only 取唯一一行（不唯一直接 panic，避免用例静默读错行）。
func (f *fakeLoginLogModel) only() *model.AccountLoginLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) != 1 {
		panic(fmt.Sprintf("期望 account_login_log 只有 1 行，实际 %d 行", len(f.rows)))
	}
	cp := *f.rows[0]
	return &cp
}

type fakeCaptureLogModel struct {
	faultInjector
	log    *callLog
	mu     sync.Mutex
	rows   []*model.AccountCaptureLog
	nextID int64
}

func newFakeCaptureLogModel(log *callLog) *fakeCaptureLogModel {
	return &fakeCaptureLogModel{log: log}
}

func (f *fakeCaptureLogModel) peekID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextID
}

func (f *fakeCaptureLogModel) Add(_ context.Context, l *model.AccountCaptureLog) error {
	f.log.add("capturelog.Add:%d/%d/%s/%d", f.peekID()+1, l.Biz, l.Target, l.Status)
	if err := f.fail("Add"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *l
	f.nextID++
	cp.ID = f.nextID
	cp.CTime = nowUnix()
	f.rows = append(f.rows, &cp)
	return nil
}

func (f *fakeCaptureLogModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// rowsFor 回读某 biz+target 的发送记录（插入顺序，值拷贝）。
func (f *fakeCaptureLogModel) rowsFor(biz int8, target string) []*model.AccountCaptureLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.AccountCaptureLog
	for _, row := range f.rows {
		if row.Biz == biz && row.Target == target {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out
}

// only 回读唯一一行（不唯一直接 panic，避免用例静默读错行）。
func (f *fakeCaptureLogModel) only() *model.AccountCaptureLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) != 1 {
		panic(fmt.Sprintf("期望 account_capture_log 只有 1 行，实际 %d 行", len(f.rows)))
	}
	cp := *f.rows[0]
	return &cp
}

// ============================================================================
// 下游 client 替身
//
// 批次1 的口径：登录/会话链路**不该**触到下游，所以每个方法一被调用就返回
// errDownstreamWired（响铃）。批次2 的资料聚合方法（Info/Card/Profile/Vip 一族）
// 必须能走「下游真的返回数据」这条分支，因此在**不改动默认行为**的前提下加了一层
// 可选布数据：对应 map 为 nil（未用 withDownstream 布过）时，行为与批次1 逐字相同；
// 布过之后才按表回答，且仍允许用 faultInjector 注入故障来驱动降级分支。
// 批次3 又按同一口径加了「写侧」（AddExp/AddMoral）与「关系族」两层可选接线。
//
// 布数据一律返回值拷贝 / proto.Clone（纪律 1）：适配器每次 RPC 都新建对象，
// 若把库存指针直接交出去，生产代码的写回就会污染后续用例。
// ============================================================================

// errDownstreamWired 是「登录/会话链路竟然调了下游」的响铃信号：
// 一旦有人把 user-profile 塞进注册或鉴权路径，所有相关用例都会因此变红，
// 而不是悄悄多出一条不可断言的 RPC 调用。
var errDownstreamWired = errors.New("account/logic test: 下游 client 被调用（登录/会话链路不应依赖 user-profile / social-graph）")

type fakeUserProfileClient struct {
	faultInjector
	log *callLog

	bases     map[int64]*repository.UserProfileBase
	members   map[int64]*repository.UserProfileMember
	levels    map[int64]*rpc.LevelInfo
	realnames map[int64]int32

	// 批次3：写侧（AddExp/AddMoral）的可选接线。writeWired 为 false 时与批次1 逐字相同
	// （一律 errDownstreamWired）；接线后按「下游写成功」返回 nil 并把**全部入参**记账，
	// 于是 exp_moral.go:16-22 / 32-37 的「写成功 → 失效该 mid 的四个缓存」才进入被测路径
	// （不接线时那次失效永远不可达，用它做断言等于自欺）。
	writeWired  bool
	mu          sync.Mutex
	expWrites   []expWrite
	moralWrites []moralWrite
}

// expWrite / moralWrite 是下游写侧收到的入参记账（float64 与三段字符串都要能逐字比对，
// 轨迹字符串会把 "" 与缺字段拼成同一条，所以必须结构化）。
type expWrite struct {
	Mid      int64
	Exp      float64
	Operater string
	Operate  string
	Reason   string
}

type moralWrite struct {
	Mid    int64
	Moral  float64
	Oper   string
	Reason string
	Remark string
}

func (f *fakeUserProfileClient) call(method string, args ...any) {
	f.log.add("userProfile.%s:%s", method, fmt.Sprint(args...))
}

func (f *fakeUserProfileClient) Base(ctx context.Context, mid int64) (*repository.UserProfileBase, error) {
	f.call("Base", mid)
	if f.bases == nil {
		return nil, f.orDownstreamWired("Base")
	}
	if err := f.fail("Base"); err != nil {
		return nil, err
	}
	b := f.bases[mid]
	if b == nil {
		return nil, nil // 适配器口径：user-profile 回 mid=0 哨兵 → (nil, nil)
	}
	cp := *b
	return &cp, nil
}

func (f *fakeUserProfileClient) Bases(ctx context.Context, mids []int64) (map[int64]*repository.UserProfileBase, error) {
	f.call("Bases", joinInts(mids))
	if f.bases == nil {
		return nil, f.orDownstreamWired("Bases")
	}
	if err := f.fail("Bases"); err != nil {
		return nil, err
	}
	out := make(map[int64]*repository.UserProfileBase, len(mids))
	for _, mid := range mids {
		b := f.bases[mid]
		if b == nil {
			continue // 不存在的 mid 不进结果（适配器会跳过 mid=0 哨兵）
		}
		cp := *b
		out[mid] = &cp
	}
	return out, nil
}

func (f *fakeUserProfileClient) Member(ctx context.Context, mid int64) (*repository.UserProfileMember, error) {
	f.call("Member", mid)
	if f.members == nil {
		return nil, f.orDownstreamWired("Member")
	}
	if err := f.fail("Member"); err != nil {
		return nil, err
	}
	return copyMember(f.members[mid]), nil
}

func (f *fakeUserProfileClient) Members(ctx context.Context, mids []int64) (map[int64]*repository.UserProfileMember, error) {
	f.call("Members", joinInts(mids))
	if f.members == nil {
		return nil, f.orDownstreamWired("Members")
	}
	if err := f.fail("Members"); err != nil {
		return nil, err
	}
	out := make(map[int64]*repository.UserProfileMember, len(mids))
	for _, mid := range mids {
		if m := f.members[mid]; m != nil {
			out[mid] = copyMember(m)
		}
	}
	return out, nil
}

func (f *fakeUserProfileClient) LevelExp(ctx context.Context, mid int64) (*rpc.LevelInfo, error) {
	f.call("LevelExp", mid)
	if f.levels == nil {
		return nil, f.orDownstreamWired("LevelExp")
	}
	if err := f.fail("LevelExp"); err != nil {
		return nil, err
	}
	if le := f.levels[mid]; le != nil {
		return proto.Clone(le).(*rpc.LevelInfo), nil
	}
	return nil, nil // 适配器口径：reply==nil → Repository 兜零值
}

func (f *fakeUserProfileClient) RealnameStatus(ctx context.Context, mid int64) (int32, error) {
	f.call("RealnameStatus", mid)
	if f.realnames == nil {
		return 0, f.orDownstreamWired("RealnameStatus")
	}
	if err := f.fail("RealnameStatus"); err != nil {
		return 0, err
	}
	return f.realnames[mid], nil // 未布的 mid 回 0（未认证）
}

// AddExp / AddMoral 是批次3 的写侧：未接线（writeWired=false）时保持批次1 的响铃口径；
// 接线后先让故障注入有机会生效（与读侧同一条优先级），再记账入参并返回成功。
func (f *fakeUserProfileClient) AddExp(_ context.Context, mid int64, exp float64, operater, operate, reason string) error {
	f.call("AddExp", mid)
	if !f.writeWired {
		return f.orDownstreamWired("AddExp")
	}
	if err := f.fail("AddExp"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expWrites = append(f.expWrites, expWrite{Mid: mid, Exp: exp, Operater: operater, Operate: operate, Reason: reason})
	return nil
}
func (f *fakeUserProfileClient) AddMoral(_ context.Context, mid int64, moral float64, oper, reason, remark string) error {
	f.call("AddMoral", mid)
	if !f.writeWired {
		return f.orDownstreamWired("AddMoral")
	}
	if err := f.fail("AddMoral"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moralWrites = append(f.moralWrites, moralWrite{Mid: mid, Moral: moral, Oper: oper, Reason: reason, Remark: remark})
	return nil
}

// expWriteAt / moralWriteAt 回读第 i 条写侧记账（越界直接 panic：
// 「用例以为只写了一次、其实一次都没有」绝不能被静默放过）。
func (f *fakeUserProfileClient) expWriteAt(i int) expWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.expWrites) {
		panic(fmt.Sprintf("期望 user-profile.AddExp 至少有 %d 条记账，实际 %d 条", i+1, len(f.expWrites)))
	}
	return f.expWrites[i]
}

func (f *fakeUserProfileClient) moralWriteAt(i int) moralWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.moralWrites) {
		panic(fmt.Sprintf("期望 user-profile.AddMoral 至少有 %d 条记账，实际 %d 条", i+1, len(f.moralWrites)))
	}
	return f.moralWrites[i]
}

// writeStats 回读两条写侧各自的记账条数。
func (f *fakeUserProfileClient) writeStats() (exp, moral int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.expWrites), len(f.moralWrites)
}

// orDownstreamWired 保留批次1 的响铃口径：注入的故障优先，否则一律 errDownstreamWired。
func (f *fakeUserProfileClient) orDownstreamWired(method string) error {
	if err := f.fail(method); err != nil {
		return err
	}
	return errDownstreamWired
}

// copyMember 深拷一层 Official：适配器每次调用都新建 OfficialInfo，
// 直接把库存指针塞进 reply 会让「上游有没有改到布数据」这件事不可证。
func copyMember(m *repository.UserProfileMember) *repository.UserProfileMember {
	if m == nil {
		return nil
	}
	cp := *m
	if m.Official != nil {
		cp.Official = proto.Clone(m.Official).(*rpc.OfficialInfo)
	}
	return &cp
}

type fakeSocialGraphClient struct {
	faultInjector
	log *callLog

	stats map[int64]*repository.RelationStat

	// 批次3：关系族的可接线布数据。对应 map 为 nil（没布过）时与批次1 逐字相同（响铃）；
	// 键的取法与 repository/relation.go 的调用完全一致：
	// Relation/Relations/Attentions/Blacks 的第一参数是 mid，RichRelations 的第一参数是 owner。
	follows    map[int64]map[int64]bool
	attentions map[int64][]int64
	blacks     map[int64]map[int64]bool
	richs      map[int64]map[int64]int32

	mu    sync.Mutex
	calls []sgCall
}

// sgCall 是一次关系族调用的**入参记账**。必须有它，因为轨迹里 owners/mids 走 joinInts：
// nil 与 []int64{} 都拼成空串、且无法区分「传了空切片」与「根本没传」，
// 「logic 把请求字段原样传到下游」这条结论在字符串轨迹上是不可证的。
type sgCall struct {
	method string
	mid    int64
	owner  int64
	list   []int64
}

func (f *fakeSocialGraphClient) call(method string, args ...any) {
	f.log.add("socialGraph.%s:%s", method, fmt.Sprint(args...))
}

// record 记账一次调用（list 保留 nil / 空 / 重复 / 顺序）。
func (f *fakeSocialGraphClient) record(c sgCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

// callsOf 按顺序回读某个方法的全部入参记账。
func (f *fakeSocialGraphClient) callsOf(method string) []sgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sgCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// onlyCall 回读某个方法的唯一一次调用（不唯一直接 panic，避免用例静默读错条）。
func (f *fakeSocialGraphClient) onlyCall(method string) sgCall {
	got := f.callsOf(method)
	if len(got) != 1 {
		panic(fmt.Sprintf("期望 social-graph.%s 只被调用 1 次，实际 %d 次", method, len(got)))
	}
	return got[0]
}

// totalCalls 回读关系族记账总条数（「生产装配下一步都没触到下游」用这条）。
func (f *fakeSocialGraphClient) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// copyInt64s 保留 nil / 空切片 / 重复 / 顺序的切片拷贝（纪律 1 的入参记账版本）。
func copyInt64s(in []int64) []int64 {
	if in == nil {
		return nil
	}
	return append([]int64{}, in...)
}

func (f *fakeSocialGraphClient) Relation(ctx context.Context, mid, owner int64) (bool, error) {
	f.call("Relation", mid, owner)
	f.record(sgCall{method: "Relation", mid: mid, owner: owner})
	if f.follows == nil {
		return false, f.orDownstreamWired("Relation")
	}
	if err := f.fail("Relation"); err != nil {
		return false, err
	}
	return f.follows[mid][owner], nil
}
func (f *fakeSocialGraphClient) Relations(ctx context.Context, mid int64, owners []int64) (map[int64]bool, error) {
	f.call("Relations", mid, joinInts(owners))
	f.record(sgCall{method: "Relations", mid: mid, list: copyInt64s(owners)})
	if f.follows == nil {
		return nil, f.orDownstreamWired("Relations")
	}
	if err := f.fail("Relations"); err != nil {
		return nil, err
	}
	return copyFollows(f.follows[mid]), nil // 该 mid 没数据 → (nil, nil)：适配器口径「查不到」
}

func copyFollows(src map[int64]bool) map[int64]bool {
	if src == nil {
		return nil
	}
	out := make(map[int64]bool, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func copyBlacks(src map[int64]bool) map[int64]bool { return copyFollows(src) }

func copyInt32s(src map[int64]int32) map[int64]int32 {
	if src == nil {
		return nil
	}
	out := make(map[int64]int32, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (f *fakeSocialGraphClient) Stat(ctx context.Context, mid int64) (*repository.RelationStat, error) {
	f.call("Stat", mid)
	f.record(sgCall{method: "Stat", mid: mid})
	if f.stats == nil {
		return nil, f.orDownstreamWired("Stat")
	}
	if err := f.fail("Stat"); err != nil {
		return nil, err
	}
	st := f.stats[mid]
	if st == nil {
		return nil, nil // Repository.Stat 把 nil 兜成零值（relation.go:116-118）
	}
	cp := *st
	return &cp, nil
}
func (f *fakeSocialGraphClient) Attentions(ctx context.Context, mid int64) ([]int64, error) {
	f.call("Attentions", mid)
	f.record(sgCall{method: "Attentions", mid: mid}) // 入参只有 mid；返回值为空是布景事实，不进记账
	if f.attentions == nil {
		return nil, f.orDownstreamWired("Attentions")
	}
	if err := f.fail("Attentions"); err != nil {
		return nil, err
	}
	return copyInt64s(f.attentions[mid]), nil // 该 mid 没数据 → (nil, nil)
}
func (f *fakeSocialGraphClient) Blacks(ctx context.Context, mid int64) (map[int64]bool, error) {
	f.call("Blacks", mid)
	f.record(sgCall{method: "Blacks", mid: mid})
	if f.blacks == nil {
		return nil, f.orDownstreamWired("Blacks")
	}
	if err := f.fail("Blacks"); err != nil {
		return nil, err
	}
	return copyBlacks(f.blacks[mid]), nil
}
func (f *fakeSocialGraphClient) RichRelations(ctx context.Context, owner int64, mids []int64) (map[int64]int32, error) {
	f.call("RichRelations", owner, joinInts(mids))
	f.record(sgCall{method: "RichRelations", owner: owner, list: copyInt64s(mids)})
	if f.richs == nil {
		return nil, f.orDownstreamWired("RichRelations")
	}
	if err := f.fail("RichRelations"); err != nil {
		return nil, err
	}
	return copyInt32s(f.richs[owner]), nil
}

// orDownstreamWired 同 fakeUserProfileClient：批次1 的响铃口径逐字保留。
func (f *fakeSocialGraphClient) orDownstreamWired(method string) error {
	if err := f.fail(method); err != nil {
		return err
	}
	return errDownstreamWired
}

// ============================================================================
// 装配
// ============================================================================

type store struct {
	log         *callLog
	cache       *fakeCache
	conn        *fakeConn
	account     *fakeAccountModel
	cred        *fakeCredentialModel
	secret      *fakeSecretModel
	session     *fakeSessionModel
	loginLog    *fakeLoginLogModel
	captureLog  *fakeCaptureLogModel
	userProfile *fakeUserProfileClient
	socialGraph *fakeSocialGraphClient
	repo        *repository.Repository
	rsa         *repository.PassportRSA
	tokenTTL    int64
	// 批次3：把下游 client 换成「未注入（nil 接口）」装配，复刻 servicecontext.go:35-38
	// 的生产事实（social-graph 适配器只有 TODO）。置位后替身仍然存在，只是不再传给 Repository。
	noSocialGraph bool
	noUserProfile bool
}

// storeOpt 在 Repository 组装之前改装配参数（只能换非 model 依赖）。
type storeOpt func(*store)

// withPassportRSA 装配「生产已配置密钥」的分支：密码需 RSA 解密，与未配置时（明文）
// 是两套真实行为，见 README 已知缺口 5。
func withPassportRSA() storeOpt {
	return func(st *store) { st.rsa = testPassportRSA }
}

// withTokenTTL 覆盖 access token 有效期（天），验证配置项真的进了签发口径。
func withTokenTTL(days int64) storeOpt {
	return func(st *store) { st.tokenTTL = days }
}

// withoutSocialGraph 复刻「**未配置** social-graph」的装配：internal/svc/servicecontext.go:37-40
// 只有在 Etcd.Hosts/Target 都为空时才不注入适配器（2026-10-03 之前该分支体是 TODO，
// 生产永远不赋值，所以本 opt 复刻的是当时的唯一形态），此时 socialGraph 是 nil 接口，
// 关系族（Relation/Relations/Attentions/Blacks/RichRelations/Stat）在
// repository/relation.go 的 nil 分支直接回空默认值。
// 关系族用例必须同时跑「本 opt 的未接线形状」与「注入了替身的可达形状」，
// 否则会把「替身接了线才有的行为」当成生产行为。
func withoutSocialGraph() storeOpt {
	return func(st *store) { st.noSocialGraph = true }
}

// withoutUserProfile 与 withoutSocialGraph 同口径，只是换成 user-profile：
// 生产里它是**真的注入了**的（servicecontext.go:30-33），因此 nil userProfile
// 分支（exp_moral.go:14-16 的 ErrNotImplemented）在纯 logic 用例里只能靠本 opt 复刻。
func withoutUserProfile() storeOpt {
	return func(st *store) { st.noUserProfile = true }
}

// Downstream 是「某个 mid 在下游真的查得到」的布景数据（批次2）。
// 字段留 nil 表示该下游方法**不接线**：对应的调用仍旧返回 errDownstreamWired，
// 于是「Profile3 只用 Member/RealnameStatus、绝不碰 Base」这类依赖面事实
// 能在调用轨迹里被看见，而不是被一张全量布好的表掩盖。
//
// 注意 Base/Member 的 Mid 字段要显式给：repository.RawInfos 按 base.Mid 归键
// （raw.go:61），布错 mid 会让结果挂到别人头上，而用例必须能发现这件事。
type Downstream struct {
	Base     *repository.UserProfileBase
	Member   *repository.UserProfileMember
	Level    *rpc.LevelInfo
	Realname *int32 // nil = 不接线；*0 = 已接线且未认证
	Stat     *repository.RelationStat
	// 批次3：关系族布数据。键的语义随方法而变——
	// Relation/Relations/Attentions/Blacks 以 mid 为键，RichRelations 以 owner 为键，
	// 因此 withDownstream(70001, ...) 里 Relation 读作「70001 关注了哪些 owner」，
	// 而 Rich 读作「owner=70001 视角下各 mid 的亲密度」。
	Relation   map[int64]bool  // SocialGraphClient.Relation/Relations 的数据源
	Attentions []int64         // SocialGraphClient.Attentions 的数据源（保留顺序）
	Blacks     map[int64]bool  // SocialGraphClient.Blacks 的数据源
	Rich       map[int64]int32 // SocialGraphClient.RichRelations 的数据源
}

// withDownstream 把某个 mid 的下游返回值布好（静默：写 map 不算被测调用）。
// 同一用例可以叠加多个 withDownstream（每个 mid 一个）。
func withDownstream(mid int64, d Downstream) storeOpt {
	return func(st *store) {
		up, sg := st.userProfile, st.socialGraph
		if d.Base != nil {
			if up.bases == nil {
				up.bases = map[int64]*repository.UserProfileBase{}
			}
			up.bases[mid] = d.Base
		}
		if d.Member != nil {
			if up.members == nil {
				up.members = map[int64]*repository.UserProfileMember{}
			}
			up.members[mid] = d.Member
		}
		if d.Level != nil {
			if up.levels == nil {
				up.levels = map[int64]*rpc.LevelInfo{}
			}
			up.levels[mid] = d.Level
		}
		if d.Realname != nil {
			if up.realnames == nil {
				up.realnames = map[int64]int32{}
			}
			up.realnames[mid] = *d.Realname
		}
		if d.Stat != nil {
			if sg.stats == nil {
				sg.stats = map[int64]*repository.RelationStat{}
			}
			sg.stats[mid] = d.Stat
		}
		if d.Relation != nil {
			if sg.follows == nil {
				sg.follows = map[int64]map[int64]bool{}
			}
			sg.follows[mid] = copyFollows(d.Relation)
		}
		if d.Attentions != nil {
			if sg.attentions == nil {
				sg.attentions = map[int64][]int64{}
			}
			sg.attentions[mid] = copyInt64s(d.Attentions)
		}
		if d.Blacks != nil {
			if sg.blacks == nil {
				sg.blacks = map[int64]map[int64]bool{}
			}
			sg.blacks[mid] = copyBlacks(d.Blacks)
		}
		if d.Rich != nil {
			if sg.richs == nil {
				sg.richs = map[int64]map[int64]int32{}
			}
			sg.richs[mid] = copyInt32s(d.Rich)
		}
	}
}

// withDownstreamWrite 只接线 user-profile 的**写侧**（AddExp/AddMoral 返回成功），
// 读侧仍按批次1/2 口径（未布数据的 mid 响铃）。
// 为什么必须有它：exp_moral.go:19-37 里缓存失效只发生在下游写成功之后，
// 不接线就永远走不到那条失效路径，「AddExp3 会失效 i3_/c3_/v3_/p3_ 四个 key」
// 就成了无法验证的断言。
func withDownstreamWrite() storeOpt {
	return func(st *store) { st.userProfile.writeWired = true }
}

// i32 取 int32 的地址，供 Downstream.Realname 区分「布成未认证(0)」与「不接线」。
func i32(v int32) *int32 { return &v }

// newStore 组装真实 Repository（依赖为替身）并清空装配期轨迹（纪律 4）。
func newStore(opts ...storeOpt) *store {
	st := newRawStore(opts...)
	st.log.reset()
	return st
}

func newRawStore(opts ...storeOpt) *store {
	lg := &callLog{}
	st := &store{
		log:         lg,
		cache:       newFakeCache(lg),
		conn:        &fakeConn{},
		account:     newFakeAccountModel(lg),
		cred:        newFakeCredentialModel(lg),
		secret:      newFakeSecretModel(lg),
		session:     newFakeSessionModel(lg),
		loginLog:    newFakeLoginLogModel(lg),
		captureLog:  newFakeCaptureLogModel(lg),
		userProfile: &fakeUserProfileClient{log: lg},
		socialGraph: &fakeSocialGraphClient{log: lg},
	}
	for _, opt := range opts {
		opt(st)
	}
	// 下游 client 必须是**未装箱的 nil 接口**才等价于生产的未注入分支
	// （(*fakeSocialGraphClient)(nil) 装箱后 != nil，走不到 relation.go:26 的兜底）。
	var up repository.UserProfileClient = st.userProfile
	var sg repository.SocialGraphClient = st.socialGraph
	if st.noUserProfile {
		up = nil
	}
	if st.noSocialGraph {
		sg = nil
	}
	st.repo = repository.NewWithDeps(st.cache, st.conn,
		st.account, st.cred, st.secret, st.session, st.loginLog, st.captureLog,
		up, sg,
		repository.Options{
			PassportRSA:  st.rsa,
			TokenTTLDays: st.tokenTTL,
		})
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

// mustTokenInfo 只关心「还认不认这个 token」的链路里用：TokenInfo 报错本身就是失败，
// 不允许被调用方当成未登录悄悄放过。
func mustTokenInfo(t *testing.T, e *env, token string) *rpc.GetTokenInfoReply {
	t.Helper()
	info, err := callTokenInfo(t, e, token)
	wantNoErr(t, "TokenInfo 不得报错（报错即失败，不许当成未登录放过）", err)
	if info == nil {
		t.Fatalf("TokenInfo 返回 nil reply")
	}
	return info
}

// errText 取错误文本（nil 安全）。用于断言「内部错误原文外传给了客户端」这类
// 只能看报文才能定性的行为（缺口 10：crypto/SQL 错误原文进响应）。
func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// ============================================================================
// 静默布景（不写轨迹）
// ============================================================================

// seedUser 布一个「用户名 + 密码」齐备的账号，返回 mid。
// 走 put 系列，因此不算被测调用。固定 mid=70001：一个 store 里要布多个账号时
// 必须改用 seedUserAt，否则 mid 相撞会让「按 mid 统计」的断言悄悄失真。
func seedUser(t *testing.T, st *store, account, password string, credType int8) int64 {
	t.Helper()
	return seedUserAt(t, st, 70001, account, password, credType)
}

// seedUserAt 与 seedUser 同口径，只是由调用方指定 mid。
func seedUserAt(t *testing.T, st *store, mid int64, account, password string, credType int8) int64 {
	t.Helper()
	st.account.put(&model.Account{Mid: mid, Status: 0, RegIP: "10.0.0.1"})
	st.cred.put(&model.AccountCredential{Mid: mid, CredentialType: credType, Identifier: account, Status: 0})
	salt := "seedseedseedseedseedseedseedseed"
	st.secret.put(&model.AccountSecret{
		Mid: mid, SecretType: model.SecretTypePassword, Salt: salt,
		Hash: seedHash(password, salt), Status: model.SecretStatusActive,
		CTime: nowUnix(), MTime: nowUnix(),
	})
	return mid
}

// seedUserWithoutSecret 布一个「有标识、没设过密码」的账号（验证码注册后的中间态），
// 用于驱动 SetPassword 的首次设置分支。
func seedUserWithoutSecret(t *testing.T, st *store, account string, credType int8) int64 {
	t.Helper()
	mid := int64(70001)
	st.account.put(&model.Account{Mid: mid, Status: 0, RegIP: "10.0.0.1"})
	st.cred.put(&model.AccountCredential{Mid: mid, CredentialType: credType, Identifier: account, Status: 0})
	return mid
}

// seedHash 与 repository.saltPwd 同算法（MD5(pwd + ">>BiLiSaLt<<" + salt)）的测试侧复刻。
// 刻意不在 repository 包导出、也不调用生产私有函数：口径不一致时所有哈希比对用例一起变红。
func seedHash(pwd, salt string) string {
	sum := md5.Sum([]byte(pwd + ">>BiLiSaLt<<" + salt))
	return hex.EncodeToString(sum[:])
}

// seedCapture 布一个验证码（biz/target 与 repository 的 key 派生一致）。
func seedCapture(st *store, biz int8, target, code string) {
	v, err := strconv.ParseInt(code, 10, 64)
	if err != nil {
		panic("验证码布景必须是十进制整数：" + err.Error())
	}
	st.cache.warmInt(fmt.Sprintf("cap_code_%d_%s", biz, target), v)
}

// capCodeKey / capErrKey / capTimesKey 复刻 login.go 的 key 派生（不一致时用例即红）。
func capCodeKey(biz int8, target string) string  { return fmt.Sprintf("cap_code_%d_%s", biz, target) }
func capErrKey(biz int8, target string) string   { return fmt.Sprintf("cap_err_%d_%s", biz, target) }
func capTimesKey(biz int8, target string) string { return fmt.Sprintf("cap_times_%d_%s", biz, target) }

// login 走真实 PasswordLogin 拿一个会话（顺带把 ak_/rk_ 缓存灌上），用于
// 「先登录、再改密/登出/刷新」这类链式用例，避免手工布会话与缓存不同步。
// 作为布景步骤，它结束后会把轨迹清零（纪律 4：布数据必须静默）；
// 需要断言登录本身序列的用例请直接调 NewPasswordLoginLogic。
func login(t *testing.T, e *env, account, password, ip, device string) *rpc.LoginReply {
	t.Helper()
	reply, err := NewPasswordLoginLogic(context.Background(), e.svcCtx).PasswordLogin(&rpc.LoginReq{
		Account: account, Password: password, LoginType: int32(model.LoginTypePassword), Ip: ip, Device: device,
	})
	wantNoErr(t, "登录布景 "+account, err)
	if reply == nil {
		t.Fatalf("登录布景返回 nil reply")
	}
	e.st.log.reset()
	return reply
}

// ============================================================================
// 测试用 RSA 密钥（进程内生成一次；两把：一把配对、一把故意不配对）
// ============================================================================

var testPassportRSA = func() *repository.PassportRSA {
	pub, priv := mustRSAPEM(2048)
	return repository.NewPassportRSA(pub, priv)
}()

// otherRSAPEM 是另一把不配对的公钥 PEM：用它加密的密码在 testPassportRSA 下必然解密失败。
var otherPubPEM = func() string {
	pub, _ := mustRSAPEM(2048)
	return pub
}()

func mustRSAPEM(bits int) (pubPEM, privPEM string) {
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		panic("测试 RSA 密钥生成失败：" + err.Error())
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		panic("测试公钥编码失败：" + err.Error())
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// encryptWithOtherKey 用「另一把公钥」加密密码，模拟客户端拿了错配/过期的公钥。
func encryptWithOtherKey(t *testing.T, plain string) string {
	t.Helper()
	other := repository.NewPassportRSA(otherPubPEM, "")
	bs, err := other.CardEncrypt([]byte(plain))
	wantNoErr(t, "测试侧 RSA 加密", err)
	return string(bs)
}

// 编译期确认替身满足 model / repository 接口（签名漂移在编译期即暴露）。
var (
	_ model.AccountModel           = (*fakeAccountModel)(nil)
	_ model.AccountCredentialModel = (*fakeCredentialModel)(nil)
	_ model.AccountSecretModel     = (*fakeSecretModel)(nil)
	_ model.AccountSessionModel    = (*fakeSessionModel)(nil)
	_ model.AccountLoginLogModel   = (*fakeLoginLogModel)(nil)
	_ model.AccountCaptureLogModel = (*fakeCaptureLogModel)(nil)
	_ repository.Cacher            = (*fakeCache)(nil)
	_ repository.UserProfileClient = (*fakeUserProfileClient)(nil)
	_ repository.SocialGraphClient = (*fakeSocialGraphClient)(nil)
)
