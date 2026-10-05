package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// TestLoadReleaseYaml 校验 etc/payment.v1.yaml 能被完整解析，
// 端口/服务名/Etcd 键与部署登记值一致，且每个 Config 字段都在 etc 里被显式赋值
// （字段有默认值时，漏配会被静默填成默认值，这里靠逐字段断言把「yaml 与结构体一一对应」钉住）。
// 本测试只解析配置，不连接 MySQL/Redis/etcd。
func TestLoadReleaseYaml(t *testing.T) {
	var c Config
	path := filepath.Join("..", "..", "etc", "payment.v1.yaml")
	if err := conf.Load(path, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载 %s 失败: %v", path, err)
	}

	if c.Name != "payment.v1.rpc" {
		t.Fatalf("Name = %q, want payment.v1.rpc（etcd 注册键须与 docs/service-catalog.md 一致）", c.Name)
	}
	if c.ListenOn != "0.0.0.0:8161" {
		t.Fatalf("ListenOn = %q, want 0.0.0.0:8161", c.ListenOn)
	}
	if c.Etcd.Key != c.Name {
		t.Fatalf("Etcd.Key = %q 应与 Name = %q 一致（不得带连字符）", c.Etcd.Key, c.Name)
	}
	if c.CacheRedis.Host == "" || c.CacheRedis.Type == "" {
		t.Fatalf("CacheRedis 配置缺失: %+v", c.CacheRedis)
	}
	if c.DataSource == "" {
		t.Fatal("DataSource 不能为空")
	}
	if !strings.Contains(c.DataSource, "go_video_payment") {
		t.Fatalf("DataSource 应指向 go_video_payment 库: %q", c.DataSource)
	}

	want := PaymentConf{
		DefaultCurrency:      "CNY",
		MinRechargeMinor:     1,
		MaxRechargeMinor:     200000,
		MaxAdjustMinor:       1000000,
		MaxPageSize:          100,
		MaxListWindowSeconds: 2592000,
		MaxListOffset:        10000,
	}
	if c.Payment.DefaultCurrency != want.DefaultCurrency ||
		c.Payment.MinRechargeMinor != want.MinRechargeMinor ||
		c.Payment.MaxRechargeMinor != want.MaxRechargeMinor ||
		c.Payment.MaxAdjustMinor != want.MaxAdjustMinor ||
		c.Payment.MaxPageSize != want.MaxPageSize ||
		c.Payment.MaxListWindowSeconds != want.MaxListWindowSeconds ||
		c.Payment.MaxListOffset != want.MaxListOffset {
		t.Fatalf("Payment 配置解析结果与预期不符:\n got %+v\nwant %+v", c.Payment, want)
	}
	// 渠道白名单是沙箱语义的开关：示例配置必须只放 SANDBOX。
	if !slices.Equal(c.Payment.AllowedChannels, []string{ChannelSandbox}) {
		t.Fatalf("Payment.AllowedChannels = %v, want [%s]", c.Payment.AllowedChannels, ChannelSandbox)
	}
	if !c.Payment.AllowsChannel(ChannelSandbox) {
		t.Fatal("AllowedChannels=[SANDBOX] 时沙箱渠道必须放行")
	}
	if c.Payment.AllowsChannel("WECHAT") {
		t.Fatal("未列出的渠道不得被放行")
	}

	assertYamlKeysMatchStruct(t, path, reflect.TypeOf(c))
}

