// fakes_test.go 是 logic 层的离线夹具。
//
// 组装口径（README「测试」）：logic → **真实 Repository**（repository.NewWithDeps /
// NewWithModels）→ 手写 fake（model 四张表 + esclient.Client + 缓存）。
// 绝不 mock Repository：那样只会测到「调了哪个方法」，测不到投影层的真实规则
// （last-write-wins 守卫、别名切换顺序、缓存失效时机、SQL 条件更新的极性）。
//
// 四条 fake 纪律：
//
//	① 读侧一律返回值拷贝，生产代码改不动夹具里的行；
//	② Insert 只发主键，不自动补生产 SQL 没写的列；
//	③ 维护**有序** callLog（"<pkg>.<method>:<key>"），用例用 wantOps 断言完整序列；
//	④ 播种是静默的（seed* 不写 callLog），否则序列断言会被种子污染。
package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/internal/svc"
	"go-video/services/search-indexer/model"
)

// 哨兵错误：断言里必须用 errors.Is 比对它们，而不是「有 error 就算过」。
var (
	errBoom  = errors.New("boom-es")
	errOther = errors.New("boom-mysql")
	// errUnimpl 用于「本用例根本不该走到」的方法：走到就说明多调了依赖。
	errUnimpl = errors.New("logic-fake: 该方法不在本用例路径上")
)

// testPrefix 是默认别名（Options.IndexPrefix）。
const testPrefix = "go_video_content"

// defaultOptions 返回可控的投影参数（不依赖 config，也不联网）。
func defaultOptions() repository.Options {
	return repository.Options{IndexPrefix: testPrefix, SchemaVersion: "v1", RetryOnConflict: 2}
}

// tsRe 匹配 NewIndexName / EnsureActiveIndex 追加的 Unix 秒（及撞名重试的纳秒尾缀）。
// 这类值来自 time.Now()，不能进精确序列，只能掩码后比对。
var tsRe = regexp.MustCompile(`_\d{10,}(_\d{1,3})?`)

func maskTS(s string) string { return tsRe.ReplaceAllString(s, "_<ts>") }

// ---------------------------------------------------------------- 基础设施

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...interface{}) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) snapshot() []string {
	out := make([]string, len(c.ops))
	copy(out, c.ops)
	return out
}

// faults 按 op 精确匹配，其次按 "<pkg>.<method>" 前缀匹配（key 可能含动态索引名）。
type faults struct{ byOp map[string]error }

func newFaults() *faults { return &faults{byOp: map[string]error{}} }

func (f *faults) fail(op string, err error) { f.byOp[op] = err }

func (f *faults) errFor(op string) (error, bool) {
	if e, ok := f.byOp[op]; ok {
		return e, true
	}
	if i := strings.Index(op, ":"); i > 0 {
		if e, ok := f.byOp[op[:i]]; ok {
			return e, true
		}
	}
	return nil, false
}

// fakeBase 让各 fake 共用一条调用流水、一个故障表和一段主键发号器。
type fakeBase struct {
	log    *callLog
	faults *faults
	idSeq  int64
}

func newFakeBase() *fakeBase {
	return &fakeBase{log: &callLog{}, faults: newFaults()}
}

func (b *fakeBase) nextID() int64 { b.idSeq++; return b.idSeq }

// trace 记录一次调用并返回 op（便于「先记后判故障」）。
func (b *fakeBase) trace(format string, args ...interface{}) string {
	op := fmt.Sprintf(format, args...)
	b.log.add("%s", op)
	return op
}

func (b *fakeBase) failIf(op string) (error, bool) { return b.faults.errFor(op) }

// ---------------------------------------------------------------- esclient

// fakeES 复刻本服务真正用到的 OpenSearch 行为：显式 _id 读写、_update 合并、
// 别名指向表、_count 404、建索引幂等（resource_already_exists）。
type fakeES struct {
	*fakeBase
	exists       map[string]bool
	counts       map[string]int64
	noCount      map[string]bool // 索引存在但 _count 返回 404（真实集群可能出现）
	docs         map[string]map[string]json.RawMessage
	aliasTargets map[string][]string
	health       map[string]string
	created      []string // 依次记录 CreateIndex 收到的索引名
	applied      []string // 依次记录 _aliases 动作集
	patches      []string // 依次记录 _update 的 body（用于读回断言）
	// updateForcesApplied=false 时 _update 返回 applied=false（文档不存在语义）。
	updateForces *bool
	createFirst  *bool // 首次 CreateIndex 强制返回 alreadyExists（撞名用例）
	createdOnce  bool
}

