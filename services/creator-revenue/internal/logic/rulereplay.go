// 本文件是 logic 包的手写扩展（规则写侧的共用校验与台账装配），不是 goctl 生成产物。

package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/creator-revenue/internal/config"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// errRuleCodeTaken 标记「新建时 rule_code 已被占用」，与「request_id 被占用」区分开：
// 前者要给调用方的建议是「改成带 rule_id 的更新」，后者才走幂等回读。
var errRuleCodeTaken = errors.New("creatorrevenue: rule_code already exists on create")

// ruleDraft 是校验完的规则写入意图（DRAFT 语义），字段名与列名一一对应。
type ruleDraft struct {
	RuleId           int64
	RuleCode         string
	SourceType       int32
	Name             string
	Description      string
	UnitPricePer1000 int64
	Currency         string
	Unit             string
	MinQuantity      int64
	MonthlyCapMinor  int64
	EffectiveFrom    int64
	ExpectedVersion  int64
	Operator         string
	RequestId        string
	Reason           string
}

// validateRuleUpsert 把 UpsertRevenueRuleReq 折成 ruleDraft，并跑完所有「不查库就能判」的规则。
//
// 护栏（全部来自 internal/config 的 CreatorRevenue 段，不写死在代码里）：
//   - 负单价、负门槛、负封顶一律拒绝：负单价会把「应付」算成「倒扣」，
//     负门槛等于允许「越不达标越有钱」；
//   - unit_price_per_1000_minor 超过 MaxRuleUnitPricePer1000Minor 直接拒写：
//     这是防手滑（把 ¥1/千次 写成 ¥1000/千次）在结算里被放大成资金事故的唯一拦点；
//   - monthly_cap_minor 超过 MaxMonthlyCapMinor 同样拒写（0 表示不限，允许）。
//
// 写侧只写 DRAFT：ACTIVE 单价直接决定应计金额，就地改等于悄悄改写别人的钱，
// 因此改价必须「新建草稿 → 复核 → SetRevenueRuleState 切换」，见 model.ErrRuleNotDraft。
func validateRuleUpsert(c config.Config, in *rpc.UpsertRevenueRuleReq) (*ruleDraft, error) {
	d := &ruleDraft{
		RuleId:          in.RuleId,
		RuleCode:        strings.TrimSpace(in.RuleCode),
		SourceType:      int32(in.SourceType),
		Name:            strings.TrimSpace(in.Name),
		Description:     strings.TrimSpace(in.Description),
		Currency:        strings.TrimSpace(in.Currency),
		Unit:            strings.TrimSpace(in.Unit),
		MinQuantity:     in.MinQuantity,
		MonthlyCapMinor: in.MonthlyCapMinor,
		EffectiveFrom:   in.EffectiveFrom,
		ExpectedVersion: in.ExpectedVersion,
	}
	if d.RuleCode == "" {
		return nil, model.ErrRuleCodeRequired
	}
	if err := checkText("rule_code", d.RuleCode, model.MaxRuleCodeBytes); err != nil {
		return nil, err
	}
	if d.Name == "" {
		return nil, fmt.Errorf("%w: name 不能为空", model.ErrTextTooLong)
	}
	if err := checkText("name", d.Name, model.MaxRuleNameBytes); err != nil {
		return nil, err
	}
	if err := checkText("description", d.Description, model.MaxDescriptionBytes); err != nil {
		return nil, err
	}
	if err := validSourceType(d.SourceType); err != nil {
		return nil, err
	}
	if d.Unit == "" {
		return nil, fmt.Errorf("%w: unit 不能为空（minute / coin / interaction）", model.ErrInvalidRuleParams)
	}
	if err := checkText("unit", d.Unit, model.MaxUnitBytes); err != nil {
		return nil, err
	}
	if d.Currency == "" {
		d.Currency = c.CreatorRevenue.DefaultCurrency
	}
	if err := checkText("currency", d.Currency, model.MaxCurrencyBytes); err != nil {
		return nil, err
	}

	if in.UnitPricePer_1000Minor < 0 {
		return nil, model.ErrNegativeUnitPrice
	}
	if ceiling := c.CreatorRevenue.MaxRuleUnitPricePer1000Minor; ceiling > 0 &&
		in.UnitPricePer_1000Minor > ceiling {
		return nil, fmt.Errorf("%w: 传入 %d 分/千单位，护栏上限 %d",
			model.ErrRuleUnitPriceTooLarge, in.UnitPricePer_1000Minor, ceiling)
	}
	d.UnitPricePer1000 = in.UnitPricePer_1000Minor

	if d.MinQuantity < 0 || d.MonthlyCapMinor < 0 {
		return nil, model.ErrInvalidRuleParams
	}
	if ceiling := c.CreatorRevenue.MaxMonthlyCapMinor; ceiling > 0 && d.MonthlyCapMinor > ceiling {
		return nil, fmt.Errorf("%w: 传入 %d 分，护栏上限 %d",
			model.ErrRuleCapTooLarge, d.MonthlyCapMinor, ceiling)
	}
	if d.EffectiveFrom < 0 {
		return nil, fmt.Errorf("%w: effective_from 不能为负", model.ErrInvalidRuleParams)
	}

	var err error
	if d.Operator, err = requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if d.Reason, err = requireReason(in.Reason); err != nil {
		return nil, err
	}
	// 规则台账要求「整条 request_id 原样落库」，所以这里的上限就是列宽。
	if d.RequestId, err = requireRequestID(in.RequestId, model.MaxRequestIDBytes); err != nil {
		return nil, err
	}
	// 更新必须带 CAS 版本；新建时 expected_version 必须留空，否则等于允许调用方指定历史版本。
	if d.RuleId > 0 && d.ExpectedVersion <= 0 {
		return nil, fmt.Errorf("%w: 更新既有规则必须带 expected_version", model.ErrVersionConflict)
	}
	if d.RuleId == 0 && d.ExpectedVersion != 0 {
		return nil, fmt.Errorf("%w: 新建规则不能指定 expected_version=%d", model.ErrVersionConflict, d.ExpectedVersion)
	}
	return d, nil
}

