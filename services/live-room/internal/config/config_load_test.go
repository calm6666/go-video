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

// TestExampleConfigsLoad 用真实的 conf.Load 加载 etc/ 下每个示例配置。
//
// 本仓库出现过两类只能靠真实加载才能发现的问题：
//  1. Config 自带的缓存字段命名为 Redis，与 zrpc.RpcServerConf 内嵌的 RedisKeyConf 同名，
//     代码可编译但启动即报 "conflict key redis"；
//  2. yaml 写了 Config 里没有的键，或 Config 的必填键 yaml 没给，同样是启动才炸。
//
// 因此本用例不允许退化成「只 reflect 结构体」。
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
			checkServiceIdentity(t, c)
			checkDownstreamKeys(t, c)
			checkLiveRoomBounds(t, c)

			// 示例配置里的每个业务参数都必须等于代码默认值：
			// 不相等说明「yaml 少写了键、被默认值悄悄补上」，示例配置就失去了暴露真实取值的作用。
			if got, want := c.LiveRoom, exampleLiveRoomConf(); !reflect.DeepEqual(got, want) {
				t.Errorf("示例配置的 LiveRoom 与代码默认值不一致（多半是 yaml 漏写某个键）:\n got=%+v\nwant=%+v", got, want)
			}
			if got, want := c.Kafka, exampleKafkaConf(); !reflect.DeepEqual(got, want) {
				t.Errorf("示例配置的 Kafka 与声明值不一致: got=%+v want=%+v", got, want)
			}
		})
	}
}

// TestDefaultsMatchExampleConfig 反向锁死同一件事：
// 去掉全部业务段的最小配置，加载后必须得到与示例配置完全相同的领域参数，
// 这样「示例配置里写的值」和「结构体 tag 里的 default」不可能各自漂移。
func TestDefaultsMatchExampleConfig(t *testing.T) {
	minimal := `Name: liveroom.v1.rpc
ListenOn: 127.0.0.1:8119
Etcd:
  Hosts:
  - 127.0.0.1:2379
  Key: liveroom.v1.rpc
CacheRedis:
  Host: 127.0.0.1:6379
  Type: node
DataSource: root:root@tcp(127.0.0.1:3306)/go_video_live_room?charset=utf8mb4&parseTime=true&loc=Local
`
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatalf("写入最小配置失败: %v", err)
	}
	var c Config
	if err := conf.Load(path, &c); err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if got, want := c.LiveRoom, exampleLiveRoomConf(); !reflect.DeepEqual(got, want) {
		t.Errorf("默认值与示例配置不一致:\n got=%+v\nwant=%+v", got, want)
	}
	// 下游依赖全部 optional：未配置时 ServiceContext 不构造客户端，
	// logic 必须显式报 ErrXxxNotConfigured，而不是当成「检查通过」。
	for name, cc := range downstreamClients(c) {
		if rpcConfigured(cc) {
			t.Errorf("%s 未写配置却被判定为已配置，PrepareLive/送审会走错分支", name)
		}
	}
}

// exampleLiveRoomConf 是示例配置应当表达的完整取值（同时是代码默认值）。
func exampleLiveRoomConf() LiveRoomConf {
	return LiveRoomConf{
		MaxRoomsPerOwner:            1,
		MaxOwnerBindingsPerMid:      20,
		MaxCohostPerRoom:            8,
		MaxManagerPerRoom:           30,
		TitleMaxLength:              80,
		AreaNameMaxLength:           32,
		DefaultListPageSize:         20,
		MaxListPageSize:             100,
		MaxAreaPageSize:             200,
		StreamInterruptGraceSeconds: 120,
		PrepareCheckTTLSeconds:      60,
		RoomCacheTTLSeconds:         10,
		AreaListCacheTTLSeconds:     300,
		ModerationBusiness:          "live",
		IdempotencyRetentionDays:    30,
		StateLogRetentionDays:       365,
		SweepBatchLimit:             200,
		BanExpirySweepEnabled:       false,
	}
}

