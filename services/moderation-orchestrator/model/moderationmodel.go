package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ModerationTask 审核任务。
// (business, submission_id) 唯一索引保证同一对象同一时刻只有一个审核中任务，
// 防止领域服务重复提交导致并发审核冲突。
type ModerationTask struct {
	ID           int64  `db:"id"`            // 主键 ID
	SubmissionID int64  `db:"submission_id"` // 提交对象 ID（稿件/评论/弹幕等的内容主键）
	ContentType  int32  `db:"content_type"`  // 内容类型（ContentType 枚举）
	Mid          int64  `db:"mid"`           // 提交用户 ID
	UpMid        int64  `db:"up_mid"`        // UP 主 ID
	Business     string `db:"business"`      // 业务名（与 submission_id 共同定位对象）
	Reason       string `db:"reason"`        // 提交审核原因
	State        int32  `db:"state"`         // 任务状态（TaskState 枚举）
	Operator     int64  `db:"operator"`      // 操作人（运营 ID，0 表示系统）
	Ctime        int64  `db:"ctime"`         // 创建时间（Unix 秒）
	Mtime        int64  `db:"mtime"`         // 修改时间（Unix 秒）
}

// ModerationTaskModel moderation_task 表查询与写入接口。
type ModerationTaskModel interface {
	// Insert 创建任务；幂等：(business, submission_id) 已存在且未终态时返回 ErrDuplicateTask。
	Insert(ctx context.Context, t *ModerationTask) (int64, error)
	// FindOne 按主键查询任务。
	FindOne(ctx context.Context, taskID int64) (*ModerationTask, error)
	// FindBySubmission 按 (business, submission_id) 查询最新任务。
	FindBySubmission(ctx context.Context, business string, submissionID int64) (*ModerationTask, error)
	// UpdateState 更新任务状态；校验旧状态在 fromStates 中，否则返回 ErrInvalidStateTransition。
	UpdateState(ctx context.Context, taskID int64, toState int32, fromStates ...int32) error
	// List 分页查询任务列表；mid、contentType、state 为 0 时表示不过滤。
	List(ctx context.Context, mid int64, contentType, state int32, pn, ps int32) ([]*ModerationTask, int32, error)
}

type defaultModerationTaskModel struct {
	conn sqlx.SqlConn
}

// NewModerationTaskModel 创建 ModerationTaskModel 实现。
func NewModerationTaskModel(conn sqlx.SqlConn) ModerationTaskModel {
	return &defaultModerationTaskModel{conn: conn}
}

