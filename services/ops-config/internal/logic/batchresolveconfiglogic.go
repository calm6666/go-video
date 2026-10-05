package logic

import (
	"context"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BatchResolveConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBatchResolveConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchResolveConfigLogic {
	return &BatchResolveConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量解析（网关聚合用，最多 50 键）
func (l *BatchResolveConfigLogic) BatchResolveConfig(in *rpc.BatchResolveConfigReq) (*rpc.BatchResolveConfigReply, error) {
	lim := newLimits(l.svcCtx.Config)
	keys, err := normalizeBatchKeys(in.GetCfgKeys(), lim.batchKeys)
	if err != nil {
		return nil, err
	}
	scope := normalizeScope(in.GetScope())
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	target, err := rolloutInputOf(in.GetTarget())
	if err != nil {
		return nil, err
	}
	// 一次取回全部项：禁止循环 FindOne 把一次首页聚合打成 N 次往返。
	found, err := l.svcCtx.Models.ConfigItem.FindByKeys(l.ctx, keys, scope)
	if err != nil {
		return nil, err
	}
	useCache := in.GetCtx().GetRequestId() != ""
	ts := model.NowUnix()

	reply := &rpc.BatchResolveConfigReply{Configs: nil, MissingKeys: nil, Ttl: 0}
	minTTL := int32(-1)
	for _, k := range keys {
		if _, ok := found[k]; !ok {
			reply.MissingKeys = append(reply.MissingKeys, k)
			continue
		}
		view, rerr := resolveOne(l.ctx, l.svcCtx, l.Logger, lim, k, scope, target, useCache, ts)
		if rerr != nil {
			if isNotFoundAsResult(rerr) {
				// 停用 / 从未发布 / 数据不一致都只进 missing_keys：
				// 整批里一个坏键不该让网关的首页其它模块一起失败。
				reply.MissingKeys = append(reply.MissingKeys, k)
				continue
			}
			return nil, rerr
		}
		reply.Configs = append(reply.Configs, view)
		// 本批最小 TTL：任何一个键被收紧都会拖住全批。
		// 这是刻意保守 —— 不能让一个低 TTL 的键混进高 TTL 批次后被缓存住。
		if minTTL < 0 || view.GetTtl() < minTTL {
			minTTL = view.GetTtl()
		}
	}
	if minTTL > 0 {
		reply.Ttl = minTTL
	}
	return reply, nil
}

// normalizeBatchKeys 去空去重并校验条数。
// 超限报 ErrBatchTooLarge 而不是「截断前 N 个」：静默截断会让上游以为自己拿到了全部，
// 首页因此永久缺一个模块，而这在所有日志里看起来都是成功的。
func normalizeBatchKeys(in []string, maxKeys int) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		k := strings.TrimSpace(raw)
		if k == "" {
			continue
		}
		if err := checkCfgKey(k, limits{}); err != nil {
			return nil, err
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, model.ErrBatchEmpty
	}
	if maxKeys <= 0 {
		maxKeys = model.MaxResolveKeys
	}
	if len(out) > maxKeys {
		return nil, model.ErrBatchTooLarge
	}
	return out, nil
}
