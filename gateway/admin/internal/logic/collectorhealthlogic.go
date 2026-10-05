// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectorHealthLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 采集与投递健康度：积压、限流、盐可用性与生效策略版本
func NewCollectorHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorHealthLogic {
	return &CollectorHealthLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorHealth 转发 event-collector GetCollectorHealth。
// 不暴露 now 入参：面板要看的就是「服务此刻的现在」，让后台自填时间戳只会造出
// 一份可对上「我几分钟前看到的那个值」的假读数；服务在 now<=0 时用自己时钟（timeNow），
// 并把 server_time 回带供前端计算差值。
//
// ttl 恒为 0（不建议客户端缓存）：积压与限流是分钟级变化的排障读数，
// 缓存一份「dead_open=0」会让人以为死信已经处理完（与 /healthz 同一口径）。
// 本路由只读，绝不触发投递推进——推进是 /delivery/retry 的职责，也是它的权限点。
func (l *CollectorHealthLogic) CollectorHealth() (resp *types.CollectorHealthResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	reply, err := l.svcCtx.EventCollector.GetCollectorHealth(l.ctx, &collectorrpc.GetCollectorHealthReq{})
	if err != nil {
		l.Errorf("gateway/admin/collectorHealth: err=%v", err)
		return nil, err
	}
	data := collectorHealthToAPI(reply)
	// 日志只记两个可判定的健康事实位，用于事后关联告警；不打 topic 明细。
	l.Infof("gateway/admin/collectorHealth: active_salt_version=%d salt_available=%t policy_version=%s",
		data.ActiveSaltVersion, data.SaltAvailable, data.PolicyVersion)
	return &types.CollectorHealthResponse{
		Code:    0,
		Message: "ok",
		Data:    data,
		TTL:     0,
	}, nil
}
