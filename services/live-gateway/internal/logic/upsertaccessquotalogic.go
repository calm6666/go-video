package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// upsertQuotaRPCName 本方法幂等键与审计里用的方法名标识。
const upsertQuotaRPCName = "UpsertAccessQuota"

type UpsertAccessQuotaLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertAccessQuotaLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertAccessQuotaLogic {
	return &UpsertAccessQuotaLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新建或更新配额配置（运营面，带版本与操作者审计）
func (l *UpsertAccessQuotaLogic) UpsertAccessQuota(in *rpc.UpsertAccessQuotaReq) (*rpc.AccessQuotaInfo, error) {
	// 已实现行为（编号对应本方法原桩注释；配额直接决定接入与广播的松紧，误写影响面是全房间）：
	// 1. 参数：quota 非空；operator 必填（ErrEmptyOperator，形态门禁）；request_id 必填（写幂等键）；
	//    expected_version<0 直接拒；scope/scope_id 组合与 TTL/载荷上下限整体交给
	//    model.CheckQuotaBounds(quota, MaxLeaseTTLSeconds, MaxTicketTTLSeconds, MaxPayloadBytes)
	//    ——本服务唯一的配额形态真值源，不在 logic 里再抄一份边界。
	// 2. 授权：g.requireDispositionWrite 之后**再收紧一层**——只有可信 OPERATOR 能改配额。
	//    SERVICE 角色不放过：内部服务能改配额就等于绕过运营审批链；
	//    AllowUnattestedOperatorWrites 的放开路径也过不了这道（未归因主体 role 不是 OPERATOR）。
	//    真正落库的 updated_by 取归因后的 caller 标识，in.operator 只作形态校验与展示（不信自报值）。
	// 3. 范围：NODE 层的 scope_id 是节点哈希、scope_key 只是可读 node_id；scope_key 缺失时
	//    记一条告警（以 scope_id 为准），不拒绝——排障时少一个可读标识不该挡住降配动作。
	// 4. 新建（expected_version==0）：Quotas.Create 靠 uniq_scope 禁止盲写覆盖。
	//    注意 model.Create 把 uniq_scope 冲突与 uniq_request_id 冲突**压成同一个 ErrVersionConflict**
	//    （isDuplicateErr 不区分索引），所以冲突后必须回读区分两种含义：
	//    行已存在 → 明确告知运营「带 expected_version 再来」；行不存在 → 是 request_id 重放，
	//    回 ErrRequestIdDuplicated 让其按首次结果处理。写之前还先 FindByRequestID 回读一次，
	//    并且**只允许回放到同一 (scope, scope_id)**：换个作用域复用同一把幂等键必须显式拒绝。
	// 5. 更新（expected_version>0）：model.Update 是整行覆盖写，所以先 FindOne 把本次留空（0=继承）
	//    的字段按原行值合并（lgwMergeQuotaInheritance），再带 version 条件更新；
	//    affected==0 时回读区分 ErrQuotaNotFound（行没有，应走新建）/ ErrVersionConflict（重读重试）。
	//    「这次就是要写 0」在 proto3 标量里表达不出来（没有 present 位），这是契约限制，
	//    见交付报告缺口；model 注释里那条「不在 SQL 玩 IF(col=0,old,new)」的约束因此只能由 logic 承接。
	// 6. 商业化边界：本表字段只有接入数/QPS/TTL/载荷/游客开关，没有也不得引入付费、投币、
	//    会员、分成相关的提额字段（AGENTS.md §1）。
	// 7. 审计与并发：version 由 conditionalUpdate 自增，updated_by/trace_id/request_id 落库；
	//    本表无历史快照，变更史由 audit 服务承担（AGENTS.md §5 不跨库写别人的表）。
	// 8. 失效缓存：Leases.BumpQuotaEpoch 让所有解析缓存下一次回源（resolveQuota 用 epoch 编进键）。
	//    自增失败只告警不失败：写已提交，最坏是延迟 QuotaCacheTTLSeconds 生效，
	//    而此时回滚这条配置反而会让 DB 与运营意图更不一致。
	// 9. 返回：回显**解析后的**生效配额（与 GetAccessQuota 同一投影、同一条 Resolve 路径），
	//    不让运营以为「写了 0 就是无限」。
	// 10. 错误映射：model 哨兵原样返回；ErrVersionConflict 是可重试错误，
	//     gateway/admin 需据此提示「配置已被他人修改」。
	if in == nil || in.GetQuota() == nil {
		return nil, fmt.Errorf("%w: quota is required", model.ErrInvalidQuotaScope)
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	info := in.GetQuota()
	scope, scopeID := int32(info.GetScope()), info.GetScopeId()
	requestID := strings.TrimSpace(in.GetRequestId())
	traceID := strings.TrimSpace(in.GetTraceId())
	if err := g.requireOperatorField(in.GetOperator()); err != nil {
		return nil, err
	}
	if err := g.requireRequestID(requestID); err != nil {
		return nil, err
	}
	if in.GetExpectedVersion() < 0 {
		return nil, fmt.Errorf("%w: expected_version=%d must be >= 0", model.ErrVersionConflict, in.GetExpectedVersion())
	}

	// --- 2. 授权（先于任何读写）---
	c, err := g.requireDispositionWrite(upsertQuotaRPCName)
	if err != nil {
		return nil, err
	}
	if !c.attested || c.role != model.RoleOperator {
		g.errorf("live-gateway: %s 被非 OPERATOR 主体调用：%s", upsertQuotaRPCName, c.String())
		return nil, fmt.Errorf("%w: %s requires an attested OPERATOR caller (got %s)",
			model.ErrPermissionDenied, upsertQuotaRPCName, c.String())
	}

	// --- 1/3. 形态与范围 ---
	q := &model.LiveGwAccessQuota{
		Scope:            scope,
		ScopeId:          scopeID,
		ScopeKey:         strings.TrimSpace(info.GetScopeKey()),
		MaxConnections:   info.GetMaxConnections(),
		BroadcastQps:     info.GetBroadcastQps(),
		DanmakuQps:       info.GetDanmakuQps(),
		LeaseTtlSeconds:  info.GetLeaseTtlSeconds(),
		TicketTtlSeconds: info.GetTicketTtlSeconds(),
		MaxPayloadBytes:  info.GetMaxPayloadBytes(),
		TraceId:          traceID,
	}
	if info.GetAllowGuest() {
		q.AllowGuest = 1
	}
	if err := model.CheckQuotaBounds(q, g.cfg().MaxLeaseTTLSeconds, g.cfg().MaxTicketTTLSeconds,
		g.cfg().MaxPayloadBytes); err != nil {
		return nil, err
	}
	if scope == model.QuotaScopeNode && q.ScopeKey == "" {
		g.errorf("live-gateway: NODE 层配额缺可读 scope_key（以 scope_id=%d 为准），排障时只能看哈希", scopeID)
	}

	store, serr := g.svcCtx.RequireStore()
	if serr != nil {
		return nil, serr
	}

	// --- 4. 幂等回读（先于任何写入）---
	first, ferr := store.Quotas.FindByRequestID(l.ctx, requestID)
	if ferr != nil {
		return nil, ferr
	}
	if first != nil {
		if first.Scope != scope || first.ScopeId != scopeID {
			return nil, fmt.Errorf("%w: request_id 已登记在 scope=%d scope_id=%d，不能复用到 scope=%d scope_id=%d",
				model.ErrRequestIdDuplicated, first.Scope, first.ScopeId, scope, scopeID)
		}
		g.infof("live-gateway: %s 幂等回放 request_id=%s scope=%d scope_id=%d version=%d",
			upsertQuotaRPCName, safeIdent(requestID), scope, scopeID, first.Version)
		return lgwQuotaView(l.ctx, g, store, scope, scopeID)
	}

	switch {
	case in.GetExpectedVersion() == 0:
		q.RequestId, q.UpdatedBy = requestID, c.String()
		if _, cerr := store.Quotas.Create(l.ctx, q); cerr != nil {
			if !errors.Is(cerr, model.ErrVersionConflict) {
				return nil, cerr
			}
			cur, rerr := store.Quotas.FindOne(l.ctx, scope, scopeID)
			if rerr != nil {
				return nil, rerr
			}
			if cur == nil {
				// 行不在 ⇒ 冲突来自 uniq_request_id：并发下另一笔同键请求已提交。
				return nil, fmt.Errorf("%w: %s 并发重放，请回读首次结果", model.ErrRequestIdDuplicated, upsertQuotaRPCName)
			}
			return nil, fmt.Errorf("%w: scope=%d scope_id=%d 已有配置行 version=%d，更新必须带 expected_version",
				model.ErrVersionConflict, scope, scopeID, cur.Version)
		}
	default:
		cur, rerr := store.Quotas.FindOne(l.ctx, scope, scopeID)
		if rerr != nil {
			return nil, rerr
		}
		if cur == nil {
			return nil, fmt.Errorf("%w: scope=%d scope_id=%d expected_version=%d，本层还没有配置行时应走 expected_version=0 新建",
				model.ErrQuotaNotFound, scope, scopeID, in.GetExpectedVersion())
		}
		lgwMergeQuotaInheritance(q, cur)
		q.RequestId, q.UpdatedBy = requestID, c.String()
		affected, uerr := store.Quotas.Update(l.ctx, scope, scopeID, in.GetExpectedVersion(), q)
		if uerr != nil {
			return nil, uerr
		}
		if affected == 0 {
			after, aerr := store.Quotas.FindOne(l.ctx, scope, scopeID)
			if aerr != nil {
				return nil, aerr
			}
			if after == nil {
				return nil, fmt.Errorf("%w: scope=%d scope_id=%d 在提交前被并发删除", model.ErrQuotaNotFound, scope, scopeID)
			}
			return nil, fmt.Errorf("%w: scope=%d scope_id=%d 当前 version=%d，请求 expected_version=%d",
				model.ErrVersionConflict, scope, scopeID, after.Version, in.GetExpectedVersion())
		}
	}

	// --- 8. 失效解析缓存 ---
	if _, berr := g.svcCtx.Leases.BumpQuotaEpoch(l.ctx); berr != nil {
		g.errorf("live-gateway: 配额代次 epoch 自增失败，新值最多延迟 %d 秒生效 scope=%d scope_id=%d: %v",
			g.cfg().QuotaCacheTTLSeconds, scope, scopeID, berr)
	}
	g.infof("live-gateway: 配额已更新 scope=%d scope_id=%d request_id=%s operator=%s 自报 operator=%s",
		scope, scopeID, safeIdent(requestID), c.String(), safeIdent(strings.TrimSpace(in.GetOperator())))
	return lgwQuotaView(l.ctx, g, store, scope, scopeID)
}

// lgwMergeQuotaInheritance 把本次未提交（0 值）的配额字段按原行值补齐。
//
// model.Update 是整行覆盖写，不做这一步会把「只改 QPS」的请求写成「其它字段全归零」，
// 而 0 在继承链里是「交给上一层」——等于顺手把该层的其它限制全部上移，影响面远超一次点保存。
func lgwMergeQuotaInheritance(q, cur *model.LiveGwAccessQuota) {
	merge := func(dst *int32, from int32) {
		if *dst == 0 {
			*dst = from
		}
	}
	merge(&q.MaxConnections, cur.MaxConnections)
	merge(&q.BroadcastQps, cur.BroadcastQps)
	merge(&q.DanmakuQps, cur.DanmakuQps)
	merge(&q.LeaseTtlSeconds, cur.LeaseTtlSeconds)
	merge(&q.TicketTtlSeconds, cur.TicketTtlSeconds)
	merge(&q.MaxPayloadBytes, cur.MaxPayloadBytes)
	if q.AllowGuest == 0 {
		q.AllowGuest = cur.AllowGuest
	}
	if q.ScopeKey == "" {
		q.ScopeKey = cur.ScopeKey
	}
}

// lgwQuotaView 按 (scope, scope_id) 回一份**解析后**的生效配额，读写两侧共用同一投影路径。
func lgwQuotaView(ctx context.Context, g gate, store *repository.Store, scope int32,
	scopeID int64) (*rpc.AccessQuotaInfo, error) {
	eff, err := g.resolveQuota(ctx, scope, scopeID)
	if err != nil {
		return nil, err
	}
	row, err := store.Quotas.FindOne(ctx, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return quotaInfo(scope, scopeID, "", eff, row), nil
}
