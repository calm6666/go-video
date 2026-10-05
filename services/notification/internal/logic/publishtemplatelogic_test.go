package logic

// PublishTemplate 用例：草稿→已发布是投递可用性的开关，
// 单已发布不变量与源状态守卫（发布已发布/已下线的版本必须被拒）都在这里钉住。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callPublish(t *testing.T, in *rpc.PublishTemplateReq) (*rpc.PublishTemplateReply, error) {
	t.Helper()
	return NewPublishTemplateLogic(context.Background(), e.svcCtx).PublishTemplate(in)
}

// publishReq 构造一条发布请求。
func publishReq(code string, version int32) *rpc.PublishTemplateReq {
	return &rpc.PublishTemplateReq{
		TemplateCode: code, Channel: rpc.Channel_CHANNEL_PUSH,
		Language: rpc.Language_LANGUAGE_ZH_CN, Version: version, Operator: opAdmin,
	}
}

func TestPublishTemplateDraftTakesPreviousVersionOffline(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody) // id=1
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, "新版标题", "新版正文 {{order_no}}",
		2, model.TemplateStateDraft) // id=2

	m := e.mark()
	reply, err := e.callPublish(t, publishReq(codeShip, 2))
	wantNoErr(t, "发布草稿", err)
	got := reply.GetTemplate()
	wantEQ(t, "发布", "version", got.GetVersion(), int32(2))
	wantEQ(t, "发布", "state", got.GetState(), rpc.TemplateState_TEMPLATE_STATE_PUBLISHED)
	wantEQ(t, "发布", "operator", got.GetOperator(), opAdmin)
	wantEQ(t, "发布", "title", got.GetTitleTpl(), "新版标题")

	v1 := e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 1)
	v2 := e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 2)
	if v1 == nil || v2 == nil {
		t.Fatalf("模板版本缺失：v1=%v v2=%v", v1, v2)
	}
	wantEQ(t, "发布", "旧已发布版本转下线", v1.State, model.TemplateStateOffline)
	wantEQ(t, "发布", "草稿转已发布", v2.State, model.TemplateStatePublished)
	// 发布人留痕写到了行上（运营审计靠这一列）。
	wantEQ(t, "发布", "v2 operator", v2.Operator, opAdmin)
	wantEQ(t, "发布", "已发布版本数（单发布不变量）",
		countTemplatesByState(e.tmpl.Rows(), model.TemplateStatePublished), 1)
	wantOps(t, "发布读库顺序", e.ops(m), []string{
		"tmpl.Find:order_shipped/1/zh-CN/v2",
		"tmpl.PublishDraft:2",
	})
}

func TestPublishTemplateMakesVersionDeliverable(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, "旧标题", "旧正文 {{order_no}}")
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, "新标题", "新正文 {{order_no}}",
		2, model.TemplateStateDraft)

	// 发布前投递只能命中 v1：草稿不可用。
	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "发布前投递", err)
	row := e.onlyDelivery(t)
	wantEQ(t, "发布前投递", "锁定的版本", row.TemplateVersion, int32(1))

	_, err = e.callPublish(t, publishReq(codeShip, 2))
	wantNoErr(t, "发布 v2", err)

	_, err = e.send(t, pushReq("ship-2", recip(midBob, "device-token-b")))
	wantNoErr(t, "发布后投递", err)
	rows := e.deliveriesByMid(t, midBob)
	if len(rows) != 1 {
		t.Fatalf("发布后投递行数 = %d, want 1", len(rows))
	}
	wantEQ(t, "发布后投递", "锁定的版本", rows[0].TemplateVersion, int32(2))
	// 已按旧版本落库的任务不受影响：v1 仍在表里（只是下线），按各自版本渲染。
	wantTrue(t, "发布后投递", "旧版本行仍在",
		e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 1) != nil)
}

func TestPublishTemplateRejectsNonDraftStates(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody) // id=1 published
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, "历史版", "历史正文",
		3, model.TemplateStateOffline) // id=3 offline

	for _, tc := range []struct {
		name    string
		version int32
		state   int32
	}{
		{"重复发布已发布版本", 1, model.TemplateStatePublished},
		{"发布已下线版本", 3, model.TemplateStateOffline},
	} {
		m := e.mark()
		reply, err := e.callPublish(t, publishReq(codeShip, tc.version))
		wantErrIs(t, tc.name, err, model.ErrIllegalStateTransition)
		wantErrContains(t, tc.name, err, "不是草稿")
		if reply != nil {
			t.Errorf("%s：拒绝时不得返回模板，实际 %+v", tc.name, reply)
		}
		// 守卫未命中时绝不触达写库：否则会把已发布版本改回草稿、现网直接投不出去。
		wantOps(t, tc.name, e.ops(m), []string{findTmplOp(tc.version)})
		row := e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, tc.version)
		if row == nil {
			t.Fatalf("%s：被拒之后版本行消失了", tc.name)
		}
		wantEQ(t, tc.name+"：状态不变", "state", row.State, tc.state)
	}
	wantEQ(t, "重复发布", "已发布版本数",
		countTemplatesByState(e.tmpl.Rows(), model.TemplateStatePublished), 1)
}

