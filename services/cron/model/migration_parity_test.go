package model

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

// 本文件把「model 的结构体 / SELECT 列常量 与 deploy/migrations/cron 的建表语句逐列一致」
// 变成可执行门禁（AGENTS.md §9）。
//
// 为什么必须有（而不是人工比对）：
//   - cron 的迁移本轮**没有**在 MySQL 上跑过，结构正确性只能靠静态一致性证明；
//   - 结构体 db tag、SELECT 列常量、INSERT 列表是三处必须同步的文本，任一处漂移都会
//     在运行期暴露成 "Unknown column" 或字段扫描错位；
//   - 幂等唯一键（uniq_task_key / uniq_fire_attempt / uniq_lease_key / uniq_task_scope）
//     一旦被删或加错列，表现不是启动失败而是「同一计划点重复产生副作用」这种最贵的故障；
//   - 文本列宽和 model 的截断/校验上限必须对齐：校验放过 4096 字节而列只有 512，
//     写入会在严格模式下报错，等价于「注册成功但永远跑不了」。
//
// 测试不连数据库：只读 SQL 文本，与 model 的常量/结构体 tag 比对。

const migrationsRelDir = "deploy/migrations/cron"

// tableSpec 声明一张 model 表与它的迁移建表语句的对应关系。
type tableSpec struct {
	table string // SQL 里的表名
	// row 是行结构体样例；model 读写的列全集取自它的 db tag。
	row any
	// selectColumns 是 model 里拼进 SELECT 的列常量原值。
	selectColumns string
	// primaryKey 是 model 定位单行 / UPSERT 冲突所使用的列组合。
	primaryKey []string
	// uniqueKeys 是幂等与不变量依赖的唯一索引：键名 -> 列组合（顺序敏感）。
	uniqueKeys map[string][]string
	// requiredIndexes 是 model 的 WHERE / ORDER BY 依赖的普通索引：键名 -> 列组合。
	requiredIndexes map[string][]string
	// textLimits 是「model 侧允许写入的最大字节数」与 SQL 列宽的对应关系。
	// 断言方向：列宽 >= model 上限，否则校验放过而写入报错。
	textLimits map[string]int
	// timeColumns 是本表存在的时间列（清理与排序依赖）。
	timeColumns []string
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:         "cron_task_definition",
			row:           TaskDefinition{},
			selectColumns: taskDefinitionColumns,
			// RegisterTask 的幂等身份：一个 task_key 只有一行定义。
			uniqueKeys: map[string][]string{"uniq_task_key": {"task_key"}},
			primaryKey: []string{"id"},
			requiredIndexes: map[string][]string{
				"idx_state_next_fire": {"state", "next_fire_at"}, // ListDue：state=ENABLED AND next_fire_at<=?
				"idx_group_state":     {"task_group", "state"},   // CountByGroupState / ListByCursor(group)
				"idx_handler":         {"handler"},               // ListByCursor(handler)
				"idx_ctime":           {"ctime"},                 // 运维按建档时间排查
			},
			textLimits: map[string]int{
				"task_key":    MaxTaskKeyBytes,
				"name":        MaxTaskNameBytes,
				"handler":     MaxHandlerBytes,
				"task_group":  MaxTaskGroupBytes,
				"cron_expr":   MaxCronExprBytes,
				"timezone":    MaxTimezoneBytes,
				"params":      MaxParamsBytes,
				"secret_refs": MaxSecretRefsBytes,
				"last_error":  MaxLastErrorBytes,
				"owner":       MaxOwnerBytes,
				"operator":    MaxOperatorBytes,
			},
			timeColumns: []string{"next_fire_at", "last_fire_at", "last_success_at", "ctime", "mtime"},
		},
		{
			table:         "cron_task_run",
			row:           TaskRun{},
			selectColumns: taskRunColumns,
			// 调度幂等的核心：同一计划时刻的同一尝试号只允许一行。
			uniqueKeys: map[string][]string{"uniq_fire_attempt": {"task_key", "planned_at", "attempt"}},
			primaryKey: []string{"id"},
			requiredIndexes: map[string][]string{
				"idx_fire":        {"task_key", "planned_at"},   // FindByFire / MaxAttempt
				"idx_state_retry": {"state", "next_retry_at"},   // ListDueRetrying
				"idx_state_lease": {"state", "lease_expire_at"}, // ListExpiredRunning（孤儿执行回收）
				"idx_planned":     {"planned_at"},               // ListByCursor 的 ORDER BY planned_at DESC
				"idx_ctime":       {"ctime"},                    // CountByStates / DeleteBefore
			},
			textLimits: map[string]int{
				"task_key":       MaxTaskKeyBytes,
				"lease_owner":    MaxLeaseOwnerBytes,
				"result_summary": MaxResultSummaryBytes,
				"last_error":     MaxLastErrorBytes,
				"operator":       MaxOperatorBytes,
				"trace_id":       MaxTraceIDBytes,
			},
			timeColumns: []string{"planned_at", "lease_expire_at", "started_at", "finished_at", "next_retry_at", "ctime", "mtime"},
		},
		{
			table:         "cron_task_lease",
			row:           TaskLease{},
			selectColumns: taskLeaseColumns,
			// 任务级互斥：一个 lease_key 只有一行持有记录。
			uniqueKeys: map[string][]string{"uniq_lease_key": {"lease_key"}},
			primaryKey: []string{"id"},
			requiredIndexes: map[string][]string{
				"idx_task_key": {"task_key"},  // FindByTask / ListByCursor(taskKey)
				"idx_expire":   {"expire_at"}, // ListByCursor 的 ORDER BY expire_at / CountExpired
				"idx_ctime":    {"ctime"},
			},
			// 租约键是 leaseKeyOf(task_key, scope) 组合出来的，所以三者必须一起约束：
			// 只校验 task_key/scope 各自的宽度，组合结果仍可能溢出 VARCHAR(128)。
			textLimits: map[string]int{
				"lease_key":      MaxLeaseKeyBytes,
				"task_key":       MaxTaskKeyBytes,
				"scope":          MaxScopeBytes,
				"owner_instance": MaxLeaseOwnerBytes,
			},
			timeColumns: []string{"acquired_at", "renewed_at", "expire_at", "ctime", "mtime"},
		},
		{
			table:         "cron_task_checkpoint",
			row:           TaskCheckpoint{},
			selectColumns: taskCheckpointColumns,
			// CAS 游标的幂等身份：同一 (task_key, scope_key) 只有一行。
			uniqueKeys: map[string][]string{"uniq_task_scope": {"task_key", "scope_key"}},
			primaryKey: []string{"id"},
			requiredIndexes: map[string][]string{
				"idx_mtime": {"mtime"}, // DeleteBefore(mtime < ?)
			},
			textLimits: map[string]int{
				"task_key":  MaxTaskKeyBytes,
				"scope_key": MaxScopeKeyBytes,
				"value_str": MaxValueStrBytes,
				"operator":  MaxOperatorBytes,
			},
			timeColumns: []string{"ctime", "mtime"},
		},
		{
			table:         "cron_task_audit",
			row:           TaskAudit{},
			selectColumns: taskAuditColumns,
			primaryKey:    []string{"id"},
			// 审计是只追加流水，没有业务唯一键：幂等靠「同事务写入」而不是去重键。
			uniqueKeys: nil,
			requiredIndexes: map[string][]string{
				"idx_task_ctime":   {"task_key", "ctime"}, // ListByCursor(taskKey) + ctime 窗口
				"idx_action_ctime": {"action", "ctime"},   // ListByCursor(action)
				"idx_ctime":        {"ctime"},             // DeleteBefore
			},
			textLimits: map[string]int{
				"task_key":   MaxTaskKeyBytes,
				"action":     MaxAuditActionBytes,
				"from_state": MaxAuditStateBytes,
				"to_state":   MaxAuditStateBytes,
				"operator":   MaxOperatorBytes,
				"trace_id":   MaxTraceIDBytes,
				// Insert 用 truncate(Detail, MaxResultSummaryBytes*2) 控制长度。
				"detail": MaxResultSummaryBytes * 2,
			},
			timeColumns: []string{"ctime"},
		},
	}
}

