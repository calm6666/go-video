package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveTopicLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveTopicLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveTopicLogic {
	return &SaveTopicLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 保存专题定义（不含条目）
//
// 一次请求只改一件事：本方法只动专题主记录，条目全量覆盖是 SaveTopicItems 的职责。
// 后台因此可以把「改标题」与「换内容」授给不同权限点，审计也能分辨是哪一类动作。
func (l *SaveTopicLogic) SaveTopic(in *rpc.SaveTopicReq) (*rpc.SaveTopicReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkWriteContext(in.GetCtx(), lim); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.GetTitle()) == "" {
		return nil, model.ErrTopicTitleRequired
	}
	if in.GetEndAt() > 0 && in.GetEndAt() <= in.GetStartAt() {
		return nil, model.ErrTopicTimeRangeInvalid
	}
	state := in.GetState()
	if state == 0 {
		// 新建即上线会让一个还没挂条目的空专题被端上取到。
		state = model.StateOff
	}
	if err := checkState(state); err != nil {
		return nil, err
	}
	zoneIDs, err := refIDList(in.GetZoneIds(), lim, "zone_ids")
	if err != nil {
		return nil, err
	}
	tagIDs, err := refIDList(in.GetTagIds(), lim, "tag_ids")
	if err != nil {
		return nil, err
	}
	m := &l.svcCtx.Models
	ts := model.NowUnix()

	// 新建：slug 是对外寻址句柄，必须有且必须合规。
	if in.GetTopicId() == 0 {
		slug := strings.TrimSpace(in.GetSlug())
		if slug == "" {
			return nil, model.ErrTopicSlugRequired
		}
		if !model.ValidTopicSlug(slug) {
			return nil, model.ErrTopicSlugInvalid
		}
		topic := &model.Topic{
			Slug:        slug,
			Title:       in.GetTitle(),
			Description: in.GetDescription(),
			Cover:       in.GetCover(),
			ZoneIDs:     zoneIDs,
			TagIDs:      tagIDs,
			State:       state,
			Sort:        in.GetSort(),
			StartAt:     in.GetStartAt(),
			EndAt:       in.GetEndAt(),
			OperatorID:  in.GetCtx().GetOperatorId(),
			Ctime:       ts,
		}
		id, ierr := m.Topic.Insert(l.ctx, topic)
		if ierr != nil {
			if errors.Is(ierr, model.ErrTopicSlugConflict) {
				return nil, fmt.Errorf("%w: slug=%s", model.ErrTopicSlugConflict, slug)
			}
			return nil, ierr
		}
		topic.TopicID = id
		// 新建没有旧投影可删，但同名 slug 可能曾被别的专题用过（改 slug 后）——照删不误。
		cacheDel(l.ctx, l.svcCtx.Cache, lim.topicKeyLocators(topic), l.Logger)
		entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "create_topic",
			"ops_config:topic", strconv.FormatInt(id, 10), "", topicDigest(topic), in.GetReason(), ts)
		return &rpc.SaveTopicReply{Topic: topicInfo(topic), AuditEntryId: entryID}, nil
	}

	cur, err := m.Topic.FindByID(l.ctx, in.GetTopicId())
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("%w: topic_id=%d", model.ErrTopicNotFound, in.GetTopicId())
	}
	slug := strings.TrimSpace(in.GetSlug())
	if slug == "" {
		// 更新时留空表示「不改寻址句柄」：端上的专题链接是按 slug 拼的，
		// 一次无意的空值就改掉 slug 等于把所有历史入口打失效。
		slug = cur.Slug
	}
	if !model.ValidTopicSlug(slug) {
		return nil, model.ErrTopicSlugInvalid
	}
	if in.GetExpectVersion() < 0 || in.GetExpectVersion() > cur.Version {
		return nil, fmt.Errorf("%w: topic_id=%d expect=%d current=%d",
			model.ErrVersionConflict, cur.TopicID, in.GetExpectVersion(), cur.Version)
	}

	next := &model.Topic{
		TopicID:     cur.TopicID,
		Slug:        slug,
		Title:       in.GetTitle(),
		Description: in.GetDescription(),
		Cover:       in.GetCover(),
		ZoneIDs:     zoneIDs,
		TagIDs:      tagIDs,
		State:       state,
		Sort:        in.GetSort(),
		StartAt:     in.GetStartAt(),
		EndAt:       in.GetEndAt(),
		Version:     cur.Version,
		OperatorID:  in.GetCtx().GetOperatorId(),
		Ctime:       cur.Ctime,
	}
	ok, uerr := m.Topic.UpdateWithVersion(l.ctx, next, in.GetExpectVersion(), ts)
	if uerr != nil {
		return nil, uerr
	}
	if !ok {
		return nil, fmt.Errorf("%w: topic_id=%d expect=%d current=%d",
			model.ErrVersionConflict, cur.TopicID, in.GetExpectVersion(), cur.Version)
	}

	// 两份键都要删：端上按 slug 寻址、后台按 ID 操作，漏一份就是「改了但没全改」。
	var staleKeys []string
	staleKeys = append(staleKeys, lim.topicKeyLocators(cur)...)
	staleKeys = append(staleKeys, lim.topicKeyLocators(next)...)
	cacheDel(l.ctx, l.svcCtx.Cache, dedupeStrings(staleKeys), l.Logger)

	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "save_topic",
		"ops_config:topic", strconv.FormatInt(cur.TopicID, 10),
		"version="+strconv.FormatInt(cur.Version, 10)+",state="+strconv.Itoa(int(cur.State))+",slug="+cur.Slug,
		topicDigest(next), in.GetReason(), ts)

	return &rpc.SaveTopicReply{Topic: topicInfo(next), AuditEntryId: entryID}, nil
}

// refIDList 把契约里的引用 ID 数组归一成存储串。
// 这里**只存 catalog 的数字 ID**，绝不写分区名/标签名（AGENTS.md §5：跨服务只传 ID）。
// 因此本服务无法在写期校验「引用的分区是否真的存在」——已知缺口，见 README。
func refIDList(ids []int64, lim limits, field string) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	if len(ids) > lim.topicMaxRefIDs {
		return "", fmt.Errorf("%w: %s 最多 %d 个", model.ErrBatchTooLarge, field, lim.topicMaxRefIDs)
	}
	ok, err := model.ValidateIDList(ids, lim.topicMaxRefIDs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	s := model.IDListString(ok)
	if len(s) > model.IDListMaxLen {
		return "", model.ErrTopicIDListTooLong
	}
	return s, nil
}

func topicDigest(t *model.Topic) string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("slug=%s,state=%d,sort=%d,version=%d,zones=%d,tags=%d,window=%d/%d",
		t.Slug, t.State, t.Sort, t.Version, len(t.ZoneIDList()), len(t.TagIDList()), t.StartAt, t.EndAt)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
