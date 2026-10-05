package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// dbName 本服务唯一自有库（AGENTS.md §5：只能写自己的 schema）。
const dbName = "go_video_open_platform"

// TestExampleConfigsLoad 对 etc/ 下每个示例配置做真实加载：
// 代码能编译不代表配置能加载——本仓库出现过 Config 自带 Redis 字段与
// zrpc.RpcServerConf 内嵌的 RedisKeyConf 同名，启动即 "conflict key redis" 的事故，
// 因此这里既走 go-zero 的真实加载器，也用反射和原文两道检查把键名锁死。
func TestExampleConfigsLoad(t *testing.T) {
	files := exampleConfigs(t)
	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			var c Config
			// conf.Load 是 go-zero 唯一的文件加载入口（没有 conf.LoadFile）。
			if err := conf.Load(f, &c); err != nil {
				t.Fatalf("加载 %s 失败: %v", f, err)
			}
			checkRedisKeyNaming(t, filepath.Base(f), reflect.ValueOf(c))
			checkNoSecretMaterial(t, filepath.Base(f), c)
			// 示例配置必须通过启动期自检：NewServiceContext 里 Validate 失败会 Severe 终止启动，
			// 「能加载但语义危险」的组合（nonce 窗口小于时间窗两倍、缓存秒数大于 access TTL 等）
			// 必须在这一步就暴露。
			if err := c.Validate(); err != nil {
				t.Fatalf("示例配置 %s 未通过 Validate: %v", filepath.Base(f), err)
			}
		})
	}
}

// TestExampleConfigYamlHasNoRedisKey 从原文侧确认没有顶层 Redis 键：
// 反射只能看到 Go 字段名，而 conflict key redis 是 yaml 键与内嵌字段撞名导致的。
func TestExampleConfigYamlHasNoRedisKey(t *testing.T) {
	topLevelRedis := regexp.MustCompile(`(?m)^Redis\s*:`)
	for _, f := range exampleConfigs(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		if topLevelRedis.Match(raw) {
			t.Errorf("%s 含顶层 Redis 键：与 zrpc.RpcServerConf 内嵌的 RedisKeyConf 冲突，业务缓存必须叫 CacheRedis",
				filepath.Base(f))
		}
		if !strings.Contains(string(raw), "CacheRedis:") {
			t.Errorf("%s 缺少 CacheRedis 段：token 校验短缓存与 nonce 防重放没有落点", filepath.Base(f))
		}
	}
}

// TestValidateRejectsStaleIntrospectCache 锁定 IntrospectCacheSeconds 的安全语义：
// 缓存只延后「拒绝」的传播，不得延后 token 过期，因此它必须 <= AccessTokenTTLSeconds。
// 该断言同时锁类型：两个字段都是 int64 秒数，比较不需要任何转换。
func TestValidateRejectsStaleIntrospectCache(t *testing.T) {
	c := mustLoad(t)
	if reflect.TypeOf(c.OpenPlatform.IntrospectCacheSeconds).Kind() != reflect.Int64 {
		t.Fatalf("IntrospectCacheSeconds 必须是 int64（与其余 TTL 字段一致），实得 %s",
			reflect.TypeOf(c.OpenPlatform.IntrospectCacheSeconds))
	}
	c.OpenPlatform.IntrospectCacheSeconds = c.OpenPlatform.AccessTokenTTLSeconds + 1
	if err := c.Validate(); err == nil {
		t.Fatal("IntrospectCacheSeconds > AccessTokenTTLSeconds 必须被 Validate 拒绝（缓存不得延后过期）")
	}
	c.OpenPlatform.IntrospectCacheSeconds = c.OpenPlatform.AccessTokenTTLSeconds
	if err := c.Validate(); err != nil {
		t.Fatalf("等于 AccessTokenTTLSeconds 是允许的上界，不应报错: %v", err)
	}
}

