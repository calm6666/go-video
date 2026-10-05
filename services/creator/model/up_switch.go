package model

import (
	"context"
	"database/sql"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UpSwitch UP 主关注弹窗开关（参考仓库 up_switch 表）。
// from：0 播放器关注开关、1 UP 主荣誉周报退订。
// state：0 关闭、1 打开。
type UpSwitch struct {
	Mid   int64 `db:"mid"`   // 用户 ID
	From  int32 `db:"from"`  // 业务来源
	State int32 `db:"state"` // 开关状态
}

// UpSwitchModel up_switch 表查询与更新接口。
type UpSwitchModel interface {
	// FindOne 查询开关状态；不存在返回 nil（调用方按默认关闭处理）。
	FindOne(ctx context.Context, mid int64, from int32) (*UpSwitch, error)
	// Upsert 新增或更新开关状态（INSERT ... ON DUPLICATE KEY UPDATE）。
	// mid 与 FindOne 同为 int64：表列是 BIGINT UNSIGNED
	// （deploy/migrations/creator/000004_create_up_switch.sql:16）。
	// 原签名是 int32，会把 mid > 2^31-1 的号截断成另一个号，写出一条不属于请求方的开关行。
	Upsert(ctx context.Context, mid int64, from, state int32) error
}

type defaultUpSwitchModel struct {
	conn sqlx.SqlConn
}

// NewUpSwitchModel 创建 UpSwitchModel 实现。
func NewUpSwitchModel(conn sqlx.SqlConn) UpSwitchModel {
	return &defaultUpSwitchModel{conn: conn}
}

func (m *defaultUpSwitchModel) FindOne(ctx context.Context, mid int64, from int32) (*UpSwitch, error) {
	var s UpSwitch
	query := "SELECT mid, `from`, state FROM up_switch WHERE mid = ? AND `from` = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, mid, from); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (m *defaultUpSwitchModel) Upsert(ctx context.Context, mid int64, from, state int32) error {
	query := "INSERT INTO up_switch (mid, `from`, state) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE state = VALUES(state)"
	_, err := m.conn.ExecCtx(ctx, query, mid, from, state)
	return err
}
