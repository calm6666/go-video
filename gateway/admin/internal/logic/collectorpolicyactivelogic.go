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

type CollectorPolicyActiveLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 当前生效的采样与脱敏策略（无 ACTIVE 时由服务明确报错，不回空策略）
func NewCollectorPolicyActiveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorPolicyActiveLogic {
	return &CollectorPolicyActiveLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorPolicyActive 转发 event-collector GetActiveDispatchPolicy（无入参）。
// 本路由刻意不传任何参数、也不缓存（ttl=0）：它复用采集侧同一份
// 「Redis 短 TTL + MySQL 真值 + 切换即失效」逻辑（cachedActivePolicy），
// 网关再缓存一层就会让「运营面板看到的生效版本」与「采集实际用的生效版本」漂移，
// 而这两个版本不一致正是排查「为什么这批被采掉了」的第一现场。
//
// 无 ACTIVE 策略时服务外抛 model.ErrNoActivePolicy 而不是回空策略：
// 「空策略」在调用方看来等于「没有规则 = 全量、什么都不丢」，与真实的保守兜底口径相反。
// 该错误经 common/httpresponse 的四字段信封原样表达，网关不折叠成 code=0 + 空 data。
func (l *CollectorPolicyActiveLogic) CollectorPolicyActive() (resp *types.CollectorPolicyResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	reply, err := l.svcCtx.EventCollector.GetActiveDispatchPolicy(l.ctx, &collectorrpc.GetActiveDispatchPolicyReq{})
	if err != nil {
		l.Errorf("gateway/admin/collectorPolicyActive: err=%v", err)
		return nil, err
	}
	return &types.CollectorPolicyResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorPolicyData{
			Policy:  collectorPolicyToAPI(reply.GetPolicy()),
			Created: reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
