package logic

// fakes_test.go 是 user-profile logic 测试的内存替身集合（35 个 logic 方法共用）。
//
// 为什么需要注入缝：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造 repository.New 会连真 Redis + 真 MySQL，还会拉起官方认证刷新协程和 Outbox
// 发布协程，测试无处塞替身。因此本包用例统一用 repository.NewWithDeps(内存缓存, 内存 conn,
// 14 个内存 model) 组装**真实的 Repository**，只把它的依赖换成替身——这样
// 「缓存读穿/回填、防击穿哨兵、等级换算、日志投影、部分命中口径」整条链路都在被测路径上，
// 而不是把 Repository 也 mock 掉。见 internal/repository/cache.go 的 Cacher 注释。
//
// 四条替身纪律（catalog / rights / playback 三轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic 里 `row.Moral = ...` 之类的写回不得污染库存行，
//     否则「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 写入按真实 SQL 的口径处理主键与默认值：user_base 的 rank/birthday 有 DDL 默认值
//     （5000 / -28800），单列 INSERT ... ON DUPLICATE 补建行时按同一默认值落，
//     自增表（member_log / member_outbox / realname_*）忽略入参 ID、自增分配。
//  3. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：user-profile 要紧的结论是
//     「先读缓存还是先查库、回源后回填了什么 TTL、批量接口部分命中后 FindMany 拿到哪几个 mid、
//     经验读失败时 Member 有没有退化」，只数次数会漏掉次序错误。
//  4. 布数据走替身的**静默写入路径**（seed* / warm* → put，而非公开的 Insert/Upsert）：
//     布景不算被测调用，轨迹断言可以直接从 0 开始数。newStore 还会清空构造期
//     loadOfficial 留下的轨迹（那是装配，不是被测代码）。
//  5. 错误注入按方法粒度（faultInjector.failWith("FindOne", err)），不用一个全局 err：
//     读侧用例会互相污染，也定位不了是哪条分支。
//
// 与 playback 多出来的一条口径：异步回填（fanout）在 NewWithDeps 里被换成**同步执行器**
// （见 repository 的 asyncRunner 注释），所以 `cache.SetJSON:bs_123/3600` 这类回填调用
// 会稳定出现在被测序列里；生产是入队异步执行，时序不同但内容一致。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（WHERE 条件、ORDER BY 方向、唯一键、LIMIT 口径），
//     不证明 SQL 与列名本身；`model/*.go` 的列名/占位符仍无单测（见 README 已知缺口）。
//   - GetInt 的替身按真实实现「值不是十进制整数就当 miss」，不返回错误——真实 Cache 也不返回。
//   - ExpStat 的 key 含 time.Now().Day()，布景与断言同源于用例开头取的那一次，
//     跨零点抖动会让期望值整体偏移（每天最多 1 次、窗口亚秒级），不做额外补偿。

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/user-profile/internal/config"
	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/model"
)

// --- 断言小工具（本包共享） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

// wantErrIs 断言错误链里有 want 这个哨兵（repository 用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %v", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

// wantNoErr 断言没有错误（「不存在」在本服务是结论不是错误，必须走这条）。
func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
	}
}

