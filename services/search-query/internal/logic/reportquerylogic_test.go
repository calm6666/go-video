package logic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

// reportReq 给一条「所有守卫都能过」的基准上报请求（各用例只改自己要的那一项）。
// 标识类字段刻意取「像脱敏产物」的值，便于把「被拒」和「通过」两类用例分开。
func reportReq() *rpc.ReportQueryReq {
	return &rpc.ReportQueryReq{
		QueryId:      "q-7f3a91c2",
		Keyword:      "开源软件",
		SearchType:   rpc.SearchType_SEARCH_TYPE_VIDEO,
		Mid:          88,
		HitCount:     1234,
		ResultState:  rpc.QueryResultState_RESULT_STATE_OK,
		LatencyMs:    27,
		Platform:     "android",
		AppVersion:   "1.4.2",
		IpHash:       "9f86d081884c7d659a2f3c1e",
		TraceId:      "trace-report-1",
		DeviceIdHash: "5b8d6f7e4a2c1d0f",
	}
}

func doReportQuery(t *testing.T, st *store, in *rpc.ReportQueryReq) (*rpc.ReportQueryReply, error) {
	t.Helper()
	return NewReportQueryLogic(context.Background(), st.svcCtx()).ReportQuery(in)
}

// TestReportQueryRejectsInvalidRequestsBeforeAnyDependency 守卫表：
// 每个坏输入都必须在**开事务/打库/写缓存之前**被拒（调用轨迹为空即证明）。
// 其中 ip_hash / device_id_hash / app_version 的空白与控制字符判定是隐私边界：
// 放行就等于允许把原始 IP、User-Agent、明文设备号塞进脱敏列（AGENTS.md §7）。
func TestReportQueryRejectsInvalidRequestsBeforeAnyDependency(t *testing.T) {
	ok := reportReq()

	cases := []struct {
		name  string
		req   *rpc.ReportQueryReq
		want  error
		cause string
	}{
		{"空请求", nil, model.ErrInvalidQueryID, "in==nil 时无幂等键可用"},
		{"缺幂等键", bad(ok, func(r *rpc.ReportQueryReq) { r.QueryId = "" }), model.ErrInvalidQueryID,
			"query_id 是去重唯一键，缺失即无法保证不重复计数"},
		{"幂等键含空白", bad(ok, func(r *rpc.ReportQueryReq) { r.QueryId = "q 7f3a" }), model.ErrInvalidPage,
			"不透明标识不允许带空白"},
		{"幂等键含控制字符", bad(ok, func(r *rpc.ReportQueryReq) { r.QueryId = "q\x017f3a" }), model.ErrInvalidPage,
			"控制字符不得进入唯一索引"},
		{"幂等键超长", bad(ok, func(r *rpc.ReportQueryReq) { r.QueryId = strings.Repeat("q", 65) }), model.ErrInvalidPage,
			"query_id 列 varchar(64)"},
		{"空关键词", bad(ok, func(r *rpc.ReportQueryReq) { r.Keyword = "" }), model.ErrInvalidKeyword, "关键词必填"},
		{"纯空白关键词", bad(ok, func(r *rpc.ReportQueryReq) { r.Keyword = " \t\n " }), model.ErrInvalidKeyword,
			"规范化后为空：热词聚合会出现空分组"},
		{"超长关键词", bad(ok, func(r *rpc.ReportQueryReq) { r.Keyword = strings.Repeat("词", 65) }), model.ErrInvalidKeyword,
			"cfg.KeywordMaxLen=64 先于 varchar(128) 生效"},
		{"负 mid", bad(ok, func(r *rpc.ReportQueryReq) { r.Mid = -88 }), model.ErrInvalidMid,
			"负 mid 不是用户；0 才是游客"},
		{"负命中数", bad(ok, func(r *rpc.ReportQueryReq) { r.HitCount = -1 }), model.ErrInvalidPage,
			"hit_count 参与热度统计，负值会污染聚合"},
		{"负耗时", bad(ok, func(r *rpc.ReportQueryReq) { r.LatencyMs = -27 }), model.ErrInvalidPage,
			"latency_ms 不得为负"},
		{"未知端", bad(ok, func(r *rpc.ReportQueryReq) { r.Platform = "windows-phone" }), model.ErrInvalidPlatform,
			"端白名单（AGENTS.md §6）"},
		{"版本号含空白", bad(ok, func(r *rpc.ReportQueryReq) { r.AppVersion = "1.4.2 beta" }), model.ErrInvalidPage,
			"防止把 User-Agent 塞进 app_version"},
		{"版本号超长", bad(ok, func(r *rpc.ReportQueryReq) { r.AppVersion = strings.Repeat("1", 33) }), model.ErrInvalidPage,
			"app_version 列 varchar(32)"},
		{"ip_hash 含空白", bad(ok, func(r *rpc.ReportQueryReq) { r.IpHash = "203.0.113.9 Mozilla/5.0" }), model.ErrInvalidPage,
			"脱敏列只收定长标识，带空白的串就是原文"},
		{"ip_hash 含控制字符", bad(ok, func(r *rpc.ReportQueryReq) { r.IpHash = "9f86\x7fd081" }), model.ErrInvalidPage,
			"控制字符不得进入日志列"},
		{"device_id_hash 含空白", bad(ok, func(r *rpc.ReportQueryReq) { r.DeviceIdHash = "5b8d 6f7e" }), model.ErrInvalidPage,
			"设备标识只收哈希后的不透明串"},
		{"未给结果状态", bad(ok, func(r *rpc.ReportQueryReq) {
			r.ResultState = rpc.QueryResultState_RESULT_STATE_UNSPECIFIED
		}), model.ErrInvalidPage, "result_state 必填：ok/empty/degraded/blocked 决定分析口径"},
		{"未知结果状态", bad(ok, func(r *rpc.ReportQueryReq) { r.ResultState = rpc.QueryResultState(99) }), model.ErrInvalidPage,
			"未知枚举不猜语义，脏值不得进 result_state"},
		{"未知搜索类型", bad(ok, func(r *rpc.ReportQueryReq) { r.SearchType = rpc.SearchType(99) }), model.ErrInvalidSearchType,
			"分析维度枚举封闭（本服务不猜 doc 语义）"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			reply, err := doReportQuery(t, st, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：被拒的请求不应返回响应体，实际 %+v", tc.name, reply)
			}
			// 守卫先于一切下游：不开事务、不碰日志表、不碰 outbox、不写计数器。
			wantNoCall(t, tc.name, st, 0)
		})
	}
}

