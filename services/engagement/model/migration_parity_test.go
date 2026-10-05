package model

// 本文件是 model 层与 deploy/migrations/engagement/*.sql 的**双向逐列**离线门禁（AGENTS.md §9）。
// 不连数据库：只读迁移 SQL 文本 + 用 go/ast 解析 model 源码里的 SQL 字面量 + 反射结构体 tag。
//
// 为什么本服务必须有这条门禁（真实缺陷，services/engagement/README.md 已知缺口 1）：
// 迁移 `000003_create_share.sql` 声明的列是 `tp`，而 `model/share_log.go` 的 SQL 曾写 `type`
// （`INSERT IGNORE INTO share_log (oid, mid, type, day, ctime)` 与 `WHERE oid = ? AND type = ?`）。
// internal/logic 的内存替身按 model 接口复刻的是**语义**、不是 SQL 文本，两边都跟着 model 走，
// 所以 138 个 logic 用例全绿，只有真库会报 Unknown column。③ 号断言
// （TestHandWrittenSQLColumnsExistInMigration）就是为这条漂移而存在。
//
// 覆盖方向（每条都注明它抓什么）：
//  ① SQL 里的表名 ↔ 迁移建表语句，双向
//     （TestEveryTableSpecIsBackedByMigration / TestModelSQLTablesAreDeclaredInMigrations）；
//  ② 结构体 db tag ↔ DDL 列名与可空性、Go 整型宽度 ↔ 列位宽、NOT NULL 列必须有写路径
//     （TestModelColumnsMatchMigrationColumns / TestColumnTypesMatchGoFieldWidth /
//     TestEveryNotNullColumnHasWritePath）；
//  ③ model 手写 SQL 的五个位置（INSERT 列清单、SET/ODKU 目标、WHERE 定位、投影、剩余裸标识符）
//     里的每个列名都必须存在于 DDL —— 抓 share_log 那种漂移；
//  ④ ON DUPLICATE KEY UPDATE / INSERT IGNORE 的冲突键必须是 DDL 里的 PRIMARY KEY 或某个 UNIQUE KEY，
//     否则 UPSERT 永不冲突、按天幂等与 1:1 不变量静默失效
//     （TestUpsertAndInsertIgnoreConflictKeysAreRealKeys）；
//  ⑤ UNIQUE 索引列组合 ↔ spec 登记 ↔ model 头注释的幂等元组 ↔ 真的按整键定位的那几条查询，
//     四处同序（TestIdempotencyKeyTuplesAgreeAcrossDDLSpecAndModelDocs）；
//  ⑥ 字符列清单 ↔ DDL 实际类型（整数列绝不允许登记成字符列）+ 长度口径与排序规则现状
//     （TestCharColumnRegistryMatchesDDLTypes）。
//
// 三处「按列登记的例外」（engagement_outbox 落地时引入，都是登记 + 反向闭合，不是放宽断言）：
//   - `noDefaultColumns`：NOT NULL 且刻意不给 DEFAULT 的必填列（②）。登记后仍要求该列
//     确实无默认值、确实 NOT NULL、确实是字符列；补了 DEFAULT 而登记还留着会反向报错。
//   - `binaryCollate`：刻意声明列级 `COLLATE utf8mb4_bin` 的字符列（⑥）。登记后仍要求
//     规则字面就是 utf8mb4_bin 且该列确实在某个 UNIQUE KEY 里；COLLATE 被删而登记还留着会报错。
//   - `uniqIndexNameExceptions`：不符合 expectedUniqIndexName 推导结果的唯一索引名（⑤）。
//     登记后仍要求该索引存在、且确实与列名推导结果不同。
//
// 本门禁**证明不了**的事（如实声明，别当成它验过 schema）：
//   - 不连库：迁移能否在 MySQL 上真的执行、语法是否被接受、索引是否真被优化器选中都不在范围内；
//   - 不推断库表默认 collation 实际是什么（那要连库才确认）：本文件只核对「有没有显式 COLLATE」
//     与登记列的规则字面，因此未声明排序规则的列仍然只是走服务端默认，本门禁不替它担保；
//   - 不比索引顺序之外的物理属性（USING BTREE、前缀长度写法在解析阶段直接判为「无法比对」）；
//   - 不覆盖 internal/repository 的 Redis key 与影子计数器（不属于 SQL schema）；
//   - 不覆盖 internal/logic 的内存替身与 SQL 是否同语义（那是 logic 用例的职责）；
//   - 不校验 model 的 SELECT 顺序等于 DDL 列顺序：本服务全部查询都显式列名，
//     favorite_folder 的结构体字段顺序（count 在最后）与 DDL 列顺序（count 在 ctime 之前）
//     本来就不一致，这是**允许**的，见 TestFullRowProjectionsMatchStructOrder 的注释。
//
// 迁移文件只读不改（AGENTS.md §10）：本文件是漂移探测器，不是改库入口。扫出新漂移时
// 先修 model 侧或另开迁移文件，不要在测试里放宽断言。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const migrationsRelDir = "deploy/migrations/engagement"

// engagementDBName 是每个迁移头注释必须声明的库名（AGENTS.md §5 数据所有权）。
const engagementDBName = "go_video_engagement"

// dayYyyymmddMax 是 errors.go 的 currentDayYyyymmdd 能产出的最大值，
// 用来核对 share_log.day 的整数位宽（见 TestDailyBucketIsIntegerYyyymmdd）。
const dayYyyymmddMax = 9999*10000 + 12*100 + 31

// tableSpec 声明一张 model 表与它那张迁移建表语句的对应关系。
// 所有列名/索引名清单一律小写，与 DDL 的反引号标识符同口径（归一比较见 lowerSet）。
type tableSpec struct {
	table  string
	goFile string // "" 表示本服务没有这张表的 model 实现
	row    any    // nil ⇒ 只有 schema、没有 model（见 modellessReason）
	// modellessReason 是「有表无 model」的登记理由，必须指向 README 已知缺口编号。
	modellessReason string
	primaryKey      []string
	// uniqueKeys 是幂等与域内不变量依赖的唯一索引：索引名 -> 列组合（顺序敏感）。
	uniqueKeys map[string][]string
	// noUniqueKeyReason 用于「确实没有唯一键」的表：必须写理由，且 DDL 也必须真的没有唯一键。
	noUniqueKeyReason string
	// idempotencyIndex + idempotencyNote：model 头注释里必须原样出现的幂等元组文本，
	// 以及它在 DDL 里对应的那个 UNIQUE KEY 名字。
	idempotencyIndex string
	idempotencyNote  string
	// wholeKeyLookups 要求「至少 N 条 model 语句以整组等值（含 IN (?)）定位在该唯一键上」。
	wholeKeyLookups map[string]int
	// conflictKeys 是 ON DUPLICATE KEY UPDATE / INSERT IGNORE 语句真正依赖的冲突键。
	conflictKeys []string
	// charColumns 是 DDL 里的字符列（VARCHAR/CHAR/TEXT/ENUM/SET）清单。
	charColumns []string
	// charLimits 是 model 侧允许的最大字符数；断言方向：列宽 >= 上限。
	charLimits map[string]int
	// unboundedChar 是「model/logic 都没做长度校验」的字符列豁免登记：列名 -> 理由 + 缺口编号。
	// 两个清单都不登记的字符列会被判失败，「新增一个没尺子的 VARCHAR」就等于留了校验缺口。
	unboundedChar map[string]string
	// enumValues 是有 model 常量的枚举列：列名 -> 常量取值集合，必须逐个出现在 DDL 注释里。
	enumValues map[string][]int64
	// unenumeratedUniqEnum 是「参与唯一键但注释未逐值枚举」的 TINYINT 列：列名 -> 理由 + 缺口编号。
	unenumeratedUniqEnum map[string]string
	// requiredIndexes 是 model 查询依赖的普通索引：索引名 -> 列组合（顺序敏感）。
	requiredIndexes map[string][]string
	// deferredIndexes 是 DDL 里存在、但当前没有任何 model 查询依赖的普通索引：索引名 -> 理由。
	// 登记而不是放行，是为了让「新增一个没人用的索引」必须写理由。
	deferredIndexes map[string]string
	// timeColumns 是 Unix 秒时间列，必须是 BIGINT。
	timeColumns []string
	// binaryCollate 是**刻意**声明列级排序规则的字符列：列名 -> 理由。
	// 本域其它表全部走库表默认（见 ⑥ 与 README 已知缺口 18 的 ②），只有 outbox 的
	// uniq_event_id 需要逐字节比较；豁免按列登记，且必须同时钉住「该列确实在唯一键里、
	// 规则确实是 utf8mb4_bin」，所以这不是放行而是换了一种更强的口径。
	binaryCollate map[string]string
	// noDefaultColumns 是「NOT NULL 且刻意不给 DEFAULT」的必填列：列名 -> 理由。
	// 没有豁免时 ② 会要求每个 NOT NULL 列都有 DEFAULT；事件 ID 给了 DEFAULT '' 会让
	// 「漏填」的第二行撞唯一键、报成重复投递，聚合 JSON（MEDIUMTEXT）也无法给默认值。
	noDefaultColumns map[string]string
	// uniqIndexNameExceptions 是不符合 expectedUniqIndexName 命名口径的唯一索引：索引名 -> 理由。
	// 该口径从列名推导（(event_id) -> uniq_event），但全仓 event_id 单列唯一键的既有命名
	// 一律是 uniq_event_id（本文件写作用到时的 16 处，含 video/upload/playback/recall_outbox
	// 与 inbox/spm/search-indexer 的去重表），唯一异名是 danmaku 的 uniq_event。
	// 改名会让本域与其余服务的排障口径断裂，所以登记例外并要求写理由。
	uniqIndexNameExceptions map[string]string
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:            "thumbup_like",
			goFile:           "thumbup_like.go",
			row:              ThumbupLike{},
			primaryKey:       []string{"id"},
			uniqueKeys:       map[string][]string{"uniq_business_mid_message": {"business", "mid", "message_id"}},
			idempotencyIndex: "uniq_business_mid_message",
			idempotencyNote:  "(business, mid, message_id)",
			wholeKeyLookups:  map[string]int{"uniq_business_mid_message": 1}, // FindStates
			conflictKeys:     []string{"uniq_business_mid_message"},
			charColumns:      []string{"business"},
			unboundedChar: map[string]string{
				"business": "logic 只校验非空（ErrInvalidBusiness），没有长度与取值白名单；见 README 已知缺口 18",
			},
			enumValues: map[string][]int64{"state": {LikeStateCancel, LikeStateLike, LikeStateDislike}},
			requiredIndexes: map[string][]string{
				"idx_origin_message": {"origin_id", "message_id"}, // ListByItem 的对象维度过滤
				"idx_mid_ctime":      {"mid", "ctime"},            // ListByMid 按 mid 取、按 ctime 倒序翻页
			},
			timeColumns: []string{"ctime", "mtime"},
		},
		{
			table:            "thumbup_stat",
			goFile:           "thumbup_stat.go",
			row:              ThumbupStat{},
			primaryKey:       []string{"id"},
			uniqueKeys:       map[string][]string{"uniq_business_origin_message": {"business", "origin_id", "message_id"}},
			idempotencyIndex: "uniq_business_origin_message",
			idempotencyNote:  "(business, origin_id, message_id)",
			wholeKeyLookups:  map[string]int{"uniq_business_origin_message": 3}, // FindOne + FindMany + UpdateChange
			conflictKeys:     []string{"uniq_business_origin_message"},
			charColumns:      []string{"business"},
			unboundedChar: map[string]string{
				"business": "同 thumbup_like：logic 未做长度校验；见 README 已知缺口 18",
			},
			timeColumns: []string{"ctime", "mtime"},
		},
		{
			table:             "favorite_folder",
			goFile:            "favorite_folder.go",
			row:               FavoriteFolder{},
			primaryKey:        []string{"fid"},
			uniqueKeys:        nil,
			noUniqueKeyReason: "新建收藏夹不做幂等：fid 由自增主键分配，Add 每次都插新行；每人 100 个夹的配额在 logic 里也没实现（README 已知缺口 8）",
			idempotencyIndex:  "",
			idempotencyNote:   "",
			wholeKeyLookups:   nil,
			conflictKeys:      nil,
			charColumns:       []string{"name", "description", "cover"},
			unboundedChar:     map[string]string{"name": "logic 只校验非空（ErrFolderNameEmpty）；见 README 已知缺口 18", "description": "无任何长度校验；见 README 已知缺口 18", "cover": "无任何长度校验；见 README 已知缺口 18"},
			enumValues:        map[string][]int64{"state": {FolderStateNormal, FolderStateDeleted}, "public": {0, 1}},
			requiredIndexes:   map[string][]string{"idx_mid_state": {"mid", "state"}}, // ListByUser
			timeColumns:       []string{"ctime", "mtime"},
		},
		{
			table:            "favorite_item",
			goFile:           "favorite_item.go",
			row:              FavoriteItem{},
			primaryKey:       []string{"id"},
			uniqueKeys:       map[string][]string{"uniq_mid_oid_tp": {"mid", "oid", "tp"}},
			idempotencyIndex: "uniq_mid_oid_tp",
			idempotencyNote:  "(mid, oid, tp)",
			wholeKeyLookups:  map[string]int{"uniq_mid_oid_tp": 3}, // IsFavored + Del 兜底 + IsFavoreds
			conflictKeys:     []string{"uniq_mid_oid_tp"},
			enumValues:       map[string][]int64{"state": {0, 1}},
			requiredIndexes:  map[string][]string{"idx_mid_fid": {"mid", "fid"}, "idx_oid_tp": {"oid", "tp"}},
			timeColumns:      []string{"ctime", "mtime"},
		},
		{
			table:            "share_log",
			goFile:           "share_log.go",
			row:              ShareLog{},
			primaryKey:       []string{"id"},
			uniqueKeys:       map[string][]string{"uniq_oid_mid_tp_day": {"oid", "mid", "tp", "day"}},
			idempotencyIndex: "uniq_oid_mid_tp_day",
			idempotencyNote:  "(oid, mid, tp, day)",
			wholeKeyLookups:  nil, // 按天幂等靠 INSERT IGNORE 落键（conflictKeys 覆盖），没有整键回读
			conflictKeys:     []string{"uniq_oid_mid_tp_day"},
			unenumeratedUniqEnum: map[string]string{
				"tp": "注释只写「目标类型」，取值枚举只在 rpc 契约（rpc/engagement.proto：2 视频、11 ugv 视频、12 音频）里；见 README 已知缺口 18",
			},
			requiredIndexes: map[string][]string{"idx_oid_tp": {"oid", "tp"}}, // CountByOid
			timeColumns:     []string{"ctime"},
		},
		{
			// share_stat 只有 schema、没有 model（README 已知缺口 13）：AddShare 的分享数是实时
			// COUNT(*) FROM share_log 得来的，这张聚合表无人读写。
			// 这里仍然登记它，是为了让 DDL 侧的结构断言（列注释、NOT NULL、唯一键命名、自述头、
			// 回滚覆盖）照样覆盖它，而不是默默漏出一张「谁都不负责」的表；
			// 写路径类断言（②③④⑤）按 row==nil 跳过，并由 TestModellessTablesAreRegisteredAndStillUnused
			// 反向钉住「model 一旦开始用它就必须补 spec」。
			table:                "share_stat",
			goFile:               "",
			row:                  nil,
			modellessReason:      "有 schema 无 model 实现：README 已知缺口 13，分享数当前由 share_log 实时 COUNT，这张聚合表待接线或改走缓存",
			primaryKey:           []string{"id"},
			uniqueKeys:           map[string][]string{"uniq_oid_tp": {"oid", "tp"}},
			idempotencyIndex:     "",
			idempotencyNote:      "",
			unenumeratedUniqEnum: map[string]string{"tp": "同 share_log.tp：注释未逐值枚举；见 README 已知缺口 18"},
			timeColumns:          []string{"ctime", "mtime"},
		},
		{
			// engagement_outbox 是领域事件 Outbox（000004）：互动写事务内追加事件行，
			// internal/publisher 按 id 升序投递到 engagement.action.v1。
			// 这张表的口径与前面五张业务表不同：它是**投递队列的持久化**，不是业务主数据，
			// 所以没有软删位（DELETE 一律禁止，见 TestSoftDeleteIsTheOnlyRemovalPath）、
			// 没有回读单行的 FindOne（发布器只批量捞待发布），幂等也只到「同一事件不写两行」。
			// (event_id) 只保证「同一事件不重复入库」；「同一用户同一对象只算一次」在
			// thumbup_like/favorite_item/share_log 的唯一键上，本表按状态变化追加行。
			// 该键只有写侧约束、没有任何整键回读（发布器按主键 id 更新状态），
			// 所以 wholeKeyLookups 与 conflictKeys 都是空：与 share_log 同理，登记而不是放行。
			table:            "engagement_outbox",
			goFile:           "engagementoutboxmodel.go",
			row:              EngagementOutbox{},
			primaryKey:       []string{"id"},
			uniqueKeys:       map[string][]string{"uniq_event_id": {"event_id"}},
			idempotencyIndex: "uniq_event_id",
			idempotencyNote:  "(event_id)",
			wholeKeyLookups:  nil,
			conflictKeys:     nil,
			charColumns:      []string{"event_id", "event_type", "aggregate_type", "aggregate_id", "payload", "last_error"},
			charLimits:       map[string]int{"event_id": 26, "event_type": 17, "aggregate_type": 7, "aggregate_id": 19},
			unboundedChar: map[string]string{
				"payload":    "事件信封 JSON 由 repository 现场装配，没有任何字节上限守卫；见 README 已知缺口 20",
				"last_error": "common/outbox 把 broker 错误原文整个写进本列，超 512 会让 MarkRetry 自身失败；见 README 已知缺口 19",
			},
			noDefaultColumns: map[string]string{
				"event_id": "必填且不给 DEFAULT ''：给了默认值会让第二行「漏填」撞 uniq_event_id，报错指向重复投递而不是装配漏字段",
				"payload":  "MEDIUMTEXT 无法给字面量默认值，且空 payload 的事件投出去只会让消费侧死信",
			},
			binaryCollate: map[string]string{
				"event_id": "uniq_event_id 必须逐字节比较：库表默认的 _ci 排序会折叠大小写，让只差大小写的两个 ULID 撞上同一唯一键（静默丢事件）",
			},
			uniqIndexNameExceptions: map[string]string{
				"uniq_event_id": "expectedUniqIndexName 从列名推导会得到 uniq_event，但全仓 event_id 唯一键的既有命名一律是 uniq_event_id，本域不另立口径",
			},
			enumValues: map[string][]int64{"state": {OutboxStatePending, OutboxStatePublished, OutboxStateFailed}},
			requiredIndexes: map[string][]string{
				"idx_state_next_retry": {"state", "next_retry_at"}, // ListPending 的前缀扫描 + LIMIT
			},
			deferredIndexes: map[string]string{
				"idx_event_type_ctime": "没有任何 model 查询按 (event_type, ctime) 读它：积压统计与按类型排障的入口还没实现（README 已知缺口 21），与 video_outbox 同形登记",
			},
			timeColumns: []string{"occurred_at", "ctime", "mtime", "next_retry_at"},
		},
	}
}

