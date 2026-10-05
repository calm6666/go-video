package model

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件把「model 的结构体 / SELECT 列常量与 deploy/migrations/creator-revenue 的建表语句逐列一致」
// 变成可执行门禁（AGENTS.md §9），不连数据库：只读 SQL 文本与 Go 侧声明做双向比对。
//
// 为什么分成域尤其需要（本服务迁移尚未在 MySQL 实例上跑过）：
//   - 结构体 db tag、SELECT 列常量、INSERT 列清单是三处必须同步的文本，任一处漂移在运行期
//     变成 "Unknown column" 或字段扫描错位 —— 而错位的对象是「该付给作者多少钱」；
//   - 分成金额是 int64 分单位的**折算结果**：一旦有人把金额列改成 DECIMAL/FLOAT，
//     ComputeAmountMinor 的「整数除法向下取整、余数丢弃」口径立即失真，且误差方向不再恒定；
//   - uniq_rule_code / uniq_metric_key / uniq_active_period_mid / uniq_request_id 是幂等与
//     「一周期一人一单」的唯一防线，被删或被降级成普通 KEY 时表现不是启动失败，而是重复出单；
//   - 参与唯一性判定的字符列必须 utf8mb4_bin：表级 unicode_ci 会把只差大小写的 rule_code /
//     request_id 折叠成同一个键，表现为「规则串档」「幂等键互相吞」且静默不报错；
//     反过来，整数列（mid、aid、source_type、state、void_seq）没有排序规则这一说，
//     给它们登记 collation 要求只会把门禁变成噪音（payment 那边就误判过一次）；
//   - 出金语义是本服务的范围红线（AGENTS.md §1、§5）：payout_state 必须写「恒 NOT_PAYABLE /
//     payout_available 恒 false」的口径，金额列必须写「应计、不是已支付」，
//     否则读侧（创作者端概览、运营面板）会把「算出来该付」误读成「钱已出账」。
//
// 与 services/payment/model/migration_parity_test.go 的差异（照抄组织方式，事实按本域改写）：
//   - 币种在本域是 VARCHAR(8)（默认 CNY），不是 payment 的 CHAR(3)；
//   - 本域无 CHECK 约束，不变量押在「复合唯一键 + DEFAULT + 服务侧 CAS」上；
//   - 枚举比对做**双向集合相等**：注释多列一个值、少列一个值都会红，
//     派生列（cr_metric.source_type）允许「指向权威列」的写法并回查权威列。
//
// 迁移文件按 AGENTS.md §10 视为源文件：本测试是漂移探测器，不是改库的入口。

const migrationsRelDir = "deploy/migrations/creator-revenue"

// targetDatabase 是本目录 SQL 的目标库，必须与 etc/creatorrevenue.v1.yaml 的 DataSource 一致，
// 并且写进迁移头注释（否则无法从文件本身确认数据所有权）。
const targetDatabase = "go_video_creator_revenue"

// int64MaxDigits 是 int64 最坏十进制位数（9223372036854775807）：mid / rule_id / rev 的宽度预算。
const int64MaxDigits = 19

// settlementNoPrefixWidth 是 BuildSettlementNo 的固定前缀 "CRS" 宽度。
const settlementNoPrefixWidth = 3

// tableSpec 声明一张 model 表与它的迁移建表语句的对应关系。
type tableSpec struct {
	table         string
	row           any
	selectColumns string
	primaryKey    []string
	// uniqueKeys 是幂等与不变量依赖的唯一索引：键名 -> 列组合（顺序敏感）。
	uniqueKeys map[string][]string
	// requiredIndexes 是 model 的 WHERE / ORDER BY 依赖的普通索引。
	requiredIndexes map[string][]string
	// binaryKeys 是参与唯一性判定/键值比对的**字符列**：必须列级 utf8mb4_bin。
	// 只登记字符列：整数列即使在唯一索引里也不进来。
	binaryKeys []string
	// textLimits 是 model 侧允许的最大字节数（errors.go 的 Max*Bytes，logic 按它拒超长入参）；
	// 断言方向：列宽 >= 上限，否则「校验放过、写入炸」。
	textLimits map[string]int
	// requiredNoDefault 是不允许带 DEFAULT 的判定列：漏传必须报错，不能被默认值静默记 0。
	requiredNoDefault []string
	// enumComments 是数值枚举列：建表注释必须**逐值且仅逐这些值**列举。
	enumComments map[string][]int32
	// enumPointers 是「取值口径指向别的列」的枚举列：列 -> 权威列（表.列）。
	// 这类列的注释必须显式点名权威列或自行逐值列举，二选一，否则口径断链。
	enumPointers map[string]string
	// commentMust 钉住「注释里必须出现的口径词」：列 -> 必须出现的子串。
	// 这是本域最容易被改丢的东西（金额单位、应计 vs 已支付、无出金通道）。
	commentMust map[string][]string
	// timeColumns 必须是 BIGINT（Unix 秒）。
	timeColumns []string
	// appendOnly 只追加台账：不得出现 mtime / state 这类「可被原地改写」的列。
	appendOnly bool
	// versionColumn 是该表 CAS 依赖的乐观锁列（BIGINT，注释说明递增）。
	versionColumn string
	// allowNoUniqueKey 显式豁免「每表至少一个唯一键」：只有更正台账适用，
	// 它的去重靠 cr_metric.UpdateCorrection 的 CAS（见 TestCorrectionLedgerDedupeReliesOnMetricCAS）。
	allowNoUniqueKey bool
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:         "cr_revenue_rule",
			row:           RevenueRule{},
			selectColumns: revenueRuleColumns,
			primaryKey:    []string{"rule_id"},
			uniqueKeys:    map[string][]string{"uniq_rule_code": {"rule_code"}},
			requiredIndexes: map[string][]string{
				// LockActiveBySource 锁整个 source_type 段（WHERE 不带 state），走该索引前缀。
				"idx_source_state": {"source_type", "state"},
				"idx_state":        {"state", "rule_id"}, // List/Count 按状态翻页
			},
			binaryKeys: []string{"rule_code"},
			textLimits: map[string]int{
				"rule_code": MaxRuleCodeBytes, "name": MaxRuleNameBytes,
				"description": MaxDescriptionBytes, "currency": MaxCurrencyBytes,
				"unit": MaxUnitBytes, "created_by": MaxOperatorBytes, "updated_by": MaxOperatorBytes,
			},
			requiredNoDefault: []string{"rule_code", "source_type"},
			enumComments: map[string][]int32{
				"state":       {RuleStateDraft, RuleStateActive, RuleStateArchived},
				"source_type": {SourceTypeVipWatch, SourceTypeCoin, SourceTypeInteraction, SourceTypeActivity},
			},
			commentMust: map[string][]string{
				"rule_code":                 {"唯一", "bin"},
				"unit_price_per_1000_minor": {"单价", "1000", "分"},
				"monthly_cap_minor":         {"封顶", "分"},
				"min_quantity":              {"门槛"},
				"currency":                  {"币种"},
				"effective_from":            {"Unix 秒"},
				"version":                   {"递增"},
				"state":                     {"ARCHIVED"},
			},
			timeColumns:   []string{"effective_from", "ctime", "mtime"},
			versionColumn: "version",
		},
		{
			table:         "cr_rule_change_log",
			row:           RuleChangeLog{},
			selectColumns: ruleChangeLogColumns,
			primaryKey:    []string{"log_id"},
			// request_id 唯一 = 运营重放同一请求不会二次生效。
			uniqueKeys: map[string][]string{"uniq_request_id": {"request_id"}},
			requiredIndexes: map[string][]string{
				"idx_rule_from_version": {"rule_code", "from_version"}, // FirstChangeFrom
				"idx_rule_ctime":        {"rule_code", "ctime"},
			},
			binaryKeys: []string{"request_id", "rule_code"},
			textLimits: map[string]int{
				"rule_code": MaxRuleCodeBytes, "action": longestRuleActionLen(),
				"operator": MaxOperatorBytes, "reason": MaxReasonBytes, "request_id": MaxRequestIDBytes,
			},
			requiredNoDefault: []string{"rule_code", "request_id"},
			commentMust: map[string][]string{
				"request_id":                     {"幂等"},
				"from_unit_price_per_1000_minor": {"分"},
				"to_unit_price_per_1000_minor":   {"分"},
				"reason":                         {"必填"},
			},
			timeColumns: []string{"ctime"},
			appendOnly:  true,
		},
		{
			table:         "cr_enrollment",
			row:           Enrollment{},
			selectColumns: enrollmentColumns,
			primaryKey:    []string{"enrollment_id"},
			uniqueKeys:    map[string][]string{"uniq_mid": {"mid"}},
			requiredIndexes: map[string][]string{
				"idx_state_mid": {"state", "mid"}, // List(state) ORDER BY mid 完全走该索引
			},
			// 注意：唯一键只有整数列 mid，所以 binaryKeys 必须为空 ——
			// 整数列没有排序规则，硬登记 utf8mb4_bin 要求就是假阳性。
			binaryKeys: nil,
			textLimits: map[string]int{
				"operator": MaxOperatorBytes, "remark": MaxRemarkBytes,
			},
			requiredNoDefault: []string{"mid"},
			enumComments:      map[string][]int32{"state": {EnrollmentStateEnrolled, EnrollmentStateLeft, EnrollmentStateSuspended}},
			commentMust: map[string][]string{
				"agreed_rule_version": {"快照"},
				"remark":              {"原因"},
			},
			timeColumns: []string{"enrolled_at", "left_at", "ctime", "mtime"},
		},
		{
			table:         "cr_metric",
			row:           RevenueMetric{},
			selectColumns: metricColumns,
			primaryKey:    []string{"metric_id"},
			// 「重复上报 = 以本次为准的更正」的判定依据，同时前缀服务出单聚合扫描。
			uniqueKeys: map[string][]string{
				"uniq_metric_key": {"period", "mid", "aid", "source_type"},
			},
			requiredIndexes: map[string][]string{
				"idx_period_mid_source": {"period", "mid", "source_type"}, // ListGroupForUpdate 封顶重算
				"idx_mid_period":        {"mid", "period"},
			},
			binaryKeys: []string{"period", "rule_code"},
			textLimits: map[string]int{
				"period": MaxPeriodBytes, "rule_code": MaxRuleCodeBytes,
				"unit": MaxUnitBytes, "source_detail": MaxSourceDetailBytes,
			},
			requiredNoDefault: []string{"period", "mid", "source_type", "rule_code"},
			enumComments:      map[string][]int32{"corrected": {0, 1}, "threshold_blocked": {0, 1}},
			enumPointers:      map[string]string{"source_type": "cr_revenue_rule.source_type"},
			commentMust: map[string][]string{
				"period":              {"YYYYMM"},
				"amount_minor":        {"应计", "分"},
				"capped_amount_minor": {"应计", "分", "门槛"},
				"rule_version":        {"规则版本"},
				"threshold_blocked":   {"门槛", "封顶"},
				"corrected":           {"更正"},
				"source_detail":       {"PII"},
				"quantity":            {"负数"},
			},
			timeColumns: []string{"ctime", "mtime"},
		},
		{
			table:         "cr_metric_change_log",
			row:           MetricChangeLog{},
			selectColumns: metricChangeLogColumns,
			primaryKey:    []string{"log_id"},
			// 本表刻意没有唯一键：request_id 允许为空且只做反查索引。
			// 去重由 cr_metric.UpdateCorrection 的 CAS（metric_id + 旧 quantity）兜底。
			uniqueKeys:       nil,
			allowNoUniqueKey: true,
			requiredIndexes: map[string][]string{
				"idx_metric_key": {"period", "mid", "aid", "source_type"},
				"idx_request_id": {"request_id"},
				"idx_ctime":      {"ctime"},
			},
			binaryKeys: []string{"period", "rule_code", "request_id"},
			textLimits: map[string]int{
				"period": MaxPeriodBytes, "rule_code": MaxRuleCodeBytes,
				"operator": MaxOperatorBytes, "reason": MaxReasonBytes, "request_id": MaxRequestIDBytes,
			},
			requiredNoDefault: []string{"period", "mid", "source_type"},
			commentMust: map[string][]string{
				"old_amount_minor":        {"分"},
				"new_amount_minor":        {"分"},
				"old_capped_amount_minor": {"分"},
				"new_capped_amount_minor": {"分"},
				"request_id":              {"幂等"},
				"reason":                  {"必填"},
			},
			timeColumns: []string{"ctime"},
			appendOnly:  true,
		},
		{
			table:         "cr_settlement",
			row:           Settlement{},
			selectColumns: settlementColumns,
			primaryKey:    []string{"settlement_id"},
			// uniq_active_period_mid 是「同一周期同一作者至多一张在效单」的实现：
			// 在效行 void_seq 恒 0，作废行把槽位改写成 settlement_id，于是新单能重新占住 0。
			uniqueKeys: map[string][]string{
				"uniq_settlement_no":     {"settlement_no"},
				"uniq_active_period_mid": {"period", "mid", "void_seq"},
				"uniq_request_id":        {"request_id"},
			},
			requiredIndexes: map[string][]string{
				"idx_period_state": {"period", "state"}, // 按周期批量出单/巡检
				"idx_mid_period":   {"mid", "period"},   // LatestActivePeriod
				"idx_mid_state":    {"mid", "state"},    // SumConfirmed
			},
			binaryKeys: []string{"settlement_no", "period", "request_id"},
			textLimits: map[string]int{
				"settlement_no": MaxSettlementNoBytes, "period": MaxPeriodBytes,
				"confirmed_by": MaxOperatorBytes, "void_reason": MaxVoidReasonBytes,
				"request_id": MaxRequestIDBytes, "currency": MaxCurrencyBytes,
			},
			requiredNoDefault: []string{"settlement_no", "period", "mid", "request_id"},
			enumComments: map[string][]int32{
				"state":        {SettlementStateDraft, SettlementStateConfirmed, SettlementStateVoided},
				"payout_state": {PayoutStateNotPayable},
			},
			commentMust: map[string][]string{
				// 范围红线：应计 != 已支付；本期无出金通道。
				"amount_minor":      {"应计", "分", "不是已支付"},
				"cap_applied_minor": {"封顶", "分"},
				"payout_state":      {"NOT_PAYABLE", "无出金通道", "payout_available", "false"},
				"void_seq":          {"在效", "作废", "settlement_id", "改写"},
				"void_reason":       {"必填"},
				"request_id":        {"幂等"},
				"period":            {"YYYYMM"},
				"currency":          {"币种"},
				"metric_count":      {"台账"},
			},
			timeColumns: []string{"confirmed_at", "ctime", "mtime"},
		},
		{
			table:         "cr_settlement_item",
			row:           SettlementItem{},
			selectColumns: settlementItemColumns,
			primaryKey:    []string{"item_id"},
			// 一单之内同来源只有一行：DRAFT 重算走「先 DELETE 再批量 INSERT」，
			// 该唯一键是「即使重算被打断，分项也不会写出双份」的兜底。
			uniqueKeys:      map[string][]string{"uniq_no_source": {"settlement_no", "source_type"}},
			requiredIndexes: nil,
			binaryKeys:      []string{"settlement_no", "rule_code"},
			textLimits: map[string]int{
				"settlement_no": MaxSettlementNoBytes, "rule_code": MaxRuleCodeBytes,
			},
			requiredNoDefault: []string{"settlement_no", "source_type"},
			commentMust: map[string][]string{
				"amount_minor": {"应计", "分"},
				"rule_code":    {"主规则"},
				"quantity":     {"聚合"},
			},
			timeColumns: []string{"ctime"},
			appendOnly:  true,
		},
	}
}