// TestReportQueryPersistsLogAndEmitsEventInOneTransaction 正常路径逐字段投影：
// 查询日志列、outbox 列、事件信封三层必须一致，且两次写入都在同一事务会话内（AGENTS.md §5）。
func TestReportQueryPersistsLogAndEmitsEventInOneTransaction(t *testing.T) {
	st := newStore(t, testConfig())
	req := bad(reportReq(), func(r *rpc.ReportQueryReq) { r.Keyword = "  开源   软件\x07 " })

	reply, err := doReportQuery(t, st, req)
	wantNoErr(t, "上报查询", err)
	wantEQ(t, "上报查询", "Accepted", reply.Accepted, true)
	wantEQ(t, "上报查询", "Deduplicated", reply.Deduplicated, false)

	logRow := st.queryLog.only(t)
	eventRow := st.outbox.only(t)
	wantEQ(t, "上报查询", "EventId 即 outbox 行的 event_id", reply.EventId, eventRow.EventId)
	wantEQ(t, "上报查询", "event_id 长度符合 CHAR(26)", len(eventRow.EventId), 26)

	wantEQ(t, "查询日志行", "QueryId", logRow.QueryId, req.QueryId)
	wantEQ(t, "查询日志行", "Mid", logRow.Mid, int64(88))
	wantEQ(t, "查询日志行", "Keyword（规范化后入库）", logRow.Keyword, "开源 软件")
	wantEQ(t, "查询日志行", "KeywordHash 对应规范化词", logRow.KeywordHash, model.KeywordHash("开源 软件"))
	wantEQ(t, "查询日志行", "HitCount", logRow.HitCount, int64(1234))
	wantEQ(t, "查询日志行", "ResultState", logRow.ResultState, model.ResultStateOK)
	wantEQ(t, "查询日志行", "LatencyMs", logRow.LatencyMs, int64(27))
	wantEQ(t, "查询日志行", "Platform（原样入库，不改大小写）", logRow.Platform, "android")
	wantEQ(t, "查询日志行", "AppVersion", logRow.AppVersion, "1.4.2")
	wantEQ(t, "查询日志行", "IpHash", logRow.IpHash, req.IpHash)
	if logRow.Ctime <= 0 {
		t.Errorf("查询日志行：Ctime = %d, want > 0（清理任务按此截断）", logRow.Ctime)
	}
	// 迁移注释：device_id_hash 不落列，避免按设备维度可查。
	if strings.Contains(jsonDump(t, logRow), req.DeviceIdHash) {
		t.Errorf("隐私：设备哈希出现在 search_query_log 行里：%s", jsonDump(t, logRow))
	}

	wantEQ(t, "事件行", "EventId", eventRow.EventId, reply.EventId)
	wantEQ(t, "事件行", "EventType", eventRow.EventType, model.EventQueryReported)
	wantEQ(t, "事件行", "SchemaVersion", eventRow.SchemaVersion, int32(model.EventSchemaVersion))
	wantEQ(t, "事件行", "AggregateType", eventRow.AggregateType, model.AggregateTypeQuery)
	wantEQ(t, "事件行", "AggregateId 用幂等键 query_id", eventRow.AggregateId, req.QueryId)
	wantEQ(t, "事件行", "State（发布器未接入前停在待发布）", eventRow.State, int32(model.OutboxStatePending))
	wantEQ(t, "事件行", "RetryCount", eventRow.RetryCount, int32(0))
	wantEQ(t, "事件行", "LastError", eventRow.LastError, "")
	wantEQ(t, "事件行", "OccurredAt 与日志 ctime 同一事实", eventRow.OccurredAt, logRow.Ctime)

	// 同事务：两次写入都拿到非 nil 会话，且整体只有一次 Begin/Commit。
	wantEQ(t, "同事务", "日志写入带会话", st.queryLog.gotTx[req.QueryId], true)
	wantEQ(t, "同事务", "事件写入带会话", st.outbox.gotTx[eventRow.EventId], true)
	wantEQ(t, "同事务", "事务次数", st.conn.transact, 1)
	wantEQ(t, "同事务", "回滚次数", st.conn.rolledBack, 0)

	env := envelopeOf(t, eventRow.Payload)
	wantSliceEQ(t, "事件信封", "顶层键（无广告/商业化字段，AGENTS.md §7）", sortedKeys(env), []string{
		"aggregate_id", "aggregate_type", "event_id", "event_type", "occurred_at", "payload", "producer",
		"schema_version", "trace_id",
	})
	wantJSONStr(t, "事件信封", "event_id", env["event_id"], eventRow.EventId)
	wantJSONStr(t, "事件信封", "event_type", env["event_type"], model.EventQueryReported)
	wantJSONNum(t, "事件信封", "schema_version", env["schema_version"], model.EventSchemaVersion)
	wantJSONStr(t, "事件信封", "producer", env["producer"], model.ProducerName)
	wantJSONStr(t, "事件信封", "aggregate_type", env["aggregate_type"], model.AggregateTypeQuery)
	wantJSONStr(t, "事件信封", "aggregate_id", env["aggregate_id"], req.QueryId)
	wantJSONStr(t, "事件信封", "trace_id 透传上报请求", env["trace_id"], req.TraceId)

	p := payloadOf(t, env)
	wantSliceEQ(t, "事件 payload", "键集合", sortedKeys(p), []string{
		"app_version", "device_id_hash", "hit_count", "ip_hash", "keyword", "keyword_hash",
		"latency_ms", "mid", "platform", "query_id", "result_state",
	})
	wantJSONStr(t, "事件 payload", "query_id", p["query_id"], req.QueryId)
	wantJSONStr(t, "事件 payload", "keyword 与库内一致", p["keyword"], logRow.Keyword)
	wantJSONStr(t, "事件 payload", "keyword_hash 与库内一致", p["keyword_hash"], logRow.KeywordHash)
	wantJSONNum(t, "事件 payload", "mid", p["mid"], 88)
	wantJSONNum(t, "事件 payload", "hit_count", p["hit_count"], 1234)
	wantJSONNum(t, "事件 payload", "latency_ms", p["latency_ms"], 27)
	wantJSONStr(t, "事件 payload", "result_state", p["result_state"], model.ResultStateOK)
	wantJSONStr(t, "事件 payload", "platform", p["platform"], "android")
	wantJSONStr(t, "事件 payload", "app_version", p["app_version"], "1.4.2")
	wantJSONStr(t, "事件 payload", "ip_hash", p["ip_hash"], req.IpHash)
	wantJSONStr(t, "事件 payload", "device_id_hash 只进事件", p["device_id_hash"], req.DeviceIdHash)

	wantReportOps(t, st, 0, model.KeywordHash("开源 软件"),
		"tx.Begin", "log.InsertIgnore:"+req.QueryId,
		"outbox.Insert:"+model.EventQueryReported+"/"+req.QueryId, "tx.Commit")
	// 上报不建搜索历史：历史只能由 Search 在登录态写，否则客户端可以伪造他人历史。
	wantCount(t, "上报不写历史", st.log, "history.", 0)
}

