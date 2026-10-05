package logic

// content_event_logic_test.go 钉 content.published.v1 生产侧的真实口径：
//   - 事件行只在 repository.TransitionState 的事务里产生（第三条写入），
//     DeleteSubmission 复用同一条仓储路径，因此两个 RPC 的事件口径必须一致；
//   - 「哪些转换算可见性变更」由本包自己的判定表 wantEventAction 给出，来源是
//     docs/api-and-events.md §5 与两个消费方的实现（search-indexer 的 ContentPublishedPayload
//     与 inbox 的模板分支），**不调用被测实现**：repository 把某条边判错时用例必须红，
//     而不是跟着实现一起改期望；
//   - payload 用镜像结构体解码，字段名照抄消费方，producer 侧改名或换 JSON tag 立刻在这里红；
//     解码到 eventenvelope.Envelope 会顺带跑 Validate，所以同一条断言也证明
//     「库里这一行能被消费者解开且不违反信封契约」；
//   - 判别性靠三组对照撑住：可见性转换 vs 非可见性转换（都写了状态与审计，只有前者多一行事件）、
//     公开过 vs 没公开过（删除只有前者产 delete 事件）、publish vs 下架类（publish_at 只在前者出现）。
//
// 本仓 fake 不回滚（fakes_test.go 纪律 5），失败用例一律断言「库里实际还剩什么」。

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/common/eventenvelope"
	"go-video/services/video/internal/repository"
	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

// wantEventAction 是本包独立维护的可见性判定表（期望值，不是实现的别名）。
// 返回 "" 表示这次转换不改变「谁能看到这条稿件」，不该有事件行。
func wantEventAction(fromState, toState int32) string {
	switch toState {
	case model.StatePublished:
		return "publish"
	case model.StateOffline:
		return "offline"
	case model.StateExpired:
		return "expired"
	case model.StateDeleted:
		// 只有公开过的稿件删除才需要清索引；草稿、上传中、被驳回的稿件从未进过搜索索引。
		switch fromState {
		case model.StatePublished, model.StateOffline, model.StateExpired:
			return "delete"
		}
	}
	return ""
}

