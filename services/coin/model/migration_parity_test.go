package model

// 本文件把「model 的结构体 / SELECT 列常量 / 条件更新 SQL 与 deploy/migrations/coin 的建表语句」
// 变成可执行门禁（AGENTS.md §9）。迁移文件是只读对照物，本测试不改任何 SQL。
//
// 为什么必须有（而不是人工比对）：
//   - coin 的余额与幂等判定**完全建立在库的约束上**：cn_flow.uniq_request_id 是「不重复扣币」
//     的唯一保证，cn_account.PRIMARY(mid) 是并发串行点，cn_daily_toss.uniq_mid_date 让跨日
//     天然重置。这些键被删掉或加错列，表现不是启动失败，而是「同一请求扣两次币」这种最贵的故障；
//   - 结构体 db tag、SELECT 列常量、INSERT 列表是三处必须同步的文本，任一处漂移都会在运行期
//     变成 "Unknown column" 或字段扫描错位；
//   - request_id / biz_no 必须是**列级 utf8mb4_bin**：表级 unicode_ci 会把 'ABC' 与 'abc'
//     判成同一个键，一次真实扣币会被当成重放静默丢弃（丢单）；
//   - 整型宽度必须与 Go 字段匹配：target_aid 若被建成 INT 而 Go 是 int64，超出 21 亿的 aid
//     会静默溢出成负数；
//   - 条件更新的 WHERE 子句（balance >= ?、tossed + ? <= ?、`count` + ? <= ?、state = ?）
//     是 logic 里所有「余额不足/超限/并发」结论的真值来源，被改掉时单元测试的假实现仍然
//     按老语义返回，只有对照 SQL 文本才能发现。

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const coinMigrationsRelDir = "deploy/migrations/coin"

// --- DDL 解析 ---

type ddlColumn struct {
	name       string
	typ        string // 大写类型名，如 BIGINT / VARCHAR / TINYINT
	width      int    // VARCHAR(n) 的 n；非字符列为 0
	notNull    bool
	collate    string // 列级 COLLATE，空表示跟随表级
	hasDefault bool
	autoInc    bool
	comment    string
}

type ddlTable struct {
	name       string
	tableLevel string // 表级 COLLATE
	columns    []ddlColumn
	primaryKey []string
	uniqueKeys map[string][]string
	indexes    map[string][]string // 普通 KEY（不含唯一键与主键）
}

func (t *ddlTable) column(name string) (ddlColumn, bool) {
	for _, c := range t.columns {
		if c.name == name {
			return c, true
		}
	}
	return ddlColumn{}, false
}

var (
	reCreateTable = regexp.MustCompile("(?s)CREATE TABLE IF NOT EXISTS `([^`]+)` \\((.*?)\\n\\) (ENGINE[^;]*)")
	reColumnLine  = regexp.MustCompile("^\\s*`([^`]+)`\\s+([A-Za-z]+)(\\((\\d+)\\))?(.*?),?\\s*$")
	reCollate     = regexp.MustCompile(`(?i)COLLATE\s*=?\s*([A-Za-z0-9_]+)`)
	reComment     = regexp.MustCompile(`COMMENT\s+'((?:[^']|'')*)'`)
	rePrimaryKey  = regexp.MustCompile("(?i)^\\s*PRIMARY KEY\\s*\\((.*)\\)")
	reUniqueKey   = regexp.MustCompile("(?i)^\\s*UNIQUE KEY\\s+`([^`]+)`\\s*\\((.*)\\)")
	reKey         = regexp.MustCompile("(?i)^\\s*(?:KEY|INDEX)\\s+`([^`]+)`\\s*\\((.*)\\)")
)

