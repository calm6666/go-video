package logic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

type UpsertPoolItemsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertPoolItemsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertPoolItemsLogic {
	return &UpsertPoolItemsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// upsertOutcome 是一次池条目写入的结果，同时作为幂等回放载荷落 recall_idempotency.result_payload。
//
// 只落控制信息（版本、影响行数、累计条数、状态），不落条目原文：
// 条目本身在 recall_pool 里按 (source, pool_key, version) 可查，复制进幂等表
// 等于把候选集备份到一张按 expire_at 过期的控制位表里（AGENTS.md §5 的定位约定）。
type upsertOutcome struct {
	Version   int64  `json:"version"`
	BatchID   string `json:"batch_id"`
	Written   int32  `json:"written"`
	ItemCount int32  `json:"item_count"`
	State     int32  `json:"state"`
	Sealed    bool   `json:"sealed"`
	// Deduplicated 只存在于回放分支：新执行的 deduplicated 恒为 false，
	// 它不进"本次写了什么"的语义，因此也不落库（落库再读回会把首执行标成重放）。
	Deduplicated bool `json:"-"`
}

// reply 投影成契约回复。
func (o *upsertOutcome) reply() *rpc.UpsertPoolItemsReply {
	return &rpc.UpsertPoolItemsReply{
		Version:      o.Version,
		Written:      o.Written,
		ItemCount:    o.ItemCount,
		State:        toRPCState(o.State),
		Deduplicated: o.Deduplicated,
	}
}

// 分批写入池条目到指定版本（idempotency_key 幂等）
//
// 核心不变量：**已封版的版本不可原地修改**。池内容的可见性变更只有一种方式——
// 写一个新版本，再经 PublishPoolVersion 切 recall_pool_current 指针（AGENTS.md §8 同源思路：
// 写入完成不代表可播放/可出数）。因此本方法只接受 BUILDING 版本：
//   - 版本行不存在 -> 事务内登记 BUILDING 行（首批）；
//   - 版本行是别人的 batch_id -> ErrVersionReuseBlocked（两个作业串写同一版本）；
//   - 版本行已是 READY/CURRENT/RETIRED/FAILED -> ErrVersionImmutable，整批拒绝。
//
// 全部写入在**一个事务**内完成（登记版本行 + 条目 upsert + 条数回填 + 封版 + 幂等标记），
// 因为这几步any一步失败都可能留下"条目写了一半但版本已 READY"的可上线半成品。
// 本方法绝不写 recall_pool_current（见 poolswitch.go：指针切换是 Publish/Rollback 的专属动作）。
func (l *UpsertPoolItemsLogic) UpsertPoolItems(in *rpc.UpsertPoolItemsReq) (*rpc.UpsertPoolItemsReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: UpsertPoolItemsReq", model.ErrRequestRequired)
	}
	repo := l.svcCtx.Repository
	if repo == nil {
		// 必需依赖缺失是装配缺陷，不是可降级状态：明确报错，不回一个"写入 0 条"的成功响应。
		return nil, fmt.Errorf("%w: repository is not assembled", repository.ErrSourceNotConfigured)
	}
	opts := repo.Options()

	source, poolKey, err := requirePool(in.GetPool())
	if err != nil {
		return nil, err
	}
	version := in.GetVersion()
	if version <= 0 {
		return nil, fmt.Errorf("%w: version=%d", model.ErrInvalidVersion, version)
	}
	batchID, err := requiredRef("batch_id", in.GetBatchId(), colRefID, model.ErrBatchIDRequired)
	if err != nil {
		return nil, err
	}
	// generator 是本写入路径的"操作者"（请求里没有单独的 operator 字段），
	// 它落 recall_pool_version.operator，缺失就断了"这批候选谁产的"这条追溯链。
	generator, err := requiredRef("generator", in.GetGenerator(), colRefID, model.ErrOperatorRequired)
	if err != nil {
		return nil, err
	}
	idemKey, err := requiredRef("idempotency_key", in.GetIdempotencyKey(), colIdempotencyKey,
		model.ErrIdempotencyKeyRequired)
	if err != nil {
		return nil, err
	}
	schemaVersion, err := poolSchemaVersion(in.GetSchemaVersion())
	if err != nil {
		return nil, err
	}
	items, aidSum, err := checkPoolItemBatch(in.GetItems(), opts.MaxBatchItems)
	if err != nil {
		return nil, err
	}

	// 指纹只取影响语义的字段：池、版本、批次、条数与 aid 和。
	// aidSum 即使极端情况下回绕也是确定的（同一请求两次算出同一串），
	// 它只喂给 sha256 做"同键同参数"判定，不是业务事实。
	fingerprint := repository.RequestFingerprint(
		model.IdempotencyScopeUpsertPoolItems,
		fmt.Sprintf("%d:%s", source, poolKey),
		strconv.FormatInt(version, 10),
		batchID,
		strconv.Itoa(len(items)),
		strconv.FormatInt(aidSum, 10),
	)
	claim, err := claimWrite(l.ctx, repo, model.IdempotencyScopeUpsertPoolItems, idemKey, fingerprint, generator)
	if err != nil {
		// 同键不同指纹在这里以 ErrIdempotencyFingerprintMismatch 原样上抛：
		// 调用方复用幂等键改参数是缺陷，执行第二份语义等于替它把数据写脏。
		return nil, err
	}
	if !claim.First {
		if err := heldOrConflict(claim); err != nil {
			return nil, err
		}
		var stored upsertOutcome
		if err := replayPayload(claim.Existing.ResultPayload, &stored); err != nil {
			return nil, err
		}
		stored.Deduplicated = true
		return stored.reply(), nil
	}

	out := &upsertOutcome{Version: version, BatchID: batchID, State: model.VersionStateBuilding}
	txErr := repo.Transact(l.ctx, func(session sqlx.Session) error {
		if err := l.upsertInTx(session, repo, source, poolKey, version, batchID, generator,
			schemaVersion, in.GetIsLastBatch(), items, out); err != nil {
			return err
		}
		payload, perr := payloadOf(out)
		if perr != nil {
			return perr
		}
		return confirmMarkSucceeded(l.ctx, repo, session, model.IdempotencyScopeUpsertPoolItems, idemKey, payload)
	})
	if txErr != nil {
		// 事务已回滚：把键放回可重试状态，否则同一把键要等租约过期才能重试。
		if mErr := markFailedBestEffort(l.ctx, repo, model.IdempotencyScopeUpsertPoolItems, idemKey, txErr); mErr != nil {
			l.Logger.Error(mErr)
		}
		return nil, txErr
	}
	return out.reply(), nil
}

