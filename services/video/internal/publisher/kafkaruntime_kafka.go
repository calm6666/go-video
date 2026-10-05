//go:build video_kafka

// kafkaruntime_kafka.go 用 github.com/zeromicro/go-queue/kq 提供真实的队列发送端。
//
// 关键选择：kq.NewPusher 必须带 WithSyncPush()。默认的异步模式把消息交给
// kq 的 ChunkExecutor，实际写入失败只写一条日志（pusher.go 的 chunk 回调里
// logx.Error(err)），调用方拿到的是 nil。Outbox 语义要求「broker 受理成功才
// 置 state=已发布」，用异步就等于把「没送出去」写成「已发布」，
// 下游索引与作者通知从此永久落后且无人知晓。同步模式下 PushWithKey 直接走
// kafka.Writer.WriteMessages(ctx, msg)，错误真实回传，退避与判死才有依据。
package publisher

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-queue/kq"

	"go-video/services/video/internal/config"
)

// kqSender 每 topic 持有一个同步 pusher。
type kqSender struct {
	pushers map[string]*kq.Pusher
	topics  []string
}

// NewSender 为配置里的每个 topic 建立一个同步生产者。
//
// 刻意不传 WithAllowAutoTopicCreation()：topic 名写错时自动建出一个空 topic，
// 消费者永远读不到，比启动即失败难查得多。topic 由 deploy 侧统一创建
// （见 docs/api-and-events.md §5 的版本化命名）。
func NewSender(s SenderSettings) (Sender, error) {
	if len(s.Brokers) == 0 {
		return nil, errors.New(label + ": kq sender requires brokers")
	}
	for i, broker := range s.Brokers {
		if strings.TrimSpace(broker) == "" {
			return nil, fmt.Errorf("%s: kq sender broker[%d] is empty", label, i)
		}
	}
	if len(s.Topics) == 0 {
		return nil, errors.New(label + ": kq sender requires at least one topic")
	}
	pushers := make(map[string]*kq.Pusher, len(s.Topics))
	topics := make([]string, 0, len(s.Topics))
	for _, topic := range s.Topics {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			for _, p := range pushers {
				_ = p.Close()
			}
			return nil, fmt.Errorf("%s: kq sender topic 含空串", label)
		}
		if _, dup := pushers[topic]; dup {
			continue
		}
		// 只同步写：不加 chunk/flush 选项，二者在同步模式下不参与发送。
		pushers[topic] = kq.NewPusher(s.Brokers, topic, kq.WithSyncPush())
		topics = append(topics, topic)
	}
	return &kqSender{pushers: pushers, topics: topics}, nil
}

// Topics 返回已建立写入通道的 topic（供启动日志与单测读取）。
func (s *kqSender) Topics() []string { return s.topics }

// Send 投递一条事件。
//
// 未登记的 topic 返回错误而不是静默丢弃：配置与 model 派生出的 topic 一旦漂移
// （例如 schema 版本递增后 PublishTopics 没跟着改），静默丢事件会让
// 「Outbox 已发布」与「下游收到」两个结论同时失真。
func (s *kqSender) Send(ctx context.Context, topic, key, payload string) error {
	p, ok := s.pushers[topic]
	if !ok {
		return fmt.Errorf("%s: topic %q 未建立发送通道（已建 %v）", label, topic, s.topics)
	}
	if key == "" {
		return errors.New(label + ": kq send requires a partition key")
	}
	// PushWithKey 会把 trace 上下文注入消息头（kq 内部用 otel propagator），
	// 因此这里必须把带 trace 的 ctx 传到底，不能用 context.Background()。
	return p.PushWithKey(ctx, key, payload)
}

// Close 关闭全部写入通道，任一失败都回传（聚合错误里点名 topic）。
func (s *kqSender) Close() error {
	var errs []string
	for topic, p := range s.pushers {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", topic, err))
		}
		delete(s.pushers, topic)
	}
	s.topics = nil
	if len(errs) > 0 {
		return fmt.Errorf("%s: 关闭 kq sender 失败: %s", label, strings.Join(errs, "; "))
	}
	return nil
}

// RuntimeNotes 返回启动时打印的运行时说明。
//
// 「已链接」不等于「已在投递」：只有 Kafka.Enabled=true 且发布循环起来，本进程才会投 outbox；
// 而无论哪种组合，本仓库都没有 broker 侧证据（从未联调）。
func RuntimeNotes(k config.KafkaConf) []string {
	notes := []string{"Kafka 发送端：已链接（build tag video_kafka）"}
	if !k.Enabled {
		notes = append(notes, "Kafka.Enabled=false：本进程不投递 outbox，"+RequiredTopic()+
			" 事件只留在 video_outbox，搜索投影与作者的下架/删除通知都不会更新")
	} else {
		notes = append(notes, "Kafka.Enabled=true：发布循环会投 "+RequiredTopic()+
			"，下游 search-indexer（-tags searchindexer_kafka）与 inbox（-tags inbox_kafka）有消费者实现，"+
			"但本仓库从未与真实 broker 联调，端到端送达没有证据（见 docs/roadmap.md MQ 接线）")
	}
	return notes
}