func splitIdentList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "`")
		// 索引列可带排序方向/前缀长度，这里只关心列名。
		if i := strings.IndexAny(p, "( "); i >= 0 {
			p = p[:i]
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseCoinDDL(t *testing.T, table string) ddlTable {
	t.Helper()
	dir := repoPath(t, coinMigrationsRelDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读不到迁移目录 %s：%v（对照物缺失不能跳过）", dir, err)
	}
	var found *ddlTable
	scanned := 0
	parsed := 0
	mentioned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		scanned++
		bs, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读 %s 失败：%v", e.Name(), err)
		}
		text := string(bs)
		mentioned += strings.Count(text, "CREATE TABLE IF NOT EXISTS")
		for _, m := range reCreateTable.FindAllStringSubmatch(text, -1) {
			parsed++
			if m[1] != table {
				continue
			}
			tbl := parseCreateBody(t, m[1], m[2], m[3])
			if found != nil {
				t.Fatalf("%s 在多个迁移文件里重复建表，哪一份是真值？", table)
			}
			found = &tbl
		}
	}
	if found == nil {
		// 建表语句存在却一条都没解析出来 = 解析器与迁移写法脱节，
		// 这比「表不存在」更常见，必须单独报错，否则整套对照会静默失效。
		if scanned > 0 && mentioned > 0 && parsed == 0 {
			t.Fatalf("%s 里出现 %d 次 CREATE TABLE IF NOT EXISTS 但解析出 0 张表：建表写法变了，先修解析器",
				coinMigrationsRelDir, mentioned)
		}
		t.Fatalf("迁移目录 %s（扫描 %d 个 .sql，解析出 %d 张表）里没有 CREATE TABLE `%s`",
			dir, scanned, parsed, table)
	}
	return *found
}

func parseCreateBody(t *testing.T, name, body, options string) ddlTable {
	tbl := ddlTable{name: name, uniqueKeys: map[string][]string{}, indexes: map[string][]string{}}
	if m := reCollate.FindStringSubmatch(options); m != nil {
		tbl.tableLevel = strings.ToLower(m[1])
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if m := rePrimaryKey.FindStringSubmatch(trimmed); m != nil {
			tbl.primaryKey = splitIdentList(m[1])
			continue
		}
		if m := reUniqueKey.FindStringSubmatch(trimmed); m != nil {
			tbl.uniqueKeys[m[1]] = splitIdentList(m[2])
			continue
		}
		if m := reKey.FindStringSubmatch(trimmed); m != nil {
			tbl.indexes[m[1]] = splitIdentList(m[2])
			continue
		}
		m := reColumnLine.FindStringSubmatch(trimmed)
		if m == nil {
			t.Fatalf("%s：无法解析的建表行 %q（新增列写法时请同步解析器）", name, trimmed)
		}
		col := ddlColumn{name: m[1], typ: strings.ToUpper(m[2])}
		if m[4] != "" {
			col.width, _ = strconv.Atoi(m[4])
		}
		rest := strings.ToUpper(m[5])
		col.notNull = strings.Contains(rest, "NOT NULL")
		col.autoInc = strings.Contains(rest, "AUTO_INCREMENT")
		col.hasDefault = strings.Contains(rest, "DEFAULT")
		if cm := reCollate.FindStringSubmatch(trimmed); cm != nil {
			col.collate = strings.ToLower(cm[1])
		}
		if cm := reComment.FindStringSubmatch(trimmed); cm != nil {
			col.comment = cm[1]
		}
		if _, dup := tbl.column(col.name); dup {
			t.Fatalf("%s：列 %s 重复定义", name, col.name)
		}
		tbl.columns = append(tbl.columns, col)
	}
	if len(tbl.columns) == 0 {
		t.Fatalf("%s：没解析出任何列", name)
	}
	return tbl
}

// structColumns 取行结构体的 db tag 列名（顺序即声明顺序）。
func structColumns(t *testing.T, row any) []string {
	t.Helper()
	rv := reflect.TypeOf(row)
	if rv == nil || rv.Kind() != reflect.Struct {
		t.Fatalf("样例结构体类型不符：%T", row)
	}
	var out []string
	for i := 0; i < rv.NumField(); i++ {
		tag := rv.Field(i).Tag.Get("db")
		if tag == "" || tag == "-" {
			t.Fatalf("%s 第 %d 个字段没有 db tag（sqlx 按 tag 扫描，缺 tag 就是错位）", rv.Name(), i)
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	return out
}

// splitSelectColumns 把 model 的列常量拆成列名（去掉反引号与空白，保留顺序）。
func splitSelectColumns(t *testing.T, cols string) []string {
	t.Helper()
	parts := strings.Split(cols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), "`"))
		if p == "" {
			t.Fatalf("列常量里有空列：%q", cols)
		}
		out = append(out, p)
	}
	return out
}

func repoPath(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
			dir = filepath.Dir(dir)
			continue
		}
		return filepath.Join(dir, filepath.FromSlash(rel))
	}
	t.Fatalf("从测试工作目录往上找不到 go.mod（当前 %s）", dir)
	return ""
}