func newFakeES(b *fakeBase) *fakeES {
	return &fakeES{
		fakeBase:     b,
		exists:       map[string]bool{},
		counts:       map[string]int64{},
		noCount:      map[string]bool{},
		docs:         map[string]map[string]json.RawMessage{},
		aliasTargets: map[string][]string{},
		health:       map[string]string{},
	}
}

// seedIndex 静默登记物理索引（不写 callLog）。
func (f *fakeES) seedIndex(index string, docCount int64) {
	f.exists[index] = true
	f.counts[index] = docCount
}

func (f *fakeES) seedIndexNoCount(index string) {
	f.exists[index] = true
	f.noCount[index] = true
}

func (f *fakeES) seedAlias(alias string, targets ...string) {
	f.aliasTargets[alias] = append([]string(nil), targets...)
}

func (f *fakeES) seedHealth(index, status string) { f.health[index] = status }

// seedSource 直接写入 _source 原文（可放脏数据）。
func (f *fakeES) seedSource(index, id string, src json.RawMessage) {
	if f.docs[index] == nil {
		f.docs[index] = map[string]json.RawMessage{}
	}
	f.docs[index][id] = src
}

// seedRevision 播种一条已有投影（probe 只需 doc_revision）。
func (f *fakeES) seedRevision(index, id string, docRevision int64) {
	f.seedSource(index, id, json.RawMessage(fmt.Sprintf(`{"doc_revision":%d,"title":"索引里的旧事实"}`, docRevision)))
}

