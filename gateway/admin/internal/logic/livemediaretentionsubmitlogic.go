// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	livemediarpc "go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveMediaRetentionSubmitLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交回收任务（超期切片/回放产物/残留档位，先登记后执行；purge=true 才真删）
func NewLiveMediaRetentionSubmitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaRetentionSubmitLogic {
	return &LiveMediaRetentionSubmitLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaRetentionSubmit 聚合 live-media SubmitRetentionTask。
//
// 回收是「先登记意图 → Worker 执行 → 回报结果」的三步流程，禁止边查边删（无法审计，AGENTS.md §8）。
// 本路由只完成第一步：落一行 PENDING 的回收任务，删除动作发生在 Worker，结果由 ReportRetentionResult
// 回报（那条是 Worker→服务口，刻意不开后台路由）。因此网关**不**在这里承诺「已删除多少」，
// 只投影登记结果，真正的凭证是台账行的 scanned/deleted/skipped。
//
// purge=true 会真删对象存储引用，purge=false 只置标记 —— 两者共用 live:retention:submit 一个权限点，
// 理由与缺口在 admin.api 头部说明：契约里 submit 只有一个入口、purge 只是它的一个布尔参数，
// 拆成两点需要服务侧提供两个方法；作为补偿，reason 在这里是**必填**（契约注释本身就写「审计必填」），
// 且 operator 一律由会话渲染成 admin:<admin_id>，表单不得自报。
//
// target_kind 的 0 是 UNSPECIFIED（不知道回收什么，服务只能全表扫或不扫），直接拒；
// room_id=0 表示全局扫描、target_id=0 表示按 expire_before 批量，都是合法哨兵（负数先拒）；
// batch_limit 上限（500）由服务夹取，网关不设上界；哪些对象仍被引用、能不能删由 live-media 判定。
func (l *LiveMediaRetentionSubmitLogic) LiveMediaRetentionSubmit(req *types.ParamLiveMediaRetentionSubmit) (resp *types.LiveMediaRetentionSubmitResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	operator, err := liveMediaOperator(l.ctx, "liveMediaRetentionSubmit")
	if err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveMediaEnum("target_kind", req.TargetKind); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"room_id", req.RoomId},
		{"target_id", req.TargetId},
		{"expire_before", req.ExpireBefore},
	} {
		if err := liveNonNeg(f.name, f.v); err != nil {
			return nil, err
		}
	}
	if err := liveNonNeg32("batch_limit", req.BatchLimit); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.SubmitRetentionTask(l.ctx, &livemediarpc.SubmitRetentionTaskReq{
		TargetKind:   livemediarpc.RetentionTargetKind(req.TargetKind),
		RoomId:       req.RoomId,
		TargetId:     req.TargetId,
		ExpireBefore: req.ExpireBefore,
		Purge:        req.Purge,
		BatchLimit:   req.BatchLimit,
		Reason:       req.Reason,
		RequestId:    req.RequestId,
		Operator:     operator,
		TraceId:      req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaRetentionSubmit: target_kind=%d room_id=%d target_id=%d purge=%v request_id=%s err=%v",
			req.TargetKind, req.RoomId, req.TargetId, req.Purge, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaRetentionSubmitResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaRetentionTaskToAPI(info),
		TTL:     0,
	}, nil
}