// applyRuleDraftInTx 在一个事务里「改主表 + 写变更台账」。
//
// 台账与主表同事务是硬要求：只写了主表说明「改价生效但没人知道是谁改的」，
// 只写台账说明「有人声称改了价但金额没变」，两种都比不改更糟（AGENTS.md §5、§8）。
// created 返回 false 表示这是一次 request_id 重放（外层已回滚），调用方走幂等回读。
func applyRuleDraftInTx(
	ctx context.Context, tx sqlx.Session, d *ruleDraft,
) (ruleID int64, created bool, err error) {
	rules := model.NewRevenueRuleModel(sqlx.NewSqlConnFromSession(tx))
	logs := model.NewRuleChangeLogModel(sqlx.NewSqlConnFromSession(tx))

	if d.RuleId == 0 {
		row := &model.RevenueRule{
			RuleCode: d.RuleCode, SourceType: d.SourceType, Name: d.Name, Description: d.Description,
			UnitPricePer1000: d.UnitPricePer1000, Currency: d.Currency, Unit: d.Unit,
			MinQuantity: d.MinQuantity, MonthlyCapMinor: d.MonthlyCapMinor,
			State: model.RuleStateDraft, EffectiveFrom: d.EffectiveFrom,
			Version: 1, CreatedBy: d.Operator, UpdatedBy: d.Operator,
		}
		id, err := rules.Insert(ctx, row)
		if err != nil {
			if model.IsDuplicateErr(err) {
				// rule_code 已存在：可能是本次请求的重放（首版 version=1），
				// 也可能是别人占了同一编码。交给调用方按 request_id 判定。
				return 0, false, fmt.Errorf("%w: %s", errRuleCodeTaken, d.RuleCode)
			}
			return 0, false, err
		}
		if _, err := logs.Insert(ctx, &model.RuleChangeLog{
			RuleCode: d.RuleCode, SourceType: d.SourceType, Action: model.RuleActionCreate,
			FromState: model.RuleStateUnspecified, ToState: model.RuleStateDraft,
			FromUnitPrice: 0, ToUnitPrice: d.UnitPricePer1000,
			FromVersion: 0, ToVersion: 1,
			Operator: d.Operator, Reason: d.Reason, RequestId: d.RequestId,
		}); err != nil {
			return 0, false, err
		}
		return id, true, nil
	}

	cur, err := rules.LockByCode(ctx, d.RuleCode)
	if err != nil {
		return 0, false, err
	}
	if cur == nil || cur.RuleId != d.RuleId {
		// rule_code 与 rule_id 指向不同行：不能凭调用方给的主键改到别人的规则。
		return 0, false, model.ErrRuleNotFound
	}
	if cur.State != model.RuleStateDraft {
		return 0, false, fmt.Errorf("%w: rule_code=%s 当前 state=%d，只有 DRAFT 可编辑",
			model.ErrRuleNotDraft, cur.RuleCode, cur.State)
	}
	if cur.Version != d.ExpectedVersion {
		return 0, false, fmt.Errorf("%w: rule_code=%s 服务端 version=%d，请求 expected_version=%d",
			model.ErrVersionConflict, cur.RuleCode, cur.Version, d.ExpectedVersion)
	}
	next := *cur
	next.SourceType = d.SourceType
	next.Name = d.Name
	next.Description = d.Description
	next.UnitPricePer1000 = d.UnitPricePer1000
	next.Currency = d.Currency
	next.Unit = d.Unit
	next.MinQuantity = d.MinQuantity
	next.MonthlyCapMinor = d.MonthlyCapMinor
	next.EffectiveFrom = d.EffectiveFrom
	next.UpdatedBy = d.Operator

	ok, err := rules.UpdateDraft(ctx, &next, d.ExpectedVersion)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, model.ErrVersionConflict
	}
	if _, err := logs.Insert(ctx, &model.RuleChangeLog{
		RuleCode: cur.RuleCode, SourceType: d.SourceType, Action: model.RuleActionUpdate,
		FromState: cur.State, ToState: model.RuleStateDraft,
		FromUnitPrice: cur.UnitPricePer1000, ToUnitPrice: d.UnitPricePer1000,
		FromVersion: cur.Version, ToVersion: cur.Version + 1,
		Operator: d.Operator, Reason: d.Reason, RequestId: d.RequestId,
	}); err != nil {
		return 0, false, err
	}
	return cur.RuleId, true, nil
}

