package logic

import (
	"context"
	"fmt"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MultiStatsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMultiStatsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MultiStatsLogic {
	return &MultiStatsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// MultiStats 跨业务批量查询计数。
// 入参按业务分组，每业务下含多对 (origin_id, message_id)。
func (l *MultiStatsLogic) MultiStats(in *rpc.MultiStatsReq) (*rpc.MultiStatsReply, error) {
	if in.Business == nil {
		return &rpc.MultiStatsReply{Business: map[string]*rpc.MultiStatsReply_Records{}}, nil
	}
	out := make(map[string]*rpc.MultiStatsReply_Records, len(in.Business))
	for name, biz := range in.Business {
		if biz == nil || len(biz.Records) == 0 {
			out[name] = &rpc.MultiStatsReply_Records{Records: map[int64]*rpc.StatState{}}
			continue
		}
		if len(biz.Records) > 100 {
			return nil, model.ErrTooManyMessageIDs
		}
		records := make(map[int64]*rpc.StatState, len(biz.Records))
		for _, r := range biz.Records {
			if r == nil {
				continue
			}
			stat, err := l.svcCtx.Repository.RawStat(l.ctx, name, r.OriginId, r.MessageId)
			if err != nil {
				l.Errorf("engagement/MultiStats: business=%s origin=%d msg=%d err=%v",
					name, r.OriginId, r.MessageId, err)
				return nil, err
			}
			item := &rpc.StatState{
				OriginId:      r.OriginId,
				MessageId:     r.MessageId,
				LikeNumber:    0,
				DislikeNumber: 0,
			}
			if stat != nil {
				item.LikeNumber = stat.LikeNumber
				item.DislikeNumber = stat.DislikeNumber
			}
			records[r.MessageId] = item
		}
		out[name] = &rpc.MultiStatsReply_Records{Records: records}
	}
	return &rpc.MultiStatsReply{Business: out}, nil
}

// _ 占位避免 fmt 未使用告警（保留以便未来日志扩展）。
var _ = fmt.Sprint
