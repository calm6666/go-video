// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"time"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetUserInterestLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetUserInterestLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUserInterestLogic {
	return &GetUserInterestLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 用户兴趣画像（脱敏权重，不返回行为明细）
func (l *GetUserInterestLogic) GetUserInterest(in *rpc.GetUserInterestReq) (*rpc.GetUserInterestReply, error) {
	// 逻辑轮规划：校验 mid>0、top_n<=MaxInterestTopN(100) -> 解析 ACTIVE 兴趣口径 -> 读 spm_user_interest（uniq_interest = mid+metric_version+interest_key）按 weight 倒序取前 N -> 画像内最大的 event_time 距今超过 Spm.InterestStaleAfterSeconds 时置 stale=true，让召回侧走冷启动而不是拿旧画像当实时兴趣。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "GetUserInterest")
	if err != nil {
		return nil, err
	}
	defer done()

	if in.GetMid() <= 0 {
		return nil, fmt.Errorf("%w: mid=%d", model.ErrInvalidMid, in.GetMid())
	}
	cfg := l.svcCtx.Config.Spm
	topN := in.GetTopN()
	if topN <= 0 {
		topN = cfg.InterestTopN
	}
	if topN > cfg.MaxInterestTopN {
		return nil, fmt.Errorf("%w: top_n=%d > %d", model.ErrTopNTooLarge, topN,
			cfg.MaxInterestTopN)
	}
	// metric_version=0 无法解析：spm_user_interest 只有 (mid, metric_version, interest_key)，
	// 没有 metric_key 列，也就没有「该画像的 ACTIVE 口径指针」可查（见 README「契约缺口」）。
	// 猜一个键名去查注册表会凭空造出口径语义，因此这里显式拒绝而不是给一个看起来能用的版本。
	version := in.GetMetricVersion()
	if version <= 0 {
		return nil, fmt.Errorf("%w: GetUserInterest 必须显式给定兴趣口径版本"+
			"（画像表无 metric_key，无法解析 ACTIVE 指针）", model.ErrMetricVersionRequired)
	}

	rows, err := l.svcCtx.Interests.ListTop(l.ctx, in.GetMid(), version, topN)
	if err != nil {
		return nil, err
	}
	interests := make([]*rpc.GetUserInterestReply_Interest, 0, len(rows))
	var lastEvent int64
	for _, r := range rows {
		if !model.ValidInterestKey(r.InterestKey) {
			// 脏键（写侧本已拒绝）不回传：自由文本一旦进响应，就是行为明细外泄的口子。
			l.Errorf("spm/GetUserInterest: 丢弃非法兴趣键 mid=%d version=%d key=%q",
				in.GetMid(), version, r.InterestKey)
			continue
		}
		if r.EventTime > lastEvent {
			lastEvent = r.EventTime
		}
		interests = append(interests, &rpc.GetUserInterestReply_Interest{
			InterestKey: r.InterestKey,
			Weight:      r.Weight,
			SampleCount: r.SampleCount,
			EventTime:   r.EventTime,
		})
	}

	// 空画像同样算 stale：「没有该版本的重算结果」与「有一份很旧的画像」对调用方是同一个动作
	// （走冷启动），而 stale=false + 空列表会被读成「这个人确实没有任何兴趣」。
	stale := lastEvent == 0 ||
		time.Now().Unix()-lastEvent > cfg.InterestStaleAfterSeconds
	return &rpc.GetUserInterestReply{
		Interests:     interests,
		MetricVersion: version,
		Stale:         stale,
	}, nil
}
