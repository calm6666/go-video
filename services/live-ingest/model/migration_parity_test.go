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

// 本文件把「model 的 SQL 与 deploy/migrations/live-ingest 逐列一致」这条约定变成可执行门禁。
//
// 为什么必须是测试而不是人工比对（AGENTS.md §9）：
//   - 每张表的列全集同时出现在三处文本里：结构体 db tag、`xColumns` SELECT 常量、INSERT 列清单。
//     任一处漂移都不会让编译失败，只会在运行期表现为 "Unknown column" 或字段错位扫描；
//   - 本服务的幂等与不变量**全部**落在数据库唯一索引上（logic 层刻意不用 Redis 兜底）：
//     uniq_request_id / uniq_publish_request / uniq_stream_seq / uniq_report_id / uniq_nonce /
//     uniq_start_event / uniq_stream_episode。少一个键或加错列，故障形态不是启动失败，
//     而是「同一个请求产生两次副作用」或「事件重号被静默吞掉」——最贵的那种；
//   - 迁移本轮**没有**在任何 MySQL 实例上执行过（见服务 README），结构正确性只能静态证明。
//
// 测试不连数据库：只读 SQL 文本，与 model 的常量/结构体 tag 比对。
// 结构照抄 services/live-room/model/migration_parity_test.go（同一套约定的先行实现），
// 差异只有一处：本服务一个迁移文件建多张强关联表（3 个文件 9 张表），
// 因此「一表一文件」的断言改成「表 -> 文件」的显式映射。

const migrationsRelDir = "deploy/migrations/live-ingest"

// tableSpec 声明一张 model 表与它的迁移表定义的对应关系。
type tableSpec struct {
	table string // SQL 里的表名
	file  string // 所在迁移文件
	// row 是行结构体样例；model 读写的列全集取自它的 db tag（含声明顺序）。
	row           any
	selectColumns string // 拼进 SELECT 的列常量原值
	// insertColumns 是 INSERT 语句显式列出的列（逗号分隔）。
	// 它是 model 真正「会写」的列，与 db tag 全集差集必须能被 explainInsertOmits 解释，
	// 否则就是「结构体有字段、INSERT 永远不写」的静默零值列。
	insertColumns string
	// insertOmits 说明 INSERT 刻意不列的列（自增主键与有 DEFAULT 的派生列）。
	insertOmits map[string]string
	// primaryKey 是 model 定位单行 / UPSERT 冲突所使用的列组合。
	primaryKey []string
	// uniqueKeys 是幂等与数据库级不变量依赖的唯一索引：键名 -> 列组合（顺序敏感）。
	uniqueKeys map[string][]string
	// requiredIndexes 是 model 的 WHERE / ORDER BY 依赖的普通索引名；
	// 列组合在 expectedIndexColumns 里单独声明，两者都要对上。
	requiredIndexes []string
	// declaredOnly 是「SQL 里有、model 当前没有查询依赖」的索引及理由：
	// 留空即视为纯写放大，用例直接失败（不允许悄悄挂着没人负责的索引）。
	declaredOnly map[string]string
	// sqlOnly 是「SQL 有、model 不读写」的列及保留理由。留空即本表零冗余列。
	sqlOnly map[string]string
	// nullable 列出允许为 NULL 的列（本服务只有幂等键可空：靠 NULL 逃逸唯一性）。
	nullable []string
	// requiredNoDefault 列出刻意不给 DEFAULT 的必填列：漏传必须在 MySQL 侧报错，
	// 而不是被默认值静默兜成 0/空串。
	requiredNoDefault []string
}

