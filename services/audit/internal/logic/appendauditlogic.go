package logic

import (
	"context"
	"fmt"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AppendAuditLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAppendAuditLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AppendAuditLogic {
	return &AppendAuditLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 追加一条审计（event_id 幂等）。
//
// 流程（规格来自本方法第一轮的注释，实现落在 deps.prepareEntry / appendIdempotent）：
//  1. CallContext 校验：caller_service 与 request_id 必填（写入者归因，AGENTS.md §5）；
//  2. 条目校验：event_id/action/action_domain 必填、枚举合法、摘要走白名单、
//     reason/actor_name 过 PII 兜底——命中即整条拒绝，不做「脱敏后入库」的降级；
//  3. ip/device_id 用盐转成 ip_hash/device_hash（盐缺失 ErrHashSaltMissing，不落裸哈希或明文）；
//  4. 先按 event_id 预检：命中则 reused=true 原样返回，不新增行；
//  5. 事务内 Ensure 链头 → LockForUpdate → 算 seq/prev_hash/entry_hash → Insert → Advance，
//     乐观锁未命中按 Write.ChainRetry 重试；
//  6. 投影永不出现明文 IP / 设备号。
func (l *AppendAuditLogic) AppendAudit(in *rpc.AppendAuditReq) (*rpc.AppendAuditReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, true); err != nil {
		return nil, err
	}
	if in.GetEntry() == nil {
		return nil, model.ErrDraftRequired
	}

	d := buildDeps(l.svcCtx)
	row, err := d.prepareEntry(cc, in.GetEntry())
	if err != nil {
		return nil, err
	}
	rows, accepted, reused, err := d.appendIdempotent(l.ctx, []*model.AuditEntry{row})
	if err != nil {
		// 只打标识字段：reason/摘要属于业务内容，日志不是审计载体（AGENTS.md §7）。
		l.Errorf("AppendAudit 写入失败 event_id=%s chain_key=%s caller=%s err=%v",
			row.EventID, row.ChainKey, row.CallerService, err)
		return nil, err
	}
	if len(rows) != 1 || rows[0] == nil {
		return nil, fmt.Errorf("audit: 追加结果数量异常 got=%d", len(rows))
	}
	return &rpc.AppendAuditReply{
		Entry:  entryView(rows[0]),
		Reused: accepted == 0 && reused > 0,
	}, nil
}
