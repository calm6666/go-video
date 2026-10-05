package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// releaseYaml 指向随服务发布的配置示例（本测试只解析，不连接 MySQL/Redis/etcd）。
const releaseYaml = "../../etc/coin.v1.yaml"

// TestLoadReleaseYaml 校验 etc/coin.v1.yaml 能被完整解析，且服务名/端口与
// docs/service-catalog.md 登记值一致，领域参数逐字段钉死。
//
// 期望值直接引用 Sanitize 的兜底常量：这样「etc 里写的」与「配置被写成 0/负数时
// 服务端实际生效的」必须是同一套数字，不会出现文档说 10、兜底给 20 的漂移。
func TestLoadReleaseYaml(t *testing.T) {
	var c Config
	if err := conf.Load(releaseYaml, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载 %s 失败: %v", releaseYaml, err)
	}

	if c.Name != "coin.v1.rpc" {
		t.Fatalf("Name = %q, want coin.v1.rpc（etcd 注册键须与 docs/service-catalog.md 一致）", c.Name)
	}
	if c.ListenOn != "0.0.0.0:8163" {
		t.Fatalf("ListenOn = %q, want 0.0.0.0:8163", c.ListenOn)
	}
	if c.Etcd.Key != c.Name {
		t.Fatalf("Etcd.Key = %q 应与 Name = %q 一致（注册键不带连字符，客户端按此发现）", c.Etcd.Key, c.Name)
	}
	if c.CacheRedis.Host == "" || c.CacheRedis.Type == "" {
		t.Fatalf("CacheRedis 配置缺失: %+v", c.CacheRedis)
	}
	if !strings.Contains(c.DataSource, "go_video_coin") {
		t.Fatalf("DataSource 必须指向本服务自有库 go_video_coin, got %q", c.DataSource)
	}
	// 幂等与余额判定依赖「affected = changed rows」：加上 clientFoundRows=true
	// 会让 RowsAffected 变成匹配行数，余额不足与重放判定全部失真（迁移 SQL 已记为破坏性变更）。
	if strings.Contains(c.DataSource, "clientFoundRows") {
		t.Fatalf("DataSource 不得启用 clientFoundRows，否则条件更新的 RowsAffected 判定失真: %q", c.DataSource)
	}
	if !strings.Contains(c.DataSource, "charset=utf8mb4") {
		t.Fatalf("DataSource 必须显式 utf8mb4: %q", c.DataSource)
	}

	want := CoinConf{
		InitialBalance:               fallbackInitialBalance,
		DailyLimit:                   fallbackDailyLimit,
		PerTargetLimit:               fallbackPerTargetLimit,
		CancelWindowSeconds:          fallbackCancelWindowSeconds,
		MinBalanceToToss:             fallbackMinBalanceToToss,
		MaxGrantDelta:                fallbackMaxGrantDelta,
		DefaultPageSize:              fallbackDefaultPageSize,
		MaxPageSize:                  fallbackMaxPageSize,
		MaxBatchAids:                 fallbackMaxBatchAids,
		TargetSummaryCacheTTLSeconds: 10,
	}
	if c.Coin != want {
		t.Fatalf("Coin 配置解析结果与预期不符:\n got %+v\nwant %+v", c.Coin, want)
	}
}

// TestYamlKeysMatchStructFields 钉住「etc yaml 键 ↔ Config 字段」一一对应：
// 多写的键 conf.Load 会静默忽略（运维以为改了限额其实没改），
// 少写的字段则靠 default 标签兜底，两种漂移都必须在 CI 阶段拦住。
func TestYamlKeysMatchStructFields(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(releaseYaml))
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	top, coinKeys := parseYamlKeys(string(raw))

	known := structKeys(reflect.TypeOf(Config{}))
	for _, k := range top {
		if _, ok := known[k]; !ok {
			t.Errorf("etc yaml 顶层键 %q 在 Config 里没有对应字段（conf.Load 会静默忽略该键）", k)
		}
	}
	// 本服务自己声明的必填段必须真的出现在 yaml 里，否则等于没提供示例值。
	for _, k := range []string{"CacheRedis", "DataSource", "Coin"} {
		if !contains(top, k) {
			t.Errorf("Config 字段 %s 在 etc yaml 里没有对应键，示例配置不完整", k)
		}
	}

	coinKnown := structKeys(reflect.TypeOf(CoinConf{}))
	for _, k := range coinKeys {
		if _, ok := coinKnown[k]; !ok {
			t.Errorf("etc yaml Coin.%s 在 CoinConf 里没有对应字段", k)
		}
	}
	for name := range coinKnown {
		if !contains(coinKeys, name) {
			t.Errorf("CoinConf.%s 在 etc yaml 的 Coin 段里没有示例值（只能依赖 default 标签，运维看不到生效口径）", name)
		}
	}
}

