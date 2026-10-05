package model

// 本文件把「model 的结构体 / SELECT 列常量」与「deploy/migrations/ops-config 的建表语句」
// 逐列对齐。之所以值得写成测试而不是靠人记：这两处分别由两次提交改动，
// 漂移的后果不是编译失败而是运行期错误——
//   * 结构体多一列 → SELECT 少读一列，字段永远为零值（最难查的一类 bug）；
//   * 列常量与 INSERT 的参数次序分叉 → 值写进隔壁列，数据当场错乱；
//   * 列宽窄于代码上限 → 一次正常保存变成 MySQL 1406「保存失败」；
//   * 唯一键被降级成普通索引 → 幂等（request_id）与位置唯一（position）同时失效，
//     重复发布、重复条目都会静默入库。
// 迁移文本是唯一事实源，因此这里读 SQL 文件而不是抄一份常量表。

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const migrationsRelDir = "deploy/migrations/ops-config"

// sqlColumn 是解析出来的一列定义。
type sqlColumn struct {
	name       string
	dataType   string // 归一化：BIGINT / INT / TINYINT / TEXT / VARCHAR(64)
	notNull    bool
	hasDefault bool
	comment    string
}

type sqlTable struct {
	file         string
	name         string
	column       []sqlColumn
	primaryKey   string
	uniqueKeys   map[string][]string
	plainIndexes map[string][]string
	tableComment string
}

func (t *sqlTable) columnByName(name string) (sqlColumn, bool) {
	for _, c := range t.column {
		if c.name == name {
			return c, true
		}
	}
	return sqlColumn{}, false
}

func (t *sqlTable) columnNames() []string {
	out := make([]string, 0, len(t.column))
	for _, c := range t.column {
		out = append(out, c.name)
	}
	return out
}

func (t *sqlTable) indexCols(name string) ([]string, bool) {
	if cols, ok := t.uniqueKeys[name]; ok {
		return cols, true
	}
	cols, ok := t.plainIndexes[name]
	return cols, ok
}

// tableSpec 声明「一张表 ↔ 一个 Go 行结构 ↔ 一段列常量」的三方对应关系。
type tableSpec struct {
	table      string
	row        any    // 行结构体零值，仅用于 reflect 取 db tag
	columns    string // model 里的列清单常量（INSERT/SELECT 共用同一份）
	selectSQL  string // model 里的 SELECT 前缀常量
	idColumn   string // 主键列
	uniqueKeys map[string][]string
	indexes    []string         // 查询侧依赖的普通索引（缺了就是全表扫描）
	enumTiny   map[string]int32 // TINYINT 枚举列 → model 侧承认的最大值
	listCols   map[string]int32 // 逗号列表列 → 参与判定的元素个数上限（用于宽度推导）
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:      "ops_config_item",
			row:        ConfigItem{},
			columns:    configItemColumns,
			selectSQL:  configItemSelect,
			idColumn:   "config_id",
			uniqueKeys: map[string][]string{"uniq_key_scope": {"cfg_key", "scope"}},
			indexes:    []string{"idx_scope_state", "idx_state"},
			enumTiny:   map[string]int32{"value_type": 4, "state": 2},
		},
		{
			table:     "ops_config_version",
			row:       ConfigVersion{},
			columns:   versionColumns,
			selectSQL: versionSelect,
			idColumn:  "version_id",
			uniqueKeys: map[string][]string{
				"uniq_config_version": {"config_id", "version"},
				"uniq_request_id":     {"request_id"},
			},
			enumTiny: map[string]int32{"value_type": 4},
		},
		{
			table:      "ops_rollout_rule",
			row:        RolloutRule{},
			columns:    rolloutRuleColumns,
			selectSQL:  rolloutRuleSelect,
			idColumn:   "rule_id",
			uniqueKeys: map[string][]string{"uniq_config_version_name": {"config_id", "version", "name"}},
			indexes:    []string{"idx_config_state"},
			enumTiny:   map[string]int32{"mode": 6, "state": 2},
			listCols:   map[string]int32{"platforms": 4, "mid_suffixes": MaxMidSuffixes},
		},
		{
			table:      "ops_topic",
			row:        Topic{},
			columns:    topicColumns,
			selectSQL:  topicSelect,
			idColumn:   "topic_id",
			uniqueKeys: map[string][]string{"uniq_slug": {"slug"}},
			indexes:    []string{"idx_state_sort"},
			enumTiny:   map[string]int32{"state": 2},
			listCols:   map[string]int32{"zone_ids": MaxTopicRefIDs, "tag_ids": MaxTopicRefIDs},
		},
		{
			table:      "ops_topic_item",
			row:        TopicItem{},
			columns:    topicItemColumns,
			selectSQL:  topicItemSelect,
			idColumn:   "id",
			uniqueKeys: map[string][]string{"uniq_topic_position": {"topic_id", "position"}},
			indexes:    []string{"idx_ref"},
			enumTiny:   map[string]int32{"state": 2},
		},
		{
			table:      "ops_recommend_slot",
			row:        RecommendSlot{},
			columns:    slotColumns,
			selectSQL:  slotSelect,
			idColumn:   "slot_id",
			uniqueKeys: map[string][]string{"uniq_code": {"code"}},
			indexes:    []string{"idx_state_code", "idx_page"},
			enumTiny:   map[string]int32{"state": 2},
			listCols:   map[string]int32{"platforms": 4},
		},
		{
			table:      "ops_recommend_slot_item",
			row:        SlotItem{},
			columns:    slotItemColumns,
			selectSQL:  slotItemSelect,
			idColumn:   "id",
			uniqueKeys: map[string][]string{"uniq_slot_position": {"slot_id", "position"}},
			indexes:    []string{"idx_ref"},
			enumTiny:   map[string]int32{"state": 2},
		},
		{
			table:      "ops_client_switch",
			row:        ClientSwitch{},
			columns:    clientSwitchColumns,
			selectSQL:  clientSwitchSelect,
			idColumn:   "switch_id",
			uniqueKeys: map[string][]string{"uniq_key_platform": {"switch_key", "platform"}},
			indexes:    []string{"idx_platform_key", "idx_config"},
			enumTiny:   map[string]int32{"platform": 4, "enabled": 2},
		},
	}
}

