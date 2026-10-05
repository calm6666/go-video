package model

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 本文件把「model 的 SQL 与 deploy/migrations/spm 逐列一致」这条约定变成可执行门禁。
//
// 为什么必须是测试而不是人工比对（AGENTS.md §9）：
//   - 本服务的幂等语义全部压在数据库唯一键上（uniq_metric 六列、uniq_watermark 四列、
//     uniq_request_id、uniq_event_id、uniq_interest、uniq_cohort、uniq_subject、
//     uniq_metric_version）。少一列不是启动失败，而是「同窗口重放变成累加」「旧事件盖掉
//     新状态」这类最贵的静默故障（README「窗口、水位与迟到」）。
//   - model 的结构体 db tag、SELECT 列常量、INSERT 列清单、手写行数列常量是四处必须同步的
//     文本，任一处漂移都只在线上 "Unknown column" / 字段错位扫描时才暴露。
//   - 榜单与聚合的每条查询路径都依赖具体索引前缀（buildHotQuery 的 LEFT JOIN、
//     FindLatestWindowStart 的 idx_metric_latest 四列等值）。索引丢列或换序的表现是
//     全表扫描，不是报错。
//   - 迁移本轮**没有**在 MySQL 上执行过，结构正确性只能靠这种静态一致性证明。
//
// 测试不连数据库：只读 SQL 文本，与 model 的常量/结构体 tag 比对。
// 参考 services/live-room/model/migration_parity_test.go 的形态，按 spm 的
// 「一个文件建多张表」「COLLATE=utf8mb4_unicode_ci」「) ENGINE 行后接 COMMENT 行」
// 三个既有差异改写。

const (
	migrationsRelDir = "deploy/migrations/spm"
	// spmDBName 是契约声明的唯一 schema（rpc/spm.proto 头部「库名 go_video_spm」），
	// 也是 etc 示例里 DataSource 的库名；迁移与配置分叉 = 服务连到别人的库。
	spmDBName = "go_video_spm"
)