// TestReportQueryResultStateMappingReachesStorage 四个结果态枚举到 DB 字符串的映射：
// 分析侧靠 result_state 区分「没搜到」「降级」「被屏蔽」，映射错位会把故障读成零命中。
func TestReportQueryResultStateMappingReachesStorage(t *testing.T) {
	cases := []struct {
		name string
		in   rpc.QueryResultState
		want string
	}{
		{"有命中", rpc.QueryResultState_RESULT_STATE_OK, model.ResultStateOK},
		{"零命中", rpc.QueryResultState_RESULT_STATE_EMPTY, model.ResultStateEmpty},
		{"降级", rpc.QueryResultState_RESULT_STATE_DEGRADED, model.ResultStateDegraded},
		{"被屏蔽", rpc.QueryResultState_RESULT_STATE_BLOCKED, model.ResultStateBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			req := bad(reportReq(), func(r *rpc.ReportQueryReq) { r.ResultState = tc.in })
			reply, err := doReportQuery(t, st, req)
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "Accepted", reply.Accepted, true)
			row := st.queryLog.only(t)
			wantEQ(t, tc.name, "search_query_log.result_state", row.ResultState, tc.want)
			wantJSONStr(t, tc.name, "事件 result_state",
				payloadOf(t, envelopeOf(t, st.outbox.only(t).Payload))["result_state"], tc.want)
		})
	}
}

