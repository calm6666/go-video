package model

// 本文件是「迁移 DDL <-> model 代码」的双向逐列对账测试，纯静态解析：
// 只读 deploy/migrations/recommend-recall/*.sql 的文本与本包源码/结构体 tag，
// 绝对不连任何数据库（本仓库的测试禁止连到本机 3306，那是用户的真实 MySQL）。
//
// 它守住六类真实故障：
//  1. model 新增/改名列而迁移没跟上 -> 上线第一次读就 Unknown column；
//  2. 迁移新增列而 model 从不读写 -> 死列（谁都不敢删）；
//  3. 幂等写入依赖的唯一键丢失或改名 -> ON DUPLICATE KEY UPDATE 变成普通插入，重放产生第二份副作用；
//  4. 唯一键多加一个（例如给 request_hash 也建唯一键）-> Claim 的 affected==1 判定失真；
//  5. 参与唯一性判定的列忘了 utf8mb4_bin -> 只差大小写或尾随空格的两个键被折叠成同一个，静默少执行一次；
//  6. 加了没人查的索引 -> 纯写放大（AGENTS.md §4）。
//
// 关键字判定全部打在「剥掉头注释与行注释之后的语句体」上：迁移文件头有大段中文说明，
// 里面就会出现 PRIMARY KEY / ON DUPLICATE KEY UPDATE / DEFAULT 这些词，
// 不做锚定的文本匹配必然误判 —— 这也是列定义只在 " COMMENT '" 之前找关键字的原因。

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

	"go-video/common/eventenvelope"
	"go-video/common/idempotency"
	"go-video/common/idgen"
)

const (
	migDirRel = "deploy/migrations/recommend-recall"
	// migTablePrefix 是本服务自有表的前缀；schema_migrations 由 scripts/migrate.ps1 维护，不在对账范围。
	migTablePrefix = "recall_"
	// migTableCollation / migBinCollation 对应 deploy/migrations/README.md「字符序约定」。
	migTableCollation = "utf8mb4_unicode_ci"
	migBinCollation   = "utf8mb4_bin"
	// migOpaque 是源码里无法静态求值的表达式片段（函数调用、局部变量）的占位符。
	migOpaque = "\x00"
)

// ciTextKeys 白名单里「值域受限」的两种证据。两者都由本文件实跑校验，不是注释。
const (
	// ciProofPoolKey：该列只能取 model.ValidatePoolKey 放行的字面量，
	// 而校验是逐字节敏感的 —— 与合法键只差大小写或空格的写法一律被拒，
	// 于是"两个都能存进去"的不同键不可能被 unicode_ci 折成同一个。
	ciProofPoolKey = "model.ValidatePoolKey 逐字节放行/拒绝"
	// ciProofULID：该列的值只来自 idgen.Prefixed("<固定前缀>_<26 位大写 ULID>")。
	// 前缀是常量、随机段全大写且整体无空白，折叠映射在该值域上是单射。
	ciProofULID = "idgen.Prefixed 固定前缀 + 大写无空白 ULID"
	// ciProofFrozen：已 applied 的迁移里唯一"证明不了、只能钉住"的一条 ——
	// 登记它而不是放过它：白名单之外的任何唯一键文本列都必须 utf8mb4_bin，
	// 这条一旦被人用 ALTER 迁移修好，测试会因为"bin 列仍在白名单里"而失败，逼着回来删条目。
	ciProofFrozen = "迁移已 applied 且文件头禁止就地修改：只能钉住债务，后续用 ALTER 迁移收紧"
)

// migTableSpec 描述一张表在 DDL 与 model 两侧的期望形状。
// 每个字段都不是按惯例编的：来源在该表的注释里点名到具体 model 方法与 SQL 片段。
type migTableSpec struct {
	file       string              // 迁移文件名（编号必须连续递增）
	table      string              // 表名
	goFile     string              // 读写这张表的 model 源文件
	row        any                 // model 行结构体零值（只用 reflect 读 db tag）
	primary    []string            // 期望主键列（有序）
	uniqueKeys map[string][]string // 期望唯一键：键名 -> 有序列
	indexes    map[string][]string // 期望普通二级索引：索引名 -> 有序列
	maxUnique  int                 // 允许的唯一键个数（多一个就可能破坏 affected 判定）
	binColumns []string            // 额外必须列级 utf8mb4_bin 的列（指纹/跨表精确比对列）
	noDefault  []string            // 必须「NOT NULL 且无 DEFAULT」的列
	insertCols []string            // model INSERT 显式写入的列（不含自增主键）
	// surrogatePK 标记「主键只是聚簇代理键」：没有任何按主键定位的读写路径。只允许 recall_pool。
	surrogatePK bool
	// ciTextKeys 登记「参与唯一性判定、但仍保持表级 utf8mb4_unicode_ci」的列 -> 值域受限的证据。
	// 这是被钉死的既有债务，不是规则例外：所在迁移已 applied（文件头明令「禁止修改本文件」，
	// 收紧字符序只能新增 ALTER 迁移），所以改不了 DDL，只能由测试证明"折叠不会撞键"。
	// 新增的唯一键文本列一律要 utf8mb4_bin；白名单条目一旦失效（列不再参与唯一性、
	// 或已经收紧成 bin）测试就会失败。
	ciTextKeys map[string]string
	// sqlOnly 允许「DDL 有、model 不读」的列，值是理由。默认为空 = 出现即判死列。
	sqlOnly map[string]string
	// headerMust 是文件头必须出现的说明段（AGENTS.md 与 deploy/migrations/README.md 的要求）。
	headerMust []string
	// defaultIs 对账「列 DEFAULT 字面量 == model 侧同义常量值」。
	defaultIs map[string]string
}

