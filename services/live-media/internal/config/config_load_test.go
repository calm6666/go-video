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
// 这里用 conf.Load 真实读取 etc/*.yaml，不 mock、不跳过。
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
			// conf.Load 只在 Config 实现 conf.Validateable 时才调用 Validate()。
			// 这里显式再调一次（zrpc.RpcServerConf 内嵌的 ServiceConf.Validate）：
			// 一旦哪天字段遮蔽了内嵌方法，Validate 会变成编译期错误而不是"静默不校验"。
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() 失败: %v", err)
			}
			checkFields(t, reflect.ValueOf(c))
			checkLiveMediaDefaults(t, c)
		})
	}
}

// checkLiveMediaDefaults 锁定直播媒体参数的取值边界：这些值直接决定任务超时、
// 分片时长与分页上限，配置写错会让 logic 层的校验整体失效。
func checkLiveMediaDefaults(t *testing.T, c Config) {
	t.Helper()
	lm := c.LiveMedia
	if lm.MaxListPageSize <= 0 || lm.MaxListPageSize > 100 {
		t.Errorf("LiveMedia.MaxListPageSize=%d 不合理（应为 1..100）", lm.MaxListPageSize)
	}
	if lm.DefaultRecordSegmentSeconds <= 0 || lm.DefaultRecordSegmentSeconds > lm.MaxRecordSegmentSeconds {
		t.Errorf("默认分片时长 %d 超过上限 %d", lm.DefaultRecordSegmentSeconds, lm.MaxRecordSegmentSeconds)
	}
	if lm.DefaultSegmentPageSize <= 0 || lm.DefaultSegmentPageSize > lm.MaxSegmentPageSize {
		t.Errorf("默认切片页大小 %d 超过上限 %d", lm.DefaultSegmentPageSize, lm.MaxSegmentPageSize)
	}
	if lm.DefaultTranscodeTimeoutSeconds <= 0 || lm.DefaultRecordTimeoutSeconds <= 0 {
		t.Error("任务超时秒数必须为正，否则超时清扫会把所有任务判失败")
	}
	if lm.DefaultRetentionBatchLimit <= 0 || lm.DefaultRetentionBatchLimit > lm.MaxRetentionBatchLimit {
		t.Errorf("回收批量默认值 %d 超过上限 %d", lm.DefaultRetentionBatchLimit, lm.MaxRetentionBatchLimit)
	}
	if lm.TaskTimeoutSweepEnabled {
		t.Error("契约轮禁止开启超时清扫：清扫依赖 logic 实现，先关闭再接线")
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
			if sf.Name == "DataSource" && !strings.Contains(f.String(), "go_video_live_media") {
				t.Errorf("DataSource 必须指向 go_video_live_media 库，实际 %s", f.String())
			}
		case reflect.Struct:
			if f.Type() == redisConfType {
				if f.Interface().(redis.RedisConf).Host == "" {
					t.Errorf("%s.Host 为空，缓存客户端无法构造", sf.Name)
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
