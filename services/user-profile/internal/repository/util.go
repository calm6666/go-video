package repository

// 本文件提供仓库层的通用工具函数与日志查询聚合。

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// uuid4 生成 UUID v4 字符串（参考 model.UUID4，使用 crypto/rand）。
func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 理论上不可达；降级为时间戳字符串保证可用性
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// formatInt 格式化整数为十进制字符串。
func formatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

// memberLogs 查询指定类型的用户变更日志并转换为 RPC 结构。
func (r *Repository) memberLogs(ctx context.Context, logType int8, mid int64) ([]*rpc.UserLogReply, error) {
	logs, err := r.logModel.FindByMid(ctx, logType, mid)
	if err != nil {
		return nil, err
	}
	reply := make([]*rpc.UserLogReply, 0, len(logs))
	for _, l := range logs {
		if l == nil {
			continue
		}
		reply = append(reply, &rpc.UserLogReply{
			Mid:     l.Mid,
			Ip:      l.IP,
			Ts:      l.TS,
			LogId:   l.LogID,
			Content: l.Content,
		})
	}
	return reply, nil
}

// toMoralReply 把节操实体转换为 RPC 结构。
func toMoralReply(m *model.UserMoral) *rpc.MoralReply {
	if m == nil {
		return nil
	}
	return &rpc.MoralReply{
		Mid:             m.Mid,
		Moral:           m.Moral,
		Added:           m.Added,
		Deducted:        m.Deducted,
		LastRecoverDate: m.LastRecoverDate,
	}
}

// facePath 提取头像 URL 的路径部分（参考 service.path：解析失败返回空）。
func facePath(faceURL string) string {
	u, err := url.Parse(faceURL)
	if err != nil {
		return ""
	}
	return u.Path
}
