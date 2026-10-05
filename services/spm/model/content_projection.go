package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ContentProjection 内容本地只读投影行（spm_content_projection 表投影）。
//
// 来源是 content.published.v1（AGENTS.md §5 明确允许的「本地只读投影」）：
// 热度榜要按分区过滤、要剔除下架内容，但「稿件属于哪个分区、作者是谁、还在不在架」
// 是 video/catalog/rights 的数据，spm 没有写权限，也不能在榜单查询里跨库 JOIN，
// 因此这里只投影「过滤必需的最小列」，不复制标题、简介、UP 昵称等可变主资料。
//
// 主体标识按 (subject_type, subject_id) 存，而不是只存 aid：
// 生产者的 payload 给的是 content_id + content_type（见 README「事件词汇」），
// UGC 的 content_id 就是 aid，PGC 的 content_id 是 catalog 条目 ID。
// 若把列写成 aid，版权内容的分区榜就只能恒为空集（JOIN 不上），是静默错误。
//
// 本表同样是投影：全量重放 content.published.v1 即可重建，不是任何服务的事实源。
type ContentProjection struct {
	ID          int64  `db:"id"`            // 主键 ID
	SubjectType int32  `db:"subject_type"`  // 1 AID（UGC 稿件）、4 CATALOG_ITEM（版权条目）
	SubjectID   int64  `db:"subject_id"`    // 主体主键：UGC 为 aid，PGC 为 catalog item_id
	ContentType int32  `db:"content_type"`  // 事件原始 content_type：1 UGC、2 PGC（留作溯源）
	ZoneID      int64  `db:"zone_id"`       // 分区（0 = 事件未给出，按「分区未知」处理）
	AuthorMid   int64  `db:"author_mid"`    // 作者 mid
	State       int32  `db:"state"`         // 0 可见、1 下架/过期/删除（不再出榜）
	Category    int64  `db:"category"`      // 一级分类 ID（兴趣键之外的补充维度，0 = 未知）
	LastEventID string `db:"last_event_id"` // 最近一次生效的事件 ID（排查乱序覆盖）
	EventTime   int64  `db:"event_time"`    // 最近一次生效事件的 occurred_at（乱序守卫基准）
	Ctime       int64  `db:"ctime"`         // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`         // 本地写入时间（Unix 秒）
}

// SubjectTypeOfContent 把 content.published.v1 的 content_type 映射成聚合主体类型。
// 返回 false 表示该 content_type 没有对应的榜单主体（例如直播：
// 直播的行为分析归 live-* 链路，不在本契约的主体集合里）。
func SubjectTypeOfContent(contentType int32) (int32, bool) {
	switch contentType {
	case ContentTypeUGC:
		return SubjectTypeAid, true
	case ContentTypePGC:
		return SubjectTypeCatalogItem, true
	default:
		return SubjectTypeUnspecified, false
	}
}

// content_type 取值（与 playback/engagement/inbox 的消费口径一致：1 UGC、2 PGC、3 直播）。
const (
	ContentTypeUnspecified int32 = 0
	ContentTypeUGC         int32 = 1
	ContentTypePGC         int32 = 2
	ContentTypeLive        int32 = 3
)

// ValidContentType 判断 content_type 是否可作为事实行的内容维度。
func ValidContentType(t int32) bool { return t >= ContentTypeUGC && t <= ContentTypeLive }

// 上游 content_type 的两种表示（这是生产者之间真实存在的不一致，本服务只做归一）：
//   - event-collector 的 rpc.BehaviorEvent.content_type 是**字符串**枚举，
//     取值 ugc/pgc/live/keyword/...（services/event-collector/rpc/eventcollector.proto）；
//   - playback.heartbeat / engagement.action / content.published 的 content_type 是**整数**
//     1/2/3。
//
// keyword 这类「非内容主体」的字符串在归一后落到 ContentTypeUnspecified：
// 它既不是稿件也不是版权条目，硬塞进 1/2 会让搜索行为被当成内容播放。
const (
	ContentTypeNameUGC   = "ugc"
	ContentTypeNamePGC   = "pgc"
	ContentTypeNameLive  = "live"
	ContentTypeNameVideo = "video" // 兼容个别生产者用 video 指代 UGC 稿件
)