// TestReportQueryNormalizesKeywordEverywhere 关键词规范化必须一次到位：
// 落库列、keyword_hash、事件 payload、Redis 计数 key 用的是同一个字符串，
// 否则热词聚合（按哈希分组）与快照刷新会对不上（AGENTS.md §7）。
func TestReportQueryNormalizesKeywordEverywhere(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"折叠内部空白", "开源   软件", "开源 软件"},
		{"裁剪两端", "\t 开源软件 \n", "开源软件"},
		{"尖括号换成空格", "开源<em>软件</em>", "开源 em 软件 /em"},
		{"控制字符直接删除", "开源\x00\x0b\x1f软件", "开源软件"},
		{"不换行空格当普通空格", "\u00a0开源\u00a0\u00a0软件\u00a0", "开源 软件"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			req := bad(reportReq(), func(r *rpc.ReportQueryReq) { r.Keyword = tc.in })
			reply, err := doReportQuery(t, st, req)
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "Accepted", reply.Accepted, true)

			row := st.queryLog.only(t)
			wantEQ(t, tc.name, "入库关键词", row.Keyword, tc.want)
			wantEQ(t, tc.name, "入库哈希对应规范化词", row.KeywordHash, model.KeywordHash(tc.want))
			p := payloadOf(t, envelopeOf(t, st.outbox.only(t).Payload))
			wantJSONStr(t, tc.name, "事件关键词", p["keyword"], tc.want)
			wantJSONStr(t, tc.name, "事件哈希", p["keyword_hash"], model.KeywordHash(tc.want))
			// Redis 计数按同一哈希累加（否则热词榜与快照表分叉）。
			wantReportOps(t, st, 0, model.KeywordHash(tc.want),
				"tx.Begin", "log.InsertIgnore:"+req.QueryId,
				"outbox.Insert:"+model.EventQueryReported+"/"+req.QueryId, "tx.Commit")
		})
	}
}

