package logic

import (
	"context"

	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RecomputeUnreadLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRecomputeUnreadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RecomputeUnreadLogic {
	return &RecomputeUnreadLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RecomputeUnread 从 inbox_user_message 重算未读，覆盖快照并回填 Redis。
// 这是计数漂移的修复入口：Redis 清空、快照表被误写、或 cron 定期校准都可以调用，
// 幂等且可重复执行。
func (l *RecomputeUnreadLogic) RecomputeUnread(in *rpc.RecomputeUnreadReq) (*rpc.RecomputeUnreadReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	snapshot, err := l.svcCtx.Repository.RecomputeUnread(l.ctx, in.Mid)
	if err != nil {
		return nil, err
	}
	l.Infof("inbox/RecomputeUnread: mid=%d total=%d source=%s", in.Mid, snapshot.Total, snapshot.Source)
	return &rpc.RecomputeUnreadReply{
		Total:      snapshot.Total,
		ByCategory: toRPCUnread(snapshot.ByCategory),
	}, nil
}
