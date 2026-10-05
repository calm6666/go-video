package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 模板状态：与 rpc/notification.proto 的 TemplateState 一致。
const (
	// TemplateStateDraft 草稿，不可用于投递。
	TemplateStateDraft int32 = 1
	// TemplateStatePublished 已发布，可投递。
	TemplateStatePublished int32 = 2
	// TemplateStateOffline 已下线，只读留档。
	TemplateStateOffline int32 = 3
)

// NotificationTemplate 通知模板（notification_template 表）。
// 同一 (template_code, channel, lang) 可有多个 version，但最多一个 version 处于已发布态。
type NotificationTemplate struct {
	Id           int64  `db:"id"`            // 自增主键
	TemplateCode string `db:"template_code"` // 模板业务码
	Channel      int32  `db:"channel"`       // 通道，见 Channel* 常量
	Lang         string `db:"lang"`          // 语言，见 Lang* 常量
	TitleTpl     string `db:"title_tpl"`     // 标题模板
	BodyTpl      string `db:"body_tpl"`      // 正文模板
	Version      int32  `db:"version"`       // 版本号
	State        int32  `db:"state"`         // 状态，见 TemplateState* 常量
	Operator     string `db:"operator"`      // 最后操作人（运营账号）
	Ctime        int64  `db:"ctime"`         // 创建时间（Unix 秒）
	Mtime        int64  `db:"mtime"`         // 修改时间（Unix 秒）
}

// TemplateFilter 模板列表过滤条件，零值字段表示不过滤。
type TemplateFilter struct {
	TemplateCode string
	Channel      int32
	Lang         string
	State        int32
}

// NotificationTemplateModel notification_template 表读写接口。
type NotificationTemplateModel interface {
	// Insert 新建模板版本，返回自增主键。
	Insert(ctx context.Context, t *NotificationTemplate) (int64, error)
	// Find 按 (code, channel, lang, version) 精确查询；version<=0 时忽略版本条件。
	// 不存在返回 (nil, nil)。
	Find(ctx context.Context, code string, channel int32, lang string, version int32) (*NotificationTemplate, error)
	// FindByState 查询指定状态的模板；不存在返回 (nil, nil)。
	FindByState(ctx context.Context, code string, channel int32, lang string, state int32) (*NotificationTemplate, error)
	// FindByID 按主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, id int64) (*NotificationTemplate, error)
	// MaxVersion 返回 (code, channel, lang) 下的最大版本号，无记录返回 0。
	MaxVersion(ctx context.Context, code string, channel int32, lang string) (int32, error)
	// UpdateDraftContent 覆盖草稿内容（仅 state=草稿 生效）。
	UpdateDraftContent(ctx context.Context, id int64, title, body, operator string) error
	// PublishDraft 单事务内把 id 置为已发布，并把同 (code, channel, lang) 其它已发布版本下线。
	// id 不是草稿时返回 ErrTemplateNotFound 或 ErrIllegalStateTransition。
	PublishDraft(ctx context.Context, id int64, operator string) (*NotificationTemplate, error)
	// List 分页查询模板，按 (template_code, channel, lang, version) 排序。
	List(ctx context.Context, f TemplateFilter, pn, ps int32) ([]*NotificationTemplate, int64, error)
}

type defaultNotificationTemplateModel struct {
	conn sqlx.SqlConn
}

// NewNotificationTemplateModel 创建 NotificationTemplateModel 实现。
func NewNotificationTemplateModel(conn sqlx.SqlConn) NotificationTemplateModel {
	return &defaultNotificationTemplateModel{conn: conn}
}

const templateCols = "id, template_code, channel, lang, title_tpl, body_tpl, version, state, operator, ctime, mtime"

func (m *defaultNotificationTemplateModel) Insert(ctx context.Context, t *NotificationTemplate) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO notification_template (template_code, channel, lang, title_tpl, body_tpl, version, state, operator, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.TemplateCode, t.Channel, t.Lang, t.TitleTpl, t.BodyTpl, t.Version, t.State, t.Operator, t.Ctime, t.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			return 0, ErrDuplicateBizKey
		}
		return 0, fmt.Errorf("notification_template Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("notification_template Insert LastInsertId: %w", err)
	}
	t.Id = id
	return id, nil
}

func (m *defaultNotificationTemplateModel) queryOne(ctx context.Context, query string, args ...any) (*NotificationTemplate, error) {
	var t NotificationTemplate
	if err := m.conn.QueryRowCtx(ctx, &t, query, args...); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_template query: %w", err)
	}
	return &t, nil
}

