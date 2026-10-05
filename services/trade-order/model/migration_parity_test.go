package model

// 本文件把「trade-order 的 model 结构体 / 列常量 / INSERT 语句 / logic 校验上限」与
// deploy/migrations/trade-order/000001_create_trade_order_tables.sql 的建表语句钉成
// 可执行门禁（AGENTS.md §9），不连数据库：只做文本解析 + 结构体反射比对。
//
// 为什么订单域需要这套漂移探测（本轮迁移未在 MySQL 上实跑）：
//   - 结构体 db tag、orderColumns/SELECT 常量、INSERT 列表与实参是四处必须同步的文本。
//     任一处漂移的运行期表现是 "Unknown column" 或字段扫描错位 —— 错位的对象是订单金额与
//     履约结论，比崩溃更难发现；
//   - uniq_order_no / uniq_request_id 是「一次建单最多一张订单」的最后防线，被删或被换成
//     普通 KEY 时不会启动失败，只会重复落单；
//   - 参与唯一性判定的字符列必须 COLLATE utf8mb4_bin：*_ci 会把大小写不同的 request_id
//     判成同一个键，重放被误吞或该重放的被判占用；整数列（mid / state / biz_type）没有
//     排序规则可言，把它们纳入登记只会把门禁变成噪音（payment 侧踩过，本文件反向钉住）；
//   - 订单状态机有 11 个取值，靠建表注释向运维和 DBA 解释「这一行为什么是 9」；新增状态值
//     而不改注释，等于线上排查时少一个口径；
//   - 金额必须是 BIGINT 且注释写明「分」：一旦出现 DECIMAL/DOUBLE，int64 的分单位语义失真；
//   - 退款只走沙箱台账、不退到银行卡（AGENTS.md §1），这一点必须写在承载退款的列注释上，
//     否则读到 refunded_minor 的人会以为存在「原路退回渠道」这条路径。
//
// 迁移文件按 AGENTS.md §10 只读：本测试是漂移探测器，不是改库入口。

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go-video/common/idgen"
)

const (
	migrationsRelDir = "deploy/migrations/trade-order"
	// logicHelpersRelPath 是 logic 侧列宽校验常量的出处：本测试直接读它，
	// 而不是把 logic 的数字抄进测试里（抄一次就等于留一份会过期的副本）。
	logicHelpersRelPath = "services/trade-order/internal/logic/helpers.go"
	// orderNoPrefixWant 与 logic.orderNoPrefix 同值，另由 TestOrderNoWidthArithmetic 从源码核对。
	orderNoPrefixWant = "to"
	ulidWidth         = 26 // common/idgen.ULID() 的固定长度
)

// tableSpec 声明一张 model 表与它的迁移建表语句的对应关系。
type tableSpec struct {
	table         string
	row           any
	selectColumns string
	insertConst   string // model 里的 INSERT 语句常量名
	primaryKey    []string
	// uniqueKeys 是幂等与不变量依赖的唯一索引：键名 -> 列组合（顺序敏感）。
	uniqueKeys map[string][]string
	// requiredIndexes 是 model 的 WHERE / ORDER BY 依赖的普通索引。
	requiredIndexes map[string][]string
	// binaryKeys 是精确匹配 / 参与唯一性判定的字符列：必须 utf8mb4_bin。
	// 只允许登记字符列，整数列登记进来会造成假失败（见 TestUniqueKeyColumnsUseBinaryCollation）。
	binaryKeys []string
	// textLimits 是每列的声明上限；断言方向：列宽 >= 上限。
	textLimits map[string]int
	// enumComments 是「注释里逐值列举」的枚举列：model 常量集合与注释数字集合必须完全相等。
	enumComments map[string][]int32
	// timeColumns 是 Unix 秒 BIGINT 时间列。
	timeColumns []string
	// appendOnly 为真表示只追加台账：不得有 mtime/version，也不得有唯一索引
	//（同一次请求会写多行，见 to_order_event.request_id 列注释）。
	appendOnly bool
	// casColumn 非空表示该表靠乐观锁推进，必须有这一列且必须是 BIGINT。
	casColumn string
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:         "to_order",
			row:           Order{},
			selectColumns: orderColumns,
			insertConst:   "insertOrderSQL",
			primaryKey:    []string{"id"},
			// 单号对外唯一 + 建单幂等（客户端重试只落一张单）。
			uniqueKeys: map[string][]string{
				"uniq_order_no":   {"order_no"},
				"uniq_request_id": {"request_id"},
			},
			requiredIndexes: map[string][]string{
				// 终端主读路径：我的订单按状态分页。
				"idx_mid_state_created": {"mid", "state", "created_at"},
				// cron 卡单扫描：ListStuck 用 state IN (...) AND updated_at < ?。
				"idx_state_updated": {"state", "updated_at"},
				// 运营面跨用户窗口扫描。
				"idx_state_created": {"state", "created_at"},
				// 按支付单号反查订单（payment_no 上刻意只是普通索引，见迁移头注释第 3 条）。
				"idx_payment_no": {"payment_no"},
			},
			binaryKeys: []string{"order_no", "request_id", "payment_no", "grant_ref", "plan_code", "client_trace_id", "currency"},
			textLimits: map[string]int{
				"order_no": 64, "request_id": 64, "plan_code": 64, "title": 200,
				"fulfill_detail": 500, "payment_no": 64, "grant_ref": 64,
				"client_trace_id": 64, "currency": 8,
			},
			enumComments: map[string][]int32{
				"state": {StateCreated, StatePaying, StatePaid, StateFulfilling, StateFulfilled,
					StateCancelled, StateFailed, StateRefundRequested, StateRefundApproved,
					StateRefunded, StateRefundRejected},
				"fulfill_state": {FulfillPending, FulfillDone, FulfillFailed},
				"biz_type":      {BizMembership, BizCoinPack},
				"pay_method":    {PayBalance, PaySandbox},
				"platform":      {PlatformAndroid, PlatformIOS, PlatformHarmony, PlatformDesktop, PlatformWeb},
			},
			timeColumns: []string{"created_at", "updated_at", "expire_at", "paid_at", "fulfilled_at", "closed_at"},
			casColumn:   "version",
		},
		{
			table:         "to_order_event",
			row:           OrderEvent{},
			selectColumns: orderEventColumns,
			insertConst:   "insertOrderEventSQL",
			primaryKey:    []string{"event_id"},
			// 台账没有唯一键：一次请求可以推进多行（同态重试也要留证据）。
			uniqueKeys: nil,
			requiredIndexes: map[string][]string{
				// ListByOrderNo：WHERE order_no ORDER BY ctime, event_id。
				"idx_order_ctime": {"order_no", "ctime"},
				// FindByRequestAndState：幂等重放判定按 (order_no, request_id) 收敛。
				"idx_order_request": {"order_no", "request_id"},
			},
			binaryKeys: []string{"order_no", "request_id"},
			textLimits: map[string]int{
				"order_no": 64, "operator": 64, "reason": 500, "request_id": 64,
			},
			// from_state/to_state 不逐值列举：取值域就是 to_order.state，
			// 由 TestLedgerStateColumnsReferenceOrderStateEnum 钉住交叉引用，
			// 免得两处注释各写一份 11 个值然后互相漂移。
			enumComments: nil,
			timeColumns:  []string{"ctime"},
			appendOnly:   true,
		},
	}
}

