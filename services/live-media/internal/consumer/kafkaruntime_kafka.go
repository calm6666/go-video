//go:build livemedia_kafka

// kafkaruntime_kafka.go 用 github.com/zeromicro/go-queue/kq 提供真实的 Kafka 消费工厂。
//
// 仅在 `-tags livemedia_kafka` 时参与编译：go-queue 虽已是主模块的直接 require（`go.mod:9`），
// 但默认构建仍不链接 Kafka 客户端，走 kafkaruntime_disabled.go 快速失败。
// 本仓库没有 broker 可验证消费语义，不能让「编译得过」被读成「已在消费」。
//
// 本文件与 publisher/kafkaruntime_kafka.go 共用同一个 build tag：本服务的发布循环与消费循环
// 要么一起可用，要么一起拒绝启动，避免出现「档位下线事件发得出去、断流事件收不进来」的半接线进程。
// Kafka 客户端在消费侧只出现在这里：信封翻译、尝试封顶与结果分类都在
// mapping.go / handler.go，替换 MQ 时只改这个文件。
package consumer

import (
	"errors"
	"fmt"

	"github.com/zeromicro/go-queue/kq"
	"github.com/zeromicro/go-zero/core/service"

	"go-video/services/live-media/internal/config"
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
// 重投不会把档位下线两次：MarkOfflineTx 的 WHERE 带 state=在线，
// 第二次扫不到行、Affected=0，因此「至少一次」投递在本链路是安全的。
//
// 注意 CaFile 已在 ValidateKafka 里验过可读：kq 内部读到不可用证书会直接 log.Fatal。
func (kqFactory) New(s Settings, topic string, h *Handler) (MessageQueue, error) {
	if h == nil {
		return nil, errors.New("livemedia/consumer: kq handler is required")
	}
	if topic == "" {
		return nil, errors.New("livemedia/consumer: kq topic is required")
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
		return nil, fmt.Errorf("livemedia/consumer: kq.NewQueue topic=%s: %w", topic, err)
	}
	return q, nil
}

// RuntimeNotes 返回启动时打印的运行时说明。
func RuntimeNotes(k config.KafkaConf) []string {
	notes := []string{"Kafka 消费运行时：已链接（build tag livemedia_kafka）"}
	if !k.Enabled {
		notes = append(notes, "Kafka.Enabled=false：本进程既不投递 livemedia.* outbox，也不消费 "+
			SupportedTopic+"；断流后档位只能靠运营手工 OfflineStreamOutput 或 online_expire_at 到期清扫下线")
		return notes
	}
	notes = append(notes, "Kafka.Enabled=true：消费者随进程启动。"+
		"kq.NewQueue 不做网络握手，真正的拉取与位点语义必须在 broker 上联调后才算成立（本仓库从未做过）")
	return notes
}
