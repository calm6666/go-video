package model

import (
	"context"
	"database/sql"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UpGroup 特殊用户组（参考仓库 up_group 表）。
// 用于给创作者打分类标签，如"高能联盟"、"知名 UP"等，前端展示为色块徽章。
type UpGroup struct {
	ID        int64  `db:"id"`         // 分组 ID
	Name      string `db:"name"`       // 分组名
	Tag       string `db:"tag"`        // 标签名
	ShortTag  string `db:"short_tag"`  // 简称
	FontColor string `db:"font_color"` // 字体色
	BgColor   string `db:"bg_color"`   // 背景色
	Note      string `db:"note"`       // 备注
}

// UpGroupModel up_group 表查询接口。
type UpGroupModel interface {
	// All 返回所有特殊用户组，按 ID 索引。
	All(ctx context.Context) (map[int64]*UpGroup, error)
}

type defaultUpGroupModel struct {
	conn sqlx.SqlConn
}

// NewUpGroupModel 创建 UpGroupModel 实现。
func NewUpGroupModel(conn sqlx.SqlConn) UpGroupModel {
	return &defaultUpGroupModel{conn: conn}
}

func (m *defaultUpGroupModel) All(ctx context.Context) (map[int64]*UpGroup, error) {
	var rows []*UpGroup
	query := `SELECT id, name, tag, short_tag, font_color, bg_color, note FROM up_group`
	if err := m.conn.QueryRowsCtx(ctx, &rows, query); err != nil {
		if err == sql.ErrNoRows {
			return map[int64]*UpGroup{}, nil
		}
		return nil, err
	}
	out := make(map[int64]*UpGroup, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}
