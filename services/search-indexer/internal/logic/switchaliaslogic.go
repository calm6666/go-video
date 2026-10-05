package logic

import (
	"context"

	"go-video/services/search-indexer/internal/svc"
	"go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SwitchAliasLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSwitchAliasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SwitchAliasLogic {
	return &SwitchAliasLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 把查询别名从旧索引切到新版本索引（expected_current 乐观校验，失败返回明确错误）。
//
// 该接口是「零停机重建」的最后一步，必须先跑重建任务、用 GetIndexHealth 校验目标索引，
// 再带 expected_current 调用；skip_health_check 仅用于紧急回滚（会跳过空索引保护）。
func (l *SwitchAliasLogic) SwitchAlias(in *rpc.SwitchAliasReq) (*rpc.SwitchAliasReply, error) {
	res, err := l.svcCtx.Repository.SwitchAlias(l.ctx, in.Alias, in.TargetIndex, in.ExpectedCurrent, in.SkipHealthCheck)
	if err != nil {
		l.Errorf("search-indexer/SwitchAlias: alias=%s target=%s expected_current=%s operator=%s err=%v",
			in.Alias, in.TargetIndex, in.ExpectedCurrent, in.Operator, err)
		return nil, err
	}
	l.Infof("search-indexer/SwitchAlias: alias=%s %s -> %s docs=%d noop=%v operator=%s",
		res.Alias, res.PreviousIndex, res.CurrentIndex, res.DocCount, res.Noop, in.Operator)

	return &rpc.SwitchAliasReply{
		Alias:         res.Alias,
		PreviousIndex: res.PreviousIndex,
		CurrentIndex:  res.CurrentIndex,
		DocCount:      res.DocCount,
		RecordState:   res.RecordState,
	}, nil
}
