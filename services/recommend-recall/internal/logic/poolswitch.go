// 本文件是 logic 包的手写扩展：PublishPoolVersion 与 RollbackPoolVersion 共用的
// 「版本指针切换」实现，不是 goctl 生成产物。
//
// 为什么必须共用一份：上线与回滚是同一个动作（把 recall_pool_current 从 X 移到 Y），
// 差别只有目标版本的来源状态与事件里的 rollback 标记。两套实现迟早在一侧漏掉 CAS
// 或漏掉 Outbox —— 那正是"指针切了但没审计/事件"的撕裂状态（AGENTS.md §5）。
//
// 一次切换在同一事务内完成四件事，缺一整事务回滚：
//  1. recall_pool_current 的 CAS 移动（Switch 的 expect_version）；
//  2. recall_pool_version 的状态推进（目标 -> CURRENT，旧 CURRENT -> RETIRED）；
//  3. recall_outbox 登记 recall.pool.published 事件（与指针同提交，杜绝"事件发了但池没切"）；
//  4. recall_idempotency.MarkSucceeded 落回放载荷（与业务写同提交，杜绝"标记成功但数据没落"）。
//
// 已发布版本不可变的落点在这里：本事务只改 state 与指针，绝不触碰 recall_pool 条目行；
// 条目写入路径（UpsertPoolItems）只允许写 BUILDING 版本，且与本事务在同一行锁上串行。
package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/eventenvelope"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/model"
)

// switchArgs 是一次版本切换的入参（上线与回滚共用）。
type switchArgs struct {
	repo *repository.Repository
	// scope 是幂等作用域；上线与回滚各自一个（同一把键不应既能解释成上线又能解释成回滚）。
	scope string
	// source/poolKey/version 定位「哪个池切到哪个版本」。
	source  int32
	poolKey string
	version int64
	// operator/reason 是审计必填项；idemKey 是写接口幂等键。
	operator  string
	reason    string
	idemKey   string
	rollback  bool
	logger    logx.Logger
	notSwitch error // 目标版本不可切换时返回的哨兵（ErrVersionNotReady / ErrRollbackTargetInvalid）
}

// switchOutcome 是切换结果，同时作为幂等回放载荷落 recall_idempotency.result_payload。
// 只落控制信息（版本对 + 事件 ID），不落候选内容：条目本身在 recall_pool 里可查。
type switchOutcome struct {
	Switched        bool   `json:"switched"`
	PreviousVersion int64  `json:"previous_version"`
	CurrentVersion  int64  `json:"current_version"`
	EventID         string `json:"event_id"`
}

// poolPublishedPayload 是 recall.pool.published.v1 的 payload。
//
// 只描述"某个池换了一批候选"这件事（AGENTS.md §7）：没有投放位、没有权重干预入口、
// 没有任何稿件正文，下游（feed/rank）据此失效自己的上游缓存。
type poolPublishedPayload struct {
	Source          int32  `json:"source"`
	PoolKey         string `json:"pool_key"`
	Version         int64  `json:"version"`
	PreviousVersion int64  `json:"previous_version"`
	BatchID         string `json:"batch_id"`
	ItemCount       int64  `json:"item_count"`
	Generator       string `json:"generator"`
	Rollback        bool   `json:"rollback"`
	Operator        string `json:"operator"`
	PublishedAt     int64  `json:"published_at"`
}

// run 执行一次切换。返回的第二个值是"本次调用是否为幂等重放"。
func (a *switchArgs) run(ctx context.Context) (*switchOutcome, bool, error) {
	fingerprint := repository.RequestFingerprint(
		a.scope,
		fmt.Sprintf("%d:%s", a.source, a.poolKey),
		strconv.FormatInt(a.version, 10),
		strconv.FormatBool(a.rollback),
	)
	claim, err := claimWrite(ctx, a.repo, a.scope, a.idemKey, fingerprint, a.operator)
	if err != nil {
		return nil, false, err
	}
	if !claim.First {
		// 已有执行者占着这把键：能回放就回放，不能回放就说明上一次还没收敛，让调用方重试。
		if err := heldOrConflict(claim); err != nil {
			return nil, false, err
		}
		var stored switchOutcome
		if err := replayPayload(claim.Existing.ResultPayload, &stored); err != nil {
			return nil, false, err
		}
		return &stored, true, nil
	}

	var out *switchOutcome
	err = a.repo.Transact(ctx, func(session sqlx.Session) error {
		var terr error
		out, terr = a.switchInTx(ctx, session)
		if terr != nil {
			return terr
		}
		payload, perr := payloadOf(out)
		if perr != nil {
			return perr
		}
		return a.markSucceeded(ctx, session, payload, out.EventID)
	})
	if err != nil {
		// 事务已回滚：把幂等键放回可重试状态，否则同一把键要等租约过期才能重试。
		if mErr := markFailedBestEffort(ctx, a.repo, a.scope, a.idemKey, err); mErr != nil {
			a.logger.Error(mErr)
		}
		return nil, false, err
	}
	return out, false, nil
}