// longestRuleActionLen 是 model 侧 action 常量的最长长度：把它当列宽下限，
// 新增一个更长的动作名而不改列宽，门禁立刻红。
func longestRuleActionLen() int {
	max := 0
	for _, a := range ruleActionValues() {
		if len(a) > max {
			max = len(a)
		}
	}
	return max
}

func ruleActionValues() []string {
	return []string{
		RuleActionCreate, RuleActionUpdate, RuleActionActivate,
		RuleActionArchive, RuleActionAutoArchive,
	}
}

// --- SQL 解析 ---

type sqlColumn struct {
	name     string
	dataType string // 列定义原文（COMMENT 之前），用于 NOT NULL / DEFAULT / COLLATE 判定
	comment  string
	notNull  bool
	// hasDefault 只看类型段之后的定义（中文注释里的「默认」不影响判定）。
	hasDefault bool
	// 以下字段只从「类型段」（去掉反引号列名后的部分）推导，
	// 避免列名里的子串（settlement_no 含 "SET"）把字符列判定带偏。
	baseType   string
	typeWidth  int
	hasWidth   bool
	unsigned   bool
	autoIncr   bool
	charsetBin bool
}

type sqlIndex struct {
	name   string
	unique bool
	cols   []string
}

type sqlTable struct {
	table        string
	tableComment string
	file         string
	columns      []sqlColumn
	primary      []string
	indexes      []sqlIndex
	checks       map[string]string // 约束名 -> CHECK 表达式原文
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

// mustTable 取表定义：表名被改掉时也要给出可读的失败，而不是 nil 解引用 panic
// （panic 会带走整个 model 包的测试，让其余门禁失去信号）。
func mustTable(t *testing.T, tables map[string]*sqlTable, name string) *sqlTable {
	t.Helper()
	tbl, ok := tables[name]
	if !ok {
		t.Fatalf("迁移里没有建表语句 %s（model 有对应结构体）", name)
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
	dir := filepath.Join(findRepoRoot(t), "deploy", "migrations", "creator-revenue")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("找不到 %s：迁移文件缺失或目录改名（%v）", migrationsRelDir, err)
	}
	return dir
}

func readFileOrFatal(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(raw)
}

// parseCreateTables 抽出所有 CREATE TABLE 块。
// 只认本仓库既有书写风格（反引号标识符、每列一行、) ENGINE=... 收尾）：
// 格式被改坏时报错，而不是悄悄解析出一张空表。
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
		base := filepath.Base(p)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", p, err)
		}
		var cur *sqlTable
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case trimmed == "" || strings.HasPrefix(trimmed, "--"):
			case cur == nil && strings.HasPrefix(trimmed, "CREATE TABLE"):
				name, ok := backtickedIdentifier(trimmed)
				if !ok {
					t.Fatalf("%s: CREATE TABLE 无法解析表名: %q", base, trimmed)
				}
				if !strings.Contains(trimmed, "IF NOT EXISTS") {
					t.Fatalf("%s: 表 %s 必须用 CREATE TABLE IF NOT EXISTS，迁移要可重复执行", base, name)
				}
				cur = &sqlTable{table: name, file: base, checks: map[string]string{}}
			case cur != nil && strings.HasPrefix(trimmed, ")"):
				if !strings.Contains(trimmed, "ENGINE=InnoDB") || !strings.Contains(trimmed, "utf8mb4") {
					t.Fatalf("%s: 表 %s 收尾行必须显式 ENGINE=InnoDB 与 CHARSET=utf8mb4，得到 %q",
						base, cur.table, trimmed)
				}
				cur.tableComment = tableCommentOf(t, base, trimmed)
				if _, dup := out[cur.table]; dup {
					t.Fatalf("%s: 表 %s 被重复创建", base, cur.table)
				}
				out[cur.table] = cur
				cur = nil
			case cur != nil:
				parseTableLine(t, cur, base, trimmed)
			default:
				t.Fatalf("%s: 出现 CREATE TABLE 之外的可执行语句 %q —— 本目录只做建表，"+
					"ALTER/回填必须另起文件并评估锁窗口", base, trimmed)
			}
		}
		if cur != nil {
			t.Fatalf("%s: CREATE TABLE 块没有收尾的 ) 行", base)
		}
	}
	return out
}