// sqlFreeFiles 是 model 目录里不含任何 SQL 的手写文件，逐个登记理由。
// 新增文件时 TestModelFileInventoryIsExact 会要求归类，避免「新加了个 model 文件但没人比对」。
var sqlFreeFiles = map[string]string{
	"errors.go": "只有错误值与状态常量，不含 SQL；状态常量的落库口径由 enumValues 比对",
	"now.go":    "只有 nowUnix 时钟辅助，不含 SQL；墙钟不可注入是 logic 用例的边界",
}

func specFor(table string) *tableSpec {
	specs := tableSpecs()
	for i := range specs {
		if specs[i].table == table {
			return &specs[i]
		}
	}
	return nil
}

func modelBackedSpecs() []tableSpec {
	out := []tableSpec{}
	for _, s := range tableSpecs() {
		if s.row != nil {
			out = append(out, s)
		}
	}
	return out
}

// --- 迁移 SQL 解析 ---

type sqlColumn struct {
	name          string
	dataType      string // 含列名的完整定义段（COMMENT 之前）
	typeExpr      string // 去掉列名后的类型表达式：判定字符列/整数列只看它
	comment       string
	notNull       bool
	hasDefault    bool
	autoIncrement bool
}

type sqlIndex struct {
	name   string
	unique bool
	cols   []string
}

type sqlTable struct {
	table   string
	file    string
	options string
	columns []sqlColumn
	primary []string
	indexes []sqlIndex
}

func (t *sqlTable) column(name string) (sqlColumn, bool) {
	for _, c := range t.columns {
		if c.name == name {
			return c, true
		}
	}
	return sqlColumn{}, false
}

func (t *sqlTable) hasColumn(name string) bool { _, ok := t.column(name); return ok }

func (t *sqlTable) index(name string) (sqlIndex, bool) {
	for _, i := range t.indexes {
		if i.name == name {
			return i, true
		}
	}
	return sqlIndex{}, false
}

func (t *sqlTable) uniqueIndexes() []sqlIndex {
	out := []sqlIndex{}
	for _, i := range t.indexes {
		if i.unique {
			out = append(out, i)
		}
	}
	return out
}

func (t *sqlTable) primaryColumn() (sqlColumn, bool) {
	if len(t.primary) != 1 {
		return sqlColumn{}, false
	}
	return t.column(t.primary[0])
}

func mustTable(t *testing.T, tables map[string]*sqlTable, name string) *sqlTable {
	t.Helper()
	tbl, ok := tables[name]
	if !ok {
		t.Fatalf("迁移里没有建表语句 %s（model 侧登记了它）", name)
	}
	return tbl
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("找不到仓库根（向上没看到 go.mod，起点 %s）", dir)
		}
		dir = parent
	}
}

func findMigrationsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(findRepoRoot(t), "deploy", "migrations", "engagement")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("找不到 %s：迁移文件缺失或目录改名（%v）", migrationsRelDir, err)
	}
	return dir
}

var reCreateTableClaim = regexp.MustCompile(`新增\s*(\d+)\s*张表`)

// parsedMigration 是一份迁移文件的逐行分类结果。
// 可执行行与注释行必须分开：头注释里正常会出现 `DROP TABLE IF EXISTS`（回滚说明）与
// 「(oid, mid, tp, day) 唯一索引」这类复述，任何计数式探针只能在**行首锚定**的可执行行上做，
// 否则会恒差若干、把门禁变成全量假阳性。
type parsedMigration struct {
	path    string
	header  string
	sql     []string
	creates []string // 行首锚定解析出的 CREATE TABLE IF NOT EXISTS 表名
}

func parseMigrationFile(t *testing.T, path string) parsedMigration {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	pf := parsedMigration{path: path}
	var header []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "--"):
			header = append(header, trimmed)
		default:
			pf.sql = append(pf.sql, trimmed)
			if name, ok := createTableName(t, trimmed); ok {
				pf.creates = append(pf.creates, name)
			}
		}
	}
	pf.header = strings.Join(header, "\n")
	return pf
}

// parseCreateTables 抽出目录里所有 CREATE TABLE 块。
// 只认本仓库既有书写风格（反引号标识符、每列一行、) ENGINE=... 收尾）：
// 写法被改坏时立即报错，而不是悄悄解析出一张空表。
func parseCreateTables(t *testing.T, dir string) map[string]*sqlTable {
	t.Helper()
	out := map[string]*sqlTable{}
	for _, p := range parseMigrations(t, dir) {
		base := filepath.Base(p.path)
		var cur *sqlTable
		for _, line := range p.sql {
			switch {
			case cur == nil && strings.HasPrefix(line, "CREATE TABLE"):
				name, ok := createTableName(t, line)
				if !ok {
					t.Fatalf("%s: CREATE TABLE 无法解析表名: %q", base, line)
				}
				cur = &sqlTable{table: name, file: base}
			case cur != nil && strings.HasPrefix(line, ")"):
				if !strings.Contains(line, "ENGINE=InnoDB") || !strings.Contains(line, "utf8mb4") {
					t.Fatalf("%s: 表 %s 收尾行必须显式 ENGINE=InnoDB 与 CHARSET=utf8mb4，得到 %q",
						base, cur.table, line)
				}
				cur.options = line
				if _, dup := out[cur.table]; dup {
					t.Fatalf("%s: 表 %s 被重复创建", base, cur.table)
				}
				out[cur.table] = cur
				cur = nil
			case cur != nil:
				parseTableLine(t, cur, base, line)
			default:
				t.Fatalf("%s: 出现 CREATE TABLE 之外的可执行语句 %q —— 本目录只做建表，"+
					"ALTER/回填必须另起文件并评估锁窗口", base, line)
			}
		}
		if cur != nil {
			t.Fatalf("%s: CREATE TABLE 块没有收尾的 ) 行", base)
		}
	}
	return out
}

func parseMigrations(t *testing.T, dir string) []parsedMigration {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("枚举 %s 失败: %v", dir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("%s 下没有任何 .sql —— 迁移缺失", migrationsRelDir)
	}
	sort.Strings(paths)
	out := make([]parsedMigration, 0, len(paths))
	for _, p := range paths {
		out = append(out, parseMigrationFile(t, p))
	}
	return out
}

const createTablePrefix = "CREATE TABLE IF NOT EXISTS "

// createTableName 只认**行首**的 CREATE TABLE，并顺手钉住可重复执行写法：
// 不做全文件子串搜索，因此不会被头注释里的复述干扰。
func createTableName(t *testing.T, line string) (string, bool) {
	if !strings.HasPrefix(line, "CREATE TABLE ") {
		return "", false
	}
	if !strings.HasPrefix(line, createTablePrefix) {
		t.Fatalf("表必须用 CREATE TABLE IF NOT EXISTS 声明，迁移要可重复执行: %q", line)
	}
	return backtickedIdentifier(line[len(createTablePrefix):])
}

