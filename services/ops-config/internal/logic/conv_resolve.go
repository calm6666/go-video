// 本文件是 logic 包的手写扩展（运行时解析内核），不是 goctl 生成产物。
//
// ResolveConfig 与 BatchResolveConfig 共用同一个 resolveOne：
// 两条入口的灰度结论必须逐字节一致，否则网关聚合出来的首页会和单键读取对不上，
// 而这类分歧只在特定 mid/版本上出现，是最难复现的一类线上问题。

package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// notFoundResults 是「按未命中返回」的错误集合，与真故障区分开。
var notFoundResults = []error{
	model.ErrConfigNotFound,
	model.ErrConfigDisabled,
	model.ErrUnpublished,
}

func isNotFoundAsResult(err error) bool {
	for _, e := range notFoundResults {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func trimKey(k string) string { return strings.TrimSpace(k) }

// resolveOne 解析一个键在当前请求上下文下的生效值。
//
// 步骤与失败语义：
//  1. 取配置项（短 TTL 投影）：不存在/停用 → ErrConfigNotFound / ErrConfigDisabled；
//  2. 取候选灰度规则（state=ON 且落在时间窗内，SQL 已按 priority,rule_id 排序）：
//     规则**不缓存**，见 configKeysOf 的注释；
//  3. model.PickRollout 取首个命中（纯函数，与写侧校验同一套语义）；
//  4. 命中的版本行若不存在 → 回落正式版本并打 Error 日志：
//     宁可不放量，也不能把一个不存在的值推给线上（AGENTS.md §8 状态只能按合法路径推进）；
//  5. 值快照走不可变投影；正式版本从未发布 → ErrUnpublished。
func resolveOne(ctx context.Context, svcCtx *svc.ServiceContext, logger logx.Logger, lim limits,
	cfgKey, scope string, in model.RolloutInput, useCache bool, ts int64) (*rpc.ConfigView, error) {

	item, err := loadConfigItem(ctx, svcCtx, lim, cfgKey, scope, useCache, logger)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, model.ErrConfigNotFound
	}
	if item.State != model.StateOn {
		return nil, model.ErrConfigDisabled
	}

	version := item.LatestVersion
	var ruleID int64
	var modeName string

	// ignore_rollout 只在「发布前预览正式值 / 故障排查」时用：跳过规则查询，直接读指针版本。
	if !in.IgnoreRollout {
		rules, rerr := svcCtx.Models.RolloutRule.ListCandidates(ctx, item.ConfigID, ts, lim.rulesPerConfig)
		if rerr != nil {
			return nil, rerr
		}
		if hit := model.PickRollout(rules, cfgKey, in, ts); hit != nil {
			ok, verr := versionExists(ctx, svcCtx, lim, item.ConfigID, hit.Version, useCache, logger)
			if verr != nil {
				return nil, verr
			}
			if ok {
				version = hit.Version
				ruleID = hit.RuleID
				modeName = model.ModeName(hit.Mode)
			} else {
				// 规则指向一个不存在的版本：读侧必须回落正式版本，并让这条规则在日志里现形。
				logger.Errorf("ops-config/resolve: 规则 %d 指向未发布的版本 config_id=%d version=%d，已回落正式版本 %d",
					hit.RuleID, item.ConfigID, hit.Version, item.LatestVersion)
			}
		}
	}

	if version <= 0 {
		return nil, model.ErrUnpublished
	}
	ver, verr := loadConfigVersion(ctx, svcCtx, lim, item.ConfigID, version, useCache, logger)
	if verr != nil {
		return nil, verr
	}
	if ver == nil {
		// 正式版本指针指向了一个不存在的快照：这是数据被人为改坏，不是「没配」。
		logger.Errorf("ops-config/resolve: config_id=%d 的 latest_version=%d 没有对应快照行", item.ConfigID, version)
		return nil, model.ErrVersionNotFound
	}
	return &rpc.ConfigView{
		ConfigId:      item.ConfigID,
		CfgKey:        item.CfgKey,
		Scope:         item.Scope,
		ValueType:     rpc.ConfigValueType(ver.ValueType),
		Value:         ver.Value,
		Version:       ver.Version,
		RolloutRuleId: ruleID,
		RolloutMode:   modeName,
		Ttl:           lim.clampTTL(lim.resolveTTL),
		Epoch:         item.Epoch,
		PublishedBy:   ver.OperatorName,
		PublishedAt:   ver.PublishedAt,
	}, nil
}

func loadConfigItem(ctx context.Context, svcCtx *svc.ServiceContext, lim limits,
	cfgKey, scope string, useCache bool, logger logx.Logger) (*model.ConfigItem, error) {

	load := func() (*model.ConfigItem, error) {
		return svcCtx.Models.ConfigItem.FindOne(ctx, cfgKey, scope)
	}
	if !useCache {
		return load()
	}
	// 指针键的 TTL 被压到 itemPointerTTLSeconds：它决定 latest_version/epoch 新旧，
	// 与其它投影不同，宁可多回源几次。
	return cacheLoad[*model.ConfigItem](ctx, cacheOf(svcCtx, useCache), lim.itemKey(scope, cfgKey),
		minInt(lim.resolveTTL, itemPointerTTLSeconds), logger, load)
}

func loadConfigVersion(ctx context.Context, svcCtx *svc.ServiceContext, lim limits,
	configID, version int64, useCache bool, logger logx.Logger) (*model.ConfigVersion, error) {

	load := func() (*model.ConfigVersion, error) {
		return svcCtx.Models.ConfigVersion.FindOne(ctx, configID, version)
	}
	if !useCache {
		return load()
	}
	return cacheLoad[*model.ConfigVersion](ctx, svcCtx.Cache, lim.versionKey(configID, version), lim.resolveTTL, logger, load)
}

func versionExists(ctx context.Context, svcCtx *svc.ServiceContext, lim limits,
	configID, version int64, useCache bool, logger logx.Logger) (bool, error) {

	if version <= 0 {
		return false, nil
	}
	ver, err := loadConfigVersion(ctx, svcCtx, lim, configID, version, useCache, logger)
	return ver != nil, err
}

func cacheOf(svcCtx *svc.ServiceContext, useCache bool) svc.CacheKV {
	if !useCache {
		return nil
	}
	return svcCtx.Cache
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
