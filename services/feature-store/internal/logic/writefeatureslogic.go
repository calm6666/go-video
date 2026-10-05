package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type WriteFeaturesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewWriteFeaturesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WriteFeaturesLogic {
	return &WriteFeaturesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// writeRow 是一行写入从入参到落库的中间态：校验失败也保留原位置，
// 逐行结果必须与请求同序同数，否则调用方无法把 rejected 对回自己发的那一条。
type writeRow struct {
	featureKey string
	entityID   string
	key        model.ValueKey
	value      *model.FeatureValue
	reject     error
}

// writeRowDetail 是回执里的逐行结果（短码，不含值原文）。
type writeRowDetail struct {
	K string `json:"k"`
	E string `json:"e"`
	O bool   `json:"o"`
	C string `json:"c,omitempty"`
}

// writeSnapshot 是整批写的可回放快照。
type writeSnapshot struct {
	Written  int32            `json:"w"`
	Rejected int32            `json:"r"`
	Rows     []writeRowDetail `json:"d,omitempty"`
}

// 批量写入特征值（request_id 整批幂等，逐行返回结果）
//
// 三条不可让的边界：
//  1. 整批按 (request_id, op_type=write) 取得执行权，命中已完成回执只回放首次结果，
//     绝不重新写一遍、也绝不重新计数一次；
//  2. 行级拒绝（格式、乱序、未激活）只影响那一行，不中断整批；
//     数据库层面失败则整批回滚，由调用方换重试次数而不是服务端猜结论；
//  3. 缓存只做删除（DEL），不做增量写：一次批量写半新半旧的缓存比空缓存更难解释。
func (l *WriteFeaturesLogic) WriteFeatures(in *rpc.WriteFeaturesReq) (*rpc.WriteFeaturesReply, error) {
	requestID := strings.TrimSpace(in.GetRequestId())
	operator := strings.TrimSpace(in.GetOperator())
	if err := checkRequestID(requestID); err != nil {
		return nil, err
	}
	if err := checkOperator(operator); err != nil {
		return nil, err
	}
	writes := in.GetWrites()
	// 行数上限在取执行权之前判：超限的请求根本不该被登记成一次「正在进行」的写。
	if err := model.ValidateBatchWriteRows(len(writes)); err != nil {
		return nil, err
	}
	writer := int32(in.GetWriter())
	if !model.ValidSource(writer) {
		return nil, fmt.Errorf("%w: writer %d is not a declared source", model.ErrSourceRequired, writer)
	}

	spec := receiptSpec{
		requestID: requestID,
		opType:    model.ReceiptOpWrite,
		rowCount:  int32(len(writes)),
		operator:  operator,
	}
	res, owner, err := beginReceipt(l.ctx, l.svcCtx, l.Logger, spec)
	if err != nil {
		return nil, err
	}
	if !res.Execute {
		return l.replay(res.Receipt)
	}

	rows := l.prepare(writes, writer, operator, requestID)
	// 缓存键按成功写入的行收集，事务提交后再删。
	var dirty []string
	var outcomes []model.UpsertOutcome
	var writable []*model.FeatureValue
	index := make([]int, 0, len(rows))
	for i := range rows {
		if rows[i].reject != nil || rows[i].value == nil {
			continue
		}
		writable = append(writable, rows[i].value)
		index = append(index, i)
	}
	if len(writable) > 0 {
		if err := l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
			got, err := l.svcCtx.Values.BatchUpsert(ctx, tx, writable, model.UpsertOptions{
				// 对外写路径永不覆盖乱序值：一次迟到的旧批次不能把线上生效值退回历史。
				AllowStaleOverwrite: false,
			})
			if err != nil {
				return err
			}
			outcomes = got
			return nil
		}); err != nil {
			failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
			return nil, err
		}
		for i, o := range outcomes {
			if o.OK {
				dirty = append(dirty, o.Key.CacheKey())
			}
			if i < len(index) {
				rows[index[i]].reject, rows[index[i]].value = outcomeErr(o), nil
			}
		}
	}
	// 提交后清缓存：删除失败只在 helper 里记日志（DB 是回源依据，残留值最长活到自己的 TTL）。
	invalidateCache(l.ctx, l.svcCtx, l.Logger, dirty)

	results, written, rejected := l.projectRows(rows)
	snap := writeSnapshot{Written: written, Rejected: rejected}
	for _, r := range results {
		snap.Rows = append(snap.Rows, writeRowDetail{K: r.GetFeatureKey(), E: r.GetEntityId(),
			O: r.GetOk(), C: r.GetError()})
	}
	full := marshalJSON(snap)
	if err := l.finish(spec, owner, snap, full, int64(written)); err != nil {
		return nil, err
	}
	l.Infow("features written", logx.Field("request_id", requestID),
		logx.Field("written", written), logx.Field("rejected", rejected),
		logx.Field("operator", operator))
	return &rpc.WriteFeaturesReply{Written: written, Rejected: rejected, Results: results}, nil
}

