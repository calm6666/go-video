package main

import (
	"flag"
	"fmt"

	"go-video/services/live-media/internal/config"
	"go-video/services/live-media/internal/consumer"
	"go-video/services/live-media/internal/server"
	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/livemedia.v1.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)
	ctx := svc.NewServiceContext(c)

	// 事件消费侧：live.state.v1（生产者 live-ingest）-> logic.OfflineSessionOutputs，断流即整场档位下线。
	// 放在入口而不是 ServiceContext：接线要调用 logic，而 logic 依赖 svc，写进 svc 会成环。
	// 发布循环已经在 NewServiceContext 里起好（svc.startPublisher），
	// 因此这里的 Kafka.Enabled 是两条链路的共同开关：关掉它既不投递也不消费。
	// Enabled=false 时返回 (nil, nil)；Enabled=true 但二进制没链接 kq 或消费参数不完整时，
	// logx.Must 直接终止启动，不允许带着「以为在消费、其实没有」的进程对外服务。
	sup, err := consumer.Start(c, ctx)
	logx.Must(err)
	if sup != nil {
		defer sup.Stop()
	}

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		rpc.RegisterLiveMediaServer(grpcServer, server.NewLiveMediaServer(ctx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	defer s.Stop()

	fmt.Printf("Starting rpc server at %s...\n", c.ListenOn)
	s.Start()
}
