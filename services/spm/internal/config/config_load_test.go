package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// TestExampleConfigsLoad 校验 etc 示例配置真的能被 go-zero 加载。
//
// 本仓库出现过两类「能编译、启动即挂」的配置事故，都在这里钉住：
//  1. Config 自带的 Redis 字段与 zrpc.RpcServerConf 内嵌的 Redis 同名
//     -> conf.Load 报 "conflict key redis"；
//  2. 字段没有 `,optional`/`default` 而 yaml 里没写 -> conf.Load 直接报缺字段。
func TestExampleConfigsLoad(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatal("etc 目录下没有示例配置")
	}
	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			var c Config
			// conf 包只有 Load/LoadFromYamlBytes（没有 LoadFile），示例配置必须走同一条
			// 与 main 里 conf.MustLoad 完全一致的解析路径，否则测的就不是启动时读的那份。
			if err := conf.Load(f, &c); err != nil {
				t.Fatalf("加载 %s 失败: %v", f, err)
			}
			checkFields(t, reflect.ValueOf(c))
			// 示例配置必须通过启动期自检：BehaviorRetentionDays<=0 这类
			// 「能加载但语义危险」的组合在 svc.NewServiceContext 里会 Severe 终止启动。
			if err := c.Validate(); err != nil {
				t.Fatalf("示例配置 %s 未通过 Validate: %v", filepath.Base(f), err)
			}
		})
	}
}

// TestNoFieldNamedRedis 用反射钉住「业务缓存字段必须叫 CacheRedis」这条全仓约定：
// Config 里任何一层都不得出现名为 Redis 的字段（内嵌的框架字段除外），
// 否则 conf.Load 会因 key 冲突让服务起不来，而这一点单靠编译看不出来。
func TestNoFieldNamedRedis(t *testing.T) {
	var found []string
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			sf := rt.Field(i)
			if sf.Anonymous {
				// 内嵌框架配置（RpcServerConf 等）自带的 Redis 字段不由本服务命名。
				continue
			}
			if !sf.IsExported() {
				continue
			}
			if sf.Name == "Redis" {
				found = append(found, rt.Name()+"."+sf.Name)
			}
			ft := sf.Type
			if ft.Kind() == reflect.Struct && ft.PkgPath() != "" &&
				strings.HasPrefix(ft.PkgPath(), "go-video/") {
				walk(ft)
			}
			if ft.Kind() == reflect.Ptr && ft.Elem().Kind() == reflect.Struct {
				walk(ft.Elem())
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
	if len(found) > 0 {
		t.Errorf("配置里出现名为 Redis 的字段 %v：与 zrpc.RpcServerConf 内嵌字段同名，"+
			"启动会报 conflict key redis，业务缓存请改名 CacheRedis", found)
	}
}

// TestValidateRejectsDangerousConfigs 逐项确认 Validate 真的会拒绝那些
// 「加载得成功但口径会静默算错」的组合。只测示例配置通过是不够的：
// 那只能证明默认值没问题，证明不了约束在生效。
func TestValidateRejectsDangerousConfigs(t *testing.T) {
	base := sampleConfig(t)

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"空 DataSource", func(c *Config) { c.DataSource = "" }},
		{"ps 上限超过契约 100", func(c *Config) { c.Spm.MaxPageSize = 500 }},
		{"MaxPageSize 小于默认页", func(c *Config) { c.Spm.MaxPageSize = 10 }},
		{"单批点数超 model 上限", func(c *Config) { c.Spm.MaxWritePoints = 501 }},
		{"单请求口径数超契约上限", func(c *Config) { c.Spm.MaxMetricKeysPerRequest = 51 }},
		{"连续窗口数超契约上限", func(c *Config) { c.Spm.MaxWindowCount = 31 }},
		{"max_day 超契约上限", func(c *Config) { c.Spm.MaxRetentionDay = 91 }},
		{"留存天数超过事实留存期", func(c *Config) { c.Spm.BehaviorRetentionDays = 30 }},
		{"事实永不清理", func(c *Config) { c.Spm.BehaviorRetentionDays = 0 }},
		{"投影比事实先被删", func(c *Config) { c.Spm.ProjectionRetentionDays = 1 }},
		{"top_n 超过画像裁剪上限", func(c *Config) { c.Spm.MaxInterestTopN = 101 }},
		{"默认 top_n 大于上限", func(c *Config) { c.Spm.InterestTopN = base.Spm.MaxInterestTopN + 1 }},
		{"实时窗口用了天粒度", func(c *Config) { c.Spm.RealtimeWindowType = 3 }},
		{"迟到容忍长于窗口", func(c *Config) { c.Spm.LateToleranceSeconds = 3600 }},
		{"画像永不判过期", func(c *Config) { c.Spm.InterestStaleAfterSeconds = 0 }},
		{"清理单批过大", func(c *Config) { c.Spm.DeleteBatchSize = 100001 }},
		{"作业无租约", func(c *Config) { c.Spm.JobLeaseSeconds = 0 }},
		{"失败无限重放", func(c *Config) { c.Spm.JobMaxRetry = -1 }},
		{"写侧 QPS 为 0", func(c *Config) { c.Spm.WriteQps = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("%s 未被 Validate 拒绝：该组合会让口径静默算错或启动即自锁", tc.name)
			}
		})
	}
}