// prepare 逐行做定义与形态校验，返回与入参同序的行数组。
// 任何一条校验失败都记在行上（reject），不参与批量 UPSERT：
// 「一整批里有一条写错」不该让其余 499 条一起失败重发。
func (l *WriteFeaturesLogic) prepare(writes []*rpc.FeatureWrite, writer int32,
	operator, requestID string) []writeRow {
	rows := make([]writeRow, len(writes))
	// 先解析 version=0（= 当前 ACTIVE 版本），再批量取定义：逐行点查会把一批写成 500 次往返。
	keyCache := map[string]int32{}
	defKeys := make([]model.DefinitionKey, 0, len(writes))
	for i, w := range writes {
		key := strings.TrimSpace(w.GetFeature().GetFeatureKey())
		version := w.GetFeature().GetVersion()
		rows[i].featureKey = key
		if err := checkFeatureKey(key); err != nil {
			rows[i].reject = err
			continue
		}
		scope, entityID, err := checkEntity(w.GetEntity())
		if err != nil {
			rows[i].reject = err
			continue
		}
		rows[i].entityID = entityID
		rows[i].key = model.ValueKey{FeatureKey: key, Version: version, EntityScope: scope, EntityID: entityID}
		if version == 0 {
			if cached, ok := keyCache[key]; ok {
				if cached < 1 {
					// 指针缺失的行在这一轮就定性，不发第二次指针查询（ErrNoActiveVersion 无法靠写值解决）。
					rows[i].reject = fmt.Errorf("%w: %s", model.ErrNoActiveVersion, key)
					continue
				}
				rows[i].key.Version = cached
				defKeys = append(defKeys, model.DefinitionKey{FeatureKey: key, Version: cached})
				continue
			}
			av, err := l.svcCtx.ActiveVersions.FindOne(l.ctx, key)
			if err != nil {
				rows[i].reject = err
				keyCache[key] = -1
				continue
			}
			if !av.HasActive() {
				rows[i].reject = fmt.Errorf("%w: %s", model.ErrNoActiveVersion, key)
				keyCache[key] = -1
				continue
			}
			keyCache[key] = av.ActiveVersion
			rows[i].key.Version = av.ActiveVersion
			defKeys = append(defKeys, model.DefinitionKey{FeatureKey: key, Version: av.ActiveVersion})
			continue
		}
		if err := checkVersion(version); err != nil {
			rows[i].reject = err
			continue
		}
		defKeys = append(defKeys, model.DefinitionKey{FeatureKey: key, Version: rows[i].key.Version})
	}
	defs, err := l.loadDefinitions(defKeys)
	if err != nil {
		// 定义读不出来就无法给任何一行定性：全部标记为同一失败，宁可整批重试也不猜口径。
		for i := range rows {
			if rows[i].reject == nil {
				rows[i].reject = err
			}
		}
		return rows
	}
	for i, w := range writes {
		if rows[i].reject != nil {
			continue
		}
		def := defs[model.DefinitionKey{FeatureKey: rows[i].key.FeatureKey, Version: rows[i].key.Version}]
		rows[i].reject = l.checkRow(&rows[i], def, writer, operator)
		if rows[i].reject != nil {
			continue
		}
		payload, err := payloadFromProto(w.GetValue())
		if err != nil {
			rows[i].reject = err
			continue
		}
		v, err := model.NewFeatureValue(def, rows[i].key, payload, w.GetEventTime(),
			strings.TrimSpace(w.GetSourceMetricKey()), requestID, operator, 0)
		if err != nil {
			rows[i].reject = err
			continue
		}
		rows[i].value = v
	}
	return rows
}