// migTableSpecs 按迁移编号升序。
func migTableSpecs() []migTableSpec {
	return []migTableSpec{
		{
			// model/pool.go：poolSelect、BatchUpsert（ODKU）、TopByVersion、ListByVersion、
			// CountByVersion、DeleteByVersions。
			file:    "000001_create_recall_pool.sql",
			table:   "recall_pool",
			goFile:  "pool.go",
			row:     RecallPool{},
			primary: []string{"id"},
			uniqueKeys: map[string][]string{
				"uniq_pool_item": {"source", "pool_key", "version", "aid"},
			},
			indexes: map[string][]string{
				"idx_pool_version_score": {"source", "pool_key", "version", "score"},
			},
			maxUnique:   1,
			insertCols:  []string{"source", "pool_key", "version", "aid", "score", "ctime", "mtime"},
			surrogatePK: true, // 条目只按 (source,pool_key,version) 整批读写，从不按 id 定位
			ciTextKeys:  map[string]string{"pool_key": ciProofPoolKey},
			headerMust:  []string{"用途", "数据所有者", "唯一键与幂等设计", "索引取自真实查询路径", "回滚", "锁风险"},
		},
		{
			// model/poolversion.go：poolVersionSelect、Register（ODKU + 回读 batch_id 比对）、
			// FindOne/FindForUpdate/FindCurrent/FindCurrentForUpdate/FindByID/FindByBatch、
			// ListByPool/ListByRefs/MaxVersion/UpdateState/SetPublishedAt/UpdateItemCount/
			// PrunableBefore/ListPrunable/Delete。
			file:    "000002_create_recall_pool_version.sql",
			table:   "recall_pool_version",
			goFile:  "poolversion.go",
			row:     RecallPoolVersion{},
			primary: []string{"id"},
			uniqueKeys: map[string][]string{
				"uniq_pool_version":       {"source", "pool_key", "version"},
				"uniq_pool_version_batch": {"source", "pool_key", "batch_id"},
			},
			indexes: map[string][]string{
				"idx_pool_state_version": {"source", "pool_key", "state", "version"},
			},
			maxUnique: 2,
			insertCols: []string{"source", "pool_key", "version", "batch_id", "generator", "schema_version",
				"item_count", "state", "published_at", "operator", "note", "ctime", "mtime"},
			// pool_key 走 ValidatePoolKey 的逐字节白名单；batch_id 是调用方文本
			// （helpers.requiredRef 只 TrimSpace + 限长，不锁大小写），而 000002 已 applied、
			// 文件头禁止就地修改 —— 这条债务本测试只能钉住不能证明安全，见 README 风险说明。
			ciTextKeys: map[string]string{"pool_key": ciProofPoolKey, "batch_id": ciProofFrozen},
			headerMust: []string{"用途", "数据所有者", "事实定位", "唯一键与幂等设计", "索引取自真实查询路径", "回滚", "锁风险"},
			defaultIs: map[string]string{
				"state":          "1", // = VersionStateBuilding
				"schema_version": "1", // = PoolVersionSchemaVersion
			},
		},
		{
			// model/poolcurrent.go：poolCurrentSelect、FindOne、FindOneForUpdate、EnsureRow（ODKU）、
			// Switch（CAS + RowsAffected）、ListBySources、ListCurrent。
			file:       "000003_create_recall_pool_current.sql",
			table:      "recall_pool_current",
			goFile:     "poolcurrent.go",
			row:        RecallPoolCurrent{},
			primary:    []string{"source", "pool_key"}, // 每池一行：聚簇主键就是唯一访问路径
			uniqueKeys: map[string][]string{},
			indexes:    map[string][]string{},
			maxUnique:  0,
			insertCols: []string{"source", "pool_key", "version", "batch_id", "previous_version", "switch_count",
				"operator", "note", "published_at", "ctime", "mtime"},
			// pool_key 是联合主键成员（=唯一性判定列），语法受 model.ValidatePoolKey 逐字节约束。
			ciTextKeys: map[string]string{"pool_key": ciProofPoolKey},
			headerMust: []string{"用途", "数据所有者", "事实定位", "回滚", "锁风险"},
		},
		{
			// model/requestlog.go：requestLogSelect、requestLogInsertColumns、Insert
			//（重复键 -> ErrRequestLogExists）、find(request_id|snapshot_id)、
			// List/Count/requestLogWhere、DeleteBefore。
			file:    "000004_create_recall_request_log.sql",
			table:   "recall_request_log",
			goFile:  "requestlog.go",
			row:     RecallRequestLog{},
			primary: []string{"id"},
			uniqueKeys: map[string][]string{
				"uniq_request_id":  {"request_id"},
				"uniq_snapshot_id": {"snapshot_id"},
			},
			indexes: map[string][]string{
				"idx_mid_ctime":   {"mid", "ctime"},
				"idx_scene_ctime": {"scene", "ctime"},
				"idx_ctime":       {"ctime"},
			},
			// 本表的 Insert 不做 affected 判定（重复键翻译成 ErrRequestLogExists），
			// 所以两个唯一键并存是安全的：这里的唯一键是"回放锚点"而不是"抢锁锚点"。
			maxUnique: 2,
			insertCols: []string{"request_id", "snapshot_id", "mid", "scene", "platform", "app_version", "region",
				"requested_sources", "per_source", "candidate_count", "returned_count", "degraded", "degrade_reason",
				"dropped_sources", "versions_digest", "cost_ms", "trace_id", "ctime"},
			// snapshot_id 只由 idgen.Prefixed("snap") 生成；request_id 允许调用方自带
			// （recallRequestID 只 TrimSpace + 限长），所以和 000002.batch_id 一样只能钉住。
			ciTextKeys: map[string]string{"request_id": ciProofFrozen, "snapshot_id": ciProofULID},
			headerMust: []string{"用途", "数据所有者", "唯一键与幂等设计", "索引取自真实查询路径", "敏感信息约束", "回滚", "锁风险"},
		},
		{
			// model/outbox.go：outboxSelect、Insert（13 列普通 INSERT，重复键 -> ErrEventExists）、
			// ListPending、MarkPublished、MarkRetry、MarkFailed、CountPending、CountStuck、DeleteSentBefore。
			file:    "000005_create_recall_outbox.sql",
			table:   "recall_outbox",
			goFile:  "outbox.go",
			row:     RecallOutbox{},
			primary: []string{"id"},
			uniqueKeys: map[string][]string{
				// Insert 里 isDuplicateErr 唯一可能命中的键。
				"uniq_event_id": {"event_id"},
			},
			indexes: map[string][]string{
				"idx_state_next_retry": {"state", "next_retry_at", "id"},
				"idx_state_occurred":   {"state", "occurred_at"},
			},
			maxUnique: 1,
			// event_id 是唯一键成员（测试会自动要求 bin）；其余列沿用表级字符序。
			binColumns: nil,
			// payload 是 TEXT（MySQL 禁止 TEXT/BLOB 设 DEFAULT）；event_id 是唯一键列，
			// 给默认值等于允许"漏赋值"静默插进第二条空串 —— 两侧都必须由 Insert 显式写入。
			noDefault: []string{"event_id", "payload"},
			insertCols: []string{"event_id", "event_type", "schema_version", "aggregate_type", "aggregate_id",
				"payload", "state", "retry_count", "next_retry_at", "occurred_at", "last_error", "ctime", "mtime"},
			headerMust: []string{"用途", "数据所有者", "事实定位", "唯一键与幂等设计", "索引取自真实查询路径", "列宽依据", "回滚", "锁风险"},
			defaultIs: map[string]string{
				"state":          "0", // = OutboxStatePending（Go int32 零值就是"待发布"）
				"schema_version": "1", // = PoolVersionSchemaVersion
			},
		},
		{
			// model/idempotency.go：idempotencySelect、Claim（INSERT ... ODKU id=id + affected==1
			// + 过期/失败后的条件重新认领 UPDATE）、MarkSucceeded、MarkFailed、find、
			// ListExpired、DeleteExpired。
			file:    "000006_create_recall_idempotency.sql",
			table:   "recall_idempotency",
			goFile:  "idempotency.go",
			row:     RecallIdempotency{},
			primary: []string{"id"},
			uniqueKeys: map[string][]string{
				// Claim 的文档注释点名的冲突键，也是除主键外唯一允许的键。
				"uniq_scope_key": {"scope", "idempotency_key"},
			},
			indexes: map[string][]string{
				"idx_expire_at": {"expire_at"},
			},
			// 硬约束：除主键外只能有一个唯一键。再加第二个，"命中新键的无变化更新"同样返回
			// affected=0，首次受理会被误判为重放（迁移头有完整推演）。
			maxUnique: 1,
			// request_hash 参与 Go 侧 exist.RequestHash != in.RequestHash 的精确比对；
			// event_id 要与 recall_outbox.event_id 逐字节对齐（两列 collation 不同连比较都会报错）。
			binColumns: []string{"request_hash", "event_id"},
			noDefault:  []string{"scope", "idempotency_key", "request_hash", "result_payload"},
			insertCols: []string{"scope", "idempotency_key", "request_hash", "state", "result_payload", "event_id",
				"operator", "lease_expire_at", "expire_at", "execution_count", "last_error", "ctime", "mtime"},
			headerMust: []string{"用途", "数据所有者", "唯一键与幂等设计", "索引取自真实查询路径", "列宽依据", "回滚", "锁风险"},
			defaultIs: map[string]string{
				"state": "'pending'", // = string(idempotency.StatePending)
			},
		},
	}
}

// -----------------------------------------------------------------------------
// 迁移文件解析
// -----------------------------------------------------------------------------

type migColumn struct {
	name          string
	typeToken     string // "BIGINT UNSIGNED" / "VARCHAR(128)" / "TEXT" ...
	collate       string // 列级 COLLATE，空表示沿用表级
	notNull       bool
	hasDefault    bool
	defaultVal    string
	autoIncrement bool
	hasComment    bool
}

type migKeyDef struct {
	name    string
	columns []string
}

type migTable struct {
	spec    migTableSpec
	file    string
	header  string // 注释行（保留 "--" 前缀）
	sql     string // 非注释行
	columns []migColumn
	primary []string
	unique  []migKeyDef
	indexes []migKeyDef
	options string // 右括号之后到分号之前的表选项
}

// migMigrationsDir 从 cwd 向上找到仓库根，返回本服务的迁移目录。
func migMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, filepath.FromSlash(migDirRel))
		if st, statErr := os.Stat(candidate); statErr == nil && st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("未找到迁移目录 %s（相对仓库根）", migDirRel)
	return ""
}

func migReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// migSplitHeaderSQL 把迁移文件拆成注释行与语句行：判据是"去缩进后行首的两个减号"，
// 不用行内位置，否则 SQL 字符串里出现的 "--" 会被误当成注释。
func migSplitHeaderSQL(content string) (string, string) {
	var header, sql strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			header.WriteString(line)
			header.WriteString("\n")
			continue
		}
		sql.WriteString(line)
		sql.WriteString("\n")
	}
	return header.String(), sql.String()
}

