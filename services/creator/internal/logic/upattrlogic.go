package logic

import (
	"context"

	"go-video/services/creator/internal/svc"
	"go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpAttrLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpAttrLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpAttrLogic {
	return &UpAttrLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询 UP 主身份属性。
// 参考 service.UpAttr：缓存→DB→回填，from 区分身份来源。
func (l *UpAttrLogic) UpAttr(in *rpc.UpAttrReq) (*rpc.UpAttrReply, error) {
	if in.Mid <= 0 {
		return nil, errInvalidMid
	}
	if in.From < 0 || in.From > 3 {
		return nil, errInvalidFrom
	}
	isAuthor, err := l.svcCtx.Repository.UpAttr(l.ctx, in.Mid, in.From)
	if err != nil {
		l.Errorf("creator/UpAttr: mid=%d from=%d err=%v", in.Mid, in.From, err)
		return nil, err
	}
	return &rpc.UpAttrReply{IsAuthor: isAuthor}, nil
}
