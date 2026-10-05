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

// 本文件把「model 的 SQL 与 deploy/migrations/live-room 逐列一致」这条约定变成可执行门禁。
//
// 为什么要用测试而不是人工比对（AGENTS.md §9）：
//   - model 的结构体 db tag、SELECT 列常量、INSERT 列表是三处必须同步的文本，
//     任一处漂移都要到运行期才暴露成 "Unknown column" 或字段扫描错位；
//   - 幂等写入依赖的唯一键（uniq_dedup_key / uniq_room_mid_role / uniq_active_owner）
//     一旦被删或加错列，表现不是启动失败而是「重复副作用」这种最贵的故障；
//   - 迁移本轮**没有**在 MySQL 上执行过，结构正确性只能靠这种静态一致性证明。
//
// 测试不连数据库：只读 SQL 文本，与 model 的常量/结构体 tag 比对。

const migrationsRelDir = "deploy/migrations/live-room"

// tableSpec 声明一张 model 表与它的迁移文件的对应关系。
type tableSpec struct {
	table string // SQL 里的表名
	file  string // model/*.go 头注里引用的迁移文件名（一张表一个文件）
	// row 是行结构体样例；model 读写的列全集取自它的 db tag（含声明顺序）。
	row           any
	selectColumns string // model 里拼进 SELECT 的列常量原值
	// primaryKey 是 model 定位单行 / UPSERT 冲突所使用的列组合。
	primaryKey []string
	// uniqueKeys 是幂等与不变量依赖的唯一索引：键名 -> 列组合（顺序敏感）。
	uniqueKeys map[string][]string
	// requiredIndexes 是 model 的 WHERE / ORDER BY 依赖的普通索引名；
	// 列组合在 expectedIndexColumns 里单独声明，两者都要对上。
	requiredIndexes []string
	// sqlOnly 是「SQL 有、model 不读写」的列及其保留理由。留空即表示本表零冗余列。
	sqlOnly map[string]string
	// nullable 列出允许为 NULL 的列（唯一例外：房主占位列靠 NULL 逃逸唯一性）。
	nullable []string
	// requiredNoDefault 列出刻意不给 DEFAULT 的必填列：漏传必须在 MySQL 侧报错，
	// 而不是被默认值静默兜成 0/空串（见 TestNullabilityAndDefaultDiscipline 第 2 条）。
	requiredNoDefault []string
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:           "live_room",
			file:            "000001_create_live_room.sql",
			row:             LiveRoom{},
			selectColumns:   liveRoomColumns,
			primaryKey:      []string{"room_id"},
			requiredIndexes: []string{"idx_owner_room", "idx_area_room", "idx_state_room", "idx_state_ban", "idx_ctime"},
		},
		{
			table:         "live_room_setting",
			file:          "000002_create_live_room_setting.sql",
			row:           LiveRoomSetting{},
			selectColumns: liveRoomSettingColumns,
			// Upsert 的 ON DUPLICATE KEY UPDATE 以 room_id 主键为冲突键，主键即幂等键。
			primaryKey: []string{"room_id"},
		},
		{
			table:           "live_room_anchor",
			file:            "000003_create_live_room_anchor.sql",
			row:             LiveRoomAnchor{},
			selectColumns:   liveRoomAnchorColumns,
			primaryKey:      []string{"id"},
			uniqueKeys:      map[string][]string{"uniq_room_mid_role": {"room_id", "mid", "role"}, "uniq_active_owner": {"owner_room_id"}},
			requiredIndexes: []string{"idx_room_role_state", "idx_mid_state_room"},
			nullable:        []string{"owner_room_id"},
			// Bind 的 INSERT 总是显式给这三列；漏给必须报错，不能被 0 兜底成
			// 「房间 0 / 用户 0 / 角色未指定」这种既合法又毫无意义的绑定行。
			requiredNoDefault: []string{"room_id", "mid", "role"},
		},
		{
			table:           "live_session",
			file:            "000004_create_live_session.sql",
			row:             LiveSession{},
			selectColumns:   liveSessionColumns,
			primaryKey:      []string{"session_id"},
			requiredIndexes: []string{"idx_room_session", "idx_room_state_session", "idx_state_session"},
			// Insert 必填：房间与主播都不可能被默认成 0。
			requiredNoDefault: []string{"room_id", "mid"},
		},
		{
			table:           "live_room_ban",
			file:            "000005_create_live_room_ban.sql",
			row:             LiveRoomBan{},
			selectColumns:   liveRoomBanColumns,
			primaryKey:      []string{"ban_id"},
			requiredIndexes: []string{"idx_room_state_end", "idx_mid_state_end", "idx_state_end"},
			// Insert 必填：禁播类型与操作者是审计责任归属，mid 允许 0（下达时房间暂无生效房主）。
			requiredNoDefault: []string{"room_id", "ban_type", "operator_mid"},
		},
		{
			table:           "live_area",
			file:            "000006_create_live_area.sql",
			row:             LiveArea{},
			selectColumns:   liveAreaColumns,
			primaryKey:      []string{"area_id"},
			uniqueKeys:      map[string][]string{"uniq_area_name": {"area_name"}},
			requiredIndexes: []string{"idx_parent_sort"},
			// 分区名不能默认成空串：空名会撞 uniq_area_name（多个空名视为同名），
			// 且 AreaNameMaxRunes 校验在 model 侧，DB 侧再兜一层。
			requiredNoDefault: []string{"area_name"},
		},
		{
			table:           "live_room_state_log",
			file:            "000007_create_live_room_state_log.sql",
			row:             LiveRoomStateLog{},
			selectColumns:   liveRoomStateLogColumns,
			primaryKey:      []string{"log_id"},
			requiredIndexes: []string{"idx_room_type_log", "idx_ctime"},
			// 日志行的房间与状态类别不可默认；from/to 允许默认 0（建档类日志无前态）。
			requiredNoDefault: []string{"room_id", "state_type"},
		},
		{
			table:           "live_room_idempotency",
			file:            "000008_create_live_room_idempotency.sql",
			row:             LiveRoomIdempotency{},
			selectColumns:   liveRoomIdempotencyColumns,
			primaryKey:      []string{"id"},
			uniqueKeys:      map[string][]string{"uniq_dedup_key": {"dedup_key"}},
			requiredIndexes: []string{"idx_ctime"},
			// dedup_key 是去重锚点本身，kind 决定清理与归因口径，都必须显式给值；
			// result_json 是 TEXT —— MySQL 禁止 TEXT/BLOB 设 DEFAULT，
			// model 的 INSERT 总是显式写空串表示「已抢键、结果未回填」。
			requiredNoDefault: []string{"dedup_key", "kind", "result_json"},
		},
	}
}