// migSplitItems 按「括号深度 0 且不在单引号内」的逗号切分，
// 这样索引定义里的 (a, b, c) 与列注释里的逗号都不会被误切。
func migSplitItems(s string) []string {
	var (
		out     []string
		depth   int
		inQuote bool
		cur     strings.Builder
	)
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
			cur.WriteRune(r)
		case inQuote:
			cur.WriteRune(r)
		case r == '(':
			depth++
			cur.WriteRune(r)
		case r == ')':
			depth--
			cur.WriteRune(r)
		case r == ',' && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

var (
	migIdentRe     = regexp.MustCompile("^\\s*`([^`]+)`")
	migCollateRe   = regexp.MustCompile(`(?i)\bCOLLATE\s+([a-zA-Z0-9_]+)`)
	migTypeRe      = regexp.MustCompile(`(?i)^\s*([A-Z]+(?:\s+UNSIGNED)?(?:\s*\(\s*\d+(?:\s*,\s*\d+\s*)?\))?)`)
	migWidthRe     = regexp.MustCompile(`(?i)^(?:VAR)?CHAR\s*\(\s*(\d+)\s*\)`)
	migTableNameRe = regexp.MustCompile("(?i)\\b(?:FROM|INTO|UPDATE|JOIN)\\s+(`?" + migTablePrefix + `[a-z0-9_]*)`)
	migAssignRe    = regexp.MustCompile(`(?m)^[ \t]*(?:(?:const|var)[ \t]+)?([A-Za-z_]\w*)[ \t]*(?::=|=)[ \t]*"`)
)

// migParseColumn 解析一条列定义。
//
// NOT NULL / DEFAULT / AUTO_INCREMENT 只在 " COMMENT '" 之前的部分上判定：
// 列注释里就会写到 DEFAULT、唯一索引这类词，不剥就会误判。
func migParseColumn(item string) (migColumn, error) {
	m := migIdentRe.FindStringSubmatch(item)
	if m == nil {
		return migColumn{}, fmt.Errorf("列定义缺少反引号列名: %q", strings.TrimSpace(item))
	}
	col := migColumn{name: m[1]}
	code := strings.TrimSpace(item[len(m[0]):])
	if idx := strings.Index(code, " COMMENT "); idx >= 0 {
		col.hasComment = true
		code = strings.TrimSpace(code[:idx])
	}
	if cm := migCollateRe.FindStringSubmatchIndex(code); cm != nil {
		col.collate = strings.ToLower(code[cm[2]:cm[3]])
		// 只摘掉 "COLLATE xxx" 这一段，后面的 NOT NULL / DEFAULT 必须留着：
		// 从 COLLATE 处截断会让 event_id 这类列看起来"允许 NULL"。
		code = strings.TrimSpace(code[:cm[0]] + " " + code[cm[1]:])
	}
	if tm := migTypeRe.FindStringSubmatch(code); tm != nil {
		col.typeToken = strings.ToUpper(strings.Join(strings.Fields(tm[1]), " "))
	}
	up := strings.ToUpper(code)
	col.notNull = strings.Contains(up, "NOT NULL")
	col.autoIncrement = strings.Contains(up, "AUTO_INCREMENT")
	if i := strings.Index(up, "DEFAULT "); i >= 0 {
		col.hasDefault = true
		rest := code[i+len("DEFAULT "):]
		cut := len(rest)
		restUp := strings.ToUpper(rest)
		for j := 0; j < len(restUp); j++ {
			if c := restUp[j]; c == ' ' || c == ',' || c == ';' {
				cut = j
				break
			}
		}
		col.defaultVal = strings.TrimSpace(rest[:cut])
	}
	return col, nil
}

// migParseKeyDef 解析 PRIMARY KEY / UNIQUE KEY / KEY 一条定义，返回定义与类别。
func migParseKeyDef(item string) (migKeyDef, string, error) {
	up := strings.ToUpper(strings.TrimSpace(item))
	var kind string
	switch {
	case strings.HasPrefix(up, "PRIMARY KEY"):
		kind = "primary"
	case strings.HasPrefix(up, "UNIQUE"):
		kind = "unique"
	case strings.HasPrefix(up, "KEY"), strings.HasPrefix(up, "INDEX"), strings.HasPrefix(up, "USING"):
		kind = "index"
	default:
		return migKeyDef{}, "", fmt.Errorf("无法识别的表内定义: %q", strings.TrimSpace(item))
	}
	open := strings.Index(item, "(")
	closing := strings.LastIndex(item, ")")
	if open < 0 || closing < open {
		return migKeyDef{}, "", fmt.Errorf("索引定义缺少括号: %q", strings.TrimSpace(item))
	}
	name := ""
	if fields := strings.Fields(strings.TrimSpace(item[:open])); len(fields) > 1 {
		name = strings.Trim(fields[len(fields)-1], "`")
	}
	var cols []string
	for _, p := range strings.Split(item[open+1:closing], ",") {
		tok := strings.TrimSpace(p)
		if i := strings.IndexAny(tok, "( "); i >= 0 { // 去掉前缀长度与 ASC/DESC 修饰
			tok = strings.TrimSpace(tok[:i])
		}
		if tok = strings.Trim(tok, "`"); tok != "" {
			cols = append(cols, tok)
		}
	}
	return migKeyDef{name: name, columns: cols}, kind, nil
}

// migLoadTables 解析本服务的 6 个迁移文件。
func migLoadTables(t *testing.T) []migTable {
	t.Helper()
	dir := migMigrationsDir(t)
	specs := migTableSpecs()
	out := make([]migTable, 0, len(specs))
	for _, spec := range specs {
		path := filepath.Join(dir, spec.file)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("缺少迁移文件 %s：%v", spec.file, err)
		}
		content := migReadFile(t, path)
		header, sql := migSplitHeaderSQL(content)
		tbl := migTable{spec: spec, file: spec.file, header: header, sql: sql}
		stmts := 0
		for _, s := range strings.Split(sql, ";") {
			body := strings.TrimSpace(s)
			if body == "" {
				continue
			}
			stmts++
			if !strings.HasPrefix(strings.ToUpper(body), "CREATE TABLE IF NOT EXISTS") {
				t.Fatalf("%s: 出现非 CREATE TABLE IF NOT EXISTS 的语句 %q", spec.file, migFirstWords(body, 6))
			}
			open := strings.Index(body, "(")
			closing := migMatchingParen(body, open)
			if open < 0 || closing < 0 {
				t.Fatalf("%s: CREATE TABLE 括号不成对", spec.file)
			}
			for _, item := range migSplitItems(body[open+1 : closing]) {
				if strings.TrimSpace(item) == "" {
					continue
				}
				head := strings.ToUpper(strings.TrimSpace(item))
				if strings.HasPrefix(head, "PRIMARY KEY") || strings.HasPrefix(head, "UNIQUE") ||
					strings.HasPrefix(head, "KEY") || strings.HasPrefix(head, "INDEX") ||
					strings.HasPrefix(head, "USING") {
					kd, kind, err := migParseKeyDef(item)
					if err != nil {
						t.Fatalf("%s: %v", spec.file, err)
					}
					switch kind {
					case "primary":
						tbl.primary = kd.columns
					case "unique":
						tbl.unique = append(tbl.unique, kd)
					default:
						tbl.indexes = append(tbl.indexes, kd)
					}
					continue
				}
				col, err := migParseColumn(item)
				if err != nil {
					t.Fatalf("%s: %v", spec.file, err)
				}
				tbl.columns = append(tbl.columns, col)
			}
			tbl.options = strings.TrimSpace(body[closing+1:])
		}
		if stmts != 1 {
			t.Fatalf("%s: 一个迁移文件必须恰好一段 CREATE TABLE，实际 %d 段", spec.file, stmts)
		}
		out = append(out, tbl)
	}
	return out
}

// migTableName 从 "CREATE TABLE IF NOT EXISTS `x`" 里取表名。
func migTableName(t *testing.T, tbl migTable) string {
	t.Helper()
	m := regexp.MustCompile("(?i)CREATE TABLE IF NOT EXISTS\\s+`?([a-z0-9_]+)`?").FindStringSubmatch(tbl.sql)
	if m == nil {
		t.Fatalf("%s: 解析不出表名", tbl.file)
	}
	return strings.ToLower(m[1])
}

