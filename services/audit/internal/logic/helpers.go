// 本文件是 logic 包的手写扩展（入参校验、哈希链装配、状态机裁决、产物格式化），
// 不是 goctl 生成产物。goctl 只生成 internal/logic 下的用例骨架，本文件可自由维护。
//
// 分工（AGENTS.md §4）：SQL 一律在 model；这里只做「校验 + 状态机/幂等判定 + 纯计算」。
// 所有函数都设计成可在无 MySQL、无 gRPC、无 Redis、无 time.Sleep 的条件下被表驱动单测钉死，
// 因此时间戳与随机量都通过 deps.now / nonce 参数注入。

package logic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/audit/internal/repository"
	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// --- 1. 数据面装配 ---

// transact 是事务执行器签名。生产实现来自 sqlx.SqlConn.TransactCtx；
// 单测注入 fake，从而在不连 MySQL 的前提下覆盖「锁链头 → 写条目 → 推链头」的编排。
type transact func(ctx context.Context, fn func(ctx context.Context, session sqlx.Session) error) error

// objectLocation 是一个已落地对象的定位与自证信息。
type objectLocation struct {
	Bucket    string
	ObjectKey string
	Size      int64
	SHA256Hex string
}

// objectWriter 是流式对象写入器。logic 逐批喂字节，实现方负责累积 sha256/size。
// Commit 之前对象不允许被引用：半途失败只 Abort，绝不留下「库里有记录、桶里没文件」的假产物。
type objectWriter interface {
	io.Writer
	Commit() (objectLocation, error)
	Abort() error
}

// objectStore 是 logic 对对象存储的全部依赖面（导出文件与归档清单都经它落地）。
type objectStore interface {
	CreateObject(ctx context.Context, bucket, objectKey string) (objectWriter, error)
	PresignDownload(ctx context.Context, bucket, objectKey string, ttlSeconds int64) (string, int64, error)
}

// unconfiguredStore 是本期的唯一生产实现：
//   - Storage.Enabled=false → model.ErrObjectStorageMissing（配置未开）；
//   - 已开但本服务未引入 S3/OSS 客户端 → model.ErrObjectStorageUnsupported。
//
// 两条分支都必须显式失败：写「已导出/已归档」的元数据而桶里没有对象，
// 等于伪造证据链产物，比直接报错严重得多（AGENTS.md §9）。
type unconfiguredStore struct {
	conf repository.StorageConf
}

func (s unconfiguredStore) CreateObject(context.Context, string, string) (objectWriter, error) {
	return nil, s.err()
}

func (s unconfiguredStore) PresignDownload(context.Context, string, string, int64) (string, int64, error) {
	return "", 0, s.err()
}

func (s unconfiguredStore) err() error {
	if !s.conf.Enabled {
		return model.ErrObjectStorageMissing
	}
	return model.ErrObjectStorageUnsupported
}

// deps 汇总一个用例需要的全部外部能力。
type deps struct {
	entries  model.AuditEntryModel
	chains   model.AuditChainHeadModel
	exports  model.ExportTaskModel
	policies model.RetentionPolicyModel
	archives model.ArchiveBatchModel

	storage objectStore
	// purge 生产指向 repository.PurgeMark：「必须先 verified + manifest_hash 非空」
	// 这条防线写死在 repository 里，不受 Archive.VerifyBeforePurge 配置影响。
	purge    func(ctx context.Context, batch *model.ArchiveBatch, ts int64) (int64, error)
	transact transact
	// now 注入时钟：哈希链分链按 UTC 日、归档按保留天数、签名地址按 TTL，
	// 全都依赖「现在是几点」，不注入就只能靠 sleep 或真实时间写测试。
	now  func() int64
	opts repository.Options

	// clampPage / checkQueryWindow / maxRangeSeconds 直接转发 repository 上已有的纯函数。
	// logic 不复写一遍规则（同一规则两处实现迟早漂移），但又不该在单测里为了调这三个
	// 方法去构造一个真 Repository，因此仍以函数字段注入。
	clampPage        func(pn, ps int32) (int32, int32, error)
	checkQueryWindow func(startAt, endAt int64, narrowed bool) error
	maxRangeSeconds  func() int64
}

// realDeps 从 ServiceContext 装配数据面。
func realDeps(svcCtx *svc.ServiceContext) deps {
	r := svcCtx.Repository
	o := r.Options()
	store := unconfiguredStore{conf: o.Storage}
	return deps{
		entries:  r.Entries,
		chains:   r.Chains,
		exports:  r.Exports,
		policies: r.Policies,
		archives: r.Archives,
		storage:  store,
		purge:    r.PurgeMark,
		transact: func(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
			return r.Conn().TransactCtx(ctx, fn)
		},
		now:  model.NowUnix,
		opts: o,

		clampPage:        r.ClampPage,
		checkQueryWindow: r.CheckQueryWindow,
		maxRangeSeconds:  r.MaxRangeSeconds,
	}
}

// buildDeps 是 logic 读取数据面的唯一入口，也是本包唯一的测试缝：
// 单测把它替换成注入 fake model 的版本，生产不替换。
var buildDeps = realDeps

// --- 2. 入参校验 ---

// textLen 按 rune 计数：VARCHAR(n) 的 n 是字符数，按字节判会把中文误判成超限，
// 反过来按「字符数放过而按字节截断」又会切出半个字。两端都用 rune 才自洽。
func textLen(s string) int { return utf8.RuneCountInString(s) }

func checkLen(field, value string, max int) error {
	if n := textLen(value); n > max {
		return fmt.Errorf("%w: %s length=%d max=%d", model.ErrFieldTooLong, field, n, max)
	}
	return nil
}

