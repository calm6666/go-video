package model

import (
	"context"
	"database/sql"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RealnameInfo 对应数据库 realname_info 表，记录用户实名认证信息。
// 移植自参考仓库 realname_info 表。证件号（Card）以 RSA 密文入库，
// 明文只存在缓存中，出库需用私钥解密。
type RealnameInfo struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// Mid 用户 ID（唯一）
	Mid int64 `db:"mid"`
	// Channel 实名渠道：0 主站、1 支付宝
	Channel int8 `db:"channel"`
	// Realname 真实姓名
	Realname string `db:"realname"`
	// Country 国家：0 中国
	Country int16 `db:"country"`
	// CardType 证件类型：0 身份证
	CardType int8 `db:"card_type"`
	// Card 证件号（RSA 加密后的 base64）
	Card string `db:"card"`
	// CardMD5 证件号哈希（salt+小写证件号+类型+国家 的 MD5），用于查重与反查
	CardMD5 string `db:"card_md5"`
	// Status 流程状态：0 审核中、1 通过、2 驳回、3 未申请
	Status int8 `db:"status"`
	// Reason 驳回原因
	Reason string `db:"reason"`
	// CTime 创建时间（Unix 秒）
	CTime int64 `db:"ctime"`
	// MTime 最近更新时间（Unix 秒）
	MTime int64 `db:"mtime"`
}

// IsPass 判断实名是否通过。
func (r *RealnameInfo) IsPass() bool {
	return r != nil && r.Status == RealnameApplyStatusPass
}

// RealnameInfoModel 抽象 realname_info 表的查询接口。
type RealnameInfoModel interface {
	// FindOne 按 mid 查询；不存在返回 nil。
	FindOne(ctx context.Context, mid int64) (*RealnameInfo, error)
	// FindOneByCardMD5 按证件哈希查询（查重）；不存在返回 nil。
	FindOneByCardMD5(ctx context.Context, cardMD5 string) (*RealnameInfo, error)
	// FindMidsByCardMD5s 批量按证件哈希反查 mid。
	FindMidsByCardMD5s(ctx context.Context, cardMD5s []string) (map[string]int64, error)
	// Upsert 写入/更新实名信息（事务内使用）。
	Upsert(ctx context.Context, tx sqlx.Session, info *RealnameInfo) error
	// UpdateStatus 更新流程状态与原因（事务内使用）。
	UpdateStatus(ctx context.Context, tx sqlx.Session, mid int64, status int8, reason string) error
}

type defaultRealnameInfoModel struct {
	conn sqlx.SqlConn
}

// NewRealnameInfoModel 创建基于 sqlx 的 RealnameInfoModel 实现。
func NewRealnameInfoModel(conn sqlx.SqlConn) RealnameInfoModel {
	return &defaultRealnameInfoModel{conn: conn}
}

func scanRealnameInfo(scanner interface {
	Scan(dest ...any) error
}) (*RealnameInfo, error) {
	info := &RealnameInfo{}
	if err := scanner.Scan(&info.ID, &info.Mid, &info.Channel, &info.Realname, &info.Country,
		&info.CardType, &info.Card, &info.CardMD5, &info.Status, &info.Reason, &info.CTime, &info.MTime); err != nil {
		return nil, err
	}
	return info, nil
}

func (m *defaultRealnameInfoModel) FindOne(ctx context.Context, mid int64) (*RealnameInfo, error) {
	query := `SELECT id, mid, channel, realname, country, card_type, card, card_md5, status, reason, ctime, mtime
		FROM realname_info WHERE mid = ? LIMIT 1`
	info, err := scanRealnameInfo(queryRowScanner{conn: m.conn, ctx: ctx, query: query, args: []any{mid}})
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return info, err
}

func (m *defaultRealnameInfoModel) FindOneByCardMD5(ctx context.Context, cardMD5 string) (*RealnameInfo, error) {
	query := `SELECT id, mid, channel, realname, country, card_type, card, card_md5, status, reason, ctime, mtime
		FROM realname_info WHERE card_md5 = ? AND status IN (0, 1) LIMIT 1`
	info, err := scanRealnameInfo(queryRowScanner{conn: m.conn, ctx: ctx, query: query, args: []any{cardMD5}})
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return info, err
}

