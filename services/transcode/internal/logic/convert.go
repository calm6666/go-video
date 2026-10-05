package logic

import (
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

// toTaskReply 把 model.TranscodeTask 转换为 rpc.TaskReply。
// 用于 SubmitTask/GetTask/ListTasks/UpdateProgress 等返回任务详情的方法。
func toTaskReply(t *model.TranscodeTask) *rpc.TaskReply {
	if t == nil {
		return &rpc.TaskReply{}
	}
	return &rpc.TaskReply{
		TaskId:       t.TaskId,
		AssetId:      t.AssetId,
		TemplateId:   t.TemplateId,
		InputBucket:  t.InputBucket,
		InputKey:     t.InputKey,
		OutputBucket: t.OutputBucket,
		OutputKey:    t.OutputKey,
		State:        rpc.TaskState(t.State),
		Progress:     t.Progress,
		Errno:        t.Errno,
		ErrMsg:       t.ErrMsg,
		Ctime:        t.Ctime,
		Mtime:        t.Mtime,
	}
}

// toTemplateReply 把 model.TranscodeTemplate 转换为 rpc.TemplateReply。
func toTemplateReply(t *model.TranscodeTemplate) *rpc.TemplateReply {
	if t == nil {
		return &rpc.TemplateReply{}
	}
	return &rpc.TemplateReply{
		TemplateId:     t.TemplateId,
		Name:           t.Name,
		Codec:          t.Codec,
		Width:          t.Width,
		Height:         t.Height,
		Bitrate:        t.Bitrate,
		Fps:            t.Fps,
		SegmentSeconds: t.SegmentSeconds,
	}
}
