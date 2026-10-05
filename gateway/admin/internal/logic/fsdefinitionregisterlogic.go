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

type FsDefinitionRegisterLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 注册特征版本（只能新增版本；一律以 DRAFT 入库，改已登记版本服务回不可变错误）
func NewFsDefinitionRegisterLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsDefinitionRegisterLogic {
	return &FsDefinitionRegisterLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsDefinitionRegister 转发 feature-store RegisterFeature（登记一个特征版本）。
//
// 网关只做三件事：会话身份（operator 只能渲染成 gateway/admin:<admin_id>，表单没有该位、
// 也不允许自报）、幂等键非空、以及「必填位没给 / 枚举位是 UNSPECIFIED / 显式版本不是正数」
// 这类不可能形状。值类型是否支持、维度与默认值形态是否自洽、TTL 是否可用、feature_key
// 是否符合命名规则、不可变字段是否冲突（ErrFeatureDefinitionImmutable/ErrFeatureMetadataImmutable）
// 全部由 model.ValidateFeatureDefinition 与服务的逐字段比对判定，网关不复算。
//
// state 位**不在表单位**：注册一律以 DRAFT 入库（注册即生效等于绕过评审），
// 上线只能走 /definition/state 与 /version/switch；created_by/ctime/mtime 同理由服务渲染。
// version 这里必须是正数（0 在写入口没有「按 ACTIVE 指针」的语义——那是指针读法，
// 用它落版本等于把新定义写进一个调用方没选过的版本）。
//
// created/reused 原样回，不折叠：created=false+reused=true 是幂等重放的正常结论，
// 也不是错误；「命中同一版本且规格全等」与「规格不同被拒」的差别由服务给出。
func (l *FsDefinitionRegisterLogic) FsDefinitionRegister(req *types.ParamFsDefinitionRegister) (resp *types.FsDefinitionRegisterResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	operator, err := fsOperator(l.ctx, "fsDefinitionRegister")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	def := req.Definition
	if err := requireNonEmpty("definition.feature_key", def.FeatureKey); err != nil {
		return nil, err
	}
	if err := fsPositive("definition.version", def.Version); err != nil {
		return nil, err
	}
	if err := fsPositive("definition.value_type", def.ValueType); err != nil {
		return nil, err
	}
	if err := fsPositive("definition.entity_scope", def.EntityScope); err != nil {
		return nil, err
	}
	if err := fsPositive("definition.source", def.Source); err != nil {
		return nil, err
	}
	// privacy_level 是「未声明的特征不允许存在」的必填位：0 = UNSPECIFIED 在服务侧必拒，
	// 挡在这里省一次往返，也避免后台把「忘了选级别」读成「系统不支持这个特征」。
	if err := fsPositive("definition.privacy_level", def.PrivacyLevel); err != nil {
		return nil, err
	}
	// window_seconds=0 是「无窗口的静态属性」、dimension=0 是「标量类」，都是合法值而非未填；
	// ttl_seconds<=0 服务侧一律拒（没有 TTL 的值等于永久可训练），但那是定义口径判定，留在这里。
	if err := fsNonNeg("definition.window_seconds", def.WindowSeconds); err != nil {
		return nil, err
	}
	if err := fsNonNeg("definition.dimension", int64(def.Dimension)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.RegisterFeature(l.ctx, &featurestorerpc.RegisterFeatureReq{
		Definition: fsDefinitionForRPC(def),
		Operator:   operator,
		RequestId:  req.IdempotencyKey,
	})
	if err != nil {
		// trace_id 只进日志（RegisterFeatureReq 没有该字段可下传）；default_value 正文不落日志。
		l.Errorf("gateway/admin/fsDefinitionRegister: feature_key=%s version=%d operator=%s trace_id=%s err=%v",
			def.FeatureKey, def.Version, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/fsDefinitionRegister: feature_key=%s version=%d created=%t reused=%t operator=%s",
		def.FeatureKey, def.Version, reply.GetCreated(), reply.GetReused(), operator)
	return &types.FsDefinitionRegisterResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsDefinitionRegisterData{
			Created:    reply.GetCreated(),
			Reused:     reply.GetReused(),
			Definition: fsDefinitionToAPI(reply.GetDefinition()),
		},
		TTL: 0,
	}, nil
}
