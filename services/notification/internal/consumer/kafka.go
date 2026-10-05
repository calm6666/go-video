package consumer

// 本文件定义队列运行时的接线契约（与具体 MQ 客户端解耦）。

import "context"

// KafkaHandler 与 kq.ConsumeHandler 的处理函数签名保持一致，
// 因此 *EventHandler 无需适配即可交给 kq.NewQueue。
type KafkaHandler interface {
	Consume(ctx context.Context, key, value string) error
}

// KafkaRuntime 表示一个已接线的消费运行时。
type KafkaRuntime interface {
	// Stop 停止消费并释放底层连接。
	Stop()
}
