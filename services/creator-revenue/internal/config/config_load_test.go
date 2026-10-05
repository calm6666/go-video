package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// TestExampleConfigsLoad 用真实的 conf.Load 加载 etc/ 下每个示例配置。
//
// 本服务踩过两类只有真实加载才能暴露的问题：
//  1. 业务缓存字段命名为 Redis —— 与 zrpc.RpcServerConf 内嵌的 RedisKeyConf 同名，
//     代码可编译，但 conf.MustLoad 在启动瞬间报 "conflict key redis"；
//  2. yaml 写了 Config 里没有的键（被静默忽略），或 Config 有的键 yaml 漏写
//     （被 default tag 悄悄补上）——护栏值配丢了不会有人发现，只会在出事故那天显形。
//
// 因此本用例不允许退化成「只 reflect 结构体」。
func TestExampleConfigsLoad(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatal("etc 目录下没有示例配置")
	}
	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			var c Config
			if err := conf.Load(f, &c); err != nil {
				t.Fatalf("加载 %s 失败: %v", f, err)
			}
			checkFields(t, reflect.ValueOf(c))
			checkServiceIdentity(t, c)
			checkCreatorRevenueBounds(t, c)
			checkYamlKeysMatchStruct(t, f)

			// 示例配置里的每个业务参数都必须等于代码默认值：
			// 不相等说明「yaml 少写了键、被默认值悄悄补上」，示例配置就失去了暴露真实取值的作用。
			if got, want := c.CreatorRevenue, exampleCreatorRevenueConf(); !reflect.DeepEqual(got, want) {
				t.Errorf("示例配置的 CreatorRevenue 与代码默认值不一致（多半是 yaml 漏写某个键）:\n got=%+v\nwant=%+v",
					got, want)
			}
		})
	}
}

