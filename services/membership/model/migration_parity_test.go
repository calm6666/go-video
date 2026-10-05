package model

// 本文件把「membership 的 model 结构体 / SELECT 列常量 / INSERT 列清单 与
// deploy/migrations/membership/000001_create_membership_tables.sql」钉成可执行门禁
// （AGENTS.md §9「涉及数据库时提供迁移、回滚和索引说明」）。不连数据库：只解析 SQL 文本，
// 与 Go 侧声明做双向比对。迁移文件在本测试里是只读对照物（AGENTS.md §10）。
//
// 为什么这个服务尤其需要：
//   - mb_grant.uniq_request_id 与 mb_biz_request 的主键是「同一次履约只加一次时长」的
//     唯一保证。被删掉或被换成普通 KEY，表现不是启动失败，而是「重复加会员天数」这种
//     最贵且最难回滚的故障（权益一旦生效就要靠 action=REVOKE 的行去冲正）；
//   - 「是不是会员」的唯一事实源是 mb_membership + mb_grant（AGENTS.md §1 资金语义）：
//     本表没有 state 列，到期口径只有 expire_at 与服务端 now 的比较。若有人「顺手补一列
//     state」，就会出现两个互相漂移的判定口径（没人写它就不会过期）；
//   - request_id / plan_code / code / action / params_fingerprint 参与逐字节比较，
//     必须是列级 utf8mb4_bin：表级是 utf8mb4_unicode_ci，删掉列级 COLLATE 之后
//     'ABC' 与 'abc' 会被判成同一个键，幂等语义静默失效；
//   - 反过来，mid / vip_type 这类**整数列**也在唯一索引（uniq_mid_vip_type）上，
//     但它们没有排序规则可言，登记它们只会把门禁变成噪音 —— 本文件用
//     TestBinaryKeyRegistryIsCharOnlyAndComplete 把这条界线钉死；
//   - 标价与时长必须是整数最小单位（price_minor 分、duration_days 天、paid_month_count 月）：
//     一旦引入 DECIMAL/DOUBLE/FLOAT，int64/int32 字段与 SQL 语义就会分叉（四舍五入位置不可控）；
//   - 数值枚举（vip_type/state/source/enabled/…）的取值口径只能靠列注释表达：库里存的是
//     一个 TINYINT，新增枚举值却忘记改建表注释时，线上排查只能靠猜；
//   - 列宽与 model 常量（MaxPlanCodeLength 等）必须同向：常量比列宽长 = 「校验放过、写入报 1406」。

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// membershipMigrationsRelDir 是本服务迁移目录（相对仓库根）。
const membershipMigrationsRelDir = "deploy/migrations/membership"

// maxRequestIdCharsInLogic 与 internal/logic/guard.go 的 maxRequestIdLen 同值。
// logic 包不能被 model 导入（会成环），所以把它钉成断言：
// TestLogicRequestIDGuardMatchesColumnWidth 会直接读 guard.go 源码比对字面量。
const maxRequestIdCharsInLogic = 64

// --- 表清单：一处声明，六张表共用同一套断言 ---

