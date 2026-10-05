// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：account passport 能力 → 客户端投影。

package logic

import (
	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"
)

// passportLoginLogsToAPI 投影本人登录记录。login_type/status 在服务端是 int32 裸值，
// 网关原样透出，不做任何文案化；空列表返回 []，避免客户端把 null 当成未加载。
func passportLoginLogsToAPI(list []*accountrpc.LoginLog) []types.PassportLoginLog {
	out := make([]types.PassportLoginLog, 0, len(list))
	for _, v := range list {
		out = append(out, types.PassportLoginLog{
			Mid:       v.GetMid(),
			IP:        v.GetIp(),
			Ts:        v.GetTs(),
			LoginType: v.GetLoginType(),
			Status:    v.GetStatus(),
			Reason:    v.GetReason(),
			Device:    v.GetDevice(),
		})
	}
	return out
}
