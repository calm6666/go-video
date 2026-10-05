// queue.go 定义 Kafka 读取端的接入抽象与消费者生命周期。
//
// 为什么抽象成 QueueFactory 而不是在 svc 里直接调 kq：
//   - 消费判定、退避与死信状态机（consumer.go、retry.go）不依赖任何 MQ 客户端，
//     单测用假工厂驱动 Handler，既不需要 broker 也不需要网络；
//   - Kafka 客户端只出现在 kafkaruntime_kafka.go（`-tags searchindexer_kafka`）一个文件里，
//     默认构建改由 kafkaruntime_disabled.go 显式返回 ErrKafkaRuntimeNotBuilt。
//
// 两条路径都只到「编译/静态检查/单测」级证据：本仓库从未与真实 broker 联调，
// 「链接了 kq」不等于「消费语义可用」，口径见 services/search-indexer/README.md「已知缺口」1。
package consumer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/search-indexer/internal/config"
)

// MessageQueue 一个 topic 消费者的生命周期，与 kq 的 queue.MessageQueue 结构一致。
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

// Handler 是绑定 topic 的消费处理器，直接满足 kq.ConsumeHandler。
type Handler struct {
	topic string
	c     *Consumer
}

// NewHandler 构造 Handler。c 为 nil 时 Consume 直接报错，不做静默成功。
func NewHandler(topic string, c *Consumer) *Handler {
	return &Handler{topic: topic, c: c}
}

// Topic 返回绑定的 topic。
func (h *Handler) Topic() string { return h.topic }

// Consume 是消息队列的回调入口。
//
// 返回值直接决定 broker 位点：ForceCommit=false 时返回非 nil 就不提交，Kafka 会重投。
// 重投是安全的：同一条消息第二次进来时 search_consumer_offset 已按 event_id 唯一索引落行，
// ProcessMessage 判定为重复并返回 nil，于是位点前进、退避重试交给本进程的清扫器
// （见 Consumer.retryLater 与 RunRetrySweeper）。也就是说本服务把「重试」的权威放在 MySQL，
// broker 重投只起「别把消息丢掉」的兜底作用，不会造成第二轮写入。
//
// key 当前不参与判定（幂等只看信封里的 event_id）。
func (h *Handler) Consume(ctx context.Context, key, value string) error {
	if h.c == nil {
		return errors.New("search-indexer/consumer: handler has no consumer bound")
	}
	// partition/offset 不在 kq 的回调参数里：流水表这两列只用于排障定位，
	// 幂等与状态推进完全按 event_id（deploy/migrations/search-indexer/000002_*.sql:52-53
	// 的列注释就写着「位点提交由读取端适配器负责」），因此这里保持 0，不伪造位点。
	return h.c.ProcessMessage(ctx, &Message{Topic: h.topic, Key: []byte(key), Value: []byte(value)})
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
		missing = append(missing, "Kafka.Brokers 为空（本地 Redpanda 应为 [127.0.0.1:9092]）")
	}
	if strings.TrimSpace(k.Group) == "" {
		missing = append(missing, "Kafka.Group 为空")
	}
	if len(k.Topics) == 0 {
		missing = append(missing, "Kafka.Topics 为空（本服务只消费 content.published.v1 / engagement.action.v1）")
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
		missing = append(missing, "Kafka.MaxRetries 必须大于 0")
	}
	if k.InProcessAttempts <= 0 {
		missing = append(missing, "Kafka.InProcessAttempts 必须大于 0")
	}
	if k.RetryBackoffSec <= 0 {
		missing = append(missing, "Kafka.RetryBackoffSec 必须大于 0")
	}
	if k.MaxRetryBackoffSec <= 0 {
		missing = append(missing, "Kafka.MaxRetryBackoffSec 必须大于 0")
	}
	if (k.Username == "") != (k.Password == "") {
		missing = append(missing, "Kafka.Username 与 Kafka.Password 必须成对配置")
	}
	if len(missing) > 0 {
		return fmt.Errorf("search-indexer: Kafka 消费配置不完整: %s", strings.Join(missing, "; "))
	}
	return nil
}

// Supervisor 管理多个 topic 的消费者生命周期。
//
// 它只负责 broker 侧的启停：退避重投清扫器与重建执行器仍由 internal/svc 按各自的
// 开关启动（两者只依赖 MySQL，不需要 MQ），所以这里不再重复起一个清扫循环。
type Supervisor struct {
	settings Settings
	topics   []string
	factory  QueueFactory
	consumer *Consumer
	queues   []MessageQueue
	started  bool
}

// NewSupervisor 构造消费者编排器。cons 是既有的状态机（*Consumer），
// factory 生产用 NewKqFactory()，单测注入假工厂。配置不完整时返回错误，
// 调用方必须显式失败（见 internal/svc）。
func NewSupervisor(c config.Config, cons *Consumer, factory QueueFactory) (*Supervisor, error) {
	if cons == nil {
		return nil, errors.New("search-indexer: consumer state machine is required")
	}
	if factory == nil {
		return nil, errors.New("search-indexer: consumer queue factory is required")
	}
	if err := ValidateKafka(c.Kafka); err != nil {
		return nil, err
	}
	// 同名 topic 去重：重复订阅会让同一条消息被处理两遍（幂等能兜住，但白烧一倍算力）。
	seen := make(map[string]struct{}, len(c.Kafka.Topics))
	unique := make([]string, 0, len(c.Kafka.Topics))
	for _, topic := range c.Kafka.Topics {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			return nil, errors.New("search-indexer: Kafka.Topics 含空串")
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
		consumer: cons,
	}, nil
}

// Topics 返回实际订阅的 topic 列表。
func (s *Supervisor) Topics() []string { return s.topics }

// Consumer 返回绑定的状态机（供运维接口与单测复用）。
func (s *Supervisor) Consumer() *Consumer { return s.consumer }

// Start 为每个 topic 建立并启动消费者。
// 任一步失败都会回滚已启动的队列，不留半启动状态。
func (s *Supervisor) Start() error {
	if s.started {
		return errors.New("search-indexer/consumer: supervisor already started")
	}
	queues := make([]MessageQueue, 0, len(s.topics))
	for _, topic := range s.topics {
		q, err := s.factory.New(s.settings, topic, NewHandler(topic, s.consumer))
		if err != nil {
			stopAll(queues)
			return fmt.Errorf("search-indexer/consumer: create consumer for topic %s: %w", topic, err)
		}
		if q == nil {
			stopAll(queues)
			return fmt.Errorf("search-indexer/consumer: nil consumer for topic %s", topic)
		}
		queues = append(queues, q)
	}
	for _, q := range queues {
		q.Start()
	}
	s.queues = queues
	s.started = true // 必须在启动成功后置位：否则重复 Start 会再起一套消费者，同一事件被双份处理

	logx.Infof("search-indexer/consumer: 已启动 topics=%v group=%s brokers=%v force_commit=%v",
		s.topics, s.settings.Group, s.settings.Brokers, s.settings.ForceCommit)
	return nil
}

// Stop 逆序关闭队列（kq 的 Stop 会等待在途消息）。
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
