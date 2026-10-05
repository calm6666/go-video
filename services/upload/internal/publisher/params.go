// params.go 是配置到发布参数的唯一映射处：默认值只在 config 层出现一次，
// 校验错误逐键点名，避免「改了 yaml 却没生效还查不出为什么」。
package publisher

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go-video/common/outbox"
	"go-video/services/upload/internal/config"
	"go-video/services/upload/model"
)

// SenderSettings 建立一个队列生产者需要的最小参数。
//
// 刻意不含 Username/Password/CaFile：go-queue v1.2.2 的 kq.NewPusher 只提供
// balancer / chunk / flush / sync / 自动建 topic 五类选项，没有 dialer 注入口，
// 因此带 SASL 或 TLS 的集群无法用本仓库现有依赖对接（登记的缺口见服务 README）。
type SenderSettings struct {
	// Brokers 队列地址列表。
	Brokers []string
	// Topics 需要建立写入通道的 topic（去重后）。
	Topics []string
}

// SenderSettingsFrom 由服务配置推导生产者参数，topic 去重但保持声明顺序。
func SenderSettingsFrom(k config.KafkaConf) SenderSettings {
	s := SenderSettings{Brokers: append([]string(nil), k.Brokers...)}
	seen := make(map[string]struct{}, len(k.PublishTopics))
	for _, topic := range k.PublishTopics {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			continue
		}
		if _, dup := seen[topic]; dup {
			continue
		}
		seen[topic] = struct{}{}
		s.Topics = append(s.Topics, topic)
	}
	return s
}

// OptionsFrom 把服务配置映射成发布循环参数。
func OptionsFrom(k config.KafkaConf) Options {
	return Options{
		Name: label,
		// MaxBackoff 小于 BaseBackoff 时由 outbox.Options.validate 拒绝：
		// 把「上限低于基数」这种自相矛盾的配置咽下去，等于替运维猜意图。
		Interval:    seconds(k.PollIntervalSec),
		Batch:       k.BatchLimit,
		MaxAttempts: int32(k.MaxRetries),
		BaseBackoff: seconds(k.RetryBackoffSec),
		MaxBackoff:  seconds(k.RetryMaxBackoffSec),
		SendTimeout: seconds(k.SendTimeoutSec),
	}
}

func seconds(n int64) time.Duration { return time.Duration(n) * time.Second }

// ValidatePublishKafka 校验发布配置，错误里点名具体配置键。
func ValidatePublishKafka(k config.KafkaConf) error {
	var bad []string
	if len(k.Brokers) == 0 {
		bad = append(bad, "Kafka.Brokers 为空（本地 Redpanda 应为 [127.0.0.1:9092]）")
	}
	for i, broker := range k.Brokers {
		if strings.TrimSpace(broker) == "" {
			bad = append(bad, fmt.Sprintf("Kafka.Brokers[%d] 是空串", i))
		}
	}
	if len(k.PublishTopics) == 0 {
		bad = append(bad, fmt.Sprintf("Kafka.PublishTopics 为空（本服务只产出 %s）", RequiredTopic()))
	}
	// topic 判定以 model 常量为锚点：配置里多写一个没实现的 topic，
	// 只会白占一条写入通道并让人以为「这个 topic 有人在发」。
	for _, topic := range k.PublishTopics {
		trimmed := strings.TrimSpace(topic)
		if trimmed == "" {
			bad = append(bad, "Kafka.PublishTopics 含空串")
			continue
		}
		if trimmed != RequiredTopic() {
			bad = append(bad, fmt.Sprintf("Kafka.PublishTopics 含 %q，本服务不产出该事件（只产出 %s）",
				trimmed, RequiredTopic()))
		}
	}
	if k.MaxRetries <= 0 {
		bad = append(bad, "Kafka.MaxRetries 必须大于 0")
	}
	if k.RetryBackoffSec <= 0 {
		bad = append(bad, "Kafka.RetryBackoffSec 必须大于 0")
	}
	if k.RetryMaxBackoffSec <= 0 {
		bad = append(bad, "Kafka.RetryMaxBackoffSec 必须大于 0")
	}
	if k.RetryMaxBackoffSec > 0 && k.RetryBackoffSec > 0 && k.RetryMaxBackoffSec < k.RetryBackoffSec {
		bad = append(bad, "Kafka.RetryMaxBackoffSec 不得小于 Kafka.RetryBackoffSec")
	}
	if k.PollIntervalSec <= 0 {
		bad = append(bad, "Kafka.PollIntervalSec 必须大于 0")
	}
	if k.BatchLimit <= 0 {
		bad = append(bad, "Kafka.BatchLimit 必须大于 0")
	}
	if k.SendTimeoutSec <= 0 {
		bad = append(bad, "Kafka.SendTimeoutSec 必须大于 0")
	}
	if len(bad) > 0 {
		return fmt.Errorf("upload: Kafka 发布配置不完整: %s", strings.Join(bad, "; "))
	}
	return nil
}

// NewPublisher 由服务配置构造发布器：先校验配置，再装配 Store 与 Sender。
//
// 与 outbox.New 的区别是它承担「配置 → 依赖」这一段，因此是 svc 的唯一入口；
// 单测走 outbox.New，注入假 Store / 假 Sender。
func NewPublisher(c config.Config, outboxMd model.UploadOutboxModel, sender Sender) (*Publisher, error) {
	if err := ValidatePublishKafka(c.Kafka); err != nil {
		return nil, err
	}
	store, err := NewOutboxStore(outboxMd)
	if err != nil {
		return nil, err
	}
	if sender == nil {
		return nil, errors.New(label + ": sender is required")
	}
	return outbox.New(store, sender, OptionsFrom(c.Kafka))
}