// eventPayload 是 content.published.v1 的 payload 镜像结构（字段与 JSON tag 照抄消费方）。
type eventPayload struct {
	Action      string   `json:"action"`
	ContentID   int64    `json:"content_id"`
	ContentType int32    `json:"content_type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	CoverURL    string   `json:"cover_url"`
	AuthorMid   int64    `json:"author_mid"`
	Typeid      int32    `json:"typeid"`
	Tags        []string `json:"tags"`
	PublishAt   int64    `json:"publish_at"`
	Ctime       int64    `json:"ctime"`
	DocRevision int64    `json:"doc_revision"`
	Reason      string   `json:"reason"`
}

// decodeEvent 把 outbox 行的 payload 列还原成「信封 + payload + 原始键集合」三层。
// 第二次解码走 Envelope.UnmarshalJSON，里面自带 Validate。
func decodeEvent(t *testing.T, row *model.VideoOutbox) (*eventenvelope.Envelope, eventPayload, map[string]any) {
	t.Helper()
	var env eventenvelope.Envelope
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		t.Fatalf("payload 列不是合法事件信封（消费者会整条判死）：%v\n%s", err, row.Payload)
	}
	var p eventPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("payload 解不进消费方的镜像结构：%v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(row.Payload), &raw); err != nil {
		t.Fatalf("payload 列不是 JSON 对象：%v", err)
	}
	return &env, p, raw
}

// payloadKeys 返回信封里 payload 对象的键集合（升序），用于把「字段清单」钉成集合等式。
func payloadKeys(t *testing.T, raw map[string]any) []string {
	t.Helper()
	body, ok := raw["payload"].(map[string]any)
	if !ok {
		t.Fatalf("信封的 payload 不是对象：%#v", raw["payload"])
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// publishSeed 布一行「即将转为可见」的稿件（SCHEDULED 是到点发布的合法前态）。
func publishSeed(st *store, aid, mid int64) { st.seedDraft(aid, mid, model.StateScheduled) }

// TestPublishTransitionWritesExactlyOnePendingEventRow 钉成功路径落在 video_outbox 的每一列，
// 并钉「列 ↔ 信封」不许分裂：六个业务列全部取自同一个 env，
// 否则消费者按其中一个去重、把另一个当新事件重复生效。
func TestPublishTransitionWritesExactlyOnePendingEventRow(t *testing.T) {
	st := newStore()
	publishSeed(st, 101, 7)
	before := time.Now().Unix()

	_, err := transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED, Operator: "cron:publish", Reason: "到点发布",
	})
	wantNoErr(t, "定时发布", err)
	after := time.Now().Unix()

	row := st.onlyOutboxRow(t, 101)
	wantEq(t, "事件行", "id 由自增分配", row.ID, int64(1))
	wantEq(t, "事件行", "event_type", row.EventType, model.EventContentPublished)
	wantEq(t, "事件行", "schema_version", row.SchemaVersion, int32(model.EventSchemaVersion))
	wantEq(t, "事件行", "aggregate_type", row.AggregateType, model.AggregateTypeSubmission)
	wantEq(t, "事件行", "aggregate_id 是 aid 的十进制串", row.AggregateID, "101")
	wantEq(t, "事件行", "state 必须是待发布", row.State, model.OutboxStatePending)
	wantEq(t, "事件行", "retry_count", row.RetryCount, int32(0))
	wantEq(t, "事件行", "next_retry_at（0 表示可立即投递）", row.NextRetryAt, int64(0))
	wantEq(t, "事件行", "last_error", row.LastError, "")
	if row.EventID == "" {
		t.Error("event_id 列为空，消费者无法按它幂等去重")
	}
	if row.OccurredAt < before || row.OccurredAt > after {
		t.Errorf("occurred_at = %d, want 落在 [%d, %d]", row.OccurredAt, before, after)
	}
	if row.Ctime == 0 || row.Mtime != row.Ctime {
		t.Errorf("ctime/mtime = %d/%d，生产 INSERT 的默认值口径没落到列上", row.Ctime, row.Mtime)
	}

	env, p, raw := decodeEvent(t, row)
	wantEq(t, "列与信封", "event_id", env.EventID, row.EventID)
	wantEq(t, "列与信封", "aggregate_id", env.AggregateID, row.AggregateID)
	wantEq(t, "列与信封", "aggregate_type", env.AggregateType, row.AggregateType)
	wantEq(t, "列与信封", "event_type", env.EventType, row.EventType)
	wantEq(t, "列与信封", "schema_version", int64(env.SchemaVersion), int64(row.SchemaVersion))
	wantEq(t, "信封", "producer", env.Producer, model.Producer)
	if env.TraceID != "" {
		t.Errorf("trace_id = %q，用例没注入链路上下文，不该凭空造一个", env.TraceID)
	}
	if _, ok := raw["trace_id"]; ok {
		t.Errorf("信封 JSON 里出现了空的 trace_id 字段：%v", raw)
	}
	// topic 是消费方的订阅锚点，这里按契约文档的字面量钉一次（不是再算一遍派生公式）。
	wantEq(t, "topic", "event_type+schema_version 派生的 topic",
		eventenvelope.Topic(env.EventType, env.SchemaVersion), "content.published.v1")
	wantEq(t, "payload", "action", p.Action, "publish")

	// 信封自己的 occurred_at 与列的 occurred_at 同源（信封是契约真源，列只是检索索引）。
	occurred, err := time.Parse(time.RFC3339, env.OccurredAt)
	wantNoErr(t, "occurred_at 必须是 RFC3339", err)
	wantEq(t, "occurred_at", "列值等于信封值", row.OccurredAt, occurred.Unix())
}

// TestEventPayloadIsExactlyTheDeclaredFieldSet 把 payload 的键集合钉成等式：
// 多一个字段（替别的 service 代答媒资时长、作者昵称、热度或版权窗口）与少一个字段一样红，
// 于是「改 payload」必然伴随一次显式的契约评审（AGENTS.md §5/§10）。
func TestEventPayloadIsExactlyTheDeclaredFieldSet(t *testing.T) {
	// 顺序是 payloadKeys 的字典序（description 排在 doc_revision 之前）。
	base := []string{
		"action", "author_mid", "content_id", "content_type", "cover_url", "ctime",
		"description", "doc_revision", "publish_at", "reason", "tags", "title", "typeid",
	}
	offline := []string{
		"action", "author_mid", "content_id", "content_type", "cover_url", "ctime",
		"description", "doc_revision", "reason", "tags", "title", "typeid",
	}

	t.Run("publish 带 publish_at 与 reason", func(t *testing.T) {
		st := newStore()
		publishSeed(st, 101, 7)
		_, err := transitionCall(st, &rpc.TransitionReq{
			Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED, Operator: "cron", Reason: "到点发布",
		})
		wantNoErr(t, "发布", err)
		_, _, raw := decodeEvent(t, st.onlyOutboxRow(t, 101))
		if got := payloadKeys(t, raw); !slices.Equal(got, base) {
			t.Errorf("publish 事件的 payload 键集合 = %v, want %v", got, base)
		}
	})

	t.Run("下架没有 publish_at", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StatePublished)
		_, err := transitionCall(st, &rpc.TransitionReq{
			Aid: 101, Target: rpc.SubmissionState_STATE_OFFLINE, Operator: "admin", Reason: "版权投诉",
		})
		wantNoErr(t, "下架", err)
		_, _, raw := decodeEvent(t, st.onlyOutboxRow(t, 101))
		if got := payloadKeys(t, raw); !slices.Equal(got, offline) {
			t.Errorf("offline 事件的 payload 键集合 = %v, want %v", got, offline)
		}
	})

	// 判别性对照：reason 为空串时 publish_at 之外再多掉一个键，
	// 证明「omitempty 生效」而不是「恰好总有值」。
	t.Run("空 reason 不占键位", func(t *testing.T) {
		st := newStore()
		publishSeed(st, 101, 7)
		_, err := transitionCall(st, &rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED})
		wantNoErr(t, "空身份发布", err)
		_, p, raw := decodeEvent(t, st.onlyOutboxRow(t, 101))
		wantEq(t, "键数", "payload 键数（publish 去掉 reason）", len(payloadKeys(t, raw)), len(base)-1)
		wantEq(t, "空 reason", "reason 解出来的零值", p.Reason, "")
	})
}

// TestEventCarriesFactsFromTheTransactionReadNotTheFirstRead 钉 payload 的数据来源：
// logic 的首读与事务内的二次校验之间，别的入口改了标题，
// 事件必须带**事务内**读到的值，否则「事件里的标题」与「库里的标题」会分裂。
// 判别性：同一改动挪到首读之前（直接 seed 新标题）拿到的也是新标题，
// 因此这里必须用 hook 在两次读之间改，才能区分两个来源。
func TestEventCarriesFactsFromTheTransactionReadNotTheFirstRead(t *testing.T) {
	st := newStore()
	publishSeed(st, 101, 7)
	st.before("db.TransactCtx", func() { st.setTitle(101, "改在事务里") })

	_, err := transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED, Operator: "cron", Reason: "到点发布",
	})
	wantNoErr(t, "发布", err)

	row := st.onlyOutboxRow(t, 101)
	env, p, _ := decodeEvent(t, row)
	wantEq(t, "payload 来源", "title 取事务内的读", p.Title, "改在事务里")
	wantEq(t, "payload", "description", p.Description, "d-101")
	wantEq(t, "payload", "cover_url", p.CoverURL, "c-101")
	wantEq(t, "payload", "content_id", p.ContentID, int64(101))
	wantEq(t, "payload", "content_id 与 aggregate_id 一致", strconv.FormatInt(p.ContentID, 10), env.AggregateID)
	wantEq(t, "payload", "content_type 恒为 UGC", p.ContentType, int32(model.ContentTypeUGC))
	wantEq(t, "payload", "author_mid", p.AuthorMid, int64(7))
	wantEq(t, "payload", "typeid", p.Typeid, int32(11))
	wantEq(t, "payload", "tags", strings.Join(p.Tags, "|"), "g-101")
	wantEq(t, "payload", "ctime 取稿件创建时间", p.Ctime, staleTime)
	wantEq(t, "payload", "reason", p.Reason, "到点发布")
	st.s.checkHooks(t)
}

// TestDocRevisionSharesOneClockWithAuditRow 钉「版本号可反查审计行」：
// doc_revision 与同事务写入的 video_audit_log.ctime 同源（秒 × 1000），
// publish_at 用同一个数字，消费方的 last-write-wins 才有可比性。
func TestDocRevisionSharesOneClockWithAuditRow(t *testing.T) {
	st := newStore()
	publishSeed(st, 101, 7)

	_, err := transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED, Operator: "cron", Reason: "首发",
	})
	wantNoErr(t, "发布", err)
	audit := st.auditRows(101)[0]
	_, p, _ := decodeEvent(t, st.onlyOutboxRow(t, 101))

	wantEq(t, "同源", "doc_revision 等于审计 ctime × 1000", p.DocRevision, audit.Ctime*1000)
	wantEq(t, "同源", "publish_at 等于审计 ctime", p.PublishAt, audit.Ctime)
	if p.DocRevision <= 0 {
		t.Errorf("doc_revision = %d，索引侧的 Validate 要求它 > 0，这条事件会被判死", p.DocRevision)
	}

	// 同一稿件第二次可见性变更（PUBLISHED→OFFLINE）必须留下第二条事件，
	// 且 event_id 不同（消费者按 event_id 去重，两个不同事实不许共用一个 ID）。
	_, err = transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_OFFLINE, Operator: "admin", Reason: "版权投诉",
	})
	wantNoErr(t, "下架", err)
	rows := st.outboxRows(101)
	if len(rows) != 2 {
		t.Fatalf("事件行数 = %d, want 2", len(rows))
	}
	if rows[0].EventID == rows[1].EventID {
		t.Errorf("两条事件共用 event_id=%s，第二条会被消费者当重复丢弃", rows[0].EventID)
	}
	_, p2, _ := decodeEvent(t, rows[1])
	wantEq(t, "第二条", "action", p2.Action, "offline")
	wantEq(t, "第二条", "doc_revision 对齐它自己的审计行",
		p2.DocRevision, st.auditRows(101)[1].Ctime*1000)
	if p2.PublishAt != 0 {
		t.Errorf("下架事件的 publish_at = %d，不该声明发布时间", p2.PublishAt)
	}
	// 同一秒内的两次变更可以拿到相同 doc_revision：消费方的比较是 >=，
	// 覆盖方向仍是「后写的生效」，这条已在 README「已知缺口」登记。
}

// TestActionMappingCoversEveryLegalEdge 遍历状态机全部合法出边：
// 事件行的有无必须逐条等于本包判定表，且 payload 的 action 与之一致。
// 于是「矩阵加一条边」时，改状态机的人必须同时回答「这条边要不要通知索引」。
func TestActionMappingCoversEveryLegalEdge(t *testing.T) {
	for from, targets := range legalTransitions {
		for _, to := range targets {
			from, to := from, to
			want := wantEventAction(from, to)
			t.Run(fmt.Sprintf("%d->%d", from, to), func(t *testing.T) {
				st := newStore()
				st.seedDraft(101, 7, from)

				_, err := transitionCall(st, &rpc.TransitionReq{
					Aid: 101, Target: stateToRPC(to), Operator: "matrix", Reason: "遍历",
				})
				wantNoErr(t, "矩阵内转换", err)
				// 审计与事务在每种转换上都必须写满；事件行的有无由下面的断言负责。
				wantEq(t, "矩阵内转换", "审计行数", st.auditCount(), 1)
				wantEq(t, "矩阵内转换", "事务次数", st.txCount(), 1)

				rows := st.outboxRows(101)
				if want == "" {
					if len(rows) != 0 {
						t.Fatalf("%d->%d 不改变可见性却写了 %d 行事件", from, to, len(rows))
					}
					wantCount(t, "矩阵内转换", st.log(), "video_outbox.", 0)
					return
				}
				if len(rows) != 1 {
					t.Fatalf("%d->%d 是可见性转换，事件行数 = %d, want 1", from, to, len(rows))
				}
				_, p, _ := decodeEvent(t, rows[0])
				wantEq(t, "payload", "action", p.Action, want)
			})
		}
	}
}

// TestDeleteEventOnlyWhenSubmissionWasPublic 钉删除侧的分叉：
// 同一动作（→DELETED）按「删除前是否公开过」决定要不要清索引，
// 而审计行在两种情况下都必须写满（否则 AGENTS.md §8 的证据链就断了）。
func TestDeleteEventOnlyWhenSubmissionWasPublic(t *testing.T) {
	neverPublic := []int32{model.StateDraft, model.StateUploading, model.StateUploaded, model.StateRejected}
	for _, from := range neverPublic {
		st := newStore()
		st.seedDraft(101, 7, from)

		_, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
		wantNoErr(t, "删除未公开稿件", err)
		wantEq(t, "删除未公开稿件", "state", st.sub(101).State, model.StateDeleted)
		wantEq(t, "删除未公开稿件", "审计行数", st.auditCount(), 1)
		wantEq(t, "删除未公开稿件", "事件行数", st.outboxCount(), 0)
		wantCount(t, "删除未公开稿件", st.log(), "video_outbox.", 0)
	}

	for _, from := range []int32{model.StatePublished, model.StateOffline} {
		st := newStore()
		st.seedDraft(101, 7, from)

		_, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
		wantNoErr(t, "删除公开过的稿件", err)
		row := st.onlyOutboxRow(t, 101)
		_, p, _ := decodeEvent(t, row)
		wantEq(t, "删除公开过的稿件", "action", p.Action, "delete")
		// reason 由 logic 固定成 "user delete submission"（deletesubmissionlogic.go），
		// 事件必须带同一个串，消费方才拿得到与审计行一致的理由。
		wantEq(t, "删除公开过的稿件", "reason 与审计行同源",
			p.Reason, st.auditRows(101)[0].Reason)
		wantEq(t, "删除公开过的稿件", "state", st.sub(101).State, model.StateDeleted)
	}
}

// TestExpiredSubmissionCannotBeDeletedYet 钉判定表里那条「暂时走不到」的边：
// wasPubliclyKnown 含 EXPIRED 是为了将来放开 EXPIRED→DELETED 时不漏投清索引，
// 但今天状态机没有这条出边，所以删除被矩阵拒掉、一行事件都不许写。
// 若哪天放开该转换，本用例会因为「得到成功而不是 ErrInvalidStateTransition」而红，
// 逼着改状态机的人同时确认上面的判定表。
func TestExpiredSubmissionCannotBeDeletedYet(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateExpired)
	wantEq(t, "前提", "EXPIRED 的合法出边数", len(legalTransitions[model.StateExpired]), 0)

	got, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantErrIs(t, "过期稿件删除", err, model.ErrInvalidStateTransition)
	if got != nil {
		t.Errorf("拒绝时仍返回 reply %+v", got)
	}
	wantEq(t, "过期稿件删除", "state 不变", st.sub(101).State, model.StateExpired)
	wantEq(t, "过期稿件删除", "审计行数", st.auditCount(), 0)
	wantEq(t, "过期稿件删除", "事件行数", st.outboxCount(), 0)

	// 判定表这一格仍然必须是 delete：它是「将来放开就该清索引」的声明，不是永真断言。
	wantEq(t, "判定表", "EXPIRED→DELETED 的动作", wantEventAction(model.StateExpired, model.StateDeleted), "delete")
}

// TestTagsAreSplitAndNeverNull 钉 tags 的两条消费侧口径：
// 无标签稿件是空数组而不是 null（null 会让索引把一个缺失字段读成「不更新标签」），
// 逗号分隔串里的空白段一律丢掉（否则索引多出一个空标签）。
func TestTagsAreSplitAndNeverNull(t *testing.T) {
	cases := []struct {
		name string
		tag  string
		want []string
	}{
		{"无标签", "", []string{}},
		{"只有逗号", " , , ", []string{}},
		{"带空白的多标签", "a, b,,c", []string{"a", "b", "c"}},
		{"单个标签", " solo ", []string{"solo"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedSub(&model.VideoSubmission{
				Aid: 101, Mid: 7, Title: "t", Desc: "d", Cover: "c", Typeid: 11,
				Tag: tc.tag, State: model.StateScheduled, Ctime: 100, Mtime: 100,
			})

			_, err := transitionCall(st, &rpc.TransitionReq{
				Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED, Operator: "cron",
			})
			wantNoErr(t, "发布", err)

			_, p, raw := decodeEvent(t, st.onlyOutboxRow(t, 101))
			if !slices.Equal(p.Tags, tc.want) {
				t.Errorf("tags = %q, want %q", p.Tags, tc.want)
			}
			body := raw["payload"].(map[string]any)
			arr, ok := body["tags"].([]any)
			if !ok {
				t.Fatalf("tags 在 JSON 里不是数组（%T），消费方会把 null 当字段缺失", body["tags"])
			}
			if len(arr) != len(tc.want) {
				t.Errorf("JSON 数组长度 = %d, want %d", len(arr), len(tc.want))
			}
		})
	}
}

// TestOutboxInsertFailurePropagatesAndStops 钉第三条写入失败时的口径：
// 错误原样透传、应答为 nil，并且 logic 立刻收手（不失效缓存、不复读）。
// fake 不回滚（纪律 5），所以这里断言的是「库里实际还剩什么」：
// 状态与审计已写、事件为 0，生产上三者同事务、任一条失败整体回滚。
func TestOutboxInsertFailurePropagatesAndStops(t *testing.T) {
	st := newStore()
	publishSeed(st, 101, 7)
	st.fail("outbox.Insert")

	got, err := transitionCall(st, &rpc.TransitionReq{
		Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED, Operator: "cron", Reason: "到点发布",
	})
	wantErrIs(t, "事件行写入故障", err, errBoom)
	if got != nil {
		t.Errorf("事件写失败仍返回 reply %+v（丢事件不能算发布成功）", got)
	}
	wantSeq(t, "轨迹", st.log(), 0,
		"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101",
		"video_submission.UpdateState:101/11", "video_audit_log.Insert:101:10->11",
		"video_outbox.Insert:101")
	wantCount(t, "事件故障后不该再动缓存", st.log(), "cache.", 0)
	wantEq(t, "事件故障", "审计行数", st.auditCount(), 1)
	wantEq(t, "事件故障", "事件行数", st.outboxCount(), 0)
}

// TestVisibilityTransitionRefusesToRunWithoutOutboxModel 钉装配缺口不静默降级：
// NewWithDeps 传 nil 事件模型时，可见性转换必须在事务里报错（而不是「状态改了、事件没写」），
// 同一仓库上的非可见性转换则照常成功（nil 检查只该作用在真要写事件的路径上）。
func TestVisibilityTransitionRefusesToRunWithoutOutboxModel(t *testing.T) {
	t.Run("可见性转换必须报错并且一行都不写", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateScheduled)
		// 只把事件模型换成 nil，其余依赖仍是同一批替身。
		weak := &svc.ServiceContext{
			Repository: repository.NewWithDeps(st.cache, st.conn, st.subs, st.vers, st.audits, nil),
		}

		got, err := NewTransitionStateLogic(context.Background(), weak).
			TransitionState(&rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_PUBLISHED})
		if err == nil {
			t.Fatalf("缺 VideoOutboxModel 却推进成功：%+v（等于生产上「已发布但永远不进索引」）", got)
		}
		if !strings.Contains(err.Error(), "VideoOutboxModel") {
			t.Errorf("错误 = %v, want 点名缺哪个依赖", err)
		}
		if got != nil {
			t.Errorf("报错仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log(), 0,
			"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101")
		wantEq(t, "缺事件模型", "state 不变", st.sub(101).State, model.StateScheduled)
		wantEq(t, "缺事件模型", "审计行数", st.auditCount(), 0)
		wantEq(t, "缺事件模型", "事件行数", st.outboxCount(), 0)
		wantCount(t, "缺事件模型", st.log(), "cache.", 0)
	})

	t.Run("非可见性转换不需要事件模型", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		weak := &svc.ServiceContext{
			Repository: repository.NewWithDeps(st.cache, st.conn, st.subs, st.vers, st.audits, nil),
		}

		_, err := NewTransitionStateLogic(context.Background(), weak).
			TransitionState(&rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_UPLOADING})
		wantNoErr(t, "DRAFT→UPLOADING 不该被 nil 事件模型拦下", err)
		wantEq(t, "缺事件模型", "state", st.sub(101).State, model.StateUploading)
		wantEq(t, "缺事件模型", "审计行数", st.auditCount(), 1)
		wantEq(t, "缺事件模型", "事件行数", st.outboxCount(), 0)
	})
}

// TestNonEventPathsNeverTouchTheOutboxTable 钉「事件只由可见性转换产生」的另一半：
// 投稿、改正文、GetSubmission / ListSubmissions / ListByState 与 GetPlayableSource
// 一次都不许碰 video_outbox。
// 若将来有人把「写事件」挪到 logic 或读路径上（例如 GetSubmission 顺便补投），这里会红。
func TestNonEventPathsNeverTouchTheOutboxTable(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.seedDraft(102, 7, model.StatePublished)
	st.seedVersion(&model.VideoVersion{Aid: 102, Version: 1, AssetID: "a-102", State: model.StatePublished, Ctime: 100})

	_, err := NewCreateSubmissionLogic(context.Background(), st.svcCtx).
		CreateSubmission(&rpc.CreateSubmissionReq{Mid: 7, Title: "新稿", Typeid: 11})
	wantNoErr(t, "投稿", err)

	_, err = updateCall(st, &rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "改了标题", Typeid: 11})
	wantNoErr(t, "改正文", err)

	_, err = NewGetSubmissionLogic(context.Background(), st.svcCtx).
		GetSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantNoErr(t, "查详情", err)

	_, err = NewListSubmissionsLogic(context.Background(), st.svcCtx).
		ListSubmissions(&rpc.ListReq{Mid: 7, Pn: 1, Ps: 20})
	wantNoErr(t, "列表", err)

	_, err = NewListByStateLogic(context.Background(), st.svcCtx).
		ListByState(&rpc.ListByStateReq{State: rpc.SubmissionState_STATE_PUBLISHED, Pn: 1, Ps: 20})
	wantNoErr(t, "按状态列举", err)

	_, err = NewGetPlayableSourceLogic(context.Background(), st.svcCtx).
		GetPlayableSource(&rpc.PlayableSourceReq{Aid: 102, Mid: 7})
	wantNoErr(t, "取可播源", err)

	wantCount(t, "读与草稿路径", st.log(), "video_outbox.", 0)
	wantEq(t, "读与草稿路径", "事件行数", st.outboxCount(), 0)
	wantCount(t, "读与草稿路径", st.log(), "db.TransactCtx", 0)
}