// migMatchingParen 返回与 open 处左括号配对的右括号下标（跳过单引号字符串）。
func migMatchingParen(s string, open int) int {
	depth, inQuote := 0, false
	for i := open; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			inQuote = !inQuote
		case inQuote:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func migFirstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

// -----------------------------------------------------------------------------
// model 侧源码：SQL 文本静态求值
// -----------------------------------------------------------------------------

// migAtom 是拼接表达式的一个成分。
type migAtom struct {
	lit     string
	ident   string
	literal bool
}

type migExpr struct {
	start int // 表达式起点（用于把 `const X = "..."` 的名字对上值）
	atoms []migAtom
}

// migParseExprs 扫描（已剥注释的）源码，按顺序返回所有"至少含一个字面量"的拼接表达式：
// `"a" + "b"`、`poolSelect + " WHERE ..."`、`"INSERT INTO t (" + colsConst + ") VALUES ("` 都能整体求值。
// 函数调用（ExecCtx(...)、inPlaceholders(13)）不作为拼接项：链条在标识符后紧跟 '(' 处停止，
// 括号里的字面量会由外层扫描重新发现。
func migParseExprs(src string) []migExpr {
	var out []migExpr
	for i := 0; i < len(src); {
		c := src[i]
		if c != '"' && !migIsNameStart(c) {
			i++
			continue
		}
		atoms, next, hasLit := migParseChain(src, i)
		if !hasLit {
			i++
			continue
		}
		if next <= i {
			next = i + 1
		}
		out = append(out, migExpr{start: i, atoms: atoms})
		i = next
	}
	return out
}

// migParseChain 从 start 处的字面量或标识符开始吃拼接链。
func migParseChain(src string, start int) ([]migAtom, int, bool) {
	var atoms []migAtom
	hasLit := false
	i := start
	for i < len(src) {
		switch {
		case src[i] == '"':
			v, next, ok := migReadGoString(src, i)
			if !ok {
				return atoms, i, hasLit
			}
			atoms = append(atoms, migAtom{lit: v, literal: true})
			hasLit = true
			// 跨行拼接（`"a, " +\n\t"b, "`）在字面量之间必然夹着换行与缩进：
			// 不跳空白就只看得到第一个字面量，列清单会被截断成前几列。
			k := migSkipSpace(src, next)
			if k >= len(src) || src[k] != '+' {
				return atoms, next, hasLit
			}
			i = migSkipSpace(src, k+1)
		case migIsNameStart(src[i]):
			k := i
			for k < len(src) && (migIsNameChar(src[k]) || src[k] == '.') {
				k++
			}
			p := migSkipSpace(src, k)
			if p < len(src) && src[p] == '(' { // 函数调用/选择器：链条到此，留给外层继续扫
				return atoms, i, hasLit
			}
			if p >= len(src) || src[p] != '+' { // 普通标识符引用（无后续拼接），无字面量价值
				return atoms, i, hasLit
			}
			atoms = append(atoms, migAtom{ident: src[i:k]})
			i = migSkipSpace(src, p+1)
		default:
			return atoms, i, hasLit
		}
	}
	return atoms, i, hasLit
}

func migSkipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

func migIsNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func migIsNameChar(c byte) bool {
	return migIsNameStart(c) || (c >= '0' && c <= '9')
}

// migReadGoString 从 i 处的双引号读出字面量内容（处理反斜杠转义），返回值与下一位置。
func migReadGoString(s string, i int) (string, int, bool) {
	var sb strings.Builder
	for j := i + 1; j < len(s); {
		switch s[j] {
		case '\\':
			if j+1 >= len(s) {
				return "", i, false
			}
			switch s[j+1] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			default:
				sb.WriteByte(s[j+1])
			}
			j += 2
		case '"':
			return sb.String(), j + 1, true
		default:
			sb.WriteByte(s[j])
			j++
		}
	}
	return "", i, false
}

// migSQLText 返回一个 model 文件里的全部可静态求值 SQL 文本：
// corpus 是它们按源码顺序的拼接（跨字面量、跨常量引用的列清单因此能整体匹配），
// fragments 是其中带占位符 `?` 的查询片段（条件/排序文本都来自它们）。
func migSQLText(t *testing.T, path string) (string, []string) {
	t.Helper()
	src := migStripGoComments(migReadFile(t, path))
	exprs := migParseExprs(src)

	// `const/var NAME = "..."` 的 NAME -> 该表达式起点，供引用处展开。
	nameAt := map[int]string{}
	for _, m := range migAssignRe.FindAllStringSubmatchIndex(src, -1) {
		nameAt[m[1]-1] = src[m[2]:m[3]]
	}
	values := map[string]string{}
	resolve := func(expr migExpr) string {
		var sb strings.Builder
		for _, a := range expr.atoms {
			switch {
			case a.literal:
				sb.WriteString(a.lit)
			default:
				if v, ok := values[a.ident]; ok {
					sb.WriteString(v)
					continue
				}
				sb.WriteString(migOpaque)
			}
		}
		return sb.String()
	}
	texts := make([]string, 0, len(exprs))
	for _, e := range exprs {
		v := resolve(e)
		if name, ok := nameAt[e.start]; ok {
			values[name] = v
		}
		texts = append(texts, v)
	}
	// 第二轮：让"引用了在文件后面才定义的常量"也能展开（本包目前没有，代价只是多一遍循环）。
	for idx, e := range exprs {
		if strings.Contains(texts[idx], migOpaque) {
			texts[idx] = resolve(e)
		}
	}
	var corpus strings.Builder
	var frags []string
	for _, v := range texts {
		if v == "" {
			continue
		}
		// 表达式之间必须留一个分隔符：直接首尾相接会造出 "recall_poolINSERT" 这种
		// 跨语句的假标识符，表名/列名正则就会把两条语句的词粘在一起读。
		if corpus.Len() > 0 {
			corpus.WriteString(" ")
		}
		corpus.WriteString(v)
		if strings.Contains(v, "?") && !migFormatVerbRe.MatchString(v) {
			frags = append(frags, v)
		}
	}
	return corpus.String(), frags
}

var migFormatVerbRe = regexp.MustCompile("%[a-zA-Z]")

// migStripGoComments 去掉 // 行注释与 /* */ 块注释；字符串字面量里的 "//" 不受影响。
func migStripGoComments(src string) string {
	var out strings.Builder
	inLine, inBlock, inStr := false, false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		var next byte
		if i+1 < len(src) {
			next = src[i+1]
		}
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out.WriteByte(c)
			}
		case inBlock:
			if c == '*' && next == '/' {
				inBlock = false
				i++
			}
		case inStr:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if c == '"' {
				inStr = false
			}
		case c == '/' && next == '/':
			inLine = true
			i++
		case c == '/' && next == '*':
			inBlock = true
			i++
		case c == '"':
			inStr = true
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// migDBTags 返回结构体 db tag 的列名，顺序即字段声明顺序。
func migDBTags(t *testing.T, row any) []string {
	t.Helper()
	rt := reflect.TypeOf(row)
	if rt.Kind() != reflect.Struct {
		t.Fatalf("行结构体必须是 struct，实际 %T", row)
	}
	var cols []string
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.TrimSpace(rt.Field(i).Tag.Get("db"))
		if tag == "" || tag == "-" {
			t.Fatalf("%T 字段 %s 没有 db tag", rt, rt.Field(i).Name)
		}
		if j := strings.Index(tag, ","); j >= 0 {
			tag = tag[:j]
		}
		cols = append(cols, tag)
	}
	return cols
}

// migSelectColumns 从 corpus 里取 "SELECT <cols> FROM <table>" 的列清单。
func migSelectColumns(t *testing.T, corpus, table string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?i)SELECT\s+([^()]*?)\s+FROM\s+` + regexp.QuoteMeta(table) + `\b`)
	m := re.FindStringSubmatch(migNormalize(corpus))
	if m == nil {
		t.Fatalf("%s: model 侧找不到 SELECT ... FROM %s 的完整列清单", table, table)
	}
	return migSplitColumns(m[1])
}

// migInsertColumns 从 corpus 里取 "INSERT INTO <table> (<cols>)" 的列清单。
func migInsertColumns(t *testing.T, corpus, table string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?i)INSERT\s+INTO\s+` + regexp.QuoteMeta(table) + `\s*\(([^)]*)\)`)
	m := re.FindStringSubmatch(migNormalize(corpus))
	if m == nil {
		t.Fatalf("%s: model 侧找不到 INSERT INTO %s (列清单) 语句", table, table)
	}
	return migSplitColumns(m[1])
}

func migSplitColumns(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func migNormalize(s string) string { return strings.Join(strings.Fields(s), " ") }

// migConditionTokens 从查询片段里抽出「条件/排序区」用到的列名，并单独标出真正参与
// 等值/范围/排序的列（可用作索引最左前缀的那些）。
//
// 只统计条件区：SELECT 清单里必然带着所有已读列，拿它判"这列被查询用过"永远为真，等于没断言。
func migConditionTokens(frags []string) (used, access map[string]bool) {
	used, access = map[string]bool{}, map[string]bool{}
	cmpRe := regexp.MustCompile(`([a-z][a-z0-9_]*)\s*(?:=|>=|<=|<|>|\bIN\s*\(|\bLIKE\b)`)
	for _, f := range frags {
		norm := migNormalize(strings.ToLower(strings.ReplaceAll(f, migOpaque, " ")))
		var regions []string
		if i := strings.LastIndex(norm, "where "); i >= 0 {
			regions = append(regions, norm[i+len("where "):])
		}
		if i := strings.Index(norm, "order by "); i >= 0 {
			tail := norm[i+len("order by "):]
			if j := strings.Index(tail, " limit "); j >= 0 {
				tail = tail[:j]
			}
			regions = append(regions, tail)
		}
		if strings.Contains(norm, " = ") && !strings.Contains(norm, "select ") &&
			!strings.Contains(norm, "insert ") && !strings.Contains(norm, "update ") &&
			!strings.Contains(norm, "delete ") {
			regions = append(regions, norm) // requestLogWhere 那种纯条件片段
		}
		for _, r := range regions {
			for _, tok := range migWords(r) {
				used[tok] = true
			}
			for _, m := range cmpRe.FindAllStringSubmatch(r, -1) {
				access[m[1]] = true
			}
			for _, part := range strings.Split(r, ",") { // ORDER BY a DESC, b ASC
				if fields := strings.Fields(part); len(fields) > 0 && migWordRe.MatchString(fields[0]) {
					access[fields[0]] = true
				}
			}
		}
	}
	return used, access
}

var migWordRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func migWords(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if migWordRe.MatchString(f) && !migSQLWords[f] {
			out = append(out, f)
		}
	}
	return out
}

