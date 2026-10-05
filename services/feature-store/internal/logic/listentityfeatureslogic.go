package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

type ListEntityFeaturesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListEntityFeaturesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListEntityFeaturesLogic {
	return &ListEntityFeaturesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 按主体导出特征（隐私核对）
//
// 这是主体「看到自己被存了什么」的合规路径，纪律有三条：
//   - 可见范围由 Privacy.ExportMaxPrivacyLevel 封顶（配置收紧时导出范围同步收紧），
//     min_privacy_level 只是调用方的兴趣下限，两者分别成列而不是互相覆盖；
//   - 只导出当前对外生效的版本（model.ListByEntity 内 JOIN ACTIVE 指针与非 RETIRED 状态），
//     并把 resolved_version / event_time / expire_at / degradation 逐条带出：
//     「这条值是哪一版产的、还新不新鲜」必须由响应说明；
//   - 分页有上限，并额外核对响应体字节数：条数合法不代表体积合法（512 维向量 × 100 条）。
//
// 日志只记条数与主体维度，绝不记 entity_id 与值原文。
func (l *ListEntityFeaturesLogic) ListEntityFeatures(
	in *rpc.ListEntityFeaturesReq) (*rpc.ListEntityFeaturesReply, error) {
	scope, entityID, err := checkEntity(in.GetEntity())
	if err != nil {
		return nil, err
	}
	if err := model.ValidatePageSize(in.GetPn(), in.GetPs()); err != nil {
		return nil, err
	}
	minPrivacy := int32(in.GetMinPrivacyLevel())
	if minPrivacy != model.PrivacyUnspecified && !model.ValidPrivacyLevel(minPrivacy) {
		return nil, fmt.Errorf("%w: min_privacy_level %d", model.ErrPrivacyUnsetNotAllowed, minPrivacy)
	}
	maxPrivacy := l.svcCtx.Config.Privacy.ExportMaxPrivacyLevel
	if !model.ValidPrivacyLevel(maxPrivacy) {
		// 配置越界时收敛到最高合法级别：导出可见范围不能因为一个坏配置变成「全部可见」。
		l.Errorw("invalid export privacy ceiling", logx.Field("configured", maxPrivacy))
		maxPrivacy = model.PrivacyUserProfile
	}
	if minPrivacy > maxPrivacy {
		// 请求的下限已在允许范围之上：能给出诚实答案的只有「这一档在本环境不对外导出」。
		// 返回空集而不是放宽上限，也不是把更敏感的行伪装成不存在（degradation 逐条已说明）。
		return &rpc.ListEntityFeaturesReply{}, nil
	}

	// DB 回源就是这一条路径的事实源：一次 JOIN 拿到「值 + 同快照的隐私级别与生效指针」。
	rows, total, err := l.svcCtx.Values.ListByEntity(l.ctx, scope, entityID, minPrivacy, maxPrivacy,
		in.GetPn(), in.GetPs())
	if err != nil {
		return nil, err
	}
	defs, err := l.loadDefs(rows)
	if err != nil {
		return nil, err
	}
	now := model.NowUnix()
	entries := make([]*rpc.FeatureEntry, 0, len(rows))
	for _, v := range rows {
		def := defs[model.DefinitionKey{FeatureKey: v.FeatureKey, Version: v.Version}]
		if def == nil {
			// JOIN 已经保证定义存在，走到这里只能是数据坏了：
			// 没有定义就没有口径，返回裸值等于让人把未知含义的数当特征用。
			return nil, fmt.Errorf("%w: %s@v%d has a value but no definition", model.ErrFeatureNotFound,
				v.FeatureKey, v.Version)
		}
		target := readTarget{def: def, version: v.Version, entityScope: scope, entityID: entityID,
			scopeOK: true}
		deg, _, err := model.ClassifyDegradation(model.ReadOutcome{
			DefinitionFound: true,
			State:           def.State,
			ResolvedVersion: v.Version,
			ValueFound:      true,
			// 导出视图里过期不是「隐藏这一条」的理由：主体看到的就是库里存着的这一条，
			// 但要带着 EXPIRED 标记，免得一个早已过 TTL 的值被当成当前生效画像。
			Expired:         v.IsExpired(now),
			AllowStale:      true,
			SourceAvailable: true,
		})
		if err != nil {
			return nil, fmt.Errorf("feature-store: %s@v%d: %w", v.FeatureKey, v.Version, err)
		}
		entry, err := valueEntry(target, v, deg, v.Version)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	reply := &rpc.ListEntityFeaturesReply{Entries: entries, Total: total}
	if size := proto.Size(reply); size > l.svcCtx.MaxResponseBytes() {
		return nil, fmt.Errorf("%w: response %d bytes > %d, reduce page size", model.ErrTooManyEntries,
			size, l.svcCtx.MaxResponseBytes())
	}
	l.Infow("entity features exported", logx.Field("entity_scope", scope),
		logx.Field("count", len(entries)), logx.Field("total", total))
	return reply, nil
}

// loadDefs 批量取本页行的定义（拿 TTL 与状态：导出条目必须能自证新鲜度口径）。
func (l *ListEntityFeaturesLogic) loadDefs(rows []*model.FeatureValue) (map[model.DefinitionKey]*model.FeatureDefinition, error) {
	out := make(map[model.DefinitionKey]*model.FeatureDefinition, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	keys := make([]model.DefinitionKey, 0, len(rows))
	seen := make(map[model.DefinitionKey]struct{}, len(rows))
	for _, v := range rows {
		k := model.DefinitionKey{FeatureKey: strings.TrimSpace(v.FeatureKey), Version: v.Version}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	for _, chunk := range chunkKeys(keys, model.MaxBatchFeatures) {
		got, err := l.svcCtx.Definitions.ListByKeys(l.ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("feature-store: load definitions for export: %w", err)
		}
		for k, v := range got {
			out[k] = v
		}
	}
	return out, nil
}
