package logic

import (
	"context"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ResolveConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewResolveConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ResolveConfigLogic {
	return &ResolveConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运行时解析单个配置（命中灰度规则，返回建议 TTL）
func (l *ResolveConfigLogic) ResolveConfig(in *rpc.ResolveConfigReq) (*rpc.ResolveConfigReply, error) {
	lim := newLimits(l.svcCtx.Config)
	cfgKey := in.GetCfgKey()
	if err := checkCfgKey(cfgKey, lim); err != nil {
		return nil, err
	}
	cfgKey = trimKey(cfgKey)
	scope := normalizeScope(in.GetScope())
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	target, err := rolloutInputOf(in.GetTarget())
	if err != nil {
		return nil, err
	}
	// 只读接口不强制 request_id，但缺了它就不进缓存路径：
	// 否则探测流量会打出大量「无身份」的回源，把热键打穿也无处归因。
	useCache := in.GetCtx().GetRequestId() != "" && !in.GetRefresh()
	view, err := resolveOne(l.ctx, l.svcCtx, l.Logger, lim, cfgKey, scope, target, useCache, model.NowUnix())
	if err != nil {
		if isNotFoundAsResult(err) {
			// 契约把「没配」定义成正常结果（found=false），不占用 gRPC 错误码：
			// 过期入口与未放量键的读取量很大，全记成错误会让告警失去意义。
			return &rpc.ResolveConfigReply{Found: false}, nil
		}
		return nil, err
	}
	return &rpc.ResolveConfigReply{Config: view, Found: true}, nil
}
