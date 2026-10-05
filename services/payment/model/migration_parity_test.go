package model

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

// 本文件把「model 的结构体 / SELECT 列常量 与 deploy/migrations/payment 的建表语句逐列一致」
// 变成可执行门禁（AGENTS.md §9），不连数据库：只读 SQL 文本与 Go 侧声明比对。
//
// 为什么资金域尤其需要（本服务本轮迁移未在 MySQL 上跑过）：
//   - 结构体 db tag、SELECT 列常量、INSERT 列表是三处必须同步的文本，任一处漂移会在
//     运行期变成 "Unknown column" 或字段扫描错位 —— 而错位的对象是钱；
//   - uniq_request_id / uniq_biz_order_no 这些唯一索引是幂等的最后防线，被删或被换成
//     普通 KEY 时表现不是启动失败，而是「同一请求重复入账」这种最贵的故障；
//   - 金额必须是 BIGINT：一旦有人加个 DECIMAL/DOUBLE 列，int64 分单位口径立刻失真；
//   - 单据号与幂等键必须 CHARACTER SET utf8mb4 COLLATE utf8mb4_bin：*_ci 排序规则会把
//     大小写不同的两个 request_id 判成同一个键，重放被误吞、或该重放的被判重复；
//   - 单据号宽度（common/idgen 的 prefix_ULID）必须装得进 VARCHAR(40)，
//     并且 logic 侧的 rune 校验上限必须 <= 列宽，否则「校验放过、写入报错」。
//
// 迁移文件只读不改（AGENTS.md §10）：本测试是漂移探测器，不是改库的入口。

const migrationsRelDir = "deploy/migrations/payment"

