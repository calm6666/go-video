package logic

// submittask_test.go 覆盖 SubmitTask：入参守卫的顺序与零副作用、落库字段与应答投影、
// 存在性/幂等/长度三类校验的缺失，以及 Insert 失败的上抛口径。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

func submitCall(e *env, in *rpc.SubmitTaskReq) (*rpc.TaskReply, error) {
	return NewSubmitTaskLogic(context.Background(), e.svcCtx).SubmitTask(in)
}

// validSubmitReq 是一条四要素齐全的任务提交请求（各用例只在被考察的字段上做手脚）。
func validSubmitReq() *rpc.SubmitTaskReq {
	return &rpc.SubmitTaskReq{
		AssetId:      5001,
		TemplateId:   7,
		InputBucket:  "in-bucket",
		InputKey:     "raw/2026/09/23/mid-100.mp4",
		OutputBucket: "out-bucket",
		OutputKey:    "hls/pending/1080p.m3u8",
	}
}

// TestSubmitTask创建PENDING任务并回填自增主键 钉住写侧主链路：一次 INSERT、应答的 13 个字段
// 与库存那一行逐字相等（含 task_id 来自 LastInsertId 而不是 0），状态机入口是 PENDING(1)/进度 0。
func TestSubmitTask创建PENDING任务并回填自增主键(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := nowUnix()

	got, err := submitCall(e, validSubmitReq())
	wantNoErr(t, "SubmitTask", err)

	wantSeq(t, "轨迹", st.log, 0, "transcode_task.Insert:901")
	row := st.task(901)
	if row == nil {
		t.Fatalf("库里没有 task_id=901 这一行（应答 %+v）", got)
	}
	wantTaskReplyMatchesRow(t, "应答 vs 库存", got, row)

	wantEq(t, "新任务", "state", int32(got.GetState()), model.TaskStatePending)
	wantEq(t, "新任务", "progress", got.GetProgress(), int32(0))
	wantEq(t, "新任务", "errno", got.GetErrno(), int32(0))
	wantEq(t, "新任务", "err_msg", got.GetErrMsg(), "")
	// ctime/mtime 由 logic 同一个 now 变量赋两次，所以是严格相等（不是 assertAround）。
	wantEq(t, "新任务", "ctime == mtime", got.GetCtime(), got.GetMtime())
	assertAround(t, "新任务", "ctime", got.GetCtime(), now, 2)
	// 本期占位：不调 FFmpeg、不发 MQ、不写缓存（Repository.SubmitTask 是裸 Insert）。
	wantCount(t, "新任务", st.log, "cache.", 0)
	wantEq(t, "新任务", "库里总行数", st.taskRows(), 1)
}

// TestSubmitTask守卫先于触库 用 8 条越界入参钉住「拒绝发生在碰依赖之前、库里什么都不留」，
// 同时钉住第 3~6 条的**哨兵用错了**：四个字符串字段缺失返回的是 ErrInvalidState（任务状态非法），
// 而不是任何「参数缺失」类错误（README 已知缺口 2）。
func TestSubmitTask守卫先于触库(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.SubmitTaskReq)
		want   error
	}{
		{"asset_id 为 0", func(in *rpc.SubmitTaskReq) { in.AssetId = 0 }, model.ErrInvalidAssetID},
		{"asset_id 为负", func(in *rpc.SubmitTaskReq) { in.AssetId = -7 }, model.ErrInvalidAssetID},
		{"template_id 为 0", func(in *rpc.SubmitTaskReq) { in.TemplateId = 0 }, model.ErrInvalidTemplateID},
		{"template_id 为负", func(in *rpc.SubmitTaskReq) { in.TemplateId = -1 }, model.ErrInvalidTemplateID},
		{"input_bucket 空", func(in *rpc.SubmitTaskReq) { in.InputBucket = "" }, model.ErrInvalidState},
		{"input_key 空", func(in *rpc.SubmitTaskReq) { in.InputKey = "" }, model.ErrInvalidState},
		{"output_bucket 空", func(in *rpc.SubmitTaskReq) { in.OutputBucket = "" }, model.ErrInvalidState},
		{"output_key 空", func(in *rpc.SubmitTaskReq) { in.OutputKey = "" }, model.ErrInvalidState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			in := validSubmitReq()
			tc.mutate(in)

			got, err := submitCall(e, in)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回应答 %+v", tc.name, got)
			}
			wantNoCall(t, tc.name, e.st.log, 0)
			wantEq(t, tc.name, "库里行数", e.st.taskRows(), 0)
		})
	}
}

