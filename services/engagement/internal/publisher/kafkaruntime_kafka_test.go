//go:build engagement_kafka

// kafkaruntime_kafka_test.go 只验证「通道建立与参数守卫」，不做任何真实投递。
//
// 为什么能离线跑：kq.NewPusher 只 new 一个 kafka.Writer（Addr 是懒解析，
// 同步模式下连 ChunkExecutor 都不建），构造与 Close 都不产生网络往返。
// 用例因此绝不向已登记的 topic 调 Send：那会真的去拨 broker，
// 在本仓库的离线环境里表现为超时而不是可断言的错误。
package publisher

import (
	"context"
	"strings"
	"testing"

	"go-video/services/engagement/internal/config"
)

func newTaggedSender(t *testing.T, settings SenderSettings) Sender {
	t.Helper()
	sender, err := NewSender(settings)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	return sender
}

// TestTaggedSenderBuildsChannelsWithoutNetwork 钉住：带 tag 的构建能按配置
// 为每个 topic 建立写入通道，重复 topic 只建一条。
func TestTaggedSenderBuildsChannelsWithoutNetwork(t *testing.T) {
	sender := newTaggedSender(t, SenderSettings{
		Brokers: []string{"127.0.0.1:9092", "127.0.0.1:9093"},
		Topics:  []string{RequiredTopic(), RequiredTopic()},
	})
	kSender, ok := sender.(*kqSender)
	if !ok {
		t.Fatalf("返回类型=%T，期望 *kqSender", sender)
	}
	if got := strings.Join(kSender.Topics(), ","); got != RequiredTopic() {
		t.Errorf("已建通道=%q，期望去重后只剩 %s", got, RequiredTopic())
	}
	if err := sender.Close(); err != nil {
		t.Errorf("从未拨过 broker 的 writer 关闭应成功：%v", err)
	}
	if got := kSender.Topics(); len(got) != 0 {
		t.Errorf("Close 后仍报告 topic：%v", got)
	}
	// 重复 Close 必须仍然安全：svc 的收尾路径可能关两次（Stop + 显式 Stop）。
	if err := sender.Close(); err != nil {
		t.Errorf("二次 Close 应幂等：%v", err)
	}
}

// TestTaggedSenderRejectsIncompleteSettings 逐条钉住入口守卫：
// 这些错误必须在建通道之前返回，否则会留下半截的 producer。
func TestTaggedSenderRejectsIncompleteSettings(t *testing.T) {
	cases := []struct {
		name string
		args SenderSettings
		want string
	}{
		{"没有 broker", SenderSettings{Topics: []string{RequiredTopic()}}, "requires brokers"},
		{"broker 是空串", SenderSettings{Brokers: []string{" "}, Topics: []string{RequiredTopic()}}, "broker[0] is empty"},
		{"没有 topic", SenderSettings{Brokers: []string{"127.0.0.1:9092"}}, "at least one topic"},
		{"topic 含空串", SenderSettings{Brokers: []string{"127.0.0.1:9092"}, Topics: []string{RequiredTopic(), " "}}, "topic 含空串"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender, err := NewSender(tc.args)
			if err == nil {
				_ = sender.Close()
				t.Fatalf("应拒绝 %+v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误=%v，期望点名 %q", err, tc.want)
			}
			if sender != nil {
				t.Errorf("拒绝时不应返回半截发送端：%+v", sender)
			}
		})
	}
}

// TestTaggedSendRejectsBeforeDialing 钉住两类在触网之前就返回的错误：
// 未登记的 topic（配置与 model 派生的 topic 漂移）与空分区键（会打散同一对象的点赞与取消点赞）。
func TestTaggedSendRejectsBeforeDialing(t *testing.T) {
	sender := newTaggedSender(t, SenderSettings{
		Brokers: []string{"127.0.0.1:9092"},
		Topics:  []string{RequiredTopic()},
	})
	ctx := context.Background()

	err := sender.Send(ctx, "content.published.v1", "101", `{}`)
	if err == nil || !strings.Contains(err.Error(), "未建立发送通道") {
		t.Errorf("未登记 topic 应报错：%v", err)
	}
	err = sender.Send(ctx, RequiredTopic(), "", `{}`)
	if err == nil || !strings.Contains(err.Error(), "partition key") {
		t.Errorf("空分区键应报错：%v", err)
	}
}

// TestTaggedRuntimeNotes 钉住「已链接」不等于「已在投递」的口径，
// 以及无论哪种组合都必须承认没有 broker 侧证据。
func TestTaggedRuntimeNotes(t *testing.T) {
	disabled := strings.Join(RuntimeNotes(config.KafkaConf{Enabled: false}), "\n")
	if !strings.Contains(disabled, "已链接") {
		t.Errorf("应说明发送端已链接：%s", disabled)
	}
	if !strings.Contains(disabled, "engagement_outbox") {
		t.Errorf("Enabled=false 要点名积压位置：%s", disabled)
	}
	enabled := strings.Join(RuntimeNotes(config.KafkaConf{Enabled: true}), "\n")
	if !strings.Contains(enabled, "已链接") || !strings.Contains(enabled, "联调") {
		t.Errorf("Enabled=true 必须同时说明未与 broker 联调：%s", enabled)
	}
	if !strings.Contains(enabled, "search-indexer") || !strings.Contains(enabled, "inbox") {
		t.Errorf("Enabled=true 要点名下游消费者的构建标签，否则运维以为开了开关索引就会更新：%s", enabled)
	}
	if strings.Contains(enabled, "engagement_outbox") {
		t.Errorf("Enabled=true 不该再说不投递：%s", enabled)
	}
}
