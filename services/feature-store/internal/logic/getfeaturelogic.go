package logic

import (
	"context"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetFeatureLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetFeatureLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetFeatureLogic {
	return &GetFeatureLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 读取单个特征值（缺失必降级，降级必须显式表达）
//
// 落地：版本解析（0=ACTIVE 指针）→ Redis 主读 → miss 回源 feature_value →
// 降级矩阵在 model.ClassifyDegradation 一处判定（GetFeature 是批量读的单条特例，
// 两条路径共用同一实现，避免两套略有差别的降级口径）。
// 定义/指针本身读不到（DB 故障、key 未注册、该 key 没有 ACTIVE 版本）是**错误**而不是降级：
// 没有定义就没有可信的 default_value，返回任何值都是伪造。
func (l *GetFeatureLogic) GetFeature(in *rpc.GetFeatureReq) (*rpc.GetFeatureReply, error) {
	r := newFeatureReader(l.ctx, l.svcCtx, l.Logger, in.GetAllowStale())
	entries, _, err := r.read([]*rpc.FeatureRef{in.GetFeature()}, []*rpc.EntityRef{in.GetEntity()})
	if err != nil {
		return nil, err
	}
	entry := entries[0]
	return &rpc.GetFeatureReply{
		Entry: entry,
		Found: foundByDegradation(entry.GetDegradation()),
	}, nil
}
