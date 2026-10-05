package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
)

// 本文件实现 policy.Store：把 Redis + MySQL 的事实读成一份可离线评估的快照。

// LookupCached 回放同一 request_id 近期已产出的裁决。
// 任何读取失败都按未命中处理：幂等回放是优化，不是裁决的前提条件。
func (r *Repository) LookupCached(ctx context.Context, requestID string) (*policy.Result, bool) {
	key := checkKey(requestID)
	if key == "" {
		return nil, false
	}
	var res policy.Result
	hit, err := r.cache.GetJSON(ctx, key, &res)
	if err != nil || !hit {
		return nil, false
	}
	if res.RequestID == "" {
		return nil, false
	}
	return &res, true
}

// LoadFacts 装载一次裁决所需的全部事实。
// 返回 error 即代表「风控无法给出可信裁决」，由引擎按 DegradePolicy 处理。
func (r *Repository) LoadFacts(ctx context.Context, in policy.Input) (*policy.Facts, error) {
	if in.Now == 0 {
		in.Now = nowUnix()
	}
	facts := &policy.Facts{CounterAvailable: true}

	// 1. 名单（黑名单优先，白名单跳过规则评估）。
	entries, err := r.activeListEntries(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("load list: %w", err)
	}
	for _, e := range entries {
		desc := describeListEntry(e)
		switch e.ListType {
		case model.ListTypeBlack:
			facts.BlacklistHits = append(facts.BlacklistHits, desc)
		case model.ListTypeWhite:
			facts.WhitelistHits = append(facts.WhitelistHits, desc)
		}
	}

	// 2. 生效处罚（含惰性过期推进）。
	punishment, err := r.activePunishment(ctx, in.Mid, in.Action, in.Now)
	if err != nil {
		return nil, fmt.Errorf("load punishment: %w", err)
	}
	facts.Punishment = punishment

	// 3. 规则与观测值。Redis 故障不把整次裁决打成 error，而是逐条标记不可观测。
	rules, err := r.activeRules(ctx, in.Action)
	if err != nil {
		return nil, fmt.Errorf("load rules: %w", err)
	}
	observations, counterAvailable := r.observe(ctx, in, rules)
	facts.Observations = observations
	facts.CounterAvailable = counterAvailable
	if !counterAvailable {
		// 计数器全盲时用进程内影子窗口兜底，防止降级窗口被脚本打穿。
		facts.LocalLimited = r.guard.limited(in.Action)
	}
	return facts, nil
}