// TestMinimalYamlAppliesDefaults 只写必填项时，CoinConf 的 default 标签必须全部生效，
// 避免运维漏配导致「日限 0 枚」这类隐性故障（svc 侧 Sanitize 还有一道兜底，两边都得对）。
func TestMinimalYamlAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "coin.min.yaml")
	content := "Name: coin.v1.rpc\nListenOn: 127.0.0.1:8163\n" +
		"CacheRedis:\n  Host: 127.0.0.1:6379\n  Type: node\nDataSource: dsn\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}

	var c Config
	if err := conf.Load(file, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if c.Coin.DailyLimit != fallbackDailyLimit || c.Coin.PerTargetLimit != fallbackPerTargetLimit ||
		c.Coin.CancelWindowSeconds != fallbackCancelWindowSeconds ||
		c.Coin.InitialBalance != fallbackInitialBalance || c.Coin.MaxBatchAids != fallbackMaxBatchAids {
		t.Fatalf("Coin 嵌套默认值未生效: %+v", c.Coin)
	}
	if c.Coin.DefaultPageSize > c.Coin.MaxPageSize {
		t.Fatalf("默认分页不应大于上限: %+v", c.Coin)
	}
}

// TestSanitizeRejectsUnusableLimits 限额被配成非正数时必须收敛到可用值，
// 否则「tossed + delta <= 0」恒假会让所有用户都投不出币。
func TestSanitizeRejectsUnusableLimits(t *testing.T) {
	c := CoinConf{
		InitialBalance:               -5,
		DailyLimit:                   0,
		PerTargetLimit:               -1,
		CancelWindowSeconds:          0,
		MinBalanceToToss:             0,
		MaxGrantDelta:                -1,
		DefaultPageSize:              500,
		MaxPageSize:                  0,
		MaxBatchAids:                 0,
		TargetSummaryCacheTTLSeconds: -1,
	}
	notes := c.Sanitize()
	if len(notes) == 0 {
		t.Fatal("非法限额必须给出纠正说明，否则运维无从得知生效值被改过")
	}
	if c.InitialBalance != 0 || c.DailyLimit != fallbackDailyLimit || c.PerTargetLimit != fallbackPerTargetLimit ||
		c.CancelWindowSeconds != fallbackCancelWindowSeconds || c.MinBalanceToToss != fallbackMinBalanceToToss ||
		c.MaxGrantDelta != fallbackMaxGrantDelta || c.MaxPageSize != fallbackMaxPageSize ||
		c.DefaultPageSize != fallbackMaxPageSize || c.MaxBatchAids != fallbackMaxBatchAids ||
		c.TargetSummaryCacheTTLSeconds != 0 {
		t.Fatalf("Sanitize 收敛结果不符: %+v", c)
	}
}

// structKeys 递归收集结构体（含匿名内嵌）的字段名，用于和 yaml 键比对。
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

// parseYamlKeys 用最小心智解析示例配置：返回顶层键与 Coin 段下的键。
// 不引入 yaml 库（go.mod 里它是 indirect，为写测试而动依赖不划算），
// 因此只认本仓约定的两空格缩进与 `Key: value` / `Key:` 形态。
func parseYamlKeys(raw string) (top []string, coin []string) {
	inCoin := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key := strings.SplitN(trimmed, ":", 2)[0]
		if !strings.HasPrefix(line, " ") {
			inCoin = key == "Coin"
			top = append(top, key)
			continue
		}
		if inCoin && !strings.HasPrefix(trimmed, "-") {
			coin = append(coin, key)
		}
	}
	return top, coin
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
