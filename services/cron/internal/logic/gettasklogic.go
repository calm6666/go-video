package logic

import (
	"context"
	"strings"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTaskLogic {
	return &GetTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个任务定义。
func (l *GetTaskLogic) GetTask(in *rpc.GetTaskReq) (*rpc.GetTaskReply, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}

	d, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, taskKey)
	if err != nil {
		l.Errorf("GetTask find failed, task_key=%s", taskKey)
		return nil, err
	}
	if d == nil {
		// 不存在就是不存在：绝不返回空 definition 假装「查到了但没配」。
		return nil, model.ErrTaskNotFound
	}
	if !l.svcCtx.Registry.Has(d.Handler) {
		// 「DB 有定义、进程没实现」是部署缺口，不改变查询结果，但要留下可定位的日志。
		l.Errorf("GetTask handler not registered in this process, task_key=%s handler=%s", d.TaskKey, d.Handler)
	}
	return &rpc.GetTaskReply{Definition: taskDefinitionInfo(d)}, nil
}