// sourceOf 读回 _source（供断言用，不记 callLog）。
func (f *fakeES) sourceOf(index, id string) (json.RawMessage, bool) {
	raw, ok := f.docs[index][id]
	if !ok {
		return nil, false
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out, true
}

func (f *fakeES) IndexDoc(ctx context.Context, index, id string, src interface{}) error {
	op := f.trace("es.IndexDoc:%s/%s", index, id)
	if err, bad := f.failIf(op); bad {
		return err
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	if f.docs[index] == nil {
		f.docs[index] = map[string]json.RawMessage{}
	}
	f.docs[index][id] = raw
	return nil
}

func (f *fakeES) GetSource(ctx context.Context, index, id string) (json.RawMessage, bool, error) {
	op := f.trace("es.GetSource:%s/%s", index, id)
	if err, bad := f.failIf(op); bad {
		return nil, false, err
	}
	raw, ok := f.sourceOf(index, id)
	if !ok {
		return nil, false, nil
	}
	return raw, true, nil
}

func (f *fakeES) DeleteDoc(ctx context.Context, index, id string) (bool, error) {
	op := f.trace("es.DeleteDoc:%s/%s", index, id)
	if err, bad := f.failIf(op); bad {
		return false, err
	}
	if _, ok := f.docs[index][id]; !ok {
		return false, nil // 原本不存在 → 幂等
	}
	delete(f.docs[index], id)
	return true, nil
}

func (f *fakeES) UpdatePartial(ctx context.Context, index, id string, partial map[string]interface{}, retryOnConflict int) (bool, error) {
	op := f.trace("es.UpdatePartial:%s/%s", index, id)
	if err, bad := f.failIf(op); bad {
		return false, err
	}
	body, err := json.Marshal(partial)
	if err != nil {
		return false, err
	}
	f.patches = append(f.patches, fmt.Sprintf("%s|retry=%d|%s", id, retryOnConflict, body))
	if f.updateForces != nil && !*f.updateForces {
		return false, nil
	}
	raw, ok := f.docs[index][id]
	if !ok {
		return false, nil // docAsUpsert=false：不新建
	}
	var merged map[string]interface{}
	if err := json.Unmarshal(raw, &merged); err != nil {
		merged = map[string]interface{}{}
	}
	for k, v := range partial {
		merged[k] = v
	}
	next, err := json.Marshal(merged)
	if err != nil {
		return false, err
	}
	f.docs[index][id] = next
	return true, nil
}

func (f *fakeES) Bulk(ctx context.Context, index string, ops []esclient.BulkOp) (*esclient.BulkResult, error) {
	op := f.trace("es.Bulk:%s/%d", index, len(ops))
	if err, bad := f.failIf(op); bad {
		return nil, err
	}
	return &esclient.BulkResult{}, errUnimpl
}

func (f *fakeES) Refresh(ctx context.Context, index string) error {
	op := f.trace("es.Refresh:%s", index)
	if err, bad := f.failIf(op); bad {
		return err
	}
	return errUnimpl
}

func (f *fakeES) CreateIndex(ctx context.Context, index, schemaVersion string) (bool, error) {
	op := f.trace("es.CreateIndex:%s", index)
	f.created = append(f.created, index)
	if err, bad := f.failIf(op); bad {
		return false, err
	}
	if f.createFirst != nil && !f.createdOnce {
		f.createdOnce = true
		return *f.createFirst, nil
	}
	if f.exists[index] {
		return true, nil // resource_already_exists_exception → 幂等
	}
	f.exists[index] = true
	f.counts[index] = 0
	return false, nil
}

func (f *fakeES) IndexExists(ctx context.Context, index string) (bool, error) {
	op := f.trace("es.IndexExists:%s", index)
	if err, bad := f.failIf(op); bad {
		return false, err
	}
	return f.exists[index], nil
}

func (f *fakeES) Count(ctx context.Context, index string) (int64, error) {
	op := f.trace("es.Count:%s", index)
	if err, bad := f.failIf(op); bad {
		return 0, err
	}
	// 真实集群对不存在的索引返回 404（= ErrNotFound），而不是 0 条。
	if !f.exists[index] {
		return 0, fmt.Errorf("%w: index %s", esclient.ErrNotFound, index)
	}
	if f.noCount[index] {
		return 0, fmt.Errorf("%w: index %s", esclient.ErrNotFound, index)
	}
	return f.counts[index], nil
}

func (f *fakeES) AliasTargets(ctx context.Context, alias string) ([]string, error) {
	op := f.trace("es.AliasTargets:%s", alias)
	if err, bad := f.failIf(op); bad {
		return nil, err
	}
	targets, ok := f.aliasTargets[alias]
	if !ok || len(targets) == 0 {
		return nil, fmt.Errorf("%w: %s", esclient.ErrNotFound, alias)
	}
	return append([]string(nil), targets...), nil
}

func (f *fakeES) ApplyAliasActions(ctx context.Context, actions []esclient.AliasAction) error {
	rendered := make([]string, 0, len(actions))
	for _, a := range actions {
		rendered = append(rendered, fmt.Sprintf("%s:%s", a.Kind, a.Index))
	}
	op := f.trace("es.ApplyAliasActions:%s", strings.Join(rendered, ","))
	f.applied = append(f.applied, strings.Join(rendered, ","))
	if err, bad := f.failIf(op); bad {
		return err
	}
	for _, a := range actions {
		switch a.Kind {
		case "add":
			f.aliasTargets[a.Alias] = append(f.aliasTargets[a.Alias], a.Index)
		case "remove":
			out := f.aliasTargets[a.Alias][:0]
			for _, idx := range f.aliasTargets[a.Alias] {
				if idx != a.Index {
					out = append(out, idx)
				}
			}
			f.aliasTargets[a.Alias] = out
		default:
			return fmt.Errorf("esclient: invalid alias action kind %q", a.Kind)
		}
	}
	return nil
}

func (f *fakeES) ReindexSlice(ctx context.Context, req esclient.ReindexSliceReq) (*esclient.ReindexResult, error) {
	op := f.trace("es.ReindexSlice:%s", req.Dest)
	if err, bad := f.failIf(op); bad {
		return nil, err
	}
	return nil, errUnimpl
}

func (f *fakeES) ClusterHealth(ctx context.Context, index string) (string, error) {
	op := f.trace("es.ClusterHealth:%s", index)
	if err, bad := f.failIf(op); bad {
		return "", err
	}
	if !f.exists[index] {
		return "missing", nil // 真实实现里 404 → "missing"
	}
	if st, ok := f.health[index]; ok {
		return st, nil
	}
	return "green", nil
}

// MaxBulkActions 是纯访问器，不记 callLog（否则污染序列断言）。
func (f *fakeES) MaxBulkActions() int { return 500 }

func (f *fakeES) Close() error { return nil }

// ---------------------------------------------------------------- model: task

type fakeTaskModel struct {
	*fakeBase
	rows []*model.SearchIndexTask
	// collisionRow 非空：下一次 Insert 命中 uniq_request_id —— 落进的是对手的行，
	// 返回 existed=true（生产语义：RowsAffected==0）。只生效一次。
	collisionRow *model.SearchIndexTask
}

func newFakeTaskModel(b *fakeBase) *fakeTaskModel { return &fakeTaskModel{fakeBase: b} }

func (m *fakeTaskModel) seed(t *model.SearchIndexTask) {
	row := *t
	row.ID = m.nextID()
	m.rows = append(m.rows, &row)
}

// clone 满足纪律①：读侧返回值拷贝。
func cloneTask(t *model.SearchIndexTask) *model.SearchIndexTask {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func (m *fakeTaskModel) clone(t *model.SearchIndexTask) *model.SearchIndexTask { return cloneTask(t) }

func (m *fakeTaskModel) byRequestID(rid string) *model.SearchIndexTask {
	for _, r := range m.rows {
		if r.RequestID == rid {
			return r
		}
	}
	return nil
}

func (m *fakeTaskModel) byTaskID(id string) *model.SearchIndexTask {
	for _, r := range m.rows {
		if r.TaskID == id {
			return r
		}
	}
	return nil
}

// Insert 命中 uniq_request_id 时返回 existed=true（= RowsAffected==0）。
func (m *fakeTaskModel) Insert(ctx context.Context, t *model.SearchIndexTask) (bool, error) {
	op := m.trace("task.Insert:%s", t.RequestID)
	if err, bad := m.failIf(op); bad {
		return false, err
	}
	if m.collisionRow != nil {
		row := cloneTask(m.collisionRow)
		row.ID = m.nextID()
		m.rows = append(m.rows, row)
		m.collisionRow = nil
		return true, nil
	}
	if m.byRequestID(t.RequestID) != nil {
		return true, nil
	}
	// 纪律②：只发主键，其余列按入参原样落库。
	row := cloneTask(t)
	row.ID = m.nextID()
	m.rows = append(m.rows, row)
	return false, nil
}

func (m *fakeTaskModel) FindOne(ctx context.Context, taskID string) (*model.SearchIndexTask, error) {
	op := m.trace("task.FindOne:%s", taskID)
	if err, bad := m.failIf(op); bad {
		return nil, err
	}
	return m.clone(m.byTaskID(taskID)), nil
}

func (m *fakeTaskModel) FindByRequestID(ctx context.Context, requestID string) (*model.SearchIndexTask, error) {
	op := m.trace("task.FindByRequestID:%s", requestID)
	if err, bad := m.failIf(op); bad {
		return nil, err
	}
	return m.clone(m.byRequestID(requestID)), nil
}

func (m *fakeTaskModel) ClaimNext(ctx context.Context, now int64) (*model.SearchIndexTask, error) {
	op := m.trace("task.ClaimNext")
	if err, bad := m.failIf(op); bad {
		return nil, err
	}
	return nil, errUnimpl
}

func (m *fakeTaskModel) UpdateProgress(ctx context.Context, taskID, cursorValue string, processed, failed, total, now int64) error {
	op := m.trace("task.UpdateProgress:%s", taskID)
	if err, bad := m.failIf(op); bad {
		return err
	}
	return errUnimpl
}

func (m *fakeTaskModel) Finish(ctx context.Context, taskID, state, lastError string, now int64) error {
	op := m.trace("task.Finish:%s", taskID)
	if err, bad := m.failIf(op); bad {
		return err
	}
	return errUnimpl
}

// List 复刻真实 SQL：LIMIT 夹取 + state 过滤 + keyset（id < cursor）+ ORDER BY id DESC，
// 且只有「取满一页」才返回 next（末页 next 为空串）。
// 调用流水记的是**夹取后真正下发给 SQL 的 limit**——被测规则是「实际查了多少行」，
// 不是「调用方传了什么」。
func (m *fakeTaskModel) List(ctx context.Context, state, cursor string, limit int) ([]*model.SearchIndexTask, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	op := m.trace("task.List:%s|%s|%d", state, cursor, limit)
	if err, bad := m.failIf(op); bad {
		return nil, "", err
	}
	var lastID int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("search_index_task List cursor %q: %w", cursor, err)
		}
		lastID = n
	}
	var cand []*model.SearchIndexTask
	for _, r := range m.rows {
		if state != "" && r.State != state {
			continue
		}
		// 真实 SQL 用 `if lastID > 0`：负游标不加条件而不是报错，这里保持一致。
		if lastID > 0 && r.ID >= lastID {
			continue
		}
		cand = append(cand, r)
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].ID > cand[j].ID })
	if len(cand) > limit {
		cand = cand[:limit]
	}
	out := make([]*model.SearchIndexTask, 0, len(cand))
	for _, r := range cand {
		out = append(out, cloneTask(r))
	}
	next := ""
	if len(out) == limit && limit > 0 {
		next = strconv.FormatInt(out[len(out)-1].ID, 10)
	}
	return out, next, nil
}

