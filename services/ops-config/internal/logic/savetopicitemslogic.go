package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveTopicItemsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveTopicItemsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveTopicItemsLogic {
	return &SaveTopicItemsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 全量覆盖专题条目（一次事务，一组位置）
//
// 语义是**全量覆盖**而非增量：调用方必须回传完整期望集合。
// 「漏传一条」在增量语义下只是没加上，在全量语义下是把它从页面上删掉了 —— 后果相反，
// 所以这条必须写进后台交互文案，而不是藏在注释里。
func (l *SaveTopicItemsLogic) SaveTopicItems(in *rpc.SaveTopicItemsReq) (*rpc.SaveTopicItemsReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkWriteContext(in.GetCtx(), lim); err != nil {
		return nil, err
	}
	if err := checkReason(in.GetReason(), lim); err != nil {
		return nil, err
	}
	if in.GetTopicId() <= 0 {
		return nil, model.ErrTopicNotFound
	}
	m := &l.svcCtx.Models

	// 专题必须先存在：全量覆盖一个不存在的专题会「静默成功」，是最难查的运营事故。
	topic, err := m.Topic.FindByID(l.ctx, in.GetTopicId())
	if err != nil {
		return nil, err
	}
	if topic == nil {
		return nil, fmt.Errorf("%w: topic_id=%d", model.ErrTopicNotFound, in.GetTopicId())
	}

	items := topicItemRows(in.GetItems())
	if len(items) == 0 {
		// 清空请走 state=2 下架整个专题：空数组被误传的概率远高于「真的要清空」的意图。
		return nil, model.ErrBatchEmpty
	}
	if len(items) > lim.topicMaxItems {
		return nil, fmt.Errorf("%w: %d > %d", model.ErrTopicItemLimit, len(items), lim.topicMaxItems)
	}
	// logic 只把「每条都必须是合法引用」这层挡住（错误信息能指到第几条），
	// position 是否 1..n 连续、条数上限这些判定留在 model.ReplaceAll 的同一事务里再判一次：
	// 两侧共用一套规则，不会出现「logic 放过、model 拒」的口径分叉。
	for i, it := range items {
		if !model.ValidItemType(it.ItemType, false) {
			return nil, fmt.Errorf("items[%d]: %w: item_type=%s", i, model.ErrItemTypeUnsupported, it.ItemType)
		}
		if strings.TrimSpace(it.ItemID) == "" {
			return nil, fmt.Errorf("items[%d]: %w", i, model.ErrItemRefRequired)
		}
		it.TopicID = topic.TopicID
		// 条目状态由服务端统一置 ON：本接口是全量覆盖，
		// 允许调用方混传停用行只会让「为什么这条没出」变成两可。
		it.State = model.StateOn
	}

	ts := model.NowUnix()
	// 先记旧条数：审计的 before 必须是「覆盖前」的状态，ReplaceAll 之后就查不到了。
	before, _ := m.TopicItem.CountByTopic(l.ctx, topic.TopicID, 0)
	affected, err := m.TopicItem.ReplaceAll(l.ctx, topic.TopicID, items, in.GetCtx().GetOperatorId(), ts, lim.topicMaxItems)
	if err != nil {
		return nil, err
	}

	// TouchMtime 给「按 mtime 增量拉取」的调用方一个信号：主记录没变，但内容变了。
	// 失败只记日志：条目已经整体换成新集合，为此回滚这次写入比少一次 mtime 更新更糟。
	if _, terr := m.Topic.TouchMtime(l.ctx, topic.TopicID, ts); terr != nil {
		l.Errorf("ops-config/topic: topic_id=%d 的 mtime 未更新，增量拉取方会晚一拍看到新条目: %v", topic.TopicID, terr)
	}
	cacheDel(l.ctx, l.svcCtx.Cache, lim.topicKeyLocators(topic), l.Logger)

	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "save_topic_items",
		"ops_config:topic", strconv.FormatInt(topic.TopicID, 10),
		"items_before="+strconv.FormatInt(before, 10),
		fmt.Sprintf("items_after=%d,affected=%d", len(items), affected), in.GetReason(), ts)

	return &rpc.SaveTopicItemsReply{
		TopicId:      topic.TopicID,
		Total:        int32(len(items)),
		AuditEntryId: entryID,
	}, nil
}
