//go:build !notification_kafka

// kafkaruntime_disabled.go 是默认构建下的 Kafka 接线占位实现。
//
// 为什么需要构建标签：`github.com/zeromicro/go-queue v1.2.2` 已是主模块 go.mod 的**直接** require
// （`go.mod:9`），`-tags notification_kafka` 的构建不需要任何依赖变更（本轮实测 rc=0）。
// 默认构建仍不链接 Kafka 客户端的理由在运行侧：本仓库没有 broker 联调过消费语义，
// 因此真正的 kq 接线放在 kafkaruntime_kafka.go（`-tags notification_kafka`）里。
//
// 本文件的行为必须是显式失败：默认构建产物绝不会假装“消费者已启动”。
package consumer

import (
	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/provider"
)

// StartKafkaRuntime 默认构建下永远返回 ErrKafkaRuntimeNotBuilt。
// 调用方（svc）会把该错误写日志，运维据此判断需要换用 notification_kafka 构建产物。
func StartKafkaRuntime(_ config.Config, _ KafkaHandler) (KafkaRuntime, error) {
	return nil, provider.ErrKafkaRuntimeNotBuilt
}