// tableCommentOf 取 ") ENGINE=... COMMENT='...'" 行上的表注释。
// 表注释是本目录的口径入口（应计 != 已支付），必须存在。
func tableCommentOf(t *testing.T, file, line string) string {
	t.Helper()
	m := regexp.MustCompile(`COMMENT='([^']*)'`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("%s: 表收尾行缺少 COMMENT='...'，表级口径必须自解释: %q", file, line)
	}
	return m[1]
}

// parseTableLine 解析表体里的一行：列 / 主键 / 唯一键 / 普通索引 / CHECK 约束。
func parseTableLine(t *testing.T, cur *sqlTable, file, line string) {
	t.Helper()
	body := strings.TrimSuffix(strings.TrimSpace(line), ",")
	switch {
	case strings.HasPrefix(body, "`"):
		name, typePart, ok := splitColumnDefinition(body)
		if !ok {
			t.Fatalf("%s: 列定义无法解析: %q", file, line)
		}
		if cur.hasColumn(name) {
			t.Fatalf("%s: 列 %s 重复声明", file, name)
		}
		// 只在 COMMENT 之前的定义段上判定 NOT NULL / DEFAULT：
		// 中文注释里出现「默认」「不为负」字样不能影响结构判定。
		defined, comment := body, ""
		if i := strings.Index(body, " COMMENT '"); i >= 0 {
			defined = body[:i]
			comment = strings.TrimSuffix(body[i+len(" COMMENT '"):], "'")
		}
		if comment == "" {
			t.Fatalf("%s: 表 %s 的列 %s 缺少 COMMENT —— 分成台账的取值口径全靠注释表达", file, cur.table, name)
		}
		base, width, hasWidth := baseTypeAndWidth(typePart)
		up := strings.ToUpper(defined)
		cur.columns = append(cur.columns, sqlColumn{
			name:       name,
			dataType:   defined,
			comment:    comment,
			notNull:    strings.Contains(up, " NOT NULL"),
			hasDefault: strings.Contains(up, " DEFAULT "),
			autoIncr:   strings.Contains(up, " AUTO_INCREMENT"),
			charsetBin: strings.Contains(defined, "COLLATE utf8mb4_bin"),
			baseType:   base, typeWidth: width, hasWidth: hasWidth,
			unsigned: strings.Contains(up, " UNSIGNED"),
		})
	case strings.HasPrefix(body, "PRIMARY KEY"):
		cur.primary = columnListInParentheses(t, file, body)
	case strings.HasPrefix(body, "UNIQUE KEY"):
		name, _ := identifierAfterKeyword(body, "UNIQUE KEY")
		cur.addIndex(t, file, name, true, columnListInParentheses(t, file, body))
	case strings.HasPrefix(body, "KEY"):
		name, _ := identifierAfterKeyword(body, "KEY")
		cur.addIndex(t, file, name, false, columnListInParentheses(t, file, body))
	case strings.HasPrefix(body, "CONSTRAINT"):
		name, ok := backtickedIdentifier(body)
		if !ok {
			t.Fatalf("%s: 约束名无法解析: %q", file, body)
		}
		if !strings.Contains(strings.ToUpper(body), "CHECK") {
			t.Fatalf("%s: 约束 %s 不是 CHECK —— 本服务不建外键，跨表一致性在服务侧保证", file, name)
		}
		if _, dup := cur.checks[name]; dup {
			t.Fatalf("%s: 约束 %s 重复声明", file, name)
		}
		open := strings.Index(body, "CHECK (")
		cur.checks[name] = strings.TrimSuffix(body[open+len("CHECK ("):], ")")
	default:
		t.Fatalf("%s: 表 %s 内出现无法识别的定义行（FOREIGN KEY / 生成列不符合本服务约定）: %q",
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
	for _, c := range cols {
		if !t.hasColumn(c) {
			tt.Fatalf("%s: 索引 %s 引用了未声明的列 %s", file, name, c)
		}
	}
	t.indexes = append(t.indexes, sqlIndex{name: name, unique: unique, cols: cols})
}

var digitsRe = regexp.MustCompile(`\d+`)

// splitColumnDefinition 把 "`name` BIGINT NOT NULL" 拆成列名与类型段。
func splitColumnDefinition(body string) (name, typePart string, ok bool) {
	s := strings.TrimSpace(body)
	if !strings.HasPrefix(s, "`") {
		return "", "", false
	}
	end := strings.Index(s[1:], "`")
	if end < 0 {
		return "", "", false
	}
	return s[1 : end+1], strings.TrimSpace(s[end+2:]), true
}

// baseTypeAndWidth 从类型段取基础类型名与可选宽度（只看开头，不含列名）。
func baseTypeAndWidth(typePart string) (base string, width int, hasWidth bool) {
	i := 0
	for i < len(typePart) {
		c := typePart[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			break
		}
		i++
	}
	base = strings.ToUpper(typePart[:i])
	rest := strings.TrimSpace(typePart[i:])
	if strings.HasPrefix(rest, "(") {
		if j := strings.Index(rest, ")"); j > 0 {
			inner := strings.TrimSpace(rest[1:j])
			if n, err := strconv.Atoi(inner); err == nil {
				return base, n, true
			}
			// DECIMAL(10,2) 之类：宽度段不是纯数字，交给 base 判定拦下。
			if first := strings.SplitN(inner, ",", 2); len(first) == 2 {
				if n, err := strconv.Atoi(strings.TrimSpace(first[0])); err == nil {
					return base, n, true
				}
			}
		}
	}
	return base, 0, false
}

// isCharType 只有字符列（CHAR/VARCHAR/TEXT/ENUM/SET）才有排序规则这一说，
// 整数列（mid、source_type、void_seq）即便参与唯一索引也不能要求登记 utf8mb4_bin。
func (c sqlColumn) isCharType() bool {
	switch c.baseType {
	case "CHAR", "VARCHAR", "TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT", "ENUM", "SET":
		return true
	default:
		return false
	}
}

func (c sqlColumn) isVarchar() bool { return c.baseType == "VARCHAR" && c.hasWidth }

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
		out = append(out, name)
	}
	return out
}

type structField struct {
	column string
	goType string
}

func structFields(t *testing.T, row any) []structField {
	t.Helper()
	v := reflect.TypeOf(row)
	if v.Kind() != reflect.Struct {
		t.Fatalf("%T 不是结构体", row)
	}
	out := make([]structField, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		tag := v.Field(i).Tag.Get("db")
		if tag == "" {
			t.Fatalf("%s 的字段 %s 缺少 db tag —— model 无法扫描该列", v.Name(), v.Field(i).Name)
		}
		out = append(out, structField{column: tag, goType: v.Field(i).Type.String()})
	}
	return out
}

func structColumns(t *testing.T, row any) []string {
	t.Helper()
	fields := structFields(t, row)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.column)
	}
	return out
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// --- 断言：表与列的双向漂移 ---

func TestMigrationFilesCoverEveryTable(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		if _, ok := tables[spec.table]; !ok {
			t.Errorf("迁移里没有建表语句 %s（model 有对应结构体）", spec.table)
		}
	}
	for name := range tables {
		known := false
		for _, spec := range tableSpecs() {
			if spec.table == name {
				known = true
				break
			}
		}
		if !known {
			t.Errorf("迁移里出现了 model 未覆盖的表 %s —— 要么补 model，要么删掉这张死表", name)
		}
	}
}

// TestModelColumnsMatchMigration 双向列比对：
// 结构体有 db tag 而 SQL 没有 => 查询 Unknown column；SQL 有列而结构体没有 => 死列（永不读写）。
func TestModelColumnsMatchMigration(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl, ok := tables[spec.table]
			if !ok {
				t.Fatalf("迁移里没有 %s", spec.table)
			}
			fields := structColumns(t, spec.row)
			seen := map[string]struct{}{}
			for _, f := range fields {
				if _, dup := seen[f]; dup {
					t.Fatalf("结构体 %T 的 db tag %s 重复", spec.row, f)
				}
				seen[f] = struct{}{}
				if !tbl.hasColumn(f) {
					t.Errorf("结构体列 %s 在建表语句里不存在 —— 查询会报 Unknown column", f)
				}
			}
			for _, c := range tbl.columns {
				if _, ok := seen[c.name]; !ok {
					t.Errorf("SQL 列 %s.%s 没有结构体字段承载 —— 该列永远不会被读写，请删除或补 model",
						spec.table, c.name)
				}
			}
			if len(fields) != len(tbl.columns) {
				t.Errorf("列数不符：%s 结构体 %d 列，SQL %d 列", spec.table, len(fields), len(tbl.columns))
			}
			for _, c := range tbl.columns {
				if !c.notNull {
					t.Errorf("%s.%s 允许 NULL —— model 用普通结构体扫描（无 sql.Null*），读到 NULL 会直接失败",
						spec.table, c.name)
				}
			}
			if tbl.tableComment == "" {
				t.Errorf("%s 缺少表级 COMMENT —— 表的口径（应计/只追加/唯一语义）必须自解释", spec.table)
			}
		})
	}
}