// checkRow 是单行的定义层校验：定义存在、已激活、来源自洽，且值载荷类型与定义一致。
func (l *WriteFeaturesLogic) checkRow(row *writeRow, def *model.FeatureDefinition,
	writer int32, operator string) error {
	if def == nil {
		return fmt.Errorf("%w: %s@v%d", model.ErrFeatureNotFound, row.key.FeatureKey, row.key.Version)
	}
	if !def.IsWritable() {
		if def.State == model.FeatureStateRetired {
			return fmt.Errorf("%w: %s@v%d", model.ErrFeatureRetired, def.FeatureKey, def.Version)
		}
		return fmt.Errorf("%w: %s@v%d is not ACTIVE", model.ErrFeatureNotActive, def.FeatureKey, def.Version)
	}
	// 写入身份必须与口径一致，除非调用方是受控系统（operator 前缀 system: / offline-job:）。
	// 例外存在的理由：event-collector 与离线重算确实要跨来源落值，
	// 但普通业务方冒充别的口径写进来会让特征值再也对不上任何上游定义。
	if writer != def.Source && !isTrustedWriter(operator) {
		return fmt.Errorf("%w: writer %d != definition source %d", model.ErrSourceMismatch, writer, def.Source)
	}
	if int32(row.key.EntityScope) != def.EntityScope {
		return fmt.Errorf("%w: request scope %d != definition scope %d",
			model.ErrEntityScopeMismatch, row.key.EntityScope, def.EntityScope)
	}
	// 值类型与 dimension 的匹配交给 model.EncodeValue（读写两侧共用同一道闸，
	// 在这里再判一遍只会让两处的规则有机会漂移）。
	return nil
}

// loadDefinitions 分块批量取定义（ListByKeys 自带条数上限）。
func (l *WriteFeaturesLogic) loadDefinitions(keys []model.DefinitionKey) (map[model.DefinitionKey]*model.FeatureDefinition, error) {
	out := map[model.DefinitionKey]*model.FeatureDefinition{}
	if len(keys) == 0 {
		return out, nil
	}
	for _, chunk := range chunkKeys(keys, model.MaxBatchFeatures) {
		got, err := l.svcCtx.Definitions.ListByKeys(l.ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("feature-store: load definitions for write: %w", err)
		}
		for k, v := range got {
			out[k] = v
		}
	}
	return out, nil
}

// projectRows 把行状态投影成契约的逐行结果与计数。
func (l *WriteFeaturesLogic) projectRows(rows []writeRow) ([]*rpc.WriteFeaturesReply_RowResult, int32, int32) {
	results := make([]*rpc.WriteFeaturesReply_RowResult, 0, len(rows))
	var written, rejected int32
	for _, r := range rows {
		ok := r.reject == nil
		if ok {
			written++
		} else {
			rejected++
		}
		results = append(results, &rpc.WriteFeaturesReply_RowResult{
			FeatureKey: r.featureKey,
			EntityId:   r.entityID,
			Ok:         ok,
			Error:      errorCodeOf(r.reject),
		})
	}
	return results, written, rejected
}

// finish 落回执快照。逐行明细超过回执列宽时只保留计数（DetailKept=false），
// 但摘要仍覆盖完整首次结果：回放时能如实说明「就是这份，只是明细没留存」。
func (l *WriteFeaturesLogic) finish(spec receiptSpec, owner string, snap writeSnapshot,
	full string, affected int64) error {
	digest := resultDigest(full)
	stored, kept := full, true
	if len(full) > model.MaxReceiptResultBytes {
		stored = marshalJSON(writeSnapshot{Written: snap.Written, Rejected: snap.Rejected})
		kept = false
	}
	return finishReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, model.ReceiptSnapshot{
		ResultJSON:   stored,
		ResultDigest: digest,
		AffectedRows: affected,
		DetailKept:   kept,
	})
}

// replay 按首次回执回答重复请求。明细未留存时只回计数：
// 现攒一份「看起来一样」的逐行结果就是伪造回放。
func (l *WriteFeaturesLogic) replay(receipt *model.WriteReceipt) (*rpc.WriteFeaturesReply, error) {
	var snap writeSnapshot
	if err := unmarshalJSON(receipt.ResultJSON, &snap); err != nil {
		return nil, fmt.Errorf("feature-store: write receipt unreadable: %w", err)
	}
	reply := &rpc.WriteFeaturesReply{Written: snap.Written, Rejected: snap.Rejected, Reused: true}
	if receipt.Replayable() {
		for _, d := range snap.Rows {
			reply.Results = append(reply.Results, &rpc.WriteFeaturesReply_RowResult{
				FeatureKey: d.K, EntityId: d.E, Ok: d.O, Error: d.C,
			})
		}
		return reply, nil
	}
	l.Infow("write replay without per-row detail", logx.Field("request_id", receipt.RequestID),
		logx.Field("result_digest", receipt.ResultDigest))
	return reply, nil
}

// outcomeErr 把 BatchUpsert 的行结果转成拒绝原因（成功时为 nil）。
func outcomeErr(o model.UpsertOutcome) error {
	if o.OK {
		return nil
	}
	if o.Err != nil {
		return o.Err
	}
	return model.ErrMalformedValue
}

// isTrustedWriter 判定跨来源写入是否来自受控系统身份。
func isTrustedWriter(operator string) bool {
	op := strings.ToLower(strings.TrimSpace(operator))
	return strings.HasPrefix(op, "system:") || strings.HasPrefix(op, "offline-job:")
}