type tableSpec struct {
	table string
	// row 是该表的行结构体样例；db tag 即列名，声明顺序必须等于建表列顺序。
	row any
	// selectColumns 是 model 的完整列清单常量（SELECT 用它，漏列就静默扫成零值）。
	selectColumns string
	// insertFrag 是 model 里该表 INSERT 语句的起始片段，用于抽出写入列清单。
	insertFrag string
	// goFile 是该表的 model 源文件名（本包全部手写，无 goctl 产物）。
	goFile string

	primaryKey []string
	// uniqueKeys 是幂等与不变量依赖的唯一索引：键名 -> 列组合（顺序敏感）。
	uniqueKeys map[string][]string
	// pkIsIdempotencyKey：mb_biz_request 的主键本身就是幂等键，因此没有额外 UNIQUE KEY。
	pkIsIdempotencyKey bool
	// requiredIndexes 是 model 的 WHERE / ORDER BY 依赖的普通索引。
	requiredIndexes map[string][]string
	// binaryKeys 是参与唯一性或逐字节等值比较的**字符列**：必须列级 utf8mb4_bin。
	binaryKeys []string
	// textLimits 是 model/logic 侧允许的最大字符数（按 rune 校验）。
	// 断言方向：列宽 >= 上限；值尽量直接取 model 常量，避免两处各写一个数字。
	textLimits map[string]int
	// enumComments 是数值枚举/标志列：注释必须逐个取值说明。
	// 取值来自 model 常量，新增常量忘记改注释即失败。
	enumComments map[string][]int64
	// stringEnums 是取值被契约钉死的字符串列：注释必须逐个写明。
	stringEnums map[string][]string
	// timeColumns 是该表全部时间列（Unix 秒）。
	timeColumns []string
	// needsVersion：该表被 CAS 写路径使用，必须有 version BIGINT。
	needsVersion bool
	// appendOnly：只追加台账，不得有 mtime/version，主时间列是 ctime。
	appendOnly bool
	// mustNotHaveColumns：按本表口径不允许出现的列（多一个口径就多一次漂移）。
	mustNotHaveColumns []string
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:         "mb_plan",
			row:           Plan{},
			selectColumns: planColumns,
			insertFrag:    "INSERT INTO mb_plan (",
			goFile:        "plan.go",
			primaryKey:    []string{"plan_id"},
			// plan_code 是对外稳定编码：下单引用它而不是自增 ID，重复即 1062。
			uniqueKeys: map[string][]string{"uniq_plan_code": {"plan_code"}},
			requiredIndexes: map[string][]string{
				"idx_state_vip_type": {"state", "vip_type"}, // ListPlans / ListPlansAdmin
				"idx_state_plan_id":  {"state", "plan_id"},  // 运营面按 plan_id 倒序翻页
				"idx_vip_type":       {"vip_type"},          // 档位维度巡检
			},
			binaryKeys: []string{"plan_code", "currency"},
			textLimits: map[string]int{
				"plan_code":   MaxPlanCodeLength,
				"name":        MaxPlanNameLength,
				"description": MaxPlanDescLength,
				"created_by":  MaxOperatorLength,
				"updated_by":  MaxOperatorLength,
				"currency":    3, // CHAR(3) 定宽，见 TestCurrencyColumnIsFixedChar3
			},
			enumComments: map[string][]int64{
				"vip_type":             {int64(VipTypePremium), int64(VipTypePremiumPlus)},
				"state":                {int64(PlanStateDraft), int64(PlanStateOnSale), int64(PlanStateOffSale)},
				"auto_renew_supported": {0, 1},
				// 位掩码列：注释必须逐个位值说明，否则库里一个 13 没人能反推是哪些端。
				"platform_mask": {int64(PlatformBitAndroid), int64(PlatformBitIOS), int64(PlatformBitHarmony),
					int64(PlatformBitDesktop), int64(PlatformBitWeb)},
			},
			stringEnums:  map[string][]string{"currency": {"CNY"}},
			timeColumns:  []string{"ctime", "mtime"},
			needsVersion: true,
		},
		{
			table:         "mb_plan_change_log",
			row:           PlanChangeLog{},
			selectColumns: planChangeLogColumns,
			insertFrag:    "INSERT INTO mb_plan_change_log (",
			goFile:        "plan_change_log.go",
			primaryKey:    []string{"log_id"},
			// 唯一索引兼当 UpsertPlan/SetPlanState 的幂等键（AGENTS.md §5「所有写接口要设计幂等键」）。
			uniqueKeys: map[string][]string{"uniq_request_id": {"request_id"}},
			requiredIndexes: map[string][]string{
				"idx_plan_ctime": {"plan_id", "ctime"}, // ListByPlan：某套餐的变更史倒序
			},
			binaryKeys: []string{"change_type", "request_id", "params_fingerprint"},
			textLimits: map[string]int{
				"change_type":        16, // 见 TestStringEnumValuesFitColumnWidth
				"operator":           MaxOperatorLength,
				"reason":             MaxReasonLength,
				"request_id":         maxRequestIdCharsInLogic,
				"params_fingerprint": 64, // CHAR(64)：sha256 hex 定宽，见 TestFingerprintWidthMatchesColumn
			},
			enumComments: map[string][]int64{
				"from_state": {0, int64(PlanStateDraft), int64(PlanStateOnSale), int64(PlanStateOffSale)},
				"to_state":   {0, int64(PlanStateDraft), int64(PlanStateOnSale), int64(PlanStateOffSale)},
			},
			stringEnums: map[string][]string{
				"change_type": {PlanChangeUpsert, PlanChangeState},
			},
			timeColumns: []string{"ctime"},
			appendOnly:  true,
		},
		{
			table:         "mb_entitlement",
			row:           Entitlement{},
			selectColumns: entitlementColumns,
			insertFrag:    "INSERT INTO mb_entitlement (",
			goFile:        "entitlement.go",
			primaryKey:    []string{"entitlement_id"},
			uniqueKeys:    map[string][]string{"uniq_code": {"code"}},
			requiredIndexes: map[string][]string{
				// List(enabledOnly=true) 与判定侧的「按档位取全部生效码」。
				"idx_enabled_min_vip": {"enabled", "min_vip_type"},
			},
			binaryKeys: []string{"code"},
			textLimits: map[string]int{
				"code":        MaxEntitlementCodeLength,
				"name":        MaxEntitlementNameLength,
				"description": MaxEntitlementDescLength,
				"updated_by":  MaxOperatorLength,
			},
			enumComments: map[string][]int64{
				"min_vip_type": {int64(VipTypePremium), int64(VipTypePremiumPlus)},
				"enabled":      {0, 1},
			},
			timeColumns:  []string{"ctime", "mtime"},
			needsVersion: true,
		},
		{
			table:         "mb_membership",
			row:           Membership{},
			selectColumns: membershipColumns,
			insertFrag:    "INSERT INTO mb_membership (",
			goFile:        "membership.go",
			primaryKey:    []string{"membership_id"},
			// 一个用户一个档位一行：这行是 (mid,vip_type) 当前态的唯一承载。
			// 注意 uniq_mid_vip_type 的两列都是整数列，不涉及排序规则。
			uniqueKeys: map[string][]string{"uniq_mid_vip_type": {"mid", "vip_type"}},
			requiredIndexes: map[string][]string{
				"idx_expire_at":            {"expire_at"},               // ListExpiring 闭区间扫描
				"idx_auto_renew_expire_at": {"auto_renew", "expire_at"}, // cron 续费批次
			},
			binaryKeys: []string{"auto_renew_channel"},
			textLimits: map[string]int{
				"auto_renew_channel": MaxAutoRenewChannelLength,
			},
			enumComments: map[string][]int64{
				"vip_type":   {int64(VipTypePremium), int64(VipTypePremiumPlus)},
				"auto_renew": {0, 1},
				"source":     grantSourceValues(),
			},
			timeColumns:  []string{"start_at", "expire_at", "auto_renew_signed_at", "ctime", "mtime"},
			needsVersion: true,
			// 本表**没有** state 列：到期口径只有 expire_at（见迁移头注释第 2 条）。
			// 有人补一列 state 就等于造出第二个判定事实源，必须红。
			mustNotHaveColumns: []string{"state"},
		},
		{
			table:         "mb_grant",
			row:           Grant{},
			selectColumns: grantColumns,
			insertFrag:    "INSERT INTO mb_grant (",
			goFile:        "grant.go",
			primaryKey:    []string{"grant_id"},
			// 权益生效的唯一凭据：这一条唯一索引就是「不重复加时长」的最后防线。
			uniqueKeys: map[string][]string{"uniq_request_id": {"request_id"}},
			requiredIndexes: map[string][]string{
				"idx_mid_ctime":     {"mid", "ctime"},             // ListGrants 我的台账翻页
				"idx_mid_vip_ctime": {"mid", "vip_type", "ctime"}, // 某档位该用户的变更史
				"idx_biz_order_no":  {"biz_order_no"},             // 退款反查
				"idx_source_ctime":  {"source", "ctime"},          // 按来源巡检
			},
			binaryKeys: []string{"action", "biz_order_no", "payment_no", "request_id"},
			textLimits: map[string]int{
				"action":       16,
				"biz_order_no": MaxBizNoLength,
				"payment_no":   MaxBizNoLength,
				"operator":     MaxOperatorLength,
				"request_id":   maxRequestIdCharsInLogic,
				"reason":       MaxReasonLength,
			},
			enumComments: map[string][]int64{
				"vip_type": {int64(VipTypePremium), int64(VipTypePremiumPlus)},
				"source":   grantSourceValues(),
			},
			stringEnums: map[string][]string{
				// 契约钉死四种动作，新增等于改 proto（AGENTS.md §10「接口有版本」）。
				"action": {ActionGrant, ActionExtend, ActionRevoke, ActionExpire},
			},
			timeColumns: []string{"before_expire_at", "after_expire_at", "ctime"},
			appendOnly:  true,
			// 撤销靠再写一行 action=REVOKE，不靠删行或改行（AGENTS.md §8 保留审计证据）。
			mustNotHaveColumns: []string{"mtime", "state", "updated_by"},
		},
		{
			table:         "mb_biz_request",
			row:           BizRequest{},
			selectColumns: bizRequestColumns,
			insertFrag:    "INSERT INTO mb_biz_request (",
			goFile:        "biz_request.go",
			primaryKey:    []string{"request_id"},
			// 主键即幂等键，因此没有额外 UNIQUE KEY：靠 pkIsIdempotencyKey 表明意图。
			uniqueKeys:         map[string][]string{},
			pkIsIdempotencyKey: true,
			requiredIndexes: map[string][]string{
				"idx_api_ctime": {"api", "ctime"}, // 按用例巡检
				"idx_mid":       {"mid"},          // 按用户回查写请求
				"idx_ctime":     {"ctime"},        // cron 按保留期清理
			},
			binaryKeys: []string{"request_id", "api", "subject", "params_fingerprint"},
			textLimits: map[string]int{
				"request_id":         maxRequestIdCharsInLogic,
				"api":                48,
				"subject":            96,
				"params_fingerprint": 64,
				"operator":           MaxOperatorLength,
			},
			enumComments: map[string][]int64{
				"vip_type": {0, int64(VipTypePremium), int64(VipTypePremiumPlus)}, // 0 = 不涉及档位
			},
			stringEnums: map[string][]string{
				"api": {ApiSetAutoRenew, ApiUpsertEntitlement},
			},
			timeColumns: []string{"ctime"},
			appendOnly:  true,
			// 只回填 result_id，不记 mtime：这条路径不是业务变更史，是幂等台账。
			mustNotHaveColumns: []string{"mtime", "log_id", "version"},
		},
	}
}

// grantSourceValues 将 rpc.GrantSource 的 model 侧常量收成有序集合，
// 供 mb_grant.source / mb_membership.source 的注释比对使用。
func grantSourceValues() []int64 {
	vals := []int32{
		GrantSourceSandboxPurchase, GrantSourceSandboxAutoRenew,
		GrantSourceAdminOps, GrantSourceExperience, GrantSourceLegacyImport,
	}
	out := make([]int64, 0, len(vals)+1)
	out = append(out, 0) // 两个 source 列都允许 0（DEFAULT 0：尚未有变更来源）
	for _, v := range vals {
		out = append(out, int64(v))
	}
	return out
}

// --- SQL 解析（与 payment/coin 的同构解析器，按 membership 的表前缀与写法调整）---

