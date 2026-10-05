// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateMetricDefinitionStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateMetricDefinitionStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateMetricDefinitionStateLogic {
	return &UpdateMetricDefinitionStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 变更口径状态（DRAFT/ACTIVE/RETIRED），不删除历史口径
func (l *UpdateMetricDefinitionStateLogic) UpdateMetricDefinitionState(in *rpc.UpdateMetricDefinitionStateReq) (*rpc.UpdateMetricDefinitionStateReply, error) {
	// 逻辑轮规划：校验状态迁移合法性（DRAFT->ACTIVE、DRAFT/ACTIVE->RETIRED，RETIRED 不允许复活）与 operator/reason 必填 -> ACTIVE 化前确认同 metric_key 至多一个 ACTIVE 版本，否则 ErrMultipleActiveDefinition（否则 version=0 的读请求语义二义）-> 更新 state/mtime 并保留 reason。
	done, err := acquireWriteToken(l.ctx, l.svcCtx, l.Logger, "UpdateMetricDefinitionState")
	if err != nil {
		return nil, err
	}
	defer done()

	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	// 状态迁移决定「哪个版本对外可读」，operator 与 reason 都是审计必填项。
	if err := checkOperator(in.GetOperator()); err != nil {
		return nil, err
	}
	if err := checkReason(in.GetReason()); err != nil {
		return nil, err
	}
	metricKey, err := checkMetricKey(in.GetMetricKey())
	if err != nil {
		return nil, err
	}
	if in.GetMetricVersion() <= 0 {
		return nil, fmt.Errorf("%w: 状态迁移必须指定具体版本（version=0 无起点可迁）",
			model.ErrMetricVersionRequired)
	}
	toState := int32(in.GetState())
	if !validDefinitionStateStrict(toState) {
		return nil, fmt.Errorf("%w: 目标状态=%d 不是 DRAFT/ACTIVE/RETIRED",
			model.ErrInvalidDefinitionState, toState)
	}

	existing, err := l.svcCtx.Definitions.FindByKeyVersion(l.ctx, metricKey, in.GetMetricVersion())
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("%w: %s@v%d", model.ErrMetricDefinitionNotFound,
			metricKey, in.GetMetricVersion())
	}
	fromState := existing.State
	if fromState == toState {
		// 已在目标状态：按重放处理（幂等键无法落列——request_id 记的是登记请求，
		// 覆写它会让「谁登记的」这条线索消失），响应回当前行 + reused=true。
		return &rpc.UpdateMetricDefinitionStateReply{
			Definition: definitionOf(existing), Reused: true,
		}, nil
	}
	if err := checkDefinitionTransition(existing, toState); err != nil {
		return nil, err
	}

	if toState == model.DefinitionStateActive {
		// version=0 的读接口靠「该 key 唯一的 ACTIVE 版本」解析口径，多 ACTIVE 会让它二义。
		// schema 里没有约束能保证这一点（见 README「契约缺口」），所以两侧都数一遍：
		// 迁移前拒绝明显冲突，迁移后发现并发赢家则回滚自己这一次。
		active, err := l.svcCtx.Definitions.CountActive(l.ctx, metricKey)
		if err != nil {
			return nil, err
		}
		if active > 0 {
			return nil, fmt.Errorf("%w: %s 已有 %d 个 ACTIVE 版本，请先退役再激活 v%d",
				model.ErrMultipleActiveDefinition, metricKey, active, in.GetMetricVersion())
		}
	}

	affected, err := l.svcCtx.Definitions.UpdateState(l.ctx, metricKey, in.GetMetricVersion(),
		fromState, toState, in.GetReason())
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		// CAS 未命中：并发下别人已经改过这一行的状态。回读给出当前真值，让调用方自己判断
		// 是重试还是接受——静默成功会把「我以为激活了」变成几天的空榜单。
		current, err := l.svcCtx.Definitions.FindByKeyVersion(l.ctx, metricKey, in.GetMetricVersion())
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, fmt.Errorf("%w: %s@v%d 在迁移期间消失",
				model.ErrMetricDefinitionNotFound, metricKey, in.GetMetricVersion())
		}
		return nil, fmt.Errorf("%w: 并发变更，%s@v%d 当前 state=%d（期望起点 %d）",
			model.ErrInvalidDefinitionState, metricKey, in.GetMetricVersion(), current.State,
			fromState)
	}
	invalidateActiveDefinition(l.ctx, l.svcCtx, metricKey)

	updated, err := l.svcCtx.Definitions.FindByKeyVersion(l.ctx, metricKey, in.GetMetricVersion())
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, fmt.Errorf("%w: %s@v%d 迁移后回读为空",
			model.ErrMetricDefinitionNotFound, metricKey, in.GetMetricVersion())
	}
	if toState == model.DefinitionStateActive {
		if err := l.guardSingleActive(metricKey, updated); err != nil {
			return nil, err
		}
	}
	return &rpc.UpdateMetricDefinitionStateReply{Definition: definitionOf(updated)}, nil
}