// TestSubmitTask守卫顺序是asset再template再字符串 判别性对照：四个字段**同时**越界时，
// 报出来的必须是第一个守卫（asset_id），说明校验顺序是 asset → template → 存储四要素。
func TestSubmitTask守卫顺序是asset再template再字符串(t *testing.T) {
	e := newEnv(t)
	in := &rpc.SubmitTaskReq{} // 全 0：三个守卫都可能报
	_, err := submitCall(e, in)
	wantErrIs(t, "全零请求", err, model.ErrInvalidAssetID)

	in.AssetId = 5001 // 放行第一个守卫，露出第二个
	_, err = submitCall(e, in)
	wantErrIs(t, "asset 合法后", err, model.ErrInvalidTemplateID)

	in.TemplateId = 7 // 放行第二个守卫，露出第三个
	_, err = submitCall(e, in)
	wantErrIs(t, "两个 id 都合法后", err, model.ErrInvalidState)
	wantNoCall(t, "三级守卫", e.st.log, 0)
}

// TestSubmitTask不做任何存在性校验 钉住：template_id 指向不存在的模板、asset_id 指向不存在的媒资，
// 任务照样落库。判别性在轨迹里——整条路径上**一次读都没有**（只有 1 条 INSERT），
// 所以既没查 transcode_template 也没查别的表，更没有调 asset RPC。
func TestSubmitTask不做任何存在性校验(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedTemplate(t, st, h264_1080p(7, "h264-1080p")) // 存在的模板，作对照

	_, err := submitCall(e, &rpc.SubmitTaskReq{
		AssetId: 5001, TemplateId: 7,
		InputBucket: "in-bucket", InputKey: "a.mp4", OutputBucket: "out", OutputKey: "b.m3u8",
	})
	wantNoErr(t, "已存在模板", err)

	in := validSubmitReq()
	in.TemplateId = 99999 // 不存在的模板
	in.AssetId = 88888    // 不存在的媒资
	got, err := submitCall(e, in)
	wantNoErr(t, "不存在的模板/媒资", err)
	wantEq(t, "不存在的模板", "应答 template_id", got.GetTemplateId(), int64(99999))

	wantSeq(t, "两次提交轨迹", st.log, 0,
		"transcode_task.Insert:901", "transcode_task.Insert:902")
	wantCount(t, "不查模板表", st.log, "transcode_template.", 0)
	wantCount(t, "不查任务表", st.log, "transcode_task.FindOne", 0)
	wantEq(t, "不存在的模板", "库里行数", st.taskRows(), 2)
}

// TestSubmitTask重放会创建两条任务 钉住无幂等键：Worker 超时重试或 MQ 重投（逐字节相同的请求）
// 会拿到两个 task_id，同一份媒资被转两遍（README 已知缺口 3）。
// 判别性对照：TestSubmitTask创建PENDING任务并回填自增主键 里「一次请求 = 一行 = 一个 id」仍然成立，
// 所以本用例的多出来的那一行来自重放本身，不是替身在自增上作弊。
func TestSubmitTask重放会创建两条任务(t *testing.T) {
	e := newEnv(t)
	st := e.st
	in := validSubmitReq()

	first, err := submitCall(e, in)
	wantNoErr(t, "首次提交", err)
	second, err := submitCall(e, in) // 逐字节相同的请求
	wantNoErr(t, "重放提交", err)

	wantSeq(t, "轨迹", st.log, 0, "transcode_task.Insert:901", "transcode_task.Insert:902")
	if first.GetTaskId() == second.GetTaskId() {
		t.Fatalf("重放拿到了同一个 task_id = %d，说明自增/应答有一处没真的落库", first.GetTaskId())
	}
	wantEq(t, "重放", "两条都是 PENDING", int32(second.GetState()), model.TaskStatePending)
	wantEq(t, "重放", "库里行数", st.taskRows(), 2)
}