// expectedIndexColumns 写的是「model 的 WHERE/ORDER BY 实际需要什么」，不是「SQL 里有什么」，
// 所以索引改名、丢列、换序都会在这里报错，而不是等到线上 filesort。
func expectedIndexColumns() map[string][]string {
	return map[string][]string{
		// live_room
		"idx_owner_room":         {"owner_mid", "room_id"},              // ListByOwner / ListRooms(owner) / CountByOwner
		"idx_area_room":          {"area_id", "room_id"},                // CountByArea / ListRooms(area)
		"idx_state_room":         {"state", "room_id"},                  // ListRooms(state) + room_id 倒序
		"idx_state_ban":          {"state", "ban_until"},                // ListBansToExpire
		"idx_ctime":              {"ctime"},                             // ROOM_ORDER_CTIME_DESC（二级索引隐含尾部 room_id）
		"idx_room_role_state":    {"room_id", "role", "state", "id"},    // FindOwner / List / Count
		"idx_mid_state_room":     {"mid", "state", "room_id"},           // CountActiveRoomsByMid / ListRoomsByMid
		"idx_room_session":       {"room_id", "session_id"},             // List（cursor 比较列）/ FindLatest
		"idx_room_state_session": {"room_id", "state", "session_id"},    // ListActiveByRoom / FindActiveByRoom
		"idx_state_session":      {"state", "session_id"},               // CountActive
		"idx_room_state_end":     {"room_id", "state", "end_at"},        // FindActiveByRoom
		"idx_mid_state_end":      {"mid", "state", "end_at"},            // HasActiveByMid（PrepareLive 热路径）
		"idx_state_end":          {"state", "end_at"},                   // ExpireDue / ListDue
		"idx_parent_sort":        {"parent_area_id", "sort", "area_id"}, // List 排序 + CountChildren
		"idx_room_type_log":      {"room_id", "state_type", "log_id"},   // ListByRoom
	}
}

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
	header     string
}

func (t *sqlTable) hasColumn(name string) bool {
	for _, c := range t.columns {
		if c.name == name {
			return true
		}
	}
	return false
}

// findMigrationsDir 从当前目录向上找仓库根的 deploy/migrations/live-room。
// 不写死相对层数：`go test ./services/live-room/model` 与在 model 目录内直接跑 cwd 不同。
func findMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "deploy", "migrations", "live-room")
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

