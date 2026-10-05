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

type FsRetentionPurgeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 手动触发一轮 TTL 过期清理（幂等键必填；批量与截止时间由服务判）
func NewFsRetentionPurgeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsRetentionPurgeLogic {
	return &FsRetentionPurgeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsRetentionPurge 转发 feature-store PurgeExpired（清理一批 TTL 过期值）。
//
// 常态调用方是 services/cron；这条路由的存在是为了「配置漏跑时补一轮」，因此挂权限点。
// 三个 0 哨兵一律原样下传，不替调用方决定：
//   - limit=0 = 服务端上限批大小；超过上限时服务**夹取**而不是拒（剩余量由 remaining 如实报），
//     网关不重复这个夹取，也不因「批大了」自行改写；
//   - before=0 = 服务当前时间；before 晚于服务时钟会删到还没过期的值，那是数据丢失，
//     服务用 ErrCutoffRequired 拒掉——判据是**服务的钟**，网关拿本地时间比一遍只会多制造误判。
//
// 契约缺口（已上报）：PurgeExpiredReq 是本域唯一没有 reason 位的写方法，
// 手工补跑一轮清理时「为什么现在跑」落不进服务侧留痕（操作人与批大小有：走幂等回执表）。
// 因此这里对 idempotency_key 的要求比别的读操作更硬：同一轮清理重放不应二次删除。
// purged=0 是正常结论（没有到期行），remaining 是近似值、只供「要不要再跑一轮」判断，
// 两者都不折叠成成功/失败。
func (l *FsRetentionPurgeLogic) FsRetentionPurge(req *types.ParamFsRetentionPurge) (resp *types.FsRetentionPurgeResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	operator, err := fsOperator(l.ctx, "fsRetentionPurge")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := fsNonNeg("limit", req.Limit); err != nil {
		return nil, err
	}
	// rpc 的 limit 是 int32：超范围的数截断后可能落到 0/负数，而那正是「清到服务端上限」的
	// 哨兵语义——一次打字失误就会变成「清一批上限」，所以这个截断必须挡在网关。
	if err := fsInt32Range("limit", req.Limit); err != nil {
		return nil, err
	}
	if err := fsNonNeg("before", req.Before); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.PurgeExpired(l.ctx, &featurestorerpc.PurgeExpiredReq{
		Limit:     int32(req.Limit),
		Before:    req.Before,
		RequestId: req.IdempotencyKey,
		Operator:  operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsRetentionPurge: limit=%d before=%d operator=%s trace_id=%s err=%v",
			req.Limit, req.Before, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/fsRetentionPurge: purged=%d remaining=%d reused=%t operator=%s",
		reply.GetPurged(), reply.GetRemaining(), reply.GetReused(), operator)
	return &types.FsRetentionPurgeResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsRetentionPurgeData{
			Purged:    reply.GetPurged(),
			Remaining: reply.GetRemaining(),
			Reused:    reply.GetReused(),
		},
		TTL: 0,
	}, nil
}