// validateCallContext 校验每个方法都必带的 CallContext。
// needRequestID 对应 proto 注释「写接口（Append/BatchAppend/CreateExport/Save/Archive）必填」。
func validateCallContext(cc *rpc.CallContext, needRequestID bool) error {
	if cc == nil {
		return fmt.Errorf("%w: ctx with caller_service is required", model.ErrRequestRequired)
	}
	caller := strings.TrimSpace(cc.GetCallerService())
	if caller == "" {
		return model.ErrCallerRequired
	}
	if err := checkLen("ctx.caller_service", caller, model.MaxCallerNameBytes); err != nil {
		return err
	}
	if err := checkLen("ctx.trace_id", strings.TrimSpace(cc.GetTraceId()), model.MaxTraceIDBytes); err != nil {
		return err
	}
	rid := strings.TrimSpace(cc.GetRequestId())
	if err := checkLen("ctx.request_id", rid, model.MaxRequestIDBytes); err != nil {
		return err
	}
	if needRequestID && rid == "" {
		return model.ErrRequestIDRequired
	}
	if cc.GetOperatorId() < 0 {
		return fmt.Errorf("%w: ctx.operator_id=%d", model.ErrActorIDInvalid, cc.GetOperatorId())
	}
	return nil
}

// reasonLimit 收敛 reason 的入库上限：配置缺省或大于列宽都夹回列宽。
// 直接用配置值有两处危险：配 0 等于完全不校验；配得比 VARCHAR(512) 还大
// 会让写入一路撞到 1406 才失败，而那时链头已经推进、条目却没落库。
func (d deps) reasonLimit() int {
	lim := d.opts.MaxReasonLen
	if lim <= 0 || lim > model.MaxReasonBytes {
		lim = model.MaxReasonBytes
	}
	return lim
}

// scanLimit 收敛导出单批行数：请求值 <=0 或超过配置时按配置，再夹到 model 的硬上限。
func (d deps) scanLimit(requested int32) int32 {
	lim := int64(d.opts.ExportBatchRows)
	if lim <= 0 {
		lim = 1
	}
	if requested > 0 && int64(requested) < lim {
		lim = int64(requested)
	}
	if lim > model.MaxScanBatchRows {
		lim = model.MaxScanBatchRows
	}
	return int32(lim)
}

// verifyLimit 收敛单次链校验的条数上限。
func (d deps) verifyLimit(requested int32) int32 {
	lim := int64(d.opts.MaxVerifyEntries)
	if lim <= 0 {
		lim = 1
	}
	if requested > 0 && int64(requested) < lim {
		lim = int64(requested)
	}
	return int32(lim)
}

// archiveLimit 收敛单归档批次条数上限。
func (d deps) archiveLimit() int64 {
	lim := int64(d.opts.ArchiveMaxEntries)
	if lim <= 0 {
		lim = 1
	}
	return lim
}

// --- 3. 条目装配（校验 + 脱敏 + 哈希输入） ---

// prepareEntry 把一条 RPC draft 转成待入库行。
// 链字段（ChainKey/Seq/PrevHash/EntryHash）里的 Seq/PrevHash/EntryHash 留空，
// 由 assignChainSegments 在锁住链头之后填——在锁外算哈希等于假设没人并发。
func (d deps) prepareEntry(cc *rpc.CallContext, draft *rpc.AuditEntryDraft) (*model.AuditEntry, error) {
	if draft == nil {
		return nil, model.ErrDraftRequired
	}
	// 盐缺失一律拒写：裸 SHA-256(IP) 可被 2^32 枚举反查，退化成弱哈希比不写更糟。
	if len(d.opts.IpHashSalt) == 0 {
		return nil, model.ErrHashSaltMissing
	}
	row := &model.AuditEntry{
		EventID:       strings.TrimSpace(draft.GetEventId()),
		ActorName:     strings.TrimSpace(draft.GetActorName()),
		Action:        strings.TrimSpace(draft.GetAction()),
		ActionDomain:  strings.TrimSpace(draft.GetActionDomain()),
		TargetType:    strings.TrimSpace(draft.GetTargetType()),
		TargetID:      strings.TrimSpace(draft.GetTargetId()),
		BeforeDigest:  strings.TrimSpace(draft.GetBeforeDigest()),
		AfterDigest:   strings.TrimSpace(draft.GetAfterDigest()),
		Reason:        strings.TrimSpace(draft.GetReason()),
		ActorType:     int32(draft.GetActorType()),
		ActorID:       draft.GetActorId(),
		Result:        int32(draft.GetResult()),
		SourceApp:     int32(draft.GetSourceApp()),
		SchemaVersion: draft.GetSchemaVersion(),
		OccurredAt:    draft.GetOccurredAt(),
	}
	if row.EventID == "" {
		return nil, model.ErrEventIDRequired
	}
	if err := checkLen("entry.event_id", row.EventID, model.MaxEventIDBytes); err != nil {
		return nil, err
	}
	if row.SchemaVersion == 0 {
		row.SchemaVersion = model.SchemaVersion
	}
	if row.SchemaVersion != model.SchemaVersion {
		return nil, fmt.Errorf("%w: schema_version=%d", model.ErrSchemaVersionUnsupported, row.SchemaVersion)
	}
	if !model.ValidActorType(row.ActorType) {
		return nil, model.ErrActorUnspecified
	}
	if row.ActorID < 0 {
		return nil, fmt.Errorf("%w: actor_id=%d", model.ErrActorIDInvalid, row.ActorID)
	}
	if row.ActorID == 0 && row.ActorType != model.ActorUnknown && row.ActorType != model.ActorSystem {
		return nil, fmt.Errorf("%w: actor_type=%d 下 actor_id 不可为 0", model.ErrActorUnspecified, row.ActorType)
	}
	if !model.ValidAction(row.Action) {
		return nil, fmt.Errorf("%w: action=%q", model.ErrActionInvalid, row.Action)
	}
	if err := checkLen("entry.action", row.Action, model.MaxActionBytes); err != nil {
		return nil, err
	}
	if !model.ValidActionDomain(row.ActionDomain) {
		return nil, fmt.Errorf("%w: action_domain=%q", model.ErrActionDomainInvalid, row.ActionDomain)
	}
	if !model.ValidResult(row.Result) {
		return nil, model.ErrResultRequired
	}
	if !model.ValidSourceApp(row.SourceApp) {
		return nil, model.ErrSourceAppRequired
	}
	if row.TargetType == "" && row.TargetID != "" {
		return nil, fmt.Errorf("%w: target_id 必须与 target_type 同时给出", model.ErrActionRequired)
	}
	for _, lim := range []struct {
		field string
		value string
		max   int
	}{
		{"entry.target_type", row.TargetType, model.MaxTargetTypeBytes},
		{"entry.target_id", row.TargetID, model.MaxTargetIDBytes},
		{"entry.before_digest", row.BeforeDigest, model.MaxDigestBytes},
		{"entry.after_digest", row.AfterDigest, model.MaxDigestBytes},
		{"entry.reason", row.Reason, d.reasonLimit()},
	} {
		if err := checkLen(lim.field, lim.value, lim.max); err != nil {
			return nil, err
		}
	}
	// 摘要走白名单格式（黑名单永远列不全），命中明文 PII 直接整条拒写，
	// 不做「脱敏后入库」的降级：降级会把「调用方传错」变成「服务端悄悄改数据」。
	for _, dg := range []struct {
		field string
		value string
	}{{"before_digest", row.BeforeDigest}, {"after_digest", row.AfterDigest}} {
		if !model.ValidDigest(dg.value) {
			return nil, fmt.Errorf("%w: %s=%q", model.ErrDigestInvalid, dg.field, dg.value)
		}
	}
	for _, free := range []struct {
		field string
		value string
	}{{"reason", row.Reason}, {"actor_name", row.ActorName}} {
		if model.LooksLikePII(free.value) {
			return nil, fmt.Errorf("%w: entry.%s 命中明文敏感信息形态", model.ErrDigestLooksPII, free.field)
		}
	}
	// actor_name 不参与哈希（改名不追改历史条目），因此允许按 rune 截断；
	// reason 参与哈希，只能拒绝不能截断，否则哈希与库值不一致。
	row.ActorName = model.TruncateRunes(row.ActorName, model.MaxActorNameBytes)

	if row.OccurredAt == 0 {
		row.OccurredAt = d.now()
	}
	if row.OccurredAt > d.now()+model.MaxFutureOccurredSkewSeconds {
		return nil, fmt.Errorf("%w: occurred_at=%d now=%d", model.ErrOccurredAtFuture, row.OccurredAt, d.now())
	}
	row.ChainKey = model.ChainKey(row.ActionDomain, row.OccurredAt)
	if err := checkLen("chain_key", row.ChainKey, model.MaxChainKeyBytes); err != nil {
		return nil, err
	}

	// 来源标识只在入参出现，落库前转加盐短哈希；原文明文连局部变量都不留。
	if ip := strings.TrimSpace(cc.GetIp()); ip != "" {
		row.IPHash = d.hashSource(ip)
	}
	if dev := strings.TrimSpace(draft.GetDeviceId()); dev != "" {
		row.DeviceHash = d.hashSource(dev)
	}
	if row.IPHash == "" && strings.TrimSpace(cc.GetIp()) != "" {
		// 有原文却没算出哈希 = 盐被中途清空，宁可拒写也不落空值。
		return nil, model.ErrHashSaltMissing
	}
	row.TraceID = strings.TrimSpace(cc.GetTraceId())
	row.RequestID = strings.TrimSpace(cc.GetRequestId())
	row.CallerService = strings.TrimSpace(cc.GetCallerService())
	row.Ctime = d.now()
	return row, nil
}

