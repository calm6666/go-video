package model

// 迁移脚本与 model 层的一致性守卫（对齐 services/recommend-rank/model/migration_sync_test.go）。
//
// 只读文件、绝不连 MySQL（AGENTS.md §9：单测不得触达真实中间件）。
// 它拦住的是本服务已经真实发生过的四类事故：
//  1. model 结构与迁移列只改了一侧（上线即 Unknown column）；
//  2. 写入列清单漏列：struct 字段已赋值、SQL 里却没有那一列，于是列恒为默认值。
//     feature_version_switch 的 ctime 就踩过这个坑 —— 审计行的时间永远是 0，
//     ListVersionSwitches 的 since/until 与 idx_ctime 一起失效；
//  3. 幂等判定依赖的唯一键缺失（ErrFeatureVersionExists / ErrJobExists /
//     ErrRequestIdReused / Begin 的 RowsAffected=0 探测全部来自唯一键冲突）；
//  4. 迁移越界写建库、切库或数据变更语句（那是 scripts/migrate.ps1 的职责）。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const migrationsDir = "../../../deploy/migrations/feature-store"

// databaseName 与 config.DatabaseName 同值；这里不 import config（会造成 import cycle 风险），
// 由 TestMigrationFilesCarryOwnerAndRollback 断言每个文件都写明了库名。
const databaseName = "go_video_feature_store"

var (
	// modelSQLTablePattern 抓 model 层 SQL 里引用的表名（裸表名，无反引号）。
	modelSQLTablePattern = regexp.MustCompile(`(?:INSERT(?:\s+IGNORE)?\s+INTO|UPDATE|FROM|JOIN)\s+(feature_[a-z_]+)`)
	// createTablePattern 抓建表语句：表名、括号内定义、以及 ENGINE 到语句结尾。
	createTablePattern = regexp.MustCompile("(?s)CREATE TABLE IF NOT EXISTS `(feature_[a-z_]+)` \\((.*?)\n\\) (ENGINE.*?;)\n")
	// columnDefPattern 抓列定义行（两空格缩进 + 反引号列名 + 类型）。列名含数字（int64_value）。
	columnDefPattern = regexp.MustCompile("(?m)^  `([a-z0-9_]+)`\\s+\\S")
	// autoIncrementPattern 标记由数据库计算的列：它们不出现在任何写入清单里。
	autoIncrementPattern = regexp.MustCompile(`(?i)AUTO_INCREMENT`)
	// dbTagPattern 抓结构体的 db tag（同样含数字列名）。
	dbTagPattern = regexp.MustCompile(`db:"([a-z0-9_]+)"`)
	// goCommentPattern 用于剥掉 Go 源码里的 `//` 行注释，避免把说明文字当 SQL 解析。
	goCommentPattern = regexp.MustCompile(`(?m)^\s*//.*$`)
	// fileVersionPattern 是迁移文件命名规范（deploy/migrations/README.md）。
	fileVersionPattern = regexp.MustCompile(`^\d{6}_[a-z0-9_]+\.sql$`)
	// bannedStatementPatterns 是迁移里不允许出现的语句：建库/切库/授权由
	// scripts/migrate.ps1 与运维负责，数据变更（含回填与清理）更不是 DDL 的职责。
	// 每条都带词边界，所以 entities_truncated 这类列名不会被误判。
	bannedStatementPatterns = []*bannedStatement{
		{"CREATE DATABASE", regexp.MustCompile(`(?i)\bCREATE\s+DATABASE\b`)},
		{"CREATE SCHEMA", regexp.MustCompile(`(?i)\bCREATE\s+SCHEMA\b`)},
		{"SCHEMA_MIGRATIONS", regexp.MustCompile(`(?i)\bSCHEMA_MIGRATIONS\b`)},
		{"USE", regexp.MustCompile(`(?i)\bUSE\b`)},
		{"GRANT", regexp.MustCompile(`(?i)\bGRANT\b`)},
		{"DELETE FROM", regexp.MustCompile(`(?i)\bDELETE\s+FROM\b`)},
		{"INSERT INTO", regexp.MustCompile(`(?i)\bINSERT\s+INTO\b`)},
		{"DROP TABLE", regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)},
		{"ALTER TABLE", regexp.MustCompile(`(?i)\bALTER\s+TABLE\b`)},
		{"TRUNCATE", regexp.MustCompile(`(?i)\bTRUNCATE\b`)},
		{"UPDATE", regexp.MustCompile(`(?i)\bUPDATE\b`)},
		{"SELECT", regexp.MustCompile(`(?i)\bSELECT\b`)},
	}
)

