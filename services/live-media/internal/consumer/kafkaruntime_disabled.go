//go:build !livemedia_kafka

// kafkaruntime_disabled.go 是默认构建下的 Kafka 消费运行时占位实现。
//
// 为什么需要它：`github.com/zeromicro/go-queue v1.2.2` 已是主模块的直接 require，
// 所以 `-tags livemedia_kafka` 的构建不需要任何依赖变更；保留构建标签的理由是运行侧的：
// 本仓库从未与任何 broker 联调过，默认构建不链接 Kafka 客户端，
// 而是让 NewKqFactory 显式返回 ErrKafkaRuntimeNotBuilt，由入口文件把进程打死：
// 绝不伪造「已经在消费」，也不允许「档位安静地挂在已断的流上继续对外分发」。
//
// 打开方式（不需要授权依赖变更）：
//  1. `go build -tags livemedia_kafka ./services/live-media`，改由 kafkaruntime_kafka.go 提供真实工厂；
//  2. 部署时把 Kafka.Enabled 置为 true（见 etc/livemedia.v1.yaml）。
//     注意：这个开关同时打开本服务的发布循环与消费循环，只打开编译路径不等于验证过消费语义。
package consumer

import (
	"errors"

	"go-video/services/live-media/internal/config"
)

// ErrKafkaRuntimeNotBuilt 表示当前二进制没有链接 Kafka 客户端。
var ErrKafkaRuntimeNotBuilt = errors.New(
	"livemedia/consumer: 当前构建未链接 Kafka 运行时（默认构建刻意不带 kq，本仓库未与任何 broker 联调），" +
		"请改用 `go build -tags livemedia_kafka`，或把 Kafka.Enabled 置为 false")

// NewKqFactory 返回真实队列工厂；默认构建恒为未就绪。
func NewKqFactory() (QueueFactory, error) {
	return nil, ErrKafkaRuntimeNotBuilt
}

// RuntimeNotes 返回启动时打印的运行时说明，保证运维在日志里能看到真实状态。
func RuntimeNotes(k config.KafkaConf) []string {
	notes := []string{"Kafka 消费运行时：未链接（default build，-tags livemedia_kafka 可启用）"}
	if !k.Enabled {
		notes = append(notes, "Kafka.Enabled=false：本进程既不投递 livemedia.* outbox，也不消费 "+
			SupportedTopic+"；断流后档位只能靠运营手工 OfflineStreamOutput 或 online_expire_at 到期清扫下线")
		return notes
	}
	notes = append(notes, "Kafka.Enabled=true 但运行时未链接：入口会以 ErrKafkaRuntimeNotBuilt 终止启动，"+
		"不会带着「以为在消费、其实没有」的进程对外服务")
	return notes
}