// --- 表的期望结构（一处声明，四张表共用同一套断言）---

type coinTableExpect struct {
	table string
	row   any
	cols  string
	// insertSQL 是 model 里该表的 INSERT 语句片段（用于比对列清单）。
	insertFrag string
	// insertIgnores 是 INSERT 不写、靠 DDL 默认值/自增填充的列。
	insertIgnores []string
	primaryKey    []string
	uniqueKeys    map[string][]string
	requiredIdx   map[string][]string
	// noSecondaryIndex 用于 cn_account：本表不做任何按余额/时间的检索，
	// 多一个索引就多一次投币写入的维护成本（迁移注释已说明）。
	noSecondaryIndex bool
	// binColumns 是参与唯一性或精确匹配鉴别、必须逐字节比较的列。
	binColumns []string
	// textLimits 是 model 侧写入的字符上限（列宽必须 >= 它，否则校验放过而写入报错）。
	textLimits map[string]int
}

func coinTableExpects() []coinTableExpect {
	return []coinTableExpect{
		{
			table: "cn_account", row: Account{}, cols: accountColumns,
			insertFrag:    "INSERT INTO cn_account (",
			insertIgnores: nil,
			// mid 沿用 account 域主键：既是账户唯一性，也是 EnsureTx 的抢键点与投币事务的第一把锁。
			primaryKey:       []string{"mid"},
			uniqueKeys:       map[string][]string{},
			noSecondaryIndex: true,
			// 余额与累计都是 int64，必须是 BIGINT。
		},
		{
			table: "cn_daily_toss", row: DailyToss{}, cols: dailyTossColumns,
			insertFrag:    "INSERT INTO cn_daily_toss (",
			insertIgnores: []string{"id", "tossed"},
			primaryKey:    []string{"id"},
			// 一行一天：跨日天然重置，不存在「自己判断是否跨日再清零」的竞态代码。
			uniqueKeys: map[string][]string{"uniq_mid_date": {"mid", "date"}},
			requiredIdx: map[string][]string{
				"idx_date": {"date"}, // 按日聚合与冷数据归档
			},
		},
		{
			table: "cn_toss", row: Toss{}, cols: tossColumns,
			insertFrag:    "INSERT INTO cn_toss (",
			insertIgnores: []string{"id"},
			primaryKey:    []string{"id"},
			uniqueKeys: map[string][]string{
				// 「一人对一内容一条聚合」的事实约束，也是 coin_count 聚合的分母依据。
				"uniq_mid_target": {"mid", "target_aid"},
			},
			requiredIdx: map[string][]string{
				"idx_mid_state_last": {"mid", "state", "last_tossed_at"}, // ListMyTosses
				"idx_target_state":   {"target_aid", "state"},            // 汇总与投币人列表
			},
			binColumns: []string{"last_request_id"},
			textLimits: map[string]int{
				"last_request_id": MaxIDChars,
				"trace_id":        MaxIDChars,
			},
		},
		{
			table: "cn_flow", row: Flow{}, cols: flowColumns,
			insertFrag:    "INSERT INTO cn_flow (",
			insertIgnores: []string{"id"},
			primaryKey:    []string{"id"},
			uniqueKeys: map[string][]string{
				// 幂等锚点：本服务不重复扣币的唯一保证。
				"uniq_request_id": {"request_id"},
			},
			requiredIdx: map[string][]string{
				"idx_mid_ctime_id": {"mid", "ctime", "id"},             // ListCoinFlows 排序
				"idx_ctime":        {"ctime"},                          // 跨用户时间窗台账
				"idx_biz_no":       {"biz_no"},                         // 按订单号对账
				"idx_target_type":  {"mid", "target_aid", "flow_type"}, // FindLatestByTarget
			},
			binColumns: []string{"request_id", "biz_no"},
			textLimits: map[string]int{
				"request_id": MaxIDChars,
				"biz_no":     MaxIDChars,
				"operator":   MaxIDChars,
				"trace_id":   MaxIDChars,
				"remark":     MaxRemarkChars,
			},
		},
	}
}

