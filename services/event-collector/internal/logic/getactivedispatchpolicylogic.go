// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetActiveDispatchPolicyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetActiveDispatchPolicyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetActiveDispatchPolicyLogic {
	return &GetActiveDispatchPolicyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询当前生效策略。
//
// 复用采集侧的 cachedActivePolicy：两条路径共用同一份「Redis 短 TTL + MySQL 真值 +
// 切换即失效」逻辑，否则「运营面板看到的生效版本」与「采集实际用的生效版本」会漂移。
//
// 无 ACTIVE 策略时明确外抛 model.ErrNoActivePolicy，不回「空策略 + nil」：
// 空策略在调用方看来等于「没有规则 = 全量、什么都不丢」，而真正的兜底口径（回落 config 保守默认）
// 只在采集路径里发生，并由 GetCollectorHealth 以 active_salt_version=0 标成不健康。
// 采集侧绝不静默丢事件（proto 注释同义）。
//
// 输出经 policyToRPC：JSON 列解码失败直接报错（空规则在调用方看来等于「不采样」），
// 且只回带 salt_ref（环境变量名），盐值本身永不出库、不出 RPC 响应（AGENTS.md §7）。
func (l *GetActiveDispatchPolicyLogic) GetActiveDispatchPolicy(in *rpc.GetActiveDispatchPolicyReq) (*rpc.DispatchPolicyReply, error) {
	p, err := cachedActivePolicy(l.ctx, l.svcCtx)
	if err != nil {
		return nil, err
	}
	policy, err := policyToRPC(p)
	if err != nil {
		return nil, err
	}
	// created 恒为 false：本方法是只读投影，没有任何一行被新建或修改。
	return &rpc.DispatchPolicyReply{Policy: policy, Created: false}, nil
}