// ------------------------------------------------------------- model: version

type fakeVersionModel struct {
	*fakeBase
	rows []*model.SearchIndexVersion
	// collideWith 非空：下一次 Insert 命中 uniq_index_name —— 落进对手登记的行，
	// 返回 registered=false（= RowsAffected==0）。只生效一次。
	collideWith *model.SearchIndexVersion
}

func newFakeVersionModel(b *fakeBase) *fakeVersionModel { return &fakeVersionModel{fakeBase: b} }

func cloneVersion(v *model.SearchIndexVersion) *model.SearchIndexVersion {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// seed 静默登记一行；state 见 model.VersionState*。
func (m *fakeVersionModel) seed(v *model.SearchIndexVersion) *model.SearchIndexVersion {
	row := cloneVersion(v)
	if row.ID == 0 {
		row.ID = m.nextID()
	}
	m.rows = append(m.rows, row)
	return row
}

func (m *fakeVersionModel) byIndexName(name string) *model.SearchIndexVersion {
	for _, r := range m.rows {
		if r.IndexName == name {
			return r
		}
	}
	return nil
}

// drop 静默删掉一行登记（构造「索引存在但未登记」的场景）。
func (m *fakeVersionModel) drop(name string) {
	out := m.rows[:0]
	for _, r := range m.rows {
		if r.IndexName != name {
			out = append(out, r)
		}
	}
	m.rows = out
}

// activeRow 复刻 `WHERE alias=? AND state='active' ORDER BY id DESC LIMIT 1`。
func (m *fakeVersionModel) activeRow(alias string) *model.SearchIndexVersion {
	var found *model.SearchIndexVersion
	for _, r := range m.rows {
		if r.Alias == alias && r.State == model.VersionStateActive {
			if found == nil || r.ID > found.ID {
				found = r
			}
		}
	}
	return found
}

func (m *fakeVersionModel) Insert(ctx context.Context, v *model.SearchIndexVersion) (bool, error) {
	op := m.trace("version.Insert:%s", v.IndexName)
	if err, bad := m.failIf(op); bad {
		return false, err
	}
	if m.collideWith != nil {
		row := cloneVersion(m.collideWith)
		row.ID = m.nextID()
		m.rows = append(m.rows, row)
		m.collideWith = nil
		return false, nil // aff==0
	}
	if m.byIndexName(v.IndexName) != nil {
		return false, nil // uniq_index_name
	}
	row := cloneVersion(v)
	row.ID = m.nextID()
	m.rows = append(m.rows, row)
	return true, nil // aff>0
}

func (m *fakeVersionModel) FindActive(ctx context.Context, alias string) (*model.SearchIndexVersion, error) {
	op := m.trace("version.FindActive:%s", alias)
	if err, bad := m.failIf(op); bad {
		return nil, err
	}
	return cloneVersion(m.activeRow(alias)), nil
}

func (m *fakeVersionModel) FindByIndexName(ctx context.Context, indexName string) (*model.SearchIndexVersion, error) {
	op := m.trace("version.FindByIndexName:%s", indexName)
	if err, bad := m.failIf(op); bad {
		return nil, err
	}
	return cloneVersion(m.byIndexName(indexName)), nil
}

// ListByAlias 复刻 `ORDER BY id DESC`。
func (m *fakeVersionModel) ListByAlias(ctx context.Context, alias string) ([]*model.SearchIndexVersion, error) {
	op := m.trace("version.ListByAlias:%s", alias)
	if err, bad := m.failIf(op); bad {
		return nil, err
	}
	var cand []*model.SearchIndexVersion
	for _, r := range m.rows {
		if r.Alias == alias {
			cand = append(cand, r)
		}
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].ID > cand[j].ID })
	return cloneVersions(cand), nil
}

