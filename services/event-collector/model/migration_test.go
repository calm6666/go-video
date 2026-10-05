package model

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 本测试把 model 结构体的 db 标签与 deploy/migrations/event-collector 的建表 SQL 逐列比对。
//
// 动机：本轮迁移 SQL 未在 MySQL 实例执行（127.0.0.1:3306 是维护者真实库，禁止连接），
// 因此「列名/列序是否对得上」只能靠自动化静态检查兜住，否则首次真跑就会报
// "Unknown column" 或扫描错位。SQL 改了忘改 model（或反之）会在这里直接失败。
//
// 约定：结构体字段必须与建表语句里的列**同名同序**，唯一例外是 ec_dispatch_policy.active_flag
// ——它只作为 UNIQUE KEY 的写入标记（ACTIVE=1，其余 NULL），不进入查询投影，
// 因为 NULL 扫进 int32 语义不清；读写一律用 state 判定。

// bt 是反引号常量：建表语句里的列名用反引号包裹，这里用转义写法避免和 Go 原始字符串冲突。
const bt = "\x60"

const migrationsDir = "../../../deploy/migrations/event-collector"

func migrationColumns(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.FromSlash(migrationsDir))
	if err != nil {
		t.Skipf("迁移目录不可用（在仓库外的环境跑测试）：%v", err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(filepath.FromSlash(migrationsDir), e.Name()))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", e.Name(), err)
		}
		for table, cols := range parseCreateTables(string(b)) {
			if prev, dup := out[table]; dup {
				t.Fatalf("表 %s 在多个迁移文件里重复建表，无法确定权威列清单（前 %d 列 / 后 %d 列）",
					table, len(prev), len(cols))
			}
			out[table] = cols
		}
	}
	if len(out) == 0 {
		t.Fatal("迁移 SQL 里没有解析出任何 CREATE TABLE，检查建表语句书写格式")
	}
	return out
}

// parseCreateTables 按行前缀解析建表语句，返回 表名 -> 列名（按 SQL 中的书写顺序）。
// 只认两种行：以 "CREATE TABLE IF NOT EXISTS " 开头、以两空格 + 反引号开头的列定义行；
// 索引/约束行（PRIMARY KEY、UNIQUE KEY、KEY ...）不以反引号开头，因此天然被排除。
func parseCreateTables(txt string) map[string][]string {
	out := map[string][]string{}
	table := ""
	for _, raw := range strings.Split(txt, "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "CREATE TABLE IF NOT EXISTS "):
			fields := strings.Fields(line)
			if len(fields) < 6 {
				continue
			}
			table = strings.Trim(fields[5], bt)
			out[table] = nil
		case table == "":
			continue
		case strings.HasPrefix(line, ")"):
			table = ""
		case strings.HasPrefix(line, "  "+bt):
			name := strings.TrimPrefix(line, "  ")
			name = strings.SplitN(name, " ", 2)[0]
			out[table] = append(out[table], strings.Trim(name, bt))
		}
	}
	return out
}

func structColumns(v any) []string {
	typ := reflect.TypeOf(v)
	var cols []string
	for i := 0; i < typ.NumField(); i++ {
		if tag := typ.Field(i).Tag.Get("db"); tag != "" {
			cols = append(cols, strings.Split(tag, ",")[0])
		}
	}
	return cols
}

// colExceptions 是有意不进 Go 投影的建表列（原因见文件头注释），结构体与投影两条检查共用。
var colExceptions = map[string][]string{
	"ec_dispatch_policy": {"active_flag"},
}

// dropExceptions 按建表顺序剔除例外列，得到 model 应该覆盖的列清单。
func dropExceptions(sql []string, exceptions []string) []string {
	if len(exceptions) == 0 {
		return sql
	}
	skip := map[string]bool{}
	for _, c := range exceptions {
		skip[c] = true
	}
	out := make([]string, 0, len(sql))
	for _, c := range sql {
		if !skip[c] {
			out = append(out, c)
		}
	}
	return out
}

