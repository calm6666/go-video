package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PurgeExpiredLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPurgeExpiredLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PurgeExpiredLogic {
	return &PurgeExpiredLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// purgeSnapshot 是清理回执的可回放快照（计数即可完整回放整批清理的对外结果）。
type purgeSnapshot struct {
	Purged    int32 `json:"p"`
	Remaining int64 `json:"r"`
}

// 清理 TTL 过期值（cron 调用）
//
// 三步顺序不能换，且每一步都 bounded：
//  1. 按 idx_expire 选一批过期行（ListExpiredForPurge 与 SelectExpiredIDs 同谓词同排序）；
//  2. 按主键删（不做范围 DELETE：范围锁会堵住在线写与隐私擦除）；
//  3. 删成功后 DEL 掉这一批行的 fs:val:* 缓存键。
//
// 第 3 步是隐私承诺的一部分：只删库里、缓存还在，等于过期值在 TTL 之后仍能被在线读到。
// 缓存删除失败只记日志不回滚 —— DB 已是事实源，缓存残留最长活到自己的 TTL。
// remaining 用同条件 COUNT 给出（允许近似），供调用方判断是否需要再跑一轮。
func (l *PurgeExpiredLogic) PurgeExpired(in *rpc.PurgeExpiredReq) (*rpc.PurgeExpiredReply, error) {
	requestID := strings.TrimSpace(in.GetRequestId())
	operator := strings.TrimSpace(in.GetOperator())
	if err := checkRequestID(requestID); err != nil {
		return nil, err
	}
	if err := checkOperator(operator); err != nil {
		return nil, err
	}
	maxRows := l.svcCtx.PurgeLimit()
	limit := in.GetLimit()
	if limit <= 0 {
		limit = maxRows
	}
	// 上限是「收敛」而不是「拒绝」：cron 的批大小来自配置，越界时按服务端硬上限跑完这一轮
	// 比整轮失败更可用（剩余量由 remaining 如实报告）。
	if limit > maxRows {
		l.Infow("purge limit clamped", logx.Field("requested", limit), logx.Field("cap", maxRows))
		limit = maxRows
	}
	now := model.NowUnix()
	before := in.GetBefore()
	if before <= 0 {
		before = now
	}
	if before > now {
		// 未来截止时间会把还没过期的值当成过期删掉，那是数据丢失而不是清理。
		return nil, fmt.Errorf("%w: before %d is later than server time %d", model.ErrCutoffRequired,
			before, now)
	}

	spec := receiptSpec{
		requestID: requestID,
		opType:    model.ReceiptOpPurge,
		rowCount:  limit,
		operator:  operator,
	}
	res, owner, err := beginReceipt(l.ctx, l.svcCtx, l.Logger, spec)
	if err != nil {
		return nil, err
	}
	if !res.Execute {
		return l.replay(res.Receipt)
	}

	rows, err := l.svcCtx.Values.ListExpiredForPurge(l.ctx, before, limit)
	if err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	var purged int64
	if len(rows) > 0 {
		ids := make([]int64, 0, len(rows))
		keys := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ValueID)
			keys = append(keys, r.Key().CacheKey())
		}
		deleted, err := l.svcCtx.Values.DeleteByIDs(l.ctx, ids)
		if err != nil {
			failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
			return nil, err
		}
		purged = deleted
		invalidateCache(l.ctx, l.svcCtx, l.Logger, keys)
	}
	remaining, err := l.svcCtx.Values.CountExpired(l.ctx, before)
	if err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	raw := marshalJSON(purgeSnapshot{Purged: int32(purged), Remaining: remaining})
	if err := finishReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, model.ReceiptSnapshot{
		ResultJSON:   raw,
		ResultDigest: resultDigest(raw),
		AffectedRows: purged,
		DetailKept:   true,
	}); err != nil {
		return nil, err
	}
	l.Infow("expired values purged", logx.Field("purged", purged), logx.Field("remaining", remaining),
		logx.Field("before", before), logx.Field("operator", operator))
	return &rpc.PurgeExpiredReply{Purged: int32(purged), Remaining: remaining}, nil
}

// replay 同一 request_id 重放：返回首次的 purged/remaining，绝不重复删一批。
func (l *PurgeExpiredLogic) replay(receipt *model.WriteReceipt) (*rpc.PurgeExpiredReply, error) {
	var snap purgeSnapshot
	if err := unmarshalJSON(receipt.ResultJSON, &snap); err != nil {
		return nil, fmt.Errorf("feature-store: purge receipt unreadable: %w", err)
	}
	return &rpc.PurgeExpiredReply{Purged: snap.Purged, Remaining: snap.Remaining, Reused: true}, nil
}