// TestSubmitTask不校验空白与长度 钉住两类「守卫只挡空串」：纯空白四要素全部入库；
// 600 字符的 key（超过迁移里 VARCHAR(512)，见 000001_create_transcode_tables.sql:13）
// 也在 logic 侧畅通无阻，只能等 MySQL 抛 1406 变成 gRPC unknown。
func TestSubmitTask不校验空白与长度(t *testing.T) {
	t.Run("空白值入库", func(t *testing.T) {
		e := newEnv(t)
		in := validSubmitReq()
		in.InputBucket, in.InputKey = " ", "\t"
		in.OutputBucket, in.OutputKey = "  ", "\n"
		got, err := submitCall(e, in)
		wantNoErr(t, "空白四要素", err)
		wantEq(t, "空白四要素", "input_bucket 原样入库", got.GetInputBucket(), " ")
		wantEq(t, "空白四要素", "input_key 原样入库", got.GetInputKey(), "\t")
		wantEq(t, "空白四要素", "库里行数", e.st.taskRows(), 1)
	})

	t.Run("超长 key 入库", func(t *testing.T) {
		e := newEnv(t)
		in := validSubmitReq()
		in.InputKey = strings.Repeat("x", 600)
		got, err := submitCall(e, in)
		wantNoErr(t, "600 字符 input_key", err)
		wantEq(t, "超长", "应答里的长度", int32(len(got.GetInputKey())), int32(600))
		wantEq(t, "超长", "库存里的长度", len(e.st.task(901).InputKey), 600)
	})
}

// TestSubmitTask插入失败原样上抛 钉住错误链：model 的 `transcode_task Insert: %w` 包装保留、
// 不降级不吞错、应答为 nil，并且**不写缓存**（所以调用方无从拿到一个「看起来已创建」的 task_id）。
// 本仓 fake 不回滚，transcode 也只有单条 INSERT，所以这里断言的是「库里 0 行」而不是回滚。
func TestSubmitTask插入失败原样上抛(t *testing.T) {
	e := newEnv(t)
	st := e.st
	dbErr := errors.New("dial tcp 127.0.0.1:3306: connectex: refused")
	st.s.fail("transcode_task.Insert", dbErr)

	got, err := submitCall(e, validSubmitReq())
	wantErrIs(t, "Insert 失败", err, dbErr)
	wantErrContains(t, "Insert 失败", err, "transcode_task Insert:")
	if got != nil {
		t.Errorf("Insert 失败仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0, "transcode_task.Insert:901")
	wantEq(t, "Insert 失败", "库里行数", st.taskRows(), 0)
	wantCount(t, "Insert 失败", st.log, "cache.", 0)
}

// TestSubmitTask插入失败不自增主键 是上一条的判别性对照：同一次失败之后再来一条正常请求，
// 拿到的仍是 901（替身刻意不在失败时消耗自增号），所以失败用例不会把后续用例的轨迹推歪。
func TestSubmitTask插入失败不自增主键(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.s.fail("transcode_task.Insert", errors.New("db is down"))
	_, err := submitCall(e, validSubmitReq())
	wantErrContains(t, "先失败一次", err, "db is down")

	st.s.fail("transcode_task.Insert", nil) // 撤掉故障
	got, err := submitCall(e, validSubmitReq())
	wantNoErr(t, "故障恢复后", err)
	wantEq(t, "故障恢复后", "task_id", got.GetTaskId(), int64(901))
}