// StoreResult 落审计日志并写入裁决回放缓存。
// DB 已被判定不可用时跳过日志写入（不再压同一份连接池），仍尝试 Redis 回放缓存。
func (r *Repository) StoreResult(ctx context.Context, in policy.Input, res *policy.Result) error {
	var errs []error
	if res.Basis != policy.BasisFallbackDB {
		logRow := &model.RiskCheckLog{
			RequestID:  in.RequestID,
			Mid:        in.Mid,
			Action:     in.Action,
			Decision:   res.Decision,
			Score:      res.Score,
			HitRuleIDs: model.FormatHits(res.HitRuleIDs, res.HitVersions),
			Basis:      res.Basis,
			Platform:   in.Platform,
			AppVersion: in.AppVersion,
			DeviceHash: in.DeviceHash,
			IPHash:     in.IPHash,
			Degraded:   boolToInt(res.Degraded),
		}
		if err := r.logMd.Insert(ctx, logRow); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.cache.SetJSON(ctx, checkKey(in.RequestID), res, int(r.cfg.DecisionCacheSeconds)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// activeListEntries 查询 mid/device/ip 三个维度上生效的名单条目。
// 一次 SQL 完成，避免三个维度三次往返。
func (r *Repository) activeListEntries(ctx context.Context, in policy.Input) ([]*model.RiskList, error) {
	pairs := make([]model.TargetPair, 0, 3)
	if in.Mid > 0 {
		pairs = append(pairs, model.TargetPair{TargetType: model.TargetTypeMid, TargetValue: strconv.FormatInt(in.Mid, 10)})
	}
	if in.DeviceHash != "" {
		pairs = append(pairs, model.TargetPair{TargetType: model.TargetTypeDevice, TargetValue: in.DeviceHash})
	}
	if in.IPHash != "" {
		pairs = append(pairs, model.TargetPair{TargetType: model.TargetTypeIpHash, TargetValue: in.IPHash})
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	return r.listMd.FindActive(ctx, in.Now, pairs)
}

// activePunishment 取覆盖该动作的生效处罚。
// 同时存在全域处罚与动作级处罚时，返回更具体的动作级处罚，
// 因为它的 reason_code 与时长才是本次裁决的真正依据。
func (r *Repository) activePunishment(ctx context.Context, mid int64, action int32, now int64) (*policy.PunishmentView, error) {
	if mid <= 0 {
		return nil, nil
	}
	// 惰性推进已到期但仍标 ACTIVE 的记录：读取即修复，不依赖 cron 时延。
	if _, err := r.punishMd.ExpireStale(ctx, mid, now); err != nil {
		return nil, err
	}
	rows, err := r.punishMd.ListActiveByMid(ctx, mid, now)
	if err != nil {
		return nil, err
	}
	var picked *model.RiskPunishment
	for _, row := range rows {
		if !row.Covers(action) {
			continue
		}
		if picked == nil || picked.Scope == model.ActionAll {
			picked = row
		}
	}
	if picked == nil {
		return nil, nil
	}
	return &policy.PunishmentView{
		ID:         picked.PunishmentID,
		Scope:      picked.Scope,
		Decision:   picked.Decision,
		StartAt:    picked.StartAt,
		EndAt:      picked.EndAt,
		ReasonCode: picked.ReasonCode,
	}, nil
}

// activeRules 读取对某动作生效的启用规则，带 Redis 短缓存。
// 规则变更（UpsertRule）会主动失效对应动作的缓存。
func (r *Repository) activeRules(ctx context.Context, action int32) ([]*model.RiskRule, error) {
	key := rulesKey(action)
	var cached []*model.RiskRule
	if hit, err := r.cache.GetJSON(ctx, key, &cached); err == nil && hit {
		return cached, nil
	}
	rules, err := r.ruleMd.ListActiveByAction(ctx, action)
	if err != nil {
		return nil, err
	}
	if err := r.cache.SetJSON(ctx, key, rules, int(r.cfg.RuleCacheSeconds)); err != nil {
		// 缓存写失败只影响读放大，不影响裁决。
		_ = err
	}
	return rules, nil
}

// invalidateRuleCache 失效某动作的规则缓存。
func (r *Repository) invalidateRuleCache(ctx context.Context, action int32) error {
	// 动作 0（全域）规则会影响所有动作，逐个失效避免脏读窗口。
	if action == model.ActionAll {
		var errs []error
		for _, a := range actionList() {
			if err := r.cache.Del(ctx, rulesKey(a)); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	return r.cache.Del(ctx, rulesKey(action))
}

// actionList 返回所有受保护动作。
func actionList() []int32 {
	return []int32{
		model.ActionSubmitVideo,
		model.ActionComment,
		model.ActionDanmaku,
		model.ActionFollow,
		model.ActionLogin,
		model.ActionRename,
		model.ActionLiveStart,
	}
}

// observe 为每条规则取观测值。
// 返回的 counterAvailable=false 表示 Redis 计数器整体不可读，
// 此时所有基于计数器的规则都被标为不可用（进入 skipped_rule_ids）。
func (r *Repository) observe(ctx context.Context, in policy.Input, rules []*model.RiskRule) ([]policy.Observation, bool) {
	observations := make([]policy.Observation, 0, len(rules))
	counterAvailable := true
	var (
		profile       *model.RiskDeviceProfile
		profileLoaded bool
		profileErr    error
	)

	for _, ru := range rules {
		obs := policy.Observation{
			RuleID:        ru.RuleID,
			Version:       ru.Version,
			Name:          ru.Name,
			Metric:        ru.Metric,
			Op:            ru.Op,
			Threshold:     ru.Threshold,
			WindowSeconds: ru.WindowSeconds,
			Decision:      ru.Decision,
			Priority:      ru.Priority,
		}
		switch ru.Metric {
		case model.MetricActionCount, model.MetricDeviceActionCount, model.MetricIpActionCount:
			key := subjectKeyForMetric(ru.Metric, in)
			if key == "" {
				break // 主体缺失（例如未上报 ip_hash），按不可观测处理
			}
			if !counterAvailable {
				break
			}
			total, observable, err := r.counter.Sum(ctx, ru.Metric, key, in.Action, ru.WindowSeconds, in.Now)
			if err != nil {
				counterAvailable = false
				break
			}
			if !observable {
				break // 窗口超出档位配置，绝不退化成近似窗口冒充命中
			}
			obs.Value = total
			obs.Available = true

		case model.MetricDeviceRiskScore, model.MetricDeviceMidCount:
			if in.DeviceHash == "" {
				break
			}
			if !profileLoaded {
				profile, profileErr = r.deviceMd.FindOne(ctx, in.DeviceHash)
				profileLoaded = true
			}
			if profileErr != nil || profile == nil {
				// 画像读失败按不可观测处理；设备从未出现过时按 0 分处理（确实是低风险）。
				break
			}
			if ru.Metric == model.MetricDeviceRiskScore {
				obs.Value = int64(profile.RiskScore)
			} else {
				obs.Value = profile.RelatedMidCount
			}
			obs.Available = true

		default:
			// 指标未实现取数：留在 skipped_rule_ids，运营后台据此发现配错的规则。
		}
		observations = append(observations, obs)
	}
	return observations, counterAvailable
}

// subjectKeyForMetric 把指标映射到计数器主体标识。
func subjectKeyForMetric(metric string, in policy.Input) string {
	switch metric {
	case model.MetricActionCount:
		if in.Mid <= 0 {
			return ""
		}
		return subjectMid(in.Mid)
	case model.MetricDeviceActionCount:
		if in.DeviceHash == "" {
			return ""
		}
		return subjectDevice(in.DeviceHash)
	case model.MetricIpActionCount:
		if in.IPHash == "" {
			return ""
		}
		return subjectIP(in.IPHash)
	default:
		return ""
	}
}

// describeListEntry 生成名单命中说明。
// 只输出类型与截断后的受控值，避免把完整摘要写进响应和日志。
func describeListEntry(e *model.RiskList) string {
	kind := "black"
	if e.ListType == model.ListTypeWhite {
		kind = "white"
	}
	target := "target"
	switch e.TargetType {
	case model.TargetTypeMid:
		target = "mid"
	case model.TargetTypeDevice:
		target = "device"
	case model.TargetTypeIpHash:
		target = "ip_hash"
	}
	return fmt.Sprintf("%s:%s=%s", kind, target, model.TruncateHash(e.TargetValue, 12))
}

func boolToInt(b bool) int32 {
	if b {
		return 1
	}
	return 0
}
