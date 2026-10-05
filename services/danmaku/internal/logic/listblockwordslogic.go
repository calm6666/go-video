package logic

import (
	"context"

	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListBlockWordsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListBlockWordsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListBlockWordsLogic {
	return &ListBlockWordsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListBlockWords 运营侧分页查询屏蔽词。
// 需要 operator_mid > 0（由 gateway/admin 注入），普通客户端不得调用本方法。
func (l *ListBlockWordsLogic) ListBlockWords(in *rpc.ListBlockWordsReq) (*rpc.ListBlockWordsReply, error) {
	if in.OperatorMid <= 0 {
		return nil, model.ErrOperatorRequired
	}
	scope := int32(0) // 0 表示不按作用域过滤
	switch in.Scope {
	case rpc.BlockWordScope_SCOPE_UNSPECIFIED:
	case rpc.BlockWordScope_SCOPE_GLOBAL, rpc.BlockWordScope_SCOPE_OID:
		scope = int32(in.Scope)
	default:
		return nil, model.ErrInvalidBlockWord
	}

	rows, total, err := l.svcCtx.Repository.ListBlockWords(l.ctx, scope, in.Oid, in.OnlyEnabled, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("danmaku/ListBlockWords: scope=%d oid=%d err=%v", scope, in.Oid, err)
		return nil, err
	}
	words := make([]*rpc.BlockWordInfo, 0, len(rows))
	for _, w := range rows {
		words = append(words, blockWordToRPC(w))
	}
	return &rpc.ListBlockWordsReply{Words: words, Total: total}, nil
}
