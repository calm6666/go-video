// wiring.go 把消费者接到本服务的入站 logic 上。
//
// 为什么这段接线不放在 internal/svc：svc 是 logic 的依赖（logic → svc），
// 而接线必须调用 logic，写进 svc 就成环。因此由入口文件调 consumer.Start，
// 本包保持 consumer → logic → svc 的单向依赖。
//
// 事件消费的写入路径与运营手工下线用的是同一段能力（StreamOutputs.MarkOfflineTx +
// appendOutboxEvent 的事务组合），因此 broker 的至少一次投递不会把档位下线两次：
// MarkOfflineTx 的 WHERE 带 state=在线，第二次扫不到行，Affected=0。
package consumer

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/live-media/internal/config"
	"go-video/services/live-media/internal/logic"
	"go-video/services/live-media/internal/svc"
)

// NewSvcApplicator 返回把下线命令交给 OfflineSessionOutputs logic 的执行器。
//
// 每条消息新建一个 logic 实例：logic 内嵌 logx.Logger 并绑定调用方 ctx，
// 复用会让日志挂到第一条消息的 trace 上，排查时指错方向。
//
// Reason 由消费者固定成 SOURCE_LOST（见 mapping.go 的 Interpret），
// 到了 logic 这一层已经不需要再判断事件语义。
func NewSvcApplicator(svcCtx *svc.ServiceContext) Applicator {
	return ApplicatorFunc(func(ctx context.Context, cmd *OfflineCommand) (*OfflineResult, error) {
		res, err := logic.NewOfflineSessionOutputsLogic(ctx, svcCtx).OfflineSessionOutputs(
			logic.OfflineSessionOutputsInput{
				RoomID:    cmd.RoomID,
				SessionID: cmd.SessionID,
				Reason:    cmd.Reason,
				EventID:   cmd.EventID,
				TraceID:   cmd.TraceID,
			})
		if err != nil {
			return nil, err
		}
		// logic 的 Scanned/Affected 是消费者判定 Outcome 的唯一依据，
		// nil 结果在这里就转成错误（handler 会把 (nil,nil) 判成 ErrNilResult）。
		if res == nil {
			return nil, errors.New("livemedia/consumer: OfflineSessionOutputs 返回 nil 结果")
		}
		return &OfflineResult{Affected: res.Affected, Scanned: res.Scanned}, nil
	})
}

// Start 按配置装配并启动消费者。三种结果都有明确日志，不存在「安静地不消费」：
//  1. Kafka.Enabled=false：返回 (nil, nil)，本进程不消费事件，档位下线只有 RPC 与到期清扫两条路；
//  2. Enabled=true、二进制链接了运行时（-tags livemedia_kafka）且参数完整：返回已启动的 Supervisor，
//     调用方负责 Stop；这只说明拉取通道建立成功，本仓库从未与真实 broker 联调；
//  3. Enabled=true 但运行时未链接、或消费参数不完整：返回错误，进程启动即失败。
//     宁可不启动，也不要让档位安静地挂在已断的流上继续被 live-gateway 分发给观众。
//
// 注意本服务的 Kafka.Enabled 同时管发布循环（common/outbox 投递 livemedia.*.v1）和这条消费循环，
// 因此关掉它等于两个方向都停：运维排查「档位不自动下线」时要先看这个开关。
func Start(c config.Config, svcCtx *svc.ServiceContext) (*Supervisor, error) {
	for _, note := range RuntimeNotes(c.Kafka) {
		logx.WithContext(context.Background()).Infof("livemedia/consumer: %s", note)
	}
	if !c.Kafka.Enabled {
		return nil, nil
	}
	if svcCtx == nil {
		return nil, errors.New("livemedia/consumer: 启用消费必须给出 ServiceContext")
	}
	factory, err := NewKqFactory()
	if err != nil {
		return nil, err
	}
	sup, err := NewSupervisor(c, NewSvcApplicator(svcCtx), factory)
	if err != nil {
		return nil, err
	}
	if err := sup.Start(context.Background()); err != nil {
		return nil, err
	}
	return sup, nil
}
