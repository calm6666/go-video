// wiring.go 把消费者接到本服务的入站 logic 上。
//
// 为什么这段接线不放在 internal/svc：svc 是 logic 的依赖（logic → svc），
// 而接线必须调用 logic，写进 svc 就成环。因此由入口文件调 consumer.Start，
// 本包保持 consumer → logic → svc 的单向依赖。
//
// 事件消费的写入路径与 RPC 路径是同一段 code：logic.ReportStreamState 内部
// 已按 event_id 去重（live_room_idempotency）并按 stream_seq 挡乱序，
// 所以 broker 的至少一次投递不会把房间状态推进两次。
package consumer

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/internal/logic"
	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/rpc"
)

// NewSvcApplicator 返回把事件交给 ReportStreamState logic 的执行器。
//
// 每条消息新建一个 logic 实例：logic 内嵌 logx.Logger 并绑定调用方 ctx，
// 复用会让日志挂到第一条消息的 trace 上，排查时指错方向。
func NewSvcApplicator(svcCtx *svc.ServiceContext) Applicator {
	return ApplicatorFunc(func(ctx context.Context, in *rpc.ReportStreamStateReq) (*rpc.ReportStreamStateReply, error) {
		return logic.NewReportStreamStateLogic(ctx, svcCtx).ReportStreamState(in)
	})
}

// Start 按配置装配并启动消费者。三种结果都有明确日志，不存在「安静地不消费」：
//  1. Kafka.Enabled=false：返回 (nil, nil)，本进程不消费事件，房间投影只由 RPC 推进；
//  2. Enabled=true、二进制链接了运行时（-tags liveroom_kafka）且参数完整：返回已启动的 Supervisor，
//     调用方负责 Stop；这只说明发送/拉取通道建立成功，本仓库从未与真实 broker 联调；
//  3. Enabled=true 但运行时未链接、或消费参数不完整：返回错误，进程启动即失败。
//     宁可不启动，也不要让房间投影安静地落后于真实流状态。
func Start(c config.Config, svcCtx *svc.ServiceContext) (*Supervisor, error) {
	for _, note := range RuntimeNotes(c.Kafka) {
		logx.WithContext(context.Background()).Infof("live-room/consumer: %s", note)
	}
	if !c.Kafka.Enabled {
		return nil, nil
	}
	if svcCtx == nil {
		return nil, errors.New("live-room/consumer: 启用消费必须给出 ServiceContext")
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