// --- SQL 解析 ---

type sqlColumn struct {
	name          string
	dataType      string // 列定义原文（COMMENT 之前），用于类型/排序规则/默认值判定
	comment       string
	defaultVal    string // DEFAULT 字面量原文（含引号），无默认值时为空
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
	comment string // 表级 COMMENT（数据所有者口径写在这里）
	columns []sqlColumn
	primary []string
	indexes []sqlIndex
	checks  map[string]string // 约束名 -> CHECK 表达式原文
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
	dir := filepath.Join(findRepoRoot(t), "deploy", "migrations", "trade-order")
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
// 只认本仓库既有书写风格（反引号标识符、每列一行、) ENGINE=... 收尾，
// 表级 COMMENT 可以单独占一行）：格式被改坏时报错，而不是悄悄解析出一张空表。
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
		pending := "" // 上一张已收尾、表级 COMMENT 还没落下来的表名
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case trimmed == "" || strings.HasPrefix(trimmed, "--"):
			case cur == nil && pending != "" && strings.HasPrefix(trimmed, "COMMENT="):
				// 本服务把表级 COMMENT 单独写在收尾行的下一行。
				out[pending].comment = strings.TrimSuffix(
					strings.TrimPrefix(trimmed, "COMMENT='"), "';")
				pending = ""
			case cur == nil && strings.HasPrefix(trimmed, "CREATE TABLE"):
				name, ok := backtickedIdentifier(trimmed)
				if !ok {
					t.Fatalf("%s: CREATE TABLE 无法解析表名: %q", base, trimmed)
				}
				if !strings.Contains(trimmed, "IF NOT EXISTS") {
					t.Fatalf("%s: 表 %s 必须用 CREATE TABLE IF NOT EXISTS，迁移要可重复执行", base, name)
				}
				pending = ""
				cur = &sqlTable{table: name, file: base, checks: map[string]string{}}
			case cur != nil && strings.HasPrefix(trimmed, ")"):
				if !strings.Contains(trimmed, "ENGINE=InnoDB") || !strings.Contains(trimmed, "utf8mb4") {
					t.Fatalf("%s: 表 %s 收尾行必须显式 ENGINE=InnoDB 与 CHARSET=utf8mb4，得到 %q",
						base, cur.table, trimmed)
				}
				if _, dup := out[cur.table]; dup {
					t.Fatalf("%s: 表 %s 被重复创建", base, cur.table)
				}
				out[cur.table] = cur
				pending = cur.table
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
		if pending != "" {
			t.Fatalf("%s: 表 %s 缺少表级 COMMENT —— 数据所有者与「谁不得回写」必须写在表注释上",
				base, pending)
		}
	}
	return out
}

// parseTableLine 解析表体里的一行：列 / 主键 / 唯一键 / 普通索引 / CHECK 约束。
func parseTableLine(t *testing.T, cur *sqlTable, file, line string) {
	t.Helper()
	body := strings.TrimSuffix(strings.TrimSpace(line), ",")
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
		// 中文注释里出现「默认」「不为负」字样不能影响结构判定。
		defined, comment := body, ""
		if i := strings.Index(body, " COMMENT '"); i >= 0 {
			defined = body[:i]
			comment = strings.TrimSuffix(body[i+len(" COMMENT '"):], "'")
		}
		if comment == "" {
			t.Fatalf("%s: 表 %s 的列 %s 缺少 COMMENT —— 订单列的取值口径全靠注释表达", file, cur.table, name)
		}
		hasDefault := strings.Contains(defined, " DEFAULT ")
		var defaultVal string
		if hasDefault {
			defaultVal = strings.TrimSpace(defined[strings.LastIndex(defined, " DEFAULT ")+len(" DEFAULT "):])
		}
		cur.columns = append(cur.columns, sqlColumn{
			name:          name,
			dataType:      defined,
			comment:       comment,
			defaultVal:    defaultVal,
			notNull:       strings.Contains(defined, " NOT NULL"),
			hasDefault:    hasDefault,
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
	case strings.HasPrefix(body, "CONSTRAINT"):
		name, ok := backtickedIdentifier(body)
		if !ok {
			t.Fatalf("%s: CHECK 约束名无法解析: %q", file, body)
		}
		if !strings.Contains(strings.ToUpper(body), "CHECK") {
			t.Fatalf("%s: 约束 %s 不是 CHECK —— 本服务不用外键，跨表一致性在服务侧保证", file, name)
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

var (
	varcharLenRe   = regexp.MustCompile(`VARCHAR\((\d+)\)`)
	numericTokenRe = regexp.MustCompile(`\d+`)
	sqlTypeRe      = regexp.MustCompile(`^(BIGINT|TINYINT|SMALLINT|MEDIUMINT|INTEGER|INT|VARCHAR|CHAR|TEXT|BLOB|DATETIME|TIMESTAMP|DATE|DECIMAL|NUMERIC|FLOAT|DOUBLE|BIT|ENUM|SET)(\s*\([\d,]+\))?(\s+UNSIGNED)?`)
	goFieldRe      = regexp.MustCompile(`\bo\.([A-Z]\w*)`)
	goEventFieldRe = regexp.MustCompile(`\be\.([A-Z]\w*)`)
	quotedRe       = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	logicLimitRe   = regexp.MustCompile(`(?m)^\s*(max[A-Za-z0-9_]*Len)\s*=\s*(\d+)`)
	logicPrefixRe  = regexp.MustCompile(`(?m)^\s*orderNoPrefix\s*=\s*"([^"]*)"`)
)

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

// baseType 取列定义里的类型词（去掉列名、CHARACTER SET 与 COMMENT 段落的干扰）。
func (c sqlColumn) baseType() string {
	rest := c.dataType
	if i := strings.Index(rest[1:], "`"); i >= 0 { // 跳过 `列名`
		rest = rest[i+2:]
	}
	up := strings.ToUpper(strings.TrimSpace(rest))
	m := sqlTypeRe.FindStringSubmatch(up)
	if m == nil {
		return up
	}
	out := m[1]
	if m[3] != "" {
		out += " UNSIGNED"
	}
	return out
}

// hasCollation 只有字符列（CHAR/VARCHAR/TEXT/ENUM/SET）才有排序规则这一说，
// 整数列（mid、state、biz_type）即便参与唯一索引也不能要求登记 utf8mb4_bin。
func hasCollation(dataType string) bool {
	up := strings.ToUpper(dataType)
	for _, kw := range []string{"CHAR", "TEXT", "ENUM", "SET"} {
		if strings.Contains(up, kw) {
			return true
		}
	}
	return false
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
			t.Fatalf("%s: 索引列必须是反引号标识符（带前缀长度的写法无法比对，请改写）: %q", file, p)
		}
		out = append(out, name)
	}
	return out
}

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

// structColumnTypes 返回 db tag -> Go 类型，供类型相容性断言使用。
func structColumnTypes(t *testing.T, row any) map[string]reflect.Type {
	t.Helper()
	v := reflect.TypeOf(row)
	out := map[string]reflect.Type{}
	for i := 0; i < v.NumField(); i++ {
		out[v.Field(i).Tag.Get("db")] = v.Field(i).Type
	}
	return out
}

// readPackageFile 读本包源文件（go test 的工作目录就是包目录）。
func readPackageFile(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	return readFileOrFatal(t, filepath.Join(wd, name))
}

// extractConstString 取出 `const NAME = "..." + "..."` 拼接后的字面量内容。
func extractConstString(t *testing.T, src, name string) string {
	t.Helper()
	idx := strings.Index(src, "const "+name+" = ")
	if idx < 0 {
		t.Fatalf("model 源码里找不到常量 %s —— 常量改名后本门禁失去意义，请同步测试", name)
	}
	var sb strings.Builder
	for _, line := range strings.Split(src[idx:], "\n") {
		for _, m := range quotedRe.FindAllStringSubmatch(line, -1) {
			sb.WriteString(m[1])
		}
		if !strings.HasSuffix(strings.TrimSpace(line), "+") {
			break
		}
	}
	if sb.Len() == 0 {
		t.Fatalf("常量 %s 解析出空字符串", name)
	}
	return sb.String()
}

// extractExecArgs 取 ExecCtx 实参里的字段名（顺序敏感）。
// marker 形如 "ExecCtx(ctx, insertOrderSQL,"，重解析时按括号深度找到调用收尾。
func extractExecArgs(t *testing.T, src, marker string, fieldRe *regexp.Regexp) []string {
	t.Helper()
	idx := strings.Index(src, marker)
	if idx < 0 {
		t.Fatalf("model 源码里找不到 %q —— 写入路径改名后 INSERT 对齐检查会失效", marker)
	}
	depth := strings.Count(marker, "(") - strings.Count(marker, ")")
	rest := src[idx+len(marker):]
	var body strings.Builder
	for _, r := range rest {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return fieldNames(fieldRe.FindAllStringSubmatch(body.String(), -1))
			}
		}
		body.WriteRune(r)
	}
	t.Fatalf("%q 的调用没有收尾括号", marker)
	return nil
}

