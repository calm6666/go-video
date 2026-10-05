// queue.go 定义消息队列的接入抽象与生命周期管理。
//
// 抽象出 QueueFactory 而不是在这里直接调 kq：
//   - 事件翻译与处理判定（mapping.go / handler.go）与 Kafka 客户端解耦，单测用假工厂驱动 Handler，
//     不需要 broker，也不需要真实网络；
//   - Kafka 客户端只出现在 kafkaruntime_kafka.go（-tags livemedia_kafka）一个文件里，
//     替换 MQ 实现时的改动面是可数的；默认构建见 kafkaruntime_disabled.go。
//
// 注意本服务的 `-tags livemedia_kafka` 同时覆盖发布侧（internal/publisher）与消费侧（本包）：
// 一个标签、两条链路，`Kafka.Enabled` 也是两条链路共用的总开关（见 wiring.go 与 RuntimeNotes）。
package consumer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/live-media/internal/config"
)

// MessageQueue 一个消费者的生命周期，与 kq 返回的 queue.MessageQueue 结构一致。
type MessageQueue interface {
	// Start 非阻塞启动拉取与处理协程。
	Start()
	// Stop 停止并等待在途消息处理完。
	Stop()
}

// Settings 建立一次 Kafka 连接需要的参数。
type Settings struct {
	// Name/Log/Mode 复用主服务的日志设置：kq.NewQueue 内部会调 ServiceConf.SetUp，
	// 留空会让它用自己的默认值重设整个进程的全局 logger（日志突然改成写文件）。
	Name string
	Log  logx.LogConf
	Mode string

	Brokers     []string
	Group       string
	Offset      string
	Conns       int
	Consumers   int
	Processors  int
	ForceCommit bool
	Username    string
	Password    string
	CaFile      string
}

// QueueFactory 为某个 topic 建立一个消费者。
type QueueFactory interface {
	New(s Settings, topic string, h *Handler) (MessageQueue, error)
}

// Supervisor 管理订阅 topic 的消费者。
type Supervisor struct {
	settings Settings
	topics   []string
	factory  QueueFactory
	handler  *Handler
	queues   []MessageQueue
	started  bool
}

// SettingsFrom 由服务配置推导队列参数。
func SettingsFrom(c config.Config) Settings {
	k := c.Kafka
	return Settings{
		Name:        c.Name,
		Log:         c.Log,
		Mode:        c.Mode,
		Brokers:     k.Brokers,
		Group:       k.Group,
		Offset:      k.Offset,
		Conns:       k.Conns,
		Consumers:   k.Consumers,
		Processors:  k.Processors,
		ForceCommit: k.ForceCommit,
		Username:    k.Username,
		Password:    k.Password,
		CaFile:      k.CaFile,
	}
}