func (m *defaultRealnameInfoModel) FindMidsByCardMD5s(ctx context.Context, cardMD5s []string) (map[string]int64, error) {
	if len(cardMD5s) == 0 {
		return map[string]int64{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(cardMD5s)), ",")
	query := `SELECT mid, card_md5 FROM realname_info WHERE card_md5 IN (` + placeholders + `) AND status IN (0, 1)`
	args := make([]any, 0, len(cardMD5s))
	for _, md5 := range cardMD5s {
		args = append(args, md5)
	}
	var rows []*struct {
		Mid     int64  `db:"mid"`
		CardMD5 string `db:"card_md5"`
	}
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		return nil, err
	}
	result := make(map[string]int64, len(rows))
	for _, r := range rows {
		result[r.CardMD5] = r.Mid
	}
	return result, nil
}

func (m *defaultRealnameInfoModel) Upsert(ctx context.Context, tx sqlx.Session, info *RealnameInfo) error {
	query := `INSERT INTO realname_info (mid, channel, realname, country, card_type, card, card_md5, status, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE channel = ?, realname = ?, country = ?, card_type = ?, card = ?, card_md5 = ?, status = ?, reason = ?`
	_, err := tx.ExecCtx(ctx, query, info.Mid, info.Channel, info.Realname, info.Country, info.CardType,
		info.Card, info.CardMD5, info.Status, info.Reason,
		info.Channel, info.Realname, info.Country, info.CardType, info.Card, info.CardMD5, info.Status, info.Reason)
	return err
}

func (m *defaultRealnameInfoModel) UpdateStatus(ctx context.Context, tx sqlx.Session, mid int64, status int8, reason string) error {
	query := `UPDATE realname_info SET status = ?, reason = ? WHERE mid = ?`
	_, err := tx.ExecCtx(ctx, query, status, reason, mid)
	return err
}

// queryRowScanner 把 sqlx.SqlConn/Session 的 QueryRowCtx 适配为 Scan 接口。
type queryRowScanner struct {
	conn interface {
		QueryRowCtx(ctx context.Context, v any, query string, args ...any) error
	}
	ctx   context.Context
	query string
	args  []any
}

func (s queryRowScanner) Scan(dest ...any) error {
	// 仅支持单目标扫描（本包全部为单结构体扫描）
	return s.conn.QueryRowCtx(s.ctx, dest[0], s.query, s.args...)
}

// RealnameApply 对应数据库 realname_apply 表，记录实名认证申请单。
// 移植自参考仓库 realname_apply 表（证件号密文入库）。
type RealnameApply struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// Mid 用户 ID
	Mid int64 `db:"mid"`
	// Realname 真实姓名
	Realname string `db:"realname"`
	// Country 国家
	Country int16 `db:"country"`
	// CardType 证件类型
	CardType int8 `db:"card_type"`
	// CardNum 证件号（RSA 加密后的 base64）
	CardNum string `db:"card_num"`
	// CardMD5 证件号哈希
	CardMD5 string `db:"card_md5"`
	// HandIMG 手持证件照图片 ID
	HandIMG int64 `db:"hand_img"`
	// FrontIMG 证件正面照图片 ID
	FrontIMG int64 `db:"front_img"`
	// BackIMG 证件背面照图片 ID
	BackIMG int64 `db:"back_img"`
	// Status 流程状态：0 审核中、1 通过、2 驳回、3 未申请
	Status int8 `db:"status"`
	// Operator 审核操作人
	Operator string `db:"operator"`
	// OperatorID 审核操作人 ID
	OperatorID int64 `db:"operator_id"`
	// OperatorTime 审核时间（Unix 秒）
	OperatorTime int64 `db:"operator_time"`
	// Remark 审核备注
	Remark string `db:"remark"`
	// RemarkStatus 备注状态
	RemarkStatus int8 `db:"remark_status"`
	// CTime 创建时间（Unix 秒）
	CTime int64 `db:"ctime"`
	// MTime 最近更新时间（Unix 秒）
	MTime int64 `db:"mtime"`
}