// --- SQL 解析 ---

type sqlColumn struct {
	name          string
	dataType      string
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

func (t *sqlTable) index(name string) (sqlIndex, bool) {
	for _, i := range t.indexes {
		if i.name == name {
			return i, true
		}
	}
	return sqlIndex{}, false
}

// findMigrationsDir 从当前目录向上找仓库根的 deploy/migrations/cron。
// 不写死相对层数：`go test ./services/cron/model` 与在 model 目录内直接跑 cwd 不同。
func findMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "deploy", "migrations", "cron")
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

// parseCreateTables 从迁移目录里抽出所有 CREATE TABLE 块。
// 只认本仓库既有书写风格（反引号标识符、每列一行、) ENGINE=... 收尾）：
// 格式被改坏时报错，而不是悄悄解析出一张空表。一个文件允许多张表（cron 按主题分组）。
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
				cur = &sqlTable{table: name, file: base}
			case cur != nil && strings.HasPrefix(trimmed, ")"):
				if !strings.Contains(trimmed, "ENGINE=InnoDB") || !strings.Contains(trimmed, "utf8mb4") {
					t.Fatalf("%s: 表 %s 收尾行必须显式 ENGINE=InnoDB 与 CHARSET=utf8mb4，得到 %q",
						base, cur.table, trimmed)
				}
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

