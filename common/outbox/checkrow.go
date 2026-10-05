// checkrow.go 是各服务 Store 适配共用的「这一行能不能发」反查。
//
// 为什么放在 common：每张事件 outbox 表的列映射不同（有的存 aggregate_id，
// 有的存 content_id/uid），但发布前的四条判据完全一致，而且都是同一类事故：
// 列与 payload 不同源。写歪一次的后果是消费方按「另一个事件」去重，
// 顺序与幂等同时失效（live-ingest 的 buildStateEnvelopePayload 就是这种三处同源约束）。
// 五个服务各抄一份这种判据，只会让某个服务漏抄其中一条。
package outbox

import (
	"encoding/json"
	"fmt"

	"go-video/common/eventenvelope"
)

// CheckRow 返回该行不可发布的原因；空串表示行自洽、可以投递。
//
// allowedTopics 必须是调用方服务真实产出的 topic 集合，且应由各服务 model 的
// event_type + schema_version 常量拼出，不要在配置或字面量里重复一遍。
//
// 本函数只写 Row.Defect，不返回 error：判死是这一行的结论，不是循环的失败，
// 真正的读库错误留给 Store 冒泡。
func CheckRow(row *Row, allowedTopics []string) string {
	if row == nil {
		return "row is nil"
	}
	allowed := make(map[string]struct{}, len(allowedTopics))
	var declared []string
	for _, topic := range allowedTopics {
		if topic == "" {
			continue
		}
		if _, dup := allowed[topic]; dup {
			continue
		}
		allowed[topic] = struct{}{}
		declared = append(declared, topic)
	}
	if len(declared) == 0 {
		return "调用方没声明本服务产出的 topic，无法判定归属"
	}
	if _, ok := allowed[row.Topic]; !ok {
		return fmt.Sprintf("topic %q 不属于本服务（本表只产出 %v）", row.Topic, declared)
	}
	if row.Key == "" {
		return "分区键为空，无法用分区键保证同聚合根顺序"
	}
	if row.EventID == "" {
		return "event_id 列为空，消费方无法按它去重"
	}
	// Envelope.UnmarshalJSON 自带 Validate：残缺信封在这里就拦下，不投给下游判死。
	var env eventenvelope.Envelope
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		return "payload 不是合法事件信封: " + err.Error()
	}
	if env.EventID != row.EventID {
		return fmt.Sprintf("payload event_id=%q 与列 event_id=%q 不一致", env.EventID, row.EventID)
	}
	if env.AggregateID != row.Key {
		return fmt.Sprintf("payload aggregate_id=%q 与列分区键=%q 不一致", env.AggregateID, row.Key)
	}
	return ""
}
