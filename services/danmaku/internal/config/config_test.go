package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// TestLoadReleaseYaml 校验 etc/danmaku.v1.yaml 能被完整解析，
// 且端口/服务名与 docs/service-catalog.md 登记值一致。
// 这里只解析配置，不会连接 MySQL/Redis/etcd。
func TestLoadReleaseYaml(t *testing.T) {
	var c Config
	path := filepath.Join("..", "..", "etc", "danmaku.v1.yaml")
	if err := conf.Load(path, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载 %s 失败: %v", path, err)
	}

	if c.Name != "danmaku.v1.rpc" {
		t.Fatalf("Name = %q, want danmaku.v1.rpc（etcd 注册键须与 docs/service-catalog.md 一致）", c.Name)
	}
	if c.ListenOn != "0.0.0.0:8103" {
		t.Fatalf("ListenOn = %q, want 0.0.0.0:8103", c.ListenOn)
	}
	if c.CacheRedis.Host == "" || c.CacheRedis.Type == "" {
		t.Fatalf("CacheRedis 配置缺失: %+v", c.CacheRedis)
	}
	if c.DataSource == "" {
		t.Fatal("DataSource 不能为空")
	}
	if c.Etcd.Key != c.Name {
		t.Fatalf("Etcd.Key = %q 应与 Name = %q 一致", c.Etcd.Key, c.Name)
	}

	want := DanmakuConf{
		SegmentSeconds:            6,
		MaxContentLength:          100,
		MaxPerUserPerMinute:       20,
		MaxPerOidPerMinute:        6000,
		PostQps:                   800,
		PostBurst:                 200,
		SensitiveWordCheckEnabled: true,
		MachineReviewEnabled:      true,
		SegmentCacheTTLSeconds:    15,
		SegmentCountTTLSeconds:    60,
		BlockWordCacheTTLSeconds:  300,
		UserBlockCacheTTLSeconds:  120,
		BlockWordCacheMaxWords:    2000,
		MaxSegWindow:              60,
		MaxListLimit:              3000,
	}
	if c.Danmaku != want {
		t.Fatalf("Danmaku 配置解析结果与预期不符:\n got %+v\nwant %+v", c.Danmaku, want)
	}
}

// TestMinimalYamlAppliesDefaults 只写必填项时，嵌套结构体的 default 标签
// 必须生效，避免运维漏配导致段宽为 0 这类隐性故障。
func TestMinimalYamlAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "danmaku.min.yaml")
	content := "Name: danmaku.v1.rpc\nListenOn: 127.0.0.1:8103\nCacheRedis:\n  Host: 127.0.0.1:6379\n  Type: node\nDataSource: dsn\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}

	var c Config
	if err := conf.Load(file, &c, conf.UseEnv()); err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if c.Danmaku.SegmentSeconds != 6 || c.Danmaku.MaxContentLength != 100 {
		t.Fatalf("嵌套默认值未生效: %+v", c.Danmaku)
	}
	if !c.Danmaku.MachineReviewEnabled || !c.Danmaku.SensitiveWordCheckEnabled {
		t.Fatalf("机审/屏蔽词门禁默认应为开启: %+v", c.Danmaku)
	}
	if c.ModerationRPC.Target != "" || len(c.ModerationRPC.Endpoints) > 0 || len(c.ModerationRPC.Etcd.Hosts) > 0 {
		t.Fatalf("ModerationRPC 未配置时应为零值, got %+v", c.ModerationRPC)
	}
	// go-zero 的语义：optional 嵌套结构体整段缺失时不会填充其内部 default，
	// 因此 Timeout 仍是 0。svc 侧以「零值即未配置」判定并拒绝构造客户端，
	// 这里显式钉住该行为，避免日后误以为可以依赖内嵌默认值。
	if c.ModerationRPC.Timeout != 0 {
		t.Fatalf("ModerationRPC 整段缺失时 Timeout 应为 0, got %d", c.ModerationRPC.Timeout)
	}
}
