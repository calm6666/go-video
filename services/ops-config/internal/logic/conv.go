// 本文件是 logic 包的手写扩展（rpc ↔ model 投影），不是 goctl 生成产物。
//
// AGENTS.md §4/§6：领域服务不把数据库原始对象直接吐给调用方。投影只做三件事：
//  1. 搬运事实字段（值、版本号、时间戳、操作人）；
//  2. 把库里的**存储形态**还原成契约里的数组/枚举（",1,4," → []ClientPlatform）；
//  3. 不在此处做任何业务判定（灰度、CAS、幂等裁决都在 logic 的用例函数里）。
//
// 商业化范围外（AGENTS.md §1）：本文件不存在任何投放、出价、分成、会员相关投影。
// 终端范围（AGENTS.md §6）：枚举只在 1..4 之间有意义，0（UNSPECIFIED）不是合法端。

package logic

import (
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

// platformEnumList 把库里的 int32 端标识转成 proto 枚举数组。
// 越界值由 model 的 PlatformList() 在还原阶段就已丢弃，这里只做类型搬运，
// 不额外补一个「未知端」的枚举值——协议里没有第五端（不支持小程序）。
func platformEnumList(in []int32) []rpc.ClientPlatform {
	if len(in) == 0 {
		return nil
	}
	out := make([]rpc.ClientPlatform, 0, len(in))
	for _, p := range in {
		out = append(out, rpc.ClientPlatform(p))
	}
	return out
}

// platformInt32List 是反方向：契约枚举 → model 的归一化入参。
func platformInt32List(in []rpc.ClientPlatform) []int32 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int32, 0, len(in))
	for _, p := range in {
		out = append(out, int32(p))
	}
	return out
}

func configItemInfo(item *model.ConfigItem) *rpc.ConfigItem {
	if item == nil {
		return nil
	}
	return &rpc.ConfigItem{
		ConfigId:      item.ConfigID,
		CfgKey:        item.CfgKey,
		Scope:         item.Scope,
		ValueType:     rpc.ConfigValueType(item.ValueType),
		Title:         item.Title,
		Description:   item.Description,
		State:         item.State,
		LatestVersion: item.LatestVersion,
		Epoch:         item.Epoch,
		OperatorId:    item.OperatorID,
		Ctime:         item.Ctime,
		Mtime:         item.Mtime,
	}
}

func configItemList(rows []*model.ConfigItem) []*rpc.ConfigItem {
	out := make([]*rpc.ConfigItem, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, configItemInfo(r))
	}
	return out
}

func configVersionInfo(v *model.ConfigVersion) *rpc.ConfigVersion {
	if v == nil {
		return nil
	}
	return &rpc.ConfigVersion{
		VersionId:    v.VersionID,
		ConfigId:     v.ConfigID,
		Version:      v.Version,
		Value:        v.Value,
		ValueType:    rpc.ConfigValueType(v.ValueType),
		ChangeType:   v.ChangeType,
		RollbackFrom: v.RollbackFrom,
		OperatorId:   v.OperatorID,
		OperatorName: v.OperatorName,
		Reason:       v.Reason,
		// AuditEntryId 原样回：0 表示审计待补偿，这是一个**可见的缺口**，
		// 不是需要被投影藏起来的空值。
		AuditEntryId: v.AuditEntryID,
		RequestId:    v.RequestID,
		PublishedAt:  v.PublishedAt,
		Ctime:        v.Ctime,
	}
}

func configVersionList(rows []*model.ConfigVersion) []*rpc.ConfigVersion {
	out := make([]*rpc.ConfigVersion, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, configVersionInfo(r))
	}
	return out
}

func rolloutRuleInfo(r *model.RolloutRule) *rpc.RolloutRule {
	if r == nil {
		return nil
	}
	return &rpc.RolloutRule{
		RuleId:        r.RuleID,
		ConfigId:      r.ConfigID,
		Version:       r.Version,
		Name:          r.Name,
		Mode:          rpc.RolloutMode(r.Mode),
		Percentage:    r.Percentage,
		AppVersionMin: r.AppVersionMin,
		AppVersionMax: r.AppVersionMax,
		Platforms:     platformEnumList(r.PlatformList()),
		MidSuffixes:   r.MidSuffixes,
		WhitelistMids: r.WhitelistList(),
		Priority:      r.Priority,
		State:         r.State,
		OperatorId:    r.OperatorID,
		Remark:        r.Remark,
		StartAt:       r.StartAt,
		EndAt:         r.EndAt,
		Ctime:         r.Ctime,
		Mtime:         r.Mtime,
	}
}

func rolloutRuleList(rows []*model.RolloutRule) []*rpc.RolloutRule {
	out := make([]*rpc.RolloutRule, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, rolloutRuleInfo(r))
	}
	return out
}