// ContentTypeFromName 把 event-collector 通道的字符串 content_type 归一成整数枚举。
// 未知值返回 ContentTypeUnspecified 和 false：调用方必须把「认不出内容类型」当成
// 需要观测的映射缺口，而不是默认成 UGC——默认成 UGC 会把版权条目的行为并进稿件榜。
func ContentTypeFromName(name string) (int32, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ContentTypeNameUGC, ContentTypeNameVideo:
		return ContentTypeUGC, true
	case ContentTypeNamePGC:
		return ContentTypePGC, true
	case ContentTypeNameLive:
		return ContentTypeLive, true
	default:
		return ContentTypeUnspecified, false
	}
}

// ResolveContentSubject 由 (content_type, content_id, aid) 决定这条事件属于哪个榜单主体。
//
// 这是消费者 mapping 阶段的唯一实现，规则与上游契约逐字对齐：
//   - UGC：subject = (SubjectTypeAid, content_id)；上游 behavior.* 同时给了 aid，
//     content_id 为 0 时回退 aid（playback/engagement 只给 content_id，不给 aid）。
//   - PGC：subject = (SubjectTypeCatalogItem, content_id)。**注意**：上游的 PGC
//     content_id 是 episode（集）级 ID（playback.proto:47），不是作品级 item_id；
//     要出「作品榜」必须由 catalog 提供 episode -> item 的映射，这是已登记的契约缺口
//     （见 README「上游契约缺口」），本函数不做任何猜测性折算。
//   - 直播：归 live-* 链路，本服务无主体（返回 false），事实行仍保留 content_type=3
//     以便后续按口径单列统计。
//
// 返回 false 时调用方不得写 subject_id=0 的投影行：那会把所有无法归属的行为并成一个主体。
func ResolveContentSubject(contentType int32, contentID, aid int64) (int32, int64, bool) {
	subjectType, ok := SubjectTypeOfContent(contentType)
	if !ok {
		return SubjectTypeUnspecified, 0, false
	}
	id := contentID
	if id <= 0 && contentType == ContentTypeUGC {
		id = aid
	}
	if id <= 0 {
		return SubjectTypeUnspecified, 0, false
	}
	return subjectType, id, true
}

// ContentProjectionModel spm_content_projection 表读写接口。
type ContentProjectionModel interface {
	// Apply 按事件推进投影（幂等，只保留最新状态）。
	// 乱序保护：事件自带 occurred_at 必须不早于既有行的 event_time，否则晚到的旧事件
	// 会把新状态覆盖回去——下架被旧的 publish 覆盖，等于把违规内容放回榜单。
	// 返回 true 表示这是首次建立该主体的投影。
	Apply(ctx context.Context, p *ContentProjection, occurredAt int64) (bool, error)
	// FindBySubject 查询单个主体的投影；不存在返回 nil。
	FindBySubject(ctx context.Context, subjectType int32, subjectID int64) (*ContentProjection, error)
	// ListSubjectsByZone 列出某分区下可见主体主键（榜单重建与自检用，带 limit 防止无界返回）。
	ListSubjectsByZone(
		ctx context.Context, subjectType int32, zoneID int64, limit int32,
	) ([]int64, error)
	// CountHidden 统计已下架的投影行数（观测「下架内容是否仍带行为数据」）。
	CountHidden(ctx context.Context) (int64, error)
	// CountZoneUnknown 统计分区未知的投影行数：content.published.v1 目前不保证带
	// zone_id，这个计数就是「分区榜还建不起来」的量化证据（README 契约缺口）。
	CountZoneUnknown(ctx context.Context) (int64, error)
}

type defaultContentProjectionModel struct {
	conn sqlx.SqlConn
}

// NewContentProjectionModel 创建 ContentProjectionModel 实现。
func NewContentProjectionModel(conn sqlx.SqlConn) ContentProjectionModel {
	return &defaultContentProjectionModel{conn: conn}
}

const contentProjectionColumns = "id, subject_type, subject_id, content_type, zone_id," +
	" author_mid, state, category, last_event_id, event_time, ctime, mtime"