func parseTableLine(t *testing.T, cur *sqlTable, file, line string) {
	t.Helper()
	body := strings.TrimSuffix(strings.TrimSpace(line), ",")
	switch {
	case strings.HasPrefix(body, "`"):
		name, ok := backtickedIdentifier(body)
		if !ok {
			t.Fatalf("%s: 列定义无法解析: %q", file, line)
		}
		if cur.hasColumn(name) {
			t.Fatalf("%s: 列 %s 重复声明", file, name)
		}
		if name != strings.ToLower(name) {
			t.Fatalf("%s: 列名 %s 不是全小写 —— 本文件的清单按小写归一比对，混用会漏比", file, name)
		}
		// 只在 COMMENT 之前的定义段上判定 NOT NULL / DEFAULT / 类型：
		// 注释里的中文（「0 取消、1 like」）不能影响结构判定。
		defined, comment := body, ""
		if i := strings.Index(body, " COMMENT '"); i >= 0 {
			defined = body[:i]
			comment = strings.TrimSuffix(body[i+len(" COMMENT '"):], "'")
		}
		if comment == "" {
			t.Fatalf("%s: 表 %s 的列 %s 缺少 COMMENT —— 计数字段与状态位的取值口径全靠注释表达",
				file, cur.table, name)
		}
		typeExpr := strings.TrimSpace(defined[len("`"+name+"`"):])
		cur.columns = append(cur.columns, sqlColumn{
			name:          name,
			dataType:      defined,
			typeExpr:      typeExpr,
			comment:       comment,
			notNull:       strings.Contains(defined, " NOT NULL"),
			hasDefault:    strings.Contains(defined, " DEFAULT "),
			autoIncrement: strings.Contains(defined, " AUTO_INCREMENT"),
		})
	case strings.HasPrefix(body, "PRIMARY KEY"):
		if len(cur.primary) > 0 {
			t.Fatalf("%s: 表 %s 声明了多次 PRIMARY KEY", file, cur.table)
		}
		cur.primary = columnListInParentheses(t, file, body)
	case strings.HasPrefix(body, "UNIQUE KEY"):
		name, _ := identifierAfterKeyword(body, "UNIQUE KEY")
		cur.addIndex(t, file, name, true, columnListInParentheses(t, file, body))
	case strings.HasPrefix(body, "KEY"):
		name, _ := identifierAfterKeyword(body, "KEY")
		cur.addIndex(t, file, name, false, columnListInParentheses(t, file, body))
	default:
		t.Fatalf("%s: 表 %s 内出现无法识别的定义行（FOREIGN KEY / CHECK / 生成列不符合本服务约定）: %q",
			file, cur.table, line)
	}
}

func (t *sqlTable) addIndex(tt *testing.T, file, name string, unique bool, cols []string) {
	tt.Helper()
	if name == "" {
		tt.Fatalf("%s: 索引缺名字: %v", file, cols)
	}
	if _, dup := t.index(name); dup {
		tt.Fatalf("%s: 索引名 %s 重复声明", file, name)
	}
	if len(cols) == 0 {
		tt.Fatalf("%s: 索引 %s 没有列", file, name)
	}
	for _, c := range cols {
		if !t.hasColumn(c) {
			tt.Fatalf("%s: 索引 %s 引用了未声明的列 %s", file, name, c)
		}
	}
	t.indexes = append(t.indexes, sqlIndex{name: name, unique: unique, cols: cols})
}

var (
	charWidthRe    = regexp.MustCompile(`^(?:VAR)?CHAR\((\d+)\)`)
	collateRe      = regexp.MustCompile(`COLLATE\s+([A-Z0-9_]+)`)
	baseTypeRe     = regexp.MustCompile(`^(BIGINT|MEDIUMINT|SMALLINT|TINYINT|INT|VARCHAR|CHAR|TEXT|MEDIUMTEXT|LONGTEXT|BLOB|JSON|DECIMAL|NUMERIC|DOUBLE|FLOAT|DATETIME|TIMESTAMP|DATE|BIT)\b`)
	numericTokenRe = regexp.MustCompile(`\d+`)
)

// integerWidthBits 返回整数列的可表示位宽；非整数列返回 0。
func (c sqlColumn) integerWidthBits() int {
	switch c.baseType() {
	case "TINYINT":
		return 8
	case "SMALLINT":
		return 16
	case "MEDIUMINT":
		return 24
	case "INT":
		return 32
	case "BIGINT":
		return 64
	}
	return 0
}

// baseType 只看「列名之后的类型表达式」的第一段，且用词首锚定的词表匹配。
// 对整行做 SET/CHAR/TEXT 子串检索会被列名与修饰词假命中（`public` TINYINT、`count` INT
// 这类列会被误判成字符列），所以这里既不整行匹配、也不 Contains 匹配。
func (c sqlColumn) baseType() string {
	m := baseTypeRe.FindStringSubmatch(strings.ToUpper(c.typeExpr))
	if m == nil {
		return ""
	}
	return m[1]
}

func (c sqlColumn) isCharType() bool {
	switch c.baseType() {
	case "VARCHAR", "CHAR", "TEXT", "MEDIUMTEXT", "LONGTEXT", "ENUM", "SET":
		return true
	}
	return false
}

// charWidth 返回定长字符列的可容纳字符数，VARCHAR(n) 与 CHAR(n) 都算。
// outbox 的 event_id 是 `CHAR(26)`（ULID 定长，用 CHAR 让「长度不是 26」在 DDL 里就可见），
// 所以长度比对不能只认 VARCHAR，否则该列的长度口径会退回「无法比对」的空转状态。
func (c sqlColumn) charWidth() (int, bool) {
	m := charWidthRe.FindStringSubmatch(strings.ToUpper(c.typeExpr))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

func (c sqlColumn) commentNumbers() map[string]bool {
	return lowerSet(numericTokenRe.FindAllString(c.comment, -1))
}

// declaredCollate 取列定义里显式声明的排序规则（`COLLATE utf8mb4_bin`）。
// 只在 COMMENT 之前的定义段上找：注释里的中文不能影响结构判定。
func (c sqlColumn) declaredCollate() (string, bool) {
	m := collateRe.FindStringSubmatch(strings.ToUpper(c.typeExpr))
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// inAnyUniqueKey 判断列是否参与该表的任意一个 UNIQUE KEY（binaryCollate 豁免的前提）。
func inAnyUniqueKey(tbl *sqlTable, column string) bool {
	for _, idx := range tbl.uniqueIndexes() {
		for _, c := range idx.cols {
			if c == column {
				return true
			}
		}
	}
	return false
}

func backtickedIdentifier(line string) (string, bool) {
	start := strings.Index(line, "`")
	if start < 0 {
		return "", false
	}
	rest := line[start+1:]
	end := strings.Index(rest, "`")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

func identifierAfterKeyword(line, keyword string) (string, bool) {
	idx := strings.Index(line, keyword)
	if idx < 0 {
		return "", false
	}
	return backtickedIdentifier(line[idx+len(keyword):])
}

func columnListInParentheses(t *testing.T, file, line string) []string {
	t.Helper()
	open := strings.LastIndex(line, "(")
	closing := strings.LastIndex(line, ")")
	if open < 0 || closing < 0 || closing < open {
		t.Fatalf("%s: 索引定义缺括号: %q", file, line)
	}
	parts := strings.Split(line[open+1:closing], ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		name, ok := backtickedIdentifier(strings.TrimSpace(p))
		if !ok {
			t.Fatalf("%s: 索引列必须是反引号标识符（带前缀长度的写法无法比对，请改写）: %q", file, p)
		}
		out = append(out, strings.ToLower(name))
	}
	return out
}

// --- 反射侧 ---

// structColumns 按字段声明顺序取归一（小写）后的 db tag 列名。
func structColumns(t *testing.T, row any) []string {
	t.Helper()
	v := reflect.TypeOf(row)
	if v == nil || v.Kind() != reflect.Struct {
		t.Fatalf("%T 不是结构体", row)
	}
	out := make([]string, 0, v.NumField())
	seen := map[string]bool{}
	for i := 0; i < v.NumField(); i++ {
		tag := v.Field(i).Tag.Get("db")
		if tag == "" || tag == "-" {
			t.Fatalf("%s 的字段 %s 缺少 db tag —— sqlx 按 tag 扫描，缺 tag 就是读不到值",
				v.Name(), v.Field(i).Name)
		}
		col := strings.ToLower(strings.Split(tag, ",")[0])
		if col == "" {
			t.Fatalf("%s 的字段 %s 的 db tag 为空: %q", v.Name(), v.Field(i).Name, tag)
		}
		if seen[col] {
			t.Fatalf("%s 的 db tag %s 重复", v.Name(), col)
		}
		seen[col] = true
		out = append(out, col)
	}
	return out
}

// structFieldKinds 给列类型比对用：列名 -> Go 基础类型名（int64/int32/string/...）。
func structFieldKinds(t *testing.T, row any) map[string]string {
	t.Helper()
	cols := structColumns(t, row)
	v := reflect.TypeOf(row)
	byCol := map[string]string{}
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		col := strings.ToLower(strings.Split(f.Tag.Get("db"), ",")[0])
		typ := f.Type
		if k := typ.Kind(); k == reflect.Pointer || k == reflect.Slice {
			// sql.NullInt64 之类的可空包装本服务没用；真用了，可空性断言会另行报。
			typ = typ.Elem()
		}
		byCol[col] = typ.String()
	}
	out := map[string]string{}
	for _, c := range cols {
		out[c] = byCol[c]
	}
	return out
}

// --- model 源码里的 SQL 语句抽取（go/ast，不是子串搜索）---

// modelStmt 是一条从 model 源码里解析出来的 SQL 语句。
// 必须走 AST：本服务的 SQL 写在 ExecCtx/QueryRowCtx 调用的字符串字面量里
// （有的先 `query := "..."` 再传变量），对源码做子串比对会在跨行拼接处落空，
// 也拿不到「占位符数 vs 实参数」这层关系。
type modelStmt struct {
	file         string
	caller       string
	sql          string
	verb         string // INSERT / INSERT IGNORE / UPDATE / SELECT / DELETE
	table        string
	insertCols   []string
	insertValues []string
	setCols      map[string]bool
	pins         map[string]bool // 等值/IN 定位列（整键判定用）
	literals     map[string][]int64
	selectCols   []string
	starSelect   bool
	odku         bool
	ignore       bool
	placeholders int
	args         int
	variadic     bool
}

func (s modelStmt) where() string {
	return fmt.Sprintf("%s.%s", strings.TrimSuffix(s.file, ".go"), s.caller)
}

func (s modelStmt) short() string {
	const max = 140
	if len(s.sql) > max {
		return s.sql[:max] + "…"
	}
	return s.sql
}

// sqlLitFiles 是 model 目录里允许出现 SQL 的文件（= 各 spec 的 goFile）。
func sqlLitFiles() []string {
	out := []string{}
	for _, s := range tableSpecs() {
		if s.goFile != "" {
			out = append(out, s.goFile)
		}
	}
	sort.Strings(out)
	return out
}

func collectModelStatements(t *testing.T) []modelStmt {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	fset := token.NewFileSet()
	var out []modelStmt
	for _, name := range sqlLitFiles() {
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("解析 model 源文件 %s 失败（语法被改坏时本门禁无法工作）: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			vars := localStringVars(fn)
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				sqlIdx, isQuery := sqlArgIndex(sel.Sel.Name)
				if !isQuery {
					return true
				}
				if len(call.Args) <= sqlIdx {
					t.Fatalf("%s.%s: %s 的实参不足以取出 SQL 文本", name, fn.Name.Name, sel.Sel.Name)
				}
				texts := stringCandidates(call.Args[sqlIdx], vars)
				if len(texts) == 0 {
					t.Fatalf("%s.%s: 传给 %s 的 SQL 不是字符串字面量（可能是运行时拼接）——"+
						"本门禁读不到 SQL 文本就会失去信号，请改成字面量或字面量拼接",
						name, fn.Name.Name, sel.Sel.Name)
				}
				for _, text := range texts {
					st := parseSQLStmt(t, name, fn.Name.Name, text)
					st.args = len(call.Args) - sqlIdx - 1
					st.variadic = call.Ellipsis.IsValid()
					out = append(out, st)
				}
				return true
			})
		}
	}
	const minStatements = 20
	if len(out) < minStatements {
		t.Fatalf("只从 model 源码里解析到 %d 条 SQL 语句，期望至少 %d 条："+
			"解析器失效或书写格式变了，整套比对会静默空转", len(out), minStatements)
	}
	return out
}

// sqlArgIndex 认 sqlx.SqlConn 上这几个方法，返回 SQL 文本所在实参下标。
func sqlArgIndex(method string) (int, bool) {
	switch method {
	case "Exec", "ExecCtx":
		return 1, true
	case "QueryRow", "QueryRowCtx", "QueryRows", "QueryRowsCtx":
		return 2, true
	}
	return 0, false
}

// localStringVars 收集函数内 `name := "字面量"` / `name = "字面量"` 的全部候选值。
// 同一变量被赋值多条 SQL（如 ListByUser 的两个分支）时每条都要参与比对，
// 只比对其中一条会让门禁变成永真。
func localStringVars(fn *ast.FuncDecl) map[string][]string {
	vars := map[string][]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		for _, v := range stringCandidates(as.Rhs[0], vars) {
			vars[id.Name] = appendUnique(vars[id.Name], v)
		}
		return true
	})
	return vars
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