// guardSingleActive 激活后的复查：真的出现多个 ACTIVE 时回滚本次迁移。
//
// 回滚只在「本次迁移确实造成了二义」时发生，之后调用方收到 ErrMultipleActiveDefinition，
// 库里的 ACTIVE 指针仍是迁移前那一个 —— 宁可拒绝上线，也不能让 version=0 的读请求
// 在两个口径之间随机切换。
func (l *UpdateMetricDefinitionStateLogic) guardSingleActive(metricKey string,
	updated *model.MetricDefinition) error {
	active, err := l.svcCtx.Definitions.CountActive(l.ctx, metricKey)
	if err != nil {
		// 已经激活成功了，计数失败只说明「这一眼没看成」；不回滚（回滚同样可能失败），
		// 但必须留下日志：下一次 version=0 的读可能撞上二义。
		l.Errorf("spm/UpdateMetricDefinitionState: 激活后 ACTIVE 计数失败 %s: %v", metricKey, err)
		return nil
	}
	if active <= 1 {
		return nil
	}
	if _, rbErr := l.svcCtx.Definitions.UpdateState(l.ctx, metricKey, updated.MetricVersion,
		model.DefinitionStateActive, updated.State, "并发双 ACTIVE，自动回滚本次激活"); rbErr != nil {
		l.Errorf("spm/UpdateMetricDefinitionState: %s@v%d 双 ACTIVE 且回滚失败: %v",
			metricKey, updated.MetricVersion, rbErr)
		return fmt.Errorf("%w: %s 出现 %d 个 ACTIVE 版本且自动回滚失败，需人工退役其一",
			model.ErrMultipleActiveDefinition, metricKey, active)
	}
	l.Errorf("spm/UpdateMetricDefinitionState: %s@v%d 与其他 ACTIVE 版本冲突，已回滚",
		metricKey, updated.MetricVersion)
	return fmt.Errorf("%w: %s 已有其他 ACTIVE 版本（并发激活），本次激活已回滚",
		model.ErrMultipleActiveDefinition, metricKey)
}

// checkDefinitionTransition 状态机：DRAFT -> ACTIVE / DRAFT -> RETIRED / ACTIVE -> RETIRED。
// RETIRED 不出（口径退役后重写历史窗口会让「同一个窗口两套解释」变成常态）；
// ACTIVE -> DRAFT 也不出（对外可读过的版本退回待评审，等于把已发布的口径说成没发布）。
func checkDefinitionTransition(existing *model.MetricDefinition, toState int32) error {
	switch existing.State {
	case model.DefinitionStateDraft:
		if toState == model.DefinitionStateActive || toState == model.DefinitionStateRetired {
			return nil
		}
	case model.DefinitionStateActive:
		if toState == model.DefinitionStateRetired {
			return nil
		}
	}
	return fmt.Errorf("%w: %s@v%d 不允许 %d -> %d", model.ErrInvalidDefinitionState,
		existing.MetricKey, existing.MetricVersion, existing.State, toState)
}

// validDefinitionStateStrict 目标状态白名单：UNSPECIFIED 与未知值都拒绝（契约同一条）。
func validDefinitionStateStrict(s int32) bool {
	return s == model.DefinitionStateDraft || s == model.DefinitionStateActive ||
		s == model.DefinitionStateRetired
}