// TestModelFieldTypesMatchSQLColumns Go 类型与 SQL 类型逐列相容。
// 方向：int64 只允许 BIGINT；int32 不允许 BIGINT（读到超 int32 的值会溢出成负数，
// 而 -1 在状态/来源判定里会被当成合法值）；浮点一律拒绝（金额与计量都是整数口径）。
func TestModelFieldTypesMatchSQLColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for _, f := range structFields(t, spec.row) {
				c, ok := tbl.column(f.column)
				if !ok {
					continue // 列存在性由上一条门禁负责
				}
				checked++
				base := c.baseType
				switch f.goType {
				case "int64":
					if base != "BIGINT" {
						t.Errorf("%s.%s 是 int64 字段但列为 %s —— 改成窄整型会静默截断金额/时间戳",
							spec.table, f.column, base)
					}
					if c.unsigned {
						t.Errorf("%s.%s 是 int64 但列为 UNSIGNED —— 减法会下溢报错而不是回绕", spec.table, f.column)
					}
				case "int32":
					if base == "BIGINT" {
						t.Errorf("%s.%s 是 int32 字段但列为 BIGINT —— 值超出 int32 时扫描直接失败或溢出为负",
							spec.table, f.column)
					}
					switch base {
					case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER":
					default:
						t.Errorf("%s.%s 是 int32 字段但列为 %s（期望 TINYINT/SMALLINT/MEDIUMINT/INT）",
							spec.table, f.column, base)
					}
				case "uint32":
					if base != "INT" || !c.unsigned {
						t.Errorf("%s.%s 是 uint32 字段但列为 %s%s（期望 INT UNSIGNED）",
							spec.table, f.column, base, unsignedSuffix(c))
					}
				case "string":
					if !c.isCharType() {
						t.Errorf("%s.%s 是 string 字段但列为 %s —— 扫描会报 type 错", spec.table, f.column, base)
					}
				case "bool":
					if base != "TINYINT" {
						t.Errorf("%s.%s 是 bool 字段但列为 %s（期望 TINYINT）", spec.table, f.column, base)
					}
				case "float32", "float64":
					t.Errorf("%s.%s 用了浮点字段 —— 分成台账与计量一律是整数（分/数量）口径", spec.table, f.column)
				default:
					t.Fatalf("%s.%s 的 Go 类型 %s 未在本门禁登记映射 —— 请显式补齐，不要默认放过",
						spec.table, f.column, f.goType)
				}
			}
		})
	}
	if checked < 90 {
		t.Fatalf("只比对了 %d 个字段类型（model 共 7 张表 99 列），说明解析器或结构体退化", checked)
	}
}

func unsignedSuffix(c sqlColumn) string {
	if c.unsigned {
		return " UNSIGNED"
	}
	return ""
}

// TestSelectColumnConstantsMatchSQL 钉住「SELECT 列常量 == 结构体字段（含顺序）」。
// 顺序不一致不会报错，只会把 mid 读成 amount —— 这是分成域最贵的静默故障。
func TestSelectColumnConstantsMatchSQL(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			cols := strings.Split(spec.selectColumns, ",")
			want := structColumns(t, spec.row)
			if len(cols) != len(want) {
				t.Fatalf("列常量 %d 列，结构体 %d 列 —— 少一列就会静默扫出零值", len(cols), len(want))
			}
			got := map[string]struct{}{}
			for _, c := range cols {
				name := strings.TrimSpace(c)
				if name == "" {
					t.Fatalf("列常量里有空列名: %q", spec.selectColumns)
				}
				if _, dup := got[name]; dup {
					t.Fatalf("列常量里 %s 重复", name)
				}
				got[name] = struct{}{}
				if !tbl.hasColumn(name) {
					t.Errorf("SELECT 列 %s 不在 %s 的建表语句里", name, spec.table)
				}
			}
			for _, c := range want {
				if _, ok := got[c]; !ok {
					t.Errorf("结构体列 %s 没进 SELECT 常量 —— FindOne 读不到该列", c)
				}
			}
			for i, c := range cols {
				if strings.TrimSpace(c) != want[i] {
					t.Fatalf("第 %d 列不符：SELECT 常量 %q，结构体字段 %q —— 顺序不一致会导致字段扫描错位",
						i+1, strings.TrimSpace(c), want[i])
				}
			}
			if !strings.HasPrefix(spec.selectColumns, spec.primaryKey[0]+",") {
				t.Errorf("SELECT 常量应以主键 %s 开头，得到 %q", spec.primaryKey[0], spec.selectColumns)
			}
		})
	}
}

// TestIdempotencyKeysAreUniqueInSQL 主键与唯一索引：幂等和「一周期一人一单」的数据库兜底。
func TestIdempotencyKeysAreUniqueInSQL(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl, ok := tables[spec.table]
			if !ok {
				t.Fatalf("迁移里没有 %s", spec.table)
			}
			if strings.Join(tbl.primary, ",") != strings.Join(spec.primaryKey, ",") {
				t.Errorf("主键不符：%s SQL %v，期望 %v", spec.table, tbl.primary, spec.primaryKey)
			}
			pk, ok := tbl.column(spec.primaryKey[0])
			if !ok {
				t.Fatalf("%s 缺少主键列 %s", spec.table, spec.primaryKey[0])
			}
			if !pk.autoIncr || pk.baseType != "BIGINT" {
				t.Errorf("%s 主键 %s 必须是 BIGINT AUTO_INCREMENT（void_seq 的「改写为 settlement_id」依赖它天然唯一），得到 %q",
					spec.table, spec.primaryKey[0], pk.dataType)
			}
			for name, cols := range spec.uniqueKeys {
				idx, ok := tbl.index(name)
				if !ok {
					t.Fatalf("%s 缺少唯一索引 %s —— 幂等失去数据库兜底，同一请求会重复出单/重复入账", spec.table, name)
				}
				if !idx.unique {
					t.Fatalf("%s 的 %s 必须是 UNIQUE KEY，普通 KEY 起不到去重作用", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("唯一索引 %s 列不符：SQL %v，期望 %v（顺序敏感）", name, idx.cols, cols)
				}
			}
			if len(spec.uniqueKeys) == 0 && !spec.allowNoUniqueKey {
				t.Errorf("%s 没有唯一键 —— 写接口无法靠数据库去重", spec.table)
			}
			// 反向：SQL 里出现了 spec 未登记的唯一键，等于凭空多了一条去重语义。
			for _, idx := range tbl.indexes {
				if !idx.unique {
					continue
				}
				if _, ok := spec.uniqueKeys[idx.name]; !ok {
					t.Errorf("%s 出现了 spec 未登记的唯一索引 %s(%v) —— 会改变幂等判定，必须一并登记",
						spec.table, idx.name, idx.cols)
				}
			}
		})
	}
}

// TestUniqueIndexesExcludeVoidedRowsOnlyOnce 语义级检查：
// cr_settlement 的在效唯一性只由 (period, mid, void_seq) 达成，
// 若有人「顺手」加一条 UNIQUE(period, mid)，强制重算会直接撞键（出单永久卡死）。
func TestUniqueIndexesExcludeVoidedRowsOnlyOnce(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	tbl := mustTable(t, tables, "cr_settlement")
	for _, idx := range tbl.indexes {
		if !idx.unique {
			continue
		}
		if len(idx.cols) == 2 && idx.cols[0] == "period" && idx.cols[1] == "mid" {
			t.Errorf("cr_settlement 出现了裸 UNIQUE(%s) —— VOIDED 历史行会撞键，重算无法出新单",
				strings.Join(idx.cols, ", "))
		}
	}
	if idx, ok := tbl.index("uniq_active_period_mid"); !ok || !idx.unique ||
		strings.Join(idx.cols, ",") != "period,mid,void_seq" {
		t.Fatalf("cr_settlement 的 uniq_active_period_mid 必须是 UNIQUE(period, mid, void_seq)")
	}
}

func TestQueryIndexesExist(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for name, cols := range spec.requiredIndexes {
				idx, ok := tbl.index(name)
				if !ok {
					t.Fatalf("%s 缺少索引 %s（model 的 WHERE/ORDER BY 依赖它，否则退化成全表扫）", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("索引 %s 列不符：SQL %v，期望 %v", name, idx.cols, cols)
				}
			}
		})
	}
}

// --- 断言：注释口径 ---