func exampleKafkaConf() KafkaConf {
	return KafkaConf{
		Enabled:         false,
		Brokers:         []string{"127.0.0.1:9092"},
		Group:           "live-room.v1",
		SubscribeTopics: []string{"live.state.v1"},
		MaxRetries:      5,
		Offset:          "last",
		Conns:           1,
		Consumers:       2,
		Processors:      4,
		ForceCommit:     false,
	}
}

// TestExampleKafkaBlockIsHonest 锁死示例配置对事件链路的三条声明：
//  1. Enabled 必须为 false：默认构建没链接 kq，示例里写 true 会让运维照抄后启动失败；
//  2. 订阅的 topic 只能是 live.state.v1：moderation.result.v1 在本仓库没有生产者，
//     订阅它等于把事件读出来再丢掉；
//  3. 示例配置不得出现明文口令/证书路径（AGENTS.md §4：密钥只进 Secret/Vault）。
func TestExampleKafkaBlockIsHonest(t *testing.T) {
	var c Config
	path := filepath.Join("..", "..", "etc", "liveroom.v1.yaml")
	if err := conf.Load(path, &c); err != nil {
		t.Fatalf("加载 %s 失败: %v", path, err)
	}
	if c.Kafka.Enabled {
		t.Error("示例配置的 Kafka.Enabled 必须为 false：默认构建不链接 kq，照抄这份配置起不来")
	}
	if got, want := c.Kafka.SubscribeTopics, []string{"live.state.v1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SubscribeTopics=%v，本服务的消费者只有 live.state.v1 一条映射，期望 %v", got, want)
	}
	if c.Kafka.Password != "" || c.Kafka.Username != "" || c.Kafka.CaFile != "" {
		t.Error("示例配置不得写 SASL 账号或证书路径，生产凭据只能由 Secret/Vault 注入")
	}
	if c.Kafka.MaxRetries <= 0 {
		t.Errorf("MaxRetries=%d 必须 > 0：没有尝试上限会让依赖故障的事件在分区里无限热循环", c.Kafka.MaxRetries)
	}
}

// checkServiceIdentity 锁定服务名与 etcd key：live-gateway 的示例配置已经按
// liveroom.v1.rpc 订阅本服务，改这个键等于把已接线的消费方直接打断。
func checkServiceIdentity(t *testing.T, c Config) {
	t.Helper()
	if c.Name != "liveroom.v1.rpc" {
		t.Errorf("Name=%q，必须与 etcd 注册键一致", c.Name)
	}
	if c.Etcd.Key != "liveroom.v1.rpc" {
		t.Errorf("Etcd.Key=%q，live-gateway 按 liveroom.v1.rpc 寻址，不可随意改", c.Etcd.Key)
	}
	if len(c.Etcd.Hosts) == 0 {
		t.Error("Etcd.Hosts 为空，服务注册不上去，下游只能静态 Endpoints 兜底")
	}
	if c.ListenOn != "0.0.0.0:8119" {
		t.Errorf("ListenOn=%q，8119 落在 cron 约定留给阶段 3-4 的 8114-8119 区间内，且避开 live-ingest 8118/live-media 8120", c.ListenOn)
	}
}