// stringCandidates 把一个表达式还原成它可能取到的全部字符串值。
func stringCandidates(expr ast.Expr, vars map[string][]string) []string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return nil
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return nil
		}
		return []string{v}
	case *ast.ParenExpr:
		return stringCandidates(e.X, vars)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return nil
		}
		var out []string
		for _, a := range stringCandidates(e.X, vars) {
			for _, b := range stringCandidates(e.Y, vars) {
				out = append(out, a+b)
			}
		}
		return out
	case *ast.Ident:
		return vars[e.Name]
	}
	return nil
}

var (
	reInsertTok  = regexp.MustCompile(`(?i)^INSERT\s+(?:(IGNORE)\s+)?INTO\s+([a-z_][a-z0-9_]*)\s*\(([^)]*)\)\s*VALUES\s*\(([^)]*)\)\s*(.*)$`)
	reUpdateTok  = regexp.MustCompile(`(?i)^UPDATE\s+([a-z_][a-z0-9_]*)\s+SET\s+(.*)$`)
	reSelectTok  = regexp.MustCompile(`(?i)^SELECT\s+(.+?)\s+FROM\s+([a-z_][a-z0-9_]*)(?:\s+WHERE\s+(.*))?$`)
	reDeleteTok  = regexp.MustCompile(`(?i)^DELETE\s+FROM\s+([a-z_][a-z0-9_]*)(?:\s+WHERE\s+(.*))?$`)
	rePinTok     = regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*)\s*(?:=\s*(?:\?|\d+)|IN\s*\(\s*\?\s*\))`)
	reLitNumTok  = regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*)\s*=\s*(\d+)\b`)
	reAssignHead = regexp.MustCompile(`(?i)^\s*([a-z_][a-z0-9_]*)\s*=`)
	reFuncCall   = regexp.MustCompile(`(?i)^([a-z_][a-z0-9_]*)\((.*)\)$`)
	reIdent      = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)
)

const odkuKeyword = "ON DUPLICATE KEY UPDATE"

// parseSQLStmt 把一条 SQL 文本拆成可比对的结构。解析失败一定报错：
// 静默跳过等于该语句永久脱离门禁，而这正是本文件要防的事。
func parseSQLStmt(t *testing.T, file, caller, raw string) modelStmt {
	t.Helper()
	sqlText := strings.Join(strings.Fields(raw), " ")
	st := modelStmt{
		file:         file,
		caller:       caller,
		sql:          sqlText,
		setCols:      map[string]bool{},
		pins:         map[string]bool{},
		literals:     map[string][]int64{},
		placeholders: strings.Count(sqlText, "?"),
	}
	head := strings.ToUpper(strings.SplitN(sqlText, " ", 2)[0])
	switch head {
	case "INSERT":
		m := reInsertTok.FindStringSubmatch(sqlText)
		if m == nil {
			t.Fatalf("%s.%s: INSERT 无法解析（本服务的 INSERT 必须是「INSERT [IGNORE] INTO t (列…) VALUES (…)」"+
				"的完整字面量，列清单与 VALUES 里都不允许出现括号）: %q", file, caller, sqlText)
		}
		st.ignore = strings.EqualFold(m[1], "IGNORE")
		st.verb = "INSERT"
		if st.ignore {
			st.verb = "INSERT IGNORE"
		}
		st.table = strings.ToLower(m[2])
		st.insertCols = splitIdentList(t, file, caller, m[3])
		st.insertValues = splitIdentList(t, file, caller, m[4])
		if tail := strings.ToUpper(m[5]); strings.Contains(tail, odkuKeyword) {
			st.odku = true
			i := strings.Index(tail, odkuKeyword)
			st.setCols = assignTargets(m[5][i+len(odkuKeyword):])
		}
	case "UPDATE":
		m := reUpdateTok.FindStringSubmatch(sqlText)
		if m == nil {
			t.Fatalf("%s.%s: UPDATE 无法解析: %q", file, caller, sqlText)
		}
		st.verb = "UPDATE"
		st.table = strings.ToLower(m[1])
		setText, whereText := splitAtKeyword(m[2], "WHERE")
		st.setCols = assignTargets(setText)
		st.pins = pinColumns(whereText)
		st.literals = literalValues(setText, whereText)
	case "SELECT":
		m := reSelectTok.FindStringSubmatch(sqlText)
		if m == nil {
			t.Fatalf("%s.%s: SELECT 无法解析（列清单必须是字面量里的显式列名；若改成拼接变量或子查询，"+
				"本门禁就读不到 SQL 了）: %q", file, caller, sqlText)
		}
		st.verb = "SELECT"
		proj := m[1]
		st.table = strings.ToLower(m[2])
		st.pins = pinColumns(m[3])
		st.literals = literalValues(m[3])
		if strings.TrimSpace(proj) == "*" {
			st.starSelect = true
		}
		st.selectCols = splitProjectionList(t, file, caller, proj)
	case "DELETE":
		m := reDeleteTok.FindStringSubmatch(sqlText)
		if m == nil {
			t.Fatalf("%s.%s: DELETE 无法解析: %q", file, caller, sqlText)
		}
		st.verb = "DELETE"
		st.table = strings.ToLower(m[1])
		st.pins = pinColumns(m[2])
		st.literals = literalValues(m[2])
	default:
		t.Fatalf("%s.%s: 传给 sqlx 的语句以 %q 开头，不在本文件识别范围内 —— "+
			"要么改成显式 SQL，要么在这里补解析分支并写清它比对了什么", file, caller, head)
	}
	if st.table == "" {
		t.Fatalf("%s.%s: 取不到目标表名: %q", file, caller, sqlText)
	}
	// VALUES 里的位置字面量也要归到列上（第 i 项对应列清单第 i 项）：
	// favorite_folder 的 INSERT 把 state 钉成 0，这类「不传参、靠 SQL 写死」的值同样要在注释里交代。
	for i, v := range st.insertValues {
		if i >= len(st.insertCols) {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			col := st.insertCols[i]
			st.literals[col] = appendUniqueInt(st.literals[col], n)
		}
	}
	return st
}

func splitIdentList(t *testing.T, file, caller, list string) []string {
	t.Helper()
	list = strings.TrimSpace(list)
	if list == "" {
		return nil
	}
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		item := strings.TrimSpace(p)
		if item == "" {
			t.Fatalf("%s.%s: SQL 列表里有空项: %q", file, caller, list)
		}
		out = append(out, strings.ToLower(item))
	}
	return out
}

// splitProjectionList 解析 SELECT 的投影清单。
// 聚合写法（COUNT(*)、SUM(oid)）不是列名：`*` 直接跳过（另有 SELECT * 的独立断言），
// 括号里的列名照样要参与 ③ 的比对，否则「SELECT COUNT(oid)」里的错列名会漏网。
func splitProjectionList(t *testing.T, file, caller, list string) []string {
	t.Helper()
	items := splitIdentList(t, file, caller, list)
	out := []string{}
	for _, item := range items {
		if item == "*" {
			continue
		}
		if m := reFuncCall.FindStringSubmatch(item); m != nil {
			inner := splitIdentList(t, file, caller, m[2])
			for _, c := range inner {
				if c != "*" {
					out = append(out, c)
				}
			}
			continue
		}
		out = append(out, item)
	}
	return out
}

// splitAtKeyword 在第一个独立的顶层关键字处切开（本服务的 SQL 没有子查询，够用）。
func splitAtKeyword(s, keyword string) (before, after string) {
	needle := " " + strings.ToUpper(keyword) + " "
	i := strings.Index(strings.ToUpper(s), needle)
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+len(needle):]
}

func assignTargets(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		m := reAssignHead.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		out[strings.ToLower(m[1])] = true
	}
	return out
}

func pinColumns(whereText string) map[string]bool {
	out := map[string]bool{}
	for _, m := range rePinTok.FindAllStringSubmatch(whereText, -1) {
		out[strings.ToLower(m[1])] = true
	}
	return out
}

func literalValues(parts ...string) map[string][]int64 {
	out := map[string][]int64{}
	for _, p := range parts {
		for _, m := range reLitNumTok.FindAllStringSubmatch(p, -1) {
			n, err := strconv.ParseInt(m[2], 10, 64)
			if err != nil {
				continue
			}
			col := strings.ToLower(m[1])
			out[col] = appendUniqueInt(out[col], n)
		}
	}
	return out
}

func appendUniqueInt(list []int64, v int64) []int64 {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

// sqlNoiseWords 是 SQL 关键字与函数名：列名比对应剔除它们，但**不含**任何列名，
// 所以拼错成 `type` 这种「看起来像关键字其实是漂移」的名字不会被误剔。
var sqlNoiseWords = lowerSet([]string{
	"insert", "ignore", "into", "values", "update", "delete", "from", "select", "set", "where",
	"and", "or", "not", "null", "on", "duplicate", "key", "order", "by", "group", "limit", "offset",
	"asc", "desc", "in", "is", "count", "sum", "max", "min", "if", "greatest", "least", "as",
	"join", "left", "inner", "having", "distinct", "for", "like", "between",
})

// columnTokens 取一段 SQL 文本里所有可能是列名的标识符（归一小写）。
func columnTokens(fragment string) []string {
	fragment = rePinTok.ReplaceAllString(fragment, " ") // 已单独比对过，剔除以免重复报错
	out := []string{}
	for _, tok := range reIdent.FindAllString(fragment, -1) {
		if sqlNoiseWords[strings.ToLower(tok)] {
			continue
		}
		out = append(out, strings.ToLower(tok))
	}
	return out
}

// --- 断言 ---

// TestEveryTableSpecIsBackedByMigration 方向①：spec ↔ 迁移，两个方向都要闭合。
func TestEveryTableSpecIsBackedByMigration(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		if _, ok := tables[spec.table]; !ok {
			t.Errorf("迁移里没有建表语句 %s（model 侧登记了它）", spec.table)
		}
	}
	for _, name := range sortedTableNames(tables) {
		if specFor(name) == nil {
			t.Errorf("迁移里的表 %s 没有任何 tableSpec 登记 —— 要么补 spec（含幂等键与字符列清单），"+
				"要么删掉这张谁都不负责的表", name)
		}
	}
	if len(tables) != len(tableSpecs()) {
		t.Errorf("迁移解析出 %d 张表、spec 登记 %d 张，数量不符就有表脱离门禁",
			len(tables), len(tableSpecs()))
	}
}

// TestModelSQLTablesAreDeclaredInMigrations 方向①另一半：SQL 里写到的表必须存在，
// 且每张有 model 实现的表都必须真被 SQL 用到（结构体存在但没人查 = 死代码）。
func TestModelSQLTablesAreDeclaredInMigrations(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	seen := map[string]int{}
	for _, s := range stmts {
		seen[s.table]++
		if _, ok := tables[s.table]; !ok {
			t.Errorf("%s: %s 的 SQL 引用了迁移里没有的表 %s（%q）", s.where(), s.verb, s.table, s.short())
		}
		if specFor(s.table) == nil {
			t.Errorf("%s: 表 %s 没有登记 tableSpec，本门禁无法比对它的列", s.where(), s.table)
		}
	}
	for _, spec := range modelBackedSpecs() {
		if seen[spec.table] == 0 {
			t.Errorf("model 里有 %T 却没有一条 SQL 指向 %s", spec.row, spec.table)
		}
	}
	if len(seen) == 0 {
		t.Fatal("一条 SQL 都没扫到，本用例失去意义")
	}
}

// TestModellessTablesAreRegisteredAndStillUnused 处理「有 schema 无 model」的表（share_stat）。
// 不默默漏掉：必须登记理由（且理由指向 README 缺口编号），同时确认 model 真的没在用它；
// 一旦有人补了 SQL，本用例反向变红，要求按其它表的口径补全 spec。
func TestModellessTablesAreRegisteredAndStillUnused(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	registered := 0
	for _, spec := range tableSpecs() {
		if spec.row != nil {
			if spec.modellessReason != "" {
				t.Errorf("%s: 有 model 实现却登记了 modellessReason", spec.table)
			}
			continue
		}
		registered++
		if _, ok := tables[spec.table]; !ok {
			t.Errorf("%s 登记成「只有 schema」，但迁移里已经没有这张表了 —— 登记要一起删", spec.table)
		}
		if spec.modellessReason == "" || !strings.Contains(spec.modellessReason, "README") {
			t.Errorf("%s 的 modellessReason 必须写明理由并指向 README 已知缺口，得到 %q",
				spec.table, spec.modellessReason)
		}
		for _, s := range stmts {
			if s.table == spec.table {
				t.Errorf("%s（%s）: model 开始读写 %s 了，但它的 spec 还是 row=nil —— "+
					"请补结构体/goFile/幂等元组，并删掉 modellessReason：%q",
					s.where(), s.verb, spec.table, spec.modellessReason)
			}
		}
	}
	if registered == 0 {
		t.Error("没有任何「有 schema 无 model」的登记：要么这张表已经补齐实现（那 DDL 侧断言要一起补），" +
			"要么解析口径变了")
	}
}

