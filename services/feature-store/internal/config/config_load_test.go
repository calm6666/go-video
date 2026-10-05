package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/feature-store/model"
)

// TestExampleConfigsLoad 校验 etc 下的示例配置真的能被 go-zero 加载并通过启动期自检。
//
// 本仓库出现过 Config 自带的 Redis 字段与 zrpc.RpcServerConf 内嵌的
// redis.RedisKeyConf 同名（键都是 redis），代码可编译但启动即报 "conflict key redis"。
// 只有真的跑一遍 conf.Load 才能抓到这类错误，所以这里不调任何 mock。
// 注意：conf 包没有 LoadFile，只有 Load / LoadFromYamlBytes。
func TestExampleConfigsLoad(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("etc 目录下没有示例配置: err=%v files=%v", err, files)
	}
	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			var c Config
			if err := conf.Load(f, &c); err != nil {
				t.Fatalf("加载 %s 失败: %v", f, err)
			}
			assertNoRedisKeyCollision(t, reflect.ValueOf(c))
			if err := c.Validate(); err != nil {
				t.Fatalf("示例配置 %s 未通过 Validate: %v", filepath.Base(f), err)
			}
		})
	}
}

// TestDataSourcePointsAtOwnSchema 锁定数据所有权：DSN 必须指向本服务独占的 schema，
// 连到别人的库要到第一次写入才暴露，那时已经污染了别人的数据（AGENTS.md §5）。
func TestDataSourcePointsAtOwnSchema(t *testing.T) {
	var c Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "featurestore.v1.yaml"), &c); err != nil {
		t.Fatalf("加载 etc/featurestore.v1.yaml 失败: %v", err)
	}
	if !strings.Contains(c.DataSource, DatabaseName) {
		t.Fatalf("DataSource 必须指向 %s 库，实得 %q", DatabaseName, c.DataSource)
	}
	for _, other := range []string{"go_video_spm", "go_video_recommend", "go_video_user_profile"} {
		if strings.Contains(c.DataSource, other) {
			t.Fatalf("DataSource 指向了其他服务的库 %s: %q", other, c.DataSource)
		}
	}
}

// TestExampleConfigsCarryNoSecrets 保证示例配置不含任何密钥材料（AGENTS.md §4）。
// DSN 形如 user:password@tcp(...)，这里检查 ':' 与 '@' 之间必须为空。
func TestExampleConfigsCarryNoSecrets(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("etc 目录下没有示例配置: err=%v", err)
	}
	for _, f := range files {
		var c Config
		if err := conf.Load(f, &c); err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		if pass := dsnPassword(c.DataSource); pass != "" {
			t.Errorf("%s 的 DataSource 含明文密码：示例配置必须留空，真实密钥走 Secret/Vault",
				filepath.Base(f))
		}
		if c.CacheRedis.Pass != "" {
			t.Errorf("%s 的 CacheRedis.Pass 含密钥材料：示例配置必须留空", filepath.Base(f))
		}
	}
}

// TestListenOnIsUniqueAmongBatchPeers 锁住端口：本服务的 ListenOn 不能与已分配端口重叠，
// 尤其不能留在 goctl 默认的 0.0.0.0:8080（那是 gateway 的 HTTP 端口）。
func TestListenOnIsUniqueAmongBatchPeers(t *testing.T) {
	var c Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "featurestore.v1.yaml"), &c); err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}
	taken := map[string]string{
		"0.0.0.0:8080": "gateway/app 与 gateway/admin 的 HTTP 端口",
		"0.0.0.0:8116": "recommend-recall",
		"0.0.0.0:8118": "live-ingest",
		"0.0.0.0:8119": "live-room",
		"0.0.0.0:8120": "live-media",
		"0.0.0.0:8121": "live-gateway",
		"0.0.0.0:8123": "recommend-rank",
		"0.0.0.0:8150": "private-message",
		"0.0.0.0:8151": "open-platform",
	}
	if owner, dup := taken[c.ListenOn]; dup {
		t.Fatalf("ListenOn %s 与 %s 冲突", c.ListenOn, owner)
	}
	if c.ListenOn != "0.0.0.0:8130" {
		t.Fatalf("feature-store 的 ListenOn 应为 0.0.0.0:8130，实得 %s", c.ListenOn)
	}
}

