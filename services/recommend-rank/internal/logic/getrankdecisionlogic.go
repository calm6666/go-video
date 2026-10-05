package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRankDecisionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRankDecisionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRankDecisionLogic {
	return &GetRankDecisionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 按 decision_id/request_id 回放一次排序决策
//
// 两个键至少给一个（否则 ErrDecisionIDRequired），decision_id 优先：
// 它是 model.NewDecisionID 产出的对外审计 ID，唯一性最强；request_id 是幂等锚点，
// 同一 request_id 只会有一行（uniq_request_id），因此也能定位到同一次决策。
//
// 未命中返回 ErrDecisionNotFound 且不回一个空 RankDecisionInfo：
// 「查无此行」和「这一行的字段都是空」是两件事，后者会让人以为线上真发生过一次零输入排序。
//
// 回放的完整性边界（README 已知缺口）：表里只存 result_digest + top_aids（前 MaxDigestAids 个 aid），
// 完整 200 位结果与每项的分数不落库（写放大不可接受），所以本接口能证明
// 「哪次请求、哪个模型版本、哪个实验分桶、是否降级、前 N 位是什么」，
// 不能逐位复现整页分数。
func (l *GetRankDecisionLogic) GetRankDecision(in *rpc.GetRankDecisionReq) (*rpc.GetRankDecisionReply, error) {
	if l.svcCtx == nil || l.svcCtx.Repository == nil {
		return nil, model.ErrRepositoryNotConfigured
	}
	if in == nil {
		return nil, model.ErrDecisionIDRequired
	}
	decisionID, err := optionalIdent("decision_id", in.GetDecisionId(), colIdent)
	if err != nil {
		return nil, err
	}
	requestID, err := optionalIdent("request_id", in.GetRequestId(), colIdent)
	if err != nil {
		return nil, err
	}
	if decisionID == "" && requestID == "" {
		return nil, model.ErrDecisionIDRequired
	}

	logs := l.svcCtx.Repository.DecisionLogs()
	var (
		row *model.RankDecisionLog
		key string
	)
	if decisionID != "" {
		row, err = logs.FindByDecisionID(l.ctx, decisionID)
		key = "decision_id=" + decisionID
	} else {
		row, err = logs.FindByRequestID(l.ctx, requestID)
		key = "request_id=" + requestID
	}
	if err != nil {
		if errors.Is(err, model.ErrDecisionNotFound) {
			return nil, fmt.Errorf("%w: %s", err, key)
		}
		return nil, err
	}
	// 命中即回投影：subject_id 落库时已只存 mid/设备摘要，这里不存在额外脱敏动作。
	return &rpc.GetRankDecisionReply{Entry: decisionInfo(row)}, nil
}