// tableSpec 声明一张 model 表与它的迁移定义的对应关系。
type tableSpec struct {
	table string // SQL 里的表名
	file  string // 建这张表的迁移文件名
	// row 是行结构体样例；model 读写的列全集取自它的 db tag（含声明顺序）。
	row any
	// selectColumns 是 model 拼进 SELECT 的列常量原值（含 COALESCE 别名列）。
	selectColumns string
	// insertColumns/insertRowLen 是批量 INSERT 的列清单与 model 手写的「单行占位符数」常量；
	// 两者必须等长——行数列常量抄错会让多值 INSERT 的占位符总数与参数总数错位，
	// MySQL 直接报 "column count doesn't match value count"。
	insertColumns string
	insertRowLen  int
	// primaryKey 是 model 定位单行 / UPSERT 冲突所使用的列组合。
	primaryKey []string
	// uniqueKeys 是幂等与不变量依赖的唯一索引：键名 -> 列组合（顺序敏感）。
	uniqueKeys map[string][]string
	// requiredIndexes 是 model 的 WHERE / ORDER BY 依赖的普通索引名；
	// 列组合在 expectedIndexColumns 里单独声明，两者都要对上。
	requiredIndexes []string
	// sqlOnly 是「SQL 有、model 不读写」的列及其保留理由。留空即表示本表零冗余列。
	sqlOnly map[string]string
	// nullable 列出允许为 NULL 的列（死信 event_id 靠 NULL 逃逸唯一性；
	// 消费位点 payload 是「成功行清空原文」的可空载荷）。
	nullable []string
	// requiredNoDefault 列出刻意不给 DEFAULT 的必填列：漏传必须在 MySQL 侧报错，
	// 而不是被默认值静默兜成 0/空串。
	requiredNoDefault []string
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:         "spm_consumer_offset",
			file:          "000001_create_spm_event_fact_tables.sql",
			row:           ConsumerOffset{},
			selectColumns: consumerOffsetFullColumns,
			primaryKey:    []string{"id"},
			// 消费状态机的幂等真值：MarkSucceeded/MarkRetry 都按 event_id 定位单行。
			uniqueKeys: map[string][]string{"uniq_event_id": {"event_id"}},
			requiredIndexes: []string{
				"idx_state_next_retry", // ListOverdueRetry：state=retry + 退避到期，按到期时间先进先出
				"idx_topic_state",      // Summarize/CountByState 的 topic × state 汇总
				"idx_state_ctime",      // DeleteSettledBefore / 清理已终结记录
			},
			// payload 只在失败路径保留、成功行清空（MarkSucceeded 写 NULL），
			// 因此必须可空：NOT NULL + 空串会让「原文已清走」与「本来就带空 payload」分不清。
			nullable: []string{"payload"},
			// event_id 是幂等锚点：默认成空串会让第二条没有 event_id 的投递撞键而不是报错。
			requiredNoDefault: []string{"event_id"},
		},
		{
			table:         "spm_dead_letter",
			file:          "000001_create_spm_event_fact_tables.sql",
			row:           DeadLetter{},
			selectColumns: deadLetterColumns,
			primaryKey:    []string{"id"},
			// 双键去重：信封可解析按 event_id，不可解析按内容摘要（见 dead_letter.go 注释）。
			uniqueKeys: map[string][]string{
				"uniq_event_id":       {"event_id"},
				"uniq_payload_digest": {"payload_digest"},
			},
			requiredIndexes: []string{
				"idx_topic_state", // List/Count 的 topic + state 过滤
				"idx_state_ctime", // CountOpen：待处理死信数（state=open + 时间窗）
			},
			// event_id 可空是设计而非疏漏：MySQL 的 NULL 不参与唯一约束，
			// 坏消息才能各留一行；读侧因此必须 COALESCE（deadLetterColumns 已如此）。
			nullable:          []string{"event_id"},
			requiredNoDefault: []string{"payload_digest"},
		},
		{
			table:         "spm_behavior_event",
			file:          "000001_create_spm_event_fact_tables.sql",
			row:           BehaviorEvent{},
			selectColumns: behaviorEventColumns,
			insertColumns: behaviorEventInsertCols,
			insertRowLen:  behaviorEventRowLen,
			primaryKey:    []string{"id"},
			uniqueKeys:    map[string][]string{"uniq_event_id": {"event_id"}},
			requiredIndexes: []string{
				"idx_event_type_occurred", "idx_content_occurred", "idx_aid_occurred",
				"idx_zone_occurred", "idx_catalog_item_occurred", "idx_target_mid_occurred",
				"idx_mid_occurred", "idx_mid_event_day", "idx_ctime",
			},
			nullable:          []string{},
			requiredNoDefault: []string{"event_id"},
		},
		{
			table:         "spm_metric_definition",
			file:          "000002_create_spm_metric_projection_tables.sql",
			row:           MetricDefinition{},
			selectColumns: metricDefinitionColumns,
			primaryKey:    []string{"id"},
			// 口径版本不可原地改写的落库形态：(metric_key, metric_version) 唯一。
			uniqueKeys: map[string][]string{"uniq_metric_version": {"metric_key", "metric_version"}},
			requiredIndexes: []string{
				"idx_request_id", // FindByRequestID：登记请求幂等回放
				"idx_key_state",  // FindActive / CountActive
			},
			nullable:          []string{},
			requiredNoDefault: []string{"metric_key", "metric_version"},
		},
		{
			table:         "spm_metric_window",
			file:          "000002_create_spm_metric_projection_tables.sql",
			row:           MetricWindow{},
			selectColumns: metricWindowColumns,
			insertColumns: metricWindowInsertCols,
			insertRowLen:  metricWindowRowLen,
			primaryKey:    []string{"id"},
			// uniq_metric 六列 = 「同窗口重放整行覆盖、绝不累加」的唯一防线
			// （README「窗口、水位与迟到」，WriteMetricWindowReq 契约注释同源）。
			uniqueKeys: map[string][]string{"uniq_metric": {
				"subject_type", "subject_id", "metric_key", "metric_version", "window_type", "window_start",
			}},
			requiredIndexes: []string{
				"idx_metric_window", // buildHotQuery / ListHot / CountHot / DeleteByWindow
				"idx_metric_latest", // FindLatestWindowStart 的 MAX(window_start) 兜底
			},
			nullable:          []string{},
			requiredNoDefault: []string{"metric_key", "metric_version"},
		},
		{
			table:         "spm_window_watermark",
			file:          "000002_create_spm_metric_projection_tables.sql",
			row:           WindowWatermark{},
			selectColumns: windowWatermarkColumns,
			primaryKey:    []string{"id"},
			// Advance 的单调推进与 window_start=0 的解析都按这四列定位一行水位。
			uniqueKeys: map[string][]string{"uniq_watermark": {
				"subject_type", "metric_key", "metric_version", "window_type",
			}},
			requiredIndexes: []string{
				"idx_metric_version", // DeleteByMetricVersion：口径退役清理（uniq 前导列用不上）
			},
			nullable:          []string{},
			requiredNoDefault: []string{"metric_key", "metric_version"},
		},
		{
			table:         "spm_content_projection",
			file:          "000002_create_spm_metric_projection_tables.sql",
			row:           ContentProjection{},
			selectColumns: contentProjectionColumns,
			primaryKey:    []string{"id"},
			// 投影只保留最新状态：uniq_subject + Apply 的 event_time 乱序守卫。
			uniqueKeys: map[string][]string{"uniq_subject": {"subject_type", "subject_id"}},
			requiredIndexes: []string{
				"idx_zone_subject", // 分区榜重建 / 「某分区还有哪些在架内容」
				"idx_state",        // CountHidden
			},
			nullable:          []string{},
			requiredNoDefault: []string{},
		},
		{
			table:         "spm_user_interest",
			file:          "000003_create_spm_projection_and_job_tables.sql",
			row:           UserInterest{},
			selectColumns: userInterestColumns,
			// INSERT 列清单内联在 ReplaceForMid 的 query 字面量里（无独立常量），
			// 由 TestInlineInsertListsMatchMigration 直接从源码解析比对，不在此手抄。
			insertRowLen: interestRowLen,
			primaryKey:   []string{"id"},
			uniqueKeys: map[string][]string{"uniq_interest": {
				"mid", "metric_version", "interest_key",
			}},
			requiredIndexes: []string{
				"idx_event_time", // CountStaleMids：画像过期观测
			},
			nullable:          []string{},
			requiredNoDefault: []string{"metric_version", "interest_key"},
		},
		{
			table:         "spm_retention_cohort",
			file:          "000003_create_spm_projection_and_job_tables.sql",
			row:           RetentionCohort{},
			selectColumns: retentionCohortColumns,
			insertColumns: retentionCohortInsertCols,
			insertRowLen:  retentionRowLen,
			primaryKey:    []string{"id"},
			// 一条留存点由「分桶类型 × 分桶日 × 分区 × 口径版本 × 第 N 日」唯一确定。
			uniqueKeys: map[string][]string{"uniq_cohort": {
				"cohort_type", "cohort_date", "zone_id", "metric_version", "day_offset",
			}},
			requiredIndexes: []string{
				"idx_cohort_date", // DeleteExpired：按分桶日区间分批清理
			},
			nullable:          []string{},
			requiredNoDefault: []string{"metric_version"},
		},
		{
			table:         "spm_aggregation_job",
			file:          "000003_create_spm_projection_and_job_tables.sql",
			row:           AggregationJob{},
			selectColumns: aggregationJobColumns,
			primaryKey:    []string{"id"},
			// uniq_request_id 是「重复提交回放到同一作业」的幂等键；
			// InsertIfAbsent 不回填主键，logic 因此必须按它回读（README「作业与租约语义」）。
			uniqueKeys: map[string][]string{"uniq_request_id": {"request_id"}},
			requiredIndexes: []string{
				"idx_claim",       // ClaimPending：PENDING / 租约过期的 RUNNING
				"idx_claimed_by",  // 按令牌回读刚领到的作业
				"idx_type_ctime",  // ListAggregationJobs(job_type)
				"idx_state_ctime", // ListAggregationJobs(state)
			},
			nullable:          []string{},
			requiredNoDefault: []string{"request_id"},
		},
	}
}

