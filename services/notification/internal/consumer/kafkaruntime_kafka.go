//go:build notification_kafka

// kafkaruntime_kafka.go 是 `-tags notification_kafka` 构建下的真实 Kafka 接线。
//
// 启用步骤（不需要任何 go.mod/go.sum 变更）：
//  1. go-queue 已是 go.mod 的直接 require（`go.mod:9`），kafka-go 在 indirect 块与 go.sum 都齐，
//     所以本文件的 import 不需要授权依赖变更（本轮实测 `go build -tags notification_kafka` rc=0）；
//  2. 用 `go build -tags notification_kafka ./services/notification` 产出消费者二进制；
//  3. 部署时把 Kafka.Brokers 配好，服务启动即自动订阅 notification.request.v1。
//     这一步仍未在真实 broker 上验证过，配了 Brokers 只代表开始拨号，不代表消费语义可用。
//
// 默认构建（无 tag）编译的是 kafkaruntime_disabled.go，会显式返回
// provider.ErrKafkaRuntimeNotBuilt，绝不伪造“已在消费”。
package consumer

import (
	"errors"
	"fmt"

	"github.com/zeromicro/go-queue/kq"

	"go-video/services/notification/internal/config"
)

// kqQueue 是 kq 队列的最小抽象（Start/Stop），避免本包再引用 go-zero 的 queue 包类型。
type kqQueue interface {
	Start()
	Stop()
}

type kqRuntime struct{ q kqQueue }

func (r kqRuntime) Stop() { r.q.Stop() }

// StartKafkaRuntime 构造并启动 kq 消费者；返回的运行时由调用方在进程退出时 Stop。
func StartKafkaRuntime(c config.Config, h KafkaHandler) (KafkaRuntime, error) {
	if h == nil {
		return nil, errors.New("notification/consumer: consume handler is required")
	}
	if len(c.Kafka.Brokers) == 0 {
		return nil, errors.New("notification/consumer: Kafka.Brokers is empty")
	}
	if c.Kafka.RequestTopic == "" {
		return nil, errors.New("notification/consumer: Kafka.RequestTopic is empty")
	}
	// 复用服务自身的 ServiceConf：kq.NewQueue 内部会 SetUp()，
	// 传空配置会把全局日志/追踪重新初始化成默认值。
	q, err := kq.NewQueue(kq.KqConf{
		ServiceConf: c.ServiceConf,
		Brokers:     c.Kafka.Brokers,
		Group:       c.Kafka.Group,
		Topic:       c.Kafka.RequestTopic,
		Offset:      c.Kafka.Offset,
		Conns:       c.Kafka.Conns,
		Consumers:   c.Kafka.Consumers,
		Processors:  c.Kafka.Processors,
	}, h)
	if err != nil {
		return nil, fmt.Errorf("notification/consumer: new kafka queue: %w", err)
	}
	go q.Start()
	return kqRuntime{q: q}, nil
}