// TestReportQueryRepeatedQueryIdAddsNoRows 幂等不变量（对着迁移 SQL 的 uniq_query_id 写）：
// 同一 query_id 重复上报既**不新增行也不更新既有行**，而是被 INSERT IGNORE 静默跳过，
// 事件也只有一条（回填首次 event_id，保证调用方幂等可见）。
func TestReportQueryRepeatedQueryIdAddsNoRows(t *testing.T) {
	st := newStore(t, testConfig())
	req := reportReq()
	first, err := doReportQuery(t, st, req)
	wantNoErr(t, "首次上报", err)
	wantEQ(t, "首次上报", "Accepted", first.Accepted, true)
	firstRow := st.queryLog.only(t)

	before := st.log.snapshot()
	retry := bad(req, func(r *rpc.ReportQueryReq) { r.HitCount = 999999 })
	second, err := doReportQuery(t, st, retry)
	wantNoErr(t, "重复上报", err)
	wantEQ(t, "重复上报", "Accepted（未新增落库）", second.Accepted, false)
	wantEQ(t, "重复上报", "Deduplicated", second.Deduplicated, true)
	wantEQ(t, "重复上报", "EventId 回填首次产生的事件", second.EventId, first.EventId)

	wantReportOps(t, st, before, firstRow.KeywordHash,
		"tx.Begin", "log.InsertIgnore:"+req.QueryId, "tx.Commit",
		"outbox.FindEventId:"+model.AggregateTypeQuery+"/"+req.QueryId)
	wantEQ(t, "重复上报", "search_query_log 行数", len(st.queryLog.rows), 1)
	wantEQ(t, "重复上报", "search_outbox 行数", len(st.outbox.rows), 1)
	// 重复上报不得改写首次的值（INSERT IGNORE，不是 UPSERT）。
	same := st.queryLog.only(t)
	wantEQ(t, "重复上报不更新", "HitCount 保持首次值", same.HitCount, firstRow.HitCount)
	wantEQ(t, "重复上报不更新", "Ctime 保持首次值", same.Ctime, firstRow.Ctime)
	wantEQ(t, "重复上报不更新", "QueryId", same.QueryId, firstRow.QueryId)
	// 有意漂移告警：两次上报都给了 Redis 关键词计数加一，而 DB 只有一行 ——
	// 这与「重复上报不产生新行」的幂等口径不一致（热词榜的 Redis 投影会重复计数），登记为缺陷。
	// 计数为 2 是**现状**而非期望：若日后改成去重后不计数，本断言变红即为修复信号。
	wantCount(t, "重复上报的 Redis 计数次数（现状：两次上报各加一次）", st.log, "cache.IncrCounters", 2)
}

// TestReportQueryBackfillLookupFailureStillReportsDedup 幂等回填的失败口径：
// 取不到首次 event_id 只影响回传字段，不改变「未写入新行」的幂等结论，也不报错。
func TestReportQueryBackfillLookupFailureStillReportsDedup(t *testing.T) {
	st := newStore(t, testConfig())
	req := reportReq()
	boom := errors.New("search_outbox FindEventIdByAggregate: connection reset")
	st.outbox.failWith("FindEventIdByAggregate", boom)
	// 直接布景「上一次上报已占住 query_id」，且不布事件行（回填必然取不到）。
	st.queryLog.seed(&model.SearchQueryLog{
		QueryId: req.QueryId, Mid: req.Mid, Keyword: req.Keyword,
		KeywordHash: model.KeywordHash(req.Keyword), ResultState: model.ResultStateOK, Ctime: nowMinus(60),
	})

	reply, err := doReportQuery(t, st, req)
	wantNoErr(t, "回填失败仍算幂等成功", err)
	wantEQ(t, "回填失败", "Accepted", reply.Accepted, false)
	wantEQ(t, "回填失败", "Deduplicated", reply.Deduplicated, true)
	wantEQ(t, "回填失败", "EventId（无法回填时为空，不编造）", reply.EventId, "")
	wantReportOps(t, st, 0, model.KeywordHash(req.Keyword),
		"tx.Begin", "log.InsertIgnore:"+req.QueryId, "tx.Commit",
		"outbox.FindEventId:"+model.AggregateTypeQuery+"/"+req.QueryId)
	wantEQ(t, "回填失败不产生事件行", "search_outbox 行数", len(st.outbox.rows), 0)
}