func (m *defaultModerationTaskModel) Insert(ctx context.Context, t *ModerationTask) (int64, error) {
	now := nowUnix()
	t.Ctime = now
	t.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO moderation_task (submission_id, content_type, mid, up_mid, business, reason, state, operator, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)",
		t.SubmissionID, t.ContentType, t.Mid, t.UpMid, t.Business, t.Reason, t.State, t.Operator, t.Ctime, t.Mtime)
	if err != nil {
		return 0, fmt.Errorf("moderation_task Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("moderation_task Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultModerationTaskModel) FindOne(ctx context.Context, taskID int64) (*ModerationTask, error) {
	var t ModerationTask
	if err := m.conn.QueryRowCtx(ctx, &t,
		"SELECT id, submission_id, content_type, mid, up_mid, business, reason, state, operator, ctime, mtime FROM moderation_task WHERE id = ?",
		taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("moderation_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultModerationTaskModel) FindBySubmission(ctx context.Context, business string, submissionID int64) (*ModerationTask, error) {
	var t ModerationTask
	if err := m.conn.QueryRowCtx(ctx, &t,
		"SELECT id, submission_id, content_type, mid, up_mid, business, reason, state, operator, ctime, mtime FROM moderation_task WHERE business = ? AND submission_id = ? ORDER BY id DESC LIMIT 1",
		business, submissionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("moderation_task FindBySubmission: %w", err)
	}
	return &t, nil
}

func (m *defaultModerationTaskModel) UpdateState(ctx context.Context, taskID int64, toState int32, fromStates ...int32) error {
	// 校验旧状态在 fromStates 中：CAS 形式更新，避免并发覆盖。
	if len(fromStates) == 0 {
		return fmt.Errorf("moderation_task UpdateState: %w", ErrInvalidStateTransition)
	}
	// 占位符顺序：SET state=?, mtime=? WHERE id=? AND state IN (?, ...);
	// 参数顺序需与占位符顺序对齐：toState, nowUnix, taskID, fromStates...
	args := make([]interface{}, 0, len(fromStates)+3)
	args = append(args, toState, nowUnix(), taskID)
	for _, s := range fromStates {
		args = append(args, s)
	}
	placeholders := ""
	for i := range fromStates {
		if i == 0 {
			placeholders = "?"
		} else {
			placeholders += ",?"
		}
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE moderation_task SET state = ?, mtime = ? WHERE id = ? AND state IN ("+placeholders+")",
		args...)
	if err != nil {
		return fmt.Errorf("moderation_task UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("moderation_task UpdateState RowsAffected: %w", err)
	}
	if aff == 0 {
		// 旧状态不在 fromStates 中，或任务不存在。
		return fmt.Errorf("moderation_task UpdateState: %w", ErrInvalidStateTransition)
	}
	return nil
}

func (m *defaultModerationTaskModel) List(ctx context.Context, mid int64, contentType, state int32, pn, ps int32) ([]*ModerationTask, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	// 动态拼接 WHERE 条件；0 值表示不过滤。
	where := "WHERE 1=1"
	args := make([]interface{}, 0, 4)
	if mid > 0 {
		where += " AND mid = ?"
		args = append(args, mid)
	}
	if contentType > 0 {
		where += " AND content_type = ?"
		args = append(args, contentType)
	}
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM moderation_task "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("moderation_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(args, ps, offset)
	var rows []*ModerationTask
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, submission_id, content_type, mid, up_mid, business, reason, state, operator, ctime, mtime FROM moderation_task "+where+" ORDER BY id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("moderation_task List list: %w", err)
	}
	return rows, total, nil
}

// ModerationRule 审核规则。
// 规则版本和操作人必填；规则可被多个任务复用。
type ModerationRule struct {
	ID       int64  `db:"id"`       // 主键 ID
	Name     string `db:"name"`     // 规则名
	Keywords string `db:"keywords"` // 关键词（逗号分隔，可空）
	ModelID  string `db:"model_id"` // 模型 ID（机审模型版本，可空）
	Priority int32  `db:"priority"` // 优先级（越大越优先）
	Action   int32  `db:"action"`   // 命中后的结论动作（Verdict）
	State    int32  `db:"state"`    // 0 禁用、1 启用
	Operator int64  `db:"operator"` // 操作人（运营 ID，必填）
	Ctime    int64  `db:"ctime"`    // 创建时间（Unix 秒）
	Mtime    int64  `db:"mtime"`    // 修改时间（Unix 秒）
}

// ModerationRuleModel moderation_rule 表查询与写入接口。
type ModerationRuleModel interface {
	// Insert 创建规则；操作人必填。
	Insert(ctx context.Context, r *ModerationRule) (int64, error)
	// FindOne 按主键查询规则。
	FindOne(ctx context.Context, ruleID int64) (*ModerationRule, error)
	// ListEnabled 查询启用的规则（按优先级倒序）。
	ListEnabled(ctx context.Context) ([]*ModerationRule, error)
}

type defaultModerationRuleModel struct {
	conn sqlx.SqlConn
}

// NewModerationRuleModel 创建 ModerationRuleModel 实现。
func NewModerationRuleModel(conn sqlx.SqlConn) ModerationRuleModel {
	return &defaultModerationRuleModel{conn: conn}
}

func (m *defaultModerationRuleModel) Insert(ctx context.Context, r *ModerationRule) (int64, error) {
	now := nowUnix()
	r.Ctime = now
	r.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO moderation_rule (name, keywords, model_id, priority, action, state, operator, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.Name, r.Keywords, r.ModelID, r.Priority, r.Action, r.State, r.Operator, r.Ctime, r.Mtime)
	if err != nil {
		return 0, fmt.Errorf("moderation_rule Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("moderation_rule Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultModerationRuleModel) FindOne(ctx context.Context, ruleID int64) (*ModerationRule, error) {
	var r ModerationRule
	if err := m.conn.QueryRowCtx(ctx, &r,
		"SELECT id, name, keywords, model_id, priority, action, state, operator, ctime, mtime FROM moderation_rule WHERE id = ?",
		ruleID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("moderation_rule FindOne: %w", err)
	}
	return &r, nil
}

func (m *defaultModerationRuleModel) ListEnabled(ctx context.Context) ([]*ModerationRule, error) {
	var rows []*ModerationRule
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, name, keywords, model_id, priority, action, state, operator, ctime, mtime FROM moderation_rule WHERE state = 1 ORDER BY priority DESC, id ASC"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("moderation_rule ListEnabled: %w", err)
	}
	return rows, nil
}

// ModerationResult 审核结论。
// task_id 唯一索引保证一个任务只有一条最新结论。
type ModerationResult struct {
	ID       int64  `db:"id"`        // 主键 ID
	TaskID   int64  `db:"task_id"`   // 关联任务 ID
	Verdict  int32  `db:"verdict"`   // 结论（Verdict 枚举）
	Reason   string `db:"reason"`    // 结论原因（关键词命中、模型分数等）
	WorkerID int64  `db:"worker_id"` // worker 实例 ID（机审）
	Reviewer int64  `db:"reviewer"`  // 人审员 ID（人审）
	Ctime    int64  `db:"ctime"`     // 创建时间（Unix 秒）
}

// ModerationResultModel moderation_result 表查询与写入接口。
type ModerationResultModel interface {
	// Upsert 插入或覆盖结论；task_id 唯一。
	Upsert(ctx context.Context, r *ModerationResult) error
	// FindOne 按 task_id 查询结论。
	FindOne(ctx context.Context, taskID int64) (*ModerationResult, error)
}

type defaultModerationResultModel struct {
	conn sqlx.SqlConn
}

// NewModerationResultModel 创建 ModerationResultModel 实现。
func NewModerationResultModel(conn sqlx.SqlConn) ModerationResultModel {
	return &defaultModerationResultModel{conn: conn}
}

func (m *defaultModerationResultModel) Upsert(ctx context.Context, r *ModerationResult) error {
	r.Ctime = nowUnix()
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO moderation_result (task_id, verdict, reason, worker_id, reviewer, ctime) VALUES (?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE verdict = VALUES(verdict), reason = VALUES(reason), worker_id = VALUES(worker_id), reviewer = VALUES(reviewer), ctime = VALUES(ctime)",
		r.TaskID, r.Verdict, r.Reason, r.WorkerID, r.Reviewer, r.Ctime)
	if err != nil {
		return fmt.Errorf("moderation_result Upsert: %w", err)
	}
	return nil
}

func (m *defaultModerationResultModel) FindOne(ctx context.Context, taskID int64) (*ModerationResult, error) {
	var r ModerationResult
	if err := m.conn.QueryRowCtx(ctx, &r,
		"SELECT id, task_id, verdict, reason, worker_id, reviewer, ctime FROM moderation_result WHERE task_id = ?",
		taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("moderation_result FindOne: %w", err)
	}
	return &r, nil
}

// ModerationAppeal 申诉。
type ModerationAppeal struct {
	ID           int64  `db:"id"`            // 主键 ID
	TaskID       int64  `db:"task_id"`       // 关联任务 ID
	Mid          int64  `db:"mid"`           // 申诉人 ID
	Content      string `db:"content"`       // 申诉理由
	FinalVerdict int32  `db:"final_verdict"` // 最终结论（处理后）
	FinalReason  string `db:"final_reason"`  // 处理说明
	Handler      int64  `db:"handler"`       // 处理人（运营 ID）
	State        int32  `db:"state"`         // 0 待处理、1 已处理
	Ctime        int64  `db:"ctime"`         // 创建时间（Unix 秒）
	Mtime        int64  `db:"mtime"`         // 处理时间（Unix 秒）
}

// ModerationAppealModel moderation_appeal 表查询与写入接口。
type ModerationAppealModel interface {
	// Insert 创建申诉；返回新 appeal_id。
	Insert(ctx context.Context, a *ModerationAppeal) (int64, error)
	// FindOne 按主键查询申诉。
	FindOne(ctx context.Context, appealID int64) (*ModerationAppeal, error)
	// Update 处理申诉；state=1（已处理）的申诉不能再次处理。
	Update(ctx context.Context, appealID, handler int64, finalVerdict int32, finalReason string) error
}

type defaultModerationAppealModel struct {
	conn sqlx.SqlConn
}

// NewModerationAppealModel 创建 ModerationAppealModel 实现。
func NewModerationAppealModel(conn sqlx.SqlConn) ModerationAppealModel {
	return &defaultModerationAppealModel{conn: conn}
}

func (m *defaultModerationAppealModel) Insert(ctx context.Context, a *ModerationAppeal) (int64, error) {
	now := nowUnix()
	a.Ctime = now
	a.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO moderation_appeal (task_id, mid, content, final_verdict, final_reason, handler, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.TaskID, a.Mid, a.Content, a.FinalVerdict, a.FinalReason, a.Handler, a.State, a.Ctime, a.Mtime)
	if err != nil {
		return 0, fmt.Errorf("moderation_appeal Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("moderation_appeal Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultModerationAppealModel) FindOne(ctx context.Context, appealID int64) (*ModerationAppeal, error) {
	var a ModerationAppeal
	if err := m.conn.QueryRowCtx(ctx, &a,
		"SELECT id, task_id, mid, content, final_verdict, final_reason, handler, state, ctime, mtime FROM moderation_appeal WHERE id = ?",
		appealID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("moderation_appeal FindOne: %w", err)
	}
	return &a, nil
}

func (m *defaultModerationAppealModel) Update(ctx context.Context, appealID, handler int64, finalVerdict int32, finalReason string) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE moderation_appeal SET final_verdict = ?, final_reason = ?, handler = ?, state = 1, mtime = ? WHERE id = ? AND state = 0",
		finalVerdict, finalReason, handler, nowUnix(), appealID)
	if err != nil {
		return fmt.Errorf("moderation_appeal Update: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("moderation_appeal Update RowsAffected: %w", err)
	}
	if aff == 0 {
		// 申诉不存在或已处理
		return ErrAppealAlreadyHandled
	}
	return nil
}
