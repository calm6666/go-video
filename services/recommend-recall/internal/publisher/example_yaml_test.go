package publisher

// example_yaml_test.go 把「示例配置」当成被测对象而不是文档：
// etc/recommendrecall.v1.yaml 是运维抄的唯一模板，它一旦与代码漂移，后果不是报错而是
// 三种静默失效，PublishTopics 写错（每一行都被判死）、Enabled 被写成 true
//（默认构建没链接发送端，进程启动即失败）、BatchLimit 被抬高到超过 model.MaxOutboxBatch
//（发布器每轮读库报错，看起来在跑其实一条发不出去）。
// 所以这里用真实的 conf.Load 读它，并逐键比对期望值。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"

	"go-video/common/eventenvelope"
	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/model"
)

// exampleYamlPath 是本服务唯一的示例配置。
const exampleYamlPath = "../../etc/recommendrecall.v1.yaml"

// loadExampleConfig 用真实的 conf.Load 读示例配置（不是手写结构字面量）：
// yaml 里多写一个 Config 没有的键、或少写一个必填键，都只有真加载才会暴露。
func loadExampleConfig(t *testing.T) config.Config {
	t.Helper()
	var c config.Config
	if err := conf.Load(filepath.FromSlash(exampleYamlPath), &c); err != nil {
		t.Fatalf("加载 %s 失败: %v", exampleYamlPath, err)
	}
	return c
}

// TestExampleYamlIsOneFlipFromPublishing 钉住示例配置与发布器之间的关系：
// Enabled=false 是刻意的默认（默认构建没链接队列发送端），但除它以外每个键都必须填好，
// 否则「打开开关就能投」是假的，运维要一边开开关一边补键。
func TestExampleYamlIsOneFlipFromPublishing(t *testing.T) {
	c := loadExampleConfig(t)
	if c.Kafka.Enabled {
		t.Fatal("示例配置必须保持 Kafka.Enabled=false：默认构建没链接队列发送端，" +
			"把它设为 true 会让进程启动即失败，示例配置不能教人踩这个坑")
	}
	if err := ValidatePublishKafka(c.Kafka); err != nil {
		t.Fatalf("示例配置的发布参数不合格（打开 Enabled 也发不出去）：%v", err)
	}
	// 逐元素比顺序：etc 里的声明顺序就是 model 派生的 topic，
	// 漂移了就要人来猜是漏登记还是重排。
	if got, want := strings.Join(c.Kafka.PublishTopics, ","), strings.Join(RequiredTopics(), ","); got != want {
		t.Errorf("Kafka.PublishTopics=%s，期望与 model 派生的 topic 逐个同序：%s", got, want)
	}
	if got := strings.Join(c.Kafka.Brokers, ","); got != "127.0.0.1:9092" {
		t.Errorf("Kafka.Brokers=%q，期望本地 Redpanda 地址", got)
	}
	// 批次必须留在 model.CheckLimit 放行的范围内：这条不是风格问题，
	// 超限的 BatchLimit 会让 ListPending 每轮返回错误（见 ValidatePublishKafka）。
	if int(c.Kafka.BatchLimit) > model.MaxOutboxBatch {
		t.Errorf("Kafka.BatchLimit=%d 超过 model.MaxOutboxBatch=%d", c.Kafka.BatchLimit, model.MaxOutboxBatch)
	}
	// 六个旋钮的字面量与 etc 注释里承诺的语义同源：改 yaml 必须同时改这里的期望。
	want := Options{
		Name:        label,
		Interval:    2 * time.Second,
		Batch:       100,
		MaxAttempts: 5,
		BaseBackoff: 2 * time.Second,
		MaxBackoff:  1800 * time.Second,
		SendTimeout: 5 * time.Second,
	}
	if got := OptionsFrom(c.Kafka); got != want {
		t.Errorf("示例配置映射出的发布参数=%+v，期望 %+v", got, want)
	}
}

