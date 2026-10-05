// repository_test.go 覆盖投影层的纯函数规则：索引命名、别名归一化、
// 重建范围校验与错误摘要脱敏。全部不连 MySQL / Redis / OpenSearch。
package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go-video/services/search-indexer/model"
)

func TestOptionsNormalize(t *testing.T) {
	// IndexPrefix 为空 ⇒ 没有任何投影能力，必须快速失败而不是用默认值蒙混。
	var empty Options
	if err := empty.normalize(); !errors.Is(err, model.ErrIndexPrefixEmpty) {
		t.Fatalf("空前缀应返回 ErrIndexPrefixEmpty，实际 %v", err)
	}

	o := Options{IndexPrefix: "  Go_Video_Content ", RetryOnConflict: -1}
	if err := o.normalize(); err != nil {
		t.Fatal(err)
	}
	if o.IndexPrefix != "go_video_content" {
		t.Fatalf("别名前缀应小写去空白，实际 %q", o.IndexPrefix)
	}
	if o.SchemaVersion != "v1" {
		t.Fatalf("结构版本默认值 = %q, want v1", o.SchemaVersion)
	}
	if o.SliceSpan <= 0 || o.SliceSize <= 0 || o.StopAfterEmptySlices <= 0 {
		t.Fatalf("重建切片参数未补齐: %+v", o)
	}
	if o.RetryOnConflict != 0 {
		t.Fatalf("负 RetryOnConflict 应夹到 0，实际 %d", o.RetryOnConflict)
	}
	// OpenSearch 索引名不允许大写。
	o2 := Options{IndexPrefix: "ABC", SchemaVersion: "V2"}
	if err := o2.normalize(); err != nil {
		t.Fatal(err)
	}
	if o2.SchemaVersion != "v2" {
		t.Fatalf("SchemaVersion 未小写: %q", o2.SchemaVersion)
	}
}

func TestOptionsAliasAndIndexName(t *testing.T) {
	o := Options{IndexPrefix: "go_video_content", SchemaVersion: "v1"}
	if got := o.DefaultAlias(); got != "go_video_content" {
		t.Fatalf("DefaultAlias = %q", got)
	}
	// 空别名一律落到默认别名，避免调用方漏传就写到无别名的孤儿索引。
	if got := o.Alias("   "); got != o.IndexPrefix {
		t.Fatalf("Alias(\"\") = %q", got)
	}
	if got := o.Alias("  Live_Zone  "); got != "live_zone" {
		t.Fatalf("Alias 归一化异常: %q", got)
	}

	name := o.NewIndexName("")
	if !strings.HasPrefix(name, "go_video_content_v1_") {
		t.Fatalf("索引名必须以 <alias>_<schema>_ 开头（SwitchAlias 据此拒绝越界索引）: %q", name)
	}
	if strings.ToLower(name) != name {
		t.Fatalf("索引名必须全小写: %q", name)
	}
	// 时间戳后缀保证同一别名可反复重建而不撞名。
	suffix := strings.TrimPrefix(name, "go_video_content_v1_")
	if suffix == name || suffix == "" {
		t.Fatalf("索引名缺少时间戳后缀: %q", name)
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			t.Fatalf("索引名后缀不是 Unix 秒: %q", name)
		}
	}
}

