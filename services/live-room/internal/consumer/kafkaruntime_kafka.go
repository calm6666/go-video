//go:build liveroom_kafka

// kafkaruntime_kafka.go 用 github.com/zeromicro/go-queue/kq 提供真实的 Kafka 消费工厂。
//
// 仅在 `-tags liveroom_kafka` 时参与编译：go-queue 虽已是主模块的直接 require（`go.mod:9`），
// 但默认构建仍不链接 Kafka 客户端，走 kafkaruntime_disabled.go 快速失败。
// 本仓库没有 broker 可验证消费语义，不能让「编译得过」被读成「已在消费」。
//
// 本文件是 Kafka 客户端唯一出现的地方：信封翻译、尝试封顶与结果分类都在
// mapping.go / handler.go，与 MQ 实现无关，替换 MQ 时只改这里。
package consumer

import (
	"errors"
	"fmt"

	"github.com/zeromicro/go-queue/kq"
	"github.com/zeromicro/go-zero/core/service"

	"go-video/services/live-room/internal/config"
)

// kqFactory 把本服务的 Settings 适配成 kq.KqConf。
type kqFactory struct{}

// NewKqFactory 返回生产可用的队列工厂。
func NewKqFactory() (QueueFactory, error) {
	return kqFactory{}, nil
}

// New 为单个 topic 建立一个消费者。
//
// ForceCommit 由配置透传，默认 false：处理失败时不提交位点，让 broker 重投。
// 重投会被 logic.ReportStreamState 的 event_id 去重判成 result=2，
// 因此「至少一次」投递不会把房间状态推进两次。
//
// 注意 CaFile 已在 ValidateKafka 里验过可读：kq 内部读到不可用证书会直接 log.Fatal。
func (kqFactory) New(s Settings, topic string, h *Handler) (MessageQueue, error) {
	if h == nil {
		return nil, errors.New("live-room/consumer: kq handler is required")
	}
	if topic == "" {
		return nil, errors.New("live-room/consumer: kq topic is required")
	}
	conf := kq.KqConf{
		// Name/Log/Mode 必须显式带上：kq.NewQueue 内部调用 ServiceConf.SetUp()，
		// 留空会用它自己的默认值重设整个进程的全局 logger（日志突然改成写文件）。
		ServiceConf: service.ServiceConf{
			Name: s.Name,
			Log:  s.Log,
			Mode: s.Mode,
		},
		Brokers:     s.Brokers,
		Group:       s.Group,
		Topic:       topic,
		Offset:      s.Offset,
		Conns:       s.Conns,
		Consumers:   s.Consumers,
		Processors:  s.Processors,
		ForceCommit: s.ForceCommit,
		Username:    s.Username,
		Password:    s.Password,
		CaFile:      s.CaFile,
	}
	// *Handler 直接满足 kq.ConsumeHandler（Consume(ctx, key, value) error）。
	q, err := kq.NewQueue(conf, h)
	if err != nil {
		return nil, fmt.Errorf("live-room/consumer: kq.NewQueue topic=%s: %w", topic, err)
	}
	return q, nil
}

// RuntimeNotes 返回启动时打印的运行时说明。
func RuntimeNotes(k config.KafkaConf) []string {
	notes := []string{"Kafka 运行时：已链接（build tag liveroom_kafka）"}
	if !k.Enabled {
		notes = append(notes, "Kafka.Enabled=false：本进程不消费 live.state.v1，"+
			"房间/场次投影只由 ReportStreamState RPC 推进")
		return notes
	}
	notes = append(notes, "Kafka.Enabled=true：消费者随进程启动。"+
		"kq.NewQueue 不做网络握手，真正的拉取与位点语义必须在 broker 上联调后才算成立（本仓库从未做过）")
	return notes
}
