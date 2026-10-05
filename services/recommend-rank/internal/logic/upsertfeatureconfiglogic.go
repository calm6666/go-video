package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertFeatureConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertFeatureConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertFeatureConfigLogic {
	return &UpsertFeatureConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 登记特征配置版本
//
// 版本不可变是这里的全部要点：同一 config_version 再登记，内容完全一致才回 deduplicated=true，
// 有任何差异（清单、缺失策略、feature-store 场景 key）都返回 ErrFeatureConfigExists。
// 原因：模型版本按 config_version 引用清单，允许原地改清单就等于让「同版本号 = 同语义」失效，
// 「模型版本可审计」会被偷偷变化的特征清单破坏（见 000002 迁移说明）。
// 改清单的唯一正确做法是发新版本号，再登记引用它的模型版本。
func (l *UpsertFeatureConfigLogic) UpsertFeatureConfig(in *rpc.UpsertFeatureConfigReq) (*rpc.UpsertFeatureConfigReply, error) {
	if l.svcCtx == nil || l.svcCtx.Repository == nil {
		return nil, model.ErrRepositoryNotConfigured
	}
	if in == nil {
		return nil, model.ErrOperatorRequired
	}
	operator, err := requireOperator(in.GetOperator())
	if err != nil {
		return nil, err
	}
	reason, err := requireReason("reason", in.GetReason())
	if err != nil {
		return nil, err
	}
	idempotencyKey, err := requireIdempotencyKey(in.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	configVersion, err := requiredIdent("config_version", in.GetConfigVersion())
	if err != nil {
		return nil, err
	}
	policy := strings.TrimSpace(in.GetMissingPolicy())
	if !model.ValidMissingPolicy(policy) {
		return nil, fmt.Errorf("%w: %q", model.ErrInvalidMissingPolicy, policy)
	}
	scene, err := optionalIdent("feature_store_scene", in.GetFeatureStoreScene(), colIdent)
	if err != nil {
		return nil, err
	}
	maxKeys := positiveInt32(l.svcCtx.Config.Rank.MaxFeatureKeys, 512)
	joined, count, err := normalizeFeatureKeys(in.GetFeatureKeys(), int(maxKeys))
	if err != nil {
		return nil, err
	}

	repo := l.svcCtx.Repository
	ctx := l.ctx
	row := &model.RankFeatureConfig{
		ConfigVersion:     configVersion,
		FeatureKeys:       joined,
		FeatureCount:      count,
		MissingPolicy:     policy,
		FeatureStoreScene: scene,
		KeysDigest:        sha256Hex(joined),
		State:             model.FeatureStateEnabled, // 新登记即生效；停用只能走 UpdateState（本契约未暴露停用入口）
		Revision:          1,
		Operator:          operator,
		Note:              reason,
	}
	err = repo.FeatureConfigs().Insert(ctx, row)
	switch {
	case err == nil:
		// idempotency_key 没有落库列（见 README 已知缺口），至少留在日志里供对账。
		l.Infof("recommend-rank: feature config registered version=%s keys=%d digest=%s operator=%s idempotency_key=%s",
			configVersion, count, row.KeysDigest, operator, idempotencyKey)
		return &rpc.UpsertFeatureConfigReply{
			ConfigVersion: row.ConfigVersion,
			FeatureCount:  row.FeatureCount,
			Revision:      row.Revision,
			Deduplicated:  false,
		}, nil
	case errors.Is(err, model.ErrFeatureConfigExists):
		return l.replayOrConflict(configVersion, joined, policy, scene, idempotencyKey, operator)
	default:
		return nil, err
	}
}

// replayOrConflict 处理「同 config_version 再登记」：完全一致才幂等回放，
// 任何差异都拒绝。冲突判定按字段逐个比对并把差异写进错误，
// 否则调用方只知道自己「撞版本了」，不知道撞在哪。
func (l *UpsertFeatureConfigLogic) replayOrConflict(configVersion, keys, policy, scene, idempotencyKey, operator string) (
	*rpc.UpsertFeatureConfigReply, error) {
	existing, err := l.svcCtx.Repository.FeatureConfigs().FindOne(l.ctx, configVersion)
	if err != nil {
		return nil, err
	}
	if diff := featureConfigDiff(existing, keys, policy, scene); len(diff) > 0 {
		return nil, fmt.Errorf("%w: %s 已登记，字段差异 %v；特征清单是不可变版本，改内容必须发新版本号",
			model.ErrFeatureConfigExists, configVersion, diff)
	}
	l.Infof("recommend-rank: feature config upsert deduplicated version=%s revision=%d operator=%s idempotency_key=%s",
		configVersion, existing.Revision, operator, idempotencyKey)
	return &rpc.UpsertFeatureConfigReply{
		ConfigVersion: existing.ConfigVersion,
		FeatureCount:  existing.FeatureCount,
		Revision:      existing.Revision,
		Deduplicated:  true,
	}, nil
}

// featureConfigDiff 返回请求与已登记行的差异字段名（顺序固定，便于断言与日志比对）。
func featureConfigDiff(existing *model.RankFeatureConfig, keys, policy, scene string) []string {
	var diff []string
	if existing.FeatureKeys != keys {
		diff = append(diff, "feature_keys")
	}
	if existing.MissingPolicy != policy {
		diff = append(diff, "missing_policy")
	}
	if existing.FeatureStoreScene != scene {
		diff = append(diff, "feature_store_scene")
	}
	return diff
}

// normalizeFeatureKeys 校验并渲染特征清单：逐条形态校验后去重、按升序以逗号连接，
// 与 model.JoinFeatureKeys 的落库口径完全一致（keys_digest 与巡检比对依赖它）。
// 条数上限按 config.Rank.MaxFeatureKeys 拒绝而不是静默裁剪——半截清单的指纹看起来和全量一样。
func normalizeFeatureKeys(in []string, maxCount int) (string, int32, error) {
	if len(in) == 0 {
		return "", 0, fmt.Errorf("%w: feature_keys is empty", model.ErrInvalidFeatureKey)
	}
	seen := make(map[string]struct{}, len(in))
	keys := make([]string, 0, len(in))
	for _, raw := range in {
		key, err := checkFeatureKey(raw)
		if err != nil {
			return "", 0, err
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if maxCount > 0 && len(keys) > maxCount {
		return "", 0, fmt.Errorf("%w: %d > %d", model.ErrTooManyFeatures, len(keys), maxCount)
	}
	sort.Strings(keys)
	return strings.Join(keys, ","), int32(len(keys)), nil
}

// checkFeatureKey 校验单个特征 key。
// 含逗号的 key 会破坏 csv 清单的往返一致性（拆分后变成两个 key），
// 含空白/控制字符的 key 在日志与 feature-store 查询里都无法稳定寻址，两者一律拒绝。
func checkFeatureKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", fmt.Errorf("%w: empty feature key", model.ErrInvalidFeatureKey)
	}
	if len(key) > maxFeatureKeyLen {
		return "", fmt.Errorf("%w: %d > %d", model.ErrInvalidFeatureKey, len(key), maxFeatureKeyLen)
	}
	if strings.ContainsRune(key, ',') {
		return "", fmt.Errorf("%w: %q must not contain ','", model.ErrInvalidFeatureKey, key)
	}
	for i := 0; i < len(key); i++ {
		if key[i] <= ' ' || key[i] == 0x7f {
			return "", fmt.Errorf("%w: %q contains blank or control characters", model.ErrInvalidFeatureKey, key)
		}
	}
	return key, nil
}
