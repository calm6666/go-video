package model

// 迁移脚本与 model 层的一致性守卫（对应「DDL 严格由代码派生」的要求）。
//
// 这里只读文件、不连 MySQL（AGENTS.md §9：单测不得触达真实中间件），
// 但能在 CI 阶段拦住四类真实事故：
//  1. model 新增了表/列却忘记补迁移（上线即 Unknown table/column）；
//  2. 迁移列名与结构体 db tag 只改了一侧；
//  3. 唯一键缺失，而 repository 的幂等判定正依赖它
//     （AdminUser.Insert 靠 uniq_username + RowsAffected==0 判重名，
//      AdminTaskModel.Insert 靠 uniq_request_id 返回 ErrTaskExists）；
//  4. 迁移越界写了建库/建 schema_migrations 的语句（那是 scripts/migrate.ps1 的职责）。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const migrationsDir = "../../../deploy/migrations/operation"

var (
	// modelSQLTablePattern 抓 model 层 SQL 字符串里的表名（本包用裸表名，不加反引号）。
	modelSQLTablePattern = regexp.MustCompile(`(?:INSERT INTO|UPDATE|FROM|JOIN)\s+(op_[a-z_]+)`)
	// createTablePattern 抓建表语句：表名、括号内定义、以及 ENGINE 到语句结尾。
	createTablePattern = regexp.MustCompile("(?s)CREATE TABLE IF NOT EXISTS `(op_[a-z_]+)` \\((.*?)\n\\) (ENGINE=.*?;)\n")
	// columnDefPattern 抓列定义行（两空格缩进 + 反引号列名 + 类型）。
	columnDefPattern = regexp.MustCompile("(?m)^  `([a-z_]+)`\\s+\\S")
	// dbTagPattern 抓结构体的 db tag。
	dbTagPattern = regexp.MustCompile(`db:"([a-z_]+)"`)
	// statementLinePattern 抓去掉行注释后的可执行 SQL 行。
	commentLinePattern = regexp.MustCompile(`(?m)^\s*--.*$`)
	// fileVersionPattern 是迁移文件命名规范（deploy/migrations/README.md）。
	fileVersionPattern = regexp.MustCompile(`^\d{6}_[a-z0-9_]+$`)
)

// aliasColumns 是 JOIN/聚合别名：它们只是 SELECT 里的别名，不是任何表的物理列。
var aliasColumns = map[string]bool{
	"role_name":  true, // op_admin_role/op_role_permission JOIN op_role.name
	"role_title": true,
	"role_ctime": true,
	"role_mtime": true,
	"perm_ctime": true,
	"cnt":        true, // COUNT(*) AS cnt（model/task.go 的聚合别名）
}

// requiredKeys 是代码实际依赖的主键/唯一键（缺一个就可能重复建号、重复建任务）。
var requiredKeys = map[string][]string{
	"op_admin_user":      {"(`username`)"},
	"op_role":            {"(`name`)"},
	"op_permission":      {"(`resource`, `action`)"},
	"op_role_permission": {"(`role_id`, `permission_id`)"},
	"op_admin_role":      {"(`admin_id`, `role_id`)"},
	"op_config":          {"(`cfg_key`, `scope`)"},
	"op_admin_task":      {"(`request_id`)"},
	"op_admin_task_step": {"(`task_id`, `step_no`)"},
	"op_admin_session":   {"(`token`)"},
}

