// queue.go 定义消息队列的接入抽象与生命周期管理。
//
// 抽象出 QueueFactory 而不是直接在新代码里调 kq：
//   - 消费逻辑（consumer.go 的状态机、retry.go 的清扫）与 Kafka 客户端解耦，
//     单测用假工厂驱动 Handler，不需要 broker、也不需要真实网络；
//   - Kafka 客户端只出现在 kafkaruntime_kafka.go（-tags inbox_kafka）一个文件里，
//     替换 MQ 实现时的改动面是可数的；默认构建见 kafkaruntime_stub.go。
package consumer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/threading"

	"go-video/services/inbox/internal/config"
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
	// Name/Log/Mode 复用主服务的日志设置：kq 内部会调用 ServiceConf.SetUp，
	// 留空会让它用默认值重设整个进程的全局 logger（日志突然改成写文件）。
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

// QueueFactory 为某个 topic 创建一个消费者。
type QueueFactory interface {
	New(s Settings, topic string, h *Handler) (MessageQueue, error)
}

// Supervisor 管理多个 topic 的消费者 + 退避重投循环。
type Supervisor struct {
	settings  Settings
	topics    []string
	factory   QueueFactory
	proc      *Processor
	queues    []MessageQueue
	stopSweep context.CancelFunc
	started   bool
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
func ValidateKafka(k config.KafkaConf) error {
	var missing []string
	if len(k.Brokers) == 0 {
		missing = append(missing, "Kafka.Brokers 为空（本地 Redpanda 应为 [localhost:9092]）")
	}
	if strings.TrimSpace(k.Group) == "" {
		missing = append(missing, "Kafka.Group 为空")
	}
	if len(k.EffectiveTopics()) == 0 {
		missing = append(missing, "Kafka.Topics 为空且未配置默认 topic")
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
	if k.MaxAttempts <= 0 {
		missing = append(missing, "Kafka.MaxAttempts 必须大于 0")
	}
	if k.RetryBaseSeconds <= 0 {
		missing = append(missing, "Kafka.RetryBaseSeconds 必须大于 0")
	}
	if k.StaleProcessingSeconds <= 0 {
		missing = append(missing, "Kafka.StaleProcessingSeconds 必须大于 0")
	}
	if (k.Username == "") != (k.Password == "") {
		missing = append(missing, "Kafka.Username 与 Kafka.Password 必须成对配置")
	}
	if len(missing) > 0 {
		return fmt.Errorf("inbox: Kafka 消费配置不完整: %s", strings.Join(missing, "; "))
	}
	return nil
}

// NewSupervisor 构造消费者编排器。store 通常是 *repository.Repository，
// factory 生产用 NewKqFactory()，单测注入假工厂。
// 配置不完整时返回错误，调用方必须显式失败（见 internal/svc）。
func NewSupervisor(c config.Config, store Store, factory QueueFactory) (*Supervisor, error) {
	if store == nil {
		return nil, errors.New("inbox: consumer store is required")
	}
	if factory == nil {
		return nil, errors.New("inbox: consumer queue factory is required")
	}
	if err := ValidateKafka(c.Kafka); err != nil {
		return nil, err
	}
	topics := c.Kafka.EffectiveTopics()
	// 同名 topic 去重：重复订阅会让同一条消息被处理两遍（虽然幂等能兜住，但白烧一倍算力）。
	seen := make(map[string]struct{}, len(topics))
	unique := make([]string, 0, len(topics))
	for _, topic := range topics {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			return nil, fmt.Errorf("inbox: Kafka.Topics 含空串")
		}
		if _, dup := seen[topic]; dup {
			continue
		}
		seen[topic] = struct{}{}
		unique = append(unique, topic)
	}
	return &Supervisor{
		settings: SettingsFrom(c),
		topics:   unique,
		factory:  factory,
		proc:     NewProcessor(store, OptionsFrom(c.Kafka)),
	}, nil
}

// Processor 暴露处理器，供单测与运维接口复用。
func (s *Supervisor) Processor() *Processor { return s.proc }

// Topics 返回实际订阅的 topic 列表。
func (s *Supervisor) Topics() []string { return s.topics }

// Start 为每个 topic 建立消费者并启动退避重投循环。
// 任一步失败都会回滚已启动的队列，不留半启动状态。
func (s *Supervisor) Start(ctx context.Context) error {
	if s.started {
		return errors.New("inbox/consumer: supervisor already started")
	}
	queues := make([]MessageQueue, 0, len(s.topics))
	for _, topic := range s.topics {
		q, err := s.factory.New(s.settings, topic, NewHandler(topic, s.proc))
		if err != nil {
			stopAll(queues)
			return fmt.Errorf("inbox/consumer: create consumer for topic %s: %w", topic, err)
		}
		if q == nil {
			stopAll(queues)
			return fmt.Errorf("inbox/consumer: nil consumer for topic %s", topic)
		}
		queues = append(queues, q)
	}
	for _, q := range queues {
		q.Start()
	}
	s.queues = queues
	s.started = true // 必须在 Start 成功后置位：否则重复 Start 会再起一套消费者，同一事件被双份处理

	sweepCtx, cancel := context.WithCancel(context.Background())
	s.stopSweep = cancel
	threading.GoSafe(func() {
		if err := s.proc.RunRetrySweeper(sweepCtx); err != nil && !errors.Is(err, context.Canceled) {
			logx.Errorf("inbox/consumer: 重试清扫退出 err=%v", err)
		}
	})
	logx.Infof("inbox/consumer: 已启动 topics=%v group=%s brokers=%v force_commit=%v",
		s.topics, s.settings.Group, s.settings.Brokers, s.settings.ForceCommit)
	return nil
}

// Stop 先停退避重投循环，再逐个关闭队列（kq 的 Stop 会等待在途消息）。
func (s *Supervisor) Stop() {
	if s.stopSweep != nil {
		s.stopSweep()
		s.stopSweep = nil
	}
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
