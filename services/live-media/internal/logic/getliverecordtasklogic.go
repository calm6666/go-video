package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetLiveRecordTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetLiveRecordTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLiveRecordTaskLogic {
	return &GetLiveRecordTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个录制任务（含 last_seq，供断点续录）
//
// 只读方法：不加锁、不推进状态、不写 Outbox。record_id<=0 直接判不存在且不查库。
// 绝不返回零值 Info 冒充成功：Worker 会把 last_seq=0 读成「从第一片重录」，
// 从而覆盖已登记切片；version=0 又会让后续上报永远撞版本冲突。
// 缓存只对终态行开启（见 taskDetailTTL）：RECORDING/STOPPING 的 last_seq/heartbeat_at
// 是续录起点与超时判定输入，必须直读主表；FAILED 也不算终态——它能被断点续录复活。
// segment_count/gap_count/recorded_duration_ms 是由 live_record_segment 重算的派生投影
// （RefreshStats 可修复），不作为唯一事实源；output_bucket/output_prefix 只是引用，
// 不含凭据，本方法不打任何日志（AGENTS.md §6）。
func (l *GetLiveRecordTaskLogic) GetLiveRecordTask(in *rpc.LiveRecordTaskReq) (*rpc.LiveRecordTaskInfo, error) {
	recordID := in.GetRecordId()
	if err := checkPositive("record_id", recordID, model.ErrRecordTaskNotFound); err != nil {
		return nil, err
	}
	cfg := l.svcCtx.Config.LiveMedia
	key := taskCacheKey(taskKindRecord, recordID)

	cached := &rpc.LiveRecordTaskInfo{}
	if readCachedInfo(l.ctx, l.svcCtx, key, cached) && cached.GetRecordId() == recordID {
		return cached, nil
	}

	task, err := l.svcCtx.RecordTasks.FindOne(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, model.ErrRecordTaskNotFound
	}
	info := recordInfo(task)
	writeCachedInfo(l.ctx, l.svcCtx, key, taskDetailTTL(cfg, model.IsRecordTerminal(task.State)), info)
	return info, nil
}
