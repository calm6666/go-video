package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"go-video/common/eventenvelope"
	"go-video/services/live-media/model"
)

// 本文件是「两个已修生产缺陷」的回归门禁。两个缺陷都是本轮补测试时发现的，
// 在被发现前躲过了原有全部用例：#1 让本服务每个带事件的写事务恒回滚，
// #2 让 Start* 在 #1 修好后仍会把事件写成 aggregate_id="0" 并对已成功登记的任务报 NotFound。
// 与之配套的兜底机制在 fakes_test.go：requireNoEnvelopeBug 在被 #1 类契约错误阻塞时显式 Skip
// （不是通过）；#1 修复后全包 0 个 Skip、78 个用例均为真断言。
//
// 缺陷 #1（跨包契约冲突，2026-09-22 已修）：
//
//	本服务 9 个 EventType* 常量曾在首段点号后使用下划线（"livemedia.record_state_changed"），
//	而 common/eventenvelope 的 isValidEventType 只允许「小写字母 / 数字 / 点号」。
//	全仓其余生产者都用点号分段（content.published、live.state、behavior.play、
//	notification.request），只有本服务与 user-profile 例外（后者同轮一并改掉）。
//	后果：helpers.go 的 eventenvelope.New 恒返回错误 → appendOutboxEvent 恒失败 →
//	承载它的 TransactCtx 恒回滚 → 13 个录制/转码写方法里所有「推进状态并登记事件」的路径
//	一次也成功不了（不是事件丢失，而是业务写一起丢，Worker 只能无限重投）。
//	修法是常量改点号分段，同时由 common/eventenvelope/event_type_gate_test.go 做全仓门禁；
//	放宽 isValidEventType 不是选项——那条规则就是 Outbox 的地基。
//
// 缺陷 #2（本包 logic 内部，被 #1 掩盖，2026-09-22 已修）：
//
//	model.LiveRecordTaskModel.InsertTx / LiveTranscodeTaskModel.InsertTx 只把自增主键
//	作为返回值，不回填结构体字段。而 startliverecordlogic.go 与 startlivetranscodelogic.go
//	曾用 `if _, insertErr :=` 丢弃它，随后事件 aggregate_id、payload.record_id、
//	提交后回读三处继续用恒为 0 的 task.RecordId / task.TaskId。
//	现在两个 Start* 都写回主键再用于事件与回读（submitretentiontasklogic.go 一直是对的写法）。

// TestOutboxEventTypesSatisfyEnvelopeContract 要求本服务登记的每个事件类型都能构造出合法信封。
// 这是 Outbox 模式的地基：事件名不满足契约，业务事务就写不进去（AGENTS.md §5）。
func TestOutboxEventTypesSatisfyEnvelopeContract(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
	}{
		{"转码状态变更", model.EventTypeTranscodeStateChanged},
		{"录制状态推进", model.EventTypeRecordStateChanged},
		{"录制停止", model.EventTypeRecordStopped},
		{"录制缺口", model.EventTypeRecordGapDetected},
		{"档位上线", model.EventTypeStreamOutputOnline},
		{"档位下线", model.EventTypeStreamOutputOffline},
		{"回放送审", model.EventTypeReplayReviewSubmitted},
		{"回放内容态投影", model.EventTypeReplayContentStateChanged},
		{"回收结束", model.EventTypeRetentionFinished},
	}
	var bad []string
	for _, tc := range cases {
		// 与 helpers.go:appendOutboxEvent 同一调用形式：New 内含 Validate。
		if _, err := eventenvelope.New(model.ProducerName, tc.eventType, model.AggregateRecordTask,
			"1", model.EventSchemaVersion, json.RawMessage(`{"probe":1}`), "trace-probe"); err != nil {
			bad = append(bad, fmt.Sprintf("%s(%s): %v", tc.name, tc.eventType, err))
		}
	}
	if len(bad) > 0 {
		t.Fatalf("缺陷 #1：%d/%d 个事件类型过不了 common/eventenvelope 的语法契约，"+
			"因此 live-media 任何带事件的写事务都会回滚（见本文件头注释）：\n  %s",
			len(bad), len(cases), strings.Join(bad, "\n  "))
	}
	// 反向自检：语法本身没被放松到「什么都能过」——带空格与连续点号的名字必须仍被拒绝，
	// 否则上面的断言会因为契约被放宽而失去意义。
	for _, invalid := range []string{"livemedia.record state", "livemedia..record", ".livemedia"} {
		if _, err := eventenvelope.New(model.ProducerName, invalid, model.AggregateRecordTask,
			"1", model.EventSchemaVersion, json.RawMessage(`{}`), ""); err == nil {
			t.Errorf("event_type %q 应当被契约拒绝，实际通过了 —— 语法被放宽了", invalid)
		}
	}
}

