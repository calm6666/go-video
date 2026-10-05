package esclient

import (
	"reflect"
	"strings"
	"testing"
)

// TestBuildReindexBody 锁定重建区间语义为 [from,to)：
// 闭区间会让相邻切片重复搬运同一批文档，开区间则可能整片漏掉。
func TestBuildReindexBody(t *testing.T) {
	body := BuildReindexBody(ReindexSliceReq{
		Source: "a_v1", Dest: "a_v2", From: 100, To: 200, Size: 500,
		Filter: map[string]interface{}{"term": map[string]interface{}{"content_type": 2}},
	})
	raw := mustJSON(t, body)
	s := string(raw)
	if !containsAll(s, `"gte":100`, `"lt":200`, `"index":"a_v1"`, `"index":"a_v2"`, `"conflicts":"proceed"`, `"size":500`) {
		t.Fatalf("重建请求体异常: %s", s)
	}
	// 两条 must 都在：区间过滤 + scope 过滤，缺一即重建范围被放大。
	src := body["source"].(map[string]interface{})
	must := src["query"].(map[string]interface{})["bool"].(map[string]interface{})["must"].([]interface{})
	if len(must) != 2 {
		t.Fatalf("must 条件数 = %d, want 2: %s", len(must), s)
	}

	// 无过滤（full）时只带区间条件，且 size<=0 时不下发 size。
	full := BuildReindexBody(ReindexSliceReq{Source: "s", Dest: "d", From: 1, To: 100})
	srcFull := full["source"].(map[string]interface{})
	m2 := srcFull["query"].(map[string]interface{})["bool"].(map[string]interface{})["must"].([]interface{})
	if len(m2) != 1 {
		t.Fatalf("full 重建应只有区间条件: %v", m2)
	}
	if _, ok := srcFull["size"]; ok {
		t.Fatal("size<=0 时不应下发 size")
	}
	if _, ok := full["conflicts"]; !ok {
		t.Fatal("必须显式声明 conflicts 策略")
	}
}

func TestReindexSliceReqStructFields(t *testing.T) {
	// 结构体字段是重建参数的唯一载体，改名会静默破坏 runner 与单测的对应关系。
	typ := reflect.TypeOf(ReindexSliceReq{})
	for _, name := range []string{"Source", "Dest", "From", "To", "Size", "Filter"} {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("ReindexSliceReq 缺少字段 %s", name)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