type sqlColumn struct {
	name          string
	dataType      string // 列定义原文（COMMENT 之前），用于类型与排序规则判定
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
	// referencedOtherTable 记录任何跨表引用写法（本服务不建外键）。
	referencedOtherTable bool
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

// uniqueIndexes 返回全部唯一索引（主键也算：mb_biz_request 的幂等锚点就是主键）。
func (t *sqlTable) uniqueIndexes() []sqlIndex {
	out := make([]sqlIndex, 0, len(t.indexes)+1)
	if len(t.primary) > 0 {
		out = append(out, sqlIndex{name: "PRIMARY", unique: true, cols: t.primary})
	}
	for _, i := range t.indexes {
		if i.unique {
			out = append(out, i)
		}
	}
	return out
}

// mustTable 取表定义：表名被改掉时给出可读失败，而不是 nil 解引用 panic
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
	dir := filepath.Join(findRepoRoot(t), "deploy", "migrations", "membership")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("找不到 %s：迁移文件缺失或目录改名（%v）", membershipMigrationsRelDir, err)
	}
	return dir
}

// parseCreateTables 抽出目录内全部 CREATE TABLE 块。
// 只认本仓库既有书写风格（反引号标识符、每列一行、) ENGINE=... 收尾）：
// 格式被改坏时立即报错，而不是悄悄解析出一张空表（见 parserSelfCheck）。
func parseCreateTables(t *testing.T, dir string) map[string]*sqlTable {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("枚举迁移文件失败: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("%s 下没有任何 .sql —— 迁移缺失", membershipMigrationsRelDir)
	}
	out := map[string]*sqlTable{}
	mentioned := 0
	for _, p := range paths {
		base := filepath.Base(p)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", p, err)
		}
		mentioned += strings.Count(string(raw), "CREATE TABLE IF NOT EXISTS")
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
	// 解析器退化自检：出现建表语句却一张都没解析出来，比「表不存在」更常见，
	// 必须单独报错，否则整套对照会静默失效、门禁空转发绿。
	if len(out) == 0 && mentioned > 0 {
		t.Fatalf("%s 里出现 %d 次 CREATE TABLE IF NOT EXISTS 但解析出 0 张表：建表写法变了，先修解析器",
			membershipMigrationsRelDir, mentioned)
	}
	return out
}

// parseTableLine 解析表体里的一行：列 / 主键 / 唯一键 / 普通索引。
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
			t.Fatalf("%s: 表 %s 的列 %s 缺少 COMMENT —— 标价、时长与档位的取值口径全靠注释表达",
				file, cur.table, name)
		}
		if strings.Contains(strings.ToUpper(defined), " REFERENCES ") {
			cur.referencedOtherTable = true
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
		t.Fatalf("%s: 表 %s 内出现无法识别的定义行（FOREIGN KEY / CHECK / 生成列不符合本服务约定）: %q",
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
	baseTypeRe     = regexp.MustCompile(`\b(BIGINT|INT|TINYINT|SMALLINT|MEDIUMINT|VARCHAR|CHAR|TEXT|BLOB|DATE|DATETIME|TIMESTAMP|DECIMAL|NUMERIC|FLOAT|DOUBLE|BIT)\b`)
)

// varcharLen 取 VARCHAR(n) 的字符宽度。
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

// charLen 取定宽 CHAR(n) 的宽度：CHAR 与 VARCHAR 在唯一性判定上等价，
// 但 CHAR 会补空格，所以定宽列的「列宽 == 值长」必须严格相等（见 fingerprint/currency）。
func (c sqlColumn) charLen() (int, bool) {
	if _, isVarchar := c.varcharLen(); isVarchar {
		return 0, false
	}
	m := charLenRe.FindStringSubmatch(strings.ToUpper(c.dataType))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// isChar3 保留与 payment 同名的判定，便于两服务的测试互相对读。
func (c sqlColumn) isChar3() bool {
	n, ok := c.charLen()
	return ok && n == 3
}

// baseType 取列的主类型名（大写），无匹配返回空串。
func (c sqlColumn) baseType() string {
	m := baseTypeRe.FindStringSubmatch(strings.ToUpper(c.dataType))
	if m == nil {
		return ""
	}
	return m[1]
}

// hasCollation 只有字符列（CHAR/VARCHAR/TEXT/ENUM/SET）才有排序规则这一说。
// 整数列（mid、vip_type、state）即便参与唯一索引也不能要求登记 utf8mb4_bin
// —— payment 那边就出现过这条误判，本文件的 TestBinaryKeyRegistryIsCharOnlyAndComplete
// 会把「把整数列登记进 binaryKeys」直接判成失败。
func hasCollation(dataType string) bool {
	up := strings.ToUpper(dataType)
	for _, kw := range []string{"VARCHAR", "CHAR", "TEXT", "ENUM", "SET"} {
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

// structColumns 取行结构体的 db tag 列名（顺序即声明顺序）。
func structColumns(t *testing.T, row any) []string {
	t.Helper()
	v := reflect.TypeOf(row)
	if v == nil || v.Kind() != reflect.Struct {
		t.Fatalf("%T 不是结构体", row)
	}
	out := make([]string, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		tag := v.Field(i).Tag.Get("db")
		if tag == "" {
			t.Fatalf("%s 的字段 %s 缺少 db tag —— sqlx 按 tag 扫描，缺 tag 就是错位", v.Name(), v.Field(i).Name)
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	return out
}

func splitSelectColumns(t *testing.T, cols string) []string {
	t.Helper()
	parts := strings.Split(cols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		name := strings.Trim(strings.TrimSpace(p), "`")
		if name == "" {
			t.Fatalf("列常量里有空列名: %q", cols)
		}
		out = append(out, name)
	}
	return out
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// --- model 源码里的 SQL 文本（INSERT 列清单可能被拆成多段字面量拼接，必须归一化）---

func readModelSource(t *testing.T, spec tableSpec) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(currentDir(t), spec.goFile))
	if err != nil {
		t.Fatalf("读 model 源文件 %s 失败：%v", spec.goFile, err)
	}
	return string(raw)
}

func currentDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	return dir
}

var reGoStringLit = regexp.MustCompile(`"((?:[^"\\\n]|\\.)*)"`)

// modelSQLText 把某个 model 文件里的字符串字面量按出现顺序拼成归一文本。
// 必须这样做而不是对源码做子串比对：Go 里一条长 SQL 会被拆成 "..." + "..." 跨行拼接。
func modelSQLText(t *testing.T, spec tableSpec) string {
	t.Helper()
	var sb strings.Builder
	for _, m := range reGoStringLit.FindAllStringSubmatch(readModelSource(t, spec), -1) {
		sb.WriteString(m[1])
		sb.WriteString(" ")
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// insertColumns 从 model 的 INSERT 片段里抠出写入列清单。
func insertColumns(t *testing.T, spec tableSpec) map[string]struct{} {
	t.Helper()
	src := modelSQLText(t, spec)
	idx := strings.Index(src, spec.insertFrag)
	if idx < 0 {
		t.Fatalf("model 的 SQL 里找不到 %q", spec.insertFrag)
	}
	rest := src[idx+len(spec.insertFrag):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("%s 的 INSERT 列清单没闭合", spec.table)
	}
	out := map[string]struct{}{}
	for _, p := range strings.Split(rest[:end], ",") {
		p = strings.Trim(strings.TrimSpace(p), "`")
		if p == "" {
			t.Fatalf("%s 的 INSERT 列清单里有空列：%q", spec.table, rest[:end])
		}
		out[p] = struct{}{}
	}
	return out
}

// --- 门禁 1：表覆盖与双向列比对 ---

func TestMigrationFilesCoverEveryTable(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		if _, ok := tables[spec.table]; !ok {
			t.Errorf("迁移里没有建表语句 %s（model 有对应结构体）", spec.table)
		}
	}
	for name := range tables {
		if !strings.HasPrefix(name, "mb_") {
			t.Errorf("表 %s 不符合 membership 的 mb_ 前缀约定，与其他服务的表混在一份迁移里", name)
		}
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

// TestModelColumnsMatchMigration 双向比对：结构体有的列 SQL 必须有（否则 Unknown column），
// SQL 有的列结构体必须承载（否则该列永远读不到，成了无人认领的死列）。
func TestModelColumnsMatchMigration(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
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
				t.Fatalf("列数不符：结构体 %d 列，SQL %d 列", len(fields), len(tbl.columns))
			}
			// 列顺序一致：列常量是 SELECT 的一部分，DDL 与结构体顺序分叉说明有人单边插列。
			for i := range fields {
				if fields[i] != tbl.columns[i].name {
					t.Errorf("第 %d 列顺序不符：结构体 %s，DDL %s（逐列一致才算双向比对）",
						i+1, fields[i], tbl.columns[i].name)
				}
			}
			// 全列 NOT NULL：model 用普通结构体扫描（无 sql.Null*），读到 NULL 直接失败。
			for _, c := range tbl.columns {
				if !c.notNull {
					t.Errorf("%s.%s 允许 NULL —— model 无 sql.Null* 包装，扫描会在运行期炸",
						spec.table, c.name)
				}
			}
			for _, banned := range spec.mustNotHaveColumns {
				if tbl.hasColumn(banned) {
					t.Errorf("%s 出现了列 %s —— 与本表的判定/台账口径冲突（见 spec 注释）", spec.table, banned)
				}
			}
		})
	}
}

// TestSelectColumnConstantsMatchSQL 钉住「SELECT 列常量 = 结构体字段，且同序」。
// 顺序不一致会把 mid 扫成 price_minor，是静默的结论污染。
func TestSelectColumnConstantsMatchSQL(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			cols := splitSelectColumns(t, spec.selectColumns)
			want := structColumns(t, spec.row)
			if len(cols) != len(want) {
				t.Fatalf("列常量 %d 列，结构体 %d 列 —— 少一列就会静默扫出零值", len(cols), len(want))
			}
			got := map[string]struct{}{}
			for _, c := range cols {
				if _, dup := got[c]; dup {
					t.Fatalf("列常量里 %s 重复", c)
				}
				got[c] = struct{}{}
				if !tbl.hasColumn(c) {
					t.Errorf("SELECT 列 %s 不在 %s 的建表语句里", c, spec.table)
				}
			}
			for i, c := range cols {
				if c != want[i] {
					t.Fatalf("第 %d 列不符：SELECT 常量 %q，结构体字段 %q —— 顺序不一致会导致字段扫描错位",
						i+1, c, want[i])
				}
			}
			if cols[0] != spec.primaryKey[0] {
				t.Errorf("%s 的 SELECT 常量应以主键 %s 开头，得到 %q", spec.table, spec.primaryKey[0], cols[0])
			}
		})
	}
}