// bannedStatement 保留可读的语句名，匹配用编译好的正则。
type bannedStatement struct {
	name string
	re   *regexp.Regexp
}

// findBannedStatement 返回迁移骨架里第一条越界语句的名字，没有则空串。
func findBannedStatement(sql string) string {
	for _, b := range bannedStatementPatterns {
		if b.re.MatchString(sql) {
			return b.name
		}
	}
	return ""
}

// tableConsts 把表名对到 model 里的列清单常量：
// selectConst 是读列（含自增主键），insertConst 是写列（不含自增主键；
// feature_definition 无自增列，读写共用同一份清单，故 insertConst 为空）。
type tableConsts struct {
	selectConst string
	insertConst string
}

var tableConstsByName = map[string]tableConsts{
	"feature_definition":     {selectConst: "featureDefinitionColumns"},
	"feature_active_version": {selectConst: "featureActiveVersionColumns", insertConst: "featureActiveVersionInsertColumns"},
	"feature_value":          {selectConst: "featureValueColumns", insertConst: "featureValueInsertColumns"},
	"feature_version_switch": {selectConst: "featureVersionSwitchColumns", insertConst: "featureVersionSwitchInsertColumns"},
	"feature_backfill_job":   {selectConst: "featureBackfillJobColumns", insertConst: "featureBackfillJobInsertColumns"},
	"feature_write_receipt":  {selectConst: "featureWriteReceiptColumns", insertConst: "featureWriteReceiptInsertColumns"},
}

// requiredKeys 是代码实际依赖的主键/唯一键：缺一个就丢掉一条幂等或唯一性承诺。
var requiredKeys = map[string][]string{
	// RegisterFeature 的「同 (key, version) 不原地改写」探测（RowsAffected=0 → ErrFeatureVersionExists）。
	"feature_definition": {"PRIMARY KEY (`feature_key`, `version`)"},
	// 「同一时刻只有一个生效版本」这条不变量的数据库层保证（单行指针表）。
	"feature_active_version": {"UNIQUE KEY `uniq_feature_key` (`feature_key`)"},
	// BatchUpsert 的整行幂等 + PurgeExpired 的「先选主键再批删」路径。
	"feature_value": {
		"UNIQUE KEY `uniq_feature_entity` (`feature_key`, `version`, `entity_scope`, `entity_id`)",
		"KEY `idx_expire` (`expire_at`, `value_id`)",
	},
	// Append 的 1062 → ErrRequestIdReused（同请求同类型不得留两条审计）。
	"feature_version_switch": {"UNIQUE KEY `uniq_request_switch` (`request_id`, `switch_type`)"},
	// SubmitBackfillJob 的整请求幂等（ErrJobExists）。
	"feature_backfill_job": {"UNIQUE KEY `uniq_request_id` (`request_id`)"},
	// WriteReceiptModel.Begin 的执行权获取（op_type 参与唯一键，防跨接口串味）。
	"feature_write_receipt": {"UNIQUE KEY `uniq_request_op` (`request_id`, `op_type`)"},
}

type ddlTable struct {
	name       string
	body       string
	tail       string
	file       string
	columns    []string
	columnSet  map[string]bool
	autoColumn map[string]bool
}

func goFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob model files: %v", err)
	}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("no model source file found: test layout drifted")
	}
	return out
}

func readGo(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func migrationFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no migration under %s: the service must ship its own DDL", migrationsDir)
	}
	sort.Strings(files)
	return files
}