// expectedIndexColumns 写的是「model 的 WHERE/ORDER BY 实际需要什么」，不是「SQL 里有什么」，
// 所以索引改名、丢列、换序都会在这里报错，而不是等到线上 filesort。
func expectedIndexColumns() map[string][]string {
	return map[string][]string{
		// spm_consumer_offset
		"idx_state_next_retry": {"state", "next_retry_at"}, // ListOverdueRetry
		"idx_topic_state":      {"topic", "state"},         // Summarize / CountByState / 死信 List
		"idx_state_ctime":      {"state", "ctime"},         // DeleteSettledBefore / CountOpen / 作业列表
		// spm_behavior_event（AggregateWindow / ScanForRetention / DeleteExpired）
		"idx_event_type_occurred":   {"event_type", "occurred_at"},
		"idx_content_occurred":      {"content_type", "content_id", "occurred_at"},
		"idx_aid_occurred":          {"aid", "occurred_at"},
		"idx_zone_occurred":         {"zone_id", "occurred_at"},
		"idx_catalog_item_occurred": {"catalog_item_id", "occurred_at"},
		"idx_target_mid_occurred":   {"target_mid", "occurred_at"},
		"idx_mid_occurred":          {"mid", "occurred_at"},
		"idx_mid_event_day":         {"mid", "event_day"},
		"idx_ctime":                 {"ctime"},
		// spm_metric_definition
		"idx_request_id": {"request_id"},
		"idx_key_state":  {"metric_key", "state"},
		// spm_metric_window：buildHotQuery 的等值前缀是 (metric_key, metric_version,
		// window_type, window_start)，随后按 metric_value 倒序出榜；
		// FindLatestWindowStart 的等值前缀是 (metric_key, metric_version,
		// subject_type, window_type) 再取 MAX(window_start)。
		"idx_metric_window": {"metric_key", "metric_version", "window_type", "window_start",
			"subject_type", "metric_value"},
		"idx_metric_latest": {"metric_key", "metric_version", "subject_type", "window_type",
			"window_start"},
		// spm_window_watermark
		"idx_metric_version": {"metric_key", "metric_version"},
		// spm_content_projection：buildHotQuery 的 LEFT JOIN 命中 uniq_subject，
		// 分区过滤再走 idx_zone_subject 的 (zone_id, subject_type) 前缀 + state。
		"idx_zone_subject": {"zone_id", "subject_type", "state"},
		"idx_state":        {"state"},
		// spm_user_interest
		"idx_event_time": {"event_time"},
		// spm_retention_cohort
		"idx_cohort_date": {"cohort_date"},
		// spm_aggregation_job：ClaimPending 的谓词是 (state=1) OR (state=2 AND lease_until<=now)，
		// 两条都按 (state, lease_until) 前缀命中，再按 job_type 收敛；
		// 列表按 id DESC 排序，二级索引隐含主键尾部，因此 (job_type, ctime)/(state, ctime)
		// 足以定位扫描区间。
		"idx_claim":      {"state", "lease_until", "job_type"},
		"idx_claimed_by": {"claimed_by", "state", "lease_until"},
		"idx_type_ctime": {"job_type", "ctime"},
	}
}

// subjectColumnIndex 声明「AggregateWindow 可分组的主体列」由哪条索引支撑：
// model.allowedSubjectColumns 是分组列白名单，白名单里的每一列都必须有 (列, occurred_at)
// 形态的索引兜着——否则加分组维度等于给事实表加一次全表扫。
// content_id 走 (content_type, content_id, occurred_at)：按内容主键分组必然同时限定
// content_type（requiresContentType），所以前导列是 content_type。
var subjectColumnIndex = map[string]string{
	"aid":             "idx_aid_occurred",
	"zone_id":         "idx_zone_occurred",
	"mid":             "idx_mid_occurred",
	"catalog_item_id": "idx_catalog_item_occurred",
	"target_mid":      "idx_target_mid_occurred",
	"content_id":      "idx_content_occurred",
}

// aggregateRequiredIndex 是 AggregateWindow / ScanForRetention 无条件依赖的索引。
var aggregateRequiredIndex = "idx_event_type_occurred"

// --- SQL 解析 ---

type sqlColumn struct {
	name          string
	dataType      string
	notNull       bool
	hasDefault    bool
	autoIncrement bool
	hasComment    bool
}

type sqlTable struct {
	table      string
	file       string
	columns    []sqlColumn
	primary    []string
	indexes    map[string][]string
	indexOrder []string
	uniqueKeys []string
	options    string
}

func (t *sqlTable) hasColumn(name string) bool { return t.column(name) != nil }

func (t *sqlTable) column(name string) *sqlColumn {
	for i := range t.columns {
		if t.columns[i].name == name {
			return &t.columns[i]
		}
	}
	return nil
}

// findMigrationsDir 从当前目录向上找仓库根的 deploy/migrations/spm。
// 不写死相对层数：`go test ./services/spm/model` 与在 model 目录内直接跑 cwd 不同。
func findMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "deploy", "migrations", "spm")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("找不到 %s：迁移文件缺失或目录改名（从 %s 向上找过）", migrationsRelDir, dir)
		}
		dir = parent
	}
}

// parsedFile 把文件切成「注释头」与「可执行 SQL 行」两部分。
// 所有禁止项检查只看可执行行：头注释里正常会出现「不建 FOREIGN KEY」「不允许 TRUNCATE」
// 这类反向说明，按全文匹配会把合规文件判成违规。
type parsedFile struct {
	header string
	sql    []string
}

func parseFile(t *testing.T, path string) parsedFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var header, sql []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "--"):
			header = append(header, trimmed)
		default:
			sql = append(sql, trimmed)
		}
	}
	return parsedFile{header: strings.Join(header, "\n"), sql: sql}
}

// parseCreateTables 从迁移文件里抽出全部 CREATE TABLE 块的结构信息。
// 只认本仓库既有书写风格（反引号标识符、每列一行、) ENGINE=... 收尾、
// COMMENT 可以单独占一行）。格式被改坏时报错，而不是悄悄解析出一张空表。
func parseCreateTables(t *testing.T, dir string) map[string]*sqlTable {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("枚举迁移文件失败: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("%s 下没有任何 .sql —— 迁移缺失", migrationsRelDir)
	}
	out := map[string]*sqlTable{}
	for _, p := range paths {
		pf := parseFile(t, p)
		base := filepath.Base(p)
		var cur *sqlTable
		for i := 0; i < len(pf.sql); i++ {
			trimmed := pf.sql[i]
			switch {
			case cur == nil && strings.HasPrefix(trimmed, "CREATE TABLE"):
				name, ok := identifierIn(trimmed, "CREATE TABLE IF NOT EXISTS")
				if !ok {
					t.Fatalf("%s: CREATE TABLE 无法解析表名: %q", base, trimmed)
				}
				if !strings.HasSuffix(trimmed, "(") {
					t.Fatalf("%s: 表 %s 的 CREATE TABLE 未以 ( 结尾，风格不符无法解析", base, name)
				}
				if _, dup := out[name]; dup {
					t.Fatalf("%s: 表 %s 被重复建（一张表只能出现在一个迁移文件里）", base, name)
				}
				cur = &sqlTable{table: name, file: base, indexes: map[string][]string{}}
			case cur != nil && strings.HasPrefix(trimmed, ")"):
				// 表尾：) ENGINE=... 之后可能另起一行写 COMMENT='...'（本目录的实际写法）。
				options := trimmed
				for !strings.HasSuffix(options, ";") && i+1 < len(pf.sql) {
					i++
					options += " " + pf.sql[i]
				}
				if !strings.HasSuffix(options, ";") {
					t.Fatalf("%s: 表 %s 的建表语句没有以 ; 收尾", base, cur.table)
				}
				cur.options = options
				out[cur.table] = cur
				cur = nil
			case cur != nil:
				parseTableLine(t, cur, base, trimmed)
			default:
				t.Fatalf("%s: 出现 CREATE TABLE 之外的可执行语句 %q —— 本目录只建表，数据变更要另开文件",
					base, trimmed)
			}
		}
		if cur != nil {
			t.Fatalf("%s: CREATE TABLE 块没有收尾的 ) 行", base)
		}
	}
	return out
}

