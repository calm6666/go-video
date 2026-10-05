// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	featurestorerpc "go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type FsBackfillSubmitLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交回填作业（为目标 DRAFT 版本补历史值；auto_switch 可选）
func NewFsBackfillSubmitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsBackfillSubmitLogic {
	return &FsBackfillSubmitLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsBackfillSubmit 转发 feature-store SubmitBackfillJob（为某个版本补历史值）。
//
// 门槛只有形状：feature_key/version/entity_scope/source 的必填与正数、幂等键与 reason 非空、
// 回填区间不反过来。以下一律是服务的结论，网关不复算：
//   - 目标版本是否处于 DRAFT（回填完成后才允许切 ACTIVE）、source 是否与定义一致、
//     取值来源是否已配置（离线快照侧当前是 stub，会如实报错而不是回一个空作业）；
//   - entity_ids 的条数上限（1000）与每个 id 的形态（DEVICE/IP_HASH 只接受十六进制摘要）；
//   - auto_switch=true 时 from_version 基线是否唯一确定。
//
// entity_ids 为空是「全量扫描」而不是「没给」，因此不要求非空；列表原样透传，
// 不裁剪、不去重、不 TrimSpace 后回写——幂等键与哈希摘要都按字节敏感。
// from_version=0（不校验）与 window_to=0（当前时间）都是合法哨兵，网关不拿自己的钟替服务填。
// 契约缺口（已上报）：BackfillJob 里没有 from_version 位，auto_switch 作业乐观基线落在哪一版
// 读不回来，因此这里能投影的只有服务给了什么。
func (l *FsBackfillSubmitLogic) FsBackfillSubmit(req *types.ParamFsBackfillSubmit) (resp *types.FsBackfillSubmitResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	operator, err := fsOperator(l.ctx, "fsBackfillSubmit")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("feature_key", req.FeatureKey); err != nil {
		return nil, err
	}
	if err := fsPositive("version", req.Version); err != nil {
		return nil, err
	}
	if err := fsPositive("entity_scope", req.EntityScope); err != nil {
		return nil, err
	}
	if err := fsPositive("source", req.Source); err != nil {
		return nil, err
	}
	if err := fsWindowRange(req.WindowFrom, req.WindowTo); err != nil {
		return nil, err
	}
	if err := fsNonNeg("from_version", int64(req.FromVersion)); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.SubmitBackfillJob(l.ctx, &featurestorerpc.SubmitBackfillJobReq{
		FeatureKey:  req.FeatureKey,
		Version:     req.Version,
		EntityScope: featurestorerpc.EntityScope(req.EntityScope),
		EntityIds:   req.EntityIds,
		WindowFrom:  req.WindowFrom,
		WindowTo:    req.WindowTo,
		Source:      featurestorerpc.FeatureSource(req.Source),
		AutoSwitch:  req.AutoSwitch,
		FromVersion: req.FromVersion,
		RequestId:   req.IdempotencyKey,
		Operator:    operator,
		Reason:      req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsBackfillSubmit: feature_key=%s version=%d entity_scope=%d entity_ids=%d auto_switch=%t operator=%s trace_id=%s err=%v",
			req.FeatureKey, req.Version, req.EntityScope, len(req.EntityIds), req.AutoSwitch, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/fsBackfillSubmit: feature_key=%s version=%d job_id=%d reused=%t operator=%s",
		req.FeatureKey, req.Version, reply.GetJobId(), reply.GetReused(), operator)
	return &types.FsBackfillSubmitResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsBackfillSubmitData{
			JobId:  reply.GetJobId(),
			Reused: reply.GetReused(),
			Job:    fsJobToAPI(reply.GetJob()),
		},
		TTL: 0,
	}, nil
}