func tableSpecs() []tableSpec {
	return []tableSpec{
		{
			table:         "live_stream_key",
			file:          "000001_create_live_ingest_key_tables.sql",
			row:           StreamKey{},
			selectColumns: streamKeyColumns,
			insertColumns: "stream_name, key_hash, key_ref, key_tail, room_id, session_id, anchor_mid, protocol_mask, " +
				"state, version, prev_key_id, rotate_to_key_id, current_stream_id, max_streams, expire_at, grace_until, " +
				"reason, request_id, trace_id, ctime, mtime",
			insertOmits: map[string]string{
				"key_id": "自增主键，由 LastInsertId 回读",
			},
			primaryKey:      []string{"key_id"},
			uniqueKeys:      map[string][]string{"uniq_key_hash": {"key_hash"}, "uniq_request_id": {"request_id"}},
			requiredIndexes: []string{"idx_stream_name_state", "idx_room_state", "idx_anchor_state", "idx_state_expire"},
			// 签发路径的每一列都是显式写值（IssueStreamKey 组装完整行），
			// 但 stream_name / key_hash / room_id / request_id 是「业务上不可能没有」的列：
			// 给它们 DEFAULT 会让漏传变成一条语义错误的密钥行，必须让 MySQL 直接报错。
			requiredNoDefault: []string{"stream_name", "key_hash", "room_id", "request_id"},
		},
		{
			table:         "live_ingest_node",
			file:          "000001_create_live_ingest_key_tables.sql",
			row:           IngestNode{},
			selectColumns: ingestNodeColumns,
			insertColumns: "node_id, name, region, protocol_mask, endpoint_rtmp, endpoint_srt, endpoint_webrtc, " +
				"state, capacity_streams, active_streams, health_score, last_heartbeat_at, labels, ctime, mtime",
			insertOmits: map[string]string{},
			// 主键 node_id 由运维分配：UPSERT 的冲突键就是它，注册即幂等。
			primaryKey:      []string{"node_id"},
			requiredIndexes: []string{"idx_state_health", "idx_state_heartbeat", "idx_region_state"},
			// 默认 state=3（OFFLINE）是刻意的：未登记完成的节点不得进分配池。
			requiredNoDefault: []string{"node_id"},
		},
		{
			table:         "live_node_assignment",
			file:          "000001_create_live_ingest_key_tables.sql",
			row:           NodeAssignment{},
			selectColumns: nodeAssignmentColumns,
			insertColumns: "request_id, stream_id, room_id, key_id, node_id, protocol, state, score, prev_node_id, " +
				"reason, release_reason, assigned_at, released_at, trace_id, ctime, mtime",
			insertOmits: map[string]string{
				"assignment_id": "自增主键，由 LastInsertId 回读",
			},
			primaryKey:      []string{"assignment_id"},
			uniqueKeys:      map[string][]string{"uniq_request_id": {"request_id"}},
			requiredIndexes: []string{"idx_stream_state", "idx_node_state", "idx_room_ctime"},
			// request_id 是分配重放的唯一防线；stream_id/node_id 缺任一都无法归因容量。
			requiredNoDefault: []string{"request_id", "stream_id", "node_id"},
		},
		{
			table:         "live_stream",
			file:          "000002_create_live_stream_tables.sql",
			row:           Stream{},
			selectColumns: streamColumns,
			insertColumns: "stream_id, key_id, stream_name, room_id, session_id, anchor_mid, protocol, node_id, state, " +
				"seq, publish_request_id, publish_started_at, state_changed_at, last_heartbeat_at, " +
				"interrupted_total_seconds, interruption_count, stop_reason, stop_detail, health_state, " +
				"health_reported_at, video_bitrate_bps, audio_bitrate_bps, fps_x100, packet_loss_ppm, trace_id, ctime, mtime",
			insertOmits: map[string]string{},
			primaryKey:  []string{"stream_id"},
			uniqueKeys:  map[string][]string{"uniq_publish_request": {"publish_request_id"}},
			// idx_name_state_ctime 支撑 FindActiveByStreamName：CDN 回调只有 stream_name，
			// 没有它就会在最大热表上按 128 字符列全表扫（回调是公网可达路径，必须走索引）。
			requiredIndexes: []string{"idx_room_state_ctime", "idx_name_state_ctime", "idx_key_state", "idx_state_heartbeat", "idx_anchor_state", "idx_node_state"},
			// stream_id（ULID 由 logic 生成）/room_id/publish_request_id 漏给即脏数据。
			requiredNoDefault: []string{"stream_id", "room_id", "publish_request_id"},
		},
		{
			table:         "live_stream_interruption",
			file:          "000002_create_live_stream_tables.sql",
			row:           StreamInterruption{},
			selectColumns: interruptionColumns,
			insertColumns: "stream_id, room_id, episode_no, node_id, started_at, ended_at, duration_seconds, " +
				"end_reason, reconnect_attempts, start_event_id, end_event_id, reason, ctime, mtime",
			insertOmits: map[string]string{
				"interruption_id": "自增主键，由 LastInsertId 回读",
			},
			primaryKey: []string{"interruption_id"},
			// uniq_start_event：一个事件只开一个断流区间（重放不重复开）；
			// uniq_stream_episode：episode_no 同事务取 MAX+1，靠它挡住并发开段。
			uniqueKeys: map[string][]string{
				"uniq_start_event":    {"start_event_id"},
				"uniq_stream_episode": {"stream_id", "episode_no"},
			},
			requiredIndexes: []string{"idx_stream_open", "idx_room_started"},
			// 归属流与开启事件 ID 是断流区间的身份，漏给必须报错而不是写进空串。
			requiredNoDefault: []string{"stream_id", "start_event_id"},
		},
		{
			table:         "live_stream_health_report",
			file:          "000002_create_live_stream_tables.sql",
			row:           StreamHealthReport{},
			selectColumns: healthReportColumns,
			insertColumns: "report_id, stream_id, node_id, video_bitrate_bps, audio_bitrate_bps, fps_x100, " +
				"packet_loss_ppm, rtt_ms, sample_window_seconds, health_state, occurred_at, trace_id, ctime",
			insertOmits: map[string]string{
				"id": "自增主键，由 LastInsertId 回读",
			},
			primaryKey:      []string{"id"},
			uniqueKeys:      map[string][]string{"uniq_report_id": {"report_id"}},
			requiredIndexes: []string{"idx_stream_occurred"},
			// report_id 是本服务仅有的可空列之一：内部采样（无调用方幂等键）写 NULL，
			// MySQL 唯一索引允许多个 NULL，因此内部行之间永不互撞。
			// 反过来说：一旦给它 DEFAULT ''，第二条内部采样就会撞 uniq_report_id，
			// 健康上报会开始随机失败——这条可空性是功能而不是疏忽。
			nullable:          []string{"report_id"},
			requiredNoDefault: []string{"stream_id"},
		},
		{
			table:         "live_stream_event",
			file:          "000003_create_live_stream_event_tables.sql",
			row:           StreamEvent{},
			selectColumns: streamEventColumns,
			insertColumns: "event_id, stream_id, room_id, session_id, seq, from_state, to_state, node_id, " +
				"interruption_id, interrupted_seconds, stop_reason, report_id, source, reason, occurred_at, trace_id, ctime",
			insertOmits: map[string]string{
				"id": "自增主键，落库顺序（发布器按 outbox.id 投递）",
			},
			primaryKey: []string{"id"},
			// 三条唯一键合起来才是事件表的正确性：
			// uniq_event_id 供消费方去重、uniq_stream_seq 挡同流重号、uniq_report_id 让上报可重放。
			uniqueKeys: map[string][]string{
				"uniq_event_id":   {"event_id"},
				"uniq_stream_seq": {"stream_id", "seq"},
				"uniq_report_id":  {"report_id"},
			},
			requiredIndexes: []string{"idx_occurred"},
			declaredOnly: map[string]string{
				// 按房间捞事件是运营排障入口（本服务只提供按流查询，ListStreamEvents 以 stream_id 为准），
				// 保留它是为了让 live-room 侧的对账工具能直查；无 model 查询依赖。
				"idx_room_ctime": "运营/消费方按 room_id 捞事件（live-room 对账工具直查），model 无查询依赖",
			},
			// report_id 可空是刻意的：内部迁移（健康触发/扫描器/级联停流）没有调用方幂等键，
			// 写 NULL 才不会彼此撞 uniq_report_id（见 streamevent.go 的 nullableString）。
			nullable:          []string{"report_id"},
			requiredNoDefault: []string{"event_id", "stream_id", "seq", "to_state"},
		},
		{
			table:         "live_ingest_outbox",
			file:          "000003_create_live_stream_event_tables.sql",
			row:           EventOutbox{},
			selectColumns: outboxColumns,
			insertColumns: "event_id, event_type, schema_version, aggregate_type, aggregate_id, stream_id, room_id, " +
				"seq, payload, state, retry_count, next_retry_at, last_error, occurred_at, ctime, mtime",
			insertOmits: map[string]string{
				"id": "自增主键，发布器按它升序投递（同流顺序保证）",
			},
			primaryKey:      []string{"id"},
			uniqueKeys:      map[string][]string{"uniq_event_id": {"event_id"}},
			requiredIndexes: []string{"idx_state_retry", "idx_state_occurred"},
			declaredOnly: map[string]string{
				// 按流对账位点是补偿路径（logic 现在按 state 聚合 Checkpoint），列已冗余备查。
				"idx_stream_id": "按 stream_id 对账事件位点（补偿工具直查），model 当前按 state 聚合",
			},
			// payload 是 TEXT：MySQL 禁止给它 DEFAULT，且事件没有「空负载」这种合法状态。
			requiredNoDefault: []string{"event_id", "payload"},
		},
		{
			table:         "live_cdn_callback",
			file:          "000003_create_live_stream_event_tables.sql",
			row:           CdnCallback{},
			selectColumns: cdnCallbackColumns,
			insertColumns: "nonce, domain, event_type, stream_name, stream_id, key_id, room_id, signature_hash, " +
				"client_ip_hash, raw_params_digest, verify_result, suggest_state, handled, reason, occurred_at, " +
				"verified_at, trace_id, ctime",
			insertOmits: map[string]string{
				"id": "自增主键，由 LastInsertId 回读并作为 callback_log_id 回带",
			},
			primaryKey:      []string{"id"},
			uniqueKeys:      map[string][]string{"uniq_nonce": {"nonce"}},
			requiredIndexes: []string{"idx_stream_occurred", "idx_occurred"},
			declaredOnly: map[string]string{
				// 回调排障的第一手输入是厂商给的 stream_name（那时还没解析出 stream_id），
				// model 只按 stream_id 回看（ListRecentByStream），故这条留给排障查询。
				"idx_name_ctime": "按厂商回传的 stream_name 捞留证（排障直查），model 按 stream_id 回看",
			},
			// nonce 是防重放的唯一锚点，空 nonce 的回调不留证（logic 先拒）；
			// 域名为空则留证毫无归因价值。
			requiredNoDefault: []string{"nonce"},
		},
	}
}