// parseTableLine 解析表体里的一行（列定义 / 主键 / 唯一键 / 普通索引）。
func parseTableLine(t *testing.T, cur *sqlTable, file, line string) {
	t.Helper()
	body := strings.TrimSuffix(line, ",")
	switch {
	case strings.HasPrefix(body, "`"):
		name, ok := backtickedIdentifier(body)
		if !ok {
			t.Fatalf("%s: 列定义无法解析: %q", file, line)
		}
		if cur.hasColumn(name) {
			t.Fatalf("%s: 列 %s 重复声明", file, name)
		}
		// 只在 COMMENT 之前的定义段上判定 NOT NULL / DEFAULT：
		// 中文注释里出现「NULL」「默认值」字样不能影响结构判定。
		defined := body
		comment := ""
		if i := strings.Index(body, " COMMENT '"); i >= 0 {
			defined = body[:i]
			comment = strings.TrimSuffix(body[i+len(" COMMENT '"):], "'")
		}
		if comment == "" {
			t.Fatalf("%s: 表 %s 的列 %s 缺少 COMMENT —— cron 的数值枚举全靠注释表达含义", file, cur.table, name)
		}
		cur.columns = append(cur.columns, sqlColumn{
			name:          name,
			dataType:      defined,
			comment:       comment,
			notNull:       strings.Contains(defined, " NOT NULL"),
			hasDefault:    strings.Contains(defined, " DEFAULT "),
			autoIncrement: strings.Contains(defined, " AUTO_INCREMENT"),
		})
	case strings.HasPrefix(body, "PRIMARY KEY"):
		cur.primary = columnListInParentheses(t, file, body)
	case strings.HasPrefix(body, "UNIQUE KEY"):
		name, _ := identifierAfterKeyword(body, "UNIQUE KEY")
		cur.addIndex(t, file, name, true, columnListInParentheses(t, file, body))
	case strings.HasPrefix(body, "KEY"):
		name, _ := identifierAfterKeyword(body, "KEY")
		cur.addIndex(t, file, name, false, columnListInParentheses(t, file, body))
	default:
		t.Fatalf("%s: 表 %s 内出现无法识别的定义行（FOREIGN KEY / CHECK / 生成列都不符合本服务约定，需先确认）: %q",
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

func (t *sqlTable) hasColumn(name string) bool {
	_, ok := t.column(name)
	return ok
}

var varcharLenRe = regexp.MustCompile(`VARCHAR\((\d+)\)`)

// varcharLen 返回列宽（非 VARCHAR 时 ok=false）。
func (c sqlColumn) varcharLen() (int, bool) {
	m := varcharLenRe.FindStringSubmatch(strings.ToUpper(c.dataType))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
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
			t.Fatalf("%s: 索引列必须是反引号标识符（带前缀长度或排序方向的写法无法比对，请改写）: %q", file, p)
		}
		out = append(out, name)
	}
	return out
}

// --- 断言 ---

// structColumns 取行结构体的 db tag 全集（声明顺序）。
func structColumns(t *testing.T, row any) []string {
	t.Helper()
	v := reflect.TypeOf(row)
	if v.Kind() != reflect.Struct {
		t.Fatalf("%T 不是结构体", row)
	}
	out := make([]string, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		tag := v.Field(i).Tag.Get("db")
		if tag == "" {
			t.Fatalf("%s 的字段 %s 缺少 db tag —— model 无法扫描该列", v.Name(), v.Field(i).Name)
		}
		out = append(out, tag)
	}
	return out
}

func TestMigrationFilesCoverEveryTable(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		if _, ok := tables[spec.table]; !ok {
			t.Errorf("迁移里没有建表语句 %s（model 有对应结构体）", spec.table)
		}
	}
	// 反向：迁移里也不允许出现「model 不认识」的表，否则会有一张没人读写却要吃备份的表。
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
					t.Errorf("SQL 列 %s 没有任何结构体字段承载 —— 该列永远不会被读写，请删除或补 model",
						c.name)
				}
			}
			if len(fields) != len(tbl.columns) {
				t.Errorf("列数不符：结构体 %d 列，SQL %d 列", len(fields), len(tbl.columns))
			}
		})
	}
}