// fieldNames 把 FindAllStringSubmatch 的结果压成字段名列表。
func fieldNames(matches [][]string) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// --- 断言 ---

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
// 结构体多了列 => 查询报 Unknown column；SQL 多了列 => 该列永不读写（等于隐藏的第二份事实）。
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
					t.Errorf("SQL 列 %s 没有结构体字段承载 —— 该列永远不会被读写，请删除或补 model", c.name)
				}
			}
			if len(fields) != len(tbl.columns) {
				t.Errorf("列数不符：结构体 %d 列，SQL %d 列", len(fields), len(tbl.columns))
			}
			for _, c := range tbl.columns {
				if !c.notNull {
					t.Errorf("%s.%s 允许 NULL —— model 用普通结构体扫描（无 sql.Null*），读到 NULL 会直接失败",
						spec.table, c.name)
				}
			}
		})
	}
}

// TestModelFieldTypesMatchSQLColumns 类型相容：int64↔BIGINT、int32↔INT/TINYINT、
// string↔CHAR/VARCHAR、uint32↔INT UNSIGNED。方向反了（Go int64 对 SQL INT）在 2038 年
// 之后或数据增长之后会静默溢出，所以两边都必须精确落进白名单。
func TestModelFieldTypesMatchSQLColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			types := structColumnTypes(t, spec.row)
			for _, c := range tbl.columns {
				gt, ok := types[c.name]
				if !ok {
					continue // 缺字段由 TestModelColumnsMatchMigration 报
				}
				checked++
				base := c.baseType()
				switch gt.Kind() {
				case reflect.Int64:
					if base != "BIGINT" {
						t.Errorf("%s.%s Go 侧 int64 而 SQL 是 %s —— 必须在 model 或 SQL 一侧改成 BIGINT",
							spec.table, c.name, base)
					}
				case reflect.Int32:
					if base != "INT" && base != "TINYINT" && base != "SMALLINT" && base != "MEDIUMINT" {
						t.Errorf("%s.%s Go 侧 int32 而 SQL 是 %s —— 本服务整数列只用 BIGINT/INT/TINYINT",
							spec.table, c.name, base)
					}
				case reflect.Uint32:
					if base != "INT UNSIGNED" {
						t.Errorf("%s.%s Go 侧 uint32 而 SQL 是 %s —— 无符号位必须两侧同时声明",
							spec.table, c.name, base)
					}
				case reflect.String:
					if base != "VARCHAR" && base != "CHAR" && base != "TEXT" {
						t.Errorf("%s.%s Go 侧 string 而 SQL 是 %s", spec.table, c.name, base)
					}
				default:
					t.Errorf("%s.%s Go 类型 %s 不在 model 允许的标量集合里", spec.table, c.name, gt)
				}
			}
		})
	}
	if checked < 39 {
		t.Fatalf("只比对了 %d 个列的类型，说明列清单被裁短（两张表合计 39 列）", checked)
	}
}

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
					t.Errorf("结构体列 %s 没进 SELECT 常量 —— FindByOrderNo 读不到该列", c)
				}
			}
			// 扫描顺序 = 列常量顺序 = 结构体字段顺序：错位会把 mid 读成 plan_id。
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

// TestInsertColumnsPlaceholdersAndArgsAgree 是资金/订单表最贵的一类漂移：
// INSERT 列清单、占位符个数与实参顺序三者必须逐项一致，否则 title 会被写进 currency，
// 编译期与启动期都不报错，只有事后对账才发现。
func TestInsertColumnsPlaceholdersAndArgsAgree(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	src := readPackageFile(t, "to_order.go") + "\n" + readPackageFile(t, "to_order_event.go")
	cases := []struct {
		spec    tableSpec
		marker  string
		fieldRe *regexp.Regexp
	}{
		{tableSpecs()[0], "ExecCtx(ctx, insertOrderSQL,", goFieldRe},
		{tableSpecs()[1], "ExecCtx(ctx, insertOrderEventSQL,", goEventFieldRe},
	}
	for _, c := range cases {
		t.Run(c.spec.table, func(t *testing.T) {
			stmt := extractConstString(t, src, c.spec.insertConst)
			cols, placeholderCount := parseInsertTarget(t, stmt, c.spec.table)
			tbl := mustTable(t, tables, c.spec.table)

			auto := 0
			for _, sc := range tbl.columns {
				if sc.autoIncrement {
					auto++
				}
			}
			if want := len(tbl.columns) - auto; len(cols) != want {
				t.Errorf("%s 的 %s 插入 %d 列，建表语句里非自增列有 %d 个 —— 漏写的那列会走 DEFAULT",
					c.spec.table, c.spec.insertConst, len(cols), want)
			}
			if placeholderCount != len(cols) {
				t.Errorf("%s 的 %s 列数 %d 与占位符数 %d 不符",
					c.spec.table, c.spec.insertConst, len(cols), placeholderCount)
			}
			for _, name := range cols {
				if !tbl.hasColumn(name) {
					t.Errorf("%s 的 %s 引用了建表语句里没有的列 %s", c.spec.table, c.spec.insertConst, name)
				}
			}
			// 实参顺序 == 列清单顺序（结构体字段与 db tag 一一对应，由列比对门禁保证）。
			args := extractExecArgs(t, src, c.marker, c.fieldRe)
			if len(args) != len(cols) {
				t.Fatalf("%s 的实参 %d 个与插入列 %d 个不符", c.spec.insertConst, len(args), len(cols))
			}
			tagByField := map[string]string{}
			v := reflect.TypeOf(c.spec.row)
			for i := 0; i < v.NumField(); i++ {
				tagByField[v.Field(i).Name] = v.Field(i).Tag.Get("db")
			}
			for i, fld := range args {
				tag, ok := tagByField[fld]
				if !ok {
					t.Fatalf("%s 的实参 %s 在结构体 %T 里没有对应字段", c.spec.insertConst, fld, c.spec.row)
				}
				if tag != cols[i] {
					t.Fatalf("%s 第 %d 个实参写的是 %s（列 %s），而 INSERT 列表第 %d 列是 %s —— 参数错位",
						c.spec.insertConst, i+1, fld, tag, i+1, cols[i])
				}
			}
		})
	}
}

