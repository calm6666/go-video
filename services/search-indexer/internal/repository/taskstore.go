// taskstore.go 管理索引重建任务（search_index_task）。
//
// 幂等：uniq_request_id 唯一索引 + 提交前查重，重复提交返回同一 task_id；
// 断点：cursor_value 记录 content_id 区间下界，进程重启后从断点续跑；
// 互斥：ClaimNext 用 state 条件做 CAS 更新，多实例只有一个能抢到同一个任务。
package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go-video/common/idgen"
	"go-video/services/search-indexer/model"
)

// SubmitRebuildInput 提交重建任务的入参（与 rpc 解耦，便于 logic 层复用）。
type SubmitRebuildInput struct {
	Scope      string // full / partition / content_type
	ScopeValue string // partition: "1000-1999"；content_type: "1"/"2"/"3"
	Alias      string // 空表示默认别名
	RequestID  string // 幂等键，必填
	Operator   string // 提交人
}

// SubmitRebuildResult 提交结果。
type SubmitRebuildResult struct {
	Task       *model.SearchIndexTask
	Duplicated bool // true 表示命中 request_id 幂等，返回的是既有任务
}

// ValidateScope 校验重建范围与取值，返回归一化后的 scope_value。
// 独立成函数供单测覆盖，也保证「先校验再落库」，不写坏任务行。
func ValidateScope(scope, scopeValue string) (string, error) {
	switch scope {
	case model.ScopeFull:
		return "", nil
	case model.ScopeContentType:
		v := strings.TrimSpace(scopeValue)
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3 {
			return "", fmt.Errorf("%w: content_type 取值必须是 1/2/3，收到 %q", model.ErrInvalidScope, scopeValue)
		}
		return strconv.Itoa(n), nil
	case model.ScopePartition:
		v := strings.TrimSpace(scopeValue)
		parts := strings.Split(v, "-")
		if len(parts) != 2 {
			return "", fmt.Errorf("%w: partition 取值必须是 \"min-max\"，收到 %q", model.ErrInvalidScope, scopeValue)
		}
		lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || hi < lo {
			return "", fmt.Errorf("%w: partition 区间非法 %q", model.ErrInvalidScope, scopeValue)
		}
		return fmt.Sprintf("%d-%d", lo, hi), nil
	default:
		return "", fmt.Errorf("%w: %q", model.ErrInvalidScope, scope)
	}
}

// SubmitRebuildTask 注册一个重建任务（不在此处建索引，索引由 runner 幂等创建，
// 避免提交接口被 OpenSearch 可用性阻塞）。
func (r *Repository) SubmitRebuildTask(ctx context.Context, in SubmitRebuildInput) (*SubmitRebuildResult, error) {
	if strings.TrimSpace(in.RequestID) == "" {
		return nil, model.ErrInvalidRequestID
	}
	normalized, err := ValidateScope(in.Scope, in.ScopeValue)
	if err != nil {
		return nil, err
	}
	alias := r.opts.Alias(in.Alias)

	// 先查幂等：同一 request_id 不允许产生第二个任务，也不允许再分配一个新索引。
	if exist, err := r.taskMd.FindByRequestID(ctx, in.RequestID); err != nil {
		return nil, err
	} else if exist != nil {
		return &SubmitRebuildResult{Task: exist, Duplicated: true}, nil
	}

	taskID, err := idgen.Prefixed("sit")
	if err != nil {
		return nil, fmt.Errorf("search-indexer: generate task_id: %w", err)
	}
	now := model.NowUnix()
	task := &model.SearchIndexTask{
		TaskID:      taskID,
		Scope:       in.Scope,
		ScopeValue:  normalized,
		State:       model.TaskStatePending,
		CursorValue: strconv.FormatInt(r.initialCursor(), 10),
		TargetIndex: r.opts.NewIndexName(alias),
		Alias:       alias,
		Operator:    in.Operator,
		RequestID:   in.RequestID,
		Ctime:       now,
		Mtime:       now,
	}
	existed, err := r.taskMd.Insert(ctx, task)
	if err != nil {
		return nil, err
	}
	if existed {
		// 与并发提交撞在同一个 request_id 上，返回对方创建的任务。
		other, err := r.taskMd.FindByRequestID(ctx, in.RequestID)
		if err != nil {
			return nil, err
		}
		if other == nil {
			return nil, fmt.Errorf("search-indexer: request_id %s 冲突但任务不可见", in.RequestID)
		}
		return &SubmitRebuildResult{Task: other, Duplicated: true}, nil
	}
	return &SubmitRebuildResult{Task: task}, nil
}

// initialCursor 重建游标起点：content_id 从 1 开始发号。
func (r *Repository) initialCursor() int64 { return 1 }

// ClaimRebuildTask 抢占一个待执行任务并置为 running；无可抢任务返回 (nil, nil)。
func (r *Repository) ClaimRebuildTask(ctx context.Context) (*model.SearchIndexTask, error) {
	return r.taskMd.ClaimNext(ctx, model.NowUnix())
}

// RebuildTask 按 task_id 查询任务；不存在返回 (nil, nil)。
func (r *Repository) RebuildTask(ctx context.Context, taskID string) (*model.SearchIndexTask, error) {
	return r.taskMd.FindOne(ctx, taskID)
}

// ListRebuildTasks 分页查询任务。
func (r *Repository) ListRebuildTasks(ctx context.Context, state, cursor string, limit int) ([]*model.SearchIndexTask, string, error) {
	if state != "" && state != model.TaskStatePending && state != model.TaskStateRunning &&
		state != model.TaskStateSucceeded && state != model.TaskStateFailed && state != model.TaskStateCanceled {
		return nil, "", fmt.Errorf("%w: 未知任务状态 %q", model.ErrInvalidScope, state)
	}
	return r.taskMd.List(ctx, state, cursor, limit)
}

// UpdateRebuildProgress 推进任务游标与计数（仅 running 可更新）。
func (r *Repository) UpdateRebuildProgress(ctx context.Context, taskID string, cursor int64, processed, failed, total int64) error {
	return r.taskMd.UpdateProgress(ctx, taskID, strconv.FormatInt(cursor, 10), processed, failed, total, model.NowUnix())
}

// FinishRebuildTask 写入任务终态。errText 为空表示成功。
func (r *Repository) FinishRebuildTask(ctx context.Context, taskID, state, errText string) error {
	if state == model.TaskStateSucceeded {
		errText = ""
	}
	return r.taskMd.Finish(ctx, taskID, state, sanitizeError(errText), model.NowUnix())
}

// errTextReplacer 把错误摘要压成单行：OpenSearch/MySQL 的报错常带 \r\n 与制表符，
// 残留控制字符会撑坏 last_error/reason 列的运维列表与日志行。
var errTextReplacer = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")

// sanitizeError 截断错误文本，避免把大对象或响应体整段落库。
func sanitizeError(s string) string {
	s = errTextReplacer.Replace(s)
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}
