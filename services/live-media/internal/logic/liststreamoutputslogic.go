package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListStreamOutputsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListStreamOutputsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListStreamOutputsLogic {
	return &ListStreamOutputsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询房间当前可分发档位（live-gateway / live-room 只读投影）
//
// 只读方法：不写库、不发事件。room_id 必填（ErrInvalidRoomID）——本方法不提供
// 「全局列档位」入口，避免被误用成扫全表的运营接口。
// 语义（与 model.ListByRoom 一致）：live_session_id<=0 且 include_offline=false 时
// 只返回当前在线档位（播放器选档的默认口径）；include_offline=true 才带出历史下线行
// （state=2，offline_reason 有效，供排障）。
// 排序固定 bitrate_level ASC, protocol ASC（档位顺序稳定，客户端不必自己排）；
// 分页见 listPage（ps 越界夹取，pn 超深翻页窗口拒绝）。
// 缓存：以 (room_id, session, include_offline, pn, ps) + 房间代际号为键，
// TTL=StreamOutputCacheTTLSeconds（直播档位变化快，默认 15 秒）；写侧 Upsert/Offline
// 递增代际键让旧列表自然失效，无需 SCAN 键空间。缓存只加速读，
// live_stream_output 才是事实源；Cache 未配置或 TTL=0 时全部回源 MySQL。
// 输出只含引用（bucket/object_key/cdn_domain）：短期播放地址由 live-gateway 侧签名生成，
// 本服务永不下发可长期使用的公网地址（AGENTS.md §6）。
func (l *ListStreamOutputsLogic) ListStreamOutputs(in *rpc.ListStreamOutputsReq) (*rpc.ListStreamOutputsReply, error) {
	cfg := l.svcCtx.Config.LiveMedia
	roomID := in.GetRoomId()
	if err := checkRoomID(roomID); err != nil {
		return nil, err
	}
	pn, ps, err := listPage(cfg, in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}
	sessionID := in.GetLiveSessionId()
	includeOffline := in.GetIncludeOffline()

	cacheKey := ""
	if cfg.StreamOutputCacheTTLSeconds > 0 {
		cacheKey = outputListCacheKey(roomID, sessionID, includeOffline, pn, ps,
			readOutputGen(l.ctx, l.svcCtx, roomID))
		cached := &rpc.ListStreamOutputsReply{}
		// Page 恒由本方法写入，缺失说明这条缓存不是本口径写的：宁缺不滥，回源。
		if readCachedInfo(l.ctx, l.svcCtx, cacheKey, cached) && cached.GetPage() != nil {
			return cached, nil
		}
	}

	rows, total, err := l.svcCtx.StreamOutputs.ListByRoom(l.ctx, roomID, sessionID, includeOffline,
		pn, ps, cfg.MaxListPageSize)
	if err != nil {
		return nil, err
	}
	reply := &rpc.ListStreamOutputsReply{Page: pageResult(total), Outputs: outputInfos(rows)}
	writeCachedInfo(l.ctx, l.svcCtx, cacheKey, cfg.StreamOutputCacheTTLSeconds, reply)
	return reply, nil
}
