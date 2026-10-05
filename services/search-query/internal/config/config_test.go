package config

import (
	"path/filepath"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// TestExampleYamlMatchesStruct 校验 etc/searchquery.v1.yaml 与配置结构一致。
//
// 目的：go-zero 的 conf.Load 对未识别的 key 会直接报错，因此这个测试能同时抓住
// 三类事故：配置项改名后 yaml 没跟上、yaml 写了结构体里没有的 key、
// 默认值被改掉（例如 PsLimit 变成 0 会让所有页大小请求走兜底分支）。
// 只解析本地 yaml，不连接任何 MySQL/Redis/OpenSearch。
func TestExampleYamlMatchesStruct(t *testing.T) {
	var c Config
	path := filepath.Join("..", "..", "etc", "searchquery.v1.yaml")
	if err := conf.Load(path, &c); err != nil {
		t.Fatalf("load %s: %v", path, err)
	}

	if c.Name != "search-query.v1.rpc" || c.Etcd.Key != "search-query.v1.rpc" {
		t.Errorf("service name/etcd key must be search-query.v1.rpc, got name=%q key=%q", c.Name, c.Etcd.Key)
	}
	if c.ListenOn != "0.0.0.0:8107" {
		t.Errorf("ListenOn = %q, want 0.0.0.0:8107", c.ListenOn)
	}
	if c.DataSource == "" {
		t.Error("DataSource must be set in the example config")
	}
	// 示例配置绝不能带真实口令（AGENTS.md：密钥走 Secret/Vault）。
	if c.OpenSearch.Password != "" {
		t.Error("OpenSearch.Password must stay empty in committed example config")
	}
	if len(c.OpenSearch.Endpoints) == 0 || c.OpenSearch.Alias == "" {
		t.Error("example config must show endpoints + read alias so the degrade switch is explicit")
	}
	if c.OpenSearch.TimeoutMs <= 0 {
		t.Errorf("OpenSearch.TimeoutMs = %d, must be positive to keep the client bounded", c.OpenSearch.TimeoutMs)
	}
	if c.Search.PsLimit <= 0 || c.Search.PsDefault <= 0 || c.Search.PsDefault > c.Search.PsLimit {
		t.Errorf("bad ps config: default=%d limit=%d", c.Search.PsDefault, c.Search.PsLimit)
	}
	if c.Search.MaxOffset <= 0 || int64(c.Search.MaxOffset)+int64(c.Search.PsLimit) > c.OpenSearch.MaxResultWindow {
		t.Errorf("max_offset=%d + ps_limit=%d must fit engine window=%d",
			c.Search.MaxOffset, c.Search.PsLimit, c.OpenSearch.MaxResultWindow)
	}
	if c.Search.KeywordMaxLen <= 0 || c.Search.HistoryLimit <= 0 || c.Search.HotKeywordLimit <= 0 {
		t.Error("keyword/history/hot limits must be positive")
	}
}

// TestHighlightTagsDefaults 确认未配置标签时回落到 <em>/</em>。
func TestHighlightTagsDefaults(t *testing.T) {
	pre, post := SearchConf{}.HighlightTags()
	if pre != "<em>" || post != "</em>" {
		t.Fatalf("default highlight tags = %q/%q, want <em>/</em>", pre, post)
	}
	pre, post = SearchConf{HighlightPreTag: "[", HighlightPostTag: "]"}.HighlightTags()
	if pre != "[" || post != "]" {
		t.Fatalf("configured highlight tags = %q/%q, want [/]", pre, post)
	}
}
