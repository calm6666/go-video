package model

// 迁移脚本与 model 层的一致性守卫（对齐 services/operation/model/migration_sync_test.go 的做法）。
//
// 这里只读文件、不连 MySQL（AGENTS.md §9：单测不得触达真实中间件），
// 但能在 CI 阶段拦住本服务真实的五类事故：
//  1. model 新增表/列却忘记补迁移（上线即 Unknown table/column）；
//  2. 迁移列名与结构体 db tag 只改了一侧；
//  3. 唯一键缺失，而幂等判定正依赖它（ErrModelVersionExists / ErrFeatureConfigExists /
//     ErrExperimentExists / ErrDecisionExists 与 assignment 的 INSERT IGNORE 全部来自唯一键 1062）；
//  4. 迁移越界写了建库或 schema_migrations 语句（那是 scripts/migrate.ps1 的职责）；
//  5. 迁移头部缺少数据所有者/回滚/锁风险说明（deploy/migrations/README.md 的硬约定）。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const migrationsDir = "../../../deploy/migrations/recommend-rank"

var (
	// modelSQLTablePattern 抓 model 层 SQL 字符串里的表名（裸表名，无反引号）。
	// INSERT IGNORE 是 rank_experiment_assignment 的 sticky 写法，必须一并匹配。
	modelSQLTablePattern = regexp.MustCompile(`(?:INSERT(?:\s+IGNORE)?\s+INTO|UPDATE|FROM|JOIN)\s+(rank_[a-z_]+)`)
	// createTablePattern 抓建表语句：表名、括号内定义、以及 ENGINE 到语句结尾。
	createTablePattern = regexp.MustCompile("(?s)CREATE TABLE IF NOT EXISTS `(rank_[a-z_]+)` \\((.*?)\n\\) (ENGINE=.*?;)\n")
	// columnDefPattern 抓列定义行（两空格缩进 + 反引号列名 + 类型）。
	columnDefPattern = regexp.MustCompile("(?m)^  `([a-z_]+)`\\s+\\S")
	// dbTagPattern 抓结构体的 db tag。
	dbTagPattern = regexp.MustCompile(`db:"([a-z_]+)"`)
	// commentLinePattern 用于剥掉 `--` 行注释，避免把说明文字当断言目标。
	commentLinePattern = regexp.MustCompile(`(?m)^\s*--.*$`)
	// fileVersionPattern 是迁移文件命名规范（deploy/migrations/README.md）。
	fileVersionPattern = regexp.MustCompile(`^\d{6}_[a-z0-9_]+$`)
)

// aliasColumns 是 SELECT 里的聚合别名，不是任何表的物理列。
var aliasColumns = map[string]bool{
	"total": true, // COUNT(*) AS total（experimentassignment.go 的 CountByVariant）
}

// generatedColumns 只存在于 DDL：由数据库计算，任何 INSERT/UPDATE 都不得写入，
// 因此 model 侧没有对应 db tag（见 000001 头部说明）。
var generatedColumns = map[string]bool{
	"active_model_key": true,
}

// requiredKeys 是代码实际依赖的主键/唯一键：缺一个就丢掉一条幂等承诺。
var requiredKeys = map[string][]string{
	// UpsertModelVersion 重放；ActiveModel 的「每个 model_key 一个 ACTIVE」也靠它。
	"rank_model_version": {("(`model_key`, `version`)"), "(`active_model_key`)"},
	// UpsertFeatureConfig 重放。
	"rank_feature_config": {("(`config_version`)")},
	// UpsertExperiment 重放，并表达「变体 key 在实验内唯一」。
	"rank_experiment": {("(`exp_key`, `variant_key`)")},
	// GetExperimentAssignment 的 sticky 语义（INSERT IGNORE 冲突判定）。
	"rank_experiment_assignment": {("(`exp_key`, `subject_type`, `subject_id`, `hash_seed`)")},
	// RankCandidates 的 request_id 幂等回放 + GetRankDecision 的审计点查。
	"rank_decision_log": {("(`request_id`)"), "(`decision_id`)"},
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
		t.Fatal("no rank_* table referenced by model: SQL pattern drifted")
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

// sqlOnly 去掉 `--` 行注释，只保留真正会被执行的语句。
func sqlOnly(src string) string {
	return commentLinePattern.ReplaceAllString(src, "")
}

func TestMigrationCoversExactlyModelTables(t *testing.T) {
	fromModel := modelTables(t)
	inDDL := map[string]ddlTable{}
	for _, tt := range readMigrations(t) {
		inDDL[tt.name] = tt
	}
	if len(fromModel) != 5 || len(inDDL) != 5 {
		t.Fatalf("expect 5 tables on both sides, got model=%d ddl=%d (%v / %v)",
			len(fromModel), len(inDDL), keysOf(fromModel), ddlNames(inDDL))
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
		if generatedColumns[col] {
			continue
		}
		if !tagCols[col] && !aliasColumns[col] {
			t.Fatalf("DDL column %q is not mapped by any model struct tag", col)
		}
	}
}

func TestMigrationTablesAreSelfDescribing(t *testing.T) {
	for _, tt := range readMigrations(t) {
		if !strings.Contains(tt.tail, "ENGINE=InnoDB") {
			t.Fatalf("%s.%s: must be ENGINE=InnoDB（状态推进与激活需要真事务）", tt.file, tt.name)
		}
		if !strings.Contains(tt.tail, "DEFAULT CHARSET=utf8mb4") {
			t.Fatalf("%s.%s: must default to utf8mb4（中文注释与 note 列）", tt.file, tt.name)
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
				t.Fatalf("%s: 代码的幂等/唯一性判定依赖键 %s，DDL 里没有", name, k)
			}
		}
	}
}

func TestMigrationFilesCarryOwnerAndRollback(t *testing.T) {
	for _, f := range migrationFiles(t) {
		src := readFile(t, f)
		name := filepath.Base(f)

		if base := strings.TrimSuffix(name, ".sql"); !fileVersionPattern.MatchString(base) {
			t.Fatalf("%s: file name must be NNNNNN_verb_table.sql", name)
		}
		for _, keyword := range []string{"数据所有者", "回滚", "锁风险", "go_video_recommend_rank"} {
			if !strings.Contains(src, keyword) {
				t.Fatalf("%s: 头部必须写明 %s（deploy/migrations/README.md 的硬约定）", name, keyword)
			}
		}
		// 建库与 schema_migrations 由 scripts/migrate.ps1 负责，DDL 只碰自己的表。
		if sql := strings.ToUpper(sqlOnly(src)); strings.Contains(sql, "CREATE DATABASE") ||
			strings.Contains(sql, "SCHEMA_MIGRATIONS") || strings.Contains(sql, "USE ") ||
			strings.Contains(sql, "GRANT ") || strings.Contains(sql, "DELETE FROM") ||
			strings.Contains(sql, "DROP TABLE") || strings.Contains(sql, "ALTER TABLE") {
			t.Fatalf("%s: contains statements outside the service's own append-only DDL", name)
		}
		for _, m := range createTablePattern.FindAllStringSubmatch(src, -1) {
			if !strings.Contains(src, "DROP TABLE IF EXISTS `"+m[1]+"`") {
				t.Fatalf("%s: rollback must list DROP TABLE IF EXISTS for %s", name, m[1])
			}
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ddlNames(m map[string]ddlTable) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
