package logic

import (
	"context"
	"fmt"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

type BatchGetFeaturesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBatchGetFeaturesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchGetFeaturesLogic {
	return &BatchGetFeaturesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量读取（feature × entity 笛卡尔积，有硬上限）
//
// 三道闸依次是：条数（model.ValidateBatchReadLimits，超限报错不截断）、
// 响应体字节（Read.MaxBatchResponseBytes，条数上限挡不住 50 个 512 维向量）、
// 降级矩阵（与 GetFeature 同一实现，保证两条读路径口径一致）。
func (l *BatchGetFeaturesLogic) BatchGetFeatures(in *rpc.BatchGetFeaturesReq) (*rpc.BatchGetFeaturesReply, error) {
	refs := in.GetFeatures()
	entities := in.GetEntities()
	entries, err := model.ValidateBatchReadLimits(len(refs), len(entities))
	if err != nil {
		return nil, err
	}

	r := newFeatureReader(l.ctx, l.svcCtx, l.Logger, in.GetAllowStale())
	list, degraded, err := r.read(refs, entities)
	if err != nil {
		return nil, err
	}
	reply := &rpc.BatchGetFeaturesReply{
		Entries:   list,
		Requested: int32(entries),
		Degraded:  degraded,
	}
	// 第二道闸按真实序列化字节判：超限报错而不是截断条目，
	// 否则调用方拿到「半个候选集」会以为那就是全部输入。
	if size := proto.Size(reply); size > l.svcCtx.MaxResponseBytes() {
		return nil, fmt.Errorf("%w: response %d bytes > %d, split the request",
			model.ErrTooManyEntries, size, l.svcCtx.MaxResponseBytes())
	}
	return reply, nil
}
