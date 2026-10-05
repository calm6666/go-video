package logic

// ListTemplates 用例：运营后台核对版本历史的读侧契约 —— 分页越界返回空页 + 真实总数，
// 不支持的语言显式拒绝，绝不把「查询失败」写成「没有数据」。

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callListTemplates(t *testing.T, in *rpc.ListTemplatesReq) (*rpc.ListTemplatesReply, error) {
	t.Helper()
	return NewListTemplatesLogic(context.Background(), e.svcCtx).ListTemplates(in)
}

func seedTemplateHistory(e *env, t *testing.T) {
	t.Helper()
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, "草稿标题", "草稿正文",
		2, model.TemplateStateDraft)
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhTW, "繁体標題", "繁體內文 {{order_no}}",
		1, model.TemplateStatePublished)
}

func TestListTemplatesPaginationBoundaries(t *testing.T) {
	e := newEnv(t)
	seedTemplateHistory(e, t)

	// pn/ps 未填 → 第 1 页、每页 20：三行都能拿到。
	m := e.mark()
	all, err := e.callListTemplates(t, &rpc.ListTemplatesReq{})
	wantNoErr(t, "默认分页", err)
	wantEQ(t, "默认分页", "total", all.GetTotal(), int64(3))
	wantEQ(t, "默认分页", "本页条数", len(all.GetTemplates()), 3)
	// 假件按 (template_code 升序, version 降序) 返回，v2 草稿应排在 v1 前。
	wantEQ(t, "默认分页", "首条版本", all.GetTemplates()[0].GetVersion(), int32(2))
	wantOps(t, "默认分页", e.ops(m), []string{listTmplOp("", 0, "", 0, 1, 20)})

	// ps 超过 model 上限 100：logic 必须先夹住，否则 model 直接报 ErrPsTooLarge。
	m2 := e.mark()
	big, err := e.callListTemplates(t, &rpc.ListTemplatesReq{Pn: 1, Ps: 5000})
	wantNoErr(t, "超大 ps 被夹到 100", err)
	wantEQ(t, "超大 ps", "total", big.GetTotal(), int64(3))
	wantOps(t, "超大 ps", e.ops(m2), []string{listTmplOp("", 0, "", 0, 1, 100)})

	// 负数页码/页大小回落成 1/20，而不是报错或返回全表。
	neg, err := e.callListTemplates(t, &rpc.ListTemplatesReq{Pn: -5, Ps: -1})
	wantNoErr(t, "负分页", err)
	wantEQ(t, "负分页", "本页条数", len(neg.GetTemplates()), 3)

	// 越界页：空列表 + 真实总数（不是错误，也不是 total=0）。
	deep, err := e.callListTemplates(t, &rpc.ListTemplatesReq{Pn: 9, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "越界页", "本页条数", len(deep.GetTemplates()), 0)
	wantEQ(t, "越界页", "total 仍是全量", deep.GetTotal(), int64(3))

	// 逐页翻完不得重复或漏项（翻页两行/共三行 → 第 2 页只有一条）。
	seen := map[int64]string{}
	for pn := int32(1); pn <= 2; pn++ {
		page, err := e.callListTemplates(t, &rpc.ListTemplatesReq{Pn: pn, Ps: 2})
		wantNoErr(t, "翻页", err)
		for _, it := range page.GetTemplates() {
			key := it.GetId()
			if key == 0 {
				t.Errorf("第 %d 页模板 id 未投影，无法做跨页去重判定", pn)
			}
			desc := it.GetTemplateCode() + "/" + it.GetLanguage().String() + "/v" + strconv.Itoa(int(it.GetVersion()))
			if prev, dup := seen[key]; dup {
				t.Errorf("第 %d 页与前一页重复：id=%d（%s 与 %s）", pn, key, prev, desc)
			}
			seen[key] = desc
		}
	}
	wantEQ(t, "翻页", "累计条数", len(seen), 3)
}

func TestListTemplatesFiltersArePassedToStore(t *testing.T) {
	e := newEnv(t)
	seedTemplateHistory(e, t)

	// 按语言过滤：只回繁体那一版。
	m := e.mark()
	byLang, err := e.callListTemplates(t, &rpc.ListTemplatesReq{Language: rpc.Language_LANGUAGE_ZH_TW})
	wantNoErr(t, "按语言过滤", err)
	wantEQ(t, "按语言过滤", "条数", len(byLang.GetTemplates()), 1)
	wantEQ(t, "按语言过滤", "lang", byLang.GetTemplates()[0].GetLanguage(), rpc.Language_LANGUAGE_ZH_TW)
	wantOps(t, "按语言过滤", e.ops(m), []string{listTmplOp("", 0, model.LangZhTW, 0, 1, 20)})

	// 按状态 + 通道 + 模板码组合过滤。
	m = e.mark()
	byState, err := e.callListTemplates(t, &rpc.ListTemplatesReq{
		TemplateCode: codeShip, Channel: rpc.Channel_CHANNEL_PUSH,
		State: rpc.TemplateState_TEMPLATE_STATE_DRAFT,
	})
	wantNoErr(t, "按状态过滤", err)
	wantEQ(t, "按状态过滤", "条数", len(byState.GetTemplates()), 1)
	wantEQ(t, "按状态过滤", "版本", byState.GetTemplates()[0].GetVersion(), int32(2))
	wantOps(t, "按状态过滤", e.ops(m), []string{listTmplOp(codeShip, model.ChannelPush, "", 1, 1, 20)})

	// 过滤条件都不匹配：空列表 + total=0，不报错。
	none, err := e.callListTemplates(t, &rpc.ListTemplatesReq{TemplateCode: "no_such_code"})
	wantNoErr(t, "无匹配", err)
	wantEQ(t, "无匹配", "条数", len(none.GetTemplates()), 0)
	wantEQ(t, "无匹配", "total", none.GetTotal(), int64(0))
}

func TestListTemplatesRejectsUnsupportedLanguageEnum(t *testing.T) {
	e := newEnv(t)
	seedTemplateHistory(e, t)

	// 契约里出现模型不支持的语言（枚举漂移）时必须显式拒绝，
	// 不能退化成「不过滤语言」把三套语言的结果混着返回。
	m := e.mark()
	reply, err := e.callListTemplates(t, &rpc.ListTemplatesReq{Language: rpc.Language(9)})
	wantErrIs(t, "未知语言枚举", err, model.ErrInvalidLang)
	if reply != nil {
		t.Errorf("拒绝时不得返回模板列表，实际 %+v", reply)
	}
	wantOps(t, "未知语言枚举", e.ops(m), nil)

	// 未指定语言是「不过滤」，与上面的「非法语言」是两回事。
	all, err := e.callListTemplates(t, &rpc.ListTemplatesReq{Language: rpc.Language_LANGUAGE_UNSPECIFIED})
	wantNoErr(t, "未指定语言", err)
	wantEQ(t, "未指定语言", "条数", len(all.GetTemplates()), 3)
}

func TestListTemplatesStorageErrorAndNilRequest(t *testing.T) {
	e := newEnv(t)
	seedTemplateHistory(e, t)
	dbDown := errors.New("select notification_template: db down")
	e.tmplSpy.Fail("List", dbDown)

	reply, err := e.callListTemplates(t, &rpc.ListTemplatesReq{})
	wantErrIs(t, "读库失败", err, dbDown)
	if reply != nil {
		t.Errorf("读库失败时不得返回空列表冒充「没有数据」，实际 %+v", reply)
	}

	if _, err := e.callListTemplates(t, nil); err == nil {
		t.Error("nil 请求必须报错")
	} else {
		wantErrContains(t, "nil 请求", err, "nil list request")
	}
}