// TestColumnTypesMatchGoFields 整型宽度必须与 Go 字段匹配：
// 窄列配宽字段会静默溢出，宽列配窄字段则扫描报错。
func TestColumnTypesMatchGoFields(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			rv := reflect.TypeOf(spec.row)
			for i := 0; i < rv.NumField(); i++ {
				name := rv.Field(i).Tag.Get("db")
				kind := rv.Field(i).Type.Kind()
				col, ok := tbl.column(name)
				if !ok {
					t.Fatalf("结构体 %s.%s 在建表语句里没有同名列", rv.Name(), name)
				}
				checked++
				up := strings.ToUpper(col.dataType)
				switch kind {
				case reflect.Int64:
					if col.baseType() != "BIGINT" {
						t.Errorf("%s.%s 是 int64 但列是 %q（超 21 亿会溢出成负数）", spec.table, name, col.dataType)
					}
				case reflect.Int32:
					switch col.baseType() {
					case "INT", "TINYINT", "SMALLINT", "MEDIUMINT":
					default:
						t.Errorf("%s.%s 是 int32 但列是 %q", spec.table, name, col.dataType)
					}
				case reflect.Uint32:
					// 位掩码用无符号列：signed INT 存 1<<31 会变负数，platform_mask 判定直接错。
					if col.baseType() != "INT" || !strings.Contains(up, "UNSIGNED") {
						t.Errorf("%s.%s 是 uint32，列必须是 INT UNSIGNED，得到 %q", spec.table, name, col.dataType)
					}
				case reflect.String:
					switch col.baseType() {
					case "VARCHAR", "CHAR", "TEXT":
					default:
						t.Errorf("%s.%s 是 string 但列是 %q", spec.table, name, col.dataType)
					}
				default:
					t.Errorf("%s.%s 用了 sqlx 不便处理的类型 %s（本服务不用 sql.Null*）",
						rv.Name(), name, kind)
				}
			}
		})
	}
	if checked < 79 {
		t.Fatalf("只比对了 %d 个字段类型，说明列口径被改动或解析器退化", checked)
	}
}

// --- 门禁 2：注释口径（单位 + 禁止浮点金额）---

// TestMoneyAndDurationColumnsDocumentTheirUnits 单位必须显式写在列注释里：
// price_minor 是「分」还是「元」、duration_days 是「天」还是「秒」，
// 只看列名和 int64 都推不出来，猜错一次就是给全量用户改口径。
func TestMoneyAndDurationColumnsDocumentTheirUnits(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var money, duration int
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		for _, c := range tbl.columns {
			lower := strings.ToLower(c.name)
			up := strings.ToUpper(c.dataType)
			// 本服务没有任何浮点/定点列：标价与时长都是整数最小单位。
			for _, banned := range []string{"FLOAT", "DOUBLE", "DECIMAL", "NUMERIC"} {
				if strings.Contains(up, banned) {
					t.Errorf("%s.%s 用了浮点/定点列 %q —— 金额与时长必须是整数最小单位",
						spec.table, c.name, c.dataType)
				}
			}
			isMoney := strings.HasSuffix(lower, "_minor")
			isDays := strings.HasSuffix(lower, "_days") || strings.Contains(lower, "duration_days")
			isMonths := strings.Contains(lower, "month")
			switch {
			case isMoney:
				money++
				if !strings.Contains(up, "BIGINT") {
					t.Errorf("%s.%s 金额列必须是 BIGINT（分），得到 %q", spec.table, c.name, c.dataType)
				}
				if !strings.Contains(c.comment, "分") {
					t.Errorf("%s.%s 是金额列但注释没写明单位是分：%q", spec.table, c.name, c.comment)
				}
			case isDays:
				duration++
				if !strings.Contains(c.comment, "天") {
					t.Errorf("%s.%s 是天数列但注释没写明单位是天：%q", spec.table, c.name, c.comment)
				}
			case isMonths:
				duration++
				if !strings.Contains(c.comment, "月") {
					t.Errorf("%s.%s 是月数列但注释没写明单位是月：%q", spec.table, c.name, c.comment)
				}
			}
		}
	}
	if money < 6 {
		t.Fatalf("只比对到 %d 个金额列（*_minor 是唯一命名约定），说明列名口径被改动", money)
	}
	if duration < 3 {
		t.Fatalf("只比对到 %d 个时长列（*_days / *month*），说明列名口径被改动", duration)
	}
}

// TestEveryColumnCommentIsNonEmpty 解析器已对缺 COMMENT 直接 Fatalf；
// 这里再钉一条「注释不能是空话」，防止有人用 COMMENT ” 或复制列名糊过去。
func TestEveryColumnCommentIsNonEmpty(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var n int
	for _, spec := range tableSpecs() {
		for _, c := range mustTable(t, tables, spec.table).columns {
			n++
			comment := strings.TrimSpace(c.comment)
			if len([]rune(comment)) < 4 {
				t.Errorf("%s.%s 的注释过短：%q —— 至少要说明取值或单位", spec.table, c.name, comment)
			}
			if strings.EqualFold(comment, c.name) {
				t.Errorf("%s.%s 的注释就是列名本身，等于没写口径", spec.table, c.name)
			}
			// 台账/目录里出现凭据与 PII 是 AGENTS.md §7 的红线，注释要写明禁止写入。
			if (c.name == "reason") && !strings.Contains(comment, "PII") {
				t.Errorf("%s.%s 是自由文本台账列，注释必须写明禁止写入凭据与 PII：%q", spec.table, c.name, comment)
			}
		}
	}
	if n < 79 {
		t.Fatalf("只检查了 %d 列的注释，解析器可能退化（六张表应至少 79 列）", n)
	}
}

