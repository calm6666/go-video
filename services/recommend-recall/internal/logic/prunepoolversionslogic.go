package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

type PrunePoolVersionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPrunePoolVersionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PrunePoolVersionsLogic {
	return &PrunePoolVersionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// pruneOutcomeLogLimit 是逐版本结论在日志里最多渲染的条数
// （一次最多可清理 model.MaxVersionListLimit 个版本，不加上限等于允许一行日志塞进 500 条明细）。
const pruneOutcomeLogLimit = 20

// 单个版本的处理结论。清理是不可逆删除，每个被看过或被跳过的版本都要留下明确理由，
// 否则运维只能面对一个 deleted_rows=0 的"成功"响应猜为什么没删。
const (
	pruneActionPruned          = "pruned"                  // 条目清零且登记行已删
	pruneActionDryRun          = "dry_run_candidate"       // dry_run：只登记为候选，未执行任何 DELETE
	pruneActionItemsPartial    = "items_partially_deleted" // 命中 max_rows，本轮只删条目
	pruneActionKeptItems       = "kept_items_remain"       // 条目仍有剩余，保留登记行等下一批
	pruneActionKeptActive      = "kept_active_pointer"     // 指针指向这个版本（在线正在出数）
	pruneActionKeptWindow      = "kept_in_rollback_window" // 还在保留窗口内
	pruneActionKeptState       = "kept_not_prunable_state" // 状态不是 RETIRED/FAILED
	pruneActionRowStateChanged = "row_state_changed"       // 复核/删除时发现登记行已被并发推进或删除
)

// pruneOutcome 是单个版本的处理结论。
//
// 为什么只在日志里表达而不是塞进响应：PrunePoolVersionsReply 的契约字段是聚合值
// (scanned_versions/deleted_rows/dry_run/has_more)，本轮契约已冻结、不得加字段。
// 因此这里守住两条：
//  1. 聚合值一律是真实计数（删了多少行就是多少行，没有"整体成功"这种含糊值）；
//  2. 逐版本结论进日志，被保护没删不等于没处理。
type pruneOutcome struct {
	Version int64
	BatchID string
	State   int32
	Action  string
}

// 分批清理过期版本（由 services/cron 调用）
//
// 三道不可删除的保护，缺一不可：
//  1. **保留窗口**：keep_versions 必须 >= 配置 MinKeepVersions，清理水位线只取
//     "按 version DESC 排在窗口之后"的版本（model.PrunableBefore），窗口内版本连候选都不进；
//  2. **ACTIVE 保护**：候选只含 RETIRED/FAILED（model.ListPrunable 的状态条件），
//     这里再按 recall_pool_current 的指针版本显式排除一次 —— state 是冗余镜像，
//     镜像漂移时（指针在 V、V 的 state 被误标成 RETIRED）只有指针能证明"这批正在在线出数"；
//  3. **事务内复核**：删条目之前对每个目标版本 FindForUpdate 复核状态与指针，
//     挡住"候选列取之后、删除之前被 Publish 切回 CURRENT"的窗口（那等于删掉在线候选）。
//
// 删除顺序固定"先条目、后版本行"且同事务：反过来的话中途失败会留下没有登记行的条目
// （孤儿），下一轮清理按版本登记找不到它们，永久留在库里。
// 本路径不占幂等表：定位键是 (source, pool_key, version)，删除本身可重放，
// cron 重试最多重复一次空删除。
func (l *PrunePoolVersionsLogic) PrunePoolVersions(in *rpc.PrunePoolVersionsReq) (*rpc.PrunePoolVersionsReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: PrunePoolVersionsReq", model.ErrRequestRequired)
	}
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, fmt.Errorf("%w: repository is not assembled", repository.ErrSourceNotConfigured)
	}
	rc := l.svcCtx.Config.Recall
	opts := repo.Options()

	source, poolKey, err := requirePool(in.GetPool())
	if err != nil {
		return nil, err
	}
	// operator 必填：事后要能回答"谁批准的这次删除"。它只进日志 —— 被删的行本来就要消失，
	// 留不下签名，而给日志凭空加一次 RPC 换不回可审计性。
	operator, err := requiredRef("operator", in.GetOperator(), colRefID, model.ErrOperatorRequired)
	if err != nil {
		return nil, err
	}
	keep, err := checkKeepVersions(in.GetKeepVersions(), opts.MinKeepVersions)
	if err != nil {
		return nil, err
	}
	maxRows, err := checkPruneMaxRows(in.GetMaxRows(), int64(rc.PruneMaxRows))
	if err != nil {
		return nil, err
	}

	watermark, err := repo.PoolVersion.PrunableBefore(l.ctx, source, poolKey, keep)
	if err != nil {
		return nil, err
	}
	dryRun := in.GetDryRun()
	if watermark <= 0 {
		// 登记版本数还在保留窗口内：没有可清理内容，如实返回全零而不是"成功清理"。
		l.Logger.Infof("recommend-recall: prune pool source=%d pool_key=%s operator=%s nothing prunable "+
			"(every version inside rollback window keep=%d)", source, poolKeyForLog(poolKey), operator, keep)
		return &rpc.PrunePoolVersionsReply{DryRun: dryRun}, nil
	}
	candidates, err := repo.PoolVersion.ListPrunable(l.ctx, source, poolKey, watermark, opts.MaxVersionList)
	if err != nil {
		return nil, err
	}
	// 指针版本：读不到存储才是错误；"该池没上线"返回 0（此时没有版本受 ACTIVE 保护）。
	current, err := currentPointerVersion(l.ctx, repo, source, poolKey)
	if err != nil {
		return nil, err
	}
	targets, outcomes := selectPrunable(candidates, watermark, current)
	scanned, err := toInt32("scanned_versions", int64(len(candidates)))
	if err != nil {
		return nil, err
	}
	// 取满一页说明窗口外可能还有可清理版本，cron 需继续调度（本方法内部不循环，
	// 避免一次 RPC 长时间持有事务）。
	hasMore := len(candidates) >= opts.MaxVersionList

	if dryRun {
		for _, oc := range outcomes {
			if oc.Action == pruneActionPruned {
				oc.Action = pruneActionDryRun
			}
		}
		l.logOutcomes(source, poolKey, operator, keep, maxRows, true, outcomes)
		return &rpc.PrunePoolVersionsReply{
			ScannedVersions: scanned,
			DeletedRows:     0,
			DryRun:          true,
			HasMore:         hasMore,
		}, nil
	}

	var (
		itemsDeleted int64
		rowsDeleted  int64
		itemsRemain  bool
	)
	if len(targets) > 0 {
		err = repo.Transact(l.ctx, func(session sqlx.Session) error {
			var terr error
			itemsDeleted, rowsDeleted, itemsRemain, terr = l.pruneInTx(session, repo, source, poolKey,
				current, maxRows, targets, outcomes)
			return terr
		})
		if err != nil {
			l.Logger.Errorf("recommend-recall: prune pool source=%d pool_key=%s operator=%s failed: %v",
				source, poolKeyForLog(poolKey), operator, err)
			return nil, err
		}
	}
	if itemsRemain {
		hasMore = true
	}
	l.logOutcomes(source, poolKey, operator, keep, maxRows, false, outcomes)

	total, err := toInt32("deleted_rows", itemsDeleted+rowsDeleted)
	if err != nil {
		return nil, err
	}
	return &rpc.PrunePoolVersionsReply{
		ScannedVersions: scanned,
		DeletedRows:     total,
		DryRun:          false,
		HasMore:         hasMore,
	}, nil
}