// hashSource 计算加盐短哈希；盐缺失时返回空串（调用方已在 prepareEntry 判过）。
func (d deps) hashSource(value string) string {
	if len(d.opts.IpHashSalt) == 0 {
		return ""
	}
	return model.ShortHash(string(d.opts.IpHashSalt), value)
}

// --- 4. 哈希链装配与追加 ---

// groupByChain 按 chain_key 分组，并返回链名升序的 key 列表。
// 固定加锁顺序是防死锁的唯一手段：两批写入若按各自到达顺序锁 A、B 两条链，
// 交叉执行就会互相等待，唯一键冲突只会在锁之后才暴露。
func groupByChain(rows []*model.AuditEntry) ([]string, map[string][]*model.AuditEntry) {
	groups := make(map[string][]*model.AuditEntry, 4)
	for _, r := range rows {
		groups[r.ChainKey] = append(groups[r.ChainKey], r)
	}
	order := make([]string, 0, len(groups))
	for k := range groups {
		order = append(order, k)
	}
	sort.Strings(order)
	return order, groups
}

// assignChainSegments 是纯函数：给定各链当前尾部状态与待写行，算出
// seq / prev_hash / entry_hash，并返回推进后的链头。
//
// 它是「哈希链连续性」的唯一实现处：
//   - 链头 Seq=0（新链）时 prev_hash 取 GenesisHash(chain_key)，创世摘要按链隔离，
//     所以把别的链整段搬过来必然断链；
//   - 组内按传入顺序连续编号，后一条的 prev_hash 严格等于前一条的 entry_hash；
//   - 链头声称有行却没有 last_hash 时直接报错，绝不把空串当成合法前驱。
func assignChainSegments(head map[string]*model.ChainHead, order []string,
	groups map[string][]*model.AuditEntry) (map[string]*model.ChainHead, error) {
	updated := make(map[string]*model.ChainHead, len(order))
	for _, key := range order {
		head0, ok := head[key]
		if !ok || head0 == nil {
			return nil, fmt.Errorf("%w: chain_key=%s", model.ErrChainHeadMissing, key)
		}
		if head0.Seq > 0 && head0.LastHash == "" {
			return nil, fmt.Errorf("%w: chain_key=%s 链头有 %d 条却没有尾部摘要",
				model.ErrChainHeadMissing, key, head0.Seq)
		}
		prev := head0.LastHash
		if prev == "" {
			prev = model.GenesisHash(key)
		}
		next := head0.Seq
		firstAt := head0.FirstAt
		for _, row := range groups[key] {
			if row.ChainKey != key {
				return nil, fmt.Errorf("audit: 分组键 %s 里混入了 chain_key=%s", key, row.ChainKey)
			}
			next++
			row.Seq = next
			row.PrevHash = prev
			row.EntryHash = model.ComputeEntryHash(row)
			prev = row.EntryHash
			if firstAt == 0 {
				firstAt = row.OccurredAt
			}
		}
		updated[key] = &model.ChainHead{
			ChainKey:    key,
			Seq:         next,
			EntryCount:  head0.EntryCount + int64(len(groups[key])),
			LastHash:    prev,
			FirstAt:     firstAt,
			Mtime:       head0.Mtime,
			LastEntryID: head0.LastEntryID,
		}
	}
	return updated, nil
}