// resolveRuleReplay 在 request_id 命中唯一键后给出结论（事务已回滚，这里只读）。
//
// 三种可能：
//  1. 该 request_id 就是本规则的最近一次变更 → 真重放，返回首次结果（幂等成功）；
//  2. 该 request_id 属于别的 rule_code → 调用方复用错了键，报冲突让它换；
//  3. 该 request_id 之后又有新变更 → 首次结果已被覆盖，回「冲突」而不是回一个
//     「看起来对但可能是今天的值」的响应。
func resolveRuleReplay(
	ctx context.Context, rules model.RevenueRuleModel, logs model.RuleChangeLogModel,
	ruleCode, requestID string,
) (*rpc.RevenueRuleInfo, error) {
	hit, err := logs.FindByRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if hit == nil {
		// 唯一键冲突却查不到台账行：只能是并发同键，让调用方重试。
		return nil, fmt.Errorf("%w: request_id=%s 撞唯一键但台账无记录，请重试", model.ErrConcurrentUpdate, requestID)
	}
	if hit.RuleCode != ruleCode {
		return nil, fmt.Errorf("%w: request_id=%s 已用于规则 %s", model.ErrRequestIDConflict, requestID, hit.RuleCode)
	}
	cur, err := rules.FindByCode(ctx, ruleCode)
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.Version != hit.ToVersion {
		return nil, fmt.Errorf("%w: request_id=%s 之后规则 %s 又发生过变更",
			model.ErrRequestIDConflict, requestID, ruleCode)
	}
	return ruleInfo(cur), nil
}

// autoArchiveSuffixBytes 是 autoArchiveKey 后缀 `#a<rule_id>` 的最大宽度预算
// （`#a` 2 字节 + int64 最多 19 位十进制）。父 request_id 必须按它留出空间。
const autoArchiveSuffixBytes = 21