// pruneInTx 在同一个事务内完成"复核 -> 删条目 -> 删版本登记行"，并把逐版本结论写回 outcomes。
//
// 复核用 FindForUpdate：与 PublishPoolVersion 争同一行锁，因此本事务持有期间
// 目标版本不可能被切回 CURRENT。锁顺序固定按 version 升序（ListPrunable 的排序），
// 与发布路径（先指针行、后版本行）不会构成环路。
func (l *PrunePoolVersionsLogic) pruneInTx(session sqlx.Session, repo *repository.Repository,
	source int32, poolKey string, current, maxRows int64, targets []*model.RecallPoolVersion,
	outcomes []*pruneOutcome) (itemsDeleted, rowsDeleted int64, itemsRemain bool, err error) {
	ctx := l.ctx
	verified := make([]*model.RecallPoolVersion, 0, len(targets))
	versions := make([]int64, 0, len(targets))
	for _, t := range targets {
		fresh, verr := repo.PoolVersion.FindForUpdate(ctx, session, source, poolKey, t.Version)
		if verr != nil {
			if errors.Is(verr, model.ErrVersionNotFound) {
				// 登记行已被别人删掉：本轮无事可做，结论如实记录。
				outcomeFor(outcomes, t.Version).Action = pruneActionRowStateChanged
				continue
			}
			return 0, 0, false, verr
		}
		if fresh.Version == current ||
			(fresh.State != model.VersionStateRetired && fresh.State != model.VersionStateFailed) {
			// 并发上线把这一批切回在线，或状态被别的作业推进过：条目一行都不许动。
			outcome := outcomeFor(outcomes, fresh.Version)
			outcome.State = fresh.State
			if fresh.Version == current {
				outcome.Action = pruneActionKeptActive
			} else {
				outcome.Action = pruneActionKeptState
			}
			l.Logger.Errorf("recommend-recall: prune skips source=%d pool_key=%s version=%d state=%s "+
				"(事务内复核不通过，指针版本=%d)", source, poolKeyForLog(poolKey), fresh.Version,
				toRPCState(fresh.State).String(), current)
			continue
		}
		verified = append(verified, fresh)
		versions = append(versions, fresh.Version)
	}
	if len(versions) == 0 {
		return 0, 0, false, nil
	}

	del, more, derr := repo.Pool.DeleteByVersionsInTx(ctx, session, source, poolKey, versions, maxRows)
	if derr != nil {
		return 0, 0, false, derr
	}
	itemsDeleted = del
	if more {
		// 命中 max_rows：条目没删完就不能动登记行，否则条目失去版本登记变成孤儿。
		for _, v := range versions {
			outcomeFor(outcomes, v).Action = pruneActionItemsPartial
		}
		return itemsDeleted, 0, true, nil
	}

	ids := make([]int64, 0, len(verified))
	for _, t := range verified {
		count, cerr := repo.Pool.CountByVersionInTx(ctx, session, source, poolKey, t.Version)
		if cerr != nil {
			return itemsDeleted, 0, false, cerr
		}
		if count > 0 {
			// DeleteByVersions 的 LIMIT 命中判定只说"这一批没删满"，
			// 还剩条目就必须保留登记行，让下一批继续（此处是逐版本的最终确认）。
			outcomeFor(outcomes, t.Version).Action = pruneActionKeptItems
			itemsRemain = true
			continue
		}
		ids = append(ids, t.ID)
	}
	if len(ids) == 0 {
		return itemsDeleted, 0, itemsRemain, nil
	}
	rows, rerr := repo.PoolVersion.Delete(ctx, session, ids)
	if rerr != nil {
		return itemsDeleted, 0, itemsRemain, rerr
	}
	rowsDeleted = rows
	if rowsDeleted != int64(len(ids)) {
		// 行锁已在本事务复核过状态，正常情况下这里必然全中；没全中说明登记行状态被
		// 事务外的路径改写过（或行已被删）。条目已清空但登记行还在，下一批继续，
		// 因此结论一律改成 row_state_changed，不再冒充 "pruned"。
		itemsRemain = true
		for _, id := range ids {
			for _, t := range verified {
				if t.ID == id {
					outcomeFor(outcomes, t.Version).Action = pruneActionRowStateChanged
				}
			}
		}
		l.Logger.Errorf("recommend-recall: prune deleted %d version rows of %d requested for source=%d "+
			"pool_key=%s（其余行的状态已被并发推进）", rowsDeleted, len(ids), source, poolKeyForLog(poolKey))
	}
	return itemsDeleted, rowsDeleted, itemsRemain, nil
}

