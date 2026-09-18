package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MidByRealnameCardLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMidByRealnameCardLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MidByRealnameCardLogic {
	return &MidByRealnameCardLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 按证件号批量反查 mid。
// 参考 service.MidByRealnameCard：证件号哈希后批量查询 realname_info。
func (l *MidByRealnameCardLogic) MidByRealnameCard(in *rpc.MidByRealnameCardsReq) (*rpc.MidByRealnameCardReply, error) {
	codeToMid, err := l.svcCtx.Repository.MidByRealnameCard(l.ctx, in.CardCode, in.Country, in.CardType)
	if err != nil {
		l.Errorf("user-profile/MidByRealnameCard: err=%v", err)
		return nil, err
	}
	return &rpc.MidByRealnameCardReply{CodeToMid: codeToMid}, nil
}