func topicInfo(t *model.Topic) *rpc.Topic {
	if t == nil {
		return nil
	}
	return &rpc.Topic{
		TopicId:     t.TopicID,
		Slug:        t.Slug,
		Title:       t.Title,
		Description: t.Description,
		Cover:       t.Cover,
		// ZoneIds/TagIds 只回引用 ID：本服务不复制 catalog 的分区名/标签名（AGENTS.md §5）。
		ZoneIds:    t.ZoneIDList(),
		TagIds:     t.TagIDList(),
		State:      t.State,
		Sort:       t.Sort,
		StartAt:    t.StartAt,
		EndAt:      t.EndAt,
		Version:    t.Version,
		OperatorId: t.OperatorID,
		Ctime:      t.Ctime,
		Mtime:      t.Mtime,
	}
}

func topicList(rows []*model.Topic) []*rpc.Topic {
	out := make([]*rpc.Topic, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, topicInfo(r))
	}
	return out
}

func topicItemInfo(it *model.TopicItem) *rpc.TopicItem {
	if it == nil {
		return nil
	}
	return &rpc.TopicItem{
		Id:         it.ID,
		TopicId:    it.TopicID,
		ItemType:   it.ItemType,
		ItemId:     it.ItemID,
		Position:   it.Position,
		State:      it.State,
		OperatorId: it.OperatorID,
		Ctime:      it.Ctime,
		Mtime:      it.Mtime,
	}
}

func topicItemList(rows []*model.TopicItem) []*rpc.TopicItem {
	out := make([]*rpc.TopicItem, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, topicItemInfo(r))
	}
	return out
}

func slotInfo(s *model.RecommendSlot) *rpc.RecommendSlot {
	if s == nil {
		return nil
	}
	return &rpc.RecommendSlot{
		SlotId:     s.SlotID,
		Code:       s.Code,
		Page:       s.Page,
		Title:      s.Title,
		Platforms:  platformEnumList(s.PlatformList()),
		Capacity:   s.Capacity,
		State:      s.State,
		Version:    s.Version,
		OperatorId: s.OperatorID,
		Remark:     s.Remark,
		Ctime:      s.Ctime,
		Mtime:      s.Mtime,
	}
}

func slotList(rows []*model.RecommendSlot) []*rpc.RecommendSlot {
	out := make([]*rpc.RecommendSlot, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, slotInfo(r))
	}
	return out
}

func slotItemInfo(it *model.SlotItem) *rpc.SlotItem {
	if it == nil {
		return nil
	}
	return &rpc.SlotItem{
		Id:         it.ID,
		SlotId:     it.SlotID,
		Position:   it.Position,
		ItemType:   it.ItemType,
		ItemId:     it.ItemID,
		Weight:     it.Weight,
		StartAt:    it.StartAt,
		EndAt:      it.EndAt,
		State:      it.State,
		OperatorId: it.OperatorID,
		Ctime:      it.Ctime,
		Mtime:      it.Mtime,
	}
}

func slotItemList(rows []*model.SlotItem) []*rpc.SlotItem {
	out := make([]*rpc.SlotItem, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, slotItemInfo(r))
	}
	return out
}

func switchInfo(s *model.ClientSwitch) *rpc.ClientSwitch {
	if s == nil {
		return nil
	}
	return &rpc.ClientSwitch{
		SwitchId:   s.SwitchID,
		SwitchKey:  s.SwitchKey,
		Platform:   rpc.ClientPlatform(s.Platform),
		MinVersion: s.MinVersion,
		MaxVersion: s.MaxVersion,
		Enabled:    s.Enabled,
		ConfigId:   s.ConfigID,
		OperatorId: s.OperatorID,
		Remark:     s.Remark,
		Version:    s.Version,
		Ctime:      s.Ctime,
		Mtime:      s.Mtime,
	}
}

func switchList(rows []*model.ClientSwitch) []*rpc.ClientSwitch {
	out := make([]*rpc.ClientSwitch, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, switchInfo(r))
	}
	return out
}

// configTopicItems / configSlotItems 把契约条目转成 model 行（全量覆盖用）。
// position 的连续性、item_type 值域等规则由 model 裁决，这里不重复实现，
// 否则两侧判定会出现「logic 放过、model 拒」的口径分叉。
func topicItemRows(in []*rpc.TopicItem) []*model.TopicItem {
	out := make([]*model.TopicItem, 0, len(in))
	for _, it := range in {
		if it == nil {
			continue
		}
		out = append(out, &model.TopicItem{
			ID:       it.GetId(),
			TopicID:  it.GetTopicId(),
			ItemType: it.GetItemType(),
			ItemID:   it.GetItemId(),
			Position: it.GetPosition(),
			State:    it.GetState(),
		})
	}
	return out
}

func slotItemRows(in []*rpc.SlotItem) []*model.SlotItem {
	out := make([]*model.SlotItem, 0, len(in))
	for _, it := range in {
		if it == nil {
			continue
		}
		out = append(out, &model.SlotItem{
			ID:       it.GetId(),
			SlotID:   it.GetSlotId(),
			Position: it.GetPosition(),
			ItemType: it.GetItemType(),
			ItemID:   it.GetItemId(),
			Weight:   it.GetWeight(),
			StartAt:  it.GetStartAt(),
			EndAt:    it.GetEndAt(),
			State:    it.GetState(),
		})
	}
	return out
}