// displayOnlyTextCols 是「只给人看、不参与定位与幂等」的文本列：
// 它们没有代码级上限（见报告里的已知缺口），因此必须可 DEFAULT ”，
// 并且注释要说清它是干什么的——否则半年后没人敢删。
var displayOnlyTextCols = map[string]string{
	"title":       "展示",
	"description": "说明",
	"cover":       "封面",
	"remark":      "备注",
	"page":        "页面",
}

// codeTextCaps 是「有代码级上限的文本列」→ 上限常量名（同一列名在多张表复用时
// 取相同口径，如 value_type 之类不是文本列就不在这里）。
func codeTextCaps() map[string]int {
	return map[string]int{
		"cfg_key":         64,
		"scope":           16,
		"change_type":     16,
		"item_type":       16,
		"item_id":         MaxItemIDChars,
		"reason":          MaxReasonChars,
		"operator_name":   MaxOperatorNameChars,
		"request_id":      MaxRequestIDChars,
		"name":            MaxRuleNameChars,
		"slug":            64,
		"code":            MaxSlotCodeChars,
		"switch_key":      64,
		"app_version_min": MaxAppVersionChars,
		"app_version_max": MaxAppVersionChars,
		"min_version":     MaxAppVersionChars,
		"max_version":     MaxAppVersionChars,
		"platforms":       32,
		"mid_suffixes":    64,
		"whitelist_mids":  MaxWhitelistBytes,
		"zone_ids":        IDListMaxLen,
		"tag_ids":         IDListMaxLen,
		"cfg_value":       MaxCfgValueBytes,
	}
}

// MaxWhitelistBytes 是 whitelist_mids 的入库字节预算：MaxWhitelistMids 个 mid，
// 每个最多 19 位 + 一个逗号。声明成变量便于下面与列类型对账。
const MaxWhitelistBytes = MaxWhitelistMids * 20

var (
	varcharRe = regexp.MustCompile(`^VARCHAR\((\d+)\)$`)
	digitRun  = regexp.MustCompile(`[0-9]+`)
)

func findMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "deploy", "migrations", "ops-config")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("找不到 %s（从 %s 向上找过）", migrationsRelDir, dir)
		}
		dir = parent
	}
}

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
			case cur == nil && strings.HasPrefix(trimmed, "COMMENT="):
				// ) ENGINE=... 之后的表级注释续行，由下面的第二趟统一解析。
			case cur == nil && strings.HasPrefix(trimmed, "CREATE TABLE"):
				name := betweenBackticks(trimmed)
				if name == "" {
					t.Fatalf("%s: CREATE TABLE 无法解析表名: %q", base, trimmed)
				}
				if !strings.Contains(trimmed, "IF NOT EXISTS") {
					t.Fatalf("%s: 表 %s 必须 CREATE TABLE IF NOT EXISTS，迁移要可重复执行", base, name)
				}
				cur = &sqlTable{
					file:         base,
					name:         name,
					uniqueKeys:   map[string][]string{},
					plainIndexes: map[string][]string{},
				}
			case cur != nil && strings.HasPrefix(trimmed, ")"):
				for _, want := range []string{"ENGINE=InnoDB", "CHARSET=utf8mb4", "COLLATE=utf8mb4_unicode_ci"} {
					if !strings.Contains(trimmed, want) {
						t.Fatalf("%s: 表 %s 收尾行缺少 %s，实得 %q（引擎/字符集不一致会让 LIKE 与唯一键判定分叉）",
							base, cur.name, want, trimmed)
					}
				}
				if _, dup := out[cur.name]; dup {
					t.Fatalf("%s: 表 %s 被重复创建", base, cur.name)
				}
				out[cur.name] = cur
				cur = nil
			case cur != nil && strings.HasPrefix(trimmed, "PRIMARY KEY"):
				cols := indexColumns(trimmed)
				if len(cols) != 1 {
					t.Fatalf("%s: 表 %s 主键必须单列，实得 %v", base, cur.name, cols)
				}
				cur.primaryKey = cols[0]
			case cur != nil && strings.HasPrefix(trimmed, "UNIQUE KEY"):
				name, cols := indexLine(trimmed)
				if name == "" || len(cols) == 0 {
					t.Fatalf("%s: UNIQUE KEY 无法解析: %q", base, trimmed)
				}
				cur.uniqueKeys[name] = cols
			case cur != nil && strings.HasPrefix(trimmed, "KEY "):
				name, cols := indexLine(trimmed)
				if name == "" || len(cols) == 0 {
					t.Fatalf("%s: KEY 无法解析: %q", base, trimmed)
				}
				if _, dup := cur.uniqueKeys[name]; dup {
					t.Fatalf("%s: 索引名 %s 与唯一键重名", base, name)
				}
				cur.plainIndexes[name] = cols
			case cur != nil:
				parseColumnLine(t, cur, base, trimmed)
			default:
				t.Fatalf("%s: 出现 CREATE TABLE 之外的可执行语句 %q —— 本目录只做建表，"+
					"ALTER/回填必须另起文件并评估锁窗口", base, trimmed)
			}
		}
		if cur != nil {
			t.Fatalf("%s: CREATE TABLE 块没有收尾的 ) 行", base)
		}
	}
	// 表级 COMMENT 与逐表 DROP（回滚段）单独走一遍，因为它们在 ) 之后/在注释里。
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", p, err)
		}
		text := string(raw)
		for name, tbl := range out {
			if tbl.file != filepath.Base(p) {
				continue
			}
			start := strings.Index(text, "CREATE TABLE IF NOT EXISTS `"+name+"`")
			if start < 0 {
				t.Fatalf("%s: 找不到表 %s 的建表起点", tbl.file, name)
			}
			i := strings.Index(text[start:], "COMMENT='")
			if i < 0 {
				t.Errorf("%s: 表 %s 缺表级 COMMENT（迁移文件是这套表唯一的自述文档）", tbl.file, name)
				continue
			}
			rest := text[start+i+len("COMMENT='"):]
			end := strings.Index(rest, "'")
			if end < 0 {
				t.Errorf("%s: 表 %s 的表级 COMMENT 引号没闭合", tbl.file, name)
				continue
			}
			tbl.tableComment = rest[:end]
			if !strings.Contains(text, "DROP TABLE IF EXISTS `"+name+"`") {
				t.Errorf("%s: 表 %s 在回滚段缺 DROP TABLE IF EXISTS —— 下线脚本会漏表", tbl.file, name)
			}
		}
		if up := strings.ToUpper(text); strings.Contains(up, "DATETIME") || strings.Contains(up, "TIMESTAMP") {
			t.Errorf("%s: 出现 DATETIME/TIMESTAMP —— 本服务时间一律 BIGINT Unix 秒（AGENTS.md §5）", filepath.Base(p))
		}
	}
	return out
}

func parseColumnLine(t *testing.T, cur *sqlTable, file, line string) {
	t.Helper()
	body := strings.TrimSuffix(line, ",")
	name := betweenBackticks(body)
	if name == "" {
		t.Fatalf("%s: 表 %s 的列定义无法解析: %q", file, cur.name, line)
	}
	if _, dup := cur.columnByName(name); dup {
		t.Fatalf("%s: 表 %s 列 %s 重复声明", file, cur.name, name)
	}
	// 只在 COMMENT 之前的定义段上判 NOT NULL / DEFAULT：
	// 中文注释里出现「NULL」「默认值」字样不能影响结构判定。
	defined, comment := body, ""
	if i := strings.Index(body, " COMMENT '"); i >= 0 {
		defined, comment = body[:i], strings.TrimSuffix(body[i+len(" COMMENT '"):], "'")
	}
	if comment == "" {
		t.Fatalf("%s: 表 %s 列 %s 缺 COMMENT —— 本服务的数值枚举全靠注释表达含义", file, cur.name, name)
	}
	fields := strings.Fields(defined)
	if len(fields) < 2 {
		t.Fatalf("%s: 表 %s 列 %s 定义过短: %q", file, cur.name, name, defined)
	}
	cur.column = append(cur.column, sqlColumn{
		name:       name,
		dataType:   strings.ToUpper(fields[1]),
		notNull:    strings.Contains(strings.ToUpper(defined), "NOT NULL"),
		hasDefault: strings.Contains(strings.ToUpper(defined), "DEFAULT"),
		comment:    comment,
	})
}

