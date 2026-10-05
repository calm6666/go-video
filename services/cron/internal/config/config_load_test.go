package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// TestExampleConfigsLoad 用真实 conf.Load 加载 etc 下每个示例配置。
//
// 为什么必须做：本仓库出现过服务自定义的 Redis 字段与 zrpc.RpcServerConf 内嵌的
// Redis 同名，代码可编译但服务启动即报 "conflict key redis"（AGENTS.md §4 要求
// 每个服务的配置可验证启动）。这里同时兜住「示例配置漏填 DataSource/CacheRedis
// 导致服务起来就不可用」这一类问题。
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
		})
	}
}

// TestSchedulerDefaultsAreUsable 校验 go-zero 的默认值真的落到字段上：
// tick/租约/分页参数为 0 会让调度循环空转或除零，必须在配置层就被发现。
func TestSchedulerDefaultsAreUsable(t *testing.T) {
	var c Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "cron.v1.yaml"), &c); err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}
	if c.Scheduler.TickSeconds <= 0 {
		t.Errorf("Scheduler.TickSeconds=%d，调度循环无法建立 tick", c.Scheduler.TickSeconds)
	}
	if c.Scheduler.BatchSize <= 0 {
		t.Errorf("Scheduler.BatchSize=%d，到期扫描批量为空", c.Scheduler.BatchSize)
	}
	if c.Scheduler.MaxConcurrentRuns <= 0 {
		t.Errorf("Scheduler.MaxConcurrentRuns=%d，会禁掉所有并发执行", c.Scheduler.MaxConcurrentRuns)
	}
	if c.Scheduler.GracefulStopSeconds <= 0 {
		t.Errorf("Scheduler.GracefulStopSeconds=%d，退出时无法等待在途执行", c.Scheduler.GracefulStopSeconds)
	}
	if c.Lease.HeartbeatSeconds <= 0 || int64(c.Lease.HeartbeatSeconds)*3 >= c.Lease.DefaultTTLSeconds {
		t.Errorf("Lease.HeartbeatSeconds=%d 与 DefaultTTLSeconds=%d 不匹配：心跳必须显著快于 TTL",
			c.Lease.HeartbeatSeconds, c.Lease.DefaultTTLSeconds)
	}
	if c.Lease.MinTTLSeconds <= 0 || c.Lease.MaxTTLSeconds <= c.Lease.MinTTLSeconds {
		t.Errorf("Lease TTL 上下界非法: min=%d max=%d", c.Lease.MinTTLSeconds, c.Lease.MaxTTLSeconds)
	}
	if c.Lease.DefaultTTLSeconds < c.Lease.MinTTLSeconds || c.Lease.DefaultTTLSeconds > c.Lease.MaxTTLSeconds {
		t.Errorf("Lease.DefaultTTLSeconds=%d 越出 [%d,%d]",
			c.Lease.DefaultTTLSeconds, c.Lease.MinTTLSeconds, c.Lease.MaxTTLSeconds)
	}
	if c.Task.MaxPageSize < c.Task.DefaultPageSize || c.Task.MaxPageSize <= 0 {
		t.Errorf("Task 分页上下界非法: default=%d max=%d", c.Task.DefaultPageSize, c.Task.MaxPageSize)
	}
	if c.Task.RunRetentionDays <= 0 || c.Task.AuditRetentionDays < c.Task.RunRetentionDays {
		t.Errorf("留存天数非法: run=%d audit=%d（审计必须比执行记录留得更久）",
			c.Task.RunRetentionDays, c.Task.AuditRetentionDays)
	}
	if c.Task.DeleteBatchSize <= 0 {
		t.Errorf("Task.DeleteBatchSize=%d，清理任务会一次删不完也一次删不死", c.Task.DeleteBatchSize)
	}
	if strings.TrimSpace(c.Task.DefaultTimezone) == "" {
		t.Errorf("Task.DefaultTimezone 为空，cron 表达式无法定位时区")
	}
}

// TestDownstreamClientsAreOptional 确认下游 RPC 配置全部可留空：
// cron 在多环境分批接入下游时不能因为某个 client 没配而启动失败，
// 未配置的客户端由 svc 置 nil，对应任务在运行时显式报
// ErrDownstreamNotConfigured（而不是绕过 RPC 去写别人的库）。
func TestDownstreamClientsAreOptional(t *testing.T) {
	dir := t.TempDir()
	minimal := filepath.Join(dir, "minimal.yaml")
	body := "" +
		"Name: cron.v1.rpc\n" +
		"ListenOn: 127.0.0.1:0\n" +
		"DataSource: root:root@tcp(127.0.0.1:3399)/go_video_cron\n" +
		"CacheRedis:\n" +
		"  Host: 127.0.0.1:6379\n" +
		"  Type: node\n"
	if err := os.WriteFile(minimal, []byte(body), 0o600); err != nil {
		t.Fatalf("写入最小配置失败: %v", err)
	}

	var c Config
	if err := conf.Load(minimal, &c); err != nil {
		t.Fatalf("最小配置（不含任何下游 RPC）应当可加载: %v", err)
	}
	for name, client := range downstreamClients(c) {
		if isConfigured(client) {
			t.Errorf("最小配置下 %s 被判为已配置，svc 会去连接不存在的下游", name)
		}
	}

	// 完整示例配置里，已声明 Etcd 的客户端必须被判为已配置，
	// 否则 svc 会静默置 nil，任务永远报「下游未配置」。
	var full Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "cron.v1.yaml"), &full); err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}
	for name, client := range downstreamClients(full) {
		if !isConfigured(client) {
			t.Errorf("示例配置里 %s 未被识别为已配置（Endpoints/Target/Etcd.Hosts 全空）", name)
		}
	}
}

// downstreamClients 列出全部下游客户端，供测试统一遍历；新增字段时必须同步，
// 否则新下游会漏掉「可留空」这条约束的检查。
func downstreamClients(c Config) map[string]zrpc.RpcClientConf {
	return map[string]zrpc.RpcClientConf{
		"RightsRPC":        c.RightsRPC,
		"CatalogRPC":       c.CatalogRPC,
		"VideoRPC":         c.VideoRPC,
		"SearchIndexerRPC": c.SearchIndexerRPC,
		"NotificationRPC":  c.NotificationRPC,
		"EngagementRPC":    c.EngagementRPC,
		"InboxRPC":         c.InboxRPC,
	}
}

var (
	redisConfType     = reflect.TypeOf(redis.RedisConf{})
	rpcClientConfType = reflect.TypeOf(zrpc.RpcClientConf{})
)

// isConfigured 与 svc 中的判定保持一致：三者皆空即视为未接入。
func isConfigured(c zrpc.RpcClientConf) bool {
	return c.Target != "" || len(c.Etcd.Hosts) > 0 || len(c.Endpoints) > 0
}

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
			// zrpc.RpcClientConf 的可选字段（Endpoints/Target/Etcd 全空）是合法状态，
			// 递归进去只会误报「Host 为空」，因此跳过。
			if f.Type() == rpcClientConfType {
				continue
			}
			checkFields(t, f)
		}
	}
}