// TestValidateRejectsReplayableNonceWindow 保住另一条同类语义：
// nonce 存活期小于 2 倍时间戳偏差即可在窗口内重放。
func TestValidateRejectsReplayableNonceWindow(t *testing.T) {
	c := mustLoad(t)
	c.Security.NonceTTLSeconds = c.Security.SignatureSkewSeconds
	if err := c.Validate(); err == nil {
		t.Fatal("NonceTTLSeconds < 2*SignatureSkewSeconds 必须被 Validate 拒绝（窗口内可重放）")
	}
}

// TestSecurityConfiguredFalse 未注入 pepper 时 SecurityConfigured 必须为 false：
// 验签与哈希路径据此返回 model.ErrSecretVerificationUnavailable，而不是退化成无 pepper 比较。
func TestSecurityConfiguredFalse(t *testing.T) {
	for _, c := range mustLoadAll(t) {
		if c.SecurityConfigured() {
			t.Errorf("示例配置 %s 竟已配好密钥材料：pepper 必须只从 Secret/Vault 注入", c.Name)
		}
	}
}

func exampleConfigs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatal("etc 目录下没有示例配置")
	}
	return files
}

func mustLoad(t *testing.T) Config {
	t.Helper()
	for _, c := range mustLoadAll(t) {
		return c
	}
	t.Fatal("etc 目录下没有示例配置")
	return Config{}
}

func mustLoadAll(t *testing.T) []Config {
	t.Helper()
	var out []Config
	for _, f := range exampleConfigs(t) {
		var c Config
		if err := conf.Load(f, &c); err != nil {
			t.Fatalf("加载 %s 失败: %v", f, err)
		}
		out = append(out, c)
	}
	return out
}

// checkRedisKeyNaming 递归遍历具名字段：任何一层都不允许出现叫 Redis 的业务字段。
// 内嵌字段（zrpc.RpcServerConf 等框架配置）跳过——它内部可选结构留空是正常的。
func checkRedisKeyNaming(t *testing.T, file string, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		sf := v.Type().Field(i)
		if !sf.IsExported() || sf.Anonymous {
			continue
		}
		if sf.Name == "Redis" {
			t.Errorf("%s: 字段 %s 与 zrpc.RpcServerConf 内嵌的 RedisKeyConf 撞名，启动即 conflict key redis；"+
				"业务缓存字段必须叫 CacheRedis", file, sf.Name)
			continue
		}
		f := v.Field(i)
		if f.Kind() != reflect.Struct {
			continue
		}
		if f.Type() == reflect.TypeOf(redis.RedisConf{}) {
			if sf.Name != "CacheRedis" {
				t.Errorf("%s: redis.RedisConf 类型字段只允许叫 CacheRedis，实得 %s", file, sf.Name)
			}
			if f.Interface().(redis.RedisConf).Host == "" {
				t.Errorf("%s: %s.Host 为空，缓存客户端无法构造", file, sf.Name)
			}
			continue
		}
		checkRedisKeyNaming(t, file, f)
	}
}

// checkNoSecretMaterial 示例配置不得含任何可用凭证（AGENTS.md §4）：
// pepper 留空、DataSource 只指向本机库且不带口令。
func checkNoSecretMaterial(t *testing.T, file string, c Config) {
	t.Helper()
	if c.Security.CredentialPepper != "" {
		t.Errorf("%s: Security.CredentialPepper 必须是空的（真实值进 Secret/Vault）", file)
	}
	if c.Security.WebhookMasterPepper != "" {
		t.Errorf("%s: Security.WebhookMasterPepper 必须是空的（回调签名派生根不外泄）", file)
	}
	if !strings.HasPrefix(c.DataSource, "root@tcp(127.0.0.1:3306)/"+dbName) {
		t.Errorf("%s: DataSource 必须是本机 %s 库且不带口令，实得 %q", file, dbName, c.DataSource)
	}
	if at := strings.LastIndex(c.DataSource, "@tcp("); at > 0 && strings.Contains(c.DataSource[:at], ":") {
		t.Errorf("%s: DSN 口令段必须留空，实得 %q", file, c.DataSource[:at])
	}
	if c.CacheRedis.Type != "node" {
		t.Errorf("%s: CacheRedis.Type 应为 node（单节点示例），实得 %q", file, c.CacheRedis.Type)
	}
}