// TestMoneyColumnsAreIntegerMinorUnits 金额口径：BIGINT 最小货币单位，禁止浮点/定点。
// 一旦出现 DECIMAL/FLOAT，ComputeAmountMinor 的「整数除法向下取整」口径与 Go 侧 int64 就分叉了。
func TestMoneyColumnsAreIntegerMinorUnits(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		for _, c := range tbl.columns {
			lower := strings.ToLower(c.name)
			isMinor := strings.HasSuffix(lower, "_minor")
			isAmount := strings.HasSuffix(lower, "_amount")
			isRate := strings.Contains(lower, "rate") || strings.Contains(lower, "percent")
			if !isMinor && !isAmount && !isRate {
				continue
			}
			checked++
			for _, banned := range []string{"FLOAT", "DOUBLE", "DECIMAL", "NUMERIC", "REAL"} {
				if c.baseType == banned {
					t.Errorf("%s.%s 用了浮点/定点列 %q —— 分成金额必须是 BIGINT 分单位", spec.table, c.name, c.dataType)
				}
			}
			if isMinor && c.baseType != "BIGINT" {
				t.Errorf("%s.%s 金额列必须是 BIGINT（分），得到 %q", spec.table, c.name, c.dataType)
			}
			if !isRate && !strings.Contains(c.comment, "分") {
				t.Errorf("%s.%s 是金额列但注释没写明单位是分：%q —— 金额列的单位必须显式表达",
					spec.table, c.name, c.comment)
			}
			if isRate && !strings.Contains(c.comment, "分") && !strings.Contains(c.comment, "万分比") {
				t.Errorf("%s.%s 是比例列但注释未表达口径（万分比/百分比）：%q", spec.table, c.name, c.comment)
			}
		}
	}
	if checked < 12 {
		t.Fatalf("只比对了 %d 个金额列（本服务 *_minor 命名是唯一约定），说明列名口径被改动", checked)
	}
}

// TestCommentMustCarrySemantics 逐列钉住注释必须出现的口径词（见 spec.commentMust）。
// 这些词是「改 SQL 时最容易顺手丢掉」的东西：单位（分）、口径（应计 != 已支付）、
// 周期格式（YYYYMM）、幂等、无出金通道。
func TestCommentMustCarrySemantics(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		for column, needles := range spec.commentMust {
			c, ok := tbl.column(column)
			if !ok {
				t.Fatalf("%s 缺少列 %s", spec.table, column)
			}
			checked++
			for _, n := range needles {
				if !strings.Contains(c.comment, n) {
					t.Errorf("%s.%s 注释缺少口径词 %q（当前注释 %q）", spec.table, column, n, c.comment)
				}
			}
		}
	}
	if checked < 30 {
		t.Fatalf("只比对了 %d 个注释口径词，spec.commentMust 被裁剪了", checked)
	}
}

// TestAccrualIsNotPayout 出金语义红线（AGENTS.md §1「范围外能力不得假成功」、§5「任何服务发起出金」禁止项）：
//   - cr_settlement.payout_state 必须 NOT NULL DEFAULT 1，注释表达「无出金通道 + payout_available 恒 false」；
//   - 结算金额列必须写明「应计、不是已支付」，表注释同样要留这句话；
//   - 不得出现任何「已支付/已打款」语义的列名，避免读侧误判钱已出账。
func TestAccrualIsNotPayout(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	st := mustTable(t, tables, "cr_settlement")

	payout, ok := st.column("payout_state")
	if !ok {
		t.Fatal("cr_settlement 缺少 payout_state —— 缺少「钱没出账」的可见标记，只剩 README 提醒")
	}
	if !payout.notNull || payout.baseType != "TINYINT" {
		t.Errorf("payout_state 必须 NOT NULL TINYINT，得到 %q", payout.dataType)
	}
	if !strings.Contains(payout.dataType, "DEFAULT 1") {
		t.Errorf("payout_state 必须 DEFAULT 1（NOT_PAYABLE）：旁路写入/手工补行也不能造出「可出金」状态，得到 %q",
			payout.dataType)
	}
	for _, needle := range []string{"NOT_PAYABLE", "无出金通道", "payout_available", "false"} {
		if !strings.Contains(payout.comment, needle) {
			t.Errorf("cr_settlement.payout_state 注释缺少 %q（当前 %q）—— 出金范围口径必须写在列上", needle, payout.comment)
		}
	}
	if _, ok := st.column("paid_at"); ok {
		t.Error("cr_settlement 出现 paid_at —— 本服务无出金通道，「已支付时间」这种列名会诱导读侧误判")
	}
	for _, name := range []string{"amount_minor", "cap_applied_minor"} {
		c, ok := st.column(name)
		if !ok {
			t.Fatalf("cr_settlement 缺少金额列 %s", name)
		}
		if !strings.Contains(c.comment, "应计") || !strings.Contains(c.comment, "分") {
			t.Errorf("cr_settlement.%s 注释必须写明「应计（分）」：结算是折算后的计量，不是真实打款（当前 %q）",
				name, c.comment)
		}
	}
	amt, _ := st.column("amount_minor")
	if !strings.Contains(amt.comment, "不是已支付") {
		t.Errorf("cr_settlement.amount_minor 注释必须显式否定「已支付」（当前 %q）", amt.comment)
	}
	if !strings.Contains(st.tableComment, "NOT_PAYABLE") {
		t.Errorf("cr_settlement 表注释必须留 payout_state 恒 NOT_PAYABLE 的口径（当前 %q）", st.tableComment)
	}
	// 计量侧同理：台账金额是折算结果。
	metric := mustTable(t, tables, "cr_metric")
	for _, name := range []string{"amount_minor", "capped_amount_minor"} {
		c, _ := metric.column(name)
		if !strings.Contains(c.comment, "应计") {
			t.Errorf("cr_metric.%s 注释必须写明应计（折算后计量）口径（当前 %q）", name, c.comment)
		}
	}
	// 头注释的取整口径必须与 ComputeAmountMinor 一致（否则重算解释不了历史金额）。
	head := readFileOrFatal(t, filepath.Join(findMigrationsDir(t), "000001_create_creator_revenue_tables.sql"))
	for _, needle := range []string{"金额语义", "折算与取整口径", "向下取整", "min_quantity"} {
		if !strings.Contains(head, needle) {
			t.Errorf("迁移头注释缺少 %q 段落 —— 金额口径必须在库里自解释", needle)
		}
	}
}

// TestUniqueKeyColumnsUseBinaryCollation 参与唯一性判定的字符列必须二进制排序规则。
// 表级 utf8mb4_unicode_ci 会把只差大小写的 rule_code / request_id 判成同一个键：
// 重放被误吞，或该重放的被判「键已占用」——两种都会让规则串档、二次出单。
//
// 双向：
//  1. spec.binaryKeys 里登记的列必须真的声明 COLLATE utf8mb4_bin；
//  2. 所有唯一索引里的**字符列**都必须登记进 spec.binaryKeys。
//     整数列（mid / aid / source_type / state / void_seq / period 之外的 *_id）跳过 ——
//     它们没有排序规则，硬要求登记只会制造噪音（payment 曾因此误判）。
func TestUniqueKeyColumnsUseBinaryCollation(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for _, name := range spec.binaryKeys {
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, name)
				}
				checked++
				if !c.isCharType() {
					t.Errorf("%s.%s 被登记进 binaryKeys，但它不是字符列（%s）—— 整数列没有排序规则，请从清单删除",
						spec.table, name, c.baseType)
					continue
				}
				if !c.charsetBin {
					t.Errorf("%s.%s 参与唯一性判定但没声明 COLLATE utf8mb4_bin：%q", spec.table, name, c.dataType)
				}
			}
			for _, idx := range tbl.indexes {
				if !idx.unique {
					continue
				}
				for _, col := range idx.cols {
					c, _ := tbl.column(col) // 列存在性已由 addIndex 钉死
					if !c.isCharType() {
						continue
					}
					if !containsString(spec.binaryKeys, col) {
						t.Errorf("%s 的唯一索引 %s 用了字符列 %s，但 spec.binaryKeys 未登记其排序规则要求",
							spec.table, idx.name, col)
					}
				}
			}
		})
	}
	if checked < 12 {
		t.Fatalf("只比对了 %d 个 utf8mb4_bin 列，spec.binaryKeys 被裁剪了", checked)
	}
}

// TestBinaryColumnsAreExactlyTheUniquenessChain 反向卫生：
// 声明了 utf8mb4_bin 的列必须确实参与唯一性判定链（否则是无意义的写法漂移）。
// 本服务允许 rule_code 在非唯一索引里也保持 bin（它同时是跨表定位键）。
func TestBinaryColumnsAreExactlyTheUniquenessChain(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	allowed := map[string]struct{}{"rule_code": {}, "period": {}, "request_id": {}, "settlement_no": {}}
	for _, spec := range tableSpecs() {
		for _, c := range mustTable(t, tables, spec.table).columns {
			if !c.isCharType() || !c.charsetBin {
				continue
			}
			if _, ok := allowed[c.name]; !ok {
				t.Errorf("%s.%s 声明了 utf8mb4_bin，但不在已知的唯一性键清单里 —— 请确认它是否参与判定并登记",
					spec.table, c.name)
			}
		}
	}
}

// --- 断言：列宽与幂等键预算 ---

// TestModelTextLimitFitsColumnWidth 钉住「model/logic 校验上限 <= 列宽」。
// 反例：logic 按 64 放行 request_id，而列只有 VARCHAR(48) —— 校验过了、写入必炸。
// 注意 Max*Bytes 是字节预算（UTF-8 下字节数 >= 字符数），所以「列宽 >= 字节预算」是保守方向。
func TestModelTextLimitFitsColumnWidth(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for column, limit := range spec.textLimits {
				c, ok := tbl.column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				if !c.isVarchar() {
					t.Fatalf("%s.%s 不是 VARCHAR，无法与长度上限比对：%q", spec.table, column, c.dataType)
				}
				if c.typeWidth < limit {
					t.Errorf("%s.%s 列宽 VARCHAR(%d) 小于 model 允许的 %d 字节 —— 校验放过而写入报错",
						spec.table, column, c.typeWidth, limit)
				}
			}
		})
	}
}