// checkDownstreamKeys 断言三个下游依赖的 etcd key 与各服务自身 yaml 里的 Key 逐字一致。
// 拼错的后果不是启动失败，而是「连到别的服务上」或者调用永远超时，属于最难排的一类配置错误。
func checkDownstreamKeys(t *testing.T, c Config) {
	t.Helper()
	want := map[string]string{
		"CreatorRPC":     "creator.v1.rpc",
		"RiskControlRPC": "risk-control.v1.rpc",
		"ModerationRPC":  "moderation.v1.rpc",
	}
	for name, cc := range downstreamClients(c) {
		if !rpcConfigured(cc) {
			t.Errorf("%s 未配置：示例配置必须展示真实下游依赖", name)
			continue
		}
		if cc.Etcd.Key != want[name] {
			t.Errorf("%s.Etcd.Key=%q，期望 %q（与下游服务自身 yaml 的 Etcd.Key 对齐）", name, cc.Etcd.Key, want[name])
		}
		if len(cc.Etcd.Hosts) == 0 && cc.Target == "" && len(cc.Endpoints) == 0 {
			t.Errorf("%s 既无 Etcd.Hosts 也无 Endpoints/Target，客户端无法寻址", name)
		}
		if !cc.NonBlock {
			t.Errorf("%s.NonBlock=false：下游未起会直接 panic 掉本服务启动；领域服务之间必须非阻塞", name)
		}
	}
}

func downstreamClients(c Config) map[string]zrpc.RpcClientConf {
	return map[string]zrpc.RpcClientConf{
		"CreatorRPC":     c.CreatorRPC,
		"RiskControlRPC": c.RiskControlRPC,
		"ModerationRPC":  c.ModerationRPC,
	}
}

// rpcConfigured 与 internal/svc/servicecontext.go 共用同一判定：
// 保证「测试认为已配置」与「svc 是否真的构造客户端」不会各说各话。
func rpcConfigured(c zrpc.RpcClientConf) bool {
	return c.Target != "" || len(c.Etcd.Hosts) > 0 || len(c.Endpoints) > 0
}

// checkLiveRoomBounds 校验房间/绑定上限与缓存 TTL 之间的边界关系。
// 这些值直接决定状态机与配额口径，配错会让校验形同虚设（0 或负数等于关掉防线）。
func checkLiveRoomBounds(t *testing.T, c Config) {
	t.Helper()
	lr := c.LiveRoom

	if lr.TitleMaxLength <= 0 || lr.TitleMaxLength > 80 {
		t.Errorf("TitleMaxLength=%d 必须落在 1..80（超过 live_room.title 列宽会写入失败）", lr.TitleMaxLength)
	}
	if lr.AreaNameMaxLength <= 0 || lr.AreaNameMaxLength > 32 {
		t.Errorf("AreaNameMaxLength=%d 必须落在 1..32（超过 live_area.area_name 列宽）", lr.AreaNameMaxLength)
	}
	if lr.DefaultListPageSize <= 0 || lr.DefaultListPageSize > lr.MaxListPageSize {
		t.Errorf("DefaultListPageSize=%d 必须落在 1..MaxListPageSize(%d)", lr.DefaultListPageSize, lr.MaxListPageSize)
	}
	if lr.MaxListPageSize <= 0 || lr.MaxAreaPageSize < lr.MaxListPageSize {
		t.Errorf("MaxAreaPageSize=%d 不应小于 MaxListPageSize=%d", lr.MaxAreaPageSize, lr.MaxListPageSize)
	}
	if lr.MaxCohostPerRoom <= 0 || lr.MaxManagerPerRoom <= lr.MaxCohostPerRoom {
		t.Errorf("连麦上限 %d 必须小于房管上限 %d，否则角色上限校验无意义", lr.MaxCohostPerRoom, lr.MaxManagerPerRoom)
	}
	if lr.MaxOwnerBindingsPerMid < lr.MaxRoomsPerOwner {
		t.Errorf("MaxOwnerBindingsPerMid=%d 不能小于 MaxRoomsPerOwner=%d", lr.MaxOwnerBindingsPerMid, lr.MaxRoomsPerOwner)
	}
	// 断流宽限期必须为正：0 表示「一断流就终止场次」，与直播端可容忍的重连行为直接冲突。
	if lr.StreamInterruptGraceSeconds <= 0 {
		t.Errorf("StreamInterruptGraceSeconds=%d 必须 > 0", lr.StreamInterruptGraceSeconds)
	}
	for name, ttl := range map[string]int{
		"PrepareCheckTTLSeconds":  lr.PrepareCheckTTLSeconds,
		"RoomCacheTTLSeconds":     lr.RoomCacheTTLSeconds,
		"AreaListCacheTTLSeconds": lr.AreaListCacheTTLSeconds,
	} {
		if ttl < 0 {
			t.Errorf("%s=%d 不能为负（0 表示关闭缓存，是合法值）", name, ttl)
		}
	}
	if lr.ModerationBusiness == "" {
		t.Error("ModerationBusiness 为空：送审时无法登记业务线，moderation 侧会拒单")
	}
	// 保留期必须长于任何合理的重试/重投窗口，否则幂等语义在窗口外失效。
	if lr.IdempotencyRetentionDays <= 0 {
		t.Errorf("IdempotencyRetentionDays=%d 必须 > 0：0 会让幂等记录被立即清理，重复扣状态风险回归", lr.IdempotencyRetentionDays)
	}
	if lr.StateLogRetentionDays < lr.IdempotencyRetentionDays {
		t.Errorf("StateLogRetentionDays=%d 不应短于 IdempotencyRetentionDays=%d：审计窗口必须覆盖重试窗口",
			lr.StateLogRetentionDays, lr.IdempotencyRetentionDays)
	}
	if lr.SweepBatchLimit <= 0 {
		t.Errorf("SweepBatchLimit=%d 必须 > 0，cron 扫描会退化成无 LIMIT 全表扫", lr.SweepBatchLimit)
	}
	// 本轮 logic 未实现：示例配置里不得出现「开关开了但没人执行」的状态。
	if lr.BanExpirySweepEnabled {
		t.Error("BanExpirySweepEnabled 在示例配置里必须为 false：到期推进逻辑本轮未实现")
	}
	if !strings.Contains(c.DataSource, "go_video_live_room") {
		t.Errorf("DataSource 必须指向本服务自有库 go_video_live_room，当前=%s", c.DataSource)
	}
	for _, forbidden := range []string{"sk-", "AKID", "-----BEGIN", "vault://"} {
		if strings.Contains(strings.ToLower(c.DataSource), strings.ToLower(forbidden)) {
			t.Errorf("DataSource 疑似包含真实凭据（命中 %q），生产密钥只能由 Secret/Vault 注入", forbidden)
		}
	}
}

