package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetLiveTranscodeTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetLiveTranscodeTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLiveTranscodeTaskLogic {
	return &GetLiveTranscodeTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个转码任务
//
// 只读方法：不加锁、不写库、不写 Outbox，也没有幂等键要求。
// task_id<=0 直接判不存在且不查库——0 是「未指定」的占位值，拿它去查等于把参数错误
// 伪装成一次正常的空结果。缓存只对终态行开启（见 taskDetailTTL）：
// state/version/heartbeat 是 Stop/Retry/Cancel/Report 的前置判据，运行中的行必须直读主表。
// 投影逐列取自 live_transcode_task：source_ref 原样回传给调用方，但本方法不打任何日志，
// errno/err_msg 在写入时已脱敏（AGENTS.md §6）。
func (l *GetLiveTranscodeTaskLogic) GetLiveTranscodeTask(in *rpc.LiveTranscodeTaskReq) (*rpc.LiveTranscodeTaskInfo, error) {
	taskID := in.GetTaskId()
	if err := checkPositive("task_id", taskID, model.ErrTranscodeTaskNotFound); err != nil {
		return nil, err
	}
	cfg := l.svcCtx.Config.LiveMedia
	key := taskCacheKey(taskKindTranscode, taskID)

	// 命中即可信：能进缓存的行都是终态行，键里的主键再校验一次防串写。
	cached := &rpc.LiveTranscodeTaskInfo{}
	if readCachedInfo(l.ctx, l.svcCtx, key, cached) && cached.GetTaskId() == taskID {
		return cached, nil
	}

	task, err := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, model.ErrTranscodeTaskNotFound
	}
	info := transcodeInfo(task)
	writeCachedInfo(l.ctx, l.svcCtx, key, taskDetailTTL(cfg, model.IsTranscodeTerminal(task.State)), info)
	return info, nil
}
