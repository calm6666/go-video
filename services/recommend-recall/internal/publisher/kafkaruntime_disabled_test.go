//go:build !recommendrecall_kafka

// kafkaruntime_disabled_test.go 钉住默认构建的「显式失败」口径。
//
// 这里要防的错误只有一种：默认二进制带上发送端之后，「编译得过」会被读成「事件在发」。
// 所以用例断言的不是「返回了某个错误」，而是错误文本能让运维照着做决定
// （要么 -tags recommendrecall_kafka 重建，要么把 Kafka.Enabled 关掉），
// 并且 Enabled=true 也不能绕过这条结论。
package publisher

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/recommend-recall/internal/config"
)

func TestDisabledBuildRejectsEverySender(t *testing.T) {
	// 参数齐备也要失败：失败原因是没链接运行时，不是配置不合格。
	sender, err := NewSender(SenderSettings{
		Brokers: []string{"127.0.0.1:9092"},
		Topics:  RequiredTopics(),
	})
	if sender != nil {
		t.Errorf("默认构建不应返回发送端实例：%+v", sender)
	}
	if !errors.Is(err, ErrKafkaRuntimeNotBuilt) {
		t.Fatalf("错误=%v，期望 ErrKafkaRuntimeNotBuilt：svc 用 logx.Must 拦住启动，靠的是这个哨兵可判定", err)
	}
	msg := err.Error()
	for _, want := range []string{"recommendrecall_kafka", "broker", "Kafka.Enabled"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误=%s，期望点名恢复手段之一 %q", msg, want)
		}
	}
}

func TestDisabledBuildRuntimeNotes(t *testing.T) {
	disabled := strings.Join(RuntimeNotes(config.KafkaConf{Enabled: false}), "\n")
	if !strings.Contains(disabled, "未链接") {
		t.Errorf("默认构建必须说明发送端未链接：%s", disabled)
	}
	// 不投递时运维最关心积压在哪、下游少了什么。
	for _, want := range []string{"recall_outbox", RequiredTopic(), "池版本"} {
		if !strings.Contains(disabled, want) {
			t.Errorf("Enabled=false 的说明要点名 %q：%s", want, disabled)
		}
	}

	// Enabled=true 但构建没链接发送端：说明仍是「未链接」，不能因为开关打开就报好消息。
	enabled := strings.Join(RuntimeNotes(config.KafkaConf{Enabled: true}), "\n")
	if !strings.Contains(enabled, "未链接") {
		t.Errorf("Enabled=true 也不能声称能投递：%s", enabled)
	}
	if strings.Contains(enabled, "已链接") || strings.Contains(enabled, "已在投递") {
		t.Errorf("默认构建出现「已链接/已在投递」字样：%s", enabled)
	}
	if !strings.Contains(enabled, "ErrKafkaRuntimeNotBuilt") {
		t.Errorf("Enabled=true 要说明进程会启动失败并可定位哨兵：%s", enabled)
	}
}