func TestPublishTemplateNotFoundBeforeWrite(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	m := e.mark()
	reply, err := e.callPublish(t, publishReq(codeShip, 9))
	wantErrIs(t, "版本不存在", err, model.ErrTemplateNotFound)
	wantErrContains(t, "版本不存在", err, "version=9")
	if reply != nil {
		t.Errorf("版本不存在时不得返回模板，实际 %+v", reply)
	}
	wantEQ(t, "版本不存在", "模板行数不变", len(e.tmpl.Rows()), 1)
	wantOps(t, "版本不存在", e.ops(m), []string{"tmpl.Find:order_shipped/1/zh-CN/v9"})
}

func TestPublishTemplateRejectsInvalidRequests(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	cases := []struct {
		name string
		req  *rpc.PublishTemplateReq
		want error
		clue string
	}{
		{"缺操作人", &rpc.PublishTemplateReq{TemplateCode: codeShip, Channel: rpc.Channel_CHANNEL_PUSH,
			Language: rpc.Language_LANGUAGE_ZH_CN, Version: 1}, ErrOperatorRequired, ""},
		{"版本非正数", &rpc.PublishTemplateReq{TemplateCode: codeShip, Channel: rpc.Channel_CHANNEL_PUSH,
			Language: rpc.Language_LANGUAGE_ZH_CN, Operator: opAdmin, Version: 0}, nil, "version must be positive"},
		{"负版本", &rpc.PublishTemplateReq{TemplateCode: codeShip, Channel: rpc.Channel_CHANNEL_PUSH,
			Language: rpc.Language_LANGUAGE_ZH_CN, Operator: opAdmin, Version: -2}, nil, "version must be positive"},
		{"未指定通道", &rpc.PublishTemplateReq{TemplateCode: codeShip, Language: rpc.Language_LANGUAGE_ZH_CN,
			Operator: opAdmin, Version: 1}, model.ErrInvalidChannel, ""},
		{"不支持的语言", &rpc.PublishTemplateReq{TemplateCode: codeShip, Channel: rpc.Channel_CHANNEL_PUSH,
			Language: rpc.Language(9), Operator: opAdmin, Version: 1}, model.ErrInvalidLang, ""},
		{"模板码空白", &rpc.PublishTemplateReq{TemplateCode: " ", Channel: rpc.Channel_CHANNEL_PUSH,
			Language: rpc.Language_LANGUAGE_ZH_CN, Operator: opAdmin, Version: 1}, nil, "template_code is required"},
	}

	for _, tc := range cases {
		m := e.mark()
		got, err := e.callPublish(t, tc.req)
		if got != nil {
			t.Errorf("%s：拒绝时不得返回模板，实际 %+v", tc.name, got)
		}
		switch {
		case err == nil:
			t.Errorf("%s：期望显式错误，实际 err=nil", tc.name)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s：错误 = %v, want errors.Is(..., %v)", tc.name, err, tc.want)
		case tc.clue != "" && !contains(err.Error(), tc.clue):
			t.Errorf("%s：错误 %q 未包含线索 %q", tc.name, err.Error(), tc.clue)
		}
		wantOps(t, tc.name+"：校验先于读库", e.ops(m), nil)
	}
}

func TestPublishTemplateStorageErrorKeepsStatesIntact(t *testing.T) {
	publishDown := errors.New("update notification_template: db down")

	// 发布写库失败：两个版本都必须留在原状态，不能出现「零已发布」或「双已发布」。
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, "新标题", "新正文", 2, model.TemplateStateDraft)
	e.tmplSpy.Fail("PublishDraft", publishDown)

	reply, err := e.callPublish(t, publishReq(codeShip, 2))
	wantErrIs(t, "发布写库失败", err, publishDown)
	if reply != nil {
		t.Errorf("写库失败时不得返回模板，实际 %+v", reply)
	}
	wantEQ(t, "发布写库失败", "v1 状态",
		e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 1).State, model.TemplateStatePublished)
	wantEQ(t, "发布写库失败", "v2 状态",
		e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 2).State, model.TemplateStateDraft)

	// 读库失败同样要上抛，而不是当成「版本不存在」。
	e2 := newEnv(t)
	readDown := errors.New("select notification_template: db down")
	e2.tmplSpy.Fail("Find", readDown)
	_, err = e2.callPublish(t, publishReq(codeShip, 1))
	wantErrIs(t, "发布前读库失败", err, readDown)
}