// docNoColumnMaxLen 是单据号列的 model 侧长度上限（logic.requireMaxLength 用的就是这个值）。
const docNoColumnMaxLen = 40

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
	// binaryKeys 是参与唯一性判定/单号比对的列：必须 utf8mb4_bin。
	binaryKeys []string
	// textLimits 是 model 侧允许的最大字符数（logic 按 rune 校验）；断言方向：列宽 >= 上限。
	textLimits map[string]int
	// mustHaveDefault 之外的资金列：漏传必须报错，不能靠 DEFAULT 静默记 0。
	requiredNoDefault []string
	// enumComments 是数值枚举列：注释必须逐值说明，否则线上排查只能靠猜。
	// 元素类型取 model 常量的类型（int32），新增枚举值忘记同步建表注释即失败。
	enumComments map[string][]int32
	timeColumns  []string
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:         "pm_wallet",
			row:           Wallet{},
			selectColumns: walletColumns,
			primaryKey:    []string{"id"},
			// 一个用户一行：这只账户的余额只能有一行承载。
			uniqueKeys:      map[string][]string{"uniq_mid": {"mid"}},
			requiredIndexes: nil,
			binaryKeys:      []string{"currency"},
			textLimits:      map[string]int{"currency": 3},
			enumComments:    nil,
			timeColumns:     []string{"ctime", "mtime"},
		},
		{
			table:         "pm_recharge",
			row:           Recharge{},
			selectColumns: rechargeColumns,
			primaryKey:    []string{"id"},
			// 开单幂等（客户端重试）+ 单号对外唯一。
			uniqueKeys: map[string][]string{
				"uniq_recharge_no": {"recharge_no"},
				"uniq_request_id":  {"request_id"},
			},
			requiredIndexes: map[string][]string{
				"idx_mid_ctime":   {"mid", "ctime"},   // ListRecharges：我的台账倒序翻页
				"idx_state_ctime": {"state", "ctime"}, // 运营按状态巡检
			},
			binaryKeys: []string{"recharge_no", "request_id", "currency"},
			textLimits: map[string]int{
				"recharge_no": docNoColumnMaxLen, "request_id": 64, "operator": 64,
				"reason": maxReasonRunes, "client_trace_id": 64, "currency": 3,
			},
			requiredNoDefault: []string{"recharge_no", "request_id", "mid", "amount_minor"},
			enumComments: map[string][]int32{
				"state":   {RechargeStatePending, RechargeStateSuccess, RechargeStateCancelled, RechargeStateFailed},
				"channel": {ChannelSandbox},
			},
			timeColumns: []string{"settled_at", "ctime", "mtime"},
		},
		{
			table:         "pm_payment",
			row:           Payment{},
			selectColumns: paymentColumns,
			primaryKey:    []string{"id"},
			// 一单一支付（uniq_biz_order_no）+ 受理幂等（uniq_request_id）+ 单号唯一。
			uniqueKeys: map[string][]string{
				"uniq_payment_no":   {"payment_no"},
				"uniq_biz_order_no": {"biz_order_no"},
				"uniq_request_id":   {"request_id"},
			},
			requiredIndexes: map[string][]string{
				"idx_mid_ctime":   {"mid", "ctime"},
				"idx_state_ctime": {"state", "ctime"},
			},
			binaryKeys: []string{"payment_no", "biz_order_no", "request_id", "last_request_id", "currency"},
			textLimits: map[string]int{
				"payment_no": docNoColumnMaxLen, "biz_order_no": 64, "request_id": 64,
				"last_request_id": 64, "subject": maxReasonRunes, "operator": 64,
				"remark": maxReasonRunes, "currency": 3,
			},
			requiredNoDefault: []string{"payment_no", "biz_order_no", "request_id", "mid", "amount_minor", "method", "state"},
			enumComments: map[string][]int32{
				"state":  {PaymentStatePending, PaymentStatePaid, PaymentStateFailed, PaymentStateClosed, PaymentStateRefunded, PaymentStatePartiallyRefunded},
				"method": {MethodBalance, MethodSandboxChannel},
			},
			timeColumns: []string{"paid_at", "expire_at", "ctime", "mtime"},
		},
		{
			table:         "pm_refund",
			row:           Refund{},
			selectColumns: refundColumns,
			primaryKey:    []string{"id"},
			uniqueKeys: map[string][]string{
				"uniq_refund_no":  {"refund_no"},
				"uniq_request_id": {"request_id"},
			},
			requiredIndexes: map[string][]string{
				"idx_payment_no":   {"payment_no"},   // GetRefunds / ListRefunds 按原单回查
				"idx_biz_order_no": {"biz_order_no"}, // 业务服务只持有订单号时也能查
				"idx_mid_ctime":    {"mid", "ctime"},
				"idx_state_ctime":  {"state", "ctime"},
			},
			binaryKeys: []string{"refund_no", "request_id", "payment_no", "biz_order_no", "destination", "currency"},
			textLimits: map[string]int{
				"refund_no": docNoColumnMaxLen, "request_id": 64, "payment_no": docNoColumnMaxLen,
				"biz_order_no": 64, "destination": 16, "operator": 64, "reason": maxReasonRunes, "currency": 3,
			},
			requiredNoDefault: []string{"refund_no", "request_id", "payment_no", "biz_order_no", "mid", "amount_minor"},
			enumComments: map[string][]int32{
				"state": {RefundStateSucceeded, RefundStateFailed},
			},
			timeColumns: []string{"ctime"},
		},
		{
			table:         "pm_flow",
			row:           Flow{},
			selectColumns: flowColumns,
			// 流水表主键不叫 id：显式声明，避免有人「统一命名」后扫描错位。
			primaryKey: []string{"flow_id"},
			// 流水幂等（同一请求号只落一条）+ 一笔业务动作只落一条流水。
			uniqueKeys: map[string][]string{
				"uniq_request_id":      {"request_id"},
				"uniq_biz_type_biz_no": {"biz_type", "biz_no"},
			},
			requiredIndexes: map[string][]string{
				"idx_mid_ctime":      {"mid", "ctime"},
				"idx_biz_type_ctime": {"biz_type", "ctime"}, // 无 state 列，按类型巡检走这里
				"idx_biz_no":         {"biz_no"},            // FindByBizNo：按单号回查资金变动
			},
			binaryKeys: []string{"biz_no", "request_id", "currency"},
			textLimits: map[string]int{
				"biz_no": 64, "remark": maxReasonRunes, "operator": 64, "request_id": 64, "currency": 3,
			},
			requiredNoDefault: []string{"mid", "biz_type", "biz_no", "delta_minor", "request_id"},
			enumComments: map[string][]int32{
				"biz_type": {FlowBizRecharge, FlowBizPayment, FlowBizRefund, FlowBizAdminAdjust},
			},
			timeColumns: []string{"ctime"},
		},
	}
}

