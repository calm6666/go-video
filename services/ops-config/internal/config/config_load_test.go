package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

// TestExampleConfigsLoad 真实加载 etc/ 下的每一个示例配置。
//
// 本仓库出现过两类只在**运行时**暴露的配置事故，光 go build 发现不了：
//  1. Config 自带的 Redis 字段与 zrpc.RpcServerConf 内嵌的同名字段冲突，
//     启动即报 "conflict key redis"（AGENTS.md §4：启动可验证）；
//  2. yaml 里写了结构体没有的键，或结构体有必填键而 yaml 漏了，服务起不来。
//
// 所以这里不是「解析一下不报错」就算完，而是继续用 checkFields 断言关键值非空。
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
				// 业务缓存字段必须叫 CacheRedis；叫 Redis 时上面这条分支根本走不到
				// （conf.Load 会先因 conflict key 失败），这条断言是第二道防线。
				if sf.Name == "Redis" {
					t.Errorf("业务缓存字段命名为 Redis，与 zrpc.RpcServerConf 内嵌字段冲突")
				}
				if f.Interface().(redis.RedisConf).Host == "" {
					t.Errorf("%s.Host 为空，缓存客户端无法构造", sf.Name)
				}
				continue
			}
			checkFields(t, f)
		}
	}
}

// TestConstraintsPositive 校验硬约束配置没有被配成 0/负数：
// 这些值是「读取必须受限、写入必须校验」这条接口约束的来源，
// 配成 0 等于放开一次无界扫描（BatchResolveConfig 会扇出到全表）。
func TestConstraintsPositive(t *testing.T) {
	var c Config
	file := filepath.Join("..", "..", "etc", "opsconfig.v1.yaml")
	if err := conf.Load(file, &c); err != nil {
		t.Fatalf("加载 %s 失败: %v", file, err)
	}
	positive := map[string]int{
		"OpsValue.MaxBytes":              c.OpsValue.MaxBytes,
		"OpsValue.MaxKeyLen":             c.OpsValue.MaxKeyLen,
		"OpsValue.MaxReasonLen":          c.OpsValue.MaxReasonLen,
		"OpsValue.MaxOperatorNameLen":    c.OpsValue.MaxOperatorNameLen,
		"OpsTopic.MaxItems":              c.OpsTopic.MaxItems,
		"OpsTopic.MaxRefIDs":             c.OpsTopic.MaxRefIDs,
		"OpsTopic.DefaultItemLimit":      c.OpsTopic.DefaultItemLimit,
		"OpsTopic.ListTTLSeconds":        c.OpsTopic.ListTTLSeconds,
		"OpsSlot.MaxCapacity":            c.OpsSlot.MaxCapacity,
		"OpsSlot.MaxItems":               c.OpsSlot.MaxItems,
		"OpsSlot.DefaultTTLSeconds":      c.OpsSlot.DefaultTTLSeconds,
		"Rollout.MaxWhitelistMids":       c.Rollout.MaxWhitelistMids,
		"Rollout.MaxRulesPerConfig":      c.Rollout.MaxRulesPerConfig,
		"Resolve.MaxBatchKeys":           c.Resolve.MaxBatchKeys,
		"Resolve.DefaultTTLSeconds":      c.Resolve.DefaultTTLSeconds,
		"Resolve.DefaultTopicTTLSeconds": c.Resolve.DefaultTopicTTLSeconds,
		"Query.MaxPageSize":              c.Query.MaxPageSize,
		"Cache.MaxTTLSeconds":            c.Cache.MaxTTLSeconds,
	}
	for name, v := range positive {
		if v <= 0 {
			t.Errorf("%s = %d，必须为正数（0 会让约束形同放开）", name, v)
		}
	}
	if c.Cache.KeyPrefix == "" {
		t.Error("Cache.KeyPrefix 为空：多环境共用 Redis 时会互相踩键")
	}
	if !c.Cache.DeleteOnBump {
		t.Error("Cache.DeleteOnBump 必须为 true：只递增 epoch 不删键会让错误放量最长挂一个 TTL")
	}
	if !c.Query.CountTotal {
		t.Error("Query.CountTotal 必须为 true：配置表是千级，契约里的 total 必须可信")
	}
}

// TestConfigWithinModelHardCaps 校验配置里的上限没有越过 model 的硬上限。
//
// 两侧的关系是「硬上限在代码（同时决定建表列宽与数组规模），配置只能在里面收紧」。
// 若允许配置放宽，就会出现「配置说可以 5000 条、SQL 列只装得下 500 条」这种
// 写入时报数据库错误、而错误信息对运营毫无意义的事故。
func TestConfigWithinModelHardCaps(t *testing.T) {
	var c Config
	file := filepath.Join("..", "..", "etc", "opsconfig.v1.yaml")
	if err := conf.Load(file, &c); err != nil {
		t.Fatalf("加载 %s 失败: %v", file, err)
	}
	caps := []struct {
		name   string
		config int
		hard   int
	}{
		{"OpsTopic.MaxItems", c.OpsTopic.MaxItems, model.MaxTopicItems},
		{"OpsTopic.MaxRefIDs", c.OpsTopic.MaxRefIDs, model.MaxTopicRefIDs},
		{"OpsSlot.MaxCapacity", c.OpsSlot.MaxCapacity, model.MaxSlotCapacityHard},
		{"OpsSlot.MaxItems", c.OpsSlot.MaxItems, model.MaxSlotItems},
		{"Rollout.MaxWhitelistMids", c.Rollout.MaxWhitelistMids, model.MaxWhitelistMids},
		{"Rollout.MaxRulesPerConfig", c.Rollout.MaxRulesPerConfig, model.MaxRolloutCandidates},
		{"Resolve.MaxBatchKeys", c.Resolve.MaxBatchKeys, model.MaxResolveKeys},
		// cfg_value 列类型是 TEXT（65535 字节），MaxBytes 是落在其中的运营上限。
		{"OpsValue.MaxBytes", c.OpsValue.MaxBytes, model.MaxCfgValueBytes},
		{"OpsValue.MaxReasonLen", c.OpsValue.MaxReasonLen, model.MaxReasonChars},
	}
	for _, item := range caps {
		if item.config > item.hard {
			t.Errorf("%s = %d 超过 model 硬上限 %d：配置只能收紧，不能放宽",
				item.name, item.config, item.hard)
		}
		if item.config <= 0 {
			t.Errorf("%s = %d，必须为正数", item.name, item.config)
		}
	}
	if c.Query.MaxPageSize > model.MaxPageSizeHard {
		t.Errorf("Query.MaxPageSize = %d 超过契约上限 %d", c.Query.MaxPageSize, model.MaxPageSizeHard)
	}
}

