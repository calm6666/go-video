package logic

import (
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// taskToRPC 把 model.ModerationTask 转换为 rpc.Task。
func taskToRPC(t *model.ModerationTask) *rpc.Task {
	if t == nil {
		return nil
	}
	return &rpc.Task{
		TaskId:       t.ID,
		SubmissionId: t.SubmissionID,
		ContentType:  rpc.ContentType(t.ContentType),
		Mid:          t.Mid,
		UpMid:        t.UpMid,
		Business:     t.Business,
		Reason:       t.Reason,
		State:        rpc.TaskState(t.State),
		Ctime:        t.Ctime,
		Mtime:        t.Mtime,
		Operator:     t.Operator,
	}
}

// resultToRPC 把 model.ModerationResult 转换为 rpc.Result。
func resultToRPC(r *model.ModerationResult) *rpc.Result {
	if r == nil {
		return nil
	}
	return &rpc.Result{
		TaskId:   r.TaskID,
		Verdict:  rpc.Verdict(r.Verdict),
		Reason:   r.Reason,
		WorkerId: r.WorkerID,
		Reviewer: r.Reviewer,
		Ctime:    r.Ctime,
	}
}

// appealToRPC 把 model.ModerationAppeal 转换为 rpc.Appeal。
func appealToRPC(a *model.ModerationAppeal) *rpc.Appeal {
	if a == nil {
		return nil
	}
	return &rpc.Appeal{
		AppealId:     a.ID,
		TaskId:       a.TaskID,
		Mid:          a.Mid,
		Content:      a.Content,
		FinalVerdict: rpc.Verdict(a.FinalVerdict),
		FinalReason:  a.FinalReason,
		Handler:      a.Handler,
		Ctime:        a.Ctime,
		Mtime:        a.Mtime,
	}
}
