package logic

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTopicLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTopicLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTopicLogic {
	return &GetTopicLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取专题定义（可选带条目）
//
// 缓存边界：只缓存**专题主记录**，条目永远回源。
// 条目是排期数据（position + state + 引用），缓存它会让「刚换完内容」在一个 TTL 内不可见，
// 而换内容正是运营最高频的动作；主记录改动少、被列表页反复读，值得缓存。
func (l *GetTopicLogic) GetTopic(in *rpc.GetTopicReq) (*rpc.GetTopicReply, error) {
	lim := newLimits(l.svcCtx.Config)
	topicID := in.GetTopicId()
	slug := strings.TrimSpace(in.GetSlug())
	if topicID <= 0 && slug == "" {
		// 两个定位符都不给是「定位条件缺失」，语义与查不到一致，因此复用 ErrTopicNotFound
		// 而不是 ErrRequestIDRequired（后者说的是身份，不是定位）。
		return nil, model.ErrTopicNotFound
	}
	m := &l.svcCtx.Models

	// 两者都给以 topic_id 为准：同一请求回两份视图总有不一致的时刻。
	var (
		topic *model.Topic
		err   error
	)
	if topicID > 0 {
		key := lim.topicKey("id:" + strconv.FormatInt(topicID, 10))
		topic, err = l.loadTopic(key, func() (*model.Topic, error) { return m.Topic.FindByID(l.ctx, topicID) }, lim)
	} else {
		key := lim.topicKey("slug:" + slug)
		topic, err = l.loadTopic(key, func() (*model.Topic, error) { return m.Topic.FindBySlug(l.ctx, slug) }, lim)
	}
	if err != nil {
		return nil, err
	}
	if topic == nil {
		// 端上按 slug 寻址时「专题不存在/已下架」是正常结果，不走 gRPC NotFound，
		// 否则每个过期入口都会变成一条错误日志。回一个短 TTL 让入口较快自愈。
		return &rpc.GetTopicReply{Found: false, Ttl: lim.clampTTL(itemPointerTTLSeconds)}, nil
	}

	// 下架专题回 ttl=0（不许缓存）：撤下的入口被继续缓存，用户点进去就是空页，
	// 而且没有任何日志会指向这条缓存。
	ttl := lim.clampTTL(lim.topicTTL)
	if topic.State != model.StateOn {
		ttl = 0
	}
	reply := &rpc.GetTopicReply{Topic: topicInfo(topic), Found: true, Ttl: ttl}
	if !in.GetWithItems() {
		return reply, nil
	}

	limit := lim.topicItemLimit
	if in.GetItemLimit() > 0 {
		limit = int(in.GetItemLimit())
		if limit > lim.topicMaxItems {
			limit = lim.topicMaxItems
		}
	}
	items, err := m.TopicItem.ListByTopic(l.ctx, topic.TopicID, model.StateOn, limit)
	if err != nil {
		return nil, err
	}
	reply.Items = topicItemList(items)
	return reply, nil
}

// loadTopic 读一份专题投影：命中即回，未命中回源并回填。
//
// 两个刻意的行为边界：
//  1. **不缓存 miss** —— 负缓存会让刚建的专题在 TTL 内一直显示「不存在」，
//     那个现象看起来完全像「保存没生效」，是最难排查的一类反馈；
//  2. 缓存侧任何错误都按未命中处理并记 Error 日志 —— Redis 只是可整域重建的投影，
//     判定必须以 MySQL 为准（见 internal/svc 的 CacheKV 注释）。
func (l *GetTopicLogic) loadTopic(key string, fetch func() (*model.Topic, error), lim limits) (*model.Topic, error) {
	if l.svcCtx.Cache != nil && lim.topicTTL > 0 {
		raw, cerr := l.svcCtx.Cache.Get(l.ctx, key)
		if cerr != nil {
			l.Errorf("ops-config/cache: 读 %s 失败，按未命中回源: %v", key, cerr)
		} else if raw != "" {
			var row model.Topic
			if uerr := json.Unmarshal([]byte(raw), &row); uerr == nil {
				return &row, nil
			} else {
				l.Errorf("ops-config/cache: 解析 %s 失败，按未命中回源: %v", key, uerr)
			}
		}
	}
	row, err := fetch()
	if err != nil || row == nil || l.svcCtx.Cache == nil || lim.topicTTL <= 0 {
		return row, err
	}
	if bs, merr := json.Marshal(row); merr == nil {
		if serr := l.svcCtx.Cache.Setex(l.ctx, key, string(bs), lim.topicTTL); serr != nil {
			l.Errorf("ops-config/cache: 回填 %s 失败（本次响应仍正确）: %v", key, serr)
		}
	}
	return row, nil
}
