package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/recommend-recall/model"
)

func TestNewRejectsNilConn(t *testing.T) {
	// 本服务所有方法都要读 MySQL，没有"没库也能起"的路径：构造期必须失败。
	if _, err := New(nil, nil, validOptions()); err == nil {
		t.Fatal("conn 为 nil 时 New 竟然成功")
	}
}

func TestNewRejectsOptionsBeyondModelCaps(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "MaxCandidates 为 0", mutate: func(o *Options) { o.MaxCandidates = 0 }},
		{name: "MaxCandidates 超上限", mutate: func(o *Options) { o.MaxCandidates = model.MaxPoolQueryLimit + 1 }},
		{name: "MaxBatchItems 超上限", mutate: func(o *Options) { o.MaxBatchItems = model.MaxPoolItemBatch + 1 }},
		{name: "MaxVersionList 超上限", mutate: func(o *Options) { o.MaxVersionList = model.MaxVersionListLimit + 1 }},
		{name: "MaxRequestLogPage 超上限", mutate: func(o *Options) { o.MaxRequestLogPage = model.MaxRequestLogPageSize + 1 }},
		{name: "MinKeepVersions 小于 1", mutate: func(o *Options) { o.MinKeepVersions = 0 }},
		{name: "MaxSeedAids 为 0", mutate: func(o *Options) { o.MaxSeedAids = 0 }},
		{name: "PoolStaleSeconds 为 0", mutate: func(o *Options) { o.PoolStaleSeconds = 0 }},
		{name: "幂等保留期不长于租约", mutate: func(o *Options) { o.IdempotencyRetentionSeconds = o.IdempotencyLeaseSeconds }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			opts := validOptions()
			tc.mutate(&opts)
			err := func() error {
				_, err := New(fakeSqlConn{}, nil, opts)
				return err
			}()
			if err == nil {
				t.Fatalf("配置 %s 应当启动失败，实际通过", tc.name)
			}
			if !strings.Contains(err.Error(), "recommend-recall") {
				t.Errorf("错误信息缺少服务名前缀: %v", err)
			}
		})
	}
	if _, err := New(fakeSqlConn{}, nil, validOptions()); err != nil {
		t.Fatalf("合法配置被拒绝: %v", err)
	}
}

func TestNewWiresOwnTablesAndStubbedDownstream(t *testing.T) {
	repo, err := New(fakeSqlConn{}, nil, validOptions())
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	// 六张自有表必须有 model，第二轮 logic 直接依赖它们。
	for name, m := range map[string]any{
		"Pool": repo.Pool, "PoolVersion": repo.PoolVersion, "Current": repo.Current,
		"RequestLog": repo.RequestLog, "Outbox": repo.Outbox, "Idempotency": repo.Idempotency,
	} {
		if m == nil {
			t.Errorf("%s 未装配", name)
		}
	}
	if _, ok := repo.Features.(UnconfiguredFeatureSource); !ok {
		t.Errorf("Features 应为显式 stub，实际 %T（禁止出现半接线的下游实现）", repo.Features)
	}
	if _, ok := repo.Visibility.(UnconfiguredVisibilitySource); !ok {
		t.Errorf("Visibility 应为显式 stub，实际 %T", repo.Visibility)
	}
	if got := repo.Options().MaxCandidates; got != validOptions().MaxCandidates {
		t.Errorf("Options() 回读=%d，与构造入参不一致", got)
	}
}

// TestUnconfiguredSourcesFailLoudly 是"红线"测试：
// 未接线的下游一律报错，且必须把集合返回成 nil —— 空集合会被 logic 读成"该用户没有兴趣/全部可见"，
// 那是比报错更严重的错误结论（会静默少降级、静默放行不可见稿件）。
func TestUnconfiguredSourcesFailLoudly(t *testing.T) {
	ctx := context.Background()
	f := UnconfiguredFeatureSource{}
	vs := UnconfiguredVisibilitySource{}

	if got, err := f.GetUserInterest(ctx, 1, 10); !errors.Is(err, ErrSourceNotConfigured) || got != nil {
		t.Errorf("GetUserInterest: err=%v got=%v", err, got)
	}
	if got, err := f.GetBehaviorSignals(ctx, 1, time.Hour, 10); !errors.Is(err, ErrSourceNotConfigured) || got != nil {
		t.Errorf("GetBehaviorSignals: err=%v got=%v", err, got)
	}
	if got, err := f.ListHotSubjects(ctx, 0, time.Hour, 10); !errors.Is(err, ErrSourceNotConfigured) || got != nil {
		t.Errorf("ListHotSubjects: err=%v got=%v（必须返回 nil 切片而不是空切片）", err, got)
	}
	if got, err := f.GetVectorCandidates(ctx, 1, 10); !errors.Is(err, ErrSourceNotConfigured) || got != nil {
		t.Errorf("GetVectorCandidates: err=%v got=%v", err, got)
	}
	if got, err := vs.FilterVisible(ctx, []int64{1, 2}); !errors.Is(err, ErrSourceNotConfigured) || got != nil {
		t.Errorf("FilterVisible: err=%v got=%v（空子集会被读成\"全部可见\"）", err, got)
	}
	if got, err := vs.FollowingUps(ctx, 1, 10); !errors.Is(err, ErrSourceNotConfigured) || got != nil {
		t.Errorf("FollowingUps: err=%v got=%v", err, got)
	}
	if got, err := vs.BlockedUps(ctx, 1, 10); !errors.Is(err, ErrSourceNotConfigured) || got != nil {
		t.Errorf("BlockedUps: err=%v got=%v", err, got)
	}
}