// ListAll 复刻 `ORDER BY alias ASC, id DESC`。
func (m *fakeVersionModel) ListAll(ctx context.Context) ([]*model.SearchIndexVersion, error) {
	op := m.trace("version.ListAll")
	if err, bad := m.failIf(op); bad {
		return nil, err
	}
	cand := append([]*model.SearchIndexVersion(nil), m.rows...)
	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].Alias != cand[j].Alias {
			return cand[i].Alias < cand[j].Alias
		}
		return cand[i].ID > cand[j].ID
	})
	return cloneVersions(cand), nil
}

func cloneVersions(rows []*model.SearchIndexVersion) []*model.SearchIndexVersion {
	out := make([]*model.SearchIndexVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, cloneVersion(r))
	}
	return out
}

// SwitchActive 复刻真实事务：FOR UPDATE 读当前 active → expectedActive 乐观校验 →
// 旧 active 降级 → 目标置 active（目标未登记则 RowsAffected==0 → ErrVersionNotFound）。
func (m *fakeVersionModel) SwitchActive(ctx context.Context, alias, indexName, expectedActive, retireState string, now int64) error {
	op := m.trace("version.SwitchActive:%s->%s(exp=%s)", alias, indexName, expectedActive)
	if err, bad := m.failIf(op); bad {
		return err
	}
	cur := m.activeRow(alias)
	if cur == nil {
		if expectedActive != "" {
			return fmt.Errorf("%w: expected %s, alias has no active index", model.ErrAliasMismatch, expectedActive)
		}
	} else if expectedActive != "" && cur.IndexName != expectedActive {
		return fmt.Errorf("%w: current=%s expected=%s", model.ErrAliasMismatch, cur.IndexName, expectedActive)
	} else if cur.IndexName != indexName {
		cur.State = retireState
		cur.Mtime = now
	}
	target := m.byIndexName(indexName)
	if target == nil {
		return model.ErrVersionNotFound
	}
	target.State = model.VersionStateActive
	target.Mtime = now
	return nil
}

