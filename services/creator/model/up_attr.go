package model

import (
	"context"
	"database/sql"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UpAttr 表示 UP 主身份属性（参考仓库 up_base 表子集）。
// from 字段区分身份来源：0 稿件作者、1 移动投稿作者、2 直播 UP、3 直播白名单。
type UpAttr struct {
	Mid      int64 `db:"mid"`       // 用户 ID
	IsAuthor int32 `db:"is_author"` // 是否有身份：0 否、1 是
	From     int32 `db:"from"`      // 来源
}

// UpAttrModel up_attr 表查询接口。
type UpAttrModel interface {
	// FindOne 查询指定来源下某 mid 是否有 UP 身份；不存在返回 nil。
	FindOne(ctx context.Context, mid int64, from int32) (*UpAttr, error)
}

type defaultUpAttrModel struct {
	conn sqlx.SqlConn
}

// NewUpAttrModel 创建 UpAttrModel 实现。
func NewUpAttrModel(conn sqlx.SqlConn) UpAttrModel {
	return &defaultUpAttrModel{conn: conn}
}

func (m *defaultUpAttrModel) FindOne(ctx context.Context, mid int64, from int32) (*UpAttr, error) {
	var a UpAttr
	query := "SELECT mid, is_author, `from` FROM up_attr WHERE mid = ? AND `from` = ?"
	if err := m.conn.QueryRowCtx(ctx, &a, query, mid, from); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}
