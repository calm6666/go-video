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

type FsVersionSwitchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 切换对外生效版本（乐观校验 + 审计留痕；目标版本须已 ACTIVE）
func NewFsVersionSwitchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsVersionSwitchLogic {
	return &FsVersionSwitchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsVersionSwitch 转发 feature-store SwitchFeatureVersion（原子移动某个 key 的 ACTIVE 指针）。
//
// 这是本域影响面最大的一次写：切完之后所有在线读到的值都换一版，因此 expected_from_version
// 的乐观并发位在契约里是显式提供的。网关的门槛只有「from/to 必须是正数、reason 与幂等键非空」：
//   - expected_from_version=0 是「不校验」的合法哨兵，原样下传。它是给回填作业内部用的，
//     后台手工切换应当带上它——但「带不带、要不要覆盖别人的切换」是使用方式，不是形状错误，
//     网关不替运营把一个显式的 0 换成一个猜出来的版本号；
//   - from==to、目标版本不在 ACTIVE、不可变字段与当前版本不一致、指针被并发改走，
//     都由服务判并回明确错误；网关不预读 ACTIVE 指针去「先检查一遍」——预读与切换之间
//     的写入会被吞掉，而且那等于把判定搬到没有判定权的一层。
//
// switched=false（没切成）也带 active_version：那是「现在对外究竟是哪一版」的真实答案，
// 必须原样回，不能折叠成错误。switch_id 是审计行主键，回滚核对时要用。
func (l *FsVersionSwitchLogic) FsVersionSwitch(req *types.ParamFsVersionSwitch) (resp *types.FsVersionSwitchResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	operator, err := fsOperator(l.ctx, "fsVersionSwitch")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("feature_key", req.FeatureKey); err != nil {
		return nil, err
	}
	if err := fsPositive("from_version", req.FromVersion); err != nil {
		return nil, err
	}
	if err := fsPositive("to_version", req.ToVersion); err != nil {
		return nil, err
	}
	if err := fsNonNeg("expected_from_version", int64(req.ExpectedFromVersion)); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.SwitchFeatureVersion(l.ctx, &featurestorerpc.SwitchFeatureVersionReq{
		FeatureKey:          req.FeatureKey,
		FromVersion:         req.FromVersion,
		ToVersion:           req.ToVersion,
		ExpectedFromVersion: req.ExpectedFromVersion,
		Operator:            operator,
		Reason:              req.Reason,
		RequestId:           req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsVersionSwitch: feature_key=%s from=%d to=%d expected_from=%d operator=%s trace_id=%s err=%v",
			req.FeatureKey, req.FromVersion, req.ToVersion, req.ExpectedFromVersion, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/fsVersionSwitch: feature_key=%s switched=%t active_version=%d reused=%t switch_id=%d operator=%s",
		req.FeatureKey, reply.GetSwitched(), reply.GetActiveVersion(), reply.GetReused(), reply.GetSwitchId(), operator)
	return &types.FsVersionSwitchResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsVersionSwitchData{
			Switched:      reply.GetSwitched(),
			Reused:        reply.GetReused(),
			ActiveVersion: reply.GetActiveVersion(),
			SwitchId:      reply.GetSwitchId(),
		},
		TTL: 0,
	}, nil
}
