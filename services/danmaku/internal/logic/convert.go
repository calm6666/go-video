package logic

// 本文件是手写领域转换层（不是 goctl 生成产物）：
// 只负责 model ↔ rpc 的字段映射，不含业务规则。

import (
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

// danmakuToRPC 把弹幕行投影为 RPC 结构。
func danmakuToRPC(d *model.Danmaku) *rpc.DanmakuInfo {
	if d == nil {
		return nil
	}
	return &rpc.DanmakuInfo{
		Dmid:       d.Dmid,
		Oid:        d.Oid,
		Aid:        d.Aid,
		Mid:        d.Mid,
		ProgressMs: d.ProgressMs,
		Mode:       d.Mode,
		Fontsize:   d.Fontsize,
		Color:      d.Color,
		Content:    d.Content,
		State:      d.State,
		Pool:       d.Pool,
		SegNo:      d.SegNo,
		Ctime:      d.Ctime,
		Mtime:      d.Mtime,
	}
}

// danmakuListToRPC 批量转换，跳过空指针。
func danmakuListToRPC(rows []*model.Danmaku) []*rpc.DanmakuInfo {
	out := make([]*rpc.DanmakuInfo, 0, len(rows))
	for _, d := range rows {
		if d == nil {
			continue
		}
		out = append(out, danmakuToRPC(d))
	}
	return out
}

// blockWordToRPC 屏蔽词条目转换。
func blockWordToRPC(w *model.BlockWord) *rpc.BlockWordInfo {
	if w == nil {
		return nil
	}
	return &rpc.BlockWordInfo{
		WordId:   w.WordID,
		Word:     w.Word,
		Scope:    w.Scope,
		Oid:      w.Oid,
		State:    w.State,
		Operator: w.Operator,
		Ctime:    w.Ctime,
		Mtime:    w.Mtime,
	}
}

// userBlockToRPC 用户屏蔽项转换。
func userBlockToRPC(b *model.UserBlock) *rpc.UserBlockInfo {
	if b == nil {
		return nil
	}
	return &rpc.UserBlockInfo{
		Id:         b.ID,
		Mid:        b.Mid,
		Type:       b.Type,
		BlockedMid: b.BlockedMid,
		Keyword:    b.Keyword,
		State:      b.State,
		Ctime:      b.Ctime,
		Mtime:      b.Mtime,
	}
}
