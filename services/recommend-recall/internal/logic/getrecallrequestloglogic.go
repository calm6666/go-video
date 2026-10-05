package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRecallRequestLogLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRecallRequestLogLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRecallRequestLogLogic {
	return &GetRecallRequestLogLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 回放一次在线召回请求（按 request_id 或 snapshot_id）
//
// 三条边界都必须守住：
//   - 两个 ID 都没给 -> ErrRequestLogRequired（调用方参数缺陷，与"未命中"严格区分）；
//   - 未命中 -> Entry=nil, err=nil（契约规定不存在时 entry 为 null，
//     既不能报 ErrRequestNotFound 让 gRPC 侧变错误码，也不能回一个空 Entry 冒充命中）；
//   - per_source 文本解析失败 -> 上抛错误，不静默给"没有逐路统计"的干净响应
//     （那会把一次降级读成正常，正是排障最不该得到的答案）。
//
// 隐私：本表永不存设备号/IP 原文（写入侧只接受 sha256 摘要，见 checkDeviceIDHash），
// 所以这里无需再脱敏，但也绝不新增回显原文的字段。
func (l *GetRecallRequestLogLogic) GetRecallRequestLog(in *rpc.GetRecallRequestLogReq) (*rpc.GetRecallRequestLogReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: GetRecallRequestLogReq", model.ErrRequestRequired)
	}
	requestID := strings.TrimSpace(in.GetRequestId())
	snapshotID := strings.TrimSpace(in.GetSnapshotId())
	if requestID == "" && snapshotID == "" {
		return nil, fmt.Errorf("%w: request_id or snapshot_id required", model.ErrRequestLogRequired)
	}
	repo := l.svcCtx.Repository
	var (
		row *model.RecallRequestLog
		err error
	)
	// 两者都给时以 request_id 为准：request_id 是幂等键（uniq_request_id），
	// snapshot_id 只是本次召回产物的标识，同一 request 重试可能产生新快照。
	if requestID != "" {
		row, err = repo.RequestLog.FindByRequestID(l.ctx, requestID)
	} else {
		row, err = repo.RequestLog.FindBySnapshotID(l.ctx, snapshotID)
	}
	if err != nil {
		if errors.Is(err, model.ErrRequestNotFound) {
			return &rpc.GetRecallRequestLogReply{Entry: nil}, nil
		}
		return nil, err
	}
	entry, err := requestLogInfo(row)
	if err != nil {
		return nil, err
	}
	return &rpc.GetRecallRequestLogReply{Entry: entry}, nil
}
