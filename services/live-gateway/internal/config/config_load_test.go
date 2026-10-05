package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// etc 示例配置必须真的能被 go-zero 加载：本仓库曾出现过 Config 自带的 Redis 字段
// 与 zrpc.RpcServerConf 内嵌的 Redis 同名，代码可编译但启动即报 "conflict key redis"。
// 对 live-gateway 尤其致命——Redis 是连接租约的主存储，配置加载失败等于服务起不来。
func TestExampleConfigsLoad(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "etc", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatal("etc 目录下没有示例配置")
	}
	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			var c Config
			if err := conf.Load(f, &c); err != nil {
				t.Fatalf("加载 %s 失败: %v", f, err)
			}
			checkFields(t, reflect.ValueOf(c))
			checkLiveGatewayBounds(t, c)
		})
	}
}

// checkLiveGatewayBounds 锁定租约/票据 TTL 与载荷上限的边界关系：
// 这三个值直接决定 Redis 键的存活时间与广播拒绝口径，配错会让限流与越权校验形同虚设。
func checkLiveGatewayBounds(t *testing.T, c Config) {
	t.Helper()
	lg := c.LiveGateway
	if lg.MinLeaseTTLSeconds <= 0 || lg.MinLeaseTTLSeconds > lg.DefaultLeaseTTLSeconds {
		t.Errorf("租约下限 %d 不能大于默认 TTL %d", lg.MinLeaseTTLSeconds, lg.DefaultLeaseTTLSeconds)
	}
	if lg.DefaultLeaseTTLSeconds > lg.MaxLeaseTTLSeconds {
		t.Errorf("默认租约 TTL %d 超过上限 %d", lg.DefaultLeaseTTLSeconds, lg.MaxLeaseTTLSeconds)
	}
	if lg.DefaultTicketTTLSeconds <= 0 || lg.DefaultTicketTTLSeconds > lg.MaxTicketTTLSeconds {
		t.Errorf("票据默认 TTL %d 超过上限 %d", lg.DefaultTicketTTLSeconds, lg.MaxTicketTTLSeconds)
	}
	if lg.ReconnectGraceSeconds > lg.DefaultTicketTTLSeconds {
		t.Errorf("断线宽限期 %d 超过票据 TTL %d：宽限期内必然换不到租约",
			lg.ReconnectGraceSeconds, lg.DefaultTicketTTLSeconds)
	}
	if lg.MaxPayloadBytes <= 0 {
		t.Error("MaxPayloadBytes 必须为正，否则广播无上限")
	}
	if lg.PayloadDigestBytes <= 0 || lg.PayloadDigestBytes > 64 {
		t.Errorf("PayloadDigestBytes=%d 超出 sha256 hex 可用长度（1..64）", lg.PayloadDigestBytes)
	}
	if lg.DefaultRoomBroadcastQps <= 0 || lg.DefaultUserBroadcastQps <= 0 {
		t.Error("默认 QPS 必须为正：0 会让所有广播被判定为限流")
	}
	if lg.BroadcastLogRetentionDays <= 0 {
		t.Error("广播审计流水必须设保留期，0 表示无限增长")
	}
}

var redisConfType = reflect.TypeOf(redis.RedisConf{})

func checkFields(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		sf := v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		f := v.Field(i)
		// 内嵌字段都是 go-zero 框架配置（RpcServerConf/ServiceConf/LogConf），
		// 其内部可选结构（如 zrpc 鉴权用的 RedisKeyInfo）留空是正常的，不检查。
		if sf.Anonymous {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			if strings.HasSuffix(sf.Name, "DataSource") && f.String() == "" {
				t.Errorf("%s 为空，服务启动后无法访问数据库", sf.Name)
			}
			// 库名必须是本服务自己的 schema（AGENTS.md §5），防止误连其它服务库。
			if sf.Name == "DataSource" && !strings.Contains(f.String(), "go_video_live_gateway") {
				t.Errorf("DataSource 必须指向 go_video_live_gateway 库，实际 %s", f.String())
			}
			// 密钥类字段不得出现在示例配置里（AGENTS.md §4：生产密钥进 Secret/Vault）。
			if sf.Name == "Token" && f.String() != "" {
				t.Errorf("%s 不应在示例配置里写入明文 Token", sf.Name)
			}
		case reflect.Struct:
			if f.Type() == redisConfType {
				if f.Interface().(redis.RedisConf).Host == "" {
					t.Errorf("%s.Host 为空，长连接主存储无法构造", sf.Name)
				}
				continue
			}
			checkFields(t, f)
		case reflect.Slice:
			// Kafka.Brokers 等切片允许为空（表示未接线），但不得含空串元素。
			for j := 0; j < f.Len(); j++ {
				if f.Index(j).Kind() == reflect.String && f.Index(j).String() == "" {
					t.Errorf("%s[%d] 为空字符串，配置里不应保留空元素", sf.Name, j)
				}
			}
		}
	}
}
