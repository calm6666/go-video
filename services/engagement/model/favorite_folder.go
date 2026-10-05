package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// FavoriteFolder 用户收藏夹。
type FavoriteFolder struct {
	Fid         int64  `db:"fid"`         // 收藏夹 ID
	Mid         int64  `db:"mid"`         // 用户 ID
	Name        string `db:"name"`        // 收藏夹名
	Description string `db:"description"` // 描述
	Cover       string `db:"cover"`       // 封面 URL
	Public      int32  `db:"public"`      // 0 私密、1 公开
	State       int32  `db:"state"`       // 0 正常、1 删除
	Ctime       int64  `db:"ctime"`       // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`       // 修改时间（Unix 秒）
	Count       int32  `db:"count"`       // 收藏数量快照
}

// FavoriteFolderModel favorite_folder 表查询与写入接口。
type FavoriteFolderModel interface {
	// Add 新建收藏夹；返回新 fid。
	Add(ctx context.Context, f *FavoriteFolder) (int64, error)
	// Del 软删除收藏夹（state=1）；校验 mid 本人。
	Del(ctx context.Context, fid, mid int64) error
	// ListByUser 查询用户的收藏夹列表（含默认夹）。
	// vmid != mid 时只返回 public=1 的。
	ListByUser(ctx context.Context, mid, vmid int64) ([]*FavoriteFolder, error)
	// FindOne 查询单个收藏夹。
	FindOne(ctx context.Context, fid int64) (*FavoriteFolder, error)
}

type defaultFavoriteFolderModel struct {
	conn sqlx.SqlConn
}

// NewFavoriteFolderModel 创建 FavoriteFolderModel 实现。
func NewFavoriteFolderModel(conn sqlx.SqlConn) FavoriteFolderModel {
	return &defaultFavoriteFolderModel{conn: conn}
}

func (m *defaultFavoriteFolderModel) Add(ctx context.Context, f *FavoriteFolder) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO favorite_folder (mid, name, description, cover, public, state, ctime, mtime, count) VALUES (?, ?, ?, ?, ?, 0, ?, ?, 0)",
		f.Mid, f.Name, f.Description, f.Cover, f.Public, f.Ctime, f.Mtime)
	if err != nil {
		return 0, fmt.Errorf("favorite_folder Add: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("favorite_folder Add LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultFavoriteFolderModel) Del(ctx context.Context, fid, mid int64) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE favorite_folder SET state = 1, mtime = ? WHERE fid = ? AND mid = ?",
		nowUnix(), fid, mid)
	if err != nil {
		return fmt.Errorf("favorite_folder Del: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("favorite_folder Del RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrFolderNotFoundOrForbidden
	}
	return nil
}

func (m *defaultFavoriteFolderModel) ListByUser(ctx context.Context, mid, vmid int64) ([]*FavoriteFolder, error) {
	query := "SELECT fid, mid, name, description, cover, public, state, ctime, mtime, count FROM favorite_folder WHERE mid = ? AND state = 0 ORDER BY fid ASC"
	args := []interface{}{vmid}
	if mid != vmid {
		// 查看他人收藏夹：只返回公开夹
		query = "SELECT fid, mid, name, description, cover, public, state, ctime, mtime, count FROM favorite_folder WHERE mid = ? AND state = 0 AND public = 1 ORDER BY fid ASC"
	}
	var rows []*FavoriteFolder
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("favorite_folder ListByUser: %w", err)
	}
	return rows, nil
}

func (m *defaultFavoriteFolderModel) FindOne(ctx context.Context, fid int64) (*FavoriteFolder, error) {
	var f FavoriteFolder
	query := "SELECT fid, mid, name, description, cover, public, state, ctime, mtime, count FROM favorite_folder WHERE fid = ?"
	if err := m.conn.QueryRowCtx(ctx, &f, query, fid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("favorite_folder FindOne: %w", err)
	}
	return &f, nil
}