// TestStructColumnsMatchMigrationColumns 证明「结构体字段 = SELECT 列 = 建表列」三处一致，
// 且顺序一致（列清单是查询语句的一部分，漏改就是 Unknown column 或扫描错位）。
func TestStructColumnsMatchMigrationColumns(t *testing.T) {
	for _, exp := range coinTableExpects() {
		t.Run(exp.table, func(t *testing.T) {
			tbl := parseCoinDDL(t, exp.table)
			fromStruct := structColumns(t, exp.row)
			fromSelect := splitSelectColumns(t, exp.cols)

			var fromDDL []string
			for _, c := range tbl.columns {
				fromDDL = append(fromDDL, c.name)
			}
			if strings.Join(fromSelect, ",") != strings.Join(fromStruct, ",") {
				t.Errorf("SELECT 列常量与结构体 db tag 不一致：\n  SELECT %v\n  结构体 %v", fromSelect, fromStruct)
			}
			if strings.Join(fromDDL, ",") != strings.Join(fromStruct, ",") {
				t.Errorf("建表列顺序与结构体不一致（model 假定逐列一致）：\n  DDL %v\n  结构体 %v", fromDDL, fromStruct)
			}
		})
	}
}

// TestIntegerWidthsMatchGoFields 窄列配宽字段会静默溢出（aid 超过 21 亿变成负数），
// 宽列配窄字段则扫描报错。
func TestIntegerWidthsMatchGoFields(t *testing.T) {
	for _, exp := range coinTableExpects() {
		t.Run(exp.table, func(t *testing.T) {
			tbl := parseCoinDDL(t, exp.table)
			rv := reflect.TypeOf(exp.row)
			for i := 0; i < rv.NumField(); i++ {
				name := rv.Field(i).Tag.Get("db")
				kind := rv.Field(i).Type.Kind()
				col, ok := tbl.column(name)
				if !ok {
					t.Fatalf("结构体字段 %s.%s 在建表语句里没有同名列", rv.Name(), name)
				}
				switch kind {
				case reflect.Int64:
					if col.typ != "BIGINT" {
						t.Errorf("%s.%s 是 int64 但列是 %s（会溢出或被截断）", rv.Name(), name, col.typ)
					}
				case reflect.Int32:
					if col.typ != "INT" && col.typ != "TINYINT" && col.typ != "SMALLINT" && col.typ != "MEDIUMINT" {
						t.Errorf("%s.%s 是 int32 但列是 %s", rv.Name(), name, col.typ)
					}
				case reflect.String:
					if col.typ != "VARCHAR" && col.typ != "CHAR" && col.typ != "TEXT" {
						t.Errorf("%s.%s 是 string 但列是 %s", rv.Name(), name, col.typ)
					}
				default:
					t.Errorf("%s.%s 用了 sqlx 不便处理的类型 %s", rv.Name(), name, kind)
				}
			}
		})
	}
}

// TestNotNullColumnsHaveWritePath 严格模式下 INSERT 漏掉「NOT NULL 且无默认值」的列
// 会直接报 1364：账户能建、投币能扣，但流水永远写不进去。
func TestNotNullColumnsHaveWritePath(t *testing.T) {
	for _, exp := range coinTableExpects() {
		t.Run(exp.table, func(t *testing.T) {
			tbl := parseCoinDDL(t, exp.table)
			inInsert := splitInsertColumns(t, exp.table, exp.insertFrag)
			ignored := map[string]struct{}{}
			for _, c := range exp.insertIgnores {
				ignored[c] = struct{}{}
			}
			for _, c := range tbl.columns {
				if _, ok := inInsert[c.name]; ok {
					continue
				}
				if _, ok := ignored[c.name]; ok {
					if !c.autoInc && !c.hasDefault {
						t.Errorf("%s.%s 被 INSERT 省略，却既非自增也无默认值", exp.table, c.name)
					}
					continue
				}
				if c.notNull && !c.hasDefault && !c.autoInc {
					t.Errorf("%s.%s NOT NULL 且无默认值，但 model 的 INSERT 不写它（严格模式下必然失败）",
						exp.table, c.name)
				}
			}
			// 反向：INSERT 里不得出现建表语句没有的列。
			for col := range inInsert {
				if _, ok := tbl.column(col); !ok {
					t.Errorf("model 的 INSERT 写了不存在的列 %s.%s", exp.table, col)
				}
			}
		})
	}
}