// TestDataSourcePointsToOwnDatabase 保证示例配置只指向本服务自有的库：
// spm 的连接串里出现别的库名，就意味着有人在写跨库 SQL（AGENTS.md §5）。
func TestDataSourcePointsToOwnDatabase(t *testing.T) {
	for _, f := range exampleConfigs(t) {
		var c Config
		if err := conf.Load(f, &c); err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		if !strings.Contains(c.DataSource, "/go_video_spm?") {
			t.Errorf("%s 的 DataSource 必须指向 go_video_spm 库，实得 %q", filepath.Base(f), c.DataSource)
		}
	}
}

// TestListenOnIsNotGatewayHTTPPort 防止示例配置退回 8080：
// 那是 gateway/app 的 HTTP 监听端口，同机启动两个服务会直接 bind 失败。
func TestListenOnIsNotGatewayHTTPPort(t *testing.T) {
	for _, f := range exampleConfigs(t) {
		var c Config
		if err := conf.Load(f, &c); err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		if strings.HasSuffix(c.ListenOn, ":8080") {
			t.Errorf("%s 的 ListenOn=%s 与 gateway 冲突，请用本服务分配的端口", filepath.Base(f), c.ListenOn)
		}
	}
}

// TestNoSecretsInExampleConfigs 保证示例配置里不含任何密钥材料（AGENTS.md §4）。
// spm 没有对称密钥字段，但反射遍历保证以后新增字段时同样受约束。
func TestNoSecretsInExampleConfigs(t *testing.T) {
	for _, f := range exampleConfigs(t) {
		var c Config
		if err := conf.Load(f, &c); err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		v := reflect.ValueOf(c)
		for _, p := range secretLikePaths(v, "") {
			if p != "" {
				t.Errorf("%s 的 %s 含疑似密钥值：示例配置必须留空，真实密钥走 Secret/Vault",
					filepath.Base(f), p)
			}
		}
	}
}

var redisConfType = reflect.TypeOf(redis.RedisConf{})

// sampleConfig 取示例配置作为 Validate 用例的基准（含全部 default 生效后的值）。
func sampleConfig(t *testing.T) Config {
	t.Helper()
	files := exampleConfigs(t)
	var c Config
	if err := conf.Load(files[0], &c); err != nil {
		t.Fatalf("加载 %s 失败: %v", files[0], err)
	}
	return c
}

func exampleConfigs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatal("etc 目录下没有示例配置")
	}
	return files
}

// secretLikePaths 返回字段名为密钥类且值非空的字段路径。
// 只查本服务自定义的结构（不深入框架配置），避免把 go-zero 内部字段误判成密钥。
func secretLikePaths(v reflect.Value, prefix string) []string {
	var out []string
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		path := prefix + sf.Name
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			if isSecretName(sf.Name) && strings.TrimSpace(f.String()) != "" {
				out = append(out, path)
			}
		case reflect.Struct:
			if f.Type() == redisConfType {
				continue
			}
			if sf.Anonymous || strings.HasPrefix(f.Type().PkgPath(), "go-video/") {
				out = append(out, secretLikePaths(f, path+".")...)
			}
		}
	}
	return out
}

func isSecretName(name string) bool {
	for _, kw := range []string{"Key", "Secret", "Pepper", "Password", "Token", "Credential"} {
		if strings.Contains(strings.ToLower(name), strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// checkFields 递归检查示例配置里不能为空的字段。
func checkFields(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		sf := v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		f := v.Field(i)
		// 内嵌字段都是 go-zero 框架配置（RpcServerConf/ServiceConf），
		// 其内部可选结构（如 zrpc 鉴权用的 RedisKeyInfo）留空是正常的，不检查。
		if sf.Anonymous {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			if strings.HasSuffix(sf.Name, "DataSource") && f.String() == "" {
				t.Errorf("%s 为空，服务启动后无法访问数据库", sf.Name)
			}
		case reflect.Struct:
			if f.Type() == redisConfType {
				if f.Interface().(redis.RedisConf).Host == "" {
					t.Errorf("%s.Host 为空，缓存客户端无法构造", sf.Name)
				}
				continue
			}
			checkFields(t, f)
		}
	}
}