func TestSelectColumnConstantsMatchSQL(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := tables[spec.table]
			cols := strings.Split(spec.selectColumns, ",")
			var want []string
			for _, c := range structColumns(t, spec.row) {
				want = append(want, c)
			}
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
					t.Errorf("结构体列 %s 没进 SELECT 常量 —— FindOne 会读不到该列", c)
				}
			}
			// id 必须在最前：Insert 之后回读与分页都按它定位。
			if !strings.HasPrefix(spec.selectColumns, "id,") {
				t.Errorf("SELECT 常量应以 id 开头，得到 %q", spec.selectColumns)
			}
		})
	}
}

func TestIdempotencyKeysAreUniqueInSQL(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl, ok := tables[spec.table]
			if !ok {
				t.Fatalf("迁移里没有 %s", spec.table)
			}
			if len(spec.primaryKey) == 0 {
				t.Fatalf("%s 必须声明主键", spec.table)
			}
			if strings.Join(tbl.primary, ",") != strings.Join(spec.primaryKey, ",") {
				t.Errorf("主键不符：SQL %v，期望 %v", tbl.primary, spec.primaryKey)
			}
			for name, cols := range spec.uniqueKeys {
				idx, ok := tbl.index(name)
				if !ok {
					t.Fatalf("%s 缺少唯一索引 %s —— 幂等失去数据库兜底，会重复产生副作用", spec.table, name)
				}
				if !idx.unique {
					t.Fatalf("%s 的 %s 必须是 UNIQUE KEY，普通 KEY 起不到去重作用", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("唯一索引 %s 列不符：SQL %v，期望 %v（顺序敏感）", name, idx.cols, cols)
				}
			}
			// 有幂等写入语义的表必须至少有一个唯一键（audit 是只追加流水，例外）。
			if len(spec.uniqueKeys) == 0 && spec.table != "cron_task_audit" {
				t.Errorf("%s 没有声明任何唯一键 —— 写接口无法靠数据库去重", spec.table)
			}
		})
	}
}

func TestQueryIndexesExist(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := tables[spec.table]
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

// TestTextColumnWidthMatchesModelLimits 钉住「校验上限 <= 列宽」这条不变量。
// 反例（本文件最初的失败用例）：secret_refs 按 MaxParamsBytes(4096) 校验，
// 而列宽只有 VARCHAR(512) —— 注册能通过、写入必炸。
func TestTextColumnWidthMatchesModelLimits(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := tables[spec.table]
			for column, limit := range spec.textLimits {
				c, ok := tbl.column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				width, ok := c.varcharLen()
				if !ok {
					t.Fatalf("%s.%s 不是 VARCHAR，无法与字节上限比对", spec.table, column)
				}
				if width < limit {
					t.Errorf("%s.%s 列宽 VARCHAR(%d) 小于 model 允许的 %d 字节 —— "+
						"校验放过而写入报错，请把上限收敛到列宽或加宽列", spec.table, column, width, limit)
				}
			}
		})
	}
}

