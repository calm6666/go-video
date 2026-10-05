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

// TestSecurityRefsAreEnvNames 校验密钥类字段只写「环境变量名」而不是凭据本身。
// 配置文件会进 git，任何真实盐值/密钥出现在这里都等于泄露（AGENTS.md §4）。
func TestSecurityRefsAreEnvNames(t *testing.T) {
	var c Config
	file := filepath.Join("..", "..", "etc", "audit.v1.yaml")
	if err := conf.Load(file, &c); err != nil {
		t.Fatalf("加载 %s 失败: %v", file, err)
	}
	refs := map[string]string{
		"Security.IpHashSaltRef": c.Security.IpHashSaltRef,
		"Storage.AccessKeyRef":   c.Storage.AccessKeyRef,
		"Storage.SecretKeyRef":   c.Storage.SecretKeyRef,
	}
	for name, ref := range refs {
		if ref == "" {
			t.Errorf("%s 为空：无法定位密钥来源环境变量", name)
			continue
		}
		if !strings.EqualFold(ref, strings.ToUpper(ref)) || strings.ContainsAny(ref, ":/@ ") {
			t.Errorf("%s=%q 看起来像凭据而不是环境变量名（应为全大写、无分隔符）", name, ref)
		}
	}
}

// TestQueryConstraintsPositive 校验硬约束配置不会被配成 0/负数：
// 这些值是「查询必须受限」这条接口约束的来源，配错等于放开全表扫描。
func TestQueryConstraintsPositive(t *testing.T) {
	var c Config
	file := filepath.Join("..", "..", "etc", "audit.v1.yaml")
	if err := conf.Load(file, &c); err != nil {
		t.Fatalf("加载 %s 失败: %v", file, err)
	}
	positive := map[string]int{
		"Query.MaxRangeDays": c.Query.MaxRangeDays,
		"Query.MaxPageSize":  c.Query.MaxPageSize,
		"Write.MaxBatchSize": c.Write.MaxBatchSize,
		"Write.ChainRetry":   c.Write.ChainRetry,
		"Write.MaxReasonLen": c.Write.MaxReasonLen,
		"Verify.MaxEntries":  c.Verify.MaxEntriesPerCall,
		"Export.BatchRows":   c.Export.BatchRows,
		"Archive.MaxEntries": c.Archive.MaxEntriesPerBatch,
	}
	for name, v := range positive {
		if v <= 0 {
			t.Errorf("%s = %d，必须为正数（0 会让约束形同放开）", name, v)
		}
	}
	if !c.Archive.VerifyBeforePurge {
		t.Error("Archive.VerifyBeforePurge 必须为 true：跳过清单校验就清热表等于销毁证据")
	}
	if c.Export.ObjectTTLSeconds <= 0 {
		t.Error("Export.ObjectTTLSeconds 必须为正：导出文件不允许永久留在桶里")
	}
}
