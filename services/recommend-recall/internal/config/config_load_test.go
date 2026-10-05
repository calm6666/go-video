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
// 因此这里不做"读结构体断言字段"的假验证，而是逐文件走 conf.Load。
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
			checkRecallConfig(t, &c)
		})
	}
}

// checkRecallConfig 断言关键业务字段真的从 yaml 读进来了（而不是结构体零值冒充成功）。
func checkRecallConfig(t *testing.T, c *Config) {
	t.Helper()

	// 数据库与缓存：DSN 必须是本服务专属库，缓存字段必须是 CacheRedis。
	if !strings.Contains(c.DataSource, "go_video_recommend_recall") {
		t.Errorf("DataSource=%q 未指向 go_video_recommend_recall 库，会写错库", c.DataSource)
	}
	if c.CacheRedis.Host == "" {
		t.Error("CacheRedis.Host 为空，缓存客户端无法构造")
	}
	if c.ListenOn == "" {
		t.Error("ListenOn 为空，zrpc 无法监听")
	}
	// "业务缓存字段不能叫 Redis" 由上面的 conf.Load 成功本身证明：
	// 若与 zrpc.RpcServerConf 内嵌的 RedisKeyConf.Redis 撞名，Load 会直接报 conflict key redis。
	// 这里不再用反射断言字段名（promoted field 会让断言真假难辨）。

	r := c.Recall
	if len(r.EnabledSources) == 0 || len(r.DefaultSources) == 0 {
		t.Errorf("EnabledSources=%v DefaultSources=%v 为空：召回路开关是部署决策，yaml 必须显式给出",
			r.EnabledSources, r.DefaultSources)
	}
	if r.FallbackSource == 0 || r.ColdStartSource == 0 {
		t.Errorf("FallbackSource=%d ColdStartSource=%d 为 0 会让降级链路指向不存在的池",
			r.FallbackSource, r.ColdStartSource)
	}
	for name, val := range map[string]int{
		"MaxCandidates": r.MaxCandidates, "DefaultLimit": r.DefaultLimit, "PerSourceMax": r.PerSourceMax,
		"MaxBatchItems": r.MaxBatchItems, "MaxVersionList": r.MaxVersionList,
		"MaxPoolSnapshotPage": r.MaxPoolSnapshotPage, "MaxRequestLogPage": r.MaxRequestLogPage,
		"MinKeepVersions": r.MinKeepVersions, "MaxSeedAids": r.MaxSeedAids,
		"MaxSeedTags": r.MaxSeedTags, "MaxExcludeAids": r.MaxExcludeAids,
		"BudgetMillis": r.BudgetMillis, "MaxReadyPools": r.MaxReadyPools,
	} {
		if val <= 0 {
			t.Errorf("Recall.%s=%d，非正数会让查询走宁缺不空的拒绝分支或直接 panic", name, val)
		}
	}
	if err := r.Validate(); err != nil {
		t.Errorf("示例配置的 Recall 段自洽性校验失败: %v", err)
	}
	if !r.DegradeEnabled {
		t.Error("示例配置应允许降级出数：关闭降级只应出现在压测/回放的显式请求里")
	}
}