// TestValidateRejectsDangerousCombos 保证 Validate 真的在拦事，而不是恒返回 nil。
func TestValidateRejectsDangerousCombos(t *testing.T) {
	var c Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "featurestore.v1.yaml"), &c); err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("示例配置应当通过自检，实得 %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"空 DataSource", func(x *Config) { x.DataSource = "  " }},
		{"DSN 指向别人的库", func(x *Config) {
			x.DataSource = "u:@tcp(127.0.0.1:3306)/go_video_spm"
		}},
		{"缺 CacheRedis", func(x *Config) { x.CacheRedis.Host = "" }},
		{"响应上限突破服务端硬上限", func(x *Config) {
			x.Read.MaxBatchResponseBytes = model.MaxBatchResponseBytes + 1
		}},
		{"抖动比例越界", func(x *Config) { x.Read.CacheJitterRatio = 0.9 }},
		{"回执租约超过上限", func(x *Config) {
			x.Write.ReceiptLeaseSeconds = model.MaxReceiptLeaseSeconds + 1
		}},
		{"幂等保留期短于租约", func(x *Config) { x.Write.ReceiptRetentionSeconds = 1 }},
		{"清理行数越界", func(x *Config) { x.Write.MaxPurgeRowsPerCall = model.MaxPurgeRows + 1 }},
		{"隐私白名单为空", func(x *Config) { x.Privacy.OperatorPrefixes = nil }},
		{"隐私白名单含空串", func(x *Config) { x.Privacy.OperatorPrefixes = []string{"privacy:", " "} }},
		{"隐私级别不是已声明枚举", func(x *Config) { x.Privacy.ExportMaxPrivacyLevel = 9 }},
		{"回填租约非正", func(x *Config) { x.Backfill.LeaseSeconds = 0 }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			bad := c
			tc.mutate(&bad)
			if err := bad.Validate(); err == nil {
				t.Fatalf("%s：Validate 应当报错，却放行了", tc.name)
			}
		})
	}
}

var redisConfType = reflect.TypeOf(redis.RedisConf{})

// assertNoRedisKeyCollision 反射遍历 Config 的全部具名字段：
// 出现任何名为 Redis 的字段就意味着与 zrpc.RpcServerConf.Redis（redis.RedisKeyConf）
// 抢同一个配置键 "redis"，conf.Load 会直接失败。
// 这里刻意不看匿名（内嵌）字段本身，但会递归进非框架的内嵌结构。
func assertNoRedisKeyCollision(t *testing.T, v reflect.Value) {
	t.Helper()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if !sf.IsExported() {
			continue
		}
		if sf.Name == "Redis" && !sf.Anonymous {
			t.Fatalf("字段 %s.Redis 与 zrpc.RpcServerConf 内嵌的 Redis 同名，"+
				"conf.Load 会报 conflict key redis；业务缓存必须叫 CacheRedis", typ.Name())
		}
		f := v.Field(i)
		if sf.Anonymous {
			// 内嵌的都是 go-zero 框架配置，其内部字段名不由我们决定，只递归不下结论。
			if f.Kind() == reflect.Struct {
				assertEmbeddedHasNoBusinessRedis(t, f)
			}
			continue
		}
		switch f.Kind() {
		case reflect.String:
			if strings.HasSuffix(sf.Name, "DataSource") && strings.TrimSpace(f.String()) == "" {
				t.Errorf("%s 为空，服务启动后无法访问数据库", sf.Name)
			}
		case reflect.Struct:
			if f.Type() == redisConfType {
				if f.Interface().(redis.RedisConf).Host == "" {
					t.Errorf("%s.Host 为空，在线特征主读缓存无法构造", sf.Name)
				}
				continue
			}
			assertNoRedisKeyCollision(t, f)
		}
	}
}

// assertEmbeddedHasNoBusinessRedis 只在内嵌框架结构里找「我们自己加的」缓存字段，
// 避免把 go-zero 自带的 RedisKeyInfo/Redis 当成违规（那是框架自己的键）。
// 做法是仅递归一层并检查字段类型是否为 redis.RedisConf（业务缓存的形态）。
func assertEmbeddedHasNoBusinessRedis(t *testing.T, v reflect.Value) {
	t.Helper()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if !sf.IsExported() {
			continue
		}
		if sf.Type == redisConfType && sf.Name != "CacheRedis" {
			t.Errorf("内嵌字段 %s.%s 的业务缓存字段名叫 %s：全仓约定叫 CacheRedis，"+
				"否则会与框架的 redis 键冲突", typ.Name(), sf.Name, sf.Name)
		}
	}
}

// dsnPassword 从 go-sql-driver 的 DSN 里取出密码段（user:pass@tcp(...) 的 pass）。
// 没有 '@' 或 ':' 时返回空串。
func dsnPassword(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return ""
	}
	cred := dsn[:at]
	colon := strings.Index(cred, ":")
	if colon < 0 {
		return ""
	}
	return cred[colon+1:]
}