// --- 门禁 3：唯一性判定列的二进制排序规则 ---

// TestUniqueKeyCharColumnsUseBinaryCollation 唯一索引/主键里的**字符列**必须登记在
// binaryKeys，并且真的声明了 COLLATE utf8mb4_bin；双向都要，缺一即漏。
func TestUniqueKeyCharColumnsUseBinaryCollation(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			// 正向：登记过的列必须真的声明 utf8mb4_bin。
			for _, name := range spec.binaryKeys {
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, name)
				}
				if !strings.Contains(c.dataType, "COLLATE utf8mb4_bin") {
					t.Errorf("%s.%s 参与唯一性/等值鉴别但没声明 COLLATE utf8mb4_bin：%q —— "+
						"表级是 unicode_ci，会把仅大小写不同的两个 request_id 判成同一个键",
						spec.table, name, c.dataType)
				}
			}
			// 反向：唯一锚点（UNIQUE KEY 与主键）里的字符列都必须登记。
			// 整数列（mid、vip_type）跳过：它们没有排序规则可言。
			for _, idx := range tbl.uniqueIndexes() {
				for _, col := range idx.cols {
					c, _ := tbl.column(col) // 列存在性已由 addIndex 钉死
					if !hasCollation(c.dataType) {
						continue
					}
					if !containsString(spec.binaryKeys, col) {
						t.Errorf("%s 的唯一锚点 %s 用了字符列 %s，但 spec.binaryKeys 未登记其排序规则要求",
							spec.table, idx.name, col)
					}
				}
			}
			// 表内出现未登记的 utf8mb4_bin 列 = 有人单边改了排序规则。
			for _, c := range tbl.columns {
				if strings.Contains(c.dataType, "COLLATE utf8mb4_bin") && !containsString(spec.binaryKeys, c.name) {
					t.Errorf("%s.%s 声明了 utf8mb4_bin 却不在 binaryKeys 里：%q",
						spec.table, c.name, c.dataType)
				}
			}
		})
	}
}

// TestBinaryKeyRegistryIsCharOnlyAndComplete 把 payment 那边出现过的误判钉死：
// mid / vip_type / state 这类整数列虽然也在唯一索引上，但没有排序规则，
// 把它们登记进 binaryKeys 会让门禁变噪音，必须报错。
func TestBinaryKeyRegistryIsCharOnlyAndComplete(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var registered int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for _, name := range spec.binaryKeys {
				registered++
				c, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, name)
				}
				if !hasCollation(c.dataType) {
					t.Errorf("%s.%s 不是字符列（%q），不得登记进 binaryKeys —— 整数列没有排序规则可言",
						spec.table, name, c.dataType)
				}
			}
		})
	}
	if registered < 15 {
		t.Fatalf("binaryKeys 只登记了 %d 列，低于实际声明 utf8mb4_bin 的 15 列，说明清单被删过", registered)
	}
}

// --- 门禁 4：主键、唯一键与普通索引 ---

func TestIdempotencyKeysAreUniqueInSQL(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			if strings.Join(tbl.primary, ",") != strings.Join(spec.primaryKey, ",") {
				t.Errorf("主键不符：SQL %v，期望 %v", tbl.primary, spec.primaryKey)
			}
			for name, cols := range spec.uniqueKeys {
				idx, ok := tbl.index(name)
				if !ok {
					t.Fatalf("%s 缺少唯一索引 %s —— 幂等失去数据库兜底，同一请求会重复改权益", spec.table, name)
				}
				if !idx.unique {
					t.Fatalf("%s 的 %s 必须是 UNIQUE KEY，普通 KEY 起不到去重作用", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("唯一索引 %s 列不符：SQL %v，期望 %v（顺序敏感）", name, idx.cols, cols)
				}
			}
			// 反向：SQL 里出现未声明的唯一键，会让某条正常写入路径开始报 1062。
			for _, idx := range tbl.indexes {
				if !idx.unique {
					continue
				}
				if _, ok := spec.uniqueKeys[idx.name]; !ok {
					t.Errorf("%s 出现未声明的唯一索引 %s（列 %v）", spec.table, idx.name, idx.cols)
				}
			}
			// 每张表都必须有唯一性锚点：UNIQUE KEY，或主键本身就是幂等键。
			if len(spec.uniqueKeys) == 0 && !spec.pkIsIdempotencyKey {
				t.Errorf("%s 没有唯一键 —— 写接口无法靠数据库去重", spec.table)
			}
			if spec.pkIsIdempotencyKey {
				if strings.Join(tbl.primary, ",") != "request_id" {
					t.Errorf("%s 声明了「主键即幂等键」，但主键是 %v", spec.table, tbl.primary)
				}
			}
		})
	}
}

func TestQueryIndexesExist(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var required int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for name, cols := range spec.requiredIndexes {
				required++
				idx, ok := tbl.index(name)
				if !ok {
					t.Fatalf("%s 缺少索引 %s（列 %v）—— model 的 WHERE/ORDER BY 会退化成全表扫描",
						spec.table, name, cols)
				}
				if idx.unique {
					t.Errorf("%s 的 %s 被建成了 UNIQUE KEY —— 巡检/分页索引不允许承担唯一性约束", spec.table, name)
				}
				if strings.Join(idx.cols, ",") != strings.Join(cols, ",") {
					t.Errorf("索引 %s 列不符：SQL %v，期望 %v", name, idx.cols, cols)
				}
			}
			// 索引引用的列必须存在（parser 已钉），这里补一条：主键必须存在。
			if len(tbl.primary) == 0 {
				t.Errorf("%s 没有主键", spec.table)
			}
		})
	}
	if required < 13 {
		t.Fatalf("只比对了 %d 个查询索引，低于预期 13 个，说明 spec.requiredIndexes 被删过", required)
	}
}

// --- 门禁 5：数值/字符串枚举注释 ---

func TestEnumColumnsDocumentEveryValue(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		for column, values := range spec.enumComments {
			checked++
			t.Run(spec.table+"."+column, func(t *testing.T) {
				col, ok := mustTable(t, tables, spec.table).column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				// 枚举列必须是窄整型：BIGINT 存枚举是浪费（也是「拿它当业务主键用」的信号），
				// 浮点/字符列存枚举则说明口径已经从库里消失。
				// INT 只允许出现在无符号位掩码列（platform_mask）上。
				switch bt := col.baseType(); bt {
				case "TINYINT", "SMALLINT":
				case "INT", "MEDIUMINT":
					if !strings.Contains(strings.ToUpper(col.dataType), "UNSIGNED") {
						t.Errorf("%s.%s 是枚举列却是 %q —— 整型枚举请用 TINYINT，位掩码请用 UNSIGNED",
							spec.table, column, col.dataType)
					}
				default:
					t.Errorf("%s.%s 应为 TINYINT（或无符号整型位掩码）枚举列，得到 %q", spec.table, column, col.dataType)
				}
				tokens := map[string]struct{}{}
				for _, n := range numericTokenRe.FindAllString(col.comment, -1) {
					tokens[n] = struct{}{}
				}
				for _, v := range values {
					if _, ok := tokens[strconv.FormatInt(v, 10)]; !ok {
						t.Errorf("%s.%s 的注释没说明取值 %d（当前注释 %q）—— 新增枚举值必须同步建表注释",
							spec.table, column, v, col.comment)
					}
				}
			})
		}
	}
	// 完整性：每个 TINYINT 列都必须是登记的枚举列，否则「新增一个 TINYINT 列」
	// 会绕过本门禁（列名千奇百怪，只有类型是稳定的判定依据）。
	for _, spec := range tableSpecs() {
		for _, c := range mustTable(t, tables, spec.table).columns {
			if c.baseType() == "TINYINT" {
				if _, ok := spec.enumComments[c.name]; !ok {
					t.Errorf("%s.%s 是 TINYINT 却没登记进 enumComments：%q", spec.table, c.name, c.comment)
				}
			}
		}
	}
	if checked < 14 {
		t.Fatalf("只比对了 %d 个数值枚举列，低于预期 14 个", checked)
	}
}