// parseTableLine 解析表体里的一行（列定义 / 主键 / 唯一键 / 普通索引）。
func parseTableLine(t *testing.T, cur *sqlTable, file, line string) {
	t.Helper()
	body := strings.TrimSuffix(line, ",")
	switch {
	case strings.HasPrefix(body, "`"):
		name, ok := identifierIn(body, "")
		if !ok {
			t.Fatalf("%s: 列定义无法解析: %q", file, line)
		}
		if cur.hasColumn(name) {
			t.Fatalf("%s: 表 %s 的列 %s 重复声明", file, cur.table, name)
		}
		// 只在 COMMENT 之前的定义段上判定类型与约束：
		// 中文注释里出现「NULL」「JSON」「TEXT」「默认」字样不能影响结构判定。
		defined := body
		if i := strings.Index(body, " COMMENT '"); i >= 0 {
			defined = body[:i]
		}
		cur.columns = append(cur.columns, sqlColumn{
			name:          name,
			dataType:      defined,
			notNull:       strings.Contains(defined, " NOT NULL"),
			hasDefault:    strings.Contains(defined, " DEFAULT "),
			autoIncrement: strings.Contains(defined, " AUTO_INCREMENT"),
			hasComment:    strings.Contains(body, " COMMENT '"),
		})
	case strings.HasPrefix(body, "PRIMARY KEY"):
		cols := columnListInParentheses(t, file, body)
		if len(cur.primary) > 0 {
			t.Fatalf("%s: 表 %s 声明了多个 PRIMARY KEY", file, cur.table)
		}
		cur.primary = cols
		for _, c := range cols {
			if !cur.hasColumn(c) {
				t.Fatalf("%s: 主键引用了未声明的列 %s", file, c)
			}
		}
	case strings.HasPrefix(body, "UNIQUE KEY"):
		name, _ := identifierAfterKeyword(body, "UNIQUE KEY")
		cur.addIndex(t, file, name, columnListInParentheses(t, file, body), true)
	case strings.HasPrefix(body, "KEY"):
		name, _ := identifierAfterKeyword(body, "KEY")
		cur.addIndex(t, file, name, columnListInParentheses(t, file, body), false)
	default:
		t.Fatalf("%s: 表 %s 内出现无法识别的定义行（FOREIGN KEY / CHECK / 生成列都不符合本服务约定，需先确认）: %q",
			file, cur.table, line)
	}
}

func (t *sqlTable) addIndex(t2 *testing.T, file, name string, cols []string, unique bool) {
	t2.Helper()
	if name == "" {
		t2.Fatalf("%s: 索引缺名字: %q", file, cols)
	}
	if _, dup := t.indexes[name]; dup {
		t2.Fatalf("%s: 索引名 %s 重复声明", file, name)
	}
	t.indexes[name] = cols
	t.indexOrder = append(t.indexOrder, name)
	if unique {
		t.uniqueKeys = append(t.uniqueKeys, name)
	}
	for _, c := range cols {
		if !t.hasColumn(c) {
			t2.Fatalf("%s: 索引 %s 引用了未声明的列 %s", file, name, c)
		}
	}
}

// identifierIn 取形如 `<prefix> \`name\“ 里的 name；prefix 为空表示从行首开始。
func identifierIn(line, prefix string) (string, bool) {
	rest := line
	if prefix != "" {
		idx := strings.Index(rest, prefix)
		if idx < 0 {
			return "", false
		}
		rest = rest[idx+len(prefix):]
	}
	start := strings.Index(rest, "`")
	if start < 0 {
		return "", false
	}
	end := strings.Index(rest[start+1:], "`")
	if end < 0 {
		return "", false
	}
	return rest[start+1 : start+1+end], true
}

// identifierAfterKeyword 取 `UNIQUE KEY \`name\“ / `KEY \`name\“ 里的索引名。
func identifierAfterKeyword(line, keyword string) (string, bool) {
	idx := strings.Index(line, keyword)
	if idx < 0 {
		return "", false
	}
	return identifierIn(line[idx+len(keyword):], "")
}

// columnListInParentheses 解析定义行里最后一对括号中的列清单。
func columnListInParentheses(t *testing.T, file, line string) []string {
	t.Helper()
	open := strings.LastIndex(line, "(")
	closing := strings.LastIndex(line, ")")
	if open < 0 || closing < 0 || closing < open {
		t.Fatalf("%s: 索引定义缺括号: %q", file, line)
	}
	parts := splitTopLevel(line[open+1 : closing])
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		name, ok := identifierIn(strings.TrimSpace(p), "")
		if !ok {
			t.Fatalf("%s: 索引列必须是反引号标识符（带 sort 方向或前缀长度的写法无法比对，请改写）: %q", file, p)
		}
		out = append(out, name)
	}
	return out
}

// splitTopLevel 按「括号外的逗号」切分，让 COALESCE(a, ”) 这种函数表达式算作一项。
func splitTopLevel(s string) []string {
	var (
		out   []string
		depth int
		start int
	)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// selectColumnNames 取 SELECT 列常量实际产出的列名（与结构体 db tag 比对用）：
// `COALESCE(x, ”) AS y` 产出 y，`a.b` 产出 b，裸列名原样。
func selectColumnNames(selectCols string) []string {
	parts := splitTopLevel(selectCols)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		item := strings.TrimSpace(p)
		if item == "" {
			continue
		}
		if i := strings.LastIndex(strings.ToUpper(item), " AS "); i >= 0 {
			out = append(out, strings.TrimSpace(item[i+len(" AS "):]))
			continue
		}
		if i := strings.LastIndex(item, "."); i >= 0 {
			out = append(out, strings.TrimSpace(item[i+1:]))
			continue
		}
		out = append(out, item)
	}
	return out
}

// --- 门禁用例 ---