func TestTransact(t *testing.T) {
	fc := &recordingConn{}
	repo, err := New(fc, nil, validOptions())
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := repo.Transact(context.Background(), nil); err == nil {
		t.Error("Transact(nil) 应当报错而不是 panic")
	}

	// 事务内的语句必须带 session 走同一条连接：这是发布流程"指针 + 版本状态 + 事件"原子性的前提。
	err = repo.Transact(context.Background(), func(session sqlx.Session) error {
		if session == nil {
			t.Error("事务回调拿不到 session，无法把指针切换与事件登记放进同一事务")
		}
		if _, err := session.ExecCtx(context.Background(), "UPDATE recall_pool_current SET version = ? WHERE source = ?", 5, 1); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("事务内错误必须上抛（否则 ErrSwitchConflict 会被吞掉）")
	}
	if !fc.committed {
		t.Error("回调没在事务内执行（TransactCtx 未被走到）")
	}
	if len(fc.stmts) != 1 {
		t.Errorf("事务内语句数=%d，期望 1 条: %v", len(fc.stmts), fc.stmts)
	}
}

func TestRequestFingerprint(t *testing.T) {
	a := RequestFingerprint("1", "global", "17", "batch-1")
	b := RequestFingerprint("1", "global", "17", "batch-1")
	c := RequestFingerprint("1", "global", "17", "batch-2")
	if a != b {
		t.Errorf("同输入指纹不稳定: %s != %s", a, b)
	}
	if a == c {
		t.Error("不同 batch_id 得到同一指纹，幂等表无法区分请求")
	}
	if len(a) != model.RequestHashLen {
		t.Errorf("指纹长度 %d != %d，会撑爆 request_hash 列", len(a), model.RequestHashLen)
	}
	if strings.ContainsAny(a, " ") {
		t.Errorf("指纹含空白，不是 hex: %q", a)
	}
}

func validOptions() Options {
	return Options{
		MaxCandidates:               400,
		DefaultLimit:                120,
		PerSourceMax:                120,
		MaxSeedAids:                 20,
		MaxSeedTags:                 20,
		MaxExcludeAids:              500,
		MaxBatchItems:               1000,
		MaxVersionList:              100,
		MaxPoolSnapshotPage:         200,
		MaxRequestLogPage:           100,
		MinKeepVersions:             2,
		PoolStaleSeconds:            1800,
		IdempotencyLeaseSeconds:     300,
		IdempotencyRetentionSeconds: 86400,
	}
}

// ---------------------------------------------------------------- SQL 替身
//
// 这些替身只满足 sqlx.SqlConn 接口，不 import 任何 driver、不建立连接：
// 本仓库测试绝对禁止连到本机 3306（那是用户的真实 MySQL）。

type fakeSqlConn struct{}

func (fakeSqlConn) TransactCtx(context.Context, func(context.Context, sqlx.Session) error) error {
	return errors.New("fakeSqlConn: TransactCtx unused in this test")
}
func (fakeSqlConn) Transact(func(sqlx.Session) error) error { return errors.New("not used") }
func (fakeSqlConn) RawDB() (*sql.DB, error)                 { return nil, errors.New("not used") }
func (fakeSqlConn) ExecCtx(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("not used")
}
func (fakeSqlConn) Exec(string, ...any) (sql.Result, error) { return nil, errors.New("not used") }
func (fakeSqlConn) Prepare(string) (sqlx.StmtSession, error) {
	return nil, errors.New("not used")
}
func (fakeSqlConn) PrepareCtx(context.Context, string) (sqlx.StmtSession, error) {
	return nil, errors.New("not used")
}
func (fakeSqlConn) QueryRowCtx(context.Context, any, string, ...any) error { return sql.ErrNoRows }
func (fakeSqlConn) QueryRow(any, string, ...any) error                     { return sql.ErrNoRows }
func (fakeSqlConn) QueryRowPartialCtx(context.Context, any, string, ...any) error {
	return sql.ErrNoRows
}
func (fakeSqlConn) QueryRowPartial(any, string, ...any) error { return sql.ErrNoRows }
func (fakeSqlConn) QueryRowsCtx(context.Context, any, string, ...any) error {
	return sql.ErrNoRows
}
func (fakeSqlConn) QueryRows(any, string, ...any) error { return sql.ErrNoRows }
func (fakeSqlConn) QueryRowsPartialCtx(context.Context, any, string, ...any) error {
	return sql.ErrNoRows
}
func (fakeSqlConn) QueryRowsPartial(any, string, ...any) error { return sql.ErrNoRows }

// recordingConn 把事务内的语句记下来，用于断言"同事务"这一事实。
type recordingConn struct {
	fakeSqlConn
	stmts     []string
	inTx      bool
	committed bool
}

func (c *recordingConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.inTx = true
	c.committed = true
	defer func() { c.inTx = false }()
	return fn(ctx, c)
}

func (c *recordingConn) ExecCtx(_ context.Context, query string, _ ...any) (sql.Result, error) {
	if !c.inTx {
		return nil, errors.New("recordingConn: 语句必须发生在事务内")
	}
	c.stmts = append(c.stmts, query)
	return fakeResult{}, nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 1, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

var (
	_ sqlx.SqlConn = fakeSqlConn{}
	_ sqlx.SqlConn = (*recordingConn)(nil)
)
