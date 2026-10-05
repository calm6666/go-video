//go:build !liveingest_kafka

// kafkaruntime_disabled.go 是默认构建下的队列发送端：什么都不做，只显式失败。
//
// 为什么默认构建不带 kq：go-queue 已是 go.mod 的直接 require（`go.mod:9`），
// 链接它没有障碍，但本仓库从未与任何 broker 联调过。让默认二进制带上生产者，
// 会把「编译得过」读成「事件在发」，而真实结论只能是「投递语义未在 broker 上验证过」。
// 因此真实接线放在 kafkaruntime_kafka.go（`-tags liveingest_kafka`）一个文件里。
package publisher

import (
	"errors"

	"go-video/services/live-ingest/internal/config"
)

// ErrKafkaRuntimeNotBuilt 表示当前二进制没有链接队列发送端。
var ErrKafkaRuntimeNotBuilt = errors.New(
	"live-ingest/publisher: 当前构建未链接 Kafka 发送端（默认构建刻意不带 kq，本仓库未与任何 broker 联调），" +
		"请改用 `go build -tags liveingest_kafka`，或把 Kafka.Enabled 置为 false")

// NewSender 返回真实队列发送端；默认构建恒为未就绪。
func NewSender(_ SenderSettings) (Sender, error) {
	return nil, ErrKafkaRuntimeNotBuilt
}

// RuntimeNotes 返回启动时打印的运行时说明，保证运维在日志里能看到真实状态。
func RuntimeNotes(k config.KafkaConf) []string {
	notes := []string{"Kafka 发送端：未链接（default build，-tags liveingest_kafka 可启用）"}
	if !k.Enabled {
		notes = append(notes, "Kafka.Enabled=false：本进程不投递 outbox，"+RequiredTopic()+
			" 事件只留在 live_ingest_outbox；下游的 live-room 消费者与 inbox 消费者都拿不到事件，"+
			"房间/场次投影与主播的断流/下播站内信都不会前进")
	}
	return notes
}