// RealnameApplyModel 抽象 realname_apply 表的查询接口。
type RealnameApplyModel interface {
	// FindOne 查询最新一条申请单；不存在返回 nil。
	FindOne(ctx context.Context, mid int64) (*RealnameApply, error)
	// Insert 新增申请单并返回自增 ID。
	Insert(ctx context.Context, apply *RealnameApply) error
}

type defaultRealnameApplyModel struct {
	conn sqlx.SqlConn
}

// NewRealnameApplyModel 创建基于 sqlx 的 RealnameApplyModel 实现。
func NewRealnameApplyModel(conn sqlx.SqlConn) RealnameApplyModel {
	return &defaultRealnameApplyModel{conn: conn}
}

func (m *defaultRealnameApplyModel) FindOne(ctx context.Context, mid int64) (*RealnameApply, error) {
	var a RealnameApply
	query := `SELECT id, mid, realname, country, card_type, card_num, card_md5, hand_img, front_img, back_img,
		status, operator, operator_id, operator_time, remark, remark_status, ctime, mtime
		FROM realname_apply WHERE mid = ? ORDER BY id DESC LIMIT 1`
	if err := m.conn.QueryRowCtx(ctx, &a, query, mid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (m *defaultRealnameApplyModel) Insert(ctx context.Context, apply *RealnameApply) error {
	query := `INSERT INTO realname_apply (mid, realname, country, card_type, card_num, card_md5, hand_img, front_img, back_img, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	res, err := m.conn.ExecCtx(ctx, query, apply.Mid, apply.Realname, apply.Country, apply.CardType,
		apply.CardNum, apply.CardMD5, apply.HandIMG, apply.FrontIMG, apply.BackIMG, apply.Status)
	if err != nil {
		return err
	}
	apply.ID, err = res.LastInsertId()
	return err
}

// RealnameApplyImage 对应数据库 realname_apply_img 表，记录实名申请证件照。
// 移植自参考仓库 realname_apply_img 表：IMGData 为对象存储 token 路径
// （/idenfiles/<token>.txt），展示时拼接 CDN URL 模板。
type RealnameApplyImage struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// IMGData 图片 token 路径（/idenfiles/<token>.txt）
	IMGData string `db:"img_data"`
	// CTime 创建时间（Unix 秒）
	CTime int64 `db:"ctime"`
	// MTime 最近更新时间（Unix 秒）
	MTime int64 `db:"mtime"`
}

// RealnameApplyImageModel 抽象 realname_apply_img 表的查询接口。
type RealnameApplyImageModel interface {
	// FindOne 按 ID 查询；不存在返回 nil。
	FindOne(ctx context.Context, id int64) (*RealnameApplyImage, error)
	// Insert 新增并返回自增 ID。
	Insert(ctx context.Context, img *RealnameApplyImage) error
}

type defaultRealnameApplyImageModel struct {
	conn sqlx.SqlConn
}

// NewRealnameApplyImageModel 创建基于 sqlx 的 RealnameApplyImageModel 实现。
func NewRealnameApplyImageModel(conn sqlx.SqlConn) RealnameApplyImageModel {
	return &defaultRealnameApplyImageModel{conn: conn}
}

func (m *defaultRealnameApplyImageModel) FindOne(ctx context.Context, id int64) (*RealnameApplyImage, error) {
	var img RealnameApplyImage
	query := `SELECT id, img_data, ctime, mtime FROM realname_apply_img WHERE id = ? LIMIT 1`
	if err := m.conn.QueryRowCtx(ctx, &img, query, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &img, nil
}

func (m *defaultRealnameApplyImageModel) Insert(ctx context.Context, img *RealnameApplyImage) error {
	query := `INSERT INTO realname_apply_img (img_data) VALUES (?)`
	res, err := m.conn.ExecCtx(ctx, query, img.IMGData)
	if err != nil {
		return err
	}
	img.ID, err = res.LastInsertId()
	return err
}