var insertTargetRe = regexp.MustCompile(`(?is)INSERT INTO (\w+)\s*\(([^)]*)\)\s*VALUES\s*\(([^)]*)\)`)

// parseInsertTarget 取 INSERT 的目标表、列清单与占位符个数。
func parseInsertTarget(t *testing.T, stmt, wantTable string) (cols []string, placeholders int) {
	t.Helper()
	m := insertTargetRe.FindStringSubmatch(stmt)
	if m == nil {
		t.Fatalf("无法从语句里解析出 INSERT INTO ... (列) VALUES (...)：%q", stmt)
	}
	if m[1] != wantTable {
		t.Fatalf("INSERT 目标表是 %s，期望 %s", m[1], wantTable)
	}
	for _, p := range strings.Split(m[2], ",") {
		name := strings.TrimSpace(p)
		if name == "" {
			t.Fatalf("INSERT 列清单里有空列名: %q", m[2])
		}
		cols = append(cols, name)
	}
	return cols, strings.Count(m[3], "?")
}

func TestIdempotencyKeysAreUniqueInSQL(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl, ok := tables[spec.table]
			if !ok {
				t.Fatalf("迁移里没有 %s", spec.table)
			}
			if strings.Join(tbl.primary, ",") != strings.Join(spec.primaryKey, ",") {
				t.Errorf("主键不符：SQL %v，期望 %v", tbl.primary, spec.primaryKey)
			}
			for name, cols := range spec.uniqueKeys {
				idx, ok := tbl.index(name)
				if !ok {
					t.Fatalf("%s 缺少唯一索引 %s —— 建单幂等失去数据库兜底，同一请求会重复下单", spec.table, name)
				}
				if !idx.unique {
					t.Fatalf("%s 的 %s 必须是 UNIQUE KEY，普通 KEY 起不到去重作用", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("唯一索引 %s 列不符：SQL %v，期望 %v（顺序敏感）", name, idx.cols, cols)
				}
			}
			switch {
			case spec.appendOnly:
				// 台账一次请求可写多行（同态重试也要留证据）：出现唯一索引就是设计被改动了。
				for _, idx := range tbl.indexes {
					if idx.unique {
						t.Errorf("%s 出现唯一索引 %s —— 只追加台账不允许按列组合去重，见迁移里 request_id 列注释",
							spec.table, idx.name)
					}
				}
			case len(spec.uniqueKeys) == 0:
				t.Errorf("%s 没有唯一键 —— 写接口无法靠数据库去重", spec.table)
			}
		})
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
				if idx.unique {
					t.Errorf("%s 的 %s 不该是唯一索引：普通查询路径占了唯一约束会拒掉合法行", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("索引 %s 列不符：SQL %v，期望 %v", name, idx.cols, cols)
				}
			}
			// 反向：SQL 里出现的索引都要在 spec 里说明用途，避免「凭印象加索引」。
			for _, idx := range tbl.indexes {
				if _, ok := spec.requiredIndexes[idx.name]; ok {
					continue
				}
				if _, ok := spec.uniqueKeys[idx.name]; ok {
					continue
				}
				t.Errorf("%s 出现了 model 未登记的索引 %s（列 %v）", spec.table, idx.name, idx.cols)
			}
		})
	}
}

// TestMoneyColumnsAreIntegerMinorUnits 金额口径：BIGINT 最小货币单位，禁止浮点/定点。
// 一旦出现 DECIMAL/DOUBLE，int64 的 Go 字段与 SQL 语义就会分叉（四舍五入位置不可控）。
func TestMoneyColumnsAreIntegerMinorUnits(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		for _, c := range tbl.columns {
			lower := strings.ToLower(c.name)
			isMoney := strings.HasSuffix(lower, "_minor")
			isFlag := strings.Contains(lower, "rate") || strings.Contains(lower, "percent")
			if !isMoney && !isFlag {
				continue
			}
			checked++
			up := strings.ToUpper(c.dataType)
			for _, banned := range []string{"FLOAT", "DOUBLE", "DECIMAL", "NUMERIC"} {
				if strings.Contains(up, banned) {
					t.Errorf("%s.%s 用了浮点/定点列 %q —— 金额必须是 BIGINT 分单位", spec.table, c.name, c.dataType)
				}
			}
			if isMoney && !strings.Contains(up, "BIGINT") {
				t.Errorf("%s.%s 金额列必须是 BIGINT（分），得到 %q", spec.table, c.name, c.dataType)
			}
			if !strings.Contains(c.comment, "分") {
				t.Errorf("%s.%s 是金额列但注释没写明单位是分：%q —— 金额列的单位必须显式表达",
					spec.table, c.name, c.comment)
			}
		}
	}
	if checked < 3 {
		t.Fatalf("只比对了 %d 个金额列（本服务有 3 个 *_minor），说明列名口径被改动", checked)
	}
}

// TestCoinColumnsAreNotMoneyColumns 硬币枚数不是金额：coin_amount 用 INT 枚数计，
// 一旦被改名成 *_minor 就会被金额门禁当成钱来要求 BIGINT 分单位，两边口径都错。
func TestCoinColumnsAreNotMoneyColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	c, ok := mustTable(t, tables, "to_order").column("coin_amount")
	if !ok {
		t.Fatal("to_order 缺少 coin_amount —— 硬币包履约快照没有落点，GrantRef 就成了无源之数")
	}
	if base := c.baseType(); base != "INT" {
		t.Errorf("to_order.coin_amount 必须是 INT 枚数，得到 %s（%q）", base, c.dataType)
	}
	if !strings.Contains(c.comment, "枚") {
		t.Errorf("to_order.coin_amount 注释没写单位是枚：%q —— 枚数与分混用会算错发放量", c.comment)
	}
}

