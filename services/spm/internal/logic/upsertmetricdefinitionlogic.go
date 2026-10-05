// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertMetricDefinitionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertMetricDefinitionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertMetricDefinitionLogic {
	return &UpsertMetricDefinitionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 登记指标口径新版本（已存在版本不可变）
func (l *UpsertMetricDefinitionLogic) UpsertMetricDefinition(in *rpc.UpsertMetricDefinitionReq) (*rpc.UpsertMetricDefinitionReply, error) {
	// 逻辑轮规划：只允许新增口径版本：按 (metric_key, metric_version) 查登记行 -> 不存在则以 DRAFT 强制入库（不允许跳过评审直接 ACTIVE）-> 存在且 formula/unit/supported_windows/source_event_types 完全一致时 reused=true -> 任一不同返回 ErrMetricVersionImmutable，绝不原地改写已有版本；request_id 落行内用于重放判定。
	done, err := acquireWriteToken(l.ctx, l.svcCtx, l.Logger, "UpsertMetricDefinition")
	if err != nil {
		return nil, err
	}
	defer done()

	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	if err := checkOperator(in.GetOperator()); err != nil {
		return nil, err
	}
	def := in.GetDefinition()
	if def == nil {
		return nil, fmt.Errorf("%w: definition 必填", model.ErrInvalidDefinitionSpec)
	}
	metricKey, err := checkMetricKey(def.GetMetricKey())
	if err != nil {
		return nil, err
	}
	if def.GetMetricVersion() < 1 {
		return nil, fmt.Errorf("%w: metric_version=%d，登记必须 >=1",
			model.ErrMetricVersionRequired, def.GetMetricVersion())
	}
	creator, err := resolveCreatedBy(def, in.GetOperator())
	if err != nil {
		return nil, err
	}
	row, err := definitionFromRequest(def, metricKey, creator, in.GetRequestId())
	if err != nil {
		return nil, err
	}

	existing, err := l.svcCtx.Definitions.FindByKeyVersion(l.ctx, metricKey, def.GetMetricVersion())
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if sameDefinitionSpec(existing, row) {
			// 同一份登记的重复投递：回首次结果，不写库。
			return &rpc.UpsertMetricDefinitionReply{
				Created: false, Reused: true, Definition: definitionOf(existing),
			}, nil
		}
		return nil, definitionImmutableError(existing, row)
	}

	created, err := l.svcCtx.Definitions.InsertIfAbsent(l.ctx, row)
	if err != nil {
		return nil, err
	}
	if !created {
		// 并发下另一个实例先登记了同一版本：重读并按同一规则判定，绝不覆盖对方那行。
		race, err := l.svcCtx.Definitions.FindByKeyVersion(l.ctx, metricKey, def.GetMetricVersion())
		if err != nil {
			return nil, err
		}
		if race == nil {
			return nil, fmt.Errorf("%w: %s@v%d 插入未生效且回读为空",
				model.ErrMetricDefinitionNotFound, metricKey, def.GetMetricVersion())
		}
		if sameDefinitionSpec(race, row) {
			return &rpc.UpsertMetricDefinitionReply{
				Created: false, Reused: true, Definition: definitionOf(race),
			}, nil
		}
		return nil, definitionImmutableError(race, row)
	}
	// DRAFT 不参与对外读，ACTIVE 指针缓存里没有它；失效一次是为了覆盖
	// 「同键先前无 ACTIVE、本次登记后被激活」的窗口。
	invalidateActiveDefinition(l.ctx, l.svcCtx, metricKey)
	return &rpc.UpsertMetricDefinitionReply{
		Created: true, Reused: false, Definition: definitionOf(row),
	}, nil
}

