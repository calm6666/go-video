package logic

import (
	"context"
	"fmt"
	"strconv"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

type GetRecallConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRecallConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRecallConfigLogic {
	return &GetRecallConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 下发在线召回参数与池健康摘要
//
// 这是一份"参数面真值"：客户端/网关据此控制请求规模，所以每个数字都直接取自
// Repository.Options() 与 Config.Recall（同一份配置），本文件不出现任何业务字面量——
// 再写一遍数字就等于让"下发上限"与"真实校验"两件事各自漂移。
//
// 结果可被网关按 ttl_seconds 短缓存，因此除游客裁剪外响应与 mid 无关：
// 掺进用户态会让一个用户的响应被缓存给另一个用户。
//
// 诚实性（本契约的红线）：ready_pools 只描述"指针指向且版本行确实存在"的池。
// 指针在而版本行没了 -> 显式 ErrVersionNotFound（绝不能回一条 version=X、item_count=0
// 的记录把数据撕裂读成"这个池空了"）；一个上线池都没有 -> 空列表 + 错误日志，
// 参数面照常下发（客户端至少要知道上限），但"在线此刻无法出数"必须被看见。
func (l *GetRecallConfigLogic) GetRecallConfig(in *rpc.GetRecallConfigReq) (*rpc.GetRecallConfigReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: GetRecallConfigReq", model.ErrRequestRequired)
	}
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, fmt.Errorf("%w: repository is not assembled", repository.ErrSourceNotConfigured)
	}
	opts := repo.Options()
	rc := l.svcCtx.Config.Recall

	// scene 在契约里是"预留：场景级参数"，当前服务只下发全局参数。
	// 仍然校验列宽：不校验就等于告诉调用方"这个字段可以随便填"。
	if _, err := optionalText("scene", in.GetScene(), colScene); err != nil {
		return nil, err
	}
	mid := in.GetMid()
	if mid < 0 {
		// 本契约用 0 表示游客，负值只能来自调用方的错误映射（例如把 -1 当"未知用户"）。
		return nil, fmt.Errorf("%w: mid=%d", model.ErrRequestLogRequired, mid)
	}

	maxCandidates, err := toInt32("max_candidates", int64(opts.MaxCandidates))
	if err != nil {
		return nil, err
	}
	defaultLimit, err := toInt32("default_limit", int64(opts.DefaultLimit))
	if err != nil {
		return nil, err
	}
	perSourceMax, err := toInt32("per_source_max", int64(opts.PerSourceMax))
	if err != nil {
		return nil, err
	}
	maxSeedAids, err := toInt32("max_seed_aids", int64(opts.MaxSeedAids))
	if err != nil {
		return nil, err
	}
	maxSeedTags, err := toInt32("max_seed_tags", int64(opts.MaxSeedTags))
	if err != nil {
		return nil, err
	}
	maxExcludeAids, err := toInt32("max_exclude_aids", int64(opts.MaxExcludeAids))
	if err != nil {
		return nil, err
	}

	enabled := sourceList(rc.EnabledSources)
	defaults := sourceList(rc.DefaultSources)
	if mid == 0 {
		// 游客裁剪与 RecallCandidates 共用同一份判定（guestSources/trimGuestSources）：
		// 没有 mid 就没有关注路与协同种子，下发全集只会诱导客户端发出必然降级的请求。
		enabled, _ = trimGuestSources(enabled, guestSources(rc))
		defaults, _ = trimGuestSources(defaults, guestSources(rc))
	}

	readyPools, err := l.readyPools(rc)
	if err != nil {
		return nil, err
	}

	ttl := rc.TTLSeconds
	if ttl < 0 {
		// 负缓存期没有意义（且 cacheSetTopN 把 <=0 当"不缓存"），钳到 0 并留下日志：
		// 这里不报错，因为缓存提示不是业务事实，为一个写反的配置让整个参数面不可用不值。
		l.Logger.Errorf("recommend-recall: Recall.TTLSeconds=%d is negative, served as 0", ttl)
		ttl = 0
	}

	return &rpc.GetRecallConfigReply{
		MaxCandidates:  maxCandidates,
		DefaultLimit:   defaultLimit,
		PerSourceMax:   perSourceMax,
		EnabledSources: rpcSourceList(enabled),
		DefaultSources: rpcSourceList(defaults),
		MaxSeedAids:    maxSeedAids,
		MaxSeedTags:    maxSeedTags,
		MaxExcludeAids: maxExcludeAids,
		DegradeEnabled: rc.DegradeEnabled,
		FallbackSource: toRPCSource(int32(rc.FallbackSource)),
		TtlSeconds:     ttl,
		ReadyPools:     readyPools,
	}, nil
}

