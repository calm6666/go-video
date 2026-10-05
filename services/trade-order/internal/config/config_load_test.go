package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/zrpc"
)

// releaseYaml 指向随服务发布的配置示例。
// 本测试只解析文件，不连 MySQL / Redis / etcd，也不执行迁移。
const releaseYaml = "../../etc/tradeorder.v1.yaml"

// wantTradeOrder 钉死 etc 示例里的领域参数取值。
// 用字面量而不是引用 default 标签：这些数字是「运维看到并据以调参」的口径，
// 一旦与 config.go 的 `json:",default=…"` 漂移，这个测试就是唯一的拦截点。
var wantTradeOrder = TradeOrderConf{
	MaxQuantityPerOrder:           10,
	OrderExpireSeconds:            1800,
	StuckScanMaxLimit:             200,
	MaxListWindowSeconds:          7776000, // 90 天
	MaxPageSize:                   100,
	FulfillMaxAttempts:            5,
	MinSecondsBetweenFulfillRetry: 30,
	DefaultCurrency:               "CNY",
}

// TestLoadReleaseYaml 校验 etc/tradeorder.v1.yaml 能被完整解析，
// 且服务名/端口与 docs/service-catalog.md 的登记值一致，领域参数逐字段对撞。
func TestLoadReleaseYaml(t *testing.T) {
	var c Config
	if err := conf.Load(releaseYaml, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载 %s 失败: %v", releaseYaml, err)
	}

	if c.Name != "tradeorder.v1.rpc" {
		t.Fatalf("Name = %q, want tradeorder.v1.rpc（etcd 注册键须与 docs/service-catalog.md 一致）", c.Name)
	}
	if c.ListenOn != "0.0.0.0:8162" {
		t.Fatalf("ListenOn = %q, want 0.0.0.0:8162", c.ListenOn)
	}
	if c.Etcd.Key != c.Name {
		t.Fatalf("Etcd.Key = %q 应与 Name = %q 一致（注册键不带连字符，客户端按此发现）", c.Etcd.Key, c.Name)
	}
	if len(c.Etcd.Hosts) == 0 {
		t.Fatal("Etcd.Hosts 为空：服务不会注册，gateway 侧发现不到订单服务")
	}
	// 字段名必须是 CacheRedis：zrpc.RpcServerConf 已内嵌 Redis（限流用），
	// 业务缓存再起名叫 Redis 会让 conf.Load 直接报 "conflict key redis"。
	if c.CacheRedis.Host == "" || c.CacheRedis.Type == "" {
		t.Fatalf("CacheRedis 配置缺失: %+v", c.CacheRedis)
	}
	if !strings.Contains(c.DataSource, "go_video_trade_order") {
		t.Fatalf("DataSource 必须指向本服务自有库 go_video_trade_order, got %q", c.DataSource)
	}
	if !strings.Contains(c.DataSource, "charset=utf8mb4") {
		t.Fatalf("DataSource 必须显式 utf8mb4: %q", c.DataSource)
	}
	// 状态推进是 CAS：UPDATE ... WHERE order_no=? AND state=? AND version=?，
	// 靠 RowsAffected()>0 判断有没有命中。DSN 加 clientFoundRows=true 会让
	// RowsAffected 变成「匹配行数」，并发抢先会被误判成推进成功 —— 直接破坏状态机。
	if strings.Contains(c.DataSource, "clientFoundRows") {
		t.Fatalf("DataSource 不得启用 clientFoundRows，否则 CAS 判定失真: %q", c.DataSource)
	}
	if c.TradeOrder != wantTradeOrder {
		t.Fatalf("TradeOrder 配置解析结果与预期不符:\n got %+v\nwant %+v", c.TradeOrder, wantTradeOrder)
	}
}

// TestDownstreamSectionsDiscoverable 三个下游 RPC 段必须给出可用的发现方式。
//
// 判定式与 svc.rpcConfigured 保持一致（Endpoints / Target / Etcd.Hosts 三选一非空）：
// 示例配置里若把某段写成空壳，svc 启动时就不会构造客户端，
// 取价/受理支付/发放会整条链显式失败（Err*NotConfigured），这是设计行为，
// 但示例配置不该演示这种「起得来但什么都干不了」的状态。
func TestDownstreamSectionsDiscoverable(t *testing.T) {
	var c Config
	if err := conf.Load(releaseYaml, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载 %s 失败: %v", releaseYaml, err)
	}
	cases := []struct {
		name    string
		conf    zrpc.RpcClientConf
		wantKey string
	}{
		{"MembershipRPC", c.MembershipRPC, "membership.v1.rpc"},
		{"PaymentRPC", c.PaymentRPC, "payment.v1.rpc"},
		{"CoinRPC", c.CoinRPC, "coin.v1.rpc"},
	}
	for _, tc := range cases {
		if !clientDiscoverable(tc.conf) {
			t.Errorf("%s 未给出 Endpoints/Target/Etcd.Hosts：示例配置会起一个没有下游能力的实例", tc.name)
			continue
		}
		if len(tc.conf.Endpoints) == 0 && tc.conf.Target == "" && tc.conf.Etcd.Key != tc.wantKey {
			t.Errorf("%s.Etcd.Key = %q, want %q（下游注册键写错等于没配，NonBlock 下只会在调用时报错）",
				tc.name, tc.conf.Etcd.Key, tc.wantKey)
		}
	}
}