// TestUniqueKeyColumnsUseBinaryCollation 参与唯一性判定/精确匹配的列必须二进制排序规则。
// *_ci/unicode_ci 会把大小写不同的 request_id 判成同一个键：重放被误吞，
// 或者该重放的请求被判成「号已被占用」——两种都是订单事实故障。
func TestUniqueKeyColumnsUseBinaryCollation(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for _, name := range spec.binaryKeys {
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, name)
				}
				// 整数列不得登记进 binaryKeys：它没有排序规则，登记进来就是假失败。
				if !hasCollation(c.dataType) {
					t.Fatalf("%s.%s 是 %s，不是字符列，不能登记在 binaryKeys 里（整数列没有 collation）",
						spec.table, name, c.baseType())
				}
				if !strings.Contains(c.dataType, "COLLATE utf8mb4_bin") {
					t.Errorf("%s.%s 是幂等/单号/精确匹配键但没声明 COLLATE utf8mb4_bin：%q",
						spec.table, name, c.dataType)
				}
			}
			// 反向：所有唯一键的**字符列**都必须在 binaryKeys 清单里，避免漏登记。
			// 整数列（mid / state / biz_type）也在唯一键上，但它们没有排序规则可言。
			for _, idx := range tbl.indexes {
				if !idx.unique {
					continue
				}
				for _, col := range idx.cols {
					c, _ := tbl.column(col) // 列存在性已由 addIndex 钉死
					if !hasCollation(c.dataType) {
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
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// logicLimitToColumns 把 logic/helpers.go 里的列宽常量映射到受它约束的表列。
// 未登记的常量与找不到的常量都会失败（见下面两个测试）。
var logicLimitToColumns = map[string][]string{
	"maxOrderNoLen":     {"to_order.order_no", "to_order_event.order_no"},
	"maxRequestIDLen":   {"to_order.request_id", "to_order_event.request_id"},
	"maxDetailLen":      {"to_order.fulfill_detail"},
	"maxReasonLen":      {"to_order_event.reason"},
	"maxTitleLen":       {"to_order.title"},
	"maxOperatorLen":    {"to_order_event.operator"},
	"maxPaymentNoLen":   {"to_order.payment_no"},
	"maxGrantRefLen":    {"to_order.grant_ref"},
	"maxClientTraceLen": {"to_order.client_trace_id"},
}

// parseLogicLimits 读 logic/helpers.go 的常量真值：不在测试里抄一份数字。
func parseLogicLimits(t *testing.T) map[string]int {
	t.Helper()
	src := readFileOrFatal(t, filepath.Join(findRepoRoot(t), filepath.FromSlash(logicHelpersRelPath)))
	out := map[string]int{}
	for _, m := range logicLimitRe.FindAllStringSubmatch(src, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("%s 的 %s 值解析失败: %v", logicHelpersRelPath, m[1], err)
		}
		out[m[1]] = n
	}
	if len(out) < 9 {
		t.Fatalf("只从 %s 解析出 %d 个列宽常量，logic 侧命名被改动（期望 max*Len）", logicHelpersRelPath, len(out))
	}
	return out
}

// TestLogicTextLimitFitsColumnWidth 钉住「logic 校验上限 <= 列宽」。
// 反例：logic 按 500 rune 放行中文摘要，而 reason 只有 VARCHAR(200) —— 校验过了、写入必炸。
func TestLogicTextLimitFitsColumnWidth(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	limits := parseLogicLimits(t)
	for name, targets := range logicLimitToColumns {
		limit, ok := limits[name]
		if !ok {
			t.Errorf("logic 侧常量 %s 已被删除或改名，测试仍在引用它（%s）", name, logicHelpersRelPath)
			continue
		}
		for _, target := range targets {
			tblName, colName, _ := strings.Cut(target, ".")
			c, ok := mustTable(t, tables, tblName).column(colName)
			if !ok {
				t.Errorf("%s 缺少列 %s（logic 的 %s 在校验它）", tblName, colName, name)
				continue
			}
			width, isVarchar := c.varcharLen()
			if !isVarchar {
				t.Errorf("%s.%s 不是 VARCHAR，无法与 logic 的 %s=%d 比对：%q", tblName, colName, name, limit, c.dataType)
				continue
			}
			if width < limit {
				t.Errorf("%s.%s 列宽 VARCHAR(%d) 小于 logic 允许的 %d 字符 —— 校验放过而写入报错",
					tblName, colName, width, limit)
			}
		}
	}
}

// TestEveryLogicLimitIsMapped 防止「logic 加了校验上限而没人管列宽」。
func TestEveryLogicLimitIsMapped(t *testing.T) {
	for name := range parseLogicLimits(t) {
		if _, ok := logicLimitToColumns[name]; !ok {
			t.Errorf("%s 里的 %s 没有映射到具体列：新增校验必须同时钉住列宽", logicHelpersRelPath, name)
		}
	}
}

// TestModelTruncateLimitMatchesLogic fulfill_detail 由 model 的 truncateForCol 兜底截断，
// 两处上限不一致时「谁后跑谁说了算」，摘要长度口径就分裂了。
func TestModelTruncateLimitMatchesLogic(t *testing.T) {
	limits := parseLogicLimits(t)
	if limits["maxDetailLen"] != maxFulfillDetailLen {
		t.Errorf("logic 的 maxDetailLen=%d 与 model 的 maxFulfillDetailLen=%d 不等",
			limits["maxDetailLen"], maxFulfillDetailLen)
	}
	tables := parseCreateTables(t, findMigrationsDir(t))
	c := mustTable(t, tables, "to_order").columns
	for _, col := range c {
		if col.name != "fulfill_detail" {
			continue
		}
		if w, ok := col.varcharLen(); !ok || w < maxFulfillDetailLen {
			t.Errorf("to_order.fulfill_detail 列宽 %q 装不下 model 的截断上限 %d", col.dataType, maxFulfillDetailLen)
		}
	}
}

// TestEveryTextColumnHasDeclaredLimit 防止「加了列忘了定上限」：
// textLimits 是手工清单，新增 VARCHAR 列不进来就等于留了个校验缺口。
func TestEveryTextColumnHasDeclaredLimit(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			for _, c := range mustTable(t, tables, spec.table).columns {
				if _, ok := c.varcharLen(); !ok {
					continue
				}
				if _, ok := spec.textLimits[c.name]; !ok {
					t.Errorf("%s.%s 是 VARCHAR 但没声明长度上限：%s",
						spec.table, c.name, "要么在 logic 里校验，要么在 spec 里写明为什么不校验")
				}
			}
		})
	}
}