// upsertInTx 在调用方事务内完成"锁版本 -> 登记 -> 写条目 -> 回填条数 -> 封版"。
//
// 读版本行必须用 FindForUpdate：普通一致性读会给出过期的 state，
// 于是"刚被 Publish 切成 CURRENT 的版本"在本事务里仍被看成 BUILDING，
// 结果就是往已上线的快照里追加条目（原地改写线上候选）。
func (l *UpsertPoolItemsLogic) upsertInTx(session sqlx.Session, repo *repository.Repository,
	source int32, poolKey string, version int64, batchID, generator string, schemaVersion int32,
	isLastBatch bool, items []model.PoolItemInput, out *upsertOutcome) error {
	ctx := l.ctx
	existing, err := repo.PoolVersion.FindForUpdate(ctx, session, source, poolKey, version)
	switch {
	case err == nil:
	case errors.Is(err, model.ErrVersionNotFound):
		existing = nil // 首批写入：下面 Register 负责建 BUILDING 行
	default:
		return err
	}
	if existing != nil {
		if existing.BatchID != batchID {
			return fmt.Errorf("%w: source=%d pool_key=%s version=%d belongs to batch %q, refused batch %q",
				model.ErrVersionReuseBlocked, source, poolKey, version, existing.BatchID, batchID)
		}
		if existing.State != model.VersionStateBuilding {
			return fmt.Errorf("%w: source=%d pool_key=%s version=%d state=%s；要改内容请写新版本再 PublishPoolVersion",
				model.ErrVersionImmutable, source, poolKey, version, toRPCState(existing.State).String())
		}
	}

	note := clipNote(fmt.Sprintf("upsert batch %s items=%d", batchID, len(items)))
	if err := repo.PoolVersion.Register(ctx, session, &model.RecallPoolVersion{
		Source:        source,
		PoolKey:       poolKey,
		Version:       version,
		BatchID:       batchID,
		Generator:     generator,
		SchemaVersion: schemaVersion,
		State:         model.VersionStateBuilding,
		Operator:      generator,
		Note:          note,
	}); err != nil {
		return err
	}
	if existing == nil {
		// Register 不返回主键；并发首批会被 uniq_pool_version 串行化，
		// 慢的一方在这里读到对方的行并因 batch_id 不同被 Register 拒绝。
		existing, err = repo.PoolVersion.FindForUpdate(ctx, session, source, poolKey, version)
		if err != nil {
			return err
		}
	}

	// 幂等写入：uniq_pool_item + ON DUPLICATE KEY UPDATE，重放同一批不产生重复行。
	// written 取 RowsAffected，按 MySQL ODKU 语义（新插入=1、值未变=0、更新=2）是"影响行数"
	// 而不是"新增行数"，重试拿到 2*条数或 0 都不是失败。
	written, err := repo.Pool.BatchUpsertInTx(ctx, session, source, poolKey, version, items)
	if err != nil {
		return err
	}
	affected, err := toInt32("written", written)
	if err != nil {
		return err
	}
	// 累计条数用事务内 COUNT(*)：本批条目尚未提交，事务外连接读不到，
	// 用"旧 item_count + len(items)"回填会在重试时把同一批算两次。
	count, err := repo.Pool.CountByVersionInTx(ctx, session, source, poolKey, version)
	if err != nil {
		return err
	}
	if err := repo.PoolVersion.UpdateItemCount(ctx, session, existing.ID, count); err != nil {
		return err
	}
	countReply, err := toInt32("item_count", count)
	if err != nil {
		return err
	}
	out.Written = affected
	out.ItemCount = countReply
	out.State = model.VersionStateBuilding

	if isLastBatch {
		// 契约没有"生成方声明总条数"的字段，因此声明值核对无处可做，
		// 这里以事务内 COUNT(*) 为唯一事实；空版本一律不许封版（ErrVersionNoItems）。
		if count <= 0 {
			return fmt.Errorf("%w: source=%d pool_key=%s version=%d 首批/末批都没落条目",
				model.ErrVersionNoItems, source, poolKey, version)
		}
		ok, err := repo.PoolVersion.UpdateState(ctx, session, existing.ID, model.VersionStateReady,
			[]int32{model.VersionStateBuilding}, generator, clipNote("sealed by batch "+batchID))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: version id=%d BUILDING->READY 未命中", model.ErrVersionStateConflict, existing.ID)
		}
		out.State = model.VersionStateReady
		out.Sealed = true
	}
	return nil
}