// TestDefaultsMatchExampleConfig 反向锁死同一件事：
// 去掉 CreatorRevenue 段的最小配置，加载后必须得到与示例配置完全相同的领域参数，
// 这样「示例配置里写的值」和「结构体 tag 里的 default」不可能各自漂移。
func TestDefaultsMatchExampleConfig(t *testing.T) {
	minimal := `Name: creatorrevenue.v1.rpc
ListenOn: 0.0.0.0:8164
Etcd:
  Hosts:
  - 127.0.0.1:2379
  Key: creatorrevenue.v1.rpc
CacheRedis:
  Host: 127.0.0.1:6379
  Type: node
DataSource: root:root@tcp(127.0.0.1:3306)/go_video_creator_revenue?charset=utf8mb4&parseTime=true&loc=Local
`
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatalf("写入最小配置失败: %v", err)
	}
	var c Config
	if err := conf.Load(path, &c); err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if got, want := c.CreatorRevenue, exampleCreatorRevenueConf(); !reflect.DeepEqual(got, want) {
		t.Errorf("默认值与示例配置不一致:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestCacheFieldMustNotBeNamedRedis 锁死本仓的命名约束：
// 业务缓存字段必须叫 CacheRedis。字段名叫 Redis 时代码照样编译，
// 但 conf.Load 会报 "conflict key redis"（zrpc.RpcServerConf 内嵌了 RedisKeyConf），
// 属于「测试环境看不到、线上启动才炸」的那一类。
func TestCacheFieldMustNotBeNamedRedis(t *testing.T) {
	tp := reflect.TypeOf(Config{})
	// 只看本结构体直接声明的字段：FieldByName 会把 zrpc.RpcServerConf 内嵌的
	// RedisKeyConf（字段名就是 Redis）也算成提升字段，那不是我们的错。
	if declaredFieldNames(tp)["Redis"] {
		t.Fatal("Config 里出现了字段 Redis，会与 zrpc.RpcServerConf 的 Redis 键冲突，必须改名 CacheRedis")
	}
	if !declaredFieldNames(tp)["CacheRedis"] {
		t.Fatal("Config 缺少 CacheRedis 字段：概览/规则读路径的加速位无处配置")
	}
	if f, _ := tp.FieldByName("CacheRedis"); f.Type != reflect.TypeOf(redis.RedisConf{}) {
		t.Fatalf("CacheRedis 类型 = %s，期望 redis.RedisConf", f.Type)
	}
}

// exampleCreatorRevenueConf 是示例配置应当表达的完整取值（同时是代码默认值）。
func exampleCreatorRevenueConf() CreatorRevenueConf {
	return CreatorRevenueConf{
		DefaultCurrency:              "CNY",
		MaxPageSize:                  100,
		MaxRuleUnitPricePer1000Minor: 1000000,
		MaxMonthlyCapMinor:           50000000,
		GenerateMaxBatch:             500,
		SummaryRecentPeriods:         12,
	}
}

// checkServiceIdentity 锁定服务名与 etcd 注册键：
// 网关与其它商业化服务按 creatorrevenue.v1.rpc 寻址，
// 改成带连字符的值不会让启动失败，只会让「服务发现永远为空、调用超时」。
func checkServiceIdentity(t *testing.T, c Config) {
	t.Helper()
	if c.Name != "creatorrevenue.v1.rpc" {
		t.Errorf("Name=%q，必须与 etcd 注册键一致", c.Name)
	}
	if c.Etcd.Key != "creatorrevenue.v1.rpc" {
		t.Errorf("Etcd.Key=%q，下游按 creatorrevenue.v1.rpc 寻址，不可随意改", c.Etcd.Key)
	}
	if strings.Contains(c.Etcd.Key, "-") {
		t.Errorf("Etcd.Key=%q 含连字符：本仓 zrpc 注册键统一用点号分段（如 creatorrevenue.v1.rpc）", c.Etcd.Key)
	}
	if len(c.Etcd.Hosts) == 0 {
		t.Error("Etcd.Hosts 为空，服务注册不上去，下游只能静态 Endpoints 兜底")
	}
	// 8164 是创作者分成域在本仓端口表里分配到的槽位（8158 起为阶段 4 的商业化服务）。
	if c.ListenOn != "0.0.0.0:8164" {
		t.Errorf("ListenOn=%q，期望 0.0.0.0:8164；改成 127.0.0.1 会让容器内其他服务连不上", c.ListenOn)
	}
}

// checkCreatorRevenueBounds 校验分成域参数之间的边界关系。
// 这些值直接决定「天价单价能不能写进规则」「一批出单锁多少行」「概览回看几个月」，
// 配成 0 或负数等于关掉防线，所以每个都要独立断言而不是只看非空。
func checkCreatorRevenueBounds(t *testing.T, c Config) {
	t.Helper()
	cr := c.CreatorRevenue

	if cr.MaxPageSize <= 0 {
		t.Errorf("MaxPageSize=%d 必须 > 0：规则/名单/台账/结算单四类列表都靠它收口", cr.MaxPageSize)
	}
	// 单价护栏是「手滑写成天价」的唯一拦点：配成 0 会让所有正单价被拒（计划直接不可用），
	// 配成负数同理；配得极大则等于没有护栏。1 亿分/千单位（¥100 万）之上视为失控。
	if cr.MaxRuleUnitPricePer1000Minor <= 0 || cr.MaxRuleUnitPricePer1000Minor > 100000000 {
		t.Errorf("MaxRuleUnitPricePer1000Minor=%d 必须落在 1..100000000", cr.MaxRuleUnitPricePer1000Minor)
	}
	// 封顶护栏允许 0（本仓语义：0 表示不限额），负数会让所有规则无法通过校验。
	if cr.MaxMonthlyCapMinor < 0 {
		t.Errorf("MaxMonthlyCapMinor=%d 不能为负（0 表示不限封顶）", cr.MaxMonthlyCapMinor)
	}
	if cr.MaxMonthlyCapMinor > 0 && cr.MaxMonthlyCapMinor < cr.MaxRuleUnitPricePer1000Minor {
		t.Errorf("MaxMonthlyCapMinor=%d 小于单价护栏 %d：封顶比千单位单价还低，规则将永远无法配出可用组合",
			cr.MaxMonthlyCapMinor, cr.MaxRuleUnitPricePer1000Minor)
	}
	if cr.GenerateMaxBatch <= 0 {
		t.Errorf("GenerateMaxBatch=%d 必须 > 0：0/负数会让全量出单退化成无界批量", cr.GenerateMaxBatch)
	}
	// 0 是合法值（全历史口径），负数会让 PeriodCutoff 直接报错。
	if cr.SummaryRecentPeriods < 0 {
		t.Errorf("SummaryRecentPeriods=%d 不能为负（0 表示已确认合计回看全历史）", cr.SummaryRecentPeriods)
	}
	if cr.DefaultCurrency == "" {
		t.Error("DefaultCurrency 为空：出单时无法确定记账币种（logic 会显式报 ErrRuleCurrencyMismatch）")
	}
	// 本轮只支持单一币种；扩币种要连规则写侧与出单聚合的跨币种判定一起改。
	if cr.DefaultCurrency != "CNY" {
		t.Errorf("DefaultCurrency=%q，本轮台账只接受 CNY", cr.DefaultCurrency)
	}

	if !strings.Contains(c.DataSource, "go_video_creator_revenue") {
		t.Errorf("DataSource 必须指向本服务自有库 go_video_creator_revenue，当前=%s", c.DataSource)
	}
	for _, forbidden := range []string{"sk-", "AKID", "-----BEGIN", "vault://"} {
		if strings.Contains(strings.ToLower(c.DataSource), strings.ToLower(forbidden)) {
			t.Errorf("DataSource 疑似包含真实凭据（命中 %q），生产密钥只能由 Secret/Vault 注入", forbidden)
		}
	}
	own := declaredFieldNames(reflect.TypeOf(Config{}))
	// 出金不在范围（AGENTS.md §1）：本服务不持有任何支付渠道、银行、票据与对账配置。
	for _, name := range []string{
		"PaymentChannel", "Alipay", "WechatPay", "ApplePay", "Bank", "Withdraw",
		"Invoice", "Tax", "PayoutChannel", "PrivateKey", "CertPath", "Webhook",
	} {
		if own[name] {
			t.Errorf("Config 出现字段 %s：创作者分成域不持有任何出金/渠道配置，出金能力刻意不实现", name)
		}
	}
	// 本轮不发事件、不接 MQ：加了 producer 配置却没人发，等于给下游一个永不来的承诺。
	for _, name := range []string{"Kafka", "RocketMQ", "Mq", "MQ", "Pulsar", "Outbox"} {
		if own[name] {
			t.Errorf("Config 出现字段 %s：本轮 creator-revenue 不接事件总线，需先补契约与 README 再启用", name)
		}
	}
}

// declaredFieldNames 返回结构体「自己声明」的字段名集合。
// 不用 reflect.Type.FieldByName：它会把内嵌结构体的字段也算成提升字段，
// 于是 zrpc.RpcServerConf 自带的 Redis / CertFile 会被误判成本服务加的字段。
func declaredFieldNames(tp reflect.Type) map[string]bool {
	set := make(map[string]bool, tp.NumField())
	for i := 0; i < tp.NumField(); i++ {
		if f := tp.Field(i); !f.Anonymous && f.IsExported() {
			set[f.Name] = true
		}
	}
	return set
}

// checkYamlKeysMatchStruct 双向核对 etc yaml 的顶层键与 Config 字段：
// go-zero 对未识别的键不保证报错，写错一个字母（CacheRedix）就会静默丢掉整段配置，
// 因此这里自己比对，不依赖 conf.Load 的严格程度。
func checkYamlKeysMatchStruct(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	declared := configKeySet(reflect.TypeOf(Config{}))
	seen := map[string]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' || line[0] == '-' {
			continue // 只看顶层键：缩进行属于某个已识别的父键
		}
		key, _, found := strings.Cut(line, ":")
		if !found {
			t.Errorf("%s:%d 不是合法的顶层键值行: %q", path, i+1, line)
			continue
		}
		key = strings.TrimSpace(key)
		seen[key] = true
		if _, ok := declared[key]; !ok {
			t.Errorf("%s 顶层键 %q 在 Config 里没有对应字段，会被 conf.Load 静默忽略", path, key)
		}
	}
	// 反向：Config 的每个非内嵌业务字段都必须在示例配置里出现，
	// 漏写意味着该字段依赖 default tag，示例配置就失去暴露真实取值的作用。
	for _, name := range []string{"CacheRedis", "DataSource", "CreatorRevenue"} {
		if !seen[name] {
			t.Errorf("%s 缺少顶层键 %s：示例配置必须写全，不能让 default tag 悄悄兜底", path, name)
		}
	}
	// CreatorRevenue 段内每个字段都要显式写出来（与 default tag 逐个对照由上面的 DeepEqual 负责）。
	if seen["CreatorRevenue"] {
		checkNestedKeys(t, path, raw, "CreatorRevenue", reflect.TypeOf(CreatorRevenueConf{}))
	}
}