// checkKeepVersions 校验保留窗口。
//
// 下界来自配置 MinKeepVersions（防止一次误参数把可回滚版本全删）；
// 上界取 model.MaxVersionListLimit：PrunableBefore 用 OFFSET keep-1 定位水位线，
// 窗口开得比单池版本列取上限还大只会把清理变成长扫描，没有实际意义。
func checkKeepVersions(keep int32, minKeep int) (int32, error) {
	if int(keep) < minKeep {
		return 0, fmt.Errorf("%w: keep_versions=%d < config MinKeepVersions=%d",
			model.ErrKeepVersionsTooSmall, keep, minKeep)
	}
	if int(keep) > model.MaxVersionListLimit {
		return 0, fmt.Errorf("%w: keep_versions=%d > %d", model.ErrLimitTooLarge, keep, model.MaxVersionListLimit)
	}
	return keep, nil
}

// checkPruneMaxRows 归一单次删除行数上限。
// 0 表示"用配置值 PruneMaxRows"；显式给值也不得越过配置上限与 model 硬上限
// （锁与主从延迟保护），model.CheckInt64Limit 是第二道独立闸门。
func checkPruneMaxRows(got, configured int64) (int64, error) {
	limit := got
	if limit == 0 {
		limit = configured
	}
	if limit <= 0 {
		return 0, fmt.Errorf("%w: max_rows=%d (config PruneMaxRows=%d)", model.ErrInvalidLimit, got, configured)
	}
	if limit > configured {
		return 0, fmt.Errorf("%w: max_rows=%d > config PruneMaxRows=%d", model.ErrLimitTooLarge, limit, configured)
	}
	if err := model.CheckInt64Limit(limit, model.MaxDeleteRows); err != nil {
		return 0, fmt.Errorf("%w: max_rows=%d exceeds model cap %d", err, limit, model.MaxDeleteRows)
	}
	return limit, nil
}