// TestReportQueryGuestAndMissingOptionalIdentifiers 游客上报（mid=0 合法）与未采集标识：
// 未采集的字段保持空串/缺省，绝不用零值或猜测值填充；上报也不得建历史、不打引擎。
func TestReportQueryGuestAndMissingOptionalIdentifiers(t *testing.T) {
	st := newStore(t, testConfig())
	req := bad(reportReq(), func(r *rpc.ReportQueryReq) {
		r.Mid = 0
		r.AppVersion = ""
		r.IpHash = ""
		r.DeviceIdHash = ""
		r.Platform = "" // 端未上报时允许空
		r.SearchType = rpc.SearchType_SEARCH_TYPE_UNSPECIFIED
	})
	reply, err := doReportQuery(t, st, req)
	wantNoErr(t, "游客上报", err)
	wantEQ(t, "游客上报", "Accepted", reply.Accepted, true)

	row := st.queryLog.only(t)
	wantEQ(t, "游客上报", "Mid=0 如实入库（不伪造用户）", row.Mid, int64(0))
	wantEQ(t, "游客上报", "AppVersion", row.AppVersion, "")
	wantEQ(t, "游客上报", "IpHash", row.IpHash, "")
	wantEQ(t, "游客上报", "Platform", row.Platform, "")

	p := payloadOf(t, envelopeOf(t, st.outbox.only(t).Payload))
	for _, field := range []string{"app_version", "ip_hash", "device_id_hash", "platform"} {
		if _, exists := p[field]; exists {
			t.Errorf("游客上报：未采集的 %s 不得出现在事件里（omitempty），实际 %v", field, p[field])
		}
	}
	wantJSONNum(t, "游客上报", "mid", p["mid"], 0)
	wantReportOps(t, st, 0, model.KeywordHash("开源软件"),
		"tx.Begin", "log.InsertIgnore:"+req.QueryId,
		"outbox.Insert:"+model.EventQueryReported+"/"+req.QueryId, "tx.Commit")
	// 上报只做行为分析：不写历史、不查引擎、不读结果缓存。
	wantNoOpsWith(t, "游客上报", st.log, 0, "history.", "es.", "cache.GetResult")
}

// TestReportQueryFailurePropagation 每个依赖各注入一次错误：
// 错误必须如实传出、不返回伪造成功，且失败不得被记成「已提交」。
//
// 边界（fakes_test.go 已声明）：内存替身不模拟回滚撤销行，因此事务失败只断言
// 「未 Commit + 已 Rollback + 错误传出 + 无事件行」，行的原子消失由 MySQL 保证。
func TestReportQueryFailurePropagation(t *testing.T) {
	t.Run("查询日志写入失败：整体回滚，既不写事件也不计数", func(t *testing.T) {
		st := newStore(t, testConfig())
		boom := errors.New("search_query_log InsertIgnore: duplicate entry for key 'PRIMARY'")
		st.queryLog.failWith("InsertIgnore", boom)

		reply, err := doReportQuery(t, st, reportReq())
		wantErrIs(t, "日志写入失败", err, boom)
		if reply != nil {
			t.Errorf("日志写入失败：不应返回成功（accepted=%v event_id=%q）", reply.Accepted, reply.EventId)
		}
		wantOps(t, "日志写入失败调用序列", st.log.ops, []string{"tx.Begin", "log.InsertIgnore:q-7f3a91c2", "tx.Rollback"})
		wantEQ(t, "日志写入失败", "search_outbox 行数", len(st.outbox.rows), 0)
		wantEQ(t, "日志写入失败", "回滚次数", st.conn.rolledBack, 1)
		wantCount(t, "日志写入失败不得预热计数器", st.log, "cache.", 0)
	})

	t.Run("事件写入失败：错误传出且未提交", func(t *testing.T) {
		st := newStore(t, testConfig())
		boom := errors.New("search_outbox Insert: deadlock found when trying to get lock")
		st.outbox.failWith("Insert", boom)

		reply, err := doReportQuery(t, st, reportReq())
		wantErrIs(t, "事件写入失败", err, boom)
		if reply != nil {
			t.Errorf("事件写入失败：不得把未提交的事务报成 accepted=true，实际 %+v", reply)
		}
		wantOps(t, "事件写入失败调用序列", st.log.ops, []string{
			"tx.Begin", "log.InsertIgnore:q-7f3a91c2", "outbox.Insert:search.query/q-7f3a91c2", "tx.Rollback",
		})
		wantCount(t, "事件写入失败不得出现第二次提交", st.log, "tx.Commit", 0)
		wantCount(t, "事件写入失败不得出现两次 Begin", st.log, "tx.Begin", 1)
		wantEQ(t, "事件写入失败", "outbox 事件行数", len(st.outbox.rows), 0)
		wantCount(t, "事件写入失败不得预热计数器", st.log, "cache.", 0)
	})

	t.Run("计数器写失败：旁路投影，不影响已提交的上报结果", func(t *testing.T) {
		st := newStore(t, testConfig())
		req := reportReq()
		boom := errors.New("redis: connection pool timeout")
		st.cache.failWith("IncrQueryCounters", boom)

		reply, err := doReportQuery(t, st, req)
		wantNoErr(t, "计数器失败", err)
		wantEQ(t, "计数器失败", "Accepted（日志与事件已提交）", reply.Accepted, true)
		wantEQ(t, "计数器失败", "Deduplicated", reply.Deduplicated, false)
		wantEQ(t, "计数器失败", "EventId 仍回传", reply.EventId, st.outbox.only(t).EventId)
		// 计数器写失败发生在 tx.Commit 之后：轨迹里仍能看到那一次（失败）计数调用。
		wantReportOps(t, st, 0, model.KeywordHash(req.Keyword),
			"tx.Begin", "log.InsertIgnore:"+req.QueryId,
			"outbox.Insert:"+model.EventQueryReported+"/"+req.QueryId, "tx.Commit")
		wantCount(t, "计数器失败不回滚", st.log, "tx.Rollback", 0)
		wantEQ(t, "计数器失败不改写幂等结论", "search_outbox 行数", len(st.outbox.rows), 1)
	})
}