func betweenBackticks(s string) string {
	i := strings.Index(s, "`")
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	j := strings.Index(rest, "`")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

var indexColsRe = regexp.MustCompile("`([^`]+)`")

func indexLine(s string) (string, []string) {
	name := betweenBackticks(s)
	return name, indexColumns(s)
}

func indexColumns(s string) []string {
	open := strings.Index(s, "(")
	close := strings.Index(s, ")")
	if open < 0 || close < open {
		return nil
	}
	var out []string
	for _, m := range indexColsRe.FindAllStringSubmatch(s[open:close], -1) {
		out = append(out, m[1])
	}
	return out
}

func varcharWidth(c sqlColumn) (int, bool) {
	if m := varcharRe.FindStringSubmatch(c.dataType); m != nil {
		var n int
		_, _ = fmt.Sscanf(m[1], "%d", &n)
		return n, true
	}
	return 0, false
}

func migrations(t *testing.T) map[string]*sqlTable {
	t.Helper()
	return parseCreateTables(t, findMigrationsDir(t))
}

// --- 用例 ---

func TestMigrationFilesCoverEveryModelTable(t *testing.T) {
	tables := migrations(t)
	specs := tableSpecs()
	if len(tables) != len(specs) {
		t.Errorf("迁移里有 %d 张表，model 声明了 %d 张: %v", len(tables), len(specs), sortedKeys(tables))
	}
	for _, s := range specs {
		if _, ok := tables[s.table]; !ok {
			t.Errorf("model 声明的表 %s 在 %s 里没有建表语句", s.table, migrationsRelDir)
		}
	}
	known := map[string]struct{}{}
	for _, s := range specs {
		known[s.table] = struct{}{}
	}
	for name := range tables {
		if _, ok := known[name]; !ok {
			t.Errorf("迁移里的表 %s 没有对应的 model 行结构——要么是没接的表，要么是漏更新的测试", name)
		}
	}
}

func TestStructColumnsAndColumnConstantMatchSQL(t *testing.T) {
	tables := migrations(t)
	for _, s := range tableSpecs() {
		tbl, ok := tables[s.table]
		if !ok {
			continue
		}
		sqlCols := tbl.columnNames()
		constCols := splitColumns(s.columns)
		tagCols := structDBTags(t, s.row)

		// 1) 列常量必须与建表语句逐列同序：INSERT 用 placeholders(n) 按位置绑定，
		//    顺序分叉就是把 A 列的值写进 B 列。
		if !equalStrings(constCols, sqlCols) {
			t.Errorf("%s: 列常量与 SQL 不一致\n 常量: %v\n SQL : %v", s.table, constCols, sqlCols)
		}
		// 2) 结构体 db tag 必须覆盖全部列（多一列=漏读，少一列=SELECT 会报未知列）。
		if !equalStrings(tagCols, sqlCols) {
			t.Errorf("%s: 结构体字段与 SQL 列不一致\n 结构: %v\n SQL : %v", s.table, tagCols, sqlCols)
		}
		// 3) SELECT 常量必须真的从这张表取数。
		wantPrefix := "SELECT " + s.columns + " FROM " + s.table
		if strings.Join(strings.Fields(s.selectSQL), " ") != wantPrefix {
			t.Errorf("%s: SELECT 常量应形如 %q，实为 %q", s.table, wantPrefix, s.selectSQL)
		}
		// 4) 主键必须是声明的那一列，并且结构体里有对应字段。
		if tbl.primaryKey != s.idColumn {
			t.Errorf("%s: 主键应是 %s，实为 %s", s.table, s.idColumn, tbl.primaryKey)
		}
		if pk, ok := tbl.columnByName(s.idColumn); !ok {
			t.Errorf("%s: 主键列 %s 不存在", s.table, s.idColumn)
		} else if pk.dataType != "BIGINT" {
			t.Errorf("%s: 主键 %s 类型应为 BIGINT，实为 %s", s.table, s.idColumn, pk.dataType)
		}
	}
}

func TestUniqueKeysAreUniqueInSQL(t *testing.T) {
	tables := migrations(t)
	for _, s := range tableSpecs() {
		tbl, ok := tables[s.table]
		if !ok {
			continue
		}
		for name, cols := range s.uniqueKeys {
			got, isUnique := tbl.uniqueKeys[name]
			if !isUnique {
				// 唯一键退化成普通索引：幂等与位置约束当场失效，而且不会报错。
				if plain, ok := tbl.plainIndexes[name]; ok {
					t.Errorf("%s.%s 被建成普通索引 KEY(%v)，必须是 UNIQUE KEY(%v)",
						s.table, name, plain, cols)
				} else {
					t.Errorf("%s: 缺少唯一键 %s（应为 UNIQUE KEY(%v)）", s.table, name, cols)
				}
				continue
			}
			if !equalStrings(got, cols) {
				t.Errorf("%s.%s 列应是 %v，实为 %v", s.table, name, cols, got)
			}
		}
		// 全量覆盖语义依赖「父键 + position」唯一：这两张表少了它就能写出重复格子。
		if s.table == "ops_topic_item" || s.table == "ops_recommend_slot_item" {
			if _, ok := tbl.uniqueKeys[fmt.Sprintf("uniq_%s_position",
				map[string]string{"ops_topic_item": "topic", "ops_recommend_slot_item": "slot"}[s.table])]; !ok {
				t.Errorf("%s: 缺 (parent, position) 唯一键，ReplaceAll 无法防重复格子", s.table)
			}
		}
		for _, idx := range s.indexes {
			if _, ok := tbl.indexCols(idx); !ok {
				t.Errorf("%s: 缺少索引 %s —— 对应查询会退化成全表扫描（本服务所有列表都要求带 LIMIT）", s.table, idx)
			}
		}
	}
}

func TestPublishCASConstraintsExist(t *testing.T) {
	// 发布链路的三条硬约束全部落在唯一键上，这里单独点名，防止被「顺手清理索引」改掉：
	//  1) uniq_key_scope：一个 (cfg_key, scope) 只有一个 config_id → CAS 才有单一基线；
	//  2) uniq_config_version：同版本不能有两行 → 回滚「追加新版本」才可能重复失败可辨；
	//  3) uniq_request_id：一次请求只产出一个版本行 → 幂等回放的唯一依据。
	tables := migrations(t)
	checks := []struct {
		table, key string
		cols       []string
		why        string
	}{
		{"ops_config_item", "uniq_key_scope", []string{"cfg_key", "scope"}, "配置项身份唯一，乐观锁才有唯一基线"},
		{"ops_config_version", "uniq_config_version", []string{"config_id", "version"}, "版本号不重复，MAX(version)+1 才安全"},
		{"ops_config_version", "uniq_request_id", []string{"request_id"}, "幂等回放的事实源"},
		{"ops_rollout_rule", "uniq_config_version_name", []string{"config_id", "version", "name"}, "规则 upsert 的幂等句柄"},
	}
	for _, c := range checks {
		tbl, ok := tables[c.table]
		if !ok {
			t.Fatalf("缺表 %s", c.table)
		}
		cols, ok := tbl.uniqueKeys[c.key]
		if !ok {
			t.Errorf("%s.%s 必须存在（%s）", c.table, c.key, c.why)
			continue
		}
		if !equalStrings(cols, c.cols) {
			t.Errorf("%s.%s 列应恰好是 %v，实为 %v", c.table, c.key, c.cols, cols)
		}
	}
}

func TestEveryTextColumnHasDeclaredLimit(t *testing.T) {
	caps := codeTextCaps()
	tables := migrations(t)
	for _, tbl := range sortedTables(tables) {
		for _, c := range tbl.column {
			width, isVarchar := varcharWidth(c)
			isText := c.dataType == "TEXT"
			if !isVarchar && !isText {
				continue
			}
			if cap, ok := caps[c.name]; ok {
				if isText {
					if c.name != "cfg_value" && c.name != "whitelist_mids" {
						t.Errorf("%s.%s 是 TEXT 但不在「允许 TEXT」名单里（TEXT 无法被索引起效，且会静默吃掉长度约定）",
							tbl.name, c.name)
					}
					if cap > MaxCfgValueBytes {
						t.Errorf("%s.%s 声明上限 %d 超过 TEXT 物理上限 %d", tbl.name, c.name, cap, MaxCfgValueBytes)
					}
					continue
				}
				if cap > width {
					// 这就是最贵的一类漂移：代码放行、MySQL 报 1406。
					t.Errorf("%s.%s 代码上限 %d 超过列宽 VARCHAR(%d)：超长会被数据库拒绝而不是被我们拒绝",
						tbl.name, c.name, cap, width)
				}
				continue
			}
			if hint, ok := displayOnlyTextCols[c.name]; ok {
				if !c.hasDefault {
					t.Errorf("%s.%s 是展示列（%s）却没给 DEFAULT ''：调用方不填就会整条写入失败", tbl.name, c.name, hint)
				}
				continue
			}
			t.Errorf("%s.%s 是文本列却既无代码上限也非展示列：%q", tbl.name, c.name, c.comment)
		}
	}
}

func TestKeyFormatsAndColumnWidthsAgree(t *testing.T) {
	// 标识列的「正则长度上限」与「列宽」必须是同一个数字：
	// 正则比列宽短 → 白拒合法输入；比列宽长 → 放过非法长度，交给 MySQL 报错。
	cases := []struct {
		table, column string
		valid         func(string) bool
	}{
		{"ops_config_item", "cfg_key", ValidCfgKey},
		{"ops_topic", "slug", ValidTopicSlug},
		{"ops_recommend_slot", "code", ValidSlotCode},
		{"ops_client_switch", "switch_key", ValidSwitchKey},
	}
	tables := migrations(t)
	for _, c := range cases {
		tbl, ok := tables[c.table]
		if !ok {
			continue
		}
		col, ok := tbl.columnByName(c.column)
		if !ok {
			t.Fatalf("%s 缺列 %s", c.table, c.column)
		}
		width, ok := varcharWidth(col)
		if !ok {
			t.Fatalf("%s.%s 不是 VARCHAR，实为 %s", c.table, c.column, col.dataType)
		}
		probe := func(n int) string { return "a" + strings.Repeat("b", n-1) }
		if !c.valid(probe(width)) {
			t.Errorf("%s.%s 上限正则不接受恰好 %d 个字符（列宽能装，代码却拒）", c.table, c.column, width)
		}
		if c.valid(probe(width + 1)) {
			t.Errorf("%s.%s 上限正则接受 %d 个字符但列宽只有 %d（会写进 MySQL 才失败）", c.table, c.column, width+1, width)
		}
	}
}

func TestListColumnWidthsFitWorstCase(t *testing.T) {
	// 列表列存的是 ",1,2,"，宽度必须容得下「声明个数的上限」的最坏情况，
	// 否则 ValidateIDList 放行后仍然会死在列宽上。
	tables := migrations(t)
	for _, s := range tableSpecs() {
		tbl, ok := tables[s.table]
		if !ok {
			continue
		}
		for col, maxElems := range s.listCols {
			c, ok := tbl.columnByName(col)
			if !ok {
				t.Errorf("%s 缺列 %s", s.table, col)
				continue
			}
			var worst int
			switch col {
			case "platforms":
				all, err := NormalizePlatformList([]int32{PlatformAndroid, PlatformIOS, PlatformHarmony, PlatformDesktop})
				if err != nil {
					t.Fatalf("NormalizePlatformList: %v", err)
				}
				worst = len(all)
			case "mid_suffixes":
				// 尾号的存储形态是 "0,3,7"（不带首尾逗号），最坏情况是十个数字全取。
				all, err := NormalizeMidSuffixes("9,8,7,6,5,4,3,2,1,0")
				if err != nil {
					t.Fatalf("NormalizeMidSuffixes: %v", err)
				}
				if n := strings.Count(all, ",") + 1; int32(n) > maxElems {
					t.Errorf("%s.%s 归一化结果 %d 个元素，超过声明上限 %d", s.table, col, n, maxElems)
				}
				worst = len(all)
			case "zone_ids", "tag_ids":
				// 数量上限与字节上限双重生效，落库前的硬约束是 IDListMaxLen。
				ids := make([]int64, 0, maxElems)
				for i := int64(0); i < int64(maxElems); i++ {
					ids = append(ids, 9223372036854775807-i) // 最长 BIGINT
				}
				if _, err := ValidateIDList(ids, int(maxElems)); err != nil {
					// IDListMaxLen 会先把这种极端组合拒掉，正是它存在的理由。
					if !strings.Contains(err.Error(), ErrTopicIDListTooLong.Error()) {
						t.Fatalf("%s.%s ValidateIDList 意外报错: %v", s.table, col, err)
					}
					worst = IDListMaxLen
					break
				}
				worst = len(IDListString(ids))
			default:
				t.Errorf("%s.%s 未纳入最坏长度计算", s.table, col)
				continue
			}
			w, ok := varcharWidth(c)
			if !ok {
				t.Errorf("%s.%s 是列表列却不是 VARCHAR，实为 %s", s.table, col, c.dataType)
				continue
			}
			if worst > w {
				t.Errorf("%s.%s 最坏情况 %d 字符 > VARCHAR(%d)：代码放行、数据库报错", s.table, col, worst, w)
			}
		}
	}
}

func TestEnumColumnsDocumentEveryLegalValue(t *testing.T) {
	tables := migrations(t)
	for _, s := range tableSpecs() {
		tbl, ok := tables[s.table]
		if !ok {
			continue
		}
		for col, modelMax := range s.enumTiny {
			c, ok := tbl.columnByName(col)
			if !ok {
				t.Errorf("%s 缺列 %s", s.table, col)
				continue
			}
			if c.dataType != "TINYINT" {
				t.Errorf("%s.%s 是枚举列，类型应为 TINYINT，实为 %s", s.table, col, c.dataType)
			}
			if strings.Contains(c.comment, "同值域") {
				// 快照列刻意引用主表注释（避免两处各写一遍、日后各自漂移）：
				// 只要它明确指向同一值域就算文档齐全，逐值枚举由主表那列负责。
				continue
			}
			digits := map[string]struct{}{}
			for _, m := range digitRun.FindAllString(c.comment, -1) {
				digits[m] = struct{}{}
			}
			for v := int32(1); v <= modelMax; v++ {
				if _, ok := digits[fmt.Sprint(v)]; !ok {
					// 注释就是这张表的值域文档：漏一个值，DBA 与运维只能靠猜。
					t.Errorf("%s.%s 的注释没写出取值 %d（当前枚举到 %d）: %q", s.table, col, v, modelMax, c.comment)
				}
			}
		}
		// 列表形态的端列不靠数字注释，靠引用 proto 与「空串=不限端」两句话。
		for _, col := range []string{"platforms"} {
			if c, ok := tbl.columnByName(col); ok {
				if !strings.Contains(c.comment, "ClientPlatform") || !strings.Contains(c.comment, "不限端") {
					t.Errorf("%s.%s 注释必须点明值域来源（proto ClientPlatform）与空串语义（不限端）: %q",
						tbl.name, col, c.comment)
				}
			}
		}
	}
}

func TestTimeColumnsAreUnixSecondsBigInt(t *testing.T) {
	tables := migrations(t)
	timeCols := []string{"ctime", "mtime", "published_at", "start_at", "end_at"}
	for _, tbl := range sortedTables(tables) {
		for _, c := range tbl.column {
			if !strings.HasSuffix(c.name, "_at") && c.name != "ctime" && c.name != "mtime" {
				continue
			}
			found := false
			for _, want := range timeCols {
				if c.name == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s.%s 是 *_at 命名的时间列却没进本用例的名单（时间列口径必须集中）", tbl.name, c.name)
				continue
			}
			if c.dataType != "BIGINT" {
				t.Errorf("%s.%s 必须是 BIGINT（Unix 秒），实为 %s", tbl.name, c.name, c.dataType)
			}
			if !strings.Contains(c.comment, "Unix") && !strings.Contains(c.comment, "时间") {
				t.Errorf("%s.%s 注释未说明是 Unix 秒: %q", tbl.name, c.name, c.comment)
			}
		}
	}
	// 反向：不许出现 created_at/updated_at 这类与 ctime/mtime 并存的第二套时间口径。
	for _, tbl := range sortedTables(tables) {
		for _, legacy := range []string{"created_at", "updated_at", "update_time", "create_time"} {
			if _, ok := tbl.columnByName(legacy); ok {
				t.Errorf("%s 出现 %s：本服务时间列统一 ctime/mtime", tbl.name, legacy)
			}
		}
	}
}

func TestAllColumnsNotNull(t *testing.T) {
	// 全列 NOT NULL 是这套 model 的前提：结构体字段是值类型（int32/int64/string），
	// 一旦有可空列，读回来就是 sql: Scan error 而不是零值，故障会出现在随机一条数据上。
	tables := migrations(t)
	for _, tbl := range sortedTables(tables) {
		for _, c := range tbl.column {
			if !c.notNull {
				t.Errorf("%s.%s 必须 NOT NULL", tbl.name, c.name)
			}
		}
	}
}

func TestNoCommercializationOrMiniProgramColumns(t *testing.T) {
	// AGENTS.md §2（不做商业化）与 §6（无小程序）在存储层的落点：
	// 本服务的库里连「列」都不该有这些语义，否则等于给未来的越界实现留好接口。
	forbiddenInName := []string{"vip", "order", "pay", "coin", "charge", "ads", "ad_",
		"member", "revenue", "divide", "sponsor", "price", "goods", "mini", "wechat",
		"weixin", "program", "h5"}
	forbiddenInComment := []string{"小程序", "会员", "支付", "订单", "投币", "分成", "充电"}
	tables := migrations(t)
	for _, tbl := range sortedTables(tables) {
		for _, c := range tbl.column {
			lower := strings.ToLower(c.name)
			for _, bad := range forbiddenInName {
				if strings.Contains(lower, bad) {
					t.Errorf("%s.%s 列名含 %q：本服务不得承载商业化/小程序语义", tbl.name, c.name, bad)
				}
			}
			for _, bad := range forbiddenInComment {
				if strings.Contains(c.comment, bad) {
					t.Errorf("%s.%s 注释出现 %q，需确认不是把商业化语义写进运营配置表", tbl.name, c.name, bad)
				}
			}
		}
		if strings.Contains(strings.ToUpper(tbl.name), "MINI") {
			t.Errorf("表名 %s 含小程序语义", tbl.name)
		}
	}
}

func TestMigrationsAreSelfDescribing(t *testing.T) {
	tables := migrations(t)
	for _, tbl := range sortedTables(tables) {
		if len([]rune(tbl.tableComment)) < 20 {
			t.Errorf("表 %s 的表级注释过短（%q）：迁移文件是这套表唯一的自述文档", tbl.name, tbl.tableComment)
		}
		for _, c := range tbl.column {
			if c.name == tbl.primaryKey {
				// 主键列的注释短不是问题，说清它是自增主键才是目的。
				if !strings.Contains(c.comment, "主键") {
					t.Errorf("%s.%s 是主键却没说明它是主键: %q", tbl.name, c.name, c.comment)
				}
				continue
			}
			if len([]rune(c.comment)) < 5 {
				t.Errorf("%s.%s 注释过短（%q）", tbl.name, c.name, c.comment)
			}
		}
		// 引用别服务主数据的列必须写明引用的是谁（AGENTS.md §5：只存引用、不复制资料）。
		evidence := []string{"引用", "不复制", "admin_id", "ops_", "audit.", "所属", "关联"}
		for _, c := range tbl.column {
			if strings.HasSuffix(c.name, "_ids") || c.name == "item_id" || c.name == "config_id" ||
				c.name == "audit_entry_id" || c.name == "operator_id" {
				ok := false
				for _, e := range evidence {
					if strings.Contains(c.comment, e) {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("%s.%s 是引用列却没说明引用谁: %q", tbl.name, c.name, c.comment)
				}
			}
		}
	}
}

func TestImmutableHistoryTablesHaveNoDeletePath(t *testing.T) {
	// ops_config_version 是「事实源」：只允许 INSERT 与 SetAuditEntry 单列补写。
	// 这里锁住它没有 (config_id) 之外的批量删依据 —— 具体表现为：
	// 该表的唯一键必须包含 config_id+version，保证「同一版本号只有一行」，
	// 回滚追加新版本时才不会被旧行挡住。
	tables := migrations(t)
	tbl := tables["ops_config_version"]
	if tbl == nil {
		t.Fatal("缺 ops_config_version")
	}
	cols := tbl.uniqueKeys["uniq_config_version"]
	if len(cols) != 2 || cols[0] != "config_id" || cols[1] != "version" {
		t.Errorf("uniq_config_version 必须是 (config_id, version)，实为 %v", cols)
	}
	// version 与 cfg_value 都必须 NOT NULL：一条没有值的版本行是不可解释的历史。
	for _, name := range []string{"version", "cfg_value", "change_type", "request_id", "reason"} {
		c, ok := tbl.columnByName(name)
		if !ok {
			t.Fatalf("ops_config_version 缺列 %s", name)
		}
		if !c.notNull {
			t.Errorf("ops_config_version.%s 必须 NOT NULL（版本行不可解释就等于历史不可信）", name)
		}
		if c.hasDefault && name != "change_type" {
			// reason 契约必填；version/request_id 给默认值等于允许「不带身份的行」。
			t.Logf("提示：ops_config_version.%s 有 DEFAULT，确认是否刻意允许零值", name)
		}
	}
}

// --- 小工具 ---

func splitColumns(s string) []string {
	parts := strings.Split(strings.Join(strings.Fields(s), ""), ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func structDBTags(t *testing.T, row any) []string {
	t.Helper()
	v := reflect.TypeOf(row)
	if v == nil {
		t.Fatal("row 结构体为零值类型？")
	}
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		t.Fatalf("%T 不是结构体", row)
	}
	out := make([]string, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.PkgPath != "" { // 非导出字段不参与映射
			continue
		}
		tag := f.Tag.Get("db")
		if tag == "" || tag == "-" {
			t.Errorf("%s 的字段 %s 没有 db tag：sqlx 会按字段名猜列名，猜错就是零值字段", v.Name(), f.Name)
			continue
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]*sqlTable) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedTables(m map[string]*sqlTable) []*sqlTable {
	out := make([]*sqlTable, 0, len(m))
	for _, name := range sortedKeys(m) {
		out = append(out, m[name])
	}
	return out
}