var migSQLWords = map[string]bool{
	"select": true, "from": true, "where": true, "and": true, "or": true, "order": true, "by": true,
	"limit": true, "offset": true, "desc": true, "asc": true, "in": true, "is": true, "not": true,
	"null": true, "set": true, "insert": true, "into": true, "values": true, "update": true, "delete": true,
	"for": true, "duplicate": true, "key": true, "on": true, "count": true, "max": true, "as": true,
}

// migUsesODKU 判定该 model 文件是否用 ON DUPLICATE KEY UPDATE 做幂等写入。
// 只看求值后的 SQL 文本，不看中文注释（注释里同样会出现这个词）。
func migUsesODKU(t *testing.T, goFile string) bool {
	t.Helper()
	corpus, _ := migSQLText(t, goFile)
	return strings.Contains(strings.ToUpper(corpus), "ON DUPLICATE KEY UPDATE")
}

// migUsesDupSentinel 判定该文件是否把唯一键冲突翻译成哨兵错误（isDuplicateErr）。
func migUsesDupSentinel(t *testing.T, goFile string) bool {
	t.Helper()
	return strings.Contains(migReadFile(t, goFile), "isDuplicateErr(")
}

// -----------------------------------------------------------------------------
// 断言
// -----------------------------------------------------------------------------

// TestMigrationFilesAreNumberedAndCoverModelTables 对账「文件编号连续 + 表集合双向闭合」：
// model 里出现 recall_* 表却没有建表迁移、或迁移建了表却没人读写，都算失败。
func TestMigrationFilesAreNumberedAndCoverModelTables(t *testing.T) {
	dir := migMigrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	specs := migTableSpecs()
	if len(files) != len(specs) {
		t.Fatalf("迁移文件数 = %d，期望 %d（%v）", len(files), len(specs), files)
	}
	for i, name := range files {
		spec := specs[i]
		if name != spec.file {
			t.Fatalf("第 %d 个迁移文件名 = %s，期望 %s（编号须连续递增且形如 NNNNNN_动词_表名.sql）", i+1, name, spec.file)
		}
		num, err := strconv.Atoi(name[:6])
		if err != nil {
			t.Fatalf("%s: 编号不是 6 位数字", name)
		}
		if num != i+1 {
			t.Fatalf("%s: 编号 %d 应与序号 %d 一致（中间不许跳号）", name, num, i+1)
		}
		if !strings.HasSuffix(name, "_"+spec.table+".sql") {
			t.Fatalf("%s: 文件名末尾必须是建出的表名 %s", name, spec.table)
		}
	}

	fromCode := migTablesInModelSource(t)
	for _, spec := range specs {
		if _, ok := fromCode[spec.table]; !ok {
			t.Errorf("%s: 迁移建了 %s，但 model 源码里没有对它的任何 SQL（补读写路径或删迁移）", spec.file, spec.table)
		}
		delete(fromCode, spec.table)
	}
	for tbl, file := range fromCode {
		t.Errorf("%s 被 %s 里的 SQL 读写，却没有建表迁移：必须新增 0000NN_create_%s.sql", tbl, file, tbl)
	}
}

// migTablesInModelSource 扫本包全部非测试源文件，抽出被 SQL 读写的 recall_* 表名。
func migTablesInModelSource(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("枚举 model 源文件失败: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		corpus, _ := migSQLText(t, f)
		for _, m := range migTableNameRe.FindAllStringSubmatch(corpus, -1) {
			out[strings.ToLower(strings.Trim(m[1], "`"))] = f
		}
	}
	return out
}

// TestEachFileCreatesExactlyOneTable 保证迁移可重复执行且一眼可审：
// 一个文件一段 CREATE TABLE IF NOT EXISTS，建出的表与文件名一致。
func TestEachFileCreatesExactlyOneTable(t *testing.T) {
	for _, tbl := range migLoadTables(t) {
		for _, s := range strings.Split(tbl.sql, ";") {
			body := strings.TrimSpace(s)
			if body == "" {
				continue
			}
			if !strings.HasPrefix(strings.ToUpper(body), "CREATE TABLE IF NOT EXISTS") {
				t.Errorf("%s: 出现非 CREATE TABLE IF NOT EXISTS 的语句 %q", tbl.file, migFirstWords(body, 6))
			}
		}
		if n := strings.Count(strings.ToUpper(tbl.sql), "CREATE TABLE"); n != 1 {
			t.Errorf("%s: CREATE TABLE 段数 = %d，一张表一段", tbl.file, n)
		}
		if got := migTableName(t, tbl); got != tbl.spec.table {
			t.Errorf("%s: 建出的表是 %s，期望 %s", tbl.file, got, tbl.spec.table)
		}
	}
}

// TestEveryTableHasPrimaryKey 要求每张表显式声明 PRIMARY KEY 且列与期望一致：
// 无主键的 InnoDB 表会隐式造一个，复制链路与运维工具都拿它当唯一定位手段，风险不可见。
func TestEveryTableHasPrimaryKey(t *testing.T) {
	for _, tbl := range migLoadTables(t) {
		if len(tbl.primary) == 0 {
			t.Errorf("%s: 缺少 PRIMARY KEY 定义", tbl.file)
			continue
		}
		if strings.Join(tbl.primary, ",") != strings.Join(tbl.spec.primary, ",") {
			t.Errorf("%s: PRIMARY KEY = (%v)，期望 (%v)", tbl.file, tbl.primary, tbl.spec.primary)
		}
	}
}

// TestDDLAndModelColumnsAgreeBothWays 是双向逐列对账：
// 正向（model -> DDL）防"上线即 Unknown column"，反向（DDL -> model）防死列；
// 再加顺序一致性（DDL 列序 == 结构体 db tag 序 == SELECT 清单序），让任一侧都能按位置对齐另一侧。
func TestDDLAndModelColumnsAgreeBothWays(t *testing.T) {
	for _, tbl := range migLoadTables(t) {
		var ddl []string
		for _, c := range tbl.columns {
			ddl = append(ddl, c.name)
		}
		corpus, _ := migSQLText(t, tbl.spec.goFile)
		sel := migSelectColumns(t, corpus, tbl.spec.table)
		tags := migDBTags(t, tbl.spec.row)

		if diff := migMissing(ddl, tags); len(diff) > 0 {
			t.Errorf("%s: DDL 有、model 结构体不读写的列 %v（死列：要么补读写路径，要么删列并说明）", tbl.file, diff)
		}
		if diff := migMissing(tags, ddl); len(diff) > 0 {
			t.Errorf("%s: model 读写、DDL 没有的列 %v（上线第一次读就 Unknown column）", tbl.file, diff)
		}
		if strings.Join(sel, ",") != strings.Join(ddl, ",") {
			t.Errorf("%s: model SELECT 列清单与 DDL 不一致\n got: %v\nwant: %v", tbl.file, sel, ddl)
		}
		if strings.Join(tags, ",") != strings.Join(ddl, ",") {
			t.Errorf("%s: 结构体 db tag 顺序与 DDL 列顺序不一致\n tags: %v\n  ddl: %v", tbl.file, tags, ddl)
		}
		for _, only := range migSortedKeys(tbl.spec.sqlOnly) {
			if migIndexOf(ddl, only) < 0 {
				t.Errorf("%s: sqlOnly 白名单里的列 %s 在 DDL 里已不存在（白名单要一并删）", tbl.file, only)
			}
		}
	}
}

// TestColumnTypesMatchGoKinds 校验 DDL 类型族与 Go 字段类型族相容，并守住三条硬事实：
// 所有列必须 NOT NULL（结构体里没有 sql.NullXxx，NULL 会让 Scan 失败）、
// TEXT/BLOB 不允许 DEFAULT、每列必须有 COMMENT。时间列一律 BIGINT Unix 秒（model.nowUnix）。
func TestColumnTypesMatchGoKinds(t *testing.T) {
	for _, tbl := range migLoadTables(t) {
		rt := reflect.TypeOf(tbl.spec.row)
		goCols := map[string]reflect.Kind{}
		for i := 0; i < rt.NumField(); i++ {
			goCols[strings.TrimSpace(strings.Split(rt.Field(i).Tag.Get("db"), ",")[0])] = rt.Field(i).Type.Kind()
		}
		for _, col := range tbl.columns {
			kind, ok := goCols[col.name]
			if !ok {
				continue // DDL-only 列由对账测试与 sqlOnly 白名单负责
			}
			fam := migFamily(col.typeToken)
			if !migKindMatchesFamily(kind, fam) {
				t.Errorf("%s.%s: Go 字段 %s 与 DDL 类型 %q 不相容", tbl.file, col.name, kind, col.typeToken)
			}
			if !col.notNull {
				t.Errorf("%s.%s: 允许 NULL，但 model 用非指针非 NullXxx 字段接收，读到 NULL 会 Scan 失败", tbl.file, col.name)
			}
			if !col.hasComment {
				t.Errorf("%s.%s: 缺少列 COMMENT", tbl.file, col.name)
			}
			if migIsLobType(col.typeToken) && col.hasDefault {
				t.Errorf("%s.%s: TEXT/BLOB 不允许 DEFAULT（MySQL 建表直接报错），实际 DEFAULT %s", tbl.file, col.name, col.defaultVal)
			}
			if fam == "time" {
				t.Errorf("%s.%s: 本服务时间列一律是 BIGINT Unix 秒，不用 DATETIME", tbl.file, col.name)
			}
		}
	}
}