// selectPrunable 把 ListPrunable 的候选过一遍保护条件，返回 (待删版本, 逐版本结论)。
// Go 侧这一道是刻意的重复判定：model 的 SQL 条件是第一道，这一道挡住
// "水位线 / 指针 / 状态"中任意一处不一致（镜像漂移、并发上线）。
func selectPrunable(candidates []*model.RecallPoolVersion, watermark, current int64) ([]*model.RecallPoolVersion, []*pruneOutcome) {
	targets := make([]*model.RecallPoolVersion, 0, len(candidates))
	outcomes := make([]*pruneOutcome, 0, len(candidates))
	for _, row := range candidates {
		if row == nil {
			continue
		}
		oc := &pruneOutcome{Version: row.Version, BatchID: row.BatchID, State: row.State}
		switch {
		case row.Version == current:
			oc.Action = pruneActionKeptActive
		case row.Version >= watermark:
			oc.Action = pruneActionKeptWindow
		case row.State != model.VersionStateRetired && row.State != model.VersionStateFailed:
			oc.Action = pruneActionKeptState
		default:
			// 先记成"将被清理"，事务内的实际结论由 pruneInTx 覆写（pruned / kept_*）。
			oc.Action = pruneActionPruned
			targets = append(targets, row)
		}
		outcomes = append(outcomes, oc)
	}
	return targets, outcomes
}

// outcomeFor 按版本号找结论；找不到返回一个丢弃用的零值指针，绝不 panic。
// （outcomes 覆盖 selectPrunable 看过的每一行，正常路径必然命中。）
func outcomeFor(outcomes []*pruneOutcome, version int64) *pruneOutcome {
	for _, oc := range outcomes {
		if oc.Version == version {
			return oc
		}
	}
	return &pruneOutcome{Version: version, Action: "not_in_outcome_list"}
}

// logOutcomes 输出逐版本结论（超过 pruneOutcomeLogLimit 条时给出被省略的数量，不静默截断）。
func (l *PrunePoolVersionsLogic) logOutcomes(source int32, poolKey, operator string, keep int32,
	maxRows int64, dryRun bool, outcomes []*pruneOutcome) {
	if len(outcomes) == 0 {
		return
	}
	shown, omitted := outcomes, 0
	if len(shown) > pruneOutcomeLogLimit {
		omitted = len(shown) - pruneOutcomeLogLimit
		shown = shown[:pruneOutcomeLogLimit]
	}
	var sb strings.Builder
	for i, oc := range shown {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(strconv.FormatInt(oc.Version, 10))
		sb.WriteString(":")
		sb.WriteString(toRPCState(oc.State).String())
		sb.WriteString(":")
		sb.WriteString(oc.Action)
	}
	if omitted > 0 {
		sb.WriteString(" (+")
		sb.WriteString(strconv.Itoa(omitted))
		sb.WriteString(" more)")
	}
	l.Logger.Infof("recommend-recall: prune pool source=%d pool_key=%s operator=%s keep=%d max_rows=%d "+
		"dry_run=%t outcomes=%s", source, poolKeyForLog(poolKey), operator, keep, maxRows, dryRun, sb.String())
}
