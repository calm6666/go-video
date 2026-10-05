//go:build !engagement_kafka

// kafkaruntime_disabled.go 是默认构建下的队列发送端：什么都不做，只显式失败。
//
// 为什么默认构建不带 kq：go-queue 已是 go.mod 的直接 require，链接它没有障碍，
// 但本仓库从未与任何 broker 联调过。让默认二进制带上生产者，
// 会把「编译得过」读成「事件在发」，而真实结论只能是「投递语义未在 broker 上验证过」。
// 因此真实接线放在 kafkaruntime_kafka.go（`-tags engagement_kafka`）一个文件里。
package publisher

import (
	"errors"

	"go-video/services/engagement/internal/config"
)

// ErrKafkaRuntimeNotBuilt 表示当前二进制没有链接队列发送端。
var ErrKafkaRuntimeNotBuilt = errors.New(
	"engagement/publisher: 当前构建未链接 Kafka 发送端（默认构建刻意不带 kq，本仓库未与任何 broker 联调），" +
		"请改用 `go build -tags engagement_kafka`，或把 Kafka.Enabled 置为 false")

// NewSender 返回真实队列发送端；默认构建恒为未就绪。
func NewSender(_ SenderSettings) (Sender, error) {
	return nil, ErrKafkaRuntimeNotBuilt
}

// RuntimeNotes 返回启动时打印的运行时说明，保证运维在日志里能看到真实状态。
func RuntimeNotes(k config.KafkaConf) []string {
	notes := []string{"Kafka 发送端：未链接（default build，-tags engagement_kafka 可启用）"}
	if !k.Enabled {
		notes = append(notes, "Kafka.Enabled=false：本进程不投递 outbox，"+RequiredTopic()+
			" 事件只留在 engagement_outbox 并持续积压；每次点赞/收藏/分享都会多一行待发布事件，"+
			"而索引里的互动计数只有这一条写入路径（search-indexer 的 PatchHeat；"+
			"content.published 整篇写入时 heat 是零值），"+
			"因此 like_count/favorite_count/share_count 会一直停在 0；"+
			"下游 search-indexer 与 inbox 的消费者实现都已经在订阅这个 topic")
	}
	return notes
}
