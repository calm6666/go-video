package esclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func doc() *ContentDoc {
	return &ContentDoc{
		ContentID: 11, ContentType: 1, Title: "测试标题", State: StatePublished,
		DocRevision: 1234, SchemaVersion: 1, Heat: Heat{ViewCount: 5, HeatRevision: 1234},
	}
}

// TestBuildBulkNDJSON_LineStructure 锁定 _bulk 协议格式：
// 动作行与数据行成对、delete 只有动作行、整体以换行结尾（否则服务端 400）。
func TestBuildBulkNDJSON_LineStructure(t *testing.T) {
	ops := []BulkOp{
		NewIndexOp("idx_v1", "1_11", doc()),
		NewHeatUpdateOp("idx_v1", "1_12", Heat{ViewCount: 9, HeatRevision: 100}),
		NewDeleteOp("idx_v1", "1_13"),
	}
	raw, err := BuildBulkNDJSON("idx_v1", ops)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("_bulk 请求体必须以换行符结尾")
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	// index: meta+doc, update: meta+doc, delete: meta only => 5 行
	if len(lines) != 5 {
		t.Fatalf("行数 = %d, want 5\n%s", len(lines), body)
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("第 %d 行为空行，NDJSON 不允许空行", i)
		}
		var probe map[string]interface{}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v\n%s", i, err, line)
		}
		if strings.Contains(line, "\r") {
			t.Fatalf("第 %d 行含 \\r，会破坏 NDJSON 分帧", i)
		}
	}

	assertMetaLine(t, lines[0], BulkActionIndex, "idx_v1", "1_11")
	assertMetaLine(t, lines[2], BulkActionUpdate, "idx_v1", "1_12")
	assertMetaLine(t, lines[4], BulkActionDelete, "idx_v1", "1_13")

	// update 的数据行必须是 {"doc": {...}} 包裹体，否则会整篇覆盖丢字段。
	var upd struct {
		Doc map[string]interface{} `json:"doc"`
	}
	if err := json.Unmarshal([]byte(lines[3]), &upd); err != nil {
		t.Fatal(err)
	}
	if _, ok := upd.Doc["heat"]; !ok {
		t.Fatalf("热度更新数据行缺少 doc.heat: %s", lines[3])
	}
	if _, ok := upd.Doc["title"]; ok {
		t.Fatalf("热度更新不应重建整篇文档: %s", lines[3])
	}
}

func assertMetaLine(t *testing.T, line, action, index, id string) {
	t.Helper()
	var meta map[string]map[string]string
	if err := json.Unmarshal([]byte(line), &meta); err != nil {
		t.Fatalf("解析动作行 %q: %v", line, err)
	}
	got, ok := meta[action]
	if !ok {
		t.Fatalf("动作行缺少 %q: %s", action, line)
	}
	if got["_index"] != index || got["_id"] != id {
		t.Fatalf("动作行内容异常: %s", line)
	}
}

func TestBuildBulkNDJSON_FillsDefaultIndex(t *testing.T) {
	raw, err := BuildBulkNDJSON("default_idx", []BulkOp{{Action: BulkActionIndex, ID: "1_1", Doc: doc()}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"_index":"default_idx"`) {
		t.Fatalf("未指定索引时应回填默认索引:\n%s", raw)
	}
}

func TestBuildBulkNDJSON_DefaultActionIsIndex(t *testing.T) {
	raw, err := BuildBulkNDJSON("idx", []BulkOp{{ID: "1_1", Doc: doc()}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"index":{`) {
		t.Fatalf("空动作应默认 index:\n%s", raw)
	}
}

func TestBuildBulkNDJSON_Errors(t *testing.T) {
	cases := []struct {
		name string
		ops  []BulkOp
		want string
	}{
		{"空批次", nil, "empty bulk ops"},
		{"缺 _id（非幂等写必须拒绝）", []BulkOp{{Action: BulkActionIndex, Index: "i", Doc: doc()}}, "missing _id"},
		{"缺索引", []BulkOp{{Action: BulkActionIndex, ID: "1_1", Doc: doc()}}, "missing index"},
		{"未知动作", []BulkOp{{Action: "upsert", Index: "i", ID: "1_1", Doc: doc()}}, "invalid action"},
		{"非 delete 缺数据体", []BulkOp{{Action: BulkActionUpdate, Index: "i", ID: "1_1"}}, "missing doc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuildBulkNDJSON("", c.ops)
			if err == nil {
				t.Fatalf("期望错误包含 %q，实际 nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望错误包含 %q，实际 %v", c.want, err)
			}
		})
	}
}