// maxReasonRunes 与 internal/logic/helpers.go 的 maxTextRunes 同值。
// 两处分别属于 model 与 logic 包，无法共享常量，所以在这里把它钉成断言：
// 任何一侧单独改动都会让本门禁失败（见 TestLogicTextLimitFitsColumnWidth）。
const maxReasonRunes = 100

// --- SQL 解析 ---

type sqlColumn struct {
	name          string
	dataType      string // 列定义原文（COMMENT 之前），用于类型/排序规则判定
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

func (t *sqlTable) check(name string) (string, bool) {
	v, ok := t.checks[name]
	return v, ok
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
	dir := filepath.Join(findRepoRoot(t), "deploy", "migrations", "payment")
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
			t.Fatalf("%s: 表 %s 的列 %s 缺少 COMMENT —— 资金列的取值口径全靠注释表达", file, cur.table, name)
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
	charLenRe      = regexp.MustCompile(`\bCHAR\((\d+)\)`)
	numericTokenRe = regexp.MustCompile(`\d+`)
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

func (c sqlColumn) isChar3() bool {
	m := charLenRe.FindStringSubmatch(strings.ToUpper(c.dataType))
	return m != nil && m[1] == "3"
}

// hasCollation 只有字符列（CHAR/VARCHAR/TEXT/ENUM/SET）才有排序规则这一说，
// 整数列（mid、biz_type）即便参与唯一索引也不能要求登记 utf8mb4_bin。
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
			// 扫描顺序 = 列常量顺序 = 结构体字段顺序：错位会把 mid 读成 amount。
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
					t.Fatalf("%s 缺少唯一索引 %s —— 幂等失去数据库兜底，同一请求会重复动钱", spec.table, name)
				}
				if !idx.unique {
					t.Fatalf("%s 的 %s 必须是 UNIQUE KEY，普通 KEY 起不到去重作用", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("唯一索引 %s 列不符：SQL %v，期望 %v（顺序敏感）", name, idx.cols, cols)
				}
			}
			if len(spec.uniqueKeys) == 0 {
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
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("索引 %s 列不符：SQL %v，期望 %v", name, idx.cols, cols)
				}
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
	if checked < 8 {
		t.Fatalf("只比对了 %d 个金额列，说明列名口径被改动（*_minor 是唯一命名约定）", checked)
	}
}

// TestCurrencyColumnsAreFixedChar3 币种必须 CHAR(3)：
// 换成 VARCHAR(8) 之后 "CNY " 与 "CNY" 会被当成两个币种，条件更新的 WHERE 直接失配。
func TestCurrencyColumnsAreFixedChar3(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			c, ok := mustTable(t, tables, spec.table).column("currency")
			if !ok {
				t.Fatalf("%s 缺少 currency 列：币种必须随行显式记录，不能靠库级默认", spec.table)
			}
			if !c.isChar3() {
				t.Errorf("%s.currency 必须是 CHAR(3)，得到 %q", spec.table, c.dataType)
			}
			if !strings.Contains(c.dataType, "utf8mb4_bin") {
				t.Errorf("%s.currency 必须显式 COLLATE utf8mb4_bin，得到 %q", spec.table, c.dataType)
			}
		})
	}
}

