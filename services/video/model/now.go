package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳。
func nowUnix() int64 {
	return time.Now().Unix()
}

// 稿件状态常量（与 rpc.SubmissionState 枚举值一致，model 层避免依赖 rpc 包）。
const (
	StateDraft          int32 = 1  // 草稿
	StateUploading      int32 = 2  // 上传中
	StateUploaded       int32 = 3  // 上传完成
	StateScanning       int32 = 4  // 扫描中
	StateTranscoding    int32 = 5  // 转码中
	StateReadyForReview int32 = 6  // 待审核
	StateRejected       int32 = 7  // 审核驳回
	StateAppeal         int32 = 8  // 申诉中
	StateApproved       int32 = 9  // 审核通过
	StateScheduled      int32 = 10 // 定时发布
	StatePublished      int32 = 11 // 已发布
	StateOffline        int32 = 12 // 下架
	StateExpired        int32 = 13 // 过期
	StateDeleted        int32 = 14 // 删除
)