// TestKeysMatchModelConflictKeys 校验唯一键的名字、列集合与个数，
// 并要求"用 ODKU 幂等写入的表必须有可命中的键"。
func TestKeysMatchModelConflictKeys(t *testing.T) {
	for _, tbl := range migLoadTables(t) {
		got := map[string]string{}
		for _, u := range tbl.unique {
			got[u.name] = strings.Join(u.columns, ",")
			if _, ok := tbl.spec.uniqueKeys[u.name]; !ok {
				t.Errorf("%s: 出现未登记的唯一键 %s(%v)。唯一键个数直接影响 ODKU 的 affected 语义，"+
					"新增必须同步更新迁移头的推演", tbl.file, u.name, u.columns)
			}
		}
		for name, cols := range tbl.spec.uniqueKeys {
			if got[name] != strings.Join(cols, ",") {
				t.Errorf("%s: 唯一键 %s = (%s)，期望 (%s)：model 的冲突判定就靠这组列",
					tbl.file, name, got[name], strings.Join(cols, ","))
			}
		}
		if len(tbl.unique) != tbl.spec.maxUnique {
			t.Errorf("%s: 唯一键个数 = %d，期望 %d", tbl.file, len(tbl.unique), tbl.spec.maxUnique)
		}
		anchor := len(tbl.unique) > 0 || len(tbl.primary) > 1
		if migUsesODKU(t, tbl.spec.goFile) && !anchor {
			t.Errorf("%s: model 用 ON DUPLICATE KEY UPDATE 幂等写入，但表上既无唯一键也无复合主键 —— "+
				"ODKU 永不命中，重放会插入第二份", tbl.file)
		}
	}
}

// TestIndexesAreReadByModelQueries 校验索引的名字与列集合与期望完全一致，
// 且每个键（主键/唯一键/二级索引）的列都真的出现在该表 model 文件的条件或排序区里：
// 这是「不为不存在的查询建索引」（AGENTS.md §4）的可执行版本 —— 加个没人查的索引就红。
//
// 例外：唯一键若被 ODKU 或 isDuplicateErr 当作冲突锚点使用，它的"用途"就是挡重复，
// 不需要出现在任何检索条件里。
func TestIndexesAreReadByModelQueries(t *testing.T) {
	for _, tbl := range migLoadTables(t) {
		got := map[string]string{}
		for _, idx := range tbl.indexes {
			got[idx.name] = strings.Join(idx.columns, ",")
			if _, ok := tbl.spec.indexes[idx.name]; !ok {
				t.Errorf("%s: 出现未登记的二级索引 %s(%v)", tbl.file, idx.name, idx.columns)
			}
		}
		for name, cols := range tbl.spec.indexes {
			if got[name] != strings.Join(cols, ",") {
				t.Errorf("%s: 二级索引 %s = (%s)，期望 (%s)", tbl.file, name, got[name], strings.Join(cols, ","))
			}
		}

		_, frags := migSQLText(t, tbl.spec.goFile)
		used, access := migConditionTokens(frags)
		odku, dup := migUsesODKU(t, tbl.spec.goFile), migUsesDupSentinel(t, tbl.spec.goFile)

		var keys []migKeyDef
		if !tbl.spec.surrogatePK {
			keys = append(keys, migKeyDef{name: "PRIMARY", columns: tbl.spec.primary})
		}
		unique := tbl.spec.sortedUniqueKeys()
		keys = append(keys, unique...)
		keys = append(keys, tbl.spec.sortedIndexes()...)

		for _, k := range keys {
			if len(k.columns) == 0 {
				continue
			}
			isUnique := false
			for _, u := range unique {
				if u.name == k.name {
					isUnique = true
				}
			}
			if isUnique && (odku || dup) {
				continue
			}
			for _, c := range k.columns {
				if !used[c] {
					t.Errorf("%s: 键 %s 的列 %s 没出现在本表 model 的任何条件/排序区 —— 索引列必须对应真实谓词",
						tbl.file, k.name, c)
				}
			}
			if head := k.columns[0]; !access[head] {
				t.Errorf("%s: 键 %s 的首列 %s 未被任何等值/范围/排序条件使用（最左前缀用不上）", tbl.file, k.name, head)
			}
		}
	}
}

// TestUniquenessColumnsUseBinaryCollation 守住字符序约定：
// 唯一键成员列与指纹/事件 ID 这类要逐字节比对的列必须列级 utf8mb4_bin，其余列不得随手设 bin。
// 表级 utf8mb4_unicode_ci 是 PAD SPACE 且大小写不敏感，会把只差大小写或尾随空格的两个键
// 折成同一个，表现为"静默少执行一次"，不报错也不留痕。
func TestUniquenessColumnsUseBinaryCollation(t *testing.T) {
	// 两条"值域受限"证据都在本测试里实跑：白名单不是免检通道。
	// 单独拆成子测试，失败时一眼看出是证据本身塌了还是 DDL 改了。
	t.Run("proofs", func(t *testing.T) {
		migPoolKeyIsByteExact(t)
		migPrefixedIDHasNoFoldableChars(t)
	})

	specs := migLoadTables(t)
	var frozen []string
	for _, tbl := range specs {
		text := map[string]string{} // 列名 -> 类型（只有文本列有字符序）
		for _, c := range tbl.columns {
			if migIsTextType(c.typeToken) {
				text[c.name] = c.typeToken
			}
		}
		// 需要逐字节比对的列 = 唯一键成员 + 复合主键成员 + 指纹/跨表精确比对列。
		// 单一列主键不算：自增 id 的值由 MySQL 决定，与字符序无关。
		want := map[string]bool{}
		for _, u := range tbl.unique {
			for _, c := range u.columns {
				if _, ok := text[c]; ok {
					want[c] = true
				}
			}
		}
		if len(tbl.primary) > 1 {
			for _, c := range tbl.primary {
				if _, ok := text[c]; ok {
					want[c] = true
				}
			}
		}
		for _, c := range tbl.spec.binColumns {
			if _, ok := text[c]; !ok {
				t.Errorf("%s.%s: binColumns 登记了无字符序的列（%s）—— 整数列没有 collation，登记是空断言",
					tbl.file, c, text[c])
			}
			want[c] = true
		}
		for name, proof := range tbl.spec.ciTextKeys {
			if !want[name] {
				t.Errorf("%s: ciTextKeys 里的 %s 已不再参与唯一性判定（或已不是文本列）：白名单条目要删", tbl.file, name)
				continue
			}
			if proof != ciProofPoolKey && proof != ciProofULID && proof != ciProofFrozen {
				t.Errorf("%s: ciTextKeys[%s] = %q，不是本文件登记的三种证据之一", tbl.file, name, proof)
			}
			if proof == ciProofFrozen {
				frozen = append(frozen, tbl.spec.table+"."+name)
			}
		}
		for _, col := range tbl.columns {
			switch {
			case !want[col.name]:
				if col.collate != "" {
					t.Errorf("%s.%s: 不参与唯一性/指纹判定的列不该单独设 COLLATE = %s（会让该列比较变成逐字节敏感）",
						tbl.file, col.name, col.collate)
				}
			case col.collate == migBinCollation:
				if _, debt := tbl.spec.ciTextKeys[col.name]; debt {
					t.Errorf("%s.%s: 已是 %s，ciTextKeys 白名单条目要一并删掉", tbl.file, col.name, migBinCollation)
				}
			case col.collate != "":
				t.Errorf("%s.%s: 列级 COLLATE = %s，既不是 %s 也不是表级 %s：请写清到底要不要逐字节比对",
					tbl.file, col.name, col.collate, migBinCollation, migTableCollation)
			default:
				if _, debt := tbl.spec.ciTextKeys[col.name]; !debt {
					t.Errorf("%s.%s(%s): 参与唯一性判定却沿用表级 %s（PAD SPACE + 大小写不敏感，"+
						"只差大小写或尾随空格的两个键会被折成同一个）：必须列级 %s，或把它登记进 ciTextKeys 并给出可实跑的证据",
						tbl.file, col.name, col.typeToken, migTableCollation, migBinCollation)
				}
			}
		}
	}
	// 债务总量钉死：新增一处"证明不了的 CI 唯一键文本列"、或有人用 ALTER 迁移修好了却不删登记，都要失败。
	sort.Strings(frozen)
	wantFrozen := []string{"recall_pool_version.batch_id", "recall_request_log.request_id"}
	if strings.Join(frozen, ",") != strings.Join(wantFrozen, ",") {
		t.Errorf("证明不了、只能钉住的字符序债务集合漂移了\ngot:  %v\nwant: %v\n"+
			"（新表的新唯一键文本列一律要 %s；已收紧的要从 ciTextKeys 删掉并同步本清单）",
			frozen, wantFrozen, migBinCollation)
	}
}

