package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ArchiveAuditEntriesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewArchiveAuditEntriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ArchiveAuditEntriesLogic {
	return &ArchiveAuditEntriesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 归档一个链区间（request_id 幂等，先落清单再标记账号）
//
// 归档是唯一能让审计行离开热表的路径，因此它自己必须被最重地审计：
//  1. request_id、chain_key 必填；to_seq 缺省取链头 seq（只归档已封口部分）；
//     区间条数超 Archive.MaxEntriesPerBatch 直接拒绝并要求分片；
//  2. 只归档 occurred_at 已超过 RetentionPolicy.archive_after_days 的区间；
//  3. FindCovering 命中已 verified/purged 的覆盖批次 → 复用返回，不写两份清单；
//  4. 严格走 pending → writing → verified → purged，任一环节失败停在 failed，绝不跳过 verified；
//  5. purged 由 repository.PurgeMark 执行：只写不参与哈希的 archived_at 列，且强制要求
//     manifest_hash 非空（这条防线不受 Archive.VerifyBeforePurge 配置影响）；
//  6. 全程写 audit.archive.writing / .verified / .purged 条目，失败也写（result=error）。
//
// 与校验的条数上限差异：VerifyAuditChain 受 Query.MaxVerifyEntries 限制（在线接口要快返回），
// 归档区间必须整段验完才允许搬走，因此上限是 Archive.MaxEntriesPerBatch 而不是校验上限。
func (l *ArchiveAuditEntriesLogic) ArchiveAuditEntries(in *rpc.ArchiveAuditEntriesReq) (*rpc.ArchiveAuditEntriesReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, true); err != nil {
		return nil, err
	}
	chainKey := strings.TrimSpace(in.GetChainKey())
	if chainKey == "" {
		return nil, model.ErrChainKeyRequired
	}
	if !model.ValidChainKey(chainKey) {
		return nil, fmt.Errorf("%w: chain_key=%q 必须形如 <action_domain>/<yyyy-MM-dd>",
			model.ErrChainKeyRequired, chainKey)
	}
	if in.GetFromSeq() < 0 || in.GetToSeq() < 0 {
		return nil, model.ErrArchiveRangeInvalid
	}
	d := buildDeps(l.svcCtx)

	head, err := d.chains.FindOne(l.ctx, chainKey)
	if err != nil {
		l.Errorf("ArchiveAuditEntries 读链头失败 chain_key=%s err=%v", chainKey, err)
		return nil, err
	}
	from := in.GetFromSeq()
	if from <= 0 {
		from = 1
	}
	to := in.GetToSeq()
	if to <= 0 || to > head.Seq {
		// 只归档已封口部分：链头 seq 之后可能正在被并发追加，取走会留下断尾。
		to = head.Seq
	}
	if from > to {
		return nil, fmt.Errorf("%w: chain_key=%s 尾 seq=%d，请求区间 [%d,%d] 无已封口条目",
			model.ErrArchiveRangeInvalid, chainKey, head.Seq, from, to)
	}
	size := to - from + 1
	if max := d.archiveLimit(); size > max {
		return nil, fmt.Errorf("%w: 区间 %d 条 > 单批上限 %d，请分片",
			model.ErrArchiveRangeTooLarge, size, max)
	}
	policy, err := d.policies.FindEffective(l.ctx, model.DomainOfChainKey(chainKey))
	if err != nil {
		l.Errorf("ArchiveAuditEntries 读保留策略失败 chain_key=%s err=%v", chainKey, err)
		return nil, err
	}
	if policy == nil {
		return nil, fmt.Errorf("%w: chain_key=%s 无生效策略（含 default 回退），拒绝归档",
			model.ErrPolicyNotFound, chainKey)
	}
	if policy.State == model.StateDisable {
		return nil, fmt.Errorf("%w: action_domain=%s 的策略已停用，归档作业跳过该域",
			model.ErrPolicyNotFound, policy.ActionDomain)
	}
	// 对象引用在建批次时就得确定：bucket/object_key 由 Insert 落库，
	// 之后的 TransitionState 只推状态与哈希，不再改对象位置（避免清单被事后挪走）。
	bucket, err := d.bucket()
	if err != nil {
		l.Errorf("ArchiveAuditEntries 对象存储未就绪 chain_key=%s range=[%d,%d] err=%v", chainKey, from, to, err)
		return nil, err
	}
	manifestKey, entriesKey := archiveObjectKeys(chainKey, from, to)

	requestID := strings.TrimSpace(cc.GetRequestId())
	if existing, eerr := d.archives.FindByRequestID(l.ctx, requestID); eerr != nil {
		return nil, eerr
	} else if existing != nil {
		return &rpc.ArchiveAuditEntriesReply{Batch: archiveBatchView(existing), Reused: true}, nil
	}
	if covering, cerr := d.archives.FindCovering(l.ctx, chainKey, from, to); cerr != nil {
		return nil, cerr
	} else if covering != nil {
		l.Infof("ArchiveAuditEntries 区间已被批次覆盖 chain_key=%s range=[%d,%d] batch_id=%d state=%s",
			chainKey, from, to, covering.BatchID, covering.State)
		return &rpc.ArchiveAuditEntriesReply{Batch: archiveBatchView(covering), Reused: true}, nil
	}

	b := &model.ArchiveBatch{
		RequestID:  requestID,
		ChainKey:   chainKey,
		FromSeq:    from,
		ToSeq:      to,
		RowCount:   size,
		Bucket:     bucket,
		ObjectKey:  manifestKey,
		State:      model.BatchStatePending,
		OperatorID: cc.GetOperatorId(),
		TraceID:    strings.TrimSpace(cc.GetTraceId()),
	}
	if _, err := d.archives.Insert(l.ctx, b); err != nil {
		if !errors.Is(err, model.ErrTaskExists) {
			l.Errorf("ArchiveAuditEntries 建批次失败 chain_key=%s request_id=%s err=%v", chainKey, requestID, err)
			return nil, err
		}
		again, aerr := d.archives.FindByRequestID(l.ctx, requestID)
		if aerr != nil {
			return nil, errors.Join(err, aerr)
		}
		if again == nil {
			return nil, err
		}
		return &rpc.ArchiveAuditEntriesReply{Batch: archiveBatchView(again), Reused: true}, nil
	}

	trail := func(action, reason string, res rpc.AuditResult, after map[string]string) {
		l.trail(d, cc, b, action, reason, res, after)
	}
	fail := func(action, what string, cause error) (*rpc.ArchiveAuditEntriesReply, error) {
		from2 := b.State
		if from2 == "" {
			from2 = model.BatchStatePending
		}
		updated, terr := d.archives.TransitionState(l.ctx, b.BatchID, from2, model.BatchStateFailed,
			"", "", clampErrMsg(what+": "+cause.Error()))
		if terr != nil {
			l.Errorf("ArchiveAuditEntries 置 failed 失败 batch_id=%d err=%v（原始失败：%v）", b.BatchID, terr, cause)
		} else if updated {
			b.State = model.BatchStateFailed
		}
		trail(action, what, rpc.AuditResult_AUDIT_RESULT_ERROR, map[string]string{"state": b.State})
		return nil, cause
	}

	ok, err := d.archives.TransitionState(l.ctx, b.BatchID, model.BatchStatePending, model.BatchStateWriting, "", "", "")
	if err != nil {
		return fail(actionArchiveWriting, "批次进入 writing 失败", err)
	}
	if !ok {
		// 批次被并发推进（同一幂等键的第二次到达）：以库为准复用返回。
		fresh, ferr := d.archives.FindOne(l.ctx, b.BatchID)
		if ferr != nil {
			return nil, errors.Join(model.ErrTaskBadTransition, ferr)
		}
		return &rpc.ArchiveAuditEntriesReply{Batch: archiveBatchView(fresh), Reused: true}, nil
	}
	b.State = model.BatchStateWriting
	trail(actionArchiveWriting, "开始归档链区间", rpc.AuditResult_AUDIT_RESULT_OK, nil)

	rows, err := d.entries.ListByChainRange(l.ctx, chainKey, from, to, int32(size)+1)
	if err != nil {
		return fail(actionArchiveVerified, "读取归档区间失败", err)
	}
	if int64(len(rows)) != size {
		return fail(actionArchiveVerified, "区间行数与序号区间不符",
			fmt.Errorf("%w: 取到 %d 行，期望 %d 行", model.ErrArchiveRangeGap, len(rows), size))
	}
	if err := archiveEligible(policy, newestOccurredAt(rows), d.now()); err != nil {
		return fail(actionArchiveVerified, "区间未到期", err)
	}
	anchor, err := d.prevAnchor(l.ctx, chainKey, from)
	if err != nil {
		return fail(actionArchiveVerified, "读取段前锚点失败", err)
	}
	res := replayChain(rows, from, to, anchor)
	if !res.intact() {
		return fail(actionArchiveVerified, "哈希链重放未通过",
			fmt.Errorf("%w: %s", model.ErrArchiveRangeGap, res.Broken.String()))
	}
	lastRow := rows[len(rows)-1]

	// 清单对象：整段搬运的证据（entry_id/seq/entry_hash 逐行），verified 靠它自证。
	mw, err := d.storage.CreateObject(l.ctx, bucket, manifestKey)
	if err != nil {
		return fail(actionArchiveVerified, "创建归档清单对象失败", err)
	}
	if _, err := mw.Write(manifestBytes(rows)); err != nil {
		abortObject(l, mw)
		return fail(actionArchiveVerified, "写归档清单失败", err)
	}
	mloc, err := mw.Commit()
	if err != nil {
		abortObject(l, mw)
		return fail(actionArchiveVerified, "提交归档清单失败", err)
	}
	if mloc.ObjectKey != manifestKey || !isHashHex(mloc.SHA256Hex) {
		return fail(actionArchiveVerified, "清单对象自证不完整", model.ErrObjectStorageUnsupported)
	}
	if want := manifestHash(rows); want != mloc.SHA256Hex {
		// 对象侧算出的摘要与本地重放的清单不一致：文件不可信，禁止推进 verified。
		return fail(actionArchiveVerified, "清单摘要不一致",
			fmt.Errorf("%w: 本地 %s 对象侧 %s", model.ErrBatchUnverified, want, mloc.SHA256Hex))
	}
	// 全量条目对象：归档后热表行虽不物理删除，但查询侧要能从冷存储把区间读回来。
	ew, err := d.storage.CreateObject(l.ctx, bucket, entriesKey)
	if err != nil {
		return fail(actionArchiveVerified, "创建归档条目对象失败", err)
	}
	if _, err := writeExportRows(ew, "json", rows); err != nil {
		abortObject(l, ew)
		return fail(actionArchiveVerified, "写归档条目对象失败", err)
	}
	eloc, err := ew.Commit()
	if err != nil {
		abortObject(l, ew)
		return fail(actionArchiveVerified, "提交归档条目对象失败", err)
	}
	if eloc.ObjectKey != entriesKey || !isHashHex(eloc.SHA256Hex) {
		return fail(actionArchiveVerified, "条目对象自证不完整", model.ErrObjectStorageUnsupported)
	}

	ok, err = d.archives.TransitionState(l.ctx, b.BatchID, model.BatchStateWriting, model.BatchStateVerified,
		mloc.SHA256Hex, lastRow.EntryHash, "")
	if err != nil {
		return fail(actionArchiveVerified, "推进 verified 失败", err)
	}
	if !ok {
		return fail(actionArchiveVerified, "verified 未命中（批次被并发推进）", model.ErrTaskBadTransition)
	}
	b.State = model.BatchStateVerified
	b.ManifestHash = mloc.SHA256Hex
	b.LastEntryHash = lastRow.EntryHash
	trail(actionArchiveVerified, "归档清单校验通过", rpc.AuditResult_AUDIT_RESULT_OK, map[string]string{
		"manifest_hash":   b.ManifestHash,
		"last_entry_hash": b.LastEntryHash,
		"row_count":       strconv.FormatInt(size, 10),
	})

	if in.GetPurgeHot() {
		if d.purge == nil {
			return nil, fmt.Errorf("%w: 未装配 PurgeMark，不能标记热表已归档", model.ErrBatchUnverified)
		}
		marked, perr := d.purge(l.ctx, b, d.now())
		if perr != nil {
			// 停在 verified：清单是真的、热表行还在，这是最接近事实的状态。
			l.Errorf("ArchiveAuditEntries 标记热表 archived_at 失败 batch_id=%d err=%v", b.BatchID, perr)
			trail(actionArchivePurged, "标记热表已归档失败", rpc.AuditResult_AUDIT_RESULT_ERROR,
				map[string]string{"state": b.State})
			return nil, perr
		}
		ok, err = d.archives.TransitionState(l.ctx, b.BatchID, model.BatchStateVerified, model.BatchStatePurged,
			mloc.SHA256Hex, lastRow.EntryHash, "")
		if err != nil || !ok {
			// 行已标记而状态没推进：必须 Error 报警，否则覆盖率统计会悄悄偏低。
			l.Errorf("ArchiveAuditEntries 已标记 %d 行但批次未推进到 purged batch_id=%d err=%v",
				marked, b.BatchID, err)
			trail(actionArchivePurged, "批次推进 purged 失败", rpc.AuditResult_AUDIT_RESULT_ERROR,
				map[string]string{"state": b.State, "marked": strconv.FormatInt(marked, 10)})
			if err == nil {
				err = model.ErrTaskBadTransition
			}
			return nil, err
		}
		b.State = model.BatchStatePurged
		trail(actionArchivePurged, "热表行已标记 archived_at", rpc.AuditResult_AUDIT_RESULT_OK, map[string]string{
			"marked":    strconv.FormatInt(marked, 10),
			"row_count": strconv.FormatInt(size, 10),
		})
	}

	fresh, err := d.archives.FindOne(l.ctx, b.BatchID)
	if err != nil {
		l.Errorf("ArchiveAuditEntries 回读批次失败 batch_id=%d err=%v", b.BatchID, err)
		return nil, err
	}
	return &rpc.ArchiveAuditEntriesReply{Batch: archiveBatchView(fresh), Reused: false}, nil
}