// applyRuleStateChange 在一个事务里完成「状态切换 + 旧 ACTIVE 自动归档 + 两笔台账」。
//
// 状态机（proto 注释即为口径）：DRAFT→ACTIVE、ACTIVE→ARCHIVED、DRAFT→ARCHIVED，
// 其余一律拒绝；ARCHIVED 是终态，没有回头路——要恢复只能新建规则草稿，
// 因为归档意味着「这套口径不再用于新周期」，复活它会让历史台账的解释变得含糊。
//
// 加锁顺序刻意是「先整段 source_type、再本行」：两条同来源规则并发生效时，
// 如果各自先锁自己那行再去看有没有别人 ACTIVE，就会互相看不见对方并各自成功，
// 最终同一来源留下两条 ACTIVE —— 计量该按哪条折算就成了悬案。
// 反之按 rule_id 升序整段加锁，后来者必然阻塞并看到前者写好的 ACTIVE，从而自动归档它。
func applyRuleStateChange(
	ctx context.Context, tx sqlx.Session, pre *model.RevenueRule,
	target int32, expectedVersion int64, operator, requestID, reason string,
) error {
	rules := model.NewRevenueRuleModel(sqlx.NewSqlConnFromSession(tx))
	logs := model.NewRuleChangeLogModel(sqlx.NewSqlConnFromSession(tx))

	var olds []*model.RevenueRule
	if target == model.RuleStateActive {
		var err error
		olds, err = rules.LockActiveBySource(ctx, pre.SourceType)
		if err != nil {
			return err
		}
	}

	cur, err := rules.LockByCode(ctx, pre.RuleCode)
	if err != nil {
		return err
	}
	if cur == nil || cur.RuleId != pre.RuleId {
		return fmt.Errorf("%w: rule_code=%s", model.ErrRuleNotFound, pre.RuleCode)
	}
	if cur.SourceType != pre.SourceType {
		// 上一步按旧来源加的段锁已经不管用了（草稿可以改 source_type）。
		return fmt.Errorf("%w: 规则来源刚被并发修改，请重载后重试", model.ErrConcurrentUpdate)
	}
	if cur.Version != expectedVersion {
		return fmt.Errorf("%w: rule_code=%s 服务端 version=%d，请求 expected_version=%d",
			model.ErrVersionConflict, cur.RuleCode, cur.Version, expectedVersion)
	}
	if err := checkRuleTransition(cur, target); err != nil {
		return err
	}

	if target == model.RuleStateActive {
		for _, old := range olds {
			if old.RuleId == cur.RuleId {
				continue
			}
			ok, err := rules.SetState(ctx, old.RuleId,
				int64(model.RuleStateActive), int64(model.RuleStateArchived), old.Version, operator)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: 归档旧 ACTIVE 规则 %s 未命中，请重试", model.ErrConcurrentUpdate, old.RuleCode)
			}
			if _, err := logs.Insert(ctx, &model.RuleChangeLog{
				RuleCode: old.RuleCode, SourceType: old.SourceType, Action: model.RuleActionAutoArchive,
				FromState: model.RuleStateActive, ToState: model.RuleStateArchived,
				FromUnitPrice: old.UnitPricePer1000, ToUnitPrice: old.UnitPricePer1000,
				FromVersion: old.Version, ToVersion: old.Version + 1,
				Operator:  operator,
				Reason:    trunc("因生效规则切换被自动归档；本次原因："+reason, model.MaxReasonBytes),
				RequestId: autoArchiveKey(requestID, old.RuleId),
			}); err != nil {
				return err
			}
		}
	}

	ok, err := rules.SetState(ctx, cur.RuleId, int64(cur.State), int64(target), expectedVersion, operator)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: rule_code=%s 状态切换未命中", model.ErrVersionConflict, cur.RuleCode)
	}
	action := model.RuleActionArchive
	if target == model.RuleStateActive {
		action = model.RuleActionActivate
	}
	if _, err := logs.Insert(ctx, &model.RuleChangeLog{
		RuleCode: cur.RuleCode, SourceType: cur.SourceType, Action: action,
		FromState: cur.State, ToState: target,
		FromUnitPrice: cur.UnitPricePer1000, ToUnitPrice: cur.UnitPricePer1000,
		FromVersion: cur.Version, ToVersion: cur.Version + 1,
		Operator: operator, Reason: reason, RequestId: requestID,
	}); err != nil {
		return err
	}
	return nil
}

// checkRuleTransition 判定状态迁移是否落在允许的三条边上，并把拒绝理由说清楚。
func checkRuleTransition(cur *model.RevenueRule, target int32) error {
	if cur.State == target {
		return fmt.Errorf("%w: rule_code=%s 已处于目标状态，不做无意义的版本递增",
			model.ErrRuleStateTransition, cur.RuleCode)
	}
	switch target {
	case model.RuleStateActive:
		if cur.State != model.RuleStateDraft {
			return fmt.Errorf("%w: 生效只能从 DRAFT 发起，当前 state=%d", model.ErrRuleStateTransition, cur.State)
		}
		return nil
	case model.RuleStateArchived:
		if cur.State != model.RuleStateDraft && cur.State != model.RuleStateActive {
			return fmt.Errorf("%w: 当前 state=%d 不可归档（ARCHIVED 是终态）", model.ErrRuleStateTransition, cur.State)
		}
		return nil
	default:
		return fmt.Errorf("%w: 目标状态只能是 ACTIVE 或 ARCHIVED，传入 %d", model.ErrRuleStateTransition, target)
	}
}