// TestReportQueryDoesNotGuessOpaqueIdentifierFormat 登记现状（不是背书）：
// 本层只挡空白/控制字符/超长，**不校验格式**，因此不带空白的原始 IP 仍会落库。
// 若日后按要求强制 sha256 hex，本断言会变红（有意漂移告警）。
func TestReportQueryDoesNotGuessOpaqueIdentifierFormat(t *testing.T) {
	st := newStore(t, testConfig())
	req := bad(reportReq(), func(r *rpc.ReportQueryReq) { r.IpHash = "203.0.113.9:51234" })
	reply, err := doReportQuery(t, st, req)
	wantNoErr(t, "无空白的原始 IP", err)
	wantEQ(t, "无空白的原始 IP", "Accepted", reply.Accepted, true)
	wantEQ(t, "无空白的原始 IP", "ip_hash 原样入库", st.queryLog.only(t).IpHash, "203.0.113.9:51234")
}

// --- 本文件专用小工具 ---

// envelopeOf 解开 outbox 行里的信封 JSON。
func envelopeOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	var env map[string]any
	wantNoErr(t, "事件信封必须是合法 JSON", json.Unmarshal([]byte(raw), &env))
	return env
}

// payloadOf 取信封里的业务 payload 对象。
func payloadOf(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	p, ok := env["payload"].(map[string]any)
	if !ok {
		t.Fatalf("事件 payload 不是对象：%#v", env["payload"])
	}
	return p
}

// wantJSONStr 断言 JSON 对象（map[string]any 取值）里的字符串字段等于 want。
// 值不是字符串（键缺失成 nil、或变成数字/布尔）一律算失败并打出实际类型：
// 直接拿 wantEQ 比较 any 与 string 编译不过，而裸 type assertion 失败会 panic，
// 这条既保住类型也保住可读的失败信息。
func wantJSONStr(t *testing.T, label, field string, got any, want string) {
	t.Helper()
	s, ok := got.(string)
	if !ok {
		t.Fatalf("%s：%s = %#v (%T), want 字符串 %q", label, field, got, got, want)
	}
	wantEQ(t, label, field, s, want)
}

// wantReportOps 断言一次上报的完整调用轨迹（从 from 起）：head 是主链（事务与写库）调用，
// 之后必须**恰好一次**关键词计数（Redis 旁路投影）。
// 计数的 day 段只校验形状：日期串由实现的 UTC 取日决定，用例重算等于抄实现。
func wantReportOps(t *testing.T, st *store, from int, keywordHash string, head ...string) {
	t.Helper()
	ops := st.log.ops[from:]
	wantEQ(t, "上报调用轨迹", "总调用次数", len(ops), len(head)+1)
	wantOps(t, "上报主链调用序列", ops[:len(head)], head)
	counter := ops[len(head)]
	hash, day, ok := strings.Cut(strings.TrimPrefix(counter, "cache.IncrCounters:"), "/")
	if !ok || hash == counter {
		t.Fatalf("第 %d 次调用不是关键词计数：%s", len(head)+1, counter)
	}
	wantEQ(t, "关键词计数", "hash 段", hash, keywordHash)
	if len(day) != 8 || strings.ContainsAny(day, "-: ./") {
		t.Errorf("关键词计数的 day 段 %q 不是 8 位日期串", day)
	}
}