// TestRecallValidateRejectsInconsistentSources 覆盖降级矩阵依赖的自洽性：
// 兜底路/冷启动路/默认组合必须是已启用的路，否则降级会在最该出数时报 pool_not_ready。
func TestRecallValidateRejectsInconsistentSources(t *testing.T) {
	base := func() RecallConf {
		return RecallConf{
			MaxCandidates:               400,
			DefaultLimit:                120,
			PerSourceMax:                120,
			EnabledSources:              []int64{1, 2, 3, 6},
			DefaultSources:              []int64{1, 2, 6},
			FallbackSource:              1,
			ColdStartSource:             6,
			MaxSeedAids:                 20,
			MaxSeedTags:                 20,
			MaxExcludeAids:              500,
			BudgetMillis:                80,
			MaxReadyPools:               50,
			MaxBatchItems:               1000,
			MaxVersionList:              100,
			MaxPoolSnapshotPage:         200,
			MaxRequestLogPage:           100,
			MinKeepVersions:             2,
			PruneMaxRows:                2000,
			RequestLogRetentionSeconds:  604800,
			IdempotencyLeaseSeconds:     300,
			IdempotencyRetentionSeconds: 86400,
		}
	}
	cases := []struct {
		name   string
		mutate func(*RecallConf)
	}{
		{name: "空 EnabledSources", mutate: func(r *RecallConf) { r.EnabledSources = nil }},
		{name: "未知召回路", mutate: func(r *RecallConf) { r.EnabledSources = []int64{1, 99} }},
		{name: "重复召回路", mutate: func(r *RecallConf) { r.EnabledSources = []int64{1, 1, 2} }},
		{name: "兜底路未启用", mutate: func(r *RecallConf) { r.FallbackSource = 4 }},
		{name: "冷启动路未启用", mutate: func(r *RecallConf) { r.ColdStartSource = 5 }},
		{name: "默认组合为空", mutate: func(r *RecallConf) { r.DefaultSources = nil }},
		{name: "默认组合含未启用路", mutate: func(r *RecallConf) { r.DefaultSources = []int64{1, 4} }},
		{name: "DefaultLimit 超总上限", mutate: func(r *RecallConf) { r.DefaultLimit = r.MaxCandidates + 1 }},
		{name: "PerSourceMax 超总上限", mutate: func(r *RecallConf) { r.PerSourceMax = r.MaxCandidates + 1 }},
		{name: "预算非正数", mutate: func(r *RecallConf) { r.BudgetMillis = 0 }},
		{name: "清理行数非正数", mutate: func(r *RecallConf) { r.PruneMaxRows = 0 }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("期望 Validate 失败: %+v", r)
			}
		})
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("基准配置应通过校验: %v", err)
	}
}

// TestSourceSliceIsRequired 固化"召回路开关不给 default"的决策：
// 漏配必须启动失败，而不是悄悄退回到某组默认路（那会让线上跑在没人评审的召回组合上）。
func TestSourceSliceIsRequired(t *testing.T) {
	const head = `Name: recommend-recall
ListenOn: 127.0.0.1:0
CacheRedis:
  Host: 127.0.0.1:6379
  Type: node
DataSource: "root:root@tcp(127.0.0.1:3306)/go_video_recommend_recall?charset=utf8mb4&parseTime=true"
`
	noSources := head + `Recall:
  MaxCandidates: 400
  DefaultLimit: 120
  FallbackSource: 1
  ColdStartSource: 6
`
	var missing Config
	if err := conf.LoadFromYamlBytes([]byte(noSources), &missing); err == nil {
		t.Fatal("缺 EnabledSources/DefaultSources 的配置竟然加载成功，说明结构体给了 default")
	}

	withSources := noSources + `  EnabledSources: [1, 2, 6]
  DefaultSources: [1, 2, 6]
`
	var ok Config
	if err := conf.LoadFromYamlBytes([]byte(withSources), &ok); err != nil {
		t.Fatalf("补齐召回路后加载失败: %v", err)
	}
	if err := ok.Recall.Validate(); err != nil {
		t.Fatalf("补齐召回路后校验失败: %v", err)
	}
	if len(ok.Recall.EnabledSources) != 3 {
		t.Errorf("EnabledSources=%v，切片未从 yaml 完整读入", ok.Recall.EnabledSources)
	}
	// 其余带 default 的项未给出时必须落到结构体默认值。
	if ok.Recall.PerSourceMax != 120 || ok.Recall.BudgetMillis != 80 || !ok.Recall.DegradeEnabled {
		t.Errorf("default 标签未生效: %+v", ok.Recall)
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
		// 内嵌字段都是 go-zero 框架配置（RpcServerConf），
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
