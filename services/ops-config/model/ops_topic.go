package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// topicColumns 是 ops_topic 的列清单，必须与
// deploy/migrations/ops-config/000002_create_ops_topic_tables.sql 一致。
const topicColumns = "topic_id, slug, title, description, cover, zone_ids, tag_ids," +
	" state, sort, start_at, end_at, version, operator_id, ctime, mtime"

// topicSlugRe 是专题 slug 格式：小写字母/数字开头，可含下划线与连字符，长度 2..64。
// slug 是端上寻址用的稳定标识（会进 URL、进 Redis key、进客户端本地缓存），
// 因此和 cfg_key 一样必须固化格式：允许大小写或空格会让「同一个专题」出现多种写法。
var topicSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,63}$`)

// ValidTopicSlug 判定 slug 格式。
func ValidTopicSlug(s string) bool { return topicSlugRe.MatchString(s) }

// MaxTopicRefIDs 单个专题可引用的分区/标签个数上限。
// 专题挂几十个分区通常是配置错误；上限同时也是 zone_ids 列（VARCHAR(512)）的
// 长度保护，两者一起才不会退化成「写入成功但 LIKE 反查漏行」。
const MaxTopicRefIDs = 64

// Topic 对应 ops_topic 表：专题/合集主记录。
//
// 数据所有权（AGENTS.md §5，结论见 services/ops-config/README.md）：
// 本表只保存**运营配置本身**（标题、封面、生效窗口、引用列表、排序）。
// zone_ids / tag_ids 是对 catalog 的 ID 引用（catalog_zone.zoneid / catalog_tag.tagid），
// 本服务不存分区名、标签名等可变主资料，展示名由调用方经 catalog RPC 解析；
// 因此 catalog 侧改名/合并分区不需要本表跟改，也不存在两侧不一致的窗口。
type Topic struct {
	// TopicID 自增主键。
	TopicID int64 `db:"topic_id"`
	// Slug 稳定标识（uniq_slug），端上按此寻址。
	Slug string `db:"slug"`
	// Title 专题标题。
	Title string `db:"title"`
	// Description 简介。
	Description string `db:"description"`
	// Cover 封面：只存展示用 URL 或 object_key 字符串。
	// 真实可播放/可访问地址由 playback/asset 侧短期签名，本服务不发签名（AGENTS.md §6）。
	Cover string `db:"cover"`
	// ZoneIDs 引用的分区 ID，存储形态 ",12,31,"（空表示不挂分区）。
	ZoneIDs string `db:"zone_ids"`
	// TagIDs 引用的标签 ID，存储形态同上。
	TagIDs string `db:"tag_ids"`
	// State 1 上架、2 下架。
	State int32 `db:"state"`
	// Sort 列表排序，小者在前（列名与 operation/op_menu.sort 保持同一约定）。
	Sort int32 `db:"sort"`
	// StartAt 生效窗口起（Unix 秒），0 表示立即。
	StartAt int64 `db:"start_at"`
	// EndAt 生效窗口止（Unix 秒），0 表示不设截止。
	EndAt int64 `db:"end_at"`
	// Version 乐观锁版本：每次成功写入 +1，SaveTopicReq.expect_version 比对的就是它。
	Version int64 `db:"version"`
	// OperatorID 最后操作人 admin_id（引用 operation，不复制资料）。
	OperatorID int64 `db:"operator_id"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// ZoneIDList 供契约投影（rpc.Topic.zone_ids）。
func (t *Topic) ZoneIDList() []int64 { return ParseIDList(t.ZoneIDs) }

// TagIDList 供契约投影（rpc.Topic.tag_ids）。
func (t *Topic) TagIDList() []int64 { return ParseIDList(t.TagIDs) }

// InWindow 判定专题在 ts 时刻是否处于生效窗口内（与 RolloutRule.InTimeWindow 同语义）。
func (t *Topic) InWindow(ts int64) bool {
	if t.StartAt > 0 && ts < t.StartAt {
		return false
	}
	if t.EndAt > 0 && ts >= t.EndAt {
		return false
	}
	return true
}

// TopicFilter 后台列表条件（ListTopics）。
type TopicFilter struct {
	State int32
	// ZoneID / TagID 按引用 ID 反查专题：0 表示不过滤。
	// 这是本服务对 catalog 唯一的「查询方向」——用 ID 反查，不复制名字。
	ZoneID  int64
	TagID   int64
	Keyword string
	// OnlineOnly true 时附加「state=ON 且当前时间在生效窗口内」。
	OnlineOnly bool
	Now        int64
	Pn         int32
	Ps         int32
}

