// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveingestrpc "go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveStreamCloseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 强制断流（处置动作：IDLE/PUBLISHING/INTERRUPTED → STOPPED，级联释放配额）
func NewLiveStreamCloseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamCloseLogic {
	return &LiveStreamCloseLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamClose 聚合 live-ingest CloseStream（运营强制断流）。
//
// 与 live-room 的 CloseRoom 同一先例：
//   - admin 固定 true，本路由在 AdminPermission 组里，能走到这里就说明权限已放行；把 admin 位
//     开放给请求体等于让表单自己声明「我是运营」；
//   - operator_mid 必须是显式声明的正整数，网关**不用会话 admin_id 覆盖**它
//     （admin_id 是 op_admin_user 主键、operator_mid 是用户 mid，不是同一编号空间），
//     两个主体一起写进网关日志；
//   - request_id 非空且原样透传（不改写、不 TrimSpace 回写），否则服务侧的幂等重放失效；
//   - 状态机能否从当前状态迁移到 STOPPED、级联释放哪些节点配额，全部由 live-ingest 判定。
//
// stop_reason 传 0 时由服务归一为 ADMIN，网关不代填——代填会把「谁定的原因」含糊掉。
// applied=false（本就终态）是幂等成功，不改写成错误。
func (l *LiveStreamCloseLogic) LiveStreamClose(req *types.ParamLiveStreamClose) (resp *types.LiveStreamCloseResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveStreamClose", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredText("stream_id", req.StreamId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("stop_reason", req.StopReason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.CloseStream(l.ctx, &liveingestrpc.CloseStreamReq{
		StreamId:    req.StreamId,
		StopReason:  liveingestrpc.StopReason(req.StopReason),
		Reason:      req.Reason,
		RequestId:   req.RequestId,
		OperatorMid: req.OperatorMid,
		Admin:       true, // 固定：运营侧断流，不接受表单声明
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamClose: stream_id=%s operator_mid=%d request_id=%s err=%v",
			req.StreamId, req.OperatorMid, req.RequestId, err)
		return nil, err
	}
	return &types.LiveStreamCloseResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamCloseData{
			State:                   int32(reply.GetState()),
			Seq:                     reply.GetSeq(),
			EventId:                 reply.GetEventId(),
			InterruptedTotalSeconds: reply.GetInterruptedTotalSeconds(),
			Replayed:                reply.GetReplayed(),
			Applied:                 reply.GetApplied(),
			Message:                 reply.GetMessage(),
		},
		TTL: 0,
	}, nil
}