// definitionFromRequest 把契约里的口径描述校验并归一成登记行。
// 归一（CSV 去重排序、空白裁剪）必须在比对之前完成：否则 "1,2" 与 "2, 1"
// 会被判成两次不同的登记，而它们表达的是同一份口径。
func definitionFromRequest(def *rpc.MetricDefinition, metricKey, createdBy,
	requestID string) (*model.MetricDefinition, error) {
	state := int32(def.GetState())
	if state != model.DefinitionStateUnspecified && state != model.DefinitionStateDraft {
		// 新登记一律 DRAFT：ACTIVE 只能由 UpdateMetricDefinitionState 走评审迁移，
		// 否则「登记即上线」绕过了口径评审这条唯一的把关线。
		return nil, fmt.Errorf("%w: 新登记口径只能是 DRAFT（state=%d），ACTIVE 请走 UpdateMetricDefinitionState",
			model.ErrInvalidDefinitionState, state)
	}
	name := strings.TrimSpace(def.GetName())
	if name == "" {
		return nil, fmt.Errorf("%w: name 必填", model.ErrInvalidDefinitionSpec)
	}
	if len(name) > maxNameBytes {
		return nil, fmt.Errorf("%w: name %d 字节 > %d", model.ErrInvalidDefinitionSpec,
			len(name), maxNameBytes)
	}
	formula := strings.TrimSpace(def.GetFormula())
	if formula == "" {
		// 公式是口径的唯一人读解释（分子/分母/去重键），空公式的登记无法复核。
		return nil, fmt.Errorf("%w: formula 必填", model.ErrInvalidDefinitionSpec)
	}
	if len(formula) > maxFormulaBytes {
		return nil, fmt.Errorf("%w: formula %d 字节 > %d", model.ErrInvalidDefinitionSpec,
			len(formula), maxFormulaBytes)
	}
	unit := strings.TrimSpace(def.GetUnit())
	if _, ok := definitionUnits[unit]; !ok {
		return nil, fmt.Errorf("%w: unit=%q，只支持 count/ratio/seconds/score",
			model.ErrInvalidDefinitionSpec, def.GetUnit())
	}
	windows, err := supportedWindowsCSV(def.GetSupportedWindows())
	if err != nil {
		return nil, err
	}
	events, err := sourceEventTypesCSV(def.GetSourceEventTypes())
	if err != nil {
		return nil, err
	}
	if len(def.GetDescription()) > maxDescriptionBytes {
		return nil, fmt.Errorf("%w: description %d 字节 > %d", model.ErrInvalidDefinitionSpec,
			len(def.GetDescription()), maxDescriptionBytes)
	}
	return &model.MetricDefinition{
		MetricKey:        metricKey,
		MetricVersion:    def.GetMetricVersion(),
		Name:             name,
		Formula:          formula,
		Unit:             unit,
		SupportedWindows: windows,
		SourceEventTypes: events,
		State:            model.DefinitionStateDraft,
		Description:      strings.TrimSpace(def.GetDescription()),
		CreatedBy:        createdBy,
		RequestID:        requestID,
	}, nil
}

// resolveCreatedBy 取登记人：definition.created_by 是契约里的登记人字段，
// 缺省时回落到 operator（同一个审计语义：谁提交的这次登记）。
// 两条来源都按 operator 列宽校验：超长在这里拒绝，而不是让 model 的 truncate
// 静默截断——库里留下的登记人被砍掉尾巴，就和调用方手里的不是同一个人了。
func resolveCreatedBy(def *rpc.MetricDefinition, operator string) (string, error) {
	createdBy := strings.TrimSpace(def.GetCreatedBy())
	if createdBy == "" {
		createdBy = strings.TrimSpace(operator)
	}
	if len(createdBy) > maxOperatorBytes {
		return "", fmt.Errorf("%w: created_by %d 字节 > %d", model.ErrOperatorRequired,
			len(createdBy), maxOperatorBytes)
	}
	return createdBy, nil
}

// sameDefinitionSpec 比较口径的不可变部分。name/description 属于说明文本，
// 不参与不可变判定（契约只把公式、单位、窗口与事件类型列为口径本体）。
func sameDefinitionSpec(a, b *model.MetricDefinition) bool {
	return a.Formula == b.Formula &&
		a.Unit == b.Unit &&
		a.SupportedWindows == b.SupportedWindows &&
		a.SourceEventTypes == b.SourceEventTypes
}

// definitionImmutableError 指出到底哪一项不同：只回「版本不可变」的话，
// 登记方要么去翻库、要么反复重投同一个请求，两边都查不出自己写错了哪一列。
func definitionImmutableError(existing, want *model.MetricDefinition) error {
	var diffs []string
	if existing.Formula != want.Formula {
		diffs = append(diffs, "formula")
	}
	if existing.Unit != want.Unit {
		diffs = append(diffs, "unit")
	}
	if existing.SupportedWindows != want.SupportedWindows {
		diffs = append(diffs, "supported_windows")
	}
	if existing.SourceEventTypes != want.SourceEventTypes {
		diffs = append(diffs, "source_event_types")
	}
	return fmt.Errorf("%w: %s@v%d 已登记，差异项 [%s]，请新增版本",
		model.ErrMetricVersionImmutable, existing.MetricKey, existing.MetricVersion,
		strings.Join(diffs, ", "))
}
