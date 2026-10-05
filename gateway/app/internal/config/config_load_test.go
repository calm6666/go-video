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

// TestPmAndLiveRpcKeysWired 覆盖本轮新接入的两个下游键（私信 / 直播间）。
//
// SvcContext 只在 Target 或 Etcd.Hosts 非空时才构造 client（其余情况留 nil，
// logic 层据此返回「service not configured」而不是空成功），所以示例配置里
// 这两个键必须真的能解析出来，否则新路由在本地一跳就退化成不可用。
// Etcd.Key 要与服务侧注册名逐字一致：
// services/private-message/etc/privatemessage.v1.yaml 与
// services/live-room/etc/liveroom.v1.yaml 均注册 <module>.v1.rpc。
func TestPmAndLiveRpcKeysWired(t *testing.T) {
	var c Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "app.yaml"), &c); err != nil {
		t.Fatalf("加载 etc/app.yaml 失败: %v", err)
	}
	for _, tc := range []struct {
		name    string
		conf    zrpc.RpcClientConf
		wantKey string
	}{
		{"PrivateMessageRPC", c.PrivateMessageRPC, "privatemessage.v1.rpc"},
		{"LiveRoomRPC", c.LiveRoomRPC, "liveroom.v1.rpc"},
	} {
		if len(tc.conf.Etcd.Hosts) == 0 && tc.conf.Target == "" {
			t.Fatalf("%s 既无 Etcd.Hosts 也无 Target，网关不会构造该 client", tc.name)
		}
		if tc.conf.Etcd.Key != tc.wantKey {
			t.Errorf("%s.Etcd.Key = %q，服务侧注册名为 %q", tc.name, tc.conf.Etcd.Key, tc.wantKey)
		}
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
