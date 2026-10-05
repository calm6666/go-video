package model

// live_stream_output 的 SQL 形状测试（本轮只覆盖断流整场下线新增的 ListOnlineBySession）。
//
// 为什么这层必须单独有测试：logic 单测把整张表换成了内存替身，替身只能证明
// 「logic 拿到这些行会怎么裁决」，证明不了「发给驱动的到底是哪条语句、实参什么顺序」。
// 而 ListOnlineBySession 的两个致命写法都不影响 logic 层结论：
//   - WHERE 里 live_session_id 与 room_id 的实参顺序写反：查询照样成功，
//     但会拿别人的档位行去下线（下线本身按 output_id 条件下线，返回码还是 1），
//     观众侧表现为「另一间房的直播被摘档」；
//   - LIMIT 少取一行（写成 MaxSessionOutputs 而不是 +1）：溢出永远判不出来，
//     脏数据会被当成「这一场只有 64 个档位」静默处理。
// 这两条只有钉住 SQL 与实参才能红。手法先例见本包 live_media_outbox_sql_test.go。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// recordingSQL 只实现档位表读侧用到的 QueryRowsCtx，其余一律 panic：
// 「用例其实什么都没断言」必须炸，不能安静通过。
type recordingSQL struct {
	calls   []recordedCall
	rows    []*LiveStreamOutput // 灌进 QueryRowsCtx 的出参，用来构造「扫到 N 行」这种库里态
	rowErr  error
	aff     int64
	affErr  error
	execErr error
	// rowsErr 让 QueryRowsCtx 返回错误（含 sql.ErrNoRows 这条特殊分支）。
	rowsErr error
	// countRes 供 QueryRowCtx 的 COUNT(*) 用；rows/ 计数都不是本表主路径。
	countRes int64
}

var _ sqlx.SqlConn = (*recordingSQL)(nil)

func (f *recordingSQL) ExecCtx(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.calls = append(f.calls, recordedCall{query: query, args: args})
	if f.execErr != nil {
		return nil, f.execErr
	}
	return fakeAffected{n: f.aff}, f.affErr
}

func (f *recordingSQL) QueryRowsCtx(_ context.Context, v any, query string, args ...any) error {
	f.calls = append(f.calls, recordedCall{query: query, args: args})
	if f.rowsErr != nil {
		return f.rowsErr
	}
	if rows, ok := v.(*[]*LiveStreamOutput); ok {
		*rows = append([]*LiveStreamOutput{}, f.rows...)
		return nil
	}
	panic("档位表的多行查询只能扫进 []*LiveStreamOutput：" + query)
}

func (f *recordingSQL) one(t *testing.T) recordedCall {
	t.Helper()
	if len(f.calls) != 1 {
		t.Fatalf("期望恰好发出 1 条 SQL，实际 %d：%+v", len(f.calls), f.calls)
	}
	return f.calls[0]
}

func (f *recordingSQL) none(t *testing.T) {
	t.Helper()
	if len(f.calls) != 0 {
		t.Fatalf("校验必须在 SQL 之前拦住，却发出了 %d 条 SQL：%+v", len(f.calls), f.calls)
	}
}

func (f *recordingSQL) Exec(query string, args ...any) (sql.Result, error) {
	panic("model 必须走 Ctx 版本：" + query)
}

func (f *recordingSQL) Prepare(query string) (sqlx.StmtSession, error) {
	panic("档位表不使用预编译语句")
}

func (f *recordingSQL) PrepareCtx(_ context.Context, query string) (sqlx.StmtSession, error) {
	panic("档位表不使用预编译语句")
}

func (f *recordingSQL) QueryRowCtx(_ context.Context, v any, query string, args ...any) error {
	f.calls = append(f.calls, recordedCall{query: query, args: args})
	if f.rowErr != nil {
		return f.rowErr
	}
	if p, ok := v.(*int64); ok {
		*p = f.countRes
		return nil
	}
	panic("档位表的单行查询只用于 COUNT(*)：" + query)
}

func (f *recordingSQL) QueryRow(v any, query string, args ...any) error {
	panic("model 必须走 Ctx 版本：" + query)
}

func (f *recordingSQL) QueryRowPartialCtx(_ context.Context, _ any, query string, _ ...any) error {
	panic("档位表不使用 partial 查询：" + query)
}

func (f *recordingSQL) QueryRowPartial(v any, query string, args ...any) error {
	panic("档位表不使用 partial 查询：" + query)
}

