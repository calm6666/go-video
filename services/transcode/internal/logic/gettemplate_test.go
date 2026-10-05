package logic

// gettemplate_test.go 覆盖 GetTemplate 的 cache-aside（模板侧）。
// 与 GetTask 同构，但三处差异必须单独钉住：
//   - 键前缀是 `tc:tpl:`（与 `tc:task:` 同 id 也互不可见，见最后一条用例）；
//   - 失败哨兵是 ErrTemplateNotFound，不是 ErrTaskNotFound；
//   - 模板缓存**没有失效路径**（Cacher 里没有 DelTemplate，Cache.DelTemplate 零调用方），
//     所以脏数据/旧数据的窗口是 600s 而不是任务侧的 60s（README 已知缺口 11）。
//
// TTL 常量（repository.go:29-30）藏在 *Cache 里、不进 Cacher 签名，因此本文件不断言秒数。

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

func getTemplateCall(e *env, templateID int64) (*rpc.TemplateReply, error) {
	return NewGetTemplateLogic(context.Background(), e.svcCtx).GetTemplate(&rpc.TemplateReq{TemplateId: templateID})
}

// seedTpl101 布一条典型模板（h264/1920x1080/6000kbps/30fps/6s 分片）。
// 时间列刻意用固定值 1700000000，便于断言「应答里根本没有时间字段」。
func seedTpl101(t *testing.T, st *store) *model.TranscodeTemplate {
	t.Helper()
	row := seedTemplate(t, st, h264_1080p(101, "h264-1080p"))
	if row == nil {
		t.Fatal("布景失败")
	}
	return row
}

// TestGetTemplate未命中时回源并回填整行 钉住模板读穿的三步序（先缓存、再库、最后回填），
// 以及回填载荷就是库存那一行的 JSON。
func TestGetTemplate未命中时回源并回填整行(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedTpl101(t, st)

	got, err := getTemplateCall(e, 101)
	wantNoErr(t, "GetTemplate", err)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTemplate:tc:tpl:101",
		"transcode_template.FindOne:101",
		"cache.SetTemplate:tc:tpl:101",
	)
	wantTemplateReplyMatchesRow(t, "应答 vs 库存", got, row)

	payload, ok := st.cache.raw(keyTemplate(101))
	if !ok {
		t.Fatal("回填没有写入 tc:tpl:101")
	}
	var decoded model.TranscodeTemplate
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("回填的不是合法 JSON：%v（载荷 %q）", err, payload)
	}
	wantTemplateReplyMatchesRow(t, "回填载荷 vs 库存", toTemplateReply(&decoded), row)
	wantEq(t, "回填载荷", "解码出的 mtime", decoded.Mtime, row.Mtime)
}

// TestGetTemplate命中缓存时不查库 与上一条构成对照：同一个 template_id 第二次读只剩一次缓存读。
func TestGetTemplate命中缓存时不查库(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedTpl101(t, st)

	if _, err := getTemplateCall(e, 101); err != nil { // 第一次读负责回填
		t.Fatalf("首次读失败：%v", err)
	}
	before := st.log.snapshot()
	got, err := getTemplateCall(e, 101)
	wantNoErr(t, "第二次读", err)
	wantSeq(t, "第二次读的轨迹", st.log, before, "cache.GetTemplate:tc:tpl:101")
	wantEq(t, "第二次读", "codec 仍来自缓存", got.GetCodec(), "h264")
}

// TestGetTemplate缓存里的旧码率胜出 钉住「模板读侧最多脏 600s」：库里已被改成 8000kbps，
// 缓存里还是 6000，应答就是 6000，而且一次库都不查。窗口是任务侧（60s）的 10 倍，
// 因为 repository.UpdateTemplate 这类失效路径根本不存在（README 已知缺口 11）。
func TestGetTemplate缓存里的旧码率胜出(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedTpl101(t, st)
	warmTemplateCache(t, st, row) // 缓存：bitrate=6000

	st.tpls.poke(101, func(r *model.TranscodeTemplate) { r.Bitrate = 8000 }) // 库存/人工改库

	got, err := getTemplateCall(e, 101)
	wantNoErr(t, "脏模板缓存", err)
	wantEq(t, "脏模板缓存", "应答 bitrate（旧值）", got.GetBitrate(), int32(6000))
	wantEq(t, "脏模板缓存", "库存 bitrate（新值）", st.tpl(101).Bitrate, int32(8000))
	wantSeq(t, "轨迹", st.log, 0, "cache.GetTemplate:tc:tpl:101")
}