// wantStringsEQ 比较字符串切片（wantEQ 受 comparable 约束，切片只能另走这条）。
func wantStringsEQ(t *testing.T, label, field string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantInt64EQ 比较 int64 切片（批量回源的 mid 列表顺序是本服务的口径之一）。
func wantInt64EQ(t *testing.T, label, field string, got, want []int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// wantNoCall 断言 from 之后没有发生任何依赖调用（守卫必须发生在触库/触缓存之前）。
func wantNoCall(t *testing.T, label string, st *store, from int) {
	t.Helper()
	if ops := st.log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

// wantOps 按顺序比较完整调用序列。
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

// wantOpsLoose 与 wantOps 同义，但 want 里以 "~" 结尾的项**只比对前缀**。
// 用于轨迹里带 time.Now()/自增号的项（如 moral.TxUpdateRecoverDate:123/<ts>）：
// 那些值无法在断言侧复现，但前缀 + 「这一步发生了」仍是硬事实。
func wantOpsLoose(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label, strings.Join(got, " → "), strings.Join(want, " → "))
	}
	for i := range want {
		w := want[i]
		if strings.HasSuffix(w, "~") {
			p := strings.TrimSuffix(w, "~")
			if !strings.HasPrefix(got[i], p) {
				t.Errorf("%s：第 %d 次调用 = %s, want 前缀 %s（完整序列 [%s]）",
					label, i+1, got[i], p, strings.Join(got, " → "))
			}
			continue
		}
		if got[i] != w {
			t.Errorf("%s：第 %d 次调用 = %s, want %s（完整序列 [%s]）",
				label, i+1, got[i], w, strings.Join(got, " → "))
		}
	}
}

// wantOpsSameSet 比较**顺序不受控**的一段调用（多重集相等即可）。
// 只用在实现确实按 map 迭代序扇出的地方（BatchUpdateMoral 的失效+通知阶段），
// 其余一律用 wantOps 按顺序断言——用错等于放弃次序这条最贵的断言。
func wantOpsSameSet(t *testing.T, label, field string, got, want []string) {
	t.Helper()
	g, w := append([]string(nil), got...), append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if !slices.Equal(g, w) {
		t.Errorf("%s：%s（不计序）= %v, want %v（完整序列 %v）", label, field, got, want, got)
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

// wantTSWindow 断言一个 Unix 秒落在 [from, to] 内（用于 time.Now() 产生的 ts，
// 不能自比、也不能只断言 >0：0 与 20 年前都必须红）。
func wantTSWindow(t *testing.T, label, field string, got, from, to int64) {
	t.Helper()
	if got < from || got > to {
		t.Errorf("%s：%s = %d, want 落在 [%d, %d]", label, field, got, from, to)
	}
}

// wantUUIDLike 断言日志 ID 是 uuid4 形态（8-4-4-4-12）。
// 只断言「非空」等于放弃幂等键这件事——空串与 fallback-<nano> 都得红。
func wantUUIDLike(t *testing.T, label, field, v string) {
	t.Helper()
	segs := strings.Split(v, "-")
	if len(segs) != 5 {
		t.Fatalf("%s：%s = %q, want uuid4 形态", label, field, v)
	}
	for i, want := range []int{8, 4, 4, 4, 12} {
		if len(segs[i]) != want {
			t.Errorf("%s：%s 第 %d 段 = %q, want 长度 %d", label, field, i+1, segs[i], want)
		}
	}
}

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

// countPrefix 统计以 prefix 开头的调用数，用于「这类操作一次都没发生」的断言。
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
func (c *callLog) reset()                    { c.ops = nil }

// itoa 拼调用轨迹里的整数段：轨迹由替身用 %d 格式化，用例期望值必须同源。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// --- 错误注入 ---

type faultInjector struct {
	by  map[string]error
	nth map[string]*nthFault
}

// nthFault 让某个方法的第 n 次调用失败，前 n-1 次照常成功。
type nthFault struct {
	left int // 还剩几次放行
	err  error
}

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

// failOn 把故障点精确放在第 n 次调用上（n 从 1 起）。
// 批量写链路必须有它：只有让「第二条」失败，才能证明实现是
// 「整批一个事务、失败即中止」而不是「逐条提交、失败跳过」。
func (f *faultInjector) failOn(method string, n int, err error) {
	if f.nth == nil {
		f.nth = map[string]*nthFault{}
	}
	f.nth[method] = &nthFault{left: n - 1, err: err}
}

func (f *faultInjector) fail(method string) error {
	if nf, ok := f.nth[method]; ok {
		if nf.left > 0 {
			nf.left--
		} else {
			return nf.err
		}
	}
	return f.by[method]
}

// --- Redis key 复刻（与 repository 未导出的 key 函数逐字对齐；不一致时读穿用例即红） ---

const (
	expShardTest  = 10000 // = repository.expShard
	statLoginTest = "login"
	statViewTest  = "watch"
	statShareTest = "shareClick" // = repository.statShare
)

func keyBase(mid int64) string     { return fmt.Sprintf("bs_%d", mid) }
func keyExp(mid int64) string      { return fmt.Sprintf("exp_%d", mid) }
func keyMoral(mid int64) string    { return fmt.Sprintf("moral_%d", mid) }
func keyRealname(mid int64) string { return fmt.Sprintf("realname_info_%d", mid) }
func keyCapCode(mid int64) string  { return fmt.Sprintf("realname_cap_code_%d", mid) }
func keyCapTimes(mid int64) string { return fmt.Sprintf("realname_cap_times_%d", mid) }
func keyCapErr(mid int64) string   { return fmt.Sprintf("realname_cap_err_times_%d", mid) }
func keyExpAdded(tp string, mid, day int64) string {
	return fmt.Sprintf("ea_%s_%d_%d", tp, day, mid/expShardTest)
}
func keyExpCoin(mid, day int64) string { return fmt.Sprintf("ecoin_%d_%d", day, mid) }

// --- 缓存替身 ---

// fakeCache 实现 repository.Cacher。
// miss 口径按真实 Cache：GetJSON 未命中返回 (nil, nil) 且不改写 v；GetInt 未命中或值不可解析
// 返回 (0, false)。GetBit 与生产一样把故障上抛（ExpStat 因此不降级，见用例）。
type fakeCache struct {
	faultInjector
	log   *callLog
	jsons map[string][]byte
	strs  map[string]string
	bits  map[string]map[int64]bool
	ttls  map[string]int
	// writes 记录**每一次**写进某个 key 的原始字节（SetJSON/SetInt 追加，Del 与覆盖写都不清除）。
	// PII 断言必须看它而不是看终态：实名链路是「先回填、再删除」，只看终态会把
	// 真的进过 Redis 的明文证件号判成干净——那正是本服务最不能接受的一类假绿。
	writes map[string][]string
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{
		log: log, jsons: map[string][]byte{}, strs: map[string]string{},
		bits: map[string]map[int64]bool{}, ttls: map[string]int{}, writes: map[string][]string{},
	}
}

// blobOf 回读某个 key 历史上被写入过的全部内容（拼接）。
func (f *fakeCache) blobOf(key string) string { return strings.Join(f.writes[key], "\n") }

func (f *fakeCache) Ping(context.Context) error {
	f.log.add("cache.Ping")
	return f.fail("Ping")
}

func (f *fakeCache) GetJSON(_ context.Context, key string, v any) error {
	f.log.add("cache.GetJSON:%s", key)
	if err := f.fail("GetJSON"); err != nil {
		return err
	}
	bs, ok := f.jsons[key]
	if !ok {
		return nil // miss：v 保持零值
	}
	_ = json.Unmarshal(bs, v) // 与真实 Cache 一致：坏值只记日志，按 miss 继续
	return nil
}

func (f *fakeCache) SetJSON(_ context.Context, key string, v any, ttlSeconds int) {
	f.log.add("cache.SetJSON:%s/%d", key, ttlSeconds)
	if err := f.fail("SetJSON"); err != nil {
		return
	}
	bs, err := json.Marshal(v)
	if err != nil {
		return
	}
	f.jsons[key] = bs
	f.ttls[key] = ttlSeconds
	delete(f.strs, key)
	f.writes[key] = append(f.writes[key], string(bs))
}

func (f *fakeCache) GetInt(_ context.Context, key string) (int64, bool) {
	f.log.add("cache.GetInt:%s", key)
	s, ok := f.strs[key]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false // 与真实实现一致：非整数视为 miss（不注入 fail，避免造出不存在的错误分支）
	}
	return v, true
}

func (f *fakeCache) SetInt(_ context.Context, key string, v int64, ttlSeconds int) {
	f.log.add("cache.SetInt:%s/%d", key, ttlSeconds)
	if err := f.fail("SetInt"); err != nil {
		return
	}
	f.strs[key] = strconv.FormatInt(v, 10)
	f.ttls[key] = ttlSeconds
	delete(f.jsons, key)
	f.writes[key] = append(f.writes[key], strconv.FormatInt(v, 10))
}

func (f *fakeCache) Del(_ context.Context, key string) error {
	f.log.add("cache.Del:%s", key)
	if err := f.fail("Del"); err != nil {
		return err
	}
	delete(f.jsons, key)
	delete(f.strs, key)
	delete(f.bits, key)
	return nil
}

func (f *fakeCache) GetBit(_ context.Context, key string, offset int64) (bool, error) {
	f.log.add("cache.GetBit:%s/%s", key, itoa(offset))
	if err := f.fail("GetBit"); err != nil {
		return false, err
	}
	return f.bits[key][offset], nil
}

func (f *fakeCache) Incr(_ context.Context, key string) (int64, error) {
	f.log.add("cache.Incr:%s", key)
	if err := f.fail("Incr"); err != nil {
		return 0, err
	}
	v, _ := strconv.ParseInt(f.strs[key], 10, 64)
	v++
	f.strs[key] = strconv.FormatInt(v, 10)
	return v, nil
}

func (f *fakeCache) Expire(_ context.Context, key string, seconds int) error {
	f.log.add("cache.Expire:%s/%d", key, seconds)
	if err := f.fail("Expire"); err != nil {
		return err
	}
	f.ttls[key] = seconds
	return nil
}

// --- 缓存静默布景 / 回读（纪律 4：不写轨迹） ---

// warmJSON 直接把值编码成 JSON 塞进缓存（布「命中」场景，不经过被测写路径）。
func (f *fakeCache) warmJSON(key string, v any) {
	bs, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("布景序列化 %s 失败：%v", key, err))
	}
	f.jsons[key] = bs
}

// warmRaw 布一个原始字符串（用于「缓存里是坏值/别的结构」）。
func (f *fakeCache) warmRaw(key, raw string) { f.jsons[key] = []byte(raw) }

// warmInt 布一个整数缓存值。
func (f *fakeCache) warmInt(key string, v int64) {
	f.strs[key] = strconv.FormatInt(v, 10)
}

// warmBit 布位图某一位。
func (f *fakeCache) warmBit(key string, offset int64, on bool) {
	if f.bits[key] == nil {
		f.bits[key] = map[int64]bool{}
	}
	f.bits[key][offset] = on
}

