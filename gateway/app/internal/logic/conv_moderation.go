// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：moderation-orchestrator RPC → 客户端投影。

package logic

import (
	"go-video/gateway/app/internal/types"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"
)

// appealToAPI 投影申诉结论。Verdict 是 protobuf 枚举，必须显式转成 int32 才能进 HTTP 信封；
// 申诉未处理时 final_verdict/final_reason/handler/mtime 由服务端留空，网关不补默认结论。
func appealToAPI(a *moderationrpc.Appeal) types.ModerationAppealInfo {
	if a == nil {
		return types.ModerationAppealInfo{}
	}
	return types.ModerationAppealInfo{
		AppealId:     a.GetAppealId(),
		TaskId:       a.GetTaskId(),
		Mid:          a.GetMid(),
		Content:      a.GetContent(),
		FinalVerdict: int32(a.GetFinalVerdict()),
		FinalReason:  a.GetFinalReason(),
		Handler:      a.GetHandler(),
		Ctime:        a.GetCtime(),
		Mtime:        a.GetMtime(),
	}
}