func (f *recordingSQL) QueryRows(v any, query string, args ...any) error {
	panic("model 必须走 Ctx 版本：" + query)
}

func (f *recordingSQL) QueryRowsPartialCtx(_ context.Context, _ any, query string, _ ...any) error {
	panic("档位表不使用 partial 查询：" + query)
}

func (f *recordingSQL) QueryRowsPartial(v any, query string, args ...any) error {
	panic("档位表不使用 partial 查询：" + query)
}

func (f *recordingSQL) TransactCtx(_ context.Context, fn func(context.Context, sqlx.Session) error) error {
	panic("model 层不得自行开事务：档位下线的事务边界由 logic 的 Transact 决定")
}

func (f *recordingSQL) Transact(fn func(sqlx.Session) error) error {
	panic("model 层不得自行开事务（且必须走 Ctx 版本）")
}

func (f *recordingSQL) RawDB() (*sql.DB, error) {
	return nil, errors.New("档位表测试不接触真实连接池")
}

func onlineRows(n int) []*LiveStreamOutput {
	rows := make([]*LiveStreamOutput, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, &LiveStreamOutput{
			OutputId: int64(1000 + i), RoomId: 71001, LiveSession: 88001,
			BitrateLevel: int32(i + 1), Protocol: 1, // 1 = rpc STREAM_PROTOCOL_HLS
			State: StreamOutputStateOnline,
		})
	}
	return rows
}

// TestListOnlineBySessionGuardsRunBeforeSQL 钉住两个坐标都在发 SQL 之前判掉。
// 少了任一条，session_id=0 就会变成「按房间下线所有场次」，room_id 漏判则等价于全表扫在线档位。
func TestListOnlineBySessionGuardsRunBeforeSQL(t *testing.T) {
	cases := []struct {
		name      string
		room, ses int64
		want      error
	}{
		{"房间为 0", 0, 88001, ErrInvalidRoomID},
		{"房间为负", -1, 88001, ErrInvalidRoomID},
		{"场次为 0", 71001, 0, ErrInvalidSessionID},
		{"场次为负", 71001, -7, ErrInvalidSessionID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &recordingSQL{}
			rows, err := NewLiveStreamOutputModel(conn).ListOnlineBySession(context.Background(), tc.room, tc.ses)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got=%v want=%v", err, tc.want)
			}
			if rows != nil {
				t.Fatalf("非法入参必须返回空结果，got=%d 行", len(rows))
			}
			conn.none(t)
		})
	}
}

// TestListOnlineBySessionSQLShape 钉住谓词、索引顺序与实参顺序。
// 实参顺序是这里唯一能证明「场次在前、房间只做归属校验」的地方：写反了测试不红就会静默串房。
func TestListOnlineBySessionSQLShape(t *testing.T) {
	conn := &recordingSQL{}
	if _, err := NewLiveStreamOutputModel(conn).ListOnlineBySession(context.Background(), 71001, 88001); err != nil {
		t.Fatalf("正常查询不该报错：%v", err)
	}
	call := conn.one(t)

	// 显式列名，不许 SELECT *：加列时结构体不会静默错位。
	if !strings.HasPrefix(call.query, liveStreamOutputColumns) {
		t.Fatalf("必须走显式列名，got=%s", call.query)
	}
	where, ok := substrAfter(call.query, "WHERE ")
	if !ok {
		t.Fatalf("查询没有 WHERE 段：%s", call.query)
	}
	// 三条谓词一条都不能少，且顺序必须是 (场次, 状态, 房间)：与 idx_session_state 对齐。
	for i, want := range []string{"live_session_id = ?", "state = ?", "room_id = ?"} {
		got, _ := splitN(where, i)
		if got != want {
			t.Fatalf("WHERE 第 %d 条谓词应为 %s，实际 WHERE=%s", i+1, want, where)
		}
	}
	if !strings.Contains(call.query, "ORDER BY bitrate_level ASC, protocol ASC") {
		t.Fatalf("必须按 (档位, 协议) 升序返回，下线顺序才可复现：%s", call.query)
	}
	if !strings.Contains(call.query, "LIMIT ?") {
		t.Fatalf("溢出判定依赖 LIMIT，必须参数化：%s", call.query)
	}
	// 实参：sessionID, state=online, roomID, MaxSessionOutputs+1
	wantArgs := []any{int64(88001), StreamOutputStateOnline, int64(71001), MaxSessionOutputs + 1}
	if len(call.args) != len(wantArgs) {
		t.Fatalf("实参数数 got=%d want=%d：%+v", len(call.args), len(wantArgs), call.args)
	}
	for i := range wantArgs {
		if call.args[i] != wantArgs[i] {
			t.Fatalf("实参第 %d 个 got=%v(%T) want=%v(%T)：房间与场次写反会让下线打到别的房间",
				i, call.args[i], call.args[i], wantArgs[i], wantArgs[i])
		}
	}
}