// intOf 回读缓存里的整数（miss 返回 0,false）。
func (f *fakeCache) intOf(key string) (int64, bool) {
	v, err := strconv.ParseInt(f.strs[key], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// jsonOf 回读缓存里的 JSON 值并反序列化到 v；key 不存在返回 false。
func (f *fakeCache) jsonOf(key string, v any) bool {
	bs, ok := f.jsons[key]
	if !ok {
		return false
	}
	return json.Unmarshal(bs, v) == nil
}

// --- 事务连接替身 ---

// txSession 只是「拿到了事务会话」的凭证；fake models 只判断它非 nil。
type txSession struct{ sqlx.Session }

// fakeConn 只实现 user-profile 用到的 TransactCtx。其它方法落在嵌入的 nil 接口上会直接 panic，
// 等价于「logic 单测路径上不允许出现任何直连 SQL」——真出现了就是越界，让它炸出来。
type fakeConn struct {
	sqlx.SqlConn
	transactions int
	rolledBack   int
}

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	f.transactions++
	if err := fn(ctx, &txSession{}); err != nil {
		f.rolledBack++
		return err
	}
	return nil
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// --- user_base ---

type fakeBaseModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.UserBase
	// lastMany 记最近一次 FindMany 的入参，用于断言「只回源 miss 的那几个」。
	lastMany []int64
}

func newFakeBaseModel(log *callLog) *fakeBaseModel {
	return &fakeBaseModel{log: log, rows: map[int64]*model.UserBase{}}
}

func (f *fakeBaseModel) FindOne(_ context.Context, mid int64) (*model.UserBase, error) {
	f.log.add("base.FindOne:%d", mid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[mid]
	if !ok {
		return nil, nil // 与 model 一致：查无此行返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeBaseModel) FindMany(_ context.Context, mids []int64) (map[int64]*model.UserBase, error) {
	f.log.add("base.FindMany:%s", joinInts(mids))
	f.lastMany = append([]int64(nil), mids...)
	if err := f.fail("FindMany"); err != nil {
		return nil, err
	}
	out := make(map[int64]*model.UserBase, len(mids))
	for _, mid := range mids {
		row, ok := f.rows[mid]
		if !ok {
			continue // 与 model 一致：缺失的 mid 不进结果
		}
		cp := *row
		out[mid] = &cp
	}
	return out, nil
}

// setCol 复刻单列 INSERT ... ON DUPLICATE：补建行时落 DDL 默认值（rank=5000、birthday=-28800）。
func (f *fakeBaseModel) setCol(method string, mid int64, apply func(*model.UserBase)) (int64, error) {
	f.log.add("base.%s:%d", method, mid)
	if err := f.fail(method); err != nil {
		return 0, err
	}
	row, ok := f.rows[mid]
	if !ok {
		row = &model.UserBase{Mid: mid, Rank: model.DefaultRank, Birthday: model.DefaultTime}
		f.rows[mid] = row
	}
	apply(row)
	return 1, nil
}

func (f *fakeBaseModel) SetBase(_ context.Context, base *model.UserBase) error {
	f.log.add("base.SetBase:%d", base.Mid)
	if err := f.fail("SetBase"); err != nil {
		return err
	}
	cp := *base
	f.rows[base.Mid] = &cp
	return nil
}

func (f *fakeBaseModel) SetSex(_ context.Context, mid, sex int64) error {
	_, err := f.setCol("SetSex", mid, func(r *model.UserBase) { r.Sex = sex })
	return err
}

func (f *fakeBaseModel) SetName(_ context.Context, mid int64, name string) error {
	_, err := f.setCol("SetName", mid, func(r *model.UserBase) { r.Name = name })
	return err
}

func (f *fakeBaseModel) SetRank(_ context.Context, mid, rank int64) error {
	_, err := f.setCol("SetRank", mid, func(r *model.UserBase) { r.Rank = rank })
	return err
}

func (f *fakeBaseModel) SetSign(_ context.Context, mid int64, sign string) error {
	_, err := f.setCol("SetSign", mid, func(r *model.UserBase) { r.Sign = sign })
	return err
}

func (f *fakeBaseModel) SetBirthday(_ context.Context, mid, birthday int64) error {
	_, err := f.setCol("SetBirthday", mid, func(r *model.UserBase) { r.Birthday = birthday })
	return err
}

func (f *fakeBaseModel) SetFace(_ context.Context, mid int64, face string) error {
	_, err := f.setCol("SetFace", mid, func(r *model.UserBase) { r.Face = face })
	return err
}

// get 回读库存行副本。
func (f *fakeBaseModel) get(mid int64) *model.UserBase {
	row, ok := f.rows[mid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// put 静默布一行基础资料（不写轨迹）。
func (f *fakeBaseModel) put(b *model.UserBase) {
	cp := *b
	f.rows[b.Mid] = &cp
}

// --- user_exp ---

type fakeExpModel struct {
	faultInjector
	log      *callLog
	rows     map[int64]int64
	lastMany []int64
}

func newFakeExpModel(log *callLog) *fakeExpModel {
	return &fakeExpModel{log: log, rows: map[int64]int64{}}
}

func (f *fakeExpModel) FindOne(_ context.Context, mid int64) (int64, error) {
	f.log.add("exp.FindOne:%d", mid)
	if err := f.fail("FindOne"); err != nil {
		return 0, err
	}
	return f.rows[mid], nil // 与 model 一致：无此行返回 0
}

func (f *fakeExpModel) FindMany(_ context.Context, mids []int64) (map[int64]int64, error) {
	f.log.add("exp.FindMany:%s", joinInts(mids))
	f.lastMany = append([]int64(nil), mids...)
	if err := f.fail("FindMany"); err != nil {
		return nil, err
	}
	out := make(map[int64]int64, len(mids))
	for _, mid := range mids {
		if v, ok := f.rows[mid]; ok {
			out[mid] = v
		}
	}
	return out, nil
}

// Set 复刻 INSERT ... ON DUPLICATE KEY UPDATE exp = VALUES(exp)。
func (f *fakeExpModel) Set(_ context.Context, mid, exp int64) (int64, error) {
	f.log.add("exp.Set:%d/%d", mid, exp)
	if err := f.fail("Set"); err != nil {
		return 0, err
	}
	_, existed := f.rows[mid]
	f.rows[mid] = exp
	if existed {
		return 2, nil // MySQL：UPDATE 命中一行时 RowsAffected=2
	}
	return 1, nil
}

// Incr 复刻 UPDATE exp = exp + ?（列是 BIGINT UNSIGNED，负增量越界由 MySQL 报错，
// 替身按饱和处理并如实返回受影响行数）。
func (f *fakeExpModel) Incr(_ context.Context, mid, delta int64) (int64, error) {
	f.log.add("exp.Incr:%d/%d", mid, delta)
	if err := f.fail("Incr"); err != nil {
		return 0, err
	}
	cur, ok := f.rows[mid]
	if !ok {
		return 0, nil // UPDATE 未命中：受影响 0 行
	}
	if v := cur + delta; v >= 0 {
		f.rows[mid] = v
	} else {
		f.rows[mid] = 0
	}
	return 1, nil
}

func (f *fakeExpModel) put(mid, exp int64) { f.rows[mid] = exp }

func (f *fakeExpModel) value(mid int64) int64 { return f.rows[mid] }

// --- user_flag ---

type fakeFlagModel struct {
	faultInjector
	log  *callLog
	rows map[int64]uint
}

func newFakeFlagModel(log *callLog) *fakeFlagModel {
	return &fakeFlagModel{log: log, rows: map[int64]uint{}}
}

// HasAttr 复刻 WHERE mid=? AND flag & ? = ?：无行或位未置位都返回 false 且不算错误。
func (f *fakeFlagModel) HasAttr(_ context.Context, mid int64, attr uint) (bool, error) {
	f.log.add("flag.HasAttr:%d/%d", mid, attr)
	if err := f.fail("HasAttr"); err != nil {
		return false, err
	}
	return model.HasAttr(f.rows[mid], attr), nil
}

func (f *fakeFlagModel) SetAttr(_ context.Context, mid int64, attr uint) error {
	f.log.add("flag.SetAttr:%d/%d", mid, attr)
	if err := f.fail("SetAttr"); err != nil {
		return err
	}
	f.rows[mid] = model.SetAttr(f.rows[mid], attr)
	return nil
}

func (f *fakeFlagModel) put(mid int64, flag uint) { f.rows[mid] = flag }

func (f *fakeFlagModel) value(mid int64) uint { return f.rows[mid] }

// --- user_moral ---

type fakeMoralModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.UserMoral
	gotTx map[int64]bool // Tx* 系列是否拿到事务会话
}

func newFakeMoralModel(log *callLog) *fakeMoralModel {
	return &fakeMoralModel{log: log, rows: map[int64]*model.UserMoral{}, gotTx: map[int64]bool{}}
}

func (f *fakeMoralModel) FindOne(ctx context.Context, mid int64) (*model.UserMoral, error) {
	f.log.add("moral.FindOne:%d", mid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	return f.txfindOne(ctx, nil, mid)
}

func (f *fakeMoralModel) TxFindOne(ctx context.Context, tx sqlx.Session, mid int64) (*model.UserMoral, error) {
	f.log.add("moral.TxFindOne:%d", mid)
	f.gotTx[mid] = tx != nil
	if err := f.fail("TxFindOne"); err != nil {
		return nil, err
	}
	return f.txfindOne(ctx, tx, mid)
}

func (f *fakeMoralModel) txfindOne(_ context.Context, _ sqlx.Session, mid int64) (*model.UserMoral, error) {
	row, ok := f.rows[mid]
	if !ok {
		return nil, nil // 与 model 一致：查无此行返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

// TxInit 复刻 INSERT IGNORE：已存在则不覆盖，也不算错误。
func (f *fakeMoralModel) TxInit(_ context.Context, tx sqlx.Session, mid, moral, added, deducted, lastRecoverDate int64) error {
	f.log.add("moral.TxInit:%d/%d", mid, moral)
	f.markTx(mid, tx)
	if err := f.fail("TxInit"); err != nil {
		return err
	}
	if _, ok := f.rows[mid]; ok {
		return nil
	}
	f.rows[mid] = &model.UserMoral{Mid: mid, Moral: moral, Added: added, Deducted: deducted, LastRecoverDate: lastRecoverDate}
	return nil
}

// TxUpdate 复刻 UPDATE moral = moral + ?, added = added + ?, deducted = deducted + ?。
func (f *fakeMoralModel) TxUpdate(_ context.Context, tx sqlx.Session, mid, moral, added, deducted int64) error {
	f.log.add("moral.TxUpdate:%d/%d/%d/%d", mid, moral, added, deducted)
	f.markTx(mid, tx)
	if err := f.fail("TxUpdate"); err != nil {
		return err
	}
	row, ok := f.rows[mid]
	if !ok {
		return nil
	}
	row.Moral = satSub(row.Moral, moral)
	row.Added += abs64(added)
	row.Deducted += abs64(deducted)
	return nil
}

func (f *fakeMoralModel) TxUpdateRecoverDate(_ context.Context, tx sqlx.Session, mid int64, recoverDate int64) error {
	f.log.add("moral.TxUpdateRecoverDate:%d/%d", mid, recoverDate)
	f.markTx(mid, tx)
	if err := f.fail("TxUpdateRecoverDate"); err != nil {
		return err
	}
	if row, ok := f.rows[mid]; ok {
		row.LastRecoverDate = recoverDate
	}
	return nil
}

func (f *fakeMoralModel) markTx(mid int64, tx sqlx.Session) { f.gotTx[mid] = tx != nil }

func (f *fakeMoralModel) get(mid int64) *model.UserMoral {
	row, ok := f.rows[mid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// put 静默布一行节操值（LastRecoverDate 给 DDL 默认值 -28800，避免用例忘记布景时被误读成 0）。
func (f *fakeMoralModel) put(m *model.UserMoral) {
	cp := *m
	if cp.LastRecoverDate == 0 {
		cp.LastRecoverDate = model.DefaultTime
	}
	f.rows[m.Mid] = &cp
}

func satSub(cur, delta int64) int64 {
	if v := cur + delta; v >= 0 {
		return v
	}
	return 0
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// --- user_official（生效认证，Member/Members 的内存快照来源） ---

type fakeOfficialModel struct {
	faultInjector
	log   *callLog
	rows  map[int64]*model.OfficialInfo
	calls int
}

func newFakeOfficialModel(log *callLog) *fakeOfficialModel {
	return &fakeOfficialModel{log: log, rows: map[int64]*model.OfficialInfo{}}
}

func (f *fakeOfficialModel) FindOne(_ context.Context, mid int64) (*model.OfficialInfo, error) {
	f.log.add("official.FindOne:%d", mid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[mid]
	if !ok {
		return nil, nil // SQL 带 role > 0，未认证行也返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

// All 复刻 SELECT ... WHERE role > 0（每次返回新建的 map 与值拷贝）。
func (f *fakeOfficialModel) All(context.Context) (map[int64]*model.OfficialInfo, error) {
	f.calls++
	f.log.add("official.All")
	if err := f.fail("All"); err != nil {
		return nil, err
	}
	out := make(map[int64]*model.OfficialInfo, len(f.rows))
	for mid, row := range f.rows {
		if row.Role <= 0 {
			continue
		}
		cp := *row
		out[mid] = &cp
	}
	return out, nil
}

func (f *fakeOfficialModel) put(mid int64, info *model.OfficialInfo) {
	cp := *info
	f.rows[mid] = &cp
}

// --- user_official_doc / user_official_doc_addit ---

type fakeOfficialDocModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.OfficialDoc
}

func newFakeOfficialDocModel(log *callLog) *fakeOfficialDocModel {
	return &fakeOfficialDocModel{log: log, rows: map[int64]*model.OfficialDoc{}}
}

func (f *fakeOfficialDocModel) FindOne(_ context.Context, mid int64) (*model.OfficialDoc, error) {
	f.log.add("officialDoc.FindOne:%d", mid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[mid]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// Upsert 复刻 ON DUPLICATE KEY UPDATE：不含 reject_reason 列，故审核驳回原因保持不变。
func (f *fakeOfficialDocModel) Upsert(_ context.Context, doc *model.OfficialDoc) error {
	f.log.add("officialDoc.Upsert:%d/%d", doc.Mid, doc.State)
	if err := f.fail("Upsert"); err != nil {
		return err
	}
	if row, ok := f.rows[doc.Mid]; ok {
		reject := row.RejectReason
		cp := *doc
		cp.RejectReason = reject
		f.rows[doc.Mid] = &cp
		return nil
	}
	cp := *doc
	f.rows[doc.Mid] = &cp
	return nil
}

func (f *fakeOfficialDocModel) get(mid int64) *model.OfficialDoc {
	row, ok := f.rows[mid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

type fakeAdditModel struct {
	faultInjector
	log  *callLog
	rows map[int64]map[string]string
}

func newFakeAdditModel(log *callLog) *fakeAdditModel {
	return &fakeAdditModel{log: log, rows: map[int64]map[string]string{}}
}

func (f *fakeAdditModel) Upsert(_ context.Context, mid int64, property, vstring string) error {
	f.log.add("addit.Upsert:%d/%s", mid, property)
	if err := f.fail("Upsert"); err != nil {
		return err
	}
	if f.rows[mid] == nil {
		f.rows[mid] = map[string]string{}
	}
	f.rows[mid][property] = vstring
	return nil
}

func (f *fakeAdditModel) value(mid int64, property string) string { return f.rows[mid][property] }

// --- user_monitor ---

type fakeMonitorModel struct {
	faultInjector
	log  *callLog
	rows map[int64]*model.UserMonitor
}

func newFakeMonitorModel(log *callLog) *fakeMonitorModel {
	return &fakeMonitorModel{log: log, rows: map[int64]*model.UserMonitor{}}
}

// InMonitor 复刻 SELECT COUNT(1) WHERE mid=? AND is_deleted=0。
func (f *fakeMonitorModel) InMonitor(_ context.Context, mid int64) (bool, error) {
	f.log.add("monitor.InMonitor:%d", mid)
	if err := f.fail("InMonitor"); err != nil {
		return false, err
	}
	row, ok := f.rows[mid]
	return ok && row.IsDeleted == 0, nil
}

// Add 复刻 ON DUPLICATE KEY UPDATE operator/remark 并清除软删除。
func (f *fakeMonitorModel) Add(_ context.Context, mid int64, operator, remark string) error {
	f.log.add("monitor.Add:%d", mid)
	if err := f.fail("Add"); err != nil {
		return err
	}
	f.rows[mid] = &model.UserMonitor{Mid: mid, Operator: operator, Remark: remark, IsDeleted: 0}
	return nil
}

func (f *fakeMonitorModel) put(m *model.UserMonitor) {
	cp := *m
	f.rows[m.Mid] = &cp
}

// --- user_property_review ---

type fakeReviewModel struct {
	faultInjector
	log   *callLog
	rows  []*model.UserPropertyReview
	next  int64
	calls []string
}

func newFakeReviewModel(log *callLog) *fakeReviewModel {
	return &fakeReviewModel{log: log}
}

func (f *fakeReviewModel) Add(_ context.Context, review *model.UserPropertyReview) error {
	f.next++
	f.log.add("review.Add:%d/%d/%d", review.Mid, review.Property, f.next)
	if err := f.fail("Add"); err != nil {
		return err
	}
	cp := *review
	f.rows = append(f.rows, &cp)
	return nil
}

// Archive 复刻 UPDATE ... WHERE mid=? AND property=? AND state=0。
func (f *fakeReviewModel) Archive(_ context.Context, mid int64, property int8, operator, remark string) error {
	f.log.add("review.Archive:%d/%d", mid, property)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%s/%s", mid, property, operator, remark))
	if err := f.fail("Archive"); err != nil {
		return err
	}
	for _, row := range f.rows {
		if row.Mid == mid && row.Property == property && row.State == model.ReviewStateWait {
			row.State = model.ReviewStateArchived
			row.Operator = operator
			row.Remark = remark
		}
	}
	return nil
}

func (f *fakeReviewModel) all() []*model.UserPropertyReview {
	out := make([]*model.UserPropertyReview, 0, len(f.rows))
	for _, row := range f.rows {
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// --- realname_info / realname_apply / realname_apply_img ---

type fakeRealnameModel struct {
	faultInjector
	log    *callLog
	rows   map[int64]*model.RealnameInfo
	nextID int64
	gotTx  map[int64]bool
}

func newFakeRealnameModel(log *callLog) *fakeRealnameModel {
	return &fakeRealnameModel{log: log, rows: map[int64]*model.RealnameInfo{}, gotTx: map[int64]bool{}}
}

func (f *fakeRealnameModel) FindOne(_ context.Context, mid int64) (*model.RealnameInfo, error) {
	f.log.add("realname.FindOne:%d", mid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[mid]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// FindOneByCardMD5 与真实语句 `WHERE card_md5 = ? AND status IN (0,1) LIMIT 1` 有意分歧：
// 这里**不过滤 status**，让 checkCardDup 里那句 Go 侧状态判断（realname.go:490）成为
// 唯一承重断言。若把它也补成 status 过滤，Go 侧判断写错也测不出来。
func (f *fakeRealnameModel) FindOneByCardMD5(_ context.Context, cardMD5 string) (*model.RealnameInfo, error) {
	f.log.add("realname.FindOneByCardMD5:%s", cardMD5)
	if err := f.fail("FindOneByCardMD5"); err != nil {
		return nil, err
	}
	for _, row := range f.rows {
		if row.CardMD5 == cardMD5 {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

// FindMidsByCardMD5s 复刻真实 SQL 的两处口径：空列表**不发查询**（model/realname.go:101
// 直接返回空 map，所以替身也不该写轨迹），以及 `AND status IN (0, 1)` 的状态过滤
// ——被驳回(2)/未申请(3)的绑定不参与反查。
func (f *fakeRealnameModel) FindMidsByCardMD5s(_ context.Context, cardMD5s []string) (map[string]int64, error) {
	if len(cardMD5s) == 0 {
		return map[string]int64{}, nil
	}
	f.log.add("realname.FindMidsByCardMD5s:%s", strings.Join(cardMD5s, ","))
	if err := f.fail("FindMidsByCardMD5s"); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, md5 := range cardMD5s {
		for _, row := range f.rows {
			if row.CardMD5 == md5 &&
				(row.Status == model.RealnameApplyStatusPending || row.Status == model.RealnameApplyStatusPass) {
				out[md5] = row.Mid
			}
		}
	}
	return out, nil
}

// Upsert 复刻 ON DUPLICATE KEY UPDATE（按 mid 唯一键）。
func (f *fakeRealnameModel) Upsert(_ context.Context, tx sqlx.Session, info *model.RealnameInfo) error {
	f.log.add("realname.Upsert:%d/%d", info.Mid, info.Status)
	f.gotTx[info.Mid] = tx != nil
	if err := f.fail("Upsert"); err != nil {
		return err
	}
	cp := *info
	if row, ok := f.rows[info.Mid]; ok {
		cp.ID = row.ID
		cp.CTime = row.CTime
	} else {
		f.nextID++
		cp.ID = f.nextID
	}
	f.rows[info.Mid] = &cp
	return nil
}

func (f *fakeRealnameModel) UpdateStatus(_ context.Context, tx sqlx.Session, mid int64, status int8, reason string) error {
	f.log.add("realname.UpdateStatus:%d/%d", mid, status)
	f.gotTx[mid] = tx != nil
	if err := f.fail("UpdateStatus"); err != nil {
		return err
	}
	if row, ok := f.rows[mid]; ok {
		row.Status = status
		row.Reason = reason
	}
	return nil
}

func (f *fakeRealnameModel) put(info *model.RealnameInfo) {
	f.nextID++
	cp := *info
	cp.ID = f.nextID
	f.rows[info.Mid] = &cp
}

func (f *fakeRealnameModel) get(mid int64) *model.RealnameInfo {
	row, ok := f.rows[mid]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

type fakeApplyModel struct {
	faultInjector
	log    *callLog
	rows   []*model.RealnameApply
	nextID int64
}

func newFakeApplyModel(log *callLog) *fakeApplyModel {
	return &fakeApplyModel{log: log}
}

// FindOne 取该 mid 的最新一条（model SQL 为 ORDER BY id DESC LIMIT 1）。
func (f *fakeApplyModel) FindOne(_ context.Context, mid int64) (*model.RealnameApply, error) {
	f.log.add("apply.FindOne:%d", mid)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	var best *model.RealnameApply
	for _, row := range f.rows {
		if row.Mid == mid && (best == nil || row.ID > best.ID) {
			best = row
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

// Insert 复刻 model/realname.go 的真实语句
// `INSERT INTO realname_apply (mid, realname, country, card_type, card_num, card_md5,
// hand_img, front_img, back_img, status)`——**列集里没有 ctime/mtime**，
// 仓库里设的 CTime 根本不会落库（DDL 默认 0），替身按同一列集裁剪，免得把「没写进去」
// 伪装成「写对了」（纪律 2）。自增 ID 按真实 model 回写入参指针（LastInsertId 语义）。
func (f *fakeApplyModel) Insert(_ context.Context, apply *model.RealnameApply) error {
	f.nextID++
	f.log.add("apply.Insert:%d/%d", apply.Mid, f.nextID)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	apply.ID = f.nextID // 真实 model：apply.ID, err = res.LastInsertId()
	cp := *apply
	cp.CTime, cp.MTime = 0, 0 // 生产 SQL 不写这两列
	f.rows = append(f.rows, &cp)
	return nil
}

func (f *fakeApplyModel) latest(mid int64) *model.RealnameApply {
	var best *model.RealnameApply
	for _, row := range f.rows {
		if row.Mid == mid && (best == nil || row.ID > best.ID) {
			best = row
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

type fakeApplyImgModel struct {
	faultInjector
	log    *callLog
	rows   map[int64]*model.RealnameApplyImage
	nextID int64
}

func newFakeApplyImgModel(log *callLog) *fakeApplyImgModel {
	return &fakeApplyImgModel{log: log, rows: map[int64]*model.RealnameApplyImage{}}
}

func (f *fakeApplyImgModel) FindOne(_ context.Context, id int64) (*model.RealnameApplyImage, error) {
	f.log.add("applyImg.FindOne:%d", id)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[id]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// Insert 复刻真实语句 `INSERT INTO realname_apply_img (img_data) VALUES (?)`：
// 列集只有 img_data（生产设的 CTime 不落库，DDL 默认 0），并按
// `img.ID, err = res.LastInsertId()` 把自增 ID **回写入参指针**——
// RealnameApply 正是靠这个回读写进 apply 的 hand/front/back_img 三列，
// 替身若只写内部副本，「图片 ID 有没有传下去」这条断言就会被掩盖（纪律 1/2）。
func (f *fakeApplyImgModel) Insert(_ context.Context, img *model.RealnameApplyImage) error {
	f.nextID++
	f.log.add("applyImg.Insert:%d", f.nextID)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	img.ID = f.nextID // 真实 model：img.ID, err = res.LastInsertId()
	cp := *img
	cp.CTime, cp.MTime = 0, 0 // 生产 SQL 不写这两列
	f.rows[cp.ID] = &cp
	return nil
}

// lastID 返回最近一次 Insert 分配的自增 ID（用于布景时预知 ID；
// 被测路径自身已能从入参指针读到回写值，不依赖这个方法）。
func (f *fakeApplyImgModel) lastID() int64 { return f.nextID }

// --- member_log ---

type fakeLogModel struct {
	faultInjector
	log    *callLog
	rows   []*model.MemberLog
	nextID int64
	calls  []string
}

func newFakeLogModel(log *callLog) *fakeLogModel {
	return &fakeLogModel{log: log}
}

func (f *fakeLogModel) Add(_ context.Context, logType int8, ul *model.UserLog) (int64, error) {
	// 复刻 deploy/migrations/user-profile/000009 的 UNIQUE KEY uk_log_type_log_id
	// (log_type, log_id)：同一 (类型, 日志 ID) 再插一次在真库是 1062 而不是「多一行」。
	// 缺了这条约束，UndoMoral 复用原 log_id 写撤销日志的冲突就会被替身悄悄吞掉。
	for _, row := range f.rows {
		if row.LogType == logType && row.LogID == ul.LogID {
			f.log.add("memberLog.Add:%d/%d/%d", logType, ul.Mid, f.nextID+1)
			f.calls = append(f.calls, fmt.Sprintf("%d/%d", logType, ul.Mid))
			return 0, fmt.Errorf("Error 1062: Duplicate entry '%d-%s' for key 'member_log.uk_log_type_log_id'", logType, ul.LogID)
		}
	}
	f.nextID++
	f.log.add("memberLog.Add:%d/%d/%d", logType, ul.Mid, f.nextID)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d", logType, ul.Mid))
	if err := f.fail("Add"); err != nil {
		return 0, err
	}
	bs, err := json.Marshal(ul.Content)
	if err != nil {
		return 0, err
	}
	f.rows = append(f.rows, &model.MemberLog{
		ID: f.nextID, LogType: logType, Mid: ul.Mid, LogID: ul.LogID,
		TS: ul.TS, IP: ul.IP, Content: string(bs), Status: model.LogStatusActive, CTime: ul.TS,
	})
	return f.nextID, nil
}

// FindByMid 复刻 WHERE log_type=? AND mid=? AND ts>=now-7d AND status=0
// ORDER BY ts DESC, id DESC LIMIT 1000；content 反序列化失败的行按真实实现跳过。
func (f *fakeLogModel) FindByMid(_ context.Context, logType int8, mid int64) ([]*model.UserLog, error) {
	f.log.add("memberLog.FindByMid:%d/%d", logType, mid)
	f.calls = append(f.calls, fmt.Sprintf("find:%d/%d", logType, mid))
	if err := f.fail("FindByMid"); err != nil {
		return nil, err
	}
	since := time.Now().Add(-time.Hour * 24 * 7).Unix()
	var ids []int64
	for _, row := range f.rows {
		if row.LogType == logType && row.Mid == mid && row.TS >= since && row.Status == model.LogStatusActive {
			ids = append(ids, row.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := f.byID(ids[i]), f.byID(ids[j])
		if a.TS != b.TS {
			return a.TS > b.TS
		}
		return a.ID > b.ID
	})
	if len(ids) > 1000 {
		ids = ids[:1000]
	}
	out := make([]*model.UserLog, 0, len(ids))
	for _, id := range ids {
		ul, err := f.byID(id).ToUserLog()
		if err != nil {
			continue // 与真实实现一致：坏 JSON 行跳过
		}
		out = append(out, ul)
	}
	return out, nil
}

func (f *fakeLogModel) FindByLogID(_ context.Context, logType int8, logID string) (*model.UserLog, error) {
	f.log.add("memberLog.FindByLogID:%d/%s", logType, logID)
	if err := f.fail("FindByLogID"); err != nil {
		return nil, err
	}
	var best *model.MemberLog
	for _, row := range f.rows {
		if row.LogType == logType && row.LogID == logID && (best == nil || row.ID > best.ID) {
			best = row
		}
	}
	if best == nil {
		return nil, nil // 真实实现返回 sql.ErrNoRows，由 repository 判定；这里保持「查无」语义
	}
	ul, err := best.ToUserLog()
	if err != nil {
		return nil, err
	}
	return ul, nil
}

func (f *fakeLogModel) MarkRevoked(_ context.Context, logType int8, logID string) error {
	f.log.add("memberLog.MarkRevoked:%d/%s", logType, logID)
	if err := f.fail("MarkRevoked"); err != nil {
		return err
	}
	for _, row := range f.rows {
		if row.LogType == logType && row.LogID == logID {
			row.Status = model.LogStatusRevoked
		}
	}
	return nil
}

func (f *fakeLogModel) byID(id int64) *model.MemberLog {
	for _, row := range f.rows {
		if row.ID == id {
			return row
		}
	}
	panic(fmt.Sprintf("布景缺失：member_log id=%d", id))
}

// put 静默布一条日志（不写轨迹）。content 为 nil 时按空 map 序列化。
func (f *fakeLogModel) put(logType int8, ul *model.UserLog, status int8) *model.MemberLog {
	f.nextID++
	bs, err := json.Marshal(ul.Content)
	if err != nil {
		panic(err)
	}
	if ul.Content == nil {
		bs = []byte("{}")
	}
	row := &model.MemberLog{
		ID: f.nextID, LogType: logType, Mid: ul.Mid, LogID: ul.LogID, TS: ul.TS,
		IP: ul.IP, Content: string(bs), Status: status, CTime: ul.TS,
	}
	f.rows = append(f.rows, row)
	return row
}

func (f *fakeLogModel) only() *model.MemberLog {
	if len(f.rows) != 1 {
		panic(fmt.Sprintf("期望 member_log 只有 1 行，实际 %d 行", len(f.rows)))
	}
	cp := *f.rows[0]
	return &cp
}

// count 返回行数（写侧用例的核心断言：一次变更一行日志，重放就是两行）。
func (f *fakeLogModel) count() int { return len(f.rows) }

// row 返回第 i 行的值拷贝（越界直接 panic，避免用例静默读到 nil）。
func (f *fakeLogModel) row(i int) *model.MemberLog {
	if i < 0 || i >= len(f.rows) {
		panic(fmt.Sprintf("member_log 第 %d 行不存在（共 %d 行）", i, len(f.rows)))
	}
	cp := *f.rows[i]
	return &cp
}

// rowsOf 返回某个 (log_type, mid) 的全部行，按插入顺序（自增 id 升序）。
func (f *fakeLogModel) rowsOf(logType int8, mid int64) []*model.MemberLog {
	var out []*model.MemberLog
	for _, row := range f.rows {
		if row.LogType == logType && row.Mid == mid {
			cp := *row
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// statusOf 回读某条 (log_type, log_id) 最新一行的 status。
func (f *fakeLogModel) statusOf(logType int8, logID string) (int8, bool) {
	var best *model.MemberLog
	for _, row := range f.rows {
		if row.LogType == logType && row.LogID == logID && (best == nil || row.ID > best.ID) {
			best = row
		}
	}
	if best == nil {
		return 0, false
	}
	return best.Status, true
}

// --- member_outbox ---

type fakeOutboxModel struct {
	faultInjector
	log     *callLog
	rows    []*model.MemberOutbox
	nextID  int64
	gotTxID []int64 // 每次 Insert 是否拿到事务会话（true 记 id）
}

func newFakeOutboxModel(log *callLog) *fakeOutboxModel {
	return &fakeOutboxModel{log: log}
}

func (f *fakeOutboxModel) Insert(_ context.Context, tx sqlx.Session, out *model.MemberOutbox) error {
	f.nextID++
	f.log.add("outbox.Insert:%s/%s/%d", out.EventType, out.AggregateID, f.nextID)
	if tx != nil {
		f.gotTxID = append(f.gotTxID, f.nextID)
	}
	if err := f.fail("Insert"); err != nil {
		return err
	}
	cp := *out
	cp.ID = f.nextID
	if cp.CreatedAt == 0 {
		cp.CreatedAt = time.Now().Unix()
	}
	f.rows = append(f.rows, &cp)
	return nil
}

func (f *fakeOutboxModel) ListPending(_ context.Context, now int64, limit int) ([]*model.MemberOutbox, error) {
	f.log.add("outbox.ListPending:%d/%d", now, limit)
	if err := f.fail("ListPending"); err != nil {
		return nil, err
	}
	var out []*model.MemberOutbox
	for _, row := range f.rows {
		if row.Status == model.OutboxStatusPending && row.NextRetryAt <= now {
			cp := *row
			out = append(out, &cp)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeOutboxModel) MarkPublished(_ context.Context, id int64, publishedAt int64) error {
	f.log.add("outbox.MarkPublished:%d", id)
	if err := f.fail("MarkPublished"); err != nil {
		return err
	}
	for _, row := range f.rows {
		if row.ID == id {
			row.Status = model.OutboxStatusPublished
			row.PublishedAt = publishedAt
		}
	}
	return nil
}

func (f *fakeOutboxModel) MarkRetry(_ context.Context, id int64, attempts int32, nextRetryAt int64, lastError string) error {
	f.log.add("outbox.MarkRetry:%d", id)
	if err := f.fail("MarkRetry"); err != nil {
		return err
	}
	for _, row := range f.rows {
		if row.ID == id {
			row.Attempts = attempts
			row.NextRetryAt = nextRetryAt
			row.LastError = lastError
		}
	}
	return nil
}

func (f *fakeOutboxModel) MarkFailed(_ context.Context, id int64, lastError string) error {
	f.log.add("outbox.MarkFailed:%d", id)
	if err := f.fail("MarkFailed"); err != nil {
		return err
	}
	for _, row := range f.rows {
		if row.ID == id {
			row.Status = model.OutboxStatusFailed
			row.LastError = lastError
		}
	}
	return nil
}

func (f *fakeOutboxModel) only() *model.MemberOutbox {
	if len(f.rows) != 1 {
		panic(fmt.Sprintf("期望 member_outbox 只有 1 行，实际 %d 行", len(f.rows)))
	}
	cp := *f.rows[0]
	return &cp
}

// count 返回事件行数。
func (f *fakeOutboxModel) count() int { return len(f.rows) }

// row 返回第 i 行的值拷贝（按插入顺序，越界 panic）。
func (f *fakeOutboxModel) row(i int) *model.MemberOutbox {
	if i < 0 || i >= len(f.rows) {
		panic(fmt.Sprintf("member_outbox 第 %d 行不存在（共 %d 行）", i, len(f.rows)))
	}
	cp := *f.rows[i]
	return &cp
}

// eventTypes 回读全部事件的 event_type（顺序即写入顺序）。
func (f *fakeOutboxModel) eventTypes() []string {
	out := make([]string, 0, len(f.rows))
	for _, row := range f.rows {
		out = append(out, row.EventType)
	}
	return out
}

// --- 小工具 ---

// joinInts 把 mid 列表拼成轨迹里的一个字段（顺序即回源顺序，可断言）。
func joinInts(mids []int64) string {
	ps := make([]string, 0, len(mids))
	for _, m := range mids {
		ps = append(ps, itoa(m))
	}
	return strings.Join(ps, ",")
}

// --- 装配 ---

type store struct {
	log         *callLog
	cache       *fakeCache
	conn        *fakeConn
	base        *fakeBaseModel
	exp         *fakeExpModel
	flag        *fakeFlagModel
	moral       *fakeMoralModel
	official    *fakeOfficialModel
	officialDoc *fakeOfficialDocModel
	addit       *fakeAdditModel
	monitor     *fakeMonitorModel
	review      *fakeReviewModel
	realname    *fakeRealnameModel
	apply       *fakeApplyModel
	applyImg    *fakeApplyImgModel
	logs        *fakeLogModel
	outbox      *fakeOutboxModel
	repo        *repository.Repository
}

// storeOpt 在 Repository 组装**之前**布景（官方认证快照只在构造时装载一次）。
type storeOpt func(*store)

// withOfficial 布一条生效官方认证，使其进入 Repository 的内存快照
// （Member/Members 的 official_info 只能来自快照，见 repository.loadOfficial）。
func withOfficial(mid int64, role int8, title, desc string) storeOpt {
	return func(st *store) {
		st.official.put(mid, &model.OfficialInfo{Role: role, Title: title, Desc: desc})
	}
}

func newStore(opts ...storeOpt) *store {
	st := newRawStore(opts...)
	// 构造期的 official.All / 快照装载属装配副作用，不算被测调用（纪律 4）。
	st.log.reset()
	return st
}

// newRawStore 组装但不清空轨迹，用于断言构造期行为本身。
func newRawStore(opts ...storeOpt) *store {
	log := &callLog{}
	st := &store{
		log:         log,
		cache:       newFakeCache(log),
		conn:        &fakeConn{},
		base:        newFakeBaseModel(log),
		exp:         newFakeExpModel(log),
		flag:        newFakeFlagModel(log),
		moral:       newFakeMoralModel(log),
		official:    newFakeOfficialModel(log),
		officialDoc: newFakeOfficialDocModel(log),
		addit:       newFakeAdditModel(log),
		monitor:     newFakeMonitorModel(log),
		review:      newFakeReviewModel(log),
		realname:    newFakeRealnameModel(log),
		apply:       newFakeApplyModel(log),
		applyImg:    newFakeApplyImgModel(log),
		logs:        newFakeLogModel(log),
		outbox:      newFakeOutboxModel(log),
	}
	for _, opt := range opts {
		opt(st)
	}
	st.repo = repository.NewWithDeps(st.cache, st.conn,
		st.base, st.exp, st.flag, st.moral, st.official, st.officialDoc, st.addit,
		st.monitor, st.review, st.realname, st.apply, st.applyImg, st.logs, st.outbox,
		repository.Options{
			IMGURLTemplate: "https://cdn.example.com/idenfiles/%s.txt",
			Cryptor:        repository.NewCardCryptor(testPubPEM, testPrivPEM),
		})
	return st
}

// env 是一次用例的完整运行时：真实 Repository（依赖为替身）+ ServiceContext。
// Config 留零值即可：读侧 logic 只经 Repository 取数（etc/*.yaml 的加载由 config 包用例负责）。
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

// --- 实名测试密钥（运行时生成的 RSA-2048 PEM，全包共用一份） ---
//
// 本节按本文件原注释的指示收口：实名轮（Realname* / MidByRealnameCard）会真的走
// CardEncrypt/CardDecrypt，占位 PEM 会让「加解密失败」伪装成业务分支结论。
// 因此这里在包初始化时生成一把**真**密钥对（一次 ~100ms，只生成一次）：
//   - 公钥用 PKIX（对应 CardCryptor.rsaEncrypt 的 ParsePKIXPublicKey）；
//   - 私钥用 PKCS#1（对应 rsaDecrypt 的 ParsePKCS1PrivateKey 首选分支）。
//
// 用例若需要「解密必然失败」的分支，请显式换一把不配的密钥（见 realnamekeys_test.go），
// 不要靠坏 PEM 触发——那正是上面要规避的坑。

var testPubPEM, testPrivPEM = mustTestCardKeyPEM()

// mustTestCardKeyPEM 生成一把 RSA-2048 并编码成 CardCryptor 能吃的 PEM 对。
func mustTestCardKeyPEM() (pub, priv string) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("实名测试密钥生成失败：" + err.Error())
	}
	pkix, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		panic("实名测试公钥编码失败：" + err.Error())
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix})),
		string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// 编译期确认替身满足 model 接口（避免接口漂移只在调用点报错）。
var (
	_ model.UserBaseModel           = (*fakeBaseModel)(nil)
	_ model.UserExpModel            = (*fakeExpModel)(nil)
	_ model.UserFlagModel           = (*fakeFlagModel)(nil)
	_ model.UserMoralModel          = (*fakeMoralModel)(nil)
	_ model.UserOfficialModel       = (*fakeOfficialModel)(nil)
	_ model.OfficialDocModel        = (*fakeOfficialDocModel)(nil)
	_ model.OfficialDocAdditModel   = (*fakeAdditModel)(nil)
	_ model.UserMonitorModel        = (*fakeMonitorModel)(nil)
	_ model.UserPropertyReviewModel = (*fakeReviewModel)(nil)
	_ model.RealnameInfoModel       = (*fakeRealnameModel)(nil)
	_ model.RealnameApplyModel      = (*fakeApplyModel)(nil)
	_ model.RealnameApplyImageModel = (*fakeApplyImgModel)(nil)
	_ model.MemberLogModel          = (*fakeLogModel)(nil)
	_ model.MemberOutboxModel       = (*fakeOutboxModel)(nil)
)

// ============================================================================
// 写侧共用断言（B 批：资料写入 / 节操变更 / Outbox 事件解码）
// ============================================================================

// outboxView 是 common/eventenvelope.Envelope 的测试侧解码视图。
// 字段名必须与生产 JSON tag 逐字对齐：对不齐时解码为空串，断言立刻变红（刻意耦合）。
type outboxView struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    string          `json:"occurred_at"`
	Producer      string          `json:"producer"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	Payload       json.RawMessage `json:"payload"`
}

// decodeOutboxView 把一行 Outbox 解成信封 + payload 对象；解不出来直接 Fatal。
func decodeOutboxView(t *testing.T, label string, row *model.MemberOutbox) (outboxView, map[string]any) {
	t.Helper()
	var env outboxView
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		t.Fatalf("%s：member_outbox.payload 不是合法事件信封：%v（原文 %s）", label, err, row.Payload)
	}
	var pl map[string]any
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		t.Fatalf("%s：事件 payload 不是 JSON 对象：%v（原文 %s）", label, err, string(env.Payload))
	}
	return env, pl
}

// plStr / plInt 取 payload 里的字符串 / 数字字段，缺字段即 Fatal（不许用 ok 吞掉）。
func plStr(t *testing.T, label string, pl map[string]any, k string) string {
	t.Helper()
	v, ok := pl[k].(string)
	if !ok {
		t.Fatalf("%s：payload.%s = %#v, want string（整个 payload %v）", label, k, pl[k], pl)
	}
	return v
}

func plInt(t *testing.T, label string, pl map[string]any, k string) int64 {
	t.Helper()
	v, ok := pl[k].(float64)
	if !ok {
		t.Fatalf("%s：payload.%s = %#v, want number（整个 payload %v）", label, k, pl[k], pl)
	}
	return int64(v)
}

// decodeLogContent 回读 member_log 某一行的 content（map[string]string）。
func decodeLogContent(t *testing.T, label string, row *model.MemberLog) map[string]string {
	t.Helper()
	var c map[string]string
	if err := json.Unmarshal([]byte(row.Content), &c); err != nil {
		t.Fatalf("%s：member_log.content 不是 map[string]string：%v（原文 %s）", label, err, row.Content)
	}
	return c
}

// wantKeysExact 断言 map 的键集合**恰好**是 want（顺序无关，多一个少一个都红）。
// 日志 content / 事件 payload 的键集合是对外契约的一部分：
// 只断言「关注的键存在」会漏掉「多写了 PII」和「少写了审计字段」两类回归。
func wantKeysExact(t *testing.T, label, field string, got map[string]string, want []string) {
	t.Helper()
	g := make([]string, 0, len(got))
	for k := range got {
		g = append(g, k)
	}
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if !slices.Equal(g, w) {
		t.Errorf("%s：%s 键集合 = %v, want %v", label, field, g, w)
	}
}

// wantSetBaseWritten 断言 setBaseTx 家族（SetRank/SetSex/SetName/SetSign/SetBirthday/SetFace）
// **成功写入后**的共用不变量，六个方法共用一条链路，逐字段差异由各自用例断言：
//  1. 调用序列严格是「单列 UPSERT → Outbox 事件 → 失效 bs_<mid>」，一步不多一步不少；
//  2. 全程只有 1 个事务、0 次回滚，且事件行**拿到事务会话**（AGENTS.md §5 的 Outbox 同事务）；
//  3. 事件信封逐字段（event_type/schema_version/producer/aggregate/payload.action+mid）；
//  4. 失效范围恰好是 bs_<mid> 这一个 key——既不多删（exp_/moral_），也不少删。
//
// eventNo 是这条事件在 outbox 替身里的 1 基序号（自增 id 与其一致），用于断言「第几条」。
func wantSetBaseWritten(t *testing.T, label string, e *env, mid int64, method, action string, eventNo int) {
	t.Helper()
	wantOps(t, label+"：调用序列", e.ops(0), []string{
		"base." + method + ":" + itoa(mid),
		fmt.Sprintf("outbox.Insert:%s/%s/%d", model.EventProfileUpdated, itoa(mid), eventNo),
		"cache.Del:" + keyBase(mid),
	})
	wantEQ(t, label, "事务次数", e.st.conn.transactions, 1)
	wantEQ(t, label, "回滚次数", e.st.conn.rolledBack, 0)
	wantEQ(t, label, "outbox 行数", e.st.outbox.count(), eventNo)
	wantEQ(t, label, "事件行是否带事务会话（§5 同事务）", len(e.st.outbox.gotTxID), eventNo)
	row := e.st.outbox.row(eventNo - 1)
	wantEQ(t, label, "event_type 列", row.EventType, model.EventProfileUpdated)
	wantEQ(t, label, "aggregate_id 列", row.AggregateID, itoa(mid))
	wantEQ(t, label, "outbox status", int(row.Status), model.OutboxStatusPending)
	env, pl := decodeOutboxView(t, label, row)
	wantEQ(t, label, "信封 event_type", env.EventType, model.EventProfileUpdated)
	wantEQ(t, label, "信封 schema_version", env.SchemaVersion, 1)
	wantEQ(t, label, "信封 producer", env.Producer, "user-profile")
	wantEQ(t, label, "信封 aggregate_type", env.AggregateType, "user")
	wantEQ(t, label, "信封 aggregate_id", env.AggregateID, itoa(mid))
	if env.EventID == "" {
		t.Errorf("%s：信封 event_id 为空（消费者无法按 event_id 幂等）", label)
	}
	wantEQ(t, label, "payload.action", plStr(t, label, pl, "action"), action)
	wantEQ(t, label, "payload.mid", plInt(t, label, pl, "mid"), mid)
	// 失效范围：只删 bs_<mid>。
	wantEQ(t, label, "缓存删除次数", e.st.log.countPrefix("cache.Del:"), 1)
	if _, ok := e.st.cache.jsons[keyBase(mid)]; ok {
		t.Errorf("%s：%s 之后 bs_%d 仍在缓存里（失效没生效）", label, method, mid)
	}
}
