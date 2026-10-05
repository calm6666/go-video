package logic

// listtemplates_test.go 覆盖 ListTemplates：两条 SQL 的取舍（COUNT 为 0 不再发 SELECT）、
// pn/ps 兜底的实际口径、排序方向与分页切片，以及「列表完全不碰缓存」这一与详情读不一致的来源。
//
// 与任务列表的三点差异都在用例里钉住：
//   - COUNT 的轨迹键**没有任何条件位**（`transcode_template.Count`），因为这条 SQL 无 WHERE，
//     proto 也没有 name/codec 过滤字段（rpc/transcode.proto 的 ListTemplatesReq 只有 pn/ps）；
//   - `ORDER BY template_id ASC`（transcodemodel.go:224），与任务列表的 DESC 方向相反；
//   - 全程零缓存调用 ⇒ 新模板/人工改库立刻反映在列表里，而详情读可能还是 600s 前的旧值。

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

func listTemplatesCall(e *env, in *rpc.ListTemplatesReq) (*rpc.TemplatesReply, error) {
	return NewListTemplatesLogic(context.Background(), e.svcCtx).ListTemplates(in)
}

// seedThreeTemplates 布 3 条模板：101 h264-1080p / 102 hevc-1080p / 103 aac-audio。
func seedThreeTemplates(t *testing.T, st *store) {
	t.Helper()
	seedTemplate(t, st, h264_1080p(101, "h264-1080p"))

	hevc := h264_1080p(102, "hevc-1080p")
	hevc.Codec = "hevc"
	hevc.Bitrate = 4000
	seedTemplate(t, st, hevc)

	aac := h264_1080p(103, "aac-96k")
	aac.Codec = "aac"
	aac.Width, aac.Height, aac.Fps = 0, 0, 0
	aac.Bitrate, aac.SegmentSeconds = 96, 0
	seedTemplate(t, st, aac)
}

func tplIDs(reply *rpc.TemplatesReply) []int64 {
	var ids []int64
	for _, tpl := range reply.GetTemplates() {
		ids = append(ids, tpl.GetTemplateId())
	}
	return ids
}

// TestListTemplates按id正序返回且完全不碰缓存 钉住 `ORDER BY template_id ASC`
// （与 TestListTasks按asset过滤且倒序返回 的 DESC 互为对照，两条方向写反都会红）
// 以及「列表 = 裸 SQL、零缓存」。
func TestListTemplates按id正序返回且完全不碰缓存(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTemplates(t, st)

	got, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 10})
	wantNoErr(t, "ListTemplates", err)
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Count", "transcode_template.Select:1/10/0")
	wantEq(t, "模板列表", "total", got.GetTotal(), int32(3))
	if !slices.Equal(tplIDs(got), []int64{101, 102, 103}) {
		t.Errorf("模板列表 = %v, want [101 102 103]（template_id 正序）", tplIDs(got))
	}
	wantCount(t, "列表无缓存", st.log, "cache.", 0)

	// 逐字段投影：应答与库存一致，且第 3 条的自适应 0 值不被兜底成别的数。
	wantTemplateReplyMatchesRow(t, "第 1 条", got.GetTemplates()[0], st.tpl(101))
	wantTemplateReplyMatchesRow(t, "第 3 条（codec=aac、宽高/帧率为 0）", got.GetTemplates()[2], st.tpl(103))
}

// TestListTemplates的COUNT无条件位 钉住模板列表「什么都过滤不了」这条契约缺口
// （README 已知缺口 12 的读侧那一半）：轨迹里的 Count 键没有 `<条件>` 段，
// 与任务列表的 `transcode_task.Count:<asset>/<state>` 形成结构差异。
func TestListTemplates的COUNT无条件位(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTemplates(t, st)
	seedTask(t, st, pendingTask(201, 5001, 101, nowUnix())) // 另一张表有数据也不影响模板 Count

	got, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 10})
	wantNoErr(t, "ListTemplates", err)
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Count", "transcode_template.Select:1/10/0")
	wantEq(t, "模板列表", "total（含全部 3 条，与任务数无关）", got.GetTotal(), int32(3))
}

// TestListTemplates的ps上限两侧 钉住 logic 的 `ps < 0 || ps > 50`（listtemplateslogic.go:29）：
// 50 放行、51 与负数拒绝，且拒绝发生在任何依赖之前。
// 与任务侧同一个现状：model 里 `ps > 50 → 20` 的兜底因此永远走不到（README 已知缺口 6）。
func TestListTemplates的ps上限两侧(t *testing.T) {
	t.Run("ps=50 放行", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedThreeTemplates(t, st)
		got, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 50})
		wantNoErr(t, "ps=50", err)
		wantSeq(t, "轨迹", st.log, 0, "transcode_template.Count", "transcode_template.Select:1/50/0")
		wantEq(t, "ps=50", "total", got.GetTotal(), int32(3))
	})
	for _, ps := range []int32{51, 1000, -1, -20} {
		t.Run("ps 越界被拒", func(t *testing.T) {
			e := newEnv(t)
			seedThreeTemplates(t, e.st)
			_, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: ps})
			wantErrIs(t, "ps 越界", err, model.ErrPsTooLarge)
			wantNoCall(t, "ps 越界", e.st.log, 0)
		})
	}
}

