package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// TestExampleConfigsLoad 用真实的 conf.Load 加载 etc/ 下每个示例配置。
//
// 本仓库出现过 Config 自带的 Redis 字段与 zrpc.RpcServerConf 内嵌的 Redis 同名，
// 代码可编译但启动即报 "conflict key redis"；也出现过 yaml 写了 Config 里没有的键、
// 或 Config 里的必填键 yaml 没给导致启动失败。这两类问题都只能靠真实加载发现，
// 因此本用例不允许改成「只 reflect 结构」。
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
			checkLiveIngestConfig(t, c)
		})
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

// checkLiveIngestConfig 断言示例配置的领域参数落在合法区间内：
// 这些值直接决定状态机与幂等语义（宽限期、超时、分页上限），
// 示例配置一旦写错会让本地环境出现「线上不可能发生」的行为。
func checkLiveIngestConfig(t *testing.T, c Config) {
	t.Helper()

	li := c.LiveIngest
	if li.IssueTtlSeconds <= 0 {
		t.Errorf("LiveIngest.IssueTtlSeconds=%d 必须 > 0", li.IssueTtlSeconds)
	}
	if li.MaxIssueTtlSeconds < li.IssueTtlSeconds {
		t.Errorf("MaxIssueTtlSeconds=%d 不能小于 IssueTtlSeconds=%d", li.MaxIssueTtlSeconds, li.IssueTtlSeconds)
	}
	if li.KeyRandomBytes < 16 {
		t.Errorf("KeyRandomBytes=%d 过短，明文密钥强度不足", li.KeyRandomBytes)
	}
	if li.CriticalMinVideoBitrateBps >= li.DegradedMinVideoBitrateBps {
		t.Errorf("CRITICAL 码率阈值(%d) 必须低于 DEGRADED 阈值(%d)",
			li.CriticalMinVideoBitrateBps, li.DegradedMinVideoBitrateBps)
	}
	if li.HealthNoDataSeconds <= int64(li.HealthSampleWindowSeconds) {
		t.Errorf("HealthNoDataSeconds=%d 必须大于采样窗口=%d",
			li.HealthNoDataSeconds, li.HealthSampleWindowSeconds)
	}
	if li.MaxListPageSize <= 0 || li.MaxEventPageSize <= 0 || li.MaxSamplePoints <= 0 {
		t.Error("分页与取样上限必须 > 0，否则列表查询会退化成无 LIMIT 扫描")
	}
	if li.CountHardLimit <= 0 {
		t.Error("CountHardLimit 必须 > 0，否则断流记录 total 统计失去上限保护")
	}
	if li.SweeperEnabled {
		t.Error("示例配置不应默认开启扫描器：本轮扫描器未实现，开启会造成运维误判")
	}
	if !strings.Contains(c.DataSource, "go_video_live_ingest") {
		t.Errorf("DataSource 必须指向本服务自有库 go_video_live_ingest，当前=%s", c.DataSource)
	}
	// 密钥/签名相关的配置位不得出现真实凭据（AGENTS.md §4：生产密钥进 Secret/Vault）。
	for _, forbidden := range []string{"sk-", "AKID", "-----BEGIN"} {
		if strings.Contains(strings.ToLower(c.Cdn.CallbackSecretRef), strings.ToLower(forbidden)) {
			t.Errorf("Cdn.CallbackSecretRef 疑似包含真实凭据（命中 %q），只能放 Secret/Vault 引用", forbidden)
		}
	}
}