// TestStartLiveRecordReturnsTheRowItCommitted 钉住缺陷 #2 的录制侧：
// 登记成功必须返回刚提交的那一行，事件也必须以该行主键为聚合标识。
func TestStartLiveRecordReturnsTheRowItCommitted(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)

	info, err := NewStartLiveRecordLogic(context.Background(), svcCtx).
		StartLiveRecord(startRecordReq("req-rec-bug2"))
	if errors.Is(err, model.ErrRecordTaskNotFound) {
		t.Fatalf("缺陷 #2：行已提交，但 startliverecordlogic.go:157 用恒为 0 的 task.RecordId 回读自己 →"+
			" 登记成功却报 ErrRecordTaskNotFound：%v", err)
	}
	if err != nil {
		t.Fatalf("首次登记必须成功。当前失败原因若含 %q 即缺陷 #1（见 TestOutboxEventTypesSatisfyEnvelopeContract）：%v",
			errEventTypeNameInvalid, err)
	}
	if info.GetRecordId() <= 0 {
		t.Errorf("登记必须返回自增 record_id，实际 %d", info.GetRecordId())
	}
	if len(db.records) != 1 {
		t.Fatalf("应恰好提交 1 行，实际 %d", len(db.records))
	}
	committed := mustRecord(t, db, firstRecordID(db))
	if committed.State != model.RecordStatePending {
		t.Errorf("登记态应为 PENDING，实际 %d", committed.State)
	}
	if info.GetRecordId() != committed.RecordId {
		t.Errorf("返回值与提交的行不是同一行：reply=%d row=%d", info.GetRecordId(), committed.RecordId)
	}
	wantEvents(t, db, []string{model.EventTypeRecordStateChanged}, "登记事件")
	if got := eventAt(t, db, 0).AggregateId; got != strconv.FormatInt(committed.RecordId, 10) {
		t.Errorf("缺陷 #2（事件侧）：aggregate_id 必须是提交后的主键，实际 %q（0 会让消费者找不到任务）", got)
	}
}

// TestStartLiveTranscodeReturnsTheRowItCommitted 钉住缺陷 #2 的转码侧（同形代码）。
func TestStartLiveTranscodeReturnsTheRowItCommitted(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)

	info, err := NewStartLiveTranscodeLogic(context.Background(), svcCtx).
		StartLiveTranscode(startTranscodeReq("req-tc-bug2"))
	if errors.Is(err, model.ErrTranscodeTaskNotFound) {
		t.Fatalf("缺陷 #2：行已提交，但 startlivetranscodelogic.go:135 用恒为 0 的 task.TaskId 回读自己 →"+
			" 登记成功却报 ErrTranscodeTaskNotFound：%v", err)
	}
	if err != nil {
		t.Fatalf("首次登记必须成功。当前失败原因若含 %q 即缺陷 #1（见 TestOutboxEventTypesSatisfyEnvelopeContract）：%v",
			errEventTypeNameInvalid, err)
	}
	if info.GetTaskId() <= 0 {
		t.Errorf("登记必须返回自增 task_id，实际 %d", info.GetTaskId())
	}
	if len(db.transcodes) != 1 {
		t.Fatalf("应恰好提交 1 行，实际 %d", len(db.transcodes))
	}
	var committed *model.LiveTranscodeTask
	for _, row := range db.transcodes {
		committed = row
	}
	if info.GetTaskId() != committed.TaskId {
		t.Errorf("返回值与提交的行不是同一行：reply=%d row=%d", info.GetTaskId(), committed.TaskId)
	}
	wantEvents(t, db, []string{model.EventTypeTranscodeStateChanged}, "登记事件")
	if got := eventAt(t, db, 0).AggregateId; got != strconv.FormatInt(committed.TaskId, 10) {
		t.Errorf("缺陷 #2（事件侧）：aggregate_id 必须是提交后的主键，实际 %q", got)
	}
}

// firstRecordID 取库里唯一一行录制任务的主键（用例已断言行数）。
func firstRecordID(db *store) int64 {
	for id := range db.records {
		return id
	}
	return 0
}
