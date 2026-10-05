package logic

import (
	"context"

	"go-video/services/risk-control/internal/repository"
	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportActionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportActionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportActionLogic {
	return &ReportActionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 行为上报：写入 Redis 滑窗计数，供 CheckAction 的频率类规则评估。
//
// 边界（AGENTS.md §7 红线）：这里的计数只服务风控频率规则，
// 不做 SPM 广告分析、不参与推荐特征产出，也不得被当作埋点通道使用。
// 幂等：event_id 非空时同 event_id 只累加一次，重复上报返回 deduplicated=true。
func (l *ReportActionLogic) ReportAction(in *rpc.ReportActionReq) (*rpc.ReportActionReply, error) {
	action, err := actionFromProto(in.GetAction())
	if err != nil {
		return nil, err
	}
	ipHash, err := normalizeIPHashOrErr(in.GetIpHash())
	if err != nil {
		return nil, err
	}

	count := in.GetCount()
	if count <= 0 {
		count = 1 // 契约约定：<=0 视为 1，避免调用方漏传时整条上报被丢弃
	}

	out, err := l.svcCtx.Repository.Report(l.ctx, repository.ReportInput{
		Mid:        in.GetMid(),
		Action:     action,
		DeviceHash: deviceHashOf(in.GetDeviceId(), ""),
		IPHash:     ipHash,
		Count:      count,
		OccurredAt: in.GetOccurredAt(),
		EventID:    sanitizeShortString(in.GetEventId(), 64),
	})
	if err != nil {
		l.Errorf("risk-control/ReportAction: report failed mid=%d action=%d err=%v", in.GetMid(), action, err)
		return nil, err
	}
	return &rpc.ReportActionReply{
		Deduplicated:  out.Deduplicated,
		WindowSeconds: out.WindowSeconds,
		MidCount:      out.MidCount,
		DeviceCount:   out.DeviceCount,
		IpCount:       out.IPCount,
	}, nil
}
