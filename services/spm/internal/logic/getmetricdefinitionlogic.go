// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetMetricDefinitionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetMetricDefinitionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetMetricDefinitionLogic {
	return &GetMetricDefinitionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个口径
func (l *GetMetricDefinitionLogic) GetMetricDefinition(in *rpc.GetMetricDefinitionReq) (*rpc.GetMetricDefinitionReply, error) {
	// 逻辑轮规划：metric_version>0 时按 (key, version) 精确读；=0 时取该 key 的 ACTIVE 版本；未登记时 found=false 且 definition 为 nil，不返回伪造口径。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "GetMetricDefinition")
	if err != nil {
		return nil, err
	}
	defer done()

	metricKey, err := checkMetricKey(in.GetMetricKey())
	if err != nil {
		return nil, err
	}
	if in.GetMetricVersion() > 0 {
		// 精确版本：DRAFT/RETIRED 也要能查到——本接口是口径评审与历史窗口解释的入口，
		// 按「只有 ACTIVE 可读」拒绝掉退役版本，就等于让历史窗口失去它的口径说明。
		row, err := l.svcCtx.Definitions.FindByKeyVersion(l.ctx, metricKey, in.GetMetricVersion())
		if err != nil {
			return nil, err
		}
		if row == nil {
			return &rpc.GetMetricDefinitionReply{Found: false}, nil
		}
		return &rpc.GetMetricDefinitionReply{Found: true, Definition: definitionOf(row)}, nil
	}
	if in.GetMetricVersion() < 0 {
		return nil, fmt.Errorf("%w: metric_version=%d", model.ErrMetricVersionRequired,
			in.GetMetricVersion())
	}
	// version=0 = 「当前 ACTIVE 版本」。走 resolveDefinition 而不是裸 FindActive：
	// 同键多 ACTIVE 时它给显式错误，而 FindActive 的 LIMIT 1 会随机挑一个版本回显，
	// 那副「查到了一份口径」的样子正是最需要避免的假成功。
	row, err := resolveDefinition(l.ctx, l.svcCtx, metricKey, 0)
	if err != nil {
		if errors.Is(err, model.ErrMetricDefinitionNotFound) {
			// 口径根本不存在 -> found=false（本接口的语义就是「查一下有没有这个口径」）。
			// 其余错误（多 ACTIVE 二义、库故障）照实返回，不做「查不到」的降级。
			return &rpc.GetMetricDefinitionReply{Found: false}, nil
		}
		return nil, err
	}
	return &rpc.GetMetricDefinitionReply{Found: true, Definition: definitionOf(row)}, nil
}