func (m *fakeVersionModel) UpdateState(ctx context.Context, indexName, state string, now int64) error {
	op := m.trace("version.UpdateState:%s", indexName)
	if err, bad := m.failIf(op); bad {
		return err
	}
	row := m.byIndexName(indexName)
	if row == nil {
		return model.ErrVersionNotFound
	}
	row.State = state
	row.Mtime = now
	return nil
}

// UpdateDocCount 生产 SQL 不检查 RowsAffected，这里也不报错（0 行时静默）。
func (m *fakeVersionModel) UpdateDocCount(ctx context.Context, indexName string, docCount, now int64) error {
	op := m.trace("version.UpdateDocCount:%s", indexName)
	if err, bad := m.failIf(op); bad {
		return err
	}
	if row := m.byIndexName(indexName); row != nil {
		row.DocCount = docCount
		row.Mtime = now
	}
	return nil
}

// rowOf 读回登记行（断言用，不记 callLog）。
func (m *fakeVersionModel) rowOf(indexName string) *model.SearchIndexVersion {
	return cloneVersion(m.byIndexName(indexName))
}

// ----------------------------------------------------------- model: offset/dlq

type fakeOffsetModel struct {
	*fakeBase
	counts map[string]int64
}

func newFakeOffsetModel(b *fakeBase) *fakeOffsetModel {
	return &fakeOffsetModel{fakeBase: b, counts: map[string]int64{}}
}

func (m *fakeOffsetModel) seedCount(state string, n int64) { m.counts[state] = n }

func (m *fakeOffsetModel) CountByState(ctx context.Context, state string) (int64, error) {
	op := m.trace("offset.CountByState:%s", state)
	if err, bad := m.failIf(op); bad {
		return 0, err
	}
	return m.counts[state], nil // sql.ErrNoRows → 0,nil（真实实现同样如此）
}

func (m *fakeOffsetModel) MarkReceived(ctx context.Context, rec *model.SearchConsumerOffset) (bool, error) {
	m.trace("offset.MarkReceived:%s", rec.EventID)
	return false, errUnimpl
}

func (m *fakeOffsetModel) FindByEventID(ctx context.Context, eventID string) (*model.SearchConsumerOffset, error) {
	m.trace("offset.FindByEventID:%s", eventID)
	return nil, errUnimpl
}

func (m *fakeOffsetModel) MarkProcessing(ctx context.Context, eventID string, now int64) error {
	m.trace("offset.MarkProcessing:%s", eventID)
	return errUnimpl
}

func (m *fakeOffsetModel) MarkSucceeded(ctx context.Context, eventID string, now int64) error {
	m.trace("offset.MarkSucceeded:%s", eventID)
	return errUnimpl
}

func (m *fakeOffsetModel) MarkRetry(ctx context.Context, eventID string, retryCount int32, nextRetryAt int64, lastError string, now int64) error {
	m.trace("offset.MarkRetry:%s", eventID)
	return errUnimpl
}

func (m *fakeOffsetModel) MarkDeadLetter(ctx context.Context, eventID, lastError string, now int64) error {
	m.trace("offset.MarkDeadLetter:%s", eventID)
	return errUnimpl
}

func (m *fakeOffsetModel) ListDueForRetry(ctx context.Context, now int64, limit int) ([]*model.SearchConsumerOffset, error) {
	m.trace("offset.ListDueForRetry")
	return nil, errUnimpl
}

type fakeDLQModel struct {
	*fakeBase
	counts map[string]int64
}

