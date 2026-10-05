package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetReplayTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetReplayTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetReplayTaskLogic {
	return &GetReplayTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个回放任务
//
// 只读方法：不加锁、不推进状态、不发事件。replay_id<=0 直接判不存在且不查库。
// 不返回零值 Info：Worker 会拿 version=0 当 expected_version，之后每次上报都撞版本冲突。
// 缓存只对终态行开启：REVIEW_SUBMITTED 及之前的行随时可能被 ApplyReplayContentState
// （video 事实投影）或 Worker 推进，直读主表才有正确判据。
// 投影边界（不得美化）：aid/bvid/asset_id 只是跨服务主键引用，本行不含稿件状态事实——
// 「回放能否对外」必须看 live_replay_asset_ref.review_state 投影（事实源在 video，
// 走 ListReplayAssetRefs）；segment_count/gap_count 是提交时按 StatsInRange 落的快照，
// 可由切片表重算，非唯一事实源。
func (l *GetReplayTaskLogic) GetReplayTask(in *rpc.ReplayTaskReq) (*rpc.LiveReplayTaskInfo, error) {
	replayID := in.GetReplayId()
	if err := checkPositive("replay_id", replayID, model.ErrReplayTaskNotFound); err != nil {
		return nil, err
	}
	cfg := l.svcCtx.Config.LiveMedia
	key := taskCacheKey(taskKindReplay, replayID)

	cached := &rpc.LiveReplayTaskInfo{}
	if readCachedInfo(l.ctx, l.svcCtx, key, cached) && cached.GetReplayId() == replayID {
		return cached, nil
	}

	task, err := l.svcCtx.ReplayTasks.FindOne(l.ctx, replayID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, model.ErrReplayTaskNotFound
	}
	info := replayInfo(task)
	writeCachedInfo(l.ctx, l.svcCtx, key, taskDetailTTL(cfg, model.IsReplayTerminal(task.State)), info)
	return info, nil
}