// trail 写一条归档过程的自审计条目。留痕失败只打 Error：批次状态已经落库，
// 回滚反而会把「清单已落地」这个事实抹掉，那比留痕缺口更糟。
func (l *ArchiveAuditEntriesLogic) trail(d deps, cc *rpc.CallContext, b *model.ArchiveBatch,
	action, reason string, res rpc.AuditResult, after map[string]string) {
	if after == nil {
		after = map[string]string{"state": b.State}
	}
	selfAuditLogged(l.ctx, l.Logger, cc, d, selfAuditSpec{
		Action:     action,
		Domain:     selfDomainSystem,
		TargetType: targetArchiveBatch,
		TargetID:   strconv.FormatInt(b.BatchID, 10),
		Reason:     reason,
		Result:     res,
		Before: map[string]string{
			"chain_key": b.ChainKey,
			"from_seq":  strconv.FormatInt(b.FromSeq, 10),
			"to_seq":    strconv.FormatInt(b.ToSeq, 10),
		},
		After: after,
		Nonce: b.BatchID,
	})
}

// abortObject 尽力放弃半成品对象（绝不留下「库里记 verified、桶里半截文件」）。
func abortObject(lg logx.Logger, w objectWriter) {
	if err := w.Abort(); err != nil {
		lg.Errorf("归档对象 Abort 失败 err=%v", err)
	}
}