// forbiddenNameParts 是商业化/小程序范围的词根（AGENTS.md §1、§6、§7）。
// 本服务承载「内容分发配置」，配置里一旦出现这些词根，就说明有人把
// 广告位、投放排期、订单或会员权益塞进了运营配置——这些都在范围外。
var forbiddenNameParts = []string{
	"ad", "adslot", "campaign", "sponsor", "bid", "billing", "charge",
	"order", "payment", "pay", "coin", "revenue", "dividend", "commission",
	"vip", "member", "membership", "subscription", "premium", "miniprogram",
	"minipro", "weapp",
}

// TestNoCommercializationConfigFields 反射扫描 Config 的**全部**字段名（含嵌套结构体），
// 断言不存在商业化/小程序范围的字段。
// 用例子而不是 review 来守这条线：范围外字段一旦被合进配置，后面所有服务都会跟着长出来。
func TestNoCommercializationConfigFields(t *testing.T) {
	var c Config
	scanForbidden(t, reflect.ValueOf(c).Type(), "Config")
}

// TestNoCommercializationContractFields 反射扫描 RPC 契约里所有请求/响应消息的字段名。
// 覆盖的是 proto 一侧（AGENTS.md §1 的禁区首先是对外契约）。
func TestNoCommercializationContractFields(t *testing.T) {
	msgs := []any{
		rpc.ConfigView{}, rpc.PublishConfigReq{}, rpc.RollbackConfigReq{}, rpc.ConfigItem{}, rpc.ConfigVersion{},
		rpc.RolloutRule{}, rpc.RolloutRuleSpec{}, rpc.SaveRolloutRuleReq{}, rpc.ListRolloutRulesReq{},
		rpc.SetRolloutRuleStateReq{}, rpc.Topic{}, rpc.TopicItem{}, rpc.SaveTopicReq{}, rpc.GetTopicReq{},
		rpc.ListTopicsReq{}, rpc.SaveTopicItemsReq{}, rpc.RecommendSlot{}, rpc.SlotItem{}, rpc.SaveSlotReq{},
		rpc.ListSlotsReq{}, rpc.SaveSlotItemsReq{}, rpc.ResolveSlotReq{}, rpc.ClientSwitch{},
		rpc.SaveClientSwitchReq{}, rpc.ListClientSwitchesReq{}, rpc.RefreshCacheReq{}, rpc.TargetContext{},
		rpc.BatchResolveConfigReq{}, rpc.ListConfigsReq{}, rpc.ListConfigVersionsReq{},
	}
	for _, m := range msgs {
		scanForbidden(t, reflect.TypeOf(m), "")
	}
}

func scanForbidden(t *testing.T, typ reflect.Type, prefix string) {
	t.Helper()
	if typ == nil {
		return
	}
	for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		// 框架内嵌字段（RpcServerConf/ServiceConf）与 protobuf 运行时内部字段
		// （state/sizeCache/unknownFields）都不由本服务定义，跳过。
		if sf.Anonymous || !sf.IsExported() {
			continue
		}
		lower := strings.ToLower(sf.Name)
		path := prefix + typ.Name() + "." + sf.Name
		for _, bad := range forbiddenNameParts {
			if strings.Contains(lower, bad) && !allowed(lower, bad) {
				t.Errorf("%s 命中范围外词根 %q（AGENTS.md §1、§6、§7）", path, bad)
			}
		}
		switch sf.Type.Kind() {
		case reflect.Struct, reflect.Ptr, reflect.Slice:
			scanForbidden(t, sf.Type, path+".")
		}
	}
}

// allowed 收录「包含禁用词根但语义完全无关」的字段，必须逐条写明理由。
// 新增条目要在这里留下原因注释，否则这条守护测试就会退化成「报错了就加白名单」。
func allowed(field, part string) bool {
	switch {
	// 含 "ad"：与广告无关，是「加载/下载/校验/地址」这类普通词根。
	case part == "ad" && (strings.Contains(field, "upload") ||
		strings.Contains(field, "download") || strings.Contains(field, "validate") ||
		strings.Contains(field, "reload") || strings.Contains(field, "grade")):
		return true
	// 含 "bid"：与出价无关的普通词（本服务目前无此类字段，留作显式白名单位）。
	case part == "bid" && strings.Contains(field, "forbid"):
		return true
	}
	return false
}