// splitInsertColumns 从 model 的 SQL 文本里把该表 INSERT 的列清单抠出来。
// 用的是 modelSQLText 而不是源码：INSERT 列清单在 Go 里常被拆成两段字面量拼接。
func splitInsertColumns(t *testing.T, table, frag string) map[string]struct{} {
	t.Helper()
	src := modelSQLText(t)
	idx := strings.Index(src, frag)
	if idx < 0 {
		t.Fatalf("model 的 SQL 里找不到 %q", frag)
	}
	rest := src[idx+len(frag):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("%s 的 INSERT 列清单没闭合", table)
	}
	out := map[string]struct{}{}
	for _, p := range strings.Split(rest[:end], ",") {
		p = strings.Trim(strings.TrimSpace(p), "`")
		if p == "" {
			t.Fatalf("%s 的 INSERT 列清单里有空列：%q", table, rest[:end])
		}
		out[p] = struct{}{}
	}
	return out
}

// TestKeysAndIndexesMatchModelSemantics 唯一键与索引是 coin 正确性的一部分，
// 不是性能优化：删掉它们的表现是「重复扣币」「跨日不自愈」「运营一次误查拖住主库」。
func TestKeysAndIndexesMatchModelSemantics(t *testing.T) {
	for _, exp := range coinTableExpects() {
		t.Run(exp.table, func(t *testing.T) {
			tbl := parseCoinDDL(t, exp.table)
			if strings.Join(tbl.primaryKey, ",") != strings.Join(exp.primaryKey, ",") {
				t.Errorf("主键不符：DDL=%v 期望=%v（model 的行锁与 UPSERT 抢键点依赖它）",
					tbl.primaryKey, exp.primaryKey)
			}
			for name, cols := range exp.uniqueKeys {
				got, ok := tbl.uniqueKeys[name]
				if !ok {
					t.Errorf("缺少唯一键 %s（列 %v）：这是幂等/不变量的唯一保证", name, cols)
					continue
				}
				if strings.Join(got, ",") != strings.Join(cols, ",") {
					t.Errorf("%s 的列组合不符：DDL=%v 期望=%v（顺序敏感）", name, got, cols)
				}
			}
			for name := range tbl.uniqueKeys {
				if _, ok := exp.uniqueKeys[name]; !ok {
					t.Errorf("出现了未声明的唯一键 %s：它会让某条正常写入路径开始报 1062", name)
				}
			}
			for name, cols := range exp.requiredIdx {
				got, ok := tbl.indexes[name]
				if !ok {
					t.Errorf("缺少索引 %s（列 %v）：model 的 WHERE/ORDER BY 会退化成全表扫描", name, cols)
					continue
				}
				if strings.Join(got, ",") != strings.Join(cols, ",") {
					t.Errorf("%s 的列组合不符：DDL=%v 期望=%v", name, got, cols)
				}
			}
			if exp.noSecondaryIndex && (len(tbl.indexes) > 0 || len(tbl.uniqueKeys) > 0) {
				t.Errorf("%s 不该有二级索引（每次余额写入都要维护它们）：%v %v",
					exp.table, tbl.indexes, tbl.uniqueKeys)
			}
			// 索引引用的列必须存在（写错列名时 MySQL 在建表阶段就报错，这里等价于防漂移）。
			for _, cols := range tbl.indexes {
				for _, c := range cols {
					if _, ok := tbl.column(c); !ok {
						t.Errorf("索引引用了不存在的列 %s", c)
					}
				}
			}
		})
	}
}

// TestIdentityColumnsUseBinaryCollation 参与唯一性判定与等值对账的列必须逐字节比较。
// 表级是 utf8mb4_unicode_ci（折叠大小写），一旦有人删掉列级 COLLATE，
// 仅大小写不同的两个 request_id 会被判成同一个键 —— 一次真实扣币被静默丢弃。
func TestIdentityColumnsUseBinaryCollation(t *testing.T) {
	for _, exp := range coinTableExpects() {
		t.Run(exp.table, func(t *testing.T) {
			tbl := parseCoinDDL(t, exp.table)
			if !strings.Contains(tbl.tableLevel, "unicode_ci") && tbl.tableLevel != "" {
				t.Logf("表级 COLLATE=%s（用例只关心列级是否显式 utf8mb4_bin）", tbl.tableLevel)
			}
			for _, name := range exp.binColumns {
				col, ok := tbl.column(name)
				if !ok {
					t.Fatalf("列 %s 不存在", name)
				}
				if col.collate != "utf8mb4_bin" {
					t.Errorf("%s.%s 的 COLLATE=%q，必须是 utf8mb4_bin（幂等/对账键不允许大小写折叠）",
						exp.table, name, col.collate)
				}
			}
			// 反过来：折叠语义的文本列不该被误设成 bin（与全仓其它库不一致）。
			for _, c := range tbl.columns {
				if c.collate == "utf8mb4_bin" && !contains(exp.binColumns, c.name) {
					t.Errorf("%s.%s 不该是 utf8mb4_bin（可读文本列要跟表级折叠语义）", exp.table, c.name)
				}
			}
		})
	}
}

