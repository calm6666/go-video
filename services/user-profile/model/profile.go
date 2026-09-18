package model

import (
	"context"
	"database/sql"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UserBase 对应数据库 user_base 表，记录用户可变展示资料。
// 移植自参考仓库 user_base_%02d（按 mid%100 分 100 张表）。
// 新项目起步阶段数据量有限，采用单表设计；如需分表，按 mid%100 命名
// user_base_00..99 并在 model 层传入表名即可，SQL 与字段定义不变。
type UserBase struct {
	// Mid 用户 ID，主键（由 account/idgen 分配）
	Mid int64 `db:"mid"`
	// Name 昵称
	Name string `db:"name"`
	// Sex 性别：0 保密、1 男、2 女
	Sex int64 `db:"sex"`
	// Face 头像 URL（空表示未设置，读取时替换为默认头像）
	Face string `db:"face"`
	// Sign 个人签名
	Sign string `db:"sign"`
	// Rank 排名（默认 5000）
	Rank int64 `db:"rank"`
	// Birthday 生日（Unix 秒，-28800 表示未设置）
	Birthday int64 `db:"birthday"`
}

// UserBaseModel 抽象 user_base 表的查询接口，便于测试替换。
type UserBaseModel interface {
	// FindOne 查询单个用户基础资料；不存在返回 nil。
	FindOne(ctx context.Context, mid int64) (*UserBase, error)
	// FindMany 批量查询；缺失的 mid 不在结果中。
	FindMany(ctx context.Context, mids []int64) (map[int64]*UserBase, error)
	// SetBase 整体写入基础资料（UPSERT，用于 /base/update 与新用户初始化）。
	SetBase(ctx context.Context, base *UserBase) error
	// SetSex 更新性别。
	SetSex(ctx context.Context, mid, sex int64) error
	// SetName 更新昵称。
	SetName(ctx context.Context, mid int64, name string) error
	// SetRank 更新排名。
	SetRank(ctx context.Context, mid, rank int64) error
	// SetSign 更新签名。
	SetSign(ctx context.Context, mid int64, sign string) error
	// SetBirthday 更新生日。
	SetBirthday(ctx context.Context, mid, birthday int64) error
	// SetFace 更新头像。
	SetFace(ctx context.Context, mid int64, face string) error
}

type defaultUserBaseModel struct {
	conn sqlx.SqlConn
}

// NewUserBaseModel 创建基于 sqlx 的 UserBaseModel 实现。
func NewUserBaseModel(conn sqlx.SqlConn) UserBaseModel {
	return &defaultUserBaseModel{conn: conn}
}

func (m *defaultUserBaseModel) FindOne(ctx context.Context, mid int64) (*UserBase, error) {
	var b UserBase
	query := `SELECT mid, name, sex, face, sign, rank, birthday FROM user_base WHERE mid = ?`
	if err := m.conn.QueryRowCtx(ctx, &b, query, mid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

func (m *defaultUserBaseModel) FindMany(ctx context.Context, mids []int64) (map[int64]*UserBase, error) {
	if len(mids) == 0 {
		return map[int64]*UserBase{}, nil
	}
	query := `SELECT mid, name, sex, face, sign, rank, birthday FROM user_base WHERE mid IN (?)`
	var rows []*UserBase
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mids); err != nil {
		return nil, err
	}
	result := make(map[int64]*UserBase, len(rows))
	for _, r := range rows {
		result[r.Mid] = r
	}
	return result, nil
}

func (m *defaultUserBaseModel) SetBase(ctx context.Context, base *UserBase) error {
	query := `INSERT INTO user_base (mid, name, sex, face, sign, rank, birthday) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE name = VALUES(name), sex = VALUES(sex), face = VALUES(face),
		sign = VALUES(sign), rank = VALUES(rank), birthday = VALUES(birthday)`
	_, err := m.conn.ExecCtx(ctx, query,
		base.Mid, base.Name, base.Sex, base.Face, base.Sign, base.Rank, base.Birthday)
	return err
}

func (m *defaultUserBaseModel) SetSex(ctx context.Context, mid, sex int64) error {
	query := `INSERT INTO user_base (mid, sex) VALUES (?, ?) ON DUPLICATE KEY UPDATE sex = ?`
	_, err := m.conn.ExecCtx(ctx, query, mid, sex, sex)
	return err
}

func (m *defaultUserBaseModel) SetName(ctx context.Context, mid int64, name string) error {
	query := `INSERT INTO user_base (mid, name) VALUES (?, ?) ON DUPLICATE KEY UPDATE name = ?`
	_, err := m.conn.ExecCtx(ctx, query, mid, name, name)
	return err
}

func (m *defaultUserBaseModel) SetRank(ctx context.Context, mid, rank int64) error {
	query := `INSERT INTO user_base (mid, rank) VALUES (?, ?) ON DUPLICATE KEY UPDATE rank = ?`
	_, err := m.conn.ExecCtx(ctx, query, mid, rank, rank)
	return err
}

func (m *defaultUserBaseModel) SetSign(ctx context.Context, mid int64, sign string) error {
	query := `INSERT INTO user_base (mid, sign) VALUES (?, ?) ON DUPLICATE KEY UPDATE sign = ?`
	_, err := m.conn.ExecCtx(ctx, query, mid, sign, sign)
	return err
}

func (m *defaultUserBaseModel) SetBirthday(ctx context.Context, mid, birthday int64) error {
	query := `INSERT INTO user_base (mid, birthday) VALUES (?, ?) ON DUPLICATE KEY UPDATE birthday = ?`
	_, err := m.conn.ExecCtx(ctx, query, mid, birthday, birthday)
	return err
}

func (m *defaultUserBaseModel) SetFace(ctx context.Context, mid int64, face string) error {
	query := `INSERT INTO user_base (mid, face) VALUES (?, ?) ON DUPLICATE KEY UPDATE face = ?`
	_, err := m.conn.ExecCtx(ctx, query, mid, face, face)
	return err
}

// UserExp 对应数据库 user_exp 表，记录用户经验值（分×100）。
// 移植自参考仓库 user_exp_%02d 分表，本服务采用单表（理由同 user_base）。
type UserExp struct {
	// Mid 用户 ID，主键
	Mid int64 `db:"mid"`
	// Exp 经验值（入库值 = 分 × 100）
	Exp int64 `db:"exp"`
}

// UserExpModel 抽象 user_exp 表的查询接口。
type UserExpModel interface {
	// FindOne 查询经验值；不存在返回 0。
	FindOne(ctx context.Context, mid int64) (int64, error)
	// FindMany 批量查询。
	FindMany(ctx context.Context, mids []int64) (map[int64]int64, error)
	// Set 设置经验值为指定值（仅运营 SetExp 使用）。
	Set(ctx context.Context, mid, exp int64) (int64, error)
	// Incr 经验值增加 delta（可负，但数据库侧 exp 为 UNSIGNED，负增量需调用方保证不溢出）。
	Incr(ctx context.Context, mid, delta int64) (int64, error)
}

type defaultUserExpModel struct {
	conn sqlx.SqlConn
}

// NewUserExpModel 创建基于 sqlx 的 UserExpModel 实现。
func NewUserExpModel(conn sqlx.SqlConn) UserExpModel {
	return &defaultUserExpModel{conn: conn}
}

func (m *defaultUserExpModel) FindOne(ctx context.Context, mid int64) (int64, error) {
	var exp int64
	query := `SELECT exp FROM user_exp WHERE mid = ?`
	if err := m.conn.QueryRowCtx(ctx, &exp, query, mid); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return exp, nil
}

func (m *defaultUserExpModel) FindMany(ctx context.Context, mids []int64) (map[int64]int64, error) {
	if len(mids) == 0 {
		return map[int64]int64{}, nil
	}
	query := `SELECT mid, exp FROM user_exp WHERE mid IN (?)`
	var rows []*UserExp
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mids); err != nil {
		return nil, err
	}
	result := make(map[int64]int64, len(rows))
	for _, r := range rows {
		result[r.Mid] = r.Exp
	}
	return result, nil
}

func (m *defaultUserExpModel) Set(ctx context.Context, mid, exp int64) (int64, error) {
	query := `INSERT INTO user_exp (mid, exp) VALUES (?, ?) ON DUPLICATE KEY UPDATE exp = VALUES(exp)`
	res, err := m.conn.ExecCtx(ctx, query, mid, exp)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (m *defaultUserExpModel) Incr(ctx context.Context, mid, delta int64) (int64, error) {
	query := `UPDATE user_exp SET exp = exp + ? WHERE mid = ?`
	res, err := m.conn.ExecCtx(ctx, query, delta, mid)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UserFlag 对应数据库 user_flag 表，记录用户标志位（首次改昵称等）。
type UserFlag struct {
	// Mid 用户 ID，主键
	Mid int64 `db:"mid"`
	// Flag 标志位集合（按位或：1 = 已首次修改昵称）
	Flag int64 `db:"flag"`
}

// UserFlagModel 抽象 user_flag 表的查询接口。
type UserFlagModel interface {
	// HasAttr 判断 mid 的 attr 位是否已置位。
	HasAttr(ctx context.Context, mid int64, attr uint) (bool, error)
	// SetAttr 置位。
	SetAttr(ctx context.Context, mid int64, attr uint) error
}

type defaultUserFlagModel struct {
	conn sqlx.SqlConn
}

// NewUserFlagModel 创建基于 sqlx 的 UserFlagModel 实现。
func NewUserFlagModel(conn sqlx.SqlConn) UserFlagModel {
	return &defaultUserFlagModel{conn: conn}
}

func (m *defaultUserFlagModel) HasAttr(ctx context.Context, mid int64, attr uint) (bool, error) {
	var flag int64
	query := `SELECT flag FROM user_flag WHERE mid = ? AND flag & ? = ?`
	if err := m.conn.QueryRowCtx(ctx, &flag, query, mid, attr, attr); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return flag != 0, nil
}

func (m *defaultUserFlagModel) SetAttr(ctx context.Context, mid int64, attr uint) error {
	query := `INSERT INTO user_flag (mid, flag) VALUES (?, ?) ON DUPLICATE KEY UPDATE flag = flag | ?`
	_, err := m.conn.ExecCtx(ctx, query, mid, attr, attr)
	return err
}
