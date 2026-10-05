package logic

import (
	"context"

	"go-video/services/search-indexer/internal/svc"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetIndexHealthLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetIndexHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetIndexHealthLogic {
	return &GetIndexHealthLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 返回各别名/索引的 doc 数与健康状态，以及重试/死信积压。
func (l *GetIndexHealthLogic) GetIndexHealth(in *rpc.GetIndexHealthReq) (*rpc.GetIndexHealthReply, error) {
	rows, err := l.svcCtx.Repository.Health(l.ctx, in.Alias)
	if err != nil {
		l.Errorf("search-indexer/GetIndexHealth: alias=%q err=%v", in.Alias, err)
		return nil, err
	}

	// 积压计数是观测值：读失败按 0 上报但必须留日志，不能让巡检接口整体失败。
	var retryPending, deadLetter int64
	if n, cerr := l.svcCtx.Repository.CountEventsByState(l.ctx, model.OffsetStateRetry); cerr != nil {
		l.Errorf("search-indexer/GetIndexHealth: 统计待重试事件失败，retry_pending 上报为 0 err=%v", cerr)
	} else {
		retryPending = n
	}
	if n, cerr := l.svcCtx.Repository.DeadLetterCount(l.ctx, model.DLQStateOpen); cerr != nil {
		l.Errorf("search-indexer/GetIndexHealth: 统计死信失败，dead_letter 上报为 0 err=%v", cerr)
	} else {
		deadLetter = n
	}

	aliases := make([]*rpc.AliasStatus, 0, len(rows))
	for _, h := range rows {
		aliases = append(aliases, aliasHealthToRPC(h))
	}
	return &rpc.GetIndexHealthReply{
		Aliases:      aliases,
		RetryPending: retryPending,
		DeadLetter:   deadLetter,
		OverallState: overallState(rows, retryPending, deadLetter),
	}, nil
}