func readMigrations(t *testing.T) []ddlTable {
	t.Helper()
	var out []ddlTable
	seen := map[string]string{}
	for _, f := range migrationFiles(t) {
		src := readGo(t, f)
		for _, m := range createTablePattern.FindAllStringSubmatch(src, -1) {
			if prev, dup := seen[m[1]]; dup {
				t.Fatalf("table %s created twice (%s and %s): migrations are append-only",
					m[1], prev, filepath.Base(f))
			}
			seen[m[1]] = filepath.Base(f)
			tt := ddlTable{name: m[1], body: m[2], tail: m[3], file: filepath.Base(f),
				columnSet: map[string]bool{}, autoColumn: map[string]bool{}}
			for _, line := range strings.Split(m[2], "\n") {
				cm := columnDefPattern.FindStringSubmatch(strings.TrimRight(line, "\r"))
				if cm == nil {
					continue
				}
				tt.columns = append(tt.columns, cm[1])
				tt.columnSet[cm[1]] = true
				if autoIncrementPattern.MatchString(line) {
					tt.autoColumn[cm[1]] = true
				}
			}
			if len(tt.columns) == 0 {
				t.Fatalf("%s: table %s has no parsable column definition", tt.file, tt.name)
			}
			out = append(out, tt)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no CREATE TABLE IF NOT EXISTS parsed from %s", migrationsDir)
	}
	return out
}

// constColumns 从 model 源码里取出「a + b + c」拼接的列清单常量并切成列名。
// 找不到就直接失败：常量改名或挪走时，本测试必须比上线更早告诉你。
func constColumns(t *testing.T, name string) []string {
	t.Helper()
	for _, f := range goFiles(t) {
		src := readGo(t, f)
		m := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(name) + `\s*=\s*((?:"[^"]*"\s*\+?\s*)+)`).
			FindStringSubmatch(src)
		if m == nil {
			continue
		}
		lits := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1)
		var joined strings.Builder
		for _, l := range lits {
			joined.WriteString(l[1])
		}
		parts := strings.Split(joined.String(), ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if c := strings.TrimSpace(p); c != "" {
				out = append(out, c)
			}
		}
		return out
	}
	t.Fatalf("column const %s not found in model sources: renamed or deleted?", name)
	return nil
}

func modelTables(t *testing.T) map[string]bool {
	t.Helper()
	tables := map[string]bool{}
	for _, f := range goFiles(t) {
		src := goCommentPattern.ReplaceAllString(readGo(t, f), "")
		for _, m := range modelSQLTablePattern.FindAllStringSubmatch(src, -1) {
			tables[m[1]] = true
		}
	}
	if len(tables) == 0 {
		t.Fatal("no feature_* table referenced by model: SQL pattern drifted")
	}
	return tables
}

func TestMigrationCoversExactlyModelTables(t *testing.T) {
	fromModel := modelTables(t)
	inDDL := map[string]bool{}
	for _, tt := range readMigrations(t) {
		inDDL[tt.name] = true
	}
	if len(inDDL) != len(tableConstsByName) {
		t.Fatalf("expect %d tables in DDL, got %d (%v)", len(tableConstsByName), len(inDDL), keys(inDDL))
	}
	for name := range tableConstsByName {
		if !fromModel[name] {
			t.Fatalf("model 应当引用 %s（表清单见 types.go 包注释），实际 SQL 里没有", name)
		}
		if !inDDL[name] {
			t.Fatalf("model 使用 %s 但 %s 没有建表语句", name, migrationsDir)
		}
	}
	for name := range inDDL {
		if !fromModel[name] {
			t.Fatalf("%s 建了表但没有任何 model SQL 引用它（死表？）", name)
		}
	}
}

// TestSelectColumnsEqualDDLColumns 逐列、逐序比对：model 的读列清单必须与 DDL 完全一致。
func TestSelectColumnsEqualDDLColumns(t *testing.T) {
	for _, tt := range readMigrations(t) {
		c, ok := tableConstsByName[tt.name]
		if !ok {
			t.Fatalf("%s: 未在 tableConstsByName 登记列常量，新增表必须同时登记", tt.name)
		}
		cols := constColumns(t, c.selectConst)
		if len(cols) != len(tt.columns) {
			t.Fatalf("%s: model 读列 %d 个 / DDL %d 个（model=%v ddl=%v）",
				tt.name, len(cols), len(tt.columns), cols, tt.columns)
		}
		for i := range cols {
			if cols[i] != tt.columns[i] {
				t.Fatalf("%s: 第 %d 列不一致 model=%s ddl=%s", tt.name, i, cols[i], tt.columns[i])
			}
		}
	}
}