// TestEveryTextColumnHasDeclaredLimit 防止「加了列忘了定上限」：
// textLimits 是手工清单，新增 VARCHAR 列不进来就等于留了个校验缺口。
func TestEveryTextColumnHasDeclaredLimit(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			for _, c := range mustTable(t, tables, spec.table).columns {
				if !c.isVarchar() {
					continue
				}
				if _, ok := spec.textLimits[c.name]; !ok {
					t.Errorf("%s.%s 是 VARCHAR 但没声明长度上限：要么在 errors.go/logic 里校验，要么在 spec 里写明为什么不校验",
						spec.table, c.name)
				}
			}
		})
	}
}

// TestPeriodColumnMatchesFormatPeriod 周期码是台账与结算单主键的成分：
// FormatPeriod 产出的 YYYYMM 必须刚好占满 VARCHAR(6)（定长才能靠字典序当时间序比较）。
func TestPeriodColumnMatchesFormatPeriod(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	sample := FormatPeriod(time.Date(2026, time.September, 22, 0, 0, 0, 0, time.Local))
	if len(sample) != MaxPeriodBytes {
		t.Fatalf("FormatPeriod 产出 %q，长度 %d != MaxPeriodBytes %d", sample, len(sample), MaxPeriodBytes)
	}
	if _, err := ValidatePeriod(sample); err != nil {
		t.Fatalf("ValidatePeriod(%q) 失败: %v", sample, err)
	}
	for _, spec := range tableSpecs() {
		c, ok := mustTable(t, tables, spec.table).column("period")
		if !ok {
			continue
		}
		if !c.isVarchar() || c.typeWidth != MaxPeriodBytes {
			t.Errorf("%s.period 必须是 VARCHAR(%d)（定长周期码），得到 %q", spec.table, MaxPeriodBytes, c.dataType)
		}
	}
	if _, ok := mustTable(t, tables, "cr_enrollment").column("period"); ok {
		t.Error("cr_enrollment 不应带 period：参与关系是「一人一行」，周期归因在台账与结算单上")
	}
}

// TestSettlementNoAndRequestKeyFitColumns 单号与行级幂等键的长度算术：
//   - BuildSettlementNo = "CRS" + period + "-" + mid + "-" + rev，最坏 mid 取 int64 上限；
//   - BuildSettlementRequestKey = request_id + "#" + period + "#" + mid，
//     后缀开销必须容得下（logic 用 requireScopedRequestID 扣后缀，
//     其下限 8 字节由 helpers.go 保证），否则「校验放过、VARCHAR(64) 截断」，
//     幂等键被悄悄改短 = 重放判不出来 = 二次出单。
func TestSettlementNoAndRequestKeyFitColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	noCol, ok := mustTable(t, tables, "cr_settlement").column("settlement_no")
	if !ok {
		t.Fatal("cr_settlement 缺少 settlement_no")
	}
	worstNo := BuildSettlementNo(FormatPeriod(time.Now().UTC()), int64(9223372036854775807), 1)
	if len(worstNo) > noCol.typeWidth {
		t.Errorf("settlement_no 列宽 VARCHAR(%d) 装不下最坏单号 %q（%d 字符）", noCol.typeWidth, worstNo, len(worstNo))
	}
	if got := specTextLimit("settlement_no"); got != MaxSettlementNoBytes {
		t.Errorf("spec 的 settlement_no 上限 %d != MaxSettlementNoBytes %d", got, MaxSettlementNoBytes)
	}

	reqCol, ok := mustTable(t, tables, "cr_settlement").column("request_id")
	if !ok {
		t.Fatal("cr_settlement 缺少 request_id")
	}
	if !reqCol.isVarchar() || reqCol.typeWidth < MaxRequestIDBytes {
		t.Fatalf("cr_settlement.request_id 必须 VARCHAR(>= %d)，得到 %q", MaxRequestIDBytes, reqCol.dataType)
	}
	suffix := len(BuildSettlementRequestKey("x", FormatPeriod(time.Now().UTC()), int64(9223372036854775807))) - 1
	const logicFloorBytes = 8 // helpers.requireScopedRequestID 的父键最小留白
	if MaxRequestIDBytes-suffix < logicFloorBytes {
		t.Errorf("行级幂等键后缀开销 %d 字节，父键只剩 %d 字节（< %d）——"+
			"requireScopedRequestID 的下限会把预算悄悄放宽，超长键将撑爆 VARCHAR(%d)",
			suffix, MaxRequestIDBytes-suffix, logicFloorBytes, reqCol.typeWidth)
	}
	// 空 request_id 的兜底键也不得占用真实键空间：noreq# 前缀必须与业务键可区分。
	if !strings.HasPrefix(BuildSettlementRequestKey("", "202609", 1), "noreq#") {
		t.Error("BuildSettlementRequestKey 的空键前缀被改动：无幂等键的行必须与有键的行可区分")
	}
}

func specTextLimit(column string) int {
	for _, spec := range tableSpecs() {
		if v, ok := spec.textLimits[column]; ok {
			return v
		}
	}
	return -1
}

// TestRequiredNoDefaultColumns 判定列不允许 DEFAULT：
// 漏传必须报错，不能被默认值静默记 0 ——「周期为空、金额为 0 的结算单」比失败更难排查。
func TestRequiredNoDefaultColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for _, name := range spec.requiredNoDefault {
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, name)
				}
				if c.autoIncr || !c.notNull {
					continue // 主键与「允许 NULL」另有断言覆盖
				}
				if c.hasDefault {
					t.Errorf("%s.%s 不该有 DEFAULT：%q —— 漏传必须报错", spec.table, name, c.dataType)
				}
			}
		})
	}
}

// TestStringEnumActionValuesListedEverywhere action 是字符串枚举列：
// 取值清单同时存在于 model 常量、列注释、logic 白名单三处，本门禁把三处钉在一起。
func TestStringEnumActionValuesListedEverywhere(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	tbl := mustTable(t, tables, "cr_rule_change_log")
	c, ok := tbl.column("action")
	if !ok {
		t.Fatal("cr_rule_change_log 缺少 action 列")
	}
	width := longestRuleActionLen()
	if !c.isVarchar() || c.typeWidth < width {
		t.Errorf("cr_rule_change_log.action 列宽 %q 装不下最长的动作名（%d 字符）", c.dataType, width)
	}
	for _, a := range ruleActionValues() {
		if !strings.Contains(c.comment, a) {
			t.Errorf("action 注释没说明取值 %q（当前 %q）—— 新增动作必须同步建表注释", a, c.comment)
		}
	}
	// 反向：注释里写的动作名不能是 model 没定义的（否则台账里会出现无处产出的值）。
	for _, token := range strings.Fields(strings.NewReplacer("：", " ", "、", " ", "/", " ").Replace(c.comment)) {
		tok := strings.Trim(token, "'")
		if tok == "" || !strings.HasPrefix(tok, "CREATE") && !strings.Contains(tok, "ARCHIVE") &&
			!strings.Contains(tok, "UPDATE") && !strings.Contains(tok, "ACTIVATE") {
			continue
		}
		if !containsString(ruleActionValues(), tok) {
			t.Errorf("action 注释出现 model 未定义的动作 %q —— 台账会写入无处产出的值", tok)
		}
	}
}

// --- 断言：数值枚举与时间列 ---

// TestEnumColumnsDocumentEveryValue 建表注释与 model 常量做**双向集合相等**。
// 少了值 = 新增枚举忘改注释；多了值 = 注释描述了一个 model 里不存在的状态（读侧无从判定）。
func TestEnumColumnsDocumentEveryValue(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		for column, values := range spec.enumComments {
			t.Run(spec.table+"."+column, func(t *testing.T) {
				col, ok := mustTable(t, tables, spec.table).column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				checked++
				if col.baseType != "TINYINT" {
					t.Errorf("%s.%s 应为 TINYINT 枚举列，得到 %s", spec.table, column, col.baseType)
				}
				tokens := numericTokens(col.comment)
				want := map[string]struct{}{}
				for _, v := range values {
					want[strconv.FormatInt(int64(v), 10)] = struct{}{}
					if _, ok := tokens[strconv.FormatInt(int64(v), 10)]; !ok {
						t.Errorf("%s.%s 的注释没说明取值 %d（当前 %q）—— 新增枚举值必须同步建表注释",
							spec.table, column, v, col.comment)
					}
				}
				for tok := range tokens {
					if _, ok := want[tok]; !ok {
						n, err := strconv.Atoi(tok)
						if err != nil || n > 99 {
							continue // 注释里的年份/位数说明之类，不当枚举值
						}
						t.Errorf("%s.%s 的注释列举了 model 不存在的取值 %s（当前 %q）",
							spec.table, column, tok, col.comment)
					}
				}
			})
		}
	}
	if checked < 6 {
		t.Fatalf("只比对了 %d 个枚举列，spec.enumComments 被裁剪了", checked)
	}
	// 状态常量的取值本身必须保持已落库的编号（重排会让历史台账解释成另一个状态）。
	if RuleStateDraft != 1 || RuleStateActive != 2 || RuleStateArchived != 3 {
		t.Error("规则状态常量被重排：cr_revenue_rule.state 注释与已落库数据都会错位")
	}
	if SettlementStateDraft != 1 || SettlementStateConfirmed != 2 || SettlementStateVoided != 3 {
		t.Error("结算状态常量被重排：cr_settlement.state 注释与已落库数据都会错位")
	}
	if EnrollmentStateEnrolled != 1 || EnrollmentStateLeft != 2 || EnrollmentStateSuspended != 3 {
		t.Error("参与状态常量被重排：cr_enrollment.state 注释与已落库数据都会错位")
	}
	if PayoutStateNotPayable != 1 {
		t.Error("PayoutStateNotPayable 被改动：cr_settlement.payout_state 的 DEFAULT 1 口径会失效")
	}
}

