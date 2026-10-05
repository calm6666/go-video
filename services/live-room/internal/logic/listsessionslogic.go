package logic

import (
	"context"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListSessionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListSessionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSessionsLogic {
	return &ListSessionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// cursor 分页拉取历史场次
//
// 只用 session_id 游标，不用 offset：新场次不断插入，offset 翻页必然重复/漏项。
// 游标解析失败直接返回 ErrCursorInvalid，绝不退化成「当作第一页」——
// 那会让客户端以为自己翻到了开头而重复渲染整页。
func (l *ListSessionsLogic) ListSessions(in *rpc.ListSessionsReq) (*rpc.ListSessionsReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if in.GetMid() < 0 {
		return nil, model.ErrInvalidMid
	}
	state, err := sessionStateFilter(in.GetState())
	if err != nil {
		return nil, err
	}
	size, err := l.svcCtx.PageSize(in.GetPageSize())
	if err != nil {
		return nil, err
	}
	beforeID, err := decodeIDCursor(in.GetCursor())
	if err != nil {
		return nil, err
	}
	rows, err := l.svcCtx.Sessions.List(l.ctx, model.SessionListModel{
		RoomID:          in.GetRoomId(),
		Mid:             in.GetMid(),
		State:           state,
		BeforeSessionID: beforeID,
		Limit:           int32(size),
	})
	if err != nil {
		return nil, err
	}
	return &rpc.ListSessionsReply{
		Sessions:   sessionInfoList(rows),
		NextCursor: nextCursor(rows, size),
	}, nil
}

// sessionStateFilter 校验场次状态过滤：UNSPECIFIED 不过滤，未知取值拒绝。
func sessionStateFilter(s rpc.SessionState) (int32, error) {
	v := int32(s)
	if v == model.SessionStateUnspecified {
		return 0, nil
	}
	if !model.ValidSessionState(v) {
		return 0, model.ErrInvalidSessionTransition
	}
	return v, nil
}
