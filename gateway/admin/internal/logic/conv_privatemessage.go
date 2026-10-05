// 手写文件（不属于 goctl 生成产物）：/admin/private-message 三个路由的共用门禁与投影。
//
// 放在这里而不是各 logic 里重复一遍：举报台账的翻页口径、处置结论的投影、
// 「网关不发明判定」这条边界只有一份实现，才不会三条路由各长出一套规则。

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// errPMServiceNotConfigured 未配 PrivateMessageRPC 时不放行：返回空台账会让运营以为「没有举报」。
var errPMServiceNotConfigured = errors.New("gateway/admin: private-message service client not configured")

// errPMRequestMissing goctl 生成的 handler 理论上总会带 req，缺了就是调用面异常，不当「零值请求」处理。
var errPMRequestMissing = errors.New("gateway/admin: request payload missing")

// errPMSessionRequired 受 AdminPermission 保护的写路由拿不到会话身份：
// 说明这条路由没被中间件保护（权限表/挂载漂移），一律 fail-closed。
var errPMSessionRequired = errors.New("gateway/admin: admin session identity required")

// errReportIDRequired report_id=0 不是「不过滤」而是「没选中任何一行」：
// 处置类入参里没有 0 的合法语义，交给下游只会变成「找不到该举报」这种读不懂的结论。
var errReportIDRequired = errors.New("gateway/admin: report_id required")

// pmSessionGate 两条写入口的共用门槛：会话身份必须存在。
//
// 与 live-room 同口径：网关**不**用 admin_id 覆盖 handler/operator —— admin_id 是
// op_admin_user 主键、handler 是用户 mid，两个编号空间，覆盖等于把处置记到无关用户头上。
// 两个主体都进日志：能追「谁点的按钮」，也能对上下游台账。
func pmSessionGate(ctx context.Context, route string, subjectMid int64) error {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return errPMSessionRequired
	}
	// 只打路由与两个 ID：私信正文、举报说明与备注都不进网关日志（AGENTS.md §7）。
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d subject_mid=%d", route, id.AdminID, subjectMid)
	return nil
}

// pmEnum 挡住 0（*_UNSPECIFIED）：状态与动作的 0 在契约里都是「未指定」而不是「全部」，
// 放过它等于让一次没填动作的点击变成一次真实处置。
func pmEnum(field string, v int32) error {
	if v <= 0 {
		return fmt.Errorf("gateway/admin: %s required (0 = *_UNSPECIFIED)", field)
	}
	return nil
}

// pmNonNeg 只挡负数：0 在私信运营面里是合法的「不过滤 / 由服务端推算」哨兵，
// 上限（page_size、batch_limit）与「哪些取值算合法动作」都由 private-message 判定。
func pmNonNeg(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("gateway/admin: %s must be >= 0, got %d", field, v)
	}
	return nil
}

// pmReportToAPI 投影一条举报台账。原样转达 reason/state/handler 的数值：
// 举报原因码枚举与状态机真值都在 private-message，网关不自造映射表（迟早漂移）。
func pmReportToAPI(r *privatemessagerpc.ReportInfo) types.PrivateMessageReport {
	if r == nil {
		return types.PrivateMessageReport{}
	}
	return types.PrivateMessageReport{
		ReportId:       r.GetReportId(),
		ConversationId: r.GetConversationId(),
		MsgId:          r.GetMsgId(),
		ReporterMid:    r.GetReporterMid(),
		TargetMid:      r.GetTargetMid(),
		Reason:         r.GetReason(),
		Description:    r.GetDescription(),
		State:          r.GetState(),
		AuditTaskId:    r.GetAuditTaskId(),
		Handler:        r.GetHandler(),
		HandleNote:     r.GetHandleNote(),
		Ctime:          r.GetCtime(),
		Mtime:          r.GetMtime(),
	}
}

func pmReportListToAPI(reply *privatemessagerpc.ListReportsReply) types.PrivateMessageReportListData {
	out := types.PrivateMessageReportListData{
		List:       make([]types.PrivateMessageReport, 0, len(reply.GetList())),
		NextCursor: reply.GetNextCursor(),
		HasMore:    reply.GetHasMore(),
	}
	for _, r := range reply.GetList() {
		out.List = append(out.List, pmReportToAPI(r))
	}
	return out
}
