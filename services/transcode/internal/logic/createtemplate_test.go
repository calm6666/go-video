package logic

// createtemplatelogic 写侧用例：CreateTemplate 只有一条守卫（name 非空），
// 因此本文件既钉住「合法侧确实落库并回显自增 id」，也钉住「非法档位原样入库」的现状，
// 并单独钉住 `uniq_name` 冲突没有被翻译成域哨兵（README 已知缺口 12）。
//
// 本方法只有一条 INSERT、没有事务，也没有任何缓存写入（repository.go:232-238），
// 所以失败类用例断言的是「库里残留什么」，不涉及回滚。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

func createTemplateCall(e *env, in *rpc.CreateTemplateReq) (*rpc.TemplateReply, error) {
	return NewCreateTemplateLogic(context.Background(), e.svcCtx).CreateTemplate(in)
}

func validCreateReq() *rpc.CreateTemplateReq {
	return &rpc.CreateTemplateReq{
		Name: "h264-1080p", Codec: "h264",
		Width: 1920, Height: 1080, Bitrate: 6000, Fps: 30, SegmentSeconds: 6,
	}
}

// TestCreateTemplate落库并回显自增id 钉住唯一一条依赖调用（INSERT，id 段 901 起）
// 与应答的逐字段投影；同时钉住「创建模板完全不碰缓存」（见缺口 11 的用例）。
func TestCreateTemplate落库并回显自增id(t *testing.T) {
	e := newEnv(t)
	st := e.st

	got, err := createTemplateCall(e, validCreateReq())
	wantNoErr(t, "CreateTemplate", err)
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Insert:901")
	wantEq(t, "CreateTemplate", "库里行数", st.tplRows(), 1)

	row := st.tpl(901)
	if row == nil {
		t.Fatal("库存里没有 901 这行")
	}
	wantTemplateReplyMatchesRow(t, "应答 vs 库存", got, row)
	wantEq(t, "CreateTemplate", "应答 template_id", got.GetTemplateId(), int64(901))
	wantCount(t, "创建不碰缓存", st.log, "cache.", 0)

	// ctime/mtime 由 model 自己取时钟（transcodemodel.go:175,178），两条同一秒。
	assertAround(t, "CreateTemplate", "库存 ctime", row.Ctime, nowUnix(), 2)
	wantEq(t, "CreateTemplate", "ctime == mtime", row.Mtime, row.Ctime)
}

// TestCreateTemplate只校验name非空 钉住守卫的另一侧：codec 空串、宽高/码率/帧率/分片时长
// 的负数与超大值全部原样入库（README 已知缺口 12）。
// 判别性对照是同一条用例末尾的 name="" ⇒ ErrTemplateNameEmpty 且一次依赖都不碰。
func TestCreateTemplate只校验name非空(t *testing.T) {
	cases := []struct {
		label  string
		mutate func(*rpc.CreateTemplateReq)
	}{
		{"codec 空串", func(in *rpc.CreateTemplateReq) { in.Codec = "" }},
		{"codec 是任意字符串", func(in *rpc.CreateTemplateReq) { in.Codec = "不是编码器名" }},
		{"width 负数", func(in *rpc.CreateTemplateReq) { in.Width = -1920 }},
		{"height 负数", func(in *rpc.CreateTemplateReq) { in.Height = -1 }},
		{"bitrate 负数（kbps 不可能为负）", func(in *rpc.CreateTemplateReq) { in.Bitrate = -6000 }},
		{"fps 负数", func(in *rpc.CreateTemplateReq) { in.Fps = -30 }},
		{"segment_seconds 负数", func(in *rpc.CreateTemplateReq) { in.SegmentSeconds = -6 }},
		{"segment_seconds 超大", func(in *rpc.CreateTemplateReq) { in.SegmentSeconds = 2147483647 }},
		{"全零档位", func(in *rpc.CreateTemplateReq) {
			in.Codec = ""
			in.Width, in.Height, in.Bitrate, in.Fps, in.SegmentSeconds = 0, 0, 0, 0, 0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			in := validCreateReq()
			tc.mutate(in)

			got, err := createTemplateCall(e, in)
			wantNoErr(t, "非法档位仍被接受", err)
			wantSeq(t, "轨迹", st.log, 0, "transcode_template.Insert:901")
			row := st.tpl(901)
			if row == nil {
				t.Fatal("库存里没有 901 这行")
			}
			// 原样入库（不是被兜底成 0，也不是被夹成正数）。
			wantEq(t, tc.label, "width", row.Width, in.Width)
			wantEq(t, tc.label, "height", row.Height, in.Height)
			wantEq(t, tc.label, "bitrate", row.Bitrate, in.Bitrate)
			wantEq(t, tc.label, "fps", row.Fps, in.Fps)
			wantEq(t, tc.label, "segment_seconds", row.SegmentSeconds, in.SegmentSeconds)
			wantTemplateReplyMatchesRow(t, "应答 vs 库存", got, row)
		})
	}

	// 判别性另一半：只有空 name 被拒，且拒在触库之前。
	e := newEnv(t)
	in := validCreateReq()
	in.Name = ""
	got, err := createTemplateCall(e, in)
	wantErrIs(t, "空 name", err, model.ErrTemplateNameEmpty)
	if got != nil {
		t.Errorf("空 name 拒绝时仍返回应答 %+v", got)
	}
	wantNoCall(t, "空 name", e.st.log, 0)
	wantEq(t, "空 name", "库里行数", e.st.tplRows(), 0)
}