// readyPools 投影"当前有生效版本的池"：一次 ListCurrent 取指针 + 一次 ListByRefs 补明细。
//
// 逐池 FindByID 会是 N+1（池数上限 model.MaxPoolRefsPerQuery=200 时不可接受），
// 因此明细必须批量取；指针是读路径的唯一权威，item_count/batch_id/state 只是它的证据附件。
func (l *GetRecallConfigLogic) readyPools(rc config.RecallConf) ([]*rpc.PoolStatus, error) {
	repo := l.svcCtx.Repository
	limit := rc.MaxReadyPools
	if limit <= 0 {
		return nil, fmt.Errorf("%w: MaxReadyPools=%d", model.ErrInvalidLimit, limit)
	}
	if limit > model.MaxPoolRefsPerQuery {
		// 超上限要在这里明确报错，而不是让 model 侧的 CheckLimit 把整个参数面变成一次失败：
		// 配置写大了是发版问题，客户端拿到"服务不可用"只会重试。
		return nil, fmt.Errorf("%w: MaxReadyPools=%d > %d", model.ErrLimitTooLarge, limit, model.MaxPoolRefsPerQuery)
	}
	pointers, err := repo.Current.ListCurrent(l.ctx, limit)
	if err != nil {
		return nil, err
	}
	if len(pointers) == 0 {
		l.Logger.Error("recommend-recall: recall_pool_current has no published pool; " +
			"RecallCandidates cannot serve any candidate until an offline job publishes one")
		return []*rpc.PoolStatus{}, nil
	}

	refs := make([]model.PoolVersionRef, 0, len(pointers))
	for _, ptr := range pointers {
		if ptr == nil {
			continue
		}
		refs = append(refs, model.PoolVersionRef{Source: ptr.Source, PoolKey: ptr.PoolKey, Version: ptr.Version})
	}
	// limit 用 refs 长度：一个指针最多命中一行版本，多给不出更大的结果集也没有意义。
	versions, err := repo.PoolVersion.ListByRefs(l.ctx, refs, len(refs))
	if err != nil {
		return nil, err
	}
	index := make(map[string]*model.RecallPoolVersion, len(versions))
	for _, row := range versions {
		if row == nil {
			continue
		}
		index[poolVersionMapKey(row.Source, row.PoolKey, row.Version)] = row
	}

	now := nowUnix()
	out := make([]*rpc.PoolStatus, 0, len(pointers))
	staleCount := 0
	for _, ptr := range pointers {
		if ptr == nil {
			continue
		}
		if ptr.Version <= 0 {
			// ListCurrent 的 SQL 已限定 version>0；读到 0 说明读取条件被改坏，
			// 按"未上线"静默跳过会把一个 bug 伪装成健康的空列表。
			return nil, fmt.Errorf("recommend-recall: recall_pool_current source=%d pool_key=%s holds version %d",
				ptr.Source, ptr.PoolKey, ptr.Version)
		}
		row, ok := index[poolVersionMapKey(ptr.Source, ptr.PoolKey, ptr.Version)]
		if !ok {
			return nil, fmt.Errorf("%w: pool source=%d pool_key=%s current version %d has no version row "+
				"(pointer references a deleted or never registered batch)",
				model.ErrVersionNotFound, ptr.Source, ptr.PoolKey, ptr.Version)
		}
		if row.State != model.VersionStateCurrent {
			// state 是指针的冗余镜像（可由指针重建），镜像落后不影响在线出数，但必须能被看见：
			// 它意味着某次发布只写了指针或只写了状态。
			l.Logger.Errorf("recommend-recall: pool source=%d pool_key=%s version=%d state mirror is %s "+
				"while the current pointer is live", ptr.Source, ptr.PoolKey, ptr.Version,
				toRPCState(row.State).String())
		}
		// 批次以指针为准（它是"在线正在出哪一批"的权威），版本行的 batch_id 只用于发现漂移。
		batchID := ptr.BatchID
		if row.BatchID != batchID {
			l.Logger.Errorf("recommend-recall: pool source=%d pool_key=%s version=%d batch drift "+
				"pointer=%q version_row=%q", ptr.Source, ptr.PoolKey, ptr.Version, batchID, row.BatchID)
		}
		itemCount, err := toInt32("item_count", row.ItemCount)
		if err != nil {
			return nil, err
		}
		// published_at<=0 一并判 stale：指针已上线却没有生效时间，
		// 说明这行是手工修出来的，新鲜度无从判断，按最坏情况告警。
		stale := ptr.PublishedAt <= 0 || now-ptr.PublishedAt > rc.PoolStaleSeconds
		if stale {
			staleCount++
		}
		out = append(out, &rpc.PoolStatus{
			Pool:           poolRef(ptr.Source, ptr.PoolKey),
			CurrentVersion: ptr.Version,
			BatchId:        batchID,
			ItemCount:      itemCount,
			PublishedAt:    ptr.PublishedAt,
			Stale:          stale,
		})
	}
	if staleCount > 0 {
		// 调用方在 stale 字段里已经能看到，这里再落一条聚合日志：
		// 池过期是离线作业的问题，不告警就会一直"在线仍可读"地把旧候选发下去。
		l.Logger.Errorf("recommend-recall: %d/%d live pool(s) exceeded PoolStaleSeconds=%d",
			staleCount, len(out), rc.PoolStaleSeconds)
	}
	return out, nil
}

// poolVersionMapKey 是 (source, pool_key, version) 的索引键。
// 分隔符用 "|"：pool_key 的受控语法里没有该字符（见 model.ValidatePoolKey）。
func poolVersionMapKey(source int32, poolKey string, version int64) string {
	return strconv.FormatInt(int64(source), 10) + "|" + poolKey + "|" + strconv.FormatInt(version, 10)
}
