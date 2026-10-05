// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	openplatformrpc "go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpenQuotaPolicyUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增/更新配额规则（policy_id=0 = 唯一键新建；enabled=false 在本入口没有可执行路径，见注释）
func NewOpenQuotaPolicyUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenQuotaPolicyUpsertLogic {
	return &OpenQuotaPolicyUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenQuotaPolicyUpsert 转发 open-platform UpsertQuotaPolicy（限额是判定链上游，参数门禁全失败关闭）。
//
// 网关只挡两处，其余一律交给服务：
//   - operator_mid 必填：owner 提高自己限额等于自己给自己放行，本方法只有自然人运营通道；
//   - **enabled=false 就地拒**：服务对 !enabled 回 errReasonRequired，因为「关掉一条限额」要有
//     问责原因，而 UpsertQuotaPolicyReq 里没有 reason 位（停用只走 model.QuotaPolicies.Disable，
//     proto 未暴露对应方法）。给一条说得清的错，比让调用方收到一句指向它没填过的字段的
//     「reason required」更诚实，也比伪造一个「已停用」的成功结论诚实——这条能力缺口记在
//     admin.api 文末与本域 README。
//
// 不复算的部分：app_id 是否存在、api_code 字符集与商业化红线类目、window_seconds 上下界、
// limit 是否合理、policy_id 与唯一键是否指向同一行、是否被更高层级规则遮蔽，全在服务侧。
// created 由服务回读（它才知道唯一键算出的那一行是新是旧），网关不按 policy_id 是否为 0 推断。
func (l *OpenQuotaPolicyUpsertLogic) OpenQuotaPolicyUpsert(req *types.ParamOpenQuotaPolicyUpsert) (resp *types.OpenQuotaPolicyUpsertResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openOperatorGate(l.ctx, "openQuotaPolicyUpsert", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if !req.Enabled {
		return nil, errOpenQuotaDisableUnsupported
	}
	if err := openNonNeg("policy_id", req.PolicyId); err != nil {
		return nil, err
	}
	if err := openNonNeg("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openNonNeg("window_seconds", req.WindowSeconds); err != nil {
		return nil, err
	}
	if err := openNonNeg("limit", req.Limit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.UpsertQuotaPolicy(l.ctx, &openplatformrpc.UpsertQuotaPolicyReq{
		PolicyId:      req.PolicyId,
		AppId:         req.AppId,
		ApiCode:       req.ApiCode,
		WindowSeconds: req.WindowSeconds,
		Limit:         req.Limit,
		Enabled:       req.Enabled,
		OperatorMid:   req.OperatorMid,
		TraceId:       req.TraceId,
	})
	if err != nil {
		// api_code 是配额维度标识（可能含业务含义），只记定位位不记规则意图正文。
		l.Errorf("gateway/admin/openQuotaPolicyUpsert: policy_id=%d app_id=%d window_seconds=%d operator_mid=%d err=%v",
			req.PolicyId, req.AppId, req.WindowSeconds, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenQuotaPolicyUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenQuotaPolicyUpsertData{
			PolicyId: reply.GetPolicyId(),
			Created:  reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