// checkNestedKeys 校验某一段下面的所有键都能在结构体里找到，且结构体字段没有漏写。
func checkNestedKeys(t *testing.T, path string, raw []byte, section string, tp reflect.Type) {
	t.Helper()
	inSection := false
	declared := map[string]bool{}
	for i := tp.NumField() - 1; i >= 0; i-- {
		declared[tp.Field(i).Name] = false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.HasPrefix(line, section+":") {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			inSection = false // 段结束
			continue
		}
		key, _, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if _, ok := declared[key]; !ok {
			t.Errorf("%s 的 %s.%s 在 %s 里没有对应字段，会被 conf.Load 静默忽略", path, section, key, tp.Name())
			continue
		}
		declared[key] = true
	}
	for key, hit := range declared {
		if !hit {
			t.Errorf("%s 的 %s 段漏写键 %s（当前依赖 default tag）", path, section, key)
		}
	}
}

// configKeySet 收集 conf.Load 视角下合法的顶层键：本结构体字段 + 内嵌结构体（含其内嵌）字段。
func configKeySet(tp reflect.Type) map[string]bool {
	set := map[string]bool{}
	collectKeys(tp, set)
	return set
}

func collectKeys(tp reflect.Type, set map[string]bool) {
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			collectKeys(f.Type, set)
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = f.Name
		}
		set[name] = true
		// go-zero 也接受字段名本身（大小写不敏感匹配），两种写法都放行。
		set[f.Name] = true
	}
}