func TestModelStructsMatchMigrationColumns(t *testing.T) {
	sqlCols := migrationColumns(t)
	cases := []struct {
		table string
		v     any
	}{
		{"ec_ingest_batch", IngestBatch{}},
		{"ec_event_record", EventRecord{}},
		{"ec_dispatch_policy", DispatchPolicy{}},
		{"ec_pending_delivery", PendingDelivery{}},
		{"ec_dead_letter", DeadLetter{}},
	}
	for _, tc := range cases {
		sql, ok := sqlCols[tc.table]
		if !ok {
			t.Errorf("迁移 SQL 里没有建 %s 表", tc.table)
			continue
		}
		got := structColumns(tc.v)
		want := dropExceptions(sql, colExceptions[tc.table])
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s 列不一致：\n model=%s\n sql  =%s", tc.table, strings.Join(got, ","), strings.Join(want, ","))
		}
		// 反向检查：结构体不得引用 SQL 里不存在的列（新增列忘写迁移也会在这里失败）。
		inSQL := map[string]bool{}
		for _, c := range sql {
			inSQL[c] = true
		}
		for _, c := range got {
			if !inSQL[c] {
				t.Errorf("%s：model 列 %s 在建表语句中不存在", tc.table, c)
			}
		}
	}
}

func TestProjectionListsMatchMigrationColumns(t *testing.T) {
	sqlCols := migrationColumns(t)
	projections := map[string]string{
		"ec_ingest_batch":     ingestBatchColumns,
		"ec_event_record":     eventRecordColumns,
		"ec_dispatch_policy":  dispatchPolicyColumns,
		"ec_pending_delivery": pendingDeliveryColumns,
		"ec_dead_letter":      deadLetterColumns,
	}
	for table, proj := range projections {
		cols := splitIdentList(proj)
		want := dropExceptions(sqlCols[table], colExceptions[table])
		if len(cols) != len(want) {
			t.Errorf("%s 投影列数 %d 与建表列数 %d 不符：\n proj=%s\n sql =%s", table, len(cols), len(want), cols, want)
			continue
		}
		if strings.Join(cols, ",") != strings.Join(want, ",") {
			t.Errorf("%s 查询投影与建表列顺序不一致：\n proj=%s\n sql =%s", table, cols, want)
		}
	}
	// 入队列清单同样必须落在建表列里（不含自增主键）。
	for table, cols := range map[string][]string{
		"ec_pending_delivery": splitIdentList(pendingDeliveryInsertColumns),
	} {
		for _, c := range cols {
			if !contains(sqlCols[table], c) {
				t.Errorf("%s：INSERT 列 %s 不存在", table, c)
			}
		}
	}
}

// TestPlaceholderCountsMatchColumnLists 防止「加了列忘了加 ?」这类只在运行期炸的错。
func TestPlaceholderCountsMatchColumnLists(t *testing.T) {
	cases := []struct {
		name string
		n    int
		cols []string
	}{
		{"ec_ingest_batch Insert", ingestBatchInsertValues, splitIdentList(ingestBatchColumns)[1:]},
		{"ec_dead_letter InsertIgnore", deadLetterInsertValues, splitIdentList(deadLetterColumns)[1:]},
		{"ec_event_record InsertIgnoreMany", eventRecordPlaceholdersPerRow, splitIdentList(eventRecordColumns)[1:]},
		// 策略表多写一个不进投影的 active_flag（见文件头说明）。
		{"ec_dispatch_policy InsertDraft", dispatchPolicyInsertValues,
			append(splitIdentList(dispatchPolicyColumns)[1:], "active_flag")},
		{"ec_pending_delivery InsertIgnoreMany", pendingDeliveryInsertValues,
			splitIdentList(pendingDeliveryInsertColumns)},
	}
	for _, tc := range cases {
		if len(tc.cols) != tc.n {
			t.Errorf("%s：占位符 %d 个，实际列 %d 个（%s）", tc.name, tc.n, len(tc.cols), strings.Join(tc.cols, ","))
		}
		if got := strings.Count(placeholders(tc.n), "?"); got != tc.n {
			t.Errorf("%s：placeholders(%d) 生成 %d 个占位符", tc.name, tc.n, got)
		}
	}
	// 批量上限常量与 placeholders 的配合关系也不许漂移。
	if maxIDList <= 0 || maxStringIDList <= 0 {
		t.Fatal("批量上限常量必须为正")
	}
}

func splitIdentList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