// TestStringEnumColumnsDocumentEveryValue 取值被契约钉死的字符串列（action/change_type/api/currency）
// 必须在注释里逐个写明：库里存的是文本，没有注释就没法判断 'EXPIRE' 是不是合法值。
func TestStringEnumColumnsDocumentEveryValue(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		for column, values := range spec.stringEnums {
			checked++
			t.Run(spec.table+"."+column, func(t *testing.T) {
				col, ok := mustTable(t, tables, spec.table).column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				for _, v := range values {
					if !strings.Contains(col.comment, v) {
						t.Errorf("%s.%s 的注释没写明取值 %q（当前注释 %q）", spec.table, column, v, col.comment)
					}
				}
			})
		}
	}
	if checked < 4 {
		t.Fatalf("只比对了 %d 个字符串枚举列，低于预期 4 个", checked)
	}
}

// TestContractConstantsKeepTheirValues 常量值本身不能漂：
// 库里已有数据按这些数字解释，重排一次就让历史行变成另一种含义。
func TestContractConstantsKeepTheirValues(t *testing.T) {
	if VipTypePremium != 1 || VipTypePremiumPlus != 2 {
		t.Error("档位常量被重排：mb_plan/mb_membership/mb_grant 的 vip_type 历史数据全部失去含义")
	}
	if PlanStateDraft != 1 || PlanStateOnSale != 2 || PlanStateOffSale != 3 {
		t.Error("套餐状态常量被重排：mb_plan.state 与 mb_plan_change_log 的前后态都会错位")
	}
	sources := []int32{GrantSourceSandboxPurchase, GrantSourceSandboxAutoRenew,
		GrantSourceAdminOps, GrantSourceExperience, GrantSourceLegacyImport}
	for i, v := range sources {
		if v != int32(i+1) {
			t.Errorf("GrantSource 取值 %d 不在 1..5 连续序列的第 %d 位（留空洞=历史值被删）", v, i+1)
		}
	}
	if !ValidGrantSource(GrantSourceLegacyImport) || ValidGrantSource(6) || ValidGrantSource(0) {
		t.Error("ValidGrantSource 的边界与 mb_grant.source 注释的取值集合不一致")
	}
	bits := []uint32{PlatformBitAndroid, PlatformBitIOS, PlatformBitHarmony, PlatformBitDesktop, PlatformBitWeb}
	for i, b := range bits {
		if b != 1<<uint(i) {
			t.Errorf("平台位 %d 不是 1<<%d（等于 %d）：mb_plan.platform_mask 注释的位值表与 mask 判定都会错位", b, i, 1<<uint(i))
		}
	}
	if len(PlatformsOfMask(0)) != 0 || len(PlatformsOfMask(31)) != 5 {
		t.Error("PlatformsOfMask 与 mb_plan.platform_mask 注释的位值集合不一致")
	}
	actions := []string{ActionGrant, ActionExtend, ActionRevoke, ActionExpire}
	for i := range actions {
		for j := i + 1; j < len(actions); j++ {
			if actions[i] == actions[j] {
				t.Errorf("动作常量 %s 重复", actions[i])
			}
		}
	}
}

// --- 门禁 6：乐观锁与时间列 ---

// TestVersionAndTimeColumnsMatchWriteModel CAS 表必须有 version BIGINT；
// 只追加台账（变更史/授予台账/幂等台账）不得有 version 与 mtime ——
// 有它们就意味着存在「原地改写历史」的入口，而台账的纠正方式是再追加一行。
func TestVersionAndTimeColumnsMatchWriteModel(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			c, hasVersion := tbl.column("version")
			if spec.needsVersion {
				if !hasVersion {
					t.Fatalf("%s 缺少 version 列 —— 写接口没有乐观锁，并发授予会互相覆盖到期时间", spec.table)
				}
				if c.baseType() != "BIGINT" {
					t.Errorf("%s.version 必须是 BIGINT，得到 %q", spec.table, c.dataType)
				}
			} else if !spec.appendOnly && hasVersion {
				t.Errorf("%s 出现了 version 列但 spec.needsVersion=false：要么补 CAS 断言，要么删列", spec.table)
			}
			if spec.appendOnly {
				if hasVersion {
					t.Errorf("%s 是只追加台账却有 version 列 —— 台账不存在原地改写", spec.table)
				}
				if tbl.hasColumn("mtime") {
					t.Errorf("%s 出现了 mtime —— 只追加台账不得原地改写", spec.table)
				}
				if _, ok := tbl.column("ctime"); !ok {
					t.Errorf("%s 缺少 ctime —— 台账的主时间列必须是创建时间（Unix 秒）", spec.table)
				}
			} else if !tbl.hasColumn("mtime") {
				t.Errorf("%s 是可改写的目录/身份表却没有 mtime，改动无法审计", spec.table)
			}
			for _, name := range spec.timeColumns {
				tc, ok := tbl.column(name)
				if !ok {
					t.Fatalf("%s 缺少时间列 %s", spec.table, name)
				}
				if tc.baseType() != "BIGINT" {
					t.Errorf("%s.%s 必须是 BIGINT（Unix 秒），得到 %q —— 改成 DATETIME 会让 int64 比较全部错位",
						spec.table, name, tc.dataType)
				}
				// 到期/签约口径全靠注释表达（AGENTS.md §1：expire_at <= now 即已过期）。
				if !strings.Contains(tc.comment, "Unix 秒") && !strings.Contains(tc.comment, "时间") {
					t.Errorf("%s.%s 是时间列但注释没说明是 Unix 秒：%q", spec.table, name, tc.comment)
				}
			}
			// 完整性：任何 *_at / ctime / mtime 列都必须登记，新增时间列不进来就是漏检。
			for _, col := range tbl.columns {
				lower := strings.ToLower(col.name)
				if strings.HasSuffix(lower, "_at") || lower == "ctime" || lower == "mtime" {
					if !containsString(spec.timeColumns, col.name) {
						t.Errorf("%s.%s 是时间列却没登记进 timeColumns", spec.table, col.name)
					}
				}
				if bt := col.baseType(); bt == "DATETIME" || bt == "TIMESTAMP" || bt == "DATE" {
					t.Errorf("%s.%s 用了 %s 列 —— 本服务时间一律 BIGINT Unix 秒", spec.table, col.name, bt)
				}
			}
		})
	}
}

// TestGrantLedgerIsTheOnlyEntitlementEvidence 授予台账是「权益是否生效」的唯一凭据：
// 撤销要写 action=REVOKE 的行，不允许删行或改判口径。
func TestGrantLedgerIsTheOnlyEntitlementEvidence(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	grant := mustTable(t, tables, "mb_grant")
	// 判定链路依赖的列必须都在（缺一个就要靠 JOIN 别的表，而本服务不建外键）。
	for _, name := range []string{"request_id", "action", "delta_days", "before_expire_at", "after_expire_at", "source", "biz_order_no", "payment_no"} {
		if !grant.hasColumn(name) {
			t.Errorf("mb_grant 缺少列 %s —— 授予判定/审计断链", name)
		}
	}
	if _, ok := grant.column("balance_minor"); ok {
		t.Error("mb_grant 出现了资金列 —— 本服务不持有资金，钱在 payment（AGENTS.md §5）")
	}
	m := mustTable(t, tables, "mb_membership")
	if _, ok := m.column("state"); ok {
		t.Error("mb_membership 出现了 state 列 —— 会员状态只能由 expire_at 与 now 比较得出")
	}
}

// --- 门禁 7：列宽 vs 代码上限 ---