// TestEveryTextColumnHasDeclaredLimit 防止「加了列忘了定上限」。
//
// textLimits 是手工维护的清单，新增 VARCHAR 列如果没进来，
// TestTextColumnWidthMatchesModelLimits 就永远看不到它，校验缺口会以写入报错的形式复活。
func TestEveryTextColumnHasDeclaredLimit(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := tables[spec.table]
			for _, c := range tbl.columns {
				if _, ok := c.varcharLen(); !ok {
					continue
				}
				if _, ok := spec.textLimits[c.name]; !ok {
					t.Errorf("%s.%s 是 VARCHAR(%d) 但 model 侧没声明字节上限：%s",
						spec.table, c.name, mustWidth(t, c),
						"要么在 errors.go 定上限并在 logic 里校验，要么在本表 spec 里写明为什么不校验")
				}
			}
		})
	}
}

// TestComposedLeaseKeyFitsColumn 钉住 lease_key 的拼接算术。
//
// lease_key = task_key + "/" + scope（见 logic.leaseKeyOf），列宽只有 VARCHAR(128)。
// 三者的上限必须满足「最坏拼接 <= 列宽」，否则两个各自合规的入参能拼出一条插不进去的键，
// 表现为 AcquireLease 永远失败 —— 而失败点在下游 INSERT，报错离根因很远。
func TestComposedLeaseKeyFitsColumn(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	col, ok := tables["cron_task_lease"].column("lease_key")
	if !ok {
		t.Fatalf("cron_task_lease 缺少 lease_key")
	}
	width, ok := col.varcharLen()
	if !ok {
		t.Fatalf("lease_key 不是 VARCHAR")
	}
	// leaseKeyOf 用 "/" 拼接，占 1 字节。
	const separatorBytes = 1
	worst := MaxTaskKeyBytes + separatorBytes + MaxScopeBytes
	if worst > width {
		t.Errorf("最坏 lease_key 长度 %d（task_key %d + 分隔符 1 + scope %d）超过列宽 VARCHAR(%d)",
			worst, MaxTaskKeyBytes, MaxScopeBytes, width)
	}
	if MaxLeaseKeyBytes != width {
		t.Errorf("MaxLeaseKeyBytes=%d 应与 lease_key 列宽 %d 相等，否则 logic 校验与库不一致", MaxLeaseKeyBytes, width)
	}
}

func mustWidth(t *testing.T, c sqlColumn) int {
	t.Helper()
	w, _ := c.varcharLen()
	return w
}

func TestEnumColumnsDocumentEveryValue(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	cases := []struct {
		table, column string
		values        []int32
	}{
		{"cron_task_definition", "schedule_type", []int32{ScheduleTypeCron, ScheduleTypeInterval, ScheduleTypeManual}},
		{"cron_task_definition", "misfire_policy", []int32{MisfirePolicyFireOnceNow, MisfirePolicySkipToNext, MisfirePolicyFireAll}},
		{"cron_task_definition", "state", []int32{TaskStateEnabled, TaskStatePaused, TaskStateDisabled}},
		{"cron_task_run", "state", []int32{RunStatePending, RunStateRunning, RunStateRetrying, RunStateSucceeded,
			RunStateFailed, RunStateTimeout, RunStateCanceled, RunStateSkipped}},
		{"cron_task_run", "trigger_type", []int32{TriggerTypeScheduled, TriggerTypeManual, TriggerTypeRetry, TriggerTypeReplay}},
	}
	for _, c := range cases {
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			tbl, ok := tables[c.table]
			if !ok {
				t.Fatalf("迁移里没有 %s", c.table)
			}
			col, ok := tbl.column(c.column)
			if !ok {
				t.Fatalf("%s 缺少列 %s", c.table, c.column)
			}
			if !strings.Contains(strings.ToUpper(col.dataType), "TINYINT") &&
				!strings.Contains(strings.ToUpper(col.dataType), "INT") {
				t.Errorf("%s.%s 应为整型列，得到 %q", c.table, c.column, col.dataType)
			}
			tokens := map[string]struct{}{}
			for _, n := range numericTokens(col.comment) {
				tokens[n] = struct{}{}
			}
			for _, v := range c.values {
				if _, ok := tokens[strconv.Itoa(int(v))]; !ok {
					t.Errorf("%s.%s 的注释没有说明取值 %d（当前注释 %q）—— 新增枚举值必须同步建表注释",
						c.table, c.column, v, col.comment)
				}
			}
		})
	}
}