// numericTokens 抽注释里的独立数字（1、2、3 这类枚举序号），
// 不抽嵌在字母里的（utf8mb4、int64）。
func numericTokens(comment string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, m := range digitsRe.FindAllString(comment, -1) {
		out[m] = struct{}{}
	}
	// 去掉形如 "utf8mb4" 的尾巴数字：正则按最长数字串匹配，这里再按上下文剔除。
	for tok := range out {
		for _, w := range regexp.MustCompile(`[A-Za-z]+`+regexp.QuoteMeta(tok)).FindAllString(comment, -1) {
			if strings.HasSuffix(w, tok) && len(w) > len(tok) {
				delete(out, tok)
			}
		}
	}
	return out
}

// TestDerivedEnumColumnsPointAtAuthority cr_metric.source_type 不重复列举取值，
// 而是声明「与 cr_revenue_rule.source_type 一致」。该指针必须存在，
// 且权威列自己必须把 model 常量逐值列举（否则口径链断在中间）。
func TestDerivedEnumColumnsPointAtAuthority(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	pointerMarkers := []string{"与规则一致", "与 cr_revenue_rule", "cr_revenue_rule.source_type"}
	for _, spec := range tableSpecs() {
		for column, authority := range spec.enumPointers {
			t.Run(spec.table+"."+column, func(t *testing.T) {
				checked++
				col, ok := mustTable(t, tables, spec.table).column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				hasPointer := false
				for _, m := range pointerMarkers {
					if strings.Contains(col.comment, m) {
						hasPointer = true
						break
					}
				}
				hasValues := len(numericTokens(col.comment)) > 0
				if !hasPointer && !hasValues {
					t.Errorf("%s.%s 注释既没逐值列举也没指向权威枚举：%q —— 派生枚举列必须写明口径来源",
						spec.table, column, col.comment)
				}
				if !hasPointer {
					return // 自行逐值列举，交给 enumComments 负责
				}
				atbl, acol, ok := splitColumnRef(authority)
				if !ok {
					t.Fatalf("spec.enumPointers 的写法必须是 表.列，得到 %q", authority)
				}
				target, ok := mustTable(t, tables, atbl).column(acol)
				if !ok {
					t.Fatalf("权威列 %s 不存在 —— 指针指向了被改名/删除的列", authority)
				}
				for _, v := range []int32{SourceTypeVipWatch, SourceTypeCoin, SourceTypeInteraction, SourceTypeActivity} {
					if _, ok := numericTokens(target.comment)[strconv.FormatInt(int64(v), 10)]; !ok {
						t.Errorf("权威列 %s 的注释缺少取值 %d，但 %s.%s 把自己的一致性挂在它上面",
							authority, v, spec.table, column)
					}
				}
			})
		}
	}
	if checked == 0 {
		t.Fatal("spec.enumPointers 为空：cr_metric.source_type 的枚举指向检查被删了")
	}
}

func splitColumnRef(ref string) (table, column string, ok bool) {
	parts := strings.SplitN(ref, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func TestTimeColumnsAreUnixSecondsBigInt(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for _, name := range spec.timeColumns {
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少时间列 %s", spec.table, name)
				}
				checked++
				if c.baseType != "BIGINT" {
					t.Errorf("%s.%s 必须是 BIGINT（Unix 秒），得到 %q —— 改成 DATETIME 会让 int64 比较全部错位",
						spec.table, name, c.dataType)
				}
				if !strings.Contains(c.comment, "Unix 秒") && !strings.Contains(c.comment, "Unix") {
					t.Errorf("%s.%s 是时间列但注释没写明 Unix 秒：%q", spec.table, name, c.comment)
				}
			}
		})
	}
	if checked < 14 {
		t.Fatalf("只比对了 %d 个时间列，spec.timeColumns 被裁剪了", checked)
	}
}

// TestAppendOnlyLedgersHaveNoMutationColumns 只追加台账（变更/更正台账、结算分项）
// 不允许 mtime / state 这类「原地改写」列：纠正只能再追加一条，审计证据不能被悄悄改掉。
// 需要 CAS 的表则必须有 BIGINT 的 version 列（cr_revenue_rule）。
func TestAppendOnlyLedgersHaveNoMutationColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		if !spec.appendOnly {
			continue
		}
		checked++
		tbl := mustTable(t, tables, spec.table)
		for _, banned := range []string{"mtime", "state", "updated_at", "version"} {
			if tbl.hasColumn(banned) {
				t.Errorf("%s 出现了列 %s —— 只追加台账不允许原地改写（更正请再追加一行）", spec.table, banned)
			}
		}
		if !tbl.hasColumn("ctime") {
			t.Errorf("%s 缺少 ctime —— 只追加台账必须以 BIGINT Unix 秒记录写入时间", spec.table)
		}
	}
	if checked < 3 {
		t.Fatalf("只有 %d 张表被判定为只追加台账（应为变更台账 x2 + 结算分项）", checked)
	}

	for _, spec := range tableSpecs() {
		if spec.versionColumn == "" {
			continue
		}
		t.Run(spec.table+".cas_version", func(t *testing.T) {
			c, ok := mustTable(t, tables, spec.table).column(spec.versionColumn)
			if !ok {
				t.Fatalf("%s 缺少 CAS 版本列 %s —— UpdateDraft/SetState 的条件更新失去依据", spec.table, spec.versionColumn)
			}
			if c.baseType != "BIGINT" || !c.notNull {
				t.Errorf("%s.%s 必须是 NOT NULL BIGINT，得到 %q", spec.table, spec.versionColumn, c.dataType)
			}
			if !strings.Contains(c.comment, "递增") {
				t.Errorf("%s.%s 注释必须说明版本递增语义（当前 %q）", spec.table, spec.versionColumn, c.comment)
			}
		})
	}
	// 没有 version 列的表，CAS 只能靠状态守卫：确认 model 侧确实用了 state 条件。
	src := readFileOrFatal(t, filepath.Join(findRepoRoot(t), "services", "creator-revenue", "model", "cr_enrollment.go"))
	if !strings.Contains(src, "WHERE mid = ? AND state = ?") {
		t.Error("cr_enrollment.Transition 的 CAS 守卫被改动 —— 参与状态迁移失去并发保护")
	}
}

// TestVoidSeqSlotInvariantIsDeclared 作废重算不变量（本服务最关键也最容易被改坏的一条）：
//
//	CRS 在效行 void_seq 恒 0 -> UNIQUE(period, mid, void_seq) 保证一周期一人一单；
//	作废行把槽位改写成自己的 settlement_id（自增、天然唯一）-> 旧行退出 0 槽位，
//	新单得以重占，于是「作废序号按 (period,mid) 单调不复用」。
//
// 本门禁能证明的部分：列存在、BIGINT NOT NULL DEFAULT 0、确实进入那条复合唯一键、
// 注释表达「在效恒 0 / 作废改写为 settlement_id」。
// 已知缺口（本轮已登记，勿在此放宽）：没有任何 CHECK/触发器约束 void_seq 只能被写成
// 0 或本行 settlement_id —— 该不变量目前只有「列 + 复合唯一键 + model 唯一写路径（Void()）」
// 三层证据，绕过 model 的修数可以造出双在效单。见 TestVoidSeqOnlyModelPathWritesIt。
func TestVoidSeqSlotInvariantIsDeclared(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	tbl := mustTable(t, tables, "cr_settlement")
	c, ok := tbl.column("void_seq")
	if !ok {
		t.Fatal("cr_settlement 缺少 void_seq —— 「(period,mid) 在效唯一 + 保留作废历史」失去实现载体")
	}
	switch c.baseType {
	case "BIGINT", "INT", "MEDIUMINT", "SMALLINT":
	default:
		t.Errorf("void_seq 必须是 BIGINT/INT 计数列，得到 %q", c.dataType)
	}
	if !c.notNull || !strings.Contains(c.dataType, "DEFAULT 0") {
		t.Errorf("void_seq 必须 NOT NULL DEFAULT 0（在效槽位），得到 %q", c.dataType)
	}
	for _, needle := range []string{"在效", "作废", "改写", "settlement_id"} {
		if !strings.Contains(c.comment, needle) {
			t.Errorf("void_seq 注释缺少 %q（当前 %q）—— 必须表达「同一 (period,mid) 的作废序号单调、重算不复用旧行」",
				needle, c.comment)
		}
	}
	if !strings.Contains(tbl.tableComment, "void_seq") && !headExplainsVoidSeqSlot(t) {
		t.Errorf("cr_settlement 表注释未提到 void_seq，且头注释没写明 UNIQUE(period, mid, void_seq) 的实现方式")
	}
	// settlement_id 的注释必须说明它兼作 void_seq 槽位值，否则两个列的关系无人知晓。
	pk, _ := tbl.column("settlement_id")
	if !strings.Contains(pk.comment, "void_seq") {
		t.Errorf("cr_settlement.settlement_id 注释未说明它兼作 void_seq 槽位值（当前 %q）", pk.comment)
	}
}