// switchInTx 在调用方事务内完成 CAS + 状态推进 + 事件登记。
func (a *switchArgs) switchInTx(ctx context.Context, session sqlx.Session) (*switchOutcome, error) {
	// a. 保证有指针行可供 CAS（首次上线时 recall_pool_current 还没有这一行）。
	if err := a.repo.Current.EnsureRow(ctx, session, a.source, a.poolKey); err != nil {
		return nil, err
	}
	// b. 行锁读指针：拿到的是最新已提交值，同一池的两次上线被串行化。
	ptr, err := a.repo.Current.FindOneForUpdate(ctx, session, a.source, a.poolKey)
	if err != nil {
		return nil, err
	}
	// c. 行锁读目标版本：普通一致性读会给出过期的 state/item_count，
	//    可能把还在写入的 BUILDING 版本当成 READY 上线。
	target, err := a.repo.PoolVersion.FindForUpdate(ctx, session, a.source, a.poolKey, a.version)
	if err != nil {
		if errors.Is(err, model.ErrVersionNotFound) {
			return nil, fmt.Errorf("%w: source=%d pool_key=%s version=%d never registered",
				a.notSwitch, a.source, a.poolKey, a.version)
		}
		return nil, err
	}
	if target.State != model.VersionStateCurrent && !model.CanPublishState(target.State) {
		return nil, fmt.Errorf("%w: source=%d pool_key=%s version=%d state=%s",
			a.notSwitch, a.source, a.poolKey, a.version, toRPCState(target.State).String())
	}
	// d. 空池不得上线：条目数为 0 时在线侧无法区分"池没内容"与"池没上线"，
	//    降级原因会报错（ErrVersionNoItems 就是为这一条准备的）。
	if target.ItemCount <= 0 {
		return nil, fmt.Errorf("%w: source=%d pool_key=%s version=%d cannot go live",
			model.ErrVersionNoItems, a.source, a.poolKey, a.version)
	}
	now := a.now()

	// e. 指针已经在目标版本：幂等无操作。不移动指针、不发事件（事件描述"池快照变了"，
	//    没变就不该产生新事件），只在版本行镜像不一致时补齐 state。
	if ptr.Version == a.version {
		if target.State != model.VersionStateCurrent {
			publishedAt := ptr.PublishedAt
			if publishedAt <= 0 {
				publishedAt = now
			}
			ok, err := a.repo.PoolVersion.SetPublishedAt(ctx, session, target.ID, publishedAt, a.operator)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("%w: mirror repair on version id=%d", model.ErrVersionStateConflict, target.ID)
			}
		}
		return &switchOutcome{Switched: false, PreviousVersion: ptr.Version, CurrentVersion: ptr.Version}, nil
	}

	// f. CAS 移动指针。ErrSwitchConflict 直接上抛并回滚整事务：
	//    "读到冲突就无条件覆盖"等于把并发的另一次上线抹掉且不留审计。
	res, err := a.repo.Current.Switch(ctx, session, &model.RecallPoolCurrent{
		Source:      a.source,
		PoolKey:     a.poolKey,
		Version:     target.Version,
		BatchID:     target.BatchID,
		Operator:    a.operator,
		Note:        a.reason,
		PublishedAt: now,
	}, ptr.Version)
	if err != nil {
		return nil, err
	}
	out := &switchOutcome{Switched: res.Switched, PreviousVersion: res.Previous, CurrentVersion: res.Current}
	if !res.Switched {
		// CAS 未命中但指针已等于目标版本：并发对手切到了同一目标，属幂等重放，不重复发事件。
		return out, nil
	}

	// g. 先退役旧 CURRENT，再把目标置 CURRENT —— 顺序不能反：
	//    FindCurrentForUpdate 按 state=CURRENT 取行，两条同时为 CURRENT 时会读到刚置的那条。
	old, err := a.repo.PoolVersion.FindCurrentForUpdate(ctx, session, a.source, a.poolKey)
	if err != nil {
		return nil, err
	}
	if old != nil && old.ID != target.ID {
		retired, err := a.repo.PoolVersion.UpdateState(ctx, session, old.ID, model.VersionStateRetired,
			[]int32{model.VersionStateCurrent}, a.operator, a.reason)
		if err != nil {
			return nil, err
		}
		if !retired {
			return nil, fmt.Errorf("%w: retire previous version id=%d", model.ErrVersionStateConflict, old.ID)
		}
		if old.Version != res.Previous {
			// 指针与 state 镜像不一致（recall_pool_current 是读路径的唯一权威，state 是冗余视图）。
			// 这里只记告警不做额外补偿：在线读按指针出数，本事务也已把目标置为 CURRENT，
			// 语义仍然正确；把它变成硬失败反而会让正常上线被历史脏镜像挡住。
			a.logger.Errorf("recommend-recall: pool mirror drift source=%d pool_key=%s pointer_version=%d "+
				"state_current_version=%d (state 视图与指针漂移，读路径以指针为准)",
				a.source, a.poolKey, res.Previous, old.Version)
		}
	}
	published, err := a.repo.PoolVersion.SetPublishedAt(ctx, session, target.ID, now, a.operator)
	if err != nil {
		return nil, err
	}
	if !published {
		return nil, fmt.Errorf("%w: publish version id=%d", model.ErrVersionStateConflict, target.ID)
	}

	// h. 事件与指针切换同事务提交。
	eventID, err := a.writePublishedEvent(ctx, session, target, res.Previous, now)
	if err != nil {
		return nil, err
	}
	out.EventID = eventID
	return out, nil
}