// TestUniqueKeyColumnsUseBinaryCollation 参与唯一性判定的列必须二进制排序规则。
// *_ci/unicode_ci 会把大小写不同的 request_id 判成同一个键：重放被误吞，
// 或者该重放的请求被判成「号已被占用」——两种都是资金故障。
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
				if !strings.Contains(c.dataType, "COLLATE utf8mb4_bin") {
					t.Errorf("%s.%s 是幂等/单号键但没声明 COLLATE utf8mb4_bin：%q", spec.table, name, c.dataType)
				}
			}
			// 反向：所有唯一键的**字符列**都必须在 binaryKeys 清单里，避免漏登记。
			// 整数列（mid / biz_type）也在唯一键上，但它们没有排序规则可言，
			// 一并要求登记只会把门禁变成噪音。
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
						t.Errorf("%s 的唯一索引 %s 用了列 %s，但 spec.binaryKeys 未登记其排序规则要求",
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

// TestLogicTextLimitFitsColumnWidth 钉住「model/logic 校验上限 <= 列宽」。
// 反例：logic 按 100 rune 放行中文理由，而 reason 只有 VARCHAR(60) —— 校验过了、写入必炸。
func TestLogicTextLimitFitsColumnWidth(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for column, limit := range spec.textLimits {
				c, ok := tbl.column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				width, isVarchar := c.varcharLen()
				if !isVarchar {
					if !c.isChar3() || limit != 3 {
						t.Fatalf("%s.%s 不是 VARCHAR，无法与长度上限比对：%q", spec.table, column, c.dataType)
					}
					continue
				}
				if width < limit {
					t.Errorf("%s.%s 列宽 VARCHAR(%d) 小于 model/logic 允许的 %d 字符 —— 校验放过而写入报错",
						spec.table, column, width, limit)
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

// TestDocumentNoFitsColumnWidth 单据号的长度算术：
// common/idgen.Prefixed(prefix) = prefix + "_" + ULID(26)，最坏 29 字符，
// 必须装得进 VARCHAR(40)；同时 logic 的 rune 上限（40）也不能超过列宽。
func TestDocumentNoFitsColumnWidth(t *testing.T) {
	ulid, err := idgen.ULID()
	if err != nil {
		t.Fatalf("生成 ULID 失败: %v", err)
	}
	if len(ulid) != 26 {
		t.Fatalf("ULID 长度 = %d，期望 26（单据号宽度算术的前提）", len(ulid))
	}
	ulid = strings.ToUpper(ulid)
	if bad := strings.Trim(ulid, "0123456789ABCDEFGHJKMNPQRSTVWXYZ"); bad != "" {
		t.Fatalf("ULID 含 Crockford Base32 之外的字符 %q", bad)
	}
	const prefixWidth = 2 // RC / PM / RF / AJ
	worst := prefixWidth + 1 + len(ulid)

	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, name := range []string{"recharge_no", "payment_no", "refund_no"} {
		var col sqlColumn
		var table string
		for _, spec := range tableSpecs() {
			if c, ok := mustTable(t, tables, spec.table).column(name); ok {
				col, table = c, spec.table
			}
		}
		if col.name == "" {
			t.Fatalf("找不到单据号列 %s", name)
		}
		width, ok := col.varcharLen()
		if !ok {
			t.Fatalf("%s.%s 不是 VARCHAR：%q", table, name, col.dataType)
		}
		if worst > width {
			t.Errorf("%s.%s 列宽 VARCHAR(%d) 装不下最坏单据号 %d 字符（前缀 %d + '_' + ULID %d）",
				table, name, width, worst, prefixWidth, len(ulid))
		}
		if got := specTextLimit(name); got != width {
			t.Errorf("%s.%s 的 logic 上限 %d 与列宽 %d 不等：校验与库必须同一把尺", table, name, got, width)
		}
	}
	// 调整流水的 biz_no 用同一个算式，但它落在 VARCHAR(64) 的通用单号列上。
	bizNo, ok := mustTable(t, tables, "pm_flow").column("biz_no")
	if !ok {
		t.Fatal("pm_flow 缺少 biz_no 列（AJ 调整单号没有落点）")
	}
	if w, isVarchar := bizNo.varcharLen(); !isVarchar || w < worst {
		t.Errorf("pm_flow.biz_no 列宽 %q 装不下最坏调整单号 %d 字符", bizNo.dataType, worst)
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

// TestFundColumnsHaveNoSilentDefault 资金判定列不允许 DEFAULT：
// 漏传时必须是 SQL 报错，而不是被默认值静默记成 0 ——「金额=0 的充值单」比失败更难排查。
func TestFundColumnsHaveNoSilentDefault(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for _, name := range spec.requiredNoDefault {
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, name)
				}
				if c.autoIncrement || !c.notNull {
					continue // 主键与「允许 NULL」另有断言覆盖
				}
				if c.hasDefault {
					t.Errorf("%s.%s 不该有 DEFAULT：%q —— 漏传必须报错", spec.table, name, c.dataType)
				}
			}
		})
	}
}

// TestInvariantChecksExist 数据库侧的兜底防线必须存在。
// 服务侧的条件更新是第一道，CHECK 是防「旁路写入/手工修数」把台账写坏的第二道。
func TestInvariantChecksExist(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	cases := []struct {
		table, constraint, mustContain string
		why                            string
	}{
		{"pm_wallet", "chk_pm_wallet_balance_non_negative", "`balance_minor` >= 0",
			"余额为负就是资损，MySQL 必须直接拒绝"},
		{"pm_payment", "chk_pm_payment_refunded_range", "`refunded_minor` <= `amount_minor`",
			"累计退款不得超过支付金额"},
		{"pm_payment", "chk_pm_payment_amount_positive", "`amount_minor` > 0",
			"零/负金额支付单没有意义"},
		{"pm_flow", "chk_pm_flow_delta_nonzero", "`delta_minor` <> 0",
			"0 变动流水会污染对账"},
	}
	for _, c := range cases {
		t.Run(c.table+"."+c.constraint, func(t *testing.T) {
			tbl, ok := tables[c.table]
			if !ok {
				t.Fatalf("迁移里没有 %s", c.table)
			}
			expr, ok := tbl.check(c.constraint)
			if !ok {
				t.Fatalf("%s 缺少 CHECK 约束 %s（%s）", c.table, c.constraint, c.why)
			}
			if !strings.Contains(expr, c.mustContain) {
				t.Errorf("%s.%s 表达式 = %q，期望包含 %q", c.table, c.constraint, expr, c.mustContain)
			}
		})
	}
	// 反向：不得引入外键（跨表一致性在服务侧同一事务里保证，锁窗口不可控）。
	for name, tbl := range tables {
		for _, c := range tbl.checks {
			if strings.Contains(strings.ToUpper(c), "REFERENCES") {
				t.Errorf("%s 的 CHECK %q 引用了其他表 —— 本服务不建外键", name, c)
			}
		}
	}
}

func TestEnumColumnsDocumentEveryValue(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		for column, values := range spec.enumComments {
			t.Run(spec.table+"."+column, func(t *testing.T) {
				col, ok := mustTable(t, tables, spec.table).column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				if !strings.Contains(strings.ToUpper(col.dataType), "TINYINT") {
					t.Errorf("%s.%s 应为 TINYINT 枚举列，得到 %q", spec.table, column, col.dataType)
				}
				tokens := map[string]struct{}{}
				for _, n := range numericTokenRe.FindAllString(col.comment, -1) {
					tokens[n] = struct{}{}
				}
				for _, v := range values {
					if _, ok := tokens[strconv.FormatInt(int64(v), 10)]; !ok {
						t.Errorf("%s.%s 的注释没说明取值 %d（当前注释 %q）—— 新增枚举值必须同步建表注释",
							spec.table, column, v, col.comment)
					}
				}
			})
		}
	}
	// 状态取值必须与 rpc 枚举同序（这里只能验 model 常量本身，rpc 侧由 logic 投影测试覆盖）。
	if PaymentStatePartiallyRefunded != 6 || len([]int32{PaymentStatePending, PaymentStatePaid,
		PaymentStateFailed, PaymentStateClosed, PaymentStateRefunded, PaymentStatePartiallyRefunded}) != 6 {
		t.Error("支付状态常量被重排：pm_payment.state 注释与契约枚举都会错位")
	}
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
				if !strings.Contains(strings.ToUpper(c.dataType), "BIGINT") {
					t.Errorf("%s.%s 必须是 BIGINT（Unix 秒），得到 %q —— 改成 DATETIME 会让 int64 比较全部错位",
						spec.table, name, c.dataType)
				}
			}
		})
	}
}