// TestModelColumnsMatchMigrationColumns 逐列双向比对 model 结构体与迁移表定义：
//   - model 用到而迁移没有 -> 运行期 Unknown column；
//   - 迁移有而 model 不读写 -> 必须在 sqlOnly 里登记理由，否则是无人维护的死列。
func TestModelColumnsMatchMigrationColumns(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl, ok := tables[spec.table]
		if !ok {
			t.Fatalf("迁移里没有表 %s（应在 %s）", spec.table, spec.file)
		}
		if tbl.file != spec.file {
			t.Errorf("%s: 实际建表文件是 %s，model 头注写的是 %s", spec.table, tbl.file, spec.file)
		}
		modelCols := dbTagColumns(t, spec.row)

		// SELECT 列常量必须与结构体 db tag 同集合：sqlx 按列名映射，
		// 漏写一列会让该字段永远读成零值（比报错更难查）。
		// 这里只比集合不比顺序：本表刻意把 payload 排在末尾
		// （consumerOffsetFullColumns = 轻量列 + COALESCE(payload)），
		// 顺序由 TestMigrationColumnOrderMatchesModelStruct 按「迁移 == 结构体」单独钉。
		sel := selectColumnNames(spec.selectColumns)
		if dup := duplicated(sel); len(dup) > 0 {
			t.Errorf("%s: SELECT 列常量里 %v 重复出现，映射结果会以最后一次为准", spec.table, dup)
		}
		if got, want := strings.Join(sortedNames(sel), ","), strings.Join(sortedNames(modelCols), ","); got != want {
			t.Errorf("%s: model 的 SELECT 列常量与结构体 db tag 不一致\n SELECT=%s\n   tags=%s",
				spec.table, got, want)
		}

		sqlSet := map[string]bool{}
		for _, c := range tbl.columns {
			sqlSet[c.name] = true
		}
		var missingInSQL []string
		modelSet := map[string]bool{}
		for _, c := range modelCols {
			modelSet[c] = true
			if !sqlSet[c] {
				missingInSQL = append(missingInSQL, c)
			}
		}
		if len(missingInSQL) > 0 {
			t.Errorf("%s（%s）: model 用到但迁移缺这些列，运行期会 Unknown column: %v",
				spec.table, tbl.file, missingInSQL)
		}
		for _, c := range tbl.columns {
			if modelSet[c.name] {
				continue
			}
			reason, declared := spec.sqlOnly[c.name]
			if !declared || strings.TrimSpace(reason) == "" {
				t.Errorf("%s: 列 %s 在迁移里存在但 model 完全不读写，且未登记保留理由（要么删列，要么在 sqlOnly 写明用途）",
					spec.table, c.name)
			}
		}
	}
}

// TestMigrationColumnOrderMatchesModelStruct 锁列序：迁移列顺序 = 结构体字段顺序，
// 这样「逐列比对」肉眼可做，排障时的 SELECT * 也不会错位。
func TestMigrationColumnOrderMatchesModelStruct(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		modelCols := dbTagColumns(t, spec.row)
		if len(tbl.columns) != len(modelCols) {
			continue // 数量差异由上一个用例报，这里不重复刷噪音
		}
		for i, c := range tbl.columns {
			if c.name != modelCols[i] {
				t.Errorf("%s: 第 %d 列顺序不一致，迁移=%s model=%s", spec.table, i+1, c.name, modelCols[i])
			}
		}
	}
}

// TestInsertColumnListMatchesRowLenAndMigration 校验 INSERT 列清单三件事：
//  1. 手写的「单行占位符数」常量 == 列清单的实际列数（抄错就是
//     "column count doesn't match value count"）；
//  2. INSERT 列必须是迁移列的真子集，且不含自增主键（自增列出现在显式 INSERT 清单里
//     意味着调用方在写 0，主键序列会被污染）；
//  3. 列顺序必须是迁移声明顺序的保序子序列——这条是**可读性约定**而不是正确性要求
//     （显式列清单按名字映射，见 insert_sql_source_test.go 的说明）：
//     守住它是为了让「逐列比对 SQL 与 model」这件事肉眼可做，排障时不必来回换算。
func TestInsertColumnListMatchesRowLenAndMigration(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		if spec.insertColumns == "" {
			continue // 该表没有批量 INSERT（单行 INSERT 的列清单在各 method 内联）
		}
		cols := selectColumnNames(spec.insertColumns)
		if len(cols) != spec.insertRowLen {
			t.Errorf("%s: 列清单有 %d 列，但 rowLen 常量是 %d —— 多值 INSERT 的占位符总数会与参数总数错位: %v",
				spec.table, len(cols), spec.insertRowLen, cols)
		}
		tbl := tables[spec.table]
		if contains(cols, "id") {
			t.Errorf("%s: 显式 INSERT 清单含自增主键 id", spec.table)
		}
		var pos []int
		for _, c := range cols {
			idx := -1
			for i := range tbl.columns {
				if tbl.columns[i].name == c {
					idx = i
					break
				}
			}
			if idx < 0 {
				t.Errorf("%s: INSERT 列 %s 在迁移里不存在", spec.table, c)
				continue
			}
			pos = append(pos, idx)
		}
		for i := 1; i < len(pos); i++ {
			if pos[i] <= pos[i-1] {
				t.Errorf("%s: INSERT 列 %s 出现在 %s 之后，但迁移里它的声明顺序更靠前"+
					"（列清单按名字映射，这条是可读性约定，见用例注释第 3 条）",
					spec.table, cols[i], cols[i-1])
			}
		}
	}
}

// TestAggregateSubjectColumnsAreIndexed 校验 AggregateWindow 的分组列白名单
// 与 spm_behavior_event 的索引一一对应：白名单加一列而迁移不加索引，
// 等于给最热的写多表加一次全表扫描。
func TestAggregateSubjectColumnsAreIndexed(t *testing.T) {
	tables := loadMigrationTables(t)
	tbl := tables["spm_behavior_event"]
	for _, spec := range tableSpecs() {
		if spec.table != "spm_behavior_event" {
			continue
		}
		for col, idxName := range subjectColumnIndex {
			if _, ok := allowedSubjectColumns[col]; !ok {
				t.Errorf("allowedSubjectColumns 已不含 %s，但 subjectColumnIndex 仍登记着它（映射表该删）", col)
			}
			if !tbl.hasColumn(col) {
				t.Errorf("分组列 %s 在 spm_behavior_event 迁移里不存在", col)
			}
			got, ok := tbl.indexes[idxName]
			if !ok {
				t.Errorf("分组列 %s 依赖索引 %s，但它不存在（实际索引 %v）", col, idxName, tbl.indexOrder)
				continue
			}
			if want := expectedIndexColumns()[idxName]; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("spm_behavior_event.%s: 实际列 (%s) 与期望 (%s) 不一致",
					idxName, strings.Join(got, ","), strings.Join(want, ","))
			}
			if !contains(got, col) {
				t.Errorf("索引 %s(%v) 不含分组列 %s —— 按它分组会退化成全表扫", idxName, got, col)
			}
			if !contains(spec.requiredIndexes, idxName) {
				t.Errorf("%s: 索引 %s 未登记进 requiredIndexes", spec.table, idxName)
			}
		}
		// 白名单里每一列都必须有上面的映射，否则新增分组维度是无人认领的。
		for col := range allowedSubjectColumns {
			if _, ok := subjectColumnIndex[col]; !ok {
				t.Errorf("allowedSubjectColumns 含 %s，但 subjectColumnIndex 没登记它的支撑索引", col)
			}
		}
		if _, ok := tbl.indexes[aggregateRequiredIndex]; !ok {
			t.Errorf("AggregateWindow 的公共条件 event_type IN (...) AND occurred_at BETWEEN 依赖 %s，但它不存在",
				aggregateRequiredIndex)
		}
	}
}

