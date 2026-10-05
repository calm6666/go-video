//go:build !inbox_kafka

// kafkaruntime_stub.go 是默认构建下的 Kafka 运行时占位实现。
//
// 为什么需要它：`github.com/zeromicro/go-queue v1.2.2` 已是主模块 go.mod 的**直接** require
// （`go.mod:9`，其传递依赖 `github.com/segmentio/kafka-go` 在 go.mod 的 indirect 块与 go.sum 里都齐），
// 所以 `-tags inbox_kafka` 的构建**不需要任何依赖变更**，本轮实测两条标签构建均 rc=0。
// 保留构建标签的理由是运行侧的：本仓库没有任何 Kafka broker 联调过，默认构建不链接 Kafka 客户端，
// 而是显式返回 ErrKafkaRuntimeNotBuilt，让 internal/svc 在启动时快速失败——
// 绝不伪造「已经在消费」。
//
// 打开方式（不需要授权依赖变更）：
//  1. `go build -tags inbox_kafka ./services/inbox`，改由 kafkaruntime_kafka.go 提供真实工厂；
//  2. 部署时把 Kafka.Enabled 置为 true（见 etc/inbox.v1.yaml）。
//     注意：这只打开编译路径，真实的消费语义仍需在 broker 上验证。
//
// 详见 services/inbox/README.md「消费者启动方式」与「缺口」。
package consumer

import (
	"errors"

	"go-video/services/inbox/internal/config"
)

// ErrKafkaRuntimeNotBuilt 表示当前二进制没有链接 Kafka 客户端。
var ErrKafkaRuntimeNotBuilt = errors.New(
	"inbox/consumer: 当前构建未链接 Kafka 运行时（默认构建刻意不带 kq，本仓库未与任何 broker 联调），" +
		"请改用 `go build -tags inbox_kafka`，或把 Kafka.Enabled 置为 false")

// NewKqFactory 返回真实队列工厂；默认构建恒为未就绪。
func NewKqFactory() (QueueFactory, error) {
	return nil, ErrKafkaRuntimeNotBuilt
}

// RuntimeNotes 返回启动时打印的运行时说明，保证运维在日志里能看到真实状态。
func RuntimeNotes(k config.KafkaConf) []string {
	notes := []string{"Kafka 运行时：未链接（default build，-tags inbox_kafka 可启用）"}
	if !k.Enabled {
		notes = append(notes, "Kafka.Enabled=false：本进程不消费事件，站内信只来自 SendSystemMessage RPC")
	}
	if k.RetrySweeperEnabled {
		notes = append(notes, "退避重投清扫器：已启动（只依赖 MySQL inbox_consumer_offset，不依赖 MQ）")
	}
	return notes
}
