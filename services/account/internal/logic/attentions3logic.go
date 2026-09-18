package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Attentions3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAttentions3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Attentions3Logic {
	return &Attentions3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询关注列表（含特别关注）。
// 参考 service.Attentions：透传 social-graph 查询 mid 的关注列表。
// 返回 attentions 字段始终为非 nil 切片，便于客户端处理。
func (l *Attentions3Logic) Attentions3(in *rpc.MidReq) (*rpc.AttentionsReply, error) {
	reply, err := l.svcCtx.Repository.Attentions(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("account/Attentions3: repository.Attentions mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	if reply == nil {
		reply = &rpc.AttentionsReply{}
	}
	if reply.Attentions == nil {
		reply.Attentions = []int64{}
	}
	return reply, nil
}