// TestListTemplates的pn与ps兜底发生在model 钉住 0 是「合法值」并被兜底成第 1 页 / 每页 20 条：
// 漏传分页参数会拿到**更大**的默认页而不是空页或报错（README 已知缺口 6 的另一半）。
func TestListTemplates的pn与ps兜底发生在model(t *testing.T) {
	for _, tc := range []struct{ pn, ps int32 }{{0, 0}, {-5, 0}, {1, 0}} {
		e := newEnv(t)
		st := e.st
		seedThreeTemplates(t, st)
		got, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: tc.pn, Ps: tc.ps})
		wantNoErr(t, "pn/ps 兜底", err)
		wantSeq(t, "轨迹", st.log, 0, "transcode_template.Count", "transcode_template.Select:1/20/0")
		wantEq(t, "pn/ps 兜底", "total", got.GetTotal(), int32(3))
		wantEq(t, "pn/ps 兜底", "返回行数", int32(len(got.GetTemplates())), int32(3))
	}
}

// TestListTemplates第二页取到的是正序后的后半段 钉住 pn/ps 真的参与 offset
// （ps=2 两页并集等于全集且不相交）。
func TestListTemplates第二页取到的是正序后的后半段(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTemplates(t, st)

	first, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 2})
	wantNoErr(t, "第一页", err)
	second, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 2, Ps: 2})
	wantNoErr(t, "第二页", err)

	wantSeq(t, "轨迹", st.log, 0,
		"transcode_template.Count", "transcode_template.Select:1/2/0",
		"transcode_template.Count", "transcode_template.Select:2/2/2")
	if !slices.Equal(tplIDs(first), []int64{101, 102}) {
		t.Errorf("第一页 = %v, want [101 102]", tplIDs(first))
	}
	if !slices.Equal(tplIDs(second), []int64{103}) {
		t.Errorf("第二页 = %v, want [103]", tplIDs(second))
	}
	wantEq(t, "翻页", "第二页的 total 仍是全量", second.GetTotal(), int32(3))
}

// TestListTemplates越界页返回空列表但total照报 钉住「total>0 而 templates 为空」的姿态
// （与任务列表同一现状，offset 已翻过结尾时不给任何提示）。
func TestListTemplates越界页返回空列表但total照报(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTemplates(t, st)

	got, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 9, Ps: 10})
	wantNoErr(t, "越界页", err)
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Count", "transcode_template.Select:9/10/80")
	wantEq(t, "越界页", "total", got.GetTotal(), int32(3))
	wantEq(t, "越界页", "返回行数", int32(len(got.GetTemplates())), int32(0))
}

// TestListTemplates空表只发一条COUNT且应答是空数组不是null 钉住 model 的 `total == 0 → return`
// 短路（transcodemodel.go:218-220）与 logic 的 `make(..., 0, len(templates))`：
// 前者保证不发第二条 SQL，后者保证 JSON 序列化出来是 `[]` 而不是 `null`。
func TestListTemplates空表只发一条COUNT且应答是空数组不是null(t *testing.T) {
	e := newEnv(t)
	st := e.st

	got, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 10})
	wantNoErr(t, "空表", err)
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Count")
	wantEq(t, "空表", "total", got.GetTotal(), int32(0))
	if got.GetTemplates() == nil {
		t.Error("templates 是 nil（应答会序列化成 null），期望非 nil 空切片")
	}
}

// TestListTemplates的COUNT失败上抛 钉住错误包装与「COUNT 失败时绝不发 SELECT」的顺序结论。
func TestListTemplates的COUNT失败上抛(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTemplates(t, st)
	st.s.fail("transcode_template.Count", errors.New("error 1053: server shutdown"))

	_, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 10})
	wantErrContains(t, "COUNT 失败", err, "transcode_template List count:")
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Count")
	wantCount(t, "COUNT 失败", st.log, "transcode_template.Select", 0)
}

// TestListTemplates的SELECT失败时total被丢弃 钉住 model 的 `return nil, 0, err`
// （transcodemodel.go:229）：已拿到的 total 不随错误返回，调用方只能整体按失败处理。
func TestListTemplates的SELECT失败时total被丢弃(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTemplates(t, st)
	st.s.fail("transcode_template.Select", errors.New("error 1146: table doesn't exist"))

	got, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 10})
	wantErrContains(t, "SELECT 失败", err, "transcode_template List:")
	if got != nil {
		t.Errorf("SELECT 失败仍返回应答 total=%d", got.GetTotal())
	}
	wantSeq(t, "轨迹", st.log, 0, "transcode_template.Count", "transcode_template.Select:1/10/0")
}

// TestListTemplates与GetTemplate对同一模板给出不同码率 是本轮模板域最重要的一条对照，
// 也是 README 已知缺口 11 的证据：详情走 600s 缓存、列表走裸 SQL，
// 于是人工改库（或日后接入模板变更）之后，同一 template_id 在两个读接口上给出两个值，
// 而本服务没有任何 RPC 能清掉那个键（Cacher 里没有 DelTemplate）。
func TestListTemplates与GetTemplate对同一模板给出不同码率(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedTpl101(t, st)
	warmTemplateCache(t, st, row)                                            // 缓存里是 6000kbps 的旧快照
	st.tpls.poke(101, func(r *model.TranscodeTemplate) { r.Bitrate = 8000 }) // 库里已是 8000kbps

	list, err := listTemplatesCall(e, &rpc.ListTemplatesReq{Pn: 1, Ps: 10})
	wantNoErr(t, "列表读", err)
	wantEq(t, "列表读（无缓存）", "bitrate", list.GetTemplates()[0].GetBitrate(), int32(8000))

	detail, err := getTemplateCall(e, 101)
	wantNoErr(t, "详情读", err)
	wantEq(t, "详情读（命中旧缓存）", "bitrate", detail.GetBitrate(), int32(6000))

	wantSeq(t, "轨迹", st.log, 0,
		"transcode_template.Count", "transcode_template.Select:1/10/0",
		"cache.GetTemplate:tc:tpl:101")
	if _, ok := st.cache.raw(keyTemplate(101)); !ok {
		t.Error("期望脏键仍在（本服务没有模板缓存失效路径）")
	}
}