func newFakeDLQModel(b *fakeBase) *fakeDLQModel {
	return &fakeDLQModel{fakeBase: b, counts: map[string]int64{}}
}

func (m *fakeDLQModel) seedCount(state string, n int64) { m.counts[state] = n }

func (m *fakeDLQModel) Count(ctx context.Context, state string) (int64, error) {
	op := m.trace("dlq.Count:%s", state)
	if err, bad := m.failIf(op); bad {
		return 0, err
	}
	return m.counts[state], nil
}

func (m *fakeDLQModel) Insert(ctx context.Context, d *model.SearchDeadLetter) (bool, error) {
	m.trace("dlq.Insert:%s", d.EventID)
	return false, errUnimpl
}

func (m *fakeDLQModel) FindByEventID(ctx context.Context, eventID string) (*model.SearchDeadLetter, error) {
	m.trace("dlq.FindByEventID:%s", eventID)
	return nil, errUnimpl
}

func (m *fakeDLQModel) ListOpen(ctx context.Context, limit int) ([]*model.SearchDeadLetter, error) {
	m.trace("dlq.ListOpen")
	return nil, errUnimpl
}

func (m *fakeDLQModel) UpdateState(ctx context.Context, eventID, state string, now int64) error {
	m.trace("dlq.UpdateState:%s", eventID)
	return errUnimpl
}

// ------------------------------------------------------------------ 缓存 fake

// fakeCacher 是 repository.Cacher 的可观测实现：把「缓存里到底有什么」暴露成断言面。
// 语义必须与生产 *Cache 一致：未配置 Redis 时读 miss、抢锁放行。
type fakeCacher struct {
	*fakeBase
	active          map[string]string
	locks           map[string]bool
	configured      bool
	lockUnavailable bool
}

func newFakeCacher(b *fakeBase) *fakeCacher {
	return &fakeCacher{fakeBase: b, active: map[string]string{}, locks: map[string]bool{}, configured: true}
}

func (c *fakeCacher) GetActiveIndex(ctx context.Context, alias string) (string, bool) {
	c.trace("cache.GetActiveIndex:%s", alias)
	v, ok := c.active[alias]
	return v, ok
}

func (c *fakeCacher) SetActiveIndex(ctx context.Context, alias, index string) {
	c.trace("cache.SetActiveIndex:%s", alias)
	c.active[alias] = index
}

func (c *fakeCacher) DelActiveIndex(ctx context.Context, alias string) {
	c.trace("cache.DelActiveIndex:%s", alias)
	delete(c.active, alias)
}

func (c *fakeCacher) AcquireLock(ctx context.Context, key string, ttl int) (bool, error) {
	op := c.trace("cache.AcquireLock:%s|%d", key, ttl)
	if err, bad := c.failIf(op); bad {
		return false, err
	}
	if c.lockUnavailable || c.locks[key] {
		return false, nil
	}
	c.locks[key] = true
	return true, nil
}

func (c *fakeCacher) ReleaseLock(ctx context.Context, key string) {
	c.trace("cache.ReleaseLock:%s", key)
	delete(c.locks, key)
}

func (c *fakeCacher) Configured() bool { return c.configured }

func (c *fakeCacher) Ping(ctx context.Context) error { return nil }

// ------------------------------------------------------------------ 组装入口

type testStore struct {
	t      *testing.T
	base   *fakeBase
	es     *fakeES
	cache  *fakeCacher // 真实 *Cache(nil) 退化路径下为 nil
	taskMd *fakeTaskModel
	ver    *fakeVersionModel
	offset *fakeOffsetModel
	dlq    *fakeDLQModel
	opts   repository.Options // 归一化后真正生效的配置
	repo   *repository.Repository
	svcCtx *svc.ServiceContext
}

// newTestStore 组装真实 Repository（可观测缓存版本）。
func newTestStore(t *testing.T, opts repository.Options) *testStore {
	t.Helper()
	base := newFakeBase()
	es := newFakeES(base)
	cache := newFakeCacher(base)
	taskMd := newFakeTaskModel(base)
	ver := newFakeVersionModel(base)
	offset := newFakeOffsetModel(base)
	dlq := newFakeDLQModel(base)
	repo, err := repository.NewWithDeps(cache, es, opts, taskMd, ver, offset, dlq)
	if err != nil {
		t.Fatalf("组装真实 Repository 失败: %v", err)
	}
	return &testStore{t: t, base: base, es: es, cache: cache, taskMd: taskMd, ver: ver,
		offset: offset, dlq: dlq, opts: repo.Opts(), repo: repo,
		svcCtx: &svc.ServiceContext{Repository: repo}}
}