// TestTextColumnsFitModelLimits model 侧的截断上限必须不超过列宽：
// 反过来（列比上限窄）才会出现「代码以为收好了、数据库仍然报错」。
func TestTextColumnsFitModelLimits(t *testing.T) {
	for _, exp := range coinTableExpects() {
		t.Run(exp.table, func(t *testing.T) {
			tbl := parseCoinDDL(t, exp.table)
			for name, limit := range exp.textLimits {
				col, ok := tbl.column(name)
				if !ok {
					t.Fatalf("列 %s 不存在", name)
				}
				if col.width == 0 {
					t.Errorf("%s.%s 不是定宽字符列，无法与 model 上限 %d 比对", exp.table, name, limit)
					continue
				}
				if col.width < limit {
					t.Errorf("%s.%s 列宽 %d < model 上限 %d：校验放过而写入报 1406",
						exp.table, name, col.width, limit)
				}
				if col.width > limit {
					t.Logf("%s.%s 列宽 %d 比 model 上限 %d 宽（允许，但不能更窄）", exp.table, name, col.width, limit)
				}
			}
		})
	}
}

// TestEnumValuesDocumentedInComments 状态/类型的整数取值必须在建表注释里逐个写明：
// 常量值被改动（比如把 CANCELLED 从 2 挪到 3）时，库里既有数据就与代码语义脱节，
// 而这种脱节在单元测试里永远看不出来。
func TestEnumValuesDocumentedInComments(t *testing.T) {
	cases := []struct {
		table   string
		column  string
		values  []int32
		labelAt int
	}{
		{"cn_toss", "state", []int32{TossStateActive, TossStateCancelled}, 0},
		{"cn_flow", "flow_type", []int32{FlowTypeToss, FlowTypeCancelToss, FlowTypeOrderPack,
			FlowTypeAdminGrant, FlowTypeExpire}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			tbl := parseCoinDDL(t, tc.table)
			col, ok := tbl.column(tc.column)
			if !ok {
				t.Fatalf("列不存在")
			}
			for i, v := range tc.values {
				want := fmt.Sprintf("%d ", v)
				if !strings.Contains(col.comment, want) {
					t.Errorf("列注释未记载取值 %q：%s", want, col.comment)
				}
				_ = i
			}
			// 取值必须是 1..n 连续分配：留空洞说明有历史值被删掉了，
			// 而库里可能还有那种行（logic 会把它们当成脏数据塌成 UNSPECIFIED）。
			for i, v := range tc.values {
				if v != int32(i+1) {
					t.Errorf("取值 %d 不在 1..n 连续序列的第 %d 位", v, i+1)
				}
			}
		})
	}
}

// TestDailyBucketIsIntegerDayNo 日桶列必须是整型且能容纳 YYYYMMDD。
func TestDailyBucketIsIntegerDayNo(t *testing.T) {
	tbl := parseCoinDDL(t, "cn_daily_toss")
	col, ok := tbl.column("date")
	if !ok {
		t.Fatal("cn_daily_toss.date 不存在")
	}
	if col.typ != "INT" {
		t.Errorf("date 列类型 %s，model 用 int32 存 YYYYMMDD", col.typ)
	}
	if !strings.Contains(col.comment, "YYYYMMDD") {
		t.Errorf("date 注释未说明日桶编号格式：%s", col.comment)
	}
	// 字符串日桶（如 '2026-03-05'）会让 uniq_mid_date 退化成按字典序比较，
	// 并且「跨日天然重置」要求日桶在整型上单调可比。
	tossTbl := parseCoinDDL(t, "cn_toss")
	if c, ok := tossTbl.column("last_toss_date"); !ok || c.typ != "INT" {
		t.Errorf("cn_toss.last_toss_date 必须是 INT 日桶（取消按它回退额度）：%+v", c)
	}
}