// TestModelFileInventoryIsExact model 目录里每个非测试文件都要归类，
// 否则「新加一个 model 文件」就等于「新加一张没人比对的表」。
func TestModelFileInventoryIsExact(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("枚举 model/*.go 失败: %v", err)
	}
	sqlFiles := lowerSet(sqlLitFiles())
	got := []string{}
	for _, p := range paths {
		base := filepath.Base(p)
		if strings.HasSuffix(base, "_test.go") {
			continue
		}
		got = append(got, base)
		if sqlFiles[base] {
			continue
		}
		reason, ok := sqlFreeFiles[base]
		if !ok {
			t.Errorf("model 文件 %s 既不是任何表的 goFile，也没在 sqlFreeFiles 里登记理由", base)
			continue
		}
		if reason == "" {
			t.Errorf("model 文件 %s 在 sqlFreeFiles 里没写理由", base)
		}
	}
	sort.Strings(got)
	for _, name := range sqlLitFiles() {
		if !contains(got, name) {
			t.Errorf("spec 登记的 model 文件 %s 在磁盘上不存在", name)
		}
	}
	if len(got) != len(sqlFiles)+len(sqlFreeFiles) {
		t.Errorf("model 目录里有 %d 个非测试文件（%v），登记了 %d 个 —— 数量对不上就有文件脱离门禁",
			len(got), got, len(sqlFiles)+len(sqlFreeFiles))
	}
}

// TestModelColumnsMatchMigrationColumns 方向②：db tag ↔ DDL 列名逐一对应，外加可空性。
func TestModelColumnsMatchMigrationColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	registeredNoDefault := 0
	for _, spec := range modelBackedSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			fields := structColumns(t, spec.row)
			seen := map[string]bool{}
			for _, f := range fields {
				seen[f] = true
				if !tbl.hasColumn(f) {
					t.Errorf("结构体列 %s 在建表语句里不存在 —— 查询会报 Unknown column（缺口 1 的形状）。"+
						"DDL 实际列：%v", f, columnNames(tbl))
				}
			}
			for _, c := range tbl.columns {
				if !seen[c.name] {
					t.Errorf("SQL 列 %s 没有结构体字段承载 —— 该列永远不会被读写，请删除或补 model", c.name)
				}
			}
			if len(fields) != len(tbl.columns) {
				t.Errorf("列数不符：结构体 %d 列，SQL %d 列", len(fields), len(tbl.columns))
			}
			for _, c := range tbl.columns {
				if !c.notNull {
					t.Errorf("%s.%s 允许 NULL —— model 用普通结构体扫描（无 sql.Null*），读到 NULL 会直接失败",
						spec.table, c.name)
				}
				if c.notNull && !c.hasDefault && !c.autoIncrement {
					// 必填列豁免按列登记：理由必须写清「为什么宁可让漏填报错，也不给默认值」，
					// 且一旦有人补了 DEFAULT 这条登记就会反向报错（见下方闭合检查）。
					reason, exempt := spec.noDefaultColumns[c.name]
					if !exempt {
						t.Errorf("%s.%s: NOT NULL 且无 DEFAULT，又不是自增主键：%q —— 局部更新/漏传会直接报错，"+
							"要么在 noDefaultColumns 里登记理由，要么补 DEFAULT", spec.table, c.name, c.typeExpr)
						continue
					}
					if reason == "" {
						t.Errorf("%s.%s: noDefaultColumns 登记了却没写理由", spec.table, c.name)
					}
					registeredNoDefault++
				}
			}
			for name := range spec.noDefaultColumns {
				c, ok := tbl.column(name)
				if !ok {
					t.Errorf("%s: noDefaultColumns 登记的 %s 已不在 DDL 里，登记要一起删", spec.table, name)
					continue
				}
				if c.hasDefault || c.autoIncrement || !c.notNull {
					t.Errorf("%s.%s: 登记成「必填无默认值」，但 DDL 现在是 %q（notNull=%v default=%v auto=%v）；"+
						"口径变了登记就该删，否则豁免会替一个已经不存在的风险担保",
						spec.table, name, c.typeExpr, c.notNull, c.hasDefault, c.autoIncrement)
				}
				if !c.isCharType() {
					t.Errorf("%s.%s: 无默认值的必填列应当是字符列（漏填才报得出列名）；整数列给 0 默认值更安全",
						spec.table, name)
				}
			}
		})
	}
	// 豁免清单非空且解析器确实看到了这些列，否则上面那段闭合检查是空转。
	if registeredNoDefault == 0 {
		t.Error("没有任何「NOT NULL 无 DEFAULT」的登记：要么所有必填列都有默认值（那豁免该删），" +
			"要么无默认值列脱离了本门禁")
	}
}

// TestColumnTypesMatchGoFieldWidth 整数位宽必须与 Go 字段宽度匹配：
// int32 字段落在 BIGINT 列上会静默截断写入值，int64 字段落在 INT 列上会在超过 2^31 时报错。
// 两种都不是内存替身能发现的方向（替身没有宽度）。
func TestColumnTypesMatchGoFieldWidth(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range modelBackedSpecs() {
		tbl := mustTable(t, tables, spec.table)
		kinds := structFieldKinds(t, spec.row)
		for _, c := range tbl.columns {
			goKind, ok := kinds[c.name]
			if !ok {
				continue
			}
			switch goKind {
			case "int64":
				checked++
				if c.baseType() != "BIGINT" {
					t.Errorf("%s.%s: Go 字段是 int64 但列是 %s（%q）—— 位宽不匹配，"+
						"写入超过 %d 的值会直接报错", spec.table, c.name, c.baseType(), c.typeExpr, int64(1)<<31-1)
				}
			case "int32":
				checked++
				if width := c.integerWidthBits(); width == 0 || width > 32 {
					t.Errorf("%s.%s: Go 字段是 int32 但列是 %s（%q）—— 列可表示范围大于字段，写满即截断",
						spec.table, c.name, c.baseType(), c.typeExpr)
				}
			case "string":
				checked++
				if !c.isCharType() {
					t.Errorf("%s.%s: Go 字段是 string 但列是 %s（%q）—— 扫描方向不匹配",
						spec.table, c.name, c.baseType(), c.typeExpr)
				}
			default:
				t.Errorf("%s.%s: Go 字段类型 %q 不在本服务 model 的允许集合（int64/int32/string）—— "+
					"新类型要同步补宽度判定，否则该列脱离本门禁", spec.table, c.name, goKind)
			}
		}
	}
	if checked < 50 {
		t.Fatalf("只比对了 %d 个列的 Go/SQL 宽度，期望至少 50（6 张 model 表共 58 列）："+
			"列名或字段类型被改动会让本用例失去信号", checked)
	}
}

// TestHandWrittenSQLColumnsExistInMigration ③：model 手写 SQL 里的每个列名都必须存在于 DDL。
// 这条就是缺口 1（share_log 的 type vs tp）的机器版防线。五个位置全部要过：
// INSERT 列清单、SET/ODKU 目标列、WHERE 定位列、SELECT 投影，以及把前三者与表名、
// 关键字都剔掉之后仍然剩下的裸标识符（覆盖 ORDER BY / LIMIT 这类尾巴）。
func TestHandWrittenSQLColumnsExistInMigration(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	checked := 0
	for _, s := range stmts {
		tbl, ok := tables[s.table]
		if !ok {
			continue // 表不存在的错在 TestModelSQLTablesAreDeclaredInMigrations 里报
		}
		explicit := append([]string{}, s.insertCols...)
		explicit = append(explicit, s.selectCols...)
		explicit = append(explicit, sortedKeysBool(s.setCols)...)
		explicit = append(explicit, sortedKeysBool(s.pins)...)
		// 前三类已经逐个比对过；把它们与表名从整条语句里按**整词**挖掉，
		// 剩下的标识符必须仍然是列名（否则就是没人比过的角落）。
		residual := s.sql
		for _, known := range append([]string{s.table}, explicit...) {
			residual = removeWordCaseInsensitively(residual, known)
		}
		explicit = append(explicit, columnTokens(residual)...)

		for _, tok := range explicit {
			if tok == "" {
				continue
			}
			checked++
			if !tbl.hasColumn(tok) {
				t.Errorf("%s: %s %s 的 SQL 里引用了不存在的列 %q（DDL 实际列：%v）\n  语句：%q",
					s.where(), s.verb, s.table, tok, columnNames(tbl), s.short())
			}
		}
	}
	if checked < 100 {
		t.Fatalf("只比对了 %d 处 SQL 列名引用，期望至少 100：解析器退化会让本门禁失去信号", checked)
	}
}

// TestInsertColumnListArityAndPlaceholders INSERT 列数 = VALUES 项数、
// 占位符数 = 实参数：列清单加了/删了但 Go 参数没跟着改，是这类漂移最常见的第二形态。
func TestInsertColumnListArityAndPlaceholders(t *testing.T) {
	stmts := collectModelStatements(t)
	inserts := 0
	for _, s := range stmts {
		if len(s.insertCols) == 0 {
			continue
		}
		inserts++
		if len(s.insertCols) != len(s.insertValues) {
			t.Errorf("%s: INSERT %s 的列清单 %d 项、VALUES %d 项 —— 位置会整体错位一格：%q",
				s.where(), s.table, len(s.insertCols), len(s.insertValues), s.short())
		}
	}
	const wantInserts = 6 // 6 张有 model 的表各一个写入口（outbox 的 Insert 是普通 INSERT，不计入 wantUpserts）
	if inserts != wantInserts {
		t.Errorf("只扫到 %d 条 INSERT，期望 %d 条：写入口增减时本用例要一起改，别让它静默缩水",
			inserts, wantInserts)
	}
	nonVariadic := 0
	for _, s := range stmts {
		if s.variadic {
			continue // `args...` 的长度由调用方构造（IN 列表展开），这里比不了
		}
		nonVariadic++
		if s.placeholders != s.args {
			t.Errorf("%s: %s 里有 %d 个 ? 但传了 %d 个实参 —— 绑定会错位或直接报参数个数不符：%q",
				s.where(), s.verb, s.placeholders, s.args, s.short())
		}
	}
	if nonVariadic < 15 {
		t.Fatalf("只比对了 %d 条非变参语句的占位符/实参，期望至少 15", nonVariadic)
	}
}

// TestEveryNotNullColumnHasWritePath 每个非自增的 NOT NULL 列都必须被某条 INSERT 或 SET 写过。
// 反例：给 thumbup_stat 加一列 `xxx_state TINYINT NOT NULL DEFAULT 0` 但 model 从不写它 ——
// 行行都是默认值，读侧看到的状态永远是 0，而所有查询都不报错。
// share_stat 按 row==nil 跳过：它没有任何写路径，这正是 README 已知缺口 13 登记的事实，
// 由 TestModellessTablesAreRegisteredAndStillUnused 负责显式记账。
func TestEveryNotNullColumnHasWritePath(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	written := map[string]map[string]bool{}
	for _, s := range stmts {
		if written[s.table] == nil {
			written[s.table] = map[string]bool{}
		}
		for _, c := range s.insertCols {
			written[s.table][c] = true
		}
		for c := range s.setCols {
			written[s.table][c] = true
		}
	}
	for _, spec := range modelBackedSpecs() {
		tbl := mustTable(t, tables, spec.table)
		for _, c := range tbl.columns {
			if c.autoIncrement {
				continue // 自增主键由数据库分配，model 只回读
			}
			if !written[spec.table][c.name] {
				t.Errorf("%s.%s: NOT NULL 列没有任何 INSERT/SET 写过它 —— 只能靠 DEFAULT 静默兜底，"+
					"读写口径已经断了", spec.table, c.name)
			}
		}
	}
}

// TestFullRowProjectionsMatchStructOrder 投影顺序 = 结构体字段顺序：
// 顺序不一致时 sqlx 会把 mid 读成 oid 这类静默错位。
// 注意：**不**要求投影顺序等于 DDL 列顺序 —— favorite_folder 的 count 在 DDL 里排在 ctime/mtime
// 之前、在结构体里排在最后，而所有查询都显式列名，所以 DDL 顺序不参与扫描，这是允许的差异。
func TestFullRowProjectionsMatchStructOrder(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	full := 0
	for _, s := range stmts {
		if s.verb != "SELECT" || len(s.selectCols) == 0 {
			continue
		}
		if s.starSelect {
			t.Errorf("%s: 出现 SELECT * —— 一旦 DDL 列顺序与结构体不同就会整体错位，加列即破坏契约：%q",
				s.where(), s.short())
			continue
		}
		spec := specFor(s.table)
		if spec == nil || spec.row == nil {
			continue
		}
		tbl, ok := tables[s.table]
		if !ok {
			continue
		}
		want := structColumns(t, spec.row)
		if len(s.selectCols) != len(want) {
			continue // 部分列投影（SELECT state / SELECT oid, state / COUNT(*)）：不比对顺序
		}
		full++
		for i := range want {
			if !tbl.hasColumn(s.selectCols[i]) {
				continue // 列不存在的错在 ③ 报过，这里不重复刷噪音
			}
			if s.selectCols[i] != want[i] {
				t.Fatalf("%s: 第 %d 列投影 %q 与结构体 %s 字段 %q 不符 —— 顺序不一致会静默扫错位",
					s.where(), i+1, s.selectCols[i], spec.table, want[i])
			}
		}
	}
	if full < 8 {
		t.Fatalf("只比对了 %d 条整行投影，期望至少 8（thumbup_like 3 条 + thumbup_stat 2 条 + favorite_folder 3 条）", full)
	}
}