// TopicModel 抽象 ops_topic 表。
type TopicModel interface {
	// Insert 新建专题；slug 冲突返回 ErrTopicSlugConflict。
	Insert(ctx context.Context, topic *Topic) (int64, error)
	// FindByID 按主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, topicID int64) (*Topic, error)
	// FindBySlug 按 slug 查询；不存在返回 (nil, nil)。
	FindBySlug(ctx context.Context, slug string) (*Topic, error)
	// UpdateWithVersion 乐观锁更新：仅当库里 version == expectVersion 时整行覆盖并 version+1。
	// 返回 updated=false 表示被人抢先改过，调用方必须重读后重试而不是覆盖对方。
	UpdateWithVersion(ctx context.Context, topic *Topic, expectVersion, ts int64) (bool, error)
	// List 分页查询（带 COUNT），固定 sort ASC, topic_id ASC，必须 LIMIT。
	List(ctx context.Context, f TopicFilter) ([]*Topic, int64, error)
	// ListOnline 运行时取生效中的专题（GetTopic/ListTopics 的 online_only 与缓存重建用）。
	// limit 必带：专题列表是给网关消费的，不能因为运营建了 5000 个专题就回 5000 行。
	ListOnline(ctx context.Context, ts int64, limit int) ([]*Topic, error)
	// TouchMtime 仅更新 mtime（条目变更后标记「内容变了」，不改主记录其它列）。
	// 返回受影响行数；topic 不存在时为 0，由调用方判定是否报错。
	TouchMtime(ctx context.Context, topicID, ts int64) (int64, error)
}

type defaultTopicModel struct {
	conn sqlx.SqlConn
}

// NewTopicModel 构造 ops_topic 的 sqlx 实现。
func NewTopicModel(conn sqlx.SqlConn) TopicModel {
	return &defaultTopicModel{conn: conn}
}

func (m *defaultTopicModel) Insert(ctx context.Context, topic *Topic) (int64, error) {
	if !ValidTopicSlug(topic.Slug) {
		return 0, ErrTopicSlugInvalid
	}
	if topic.Title == "" {
		return 0, ErrTopicTitleRequired
	}
	if topic.EndAt > 0 && topic.EndAt <= topic.StartAt {
		return 0, ErrTopicTimeRangeInvalid
	}
	if _, err := ValidateIDList(topic.ZoneIDList(), MaxTopicRefIDs); err != nil {
		return 0, err
	}
	if _, err := ValidateIDList(topic.TagIDList(), MaxTopicRefIDs); err != nil {
		return 0, err
	}
	if topic.Ctime == 0 {
		topic.Ctime = nowUnix()
	}
	topic.Mtime = topic.Ctime
	if topic.State == 0 {
		// 默认下架：新建专题就立刻对外可见不是任何人想要的语义——
		// 条目还没挂就先上线，端上会看到一个空专题。
		topic.State = StateOff
	}
	if topic.Version == 0 {
		topic.Version = 1
	}
	// uniq_slug 冲突时 ON DUPLICATE KEY UPDATE 刻意空更新：RowsAffected==0 即冲突，
	// 不依赖驱动专有错误码（AGENTS.md §5）。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO ops_topic ("+topicColumns+") VALUES ("+placeholders(15)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		topic.TopicID, topic.Slug, topic.Title, topic.Description, topic.Cover,
		topic.ZoneIDs, topic.TagIDs, topic.State, topic.Sort, topic.StartAt, topic.EndAt,
		topic.Version, topic.OperatorID, topic.Ctime, topic.Mtime)
	if err != nil {
		return 0, fmt.Errorf("ops_topic Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_topic Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return 0, ErrTopicSlugConflict
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ops_topic Insert LastInsertId: %w", err)
	}
	topic.TopicID = id
	return id, nil
}

const topicSelect = "SELECT " + topicColumns + " FROM ops_topic"

func (m *defaultTopicModel) FindByID(ctx context.Context, topicID int64) (*Topic, error) {
	var row Topic
	query := topicSelect + " WHERE topic_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, topicID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_topic FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultTopicModel) FindBySlug(ctx context.Context, slug string) (*Topic, error) {
	if slug == "" {
		return nil, ErrTopicSlugRequired
	}
	var row Topic
	query := topicSelect + " WHERE slug = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, slug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_topic FindBySlug: %w", err)
	}
	return &row, nil
}

