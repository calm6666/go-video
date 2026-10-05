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

type FsEntityFeatureListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按主体导出特征（隐私核对；可见级别受服务侧上限收敛）
func NewFsEntityFeatureListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsEntityFeatureListLogic {
	return &FsEntityFeatureListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsEntityFeatureList 转发 feature-store ListEntityFeatures（针对单个主体的特征导出）。
//
// 与目录/审计类只读不同档：它是「定向读某个人的全部特征值」，有明确的被读主体，
// 因此挂 AdminPermission 单独占一个权限点（授权能单独收回、访问能进判定与留痕），
// 与 spm:interest 同一口径。
// 契约缺口（已上报）：ListEntityFeaturesReq 只有 entity/min_privacy_level/pn/ps，**没有
// operator 位**，所以这次读取在 feature-store 侧落不下「谁读的」——留痕只剩网关访问日志与
// 权限中间件的判定记录。会话身份因此仍要取（拿不到就 fail-closed），但只用于日志主体，
// 不塞进别的字段。
// 可见范围由服务的 Privacy.ExportMaxPrivacyLevel 收敛：超上限的级别返回空集而不是放宽，
// 网关不把「被上限挡住」翻译成「这个主体没有特征」，也不代为预读一次判断会不会被挡。
// entity_id 与各条特征值都回在响应体里（核对就是它的用途），但**永不进日志**（§7）；
// 日志只记 scope、级别、分页与条数。
func (l *FsEntityFeatureListLogic) FsEntityFeatureList(req *types.ParamFsEntityFeatureList) (resp *types.FsEntityFeatureListResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	operator, err := fsOperator(l.ctx, "fsEntityFeatureList")
	if err != nil {
		return nil, err
	}
	entity, err := fsEntity(req.EntityScope, req.EntityId)
	if err != nil {
		return nil, err
	}
	if err := fsPrivacyFilter("min_privacy_level", req.MinPrivacyLevel); err != nil {
		return nil, err
	}
	if err := fsPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.ListEntityFeatures(l.ctx, &featurestorerpc.ListEntityFeaturesReq{
		Entity:          entity,
		MinPrivacyLevel: featurestorerpc.PrivacyLevel(req.MinPrivacyLevel),
		Pn:              req.Pn,
		Ps:              req.Ps,
	})
	if err != nil {
		// 失败分支同样不打 entity_id：错误文本可能带主键，日志侧不能依赖服务不回显。
		l.Errorf("gateway/admin/fsEntityFeatureList: entity_scope=%s min_privacy_level=%d pn=%d ps=%d operator=%s err=%v",
			entity.EntityScope.String(), req.MinPrivacyLevel, req.Pn, req.Ps, operator, err)
		return nil, err
	}
	l.Infof("gateway/admin/fsEntityFeatureList: entity_scope=%s entries=%d total=%d operator=%s",
		entity.EntityScope.String(), len(reply.GetEntries()), reply.GetTotal(), operator)
	return &types.FsEntityFeatureListResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsEntityFeatureListData{
			Entries: fsEntriesToAPI(reply.GetEntries()),
			Total:   reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
