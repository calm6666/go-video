package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetStreamStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetStreamStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetStreamStateLogic {
	return &GetStreamStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单流当前状态（按 stream_id 或房间的活跃流）
//
// 「查不到」是 found=false 而不是错误：live-room 在开播前置检查里调用它，
// 把「这间房现在没活流」当异常抛出会让调用方无法区分「没有」与「查失败」。
func (l *GetStreamStateLogic) GetStreamState(in *rpc.GetStreamStateReq) (*rpc.GetStreamStateReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}

	var (
		s   *model.Stream
		err error
	)
	switch {
	case in.StreamId != "":
		var streamID string
		if streamID, err = checkStreamID(in.StreamId); err != nil {
			return nil, err
		}
		s, err = repo.Stream.FindOne(l.ctx, streamID)
	case in.RoomId > 0:
		s, err = repo.Stream.FindActiveByRoom(l.ctx, in.RoomId)
	default:
		return nil, model.ErrInvalidStreamId
	}
	if err != nil {
		return nil, err
	}
	if s == nil {
		return &rpc.GetStreamStateReply{Found: false}, nil
	}
	return &rpc.GetStreamStateReply{Stream: streamInfo(s), Found: true}, nil
}