// TestBuildBulkNDJSON_NoHTMLEscape 标题里的 <>& 必须原样入索引，
// 否则 Go 默认的 \u003c 转义会让搜索结果与原文不一致。
func TestBuildBulkNDJSON_NoHTMLEscape(t *testing.T) {
	d := doc()
	d.Title = "A&B <直播> 100%「中文字符」"
	raw, err := BuildBulkNDJSON("idx", []BulkOp{NewIndexOp("idx", d.ID(), d)})
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{"&", "<", "%"} {
		if !strings.Contains(body, want) {
			t.Fatalf("请求体丢失原始字符 %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `\u003c`) || strings.Contains(body, `\u0026`) {
		t.Fatalf("请求体被 HTML 转义:\n%s", body)
	}
	// 反读一次确认仍是合法 JSON 且字段值未变。
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	var got ContentDoc
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != d.Title {
		t.Fatalf("标题回读不一致: %q != %q", got.Title, d.Title)
	}
}

func TestParseBulkResponse_FlattensItemsAndDetectsFailure(t *testing.T) {
	resp := `{"took":7,"errors":true,"items":[
	  {"index":{"_id":"1_11","_index":"idx","status":201}},
	  {"update":{"_id":"1_12","_index":"idx","status":409,"error":{"type":"version_conflict_engine_exception","reason":"文档版本冲突"}}},
	  {"delete":{"_id":"1_13","_index":"idx","status":404}}
	]}`
	res, err := parseBulkResponse([]byte(resp))
	if err != nil {
		t.Fatal(err)
	}
	if res.Took != 7 || !res.Errors {
		t.Fatalf("汇总字段异常: %+v", res)
	}
	if len(res.Items) != 3 {
		t.Fatalf("逐项数 = %d, want 3", len(res.Items))
	}
	if res.Items[0].Action != BulkActionIndex || res.Items[0].ID != "1_11" || res.Items[0].Index != "idx" {
		t.Fatalf("逐项展平结果异常: %+v", res.Items[0])
	}
	// 404 是幂等成功（删除不存在的文档），409 是真失败。
	if !res.Items[2].Succeeded() {
		t.Fatal("delete 404 应视为幂等成功")
	}
	if res.Items[1].Succeeded() {
		t.Fatal("409 版本冲突必须判失败")
	}
	bad := res.Failed()
	if len(bad) != 1 || bad[0].ID != "1_12" {
		t.Fatalf("Failed() = %+v, want 只含 1_12", bad)
	}
	if !strings.Contains(bad[0].Error, "version_conflict_engine_exception") {
		t.Fatalf("失败原因未保留: %q", bad[0].Error)
	}
}

func TestParseBulkResponse_TruncatesLongReason(t *testing.T) {
	long := strings.Repeat("x", 2000)
	res, err := parseBulkResponse([]byte(`{"took":1,"errors":true,"items":[{"index":{"_id":"1_1","_index":"i","status":500,"error":{"type":"mapper_parsing_exception","reason":"` + long + `"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Items[0].Error; len(got) > 560 || !strings.HasSuffix(got, "...") {
		t.Fatalf("失败原因未截断，长度=%d", len(got))
	}
}

func TestParseBulkResponse_InvalidJSON(t *testing.T) {
	if _, err := parseBulkResponse([]byte("not json")); err == nil {
		t.Fatal("非法响应必须报错，不能返回空结果")
	}
}

func TestBulkResultFailedNilSafe(t *testing.T) {
	var r *BulkResult
	if len(r.Failed()) != 0 {
		t.Fatal("nil 结果的 Failed 应为空")
	}
}