// TestPublisherBuiltFromExampleYaml 用示例配置 + 假 model + 假发送端走完整装配路径，
// 钉住 svc 入口在真实参数下能构造、能投递（NewPublisher 不看 Enabled，
// Enabled 只决定 svc 是否启动循环）。
func TestPublisherBuiltFromExampleYaml(t *testing.T) {
	c := loadExampleConfig(t)
	m := newFakeOutbox(poolPublishedRow(t, 51, "01EVENTIDAAA0000000000000K"))
	sender := &fakeSender{}
	pub, err := NewPublisher(c, m, sender)
	if err != nil {
		t.Fatalf("NewPublisher(示例配置): %v", err)
	}
	if got := pub.Options(); got != OptionsFrom(c.Kafka) {
		t.Errorf("发布参数没透传：%+v", got)
	}
	before := time.Now().Unix()
	handled, err := pub.RunOnce(context.Background())
	after := time.Now().Unix()
	if err != nil || handled != 1 {
		t.Fatalf("RunOnce=(%d,%v)，期望 (1,nil)", handled, err)
	}
	sent := sender.calls()
	wantTopic := eventenvelope.Topic(model.EventPoolPublished, model.PoolVersionSchemaVersion)
	if len(sent) != 1 || sent[0].topic != wantTopic {
		t.Fatalf("投递=%+v，期望恰好一条且落在 %s", sent, wantTopic)
	}
	// 分区键是 aggregate_id（source:pool_key:version）：同池同版本的事件必须落同一分区，
	// 用 event_id 做键会把它们打散。
	if sent[0].key != "1:hot:51" {
		t.Errorf("分区键=%q，期望 aggregate_id \"1:hot:51\"", sent[0].key)
	}
	if sent[0].timeout <= 0 || sent[0].timeout > 5*time.Second {
		t.Errorf("单条投递超时=%s，期望按 Kafka.SendTimeoutSec=5s 生效", sent[0].timeout)
	}
	// 真实时钟下位点必须落在调用前后区间内（本用例不注入时钟，就是为了验默认装配）。
	if got := m.publishAt[51]; got < before || got > after {
		t.Errorf("已发布位点=%d，不在 [%d,%d] 内", got, before, after)
	}
}

// TestExampleYamlKeepsOtherSectionsLoadable 防住一件与发布无关但同文件的风险：
// 给 Kafka 补键时把别的段落改坏（缩进/键名/新增未定义键），会让整个服务启动失败。
// 这里只确认这些段落仍按 yaml 原意加载，不重复 logic 侧的语义测试。
func TestExampleYamlKeepsOtherSectionsLoadable(t *testing.T) {
	c := loadExampleConfig(t)
	if c.Name == "" || c.ListenOn == "" {
		t.Errorf("RpcServerConf 缺字段：Name=%q ListenOn=%q", c.Name, c.ListenOn)
	}
	if c.DataSource == "" {
		t.Error("DataSource 为空会让服务启动即失败，示例配置不能留这种坑")
	}
	// 全仓统一 CacheRedis：写成 Redis 会与 zrpc.RpcServerConf 内嵌字段撞名（conf.Load 直接报错）。
	if c.CacheRedis.Host == "" {
		t.Error("CacheRedis.Host 为空：池快照与召回结果缓存都依赖它")
	}
	// 召回路开关组合的自洽性由 Recall.Validate 判定，示例配置必须是它放行的一份：
	// 兜底路或冷启动路指向未启用的路时，降级会在最该出数的时候失败。
	if err := c.Recall.Validate(); err != nil {
		t.Errorf("示例配置的召回参数不自洽：%v", err)
	}
	if len(c.Recall.EnabledSources) == 0 {
		t.Error("EnabledSources 有意不提供默认值：示例配置必须显式写出召回路组合")
	}
	// 幂等租约必须小于保留期，否则「租约还没过、记录已被清掉」会让写接口重复执行。
	if c.Recall.IdempotencyRetentionSeconds <= c.Recall.IdempotencyLeaseSeconds {
		t.Errorf("IdempotencyRetentionSeconds=%d 必须大于 IdempotencyLeaseSeconds=%d",
			c.Recall.IdempotencyRetentionSeconds, c.Recall.IdempotencyLeaseSeconds)
	}
}