// ValidateKafka 校验消费配置，错误里点名具体配置键，避免「配置没写对就静默不消费」。
//
// 特别的一条：订阅的 topic 必须是本包有映射的那一个。livemedia.*.v1 那 9 条是本服务的
// **出站** topic，订阅它们等于把自己的事件读回来再丢掉（还会和自己的发布循环组成回环）；
// content.published.v1 这类「消费者已有、生产者还没有」的 topic 同理。
// 因此在这里直接拒绝而不是留成运行期告警。
func ValidateKafka(k config.KafkaConf) error {
	var missing []string
	if !hasUsableBroker(k.Brokers) {
		missing = append(missing, "Kafka.Brokers 为空（本地 Redpanda 应为 [127.0.0.1:9092]）")
	}
	if strings.TrimSpace(k.Group) == "" {
		missing = append(missing, "Kafka.Group 为空")
	}
	topics := EffectiveTopics(k)
	if len(topics) == 0 {
		missing = append(missing, fmt.Sprintf("Kafka.SubscribeTopics 为空（本服务只消费 %s）", SupportedTopic))
	}
	for _, t := range topics {
		if t != SupportedTopic {
			missing = append(missing, fmt.Sprintf(
				"Kafka.SubscribeTopics 含 %s：本服务只有 %s 的映射，订阅它等于读出来再丢掉", t, SupportedTopic))
		}
	}
	if k.Offset != "first" && k.Offset != "last" {
		missing = append(missing, "Kafka.Offset 只能是 first 或 last")
	}
	if k.Conns <= 0 {
		missing = append(missing, "Kafka.Conns 必须大于 0")
	}
	if k.Consumers <= 0 {
		missing = append(missing, "Kafka.Consumers 必须大于 0")
	}
	if k.Processors <= 0 {
		missing = append(missing, "Kafka.Processors 必须大于 0")
	}
	if k.MaxRetries <= 0 {
		missing = append(missing, "Kafka.MaxRetries 必须大于 0（没有尝试上限会让故障事件无限热循环）")
	}
	if (k.Username == "") != (k.Password == "") {
		missing = append(missing, "Kafka.Username 与 Kafka.Password 必须成对配置")
	}
	// kq 读到不可用的 CaFile 会直接 log.Fatal 打死进程（见 go-queue@v1.2.2 kq/queue.go），
	// 因此这里提前以可诊断的错误拒绝，而不是让进程在没有日志的情况下消失。
	if k.CaFile != "" {
		if _, err := os.ReadFile(k.CaFile); err != nil {
			missing = append(missing, fmt.Sprintf("Kafka.CaFile=%s 不可读: %v", k.CaFile, err))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("live-media: Kafka 消费配置不完整: %s", strings.Join(missing, "; "))
	}
	return nil
}

// hasUsableBroker 判断 broker 列表里是否至少有一个非空白地址。
// yaml 写成 `- ""` 时切片长度不为 0，但客户端只会拨号空地址，
// 表现是「启动成功、一直在重连」，因此等同于没配。
func hasUsableBroker(brokers []string) bool {
	for _, b := range brokers {
		if strings.TrimSpace(b) != "" {
			return true
		}
	}
	return false
}

// EffectiveTopics 返回去空白、去重后的订阅列表（保持配置里的书写顺序）。
func EffectiveTopics(k config.KafkaConf) []string {
	seen := make(map[string]struct{}, len(k.SubscribeTopics))
	out := make([]string, 0, len(k.SubscribeTopics))
	for _, t := range k.SubscribeTopics {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// NewSupervisor 构造消费者编排器。app 生产上由 wiring.go 接到 OfflineSessionOutputs logic，
// factory 生产用 NewKqFactory()，单测注入假工厂。配置不完整时返回错误，调用方必须显式失败。
func NewSupervisor(c config.Config, app Applicator, factory QueueFactory) (*Supervisor, error) {
	if app == nil {
		return nil, errors.New("livemedia/consumer: applicator is required")
	}
	if factory == nil {
		return nil, errors.New("livemedia/consumer: queue factory is required")
	}
	if err := ValidateKafka(c.Kafka); err != nil {
		return nil, err
	}
	topics := EffectiveTopics(c.Kafka)
	return &Supervisor{
		settings: SettingsFrom(c),
		topics:   topics,
		factory:  factory,
		handler:  NewHandler(app, OptionsFrom(c.Kafka)),
	}, nil
}

// Handler 暴露处理器，供单测与运维接口复用（计数与重试台账都在它身上）。
func (s *Supervisor) Handler() *Handler { return s.handler }

// Topics 返回实际订阅的 topic 列表。
func (s *Supervisor) Topics() []string { return s.topics }

// Started 表示消费者是否已经在跑，供启动日志与单测断言。
func (s *Supervisor) Started() bool { return s.started }

// Start 为每个 topic 建立消费者并启动。
// 任一步失败都会回滚已启动的队列，不留半启动状态。
func (s *Supervisor) Start(ctx context.Context) error {
	if s.started {
		return errors.New("livemedia/consumer: supervisor already started")
	}
	queues := make([]MessageQueue, 0, len(s.topics))
	for _, topic := range s.topics {
		q, err := s.factory.New(s.settings, topic, s.handler)
		if err != nil {
			stopAll(queues)
			return fmt.Errorf("livemedia/consumer: create consumer for topic %s: %w", topic, err)
		}
		if q == nil {
			stopAll(queues)
			return fmt.Errorf("livemedia/consumer: nil consumer for topic %s", topic)
		}
		queues = append(queues, q)
	}
	for _, q := range queues {
		q.Start()
	}
	s.queues = queues
	s.started = true // 必须在每个 topic 都建好之后才置位：半途失败时不能宣称已启动

	logx.WithContext(ctx).Infof("livemedia/consumer: 已启动 topics=%v group=%s brokers=%v force_commit=%v max_attempts=%d",
		s.topics, s.settings.Group, s.settings.Brokers, s.settings.ForceCommit, s.handler.Options().MaxAttempts)
	return nil
}

// Stop 逐个关闭队列（kq 的 Stop 会等待在途消息）。可重复调用。
func (s *Supervisor) Stop() {
	stopAll(s.queues)
	s.queues = nil
	s.started = false
}

// stopAll 逆序关闭，保证先启动的最后关。
func stopAll(queues []MessageQueue) {
	for i := len(queues) - 1; i >= 0; i-- {
		queues[i].Stop()
	}
}
