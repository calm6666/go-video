package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListStreamKeysLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListStreamKeysLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListStreamKeysLogic {
	return &ListStreamKeysLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询密钥列表
//
// 越权收口：admin=false 时把过滤条件强制收敛为 anchor_mid = operator_mid，
// 调用方即使传了别人的 anchor_mid/room_id 也看不到他人密钥（不是「返回后再过滤」）。
func (l *ListStreamKeysLogic) ListStreamKeys(in *rpc.ListStreamKeysReq) (*rpc.ListStreamKeysReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	if in.RoomId < 0 || in.AnchorMid < 0 {
		return nil, model.ErrInvalidRoomId
	}

	filter := model.StreamKeyFilter{RoomID: in.RoomId, AnchorMid: in.AnchorMid}
	if !in.Admin {
		filter.RoomID = 0
		filter.AnchorMid = in.OperatorMid
	}
	if in.State != rpc.StreamKeyState_STREAM_KEY_STATE_UNSPECIFIED {
		state := keyStateFromRPC(in.State)
		if state == 0 {
			return nil, model.ErrStreamKeyNotUsable
		}
		filter.State = state
	}

	ps := clampPageSize(in.Ps, l.svcCtx.Config.LiveIngest.MaxListPageSize)
	pn := clampPageNo(in.Pn)
	filter.Limit = ps
	filter.Offset = pageOffset(pn, ps)

	rows, err := repo.StreamKey.ListByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	total, err := repo.StreamKey.CountByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	return &rpc.ListStreamKeysReply{
		Keys:  keyInfos(rows),
		Total: toInt32Total(total),
		Pn:    pn,
		Ps:    ps,
	}, nil
}