// executableLines 把文件切成「注释头」与「可执行 SQL 行」两部分。
// 所有禁止项检查只看可执行行：头注释里正常会出现「TRUNCATE 不允许」「不建 FOREIGN KEY」
// 这类说明文字，若按全文匹配会把合规文件判成违规。
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
	var (
		header []string
		sql    []string
	)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
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

// parseCreateTables 从迁移文件里抽出 CREATE TABLE 块的结构信息。
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
		pf := parseFile(t, p)
		base := filepath.Base(p)
		var cur *sqlTable
		for _, trimmed := range pf.sql {
			switch {
			case cur == nil && strings.HasPrefix(trimmed, "CREATE TABLE"):
				name, ok := identifierIn(trimmed, "CREATE TABLE IF NOT EXISTS")
				if !ok {
					t.Fatalf("%s: CREATE TABLE 无法解析表名: %q", base, trimmed)
				}
				if !strings.HasSuffix(trimmed, "(") {
					t.Fatalf("%s: 表 %s 的 CREATE TABLE 未以 ( 结尾，风格不符无法解析", base, name)
				}
				cur = &sqlTable{table: name, file: base, indexes: map[string][]string{}, header: pf.header}
			case cur != nil && strings.HasPrefix(trimmed, ")"):
				cur.options = trimmed
				out[cur.table] = cur
				cur = nil
			case cur != nil:
				parseTableLine(t, cur, base, trimmed)
			default:
				t.Fatalf("%s: 出现 CREATE TABLE 之外的可执行语句 %q —— 本目录每个文件只建一张表", base, trimmed)
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
			t.Fatalf("%s: 列 %s 重复声明", file, name)
		}
		// 只在 COMMENT 之前的定义段上判定 NOT NULL / DEFAULT：
		// 中文注释里出现「NULL」「默认值」字样不能影响结构判定。
		defined := body
		beforeComment := body
		if i := strings.Index(body, " COMMENT '"); i >= 0 {
			beforeComment = body[:i]
		}
		cur.columns = append(cur.columns, sqlColumn{
			name:          name,
			dataType:      defined,
			notNull:       strings.Contains(beforeComment, " NOT NULL"),
			hasDefault:    strings.Contains(beforeComment, " DEFAULT "),
			autoIncrement: strings.Contains(beforeComment, " AUTO_INCREMENT"),
			hasComment:    strings.Contains(body, " COMMENT '"),
		})
	case strings.HasPrefix(body, "PRIMARY KEY"):
		cur.primary = columnListInParentheses(t, file, body)
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
	parts := strings.Split(line[open+1:closing], ",")
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

// --- 门禁用例 ---

// TestModelColumnsMatchMigrationColumns 逐列比对 model 结构体与迁移表定义，
// 并把两类差异分别报出来：
//   - model 用到而迁移没有 -> 运行期 Unknown column；
//   - 迁移有而 model 不读写 -> 必须在 sqlOnly 里登记理由，否则是无人维护的死列。
func TestModelColumnsMatchMigrationColumns(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl, ok := tables[spec.table]
		if !ok {
			t.Fatalf("迁移里没有表 %s（应在 %s）", spec.table, spec.file)
		}
		modelCols := dbTagColumns(t, spec.row)

		// SELECT 列常量必须与结构体 db tag 同集合：sqlx 按列名映射，
		// 漏写一列会让该字段永远读成零值（比报错更难查）。
		if normalizeCols(spec.selectColumns) != normalizeCols(strings.Join(modelCols, ",")) {
			t.Errorf("%s: model 的 SELECT 列常量与结构体 db tag 不一致\n SELECT=%s\n   tags=%s",
				spec.table, spec.selectColumns, strings.Join(modelCols, ", "))
		}

		sqlCols := make([]string, 0, len(tbl.columns))
		sqlSet := map[string]bool{}
		for _, c := range tbl.columns {
			sqlCols = append(sqlCols, c.name)
			sqlSet[c.name] = true
		}
		var missingInSQL, unusedByModel []string
		modelSet := map[string]bool{}
		for _, c := range modelCols {
			modelSet[c] = true
			if !sqlSet[c] {
				missingInSQL = append(missingInSQL, c)
			}
		}
		for _, c := range sqlCols {
			if !modelSet[c] {
				unusedByModel = append(unusedByModel, c)
			}
		}
		if len(missingInSQL) > 0 {
			t.Errorf("%s（%s）: model 用到但迁移缺这些列，运行期会 Unknown column: %v",
				spec.table, tbl.file, missingInSQL)
		}
		for _, c := range unusedByModel {
			reason, declared := spec.sqlOnly[c]
			if !declared || strings.TrimSpace(reason) == "" {
				t.Errorf("%s: 列 %s 在迁移里存在但 model 完全不读写，且未登记保留理由（要么删列，要么在 sqlOnly 写明用途）",
					spec.table, c)
			}
		}
	}
}