// newTestStoreNoRedis 走 NewWithModels(nil, ...)：缓存是生产 *Cache(nil)，
// 所有方法靠判空退化，用于验证「没有 Redis 时真实退化成什么样」。
func newTestStoreNoRedis(t *testing.T, opts repository.Options) *testStore {
	t.Helper()
	base := newFakeBase()
	es := newFakeES(base)
	taskMd := newFakeTaskModel(base)
	ver := newFakeVersionModel(base)
	offset := newFakeOffsetModel(base)
	dlq := newFakeDLQModel(base)
	repo, err := repository.NewWithModels(nil, es, opts, taskMd, ver, offset, dlq)
	if err != nil {
		t.Fatalf("组装真实 Repository 失败: %v", err)
	}
	return &testStore{t: t, base: base, es: es, taskMd: taskMd, ver: ver,
		offset: offset, dlq: dlq, opts: repo.Opts(), repo: repo,
		svcCtx: &svc.ServiceContext{Repository: repo}}
}

func (s *testStore) ops() []string             { return s.base.log.snapshot() }
func (s *testStore) fail(op string, err error) { s.base.faults.fail(op, err) }
func (s *testStore) resetOps()                 { s.base.log.ops = nil }
func (s *testStore) count(prefix string) int {
	n := 0
	for _, op := range s.base.log.ops {
		if strings.HasPrefix(op, prefix) {
			n++
		}
	}
	return n
}

// seedActiveIndex 静默登记「别名 → active 物理索引」，并让 OpenSearch 侧一致。
func (s *testStore) seedActiveIndex(alias, index string, docCount int64) *model.SearchIndexVersion {
	s.es.seedIndex(index, docCount)
	return s.ver.seed(&model.SearchIndexVersion{
		Alias: alias, IndexName: index, SchemaVersion: s.opts.SchemaVersion,
		DocCount: docCount, State: model.VersionStateActive, CreatedBy: "bootstrap",
		Ctime: 1000, Mtime: 1000,
	})
}

// seedCacheActive 静默预热写入索引缓存（模拟 30 秒 TTL 内的命中）。
func (s *testStore) seedCacheActive(alias, index string) { s.cache.active[alias] = index }

// ------------------------------------------------------------------ 断言助手

func wantNoErr(t *testing.T, err error, what ...string) {
	t.Helper()
	if err != nil {
		if len(what) > 0 {
			t.Fatalf("%s: 不该失败: %v", what[0], err)
		}
		t.Fatalf("不该失败: %v", err)
	}
}

func wantErrIs(t *testing.T, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: 错误 = %v, want errors.Is(%v)", what, err, target)
	}
}

func wantErr(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 期待失败，实际返回 nil", what)
	}
}

func wantEQ[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func wantContains(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%s = %q, 不包含 %q", what, got, want)
	}
}

// wantOps 断言**完整有序**的调用序列（不掩码）。
func wantOps(t *testing.T, ops []string, want ...string) {
	t.Helper()
	if len(ops) != len(want) || strings.Join(ops, " ") != strings.Join(want, " ") {
		t.Fatalf("调用序列不符\n got: %v\nwant: %v", ops, want)
	}
}

// wantOpsMasked 断言掩码掉时间戳后缀后的完整有序序列（用于自动生成的索引名）。
func wantOpsMasked(t *testing.T, ops []string, want ...string) {
	t.Helper()
	got := make([]string, 0, len(ops))
	for _, op := range ops {
		got = append(got, maskTS(op))
	}
	wantOps(t, got, want...)
}

func wantNoCall(t *testing.T, ops []string, prefix string) {
	t.Helper()
	for _, op := range ops {
		if strings.HasPrefix(op, prefix) {
			t.Fatalf("不该调用 %s（实际序列 %v）", prefix, ops)
		}
	}
}

// wantCount 断言某个前缀被调用的次数。
func wantCount(t *testing.T, ops []string, prefix string, want int) {
	t.Helper()
	got := 0
	for _, op := range ops {
		if strings.HasPrefix(op, prefix) {
			got++
		}
	}
	if got != want {
		t.Fatalf("%s 调用次数 = %d, want %d（序列 %v）", prefix, got, want, ops)
	}
}

func isRecentUnix(t *testing.T, v int64, what string) {
	t.Helper()
	now := time.Now().Unix()
	if v < now-30 || v > now+30 {
		t.Fatalf("%s = %d，不在当前时间 ±30s 内（%d）", what, v, now)
	}
}

func boolPtr(v bool) *bool { return &v }
