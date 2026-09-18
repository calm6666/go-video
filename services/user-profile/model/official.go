package model

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// OfficialInfo 官方认证生效信息（参考 model.OfficialInfo，来源于 user_official 表）。
type OfficialInfo struct {
	// Role 认证角色：0 未认证、1 UP 主、2 身份、3 企业、4 政府、5 媒体、6 其他
	Role int8 `db:"role"`
	// Title 认证称号
	Title string `db:"title"`
	// Desc 认证描述
	Desc string `db:"desc"`
}

// Equal 判断两个认证信息是否一致（参考 OfficialInfo.Equal）。
func (o *OfficialInfo) Equal(c *OfficialInfo) bool {
	if o == nil || c == nil {
		return o == c
	}
	return o.Role == c.Role && o.Title == c.Title && o.Desc == c.Desc
}

// UserOfficial 对应数据库 user_official 表，记录已生效的官方认证信息。
type UserOfficial struct {
	// Mid 用户 ID，主键
	Mid int64 `db:"mid"`
	// Role 认证角色
	Role int8 `db:"role"`
	// Title 认证称号
	Title string `db:"title"`
	// Desc 认证描述
	Desc string `db:"desc"`
}

// UserOfficialModel 抽象 user_official 表的查询接口。
type UserOfficialModel interface {
	// FindOne 查询单个用户的生效认证；不存在返回 nil。
	FindOne(ctx context.Context, mid int64) (*OfficialInfo, error)
	// All 查询全部生效认证（服务启动时载入内存，供批量聚合）。
	All(ctx context.Context) (map[int64]*OfficialInfo, error)
}

type defaultUserOfficialModel struct {
	conn sqlx.SqlConn
}

// NewUserOfficialModel 创建基于 sqlx 的 UserOfficialModel 实现。
func NewUserOfficialModel(conn sqlx.SqlConn) UserOfficialModel {
	return &defaultUserOfficialModel{conn: conn}
}