// TestOrderNoWidthArithmetic order_no = idgen.Prefixed("to") = 前缀 + "_" + ULID(26)，
// 最坏 29 字符，必须装得进 VARCHAR(64)；同时 logic 的 rune 上限不能小于这个算式结果，
// 否则合法单号会被入口校验拒掉。
func TestOrderNoWidthArithmetic(t *testing.T) {
	ulid, err := idgen.ULID()
	if err != nil {
		t.Fatalf("生成 ULID 失败: %v", err)
	}
	if len(ulid) != ulidWidth {
		t.Fatalf("ULID 长度 = %d，期望 %d（单号宽度算术的前提）", len(ulid), ulidWidth)
	}
	if bad := strings.Trim(ulid, "0123456789ABCDEFGHJKMNPQRSTVWXYZ"); bad != "" {
		t.Fatalf("ULID 含 Crockford Base32 之外的字符 %q", bad)
	}
	src := readFileOrFatal(t, filepath.Join(findRepoRoot(t), filepath.FromSlash(logicHelpersRelPath)))
	m := logicPrefixRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s 里找不到 orderNoPrefix 常量（前缀被改动就要重新核对单号宽度）", logicHelpersRelPath)
	}
	if m[1] != orderNoPrefixWant {
		t.Fatalf("logic 的 orderNoPrefix=%q 与本测试登记的 %q 不符（迁移头注释写的是 to_ + ULID）",
			m[1], orderNoPrefixWant)
	}
	worst := len(m[1]) + 1 + len(ulid)

	tables := parseCreateTables(t, findMigrationsDir(t))
	c, ok := mustTable(t, tables, "to_order").column("order_no")
	if !ok {
		t.Fatal("to_order 缺少 order_no")
	}
	width, isVarchar := c.varcharLen()
	if !isVarchar {
		t.Fatalf("to_order.order_no 不是 VARCHAR：%q", c.dataType)
	}
	if worst > width {
		t.Errorf("to_order.order_no 列宽 VARCHAR(%d) 装不下最坏订单号 %d 字符（前缀 %d + '_' + ULID %d）",
			width, worst, len(m[1]), len(ulid))
	}
	if got := parseLogicLimits(t)["maxOrderNoLen"]; got < worst {
		t.Errorf("logic 的 maxOrderNoLen=%d 小于最坏单号 %d 字符 —— 校验会把自家订单号判为非法", got, worst)
	}
	// 下游幂等键由 purpose + "_" + order_no 派生（见 logic.deriveKey），
	// 必须落在 request_id 列宽之内，否则 grant_/revoke_ 键会被静默截断成同一个值。
	if rc, ok := mustTable(t, tables, "to_order").column("request_id"); !ok {
		t.Fatal("to_order 缺少 request_id")
	} else if w, isVarchar := rc.varcharLen(); !isVarchar || w < worst+len("grant_")+1 {
		t.Errorf("to_order.request_id 列宽 %q 装不下派生幂等键（单号 %d + purpose 前缀）", rc.dataType, worst)
	}
}

// TestEnumColumnsDocumentEveryValue 枚举双向比对：
// model 常量必须逐个出现在建表注释里（新增状态值不改注释 => 失败）；
// 注释里出现的数字取值也必须是已定义常量（注释写脏 => 失败）。
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
				if base := col.baseType(); base != "TINYINT" {
					t.Errorf("%s.%s 应为 TINYINT 枚举列，得到 %s（%q）", spec.table, column, base, col.dataType)
				}
				tokens := map[int32]struct{}{}
				for _, n := range numericTokenRe.FindAllString(col.comment, -1) {
					v, err := strconv.ParseInt(n, 10, 32)
					if err != nil {
						continue
					}
					tokens[int32(v)] = struct{}{}
				}
				defined := map[int32]struct{}{}
				for _, v := range values {
					defined[v] = struct{}{}
					if _, ok := tokens[v]; !ok {
						t.Errorf("%s.%s 的注释没说明取值 %d（当前注释 %q）—— 新增枚举值必须同步建表注释",
							spec.table, column, v, col.comment)
					}
				}
				for v := range tokens {
					if _, ok := defined[v]; !ok {
						t.Errorf("%s.%s 的注释列举了 model 未定义的取值 %d（注释 %q）—— 注释与常量集不等，运维按注释排查会扑空",
							spec.table, column, v, col.comment)
					}
				}
			})
		}
	}
	if checked < 5 {
		t.Fatalf("只比对了 %d 个枚举列（state/fulfill_state/biz_type/pay_method/platform）", checked)
	}
	// 状态取值必须保持 1..11 连续：ValidState 用的是区间判断，重排会把非法值放进来。
	if StateCreated != 1 || StateRefundRejected != 11 || len(tableSpecs()[0].enumComments["state"]) != 11 {
		t.Error("订单状态常量被重排：to_order.state 注释与 rpc.OrderState 契约枚举都会错位")
	}
	for _, v := range tableSpecs()[0].enumComments["state"] {
		if !ValidState(v) {
			t.Errorf("ValidState(%d) 为假，但它在状态集合里", v)
		}
		if strings.HasPrefix(StateName(v), "UNKNOWN") {
			t.Errorf("StateName(%d) 回落到 UNKNOWN —— 错误信息与台账理由会失去可读性", v)
		}
	}
	for _, v := range []int32{0, 12} {
		if ValidState(v) {
			t.Errorf("ValidState(%d) 为真 —— 状态区间没跟着枚举收紧", v)
		}
	}
}

// TestLedgerStateColumnsReferenceOrderStateEnum to_order_event 的 from_state/to_state
// 与主表共用同一取值域：注释必须显式交叉引用，不能各写一份清单。
func TestLedgerStateColumnsReferenceOrderStateEnum(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	ledger := mustTable(t, tables, "to_order_event")
	toState, ok := ledger.column("to_state")
	if !ok {
		t.Fatal("to_order_event 缺少 to_state")
	}
	if !strings.Contains(toState.comment, "to_order.state") {
		t.Errorf("to_order_event.to_state 注释没有引用 to_order.state：%q —— 两份取值清单必然漂移", toState.comment)
	}
	fromState, ok := ledger.column("from_state")
	if !ok {
		t.Fatal("to_order_event 缺少 from_state")
	}
	if !strings.Contains(fromState.comment, "0") || !strings.Contains(fromState.comment, "建单") {
		t.Errorf("to_order_event.from_state 注释没说明 0=建单行：%q —— 台账里会出现无法解释的 0", fromState.comment)
	}
	if !fromState.hasDefault || fromState.defaultVal != "0" {
		t.Errorf("to_order_event.from_state 必须 DEFAULT 0（建单行此前不存在状态），得到 %q", fromState.dataType)
	}
	if base := toState.baseType(); base != "TINYINT" {
		t.Errorf("to_order_event.to_state 应为 TINYINT（与主表同类型），得到 %s", base)
	}
	master, ok := mustTable(t, tables, "to_order").column("state")
	if !ok {
		t.Fatal("to_order 缺少 state 列（台账的取值域无处可引）")
	}
	if base := master.baseType(); base != "TINYINT" {
		t.Errorf("to_order.state 应为 TINYINT，得到 %s（台账列与主表类型不再一致）", base)
	}
}

// TestAppendOnlyLedgerHasNoMutationColumns 台账只追加：
// 没有 mtime/version，需要纠正只能再记一行（主表状态可以被覆盖，历史不行）。
func TestAppendOnlyLedgerHasNoMutationColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	tbl := mustTable(t, tables, "to_order_event")
	for _, banned := range []string{"mtime", "updated_at", "version", "deleted_at"} {
		if tbl.hasColumn(banned) {
			t.Errorf("to_order_event 出现了列 %s —— 只追加台账不允许原地改写", banned)
		}
	}
	if _, ok := tbl.column("ctime"); !ok {
		t.Error("to_order_event 缺少 ctime —— 台账必须自带时间戳，否则「谁先推的」不可考")
	}
	if !strings.Contains(tbl.comment, "只追加") {
		t.Errorf("to_order_event 的表注释没声明只追加：%q", tbl.comment)
	}
	// 台账里承载「谁推的、为什么」两列必须存在且有口径（RejectRefund 靠它反查原状态）。
	for _, name := range []string{"operator", "reason"} {
		c, ok := tbl.column(name)
		if !ok {
			t.Fatalf("to_order_event 缺少列 %s", name)
		}
		if !strings.Contains(c.comment, "PII") && name == "reason" {
			t.Errorf("to_order_event.reason 注释没写明不含 PII：%q", c.comment)
		}
	}
}

