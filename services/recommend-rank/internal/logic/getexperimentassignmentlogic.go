package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetExperimentAssignmentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetExperimentAssignmentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetExperimentAssignmentLogic {
	return &GetExperimentAssignmentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询/登记主体在实验中的稳定分桶
//
// 确定性（不变量：同一入参永远同一答案）：
//   - 桶号是 model.BucketOf(subject_type, subject_id, exp_key, hash_seed, 落库桶空间) 的纯函数，
//     不看时间、不看实例、不看请求顺序，因此任意两个实例会给出同一个变体；
//   - 实验的 hash_seed 取「(bucket_start,id) 最小的变体」的盐，且要求参与集合内所有变体同盐，
//     否则同一主体会因扫描顺序不同落到不同变体，直接返回 ErrExperimentSeedConflict；
//   - 已落库的 sticky 行优先返回：实验 PAUSED/STOPPED 之后仍回历史分组（跳组会让实验读数失效）。
//
// 只写一份事实：入参 bucket_count 与落库口径不一致时只用 model.ScaleBucket 换算回显，
// 绝不按调用方的口径再产生第二行记录（否则同一主体在表里会有两个互不相容的桶号）。
//
// 隐私（AGENTS.md §2/§7）：subject_id 只接受 MID 十进制串或设备 sha256 摘要，
// 明文设备号由 model.ValidateSubjectID 拒绝，本接口不会成为可反查的设备表。
func (l *GetExperimentAssignmentLogic) GetExperimentAssignment(in *rpc.GetExperimentAssignmentReq) (*rpc.GetExperimentAssignmentReply, error) {
	if l.svcCtx == nil || l.svcCtx.Repository == nil {
		return nil, model.ErrRepositoryNotConfigured
	}
	if in == nil {
		return nil, model.ErrSubjectIDRequired
	}
	expKey, err := requiredIdent("exp_key", in.GetExpKey())
	if err != nil {
		return nil, err
	}
	subjectType := int32(in.GetSubjectType())
	subjectID := lowerHex(in.GetSubjectId())
	if err := model.ValidateSubjectID(subjectType, subjectID); err != nil {
		return nil, err
	}
	storedCount := l.svcCtx.BucketCount
	if storedCount <= 0 {
		storedCount = model.DefaultBucketCount
	}
	// 入参桶空间只影响回显；上界防的是 ScaleBucket 的乘法溢出（入参攻击）。
	echoCount := in.GetBucketCount()
	if echoCount < 0 || echoCount > maxEchoBucketCount {
		return nil, fmt.Errorf("%w: bucket_count=%d 需落在 [0,%d]", model.ErrInvalidBucketCount, in.GetBucketCount(), maxEchoBucketCount)
	}
	if echoCount == 0 {
		echoCount = storedCount
	}

	repo := l.svcCtx.Repository
	rows, err := repo.Experiments().ListByExpKey(l.ctx, expKey, positiveInt(repo.Options().LayerVariantScanLimit, 200))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: %s", model.ErrExperimentNotFound, expKey)
	}
	seed, err := canonicalHashSeed(rows)
	if err != nil {
		return nil, err
	}
	if err := checkVariantBucketSpace(rows, storedCount); err != nil {
		return nil, err
	}

	// sticky 优先：表里已有这一行就以它为准，PAUSED/STOPPED 也不改判。
	row, err := repo.Assignments().FindOne(l.ctx, expKey, subjectType, subjectID, seed)
	switch {
	case err == nil:
		if row.BucketCount != storedCount {
			// 落库行属于另一个桶空间：换算回显会给出错误结论，宁可让人来核对。
			return nil, fmt.Errorf("%w: 已落库分桶 bucket_count=%d 与当前口径 %d 不一致，需人工核对",
				model.ErrInvalidBucketCount, row.BucketCount, storedCount)
		}
		return l.assignmentReply(expKey, subjectType, subjectID, row.BucketNo, row.VariantKey,
			row.AssignedAt, seed, false, echoCount, storedCount), nil
	case errors.Is(err, model.ErrAssignmentNotFound):
		// 首次分桶：继续往下算并落一行事实。
	default:
		return nil, err
	}

	bucketNo := model.BucketOf(subjectType, subjectID, expKey, seed, storedCount)
	hit, running := variantForBucket(rows, bucketNo, model.NowUnix())
	if !running {
		// 实验当前没有任何生效的 RUNNING 变体：此时落库会把主体永久钉在 control 上，
		// 因此返回显式错误，让调用方按「无实验」路径继续排序（不产生污染 sticky 的行）。
		return nil, fmt.Errorf("%w: %s 没有生效中的 RUNNING 变体", model.ErrExperimentNotRunning, expKey)
	}
	newRow := &model.RankExperimentAssignment{
		ExpKey:      expKey,
		LayerKey:    layerKeyOf(rows, hit),
		SubjectType: subjectType,
		SubjectID:   subjectID,
		HashSeed:    seed,
		BucketCount: storedCount,
		BucketNo:    bucketNo,
		Revision:    revisionOf(hit),
		AssignedAt:  model.NowUnix(),
	}
	existed, err := repo.Assignments().Insert(l.ctx, newRow)
	if err != nil {
		return nil, err
	}
	if existed {
		// INSERT IGNORE 命中唯一键：并发下别的实例已经写过，以表里的行为准（不重复计人）。
		sticky, err := repo.Assignments().FindOne(l.ctx, expKey, subjectType, subjectID, seed)
		if err != nil {
			return nil, err
		}
		return l.assignmentReply(expKey, subjectType, subjectID, sticky.BucketNo, sticky.VariantKey,
			sticky.AssignedAt, seed, false, echoCount, storedCount), nil
	}
	newRow.VariantKey = variantKeyOf(hit)
	l.Infof("recommend-rank: experiment assignment created exp_key=%s subject_type=%d bucket_no=%d/%d "+
		"variant_key=%s running=%t material=%q", expKey, subjectType, bucketNo, storedCount,
		newRow.VariantKey, hit != nil, model.BucketMaterial(subjectType, subjectID, expKey, seed))
	return l.assignmentReply(expKey, subjectType, subjectID, bucketNo, newRow.VariantKey,
		newRow.AssignedAt, seed, true, echoCount, storedCount), nil
}