// TestMinimalYamlAppliesDefaults 只写必填项时：领域参数全部由 default 标签生效，
// 三个下游段保持零值（svc 据此不构造客户端，而不是连一个不存在的地址）。
func TestMinimalYamlAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tradeorder.min.yaml")
	content := "Name: tradeorder.v1.rpc\nListenOn: 127.0.0.1:8162\n" +
		"CacheRedis:\n  Host: 127.0.0.1:6379\n  Type: node\nDataSource: dsn\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}

	var c Config
	if err := conf.Load(file, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if c.TradeOrder != wantTradeOrder {
		t.Fatalf("TradeOrder 嵌套默认值未生效: %+v", c.TradeOrder)
	}
	if c.MembershipRPC.Etcd.Key != "" || len(c.MembershipRPC.Endpoints) > 0 || c.MembershipRPC.Target != "" ||
		c.PaymentRPC.Etcd.Key != "" || c.CoinRPC.Etcd.Key != "" {
		t.Fatalf("optional 下游段应保持零值（零值=不构造客户端）: membership=%+v payment=%+v coin=%+v",
			c.MembershipRPC, c.PaymentRPC, c.CoinRPC)
	}
	if clientDiscoverable(c.MembershipRPC) || clientDiscoverable(c.PaymentRPC) || clientDiscoverable(c.CoinRPC) {
		t.Fatal("最小配置不应被 svc 判成「下游已接入」")
	}
}

// TestYamlKeysMatchStructFields 钉住「etc yaml 键 ↔ Config 字段」一一对应：
// 多写的键会被静默忽略（运维以为调了退款窗口其实没调），
// 少写的键只能依赖 default 标签，两种漂移都必须在 CI 阶段拦住。
func TestYamlKeysMatchStructFields(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(releaseYaml))
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	top, tradeOrderKeys := parseYamlKeys(string(raw))

	known := structKeys(reflect.TypeOf(Config{}))
	for _, k := range top {
		if _, ok := known[k]; !ok {
			t.Errorf("etc yaml 顶层键 %q 在 Config 里没有对应字段（会被静默忽略）", k)
		}
	}
	for _, k := range []string{"CacheRedis", "DataSource", "TradeOrder",
		"MembershipRPC", "PaymentRPC", "CoinRPC"} {
		if !contains(top, k) {
			t.Errorf("Config 字段 %s 在 etc yaml 里没有对应键，示例配置不完整", k)
		}
	}

	toKnown := structKeys(reflect.TypeOf(TradeOrderConf{}))
	for _, k := range tradeOrderKeys {
		if _, ok := toKnown[k]; !ok {
			t.Errorf("etc yaml TradeOrder.%s 在 TradeOrderConf 里没有对应字段", k)
		}
	}
	for name := range toKnown {
		if !contains(tradeOrderKeys, name) {
			t.Errorf("TradeOrderConf.%s 在 etc yaml 的 TradeOrder 段里没有示例值（只能依赖 default 标签）", name)
		}
	}
}

// clientDiscoverable 与 svc.rpcConfigured 同判定式，这里复制一份而不跨包引用：
// svc 依赖 config，反向 import 会成环。
func clientDiscoverable(c zrpc.RpcClientConf) bool {
	return len(c.Endpoints) > 0 || c.Target != "" || len(c.Etcd.Hosts) > 0
}

// structKeys 收集结构体（含匿名内嵌）的字段名，用于和 yaml 键比对。
func structKeys(typ reflect.Type) map[string]struct{} {
	out := make(map[string]struct{})
	var walk func(reflect.Type, int)
	walk = func(t reflect.Type, depth int) {
		if t == nil || t.Kind() != reflect.Struct || depth > 3 {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				walk(f.Type, depth+1)
				continue
			}
			name := f.Name
			if tag := f.Tag.Get("json"); tag != "" {
				if first := strings.Split(tag, ",")[0]; first != "" && first != "-" {
					name = first
				}
			}
			out[name] = struct{}{}
			out[f.Name] = struct{}{}
		}
	}
	walk(typ, 0)
	return out
}

// parseYamlKeys 最小小心智解析：返回顶层键与 TradeOrder 段下的键。
// 不引入 yaml 库（go.mod 里它是 indirect，为写测试动依赖不划算），
// 因此只认本仓约定的两空格缩进与 `Key: value` / `Key:` 形态。
func parseYamlKeys(raw string) (top []string, tradeOrder []string) {
	inTradeOrder := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key := strings.SplitN(trimmed, ":", 2)[0]
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
			inTradeOrder = key == "TradeOrder"
			top = append(top, key)
			continue
		}
		if inTradeOrder && !strings.HasPrefix(trimmed, "-") {
			tradeOrder = append(tradeOrder, key)
		}
	}
	return top, tradeOrder
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