// TestPrimaryAndUniqueKeysMatchModelDependencies 校验幂等写入与数据库级不变量依赖的键。
func TestPrimaryAndUniqueKeysMatchModelDependencies(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		if got, want := strings.Join(tbl.primary, ","), strings.Join(spec.primaryKey, ","); got != want {
			t.Errorf("%s: 主键是 (%s)，model 依赖 (%s)。改主键等于改 FindByID/Update 的定位语义",
				spec.table, got, want)
		}
		for name, wantCols := range spec.uniqueKeys {
			got, ok := tbl.indexes[name]
			if !ok {
				t.Errorf("%s: 缺唯一索引 %s（列组合 %v）——它是幂等的最终防线，不能只靠 logic 预检",
					spec.table, name, wantCols)
				continue
			}
			if strings.Join(got, ",") != strings.Join(wantCols, ",") {
				t.Errorf("%s.%s: 列组合是 (%s)，期望 (%s)。uniq_metric/uniq_watermark 少一列就会把「同窗口重放」变成累加",
					spec.table, name, strings.Join(got, ","), strings.Join(wantCols, ","))
			}
		}
		// 反向：SQL 里多出来的唯一键必须登记。uniq_request_id 之外再加一条唯一键，
		// 会让 InsertIfAbsent 的 affected 判定失真（幂等回放被读成「新作业」）。
		for _, name := range tbl.uniqueKeys {
			if _, ok := spec.uniqueKeys[name]; !ok {
				t.Errorf("%s: 唯一索引 %s 未被任何 model 依赖登记（要么删，要么在 uniqueKeys 写明它保证哪条不变量）",
					spec.table, name)
			}
		}
	}
}

// TestModelIndexesExistInMigration 校验 model 的每条查询路径都有索引支撑，
// 反向也查：SQL 里声明了却没有任何 model 查询依赖的索引属于纯写放大。
func TestModelIndexesExistInMigration(t *testing.T) {
	tables := loadMigrationTables(t)
	expected := expectedIndexColumns()
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		for _, idx := range spec.requiredIndexes {
			got, ok := tbl.indexes[idx]
			if !ok {
				t.Errorf("%s（%s）: model 依赖索引 %s 不存在，实际索引 %v ——查询会退化成全表扫",
					spec.table, tbl.file, idx, tbl.indexOrder)
				continue
			}
			want, declared := expected[idx]
			if !declared || len(want) == 0 {
				t.Errorf("%s: 索引 %s 未在 expectedIndexColumns 登记它支撑哪条查询", spec.table, idx)
				continue
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s.%s: 实际列 (%s) 与 model 查询所需 (%s) 不一致",
					spec.table, idx, strings.Join(got, ","), strings.Join(want, ","))
			}
		}
		for _, idx := range tbl.indexOrder {
			// 唯一键承载的是不变量（幂等键、口径版本、主体唯一），不是查询加速，
			// 即使没有任何 WHERE 依赖它也必须存在。
			if _, isUnique := spec.uniqueKeys[idx]; isUnique {
				continue
			}
			if !contains(spec.requiredIndexes, idx) {
				t.Errorf("%s: 索引 %s(%v) 既不是唯一键也没有任何 model 查询依赖它，属于纯写放大，应删除或说明用途",
					spec.table, idx, tbl.indexes[idx])
			}
		}
	}
}

// TestNullabilityAndDefaultDiscipline 校验可空性与默认值口径。
//
// 两条约定与本服务的具体理由：
//  1. 除 spm_dead_letter.event_id（靠 NULL 逃逸唯一性，坏消息才各留一行）与
//     spm_consumer_offset.payload（成功行清空原文，NULL 才是「原文已清走」）外，
//     所有列一律 NOT NULL，用哨兵值（0 / 空串）表达「无」。NULL 会让
//     GREATEST/COALESCE 之外的比较静默变三值逻辑，sqlx 也会在非空 string 字段上
//     报 "converting NULL to string" 让整条列表查询失败。
//  2. NOT NULL 列应当有 DEFAULT，**除非**它是自增主键、主键本身，或登记在
//     requiredNoDefault 里的幂等键/枚举列（event_id、metric_key+metric_version、
//     request_id、interest_key…）。这些列刻意不给 DEFAULT：model 的 INSERT 总是显式给值，
//     漏给时 MySQL 严格模式报错远好于静默兜底 ——
//     request_id 兜成空串会让 uniq_request_id 把所有无键作业并成一条（README「作业与租约语义」），
//     uniq_metric_version 兜成 (key,0) 会让「未指定版本」的口径互相撞键。
//     反过来，可空的载荷列必须有 DEFAULT，否则局部更新写不动。
func TestNullabilityAndDefaultDiscipline(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		nullableOK := specSet(spec.nullable)
		requiredNoDefault := specSet(spec.requiredNoDefault)
		primary := specSet(tbl.primary)
		for _, c := range tbl.columns {
			if !c.notNull && !nullableOK[c.name] {
				t.Errorf("%s.%s: 允许 NULL，见本用例注释第 1 条；若确需可空请把列名加进 nullable 并写明理由", spec.table, c.name)
			}
			exempt := c.autoIncrement || primary[c.name] || requiredNoDefault[c.name]
			if c.notNull && !c.hasDefault && !exempt {
				t.Errorf("%s.%s: NOT NULL 且无 DEFAULT，又不属于自增主键/主键/requiredNoDefault 三类必填列。实际定义 %q",
					spec.table, c.name, c.dataType)
			}
			if requiredNoDefault[c.name] && !c.notNull {
				t.Errorf("%s.%s: 登记为必填列但实际可空", spec.table, c.name)
			}
			if requiredNoDefault[c.name] && c.hasDefault {
				t.Errorf("%s.%s: 登记为必填列（漏传应当报错）却给了 DEFAULT，静默兜底会吞掉调用方 bug", spec.table, c.name)
			}
			// MySQL 禁止 TEXT/BLOB 设默认值：这类列必须走 requiredNoDefault。
			if isTextType(c.dataType) && c.hasDefault {
				t.Errorf("%s.%s: TEXT/BLOB 不能设 DEFAULT（MySQL 会直接报错），实际 %q", spec.table, c.name, c.dataType)
			}
		}
		for _, name := range tbl.uniqueKeys {
			for _, col := range tbl.indexes[name] {
				if nullableOK[col] {
					continue
				}
				if c := tbl.column(col); c != nil && !c.notNull {
					t.Errorf("%s.%s: 唯一索引列 %s 可空，唯一性会静默失效", spec.table, name, col)
				}
			}
		}
	}
}

