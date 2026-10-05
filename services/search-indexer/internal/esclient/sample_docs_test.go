package esclient

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sampleDocsPath 是 deploy/opensearch 下样例文档相对本包的路径。
// 该文件由 scripts/es-init.ps1 灌进开发索引，属于「文档脚本」的一部分，
// 因此必须在这里锁住与 ContentDoc / mapping 的一致性，而不是等脚本运行时报错。
func sampleDocsPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "..", "deploy", "opensearch", "samples", "content.sample.ndjson")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("样例文档缺失 %s: %v", p, err)
	}
	return p
}

// TestSampleDocsValidateAsContentDoc 样例文档必须能按投影结构解析并通过校验。
func TestSampleDocsValidateAsContentDoc(t *testing.T) {
	docs := readSampleDocs(t)
	if len(docs) < 3 {
		t.Fatalf("样例文档太少（%d 条），覆盖不到 UGC/PGC/直播与状态过滤", len(docs))
	}

	seen := make(map[string]struct{}, len(docs))
	var hasOffline, hasPublished bool
	for i, raw := range docs {
		var doc ContentDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("第 %d 行样例文档解析失败: %v", i+1, err)
		}
		if err := doc.Validate(); err != nil {
			t.Errorf("第 %d 行样例文档未通过 Validate: %v", i+1, err)
		}
		if _, dup := seen[doc.ID()]; dup {
			t.Errorf("文档主键重复：%s（bulk 会互相覆盖，脚本命中数不可信）", doc.ID())
		}
		seen[doc.ID()] = struct{}{}
		switch {
		case doc.State == StatePublished:
			hasPublished = true
		case doc.State == StateOffline:
			hasOffline = true
		}
		if doc.PublishAt < doc.Ctime {
			t.Errorf("文档 %s publish_at 早于 ctime", doc.ID())
		}
	}
	if !hasPublished || !hasOffline {
		t.Errorf("样例必须同时包含可检索（state=%d）与下架（state=%d）文档，用于负向断言", StatePublished, StateOffline)
	}
}

// TestSampleDocsFieldsExistInMapping 拦住「样例里有字段、mapping 里没有」：
// 这类漂移在集群上会被动态映射猜成意外类型，之后只能重建索引。
func TestSampleDocsFieldsExistInMapping(t *testing.T) {
	props := mappingsProperties(t, mustParseIndexBody(t, DefaultAnalyzer()))
	docs := readSampleDocs(t)
	for i, raw := range docs {
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("第 %d 行不是 JSON 对象: %v", i+1, err)
		}
		for k := range obj {
			if _, ok := props[k]; !ok {
				t.Errorf("第 %d 行字段 %q 不在索引结构里", i+1, k)
			}
		}
		if heat, ok := obj["heat"].(map[string]interface{}); ok {
			hp, ok := props["heat"].(map[string]interface{})["properties"].(map[string]interface{})
			if !ok {
				t.Fatal("索引结构缺少 heat.properties")
			}
			for k := range heat {
				if _, ok := hp[k]; !ok {
					t.Errorf("第 %d 行 heat.%q 不在索引结构里", i+1, k)
				}
			}
		}
	}
}

// TestSampleDocsHaveNoSensitiveValues 样例文档是公开文件，不允许出现任何真实标识或凭据。
func TestSampleDocsHaveNoSensitiveValues(t *testing.T) {
	for i, raw := range readSampleDocs(t) {
		s := string(raw)
		for _, forbidden := range []string{"138", "passwd", "password", "token", "access_key", "secret"} {
			if strings.Contains(strings.ToLower(s), forbidden) {
				t.Errorf("第 %d 行样例文档含疑似敏感取值 %q", i+1, forbidden)
			}
		}
	}
}

func readSampleDocs(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open(sampleDocsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var docs [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		docs = append(docs, []byte(line))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读取样例文档失败: %v", err)
	}
	return docs
}

func mustParseIndexBody(t *testing.T, a Analyzer) map[string]interface{} {
	t.Helper()
	raw, err := IndexBody(DefaultSchemaVersion, a)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(body, map[string]interface{}{}) {
		t.Fatal("IndexBody 返回空结构")
	}
	return body
}