// headExplainsVoidSeqSlot 头注释是否用 (period, mid, void_seq) 复合唯一键的写法解释了该设计。
func headExplainsVoidSeqSlot(t *testing.T) bool {
	t.Helper()
	head := readFileOrFatal(t, filepath.Join(findMigrationsDir(t), "000001_create_creator_revenue_tables.sql"))
	return strings.Contains(head, "`period`, `mid`, `void_seq`")
}

// TestVoidSeqOnlyModelPathWritesIt 唯一写路径核对：只有 Void() 把 void_seq 改写成 settlement_id，
// 其余 UPDATE 语句必须带 void_seq 守卫（否则「在效唯一」会被绕过）。
func TestVoidSeqOnlyModelPathWritesIt(t *testing.T) {
	src := readFileOrFatal(t, "cr_settlement.go")
	if !strings.Contains(src, "SET state = ?, void_reason = ?, void_seq = settlement_id") {
		t.Error("cr_settlement.Void 不再把 void_seq 改写为 settlement_id —— 复合唯一键的作废语义失效")
	}
	for _, needle := range []string{"AND void_seq = ?", "voidSeqLive int64 = 0"} {
		if !strings.Contains(src, needle) {
			t.Errorf("cr_settlement.go 缺少 %q —— 在效行的槽位常量或 CAS 守卫丢失", needle)
		}
	}
	// 头注释与本门禁同源：void_seq 只被写成 0 或 settlement_id，没有 DB 级 CHECK 兜底。
	if _, ok := mustTable(t, parseCreateTables(t, findMigrationsDir(t)), "cr_settlement").index("uniq_active_period_mid"); !ok {
		t.Fatal("uniq_active_period_mid 缺失")
	}
}

// TestCorrectionLedgerDedupeReliesOnMetricCAS cr_metric_change_log 刻意没有唯一键
// （request_id 可空、仅索引）。那么「同一次更正被写两遍台账」靠什么挡？
// 答案必须是 cr_metric.UpdateCorrection 的 CAS（metric_id + 旧 quantity）。
// 一旦那条 CAS 被改掉，本表就真的没有去重手段了 —— 所以这里把两者钉在一起。
func TestCorrectionLedgerDedupeReliesOnMetricCAS(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	logTbl := mustTable(t, tables, "cr_metric_change_log")
	if idx, ok := logTbl.index("uniq_request_id"); ok {
		t.Errorf("cr_metric_change_log 出现了唯一索引 %s —— 该表的 request_id 允许为空且可重复，"+
			"加唯一键会让第二条更正台账被拒；请同步更新本门禁", idx.name)
	}
	src := readFileOrFatal(t, "cr_metric.go")
	if !strings.Contains(src, "WHERE metric_id = ? AND quantity = ?") {
		t.Error("cr_metric.UpdateCorrection 的 CAS 守卫（metric_id + 旧 quantity）丢失 —— " +
			"更正台账没有唯一键，失去 CAS 就再没有任何机制阻止重复追加")
	}
	// 更正前必须留痕：model 侧写 change log 的入口与更正同事务（logic 负责，这里核对列齐备）。
	for _, c := range []string{"old_amount_minor", "new_amount_minor", "old_capped_amount_minor", "new_capped_amount_minor", "reason"} {
		if !logTbl.hasColumn(c) {
			t.Errorf("cr_metric_change_log 缺少 %s —— 更正证据不完整", c)
		}
	}
}

// --- 断言：币种与迁移卫生 ---

// TestCurrencyColumnsAreConsistentMinorLedger 本域币种是 VARCHAR(8)（默认 CNY），
// 不是 payment 的 CHAR(3)：三处（规则、结算单）定义必须完全一致，否则
// 「台账禁止跨币种混算」的判定会在不同表上得到不同结果。
func TestCurrencyColumnsAreConsistentMinorLedger(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var first sqlColumn
	var firstName string
	var checked int
	for _, name := range []string{"cr_revenue_rule", "cr_settlement"} {
		c, ok := mustTable(t, tables, name).column("currency")
		if !ok {
			t.Fatalf("%s 缺少 currency 列：币种必须随行显式记录，不能靠库级默认", name)
		}
		checked++
		if !c.isVarchar() || c.typeWidth < MaxCurrencyBytes {
			t.Errorf("%s.currency 必须 VARCHAR(>= %d)，得到 %q", name, MaxCurrencyBytes, c.dataType)
		}
		if !strings.Contains(c.dataType, "DEFAULT 'CNY'") {
			t.Errorf("%s.currency 必须 DEFAULT 'CNY'（旁路写入不得留下空币种），得到 %q", name, c.dataType)
		}
		if !strings.Contains(c.comment, "币种") {
			t.Errorf("%s.currency 注释必须写明是记账币种（当前 %q）", name, c.comment)
		}
		if firstName == "" {
			first, firstName = c, name
			continue
		}
		if normalizeSpaces(first.dataType) != normalizeSpaces(c.dataType) {
			t.Errorf("%s.currency 与 %s.currency 定义不一致：%q vs %q —— 跨币种校验必须同一把尺",
				firstName, name, first.dataType, c.dataType)
		}
	}
	if checked < 2 {
		t.Fatalf("只比对了 %d 个币种列", checked)
	}
	// cr_metric 不带 currency：币种由 rule_code + rule_version 锁定，
	// 这条设计写进 model 注释（见 RevenueMetric 上方），此处防止有人「顺手补列」造成双口径。
	if mustTable(t, tables, "cr_metric").hasColumn("currency") {
		t.Error("cr_metric 出现了 currency 列 —— 台账币种口径将由「规则锁定」变成「行内自带」，需同步改 logic 校验")
	}
}

func normalizeSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func TestMigrationsAreSelfDescribing(t *testing.T) {
	dir := findMigrationsDir(t)
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("枚举 %s 失败: %v", dir, err)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			text := readFileOrFatal(t, p)
			for _, marker := range []string{"数据所有者", "回滚", "幂等", "索引要点", "锁风险", "字符序"} {
				if !strings.Contains(text, marker) {
					t.Errorf("迁移头注释缺少 %q 段落（分成库的口径、索引与回滚必须写清）", marker)
				}
			}
			for _, banned := range []string{"TRUNCATE", "DROP DATABASE", "GRANT ", "FOREIGN KEY", "DELETE FROM"} {
				if containsOutsideComments(text, banned) {
					t.Errorf("迁移里出现了禁止语句 %q —— 本目录只建表，不清库、不授权、不删数据", banned)
				}
			}
			if !strings.Contains(text, targetDatabase) {
				t.Errorf("迁移未声明所属库 %s，无法确认数据所有权", targetDatabase)
			}
			// 回滚清单必须覆盖本文件建的每一张表（按依赖倒序 DROP）。
			for _, spec := range tableSpecs() {
				if !strings.Contains(text, "DROP TABLE IF EXISTS `"+spec.table+"`") {
					t.Errorf("回滚注释缺少 %s 的 DROP 行", spec.table)
				}
			}
		})
	}
}

func containsOutsideComments(text, keyword string) bool {
	upper := strings.ToUpper(keyword)
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(strings.ToUpper(trimmed), upper) {
			return true
		}
	}
	return false
}

// --- 断言：解析器自检（防止解析退化导致空转绿） ---

func TestParserExtractedExpectedShape(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	if len(tables) < len(tableSpecs()) {
		t.Fatalf("只解析到 %d 张表，spec 声明了 %d 张 —— 解析器退化会让所有列比对空转",
			len(tables), len(tableSpecs()))
	}
	var columns, commented, uniqIdx, normIdx, chars, binCols int
	for _, tbl := range tables {
		columns += len(tbl.columns)
		for _, c := range tbl.columns {
			if c.comment != "" {
				commented++
			}
			if c.isCharType() {
				chars++
				if c.charsetBin {
					binCols++
				}
			}
		}
		for _, i := range tbl.indexes {
			if i.unique {
				uniqIdx++
			} else {
				normIdx++
			}
		}
		if len(tbl.primary) == 0 {
			t.Fatalf("%s 没解析到主键", tbl.table)
		}
	}
	if columns < 99 {
		t.Fatalf("共解析到 %d 列（本服务 7 表应为 99 列）—— 列解析退化", columns)
	}
	if commented != columns {
		t.Fatalf("%d 列里只有 %d 列有注释", columns, commented)
	}
	if uniqIdx < 7 {
		t.Fatalf("只解析到 %d 个唯一索引（rule_code / request_id x2 / mid / metric_key / settlement_no+period+request_id / no_source）", uniqIdx)
	}
	if normIdx < 8 {
		t.Fatalf("只解析到 %d 个普通索引", normIdx)
	}
	if chars < 31 {
		t.Fatalf("只解析到 %d 个字符列（VARCHAR 列共 31 个）", chars)
	}
	if binCols < 13 {
		t.Fatalf("只识别到 %d 个 utf8mb4_bin 列", binCols)
	}
	// numericTokens 自检：独立数字抽取不能把 utf8mb4 / int64 的尾数当成枚举值。
	if _, ok := numericTokens("utf8mb4_bin 与 int64 混排")["4"]; ok {
		t.Error("numericTokens 把标识符尾数当成了枚举值")
	}
	if _, ok := numericTokens("状态：1 DRAFT、2 ACTIVE")["2"]; !ok {
		t.Error("numericTokens 抽不出枚举值 2")
	}
}
