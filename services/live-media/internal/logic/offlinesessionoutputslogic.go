package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// OfflineSessionOutputsInput 是「按场次整场下线档位」的入参。
//
// 与 rpc.OfflineStreamOutputReq 的区别是它没有档位维度：驱动它的是 live.state.v1
// （生产者只给 room_id + session_id，见 live-ingest internal/logic/streamstate.go 的
// stateEventPayload），一次断流要摘掉的是这场直播开出来的**所有**档位。
// 因此它不是 RPC 入参，只有 internal/consumer 一个调用方；运营手工下线仍走
// OfflineStreamOutput（能精确到档位，也能带 operator 语义的 request_id）。
type OfflineSessionOutputsInput struct {
	// RoomID 事件里的房间，必须为正：model 侧再按它做一次归属校验。
	RoomID int64
	// SessionID 事件里的场次，必须为正。0 或负数会被拒绝：
	// 拿它去匹配 live_session_id=0 的档位行等于「按房间下线所有场次」，
	// 一条迟到的旧断流事件就能摘掉刚开播场次的分发。
	SessionID int64
	// Reason 下线原因（model.Reason* 之一），必填且不能是 ReasonUnspecified：
	// 事件驱动的下线没有 operator 列，归因全靠 reason + event_id + trace_id。
	Reason int32
	// EventID 驱动本次下线的事件 ID，写进每个档位事件做反向追溯。
	EventID string
	// TraceID 关联链路，超长按列宽裁剪（与 OfflineStreamOutput 同口径）。
	TraceID string
}

// OfflineSessionOutputsResult 一次整场下线的结论。
type OfflineSessionOutputsResult struct {
	// Affected 实际被本次调用置为下线的档位数（并发已下线的行不计）。
	Affected int32
	// OutputIDs 实际下线的档位主键，按 (bitrate_level, protocol) 升序。
	OutputIDs []int64
	// Scanned 本次扫到的在线档位数；与 Affected 不等说明有行在并发中被别的入口下线了。
	Scanned int32
}

// Noop 表示这场次已经没有可下线的在线档位：重复事件、迟到事件或从未开过档位都会落在这里。
// 它是成功结论而不是错误（不提交位点就会让分区永久卡住）。
func (r *OfflineSessionOutputsResult) Noop() bool { return r == nil || r.Affected == 0 }

// OfflineSessionOutputsLogic 把一场直播的在线档位整场置下线，并为每个档位登记
// livemedia.stream.output.offline 事件。
type OfflineSessionOutputsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewOfflineSessionOutputsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OfflineSessionOutputsLogic {
	return &OfflineSessionOutputsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 断流事件驱动的整场档位下线（live.state.v1 的唯一写入路径）
//
// 幂等性靠行本身，而不是靠事件表：live_stream_output 没有 version 列，state 就是 CAS 条件
// （MarkOfflineTx 的 WHERE 带 state=在线），所以同一事件重投第二次扫不到行、Affected=0。
// 这比「按 event_id 去重」更强：任何入口（另一次断流、运营手工下线、到期清扫）先把档位摘掉，
// 本方法都不会重复下线、也不会为同一档位补第二条事件。
//
// 乱序安全性同理：扫描条件带 live_session_id，因此「上一场的 Stopped 迟到」只会命中上一场的
// 档位行，碰不到刚开播场次的分发。场次维度是这条链路的唯一护栏，所以 SessionID<=0 直接拒绝。
//
// 事务边界：所有档位的「条件下线 + Outbox 事件」在**同一个事务**里提交（AGENTS.md §5）。
// 刻意不逐档位各开一个事务：一半档位下线、另一半失败会留下观众侧可播但源已断的中间态，
// 比整场不动更难排查，也让 live_media_outbox 里的结论与主表不一致。
//
// 失败关闭：扫到超过 model.MaxSessionOutputs 的在线档位直接报错（数据异常），
// 由消费者按退避重试封顶处理，绝不挑一部分下线。
func (l *OfflineSessionOutputsLogic) OfflineSessionOutputs(
	in OfflineSessionOutputsInput) (*OfflineSessionOutputsResult, error) {
	if err := checkRoomID(in.RoomID); err != nil {
		return nil, err
	}
	if in.SessionID <= 0 {
		return nil, fmt.Errorf("live-media: offline by session needs live_session_id>0 (got %d): %w",
			in.SessionID, model.ErrInvalidSessionID)
	}
	if err := checkFailureReason(in.Reason); err != nil {
		return nil, err
	}
	if in.Reason == model.ReasonUnspecified {
		return nil, fmt.Errorf("live-media: offline needs a concrete reason (SOURCE_LOST/TIMEOUT/MANUAL): %w",
			model.ErrInvalidTransition)
	}
	if err := checkEventID(in.EventID); err != nil {
		return nil, err
	}
	eventID := strings.TrimSpace(in.EventID)
	traceID := sanitizeTraceID(in.TraceID)

	rows, err := l.svcCtx.StreamOutputs.ListOnlineBySession(l.ctx, in.RoomID, in.SessionID)
	if err != nil {
		return nil, err
	}
	res := &OfflineSessionOutputsResult{Scanned: int32(len(rows))}
	if len(rows) == 0 {
		return res, nil
	}
	// request_id 只进事件 payload（不落 VARCHAR 列），前缀 evt: 表明这次下线的驱动来源是事件
	// 而不是某个调用方给的幂等键，运营据此区分「自动下线」与「人工下线」。
	requestID := "evt:" + eventID

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		for _, row := range rows {
			aff, updErr := l.svcCtx.StreamOutputs.MarkOfflineTx(ctx, sess, row.OutputId, in.Reason, traceID)
			if updErr != nil {
				return updErr
			}
			if aff == 0 {
				// 并发入口（运营手工下线 / 到期清扫）已抢先把这一档摘下：
				// 与 OfflineStreamOutput 的抢跑分支同口径，不报错、不补第二条事件。
				l.Infof("livemedia/OfflineSessionOutputs: output_id=%d offline lost race, skipped", row.OutputId)
				continue
			}
			// payload 不带 offline_at：该列由 MarkOfflineTx 内部取时钟，行本身才是事实源
			// （与 OfflineStreamOutput 的注释同一条理由）。
			if evErr := appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeStreamOutputOffline,
				model.AggregateStreamOutput, row.OutputId, row.RoomId, map[string]any{
					"output_id":         row.OutputId,
					"room_id":           row.RoomId,
					"live_session_id":   row.LiveSession,
					"bitrate_level":     row.BitrateLevel,
					"protocol":          row.Protocol,
					"task_id":           row.TaskId,
					"prev_state":        row.State,
					"state":             model.StreamOutputStateOffline,
					"offline_reason":    in.Reason,
					"request_id":        requestID,
					"source_event_id":   eventID,
					"source_event_type": sourceEventTypeStreamState,
				}, traceID); evErr != nil {
				return evErr
			}
			res.Affected++
			res.OutputIDs = append(res.OutputIDs, row.OutputId)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res.Affected > 0 {
		// 与 OfflineStreamOutput 同一个失效口径：房间档位列表键代际自增。
		bumpOutputGen(l.ctx, l.svcCtx, in.RoomID)
	}
	return res, nil
}

// sourceEventTypeStreamState 标注驱动这次下线的事件类型（写进 payload 便于下游与排障区分
// 「因断流下线」与「因到期/人工下线」）。它是字符串常量而不是 model.EventType*：
// 那个常量表属于本服务的**出站**契约，入站类型不在其中。
const sourceEventTypeStreamState = "live.state"