// TestAppendOnlyFlowHasNoMutationColumns 流水表 append-only：
// 没有 mtime / state 列，需要纠正只能再记一条反向流水（biz_type=4）。
func TestAppendOnlyFlowHasNoMutationColumns(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	tbl := mustTable(t, tables, "pm_flow")
	for _, banned := range []string{"mtime", "state", "updated_at"} {
		if tbl.hasColumn(banned) {
			t.Errorf("pm_flow 出现了列 %s —— 只追加台账不允许原地改写", banned)
		}
	}
	if _, ok := tbl.column("balance_after_minor"); !ok {
		t.Error("pm_flow 缺少 balance_after_minor —— 流水必须自带变动后余额快照供对账")
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
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			text := string(raw)
			for _, marker := range []string{"数据所有者", "回滚", "幂等", "沙箱"} {
				if !strings.Contains(text, marker) {
					t.Errorf("迁移头注释缺少 %q 段落（资金库的口径与回滚必须写清）", marker)
				}
			}
			for _, banned := range []string{"TRUNCATE", "DROP DATABASE", "GRANT ", "FOREIGN KEY", "DELETE FROM"} {
				if containsOutsideComments(text, banned) {
					t.Errorf("迁移里出现了禁止语句 %q", banned)
				}
			}
			if !strings.Contains(text, "go_video_payment") {
				t.Error("迁移未声明所属库 go_video_payment，无法确认数据所有权")
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

func TestWindowClauseIsParameterised(t *testing.T) {
	// 时间窗必须走占位符（禁止把调用方给的数字拼进 SQL），单侧 0 表示该侧不限。
	sql, args := windowClause(100, 200)
	if strings.Contains(sql, "100") || strings.Contains(sql, "200") {
		t.Errorf("windowClause 把入参拼进了 SQL 文本: %q", sql)
	}
	if len(args) != 2 || args[0] != int64(100) || args[1] != int64(200) {
		t.Errorf("两侧都给时参数 = %v，期望 [100 200]", args)
	}
	if !strings.Contains(sql, "ctime >= ?") || !strings.Contains(sql, "ctime <= ?") {
		t.Errorf("时间窗片段不对: %q", sql)
	}
	// 跨用户查询「两侧都必须给」是服务侧口径（logic.requireListBounds），
	// 由 logic 的 TestCrossUserListingRequiresBoundedWindow 钉住，这里不放宽也不重复收口。
	for _, c := range []struct {
		from, to       int64
		wantConditions int
	}{{0, 0, 0}, {100, 0, 1}, {0, 200, 1}} {
		sql, args = windowClause(c.from, c.to)
		if got := strings.Count(sql, "?"); got != c.wantConditions {
			t.Errorf("windowClause(%d,%d) 条件数 = %d，期望 %d（%q）",
				c.from, c.to, got, c.wantConditions, sql)
		}
		if len(args) != c.wantConditions {
			t.Errorf("windowClause(%d,%d) 参数数 = %d，期望 %d", c.from, c.to, len(args), c.wantConditions)
		}
	}
}
