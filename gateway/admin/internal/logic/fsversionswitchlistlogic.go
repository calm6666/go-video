// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	featurestorerpc "go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type FsVersionSwitchListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 版本切换审计分页（feature_key 为空 = 全部；含操作人与理由）
func NewFsVersionSwitchListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsVersionSwitchListLogic {
	return &FsVersionSwitchListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsVersionSwitchList 转发 feature-store ListVersionSwitches（版本切换审计流水）。
//
// 审计流水的默认视图是「最近发生过什么」，所以 feature_key 留空 = 全部是正确语义，
// 不在这里要求必填（与 definition/get 的 key 必填是两回事：那里问的是某一个特征）。
// since=0 是「不限时间」哨兵，原样下传；网关不拿自己的钟替服务判起点。
// 契约缺口（已上报）：SwitchRecord 没有 switch_type 位，回滚与前进切换在流水里
// 长得一样，只能靠 from/to 的大小关系猜——因此这里把 from/to 逐字段回全，不裁剪。
func (l *FsVersionSwitchListLogic) FsVersionSwitchList(req *types.ParamFsVersionSwitchList) (resp *types.FsVersionSwitchListResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	if err := fsPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	if err := fsNonNeg("since", req.Since); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.ListVersionSwitches(l.ctx, &featurestorerpc.ListVersionSwitchesReq{
		FeatureKey: req.FeatureKey,
		Since:      req.Since,
		Pn:         req.Pn,
		Ps:         req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsVersionSwitchList: feature_key=%s since=%d pn=%d ps=%d err=%v",
			req.FeatureKey, req.Since, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.FsVersionSwitchListResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsVersionSwitchListData{
			Items: fsSwitchRecordsToAPI(reply.GetItems()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