func TestTimeColumnsAreUnixSecondsBigInt(t *testing.T) {
	// README/契约规定「除 duration_ms 外全部使用 Unix 秒（BIGINT）」：
	// 一旦有人把某列改成 DATETIME，logic 里所有 int64 比较都会错位。
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := tables[spec.table]
			for _, name := range spec.timeColumns {
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少时间列 %s", spec.table, name)
				}
				if !strings.Contains(strings.ToUpper(c.dataType), "BIGINT") {
					t.Errorf("%s.%s 必须是 BIGINT（Unix 秒），得到 %q", spec.table, name, c.dataType)
				}
			}
		})
	}
}

func TestNoLegacyCommercialOrMiniProgramColumns(t *testing.T) {
	// 范围守卫（AGENTS.md §1）：cron 不得承载会员/订单/支付/广告/小程序语义的列。
	// 这条测试的存在是为了让「顺手加一列 charge_flag」在评审阶段就被拦下。
	tables := parseCreateTables(t, findMigrationsDir(t))
	forbidden := []string{"pay", "order", "member", "coin", "advert", "mini", "vip", "revenue"}
	for name, tbl := range tables {
		for _, c := range tbl.columns {
			lower := strings.ToLower(c.name)
			for _, f := range forbidden {
				if strings.Contains(lower, f) {
					t.Errorf("%s.%s 命中了范围外关键词 %q —— 本期不做商业化/小程序", name, c.name, f)
				}
			}
		}
	}
}

func TestAllColumnsNotNullWithDefault(t *testing.T) {
	// cron 的列全部 NOT NULL + 默认值：model 用普通结构体扫描（不用 sql.Null*），
	// 允许 NULL 会在扫描时报错；没有默认值则要求每条 INSERT 都显式给列。
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := tables[spec.table]
			for _, c := range tbl.columns {
				if c.autoIncrement {
					continue
				}
				if !c.notNull {
					t.Errorf("%s.%s 允许 NULL —— model 结构体没有 sql.Null* 承载，扫描会失败",
						spec.table, c.name)
				}
				if !c.hasDefault {
					t.Errorf("%s.%s 没有 DEFAULT —— 漏传该列会直接报错，而 model 的 INSERT 列表一旦收窄就会踩到",
						spec.table, c.name)
				}
			}
		})
	}
}

func numericTokens(text string) []string {
	return numericTokenRe.FindAllString(text, -1)
}

var numericTokenRe = regexp.MustCompile(`\d+`)

func TestMigrationsAreSelfDescribing(t *testing.T) {
	// 运维要求每个文件都能独立回答「影响面与回滚」（AGENTS.md §10）。
	dir := findMigrationsDir(t)
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("枚举 %s 失败: %v", dir, err)
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			text := string(raw)
			for _, marker := range []string{"owner", "回滚", "影响"} {
				if !strings.Contains(text, marker) {
					t.Errorf("迁移头注释缺少 %q 段落（回滚与影响面必须写清）", marker)
				}
			}
			for _, banned := range []string{"TRUNCATE", "DROP DATABASE", "GRANT ", "FOREIGN KEY"} {
				if containsOutsideComments(text, banned) {
					t.Errorf("迁移里出现了禁止语句 %q", banned)
				}
			}
			if !strings.Contains(text, "go_video_cron") {
				t.Error("迁移未声明所属库 go_video_cron，无法确认数据所有权")
			}
		})
	}
}

// containsOutsideComments 只在可执行 SQL 行里找关键词：
// 头注释里正常会出现「TRUNCATE 不允许」「不建 FOREIGN KEY」这类说明文字。
func containsOutsideComments(text, keyword string) bool {
	upper := strings.ToUpper(keyword)
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		if strings.Contains(strings.ToUpper(t), upper) {
			return true
		}
	}
	return false
}

func TestSpecTableNamesResolveToExistingConstants(t *testing.T) {
	// 自检：tableSpecs 引用的列常量必须与结构体同数量，防止「加了列忘了改常量」。
	for _, spec := range tableSpecs() {
		cols := strings.Count(spec.selectColumns, ",") + 1
		fields := len(structColumns(t, spec.row))
		if cols != fields {
			t.Errorf("%s 的 SELECT 常量 %d 列，结构体 %d 列", spec.table, cols, fields)
		}
		if !strings.Contains(fmt.Sprintf("%T", spec.row), "model.") {
			t.Errorf("%s 的行样例 %T 不在 model 包内", spec.table, spec.row)
		}
	}
}
