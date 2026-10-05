// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	spmrpc "go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SpmMetricDefinitionUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 登记新口径版本（只能新增，改已登记版本服务回 ErrMetricVersionImmutable；无删除语义）
func NewSpmMetricDefinitionUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmMetricDefinitionUpsertLogic {
	return &SpmMetricDefinitionUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmMetricDefinitionUpsert 转发 spm UpsertMetricDefinition（登记一个新口径版本）。
//
// 网关做三件事，其余全部交回服务（§5 口径注册表属 spm）：
//  1. 身份：operator 只能由会话渲染成 gateway/admin:<admin_id>（表单没有 operator 位，
//     也不允许自报）；幂等键原样透传到 request_id，改一个字符等于换一次执行权；
//  2. 形状门槛：只挡 metric_key/name/idempotency_key 的「空」与负数版本号；
//     公式是否自洽、单位是否支持、窗口组合是否合法、事件类型是否在白名单、
//     描述长度、以及「这个版本能不能用」全部由服务判定；
//  3. created_by/ctime/mtime 不在表单位，也由服务按会话与库时钟渲染——
//     网关自报经办人等于伪造审计主体，本地造时间等于造一个假版本。
//
// 「只能新增版本、命中已登记版本且规格不同回 ErrMetricVersionImmutable」是服务的结论，
// 网关不预读一次列表去判断「算不算新增」（那会把两次读之间的写入吞掉，还会多一次往返）。
// reused/created 原样回：reused=true 是幂等重放的正常结论，不折叠成错误也不折叠成「新增成功」。
func (l *SpmMetricDefinitionUpsertLogic) SpmMetricDefinitionUpsert(req *types.ParamSpmMetricDefinitionUpsert) (resp *types.SpmMetricDefinitionUpsertResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	operator, err := spmOperator(l.ctx, "spmMetricDefinitionUpsert")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("definition.metric_key", req.Definition.MetricKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("definition.name", req.Definition.Name); err != nil {
		return nil, err
	}
	if err := spmNonNeg("definition.metric_version", int64(req.Definition.MetricVersion)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.UpsertMetricDefinition(l.ctx, &spmrpc.UpsertMetricDefinitionReq{
		Definition: spmDefinitionForRPC(req.Definition),
		Operator:   operator,
		RequestId:  req.IdempotencyKey,
	})
	if err != nil {
		// trace_id 只进日志（UpsertMetricDefinitionReq 没有该字段可下传）；公式正文不落日志。
		l.Errorf("gateway/admin/spmMetricDefinitionUpsert: metric_key=%s metric_version=%d operator=%s trace_id=%s err=%v",
			req.Definition.MetricKey, req.Definition.MetricVersion, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/spmMetricDefinitionUpsert: metric_key=%s metric_version=%d created=%t reused=%t operator=%s",
		req.Definition.MetricKey, req.Definition.MetricVersion, reply.GetCreated(), reply.GetReused(), operator)
	return &types.SpmMetricDefinitionUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmMetricDefinitionUpsertData{
			Created:    reply.GetCreated(),
			Reused:     reply.GetReused(),
			Definition: spmDefinitionToAPI(reply.GetDefinition()),
		},
		TTL: 0,
	}, nil
}