// TestMinimalYamlAppliesDefaults 只写必填项时，Payment 段的 default 标签必须生效，
// 避免运维漏配导致金额上限为 0 这类隐性故障（0 上限会把所有充值判成超限）。
func TestMinimalYamlAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "payment.min.yaml")
	content := strings.Join([]string{
		"Name: payment.v1.rpc",
		"ListenOn: 127.0.0.1:8161",
		"CacheRedis:",
		"  Host: 127.0.0.1:6379",
		"  Type: node",
		"DataSource: dsn",
	}, "\n") + "\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}

	var c Config
	if err := conf.Load(file, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if c.Payment.DefaultCurrency != "CNY" || c.Payment.MinRechargeMinor != 1 || c.Payment.MaxRechargeMinor != 200000 {
		t.Fatalf("Payment 默认值未生效: %+v", c.Payment)
	}
	if c.Payment.MaxAdjustMinor != 1000000 || c.Payment.MaxPageSize != 100 ||
		c.Payment.MaxListWindowSeconds != 2592000 || c.Payment.MaxListOffset != 10000 {
		t.Fatalf("Payment 默认值未生效: %+v", c.Payment)
	}
	if len(c.Payment.AllowedChannels) != 0 {
		t.Fatalf("AllowedChannels 未配置时应为空: %v", c.Payment.AllowedChannels)
	}
	// 空列表按「只放行 SANDBOX」的默认口径处理，且仍然拒绝任何别的渠道名。
	if !c.Payment.AllowsChannel(ChannelSandbox) || c.Payment.AllowsChannel("") || c.Payment.AllowsChannel("ALIPAY") {
		t.Fatalf("空 AllowedChannels 的默认口径应为只放行 SANDBOX: %+v", c.Payment.AllowedChannels)
	}
	if !c.Payment.RechargeAmountAllowed(1) || !c.Payment.RechargeAmountAllowed(200000) ||
		c.Payment.RechargeAmountAllowed(0) || c.Payment.RechargeAmountAllowed(200001) {
		t.Fatal("充值区间应按 [MinRechargeMinor, MaxRechargeMinor] 判定")
	}
	if c.Payment.AdjustAmountAllowed(0) || !c.Payment.AdjustAmountAllowed(-1000000) ||
		c.Payment.AdjustAmountAllowed(1000001) {
		t.Fatal("调整幅度应按绝对值上限判定，且 0 一律拒绝")
	}
	if c.Payment.NormalizeCurrency(" cny ") != "CNY" || c.Payment.NormalizeCurrency("USD") != "USD" {
		t.Fatal("NormalizeCurrency 应只做归一化，放行判定在 logic 侧")
	}
}

// assertYamlKeysMatchStruct 做「yaml 键 ↔ 结构体字段」双向核对（不引入 yaml 依赖，
// 直接按缩进解析键名）：
//  1. etc 里出现的每个键都必须能被 Config（含内嵌结构体）消费，拼错的键会被
//     go-zero 静默忽略，只靠解析断言发现不了；
//  2. Payment 段的键必须与 PaymentConf 字段完全一致，新增字段必须同时补 etc。
func assertYamlKeysMatchStruct(t *testing.T, path string, typ reflect.Type) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("读取 etc yaml 失败: %v", err)
	}
	top, sections := parseYamlKeys(string(raw))

	consumable := structKeys(typ)
	for _, key := range top {
		if !consumable[key] {
			t.Errorf("etc yaml 顶层键 %q 在 Config 中没有对应字段（拼写错误会被 go-zero 静默忽略）", key)
		}
	}

	paymentKeys := sections["Payment"]
	if len(paymentKeys) == 0 {
		t.Fatal("etc yaml 缺少 Payment 段")
	}
	wantPayment := structKeys(reflect.TypeOf(PaymentConf{}))
	seen := map[string]bool{}
	for _, key := range paymentKeys {
		seen[key] = true
		if !wantPayment[key] {
			t.Errorf("Payment.%s 在 PaymentConf 中没有对应字段", key)
		}
	}
	for field := range wantPayment {
		if !seen[field] {
			t.Errorf("PaymentConf 字段 %s 在 etc yaml 的 Payment 段里没有显式配置", field)
		}
	}
}

// parseYamlKeys 按缩进粗粒度解析「顶层键」与「各段下的键」，
// 只识别键名、不解析值，因此不需要引入 yaml 依赖。
func parseYamlKeys(content string) ([]string, map[string][]string) {
	var top []string
	sections := map[string][]string{}
	current := ""

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line == strings.TrimLeft(line, " ") {
			key, ok := yamlKeyOf(line)
			if !ok {
				continue
			}
			top = append(top, key)
			current = key
			continue
		}
		if current == "" {
			continue
		}
		if key, ok := yamlKeyOf(line); ok {
			sections[current] = append(sections[current], key)
		}
	}
	return top, sections
}

// yamlKeyOf 取一行里的键名（`Key: value` 或 `  Key:`），列表项 `- x` 不是键。
func yamlKeyOf(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	idx := strings.Index(trimmed, ":")
	if idx <= 0 {
		return "", false
	}
	return strings.TrimSpace(trimmed[:idx]), true
}

// structKeys 列出可被 conf.Load 消费的键名：优先 json tag，内嵌结构体递归展开。
func structKeys(typ reflect.Type) map[string]bool {
	keys := map[string]bool{}
	if typ.Kind() != reflect.Struct {
		return keys
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Anonymous {
			base := field.Type
			if base.Kind() == reflect.Ptr {
				base = base.Elem()
			}
			for key := range structKeys(base) {
				keys[key] = true
			}
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == ",optional" {
			name = field.Name
		}
		keys[name] = true
	}
	return keys
}
