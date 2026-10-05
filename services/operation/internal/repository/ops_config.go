package repository

// 本文件实现运营配置读写（对应 docs/data-design.md 的 ops_config 聚合）。
//
// 范围约束（AGENTS.md §1）：只承载内容展示、审核阈值、灰度开关等运营参数，
// 不含会员/订单/支付/广告投放配置。
//
// 并发语义：op_config.version 是乐观锁列。SaveOpsConfig 必须带 expect_version
// （0 表示新建），版本被他人抢先推进时返回 ErrConfigVersionConflict，
// 由前端重新拉取后重试，避免后台双人编辑互相覆盖。
// 与 services/ops-config 的边界见 README：本期由本服务持有该表，
// ops-config 服务落地时以迁移+RPC 方式接管，调用方契约不变。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/operation/model"
)

// 配置值类型（op_config.value_type）。
const (
	ValueTypeString = "string"
	ValueTypeInt    = "int"
	ValueTypeBool   = "bool"
	ValueTypeJSON   = "json"
)

// defaultScope 未指定 scope 时的生效范围。
const defaultScope = "global"

// maxConfigValueLen 与 op_config.cfg_value 列宽一致（2000）。
const maxConfigValueLen = 2000

// SaveConfigInput SaveOpsConfig 入参。
type SaveConfigInput struct {
	CfgKey        string
	CfgValue      string
	ValueType     string
	Scope         string
	ExpectVersion int64
	State         int32
	Remark        string
}

// normalizeConfigScope 归一化生效范围。
func normalizeConfigScope(scope string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(scope))
	if s == "" {
		s = defaultScope
	}
	if len(s) > 32 {
		return "", fmt.Errorf("operation: config scope too long (max 32)")
	}
	for _, r := range s {
		isAlpha := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !isAlpha && r != '_' && r != '-' && r != '.' {
			return "", fmt.Errorf("operation: invalid config scope %q", s)
		}
	}
	return s, nil
}

// normalizeValueType 归一化值类型，空值按 string 处理。
func normalizeValueType(t string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "":
		return ValueTypeString, nil
	case ValueTypeString, ValueTypeInt, ValueTypeBool, ValueTypeJSON:
		return strings.ToLower(strings.TrimSpace(t)), nil
	default:
		return "", fmt.Errorf("operation: invalid value_type %q (string/int/bool/json)", t)
	}
}

// validateConfigValue 按 value_type 校验 cfg_value，避免把脏值推给读取方。
func validateConfigValue(valueType, value string) error {
	if len([]rune(value)) > maxConfigValueLen {
		return fmt.Errorf("%w: cfg_value too long (max %d)", model.ErrConfigValueInvalid, maxConfigValueLen)
	}
	switch valueType {
	case ValueTypeString:
		return nil
	case ValueTypeInt:
		if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil {
			return fmt.Errorf("%w: expect integer, got %q", model.ErrConfigValueInvalid, value)
		}
		return nil
	case ValueTypeBool:
		if _, err := strconv.ParseBool(strings.TrimSpace(value)); err != nil {
			return fmt.Errorf("%w: expect true/false, got %q", model.ErrConfigValueInvalid, value)
		}
		return nil
	case ValueTypeJSON:
		var v any
		if err := json.Unmarshal([]byte(value), &v); err != nil {
			return fmt.Errorf("%w: invalid json: %v", model.ErrConfigValueInvalid, err)
		}
		return nil
	default:
		return model.ErrConfigValueInvalid
	}
}

// configCacheKey 由 (scope, cfg_key) 派生缓存 key。
func configCacheKey(cfgKey, scope string) string {
	return fmt.Sprintf(keyConfig, scope, cfgKey)
}

