//go:build !liveroom_kafka

// kafkaruntime_disabled_test.go 钉住默认构建的「不会假装在消费」。
//
// 这里最重要的行为断言是 Start 的 Enabled=true 分支：必须返回 ErrKafkaRuntimeNotBuilt，
// 由入口的 logx.Must 终止启动。若它退化成「返回 nil 继续跑」，
// 房间投影会安静地落后于真实流状态，而且没有任何信号。
// 与构建无关的 wiring 行为（关闭开关、缺 ServiceContext）在 wiring_test.go 里。
package consumer

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/live-room/internal/svc"
)

func TestDefaultBuildRefusesToProvideFactory(t *testing.T) {
	factory, err := NewKqFactory()
	if !errors.Is(err, ErrKafkaRuntimeNotBuilt) {
		t.Fatalf("默认构建必须报 ErrKafkaRuntimeNotBuilt，实得: %v", err)
	}
	if factory != nil {
		t.Fatal("未链接运行时时不得返回工厂，否则 Supervisor 会以为消费者可用")
	}
	// 错误文案要能自解释：值班只会照错误里写的改。
	msg := err.Error()
	for _, want := range []string{"liveroom_kafka", "Kafka.Enabled"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误应给出可执行的下一步（缺 %q）: %s", want, msg)
		}
	}
}

func TestDefaultRuntimeNotesSayWhatIsMissing(t *testing.T) {
	k := validKafka()
	k.Enabled = false
	notes := strings.Join(RuntimeNotes(k), "\n")
	if !strings.Contains(notes, "未链接") || !strings.Contains(notes, "liveroom_kafka") {
		t.Fatalf("必须说明默认构建没有链接 Kafka: %s", notes)
	}
	if strings.Contains(notes, "已在消费") || strings.Contains(notes, "消费正常") {
		t.Fatalf("默认构建不得给出「在消费」的印象: %s", notes)
	}
	// Enabled=false 时要写清投影由谁推进、事件会停在哪：这是排查「房间状态不更新」的第一条线索。
	for _, want := range []string{"live.state.v1", "ReportStreamState", "live_ingest_outbox"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("未启用消费的说明缺少 %q: %s", want, notes)
		}
	}

	k.Enabled = true
	if notes = strings.Join(RuntimeNotes(k), "\n"); !strings.Contains(notes, "终止") {
		t.Fatalf("开关打开但运行时未链接时必须说明会终止启动: %s", notes)
	}
}

func TestStartWithoutRuntimeFailsLoudly(t *testing.T) {
	c := validConfig()
	c.Kafka.Enabled = true
	sup, err := Start(c, &svc.ServiceContext{})
	if !errors.Is(err, ErrKafkaRuntimeNotBuilt) {
		t.Fatalf("Enabled=true 且未链接运行时应终止启动，实得: %v", err)
	}
	if sup != nil {
		t.Fatal("失败路径不得返回 supervisor")
	}
}
