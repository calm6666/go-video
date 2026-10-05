package logic

import (
	"context"
	"fmt"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTopicsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTopicsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTopicsLogic {
	return &ListTopicsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 后台分页列出专题（只读，不写审计）
func (l *ListTopicsLogic) ListTopics(in *rpc.ListTopicsReq) (*rpc.ListTopicsReply, error) {
	lim := newLimits(l.svcCtx.Config)
	pn, ps, err := pageOf(in.GetPn(), in.GetPs(), lim.maxPageSize)
	if err != nil {
		return nil, err
	}
	state := in.GetState()
	if state != 0 {
		if err := checkState(state); err != nil {
			return nil, err
		}
	}
	keyword, err := checkKeywordLen(in.GetKeyword())
	if err != nil {
		return nil, err
	}
	if in.GetZoneId() < 0 || in.GetTagId() < 0 {
		return nil, fmt.Errorf("%w: zone_id/tag_id 不能为负", model.ErrItemRefRequired)
	}

	now := model.NowUnix()
	if !in.GetOnlineOnly() {
		// 非 online_only 时不做窗口判定，Now 就没参与 WHERE；显式传 0 以免被误读成「按 now 过滤」。
		now = 0
	}

	rows, total, err := l.svcCtx.Models.Topic.List(l.ctx, model.TopicFilter{
		State:   state,
		ZoneID:  in.GetZoneId(),
		TagID:   in.GetTagId(),
		Keyword: keyword,
		// 生效窗口是服务端判定：用调用方时钟会出现同一专题两端结论不同。
		OnlineOnly: in.GetOnlineOnly(),
		Now:        now,
		Pn:         pn,
		Ps:         ps,
	})
	if err != nil {
		return nil, err
	}
	// 排序固定 sort ASC, topic_id ASC（model 侧实现）：没有第二个 tiebreaker 的 ORDER BY
	// 在翻页时会重复或漏行。zone_ids/tag_ids 只回 ID，展示名由调用方去 catalog 批量取。
	return &rpc.ListTopicsReply{Items: topicList(rows), Total: lim.totalOf(total)}, nil
}