// expectedIndexColumns 写的是「model 的查询实际需要什么」，不是「SQL 里有什么」，
// 所以索引改名、丢列、换序都会在这里报错，而不是等到线上 filesort。
//
// 按表分键：本服务的索引名是可复用的（idx_state_heartbeat 在 node 与 stream 两张表
// 都在，语义却不同），用全局索引名做键会让一张表的漏配被另一张表悄悄顶掉。
// 每个 requiredIndexes 项必须在这里有一条非空列组合，否则视为「无人认领的索引」。
func expectedIndexColumns() map[string]map[string][]string {
	return map[string]map[string][]string{
		"live_stream_key": {
			"idx_stream_name_state": {"stream_name", "state", "key_id"}, // FindCurrentByName（ACTIVE/ROTATING + key_id 倒序）
			"idx_room_state":        {"room_id", "state"},               // ListByFilter/CountByFilter
			"idx_anchor_state":      {"anchor_mid", "state"},            // ListByFilter(anchor_mid)
			"idx_state_expire":      {"state", "expire_at"},             // MarkExpiredByScan
		},
		"live_ingest_node": {
			"idx_state_health":    {"state", "health_score"},      // ListCandidates（在线 + 健康分排序）/ListByFilter
			"idx_state_heartbeat": {"state", "last_heartbeat_at"}, // MarkOfflineByHeartbeatTimeout
			"idx_region_state":    {"region", "state"},            // ListCandidates(region) / ListByFilter(region)
		},
		"live_node_assignment": {
			"idx_stream_state": {"stream_id", "state", "assignment_id"}, // FindActiveByStream
			"idx_node_state":   {"node_id", "state"},                    // CountActiveByNode / ListByFilter(node_id)
			"idx_room_ctime":   {"room_id", "ctime"},                    // ListByFilter(room_id)（容量对账按房间捞）
		},
		"live_stream": {
			"idx_room_state_ctime": {"room_id", "state", "ctime"},     // FindActiveByRoom
			"idx_name_state_ctime": {"stream_name", "state", "ctime"}, // FindActiveByStreamName（CDN 回调归属）
			"idx_key_state":        {"key_id", "state"},               // ListActiveByKey / CountActiveStreams
			"idx_state_heartbeat":  {"state", "last_heartbeat_at"},    // ListStaleActive / ListByFilter 排序
			"idx_anchor_state":     {"anchor_mid", "state"},           // ListByFilter(anchor_mid)
			"idx_node_state":       {"node_id", "state"},              // ListByFilter(node_id)
		},
		"live_stream_interruption": {
			"idx_stream_open":  {"stream_id", "ended_at", "started_at"}, // FindOpenByStream / BumpReconnectAttempts / ListByFilter
			"idx_room_started": {"room_id", "started_at"},               // ListByFilter(room_id) 统计断流
		},
		"live_stream_health_report": {
			"idx_stream_occurred": {"stream_id", "occurred_at"}, // ListRecent / AggregateWindow 窗口聚合
		},
		"live_stream_event": {
			"idx_occurred": {"occurred_at"}, // Prune 按时间清历史
		},
		"live_ingest_outbox": {
			"idx_state_retry":    {"state", "next_retry_at", "id"}, // ClaimDue（可投递 + 按 id 升序）
			"idx_state_occurred": {"state", "occurred_at"},         // Checkpoint / Prune
		},
		"live_cdn_callback": {
			"idx_stream_occurred": {"stream_id", "occurred_at"}, // ListRecentByStream
			"idx_occurred":        {"occurred_at"},              // Prune
		},
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

func (t *sqlTable) column(name string) *sqlColumn {
	for i := range t.columns {
		if t.columns[i].name == name {
			return &t.columns[i]
		}
	}
	return nil
}

// findMigrationsDir 从当前目录向上找仓库根的 deploy/migrations/live-ingest。
// 不写死相对层数：`go test ./services/live-ingest/model` 与在 model 目录内直接跑 cwd 不同。
func findMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "deploy", "migrations", "live-ingest")
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

// parsedFile 把文件切成「注释头」与「可执行 SQL 行」两部分。
// 所有禁止项检查只看可执行行：头注释里正常会出现「不建 FOREIGN KEY」「不 TRUNCATE」
// 这类说明文字，按全文匹配会把合规文件判成违规。
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
				if _, dup := out[name]; dup {
					t.Fatalf("%s: 表 %s 在多个迁移文件里重复建表", base, name)
				}
				cur = &sqlTable{table: name, file: base, indexes: map[string][]string{}, header: pf.header}
			case cur != nil && strings.HasPrefix(trimmed, ")"):
				cur.options = trimmed
				out[cur.table] = cur
				cur = nil
			case cur != nil:
				parseTableLine(t, cur, base, trimmed)
			default:
				t.Fatalf("%s: 出现 CREATE TABLE 之外的可执行语句 %q —— 本目录每个文件只做建表", base, trimmed)
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
			t.Fatalf("%s: 表 %s 的列 %s 重复声明", file, cur.table, name)
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
		if len(cur.primary) > 0 {
			t.Fatalf("%s: 表 %s 声明了多个主键", file, cur.table)
		}
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

func (t *sqlTable) addIndex(ti *testing.T, file, name string, cols []string, unique bool) {
	ti.Helper()
	if name == "" {
		ti.Fatalf("%s: 索引缺名字: %v", file, cols)
	}
	if _, dup := t.indexes[name]; dup {
		ti.Fatalf("%s: 索引名 %s 重复声明", file, name)
	}
	t.indexes[name] = cols
	t.indexOrder = append(t.indexOrder, name)
	if unique {
		t.uniqueKeys = append(t.uniqueKeys, name)
	}
	for _, c := range cols {
		if !t.hasColumn(c) {
			ti.Fatalf("%s: 索引 %s 引用了未声明的列 %s", file, name, c)
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

// TestModelColumnsMatchMigrationColumns 逐列比对 model 结构体与迁移表定义：
//   - model 用到而迁移没有 -> 运行期 Unknown column；
//   - 迁移有而 model 不读写 -> 必须在 sqlOnly 里登记理由，否则是无人维护的死列。
func TestModelColumnsMatchMigrationColumns(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl, ok := tables[spec.table]
		if !ok {
			t.Fatalf("迁移里没有表 %s（应在 %s）", spec.table, spec.file)
		}
		if tbl.file != spec.file {
			t.Errorf("%s: 实际在 %s，model 头注记的是 %s", spec.table, tbl.file, spec.file)
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

// TestInsertColumnsCoveredAndOmittedExplained 校验 INSERT 列清单：
//   - 每个显式列出的列都必须真实存在于迁移里（写错一个字母就是一次运行期失败）；
//   - db tag 全集里没被 INSERT 写到的列，必须逐列说明为什么可以不写
//     （自增主键、或由 DEFAULT 承载且语义上确实「不适用」）。
//
// 这一条挡住的是最隐蔽的一类漂移：给结构体与 SELECT 加了字段，却忘了改 INSERT，
// 于是列永远取默认值，读的时候又「看起来一切正常」。
func TestInsertColumnsCoveredAndOmittedExplained(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		insertCols := splitCols(spec.insertColumns)
		if len(insertCols) == 0 {
			t.Errorf("%s: tableSpec 未登记 INSERT 列清单", spec.table)
			continue
		}
		insertSet := map[string]bool{}
		for _, c := range insertCols {
			insertSet[c] = true
			if !tbl.hasColumn(c) {
				t.Errorf("%s: INSERT 写了迁移里不存在的列 %s", spec.table, c)
			}
		}
		for _, c := range dbTagColumns(t, spec.row) {
			if insertSet[c] {
				continue
			}
			if reason := spec.insertOmits[c]; strings.TrimSpace(reason) != "" {
				continue
			}
			t.Errorf("%s: 列 %s 在结构体与 SELECT 里，却不在 INSERT 列清单里，且未说明原因——它会永远落默认值", spec.table, c)
		}
		for c, reason := range spec.insertOmits {
			if !strings.Contains(reason, "主键") && !strings.Contains(reason, "默认") {
				t.Errorf("%s: insertOmits[%s] 的理由不成立：%q", spec.table, c, reason)
			}
			if insertSet[c] {
				t.Errorf("%s: 列 %s 既在 INSERT 清单里又登记为「不写」，说明表意已过期", spec.table, c)
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
			continue // 数量差异由列集合用例报，这里不重复刷噪音
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
			t.Errorf("%s: 主键是 (%s)，model 依赖 (%s)。live_ingest_node 的 UPSERT 以主键 node_id 为冲突键，"+
				"改主键等于改幂等语义", spec.table, got, want)
		}
		for name, wantCols := range spec.uniqueKeys {
			got, ok := tbl.indexes[name]
			if !ok {
				t.Errorf("%s: 缺唯一索引 %s（列组合 %v）——它是幂等/不变量的最终防线，logic 层刻意不用 Redis 兜底，"+
					"没有它就等于没有幂等", spec.table, name, wantCols)
				continue
			}
			if strings.Join(got, ",") != strings.Join(wantCols, ",") {
				t.Errorf("%s.%s: 列组合是 (%s)，期望 (%s)", spec.table, name, strings.Join(got, ","), strings.Join(wantCols, ","))
			}
		}
		// 可空唯一键只有一个合法用途：靠 NULL 逃逸唯一性承载「多行无幂等键」。
		// 若哪天有人给 report_id 补上 DEFAULT ''，第二条内部上报就会撞键，
		// 表现为「健康上报随机失败」，所以这里把它钉住。
		for name, cols := range spec.uniqueKeys {
			for _, col := range cols {
				c := tbl.column(col)
				if c == nil || c.notNull {
					continue
				}
				if !contains(spec.nullable, col) {
					t.Errorf("%s.%s: 唯一键列 %s 可空但未登记在 nullable —— 唯一性会静默失效", spec.table, name, col)
				}
			}
		}
	}
}

// TestModelIndexesExistInMigration 校验 model 的每条查询路径都有索引支撑，
// 反向也查：SQL 里声明了、既没有查询依赖又没登记理由的索引属于纯写放大。
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
			want, declared := expected[spec.table][idx]
			if !declared || len(want) == 0 {
				t.Errorf("%s: 索引 %s 未在 expectedIndexColumns 登记它支撑哪条查询", spec.table, idx)
				continue
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s.%s: 实际列 (%s) 与 model 查询所需 (%s) 不一致",
					spec.table, idx, strings.Join(got, ","), strings.Join(want, ","))
			}
		}
		for idx := range expected[spec.table] {
			if !contains(spec.requiredIndexes, idx) {
				t.Errorf("%s: expectedIndexColumns 声称 model 依赖索引 %s，但它不在 requiredIndexes —— "+
					"要么 model 真在用它（补进 requiredIndexes），要么这条声明是过期的", spec.table, idx)
			}
		}
		for _, idx := range tbl.indexOrder {
			// 唯一键承载不变量（幂等锚点、断流区间不重复开），不需要查询依赖背书。
			if _, isUnique := spec.uniqueKeys[idx]; isUnique {
				continue
			}
			if contains(spec.requiredIndexes, idx) {
				continue
			}
			if reason := spec.declaredOnly[idx]; strings.TrimSpace(reason) != "" {
				continue
			}
			t.Errorf("%s: 索引 %s(%v) 既不是唯一键、也没有 model 查询依赖、又未登记保留理由 —— "+
				"它是这张表的纯写放大来源，应删除或在 declaredOnly 写明谁在用它", spec.table, idx, tbl.indexes[idx])
		}
		for idx := range spec.declaredOnly {
			if _, ok := tbl.indexes[idx]; !ok {
				t.Errorf("%s: declaredOnly 登记的索引 %s 在迁移里已不存在 —— 理由该一起清掉", spec.table, idx)
			}
		}
	}
}

// TestNullabilityAndDefaultDiscipline 校验可空性与默认值口径。
//
// 两条约定（迁移文件头与 model 注释里都有对应说明）：
//  1. 除幂等键（live_stream_health_report.report_id、live_stream_event.report_id）外，
//     所有列一律 NOT NULL，用哨兵值（0 / 空串）表达「无」。NULL 会让
//     GREATEST(ended_at - started_at, 0)、`current_stream_id = ”` 这类条件变成
//     永远不成立的三值逻辑，也会让 COUNT/聚合静默漏行。
//  2. NOT NULL 列应当有 DEFAULT，**除非**它属于必填列：自增主键、主键本身，
//     或登记在 requiredNoDefault 里的业务主键/幂等键/TEXT 载荷。
//     这些列刻意不给 DEFAULT —— model 的 INSERT 总是显式给值，
//     万一漏给，MySQL 严格模式报错远好于静默写入 0/空串的脏行
//     （空 request_id 会撞唯一索引，但 DEFAULT ” 会让第二次签发直接冲突成「重放」，
//     把一个调用方 bug 伪装成幂等命中）。
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
			if !c.notNull && nullableOK[c.name] && c.hasDefault && !strings.Contains(c.dataType, "DEFAULT NULL") {
				t.Errorf("%s.%s: 登记为可空列却带非 NULL 的 DEFAULT，语义互相矛盾", spec.table, c.name)
			}
			exempt := c.autoIncrement || primary[c.name] || requiredNoDefault[c.name]
			if c.notNull && !c.hasDefault && !exempt {
				t.Errorf("%s.%s: NOT NULL 且无 DEFAULT，又不属于自增主键/主键/requiredNoDefault 三类必填列。实际定义 %q",
					spec.table, c.name, c.dataType)
			}
			if requiredNoDefault[c.name] && !c.notNull {
				t.Errorf("%s.%s: 登记为必填列但实际可空", spec.table, c.name)
			}
			if requiredNoDefault[c.name] && c.hasDefault {
				t.Errorf("%s.%s: 登记为必填列（漏传应当报错）却给了 DEFAULT，静默兜底会吞掉调用方 bug", spec.table, c.name)
			}
			// MySQL 禁止 TEXT/BLOB 设默认值：这类列必须走 requiredNoDefault。
			if isTextType(c.dataType) {
				if c.hasDefault {
					t.Errorf("%s.%s: TEXT/BLOB 不能设 DEFAULT（MySQL 会直接报错），实际 %q", spec.table, c.name, c.dataType)
				}
				if !requiredNoDefault[c.name] {
					t.Errorf("%s.%s: TEXT 列必然无 DEFAULT，应登记进 requiredNoDefault 说明它是必填载荷", spec.table, c.name)
				}
			}
			// 时间列口径：全部 BIGINT Unix 秒，不用 DATETIME（迁移头「时间列」段）。
			if strings.HasSuffix(c.name, "_at") && !strings.Contains(strings.ToUpper(c.dataType), "BIGINT") {
				t.Errorf("%s.%s: 时间列必须是 BIGINT Unix 秒，实际 %q", spec.table, c.name, c.dataType)
			}
			// 密钥纪律（AGENTS.md §6）：任何列名都不许承载明文密钥。
			for _, forbidden := range []string{"plain", "secret", "token", "password"} {
				if strings.Contains(strings.ToLower(c.name), forbidden) {
					t.Errorf("%s.%s: 列名含 %q ——本服务禁止入库任何明文密钥/厂商凭据，只允许 hash/ref/tail",
						spec.table, c.name, forbidden)
				}
			}
		}
		// 唯一索引的非可空列必须真的 NOT NULL —— 否则唯一性静默失效。
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

// TestEveryColumnCommentedAndTableOptions 校验列注释与表选项。
// 表选项不要求 COLLATE：本仓库 live-room 显式钉 utf8mb4_0900_ai_ci，
// live-media/live-ingest 沿用实例默认（差异见服务 README「已知缺口」）。
func TestEveryColumnCommentedAndTableOptions(t *testing.T) {
	tables := loadMigrationTables(t)
	for _, spec := range tableSpecs() {
		tbl := tables[spec.table]
		for _, c := range tbl.columns {
			if !c.hasComment {
				t.Errorf("%s.%s: 缺 COMMENT —— 本仓库要求每列有注释", spec.table, c.name)
			}
		}
		for _, want := range []string{"ENGINE=InnoDB", "DEFAULT CHARSET=utf8mb4", "COMMENT="} {
			if !strings.Contains(tbl.options, want) {
				t.Errorf("%s: 表选项缺 %s，实际 %q", spec.table, want, tbl.options)
			}
		}
	}
}

// TestMigrationHeaderSections 校验文件头四要素：用途 / 数据所有者 / 回滚 / 锁风险。
func TestMigrationHeaderSections(t *testing.T) {
	tables := loadMigrationTables(t)
	seen := map[string]bool{}
	for _, spec := range tableSpecs() {
		tbl, ok := tables[spec.table]
		if !ok || seen[tbl.file] {
			continue
		}
		seen[tbl.file] = true
		for _, want := range []string{"用途", "数据所有者", "回滚", "锁风险"} {
			if !strings.Contains(tbl.header, want) {
				t.Errorf("%s: 文件头缺「%s」段（AGENTS.md §10 要求回滚步骤可查）", tbl.file, want)
			}
		}
		if !strings.Contains(tbl.header, "live-ingest") {
			t.Errorf("%s: 文件头未写明数据所有者是 live-ingest", tbl.file)
		}
	}
	if len(seen) == 0 {
		t.Fatal("没有任何迁移文件被解析到")
	}
}

// TestMigrationsRollbackCoversEveryTable 校验回滚段列出了本文件建的全部表：
// 漏一张就会留下「没人记得为什么还在」的孤儿表，DROP 顺序错了还会被外键式依赖卡住
// （本仓库不建外键，所以只能靠注释里的反向顺序约束人）。
func TestMigrationsRollbackCoversEveryTable(t *testing.T) {
	dir := findMigrationsDir(t)
	tables := loadMigrationTables(t)
	byFile := map[string][]string{}
	for _, spec := range tableSpecs() {
		byFile[spec.file] = append(byFile[spec.file], spec.table)
	}
	for file, want := range byFile {
		pf := parseFile(t, filepath.Join(dir, file))
		for _, tbl := range want {
			if !strings.Contains(pf.header, "DROP TABLE IF EXISTS `"+tbl+"`") {
				t.Errorf("%s: 回滚段没有 DROP TABLE IF EXISTS `%s`", file, tbl)
			}
		}
		// 文件里出现了没被任何 spec 声明的表 -> model 有表没对上，或 SQL 有多余表。
		for name, tbl := range tables {
			if tbl.file != file {
				continue
			}
			if !contains(want, name) {
				t.Errorf("%s: 建了表 %s，但没有任何 tableSpec 认领它", file, name)
			}
		}
	}
}

// TestMigrationsRepeatableAndForbidden 校验每个迁移文件只建表、可重复执行，
// 且可执行 SQL 里不出现被禁止的语句。只看非注释行。
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
					t.Errorf("%s: 可执行语句以被禁止的 %q 开头: %q —— 迁移只建表；数据变更要另开文件并写明回滚",
						base, bad, line)
					break
				}
			}
		}
		if creates == 0 {
			t.Errorf("%s: 没有任何 CREATE TABLE IF NOT EXISTS", base)
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

// TestMigrationFileNamesAndOrder 校验文件编号连续升序，且集合与 spec 声明的一致。
func TestMigrationFileNamesAndOrder(t *testing.T) {
	dir := findMigrationsDir(t)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	got := make([]string, 0, len(paths))
	for _, p := range paths {
		got = append(got, filepath.Base(p))
	}
	sort.Strings(got)
	fileSet := map[string]bool{}
	want := make([]string, 0, len(tableSpecs()))
	for _, s := range tableSpecs() {
		if !fileSet[s.file] {
			fileSet[s.file] = true
			want = append(want, s.file)
		}
	}
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("迁移文件清单与 model 侧声明的不一致:\n 目录里=%v\n 期望=%v", got, want)
	}
	for i, f := range got {
		prefix := fmt.Sprintf("0000%02d_", i+1)
		if !strings.HasPrefix(f, prefix) {
			t.Errorf("第 %d 个文件 %s 的编号不是 %s：编号必须连续升序，回滚按它倒序执行", i+1, f, prefix)
		}
	}
}

