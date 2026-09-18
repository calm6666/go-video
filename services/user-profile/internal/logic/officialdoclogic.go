package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OfficialDocLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewOfficialDocLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OfficialDocLogic {
	return &OfficialDocLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询官方认证文档。
// 参考 service.OfficialDoc：查询 user_official_doc 并解析附加资料 JSON。
func (l *OfficialDocLogic) OfficialDoc(in *rpc.MidReq) (*rpc.OfficialDocInfoReply, error) {
	reply, err := l.svcCtx.Repository.OfficialDoc(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/OfficialDoc: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