func TestValidateScope(t *testing.T) {
	cases := []struct {
		name       string
		scope      string
		in         string
		want       string
		wantErrIs  error
		wantErrNot bool
	}{
		{name: "全量忽略取值", scope: model.ScopeFull, in: "任意", want: ""},
		{name: "全量空取值", scope: model.ScopeFull, in: "", want: ""},
		{name: "按内容类型", scope: model.ScopeContentType, in: " 2 ", want: "2"},
		{name: "内容类型 0 越界", scope: model.ScopeContentType, in: "0", wantErrIs: model.ErrInvalidScope},
		{name: "内容类型 4 越界", scope: model.ScopeContentType, in: "4", wantErrIs: model.ErrInvalidScope},
		{name: "内容类型非数字", scope: model.ScopeContentType, in: "ugc", wantErrIs: model.ErrInvalidScope},
		{name: "分区区间归一化", scope: model.ScopePartition, in: " 1000 - 1999 ", want: "1000-1999"},
		{name: "分区单点", scope: model.ScopePartition, in: "5-5", want: "5-5"},
		{name: "分区上下界颠倒", scope: model.ScopePartition, in: "2000-1000", wantErrIs: model.ErrInvalidScope},
		{name: "分区缺上界", scope: model.ScopePartition, in: "1000", wantErrIs: model.ErrInvalidScope},
		{name: "分区非数字", scope: model.ScopePartition, in: "a-b", wantErrIs: model.ErrInvalidScope},
		{name: "未知 scope", scope: "author", in: "1", wantErrIs: model.ErrInvalidScope},
		{name: "空 scope", scope: "", in: "", wantErrIs: model.ErrInvalidScope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateScope(c.scope, c.in)
			if c.wantErrIs != nil {
				if !errors.Is(err, c.wantErrIs) {
					t.Fatalf("错误 = %v, want %v", err, c.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("合法输入被拒绝: %v", err)
			}
			if got != c.want {
				t.Fatalf("归一化取值 = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPayloadDigest(t *testing.T) {
	payload := []byte(`{"event_id":"e1","payload":{"action":"publish"}}`)
	got := PayloadDigest(payload)
	if len(got) != 32 {
		t.Fatalf("摘要长度 = %d, want 32（对应 CHAR(32) 列）", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("摘要不是 hex: %v", err)
	}
	full := sha256.Sum256(payload)
	if want := hex.EncodeToString(full[:])[:32]; got != want {
		t.Fatalf("摘要算法不一致: %s != %s", got, want)
	}
	// 内容不同必须得到不同摘要，否则死信比对失去意义。
	if PayloadDigest([]byte("a")) == PayloadDigest([]byte("b")) {
		t.Fatal("摘要碰撞")
	}
	// 空 payload 也要给出确定值，不能返回空串（会绕过 NOT NULL 列的语义）。
	if PayloadDigest(nil) == "" {
		t.Fatal("空 payload 摘要不应为空串")
	}
}

// TestSanitizeError last_error/reason 列宽有限且是单行文本，
// 落库前必须截断并去掉换行，否则一条大响应体会把日志与运维列表撑爆。
func TestSanitizeError(t *testing.T) {
	if got := sanitizeError(""); got != "" {
		t.Fatalf("空串应保持为空，实际 %q", got)
	}
	nl := sanitizeError("第一行\n第二行\r\n第三行")
	if strings.ContainsAny(nl, "\n\r") {
		t.Fatalf("换行未清除: %q", nl)
	}
	long := sanitizeError(strings.Repeat("错误", 600))
	if len(long) > 1510 || !strings.HasSuffix(long, "...") {
		t.Fatalf("未截断: 长度=%d", len(long))
	}
	// 短错误原文保留，不能因为规范化而丢失排障线索。
	if got := sanitizeError("esclient: HTTP 503"); got != "esclient: HTTP 503" {
		t.Fatalf("短错误被改写: %q", got)
	}
}

func TestCacheNilDegeneratesToSingleInstance(t *testing.T) {
	// NewCache(nil) 返回 nil：没有 Redis 时锁语义退化为「单实例总是抢到」，
	// 幂等仍由 MySQL 唯一索引保证，这一点在 README 里有说明。
	if c := NewCache(nil); c != nil {
		t.Fatal("未配置 Redis 时 Cache 应为 nil")
	}
	var c *Cache
	if ok, err := c.AcquireLock(t.Context(), "k", 1); !ok || err != nil {
		t.Fatalf("nil Cache 的 acquireLock 应放行，实际 %v/%v", ok, err)
	}
	if v, ok := c.GetActiveIndex(t.Context(), "alias"); ok || v != "" {
		t.Fatalf("nil Cache 不应有缓存命中: %q/%v", v, ok)
	}
	c.SetActiveIndex(t.Context(), "alias", "idx")
	c.DelActiveIndex(t.Context(), "alias")
	c.ReleaseLock(t.Context(), "k")
	if err := c.Ping(t.Context()); err == nil {
		t.Fatal("未配置 Redis 时 Ping 必须报错，不能假装健康")
	}
	// Configured 是 Repository.PingRedis 判定「是否真的有 Redis 后端」的唯一口径：
	// 类型化 nil *Cache 装进 Cacher 接口后接口本身非 nil，若仍用 r.cache == nil 判定
	// 会把「未配置」误判成「已配置」，这里钉住语义。
	if c.Configured() {
		t.Fatal("nil Cache 不该报告已配置")
	}
	if got := newCacheStore(nil); got == nil || got.Configured() {
		t.Fatalf("newCacheStore(nil) 必须是可安全调用的类型化 nil，实际 %v", got)
	}
	if (&Cache{rds: nil}).Configured() {
		t.Fatal("rds 为 nil 的 Cache 不该报告已配置")
	}
}