// TestSecretColumnsStayDigestOnly 是 live-ingest 特有的红线：
// 全库不许出现能还原明文推流密钥的列。密钥域只有 key_hash（单向）、key_ref（引用）、
// key_tail（末 4 位辨认串，不参与鉴权）三种形态，回调域只有 *_hash / *_digest。
// 任何人加一列 key_plain / callback_secret 都会在这里失败，而不是等到审计时才发现。
//
// 判定按「列能不能装下字符串材料」而不是「列名里有没有 key 这个词」：
// key_id / prev_key_id 这类是 BIGINT 代理主键（外键引用），物理上存不了密钥内容，
// 一律拦会让红线充满假阳性、最后被人整条注释掉。真正要拦的是文本型密钥词列——
// 明文密钥、厂商签名串、回调 secret 只可能落在 CHAR/VARCHAR/TEXT 上。
func TestSecretColumnsStayDigestOnly(t *testing.T) {
	tables := loadMigrationTables(t)
	allowedMaterial := []string{"key_hash", "key_ref", "key_tail", "signature_hash", "client_ip_hash", "raw_params_digest"}
	allowedSet := specSet(allowedMaterial)
	risky := []string{"key", "secret", "signature", "token", "password", "credential", "plain", "raw"}
	seen := map[string]bool{}
	for _, tbl := range tables {
		for _, c := range tbl.columns {
			lower := strings.ToLower(c.name)
			if allowedSet[c.name] {
				seen[c.name] = true
				if !canHoldString(c.dataType) {
					t.Errorf("%s.%s: 摘要是字符串材料，列定义却是 %s —— 存不下 SHA-256 十六进制串，等于是个假摘要",
						tbl.table, c.name, c.dataType)
				}
				continue
			}
			if !canHoldString(c.dataType) {
				// 非文本列（整型/时间戳）容不下密钥材料，跳过名字撞词的假阳性。
				continue
			}
			for _, word := range risky {
				if strings.Contains(lower, word) {
					t.Errorf("%s.%s(%s): 列名含 %q 且是文本类型，又不在「只允许单向摘要/引用」白名单 %v 内 —— "+
						"明文密钥与厂商凭据一律不得入库（AGENTS.md §6）",
						tbl.table, c.name, c.dataType, word, allowedMaterial)
					break
				}
			}
		}
	}
	for _, name := range allowedMaterial {
		if !seen[name] {
			t.Errorf("摘要列 %s 在迁移里消失了：model/logic 依赖它承载密钥与回调凭据的单向形态，"+
				"删除前请确认没有把明文落库的替代方案", name)
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

func splitCols(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func normalizeCols(s string) string { return strings.Join(splitCols(s), ",") }

func specSet(list []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range list {
		out[v] = true
	}
	return out
}

func isTextType(def string) bool {
	d := strings.ToUpper(def)
	for _, t := range []string{"TEXT", "BLOB", "MEDIUMTEXT", "LONGTEXT"} {
		if strings.Contains(d, t) {
			return true
		}
	}
	return false
}

// canHoldString 判定一列物理上能否装下任意字符串材料。
// 密钥红线只可能落在字符类型上，因此这里刻意把 ENUM/SET/JSON/BLOB 也算进来，
// 宁可多拦也不放过「换个类型名就绕过白名单」的写法。
// 判定只看列名之后、COMMENT 之前的类型段，避免中文注释里的字样影响结论。
func canHoldString(def string) bool {
	s := def
	if i := strings.IndexByte(s, '`'); i >= 0 {
		if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
			s = s[i+j+2:]
		}
	}
	if i := indexAny(s, " COMMENT '", " comment '"); i >= 0 {
		s = s[:i]
	}
	d := strings.ToUpper(s)
	for _, t := range []string{"CHAR", "TEXT", "BLOB", "ENUM", "SET", "JSON"} {
		if strings.Contains(d, t) {
			return true
		}
	}
	return false
}

// indexAny 返回任一候选子串的首个出现位置，都没命中则 -1。
func indexAny(s string, candidates ...string) int {
	best := -1
	for _, c := range candidates {
		if i := strings.Index(s, c); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