// appendOnce 在一个事务里完成「建链 → 锁链头 → 算哈希 → 插条目 → 推链头」。
// 多条链按链名升序逐个加锁，锁全部拿到后才开始写入，避免交叉死锁。
func (d deps) appendOnce(ctx context.Context, rows []*model.AuditEntry) error {
	return d.transact(ctx, func(ctx context.Context, session sqlx.Session) error {
		order, groups := groupByChain(rows)
		for _, key := range order {
			if err := d.chains.Ensure(ctx, session, key); err != nil {
				return err
			}
		}
		heads := make(map[string]*model.ChainHead, len(order))
		for _, key := range order {
			h, err := d.chains.LockForUpdate(ctx, session, key)
			if err != nil {
				return err
			}
			heads[key] = h
		}
		updated, err := assignChainSegments(heads, order, groups)
		if err != nil {
			return err
		}
		for _, key := range order {
			for _, row := range groups[key] {
				id, err := d.entries.Insert(ctx, session, row)
				if err != nil {
					return err
				}
				// LastEntryID 取本组最后一条：链尾指针必须指向真实存在的行。
				_ = id
			}
			last := groups[key][len(groups[key])-1]
			newHead := updated[key]
			newHead.LastEntryID = last.EntryID
			ok, err := d.chains.Advance(ctx, session, key, heads[key].Seq, newHead)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: chain_key=%s expect_seq=%d", model.ErrChainConflict, key, heads[key].Seq)
			}
		}
		return nil
	})
}

// appendCommitted 带链头冲突重试地提交一批条目。
//
// 重试判据只认 model.ErrChainConflict：其它错误重试等于把参数错误伪装成瞬时故障。
// 但唯一键冲突（并发把同一 event_id 抢先写了）在 sqlx 层是驱动专有错误，
// 因此失败后先回查一次幂等键：若全部已落库就按「幂等回放」成功返回，
// 查不齐才原样报错——既不吞错误，也不让并发重试产生重复条目。
func (d deps) appendCommitted(ctx context.Context, rows []*model.AuditEntry) (int32, error) {
	attempts := d.opts.ChainRetry
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		err := d.appendOnce(ctx, rows)
		if err == nil {
			return int32(len(rows)), nil
		}
		lastErr = err
		if errors.Is(err, model.ErrChainConflict) {
			continue
		}
		return 0, err
	}
	return 0, lastErr
}

// appendIdempotent 先按 event_id 预检幂等，再链上追加，返回与入参同序的结果行。
//
// 幂等的事实来源是 audit_entry.uniq_event_id 唯一索引（AGENTS.md §5），
// 预检只是让重复投递少打一次库；预检漏掉的并发写入由 appendCommitted 的回查兜底。
func (d deps) appendIdempotent(ctx context.Context, rows []*model.AuditEntry) ([]*model.AuditEntry, int32, int32, error) {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.EventID)
	}
	existing, err := d.entries.FindByEventIDs(ctx, ids)
	if err != nil {
		return nil, 0, 0, err
	}
	pending := make([]*model.AuditEntry, 0, len(rows))
	out := make([]*model.AuditEntry, len(rows))
	var reused int32
	for i, r := range rows {
		if e, ok := existing[r.EventID]; ok && e != nil {
			out[i] = e
			reused++
			continue
		}
		pending = append(pending, r)
	}
	if len(pending) > 0 {
		accepted, err := d.appendCommitted(ctx, pending)
		if err != nil {
			// 并发窗口：事务失败可能正是因为别人刚把同一 event_id 写进去了。
			recheck, rerr := d.recheckExisting(ctx, pending)
			if rerr != nil {
				return nil, 0, 0, errors.Join(err, rerr)
			}
			if recheck != nil {
				out = mergeResults(rows, recheck)
				return out, 0, int32(len(rows)), nil
			}
			return nil, 0, 0, err
		}
		j := 0
		for i := range rows {
			if out[i] != nil {
				continue
			}
			out[i] = pending[j]
			j++
		}
		return out, accepted, reused, nil
	}
	return out, 0, reused, nil
}

// recheckExisting 回查一批待写条目是否已被并发写入；全部命中才返回既有行。
// 只要有一条确实不存在就返回 nil，让上层把原始错误如实抛出（不吞错误）。
func (d deps) recheckExisting(ctx context.Context, pending []*model.AuditEntry) ([]*model.AuditEntry, error) {
	ids := make([]string, 0, len(pending))
	for _, r := range pending {
		ids = append(ids, r.EventID)
	}
	found, err := d.entries.FindByEventIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*model.AuditEntry, 0, len(pending))
	for _, r := range pending {
		e, ok := found[r.EventID]
		if !ok || e == nil {
			return nil, nil
		}
		out = append(out, e)
	}
	return out, nil
}

// mergeResults 把回查到的既有行按 event_id 对回原始顺序。
func mergeResults(rows, resolved []*model.AuditEntry) []*model.AuditEntry {
	byEvent := make(map[string]*model.AuditEntry, len(resolved))
	for _, r := range resolved {
		byEvent[r.EventID] = r
	}
	out := make([]*model.AuditEntry, 0, len(rows))
	for _, r := range rows {
		if e, ok := byEvent[r.EventID]; ok {
			out = append(out, e)
			continue
		}
		out = append(out, r)
	}
	return out
}

// --- 5. 自审计（读侧与运维动作留痕） ---

// 本服务自身的动作域划分（写死成常量，避免每个用例自己拼字符串）：
//   - data_access：任何「看到存证内容」的路径（查单条、查列表、换下载地址）
//   - system：完整性与容量治理动作（链校验、归档、保留期变更）
const (
	selfDomainDataAccess = "data_access"
	selfDomainSystem     = "system"

	selfCallerService = "audit"

	actionEntryGet        = "audit.entry.get"
	actionEntryList       = "audit.entry.list"
	actionChainVerify     = "audit.chain.verify"
	actionExportSubmit    = "audit.export.submit"
	actionExportFinish    = "audit.export.finish"
	actionExportDownload  = "audit.export.download"
	actionRetentionSave   = "audit.retention.save"
	actionRetentionLoosen = "audit.retention.loosen"
	actionArchiveWriting  = "audit.archive.writing"
	actionArchiveVerified = "audit.archive.verified"
	actionArchivePurged   = "audit.archive.purged"
)