// poolSchemaVersion 归一条目结构版本：0 表示"用服务当前版本"，
// 其它不等于服务版本的值一律拒绝——收下它等于写进一批在线侧读不懂的条目。
func poolSchemaVersion(got int32) (int32, error) {
	if got == 0 {
		return model.PoolVersionSchemaVersion, nil
	}
	if got != model.PoolVersionSchemaVersion {
		return 0, fmt.Errorf("%w: request=%d service=%d", model.ErrSchemaVersionUnsupported,
			got, model.PoolVersionSchemaVersion)
	}
	return got, nil
}

// checkPoolItemBatch 校验整批条目并返回 (模型入参, aid 和)。
//
// 任一条目不合法就整批拒绝（不做部分写入）：静默丢掉一条会让"生成方声明的条数"
// 与实到条数对不上，封版时无人能判断差异是哪来的。
func checkPoolItemBatch(in []*rpc.PoolItemInput, maxBatch int) ([]model.PoolItemInput, int64, error) {
	if len(in) == 0 {
		return nil, 0, model.ErrItemsRequired
	}
	if len(in) > maxBatch {
		return nil, 0, fmt.Errorf("%w: items=%d > %d", model.ErrTooManyItems, len(in), maxBatch)
	}
	items := make([]model.PoolItemInput, 0, len(in))
	seen := make(map[int64]struct{}, len(in))
	var aidSum int64
	for i, item := range in {
		aid := item.GetAid()
		if aid <= 0 {
			return nil, 0, fmt.Errorf("%w: items[%d].aid=%d", model.ErrInvalidAid, i, aid)
		}
		score := item.GetScore()
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, 0, fmt.Errorf("%w: items[%d].aid=%d score=%v", model.ErrInvalidScore, i, aid, score)
		}
		if _, dup := seen[aid]; dup {
			return nil, 0, fmt.Errorf("%w: items[%d].aid=%d", model.ErrDuplicateItem, i, aid)
		}
		seen[aid] = struct{}{}
		items = append(items, model.PoolItemInput{Aid: aid, Score: score})
		aidSum += aid
	}
	return items, aidSum, nil
}

// confirmMarkSucceeded 与业务写同事务落幂等成功标记，并在 RowsAffected=0 时重读确认。
//
// model 侧约定：值完全相同的 UPDATE 也返回 0，因此 marked=false 不能直接判失败；
// 但也不能默认成功——只有确认库里的回放载荷与本次一致才算成功。
// 与 poolswitch.go 的 markSucceeded 同一口径（那份是切换路径的私有方法，这里不共用结构体）。
func confirmMarkSucceeded(ctx context.Context, repo *repository.Repository, session sqlx.Session,
	scope, key, payload string) error {
	marked, err := repo.Idempotency.MarkSucceeded(ctx, session, scope, key, payload, "")
	if err != nil {
		return err
	}
	if marked {
		return nil
	}
	row, ferr := repo.Idempotency.Find(ctx, scope, key)
	if ferr != nil {
		return fmt.Errorf("recommend-recall: confirm idempotency mark for key=%s: %w", key, ferr)
	}
	if strings.TrimSpace(row.ResultPayload) != payload {
		return fmt.Errorf("%w: idempotency row holds another result for key=%s", model.ErrIdempotencyExists, key)
	}
	return nil
}

// toInt32 收窄 int64 计数到 rpc 的 int32 字段。
// 超出范围必须报错，不能截断成一个看起来合理的数字。
func toInt32(name string, v int64) (int32, error) {
	if v < 0 {
		return 0, fmt.Errorf("recommend-recall: %s is negative: %d", name, v)
	}
	if v > math.MaxInt32 {
		return 0, fmt.Errorf("recommend-recall: %s=%d exceeds int32 reply field", name, v)
	}
	return int32(v), nil
}

// clipNote 把写进 note 的排障文本压到列宽内。
// note 不是业务事实（事实在 batch_id/item_count 里），这里允许截断；
// 入参文本由本服务自己渲染，不含调用方输入的超长字段。
func clipNote(s string) string {
	if len(s) > colNote {
		return s[:colNote]
	}
	return s
}
