package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPoolVersionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListPoolVersionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPoolVersionsLogic {
	return &ListPoolVersionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 列出版本与生成批次（可追溯性）
//
// 这张表是"候选能追溯到生成批次"的唯一入口：一行版本记录了 batch_id（谁产的）与
// generator（哪个作业产的）。本方法只读，不改任何状态。
//
// current_version=0 的语义是"该池在线不出数"（指针行不存在或 version=0），
// 这里刻意不报错 —— 列版本正是排障"为什么没出数"的第一步，
// 报 ErrPoolNotFound 会把可用信息一起挡掉；在线读路径（GetPoolSnapshot/RecallCandidates）
// 才必须把它当错误。
func (l *ListPoolVersionsLogic) ListPoolVersions(in *rpc.ListPoolVersionsReq) (*rpc.ListPoolVersionsReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: ListPoolVersionsReq", model.ErrRequestRequired)
	}
	repo := l.svcCtx.Repository
	opts := repo.Options()
	source, poolKey, err := requirePool(in.GetPool())
	if err != nil {
		return nil, err
	}
	limit := int(in.GetLimit())
	if limit <= 0 {
		limit = opts.MaxVersionList
	}
	// 超上限报 ErrLimitTooLarge 而不是静默裁小：调用方以为拿到了 500 个版本、
	// 实际只拿到 100 个时，"这个池从没发布过旧批次"就是被误判出来的结论。
	if limit > opts.MaxVersionList {
		return nil, fmt.Errorf("%w: limit=%d > %d", model.ErrLimitTooLarge, limit, opts.MaxVersionList)
	}
	// 状态过滤在 SQL 里做（ListByPool 的 include_retired 条件），不在内存里筛完再截断：
	// 后者会让 limit 的语义从"取多少行"变成"筛后剩多少行"。
	rows, err := repo.PoolVersion.ListByPool(l.ctx, source, poolKey, in.GetIncludeRetired(), limit)
	if err != nil {
		return nil, err
	}
	versions := make([]*rpc.PoolVersionInfo, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		versions = append(versions, &rpc.PoolVersionInfo{
			Pool:          poolRef(row.Source, row.PoolKey),
			Version:       row.Version,
			BatchId:       row.BatchID,
			Generator:     row.Generator,
			SchemaVersion: row.SchemaVersion,
			ItemCount:     int32(row.ItemCount),
			State:         toRPCState(row.State),
			PublishedAt:   row.PublishedAt,
			Operator:      row.Operator,
			Note:          row.Note,
			Ctime:         row.Ctime,
			Mtime:         row.Mtime,
		})
	}
	current, err := currentPointerVersion(l.ctx, repo, source, poolKey)
	if err != nil {
		return nil, err
	}
	return &rpc.ListPoolVersionsReply{Versions: versions, CurrentVersion: current}, nil
}

// currentPointerVersion 读某池的 CURRENT 版本号；无指针行/未上线返回 0。
// 只有真正的存储故障才上抛错误 —— "这个池没上线"是数据状态，不是调用失败。
func currentPointerVersion(ctx context.Context, repo *repository.Repository, source int32, poolKey string) (int64, error) {
	ptr, err := repo.Current.FindOne(ctx, source, poolKey)
	if err != nil {
		if errors.Is(err, model.ErrPoolNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if ptr == nil {
		return 0, nil
	}
	if ptr.Version < 0 {
		return 0, fmt.Errorf("recommend-recall: pool source=%d pool_key=%s has negative pointer version %d",
			source, poolKey, ptr.Version)
	}
	return ptr.Version, nil
}