// TestUpsertAndInsertIgnoreConflictKeysAreRealKeys ④：
// ON DUPLICATE KEY UPDATE / INSERT IGNORE 只在「新行撞上 PRIMARY KEY 或某个 UNIQUE KEY」时生效。
// 本服务这几张表的冲突键都不可能是主键（主键是自增 id，且 INSERT 列清单里没有它），
// 所以唯一键一旦被改名、被降级成普通 KEY、或被删掉，UPSERT 就退化成「每次都插新行」：
//   - thumbup_stat / favorite_item 的计数与收藏会重复累加；
//   - share_log 的 INSERT IGNORE 会每次都 RowsAffected=1，AddShare 的按天幂等静默失效
//     （live-room 查出过同形状的真缺陷）。
//
// 因此这条是语义门禁，不是命名门禁。
func TestUpsertAndInsertIgnoreConflictKeysAreRealKeys(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	upserts := 0
	const wantUpserts = 4 // thumbup_like.Upsert、thumbup_stat.Incr、favorite_item.Add、share_log.AddIfNotExists
	for _, s := range stmts {
		if !s.odku && !s.ignore {
			continue
		}
		upserts++
		spec := specFor(s.table)
		if spec == nil {
			continue
		}
		tbl := mustTable(t, tables, s.table)
		pk, ok := tbl.primaryColumn()
		if !ok {
			t.Errorf("%s: %s 依赖冲突键，但 %s 的主键无法判定（primary=%v）", s.where(), s.verb, s.table, tbl.primary)
			continue
		}
		if len(spec.conflictKeys) == 0 {
			t.Errorf("%s: %s 依赖冲突键，但 spec.conflictKeys 为空 —— 幂等口径没登记，等于没有门禁",
				s.where(), s.verb)
			continue
		}
		supplied := lowerSet(s.insertCols)
		if supplied[pk.name] {
			t.Errorf("%s: INSERT 显式给了自增主键 %s，冲突会变成「覆盖已有行」而不是累加/去重 —— "+
				"与 UPSERT 语义不同，要单独评审", s.where(), pk.name)
		}
		for _, keyName := range spec.conflictKeys {
			idx, ok := tbl.index(keyName)
			if !ok {
				t.Errorf("%s: %s 依赖的冲突键 %s 在 %s 的 DDL 里不存在（现有索引：%v）—— "+
					"UPSERT 会永不冲突、幂等静默失效", s.where(), s.verb, keyName, s.table, indexNames(tbl))
				continue
			}
			if !idx.unique {
				t.Errorf("%s: 冲突键 %s 不是 UNIQUE KEY（普通 KEY 不产生冲突，UPSERT 退化成 INSERT）",
					s.where(), keyName)
				continue
			}
			for _, c := range idx.cols {
				if !supplied[c] {
					t.Errorf("%s: 冲突键 %s 的列 %s 没出现在 INSERT 列清单里 —— 它只能取 DEFAULT，"+
						"实际参与冲突的键值与登记的幂等口径不是同一个：%v", s.where(), keyName, c, supplied)
				}
			}
		}
		// 除登记的冲突键外不能再有第二个唯一键：命中哪个键不确定，
		// 「新增 vs 重放」的 RowsAffected 判定就会失真。
		if others := extraUniqueKeys(tbl, spec.conflictKeys); len(others) > 0 {
			t.Errorf("%s: %s 除登记的冲突键外还有唯一键 %v —— 命中哪个键不确定，"+
				"RowsAffected 的幂等判定会失真", s.where(), s.verb, others)
		}
		if s.odku && len(s.setCols) == 0 {
			t.Errorf("%s: 写了 ON DUPLICATE KEY UPDATE 但解析不出任何赋值目标：%q", s.where(), s.short())
		}
	}
	if upserts != wantUpserts {
		t.Fatalf("只扫到 %d 条 UPSERT/INSERT IGNORE 语句，期望 %d 条：写入口增减时本用例要一起改",
			upserts, wantUpserts)
	}
}

// TestIdempotencyKeyTuplesAgreeAcrossDDLSpecAndModelDocs ⑤：
// UNIQUE 索引列组合 ↔ spec 登记 ↔ model 头注释的幂等元组 ↔ 真的按整键定位的查询，四处同序。
// 只看 DDL 与 spec 会一起改错（两边都是人写的），第三、四处是独立来源的口径。
func TestIdempotencyKeyTuplesAgreeAcrossDDLSpecAndModelDocs(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			// DDL 侧：唯一索引必须都被 spec 登记，且索引名要等于 uniq_<列名_拼接>。
			for _, idx := range tbl.uniqueIndexes() {
				wantCols, ok := spec.uniqueKeys[idx.name]
				if !ok {
					t.Errorf("%s: DDL 里有唯一索引 %s%v，但 spec.uniqueKeys 没登记 —— "+
						"未登记的唯一键会悄悄改变 UPSERT/IGNORE 的冲突语义", spec.table, idx.name, idx.cols)
					continue
				}
				if strings.Join(idx.cols, ",") != strings.Join(wantCols, ",") {
					t.Errorf("%s.%s: DDL 列组合 %v 与 spec 登记 %v 不符（顺序敏感）",
						spec.table, idx.name, idx.cols, wantCols)
				}
				if want := expectedUniqIndexName(idx.cols); idx.name != want {
					// 例外必须登记：命名口径从列名推导，而全仓 event_id 唯一键另有既定名字，
					// 两者冲突时登记理由而不是改 DDL 或放宽本断言（见 spec 字段注释）。
					reason, exempt := spec.uniqIndexNameExceptions[idx.name]
					if !exempt {
						t.Errorf("%s: 唯一索引名 %s 与列组合 %v 不符，期望 %s —— 命名承载着幂等口径，"+
							"改名要同步文档；要保留现名请在 uniqIndexNameExceptions 里登记理由",
							spec.table, idx.name, idx.cols, want)
						continue
					}
					if reason == "" {
						t.Errorf("%s: uniqIndexNameExceptions 登记了 %s 却没写理由", spec.table, idx.name)
					}
					if got := expectedUniqIndexName(idx.cols); got == idx.name {
						t.Errorf("%s: %s 其实符合命名口径（%v -> %s），例外登记该删", spec.table, idx.name, idx.cols, got)
					}
				}
			}
			for name, wantCols := range spec.uniqueKeys {
				idx, ok := tbl.index(name)
				if !ok {
					t.Fatalf("%s: spec 登记了唯一索引 %s，DDL 里没有（幂等失去数据库兜底）", spec.table, name)
				}
				if !idx.unique {
					t.Fatalf("%s.%s: 不是 UNIQUE KEY", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(wantCols, ",") {
					t.Errorf("%s.%s: DDL %v，spec %v", spec.table, name, idx.cols, wantCols)
				}
			}
			for name := range spec.uniqIndexNameExceptions {
				if _, ok := tbl.index(name); !ok {
					t.Errorf("%s: uniqIndexNameExceptions 登记的 %s 在 DDL 里不存在，例外登记要一起删",
						spec.table, name)
				}
			}
			if len(tbl.uniqueIndexes()) == 0 {
				if spec.noUniqueKeyReason == "" {
					t.Errorf("%s: DDL 没有唯一键，spec 也没写 noUniqueKeyReason —— 幂等只能靠 logic 预检，必须说明取舍",
						spec.table)
				}
				if len(spec.conflictKeys) > 0 {
					t.Errorf("%s: 没有唯一键却登记了 conflictKeys：%v 不可能产生冲突",
						spec.table, spec.conflictKeys)
				}
				return
			}
			if spec.noUniqueKeyReason != "" {
				t.Errorf("%s: 登记了 noUniqueKeyReason，但 DDL 已经有唯一键了：%v",
					spec.table, tbl.uniqueIndexes())
			}
			// model 侧：幂等元组必须原样出现在该表的 model 文件注释里。
			if spec.row == nil {
				if spec.idempotencyNote != "" || spec.idempotencyIndex != "" {
					t.Errorf("%s: 没有 model 实现却登记了 idempotencyNote/Index，口径来源不存在", spec.table)
				}
				return
			}
			if spec.idempotencyNote == "" || spec.idempotencyIndex == "" {
				t.Errorf("%s: 有 model 实现、DDL 也有唯一键，但没登记幂等元组注释 —— "+
					"注释与 DDL 漂移时，读代码的人只会信注释", spec.table)
				return
			}
			src := readModelFileOrFatal(t, spec.goFile)
			if !strings.Contains(src, spec.idempotencyNote) {
				t.Errorf("%s 的 model 文件里找不到幂等元组 %q —— 注释与 DDL 唯一键已经漂移：%q",
					spec.table, spec.idempotencyNote, spec.idempotencyIndex)
			}
			idx, ok := tbl.index(spec.idempotencyIndex)
			if !ok {
				t.Fatalf("%s: idempotencyIndex %s 在 DDL 里不存在", spec.table, spec.idempotencyIndex)
			}
			if strings.Join(tupleColumns(t, spec.table, spec.idempotencyNote), ",") != strings.Join(idx.cols, ",") {
				t.Errorf("%s: 幂等元组注释 %q 与 DDL 唯一键 %s%v 不同序",
					spec.table, spec.idempotencyNote, idx.name, idx.cols)
			}
			// 查询侧：至少 N 条语句「钉住整键」（键列全部等值/IN，允许再叠加其它谓词）。
			got, want := 0, spec.wholeKeyLookups[idx.name]
			for _, s := range stmts {
				if s.table == spec.table && pinsKey(s.pins, idx.cols) {
					got++
				}
			}
			if got < want {
				t.Errorf("%s: 只有 %d 条语句以整键 %v 定位，期望至少 %d 条 —— 按部分键读单行会读到不确定的那一行，"+
					"幂等口径失去可执行证据", spec.table, got, idx.cols, want)
			}
		})
	}
}

// TestPrimaryKeyIsAutoIncrementAndSingleRowReadsUseIt 主键口径：单列自增 BIGINT，
// spec 登记一致，且确实存在按主键等值定位单行的读路径。
func TestPrimaryKeyIsAutoIncrementAndSingleRowReadsUseIt(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	byPK := 0
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		if strings.Join(tbl.primary, ",") != strings.Join(spec.primaryKey, ",") {
			t.Errorf("%s: DDL 主键 %v 与 spec 登记 %v 不符", spec.table, tbl.primary, spec.primaryKey)
		}
		if len(tbl.primary) != 1 {
			t.Errorf("%s: 主键列是 %v，本服务一律用单列自增主键（复合主键会让 UPSERT 语义更难推）",
				spec.table, tbl.primary)
			continue
		}
		pk, ok := tbl.column(tbl.primary[0])
		if !ok {
			t.Fatalf("%s: 主键列 %s 没在列定义里出现", spec.table, tbl.primary[0])
		}
		if !pk.autoIncrement || pk.baseType() != "BIGINT" {
			t.Errorf("%s: 主键 %s 必须是 BIGINT AUTO_INCREMENT，得到 %q", spec.table, pk.name, pk.typeExpr)
		}
		if spec.row == nil {
			continue
		}
		if !contains(structColumns(t, spec.row), pk.name) {
			t.Errorf("%s: 结构体没有承载主键列 %s —— 自增 id 取不回来，按主键的更新写不动",
				spec.table, pk.name)
		}
	}
	for _, s := range stmts {
		tbl, ok := tables[s.table]
		if !ok || s.verb != "SELECT" || len(tbl.primary) != 1 {
			continue
		}
		if sameSet(s.pins, tbl.primary) {
			byPK++
		}
	}
	if byPK == 0 {
		t.Error("没有任何查询按主键等值定位单行 —— 本服务应当有 FindOne 走主键，" +
			"一条都没有说明读路径改过且没同步本用例")
	}
}

// TestQueryIndexesExistAndNoneUnregistered model 查询依赖的索引必须存在且列组合精确；
// DDL 里每个普通索引要么被某条查询依赖，要么显式登记「暂时无依赖」的理由。
func TestQueryIndexesExistAndNoneUnregistered(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for name, cols := range spec.requiredIndexes {
				idx, ok := tbl.index(name)
				if !ok {
					t.Fatalf("%s 缺少索引 %s%v：model 的 WHERE/ORDER BY 依赖它，否则退化成全表扫",
						spec.table, name, cols)
				}
				if idx.unique {
					t.Errorf("%s.%s: spec 当它是普通查询索引，DDL 里却是 UNIQUE KEY —— 语义不同", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("索引 %s 列不符：SQL %v，期望 %v（顺序敏感，前缀失效就不走它）", name, idx.cols, cols)
				}
			}
			for _, idx := range tbl.indexes {
				if idx.unique {
					continue // 唯一键承载不变量，不进「查询依赖」清单，由 ⑤ 管
				}
				if _, ok := spec.requiredIndexes[idx.name]; ok {
					continue
				}
				if reason, ok := spec.deferredIndexes[idx.name]; ok {
					if reason == "" {
						t.Errorf("%s: 索引 %s 登记成「暂时无查询依赖」却没写理由", spec.table, idx.name)
					}
					continue
				}
				t.Errorf("%s: 索引 %s%v 既没有 model 查询依赖、也没在 deferredIndexes 里写理由 —— "+
					"纯写放大的索引应当删掉，将来要用的索引应当连查询一起加", spec.table, idx.name, idx.cols)
			}
			for name := range spec.deferredIndexes {
				if _, ok := tbl.index(name); !ok {
					t.Errorf("%s: deferredIndexes 登记的 %s 在 DDL 里不存在，登记要一起删", spec.table, name)
				}
			}
			for name := range spec.requiredIndexes {
				if _, ok := spec.deferredIndexes[name]; ok {
					t.Errorf("%s: 索引 %s 同时登记成「查询依赖」和「无人使用」", spec.table, name)
				}
			}
		})
	}
}