// migPoolKeyIsByteExact 实跑 ValidatePoolKey：合法键必须放行，而任何与它只差大小写/空格的
// 写法必须被拒。两条同时成立，才说明「pool_key 留在 utf8mb4_unicode_ci 下也不会把两个
// 都能存进去的键折成同一个」—— 白名单的证据必须可执行，不能是注释。
func migPoolKeyIsByteExact(t *testing.T) {
	t.Helper()
	for _, c := range []struct {
		source int32
		key    string
	}{
		{SourceHot, "global"}, {SourceHot, "zone:7"},
		{SourceCold, "global"}, {SourceCold, "platform:2"},
		{SourceFollow, "mid:42"}, {SourceVector, "mid:42"},
		{SourceTag, "tag:9"}, {SourceCollab, "aid:1001"},
	} {
		if err := ValidatePoolKey(c.source, c.key); err != nil {
			t.Fatalf("样例池键 %q(source=%d) 本该合法，ValidatePoolKey 却拒绝：%v —— pool_key 走表级字符序的前提已不成立", c.key, c.source, err)
		}
		for _, variant := range []string{strings.ToUpper(c.key), strings.ToTitle(c.key), c.key + " ", " " + c.key, c.key + "\t"} {
			if err := ValidatePoolKey(c.source, variant); err == nil {
				t.Fatalf("ValidatePoolKey(%d, %q) 竟然放行：它与 %q 在 %s 下是同一个键，两个不同的池会被折成一个",
					c.source, variant, c.key, migTableCollation)
			}
		}
	}
}

// migPrefixedIDHasNoFoldableChars 实跑 idgen.Prefixed，证明「<固定前缀>_<26 位大写 ULID>」
// 这个值域在 utf8mb4_unicode_ci 下折不动：前缀是常量（不随取值变化，折叠不了两个不同值），
// 随机部分只含大写 Crockford 字符（大小写不敏感等于恒等），整体无空白（PAD SPACE 也用不上）。
// 于是值 -> 折叠后形态 是单射，唯一键不会把两个不同 ID 当成一个。
func migPrefixedIDHasNoFoldableChars(t *testing.T) {
	t.Helper()
	for _, prefix := range []string{"snap", "recall"} {
		id, err := idgen.Prefixed(prefix)
		if err != nil {
			t.Fatalf("idgen.Prefixed(%q): %v", prefix, err)
		}
		body, ok := strings.CutPrefix(id, prefix+"_")
		if !ok || len(body) != 26 {
			t.Fatalf("idgen.Prefixed(%q) = %q：不再是 \"%s_<26 位 ULID>\" 的形状，白名单证据要重估", prefix, id, prefix)
		}
		if strings.TrimSpace(id) != id {
			t.Fatalf("idgen.Prefixed(%q) = %q：首尾带空白，%s 的 PAD SPACE 会把它和另一个值折成同一个", prefix, id, migTableCollation)
		}
		for i := 0; i < len(body); i++ {
			if c := body[i]; !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				t.Fatalf("idgen.Prefixed(%q) 的 ULID 部分 %q 含 %q：小写或符号字符让 %s 的折叠不再安全，该列必须 %s",
					prefix, body, c, migTableCollation, migBinCollation)
			}
		}
	}
}

// TestNoDefaultColumnsAreExplicitlyInserted 校验「NOT NULL 且无 DEFAULT」的列一定被 INSERT 显式写入，
// 且测试登记的 INSERT 列清单与源码逐列一致（防测试副本先于代码腐化）。
//
//	MySQL 严格模式下漏列是 1364/1048；而唯一键列与 TEXT 列恰恰不能给默认值
//
// （唯一键给 DEFAULT ” 等于允许第二条空串，TEXT 禁止 DEFAULT）。
func TestNoDefaultColumnsAreExplicitlyInserted(t *testing.T) {
	for _, tbl := range migLoadTables(t) {
		corpus, _ := migSQLText(t, tbl.spec.goFile)
		ins := migInsertColumns(t, corpus, tbl.spec.table)
		if strings.Join(ins, ",") != strings.Join(tbl.spec.insertCols, ",") {
			t.Errorf("%s: model 的 INSERT 列清单 = %v，测试期望 %v（任一侧改动都要同步）", tbl.file, ins, tbl.spec.insertCols)
		}
		set := map[string]bool{}
		for _, c := range ins {
			set[c] = true
		}
		for _, col := range tbl.columns {
			if col.notNull && !col.hasDefault && !col.autoIncrement && !set[col.name] {
				t.Errorf("%s.%s: NOT NULL 且无 DEFAULT，但 INSERT 没写它（严格模式下这条 INSERT 会失败）", tbl.file, col.name)
			}
			if col.autoIncrement && migIndexOf(tbl.primary, col.name) < 0 {
				t.Errorf("%s.%s: 自增列必须是主键的一部分（InnoDB 要求 AUTO_INCREMENT 被索引，"+
					"放到别处会让聚簇顺序与插入顺序脱钩）", tbl.file, col.name)
			}
		}
		for _, c := range tbl.spec.noDefault {
			col := migColumnByName(t, tbl, c)
			if col.hasDefault {
				t.Errorf("%s.%s: 该列必须无 DEFAULT（唯一键列/TEXT 列），实际 DEFAULT %s", tbl.file, c, col.defaultVal)
			}
			if !col.notNull {
				t.Errorf("%s.%s: 必须 NOT NULL", tbl.file, c)
			}
		}
	}
}

// TestColumnWidthsComeFromModelLimits 校验列宽逐列来自 model 侧的真实上限，而不是惯例：
// 受硬上限约束的列必须与常量完全一致（宽了是浪费、窄了会截断），
// 变长内容列必须 >= model 能写出的最坏值。
func TestColumnWidthsComeFromModelLimits(t *testing.T) {
	// 事件 ID 由 idgen.ULID() 生成：长度以实际产出为准，不在测试里写死 26。
	ulid, err := idgen.ULID()
	if err != nil {
		t.Fatalf("idgen.ULID: %v", err)
	}
	scopeMax := 0
	for _, s := range []string{IdempotencyScopeUpsertPoolItems, IdempotencyScopePublishPoolVersion,
		IdempotencyScopeRollbackPoolVersion} {
		if len(s) > scopeMax {
			scopeMax = len(s)
		}
	}
	byTable := map[string]migTable{}
	for _, tbl := range migLoadTables(t) {
		byTable[tbl.spec.table] = tbl
	}

	exact := []struct {
		table, column string
		want          int
		why           string
	}{
		{"recall_pool", "pool_key", MaxPoolKeyLen, "model.MaxPoolKeyLen（ValidatePoolKey 超限即拒）"},
		{"recall_pool_version", "pool_key", MaxPoolKeyLen, "同上，三张表的 pool_key 必须同宽"},
		{"recall_pool_current", "pool_key", MaxPoolKeyLen, "同上"},
		{"recall_idempotency", "idempotency_key", MaxIdempotencyKeyLen, "MaxIdempotencyKeyLen 的注释就写着与迁移列宽一致"},
		{"recall_idempotency", "request_hash", RequestHashLen, "RequestHashLen：sha256 hex 定长，长度不等直接判指纹不符"},
	}
	mini := []struct {
		table, column string
		want          int
		why           string
	}{
		{"recall_idempotency", "scope", scopeMax, "最长的作用域字面量"},
		{"recall_idempotency", "state", len(string(idempotency.StateSucceeded)), "common/idempotency 状态词表最长值"},
		{"recall_idempotency", "last_error", 512, "idempotency.go MarkFailed 的按字节裁剪上限"},
		{"recall_outbox", "last_error", 512, "outbox.go MarkFailed 的按字节裁剪上限"},
		{"recall_outbox", "event_id", len(ulid), "eventenvelope.New 用 idgen.ULID 生成 event_id"},
		{"recall_idempotency", "event_id", len(ulid), "回填 recall_outbox.event_id，两列必须同宽"},
		{"recall_outbox", "event_type", len(EventPoolPublished), "model.EventPoolPublished"},
		{"recall_outbox", "aggregate_type", len(AggregateTypePool), "model.AggregateTypePool"},
		// aggregateIDOf 是 "%d:%s:%d"：int32 最坏 11 字符 + ':' + pool_key + ':' + int64 最坏 20 字符。
		{"recall_outbox", "aggregate_id", 11 + 1 + MaxPoolKeyLen + 1 + 20, "logic aggregateIDOf 的最坏长度"},
	}

	for _, e := range exact {
		tbl, ok := byTable[e.table]
		if !ok {
			t.Fatalf("未加载表 %s", e.table)
		}
		col := migColumnByName(t, tbl, e.column)
		if got, isChar := migWidthOf(col.typeToken); !isChar || got != e.want {
			t.Errorf("%s.%s: 列宽 %q 应为 %d（依据 %s）", e.table, e.column, col.typeToken, e.want, e.why)
		}
	}
	for _, m := range mini {
		tbl, ok := byTable[m.table]
		if !ok {
			t.Fatalf("未加载表 %s", m.table)
		}
		col := migColumnByName(t, tbl, m.column)
		if got, isChar := migWidthOf(col.typeToken); !isChar || got < m.want {
			t.Errorf("%s.%s: 列宽 %q 小于 model 侧最坏值 %d（依据 %s）", m.table, m.column, col.typeToken, m.want, m.why)
		}
	}

	// outbox 只存 event_type，投递 topic 由 event_type + schema_version 拼出来；
	// 三者不自洽 = 事件写进了表却投不到约定 topic。
	if got := eventenvelope.Topic(EventPoolPublished, PoolVersionSchemaVersion); got != TopicPoolPublished {
		t.Errorf("TopicPoolPublished = %s，与 event_type+schema_version 推出的 %s 不一致", TopicPoolPublished, got)
	}
}