func (m *defaultTopicModel) UpdateWithVersion(ctx context.Context, topic *Topic, expectVersion, ts int64) (bool, error) {
	if topic.TopicID <= 0 {
		return false, ErrTopicNotFound
	}
	if !ValidTopicSlug(topic.Slug) {
		return false, ErrTopicSlugInvalid
	}
	if topic.Title == "" {
		return false, ErrTopicTitleRequired
	}
	if topic.EndAt > 0 && topic.EndAt <= topic.StartAt {
		return false, ErrTopicTimeRangeInvalid
	}
	if _, err := ValidateIDList(topic.ZoneIDList(), MaxTopicRefIDs); err != nil {
		return false, err
	}
	if _, err := ValidateIDList(topic.TagIDList(), MaxTopicRefIDs); err != nil {
		return false, err
	}
	// slug 可改，因此 UPDATE 也可能撞 uniq_slug；撞上的行不会更新（WHERE 未命中），
	// 但为了把「slug 被占用」和「版本冲突」区分开，先做一次显式探测。
	var holder int64
	err := m.conn.QueryRowCtx(ctx, &holder,
		"SELECT topic_id FROM ops_topic WHERE slug = ? AND topic_id <> ? LIMIT 1",
		topic.Slug, topic.TopicID)
	if err != nil {
		// 注意语义方向：查不到行 = 没有别人占着这个 slug，是正常的「可以继续」。
		// （早先这里把 ErrNoRows 当成冲突返回，结果每次改标题都必失败。）
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("ops_topic UpdateWithVersion slug probe: %w", err)
		}
	} else if holder > 0 {
		return false, ErrTopicSlugConflict
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE ops_topic SET slug = ?, title = ?, description = ?, cover = ?, zone_ids = ?,"+
			" tag_ids = ?, state = ?, sort = ?, start_at = ?, end_at = ?,"+
			" version = version + 1, operator_id = ?, mtime = ?"+
			" WHERE topic_id = ? AND version = ?",
		topic.Slug, topic.Title, topic.Description, topic.Cover, topic.ZoneIDs,
		topic.TagIDs, topic.State, topic.Sort, topic.StartAt, topic.EndAt,
		topic.OperatorID, ts, topic.TopicID, expectVersion)
	if err != nil {
		return false, fmt.Errorf("ops_topic UpdateWithVersion: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ops_topic UpdateWithVersion RowsAffected: %w", err)
	}
	if aff == 0 {
		return false, nil
	}
	// 回写内存对象，调用方直接投影新值而不用再查一次库。
	topic.Version = expectVersion + 1
	topic.Mtime = ts
	return true, nil
}

func (m *defaultTopicModel) List(ctx context.Context, f TopicFilter) ([]*Topic, int64, error) {
	where, args := f.build()
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM ops_topic "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("ops_topic List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*Topic
	// sort ASC, topic_id ASC：运营手工排的序就是列表序；topic_id 兜底保证翻页稳定
	// （只按 sort 排时同值行的次序由引擎决定，翻页会出现重复与漏项）。
	query := topicSelect + where + " ORDER BY sort ASC, topic_id ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("ops_topic List: %w", err)
	}
	return rows, total, nil
}

// build 拼 WHERE。ZoneID/TagID 用 `zone_ids LIKE '%,12,%'` 判定：
// 与内存侧 IDListContains 同口径（首尾逗号保证 12 不会误命中 120）。
// 这一列不做索引是刻意的：专题总量在千级，且「按分区反查专题」是后台低频操作；
// 真要提速应当加一张 ops_topic_zone_ref 关系表，而不是把 LIKE 塞进运行时热路径。
func (f TopicFilter) build() (string, []any) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 5)
	if f.State > 0 {
		where += " AND state = ?"
		args = append(args, f.State)
	}
	if f.ZoneID > 0 {
		where += " AND zone_ids LIKE ?"
		args = append(args, IDListLike(f.ZoneID))
	}
	if f.TagID > 0 {
		where += " AND tag_ids LIKE ?"
		args = append(args, IDListLike(f.TagID))
	}
	if f.OnlineOnly {
		where += " AND state = ? AND (start_at = 0 OR start_at <= ?) AND (end_at = 0 OR end_at > ?)"
		args = append(args, StateOn, f.Now, f.Now)
	}
	if f.Keyword != "" {
		where += " AND (slug LIKE ? OR title LIKE ?)"
		like := "%" + escapeLike(f.Keyword) + "%"
		args = append(args, like, like)
	}
	return where, args
}

func (m *defaultTopicModel) ListOnline(ctx context.Context, ts int64, limit int) ([]*Topic, error) {
	if limit <= 0 {
		limit = DefaultTopicListLimit
	}
	var rows []*Topic
	f := TopicFilter{OnlineOnly: true, Now: ts, Pn: 1, Ps: int32(limit)}
	where, args := f.build()
	listArgs := append(append([]any{}, args...), limit, 0)
	query := topicSelect + where + " ORDER BY sort ASC, topic_id ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_topic ListOnline: %w", err)
	}
	return rows, nil
}

func (m *defaultTopicModel) TouchMtime(ctx context.Context, topicID, ts int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx, "UPDATE ops_topic SET mtime = ? WHERE topic_id = ?", ts, topicID)
	if err != nil {
		return 0, fmt.Errorf("ops_topic TouchMtime: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_topic TouchMtime RowsAffected: %w", err)
	}
	return n, nil
}

// DefaultTopicListLimit GetTopicReq.item_limit 与 ListOnline 的服务端默认上限。
// 契约里 item_limit<=0 时走它：网关一次拉专题详情不应该把整表条目搬回去。
const DefaultTopicListLimit = 100