// TestCharColumnRegistryMatchesDDLTypes ⑥：字符列清单与 DDL 实际类型完全一致。两个方向都钉。
//
//   - 登记的每一列在 DDL 里必须真是字符列：整数列（mid、oid、tp、day、biz_type 这类）
//     没有排序规则可言，登记进来只会把门禁变成噪音；
//   - DDL 里每个字符列都必须被登记，并给出「model 侧长度上限」或「无上限 + 理由 + 缺口编号」。
//
// 另外钉住排序规则现状：本服务的 DDL 不声明任何列级/表级 COLLATE，字符列一律走库表默认
// （大小写不敏感）。business 参与唯一键，这条事实必须显式记录：有人悄悄加上 utf8mb4_bin
// （或删掉）都会在这里浮出来，而它必须连同「唯一键大小写敏感性」的评审一起改。
func TestCharColumnRegistryMatchesDDLTypes(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var registered, ddlChar int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			actual := map[string]bool{}
			for _, c := range tbl.columns {
				if c.isCharType() {
					actual[c.name] = true
					ddlChar++
				}
			}
			want := lowerSet(spec.charColumns)
			for _, name := range sortedKeysBool(want) {
				registered++
				c, ok := tbl.column(name)
				if !ok {
					t.Errorf("%s.%s: spec 登记为字符列，但 DDL 里没有这一列", spec.table, name)
					continue
				}
				if !c.isCharType() {
					t.Errorf("%s.%s: spec 登记为字符列，但 DDL 类型是 %q —— 整数列不得进这张表",
						spec.table, name, c.typeExpr)
				}
			}
			for name := range actual {
				if !want[name] {
					t.Errorf("%s.%s: DDL 是字符列，但 spec.charColumns 未登记 —— 新增字符列必须同时决定长度口径",
						spec.table, name)
				}
			}
			for name := range actual {
				c, _ := tbl.column(name)
				limit, hasLimit := spec.charLimits[name]
				_, hasExempt := spec.unboundedChar[name]
				switch {
				case hasLimit && hasExempt:
					t.Errorf("%s.%s: 同时登记了长度上限 %d 与「无上限」豁免", spec.table, name, limit)
				case hasLimit:
					width, isFixed := c.charWidth()
					if !isFixed {
						t.Errorf("%s.%s: 不是 VARCHAR/CHAR，无法与上限比对：%q", spec.table, name, c.typeExpr)
						continue
					}
					if width < limit {
						t.Errorf("%s.%s: 列宽 %s(%d) 小于 model 侧上限 %d —— 校验放过而写入报错",
							spec.table, name, c.baseType(), width, limit)
					}
				case hasExempt:
					if !strings.Contains(spec.unboundedChar[name], "缺口") {
						t.Errorf("%s.%s 的「无长度上限」理由必须指向 README 已知缺口编号，得到 %q",
							spec.table, name, spec.unboundedChar[name])
					}
				default:
					t.Errorf("%s.%s: 字符列既没有 charLimits 上限，也没有 unboundedChar 豁免 —— 长度口径空白就是校验缺口",
						spec.table, name)
				}
			}
			for name := range spec.unboundedChar {
				if !actual[name] {
					t.Errorf("%s.%s: 豁免登记还在，但 DDL 里它已经不是字符列（或已删列）—— 登记要一起删",
						spec.table, name)
				}
			}
			for name := range spec.charLimits {
				if !actual[name] {
					t.Errorf("%s.%s: charLimits 登记还在，但 DDL 里它已经不是字符列（或已删列）", spec.table, name)
				}
			}
			for _, c := range tbl.columns {
				declared, hasCollate := c.declaredCollate()
				reason, exempt := spec.binaryCollate[c.name]
				switch {
				case hasCollate && !exempt:
					t.Errorf("%s.%s: DDL 出现了未登记的列级 COLLATE %q；本域只有 binaryCollate 登记过的列"+
						"才允许声明排序规则，要改必须先评审它所在唯一键的大小写敏感性，并同步本用例",
						spec.table, c.name, c.typeExpr)
				case exempt && !hasCollate:
					t.Errorf("%s.%s: binaryCollate 还在替它担保逐字节比较，但 DDL 已经没有 COLLATE 了：%q；"+
						"规则被删掉等于唯一键恢复大小写不敏感，登记要连同口径一起改回来",
						spec.table, c.name, c.typeExpr)
				case hasCollate:
					if reason == "" {
						t.Errorf("%s.%s: binaryCollate 登记了却没写理由", spec.table, c.name)
					}
					if declared != "utf8mb4_bin" {
						t.Errorf("%s.%s: 登记的列级排序规则是 %q，只允许 utf8mb4_bin（逐字节、不折叠大小写）",
							spec.table, c.name, declared)
					}
					// 逐字节比较只在「该列参与唯一键」时才有意义；普通字符列上声明 bin 排序
					// 只会让它与其它列的比较口径不一致（索引下推、JOIN、ORDER BY 都要重看）。
					if !inAnyUniqueKey(tbl, c.name) {
						t.Errorf("%s.%s: 声明了 %s 却不属于任何 UNIQUE KEY；豁免的前提是「唯一键要逐字节比较」，"+
							"普通列上换排序规则会让本域的字符列出现两套比较口径", spec.table, c.name, declared)
					}
				}
			}
		})
	}
	const wantCharColumns = 11 // business x2、folder name/description/cover、outbox event_id/event_type/aggregate_type/aggregate_id/payload/last_error
	if registered != wantCharColumns || ddlChar != wantCharColumns {
		t.Errorf("字符列比对了登记 %d 列 / DDL 实际 %d 列，期望各 %d —— 数量变了要同步本用例与长度口径",
			registered, ddlChar, wantCharColumns)
	}
}

// TestEnumCommentsCoverModelConstantsAndSQLLiterals 数值枚举列：model 常量与 SQL 里写死的
// 字面量都必须逐个出现在 DDL 注释里，否则线上排查只能靠猜（注释是唯一还活着的取值清单）。
// 参与唯一键的 TINYINT 列还必须逐值枚举（或在 unenumeratedUniqEnum 里写明理由 + 缺口编号）：
// 唯一键上的枚举值一旦口径漂移，去重范围就会静默改变（share_log.tp 就是这种形状）。
func TestEnumCommentsCoverModelConstantsAndSQLLiterals(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	stmts := collectModelStatements(t)
	checked := 0
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		for col, values := range spec.enumValues {
			c, ok := tbl.column(col)
			if !ok {
				t.Errorf("%s 缺少枚举列 %s", spec.table, col)
				continue
			}
			if c.baseType() != "TINYINT" {
				t.Errorf("%s.%s: 登记为 TINYINT 枚举列，实际类型 %q", spec.table, col, c.typeExpr)
			}
			nums := c.commentNumbers()
			for _, v := range values {
				checked++
				if !nums[strconv.FormatInt(v, 10)] {
					t.Errorf("%s.%s 的注释 %q 没说明取值 %d —— 新增枚举值必须同步建表注释",
						spec.table, col, c.comment, v)
				}
			}
		}
		for name, reason := range spec.unenumeratedUniqEnum {
			c, ok := tbl.column(name)
			if !ok {
				t.Errorf("%s: unenumeratedUniqEnum 登记的 %s 已不在 DDL 里，登记要删", spec.table, name)
				continue
			}
			if len(c.commentNumbers()) > 0 {
				t.Errorf("%s.%s: 注释已经逐值枚举（%q），豁免该删了", spec.table, name, c.comment)
			}
			if !strings.Contains(reason, "缺口") {
				t.Errorf("%s.%s 的豁免理由要指向 README 已知缺口编号：%q", spec.table, name, reason)
			}
		}
		for _, idx := range tbl.uniqueIndexes() {
			for _, col := range idx.cols {
				c, ok := tbl.column(col)
				if !ok || c.baseType() != "TINYINT" {
					continue
				}
				if _, exempt := spec.unenumeratedUniqEnum[col]; exempt {
					continue
				}
				if len(c.commentNumbers()) == 0 {
					t.Errorf("%s.%s: 参与唯一键 %s 的枚举列注释没有逐值说明：%q —— "+
						"唯一键上的取值口径漂移会静默改变去重范围", spec.table, col, idx.name, c.comment)
				}
			}
		}
		// SQL 里写死的 TINYINT 字面量也要被注释覆盖（例如 SET state = 1、AND public = 1）。
		for _, s := range stmts {
			if s.table != spec.table {
				continue
			}
			for col, vals := range s.literals {
				c, ok := tbl.column(col)
				if !ok || c.baseType() != "TINYINT" {
					continue
				}
				nums := c.commentNumbers()
				for _, v := range vals {
					checked++
					if !nums[strconv.FormatInt(v, 10)] {
						t.Errorf("%s: SQL 把 %s.%s 写成 %d，但 DDL 注释 %q 里没有这个取值",
							s.where(), spec.table, col, v, c.comment)
					}
				}
			}
		}
	}
	if checked < 8 {
		t.Fatalf("只核对了 %d 个枚举取值，期望至少 8：常量或列名被改动会让本用例失去信号", checked)
	}
}

// TestTimeColumnsAreUnixSecondsBigInt 时间列一律 BIGINT Unix 秒：改成 DATETIME 会让
// model 的 int64 扫描直接失败，而内存替身两边都不会报。
func TestTimeColumnsAreUnixSecondsBigInt(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	checked := 0
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		for _, name := range spec.timeColumns {
			c, ok := tbl.column(name)
			if !ok {
				t.Fatalf("%s 缺少时间列 %s", spec.table, name)
			}
			checked++
			if c.baseType() != "BIGINT" {
				t.Errorf("%s.%s 必须是 BIGINT（Unix 秒），得到 %q", spec.table, name, c.typeExpr)
			}
			if !strings.Contains(c.comment, "Unix") {
				t.Errorf("%s.%s 的注释没写明 Unix 秒口径：%q", spec.table, name, c.comment)
			}
		}
	}
	if checked < 9 {
		t.Fatalf("只核对了 %d 个时间列，期望至少 9", checked)
	}
}

// TestDailyBucketIsIntegerYyyymmdd share_log.day 的幂等粒度靠 YYYYMMDD 整数实现：
// 列必须是 32 位整型（放得下 99991231），注释必须写明口径，model 侧必须是 int32。
// 改成 DATE/DATETIME 会让 model 的 int32 扫不出来，改成 SMALLINT 会溢出，
// 而这两条在内存替身里都表现为「一切正常」。
func TestDailyBucketIsIntegerYyyymmdd(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	c, ok := mustTable(t, tables, "share_log").column("day")
	if !ok {
		t.Fatal("share_log 缺少 day 列：按天幂等没有落点")
	}
	if width := c.integerWidthBits(); width != 32 {
		t.Errorf("share_log.day 必须是 32 位整型 INT，得到 %q（位宽 %d）", c.typeExpr, width)
	}
	if !strings.Contains(c.comment, "YYYYMMDD") {
		t.Errorf("share_log.day 注释必须写明 YYYYMMDD 口径，得到 %q", c.comment)
	}
	if !c.notNull {
		t.Error("share_log.day 允许 NULL —— 幂等键里出现 NULL 会让唯一性静默失效")
	}
	if kinds := structFieldKinds(t, ShareLog{}); kinds["day"] != "int32" {
		t.Errorf("ShareLog.Day 应为 int32 以匹配 INT 列，得到 %q", kinds["day"])
	}
	if dayYyyymmddMax >= 1<<31 {
		t.Fatalf("dayYyyymmddMax=%d 已超出 int32，前面的宽度算术前提不成立", dayYyyymmddMax)
	}
	got := currentDayYyyymmdd()
	if got <= 0 || got > dayYyyymmddMax {
		t.Errorf("currentDayYyyymmdd() 返回 %d，不在 (0, %d] 内 —— 幂等粒度不再是「天」", got, dayYyyymmddMax)
	}
	if cols := specFor("share_log").uniqueKeys["uniq_oid_mid_tp_day"]; !contains(cols, "day") {
		t.Errorf("share_log 的幂等键 %v 里没有 day —— 按天去重已经不成立，本用例与 README 都要改", cols)
	}
}

