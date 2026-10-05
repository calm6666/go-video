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
// 本服务的业务缓存字段固定叫 CacheRedis，这条测试就是它的守门人。
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

// TestBusinessRedisFieldIsNamedCacheRedis 用反射钉住命名约定：
// 任何新加的 redis.RedisConf 字段都不允许叫 Redis，否则 conf.Load 与
// zrpc.RpcServerConf 内嵌的 RedisKeyConf.Redis 撞名，服务启动即失败。
func TestBusinessRedisFieldIsNamedCacheRedis(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if sf.Anonymous {
			continue
		}
		if sf.Type == redisConfType && sf.Name == "Redis" {
			t.Fatalf("字段 %s 与 zrpc.RpcServerConf 的 Redis 冲突，必须改名 CacheRedis", sf.Name)
		}
	}
	if _, ok := typ.FieldByName("CacheRedis"); !ok {
		t.Fatal("缺少 CacheRedis：本服务的配置/实验/决策指针缓存无法构造")
	}
}

// TestRankLimitsAreConfigured 确认契约承诺的上限没有落到代码常量里：
// rpc 里的 MaxCandidates / MaxReturn / BucketCount / MaxDecisionPage / MaxDigestAids /
// MaxFeatureKeys 必须由 etc yaml 提供，否则无法按场景灰度。
func TestRankLimitsAreConfigured(t *testing.T) {
	var c Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "recommendrank.v1.yaml"), &c); err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}
	limits := map[string]int32{
		"MaxCandidates":   c.Rank.MaxCandidates,
		"MaxReturn":       c.Rank.MaxReturn,
		"BucketCount":     c.Rank.BucketCount,
		"MaxDecisionPage": c.Rank.MaxDecisionPage,
		"MaxDigestAids":   c.Rank.MaxDigestAids,
		"MaxFeatureKeys":  c.Rank.MaxFeatureKeys,
	}
	for name, value := range limits {
		if value <= 0 {
			t.Errorf("Rank.%s = %d，必须为正数上限", name, value)
		}
	}
	if c.Rank.MaxReturn > c.Rank.MaxCandidates {
		t.Errorf("MaxReturn(%d) 不应大于 MaxCandidates(%d)", c.Rank.MaxReturn, c.Rank.MaxCandidates)
	}
	if c.Rank.BucketCount < 100 {
		t.Errorf("BucketCount(%d) 过小会导致实验无法细分", c.Rank.BucketCount)
	}
	if c.Rank.DownstreamTimeoutMs >= c.Rank.ScoreBudgetMs {
		t.Errorf("DownstreamTimeoutMs(%dms) 必须小于 ScoreBudgetMs(%dms)，否则预算耗尽前无法判定下游故障",
			c.Rank.DownstreamTimeoutMs, c.Rank.ScoreBudgetMs)
	}
	if c.Rank.DecisionRetentionDays <= 0 {
		t.Error("DecisionRetentionDays 必须为正：rank_decision_log 不能无限增长")
	}
	if c.Rank.DefaultFallback == "" {
		t.Error("DefaultFallback 为空，故障时无法确定兜底策略")
	}
}