// TestWritableColumnsAreAllInserted 是本测试存在的核心理由：
// DDL 里每个非自增列都必须出现在 model 的写入列清单中。
// 漏列的列会永远停留在 DEFAULT 值上（ctime=0 这类「看着正常、其实从没写过」的缺陷
// 在编译期和单测里都发现不了，只能靠这种结构性比对）。
func TestWritableColumnsAreAllInserted(t *testing.T) {
	for _, tt := range readMigrations(t) {
		c := tableConstsByName[tt.name]
		if c.insertConst == "" {
			// 无自增列的表（feature_definition）读写共用一份清单，
			// 那就要求确实没有自增列，否则默认值列会静默丢失。
			for _, col := range tt.columns {
				if tt.autoColumn[col] {
					t.Fatalf("%s: 列 %s 是自增列但表未声明写入列清单常量，确认是否漏配 %sInsertColumns",
						tt.name, col, tt.name)
				}
			}
			continue
		}
		ins := constColumns(t, c.insertConst)
		insSet := map[string]bool{}
		for _, col := range ins {
			insSet[col] = true
		}
		for _, col := range tt.columns {
			if tt.autoColumn[col] {
				if insSet[col] {
					t.Fatalf("%s: 自增列 %s 出现在写入清单 %s 里（依赖 sql_mode 传 0 是危险写法）",
						tt.name, col, c.insertConst)
				}
				continue
			}
			if !insSet[col] {
				t.Fatalf("%s: DDL 列 %s 未出现在 %s 中 —— 该列会恒为 DEFAULT 值",
					tt.name, col, c.insertConst)
			}
		}
		for col := range insSet {
			if !tt.columnSet[col] {
				t.Fatalf("%s: 写入清单列 %s 在 DDL 里不存在", tt.name, col)
			}
		}
	}
}

func TestMigrationColumnsMatchModelTags(t *testing.T) {
	ddlCols := map[string]string{}
	for _, tt := range readMigrations(t) {
		for _, c := range tt.columns {
			ddlCols[c] = tt.name
		}
	}
	tagCols := map[string]bool{}
	for _, f := range goFiles(t) {
		for _, m := range dbTagPattern.FindAllStringSubmatch(readGo(t, f), -1) {
			tagCols[m[1]] = true
		}
	}
	if len(tagCols) == 0 {
		t.Fatal("no db tag found: pattern drifted")
	}
	for col := range tagCols {
		if _, ok := ddlCols[col]; !ok {
			t.Fatalf("db tag %q 在 %s 的 DDL 里没有对应列", col, migrationsDir)
		}
	}
	for col, table := range ddlCols {
		if !tagCols[col] {
			t.Fatalf("%s 的列 %q 没有被任何 model 结构体 db tag 映射", table, col)
		}
	}
}

func TestMigrationIdempotencyKeysExist(t *testing.T) {
	byName := map[string]ddlTable{}
	for _, tt := range readMigrations(t) {
		byName[tt.name] = tt
	}
	for name, keys := range requiredKeys {
		tt, ok := byName[name]
		if !ok {
			t.Fatalf("必需表 %s 未在迁移里出现", name)
		}
		hay := strings.Join(strings.Fields(tt.body), " ")
		for _, k := range keys {
			if !strings.Contains(hay, strings.Join(strings.Fields(k), " ")) {
				t.Fatalf("%s: 代码的幂等/唯一性判定依赖 %s，DDL 里没有", name, k)
			}
		}
	}
}

func TestMigrationTablesAreSelfDescribing(t *testing.T) {
	for _, tt := range readMigrations(t) {
		if !strings.Contains(strings.Join(strings.Fields(tt.tail), ""), "ENGINE=InnoDB") {
			t.Fatalf("%s.%s: 必须 ENGINE = InnoDB（指针切换 + 审计行需要真事务）", tt.file, tt.name)
		}
		if !strings.Contains(strings.Join(strings.Fields(tt.tail), ""), "DEFAULTCHARSET=utf8mb4") {
			t.Fatalf("%s.%s: 必须 utf8mb4（中文注释与 reason 列）", tt.file, tt.name)
		}
		if !strings.Contains(tt.tail, "COMMENT =") && !strings.Contains(tt.tail, "COMMENT=") {
			t.Fatalf("%s.%s: 缺表级 COMMENT", tt.file, tt.name)
		}
		if strings.Count(tt.body, "COMMENT '") < len(tt.columns) {
			t.Fatalf("%s.%s: 每列都要有 COMMENT（%d 列）", tt.file, tt.name, len(tt.columns))
		}
	}
}