// TestCountColumnsAreSignedIntegersAndDocumented 计数字段的宽度口径：
// 展示计数用 BIGINT（会被运营修正增量叠加、也可能被写负），快照类小计数用 INT，
// 且注释必须非空 —— 计数列的单位一旦含糊，读侧只能猜。
func TestCountColumnsAreSignedIntegersAndDocumented(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	cases := []struct{ table, column, wantType string }{
		{"thumbup_stat", "like_number", "BIGINT"},
		{"thumbup_stat", "dislike_number", "BIGINT"},
		{"thumbup_stat", "like_change", "BIGINT"},
		{"thumbup_stat", "dislike_change", "BIGINT"},
		{"favorite_folder", "count", "INT"},
		{"share_stat", "count", "BIGINT"},
		{"engagement_outbox", "retry_count", "INT"},
	}
	for _, c := range cases {
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			col, ok := mustTable(t, tables, c.table).column(c.column)
			if !ok {
				t.Fatalf("%s 缺少计数列 %s", c.table, c.column)
			}
			if col.baseType() != c.wantType {
				t.Errorf("%s.%s 应为 %s，得到 %q", c.table, c.column, c.wantType, col.typeExpr)
			}
			if col.comment == "" {
				t.Errorf("%s.%s 缺少注释", c.table, c.column)
			}
		})
	}
	// 展示值 = 原始累加 + 运营修正，两组列必须在同一张表里：
	// 分表存放会让 UpdateCount 的修正与 Incr 的累加不在同一次读里合成（README 已知缺口 5 的现状）。
	stat := mustTable(t, tables, "thumbup_stat")
	for _, name := range []string{"like_number", "dislike_number", "like_change", "dislike_change"} {
		if !stat.hasColumn(name) {
			t.Errorf("thumbup_stat 缺少 %s —— 修正增量与展示值必须同表，否则读侧无法合成", name)
		}
	}
}

// TestSoftDeleteIsTheOnlyRemovalPath 本服务的删除一律软删（state 位）：
// model 里不许出现 DELETE，且三个软删位必须逐值注释清楚（model 的 SQL 会写死 0/1）。
func TestSoftDeleteIsTheOnlyRemovalPath(t *testing.T) {
	stmts := collectModelStatements(t)
	for _, s := range stmts {
		if s.verb == "DELETE" {
			t.Errorf("%s: 出现了 DELETE 语句 %q —— 互动关系与收藏都是软删（state=1），"+
				"物理删除会毁掉「取消后重投」的幂等判定", s.where(), s.short())
		}
	}
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, c := range []struct{ table, column string }{
		{"thumbup_like", "state"}, {"favorite_item", "state"}, {"favorite_folder", "state"},
	} {
		tbl, ok := tables[c.table]
		if !ok {
			continue
		}
		col, ok := tbl.column(c.column)
		if !ok {
			t.Errorf("%s 缺少 state 列（软删位）", c.table)
			continue
		}
		nums := col.commentNumbers()
		if !nums["0"] || !nums["1"] {
			t.Errorf("%s.state 注释 %q 没同时说明 0 与 1 —— model 的 SQL 会写死这两个值", c.table, col.comment)
		}
	}
}

// TestMigrationsAreSelfDescribing 迁移文件自述：owner / 回滚 / 影响范围 / 库名 / 幂等，
// 并且「影响范围：新增 N 张表」必须等于文件里**行首锚定**到的 CREATE TABLE IF NOT EXISTS 条数
// —— 计数式探针不做全文件子串搜索，否则头注释里的复述会让它恒差若干、变成全量假阳性。
func TestMigrationsAreSelfDescribing(t *testing.T) {
	pf := parseMigrations(t, findMigrationsDir(t))
	const wantFiles = 4 // 000001 点赞 / 000002 收藏 / 000003 分享 / 000004 事件 Outbox
	if len(pf) != wantFiles {
		t.Errorf("迁移文件 %d 份，期望 %d 份", len(pf), wantFiles)
	}
	for _, p := range pf {
		base := filepath.Base(p.path)
		t.Run(base, func(t *testing.T) {
			for _, marker := range []string{"owner", "回滚", "影响范围", "幂等", engagementDBName} {
				if !strings.Contains(p.header, marker) {
					t.Errorf("%s: 头注释缺少 %q 段落（本域表的口径、影响范围与回滚必须写清）", base, marker)
				}
			}
			m := reCreateTableClaim.FindStringSubmatch(p.header)
			if m == nil {
				t.Fatalf("%s: 头注释里找不到「影响范围：新增 N 张表」，无法核对声明与实际表数", base)
			}
			claim, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("%s: 头注释的表数 %q 不是数字", base, m[1])
			}
			if claim != len(p.creates) {
				t.Errorf("%s: 头注释声明新增 %d 张表，实际解析到 %d 条 CREATE TABLE IF NOT EXISTS（%v）",
					base, claim, len(p.creates), p.creates)
			}
			for _, name := range p.creates {
				if !strings.Contains(p.header, "DROP TABLE IF EXISTS `"+name+"`") {
					t.Errorf("%s: 回滚段落没覆盖表 %s —— 回滚会留下孤儿表", base, name)
				}
			}
			for _, line := range p.sql {
				upper := strings.ToUpper(line)
				for _, bad := range []string{"ALTER TABLE", "DELETE FROM", "TRUNCATE", "DROP DATABASE",
					"GRANT ", "INSERT INTO", "UPDATE ", "FOREIGN KEY", "CREATE INDEX"} {
					if strings.HasPrefix(upper, bad) {
						t.Errorf("%s: 可执行语句以被禁止的 %q 开头: %q —— 本目录只建表；"+
							"数据变更要另开 0000NN 文件并写明回滚与锁窗口", base, bad, line)
					}
				}
				if !strings.HasPrefix(line, ")") {
					continue
				}
				if !strings.Contains(line, "COMMENT='") {
					t.Errorf("%s: 表收尾行缺少表级 COMMENT='…'：%q", base, line)
				}
				if strings.Contains(upper, "COLLATE") {
					t.Errorf("%s: 表选项里出现了 COLLATE：%q —— 排序规则影响唯一键的大小写敏感性，"+
						"必须先评审并同步 TestCharColumnRegistryMatchesDDLTypes", base, line)
				}
			}
			all := strings.ToUpper(p.header + "\n" + strings.Join(p.sql, "\n"))
			for _, bad := range []string{"ACCESSKEY", "SECRET_KEY", "PRIVATE KEY", "-----BEGIN"} {
				if strings.Contains(all, bad) {
					t.Errorf("%s: 出现疑似凭据字面量 %q", base, bad)
				}
			}
		})
	}
}

// TestParserActuallyParsedTheSchema 解析器退化自检：任何一处「格式改坏了、解析出空结构」
// 都会让上面所有断言空转发绿，所以这里把已解析到的表/列/索引数量钉成精确值或下限。
func TestParserActuallyParsedTheSchema(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	if len(tables) != 7 {
		t.Fatalf("解析出 %d 张表，期望 7 张：%v", len(tables), sortedTableNames(tables))
	}
	columns, indexes, uniques, primaries := 0, 0, 0, 0
	for _, name := range sortedTableNames(tables) {
		tbl := tables[name]
		if len(tbl.columns) < 5 {
			t.Errorf("表 %s 只解析出 %d 列 —— 列定义写法变了，逐列比对已失效", name, len(tbl.columns))
		}
		if len(tbl.primary) == 0 {
			t.Errorf("表 %s 没解析出 PRIMARY KEY", name)
		}
		if tbl.options == "" {
			t.Errorf("表 %s 没解析出收尾行/表选项", name)
		}
		if tbl.file == "" {
			t.Errorf("表 %s 没记录来源文件，失败信息将无法定位", name)
		}
		columns += len(tbl.columns)
		indexes += len(tbl.indexes)
		uniques += len(tbl.uniqueIndexes())
		primaries += len(tbl.primary)
	}
	const wantColumns = 64 // 9 + 10 + 10 + 9 + 6 + 6 + 14（engagement_outbox）
	if columns != wantColumns {
		t.Errorf("共解析出 %d 列，期望 %d —— 有人加/删了列，所有清单与用例都要一起同步", columns, wantColumns)
	}
	// PRIMARY KEY 记在 tbl.primary、不进 tbl.indexes，所以 14 = 6 个 UNIQUE + 8 个普通 KEY
	// （thumbup_like 2、favorite_folder 1、favorite_item 2、share_log 1、engagement_outbox 2）。
	const (
		wantIndexes   = 14
		wantUniques   = 6
		wantPrimaries = 7
	)
	if primaries != wantPrimaries {
		t.Fatalf("只解析出 %d 个 PRIMARY KEY，期望 %d —— 每张表都应有且仅有一个主键，解析或 DDL 变了",
			primaries, wantPrimaries)
	}
	if indexes != wantIndexes || uniques != wantUniques {
		t.Fatalf("索引解析结果异常（索引 %d/唯一 %d，期望 %d/%d）：索引定义写法变了，④⑤ 号断言会失去信号",
			indexes, uniques, wantIndexes, wantUniques)
	}
}

// TestStatementsSpreadOverEveryModelFile 交叉确认每张表的语句条数，
// 防止「表还在、写入口或读路径被整体删掉」这种静默缩水。
func TestStatementsSpreadOverEveryModelFile(t *testing.T) {
	stmts := collectModelStatements(t)
	perTable := map[string]int{}
	byFile := map[string]int{}
	for _, s := range stmts {
		perTable[s.table]++
		byFile[s.file]++
		if specFor(s.table) == nil {
			t.Errorf("SQL 命中了未登记的表 %s（%s）", s.table, s.where())
		}
	}
	for _, spec := range modelBackedSpecs() {
		if perTable[spec.table] < 2 {
			t.Errorf("%s: 只扫到 %d 条 SQL，少于 2 条（读与写各至少一条）—— 用例覆盖与 spec 要同步收缩",
				spec.table, perTable[spec.table])
		}
	}
	for _, name := range sqlLitFiles() {
		if byFile[name] == 0 {
			t.Errorf("model 文件 %s 里一条 SQL 都没解析到，但它被登记成某张表的 goFile", name)
		}
	}
}

// --- 小工具 ---

func readModelFileOrFatal(t *testing.T, name string) string {
	t.Helper()
	if name == "" {
		t.Fatal("spec 没有登记 goFile")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("读取 model/%s 失败: %v", name, err)
	}
	return string(raw)
}

// tupleColumns 解析 "(a, b, c)" 形态的幂等元组，返回归一小写列名（保持顺序）。
func tupleColumns(t *testing.T, table, note string) []string {
	t.Helper()
	open := strings.Index(note, "(")
	closing := strings.LastIndex(note, ")")
	if open < 0 || closing < open {
		t.Fatalf("%s: idempotencyNote %q 不是 (a, b) 形态", table, note)
	}
	parts := strings.Split(note[open+1:closing], ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		col := strings.ToLower(strings.TrimSpace(p))
		if col == "" {
			t.Fatalf("%s: idempotencyNote %q 里有空列名", table, note)
		}
		out = append(out, col)
	}
	return out
}

// sameSet 判断「语句的等值定位列集合」与「索引列集合」恰好是同一组（顺序无关）。
// 用集合相等而不是前缀包含：按部分键读单行本身就是风险。
func sameSet(got map[string]bool, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, c := range want {
		if !got[strings.ToLower(c)] {
			return false
		}
	}
	return true
}

// pinsKey 判断语句是否**钉住**了整键：键列全部等值（或 IN），允许再叠加别的谓词
// （例如 favorite_item.Del 是 mid+oid+tp+fid+state）。
// 与 sameSet 的分工：sameSet 用于「谓词恰好就是这组键」的强口径（主键读单行），
// pinsKey 只保证唯一键被完整钉住，多出来的谓词只会让结果更少，不会读到不确定的那一行。
func pinsKey(got map[string]bool, want []string) bool {
	for _, c := range want {
		if !got[strings.ToLower(c)] {
			return false
		}
	}
	return true
}

// expectedUniqIndexName 复现 deploy/migrations/engagement 的唯一索引命名口径：
// uniq_ + 各列名去掉 _id 后缀后按 DDL 顺序用 _ 拼接
// （uniq_business_mid_message ← (business, mid, message_id)、
//
//	uniq_business_origin_message ← (business, origin_id, message_id)）。
//
// 命名里带着幂等元组，所以列名变了索引名必须跟着变，否则评审时看不出来键换过。
func expectedUniqIndexName(cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		c = strings.ToLower(strings.TrimSpace(c))
		parts = append(parts, strings.TrimSuffix(c, "_id"))
	}
	return "uniq_" + strings.Join(parts, "_")
}

// removeWordCaseInsensitively 按**整词**（_ 视为词字符）挖掉一个标识符。
// 直接 strings.ReplaceAll 会咬进子串：把 "id" 挖掉会让 "origin_id" 变成 "origin_"，
// 剩下的 "origin_" 又被当成一个列名 token，产生假阳性。
func removeWordCaseInsensitively(text, word string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(word) + `\b`)
	return re.ReplaceAllString(text, " ")
}

func extraUniqueKeys(tbl *sqlTable, registered []string) []string {
	have := lowerSet(registered)
	out := []string{}
	for _, idx := range tbl.uniqueIndexes() {
		if !have[idx.name] {
			out = append(out, idx.name)
		}
	}
	sort.Strings(out)
	return out
}

func indexNames(tbl *sqlTable) []string {
	out := []string{}
	for _, i := range tbl.indexes {
		out = append(out, i.name)
	}
	sort.Strings(out)
	return out
}

func columnNames(tbl *sqlTable) []string {
	out := []string{}
	for _, c := range tbl.columns {
		out = append(out, c.name)
	}
	return out
}

func lowerSet(list []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range list {
		out[strings.ToLower(strings.TrimSpace(v))] = true
	}
	return out
}

func sortedTableNames(tables map[string]*sqlTable) []string {
	out := make([]string, 0, len(tables))
	for k := range tables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
