// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListMetricDefinitionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListMetricDefinitionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListMetricDefinitionsLogic {
	return &ListMetricDefinitionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 口径列表（分页）
func (l *ListMetricDefinitionsLogic) ListMetricDefinitions(in *rpc.ListMetricDefinitionsReq) (*rpc.ListMetricDefinitionsReply, error) {
	// 逻辑轮规划：校验 ps<=100 -> 按 metric_key 前缀与 state 过滤，(metric_key, metric_version) 倒序分页 -> 回带 total；供管理后台与口径评审自查。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "ListMetricDefinitions")
	if err != nil {
		return nil, err
	}
	defer done()

	size, err := pageSize(l.svcCtx, in.GetPs())
	if err != nil {
		return nil, err
	}
	filter := model.MetricDefinitionFilter{Limit: size, Offset: offsetTo32(pageOffset(in.GetPn(), size))}
	if key := trimMetricKey(in.GetMetricKey()); key != "" {
		// 精确匹配而不是前缀 LIKE：model 的过滤条件走 uniq_metric_version 的最左列，
		// 前缀匹配（尤其 % 在前的写法）会退化成全表扫描，而这张表是所有人读口径的入口。
		filter.MetricKey = key
	}
	if state := int32(in.GetState()); state != model.DefinitionStateUnspecified {
		if !validDefinitionStateStrict(state) {
			return nil, fmt.Errorf("%w: 过滤状态=%d 不是 DRAFT/ACTIVE/RETIRED",
				model.ErrInvalidDefinitionState, state)
		}
		filter.State = state
	}

	total, err := l.svcCtx.Definitions.Count(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	reply := &rpc.ListMetricDefinitionsReply{Total: total}
	if !pageFits(int64(filter.Offset), total, size) {
		// 越界页/深翻页：total 已回带，不再发查询。
		return reply, nil
	}
	rows, err := l.svcCtx.Definitions.List(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	reply.Definitions = definitionList(rows)
	return reply, nil
}

// trimMetricKey 裁剪过滤键：空串 = 不限。超长的过滤串不可能是已登记的键
// （登记侧按列宽拒绝），这里按「不过滤」处理而不是让列表接口整体失败。
func trimMetricKey(raw string) string {
	key := strings.TrimSpace(raw)
	if len(key) > maxMetricKeyBytes {
		return ""
	}
	return key
}
