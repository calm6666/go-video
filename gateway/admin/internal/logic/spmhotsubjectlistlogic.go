// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	spmrpc "go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SpmHotSubjectListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 热点榜（回显实际使用的窗口与口径版本）
func NewSpmHotSubjectListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmHotSubjectListLogic {
	return &SpmHotSubjectListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmHotSubjectList 转发 spm ListHotSubjects（按指标值倒序取主体主键的只读榜单投影）。
//
// 榜单本身是服务的结论：谁进榜、并列怎么排、ps 上限 100、这个口径能不能出榜，网关一概不复算，
// 也不做「本地再排一次」——重排会让名次与 rank 字段不一致。
// 回包里的 window_start / metric_version 是**实际用了哪一版口径、哪个窗口**的回显，
// 必须以服务回值为准（传 0 时用的是最近闭合窗口，网关猜不出来），否则排障时对不上号。
// zone_id=0 是「不限分区」的合法哨兵，不改写；它是否只对 AID/CATALOG_ITEM 有意义也由服务判。
func (l *SpmHotSubjectListLogic) SpmHotSubjectList(req *types.ParamSpmHotSubjectList) (resp *types.SpmHotSubjectListResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := spmPositive("subject_type", req.SubjectType); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("metric_key", req.MetricKey); err != nil {
		return nil, err
	}
	if err := spmPositive("window_type", req.WindowType); err != nil {
		return nil, err
	}
	if err := spmNonNeg("metric_version", int64(req.MetricVersion)); err != nil {
		return nil, err
	}
	if err := spmNonNeg("window_start", req.WindowStart); err != nil {
		return nil, err
	}
	if err := spmNonNeg("zone_id", req.ZoneId); err != nil {
		return nil, err
	}
	if err := spmPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.ListHotSubjects(l.ctx, &spmrpc.ListHotSubjectsReq{
		SubjectType:   spmrpc.SubjectType(req.SubjectType),
		MetricKey:     req.MetricKey,
		MetricVersion: req.MetricVersion,
		WindowType:    spmrpc.WindowType(req.WindowType),
		WindowStart:   req.WindowStart,
		ZoneId:        req.ZoneId,
		Pn:            req.Pn,
		Ps:            req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmHotSubjectList: subject_type=%d metric_key=%s metric_version=%d window_type=%d window_start=%d zone_id=%d pn=%d ps=%d err=%v",
			req.SubjectType, req.MetricKey, req.MetricVersion, req.WindowType, req.WindowStart, req.ZoneId, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.SpmHotSubjectListResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmHotSubjectListData{
			Subjects:      spmHotSubjectsToAPI(reply.GetSubjects()),
			Total:         reply.GetTotal(),
			WindowStart:   reply.GetWindowStart(),
			MetricVersion: reply.GetMetricVersion(),
		},
		TTL: 0,
	}, nil
}