// --- 条件更新 SQL 的守卫子句：logic 的全部结论都建立在它上面 ---

// modelGoFiles 是四个手写 model 文件（不含 goctl 生成物：coin 的 model 全部手写）。
var modelGoFiles = []string{"cn_account.go", "cn_daily_toss.go", "cn_toss.go", "cn_flow.go"}

func readModelSource(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, name := range modelGoFiles {
		bs, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读 model 源文件 %s 失败：%v", name, err)
		}
		sb.Write(bs)
		sb.WriteString("\n")
	}
	return sb.String()
}

var reGoStringLit = regexp.MustCompile(`"((?:[^"\\\n]|\\.)*)"`)

// modelSQLText 把 model 源码里的字符串字面量按出现顺序拼成一段归一文本。
// 必须这样做而不是直接对源码做子串比对：Go 里一条长 SQL 会被拆成
// "..." + "..." 跨行拼接，源码文本中间夹着 `"+` 与换行，任何逐字的 SQL 断言都会落空。
func modelSQLText(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, m := range reGoStringLit.FindAllStringSubmatch(readModelSource(t), -1) {
		sb.WriteString(m[1])
		sb.WriteString(" ")
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// TestConditionalUpdatesKeepTheirGuards 这几条 WHERE 是「余额不足 / 超限 / 状态机」的唯一真值来源。
// 它们被改掉时测试用的假实现仍会按老语义返回，只有对照 SQL 才能发现。
func TestConditionalUpdatesKeepTheirGuards(t *testing.T) {
	src := modelSQLText(t)
	cases := []struct {
		name    string
		mustHas []string
	}{
		{"扣减不得穿底", []string{"UPDATE cn_account SET balance = balance - ?", "WHERE mid = ? AND balance >= ?"}},
		{"扣减同事务累计历史", []string{"total_tossed = total_tossed + ?"}},
		{"发放扣回不得写成负余额", []string{"UPDATE cn_account SET balance = balance + ?, version = version + 1, mtime = ? WHERE mid = ? AND balance + ? >= 0"}},
		{"取消退回不改历史口径", []string{"UPDATE cn_account SET balance = balance + ?, version = version + 1, mtime = ? WHERE mid = ?"}},
		{"日额度累加带上限", []string{"UPDATE cn_daily_toss SET tossed = tossed + ?, mtime = ? WHERE mid = ? AND date = ? AND tossed + ? <= ?"}},
		{"日额度回退夹底到 0", []string{"tossed = GREATEST(tossed - ?, 0)"}},
		{"投币累计带单片上限且只认生效行", []string{"WHERE id = ? AND state = ? AND `count` + ? <= ?"}},
		{"取消只作用于生效行", []string{"UPDATE cn_toss SET state = ?, cancelled_at = ?, mtime = ? WHERE id = ? AND state = ?"}},
		{"重投覆盖枚数而非累加", []string{"UPDATE cn_toss SET state = ?, `count` = ?, last_tossed_at = ?, last_toss_date = ?, cancelled_at = 0"}},
		{"建仓靠唯一键返回 affected", []string{"INSERT INTO cn_account (mid, balance, total_tossed, version, ctime, mtime) VALUES (?, ?, 0, 0, ?, ?) ON DUPLICATE KEY UPDATE mid = mid"}},
		{"日桶靠唯一键保证一行一天", []string{"INSERT INTO cn_daily_toss (mid, date, tossed, ctime, mtime) VALUES (?, ?, 0, ?, ?) ON DUPLICATE KEY UPDATE mtime = mtime"}},
		{"投币事务先锁账户行", []string{"FROM cn_account WHERE mid = ? FOR UPDATE"}},
		{"取消前锁投币行", []string{"WHERE mid = ? AND target_aid = ? LIMIT 1 FOR UPDATE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range tc.mustHas {
				needle := strings.Join(strings.Fields(want), " ")
				if !strings.Contains(src, needle) {
					t.Errorf("SQL 里找不到 %q：\n  这条口径是 %s 的唯一真值来源，被改动时必须同步本用例与假实现",
						needle, tc.name)
				}
			}
		})
	}
}

// TestLedgerTablesAreAppendOnly 台账与投币记录都不许物理删除：
// AGENTS.md §8 要求删除/下架保留审计证据，硬币台账还是对账的唯一依据。
func TestLedgerTablesAreAppendOnly(t *testing.T) {
	src := modelSQLText(t)
	for _, stmt := range []string{
		"DELETE FROM cn_flow", "UPDATE cn_flow",
		"DELETE FROM cn_toss", "DELETE FROM cn_account", "DELETE FROM cn_daily_toss",
	} {
		if strings.Contains(src, stmt) {
			t.Errorf("model 里出现了 %q：台账/账户/日额度都不许物理删除或改写历史", stmt)
		}
	}
	// 台账的更新只允许通过 INSERT 追加；幂等读只按 request_id 定位。
	if !strings.Contains(src, "INSERT INTO cn_flow (") {
		t.Error("cn_flow 必须只有追加写入口")
	}
	if !strings.Contains(src, "WHERE request_id = ?") {
		t.Error("幂等判定必须只按 request_id 唯一键定位流水")
	}
	// 余额的每一次变动都必须在写流水的同一条路径上：model 侧不得存在「只改余额不记账」的入口，
	// 因此三条余额语句各自只出现一次，且都以 Tx 结尾（由 logic 在同一事务内配对写流水）。
	for _, op := range []string{"DeductForTossTx", "RefundForCancelTx", "ApplyGrantTx"} {
		if n := strings.Count(readModelSource(t), "func (m *defaultAccountModel) "+op); n != 1 {
			t.Errorf("账户余额写入口 %s 出现 %d 次，必须是 1 次（多一个入口就多一条不记账的路）", op, n)
		}
	}
}

// TestQueriesBindEveryFilterValue 禁止把外部输入拼进 SQL 文本（注入面）。
func TestQueriesBindEveryFilterValue(t *testing.T) {
	goSrc := readModelSource(t)
	// 允许 fmt.Sprintf 出现在占位符数量拼接上（placeholders），但不允许把值拼进 WHERE。
	if m := reValueInSQLText.FindString(goSrc); m != "" {
		t.Errorf("条件拼接疑似把值写进了 SQL 文本：%s", m)
	}
	// 台账过滤条件全部走占位符。
	for _, want := range []string{"mid = ?", "flow_type = ?", "biz_no = ?", "ctime >= ?", "ctime <= ?"} {
		if !strings.Contains(modelSQLText(t), want) {
			t.Errorf("台账过滤条件缺少占位符 %q", want)
		}
	}
	// 批量聚合的 IN (...) 只许拼占位符，aid 一律走 args。
	if !strings.Contains(modelSQLText(t), "target_aid IN ( ) GROUP BY target_aid") {
		t.Error("SummarizeTargets 的 IN 列表必须是 placeholders 生成的占位符串")
	}
	if !strings.Contains(goSrc, "placeholders(len(aids))") {
		t.Error("IN 列表未使用 placeholders()，aid 有被拼进 SQL 文本的风险")
	}
}

// reValueInSQLText 抓「fmt.Sprintf 里把值拼进 SQL」的写法（列清单/占位符数量是允许的）。
var reValueInSQLText = regexp.MustCompile(`fmt\.Sprintf\([^)]*(?:WHERE|request_id\s*=\s*'|biz_no\s*=\s*'|mid\s*=\s*\d)[^)]*\)`)

func TestTableNamesReferencedByModelExistInMigrations(t *testing.T) {
	declared := map[string]struct{}{}
	for _, exp := range coinTableExpects() {
		tbl := parseCoinDDL(t, exp.table)
		declared[tbl.name] = struct{}{}
	}
	src := modelSQLText(t)
	re := regexp.MustCompile(`(?:FROM|INTO|UPDATE)\s+(cn_[a-z_]+)`)
	seen := map[string]struct{}{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		seen[m[1]] = struct{}{}
		if _, ok := declared[m[1]]; !ok {
			keys := make([]string, 0, len(declared))
			for k := range declared {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Errorf("model 引用了迁移里没建的表 %s（已声明：%v）", m[1], keys)
		}
	}
	if len(seen) == 0 {
		t.Fatal("一条 SQL 都没扫到，用例失去意义")
	}
	// 反向：迁移里声明的表必须都被 model 用到，否则要么 SQL 多余、要么功能没落地。
	for exp := range declared {
		if _, ok := seen[exp]; !ok {
			t.Errorf("迁移建了 %s 但 model 一条 SQL 都没引用它", exp)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
