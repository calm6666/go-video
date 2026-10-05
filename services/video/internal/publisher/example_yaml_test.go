package publisher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"

	"go-video/services/video/internal/config"
)

// exampleYamlPath 是本服务唯一的示例配置。
const exampleYamlPath = "../../etc/video.v1.yaml"

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
// Enabled=false 是刻意的默认（默认构建没链接发送端），但除它以外每个键都必须填好，
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
	if got := strings.Join(c.Kafka.PublishTopics, ","); got != RequiredTopic() {
		t.Errorf("Kafka.PublishTopics=%q，期望恰好 %s（多写的 topic 本服务不产出）", got, RequiredTopic())
	}
	if got := strings.Join(c.Kafka.Brokers, ","); got != "127.0.0.1:9092" {
		t.Errorf("Kafka.Brokers=%q，期望本地 Redpanda 地址", got)
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
	m := newFakeOutbox(pendingRow(t, 61, "01EXAMPLEEVENT", "101"))
	sender := &fakeSender{}
	pub, err := NewPublisher(c, m, sender)
	if err != nil {
		t.Fatalf("NewPublisher(示例配置): %v", err)
	}
	if got := pub.Options(); got != OptionsFrom(c.Kafka) {
		t.Errorf("发布参数没透传：%+v", got)
	}
	// SendTimeout 真的落到每次投递的上下文上：示例值 5s 若被吞掉，
	// 一条卡死的 broker 连接就能把整个发布循环挂住。
	before := time.Now().Unix()
	handled, err := pub.RunOnce(context.Background())
	after := time.Now().Unix()
	if err != nil || handled != 1 {
		t.Fatalf("RunOnce=(%d,%v)，期望 (1,nil)", handled, err)
	}
	sent := sender.calls()
	if len(sent) != 1 || sent[0].topic != RequiredTopic() || sent[0].key != "101" {
		t.Fatalf("投递不符：%+v", sent)
	}
	if sent[0].timeout <= 0 || sent[0].timeout > 5*time.Second {
		t.Errorf("单条投递超时=%s，期望按 Kafka.SendTimeoutSec=5s 生效", sent[0].timeout)
	}
	// 真实时钟下位点必须落在调用前后区间内（本用例不注入时钟，就是为了验默认装配）。
	if got := m.publishAt[61]; got < before || got > after {
		t.Errorf("已发布位点=%d，不在 [%d,%d] 内", got, before, after)
	}
}

// TestExampleYamlKeepsStorageConfigSane 防住一件与发布无关但同文件的风险：
// 给 Kafka 补键时把上面的 Redis/MySQL 段落改坏（缩进、键名、把示例 DSN 换成远端库），
// 会让整个服务启动失败或悄悄连到别人的实例。yaml 能否加载由
// internal/config/config_load_test.go 负责，这里只钉 startup 必须成立的取值。
func TestExampleYamlKeepsStorageConfigSane(t *testing.T) {
	c := loadExampleConfig(t)
	if c.Name != "video.v1.rpc" || c.ListenOn != "0.0.0.0:8095" {
		t.Errorf("服务名/端口=%q %q，示例配置改动会让网关按 Key 找不到本服务", c.Name, c.ListenOn)
	}
	if c.CacheRedis.Host == "" {
		t.Error("CacheRedis.Host 为空：zrpc 的内嵌 Redis 与业务 CacheRedis 撞名时也会加载失败，这里要能看出来")
	}
	// DSN 必须留在本机的示例库，且必须指向本服务自己拥有的 schema（AGENTS.md §5）。
	if !strings.Contains(c.DataSource, "127.0.0.1") {
		t.Errorf("DataSource 指向非本机地址，示例配置不得引导进程连远端库：%s",
			strings.ReplaceAll(c.DataSource, "root:root", "<redacted>"))
	}
	if !strings.Contains(c.DataSource, "go_video_video") {
		t.Errorf("DataSource 未指向 video 自有 schema，跨库连接等于越权写别人的主数据：%s",
			strings.ReplaceAll(c.DataSource, "root:root", "<redacted>"))
	}
}
