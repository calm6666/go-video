package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAccessQuotaLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetAccessQuotaLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAccessQuotaLogic {
	return &GetAccessQuotaLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 读取某作用域生效的配额（含继承链解析结果）
func (l *GetAccessQuotaLogic) GetAccessQuota(in *rpc.AccessQuotaReq) (*rpc.AccessQuotaInfo, error) {
	// 已实现行为（编号对应本方法原桩注释）：
	// 1. 参数：model.ValidQuotaScope（ErrInvalidQuotaScope）；GLOBAL 层 scope_id 必须为 0、
	//    其它层必须 >0 —— 与 model.CheckQuotaBounds 同一套约束，读路径也校，
	//    否则「拿 scope_id=0 查 ROOM 层」会把 GLOBAL 行读成某个房间的生效值。
	// 2. 语义是**实际生效**配额，不是某一行原始配置：整体复用 g.resolveQuota
	//    （QuotaScopeChain 自外向内覆盖 + quotaDefaultsFromConfig 兜底），
	//    与 Acquire/Join/Broadcast 的判定用的是同一个函数，避免「读到的配额」和
	//    「实际限流的配额」两条路径漂移。
	// 3. 返回体里 0 值继承必须已被展开成具体数字（conv.quotaInfo 投的是 EffectiveQuota），
	//    客户端不再解继承。
	// 4. 可解释性：EffectiveQuota.HitScopes 记了哪些层真正贡献了值。
	//    AccessQuotaInfo **没有承载 hit 链的字段**（本轮禁止改 proto），所以命中链只进服务端日志；
	//    运营要「一条连接被哪层拒了」需要契约新增字段，见交付报告缺口。
	// 5. 缓存：g.resolveQuota 内部按 QuotaCacheTTLSeconds + 配额代次 epoch 缓存，
	//    UpsertAccessQuota 用 BumpQuotaEpoch 主动失效，不需要 SCAN 删键。
	// 6. 无 GLOBAL 行不报错：quotaDefaultsFromConfig 是兜底层（配置是兜底，数据以 DB 为准）。
	// 7. 只读无副作用：不写 version、不建默认行、不自增任何计数。
	//    唯一的一次额外读是 FindOne 取本层原始行的 version/updated_by/ctime 供展示，
	//    行不存在时 row=nil，投影照常回生效值（这不是错误，是「本层未配置」）。
	// 8. 错误映射：ErrInvalidQuotaScope 原样返回；DB 故障由 resolveQuota 原样抛出，
	//    绝不降级成默认值（拿默认值当生效值会让限流判定与运营意图相反）。
	if in == nil {
		return nil, model.ErrInvalidQuotaScope
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	scope, scopeID := int32(in.GetScope()), in.GetScopeId()
	if !model.ValidQuotaScope(scope) {
		return nil, fmt.Errorf("%w: scope=%d", model.ErrInvalidQuotaScope, scope)
	}
	if scope == model.QuotaScopeGlobal && scopeID != 0 {
		return nil, fmt.Errorf("%w: global scope needs scope_id=0, got %d", model.ErrInvalidQuotaScope, scopeID)
	}
	if scope != model.QuotaScopeGlobal && scopeID <= 0 {
		return nil, fmt.Errorf("%w: scope=%d needs positive scope_id", model.ErrInvalidQuotaScope, scope)
	}

	eff, err := g.resolveQuota(l.ctx, scope, scopeID)
	if err != nil {
		return nil, err
	}

	var row *model.LiveGwAccessQuota
	if store, serr := g.svcCtx.RequireStore(); serr == nil {
		if row, serr = store.Quotas.FindOne(l.ctx, scope, scopeID); serr != nil {
			return nil, serr
		}
	} else {
		return nil, serr
	}
	g.infof("live-gateway: 配额解析 scope=%d scope_id=%d 命中链=%v 来自原始行=%t",
		scope, scopeID, strings.Join(eff.HitScopes, ">"), row != nil)
	return quotaInfo(scope, scopeID, "", eff, row), nil
}
