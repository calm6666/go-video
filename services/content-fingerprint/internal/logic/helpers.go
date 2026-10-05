package logic

import (
	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"
)

// fpTypeToModel 把 rpc.FpType 转换为 model 层 int32 常量。
func fpTypeToModel(t rpc.FpType) int32 {
	switch t {
	case rpc.FpType_FP_TYPE_VIDEO:
		return model.FpTypeVideo
	case rpc.FpType_FP_TYPE_AUDIO:
		return model.FpTypeAudio
	default:
		return 0
	}
}

// fpTypeFromModel 把 model 层 int32 转换为 rpc.FpType。
func fpTypeFromModel(t int32) rpc.FpType {
	switch t {
	case model.FpTypeVideo:
		return rpc.FpType_FP_TYPE_VIDEO
	case model.FpTypeAudio:
		return rpc.FpType_FP_TYPE_AUDIO
	default:
		return rpc.FpType_FP_TYPE_UNSPECIFIED
	}
}

// taskStateToModel 把 rpc.TaskState 转换为 model 层 int32 常量。
func taskStateToModel(s rpc.TaskState) int32 {
	switch s {
	case rpc.TaskState_TASK_STATE_PENDING:
		return model.TaskStatePending
	case rpc.TaskState_TASK_STATE_SUCCEEDED:
		return model.TaskStateSucceeded
	case rpc.TaskState_TASK_STATE_FAILED:
		return model.TaskStateFailed
	default:
		return 0
	}
}

// taskStateFromModel 把 model 层 int32 转换为 rpc.TaskState。
func taskStateFromModel(s int32) rpc.TaskState {
	switch s {
	case model.TaskStatePending:
		return rpc.TaskState_TASK_STATE_PENDING
	case model.TaskStateSucceeded:
		return rpc.TaskState_TASK_STATE_SUCCEEDED
	case model.TaskStateFailed:
		return rpc.TaskState_TASK_STATE_FAILED
	default:
		return rpc.TaskState_TASK_STATE_UNSPECIFIED
	}
}

// taskToReply 把 model.FingerprintTask 转换为 rpc.TaskReply。
func taskToReply(t *model.FingerprintTask) *rpc.TaskReply {
	if t == nil {
		return &rpc.TaskReply{}
	}
	return &rpc.TaskReply{
		TaskId:   t.TaskID,
		AssetId:  t.AssetID,
		FpType:   fpTypeFromModel(t.FpType),
		VideoKey: t.VideoKey,
		AudioKey: t.AudioKey,
		State:    taskStateFromModel(t.State),
		Ctime:    t.Ctime,
		Mtime:    t.Mtime,
	}
}

// recordToItem 把 model.FingerprintRecord 转换为 rpc.MatchItem。
// 本期占位：Score=0（无相似度算法）。
func recordToItem(r *model.FingerprintRecord) *rpc.MatchItem {
	if r == nil {
		return &rpc.MatchItem{}
	}
	return &rpc.MatchItem{
		AssetId: r.AssetID,
		FpKey:   r.Key,
		FpHash:  r.Hash,
		Score:   0, // TODO(后续)：相似度算法接入后填写
		FpType:  fpTypeFromModel(r.FpType),
	}
}
