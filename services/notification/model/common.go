package model

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// 通道编码：与 rpc/notification.proto 的 Channel 枚举严格一致，
// 落库为 TINYINT，其它服务读取 notification_delivery.channel 时不需要再映射。
const (
	// ChannelPush 应用推送。
	ChannelPush int32 = 1
	// ChannelSMS 短信。
	ChannelSMS int32 = 2
	// ChannelEmail 邮件。
	ChannelEmail int32 = 3
)

// ChannelName 返回通道的稳定字符串名（provider 与事件 payload 使用）。
// 未知通道返回空串，调用方需自行拒绝。
func ChannelName(channel int32) string {
	switch channel {
	case ChannelPush:
		return "push"
	case ChannelSMS:
		return "sms"
	case ChannelEmail:
		return "email"
	default:
		return ""
	}
}

// ChannelCodeOf 把通道字符串名转成编码，未知返回 0。
func ChannelCodeOf(name string) int32 {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "push":
		return ChannelPush
	case "sms", "mobile":
		return ChannelSMS
	case "email", "mail":
		return ChannelEmail
	default:
		return 0
	}
}

// 语言编码：本项目不支持小程序，语言集合仅 zh-CN/zh-TW/en。
const (
	// LangZhCN 简体中文。
	LangZhCN = "zh-CN"
	// LangZhTW 繁体中文。
	LangZhTW = "zh-TW"
	// LangEn 英文。
	LangEn = "en"
)

// IsValidLang 判断语言编码是否受支持。
func IsValidLang(lang string) bool {
	switch lang {
	case LangZhCN, LangZhTW, LangEn:
		return true
	default:
		return false
	}
}

// IsValidChannel 判断通道编码是否受支持。
func IsValidChannel(channel int32) bool {
	return ChannelName(channel) != ""
}

// NowUnix 返回当前 Unix 秒时间戳。
func NowUnix() int64 { return time.Now().Unix() }

// nowUnix 包内别名，保持 model 层调用一致。
func nowUnix() int64 { return time.Now().Unix() }

// isDuplicateErr 识别 MySQL 唯一索引冲突（错误号 1062）。
// go-sql-driver 目前是间接依赖，这里按错误文案判定，避免为错误码引入直接依赖。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}

// isNoRows 判断查询是否命中“无记录”。
func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// placeholders 返回 n 个 "?" 组成的 in 列表占位符（n<=0 时返回单个占位符）。
func placeholders(n int) string {
	if n <= 0 {
		return "?"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// int32Args 把 []int32 转成 SQL 变参。
func int32Args(vals []int32) []any {
	args := make([]any, 0, len(vals))
	for _, v := range vals {
		args = append(args, v)
	}
	return args
}
