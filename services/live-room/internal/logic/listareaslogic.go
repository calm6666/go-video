package logic

import (
	"context"
	"encoding/json"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAreasLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAreasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAreasLogic {
	return &ListAreasLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分区列表（客户端与运营共用）
//
// -1 才是「不过滤」，0 是合法取值（只取一级分区 / 停用分区），
// 所以这里绝不做「为 0 就丢掉条件」的归一。
// 停用分区同样返回：本服务不替调用方藏数据，客户端选区时自己按 state 过滤。
func (l *ListAreasLogic) ListAreas(in *rpc.ListAreasReq) (*rpc.ListAreasReply, error) {
	if in == nil {
		return &rpc.ListAreasReply{}, nil
	}
	if in.GetParentAreaId() < areaCacheNoFilter {
		return nil, model.ErrInvalidAreaID
	}
	if in.GetState() < areaCacheNoFilter || in.GetState() > model.AreaStateEnabled {
		return nil, model.ErrAreaStateInvalid
	}
	size, err := l.svcCtx.AreaPageSize(in.GetPageSize())
	if err != nil {
		return nil, err
	}
	page, err := clampPage(in.GetPage())
	if err != nil {
		return nil, err
	}
	ttl := l.svcCtx.Config.LiveRoom.AreaListCacheTTLSeconds
	cacheKey := areaListCacheKey(in.GetParentAreaId(), in.GetState(), int32(page), int32(size))
	if ttl > 0 && l.svcCtx.Cache != nil {
		if reply := l.cachedAreas(cacheKey); reply != nil {
			return reply, nil
		}
	}
	q := model.AreaListQuery{
		ParentAreaID: in.GetParentAreaId(),
		State:        in.GetState(),
		Offset:       pageOffset(page, size),
		Limit:        int32(size),
	}
	rows, err := l.svcCtx.Areas.List(l.ctx, q)
	if err != nil {
		return nil, err
	}
	total, err := l.svcCtx.Areas.Count(l.ctx, q)
	if err != nil {
		return nil, err
	}
	reply := &rpc.ListAreasReply{Areas: areaInfoList(rows), Total: clampTotal(total)}
	if ttl > 0 && l.svcCtx.Cache != nil {
		l.putCachedAreas(cacheKey, reply, ttl)
	}
	return reply, nil
}

// cachedAreas 读分区列表缓存；任何异常（未配置、脏数据）都返回 nil 回源。
func (l *ListAreasLogic) cachedAreas(key string) *rpc.ListAreasReply {
	raw, err := l.svcCtx.Cache.GetCtx(l.ctx, key)
	if err != nil || raw == "" {
		return nil
	}
	reply := &rpc.ListAreasReply{}
	if err := json.Unmarshal([]byte(raw), reply); err != nil {
		l.Errorf("liveroom: bad area list cache %s: %v", key, err)
		return nil
	}
	return reply
}

func (l *ListAreasLogic) putCachedAreas(key string, reply *rpc.ListAreasReply, ttl int) {
	raw, err := json.Marshal(reply)
	if err != nil {
		return
	}
	if err := l.svcCtx.Cache.SetexCtx(l.ctx, key, string(raw), ttl); err != nil {
		l.Errorf("liveroom: set area list cache %s: %v", key, err)
	}
}