// TestCreateTemplate的空白与超长name都入库 钉住 name 只有「非空」这一条：
// 全空格算非空，200 字符也放行。迁移里 name 是 VARCHAR(128) 且有 uniq_name
// （000001_create_transcode_tables.sql:33,43），所以超长在真库上是 error 1406 或
// （非 strict 模式下）静默截断 ⇒ 替身证明不了，只登记为 README 已知缺口 12。
func TestCreateTemplate的空白与超长name都入库(t *testing.T) {
	t.Run("全空格 name 被当作合法", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		in := validCreateReq()
		in.Name = "   "
		got, err := createTemplateCall(e, in)
		wantNoErr(t, "空白 name", err)
		wantEq(t, "空白 name", "库存原样存了 3 个空格", st.tpl(901).Name, "   ")
		wantEq(t, "空白 name", "应答原样回显", got.GetName(), "   ")
	})
	t.Run("200 字符 name 在 logic 侧无长度守卫", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		in := validCreateReq()
		in.Name = strings.Repeat("长", 200)
		_, err := createTemplateCall(e, in)
		wantNoErr(t, "超长 name", err)
		wantEq(t, "超长 name", "库存里的 name 长度", int32(len([]rune(st.tpl(901).Name))), int32(200))
	})
}

// TestCreateTemplate重名返回驱动原始错误且不落第二行 钉住 `uniq_name` 冲突没有被翻译成
// 域哨兵（model/transcodemodel.go:180 只做 `fmt.Errorf("transcode_template Insert: %w", err)`）：
// 调用方拿到的是驱动错误文本，既不是 ErrTemplateNameEmpty 也不是任何「已存在」哨兵，
// 因此运营面无法区分「重名」与「库挂了」。判别性：换一个 name 就成功。
func TestCreateTemplate重名返回驱动原始错误且不落第二行(t *testing.T) {
	e := newEnv(t)
	st := e.st

	first, err := createTemplateCall(e, validCreateReq())
	wantNoErr(t, "首次创建", err)
	wantEq(t, "首次创建", "应答 template_id", first.GetTemplateId(), int64(901))

	dup := validCreateReq()
	dup.Bitrate = 9000 // 只有 name 相同才冲突，其余字段刻意不同
	got, err := createTemplateCall(e, dup)
	wantErrContains(t, "重名", err, "Error 1062")
	wantErrContains(t, "重名", err, "uniq_name")
	wantErrContains(t, "重名", err, "transcode_template Insert:")
	if got != nil {
		t.Errorf("重名时仍返回应答 %+v", got)
	}
	for _, sentinel := range []error{model.ErrTemplateNameEmpty, model.ErrTemplateNotFound, model.ErrInvalidState} {
		if errors.Is(err, sentinel) {
			t.Errorf("重名错误被翻译成了域哨兵 %v，与本用例钉住的现状不符", sentinel)
		}
	}
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Insert:901", "transcode_template.Insert:902")
	wantEq(t, "重名", "库里仍只有 1 行", st.tplRows(), 1)
	wantEq(t, "重名", "首行 bitrate 未被改写成 9000", st.tpl(901).Bitrate, int32(6000))

	// 换 name 即成功；InnoDB 的号在冲突那次已消耗，所以这里是 903 而不是 902。
	other := validCreateReq()
	other.Name = "h264-720p"
	third, err := createTemplateCall(e, other)
	wantNoErr(t, "换个 name 再建", err)
	wantEq(t, "换个 name 再建", "template_id 跳号", third.GetTemplateId(), int64(903))
	wantSeq(t, "轨迹", st.log, 0,
		"transcode_template.Insert:901", "transcode_template.Insert:902", "transcode_template.Insert:903")
	wantEq(t, "重名后", "库里 2 行", st.tplRows(), 2)
	wantEq(t, "重名后", "首行 bitrate 仍是 6000", st.tpl(901).Bitrate, int32(6000))
}

