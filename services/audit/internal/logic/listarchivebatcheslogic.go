package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListArchiveBatchesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListArchiveBatchesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListArchiveBatchesLogic {
	return &ListArchiveBatchesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页列出归档批次
//
// 实现要点（规格 1~3）：
//  1. clampPage 归一化分页；chain_key / state / ctime 区间为可选过滤，排序键固定在
//     model 侧（batch_id DESC，等价时间倒序且走主键，不需要 filesort）；
//  2. 回参含 manifest_hash 与 last_entry_hash：运维据此核对「已归档段尾哈希」与
//     「热表段首 prev_hash」是否衔接；
//  3. 本接口是归档覆盖率的唯一入口，配合 VerifyAuditChain 回答「删了多少、还剩什么」。
//
// chain_key 给全名（含日期）时按等值过滤；只给动作域（不含 "/"）时不当成 chain_key，
// 直接报错而不是猜：把 "media" 前缀匹配成 "media/%" 会让运维以为整域都归档了。
func (l *ListArchiveBatchesLogic) ListArchiveBatches(in *rpc.ListArchiveBatchesReq) (*rpc.ListArchiveBatchesReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, false); err != nil {
		return nil, err
	}
	chainKey := strings.TrimSpace(in.GetChainKey())
	if chainKey != "" && !model.ValidChainKey(chainKey) {
		return nil, fmt.Errorf("%w: chain_key=%q 必须形如 <action_domain>/<yyyy-MM-dd>",
			model.ErrChainKeyRequired, chainKey)
	}
	state, err := batchStateFilter(in.GetState())
	if err != nil {
		return nil, err
	}
	if err := ctimeWindow(in.GetStartAt(), in.GetEndAt()); err != nil {
		return nil, err
	}
	d := buildDeps(l.svcCtx)
	pn, ps, err := d.clampPage(in.GetPn(), in.GetPs())
	if err != nil {
		return nil, err
	}
	rows, total, err := d.archives.List(l.ctx, model.ArchiveBatchFilter{
		ChainKey: chainKey,
		State:    state,
		StartAt:  in.GetStartAt(),
		EndAt:    in.GetEndAt(),
		Pn:       pn,
		Ps:       ps,
	})
	if err != nil {
		l.Errorf("ListArchiveBatches 查询失败 caller=%s chain_key=%s state=%s err=%v",
			strings.TrimSpace(cc.GetCallerService()), chainKey, state, err)
		return nil, err
	}
	return &rpc.ListArchiveBatchesReply{Items: archiveBatchViews(rows), Total: total}, nil
}
