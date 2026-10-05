package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListStreamsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListStreamsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListStreamsLogic {
	return &ListStreamsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询流列表（开播巡检、断流扫描）
//
// 越权收口同 ListStreamKeys：非 admin 一律把条件收敛到 anchor_mid = operator_mid。
// 排序按 last_heartbeat_at 升序，最可疑（最久没心跳）的在前，巡检第一页就能拿到要处理的流。
func (l *ListStreamsLogic) ListStreams(in *rpc.ListStreamsReq) (*rpc.ListStreamsReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	if len(in.RoomIds) > maxRoomIDsPerQuery {
		// 拒绝而不是截断：截断会让调用方误以为「其余房间没有活流」。
		return nil, model.ErrInvalidRoomId
	}
	roomIDs := make([]int64, 0, len(in.RoomIds))
	for _, id := range in.RoomIds {
		if id <= 0 {
			return nil, model.ErrInvalidRoomId
		}
		roomIDs = append(roomIDs, id)
	}
	if in.HeartbeatBefore < 0 {
		return nil, model.ErrInvalidStreamId
	}

	filter := model.StreamFilter{RoomIDs: roomIDs, NodeID: in.NodeId, MaxHeartbeat: in.HeartbeatBefore}
	if !in.Admin {
		filter.AnchorMid = in.OperatorMid
	}
	if in.State != rpc.StreamState_STREAM_STATE_UNSPECIFIED {
		state := streamStateFromRPC(in.State)
		if state == 0 {
			return nil, model.ErrInvalidStreamState
		}
		filter.State = state
	} // 未指定状态 => model.StreamFilter.State 为 0，按非终态集合过滤（不会全表扫）
	if in.Protocol != rpc.IngestProtocol_PROTOCOL_UNSPECIFIED {
		protocol := int32(in.Protocol)
		if _, ok := model.ProtocolMask(protocol); !ok {
			return nil, model.ErrInvalidProtocol
		}
		filter.Protocol = protocol
	}

	ps := clampPageSize(in.Ps, l.svcCtx.Config.LiveIngest.MaxListPageSize)
	pn := clampPageNo(in.Pn)
	filter.Limit = ps
	filter.Offset = pageOffset(pn, ps)

	rows, err := repo.Stream.ListByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	total, err := repo.Stream.CountByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	return &rpc.ListStreamsReply{
		Streams:    streamInfos(rows),
		Total:      toInt32Total(total),
		Pn:         pn,
		Ps:         ps,
		ServerTime: nowUnix(),
	}, nil
}