// TestListOnlineBySessionOverflowIsErrorNotTruncation 钉住「扫到第 65 行必须报错」。
// 只判 len>上限 而不报错、或把 LIMIT 写成 MaxSessionOutputs，都会让脏数据被静默处理。
func TestListOnlineBySessionOverflowIsErrorNotTruncation(t *testing.T) {
	t.Run("恰好到上限放行", func(t *testing.T) {
		conn := &recordingSQL{rows: onlineRows(MaxSessionOutputs)}
		rows, err := NewLiveStreamOutputModel(conn).ListOnlineBySession(context.Background(), 71001, 88001)
		if err != nil {
			t.Fatalf("到上限不是溢出：%v", err)
		}
		if len(rows) != MaxSessionOutputs {
			t.Fatalf("got=%d 行 want=%d", len(rows), MaxSessionOutputs)
		}
	})
	t.Run("多一行即溢出", func(t *testing.T) {
		conn := &recordingSQL{rows: onlineRows(MaxSessionOutputs + 1)}
		rows, err := NewLiveStreamOutputModel(conn).ListOnlineBySession(context.Background(), 71001, 88001)
		if !errors.Is(err, ErrSessionOutputOverflow) {
			t.Fatalf("必须报档位溢出：got=%v", err)
		}
		if rows != nil {
			t.Fatalf("溢出不得返回部分行（调用方会挑一部分下线）：%d 行", len(rows))
		}
	})
}

// TestListOnlineBySessionDistinguishesEmptyFromFailure：查无档位是成功结论（Noop），
// 驱动报错必须原样上抛让消费者不提交位点。两者混为一谈会把故障洗成「这场没开档位」。
func TestListOnlineBySessionDistinguishesEmptyFromFailure(t *testing.T) {
	t.Run("空结果", func(t *testing.T) {
		conn := &recordingSQL{}
		rows, err := NewLiveStreamOutputModel(conn).ListOnlineBySession(context.Background(), 71001, 88001)
		if err != nil {
			t.Fatalf("没有在线档位不是错误：%v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("got=%d 行 want 0", len(rows))
		}
	})
	t.Run("驱动报错", func(t *testing.T) {
		conn := &recordingSQL{rowsErr: errors.New("too many connections")}
		rows, err := NewLiveStreamOutputModel(conn).ListOnlineBySession(context.Background(), 71001, 88001)
		if err == nil || !strings.Contains(err.Error(), "too many connections") {
			t.Fatalf("上游原因必须带上：%v", err)
		}
		if !strings.Contains(err.Error(), "live_stream_output ListOnlineBySession") {
			t.Fatalf("错误必须点名是哪条查询：%v", err)
		}
		if rows != nil {
			t.Fatalf("失败不得返回行：%+v", rows)
		}
	})
	t.Run("sql.ErrNoRows 视为空", func(t *testing.T) {
		conn := &recordingSQL{rowsErr: sql.ErrNoRows}
		rows, err := NewLiveStreamOutputModel(conn).ListOnlineBySession(context.Background(), 71001, 88001)
		if err != nil {
			t.Fatalf("ErrNoRows 应按空结果处理：%v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("got=%d 行 want 0", len(rows))
		}
	})
}

// substrAfter / splitN 是断言用小工具：把 WHERE 段按 " AND " 拆开逐条比，
// 比整串相等更能指出「哪一条谓词丢了或换了位置」。
func substrAfter(s, mark string) (string, bool) {
	i := strings.Index(s, mark)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(mark):]
	if j := strings.Index(rest, " ORDER BY "); j >= 0 {
		rest = rest[:j]
	}
	return rest, true
}

func splitN(where string, n int) (string, int) {
	parts := strings.Split(where, " AND ")
	if n >= len(parts) {
		return "", len(parts)
	}
	return parts[n], len(parts)
}
