package config

// config_test.go 校验示例配置能被 go-zero 正常解析，并确认「未配置即严格」的默认语义。
// 只解析文件，不连接 MySQL/Redis/etcd。

import (
	"path/filepath"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

func TestExampleConfigLoads(t *testing.T) {
	var c Config
	path := filepath.Join("..", "..", "etc", "catalog.v1.yaml")
	if err := conf.Load(path, &c); err != nil {
		t.Fatalf("conf.Load(%s) error = %v", path, err)
	}
	if c.Name != "catalog.v1.rpc" {
		t.Errorf("Name = %q, want catalog.v1.rpc", c.Name)
	}
	if c.CacheRedis.Host == "" {
		t.Error("CacheRedis.Host 为空，服务无法构造缓存客户端")
	}
	if len(c.RightsRPC.Etcd.Hosts) == 0 || c.RightsRPC.Etcd.Key != "rights.v1.rpc" {
		t.Errorf("RightsRPC = %+v, want etcd 服务发现且 Key=rights.v1.rpc", c.RightsRPC.Etcd)
	}
	if len(c.AssetRPC.Etcd.Hosts) == 0 || c.AssetRPC.Etcd.Key != "asset.v1.rpc" {
		t.Errorf("AssetRPC = %+v, want etcd 服务发现且 Key=asset.v1.rpc", c.AssetRPC.Etcd)
	}
	if c.DefaultRegion != "CN" {
		t.Errorf("DefaultRegion = %q, want CN", c.DefaultRegion)
	}
	if c.DisableRightsCheck || c.DisableAssetCheck {
		t.Error("示例配置默认必须开启校验，禁止默认跳过 rights/asset 检查")
	}
}

func TestZeroValueConfigIsStrict(t *testing.T) {
	// 结构体零值（未写任何校验配置）必须落在“校验开启”一侧。
	c := Config{}
	if c.DisableRightsCheck || c.DisableAssetCheck {
		t.Fatalf("零值配置关闭了校验：rights=%v asset=%v", c.DisableRightsCheck, c.DisableAssetCheck)
	}
	if c.DefaultRegion != "" {
		t.Fatalf("零值 DefaultRegion = %q, want 空（请求必须显式传 region）", c.DefaultRegion)
	}
}