// selfAuditSpec 描述一条自审计条目的意图。
// 维度一律经 digestOf 压成 `字段名=16 位十六进制`，自由文本只留 reason：
// 过滤条件本身可能是「按手机号查审计」，它不能以原文进库（AGENTS.md §7）。
type selfAuditSpec struct {
	Action     string
	Domain     string
	TargetType string
	TargetID   string
	Reason     string
	Before     map[string]string
	After      map[string]string
	// Result 留痕本身的结果码。默认（0）按 OK 记：绝大多数自审计就是「做成了这件事」。
	// 失败路径必须显式传 AUDIT_RESULT_ERROR，否则会写出一条「声称成功」的假留痕。
	Result rpc.AuditResult
	// Nonce 让「同一秒内两次独立读」产生不同 event_id（调用方没给 request_id 时才有意义）。
	Nonce int64
}

// digestOf 把任意长度的维度值压成 16 位十六进制（sha256 前缀）。
// 取 16 位与 model.ValidDigest 的字段摘要格式一致：审计维度只做等值比对，
// 16 hex = 64 bit，碰撞概率对「同一管理员同一秒查同一条件」完全够用。
func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

// digestPairs 把维度映射渲染成白名单摘要文本（键升序，保证同一条件恒定产出同一串）。
//
// 长度受 before_digest/after_digest 列宽约束（255 字符）。列宽不够时必须丢维度，
// 但不能「悄悄丢」：被丢掉的维度整体折成一个 more=<16hex> 尾巴，
// 这样「同一组查询条件」仍然恒定映射到同一串文本，事后比对不会因为截断位置而误判。
const digestPairsBudget = 250