// TestOptimisticLockVersionColumn 状态推进只有 CAS 一条路：version 位点必须存在且是 BIGINT。
// （WHERE 里带不带 version 属于 logic 测试范围，本文件不管。）
func TestOptimisticLockVersionColumn(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		if spec.casColumn == "" {
			if tbl.hasColumn("version") {
				t.Errorf("%s 有 version 列但 spec 未声明 CAS 用途（台账表不应有可改写的位点）", spec.table)
			}
			continue
		}
		c, ok := tbl.column(spec.casColumn)
		if !ok {
			t.Fatalf("%s 缺少乐观锁列 %s —— 读改写并发会把订单推回旧状态", spec.table, spec.casColumn)
		}
		if base := c.baseType(); base != "BIGINT" {
			t.Errorf("%s.%s 必须是 BIGINT，得到 %s", spec.table, spec.casColumn, base)
		}
		if !c.notNull {
			t.Errorf("%s.%s 必须 NOT NULL：NULL 位点让 WHERE version=? 永不命中", spec.table, spec.casColumn)
		}
		if !strings.Contains(c.comment, "CAS") && !strings.Contains(c.comment, "乐观锁") {
			t.Errorf("%s.%s 注释没说明是 CAS 位点：%q", spec.table, spec.casColumn, c.comment)
		}
		if gt := structColumnTypes(t, spec.row)[spec.casColumn]; gt.Kind() != reflect.Int64 {
			t.Errorf("%s.%s 的 Go 类型是 %s，SQL 是 BIGINT —— 期望 int64", spec.table, spec.casColumn, gt)
		}
	}
}

// TestColumnDefaultsMatchModelInit 有 DEFAULT 的枚举/位点列必须与 model 的初值一致：
// 一旦 renumber（比如 FulfillPending 改成 0），库里默认值会静默造出一个未定义状态行。
func TestColumnDefaultsMatchModelInit(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	cases := []struct {
		table, column, want string
		why                 string
	}{
		{"to_order", "fulfill_state", strconv.Itoa(int(FulfillPending)),
			"新单的履约结果必须是 PENDING 常量值"},
		{"to_order", "version", "1", "model.InsertTx 在 Version==0 时补 1，库默认必须同值"},
		{"to_order", "currency", "'CNY'", "DEFAULT 币种必须带引号落库且与服务默认一致"},
		{"to_order", "quantity", "1", "缺份数时按 1 份计，与 logic 的兜底一致"},
	}
	for _, c := range cases {
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			col, ok := mustTable(t, tables, c.table).column(c.column)
			if !ok {
				t.Fatalf("%s 缺少列 %s", c.table, c.column)
			}
			if !col.hasDefault {
				t.Fatalf("%s.%s 没有 DEFAULT：%s", c.table, c.column, c.why)
			}
			if col.defaultVal != c.want {
				t.Errorf("%s.%s DEFAULT=%q，期望 %q（%s）", c.table, c.column, col.defaultVal, c.want, c.why)
			}
		})
	}
}

// unixUnitMustBeDocumented 是「注释必须写明 Unix 秒」的列：这些列的值会被拿去和
// 服务端算出来的界做比较（分页排序、卡单超时、关单期限），单位口径写错就是判据写错。
// 纯展示时间戳（paid_at/fulfilled_at/closed_at）只由 nowUnix() 写入、不参与比较，
// 不在本条口径内（见交付报告：那三列的注释确实也没写单位）。
var unixUnitMustBeDocumented = map[string]struct{}{
	"to_order.created_at":  {},
	"to_order.updated_at":  {},
	"to_order.expire_at":   {},
	"to_order_event.ctime": {},
}

func TestTimeColumnsAreUnixSecondsBigInt(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for _, name := range spec.timeColumns {
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少时间列 %s", spec.table, name)
				}
				if base := c.baseType(); base != "BIGINT" {
					t.Errorf("%s.%s 必须是 BIGINT（Unix 秒），得到 %s（%q）—— 改成 DATETIME 会让 int64 比较全部错位",
						spec.table, name, base, c.dataType)
				}
				if _, must := unixUnitMustBeDocumented[spec.table+"."+name]; must &&
					!strings.Contains(c.comment, "Unix 秒") {
					t.Errorf("%s.%s 是被比较的时间列，注释必须写明「Unix 秒」：%q", spec.table, name, c.comment)
				}
			}
			// 反向：任何名字带 _at/ctime 的列都必须在 spec.timeColumns 里登记，
			// 否则新增时间列会绕过 BIGINT 口径检查。
			for _, c := range tbl.columns {
				lower := strings.ToLower(c.name)
				if !strings.HasSuffix(lower, "_at") && lower != "ctime" {
					continue
				}
				if !containsString(spec.timeColumns, c.name) {
					t.Errorf("%s.%s 是时间列却没登记在 spec.timeColumns —— 逃过类型门禁", spec.table, c.name)
				}
			}
		})
	}
}

// TestOwnershipAndActorColumnsDocumented 表注释与操作者列必须说清「谁唯一可写、谁能推」。
// 订单只有买家侧能建单/取消/申退，后台只做退款裁决：这一点在库里的可见证据就是 operator 取值域。
func TestOwnershipAndActorColumnsDocumented(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	order := mustTable(t, tables, "to_order")
	for _, marker := range []string{"唯一所有者", "不得回写"} {
		if !strings.Contains(order.comment, marker) {
			t.Errorf("to_order 表注释缺少 %q：%q —— 数据所有者（AGENTS.md §5）必须写在表上", marker, order.comment)
		}
	}
	if mc, ok := order.column("mid"); !ok {
		t.Fatal("to_order 缺少 mid")
	} else if !strings.Contains(mc.comment, "下单用户") || !strings.Contains(mc.comment, "游客") {
		t.Errorf("to_order.mid 注释没说清下单主体口径：%q", mc.comment)
	}
	ledger := mustTable(t, tables, "to_order_event")
	op, ok := ledger.column("operator")
	if !ok {
		t.Fatal("to_order_event 缺少 operator")
	}
	for _, marker := range []string{"user", "运营", "cron"} {
		if !strings.Contains(op.comment, marker) {
			t.Errorf("to_order_event.operator 注释未列出操作者 %q：%q —— 买家侧/运营/定时任务三种来源必须可分辨",
				marker, op.comment)
		}
	}
	// 跨服务引用列必须注明「只存引用、不建外键」，否则有人会补 FK 把锁窗口带进来。
	for _, spec := range []struct{ table, column, marker string }{
		{"to_order", "plan_id", "不建外键"},
		{"to_order", "payment_no", "引用"},
		{"to_order_event", "order_no", "不建外键"},
	} {
		c, ok := mustTable(t, tables, spec.table).column(spec.column)
		if !ok {
			t.Fatalf("%s 缺少列 %s", spec.table, spec.column)
		}
		if !strings.Contains(c.comment, spec.marker) {
			t.Errorf("%s.%s 注释没写明 %q：%q", spec.table, spec.column, spec.marker, c.comment)
		}
	}
}

