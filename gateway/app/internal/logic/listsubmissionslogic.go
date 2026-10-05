// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	videorpc "go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListSubmissionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询稿件（按 mid 或 typeid 过滤）
func NewListSubmissionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSubmissionsLogic {
	return &ListSubmissionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询稿件：聚合 video ListSubmissions RPC。
func (l *ListSubmissionsLogic) ListSubmissions(req *types.ParamListSubmissions) (resp *types.VideoSubmissionsResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	reply, err := l.svcCtx.Video.ListSubmissions(l.ctx, &videorpc.ListReq{
		Mid:    req.Mid,
		Typeid: req.Typeid,
		Pn:     req.Pn,
		Ps:     req.Ps,
		Ip:     req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/listSubmissions: mid=%d typeid=%d err=%v", req.Mid, req.Typeid, err)
		return nil, err
	}
	return &types.VideoSubmissionsResponse{
		Code:    0,
		Message: "ok",
		Data: types.VideoSubmissionsData{
			Total:       reply.GetTotal(),
			Submissions: toVideoSubmissions(reply.GetSubmissions()),
		},
		TTL: 0,
	}, nil
}
