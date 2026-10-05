package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
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
			// 示例配置必须通过启动期自检：MessageRetentionDays<=0 这类
			// 「能加载但语义危险」的组合在 svc.NewServiceContext 里会 Severe 终止启动。
			if err := c.Validate(); err != nil {
				t.Fatalf("示例配置 %s 未通过 Validate: %v", filepath.Base(f), err)
			}
		})
	}
}

// TestDefaultAllowFromOptions 锁定「未设置偏好的用户按站点级默认门禁」这条语义：
// 取值必须落在 1..4，且示例配置默认取 2（仅我关注的人可发）。
func TestDefaultAllowFromOptions(t *testing.T) {
	// 示例配置在 etc/ 下（conf 包没有 LoadFile，只有 Load/LoadFromYamlBytes）。
	var c Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "privatemessage.v1.yaml"), &c); err != nil {
		t.Fatalf("加载 etc/privatemessage.v1.yaml 失败: %v", err)
	}
	if got := c.PrivateMessage.DefaultAllowFrom; got < allowFromAnyone || got > allowFromNone {
		t.Fatalf("DefaultAllowFrom=%d 越界", got)
	}
}

// TestCipherKeysNotCommitted 保证示例配置里不含任何密钥材料（AGENTS.md §4）。
func TestCipherKeysNotCommitted(t *testing.T) {
	var c Config
	files, _ := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	for _, f := range files {
		if err := conf.Load(f, &c); err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		if c.CipherConfigured() {
			t.Errorf("%s 含 Cipher 密钥材料：示例配置必须留空，真实密钥走 Secret/Vault", filepath.Base(f))
		}
		if strings.Contains(c.DataSource, "@tcp(127.0.0.1:3306)/go_video_private_message") == false {
			t.Errorf("%s 的 DataSource 必须指向 go_video_private_message 库，实得 %q", filepath.Base(f), c.DataSource)
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