// assignmentReply 组装回包；bucket_no 按调用方口径换算，落库口径始终是 storedCount。
func (l *GetExperimentAssignmentLogic) assignmentReply(expKey string, subjectType int32, subjectID string,
	bucketNo int32, variantKey string, assignedAt int64, seed string, newly bool, echoCount, storedCount int32,
) *rpc.GetExperimentAssignmentReply {
	if variantKey == "" {
		variantKey = model.ControlVariant
	}
	return &rpc.GetExperimentAssignmentReply{
		ExpKey:        expKey,
		SubjectType:   toRPCSubject(subjectType),
		SubjectId:     subjectID,
		BucketNo:      model.ScaleBucket(bucketNo, storedCount, echoCount),
		VariantKey:    variantKey,
		NewlyAssigned: newly,
		AssignedAt:    assignedAt,
		HashSeed:      seed,
	}
}

// canonicalHashSeed 取实验的规范哈希盐：ListByExpKey 已按 (bucket_start,id) 升序，
// 因此「第一个生效变体的盐」是确定的；参与集合内不同盐说明配置被改坏了（同一实验两套分流），
// 只能报错让人来判，不能挑一个继续。
// 参与集合优先取 RUNNING/PAUSED（正在管理的变体），全是 DRAFT/STOPPED 时才退回全集。
func canonicalHashSeed(rows []*model.RankExperiment) (string, error) {
	pool := make([]*model.RankExperiment, 0, len(rows))
	for _, r := range rows {
		if r.State == model.ExpStateRunning || r.State == model.ExpStatePaused {
			pool = append(pool, r)
		}
	}
	if len(pool) == 0 {
		pool = rows
	}
	seed := pool[0].HashSeed
	if seed == "" {
		return "", fmt.Errorf("%w: %s/%s 未登记 hash_seed", model.ErrHashSeedRequired, pool[0].ExpKey, pool[0].VariantKey)
	}
	for _, r := range pool[1:] {
		if r.HashSeed != seed {
			return "", fmt.Errorf("%w: %s 下 %s 用 %q、%s 用 %q，同一实验必须同盐",
				model.ErrExperimentSeedConflict, r.ExpKey, pool[0].VariantKey, seed, r.VariantKey, r.HashSeed)
		}
	}
	return seed, nil
}

// checkVariantBucketSpace 确认变体登记的桶空间与当前落库口径一致：
// 在 A 空间算出的桶号去比 B 空间的区间，会把人分错组，因此宁可拒绝。
// DRAFT/STOPPED 的历史行不参与校验（它们不承接当前流量）。
func checkVariantBucketSpace(rows []*model.RankExperiment, storedCount int32) error {
	for _, r := range rows {
		if r.State != model.ExpStateRunning && r.State != model.ExpStatePaused {
			continue
		}
		if r.BucketCount != storedCount {
			return fmt.Errorf("%w: 变体 %s/%s 登记桶空间 %d != 当前口径 %d",
				model.ErrInvalidBucketCount, r.ExpKey, r.VariantKey, r.BucketCount, storedCount)
		}
	}
	return nil
}

// variantForBucket 按桶号解析命中的变体：只认 RUNNING 且落在生效时间窗内的行。
// running 表示「该实验当前确有生效变体」，与「这一位是否命中变体」是两件事：
// 实验在跑但桶落在所有区间之外，就是合法的 control 对照组。
func variantForBucket(rows []*model.RankExperiment, bucketNo int32, at int64) (*model.RankExperiment, bool) {
	var hit *model.RankExperiment
	running := false
	for _, r := range rows {
		if r.State != model.ExpStateRunning {
			continue
		}
		if r.StartAt > at || (r.EndAt != 0 && r.EndAt <= at) {
			continue
		}
		running = true
		if hit == nil && bucketNo >= r.BucketStart && bucketNo < r.BucketEnd {
			hit = r
		}
	}
	return hit, running
}

// layerKeyOf 返回命中变体所在层；未命中（control）时取实验内第一个变体的层，
// 保证 control 行也能按层核对分流均匀度。
func layerKeyOf(rows []*model.RankExperiment, hit *model.RankExperiment) string {
	if hit != nil {
		return hit.LayerKey
	}
	if len(rows) == 0 {
		return defaultLayerKey
	}
	return rows[0].LayerKey
}

// variantKeyOf 给出落库的变体名，未命中一律 control（与 rpc 注释口径一致）。
func variantKeyOf(hit *model.RankExperiment) string {
	if hit == nil {
		return model.ControlVariant
	}
	return hit.VariantKey
}

// revisionOf 记录命中变体当时的 revision，让审计能回指「这次分组用的是第几版配置」。
func revisionOf(hit *model.RankExperiment) int32 {
	if hit == nil {
		return 0
	}
	return hit.Revision
}