// TestColumnDefaultsMatchModelConstants 校验「DDL 默认值 == model 侧同一语义常量」：
// 两者分叉时，绕过 Go 写入的行（人工修数、补偿脚本）会落进没人解释的状态。
func TestColumnDefaultsMatchModelConstants(t *testing.T) {
	explain := map[string]string{
		"0":         "OutboxStatePending（Go int32 零值即待发布）",
		"1":         "VersionStateBuilding / PoolVersionSchemaVersion",
		"'pending'": "string(idempotency.StatePending)",
	}
	for _, tbl := range migLoadTables(t) {
		for col, want := range tbl.spec.defaultIs {
			c := migColumnByName(t, tbl, col)
			if !c.hasDefault {
				t.Errorf("%s.%s: 期望 DEFAULT %s（= %s），实际没有默认值", tbl.file, col, want, explain[want])
				continue
			}
			if migNormalize(c.defaultVal) != want {
				t.Errorf("%s.%s: DEFAULT = %s，期望 %s", tbl.file, col, c.defaultVal, want)
			}
			switch want {
			case "0":
				if OutboxStatePending != 0 {
					t.Errorf("%s.%s: 默认值 0 已不再等于 OutboxStatePending=%d", tbl.file, col, OutboxStatePending)
				}
			case "1":
				if PoolVersionSchemaVersion != 1 || VersionStateBuilding != 1 {
					t.Errorf("%s.%s: 默认值 1 与 PoolVersionSchemaVersion=%d / VersionStateBuilding=%d 分叉",
						tbl.file, col, PoolVersionSchemaVersion, VersionStateBuilding)
				}
			case "'pending'":
				if string(idempotency.StatePending) != "pending" {
					t.Errorf("%s.%s: 默认值 'pending' 与 StatePending=%q 分叉", tbl.file, col, idempotency.StatePending)
				}
			}
		}
	}
}

// TestTableOptionsAndHeaderSections 校验表级选项与文件头说明段：
// ENGINE=InnoDB、utf8mb4、表级默认字符序取本服务既有文件一致的 utf8mb4_unicode_ci、表级 COMMENT，
// 以及用途/数据所有者/幂等设计/索引依据/回滚/锁风险逐项必备。
func TestTableOptionsAndHeaderSections(t *testing.T) {
	for _, tbl := range migLoadTables(t) {
		up := strings.ToUpper(migNormalize(tbl.options))
		for _, want := range []string{"ENGINE = INNODB", "DEFAULT CHARSET = UTF8MB4",
			"COLLATE = UTF8MB4_UNICODE_CI", "COMMENT ="} {
			if !strings.Contains(up, want) {
				t.Errorf("%s: 表选项缺少 %s（实际 %q）", tbl.file, want, migNormalize(tbl.options))
			}
		}
		if strings.Contains(up, "UTF8MB4_0900") {
			t.Errorf("%s: 表级字符序用了 0900 系，本服务既有 4 个文件都是 %s", tbl.file, migTableCollation)
		}
		for _, seg := range tbl.spec.headerMust {
			if !migHeaderHas(tbl.header, seg) {
				t.Errorf("%s: 文件头缺少「%s」说明段", tbl.file, seg)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// 小工具
// -----------------------------------------------------------------------------

func (s migTableSpec) sortedUniqueKeys() []migKeyDef { return migKeyDefs(s.uniqueKeys) }
func (s migTableSpec) sortedIndexes() []migKeyDef    { return migKeyDefs(s.indexes) }

func migKeyDefs(m map[string][]string) []migKeyDef {
	out := make([]migKeyDef, 0, len(m))
	for name, cols := range m {
		out = append(out, migKeyDef{name: name, columns: cols})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func migColumnByName(t *testing.T, tbl migTable, name string) migColumn {
	t.Helper()
	for _, c := range tbl.columns {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("%s: DDL 里没有列 %s", tbl.file, name)
	return migColumn{}
}

// migIsLobType 判定「大对象列」：只有这类列在 MySQL 里禁止 DEFAULT 字面量。
// VARCHAR/CHAR 不在此列 —— 把整个 text 族都当成 LOB 会让每个 VARCHAR ... DEFAULT ” 都报错。
func migIsLobType(sqlType string) bool {
	up := migBaseType(sqlType)
	return strings.HasSuffix(up, "TEXT") || strings.HasSuffix(up, "BLOB") || up == "JSON"
}

// migIsTextType 判定有字符序（collation）的列：只有它们参与"折叠 vs 逐字节"的取舍。
// 整型/浮点列没有 collation，要求它们写 utf8mb4_bin 是伪需求。
func migIsTextType(sqlType string) bool {
	up := migBaseType(sqlType)
	return strings.HasSuffix(up, "CHAR") || strings.HasSuffix(up, "TEXT") ||
		strings.HasSuffix(up, "BINARY") || up == "ENUM" || up == "SET" || up == "JSON"
}

// migBaseType 去掉 UNSIGNED 后缀与 (n) 长度，返回大写基类型名。
func migBaseType(sqlType string) string {
	up := strings.ToUpper(strings.TrimSpace(sqlType))
	if i := strings.IndexAny(up, "( "); i >= 0 {
		up = up[:i]
	}
	return up
}

func migFamily(sqlType string) string {
	up := strings.ToUpper(strings.TrimSpace(sqlType))
	switch {
	case strings.HasPrefix(up, "TINYINT"), strings.HasPrefix(up, "SMALLINT"), strings.HasPrefix(up, "MEDIUMINT"),
		strings.HasPrefix(up, "INT"), strings.HasPrefix(up, "INTEGER"), strings.HasPrefix(up, "BIGINT"):
		return "int"
	case strings.HasPrefix(up, "DOUBLE"), strings.HasPrefix(up, "FLOAT"), strings.HasPrefix(up, "DECIMAL"),
		strings.HasPrefix(up, "NUMERIC"):
		return "float"
	case strings.HasPrefix(up, "CHAR"), strings.HasPrefix(up, "VARCHAR"), strings.HasPrefix(up, "TINYTEXT"),
		strings.HasPrefix(up, "TEXT"), strings.HasPrefix(up, "MEDIUMTEXT"), strings.HasPrefix(up, "LONGTEXT"),
		strings.HasPrefix(up, "JSON"), strings.HasPrefix(up, "ENUM"):
		return "text"
	case strings.HasPrefix(up, "DATETIME"), strings.HasPrefix(up, "TIMESTAMP"), strings.HasPrefix(up, "DATE"):
		return "time"
	case strings.HasPrefix(up, "BLOB"), strings.HasPrefix(up, "MEDIUMBLOB"), strings.HasPrefix(up, "TINYBLOB"),
		strings.HasPrefix(up, "LONGBLOB"), strings.HasPrefix(up, "BINARY"), strings.HasPrefix(up, "VARBINARY"):
		return "blob"
	default:
		return "other:" + up
	}
}

func migKindMatchesFamily(kind reflect.Kind, fam string) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Bool:
		// 本服务所有整型列都读进有符号 Go 字段；UNSIGNED 只用在自增主键上（id 恒为正）。
		return fam == "int"
	case reflect.Float32, reflect.Float64:
		return fam == "float"
	case reflect.String:
		return fam == "text" || fam == "blob"
	default:
		return false
	}
}

func migWidthOf(sqlType string) (int, bool) {
	m := migWidthRe.FindStringSubmatch(sqlType)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

func migMissing(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if !set[x] {
			out = append(out, x)
		}
	}
	return out
}

func migIndexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

func migSortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// migHeaderHas 判定头注释里是否出现某个说明段标题（形如 "-- 用途："）。
func migHeaderHas(header, seg string) bool {
	for _, line := range strings.Split(header, "\n") {
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "--"))
		if strings.HasPrefix(text, seg) {
			return true
		}
	}
	return false
}