// TestTextLimitsFitColumnWidth model/logic 侧的字符上限必须不超过列宽。
// 反例：MaxPlanCodeLength 从 64 涨到 80 而 plan_code 仍是 VARCHAR(64)
// —— 校验放过、写入报 1406，而且错误发生在已经写完台账的事务里。
func TestTextLimitsFitColumnWidth(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var checked int
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			for column, limit := range spec.textLimits {
				checked++
				c, ok := tbl.column(column)
				if !ok {
					t.Fatalf("%s 缺少列 %s", spec.table, column)
				}
				width, isVarchar := c.varcharLen()
				if !isVarchar {
					n, isChar := c.charLen()
					if !isChar {
						t.Fatalf("%s.%s 不是字符列，无法与上限比对：%q", spec.table, column, c.dataType)
					}
					if n < limit {
						t.Errorf("%s.%s 列宽 CHAR(%d) 小于上限 %d 字符", spec.table, column, n, limit)
					}
					continue
				}
				if width < limit {
					t.Errorf("%s.%s 列宽 VARCHAR(%d) 小于 model/logic 允许的 %d 字符 —— 校验放过而写入报错",
						spec.table, column, width, limit)
				}
				if width > limit {
					t.Logf("%s.%s 列宽 VARCHAR(%d) 比上限 %d 宽（允许，但不能更窄）", spec.table, column, width, limit)
				}
			}
		})
	}
	if checked < 27 {
		t.Fatalf("只比对了 %d 个字符列上限，低于全服务字符列总数 27，说明清单被删过", checked)
	}
}

// TestEveryCharColumnHasDeclaredLimit 防止「加了列忘了定上限」：
// textLimits 是手工清单，新增 VARCHAR/CHAR 列不进来就等于留了个校验缺口。
func TestEveryCharColumnHasDeclaredLimit(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	var n int
	for _, spec := range tableSpecs() {
		for _, c := range mustTable(t, tables, spec.table).columns {
			if _, ok := c.varcharLen(); !ok {
				if _, ok := c.charLen(); !ok {
					continue
				}
			}
			n++
			if _, ok := spec.textLimits[c.name]; !ok {
				t.Errorf("%s.%s 是字符列但没声明长度上限：要么在 logic 里按 rune 校验，要么在 spec 里写明为什么不校验",
					spec.table, c.name)
			}
		}
	}
	if n < 27 {
		t.Fatalf("只扫到 %d 个字符列，低于预期 27 个，说明解析器退化", n)
	}
}

// TestStringEnumValuesFitColumnWidth 契约钉死的字符串取值必须装得进自己的列。
// 这些常量是「新增即改契约」的取值，列宽比取值窄 = 新取值永远写不进去。
func TestStringEnumValuesFitColumnWidth(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	cases := []struct {
		table, column string
		values        []string
	}{
		{"mb_grant", "action", []string{ActionGrant, ActionExtend, ActionRevoke, ActionExpire}},
		{"mb_plan_change_log", "change_type", []string{PlanChangeUpsert, PlanChangeState}},
		{"mb_biz_request", "api", []string{ApiSetAutoRenew, ApiUpsertEntitlement}},
		{"mb_plan", "currency", []string{"CNY"}},
		{"mb_membership", "auto_renew_channel", []string{AllowedAutoRenewChannel}},
	}
	for _, c := range cases {
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			col, ok := mustTable(t, tables, c.table).column(c.column)
			if !ok {
				t.Fatalf("%s 缺少列 %s", c.table, c.column)
			}
			for _, v := range c.values {
				width, isChar := col.varcharLen()
				if !isChar {
					width, isChar = col.charLen()
				}
				if !isChar {
					t.Fatalf("%s.%s 不是字符列：%q", c.table, c.column, col.dataType)
				}
				if len(v) > width {
					t.Errorf("%s.%s 列宽 %d 装不下取值 %q（%d 字符）", c.table, c.column, width, v, len(v))
				}
			}
		})
	}
}

// TestSubjectKeyFitsColumnWidth mb_biz_request.subject 是拼接键，
// 最坏长度必须按真实的 int64 上界算，而不是按测试里的小 mid 算。
func TestSubjectKeyFitsColumnWidth(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	// 与 internal/logic/guard.go 的 membershipSubject 同式；
	// TestLogicSubjectFormatStillMatches 会去读 logic 源码钉住这个格式。
	worst := fmt.Sprintf("mid:%d:vip:%d", int64(^uint64(0)>>1), int32(99))

	col, ok := mustTable(t, tables, "mb_biz_request").column("subject")
	if !ok {
		t.Fatal("mb_biz_request 缺少 subject 列")
	}
	width, isVarchar := col.varcharLen()
	if !isVarchar {
		t.Fatalf("subject 必须是 VARCHAR，得到 %q", col.dataType)
	}
	if len(worst) > width {
		t.Errorf("subject 列宽 VARCHAR(%d) 装不下最坏键 %q（%d 字符）", width, worst, len(worst))
	}
	// 权益码也会直接作为 subject 落库，所以码长上限也不能超过它。
	if MaxEntitlementCodeLength > width {
		t.Errorf("subject 列宽 %d 小于 MaxEntitlementCodeLength=%d", width, MaxEntitlementCodeLength)
	}
}

// TestFingerprintWidthMatchesColumn params_fingerprint 是 CHAR(64)：
// model.Fingerprint 返回 sha256 hex，长度变了就意味着比对逻辑与列宽分叉。
func TestFingerprintWidthMatchesColumn(t *testing.T) {
	got := Fingerprint("SetAutoRenew", "mid:1:vip:1", "true")
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range []struct{ table, column string }{
		{"mb_plan_change_log", "params_fingerprint"},
		{"mb_biz_request", "params_fingerprint"},
	} {
		col, ok := mustTable(t, tables, spec.table).column(spec.column)
		if !ok {
			t.Fatalf("%s 缺少列 %s", spec.table, spec.column)
		}
		width, isChar := col.charLen()
		if !isChar {
			t.Fatalf("%s.%s 必须是定宽 CHAR（补齐空格会破坏比对），得到 %q", spec.table, spec.column, col.dataType)
		}
		if width != len(got) {
			t.Errorf("%s.%s 宽 %d，但 Fingerprint() 产出 %d 字符 —— 幂等冲突判定会因截断失效",
				spec.table, spec.column, width, len(got))
		}
		if !strings.Contains(col.comment, "sha256") {
			t.Errorf("%s.%s 注释没写明指纹算法：%q", spec.table, spec.column, col.comment)
		}
	}
}

// TestCurrencyColumnIsFixedChar3 币种必须 CHAR(3)：换成 VARCHAR(8) 后
// "CNY " 与 "CNY" 会被当成两个币种，条件更新的 WHERE 直接失配。
func TestCurrencyColumnIsFixedChar3(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	c, ok := mustTable(t, tables, "mb_plan").column("currency")
	if !ok {
		t.Fatal("mb_plan 缺少 currency 列：标价必须随行显式记录币种")
	}
	if !c.isChar3() {
		t.Errorf("mb_plan.currency 必须是 CHAR(3)，得到 %q", c.dataType)
	}
	if !strings.Contains(c.dataType, "utf8mb4_bin") {
		t.Errorf("mb_plan.currency 必须显式 COLLATE utf8mb4_bin，得到 %q", c.dataType)
	}
	// 标价列与币种同表，缺币种就没法解释 price_minor。
	if _, ok := mustTable(t, tables, "mb_plan").column("price_minor"); !ok {
		t.Error("mb_plan 缺少 price_minor —— 标价与币种必须同表，否则口径要跨表猜")
	}
}

// --- 门禁 8：写入路径与严格模式 ---

