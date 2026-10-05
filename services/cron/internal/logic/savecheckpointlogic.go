package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SaveCheckpointLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveCheckpointLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveCheckpointLogic {
	return &SaveCheckpointLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 独立推进游标（CAS）。
//
// 游标是 cron「重放不重复产生副作用」的落点：报表/归档/索引重建按主键或时间水位分段
// 推进，每段完成后 CAS 前进。本服务只保证 CAS 与「同一 (task_key, scope_key) 只有一个
// 赢家」，value 的单调性由处理器自己负责（有的任务按别名走，本来就是无序字符串）。
//
// 版本语义（与 model.Save 一对一绑定，绝不做「读到冲突就顺手覆盖」的贴心处理）：
//   - expected_version=0：要求游标尚不存在，并发首写只有一个赢家；
//   - expected_version=N>0：要求当前版本恰为 N；
//   - 冲突返回 model.ErrCheckpointConflict，处理器自己决定重读后放弃本轮还是继续。
func (l *SaveCheckpointLogic) SaveCheckpoint(in *rpc.SaveCheckpointReq) (*rpc.SaveCheckpointReply, error) {
	if in == nil || in.Checkpoint == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
		return nil, err
	}
	c, err := checkpointFromProto(in.Checkpoint)
	if err != nil {
		return nil, err
	}
	// 各列长度已在 checkpointFromProto 里按列宽校验过，这里不再重复一次「看起来有校验」的 4096。
	if in.ExpectedVersion < 0 {
		return nil, fmt.Errorf("%w: expected_version=%d 不能为负", model.ErrCheckpointConflict, in.ExpectedVersion)
	}
	// 请求里的两个版本字段描述的是同一个期望值，不一致就是调用方自己矛盾：
	// 猜任何一个都可能覆盖别人已推进的游标，所以直接拒绝。
	if in.Checkpoint.Version != in.ExpectedVersion {
		return nil, fmt.Errorf("%w: checkpoint.version=%d 与 expected_version=%d 不一致",
			model.ErrCheckpointConflict, in.Checkpoint.Version, in.ExpectedVersion)
	}
	c.Operator = firstNonEmpty(strings.TrimSpace(in.Operator), strings.TrimSpace(in.Checkpoint.Operator))
	if c.Operator == "" {
		c.Operator = l.svcCtx.WorkerID()
	}
	// 顶层 in.operator 与 checkpoint.operator 是两条来源，checkpointFromProto 只看得到后者，
	// 所以合并后必须再按 operator 列宽（VARCHAR(64)）兜一次，否则长操作人仍然写不进去。
	if err := checkTextLimit("operator", c.Operator, model.MaxOperatorBytes); err != nil {
		return nil, err
	}

	// 游标键必须挂在已注册的任务上：否则拼错 task_key 会留下一张永远没人读的游标行，
	// 而「重放时以为已经推进过了」正是最难查的事故。
	def, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, c.TaskKey)
	if err != nil {
		l.Errorf("SaveCheckpoint read definition failed, task_key=%s", c.TaskKey)
		return nil, err
	}
	if def == nil {
		return nil, model.ErrTaskNotFound
	}

	var stored *model.TaskCheckpoint
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		advanced, err := l.svcCtx.Checkpoints.SaveTx(ctx, tx, c, in.ExpectedVersion)
		if err != nil {
			return err
		}
		if !advanced {
			return fmt.Errorf("%w: task_key=%s scope_key=%s expected_version=%d",
				model.ErrCheckpointConflict, c.TaskKey, c.ScopeKey, in.ExpectedVersion)
		}
		// 回读服务端真值：version/ctime/mtime 都由库里算，响应不能回显请求值。
		row, err := l.svcCtx.Checkpoints.FindOneTx(ctx, tx, c.TaskKey, c.ScopeKey)
		if err != nil {
			return err
		}
		if row == nil {
			return fmt.Errorf("%w: 游标写入后读不回，task_key=%s scope_key=%s",
				model.ErrCheckpointConflict, c.TaskKey, c.ScopeKey)
		}
		stored = row
		return nil
	})
	if err != nil {
		l.Errorf("SaveCheckpoint failed, task_key=%s scope_key=%s expected_version=%d",
			c.TaskKey, c.ScopeKey, in.ExpectedVersion)
		return nil, err
	}
	return &rpc.SaveCheckpointReply{Checkpoint: checkpointInfo(stored), Advanced: true}, nil
}