// TestGetTemplate查无此行返回哨兵且不写负缓存 钉住 model 的 (nil,nil) 在 repository
// 被翻译成 ErrTemplateNotFound（repository.go:222-224），且失败路径不留键。
func TestGetTemplate查无此行返回哨兵且不写负缓存(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedTpl101(t, st) // 库里有一行，证明「查不到」不是因为表空

	got, err := getTemplateCall(e, 404)
	wantErrIs(t, "不存在的模板", err, model.ErrTemplateNotFound)
	if got != nil {
		t.Errorf("拒绝时仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTemplate:tc:tpl:404", "transcode_template.FindOne:404")
	wantCount(t, "负缓存", st.log, "cache.SetTemplate", 0)
	if _, ok := st.cache.raw(keyTemplate(404)); ok {
		t.Error("查无此行却留下了缓存键")
	}
}

// TestGetTemplate脏缓存报错后无人能清键 钉住 repository.go:213-215：Unmarshal 失败即上抛，
// 既不回源也不清键——而模板侧压根没有清键方法（Cacher 无 DelTemplate）。
// 结论：一条脏数据能让某个 template_id 的详情读在 600s 内**恒定失败**，
// 期间没有任何 RPC 能救（ListTemplates 不受影响，见 listtemplates_test.go 的对照用例）。
func TestGetTemplate脏缓存报错后无人能清键(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedTpl101(t, st)
	st.cache.kv[keyTemplate(101)] = `{"TemplateId":101,` // 半截 JSON

	got, err := getTemplateCall(e, 101)
	wantErrContains(t, "脏模板缓存", err, "transcode/GetTemplate unmarshal cache")
	if got != nil {
		t.Errorf("脏缓存路径仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0, "cache.GetTemplate:tc:tpl:101")
	wantCount(t, "脏缓存不回源", st.log, "transcode_template.FindOne", 0)
	wantCount(t, "模板侧没有清键路径", st.log, "cache.Del", 0)
	if _, ok := st.cache.raw(keyTemplate(101)); !ok {
		t.Error("脏键被清掉了，与生产语义不符")
	}
}

// TestGetTemplate缓存读失败时不降级回源 与「未命中回源」对照：
// miss（无错误）会走到 FindOne，带错误的 GetTemplate 直接返回，一次库都不查。
func TestGetTemplate缓存读失败时不降级回源(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedTpl101(t, st)
	redisDown := errors.New("redis: connection refused")
	st.s.fail("cache.GetTemplate", redisDown)

	got, err := getTemplateCall(e, 101)
	wantErrIs(t, "缓存读失败", err, redisDown)
	if got != nil {
		t.Errorf("缓存读失败仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0, "cache.GetTemplate:tc:tpl:101")
	wantCount(t, "缓存读失败", st.log, "transcode_template.FindOne", 0)
}

// TestGetTemplate库读失败原样上抛且不回填 钉住错误包装（`transcode_template FindOne: %w`）
// 与「失败路径不留脏键」。
func TestGetTemplate库读失败原样上抛且不回填(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedTpl101(t, st)
	st.s.fail("transcode_template.FindOne", errors.New("error 1146: table doesn't exist"))

	_, err := getTemplateCall(e, 101)
	wantErrContains(t, "库读失败", err, "transcode_template FindOne:")
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTemplate:tc:tpl:101", "transcode_template.FindOne:101")
	wantCount(t, "库读失败", st.log, "cache.SetTemplate", 0)
}

// TestGetTemplate回填失败被吞 钉住 repository.go:226 的 `_ = r.cache.SetTemplate(...)`：
// 缓存写挂了不影响读接口可用性，代价是下一次仍然回源（并再次尝试回填）。
func TestGetTemplate回填失败被吞(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedTpl101(t, st)
	st.s.fail("cache.SetTemplate", errors.New("OOM command not allowed"))

	got, err := getTemplateCall(e, 101)
	wantNoErr(t, "回填失败", err)
	wantTemplateReplyMatchesRow(t, "回填失败仍返回数据", got, row)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTemplate:tc:tpl:101", "transcode_template.FindOne:101", "cache.SetTemplate:tc:tpl:101")
	if _, ok := st.cache.raw(keyTemplate(101)); ok {
		t.Error("SetTemplate 报错了但键仍然写进去了，替身与生产口径不符")
	}
}

// TestGetTemplate非正template_id先于任何依赖被拒 钉住守卫顺序（0 与负数两侧都拒，1 放行）。
func TestGetTemplate非正template_id先于任何依赖被拒(t *testing.T) {
	for _, id := range []int64{0, -1, -101} {
		e := newEnv(t)
		seedTpl101(t, e.st)
		got, err := getTemplateCall(e, id)
		wantErrIs(t, "template_id 越界", err, model.ErrInvalidTemplateID)
		if got != nil {
			t.Errorf("template_id=%d 拒绝时仍返回应答 %+v", id, got)
		}
		wantNoCall(t, "template_id 越界", e.st.log, 0)
	}
	e := newEnv(t)
	seedTpl101(t, e.st)
	_, err := getTemplateCall(e, 1) // 判别性另一半：1 是合法最小值，会真的去查
	wantErrIs(t, "template_id=1 走查库", err, model.ErrTemplateNotFound)
	wantSeq(t, "轨迹", e.st.log, 0,
		"cache.GetTemplate:tc:tpl:1", "transcode_template.FindOne:1")
}

// TestGetTemplate不会读到同id的任务缓存 是键拼装的判别性对照：库里同时存在 task_id=101 与
// template_id=101，且任务缓存已预热。若两个域共用一个前缀，这条用例会命中任务 JSON、
// 解出一个「全零模板」并当成命中（无错误），从而静默返回错数据。
func TestGetTemplate不会读到同id的任务缓存(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedTpl101(t, st)
	now := nowUnix()
	seedTask(t, st, pendingTask(101, 5001, 101, now))
	warmTaskCache(t, st, st.task(101)) // 只预热 tc:task:101

	got, err := getTemplateCall(e, 101)
	wantNoErr(t, "同 id 的模板读", err)
	wantTemplateReplyMatchesRow(t, "模板读结果", got, row)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTemplate:tc:tpl:101", "transcode_template.FindOne:101", "cache.SetTemplate:tc:tpl:101")
	wantCount(t, "模板读不得碰任务缓存", st.log, "cache.GetTask", 0)
	wantCount(t, "模板读不得写任务缓存", st.log, "cache.SetTask", 0)
}

// TestGetTemplate应答不带时间列 是「proto 字段漂移哨兵」：库存有 ctime/mtime，
// rpc.TemplateReply 只有 8 个字段（transcode.proto 的 TemplateReply），
// 所以运营面无法从模板详情判断档位是什么时候建的（对比任务详情 TaskReply 带时间）。
// 若日后给 TemplateReply 加了时间字段，本用例会红，逼调用方口径一起更新。
func TestGetTemplate应答不带时间列(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedTpl101(t, st)
	wantEq(t, "库存", "ctime 是有值的", row.Ctime, int64(1700000000))

	_, err := getTemplateCall(e, 101)
	wantNoErr(t, "GetTemplate", err)

	typ := reflect.TypeOf(rpc.TemplateReply{})
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		if name := typ.Field(i).Name; !strings.HasPrefix(name, "state") && name != "sizeCache" && name != "unknownFields" {
			fields = append(fields, name)
		}
	}
	for _, name := range fields {
		if name == "Ctime" || name == "Mtime" {
			t.Errorf("TemplateReply 新增了时间字段 %s：应答口径与 README 需一起复核", name)
		}
	}
}