// writePublishedEvent 登记 recall.pool.published 事件并返回 event_id。
func (a *switchArgs) writePublishedEvent(ctx context.Context, session sqlx.Session,
	target *model.RecallPoolVersion, previousVersion, publishedAt int64) (string, error) {
	raw, err := json.Marshal(&poolPublishedPayload{
		Source:          a.source,
		PoolKey:         a.poolKey,
		Version:         target.Version,
		PreviousVersion: previousVersion,
		BatchID:         target.BatchID,
		ItemCount:       target.ItemCount,
		Generator:       target.Generator,
		Rollback:        a.rollback,
		Operator:        a.operator,
		PublishedAt:     publishedAt,
	})
	if err != nil {
		return "", fmt.Errorf("recommend-recall: marshal pool published payload: %w", err)
	}
	env, err := eventenvelope.New(model.ProducerName, model.EventPoolPublished,
		model.AggregateTypePool, aggregateIDOf(a.source, a.poolKey, target.Version),
		model.PoolVersionSchemaVersion, json.RawMessage(raw), "")
	if err != nil {
		return "", fmt.Errorf("recommend-recall: build pool published envelope: %w", err)
	}
	envelopeJSON, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("recommend-recall: marshal pool published envelope: %w", err)
	}
	if err := a.repo.Outbox.Insert(ctx, session, &model.RecallOutbox{
		EventID:       env.EventID,
		EventType:     env.EventType,
		SchemaVersion: model.PoolVersionSchemaVersion,
		AggregateType: env.AggregateType,
		AggregateID:   env.AggregateID,
		Payload:       string(envelopeJSON),
		State:         model.OutboxStatePending,
		OccurredAt:    publishedAt,
	}); err != nil {
		return "", err
	}
	return env.EventID, nil
}

// markSucceeded 与业务写同事务落幂等成功标记。
// model 侧约定 RowsAffected=0 时不能直接判定失败（值完全相同的更新也返回 0），
// 因此这里重读确认状态，确认不了才算失败。
func (a *switchArgs) markSucceeded(ctx context.Context, session sqlx.Session, payload, eventID string) error {
	marked, err := a.repo.Idempotency.MarkSucceeded(ctx, session, a.scope, a.idemKey, payload, eventID)
	if err != nil {
		return err
	}
	if marked {
		return nil
	}
	row, ferr := a.repo.Idempotency.Find(ctx, a.scope, a.idemKey)
	if ferr != nil {
		return fmt.Errorf("recommend-recall: confirm idempotency mark for key=%s: %w", a.idemKey, ferr)
	}
	if strings.TrimSpace(row.ResultPayload) != payload {
		return fmt.Errorf("%w: idempotency row holds another result for key=%s", model.ErrIdempotencyExists, a.idemKey)
	}
	return nil
}

// now 返回本次切换的生效时间（Unix 秒）。
// 用一次取值贯穿"指针 published_at / 版本 published_at / 事件 occurred_at"，
// 避免出现三个相差几秒的时间戳让回放看起来像三次操作。
func (a *switchArgs) now() int64 { return nowUnix() }

// aggregateIDOf 是事件的聚合根 ID：source:pool_key:version，与 outbox.aggregate_id 列一致。
func aggregateIDOf(source int32, poolKey string, version int64) string {
	return fmt.Sprintf("%d:%s:%d", source, poolKey, version)
}
