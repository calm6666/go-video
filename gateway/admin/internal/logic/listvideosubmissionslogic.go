// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	videorpc "go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListVideoSubmissionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询稿件（按 mid/typeid/state 过滤）
func NewListVideoSubmissionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListVideoSubmissionsLogic {
	return &ListVideoSubmissionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询稿件：聚合 video 服务 RPC。
//
// 契约缺口（详见交付报告）：video.v1.ListReq 支持 mid/typeid 但没有 state，
// video.v1.ListByStateReq 支持 state 但没有 mid/typeid。网关不在本地对分页结果做二次过滤，
// 否则 total 与翻页语义都会失真，因此：
//   - 只按 mid/typeid 过滤 -> ListSubmissions；
//   - 只按 state 过滤     -> ListByState；
//   - state 与 mid/typeid 同时出现 -> 返回明确错误，等 video proto 扩字段后收敛。
func (l *ListVideoSubmissionsLogic) ListVideoSubmissions(req *types.ParamListVideoSubmissions) (resp *types.VideoSubmissionsResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	var reply *videorpc.SubmissionsReply
	switch {
	case req.State > 0 && (req.Mid > 0 || req.Typeid > 0):
		l.Errorf("gateway/admin/listVideoSubmissions: 不支持 state 与 mid/typeid 组合过滤 mid=%d typeid=%d state=%d",
			req.Mid, req.Typeid, req.State)
		return nil, errors.New("video service cannot filter state together with mid/typeid, query one of them")
	case req.State > 0:
		reply, err = l.svcCtx.Video.ListByState(l.ctx, &videorpc.ListByStateReq{
			State: videorpc.SubmissionState(req.State),
			Pn:    req.Pn,
			Ps:    req.Ps,
		})
	default:
		reply, err = l.svcCtx.Video.ListSubmissions(l.ctx, &videorpc.ListReq{
			Mid:    req.Mid,
			Typeid: req.Typeid,
			Pn:     req.Pn,
			Ps:     req.Ps,
		})
	}
	if err != nil {
		l.Errorf("gateway/admin/listVideoSubmissions: mid=%d typeid=%d state=%d pn=%d ps=%d err=%v",
			req.Mid, req.Typeid, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	submissions := make([]types.VideoSubmissionItem, 0, len(reply.GetSubmissions()))
	for _, s := range reply.GetSubmissions() {
		submissions = append(submissions, types.VideoSubmissionItem{
			Aid:    s.GetAid(),
			Mid:    s.GetMid(),
			Title:  s.GetTitle(),
			Desc:   s.GetDesc(),
			Cover:  s.GetCover(),
			Typeid: s.GetTypeid(),
			Tag:    s.GetTag(),
			State:  int32(s.GetState()),
			Ctime:  s.GetCtime(),
			Mtime:  s.GetMtime(),
		})
	}
	return &types.VideoSubmissionsResponse{
		Code:    0,
		Message: "ok",
		Data: types.VideoSubmissionsData{
			Total:       int64(reply.GetTotal()),
			Submissions: submissions,
		},
		TTL: 0,
	}, nil
}