func (m *defaultUserOfficialModel) FindOne(ctx context.Context, mid int64) (*OfficialInfo, error) {
	var o OfficialInfo
	query := `SELECT role, title, description FROM user_official WHERE mid = ? AND role > 0`
	if err := m.conn.QueryRowCtx(ctx, &o, query, mid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

func (m *defaultUserOfficialModel) All(ctx context.Context) (map[int64]*OfficialInfo, error) {
	query := `SELECT mid, role, title, description FROM user_official WHERE role > 0`
	var rows []*UserOfficial
	if err := m.conn.QueryRowsCtx(ctx, &rows, query); err != nil {
		return nil, err
	}
	result := make(map[int64]*OfficialInfo, len(rows))
	for _, r := range rows {
		result[r.Mid] = &OfficialInfo{Role: r.Role, Title: r.Title, Desc: r.Desc}
	}
	return result, nil
}

// OfficialExtra 认证附加资料（参考 model.OfficialExtra），以 JSON 存于
// user_official_doc.extra 字段。
type OfficialExtra struct {
	// Realname 是否实名：0 否、1 是
	Realname int8 `json:"realname"`
	// Operator 经营人/联系人
	Operator string `json:"operator"`
	// Telephone 联系电话
	Telephone string `json:"telephone"`
	// Email 联系邮箱
	Email string `json:"email"`
	// Address 联系地址
	Address string `json:"address"`
	// Company 公司名称
	Company string `json:"company"`
	// CreditCode 统一社会信用代码
	CreditCode string `json:"credit_code"`
	// Organization 政府或组织机构名称
	Organization string `json:"organization"`
	// OrganizationType 组织机构类型
	OrganizationType string `json:"organization_type"`
	// BusinessLicense 营业执照
	BusinessLicense string `json:"business_license"`
	// BusinessScale 企业规模
	BusinessScale string `json:"business_scale"`
	// BusinessLevel 企业等级
	BusinessLevel string `json:"business_level"`
	// BusinessAuth 企业授权函
	BusinessAuth string `json:"business_auth"`
	// Supplement 其他补充材料
	Supplement string `json:"supplement"`
	// Professional 专业资质
	Professional string `json:"professional"`
	// Identification 身份证明
	Identification string `json:"identification"`
	// OfficialSite 官网地址
	OfficialSite string `json:"official_site"`
	// RegisteredCapital 注册资金
	RegisteredCapital string `json:"registered_capital"`
}

// String 序列化附加资料为 JSON（参考 OfficialExtra.String）。
func (e OfficialExtra) String() string {
	bs, _ := json.Marshal(e)
	if len(bs) == 0 {
		return "{}"
	}
	return string(bs)
}

// ParseExtra 从 JSON 解析附加资料（参考 OfficialDoc.ParseExtra）。
func ParseExtra(s string) OfficialExtra {
	oe := OfficialExtra{}
	if len(s) > 0 {
		_ = json.Unmarshal([]byte(s), &oe)
	}
	return oe
}

// OfficialDoc 对应数据库 user_official_doc 表，记录官方认证申请文档。
// 认证审核工作流后续由 creator/operation 服务消费；本表为 user-profile
// 持有的提交与查询入口（与参考仓库 member 服务一致）。
type OfficialDoc struct {
	// Mid 用户 ID，主键
	Mid int64 `db:"mid"`
	// Name 认证主体名称
	Name string `db:"name"`
	// State 审核状态：0 待审核、1 通过、2 不通过、3 重新提交
	State int8 `db:"state"`
	// Role 认证角色
	Role int8 `db:"role"`
	// Title 认证称号
	Title string `db:"title"`
	// Desc 认证描述
	Desc string `db:"desc"`
	// RejectReason 拒绝原因
	RejectReason string `db:"reject_reason"`
	// Extra 附加资料 JSON（OfficialExtra）
	Extra string `db:"extra"`
	// SubmitSource 提交来源
	SubmitSource string `db:"submit_source"`
	// SubmitTime 最后提交时间（Unix 秒）
	SubmitTime int64 `db:"submit_time"`
}

// Validate 校验认证文档必填字段（参考 OfficialDoc.Validate）。
func (d *OfficialDoc) Validate() bool {
	return d != nil && d.Mid > 0 && d.Name != "" && d.Role > 0 && d.Title != ""
}

// OfficialDocModel 抽象 user_official_doc 表的查询接口。
type OfficialDocModel interface {
	// FindOne 查询认证文档；不存在返回 nil。
	FindOne(ctx context.Context, mid int64) (*OfficialDoc, error)
	// Upsert 写入/更新认证文档（提交时强制 state=待审核）。
	Upsert(ctx context.Context, doc *OfficialDoc) error
}

type defaultOfficialDocModel struct {
	conn sqlx.SqlConn
}

// NewOfficialDocModel 创建基于 sqlx 的 OfficialDocModel 实现。
func NewOfficialDocModel(conn sqlx.SqlConn) OfficialDocModel {
	return &defaultOfficialDocModel{conn: conn}
}

func (m *defaultOfficialDocModel) FindOne(ctx context.Context, mid int64) (*OfficialDoc, error) {
	var d OfficialDoc
	query := `SELECT mid, name, state, role, title, description, reject_reason, extra, submit_source, submit_time
		FROM user_official_doc WHERE mid = ?`
	if err := m.conn.QueryRowCtx(ctx, &d, query, mid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

func (m *defaultOfficialDocModel) Upsert(ctx context.Context, doc *OfficialDoc) error {
	query := `INSERT INTO user_official_doc (mid, name, state, role, title, description, extra, submit_source, submit_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE name = VALUES(name), state = VALUES(state), role = VALUES(role),
		title = VALUES(title), description = VALUES(description), extra = VALUES(extra),
		submit_source = VALUES(submit_source), submit_time = VALUES(submit_time)`
	_, err := m.conn.ExecCtx(ctx, query, doc.Mid, doc.Name, doc.State, doc.Role, doc.Title,
		doc.Desc, doc.Extra, doc.SubmitSource, doc.SubmitTime)
	return err
}

// OfficialDocAdditModel 抽象 user_official_doc_addit 表的查询接口。
// 附加键值表（参考仓库 SetOfficialDocAddit），当前用于统一社会信用代码。
type OfficialDocAdditModel interface {
	// Upsert 写入/更新键值。
	Upsert(ctx context.Context, mid int64, property, vstring string) error
}

type defaultOfficialDocAdditModel struct {
	conn sqlx.SqlConn
}

// NewOfficialDocAdditModel 创建基于 sqlx 的 OfficialDocAdditModel 实现。
func NewOfficialDocAdditModel(conn sqlx.SqlConn) OfficialDocAdditModel {
	return &defaultOfficialDocAdditModel{conn: conn}
}

func (m *defaultOfficialDocAdditModel) Upsert(ctx context.Context, mid int64, property, vstring string) error {
	query := `INSERT INTO user_official_doc_addit (mid, property, vstring) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE vstring = VALUES(vstring)`
	_, err := m.conn.ExecCtx(ctx, query, mid, property, vstring)
	return err
}
