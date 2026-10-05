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

type FsEntityFeatureEraseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按主体擦除个体特征（隐私工单执行；操作人前缀白名单在服务侧二次把关）
func NewFsEntityFeatureEraseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsEntityFeatureEraseLogic {
	return &FsEntityFeatureEraseLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsEntityFeatureErase 转发 feature-store EraseEntityFeatures（按主体删除个体特征值）。
//
// 本域影响面最大且不可逆的一次操作，因此双重门槛：
//  1. 网关侧：AdminPermission 单独一个权限点（fs:entity-feature:erase）+ 会话身份；
//  2. 服务侧：operator 必须落在 Privacy.OperatorPrefixes 白名单里，**空白名单 = 谁都拒**。
//     网关不代替它放行，也不因为「自己是网关」而换一个前缀——渲染出的
//     gateway/admin:<admin_id> 就是审计与白名单匹配的那个身份，配置没放行时如实失败。
//
// min_privacy_level=0 是「全部个体特征」（契约语义），原样下传；级别与主体维度是否自洽、
// 定义与审计是否保留（本方法只删值）、幂等键是否命中已执行请求都由服务判。
// reason 必须是工单号一类的可追溯依据：非空在这里判，内容是否构成有效工单由服务与流程判。
// erased_rows=0 是合法结论（这个主体本来没有可删的值），不折叠成错误；reused=true 同理。
// 日志只记 scope、级别与行数——entity_id 与剩余特征一律不落日志（§7）。
func (l *FsEntityFeatureEraseLogic) FsEntityFeatureErase(req *types.ParamFsEntityFeatureErase) (resp *types.FsEntityFeatureEraseResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	operator, err := fsOperator(l.ctx, "fsEntityFeatureErase")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	entity, err := fsEntity(req.EntityScope, req.EntityId)
	if err != nil {
		return nil, err
	}
	if err := fsPrivacyFilter("min_privacy_level", req.MinPrivacyLevel); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.EraseEntityFeatures(l.ctx, &featurestorerpc.EraseEntityFeaturesReq{
		Entity:          entity,
		MinPrivacyLevel: featurestorerpc.PrivacyLevel(req.MinPrivacyLevel),
		Operator:        operator,
		Reason:          req.Reason,
		RequestId:       req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsEntityFeatureErase: entity_scope=%s min_privacy_level=%d operator=%s trace_id=%s err=%v",
			entity.EntityScope.String(), req.MinPrivacyLevel, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/fsEntityFeatureErase: entity_scope=%s erased_rows=%d features_touched=%d reused=%t operator=%s",
		entity.EntityScope.String(), reply.GetErasedRows(), reply.GetFeaturesTouched(), reply.GetReused(), operator)
	return &types.FsEntityFeatureEraseResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsEntityFeatureEraseData{
			ErasedRows:      reply.GetErasedRows(),
			FeaturesTouched: reply.GetFeaturesTouched(),
			Reused:          reply.GetReused(),
		},
		TTL: 0,
	}, nil
}