func (m *defaultContentProjectionModel) Apply(
	ctx context.Context, p *ContentProjection, occurredAt int64,
) (bool, error) {
	if !ValidSubjectType(p.SubjectType) || p.SubjectID <= 0 {
		return false, ErrInvalidSubject
	}
	if p.State != ContentStateNormal && p.State != ContentStateHidden {
		return false, ErrInvalidSubject
	}
	if strings.TrimSpace(p.LastEventID) == "" {
		return false, ErrEventIDEmpty
	}
	if occurredAt <= 0 {
		// 绝不拿本地时钟顶替 occurred_at：乱序守卫比较的就是它，
		// 补一个「现在」会让这条事件永远赢过真实的新事件（或永远输），
		// 取决于到达顺序，表现为偶发的榜单状态错误。缺时间戳就是坏消息。
		return false, ErrInvalidWindow
	}
	now := nowUnix()
	// 乱序守卫比较的是 event_time（最近一次生效事件的 occurred_at），不是 mtime：
	// mtime 是本地写入时刻，用例会因写入延迟而大于新事件的 occurred_at，
	// 把「新状态」误判成迟到数据丢掉。整段用 VALUES() 引用插入值，无需占位符。
	const (
		cols      = "content_type, zone_id, author_mid, state, category, last_event_id"
		guardForm = "col = IF(event_time <= VALUES(event_time), VALUES(col), col)"
	)
	sets := make([]string, 0, 8)
	for _, col := range strings.Split(cols, ", ") {
		sets = append(sets, strings.ReplaceAll(guardForm, "col", col))
	}
	// event_time 必须排在所有守卫列之后：MySQL 从左到右求值，先更新它会让后续判定失效。
	sets = append(sets, "event_time = VALUES(event_time)", "mtime = VALUES(mtime)")

	query := "INSERT INTO spm_content_projection (" + cols +
		", subject_type, subject_id, event_time, ctime, mtime)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)" +
		" ON DUPLICATE KEY UPDATE " + strings.Join(sets, ", ")
	res, err := m.conn.ExecCtx(ctx, query,
		p.ContentType, p.ZoneID, p.AuthorMid, p.State, p.Category, truncate(p.LastEventID, 64),
		p.SubjectType, p.SubjectID, occurredAt, now, now)
	if err != nil {
		return false, fmt.Errorf("spm_content_projection Apply: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("spm_content_projection Apply RowsAffected: %w", err)
	}
	p.EventTime, p.Ctime, p.Mtime = occurredAt, now, now
	// MySQL 对 INSERT..ON DUPLICATE 的 affected 语义：插入 1、更新且值变化 2、值未变化 0。
	return affected == 1, nil
}

func (m *defaultContentProjectionModel) FindBySubject(
	ctx context.Context, subjectType int32, subjectID int64,
) (*ContentProjection, error) {
	var row ContentProjection
	query := "SELECT " + contentProjectionColumns +
		" FROM spm_content_projection WHERE subject_type = ? AND subject_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, subjectType, subjectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_content_projection FindBySubject: %w", err)
	}
	return &row, nil
}

func (m *defaultContentProjectionModel) ListSubjectsByZone(
	ctx context.Context, subjectType int32, zoneID int64, limit int32,
) ([]int64, error) {
	if !ValidSubjectType(subjectType) || zoneID <= 0 {
		return nil, ErrInvalidSubject
	}
	var rows []int64
	query := "SELECT subject_id FROM spm_content_projection" +
		" WHERE subject_type = ? AND zone_id = ? AND state = ?" +
		" ORDER BY subject_id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query,
		subjectType, zoneID, ContentStateNormal, clampLimit(limit)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_content_projection ListSubjectsByZone: %w", err)
	}
	return rows, nil
}

func (m *defaultContentProjectionModel) CountHidden(ctx context.Context) (int64, error) {
	var n int64
	query := "SELECT COUNT(*) FROM spm_content_projection WHERE state = ?"
	if err := m.conn.QueryRowCtx(ctx, &n, query, ContentStateHidden); err != nil {
		return 0, fmt.Errorf("spm_content_projection CountHidden: %w", err)
	}
	return n, nil
}

func (m *defaultContentProjectionModel) CountZoneUnknown(ctx context.Context) (int64, error) {
	var n int64
	query := "SELECT COUNT(*) FROM spm_content_projection WHERE zone_id = 0"
	if err := m.conn.QueryRowCtx(ctx, &n, query); err != nil {
		return 0, fmt.Errorf("spm_content_projection CountZoneUnknown: %w", err)
	}
	return n, nil
}
