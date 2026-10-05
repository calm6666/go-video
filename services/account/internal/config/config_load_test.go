package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// etc 示例配置必须真的能被 go-zero 加载：本仓库曾出现过 Config 自带的 Redis 字段
// 与 zrpc.RpcServerConf 内嵌的 Redis 同名，代码可编译但启动即报 "conflict key redis"。
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

var redisConfType = reflect.TypeOf(redis.RedisConf{})

// TestDownstreamRpcBlocksAreConfigured 钉住「示例配置真的接了下游」：
// 下游 RPC 段一旦被注释掉，serviceContext 就走 nil 分支，关系/资料聚合静默降级成空值
// 而应答里看不出来（见 README 已知缺口 1(a)）。这条断言不检查连通性——
// 本仓库从未启动过任何服务，它只保证「按示例配置启动时适配器会被注入」。
func TestDownstreamRpcBlocksAreConfigured(t *testing.T) {
	var c Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "account.v1.yaml"), &c); err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}
	for _, tc := range []struct {
		name string
		conf zrpc.RpcClientConf
	}{
		{"UserProfileRPC", c.UserProfileRPC},
		{"SocialGraphRPC", c.SocialGraphRPC},
	} {
		if len(tc.conf.Etcd.Hosts) == 0 && tc.conf.Target == "" {
			t.Errorf("%s 既没有 Etcd.Hosts 也没有 Target：svc 会传 nil 接口，读侧静默回空默认值（缺口 1(a)）", tc.name)
		}
	}
	if got := c.SocialGraphRPC.Etcd.Key; got != "socialgraph.v1.rpc" {
		t.Errorf("SocialGraphRPC.Etcd.Key=%q 与 social-graph 服务自己注册的 Key 不一致", got)
	}
	if got := c.UserProfileRPC.Etcd.Key; got != "user-profile.v1.rpc" {
		t.Errorf("UserProfileRPC.Etcd.Key=%q 与 user-profile 服务自己注册的 Key 不一致", got)
	}

	// 对照形态：下游段缺失时 Config 留下的就是零值——上面那条断言针对的正是这个形状。
	// 如果哪天给 RpcClientConf 加了默认 Hosts/Target，nil 分支会变成不可达，
	// 本用例的判别力也随之失效，所以这里把它钉成前提。
	var zero Config
	if len(zero.SocialGraphRPC.Etcd.Hosts) != 0 || zero.SocialGraphRPC.Target != "" ||
		len(zero.UserProfileRPC.Etcd.Hosts) != 0 || zero.UserProfileRPC.Target != "" {
		t.Fatal("对照前提失效：未配置的下游 RPC 段不再是零值形态，上面的断言需要重写")
	}
}

func checkFields(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		sf := v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		f := v.Field(i)
		// 内嵌字段都是 go-zero 框架配置（RpcServerConf/RestConf/ServiceConf），
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
