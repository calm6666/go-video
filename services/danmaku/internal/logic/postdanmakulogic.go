package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"go-video/common/idempotency"
	"go-video/common/ratelimit"
	"go-video/services/danmaku/internal/policy"
	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 弹幕展示与字号默认值，与 rpc.DanmakuMode 取值一致。
const (
	defaultFontsize int32 = 25
	defaultColor    int32 = 0xFFFFFF
	minFontsize     int32 = 10
	maxFontsize     int32 = 64
)

type PostDanmakuLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPostDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PostDanmakuLogic {
	return &PostDanmakuLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// PostDanmaku 发送弹幕。
//
// 处理顺序（AGENTS.md §8 审核门禁 + §5 写接口幂等）：
//  1. 参数校验（含字号/颜色/长度归一）；
//  2. 进程级令牌桶（common/ratelimit），保护 MySQL 与审核下游；
//  3. 幂等键重放：命中已落库弹幕则直接返回原 dmid，不消耗发送配额；
//  4. Redis 分钟窗口防刷屏（按 mid、按 oid 两级）；
//  5. 屏蔽词过滤（全局 + 分区词库）；
//  6. 落库：未命中屏蔽词且机审门禁开启 → 待审核/审核池；命中 → 折叠/屏蔽池；
//     只有机审门禁关闭才直接进普通池；
//  7. 提交机审并回填 task_id；下游不可用返回明确错误，不伪造“已送审”。
func (l *PostDanmakuLogic) PostDanmaku(in *rpc.PostDanmakuReq) (*rpc.PostDanmakuReply, error) {
	cfg := l.svcCtx.Config.Danmaku

	if in.Oid <= 0 {
		return nil, model.ErrInvalidOid
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.ProgressMs < 0 {
		return nil, model.ErrInvalidProgress
	}
	if err := validateContent(in.Content, cfg.MaxContentLength); err != nil {
		return nil, err
	}
	mode, fontsize, color := normalizeDisplay(in.Mode, in.Fontsize, in.Color)
	segNo := policy.SegNo(in.ProgressMs, l.svcCtx.SegmentSeconds)

	// 进程级令牌桶：无令牌立即拒绝，避免突发弹幕穿透到 MySQL。
	done, err := l.svcCtx.PostLimiter.Allow(l.ctx)
	if err != nil {
		l.Errorf("danmaku/PostDanmaku: process limiter oid=%d mid=%d err=%v", in.Oid, in.Mid, err)
		return nil, model.ErrRateLimited
	}
	defer done(ratelimit.Success)

	idemKey, err := idempotencyKey(in)
	if err != nil {
		return nil, err
	}

	// 幂等重放：客户端重试不重复计数，也不重复送审。
	existing, err := l.svcCtx.Repository.FindByIdempotencyKey(l.ctx, idemKey)
	if err != nil {
		l.Errorf("danmaku/PostDanmaku: replay lookup oid=%d key=%s err=%v", in.Oid, idemKey, err)
		return nil, err
	}
	if existing != nil {
		return l.replay(existing, cfg.MachineReviewEnabled)
	}

	// 防刷屏：两个维度的分钟窗口各自 +1 后统一判定。
	midCount, oidCount, err := l.svcCtx.Repository.IncrSendWindows(l.ctx, in.Mid, in.Oid, time.Now())
	if err != nil {
		l.Errorf("danmaku/PostDanmaku: rate window oid=%d mid=%d err=%v", in.Oid, in.Mid, err)
		return nil, err
	}
	if ok, dim := policy.CheckWindows(
		policy.Window{Dimension: "mid", Count: midCount, Limit: cfg.MaxPerUserPerMinute},
		policy.Window{Dimension: "oid", Count: oidCount, Limit: cfg.MaxPerOidPerMinute},
	); !ok {
		l.Errorf("danmaku/PostDanmaku: rate limited oid=%d mid=%d dim=%s midCount=%d oidCount=%d",
			in.Oid, in.Mid, dim, midCount, oidCount)
		return nil, fmt.Errorf("%w: %s", model.ErrRateLimited, dim)
	}

	// 屏蔽词过滤：词库读失败必须显式失败，不能默认放行。
	blocked := false
	if cfg.SensitiveWordCheckEnabled {
		filter, err := l.svcCtx.Repository.BlockWordFilter(l.ctx, in.Oid)
		if err != nil {
			l.Errorf("danmaku/PostDanmaku: load block words oid=%d err=%v", in.Oid, err)
			return nil, err
		}
		if word, hit := filter.Match(in.Content); hit {
			blocked = true
			l.Infof("danmaku/PostDanmaku: block word hit oid=%d mid=%d word=%s", in.Oid, in.Mid, word)
		}
	}

	state, pool := policy.InitialState(blocked, cfg.MachineReviewEnabled)
	now := time.Now().Unix()
	d := &model.Danmaku{
		Oid:            in.Oid,
		Aid:            aidOf(in),
		Mid:            in.Mid,
		ProgressMs:     in.ProgressMs,
		Mode:           mode,
		Fontsize:       fontsize,
		Color:          color,
		Content:        in.Content,
		State:          state,
		Pool:           pool,
		SegNo:          segNo,
		IdempotencyKey: idemKey,
		TraceId:        in.TraceId,
		Ctime:          now,
		Mtime:          now,
	}
	dmid, created, err := l.svcCtx.Repository.CreateDanmaku(l.ctx, d)
	if err != nil {
		l.Errorf("danmaku/PostDanmaku: create oid=%d mid=%d err=%v", in.Oid, in.Mid, err)
		return nil, err
	}
	if !created {
		// 并发下唯一索引兜底：另一个请求已写入同一幂等键。
		again, err := l.svcCtx.Repository.FindByIdempotencyKey(l.ctx, idemKey)
		if err != nil {
			return nil, err
		}
		if again == nil {
			again = d
			again.Dmid = dmid
		}
		return l.replay(again, cfg.MachineReviewEnabled)
	}
	d.Dmid = dmid

	var taskID int64
	if cfg.MachineReviewEnabled {
		taskID, err = l.submitForReview(d, blocked)
		if err != nil {
			// 行已落库且处于待审/屏蔽池（不会被下发），把失败原样返回给调用方重试。
			l.Errorf("danmaku/PostDanmaku: submit moderation dmid=%d oid=%d err=%v", dmid, in.Oid, err)
			return nil, err
		}
	}

	return &rpc.PostDanmakuReply{
		Dmid:             dmid,
		State:            d.State,
		Pool:             d.Pool,
		SegNo:            segNo,
		Ctime:            d.Ctime,
		ModerationTaskId: taskID,
	}, nil
}

// submitForReview 提交机审并回填 task_id。
// 客户端未配置审核 RPC 时返回 model.ErrModerationNotConfigured，绝不静默放行。
func (l *PostDanmakuLogic) submitForReview(d *model.Danmaku, blocked bool) (int64, error) {
	if l.svcCtx.Moderation == nil {
		return 0, model.ErrModerationNotConfigured
	}
	reason := "danmaku-publish"
	if blocked {
		reason = "danmaku-blockword"
	}
	taskID, err := l.svcCtx.Moderation.SubmitForReview(l.ctx, d.Dmid, d.Mid, reason)
	if err != nil {
		return 0, err
	}
	if err := l.svcCtx.Repository.SetModerationTaskID(l.ctx, d.Dmid, taskID); err != nil {
		// 任务已创建，回填失败只记日志：重放路径会按 moderation_task_id=0 再次送审。
		l.Errorf("danmaku/PostDanmaku: backfill task id dmid=%d task=%d err=%v", d.Dmid, taskID, err)
	}
	return taskID, nil
}

// replay 返回幂等重放结果；上一次送审失败时在本次重试补送。
func (l *PostDanmakuLogic) replay(d *model.Danmaku, machineReview bool) (*rpc.PostDanmakuReply, error) {
	taskID := d.ModerationTaskId
	if machineReview && taskID == 0 {
		submitted, err := l.submitForReview(d, d.Pool == model.PoolBlock)
		if err != nil {
			l.Errorf("danmaku/PostDanmaku: replay submit moderation dmid=%d err=%v", d.Dmid, err)
			return nil, err
		}
		taskID = submitted
	}
	return &rpc.PostDanmakuReply{
		Dmid:             d.Dmid,
		State:            d.State,
		Pool:             d.Pool,
		SegNo:            d.SegNo,
		Ctime:            d.Ctime,
		Replayed:         true,
		ModerationTaskId: taskID,
	}, nil
}

// validateContent 校验弹幕正文长度（按 rune，避免中文按字节误判）。
func validateContent(content string, maxLen int32) error {
	if content == "" {
		return model.ErrContentEmpty
	}
	if maxLen <= 0 {
		maxLen = 100
	}
	if int32(utf8.RuneCountInString(content)) > maxLen {
		return model.ErrContentTooLong
	}
	return nil
}

// normalizeDisplay 归一展示模式、字号与颜色。
func normalizeDisplay(mode rpc.DanmakuMode, fontsize, color int32) (int32, int32, int32) {
	m := int32(mode)
	if m < int32(rpc.DanmakuMode_MODE_SCROLL) || m > int32(rpc.DanmakuMode_MODE_ADVANCED) {
		m = int32(rpc.DanmakuMode_MODE_SCROLL)
	}
	if fontsize == 0 {
		fontsize = defaultFontsize
	}
	if fontsize < minFontsize || fontsize > maxFontsize {
		fontsize = defaultFontsize
	}
	if color == 0 {
		color = defaultColor
	}
	if color < 0 || color > 0xFFFFFF {
		color = defaultColor
	}
	return m, fontsize, color
}

// aidOf 缺省 aid 时按 oid 处理，保证归档投影列非空。
func aidOf(in *rpc.PostDanmakuReq) int64 {
	if in.Aid > 0 {
		return in.Aid
	}
	return in.Oid
}

// idempotencyKey 生成落库用的稳定幂等键。
// 键空间限定到 (oid, mid)，再取 SHA-256，避免不同用户选到同一个客户端键时冲突，
// 同时保持 danmaku.idempotency_key 上单列唯一索引可用。
func idempotencyKey(in *rpc.PostDanmakuReq) (string, error) {
	clientKey := in.IdempotencyKey
	if clientKey == "" {
		clientKey = in.ClientMsgId
	}
	if clientKey == "" {
		return "", model.ErrIdempotencyKeyRequired
	}
	if len(clientKey) > 64 {
		return "", model.ErrIdempotencyKeyRequired
	}
	return idempotency.NewKey(
		strconv.FormatInt(in.Oid, 10),
		strconv.FormatInt(in.Mid, 10),
		clientKey,
	).String(), nil
}
