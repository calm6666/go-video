package consumer

// example_yaml_test.go 把 etc/livemedia.v1.yaml 当成消费者的一半来测。
//
// 这份 yaml 是运维抄的唯一模板，「打开 Enabled 就能自动下线档位」必须是真话：
// 少一个消费键的后果不是报错而是进程启动即失败（ValidateKafka 拒），
// 或者消费者建好了却订阅了一个本包没有映射的 topic（一条消息都不来，静默得像没接）。
// 所以这里用真实 conf.Load 读它，逐键比对，再用假工厂走一遍 NewSupervisor：
// 全程不触网，也不声称与任何 broker 通过。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"

	"go-video/services/live-media/internal/config"
)

const exampleYamlPath = "../../etc/livemedia.v1.yaml"

func loadExampleConfig(t *testing.T) config.Config {
	t.Helper()
	var c config.Config
	// 与 internal/publisher/example_yaml_test.go 读同一个文件：
	// 两个方向共用一段 Kafka 配置，任何一侧改键名都会让另一侧红。
	if err := conf.Load(filepath.FromSlash(exampleYamlPath), &c); err != nil {
		t.Fatalf("加载 %s 失败: %v", exampleYamlPath, err)
	}
	return c
}

// TestExampleYamlIsOneFlipFromConsuming 断言示例配置的消费参数已完整合格。
func TestExampleYamlIsOneFlipFromConsuming(t *testing.T) {
	c := loadExampleConfig(t)
	if c.Kafka.Enabled {
		t.Fatal("示例配置必须保持 Kafka.Enabled=false：默认构建没链接队列运行时，" +
			"置 true 会让 NewKqFactory/Start 直接失败，模板不能教人踩这个坑")
	}
	if err := ValidateKafka(c.Kafka); err != nil {
		t.Fatalf("示例配置的消费参数不合格（打开 Enabled 也消费不了）：%v", err)
	}
	if got := strings.Join(EffectiveTopics(c.Kafka), ","); got != SupportedTopic {
		t.Errorf("Kafka.SubscribeTopics=%q，期望只有 %s", got, SupportedTopic)
	}
	if c.Kafka.Group != "live-media.v1" {
		t.Errorf("Kafka.Group=%q，期望 live-media.v1：消费组名与 topic 命名同源，便于 broker 侧排障", c.Kafka.Group)
	}
	// 消费循环的五个旋钮逐键钉死：README「配置 key」表写的默认值与 etc 注释必须同源。
	if c.Kafka.Offset != "last" || c.Kafka.Conns != 1 || c.Kafka.Consumers != 2 ||
		c.Kafka.Processors != 4 || c.Kafka.ForceCommit {
		t.Errorf("消费循环参数与模板承诺不一致（期望 last/1/2/4/false）：%+v", c.Kafka)
	}
	if c.Kafka.MaxRetries != 5 {
		t.Errorf("Kafka.MaxRetries=%d，模板承诺 5：它同时是发布判死上限与本包尝试上限", c.Kafka.MaxRetries)
	}
	// 凭据一律留空：模板里写死任何 SASL/TLS 值都会被抄进生产。
	if c.Kafka.Username != "" || c.Kafka.Password != "" || c.Kafka.CaFile != "" {
		t.Errorf("示例配置不得携带 SASL/TLS 取值（应进 Secret/Vault）：%q %q %q",
			c.Kafka.Username, c.Kafka.Password, c.Kafka.CaFile)
	}
}

// TestSupervisorAssemblesFromExampleConfig 用假工厂装配真实模板：
// 证明「这份 yaml 能建出订阅 live.state.v1 的消费者，且尝试上限来自 Kafka.MaxRetries」。
// 工厂是替身，所以这条只证配置层，不证 broker 侧的分区与位点语义。
func TestSupervisorAssemblesFromExampleConfig(t *testing.T) {
	c := loadExampleConfig(t)
	f := newFakeFactory()
	sup, err := NewSupervisor(c, countApplicator(), f)
	if err != nil {
		t.Fatalf("按示例配置装配失败: %v", err)
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("按示例配置启动失败: %v", err)
	}
	defer sup.Stop()

	if got := sup.Topics(); len(got) != 1 || got[0] != SupportedTopic {
		t.Fatalf("订阅 topic 不符: %v", got)
	}
	if got := sup.Handler().Options().MaxAttempts; got != c.Kafka.MaxRetries {
		t.Fatalf("Handler 尝试上限应为 Kafka.MaxRetries=%d，实得 %d", c.Kafka.MaxRetries, got)
	}
	calls := f.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("应建立 1 个消费者: %d", len(calls))
	}
	if !strings.Contains(calls[0].settings.Name, "livemedia") {
		t.Fatalf("Settings.Name 必须是本服务名（kq.NewQueue 会拿它重设全局 logger）：%q", calls[0].settings.Name)
	}
	// 日志设置必须与主服务同源：留空会让 kq 用它的默认值改掉整个进程的输出方式。
	if calls[0].settings.Log.Mode != c.Log.Mode || calls[0].settings.Mode != c.Mode {
		t.Fatalf("Settings 的日志身份未与主服务同源: 主 log=%q mode=%q，队列 log=%q mode=%q",
			c.Log.Mode, c.Mode, calls[0].settings.Log.Mode, calls[0].settings.Mode)
	}
}
