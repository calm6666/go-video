package logic

import (
	"context"

	"go-video/services/creator/internal/svc"
	"go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetHighAllyUpsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetHighAllyUpsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetHighAllyUpsLogic {
	return &GetHighAllyUpsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询高能联盟 UP 主签约信息。
// 参考 service.GetHighAllyUps：批量打 DB，不走缓存。
func (l *GetHighAllyUpsLogic) GetHighAllyUps(in *rpc.HighAllyUpsReq) (*rpc.HighAllyUpsReply, error) {
	if len(in.Mids) == 0 {
		return &rpc.HighAllyUpsReply{Lists: map[int64]*rpc.SignUp{}}, nil
	}
	signs, err := l.svcCtx.Repository.GetHighAllyUps(l.ctx, in.Mids)
	if err != nil {
		l.Errorf("creator/GetHighAllyUps: count=%d err=%v", len(in.Mids), err)
		return nil, err
	}
	out := make(map[int64]*rpc.SignUp, len(signs))
	for mid, s := range signs {
		out[mid] = &rpc.SignUp{
			Mid:       s.Mid,
			State:     s.State,
			BeginDate: s.BeginDate,
			EndDate:   s.EndDate,
		}
	}
	return &rpc.HighAllyUpsReply{Lists: out}, nil
}
