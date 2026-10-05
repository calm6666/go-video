package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// TestExampleConfigsLoad 保证 etc 示例配置真的能被 go-zero 加载。
//
// 本仓库出现过两类只在启动时炸的缺陷：
//  1. Config 自带 Redis 字段与 zrpc.RpcServerConf 内嵌的 Redis 同名 →
//     "conflict key redis"（能编译，启动即挂）；
//  2. 新增字段没有 default / 示例配置漏键 → conf.Load 报 unexpected value。
//
// 因此这里逐个文件 conf.Load + checkFields 反射遍历 + Validate，全部走真实解析。
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
			if err := c.Validate(); err != nil {
				t.Fatalf("示例配置 %s 未通过 Validate: %v", filepath.Base(f), err)
			}
		})
	}
}

// TestNoFieldNamedRedis 锁定「业务 Redis 字段必须叫 CacheRedis」这条全仓约定：
// 任何直接叫 Redis 的字段都会与 zrpc.RpcServerConf.Redis 冲突，启动即失败。
func TestNoFieldNamedRedis(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if !sf.Anonymous && strings.EqualFold(sf.Name, "Redis") {
			t.Fatalf("Config 出现了名为 %s 的业务字段：与 zrpc.RpcServerConf 内嵌的 Redis 冲突，必须改名 CacheRedis", sf.Name)
		}
	}
	if _, ok := typ.FieldByName("CacheRedis"); !ok {
		t.Fatal("Config 必须提供 CacheRedis 字段（redis.RedisConf）")
	}
}

// TestPrivacySaltNotCommitted 保证示例配置不含盐值、且 DataSource 指向本服务自有库。
func TestPrivacySaltNotCommitted(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	if len(files) == 0 {
		t.Fatal("etc 目录下没有示例配置")
	}
	for _, f := range files {
		var c Config
		if err := conf.Load(f, &c); err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		if c.SaltConfigured() {
			t.Errorf("%s 含 Privacy.SaltValue：盐值必须只从 Secret/环境变量注入（SaltRef 指定变量名）", filepath.Base(f))
		}
		if !strings.Contains(c.DataSource, "go_video_event_collector") {
			t.Errorf("%s 的 DataSource 必须指向 go_video_event_collector 库，实得 %q", filepath.Base(f), c.DataSource)
		}
		if c.Privacy.SaltRef == "" {
			t.Errorf("%s 的 Privacy.SaltRef 为空，无法定位盐值来源", filepath.Base(f))
		}
	}
}

// TestValidateRejectsDangerousDefaults 抽样验证启动期自检真的会拦住危险配置：
// 采样比例越界、IP 段前缀等于明文、租约不回收这几类必须在启动时报错。
func TestValidateRejectsDangerousDefaults(t *testing.T) {
	base := Config{
		Collector: CollectorConf{
			PageSize:                50,
			MaxPageSize:             500,
			MaxEventsPerBatch:       200,
			MaxRequestBytes:         1 << 20,
			MaxEventPayloadBytes:    8192,
			MaxClockSkewSeconds:     300,
			MaxBackfillSeconds:      86400,
			KeywordMaxRunes:         64,
			DefaultSampleBps:        10000,
			SupportedSchemaVersion:  1,
			IPSegmentBits:           24,
			GlobalQps:               2000,
			GlobalBurst:             800,
			RetentionDays:           30,
			DeadLetterRetentionDays: 90,
			BatchLimit:              200,
			MaxReplayPerRequest:     100,
		},
		Privacy: PrivacyConf{SaltRef: "EVENT_COLLECTOR_SALT_V1", SaltVersion: 1},
		Dispatch: DispatchConf{
			BatchSize: 200, DeliverMaxAttempts: 8, RetryBaseSeconds: 5, RetryMaxSeconds: 3600,
			LeaseSeconds: 60, RetryScanLookaheadSeconds: 86400,
		},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("基准配置应通过 Validate，实得 %v", err)
	}

	cases := []struct {
		name string
		mut  func(c *Config)
	}{
		{"采样比例越界", func(c *Config) { c.Collector.DefaultSampleBps = 10001 }},
		{"IP 段前缀等于明文", func(c *Config) { c.Collector.IPSegmentBits = 32 }},
		{"无盐版本", func(c *Config) { c.Privacy.SaltVersion = 0 }},
		{"回补窗口小于时钟偏差", func(c *Config) { c.Collector.MaxBackfillSeconds = 10 }},
		{"投递重试上限为 0", func(c *Config) { c.Dispatch.DeliverMaxAttempts = 0 }},
		{"租约不回收", func(c *Config) { c.Dispatch.LeaseSeconds = 0 }},
		{"台账永久保留", func(c *Config) { c.Collector.RetentionDays = 0 }},
	}
	for _, tc := range cases {
		c := base
		tc.mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s：Validate 应返回 error，实得 nil", tc.name)
		}
	}
}

var redisConfType = reflect.TypeOf(redis.RedisConf{})

func checkFields(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		sf := v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		// 内嵌字段都是 go-zero 框架配置（RpcServerConf/ServiceConf），其中本就叫 Redis
		// 的可选结构（zrpc 鉴权用的 RedisKeyConf）是正常的，不能算本服务的业务字段。
		if sf.Anonymous {
			continue
		}
		if !sf.Anonymous && strings.EqualFold(sf.Name, "Redis") {
			t.Errorf("字段 %s 与 zrpc.RpcServerConf 内嵌的 Redis 同名，会触发 conflict key redis", sf.Name)
			continue
		}
		f := v.Field(i)
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