// TestColumnWidthsMatchModelTruncate 校验「model 的截断长度 == 列宽」：
// 列宽 64、truncate(_, 100) 会静默放行超长值给 MySQL（严格模式报错、非严格模式截断），
// 而 model 以为「我已经截过了」。helpers.go 的列宽常量注释也明确写着
// 「与 deploy/migrations/spm 的 VARCHAR 宽度一致」。
func TestColumnWidthsMatchModelTruncate(t *testing.T) {
	tables := loadMigrationTables(t)
	cases := []struct {
		table  string
		column string
		want   int
	}{
		{"spm_metric_definition", "metric_key", maxMetricKeyLen},
		{"spm_metric_window", "metric_key", maxMetricKeyLen},
		{"spm_metric_definition", "name", 100},
		{"spm_metric_definition", "formula", 500},
		{"spm_metric_definition", "unit", 32},
		{"spm_metric_definition", "source_event_types", 255},
		{"spm_metric_definition", "description", 500},
		{"spm_metric_definition", "created_by", 64},
		{"spm_metric_definition", "request_id", 128},
		{"spm_metric_window", "write_request_id", 128},
		{"spm_window_watermark", "update_request_id", 128},
		{"spm_user_interest", "interest_key", 100},
		{"spm_aggregation_job", "request_id", 128},
		{"spm_aggregation_job", "operator", 64},
		{"spm_aggregation_job", "reason", 500},
		{"spm_aggregation_job", "last_error", 512},
		{"spm_aggregation_job", "claimed_by", 64},
		{"spm_dead_letter", "payload_digest", 80},
		{"spm_dead_letter", "payload_preview", 512},
		{"spm_dead_letter", "reason", 512},
		{"spm_dead_letter", "operator", 64},
		{"spm_consumer_offset", "last_error", 512},
		{"spm_behavior_event", "action_key", 32},
		{"spm_behavior_event", "pseudonym", 128},
		{"spm_behavior_event", "pseudonym_kind", 32},
		{"spm_behavior_event", "dim_key", 64},
		{"spm_behavior_event", "dim_value", 128},
		{"spm_behavior_event", "trace_id", 64},
		{"spm_behavior_event", "topic", 128},
		{"spm_content_projection", "last_event_id", 64},
	}
	for _, tc := range cases {
		tbl, ok := tables[tc.table]
		if !ok {
			t.Fatalf("迁移里没有表 %s", tc.table)
		}
		c := tbl.column(tc.column)
		if c == nil {
			t.Errorf("%s.%s: 列不存在", tc.table, tc.column)
			continue
		}
		got, ok := varcharLen(c.dataType)
		if !ok {
			t.Errorf("%s.%s: 不是带长度的 VARCHAR，无法比对截断长度，实际 %q", tc.table, tc.column, c.dataType)
			continue
		}
		if got < tc.want {
			t.Errorf("%s.%s: 列宽 %d < model 允许写入的 %d 字节，超长值会被 MySQL 拒绝或截断",
				tc.table, tc.column, got, tc.want)
		}
	}
	// 反向：本用例清单必须覆盖每一张带截断写入的表，防止新增列时被静默漏掉。
	if len(cases) < 25 {
		t.Fatalf("列宽比对用例只有 %d 条，明显漏了列：新增 VARCHAR 列时必须一起登记", len(cases))
	}
}

// TestEveryColumnCommentedAndTableOptions 校验列注释与表选项。
func TestEveryColumnCommentedAndTableOptions(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		for _, c := range tbl.columns {
			if !c.hasComment {
				t.Errorf("%s.%s: 缺 COMMENT —— 本仓库要求每列有注释", spec.table, c.name)
			}
		}
		for _, want := range []string{
			"ENGINE=InnoDB", "DEFAULT CHARSET=utf8mb4", "COLLATE=utf8mb4_unicode_ci", "COMMENT='",
		} {
			if !strings.Contains(tbl.options, want) {
				t.Errorf("%s: 表选项缺 %s，实际 %q", spec.table, want, tbl.options)
			}
		}
	}
}

// TestMigrationHeaderSections 校验文件头：库名 / 数据所有者 / 回滚 / 锁风险 四段必须在，
// 且回滚段要为该文件里每一张表给出 DROP（AGENTS.md §10 要求回滚步骤可查）。
func TestMigrationHeaderSections(t *testing.T) {
	dir := findMigrationsDir(t)
	tables := parseCreateTables(t, dir)
	byFile := map[string][]string{}
	for _, tbl := range tables {
		byFile[tbl.file] = append(byFile[tbl.file], tbl.table)
	}
	if len(byFile) == 0 {
		t.Fatalf("没有解析出任何表")
	}
	for _, spec := range tableSpecs() {
		byFile[spec.file] = append(byFile[spec.file], spec.table)
	}
	for file, wantTables := range byFile {
		pf := parseFile(t, filepath.Join(dir, file))
		for _, want := range []string{"库名", "数据所有者", "回滚", "锁风险"} {
			if !strings.Contains(pf.header, want) {
				t.Errorf("%s: 文件头缺「%s」段", file, want)
			}
		}
		if !strings.Contains(pf.header, spmDBName) {
			t.Errorf("%s: 文件头未写明库名 %s（与 etc 的 DataSource 必须同库）", file, spmDBName)
		}
		if !strings.Contains(pf.header, "spm") {
			t.Errorf("%s: 文件头未写明数据所有者是 spm", file)
		}
		// 回滚段逐表点名：只写「DROP 本文件」不够，回滚时要按表倒序执行。
		rollback := pf.header
		if i := strings.Index(rollback, "回滚"); i >= 0 {
			rollback = rollback[i:]
			if j := strings.Index(rollback, "锁风险"); j >= 0 {
				rollback = rollback[:j]
			}
		}
		for _, tbl := range uniqueNames(wantTables) {
			if !strings.Contains(rollback, "`"+tbl+"`") {
				t.Errorf("%s: 回滚段没给出 DROP TABLE %s 的语句", file, tbl)
			}
		}
	}
}