// TestMigrationHasNoForeignKeyOrPIILabel 守住两条领域边界：
//   - 不建跨库外键（AGENTS.md §5：跨服务只传主键与受控标识）；
//   - 不出现明文 PII 列名（隐私标识必须是 hash 形态，见 model.ValidEntityID）。
func TestMigrationHasNoForeignKeyOrPIILabel(t *testing.T) {
	pii := []string{"`phone", "`mobile", "`id_card", "`email`", "`ip`", "`device_id`", "`imei`", "`mac`"}
	for _, tt := range readMigrations(t) {
		if strings.Contains(strings.ToUpper(tt.body), "FOREIGN KEY") {
			t.Fatalf("%s: 禁止外键（跨服务数据所有权由 RPC 承担，见 AGENTS.md §5）", tt.name)
		}
		lower := strings.ToLower(tt.body)
		for _, p := range pii {
			if strings.Contains(lower, strings.ToLower(p)) {
				t.Fatalf("%s: 列名 %s 是明文 PII 形态，个体维度只允许加盐哈希摘要", tt.name, p)
			}
		}
	}
}

func TestMigrationFilesCarryOwnerAndRollback(t *testing.T) {
	for _, f := range migrationFiles(t) {
		src := readGo(t, f)
		name := filepath.Base(f)

		if !fileVersionPattern.MatchString(name) {
			t.Fatalf("%s: 文件名必须是 NNNNNN_动词_表名.sql", name)
		}
		for _, keyword := range []string{"数据所有者", "回滚", "锁风险", "用途", databaseName} {
			if !strings.Contains(src, keyword) {
				t.Fatalf("%s: 头部必须写明 %s（deploy/migrations/README.md 的硬约定）", name, keyword)
			}
		}
		// 建库、切库与数据变更由 scripts/migrate.ps1 与运维负责，DDL 只碰自己的表。
		// 词边界匹配而非裸子串：entities_truncated 这类列名里就含 TRUNCATE，
		// 用 strings.Contains 会把合法 DDL 判成越界语句。
		if banned := findBannedStatement(sqlOnly(src)); banned != "" {
			t.Fatalf("%s: 含越界语句 %s（迁移必须是纯 CREATE TABLE IF NOT EXISTS）", name, banned)
		}
		for _, m := range createTablePattern.FindAllStringSubmatch(src, -1) {
			if !strings.Contains(src, "DROP TABLE IF EXISTS `"+m[1]+"`") {
				t.Fatalf("%s: 头部回滚段必须写明 DROP TABLE IF EXISTS `%s`", name, m[1])
			}
			if !strings.Contains(src, "CREATE TABLE IF NOT EXISTS `"+m[1]+"`") {
				t.Fatalf("%s: 建表必须幂等（IF NOT EXISTS）：%s", name, m[1])
			}
		}
	}
}

// sqlOnly 剥掉 SQL 的 `--` 行注释、`/* */` 块注释与单引号字面量，只留语句骨架。
//
// 必须连字面量一起剥：列注释里的描述文字（例如 feature_value.event_time 的
// 「…UpsertCtx 的 UPDATE 带 event_time <= ? 守卫…」）不是可执行语句，
// 但只看 `--` 注释的版本会把它们扫成「含越界语句 UPDATE」而误报。
// 逐字符扫描（而非正则）才能正确处理 MySQL 的 ” 与 \' 两种转义，
// 避免正则把两个字面量之间的正常 SQL 也吞掉。
func sqlOnly(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case src[i] == '\'':
			i++ // 跳过左引号
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '\'' {
					if i+1 < len(src) && src[i+1] == '\'' {
						i += 2
						continue
					}
					i++ // 右引号
					break
				}
				i++
			}
			b.WriteString("''")
		case strings.HasPrefix(src[i:], "--"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += 2 + end + 2
			}
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