// TestCreateTemplate的INSERT失败原样上抛且不落行 钉住错误包装与「失败不消耗自增号」：
// 连接级失败发生在语句之前，下一次成功插入仍然是 901。
func TestCreateTemplate的INSERT失败原样上抛且不落行(t *testing.T) {
	e := newEnv(t)
	st := e.st
	drvErr := errors.New("error 1406: Data too long for column 'name'")
	st.s.fail("transcode_template.Insert", drvErr)

	got, err := createTemplateCall(e, validCreateReq())
	wantErrContains(t, "Insert 失败", err, "transcode_template Insert:")
	wantErrIs(t, "Insert 失败包装的原始错误", err, drvErr)
	if got != nil {
		t.Errorf("失败时仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Insert:901")
	wantEq(t, "Insert 失败", "库里行数", st.tplRows(), 0)
	wantCount(t, "Insert 失败", st.log, "cache.", 0)

	st.s.fail("transcode_template.Insert", nil) // 修好依赖后重试
	id, err := createTemplateCall(e, validCreateReq())
	wantNoErr(t, "重试", err)
	wantEq(t, "重试", "自增号没被失败的语句消耗", id.GetTemplateId(), int64(901))
}

// TestCreateTemplate后详情读仍需回源一次 钉住 repository.go:4 的类注释与实现的漂移：
// 注释说「模板详情长 TTL（600s），创建时刷新」，但 CreateTemplate 一条缓存调用都不发
// （repository.go:232-238），Cache.DelTemplate（repository.go:94-97）在全仓零调用方、
// 也没进 Cacher（repository.go:111-118）。后果：模板缓存既没有写入侧的预热、
// 也没有任何失效入口，只能靠 TTL 自然过期（README 已知缺口 11）。
func TestCreateTemplate后详情读仍需回源一次(t *testing.T) {
	e := newEnv(t)
	st := e.st

	_, err := createTemplateCall(e, validCreateReq())
	wantNoErr(t, "CreateTemplate", err)
	before := st.log.snapshot()

	got, err := getTemplateCall(e, 901)
	wantNoErr(t, "创建后立刻读", err)
	wantTemplateReplyMatchesRow(t, "创建后读到的", got, st.tpl(901))
	wantSeq(t, "创建后的详情读轨迹", st.log, before,
		"cache.GetTemplate:tc:tpl:901", "transcode_template.FindOne:901", "cache.SetTemplate:tc:tpl:901")
	wantCount(t, "创建阶段", st.log, "cache.SetTemplate", 1) // 只有回源那一次，创建本身没写缓存
}

// TestCreateTemplate应答是投影而非库存指针 钉住「改应答不会改库」：logic 返回的
// TemplateReply 是新构造的结构体，且 model 行也是值拷贝。
func TestCreateTemplate应答是投影而非库存指针(t *testing.T) {
	e := newEnv(t)
	st := e.st

	got, err := createTemplateCall(e, validCreateReq())
	wantNoErr(t, "CreateTemplate", err)
	got.Name = "被调用方改写了"
	got.Bitrate = 1

	row := st.tpl(901)
	wantEq(t, "投影性", "库存 name 不受应答改写影响", row.Name, "h264-1080p")
	wantEq(t, "投影性", "库存 bitrate 不受应答改写影响", row.Bitrate, int32(6000))

	again, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 10})
	wantNoErr(t, "列表复核", err)
	wantEq(t, "投影性", "列表里的 bitrate", again.GetTemplates()[0].GetBitrate(), int32(6000))
	wantEq(t, "投影性", "列表里的 name", again.GetTemplates()[0].GetName(), "h264-1080p")
}
