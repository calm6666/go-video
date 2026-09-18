package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetOfficialDocLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetOfficialDocLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetOfficialDocLogic {
	return &SetOfficialDocLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 提交官方认证文档。
// 参考 service.SetOfficialDoc：校验必填字段→状态置待审核→UPSERT→信用代码附加表。
func (l *SetOfficialDocLogic) SetOfficialDoc(in *rpc.OfficialDocReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetOfficialDoc(l.ctx, in); err != nil {
		l.Errorf("user-profile/SetOfficialDoc: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
