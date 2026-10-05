package logic

import (
	"context"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListVersionSwitchesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListVersionSwitchesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListVersionSwitchesLogic {
	return &ListVersionSwitchesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 版本切换审计列表
//
// 审计行只增不改，回滚表现为一条新的 rollback 审计而不是删掉历史行；
// 排序固定 switch_id 倒序（自增主键即时间序，避免 filesort），ps 越界直接报错。
func (l *ListVersionSwitchesLogic) ListVersionSwitches(
	in *rpc.ListVersionSwitchesReq) (*rpc.ListVersionSwitchesReply, error) {
	if err := model.ValidatePageSize(in.GetPn(), in.GetPs()); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.GetFeatureKey())
	if key != "" {
		if err := checkFeatureKey(key); err != nil {
			return nil, err
		}
	}
	if in.GetSince() < 0 {
		return nil, model.ErrBackfillWindowInvalid
	}
	rows, total, err := l.svcCtx.Switches.List(l.ctx, model.VersionSwitchFilter{
		FeatureKey: key,
		Since:      in.GetSince(),
		Pn:         in.GetPn(),
		Ps:         in.GetPs(),
	})
	if err != nil {
		return nil, err
	}
	items := make([]*rpc.ListVersionSwitchesReply_SwitchRecord, 0, len(rows))
	for _, r := range rows {
		items = append(items, &rpc.ListVersionSwitchesReply_SwitchRecord{
			SwitchId:    r.SwitchID,
			FeatureKey:  r.FeatureKey,
			FromVersion: r.FromVersion,
			ToVersion:   r.ToVersion,
			Operator:    r.Operator,
			Reason:      r.Reason,
			RequestId:   r.RequestID,
			Ctime:       r.Ctime,
		})
	}
	return &rpc.ListVersionSwitchesReply{Items: items, Total: total}, nil
}