// TestRefundColumnsStateSandboxDestination 退款只回沙箱余额、不到银行卡（AGENTS.md §1：
// 「退款到卡」在本期范围外）。承载退款金额的列必须自带去向口径：读到 refunded_minor 的人
// 只会看这一行注释，不看迁移头，也不看 proto。
func TestRefundColumnsStateSandboxDestination(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	destMarkers := []string{"余额", "银行卡", "沙箱"}
	var checked int
	for _, spec := range tableSpecs() {
		for _, c := range mustTable(t, tables, spec.table).columns {
			if !strings.Contains(strings.ToLower(c.name), "refund") {
				// 只按列名识别退款承载列：注释里出现「退款」二字的时间列不算。
				continue
			}
			checked++
			found := false
			for _, marker := range destMarkers {
				if strings.Contains(c.comment, marker) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s.%s 承载退款金额但注释没写退款去向（%q 之一）：%q —— "+
					"本服务退款只回沙箱余额、不到银行卡，缺这句会被读成「存在原路退回渠道」",
					spec.table, c.name, strings.Join(destMarkers, "/"), c.comment)
			}
		}
	}
	if checked < 1 {
		t.Fatalf("一个退款列都没识别到（应为 to_order.refunded_minor），退款列名口径被改动")
	}
}

// TestUniquenessClaimsMatchIndexes 列注释里「参与唯一性判定」的声明必须有唯一索引支撑：
// 注释这样写、索引却是普通 KEY，会误导后来人以为数据库在这列上去重。
func TestUniquenessClaimsMatchIndexes(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var claims int
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		unique := map[string]bool{}
		for _, idx := range tbl.indexes {
			if !idx.unique {
				continue
			}
			for _, col := range idx.cols {
				unique[col] = true
			}
		}
		for _, c := range tbl.columns {
			if !strings.Contains(c.comment, "参与唯一性判定") {
				continue
			}
			claims++
			if !unique[c.name] {
				t.Errorf("%s.%s 的注释声明「参与唯一性判定」，但该表上没有任何 UNIQUE 索引覆盖它：%q —— "+
					"注释与索引互相矛盾，去重责任会被读错（迁移头注释第 3 条说明它只能是普通索引）",
					spec.table, c.name, c.comment)
			}
		}
	}
	if claims < 2 {
		t.Fatalf("只识别到 %d 条唯一性声明注释，说明相关列注释被批量改动", claims)
	}
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
			for _, marker := range []string{"数据所有者", "回滚", "幂等", "目标库", "沙箱"} {
				if !strings.Contains(text, marker) {
					t.Errorf("迁移头注释缺少 %q 段落（订单库的口径与回滚必须写清）", marker)
				}
			}
			for _, banned := range []string{"TRUNCATE", "DROP DATABASE", "GRANT ", "FOREIGN KEY", "DELETE FROM", "ALTER TABLE"} {
				if containsOutsideComments(text, banned) {
					t.Errorf("迁移里出现了禁止语句 %q", banned)
				}
			}
			if !strings.Contains(text, "go_video_trade_order") {
				t.Error("迁移未声明所属库 go_video_trade_order，无法确认数据所有权")
			}
			// 回滚段里给出的 DROP 语句必须覆盖两张表，否则回滚只能靠人记。
			for _, name := range []string{"to_order", "to_order_event"} {
				if !strings.Contains(text, "DROP TABLE IF EXISTS `"+name+"`") {
					t.Errorf("迁移头注释缺少 %s 的回滚 DROP 语句", name)
				}
			}
		})
	}
}

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

// TestFilterUsesPlaceholders 筛选值只能走占位符：把调用方给的字符串拼进 WHERE
// 就是一处注入面，而 OrderFilter 的字段全部来自 RPC 入参。
func TestFilterUsesPlaceholders(t *testing.T) {
	f := &OrderFilter{
		Mid: 4242, States: []int32{1, 2, 3}, BizType: 1, PayMethod: 2,
		OrderNo: "to_01ABCDEFGHIJKLMNPQRSTVWXYZ", PaymentNo: "PM_01ABCDEFGHIJKLMNPQRSTVWXY",
		FromTs: 1700000000, ToTs: 1800000000,
	}
	where, args := f.build()
	if got := strings.Count(where, "?"); got != len(args) {
		t.Errorf("占位符 %d 个而参数 %d 个：%q", got, len(args), where)
	}
	for _, leak := range []string{"4242", "to_01", "PM_01", "1700000000", "1800000000"} {
		if strings.Contains(where, leak) {
			t.Errorf("筛选值 %s 被拼进了 WHERE 文本：%q —— 必须走占位符", leak, where)
		}
	}
	if !strings.Contains(where, "state IN (?,?,?)") {
		t.Errorf("States 非空时应按 IN 过滤且个数匹配：%q", where)
	}
	empty, emptyArgs := (&OrderFilter{}).build()
	if empty != "1 = 0" || len(emptyArgs) != 0 {
		t.Errorf("无过滤条件时应拒绝而不是全表扫，得到 %q / %v", empty, emptyArgs)
	}
	// placeholders() 是全仓 IN 片段的唯一来源，退化一次就会少一个参数。
	if got := placeholders(1); got != "?" {
		t.Errorf("placeholders(1) = %q", got)
	}
	if got := placeholders(3); got != "?,?,?" {
		t.Errorf("placeholders(3) = %q", got)
	}
}

// TestParserActuallyParsedSomething 解析器自检：上面的门禁全部依赖解析结果，
// 解析退化成空表时宁可当场报错，也不能让一串「比对了 0 个东西」的断言发绿。
func TestParserActuallyParsedSomething(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	if len(tables) < 2 {
		t.Fatalf("只解析出 %d 张表，期望至少 2 张（to_order / to_order_event）", len(tables))
	}
	var cols, idx, uniq int
	for _, name := range []string{"to_order", "to_order_event"} {
		tbl := mustTable(t, tables, name)
		cols += len(tbl.columns)
		idx += len(tbl.indexes)
		for _, i := range tbl.indexes {
			if i.unique {
				uniq++
			}
		}
		if tbl.comment == "" {
			t.Errorf("%s 的表级 COMMENT 没解析出来", name)
		}
	}
	if cols < 39 {
		t.Fatalf("只解析出 %d 列，model 两张表合计 39 列 —— 解析器退化", cols)
	}
	if idx < 6 {
		t.Fatalf("只解析出 %d 个索引，期望至少 6 个（4 普通 + 2 唯一）", idx)
	}
	if uniq < 2 {
		t.Fatalf("只解析出 %d 个唯一索引，订单幂等至少需要 uniq_order_no 与 uniq_request_id", uniq)
	}
	if got := len(structColumns(t, Order{})); got != 31 {
		t.Fatalf("Order 结构体列数 %d，与建表列数不再一一对应（SQL 为 %d 列）",
			got, len(mustTable(t, tables, "to_order").columns))
	}
	if got := len(structColumns(t, OrderEvent{})); got != 8 {
		t.Fatalf("OrderEvent 结构体列数 %d，与建表列数不再一一对应", got)
	}
}