type ddlTable struct {
	name    string
	body    string // 括号内的列与索引定义
	tail    string // ) ENGINE=... COMMENT='...'
	file    string
	columns map[string]bool
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// modelGoFiles 返回本包的非测试源文件。
func modelGoFiles(t *testing.T) []string {
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

func modelTables(t *testing.T) map[string]bool {
	t.Helper()
	tables := map[string]bool{}
	for _, f := range modelGoFiles(t) {
		for _, m := range modelSQLTablePattern.FindAllStringSubmatch(readFile(t, f), -1) {
			tables[m[1]] = true
		}
	}
	if len(tables) == 0 {
		t.Fatal("no op_* table referenced by model: SQL pattern drifted")
	}
	return tables
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
	sort.Strings(files) // 版本号升序，便于定位
	return files
}

func readMigrations(t *testing.T) []ddlTable {
	t.Helper()
	var out []ddlTable
	seen := map[string]string{}
	for _, f := range migrationFiles(t) {
		for _, m := range createTablePattern.FindAllStringSubmatch(readFile(t, f), -1) {
			if prev, dup := seen[m[1]]; dup {
				t.Fatalf("table %s created twice (%s and %s): migrations are append-only, one DDL per table",
					m[1], prev, filepath.Base(f))
			}
			seen[m[1]] = filepath.Base(f)
			tt := ddlTable{name: m[1], body: m[2], tail: m[3], file: filepath.Base(f), columns: map[string]bool{}}
			for _, c := range columnDefPattern.FindAllStringSubmatch(m[2], -1) {
				tt.columns[c[1]] = true
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

// sqlOnly 去掉 `--` 行注释，只保留真正会被执行的语句，避免把说明文字当断言目标。
func sqlOnly(src string) string {
	return commentLinePattern.ReplaceAllString(src, "")
}

func TestMigrationCoversExactlyModelTables(t *testing.T) {
	fromModel := modelTables(t)
	tables := readMigrations(t)
	inDDL := map[string]ddlTable{}
	for _, tt := range tables {
		inDDL[tt.name] = tt
	}
	for name := range fromModel {
		if _, ok := inDDL[name]; !ok {
			t.Fatalf("model touches %s but %s has no DDL for it", name, migrationsDir)
		}
	}
	for name, tt := range inDDL {
		if !fromModel[name] {
			t.Fatalf("%s creates %s but no model SQL references it (dead table?)", tt.file, name)
		}
	}
}

func TestMigrationColumnsMatchModelTags(t *testing.T) {
	ddlCols := map[string]bool{}
	for _, tt := range readMigrations(t) {
		for c := range tt.columns {
			ddlCols[c] = true
		}
	}
	tagCols := map[string]bool{}
	for _, f := range modelGoFiles(t) {
		for _, m := range dbTagPattern.FindAllStringSubmatch(readFile(t, f), -1) {
			tagCols[m[1]] = true
		}
	}
	if len(tagCols) == 0 {
		t.Fatal("no db tag found: pattern drifted")
	}
	for col := range tagCols {
		if aliasColumns[col] {
			continue
		}
		if !ddlCols[col] {
			t.Fatalf("db tag %q has no matching column in %s DDL", col, migrationsDir)
		}
	}
	for col := range ddlCols {
		if !tagCols[col] && !aliasColumns[col] {
			t.Fatalf("DDL column %q is not mapped by any model struct tag", col)
		}
	}
}

func TestMigrationTablesAreSelfDescribing(t *testing.T) {
	for _, tt := range readMigrations(t) {
		if !strings.Contains(tt.tail, "ENGINE=InnoDB") {
			t.Fatalf("%s.%s: must be ENGINE=InnoDB (task/audit writes need transactions)", tt.file, tt.name)
		}
		if !strings.Contains(tt.tail, "DEFAULT CHARSET=utf8mb4") {
			t.Fatalf("%s.%s: must default to utf8mb4 (Chinese labels in this database)", tt.file, tt.name)
		}
		if !strings.Contains(tt.tail, "COMMENT='") {
			t.Fatalf("%s.%s: table-level COMMENT missing", tt.file, tt.name)
		}
		if strings.Count(tt.body, "COMMENT '") < len(tt.columns) {
			t.Fatalf("%s.%s: every column needs a COMMENT (%d columns)", tt.file, tt.name, len(tt.columns))
		}
	}
}

func TestMigrationIdempotencyKeysExist(t *testing.T) {
	tables := map[string]ddlTable{}
	for _, tt := range readMigrations(t) {
		tables[tt.name] = tt
	}
	for name, keys := range requiredKeys {
		tt, ok := tables[name]
		if !ok {
			t.Fatalf("required table %s absent from migrations", name)
		}
		hay := strings.Join(strings.Fields(tt.body), " ")
		for _, k := range keys {
			if !strings.Contains(hay, k) {
				t.Fatalf("%s: repository idempotency depends on key %s", name, k)
			}
		}
	}
}

func TestMigrationFilesCarryOwnerAndRollback(t *testing.T) {
	for _, f := range migrationFiles(t) {
		src := readFile(t, f)
		name := filepath.Base(f)

		if base := strings.TrimSuffix(name, ".sql"); !fileVersionPattern.MatchString(base) {
			t.Fatalf("%s: file name must be NNNNNN_verb_tables.sql", name)
		}
		if !strings.Contains(src, "owner") {
			t.Fatalf("%s: header must name the owner", name)
		}
		if !strings.Contains(src, "回滚") {
			t.Fatalf("%s: header must document the rollback", name)
		}
		// 建库与 schema_migrations 由 scripts/migrate.ps1 负责，DDL 只碰自己的表。
		if sql := strings.ToUpper(sqlOnly(src)); strings.Contains(sql, "CREATE DATABASE") ||
			strings.Contains(sql, "SCHEMA_MIGRATIONS") || strings.Contains(sql, "USE ") ||
			strings.Contains(sql, "GRANT ") || strings.Contains(sql, "DELETE FROM") {
			t.Fatalf("%s: contains statements outside the service's own append-only DDL", name)
		}
		for _, m := range createTablePattern.FindAllStringSubmatch(src, -1) {
			if !strings.Contains(src, "DROP TABLE IF EXISTS `"+m[1]+"`") {
				t.Fatalf("%s: rollback must list DROP TABLE IF EXISTS for %s", name, m[1])
			}
		}
	}
}