func digestPairs(pairs map[string]string) string {
	if len(pairs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(pairs))
	for k, v := range pairs {
		if v == "" || !model.ValidDigestFieldName(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	used := 0
	var dropped []string
	for _, k := range keys {
		pair := k + "=" + digestOf(pairs[k])
		if used+len(pair)+1 > digestPairsBudget {
			dropped = append(dropped, pair)
			continue
		}
		used += len(pair) + 1
		out = append(out, pair)
	}
	if len(dropped) > 0 {
		out = append(out, "more="+digestOf(strings.Join(dropped, ";")))
	}
	return strings.Join(out, ";")
}

// selfAuditEventID 由动作 + 幂等键派生 event_id。
// 说明：这里的哈希是「标识派生」而不是隐私脱敏，因此不需要盐；
// 真正的来源标识脱敏走 model.ShortHash（带盐），两者不要混淆。
func selfAuditEventID(spec selfAuditSpec, requestID string, now int64) string {
	seed := spec.Action + "|" + requestID + "|" + strconv.FormatInt(now, 10) + "|" + strconv.FormatInt(spec.Nonce, 10)
	sum := sha256.Sum256([]byte(seed))
	return spec.Action + "/" + hex.EncodeToString(sum[:])[:32]
}

// appendSelfAudit 写一条本服务的自审计条目。
//
// 递归防线（README「已知缺口」里那条待评审项的实现约定）：
// 自审计只经 prepareEntry + appendIdempotent 直接上链，不调用任何读接口，
// 而读接口是唯一会触发 appendSelfAudit 的地方，因此深度恒为 1。
func (d deps) appendSelfAudit(ctx context.Context, cc *rpc.CallContext, spec selfAuditSpec) error {
	if spec.Action == "" || spec.Domain == "" {
		return fmt.Errorf("%w: 自审计条目缺少 action 或 action_domain", model.ErrActionRequired)
	}
	now := d.now()
	eventID := selfAuditEventID(spec, strings.TrimSpace(cc.GetRequestId()), now)
	actorType := int32(rpc.ActorType_ACTOR_TYPE_SYSTEM)
	if cc.GetOperatorId() > 0 {
		actorType = int32(rpc.ActorType_ACTOR_TYPE_ADMIN)
	}
	result := spec.Result
	if result == rpc.AuditResult_AUDIT_RESULT_UNSPECIFIED {
		result = rpc.AuditResult_AUDIT_RESULT_OK
	}
	draft := &rpc.AuditEntryDraft{
		EventId:      eventID,
		ActorType:    rpc.ActorType(actorType),
		ActorId:      cc.GetOperatorId(),
		Action:       spec.Action,
		ActionDomain: spec.Domain,
		TargetType:   spec.TargetType,
		TargetId:     spec.TargetID,
		Result:       result,
		BeforeDigest: digestPairs(spec.Before),
		AfterDigest:  digestPairs(spec.After),
		Reason:       spec.Reason,
		SourceApp:    rpc.SourceApp_SOURCE_APP_INTERNAL_RPC,
		OccurredAt:   now,
	}
	row, err := d.prepareEntry(selfAuditContext(cc), draft)
	if err != nil {
		return err
	}
	_, _, _, err = d.appendIdempotent(ctx, []*model.AuditEntry{row})
	return err
}

// selfAuditContext 把调用上下文改写成本服务自审计的归因：
// caller_service 固定为 "audit"（回答「这条是谁写的」），request_id 沿用原请求
// （让「同一次调用的自审计」可被幂等去重），ip 不透传（自审计不该复制来源标识）。
func selfAuditContext(cc *rpc.CallContext) *rpc.CallContext {
	return &rpc.CallContext{
		CallerService: selfCallerService,
		OperatorId:    cc.GetOperatorId(),
		TraceId:       cc.GetTraceId(),
		RequestId:     cc.GetRequestId(),
	}
}

// 自审计条目的 target_type 取值：与 audit_* 表名一致，
// 这样「按 target_type 筛审计」就能直接回答「这类对象被动过哪些操作」。
const (
	targetAuditEntry      = "audit.entry"
	targetAuditChain      = "audit.chain"
	targetExportTask      = "audit.export_task"
	targetRetentionPolicy = "audit.retention_policy"
	targetArchiveBatch    = "audit.archive_batch"
)

// selfAuditLogged 尽力写一条自审计条目，失败只打 Error 日志。
//
// 为什么吞掉错误但不静默：读接口的结果不该被「留痕写失败」这件事改掉
// （否则哈希链一抖动整后台就查不到数据，故障面比审计缺口大得多）；
// 但审计缺口必须显式可见，所以固定 Error 级 + 带定位键，让监控能报警。
// 写侧用例（导出/归档/保留策略）不用这个 helper：那里的留痕失败要按各自语义处理。
func selfAuditLogged(ctx context.Context, lg logx.Logger, cc *rpc.CallContext, d deps, spec selfAuditSpec) {
	if lg == nil {
		lg = logx.WithContext(ctx)
	}
	if err := d.appendSelfAudit(ctx, cc, spec); err != nil {
		lg.Errorf("audit 自审计写入失败 action=%s domain=%s target=%s request_id=%s err=%v",
			spec.Action, spec.Domain, spec.TargetID, cc.GetRequestId(), err)
	}
}

// --- 6. 链校验重放 ---

// 断点原因文本，取值与 rpc.VerifyAuditChainReply.broken_reason 注释一致。
const (
	brokenSeqGap            = "seq_gap"
	brokenPrevHashMismatch  = "prev_hash_mismatch"
	brokenEntryHashMismatch = "entry_hash_mismatch"
	brokenTruncated         = "truncated"
)

// chainBreak 是一条断点的定位信息。EntryID 为 0 表示「该有的行根本不在表里」。
type chainBreak struct {
	Seq     int64
	EntryID int64
	Reason  string
}

func (b *chainBreak) String() string {
	if b == nil {
		return ""
	}
	return fmt.Sprintf("seq=%d entry_id=%d reason=%s", b.Seq, b.EntryID, b.Reason)
}

// replayResult 是重放结论。
type replayResult struct {
	Checked   int64
	Broken    *chainBreak
	LastHash  string
	LastSeq   int64
	Truncated bool
}

func (r replayResult) intact() bool { return r.Broken == nil && !r.Truncated }

// replayChain 按 seq 升序重放 V1_SERIALIZATION 并逐条判定。
//
// 参数语义：
//   - fromSeq：本次判定的起始序号（已补全，恒 >= 1）；
//   - anchorHash：fromSeq 之前那条的 entry_hash。空串表示「无从比对第一行的 prev_hash」，
//     对应契约缺口的场景（VerifyAuditChainReq 没有 expected_prev_hash 字段），
//     此时第一行只校验 seq 与自身 entry_hash，不做 prev 判定——
//     假装校验通过比明说「这一环没验」危险得多。
//   - toSeq：期望覆盖到的序号（已按链头补全）。返回的行数不足或末号不足即 seq_gap。
//
// 判定在首个断点处停止：继续往下只会把同一处篡改重复报成 N 条异常。
func replayChain(rows []*model.AuditEntry, fromSeq, toSeq int64, anchorHash string) replayResult {
	res := replayResult{}
	expectSeq := fromSeq
	prev := anchorHash
	for _, row := range rows {
		if row.Seq != expectSeq {
			// 中间的洞：库里根本没有这一号（或序号被改写）。EntryID 未知，报 0。
			res.Broken = &chainBreak{Seq: expectSeq, Reason: brokenSeqGap}
			return res
		}
		if prev != "" && row.PrevHash != prev {
			res.Broken = &chainBreak{Seq: row.Seq, EntryID: row.EntryID, Reason: brokenPrevHashMismatch}
			return res
		}
		if model.ComputeEntryHash(row) != row.EntryHash {
			res.Broken = &chainBreak{Seq: row.Seq, EntryID: row.EntryID, Reason: brokenEntryHashMismatch}
			return res
		}
		res.Checked++
		res.LastHash = row.EntryHash
		res.LastSeq = row.Seq
		prev = row.EntryHash
		expectSeq++
	}
	if res.Broken != nil {
		return res
	}
	if res.LastSeq < toSeq {
		// 尾部缺行：整段被截断或删除，第一个丢失的序号就是 res.LastSeq+1（区间内无行时是 fromSeq）。
		missing := res.LastSeq + 1
		if res.Checked == 0 {
			missing = fromSeq
		}
		res.Broken = &chainBreak{Seq: missing, Reason: brokenSeqGap}
	}
	return res
}

// markTruncated 在达到单次条数上限时给出 truncated 结论（调用方已停止取行）。
func (r *replayResult) markTruncated() {
	r.Truncated = true
	if r.Broken == nil {
		r.Broken = &chainBreak{Seq: r.LastSeq + 1, Reason: brokenTruncated}
	}
}

// brokenReason 安全取断点原因文本（无断点时为空串），避免上层到处写 nil 判定。
func (r replayResult) brokenReason() string {
	if r.Broken == nil {
		return ""
	}
	return r.Broken.Reason
}

// prevAnchor 取 from_seq 前一条的 entry_hash 作为重放起点；from=1 时取创世摘要。
// 前一条确实不存在（历史被截断 / 该号从未写过）时返回空串，让 replayChain 跳过
// 第一行的 prev 判定：没有锚点就是没有，绝不拿创世哈希去冒充一个不存在的上一环。
func (d deps) prevAnchor(ctx context.Context, chainKey string, fromSeq int64) (string, error) {
	if fromSeq <= 1 {
		return model.GenesisHash(chainKey), nil
	}
	rows, err := d.entries.ListByChainRange(ctx, chainKey, fromSeq-1, fromSeq-1, 1)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].EntryHash, nil
}

// --- 7. 导出任务 ---

// exportFilterSpec 是落进 audit_export_task.filter_json 的查询条件快照。
// 只带维度，不带任何原文值之外的东西：时间范围必填，因此快照本身也保证
// 「不存在导出全表的任务」。
type exportFilterSpec struct {
	StartAt      int64  `json:"start_at"`
	EndAt        int64  `json:"end_at"`
	ActorType    int32  `json:"actor_type,omitempty"`
	ActorID      int64  `json:"actor_id,omitempty"`
	Action       string `json:"action,omitempty"`
	ActionDomain string `json:"action_domain,omitempty"`
	TargetType   string `json:"target_type,omitempty"`
	TargetID     string `json:"target_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// encode 渲染 filter_json。键顺序由结构体字段顺序决定（encoding/json 稳定），
// 同一条件恒定产出同一文本，便于比对两次提交是否真的等价。
func (f exportFilterSpec) encode() (string, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("%w: %v", model.ErrExportFilterInvalid, err)
	}
	return string(b), nil
}

func parseExportFilter(raw string) (exportFilterSpec, error) {
	var f exportFilterSpec
	if strings.TrimSpace(raw) == "" {
		return f, fmt.Errorf("%w: 快照为空", model.ErrExportFilterInvalid)
	}
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return f, fmt.Errorf("%w: %v", model.ErrExportFilterInvalid, err)
	}
	if f.StartAt <= 0 || f.EndAt <= 0 || f.StartAt >= f.EndAt {
		return f, fmt.Errorf("%w: 时间范围缺失或倒置", model.ErrExportFilterInvalid)
	}
	if !f.narrowed() {
		return f, fmt.Errorf("%w: 缺少收窄维度", model.ErrExportFilterInvalid)
	}
	return f, nil
}

func (f exportFilterSpec) narrowed() bool {
	// 与 narrowedFilter.narrowed 保持同一条规则：target_type 单独给出即算收窄。
	// 裸 target_id 不算，因为不知道类型的 ID 用不上 idx_target 的复合前缀。
	return f.ActorID > 0 || f.Action != "" || f.ActionDomain != "" || f.TargetType != ""
}

// exportStopped 回报任务是否已不可能再被 RunAuditExportTask 推进，
// 也就是 RunAuditExportTaskReply.finished 的语义（proto 注释：「任务已进入 succeeded/failed」）。
//
// 刻意不复用 model.IsExportFinalState：那里的 succeeded 不算终态（对象到期还要流转 expired），
// 照搬会让一次成功的导出回 finished=false，与 proto 注释相反，
// 并把按 finished 停步的 cron 推进器骗进「同任务反复重跑直到报 ErrTaskBadTransition」。
// 这里判的是「acquire 还会不会接受这个状态」，与推进权取的是同一个口径。
func exportStopped(state string) bool {
	return state != model.ExportStatePending && state != model.ExportStateRunning
}

// bucket 返回生效的对象存储桶名；未配置桶时给出与写文件路径一致的显式错误，
// 绝不允许「元数据写了 bucket、桶里没对象」。
func (d deps) bucket() (string, error) {
	b := strings.TrimSpace(d.opts.Storage.Bucket)
	if b == "" {
		return "", model.ErrObjectStorageMissing
	}
	if err := checkLen("bucket", b, model.MaxBucketBytes); err != nil {
		return "", err
	}
	return b, nil
}

func (f exportFilterSpec) entryFilter(ps int32) model.EntryFilter {
	return model.EntryFilter{
		StartAt:      f.StartAt,
		EndAt:        f.EndAt,
		ActorType:    f.ActorType,
		ActorID:      f.ActorID,
		Action:       f.Action,
		ActionDomain: f.ActionDomain,
		TargetType:   f.TargetType,
		TargetID:     f.TargetID,
		Pn:           1,
		Ps:           ps,
	}
}

// piiFields 列出会被写进 filter_json 的自由文本，供 PII 扫描。
// 「按手机号筛审计」这个查询本身就是第二条泄露通道，所以条件值也必须过一遍判定。
func (f exportFilterSpec) piiFields() []string {
	return []string{f.Action, f.ActionDomain, f.TargetType, f.TargetID, f.Reason}
}

// sanitizeKeyPart 把幂等键压成可安全进对象键的字符集。
// request_id 由调用方决定，可能含 "/"、".."、空格；不清洗就能拼出越界对象键。
func sanitizeKeyPart(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	out := sb.String()
	if textLen(out) > 96 {
		out = out[:96]
	}
	return out
}

// exportObjectKey 生成导出对象键。
func exportObjectKey(requestID, format string) string {
	return "audit-export/" + sanitizeKeyPart(requestID) + "." + format
}

// archiveObjectKeys 生成归档清单与全量条目对象键。
// 表里只有一个 object_key 列（契约缺口，见 README），因此落两个对象时
// 只在列里存清单键，条目文件用同前缀的兄弟键，两者都能由区间反查。
func archiveObjectKeys(chainKey string, fromSeq, toSeq int64) (manifestKey, entriesKey string) {
	base := "audit-archive/" + strings.ReplaceAll(chainKey, "/", "-") +
		"/seq-" + fmt.Sprintf("%012d", fromSeq) + "-" + fmt.Sprintf("%012d", toSeq)
	return base + ".manifest.csv", base + ".entries.jsonl"
}

// manifestLine 是归档清单一行：entry_id,seq,entry_hash。
// 清单刻意只含这三列：它是「这段链被完整搬走了」的证据，
// 多一列就多一分把存证内容搬到桶里裸放的风险。
func manifestLine(e *model.AuditEntry) string {
	return strconv.FormatInt(e.EntryID, 10) + "," + strconv.FormatInt(e.Seq, 10) + "," + e.EntryHash + "\n"
}

func manifestBytes(rows []*model.AuditEntry) []byte {
	var sb strings.Builder
	for _, r := range rows {
		sb.WriteString(manifestLine(r))
	}
	return []byte(sb.String())
}

// manifestHash 重算清单 sha256hex。verified 步骤拿它与对象侧回传的哈希比对。
func manifestHash(rows []*model.AuditEntry) string {
	sum := sha256.Sum256(manifestBytes(rows))
	return hex.EncodeToString(sum[:])
}

// newestOccurredAt 取区间内最大业务时间（归档到期判定用）。
func newestOccurredAt(rows []*model.AuditEntry) int64 {
	var max int64
	for _, r := range rows {
		if r.OccurredAt > max {
			max = r.OccurredAt
		}
	}
	return max
}

// archiveEligible 判定区间是否已过归档线。
// 用「区间内最大 occurred_at」而不是「今天减 days」：链按 UTC 日分段，
// 段内时间跨度可达一天，取最大值才可能出现的提前归档。
func archiveEligible(policy *model.RetentionPolicy, newest int64, now int64) error {
	if policy == nil {
		return model.ErrPolicyNotFound
	}
	if newest == 0 {
		return model.ErrArchiveRangeInvalid
	}
	eligibleAt := newest + int64(policy.ArchiveAfterDays)*86400
	if now < eligibleAt {
		return fmt.Errorf("%w: archive_after_days=%d newest_occurred_at=%d eligible_at=%d now=%d",
			model.ErrArchiveNotDue, policy.ArchiveAfterDays, newest, eligibleAt, now)
	}
	return nil
}

// --- 8. 保留策略 ---

// policyState 归一化策略状态：0 视为启用（proto 注释），其它值只允许 1/2。
func policyState(v int32) (int32, error) {
	switch v {
	case 0, model.StateEnable:
		return model.StateEnable, nil
	case model.StateDisable:
		return model.StateDisable, nil
	default:
		return 0, fmt.Errorf("%w: retention state=%d", model.ErrPolicyDaysInvalid, v)
	}
}

// retentionLoosened 判定「更新后」是否放宽了物理清理窗口。
//
// 判定只看 delete_after_days，因为它是唯一会让证据更早消失的旋钮：
//   - 原值 0（永久保留）→ 新值 >0：从「永不删」变成「可删」，最危险的一类；
//   - 两者都 >0 且新值更大：清理提前。
//
// archive_after_days / hot_days 的变化不影响可删除时点，因此不算放宽。
func retentionLoosened(oldPolicy, next *model.RetentionPolicy) bool {
	if oldPolicy == nil || next == nil {
		return false
	}
	if oldPolicy.DeleteAfterDays == 0 && next.DeleteAfterDays > 0 {
		return true
	}
	return oldPolicy.DeleteAfterDays > 0 && next.DeleteAfterDays > oldPolicy.DeleteAfterDays
}

// samePolicy 判定两次新建提交是否等价（幂等回放用）。
func samePolicy(a, b *model.RetentionPolicy) bool {
	if a == nil || b == nil {
		return false
	}
	return a.HotDays == b.HotDays && a.ArchiveAfterDays == b.ArchiveAfterDays &&
		a.DeleteAfterDays == b.DeleteAfterDays && a.State == b.State && a.Remark == b.Remark
}

// --- 9. 杂项 ---

// firstNonEmpty 返回第一个非空（去空格）字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// exportFormat 归一化导出格式：空 → csv，其余只允许 csv/json。
func exportFormat(v string) (string, error) {
	f := strings.ToLower(strings.TrimSpace(v))
	if f == "" {
		return "csv", nil
	}
	switch f {
	case "csv", "json":
		if err := checkLen("format", f, model.MaxFormatBytes); err != nil {
			return "", err
		}
		return f, nil
	default:
		return "", fmt.Errorf("%w: %q，本期只支持 csv/json", model.ErrExportFormatUnsupported, v)
	}
}

// clampErrMsg 把失败原因压进 err_msg 列宽，并去掉可能带的堆栈/连接串特征。
// err_msg 是脱敏文本：只保留业务原因，第一行且不超过列宽。
func clampErrMsg(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if model.LooksLikePII(s) {
		// 错误文本里出现疑似 PII（例如上游把手机号塞进异常消息）时，
		// 只保留错误类型，不留原文：err_msg 也是能被导出的高敏面。
		return "redacted: message matched PII pattern"
	}
	return model.TruncateRunes(s, model.MaxErrMsgBytes)
}

// isHashHex 判定完整 sha256hex（64 位小写十六进制）。
// 对象存储回传的摘要必须先过这一关：摘要缺失/被截断就写「已落地」的元数据，
// 等于给一个无法自证的文件背书。
func isHashHex(s string) bool {
	if len(s) != model.HashHexLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// exportStateFilter 校验列表过滤用的任务状态（空串 = 不过滤）。
// 未定义的状态值一律拒绝：拼错的 state 若被当成「不过滤」，
// 调用方会以为自己在看失败任务，实际看到全部。
func exportStateFilter(v string) (string, error) {
	s := strings.TrimSpace(v)
	switch s {
	case "", model.ExportStatePending, model.ExportStateRunning, model.ExportStateSucceeded,
		model.ExportStateFailed, model.ExportStateExpired, model.ExportStateCanceled:
		return s, nil
	default:
		return "", fmt.Errorf("%w: state=%q", model.ErrTaskBadTransition, s)
	}
}

// batchStateFilter 校验归档批次列表的状态过滤值（同上，空串 = 不过滤）。
func batchStateFilter(v string) (string, error) {
	s := strings.TrimSpace(v)
	switch s {
	case "", model.BatchStatePending, model.BatchStateWriting, model.BatchStateVerified,
		model.BatchStatePurged, model.BatchStateFailed:
		return s, nil
	default:
		return "", fmt.Errorf("%w: state=%q", model.ErrTaskBadTransition, s)
	}
}

// policyStateFilter 校验保留策略列表的状态过滤值：0 = 全部，其余只允许 1/2。
func policyStateFilter(v int32) (int32, error) {
	switch v {
	case 0, model.StateEnable, model.StateDisable:
		return v, nil
	default:
		return 0, fmt.Errorf("%w: state=%d", model.ErrPolicyDaysInvalid, v)
	}
}

// ctimeWindow 校验「按创建时间过滤」的可选区间：只给一端也允许，但两端都给时必须有序。
func ctimeWindow(startAt, endAt int64) error {
	if startAt > 0 && endAt > 0 && startAt >= endAt {
		return fmt.Errorf("%w: start_at=%d end_at=%d", model.ErrQueryRangeRequired, startAt, endAt)
	}
	if startAt < 0 || endAt < 0 {
		return fmt.Errorf("%w: start_at=%d end_at=%d", model.ErrQueryRangeRequired, startAt, endAt)
	}
	return nil
}

// scanPIIValues 扫描一批自由文本，命中明文敏感形态即拒绝（导出条件、策略备注共用）。
// 这些文本都会以原文落库并可被再次导出，等于「第二条泄露通道」，
// 因此处理方式与条目摘要一致：拒绝，不做「脱敏后落库」的降级。
func scanPIIValues(field string, values ...string) error {
	for _, v := range values {
		if v == "" {
			continue
		}
		if model.LooksLikePII(v) {
			return fmt.Errorf("%w: %s=%q 命中明文敏感信息形态", model.ErrDigestLooksPII, field, model.TruncateRunes(v, 8))
		}
	}
	return nil
}
