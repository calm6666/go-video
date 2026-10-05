package logic

import (
	"context"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListFeatureDefinitionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListFeatureDefinitionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListFeatureDefinitionsLogic {
	return &ListFeatureDefinitionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 特征定义列表（分页、按 scope/source/state/隐私级别过滤）
//
// max_privacy_level 由调用方按自身授权传入，服务端只保证「不返回高于它的行」；
// ps 越界是入参错误（ValidatePageSize），不静默夹小——夹小会让调用方的分页循环少拿一页。
func (l *ListFeatureDefinitionsLogic) ListFeatureDefinitions(
	in *rpc.ListFeatureDefinitionsReq) (*rpc.ListFeatureDefinitionsReply, error) {
	if err := model.ValidatePageSize(in.GetPn(), in.GetPs()); err != nil {
		return nil, err
	}
	maxPrivacy := int32(in.GetMaxPrivacyLevel())
	if maxPrivacy != 0 && !model.ValidPrivacyLevel(maxPrivacy) {
		return nil, model.ErrPrivacyUnsetNotAllowed
	}
	scope := int32(in.GetEntityScope())
	if scope != 0 && !model.ValidEntityScope(scope) {
		return nil, model.ErrEntityScopeRequired
	}
	source := int32(in.GetSource())
	if source != 0 && !model.ValidSource(source) {
		return nil, model.ErrSourceRequired
	}
	state := int32(in.GetState())
	if state != 0 && !model.ValidFeatureState(state) {
		return nil, model.ErrFeatureStateTransition
	}

	filter := model.FeatureDefinitionFilter{
		KeyPrefix:       strings.TrimSpace(in.GetFeatureKeyPrefix()),
		EntityScope:     scope,
		Source:          source,
		State:           state,
		MaxPrivacyLevel: maxPrivacy,
		Pn:              in.GetPn(),
		Ps:              in.GetPs(),
	}
	filter.Normalize()
	rows, total, err := l.svcCtx.Definitions.List(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	defs := make([]*rpc.FeatureDefinition, 0, len(rows))
	for _, d := range rows {
		defs = append(defs, defToProto(d))
	}
	return &rpc.ListFeatureDefinitionsReply{Definitions: defs, Total: total}, nil
}
