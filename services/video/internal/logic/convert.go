package logic

import (
	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

// stateToRPC 把 model 的 int32 状态常量映射为 rpc.SubmissionState。
func stateToRPC(s int32) rpc.SubmissionState {
	return rpc.SubmissionState(s)
}

// stateFromRPC 把 rpc.SubmissionState 映射为 model int32 状态。
func stateFromRPC(s rpc.SubmissionState) int32 {
	return int32(s)
}

// submissionModelToRPC 把 model.VideoSubmission 转为 rpc.Submission。
func submissionModelToRPC(s *model.VideoSubmission) *rpc.Submission {
	if s == nil {
		return nil
	}
	return &rpc.Submission{
		Aid:    s.Aid,
		Mid:    s.Mid,
		Title:  s.Title,
		Desc:   s.Desc,
		Cover:  s.Cover,
		Typeid: s.Typeid,
		Tag:    s.Tag,
		State:  stateToRPC(s.State),
		Ctime:  s.Ctime,
		Mtime:  s.Mtime,
	}
}

// versionModelToRPC 把 model.VideoVersion 转为 rpc.VideoVersion。
func versionModelToRPC(v *model.VideoVersion) *rpc.VideoVersion {
	if v == nil {
		return nil
	}
	return &rpc.VideoVersion{
		Aid:     v.Aid,
		Version: v.Version,
		AssetId: v.AssetID,
		State:   stateToRPC(v.State),
		Ctime:   v.Ctime,
	}
}
