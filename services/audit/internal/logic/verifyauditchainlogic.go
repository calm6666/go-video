package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type VerifyAuditChainLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVerifyAuditChainLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VerifyAuditChainLogic {
	return &VerifyAuditChainLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 哈希链完整性自证（按链区间重放校验）。算法定义见 model/hashchain.go 与 rpc/audit.proto 文件头。
//
//  1. chain_key 必填且必须是 <action_domain>/<yyyy-MM-dd>；from/to 缺省补全为整链，
//     上限夹到 Verify.MaxEntriesPerCall；
//  2. 按 seq 升序重放，依次判定 seq 连续、prev_hash 衔接、entry_hash 可复算；
//  3. 首个断点即停止判定（继续往下只会把同一处篡改重复报成 N 条异常）；
//  4. 结果不缓存：校验的意义就是此刻重算；
//  5. 每次校验写一条 action=audit.chain.verify 的条目（校验行为本身也是敏感操作）。
//
// 契约缺口：VerifyAuditChainReq 没有 expected_prev_hash，因此从链中间起验时，
// 第一行的 prev_hash 无锚可比。本实现会主动多读一行（from_seq-1）取锚点；
// 那一行也不存在时明确跳过该次比对（不做假判定），并在自审计维度里记 anchor=missing。
func (l *VerifyAuditChainLogic) VerifyAuditChain(in *rpc.VerifyAuditChainReq) (*rpc.VerifyAuditChainReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, false); err != nil {
		return nil, err
	}
	chainKey := strings.TrimSpace(in.GetChainKey())
	if chainKey == "" {
		return nil, model.ErrChainKeyRequired
	}
	if !model.ValidChainKey(chainKey) {
		return nil, fmt.Errorf("%w: chain_key=%q 必须形如 <action_domain>/<yyyy-MM-dd>", model.ErrChainKeyRequired, chainKey)
	}
	if in.GetFromSeq() < 0 || in.GetToSeq() < 0 {
		return nil, model.ErrArchiveRangeInvalid
	}
	if in.GetFromSeq() > 0 && in.GetToSeq() > 0 && in.GetFromSeq() > in.GetToSeq() {
		return nil, fmt.Errorf("%w: from_seq=%d > to_seq=%d", model.ErrArchiveRangeInvalid,
			in.GetFromSeq(), in.GetToSeq())
	}

	d := buildDeps(l.svcCtx)
	head, err := d.chains.FindOne(l.ctx, chainKey)
	if err != nil {
		// 链头不存在 = 这条链从未写过条目（或链头被删）。没有事实可校验，明确报错。
		l.Errorf("VerifyAuditChain 读链头失败 chain_key=%s err=%v", chainKey, err)
		return nil, err
	}
	from := in.GetFromSeq()
	if from <= 0 {
		from = 1
	}
	to := in.GetToSeq()
	if to <= 0 || to > head.Seq {
		to = head.Seq
	}
	limit := d.verifyLimit(in.GetMaxEntries())
	reply := &rpc.VerifyAuditChainReply{}
	if from > to || head.Seq == 0 {
		// 区间落在链尾之外：没有可校验的行，intact=true / checked=0 是诚实答案，
		// 不是「校验通过了一条都没有」。续验方靠 last_entry_hash=链头摘要判断接得上。
		reply.Intact = true
		reply.LastEntryHash = head.LastHash
		selfAuditLogged(l.ctx, l.Logger, cc, d,
			chainVerifySpec(chainKey, from, to, replayResult{}, model.GenesisHash(chainKey)))
		return reply, nil
	}

	rows, err := d.entries.ListByChainRange(l.ctx, chainKey, from, to, limit+1)
	if err != nil {
		l.Errorf("VerifyAuditChain 取区间失败 chain_key=%s from=%d to=%d err=%v", chainKey, from, to, err)
		return nil, err
	}
	truncated := int32(len(rows)) > limit
	if truncated {
		rows = rows[:limit]
	}
	anchor, err := d.prevAnchor(l.ctx, chainKey, from)
	if err != nil {
		return nil, err
	}
	res := replayChain(rows, from, to, anchor)
	if truncated {
		res.markTruncated()
	}

	reply.Checked = res.Checked
	reply.LastEntryHash = res.LastHash
	reply.Intact = res.intact()
	reply.Truncated = res.Truncated
	if res.Broken != nil {
		reply.FirstBrokenSeq = res.Broken.Seq
		reply.FirstBrokenEntryId = res.Broken.EntryID
		reply.BrokenReason = res.Broken.Reason
		// 断链是最高级别的告警：除了回参还要打 Error，让日志侧也能独立发现。
		l.Errorf("VerifyAuditChain 检出断链 chain_key=%s %s", chainKey, res.Broken.String())
	}
	selfAuditLogged(l.ctx, l.Logger, cc, d, chainVerifySpec(chainKey, from, to, res, anchor))
	return reply, nil
}

// chainVerifySpec 描述一次校验的留痕意图：链名与区间进 before，结论进 after。
// anchor 缺失（from_seq-1 那行不存在）单独记一笔：否则「第一行没比 prev」这件事不可见。
func chainVerifySpec(chainKey string, from, to int64, res replayResult, anchor string) selfAuditSpec {
	after := map[string]string{
		"checked":   strconv.FormatInt(res.Checked, 10),
		"last_seq":  strconv.FormatInt(res.LastSeq, 10),
		"intact":    strconv.FormatBool(res.intact()),
		"truncated": strconv.FormatBool(res.Truncated),
	}
	if r := res.brokenReason(); r != "" {
		after["broken_reason"] = r
	}
	if anchor == "" && from > 1 {
		after["anchor"] = "missing"
	}
	return selfAuditSpec{
		Action:     actionChainVerify,
		Domain:     selfDomainSystem,
		TargetType: targetAuditChain,
		TargetID:   chainKey,
		Reason:     "重放校验哈希链",
		Before: map[string]string{
			"chain_key": chainKey,
			"from_seq":  strconv.FormatInt(from, 10),
			"to_seq":    strconv.FormatInt(to, 10),
		},
		After: after,
	}
}
