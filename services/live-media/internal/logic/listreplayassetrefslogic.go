package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListReplayAssetRefsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListReplayAssetRefsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListReplayAssetRefsLogic {
	return &ListReplayAssetRefsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询回放资产引用（房间/场次/主播/投影状态）
//
// 只读方法：不写库、不发事件；bucket/object_key 只回引用，签名地址由 playback/live-gateway 生成。
// 过滤语义（与 model.ReplayRefFilter 一致）：room_id / live_session_id / anchor_mid 非正数不过滤。
// review_state 有一个必须如实保留的契约缺口：0（UNSPECIFIED）同时也是有列值意义的
// 「未同步」，所以本入口的 0 只能当「不过滤」（否则调用方无法区分两者）。
// 精确查未同步行要靠 OnlyUnsynced=true，那是投影补刷任务的内部开关，本 RPC 恒传 false；
// 越界取值（>5）仍按非法过滤拒绝，不退化成「恒空」。
// 分页见 listPage；排序固定 id DESC。
// 投影语义不得美化：review_state / review_state_at / published_at 都是 video 事实状态的
// 本地只读投影，可能滞后；review_state=0 表示尚未同步，调用方（live-gateway/运营）
// 绝不能把「未同步」当「未通过审核」——那会把可播的回放判成不可播。
// retention_state（0 正常/1 待回收/2 已回收）是引用行的生命周期，
// 2 表示产物已删但引用行留审计证据（AGENTS.md §8）。
func (l *ListReplayAssetRefsLogic) ListReplayAssetRefs(in *rpc.ListReplayAssetRefsReq) (*rpc.ListReplayAssetRefsReply, error) {
	cfg := l.svcCtx.Config.LiveMedia
	reviewState, err := filterState(int32(in.GetReviewState()), checkReviewState)
	if err != nil {
		return nil, err
	}
	pn, ps, err := listPage(cfg, in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.ReplayRefs.List(l.ctx, model.ReplayRefFilter{
		RoomId:      in.GetRoomId(),
		SessionId:   in.GetLiveSessionId(),
		AnchorMid:   in.GetAnchorMid(),
		ReviewState: reviewState,
		Pn:          pn,
		Ps:          ps,
		MaxPageSize: cfg.MaxListPageSize,
	})
	if err != nil {
		return nil, err
	}
	return &rpc.ListReplayAssetRefsReply{Page: pageResult(total), Refs: refInfos(rows)}, nil
}
