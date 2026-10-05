package esclient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDocID(t *testing.T) {
	// 不同内容类型可能重号，主键必须带类型前缀。
	if got := DocID(2, 12345); got != "2_12345" {
		t.Fatalf("DocID = %q, want 2_12345", got)
	}
	d := &ContentDoc{ContentID: 7, ContentType: 3}
	if d.ID() != "3_7" {
		t.Fatalf("ContentDoc.ID = %q, want 3_7", d.ID())
	}
}

func TestContentDocValidate(t *testing.T) {
	base := func() *ContentDoc {
		return &ContentDoc{
			ContentID: 1, ContentType: 1, Title: "标题",
			State: StatePublished, DocRevision: 1000, SchemaVersion: 1,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("合法文档被拒绝: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ContentDoc)
	}{
		{"content_id 为 0", func(d *ContentDoc) { d.ContentID = 0 }},
		{"content_id 负数", func(d *ContentDoc) { d.ContentID = -5 }},
		{"content_type 越界(0)", func(d *ContentDoc) { d.ContentType = 0 }},
		{"content_type 越界(4)", func(d *ContentDoc) { d.ContentType = 4 }},
		{"缺 doc_revision", func(d *ContentDoc) { d.DocRevision = 0 }},
		{"state 越界(0)", func(d *ContentDoc) { d.State = 0 }},
		{"state 越界(6)", func(d *ContentDoc) { d.State = 6 }},
		{"已发布但标题为空", func(d *ContentDoc) { d.Title = "   " }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := base()
			c.mutate(d)
			if err := d.Validate(); err == nil {
				t.Fatal("期望校验失败，实际通过")
			}
		})
	}

	// 非发布状态允许无标题（下架/删除的降级更新不应被字段校验挡住）。
	offline := base()
	offline.Title = ""
	offline.State = StateOffline
	if err := offline.Validate(); err != nil {
		t.Fatalf("下架文档无标题应可通过校验: %v", err)
	}

	var nilDoc *ContentDoc
	if err := nilDoc.Validate(); err == nil {
		t.Fatal("nil 文档必须报错")
	}
}

func TestShouldOverwriteBlocksStaleDoc(t *testing.T) {
	existing := &ContentDoc{DocRevision: 2000}
	if !ShouldOverwrite(nil, existing) {
		t.Fatal("文档不存在时必须允许写入")
	}
	if !ShouldOverwrite(existing, &ContentDoc{DocRevision: 2001}) {
		t.Fatal("更新的 revision 必须允许覆盖")
	}
	// 同版本重放（MQ at-least-once）必须放行，否则重试永远收敛不了。
	if !ShouldOverwrite(existing, &ContentDoc{DocRevision: 2000}) {
		t.Fatal("同 revision 的幂等重写必须放行")
	}
	if ShouldOverwrite(existing, &ContentDoc{DocRevision: 1999}) {
		t.Fatal("旧版本不得覆盖新版本")
	}
}

func TestShouldPatchHeat(t *testing.T) {
	if !ShouldPatchHeat(500, 501) {
		t.Fatal("更新的热度快照必须应用")
	}
	if !ShouldPatchHeat(500, 500) {
		t.Fatal("同版本快照必须可幂等重放")
	}
	if ShouldPatchHeat(500, 499) {
		t.Fatal("过期热度快照不得覆盖新快照")
	}
	if ShouldPatchHeat(0, -1) {
		t.Fatal("负版本不得通过")
	}
}

func TestStatePatchBody(t *testing.T) {
	body := StatePatchBody(StateExpired, 4321)
	if body["state"] != StateExpired {
		t.Fatalf("state = %v, want %d", body["state"], StateExpired)
	}
	if body["doc_revision"] != int64(4321) {
		t.Fatalf("doc_revision = %v, want 4321", body["doc_revision"])
	}
	// 降级更新只碰这两个字段，不得把整篇文档带进 _update 请求体。
	if len(body) != 2 {
		t.Fatalf("StatePatchBody 字段数 = %d, want 2 (%v)", len(body), body)
	}
}

func TestIsRetrievable(t *testing.T) {
	for state, want := range map[int32]bool{
		StatePending: false, StatePublished: true, StateOffline: false,
		StateExpired: false, StateDeleted: false,
	} {
		if got := IsRetrievable(state); got != want {
			t.Errorf("IsRetrievable(%d) = %v, want %v", state, got, want)
		}
	}
}

// TestHeatPatchScope 锁定热度部分更新的请求体只含 heat，
// 避免有人改成整篇重建（会把上游读压力放大到全量）。
func TestHeatPatchScope(t *testing.T) {
	p := &HeatPatch{ContentID: 9, ContentType: 1, Heat: Heat{ViewCount: 10, HeatRevision: 99}}
	if p.ID() != "1_9" {
		t.Fatalf("HeatPatch.ID = %q, want 1_9", p.ID())
	}
	body := p.PartialBody()
	if len(body) != 1 {
		t.Fatalf("部分更新体字段数 = %d, want 1: %v", len(body), body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"heat":{`) {
		t.Fatalf("部分更新体结构异常: %s", raw)
	}
	// ContentID/ContentType 只用于定位文档，绝不能出现在 _source 里。
	if strings.Contains(string(raw), "content_id") {
		t.Fatalf("部分更新体不应包含 content_id: %s", raw)
	}
}

// TestIndexBodyMatchesDocStruct 保证 mapping 与投影结构同源：
// 新增/重命名 ContentDoc、Heat 字段却忘了改 mapping 时，ES 会按动态映射猜类型，
// 之后只能重建索引，因此这里在单测阶段就拦住。
func TestIndexBodyMatchesDocStruct(t *testing.T) {
	var body map[string]interface{}
	if err := json.Unmarshal(mustIndexBody(t, ""), &body); err != nil {
		t.Fatalf("IndexBody 不是合法 JSON: %v", err)
	}
	props := mappingsProperties(t, body)

	assertPresent(t, props, jsonFieldNames(reflect.TypeOf(ContentDoc{})))
	heat, ok := props["heat"].(map[string]interface{})
	if !ok {
		t.Fatal("mapping 缺少 heat 对象")
	}
	heatProps, ok := heat["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("mapping heat 缺少 properties")
	}
	assertPresent(t, heatProps, jsonFieldNames(reflect.TypeOf(Heat{})))

	// schema_version 必须登记在 mapping 里：查询侧据此判断结构是否兼容。
	if _, ok := props["schema_version"]; !ok {
		t.Fatal("mapping 缺少 schema_version 字段")
	}
	// doc_revision / heat_revision 是防旧覆盖新的守卫字段，类型必须是 long。
	for _, f := range []string{"doc_revision", "rights_expire_at"} {
		if m, ok := props[f].(map[string]interface{}); !ok || m["type"] != "long" {
			t.Errorf("字段 %s 类型应为 long，实际 %v", f, props[f])
		}
	}
	if m, ok := props["state"].(map[string]interface{}); !ok || m["type"] != "integer" {
		t.Errorf("state 类型应为 integer，实际 %v", props["state"])
	}
}

// TestIndexBodySensitiveFieldsAbsent 断言索引结构里没有隐私字段。
func TestIndexBodySensitiveFieldsAbsent(t *testing.T) {
	var body map[string]interface{}
	if err := json.Unmarshal(mustIndexBody(t, "v2"), &body); err != nil {
		t.Fatal(err)
	}
	raw := string(mustJSON(t, body))
	// 按 JSON key 形态匹配：分词配置里合法存在 tokenizer 之类的键，
	// 只匹配裸词会让本用例把正常的 analysis 段误判成隐私字段。
	for _, forbidden := range []string{"mobile", "phone", "id_card", "ip_addr", "ip", "token", "password", "cookie", "device_id"} {
		if strings.Contains(raw, `"`+forbidden+`":`) {
			t.Errorf("索引结构包含敏感字段 %q", forbidden)
		}
	}
	if !strings.Contains(raw, `"refresh_interval":"1s"`) {
		t.Errorf("索引 settings 缺少 refresh_interval，逐条写入会打爆集群: %s", raw)
	}

	// schemaVersion 参数必须真的落到索引里（mappings._meta），
	// 否则排障时只能靠索引名反推结构版本。
	mappings, ok := body["mappings"].(map[string]interface{})
	if !ok {
		t.Fatal("IndexBody 缺少 mappings")
	}
	meta, ok := mappings["_meta"].(map[string]interface{})
	if !ok {
		t.Fatal("IndexBody 缺少 mappings._meta，结构版本未随索引落地")
	}
	if meta["schema_version"] != "v2" {
		t.Fatalf("_meta.schema_version = %v, want v2", meta["schema_version"])
	}
	if meta["owner"] != "search-indexer" {
		t.Fatalf("_meta.owner = %v, want search-indexer", meta["owner"])
	}
	if def := string(mustJSON(t, mustIndexBodyDefault(t))); !strings.Contains(def, `"schema_version":"v1"`) {
		t.Fatalf("默认结构版本应为 v1: %s", def)
	}
}

// mustIndexBodyDefault 复核空版本入参的回退行为。
func mustIndexBodyDefault(t *testing.T) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(mustIndexBody(t, ""), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// mustIndexBody 用默认分词策略生成索引请求体，失败即终止用例。
func mustIndexBody(t *testing.T, schemaVersion string) []byte {
	t.Helper()
	body, err := IndexBody(schemaVersion, Analyzer{})
	if err != nil {
		t.Fatalf("IndexBody(%q) 失败: %v", schemaVersion, err)
	}
	return body
}

func mappingsProperties(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	mappings, ok := body["mappings"].(map[string]interface{})
	if !ok {
		t.Fatal("IndexBody 缺少 mappings")
	}
	props, ok := mappings["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("mappings 缺少 properties")
	}
	return props
}

func assertPresent(t *testing.T, props map[string]interface{}, names []string) {
	t.Helper()
	for _, n := range names {
		if _, ok := props[n]; !ok {
			t.Errorf("mapping 缺少投影字段 %q", n)
		}
	}
}

// jsonFieldNames 取结构体的 json 字段名（omitempty 后缀已剥离）。
func jsonFieldNames(typ reflect.Type) []string {
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
