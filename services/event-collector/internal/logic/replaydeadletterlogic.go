// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReplayDeadLetterLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReplayDeadLetterLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReplayDeadLetterLogic {
	return &ReplayDeadLetterLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 重放投递死信：重新进入投递队列，不重新做采样判定。
//
// 审计义务（AGENTS.md §8）：idempotency_key + operator + reason 三者必填，重放必须能回答
// 「谁、凭哪次请求、为什么重放」——这三列都落在 ec_dead_letter 行上（operator/replay_key/
// replay_reason，reason 入库前经 redactSensitive 收敛，见 dispatch.go replayOneDeadLetter）。
//
// 不重复注入（AGENTS.md §5）：
//   - 死信侧只接受 state=open 的迁移（MarkHandled 的 WHERE state=open），已 replayed/
//     discarded 的终态计入 skipped，不会被二次处置；
//   - 投递侧只接受 state=DEAD 的行回队（Pending.Requeue），已在队列里或已发送的行不动；
//   - 回队刻意保留原 envelope_event_id：下游按信封 event_id 去重（uniq 键），
//     所以「重放」不可能把一条已成功投递的事件变成第二条事实。
//
// 逐行成事、按行报告：一次重放里「正常入队 / 已是终态 / Outbox 行已被清理」三类会同时出现，
// 只有逐行才能给出 replayed/skipped/failed_ids 的诚实拆分。单行事务失败自动回滚，
// 该行留在 open，不留下「已标重放却永远没人投」的假象。
//
// 不重新采样、不重新脱敏：首次入库定型的 policy_version/sanitize_version 保持归因；
// 要改判必须走 ActivateDispatchPolicy 新版本 + 数据回填任务（proto 注释同义）。
func (l *ReplayDeadLetterLogic) ReplayDeadLetter(in *rpc.ReplayDeadLetterReq) (*rpc.ReplayDeadLetterReply, error) {
	key := strings.TrimSpace(in.GetIdempotencyKey())
	if key == "" || len(key) > maxIdemKeyBytes {
		return nil, model.ErrIdempotencyKeyRequired
	}
	operator := strings.TrimSpace(in.GetOperator())
	if !validOperatorName(operator) {
		return nil, fmt.Errorf("%w: operator 必填、<= %d 字节且不含空白", model.ErrOperatorRequired,
			maxOperatorBytes)
	}
	reason := strings.TrimSpace(in.GetReason())
	if reason == "" {
		return nil, fmt.Errorf("%w: 重放必须给出 reason（为什么重放，审计义务）", model.ErrOperatorRequired)
	}
	ids, err := l.normalizeIDs(in.GetDeadLetterIds())
	if err != nil {
		return nil, err
	}
	// fail closed：dispatcher 未启用时在任何写入之前拒绝。
	// 此时把死信迁成 replayed 只会让它「看起来已被处置」而实际永远投不出去；
	// 留在 open 才让「MQ 接好后按同一批 ID 重放」这条路径成立。
	if _, derr := dispatcherSender(l.svcCtx); derr != nil {
		return nil, derr
	}

	return withRoundDedup(l.ctx, l.svcCtx, rpcReplayDeadLetter, key, l.Logger,
		func() *rpc.ReplayDeadLetterReply { return &rpc.ReplayDeadLetterReply{} },
		func() (*rpc.ReplayDeadLetterReply, error) { return l.replayAll(ids, operator, key, reason) })
}

// replayAll 逐条处置死信并汇总三类结论。
func (l *ReplayDeadLetterLogic) replayAll(ids []int64, operator, key, reason string) (*rpc.ReplayDeadLetterReply, error) {
	found, err := l.loadDeadLetters(ids)
	if err != nil {
		return nil, err
	}
	now := timeNow()
	var replayed, skipped int32
	var failed []int64
	for _, id := range ids {
		dl := found[id]
		if dl == nil {
			// 查不到（ID 写错，或已处置行被留存清理）：进 failed_ids，
			// 不能算 skipped —— 那会把「找不到」伪装成「已经处置过」。
			failed = append(failed, id)
			l.Errorf("event-collector/logic: 重放的死信不存在 dead_letter_id=%d operator=%s", id,
				fitColumn(operator, maxOperatorBytes))
			continue
		}
		ok, rerr := replayOneDeadLetter(l.ctx, l.svcCtx, dl, operator, key, reason, now)
		switch {
		case rerr != nil:
			failed = append(failed, id)
			l.Errorf("event-collector/logic: 死信重放入队失败 dead_letter_id=%d event_id=%s: %v", id,
				fitColumn(dl.EventID, maxEventIDBytes), errorText(rerr))
		case ok:
			replayed++
		default:
			skipped++
		}
	}
	l.Infof("event-collector/logic: 死信重放请求完成 requested=%d replayed=%d skipped=%d failed=%d "+
		"operator=%s idempotency_key=%s", len(ids), replayed, skipped, len(failed),
		fitColumn(operator, maxOperatorBytes), fitColumn(key, maxIdemKeyBytes))
	return &rpc.ReplayDeadLetterReply{
		Replayed:  replayed,
		Skipped:   skipped,
		FailedIds: failed,
	}, nil
}

// normalizeIDs 校验并按单次上限收敛死信 ID 列表（去重、去非正数）。
//
// 去重是为了让 replayed+skipped+failed == 请求里真正不同的 ID 数：
// 不去重时同一个 ID 提交两次会既算 replayed 又算 skipped，报表上的处置数就超过实际行数。
func (l *ReplayDeadLetterLogic) normalizeIDs(raw []int64) ([]int64, error) {
	maxCap := l.svcCtx.Config.Collector.MaxReplayPerRequest
	if maxCap <= 0 {
		return nil, model.ErrBatchLimitTooLarge
	}
	seen := make(map[int64]struct{}, len(raw))
	out := make([]int64, 0, len(raw))
	for _, id := range raw {
		if id <= 0 {
			return nil, fmt.Errorf("%w: dead_letter_ids 含非正数 ID %d", model.ErrDeadLetterNotFound, id)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if int32(len(out)) > maxCap {
			return nil, fmt.Errorf("%w: 单次重放最多 %d 条（去重后仍然超限）", model.ErrBatchLimitTooLarge, maxCap)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: dead_letter_ids 必填", model.ErrDeadLetterNotFound)
	}
	return out, nil
}

// loadDeadLetters 批量回查死信行（分块，单块不超过 model 侧上限）。
func (l *ReplayDeadLetterLogic) loadDeadLetters(ids []int64) (map[int64]*model.DeadLetter, error) {
	out := make(map[int64]*model.DeadLetter, len(ids))
	for _, chunk := range chunkRows(ids, listChunkSize) {
		rows, err := l.svcCtx.DeadLetters.ListByIDs(l.ctx, nil, chunk)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r != nil {
				out[r.ID] = r
			}
		}
	}
	return out, nil
}
