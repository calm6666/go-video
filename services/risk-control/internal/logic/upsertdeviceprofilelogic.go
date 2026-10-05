package logic

import (
	"context"
	"fmt"

	"go-video/services/risk-control/internal/repository"
	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertDeviceProfileLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertDeviceProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertDeviceProfileLogic {
	return &UpsertDeviceProfileLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 允许写入画像的来源。人工来源（operation）必须留 operator，
// 系统来源（login/gateway/system）允许 operator=0，
// 这样既能审计「谁改的设备标签」，又不会让登录链路被审计字段卡住。
var allowedProfileSources = map[string]struct{}{
	"login":     {},
	"gateway":   {},
	"operation": {},
	"system":    {},
}

const maxLabelsPerRequest = 20

// maxProfileSourceLen 对齐 risk_device_profile.source 的列宽 VARCHAR(32)
// （deploy/migrations/risk-control/000002_create_risk_device_and_check_log_tables.sql:52）。
const maxProfileSourceLen = 32

// 写入/更新设备画像与设备-账号关联。
// risk_score 语义：>=0 覆盖，传 -1 表示不修改（proto3 无 optional，需显式传哨兵值）。
// 设备号原文只在计算 SHA-256 的瞬间存在，不落库、不入日志。
func (l *UpsertDeviceProfileLogic) UpsertDeviceProfile(in *rpc.UpsertDeviceProfileReq) (*rpc.UpsertDeviceProfileReply, error) {
	hash := deviceHashOf(in.GetDeviceId(), in.GetDeviceHash())
	if hash == "" {
		return nil, model.ErrEmptyDeviceID
	}
	// source 先规范化（去空白/控制符）再按列宽**拒绝**，不能交给 sanitizeShortString 裁断：
	// 裁断会把 "login-<30 字节尾巴>" 变成恰好合法的 "login"（伪造审计来源），
	// 也会把超长中文来源切成非法 UTF-8（与规则名同一类缺陷）。
	source := sanitizeShortString(in.GetSource(), 0)
	if len(source) > maxProfileSourceLen {
		return nil, fmt.Errorf("%w: source too long, max %d bytes", model.ErrInvalidTarget, maxProfileSourceLen)
	}
	if source == "" {
		source = "system"
	}
	if _, ok := allowedProfileSources[source]; !ok {
		return nil, fmt.Errorf("%w: source=%s not allowed", model.ErrInvalidTarget, source)
	}
	if source == "operation" && in.GetOperator() <= 0 {
		return nil, model.ErrOperatorRequired
	}
	labels := in.GetLabels()
	if len(labels) > maxLabelsPerRequest {
		return nil, fmt.Errorf("%w: too many labels, max %d", model.ErrInvalidTarget, maxLabelsPerRequest)
	}

	out, err := l.svcCtx.Repository.UpsertDeviceProfile(l.ctx, repository.UpsertDeviceProfileInput{
		DeviceHash: hash,
		Labels:     labels,
		RiskScore:  in.GetRiskScore(),
		Mid:        in.GetMid(),
		Source:     source,
		Operator:   in.GetOperator(),
	})
	if err != nil {
		l.Errorf("risk-control/UpsertDeviceProfile: failed device=%s source=%s err=%v",
			model.TruncateHash(hash, 12), source, err)
		return nil, err
	}
	return &rpc.UpsertDeviceProfileReply{
		Profile:       deviceToProto(out.Profile),
		Created:       out.Created,
		RelationAdded: out.RelationAdded,
	}, nil
}