// GetOpsConfig 读取配置：默认走缓存，refresh=true 强制回源并回填。
// 返回 (配置, 是否命中缓存)。配置不存在时返回 model.ErrConfigNotFound。
func (r *Repository) GetOpsConfig(ctx context.Context, cfgKey, scope string, refresh bool) (*model.OpsConfig, bool, error) {
	key := strings.TrimSpace(cfgKey)
	if key == "" {
		return nil, false, model.ErrConfigKeyEmpty
	}
	scopeVal, err := normalizeConfigScope(scope)
	if err != nil {
		return nil, false, err
	}
	ck := configCacheKey(key, scopeVal)

	if !refresh {
		var cached model.OpsConfig
		if r.cache.getJSON(ctx, ck, &cached) {
			// 缓存不过滤 state：已下线配置对后台仍需可读（便于复查与恢复），
			// 是否生效由调用方按 State 判定，与回源路径语义一致。
			return &cached, true, nil
		}
	}

	row, err := r.configMd.FindOne(ctx, key, scopeVal)
	if err != nil {
		return nil, false, err
	}
	if row == nil {
		// 空值哨兵防穿透：不存在的 key 短时间内不再反复打库。
		r.cache.setMiss(ctx, ck, defaultMissTTL)
		return nil, false, model.ErrConfigNotFound
	}
	r.cache.setJSON(ctx, ck, row, r.cfg.ConfigTTL)
	return row, false, nil
}

// SaveOpsConfig 写入配置（新建或按 expect_version 乐观锁更新），写后立即失效缓存。
func (r *Repository) SaveOpsConfig(ctx context.Context, actor Actor, in SaveConfigInput) (*model.OpsConfig, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.CfgKey)
	if key == "" || len([]rune(key)) > 128 {
		return nil, model.ErrConfigKeyEmpty
	}
	scopeVal, err := normalizeConfigScope(in.Scope)
	if err != nil {
		return nil, err
	}
	valueType, err := normalizeValueType(in.ValueType)
	if err != nil {
		return nil, err
	}
	if err := validateConfigValue(valueType, in.CfgValue); err != nil {
		return nil, err
	}
	state := in.State
	if state == 0 {
		state = model.StateEnable
	}
	if state != model.StateEnable && state != model.StateDisable {
		return nil, fmt.Errorf("operation: invalid config state %d (1 enable, 2 disable)", state)
	}
	remark := strings.TrimSpace(in.Remark)
	if len([]rune(remark)) > maxRemarkLen {
		return nil, fmt.Errorf("operation: config remark too long (max %d)", maxRemarkLen)
	}
	if in.ExpectVersion < 0 {
		return nil, fmt.Errorf("operation: expect_version must be >= 0")
	}

	ck := configCacheKey(key, scopeVal)
	if in.ExpectVersion == 0 {
		created := &model.OpsConfig{
			CfgKey:    key,
			CfgValue:  in.CfgValue,
			ValueType: valueType,
			Scope:     scopeVal,
			Version:   1,
			State:     state,
			Operator:  actor.AdminID,
			Remark:    remark,
		}
		if _, err := r.configMd.Insert(ctx, created); err != nil {
			if errors.Is(err, model.ErrConfigExists) {
				// 已存在却不带 expect_version：拒绝写，避免误覆盖别人的变更。
				return nil, fmt.Errorf("%w: config exists, pass expect_version", model.ErrConfigVersionConflict)
			}
			return nil, err
		}
		r.cache.del(ctx, ck)
		if err := r.writeAudit(ctx, actor, actionConfigSave, "ops_config", key+"/"+scopeVal, model.AuditResultOK); err != nil {
			return nil, err
		}
		return created, nil
	}

	current, err := r.configMd.FindOne(ctx, key, scopeVal)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, model.ErrConfigNotFound
	}
	if current.Version != in.ExpectVersion {
		return nil, fmt.Errorf("%w: current version %d", model.ErrConfigVersionConflict, current.Version)
	}
	pending := *current
	pending.CfgValue = in.CfgValue
	pending.ValueType = valueType
	pending.State = state
	pending.Operator = actor.AdminID
	pending.Remark = remark

	updated, ok, err := r.configMd.UpdateWithVersion(ctx, &pending, in.ExpectVersion)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 读后写之间被抢先推进：与显式版本不符同等处理，让调用方重试。
		return nil, model.ErrConfigVersionConflict
	}
	r.cache.del(ctx, ck)
	if err := r.writeAudit(ctx, actor, actionConfigSave, "ops_config",
		fmt.Sprintf("%s/%s?v=%d", key, scopeVal, updated.Version), model.AuditResultOK); err != nil {
		return nil, err
	}
	return updated, nil
}