var redisConfType = reflect.TypeOf(redis.RedisConf{})

// checkFields 递归检查配置：缓存宿主必填、库名必须是本服务自有 schema、示例配置不得含明文 Token。
func checkFields(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		sf := v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		f := v.Field(i)
		// 内嵌字段都是 go-zero 框架配置（RpcServerConf/ServiceConf/LogConf），
		// 其内部可选结构（如 zrpc 鉴权用的 RedisKeyInfo）留空是正常的，不检查。
		if sf.Anonymous {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			if strings.HasSuffix(sf.Name, "DataSource") && f.String() == "" {
				t.Errorf("%s 为空，服务启动后无法访问数据库", sf.Name)
			}
			// zrpc.RpcClientConf.Key 是服务发现键（Etcd.Key 例外，由 checkDownstreamKeys 精确断言）。
			if sf.Name == "Token" && f.String() != "" {
				t.Errorf("%s 不应在示例配置里写入明文 Token", sf.Name)
			}
		case reflect.Struct:
			if f.Type() == redisConfType {
				if f.Interface().(redis.RedisConf).Host == "" {
					t.Errorf("%s.Host 为空，缓存客户端无法构造", sf.Name)
				}
				continue
			}
			checkFields(t, f)
		case reflect.Slice:
			// Brokers/SubscribeTopics 等切片允许为空（表示未接线），但不得含空串元素。
			for j := 0; j < f.Len(); j++ {
				if f.Index(j).Kind() == reflect.String && f.Index(j).String() == "" {
					t.Errorf("%s[%d] 为空字符串，配置里不应保留空元素", sf.Name, j)
				}
			}
		}
	}
}
