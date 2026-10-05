package logic

import (
	"context"

	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListUserBlocksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListUserBlocksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListUserBlocksLogic {
	return &ListUserBlocksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListUserBlocks 查询某用户的生效屏蔽项。
// 只允许本人查询（由 gateway 校验登录 mid 与 in.Mid 一致），本服务按 mid 维度返回。
func (l *ListUserBlocksLogic) ListUserBlocks(in *rpc.ListUserBlocksReq) (*rpc.ListUserBlocksReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	blockType := int32(0) // 0 表示不按类型过滤
	switch in.Type {
	case rpc.UserBlockType_USER_BLOCK_UNSPECIFIED:
	case rpc.UserBlockType_USER_BLOCK_MID, rpc.UserBlockType_USER_BLOCK_KEYWORD:
		blockType = int32(in.Type)
	default:
		return nil, model.ErrInvalidUserBlock
	}

	rows, total, err := l.svcCtx.Repository.ListUserBlocks(l.ctx, in.Mid, blockType, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("danmaku/ListUserBlocks: mid=%d type=%d err=%v", in.Mid, blockType, err)
		return nil, err
	}
	blocks := make([]*rpc.UserBlockInfo, 0, len(rows))
	for _, b := range rows {
		blocks = append(blocks, userBlockToRPC(b))
	}
	return &rpc.ListUserBlocksReply{Blocks: blocks, Total: total}, nil
}
