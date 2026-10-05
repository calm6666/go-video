package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetPoolSnapshotLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetPoolSnapshotLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPoolSnapshotLogic {
	return &GetPoolSnapshotLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 读某个池的某个版本条目（运维/排障，分页有上限）
//
// 这是运维面读路径：不写缓存（在线 TopN 缓存键里带 limit，运维分页写进去只会污染命中率），
// 也不做任何降级伪装 —— 池没上线就是 ErrPoolNotFound，
// 与"池上线了但这一页没有条目"（items 为空、has_more=false）严格区分。
func (l *GetPoolSnapshotLogic) GetPoolSnapshot(in *rpc.GetPoolSnapshotReq) (*rpc.GetPoolSnapshotReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: GetPoolSnapshotReq", model.ErrRequestRequired)
	}
	repo := l.svcCtx.Repository
	opts := repo.Options()
	source, poolKey, err := requirePool(in.GetPool())
	if err != nil {
		return nil, err
	}
	// 默认页大小取"单页上限"：本接口是排障路径，调用方要的是"尽量一次看全"，
	// 而不是像列表页那样省流量；上限本身已由 Options().MaxPoolSnapshotPage 兜住。
	offset, limit, err := pageArgs(in.GetPn(), in.GetPs(), opts.MaxPoolSnapshotPage,
		opts.MaxPoolSnapshotPage, model.MaxPoolSnapshotOffset)
	if err != nil {
		return nil, err
	}

	version := in.GetVersion()
	if version < 0 {
		return nil, fmt.Errorf("%w: version=%d", model.ErrInvalidVersion, version)
	}
	if version == 0 {
		// version==0 表示"读当前生效版本"：指针必须经 recall_pool_current 解析，
		// 不能在版本表里按 state=CURRENT 找 —— 指针是读路径的唯一权威，state 是冗余视图。
		ptr, perr := repo.Current.FindOne(l.ctx, source, poolKey)
		if perr != nil {
			if errors.Is(perr, model.ErrPoolNotFound) {
				return nil, fmt.Errorf("%w: source=%d pool_key=%s has no current version",
					model.ErrPoolNotFound, source, poolKey)
			}
			return nil, perr
		}
		if ptr == nil || ptr.Version <= 0 {
			// 指针行存在但 version=0：该池从未上线。绝不能返回 items 为空的"成功"响应
			// 冒充空池 —— 那会让排障把"没发布"读成"发布了空批次"。
			return nil, fmt.Errorf("%w: source=%d pool_key=%s pointer version is 0 (never published)",
				model.ErrPoolNotFound, source, poolKey)
		}
		version = ptr.Version
	}

	row, err := repo.PoolVersion.FindOne(l.ctx, source, poolKey, version)
	if err != nil {
		return nil, err
	}
	items, err := repo.Pool.ListByVersion(l.ctx, source, poolKey, version, offset, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*rpc.PoolItem, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		out = append(out, &rpc.PoolItem{
			Aid:     item.Aid,
			Score:   item.Score,
			Version: item.Version,
			Ctime:   item.Ctime,
		})
	}
	// 总数用版本行的 item_count（登记值），不额外 COUNT(*)：
	// 已发布版本不可变，登记值即事实；再扫一次大表只为算总数是本服务禁止的模式。
	hasMore := int64(offset)+int64(len(out)) < row.ItemCount
	return &rpc.GetPoolSnapshotReply{
		Pool:        poolRef(source, poolKey),
		Version:     row.Version,
		BatchId:     row.BatchID,
		State:       toRPCState(row.State),
		ItemCount:   int32(row.ItemCount),
		Items:       out,
		HasMore:     hasMore,
		PublishedAt: row.PublishedAt,
	}, nil
}