var redisConfType = reflect.TypeOf(redis.RedisConf{})

// checkFields 递归检查配置：缓存宿主必填、库名必须是本服务自有 schema、示例配置不得含明文 Token。
func checkFields(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		sf := v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		f := v.Field(i)
		// 内嵌字段都是 go-zero 框架配置（RpcServerConf/ServiceConf/LogConf），
		// 其内部可选结构（如 zrpc 鉴权用的 RedisKeyInfo）留空是正常的，不检查。
		if sf.Anonymous {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			if strings.HasSuffix(sf.Name, "DataSource") && f.String() == "" {
				t.Errorf("%s 为空，服务启动后无法访问数据库（写接口会一律回 ErrDBNotConfigured）", sf.Name)
			}
			if sf.Name == "Token" && f.String() != "" {
				t.Errorf("%s 不应在示例配置里写入明文 Token", sf.Name)
			}
		case reflect.Struct:
			if f.Type() == redisConfType {
				rc := f.Interface().(redis.RedisConf)
				if rc.Host == "" {
					t.Errorf("%s.Host 为空，缓存客户端无法构造", sf.Name)
				}
				if rc.Type != "node" && rc.Type != "cluster" {
					t.Errorf("%s.Type=%q，只支持 node/cluster；写错值 MustNewRedis 会直接 panic", sf.Name, rc.Type)
				}
				continue
			}
			checkFields(t, f)
		case reflect.Slice:
			for j := 0; j < f.Len(); j++ {
				if f.Index(j).Kind() == reflect.String && f.Index(j).String() == "" {
					t.Errorf("%s[%d] 为空字符串，配置里不应保留空元素", sf.Name, j)
				}
			}
		}
	}
}
