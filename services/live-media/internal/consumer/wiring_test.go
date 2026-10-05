package consumer

// wiring_test.go 覆盖与构建标签无关的接线行为：
// 开关关闭时必须什么都不做，开关打开时必须先确认依赖齐不齐。
// 「未链接运行时」这条分支只在默认构建成立，见 kafkaruntime_disabled_test.go。

import (
	"strings"
	"testing"

	"go-video/services/live-media/internal/svc"
)

// TestStartDisabledTouchesNothing 关闭开关时即使 Kafka 参数全是坏的也不报错：
// 默认构建 + 不消费是本服务当前的常态启动形态，不能被无关配置卡死；
// 同时绝不返回半启动的 supervisor。
func TestStartDisabledTouchesNothing(t *testing.T) {
	c := validConfig()
	c.Kafka.Enabled = false
	c.Kafka.Brokers = nil
	c.Kafka.SubscribeTopics = []string{"livemedia.stream.output.offline.v1"}
	c.Kafka.Offset = ""

	sup, err := Start(c, nil)
	if err != nil {
		t.Fatalf("未启用消费不应阻塞启动: %v", err)
	}
	if sup != nil {
		t.Fatal("未启用消费必须返回 nil supervisor")
	}
	if s := SettingsFrom(c); s.Brokers != nil {
		t.Fatalf("SettingsFrom 应原样透传而不是加工配置: %v", s.Brokers)
	}
}

// TestStartRequiresServiceContext 空指针守卫排在工厂之前：
// 少了 ServiceContext 就没有可执行的 logic，两种构建下都必须先报这一条。
func TestStartRequiresServiceContext(t *testing.T) {
	c := validConfig()
	c.Kafka.Enabled = true
	if _, err := Start(c, nil); err == nil || !strings.Contains(err.Error(), "ServiceContext") {
		t.Fatalf("启用消费却没给 ServiceContext 必须报错: %v", err)
	}
}

func TestNewSvcApplicatorIsAlwaysUsableHandle(t *testing.T) {
	// 只断言构造结果非 nil：真调用会进 logic 打数据库，本包单测不接任何 DB。
	// 映射本身（OfflineCommand -> OfflineSessionOutputsInput 的五个字段）不在此断言：
	// 那需要一个能跑的 ServiceContext，而 logic 侧的同名入参已由
	// internal/logic/session_offline_test.go 逐字段钉住（含 SessionID<=0 拒绝、
	// Reason 必须是 SOURCE_LOST 等值域），接线只是把那五个字段原样递过去。
	if app := NewSvcApplicator(&svc.ServiceContext{}); app == nil {
		t.Fatal("app 为 nil 时 NewSupervisor 会直接拒绝装配")
	}
}