func (m *defaultNotificationTemplateModel) Find(ctx context.Context, code string, channel int32, lang string, version int32) (*NotificationTemplate, error) {
	if version > 0 {
		return m.queryOne(ctx,
			"SELECT "+templateCols+" FROM notification_template WHERE template_code = ? AND channel = ? AND lang = ? AND version = ? LIMIT 1",
			code, channel, lang, version)
	}
	return m.queryOne(ctx,
		"SELECT "+templateCols+" FROM notification_template WHERE template_code = ? AND channel = ? AND lang = ? ORDER BY version DESC LIMIT 1",
		code, channel, lang)
}

func (m *defaultNotificationTemplateModel) FindByID(ctx context.Context, id int64) (*NotificationTemplate, error) {
	return m.queryOne(ctx, "SELECT "+templateCols+" FROM notification_template WHERE id = ? LIMIT 1", id)
}

func (m *defaultNotificationTemplateModel) FindByState(ctx context.Context, code string, channel int32, lang string, state int32) (*NotificationTemplate, error) {
	return m.queryOne(ctx,
		"SELECT "+templateCols+" FROM notification_template WHERE template_code = ? AND channel = ? AND lang = ? AND state = ? ORDER BY version DESC LIMIT 1",
		code, channel, lang, state)
}

func (m *defaultNotificationTemplateModel) MaxVersion(ctx context.Context, code string, channel int32, lang string) (int32, error) {
	var version int32
	err := m.conn.QueryRowCtx(ctx, &version,
		"SELECT COALESCE(MAX(version), 0) FROM notification_template WHERE template_code = ? AND channel = ? AND lang = ?",
		code, channel, lang)
	if err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("notification_template MaxVersion: %w", err)
	}
	return version, nil
}

func (m *defaultNotificationTemplateModel) UpdateDraftContent(ctx context.Context, id int64, title, body, operator string) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE notification_template SET title_tpl = ?, body_tpl = ?, operator = ?, mtime = ? WHERE id = ? AND state = ?",
		title, body, operator, nowUnix(), id, TemplateStateDraft)
	if err != nil {
		return fmt.Errorf("notification_template UpdateDraftContent: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("notification_template UpdateDraftContent RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrIllegalStateTransition
	}
	return nil
}

func (m *defaultNotificationTemplateModel) PublishDraft(ctx context.Context, id int64, operator string) (*NotificationTemplate, error) {
	t, err := m.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrTemplateNotFound
	}
	if t.State != TemplateStateDraft {
		return nil, ErrIllegalStateTransition
	}
	err = m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		// 先把同键已发布版本下线，保证“每个 (code, channel, lang) 至多一个已发布版本”。
		if _, err := session.ExecCtx(ctx,
			"UPDATE notification_template SET state = ?, mtime = ? WHERE template_code = ? AND channel = ? AND lang = ? AND state = ?",
			TemplateStateOffline, nowUnix(), t.TemplateCode, t.Channel, t.Lang, TemplateStatePublished); err != nil {
			return fmt.Errorf("notification_template offline previous: %w", err)
		}
		if _, err := session.ExecCtx(ctx,
			"UPDATE notification_template SET state = ?, operator = ?, mtime = ? WHERE id = ? AND state = ?",
			TemplateStatePublished, operator, nowUnix(), id, TemplateStateDraft); err != nil {
			return fmt.Errorf("notification_template publish: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	t.State = TemplateStatePublished
	t.Operator = operator
	return t, nil
}

func (m *defaultNotificationTemplateModel) List(ctx context.Context, f TemplateFilter, pn, ps int32) ([]*NotificationTemplate, int64, error) {
	if ps > 100 {
		return nil, 0, ErrPsTooLarge
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	where := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if f.TemplateCode != "" {
		where = append(where, "template_code = ?")
		args = append(args, f.TemplateCode)
	}
	if f.Channel != 0 {
		where = append(where, "channel = ?")
		args = append(args, f.Channel)
	}
	if f.Lang != "" {
		where = append(where, "lang = ?")
		args = append(args, f.Lang)
	}
	if f.State != 0 {
		where = append(where, "state = ?")
		args = append(args, f.State)
	}
	cond := joinAnd(where)

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM notification_template WHERE "+cond, args...); err != nil {
		return nil, 0, fmt.Errorf("notification_template List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*NotificationTemplate
	q := "SELECT " + templateCols + " FROM notification_template WHERE " + cond +
		" ORDER BY template_code ASC, channel ASC, lang ASC, version DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, q, listArgs...); err != nil {
		if isNoRows(err) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("notification_template List: %w", err)
	}
	return rows, total, nil
}

// joinAnd 拼接 WHERE 片段。
func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}