// TestMigrationsRepeatableAndForbidden 校验迁移只建表、可重复执行，
// 且可执行 SQL 里不出现被禁止的语句与被禁范围（AGENTS.md §1 商业化范围外）。
func TestMigrationsRepeatableAndForbidden(t *testing.T) {
	dir := findMigrationsDir(t)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	if len(paths) == 0 {
		t.Fatalf("%s 下没有迁移文件", migrationsRelDir)
	}
	forbiddenPrefix := []string{"ALTER TABLE", "DROP TABLE", "DELETE FROM", "UPDATE ", "INSERT INTO",
		"CREATE DATABASE", "DROP DATABASE", "TRUNCATE", "USE ", "CREATE INDEX", "RENAME", "GRANT", "SET "}
	// 范围外词汇（AGENTS.md §1）：会员/订单/支付/投币/广告/分成不得作为本服务的字段或语义。
	// 这里按「列名/表名」精确匹配，避免把中文说明里的「默认值」等词误判。
	forbiddenIdents := []string{"vip", "member", "order", "payment", "pay_", "coin", "advert",
		"campaign", "billing", "revenue", "sponsor", "reward", "miniapp", "miniprogram"}
	for _, p := range paths {
		pf := parseFile(t, p)
		base := filepath.Base(p)
		if len(pf.sql) == 0 {
			t.Errorf("%s: 没有可执行 SQL", base)
		}
		for _, line := range pf.sql {
			upper := strings.ToUpper(line)
			if strings.HasPrefix(upper, "CREATE TABLE IF NOT EXISTS") {
				continue // 可重复执行是本目录的硬要求
			}
			for _, bad := range forbiddenPrefix {
				if strings.HasPrefix(upper, bad) {
					t.Errorf("%s: 可执行语句以被禁止的 %q 开头: %q —— 迁移只建表；数据变更要另开 0000NN 文件并写明回滚",
						base, bad, line)
					break
				}
			}
		}
		for _, tbl := range parseCreateTables(t, dir) {
			for _, c := range tbl.columns {
				lower := strings.ToLower(c.name)
				for _, bad := range forbiddenIdents {
					if strings.Contains(lower, bad) {
						t.Errorf("%s.%s: 列名含范围外词汇 %q（AGENTS.md §1 商业化范围外）", tbl.table, c.name, bad)
					}
				}
			}
			if i := strings.Index(strings.ToUpper(tbl.options), "COMMENT='"); i >= 0 {
				comment := strings.ToLower(tbl.options[i:])
				for _, bad := range forbiddenIdents {
					if strings.Contains(comment, bad) {
						t.Errorf("%s: 表注释含范围外词汇 %q", tbl.table, bad)
					}
				}
			}
		}
		// 凭据字面量禁止入库（AGENTS.md §4/§6）。
		all := strings.ToUpper(pf.header + strings.Join(pf.sql, "\n"))
		for _, bad := range []string{"ACCESSKEY", "SECRET_KEY", "PRIVATE KEY", "-----BEGIN", "PASSWORD="} {
			if strings.Contains(all, bad) {
				t.Errorf("%s: 出现疑似凭据字面量 %q", base, bad)
			}
		}
		// 跨库外键禁止（AGENTS.md §5：本服务只拥有 go_video_spm）。
		if strings.Contains(all, "FOREIGN KEY") || strings.Contains(all, "REFERENCES") {
			t.Errorf("%s: 出现外键 —— 跨库一致性靠事件重放与 RecomputeMetrics，不建 FOREIGN KEY", base)
		}
	}
}

// TestMigrationFileNamesAndOrder 校验文件编号连续升序，且与 model 头注引用的文件清单一致。
func TestMigrationFileNamesAndOrder(t *testing.T) {
	dir := findMigrationsDir(t)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	got := make([]string, 0, len(paths))
	for _, p := range paths {
		got = append(got, filepath.Base(p))
	}
	sort.Strings(got)
	want := map[string]bool{}
	for _, s := range tableSpecs() {
		want[s.file] = true
	}
	wantList := make([]string, 0, len(want))
	for f := range want {
		wantList = append(wantList, f)
	}
	sort.Strings(wantList)
	if strings.Join(got, "|") != strings.Join(wantList, "|") {
		t.Errorf("迁移文件清单与 model 头注引用的不一致:\n 目录里=%v\n 期望=%v", got, wantList)
	}
	for i, f := range got {
		prefix := fmt.Sprintf("0000%02d_", i+1)
		if !strings.HasPrefix(f, prefix) {
			t.Errorf("第 %d 个文件 %s 的编号不是 %s：编号必须连续升序，回滚按它倒序执行", i+1, f, prefix)
		}
	}
	// 本服务声明 10 张自有表（rpc/spm.proto 头部「数据所有权」），一张都不能少。
	tables := parseCreateTables(t, dir)
	if len(tables) != 10 {
		t.Errorf("迁移共 %d 张表，契约声明 10 张自有表（rpc/spm.proto 头部）", len(tables))
	}
	if len(tableSpecs()) != 10 {
		t.Errorf("tableSpecs 共 %d 条，应为 10", len(tableSpecs()))
	}
}

// TestNoMigrationColumnLeftUnmodeled 把「迁移表全集」与「spec 声明的表全集」对齐：
// 新增一张表而不进 spec，等于它没有任何一致性门禁。
func TestNoMigrationColumnLeftUnmodeled(t *testing.T) {
	tables := loadMigrationTables(t)
	declared := map[string]bool{}
	for _, spec := range tableSpecs() {
		declared[spec.table] = true
	}
	for tbl := range tables {
		if !declared[tbl] {
			t.Errorf("迁移里有表 %s 但 tableSpecs 没登记它 —— 它的列/索引/键现在无人比对", tbl)
		}
	}
}

// --- helpers ---

func loadMigrationTables(t *testing.T) map[string]*sqlTable {
	t.Helper()
	return parseCreateTables(t, findMigrationsDir(t))
}

// dbTagColumns 按字段声明顺序取 db tag，即 model 读写的全部列。
func dbTagColumns(t *testing.T, row any) []string {
	t.Helper()
	rt := reflect.TypeOf(row)
	if rt.Kind() != reflect.Struct {
		t.Fatalf("%T 不是结构体", row)
	}
	cols := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("db")
		if tag == "" || tag == "-" {
			t.Errorf("%s.%s: 缺 db tag，sqlx 无法映射该字段", rt.Name(), rt.Field(i).Name)
			continue
		}
		cols = append(cols, strings.Split(tag, ",")[0])
	}
	return cols
}

func specSet(list []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range list {
		out[v] = true
	}
	return out
}

// sortedNames 返回排序后的副本，用于「只看集合不看顺序」的比对。
func sortedNames(list []string) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}

// duplicated 返回出现两次以上的名字（顺序稳定，报错可复现）。
func duplicated(list []string) []string {
	seen := map[string]int{}
	for _, v := range list {
		seen[v]++
	}
	var out []string
	for _, v := range sortedNames(list) {
		if seen[v] > 1 {
			out = append(out, v)
			seen[v] = 0 // 同名只报一次
		}
	}
	return out
}

// varcharLen 取列定义里的 VARCHAR(n) 长度。
func varcharLen(def string) (int, bool) {
	up := strings.ToUpper(def)
	i := strings.Index(up, "VARCHAR(")
	if i < 0 {
		return 0, false
	}
	rest := def[i+len("VARCHAR("):]
	j := strings.Index(rest, ")")
	if j < 0 {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(rest[:j], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

func isTextType(def string) bool {
	d := strings.ToUpper(def)
	for _, t := range []string{"TEXT", "BLOB", "JSON", "MEDIUMTEXT", "LONGTEXT"} {
		if strings.Contains(d, t) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func uniqueNames(list []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