// TestNotNullColumnsHaveWritePath NOT NULL 且无默认值的列若不被 INSERT 写，
// 严格模式下必然报 1364：能建表、能读，但台账永远写不进去。
func TestNotNullColumnsHaveWritePath(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		t.Run(spec.table, func(t *testing.T) {
			tbl := mustTable(t, tables, spec.table)
			inInsert := insertColumns(t, spec)
			for _, c := range tbl.columns {
				if _, ok := inInsert[c.name]; ok {
					continue
				}
				if c.autoIncrement || c.hasDefault {
					continue // 自增主键与靠默认值填充的列允许省略
				}
				if c.notNull {
					t.Errorf("%s.%s NOT NULL 且无默认值，但 model 的 INSERT 不写它", spec.table, c.name)
				}
			}
			// 反向：INSERT 不得写建表语句里不存在的列。
			for col := range inInsert {
				if !tbl.hasColumn(col) {
					t.Errorf("model 的 INSERT 写了 %s 不存在的列 %s", spec.table, col)
				}
			}
		})
	}
}

// TestCasWritePathsFilterOnVersion 乐观锁的 WHERE 必须真的带 version 条件；
// 去掉它，两个并发授予会各自读到同一快照并互相覆盖到期时间。
func TestCasWritePathsFilterOnVersion(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		tbl := mustTable(t, tables, spec.table)
		if _, ok := tbl.column("version"); !ok {
			continue
		}
		t.Run(spec.table, func(t *testing.T) {
			src := modelSQLText(t, spec)
			if !strings.Contains(src, "version = version + 1") {
				t.Errorf("%s 的写路径没有 version = version + 1 —— CAS 位不会推进", spec.table)
			}
			if !strings.Contains(src, "AND version = ?") {
				t.Errorf("%s 的 UPDATE 没有 WHERE ... AND version = ? —— 乐观锁失效", spec.table)
			}
		})
	}
}

// TestLogicGuardsUsePlaceholdersAndColumnLimits 把 logic 侧的两处口径钉在库上：
//  1. request_id 的 rune 上限必须等于三个 request_id 列的列宽（一处单边改动即红）；
//  2. membershipSubject 的格式串必须与本测试算最坏长度时用的式子一致；
//  3. 过滤值一律走占位符，不允许把值拼进 SQL 文本（注入面）。
func TestLogicGuardsUsePlaceholdersAndColumnLimits(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(findRepoRoot(t), "services", "membership", "internal", "logic", "guard.go"))
	if err != nil {
		t.Fatalf("读 internal/logic/guard.go 失败：%v（本门禁依赖它，不能跳过）", err)
	}
	src := string(raw)

	m := regexp.MustCompile(`maxRequestIdLen\s*=\s*(\d+)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("guard.go 里找不到 maxRequestIdLen 常量：request_id 上限的口径来源变了")
	}
	limit, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("解析 maxRequestIdLen 失败: %v", err)
	}
	if limit != maxRequestIdCharsInLogic {
		t.Errorf("guard.go 的 maxRequestIdLen = %d，与钉住的 %d 不符", limit, maxRequestIdCharsInLogic)
	}

	tables := parseCreateTables(t, findMigrationsDir(t))
	for _, spec := range tableSpecs() {
		c, ok := mustTable(t, tables, spec.table).column("request_id")
		if !ok {
			continue
		}
		width, isVarchar := c.varcharLen()
		if !isVarchar {
			t.Fatalf("%s.request_id 必须是 VARCHAR，得到 %q", spec.table, c.dataType)
		}
		if width != limit {
			t.Errorf("%s.request_id 列宽 VARCHAR(%d) 与 logic 的 maxRequestIdLen %d 不等：校验与库必须同一把尺",
				spec.table, width, limit)
		}
	}

	if !strings.Contains(src, `"mid:%d:vip:%d"`) {
		t.Error("guard.go 的 membershipSubject 格式串变了：TestSubjectKeyFitsColumnWidth 的最坏长度算式需同步")
	}
	if m := regexp.MustCompile(`fmt\.Sprintf\([^)]*(?:WHERE|request_id\s*=\s*'|mid\s*=\s*\d)`).FindString(src); m != "" {
		t.Errorf("logic 疑似把值拼进了 SQL 文本：%s", m)
	}
}

// --- 门禁 9：迁移卫生 ---

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
			for _, marker := range []string{"数据所有者", "回滚", "幂等"} {
				if !strings.Contains(text, marker) {
					t.Errorf("迁移头注释缺少 %q 段落（会员身份与台账的口径、回滚必须写清）", marker)
				}
			}
			if !strings.Contains(text, "go_video_membership") {
				t.Error("迁移未声明所属库 go_video_membership，无法确认数据所有权（AGENTS.md §5）")
			}
			for _, banned := range []string{"TRUNCATE", "DROP DATABASE", "GRANT ", "FOREIGN KEY", "DELETE FROM", "CREATE DATABASE"} {
				if containsOutsideComments(text, banned) {
					t.Errorf("迁移里出现了禁止语句 %q", banned)
				}
			}
			// 服务只能写自己的库：表前缀与库名不能混用别的服务的命名。
			for _, other := range []string{"pm_", "cn_", "to_"} {
				if regexp.MustCompile("(?m)^\\s*CREATE TABLE IF NOT EXISTS `" + other).MatchString(text) {
					t.Errorf("迁移里出现了别的服务的表前缀 %s —— 数据所有权越界", other)
				}
			}
		})
	}
}

func containsOutsideComments(text, keyword string) bool {
	upper := strings.ToUpper(keyword)
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(strings.ToUpper(trimmed), upper) {
			return true
		}
	}
	return false
}

// TestNoCrossTableReferences 本服务不建外键：跨表只存主键值，
// 一致性由同一事务内的服务侧写入保证（AGENTS.md §5）。
func TestNoCrossTableReferences(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	for name, tbl := range tables {
		if tbl.referencedOtherTable {
			t.Errorf("%s 出现 REFERENCES 写法 —— 跨库/跨表外键会让迁移的锁窗口不可控", name)
		}
	}
}

// --- 门禁 10：解析器自检（防止测试空转发绿）---

// TestParserActuallyParsedTheSchema 对「实际比对到的表数/列数/字符列数/枚举列数」设下限。
// 解析器一旦与迁移写法脱节，其余断言会全部落在空集合上并安静通过。
func TestParserActuallyParsedTheSchema(t *testing.T) {
	tables := parseCreateTables(t, findMigrationsDir(t))
	if len(tables) < 6 {
		t.Fatalf("只解析出 %d 张表，membership 应有 6 张（mb_plan/mb_plan_change_log/mb_entitlement/"+
			"mb_membership/mb_grant/mb_biz_request）", len(tables))
	}
	var columns, charCols, binCols, tinyCols, versionCols int
	for _, tbl := range tables {
		if len(tbl.columns) < 9 {
			t.Fatalf("%s 只解析出 %d 列，说明建表写法与解析器脱节", tbl.table, len(tbl.columns))
		}
		columns += len(tbl.columns)
		for _, c := range tbl.columns {
			if _, ok := c.varcharLen(); ok {
				charCols++
			} else if _, ok := c.charLen(); ok {
				charCols++
			}
			if strings.Contains(c.dataType, "utf8mb4_bin") {
				binCols++
			}
			if c.baseType() == "TINYINT" {
				tinyCols++
			}
			if c.name == "version" {
				versionCols++
			}
			if c.baseType() == "" {
				t.Fatalf("%s.%s 的主类型无法识别：%q —— 解析器需要跟上新写法", tbl.table, c.name, c.dataType)
			}
		}
	}
	for _, want := range []struct {
		what string
		got  int
		min  int
	}{
		{"列", columns, 79},
		{"字符列", charCols, 27},
		{"utf8mb4_bin 列", binCols, 15},
		{"TINYINT 枚举列", tinyCols, 13},
		{"version 列", versionCols, 3},
	} {
		if want.got < want.min {
			t.Fatalf("实际比对到 %d 个%s，低于下限 %d：解析器退化会让其余门禁空转发绿",
				want.got, want.what, want.min)
		}
	}
	t.Logf("解析自检通过：%d 张表 / %d 列 / %d 字符列 / %d 二进制排序列 / %d TINYINT 枚举列 / %d version 列",
		len(tables), columns, charCols, binCols, tinyCols, versionCols)
}