// TestMigrationColumnOrderMatchesModelStruct 锁列序：迁移列顺序 = 结构体字段顺序，
// 这样「逐列比对」肉眼可做，临时 SELECT * 排障也不会错位在 sqlx 映射之外。
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

// TestPrimaryAndUniqueKeysMatchModelDependencies 校验幂等写入与数据库级不变量依赖的键。
func TestPrimaryAndUniqueKeysMatchModelDependencies(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		if got, want := strings.Join(tbl.primary, ","), strings.Join(spec.primaryKey, ","); got != want {
			t.Errorf("%s: 主键是 (%s)，model 依赖 (%s)。live_room_setting 的 Upsert 就以主键为冲突键，改主键等于改幂等语义",
				spec.table, got, want)
		}
		for name, wantCols := range spec.uniqueKeys {
			got, ok := tbl.indexes[name]
			if !ok {
				t.Errorf("%s: 缺唯一索引 %s（列组合 %v）——它是幂等/不变量的最终防线，不能只靠 logic 预检",
					spec.table, name, wantCols)
				continue
			}
			if strings.Join(got, ",") != strings.Join(wantCols, ",") {
				t.Errorf("%s.%s: 列组合是 (%s)，期望 (%s)", spec.table, name, strings.Join(got, ","), strings.Join(wantCols, ","))
			}
		}
		// Claim 用 `ON DUPLICATE KEY UPDATE id = id` + RowsAffected==1 区分首次受理与重放。
		// 主键之外再多一个唯一键，会让「命中第二个键的无变化更新」也返回 0，
		// 首次受理被误判为重放：建房失败、事件被静默吞掉。
		if spec.table == "live_room_idempotency" && len(tbl.uniqueKeys) != 1 {
			t.Errorf("%s: 唯一键数量是 %d (%v)，必须恰好 1 个，否则 Claim 的 affected==1 判定失真",
				spec.table, len(tbl.uniqueKeys), tbl.uniqueKeys)
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
			// 唯一键承载的是不变量（房主唯一、去重键唯一），不是查询加速，
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
// 两条约定（在 model/doc.go 与本目录迁移头注里都有对应说明）：
//  1. 除 live_room_anchor.owner_room_id 占位列外，所有列一律 NOT NULL，
//     用哨兵值（0 / 空串）表达「无」。NULL 会让 GREATEST(ended_at - started_at, 0)
//     算出的时长变成 NULL，也会让 `stream_id = ”`、`result_json = ”` 这类
//     「只在未写过时回填」的条件永远不成立。
//  2. NOT NULL 列应当有 DEFAULT，**除非**它属于「必填列」：
//     自增主键、主键本身，或登记在 requiredNoDefault 里的业务主键/枚举列。
//     这些列刻意不给 DEFAULT —— model 的 INSERT 总是显式给值，
//     万一调用方漏给，MySQL 严格模式报错远好于静默写入 0 的脏行
//     （房间表 area_id 静默为 0 会让「未选分区」看起来像合法状态）。
//     反过来，可空的载荷列（标题、封面、原因）必须有 DEFAULT，否则局部更新写不动。
func TestNullabilityAndDefaultDiscipline(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		nullableOK := specSet(spec.nullable)
		requiredNoDefault := specSet(spec.requiredNoDefault)
		primary := specSet(tbl.primary)
		for _, c := range tbl.columns {
			if !c.notNull && !nullableOK[c.name] {
				t.Errorf("%s.%s: 允许 NULL。见本用例注释第 1 条；若确需可空请把列名加进 nullable 并写明理由", spec.table, c.name)
			}
			exempt := c.autoIncrement || primary[c.name] || requiredNoDefault[c.name]
			if c.notNull && !c.hasDefault && !exempt {
				t.Errorf("%s.%s: NOT NULL 且无 DEFAULT，又不属于自增主键/主键/requiredNoDefault 三类必填列。"+
					"实际定义 %q", spec.table, c.name, c.dataType)
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
		// 唯一索引的列必须 NOT NULL —— owner_room_id 是唯一例外，它靠 NULL 逃逸唯一性。
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

func (t *sqlTable) column(name string) *sqlColumn {
	for i := range t.columns {
		if t.columns[i].name == name {
			return &t.columns[i]
		}
	}
	return nil
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
		for _, want := range []string{"ENGINE=InnoDB", "DEFAULT CHARSET=utf8mb4", "COLLATE=utf8mb4_0900_ai_ci", "COMMENT="} {
			if !strings.Contains(tbl.options, want) {
				t.Errorf("%s: 表选项缺 %s，实际 %q", spec.table, want, tbl.options)
			}
		}
	}
}

// TestMigrationHeaderSections 校验文件头四要素：用途 / 数据所有者 / 回滚 / 锁风险。
func TestMigrationHeaderSections(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl, ok := tables[spec.table]
		if !ok {
			continue
		}
		for _, want := range []string{"用途", "数据所有者", "回滚", "锁风险"} {
			if !strings.Contains(tbl.header, want) {
				t.Errorf("%s: 文件头缺「%s」段（AGENTS.md §10 要求回滚步骤可查）", spec.file, want)
			}
		}
		if !strings.Contains(tbl.header, "live-room") {
			t.Errorf("%s: 文件头未写明数据所有者是 live-room", spec.file)
		}
	}
}

// TestMigrationsRepeatableAndForbidden 校验每个迁移文件只建表、可重复执行，
// 且可执行 SQL 里不出现被禁止的语句。只看非注释行：
// 头注释正常会提到「不建 FOREIGN KEY」「不 TRUNCATE」这类反向说明。
func TestMigrationsRepeatableAndForbidden(t *testing.T) {
	dir := findMigrationsDir(t)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	if len(paths) == 0 {
		t.Fatalf("%s 下没有迁移文件", migrationsRelDir)
	}
	// 以语句关键字开头即判违规（注释已剔除）。
	forbiddenPrefix := []string{"ALTER TABLE", "DROP TABLE", "DELETE FROM", "UPDATE ", "INSERT INTO",
		"CREATE DATABASE", "DROP DATABASE", "TRUNCATE", "USE ", "CREATE INDEX", "RENAME", "GRANT", "SET "}
	for _, p := range paths {
		pf := parseFile(t, p)
		base := filepath.Base(p)
		if len(pf.sql) == 0 {
			t.Errorf("%s: 没有可执行 SQL", base)
		}
		creates := 0
		for _, line := range pf.sql {
			upper := strings.ToUpper(line)
			if strings.HasPrefix(upper, "CREATE TABLE IF NOT EXISTS") {
				creates++
				continue
			}
			for _, bad := range forbiddenPrefix {
				if strings.HasPrefix(upper, bad) {
					t.Errorf("%s: 可执行语句以被禁止的 %q 开头: %q —— 迁移只建表；数据变更要另开 0000NN 文件并写明回滚",
						base, bad, line)
					break
				}
			}
		}
		if creates != 1 {
			t.Errorf("%s: 含 %d 段 CREATE TABLE IF NOT EXISTS，本目录约定一张表一个文件", base, creates)
		}
		// 凭据字面量禁止入库（AGENTS.md §4/§6）。
		all := strings.ToUpper(pf.header + strings.Join(pf.sql, "\n"))
		for _, bad := range []string{"ACCESSKEY", "SECRET_KEY", "PRIVATE KEY", "-----BEGIN"} {
			if strings.Contains(all, bad) {
				t.Errorf("%s: 出现疑似凭据字面量 %q", base, bad)
			}
		}
	}
}

// TestMigrationFileNamesAndOrder 校验文件编号连续升序，且与 model 头注引用的文件名逐字一致。
func TestMigrationFileNamesAndOrder(t *testing.T) {
	dir := findMigrationsDir(t)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	got := make([]string, 0, len(paths))
	for _, p := range paths {
		got = append(got, filepath.Base(p))
	}
	sort.Strings(got)
	want := make([]string, 0, len(tableSpecs()))
	for _, s := range tableSpecs() {
		want = append(want, s.file)
	}
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("迁移文件清单与 model 头注引用的不一致:\n 目录里=%v\n 期望=%v", got, want)
	}
	for i, f := range got {
		prefix := fmt.Sprintf("0000%02d_", i+1)
		if !strings.HasPrefix(f, prefix) {
			t.Errorf("第 %d 个文件 %s 的编号不是 %s：编号必须连续升序，回滚按它倒序执行", i+1, f, prefix)
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

func normalizeCols(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

func specSet(list []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range list {
		out[v] = true
	}
	return out
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
