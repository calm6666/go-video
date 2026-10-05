package logic

// UpsertTemplate 用例：入库前执行与投递时同一套模板校验（policy.CheckTemplate），
// 版本推进/草稿覆盖/唯一键冲突都必须可判定，校验不通过时一行都不落。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callUpsert(t *testing.T, in *rpc.UpsertTemplateReq) (*rpc.UpsertTemplateReply, error) {
	t.Helper()
	return NewUpsertTemplateLogic(context.Background(), e.svcCtx).UpsertTemplate(in)
}

// upsertReq 构造一条合法草稿请求。
func upsertReq(code, title, body string, publish bool) *rpc.UpsertTemplateReq {
	return &rpc.UpsertTemplateReq{
		TemplateCode: code, Channel: rpc.Channel_CHANNEL_PUSH,
		Language: rpc.Language_LANGUAGE_ZH_CN, TitleTpl: title, BodyTpl: body,
		Operator: opAdmin, Publish: publish,
	}
}

func TestUpsertTemplateCreatesFirstDraftVersion(t *testing.T) {
	e := newEnv(t)

	m := e.mark()
	reply, err := e.callUpsert(t, upsertReq(codeShip, tplTitle, tplBody, false))
	wantNoErr(t, "新建草稿", err)

	got := reply.GetTemplate()
	wantEQ(t, "新建草稿", "version", got.GetVersion(), int32(1))
	wantEQ(t, "新建草稿", "state", got.GetState(), rpc.TemplateState_TEMPLATE_STATE_DRAFT)
	wantEQ(t, "新建草稿", "template_code", got.GetTemplateCode(), codeShip)
	wantEQ(t, "新建草稿", "language", got.GetLanguage(), rpc.Language_LANGUAGE_ZH_CN)
	wantEQ(t, "新建草稿", "operator", got.GetOperator(), opAdmin)
	// 主键由假件像真实自增列一样分配并回写：这里断言「不是手写的 0」。
	wantTrue(t, "新建草稿", "id 已分配", got.GetId() > 0)

	row := e.onlyTemplate(t)
	wantEQ(t, "新建草稿", "落库 id 与响应一致", row.Id, got.GetId())
	wantEQ(t, "新建草稿", "响应 version 与入库一致", row.Version, got.GetVersion())
	wantEQ(t, "新建草稿", "响应 state 与入库一致", row.State, int32(got.GetState()))
	wantTrue(t, "新建草稿", "ctime/mtime 由数据访问层填充", row.Ctime > 0 && row.Mtime > 0)
	wantOps(t, "新建草稿读库顺序", e.ops(m), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st1",
		"tmpl.MaxVersion:order_shipped/1/zh-CN",
		"tmpl.Insert:order_shipped/1/zh-CN/v1",
	})
}

func TestUpsertTemplateOverwritesDraftInsteadOfNewVersion(t *testing.T) {
	e := newEnv(t)
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, "旧标题", "旧正文 {{order_no}}",
		1, model.TemplateStateDraft)

	m := e.mark()
	reply, err := e.callUpsert(t, upsertReq(codeShip, "新标题", "新正文 {{order_no}}", false))
	wantNoErr(t, "覆盖草稿", err)
	// 已有草稿时覆盖草稿内容，版本不漂移（否则运营会看到两个「最新草稿」）。
	wantEQ(t, "覆盖草稿", "version", reply.GetTemplate().GetVersion(), int32(1))
	wantEQ(t, "覆盖草稿", "title", reply.GetTemplate().GetTitleTpl(), "新标题")
	rows := e.tmpl.Rows()
	wantEQ(t, "覆盖草稿", "模板行数", len(rows), 1)
	wantEQ(t, "覆盖草稿", "落库标题", rows[0].TitleTpl, "新标题")
	wantEQ(t, "覆盖草稿", "落库正文", rows[0].BodyTpl, "新正文 {{order_no}}")
	wantOps(t, "覆盖草稿只 UPDATE 不 INSERT", e.ops(m), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st1",
		"tmpl.UpdateDraft:1",
	})
}

func TestUpsertTemplateBumpsVersionAfterPublished(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	_, err := e.callUpsert(t, upsertReq(codeShip, "改版标题", "改版正文 {{order_no}}", false))
	wantNoErr(t, "已发布后改稿", err)

	v1 := e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 1)
	v2 := e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 2)
	if v1 == nil || v2 == nil {
		t.Fatalf("模板版本缺失：v1=%v v2=%v", v1, v2)
	}
	// 已发布版本只读留档，不能被草稿覆盖；新版本先草稿、待发布，现网内容不受影响。
	wantEQ(t, "已发布后改稿", "v1 state", v1.State, model.TemplateStatePublished)
	wantEQ(t, "已发布后改稿", "v1 body", v1.BodyTpl, tplBody)
	wantEQ(t, "已发布后改稿", "v2 state", v2.State, model.TemplateStateDraft)
	wantEQ(t, "已发布后改稿", "v2 title", v2.TitleTpl, "改版标题")
}

func TestUpsertTemplatePublishTrueKeepsSinglePublishedVersion(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	reply, err := e.callUpsert(t, upsertReq(codeShip, "直发标题", "直发正文 {{order_no}}", true))
	wantNoErr(t, "直接发布", err)
	wantEQ(t, "直接发布", "version", reply.GetTemplate().GetVersion(), int32(2))
	// 响应里的 state 必须与入库一致（发布走 PublishDraft 之后再回读，不能凭请求猜测）。
	wantEQ(t, "直接发布", "state", reply.GetTemplate().GetState(), rpc.TemplateState_TEMPLATE_STATE_PUBLISHED)

	v1 := e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 1)
	v2 := e.templateAt(t, codeShip, model.ChannelPush, model.LangZhCN, 2)
	if v1 == nil || v2 == nil {
		t.Fatalf("模板版本缺失：v1=%v v2=%v", v1, v2)
	}
	wantEQ(t, "直接发布", "旧版本转下线", v1.State, model.TemplateStateOffline)
	wantEQ(t, "直接发布", "新版本已发布", v2.State, model.TemplateStatePublished)
	// 单已发布不变量：同 (code, channel, lang) 至多一个 published。
	wantEQ(t, "直接发布", "已发布版本数", countTemplatesByState(e.tmpl.Rows(), model.TemplateStatePublished), 1)
}

func TestUpsertTemplateValidatesLikeDispatchDoes(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("订", 1400) // 超过 policy.maxBodyLen=4000 字节（每汉字 3 字节）

	cases := []struct {
		name string
		req  *rpc.UpsertTemplateReq
		want error
		clue string
	}{
		{"正文为空", upsertReq(codeShip, tplTitle, "   ", false), policy.ErrInvalidTemplate, "body template is empty"},
		{"畸形占位符", upsertReq(codeShip, tplTitle, "订单 {{order_no 已发货", false), policy.ErrInvalidTemplate, "malformed or nested placeholder"},
		{"嵌套占位符", upsertReq(codeShip, tplTitle, "订单 {{a{{b}}}}", false), policy.ErrInvalidTemplate, "malformed or nested placeholder"},
		{"标题换行", upsertReq(codeShip, "第一行\n第二行", "正文", false), policy.ErrInvalidTemplate, "title must be single line"},
		{"回车控制符", upsertReq(codeShip, tplTitle, "正文\r非法", false), policy.ErrInvalidTemplate, "carriage return"},
		{"正文超长", upsertReq(codeShip, tplTitle, long, false), policy.ErrInvalidTemplate, "body<=4000"},
		{"未知语言", &rpc.UpsertTemplateReq{TemplateCode: codeShip, Channel: rpc.Channel_CHANNEL_PUSH,
			TitleTpl: tplTitle, BodyTpl: "正文", Operator: opAdmin}, model.ErrInvalidLang, ""},
		{"未知通道", &rpc.UpsertTemplateReq{TemplateCode: codeShip, Language: rpc.Language_LANGUAGE_ZH_CN,
			TitleTpl: tplTitle, BodyTpl: "正文", Operator: opAdmin}, model.ErrInvalidChannel, ""},
		{"缺操作人", &rpc.UpsertTemplateReq{TemplateCode: codeShip, Channel: rpc.Channel_CHANNEL_PUSH,
			Language: rpc.Language_LANGUAGE_ZH_CN, TitleTpl: tplTitle, BodyTpl: "正文"}, ErrOperatorRequired, ""},
		{"模板码空白", upsertReq("  ", tplTitle, "正文", false), nil, "template_code is required"},
	}

	for _, tc := range cases {
		m := e.mark()
		reply, err := e.callUpsert(t, tc.req)
		if reply != nil {
			t.Errorf("%s：拒绝时不得返回模板，实际 %+v", tc.name, reply)
		}
		switch {
		case err == nil:
			t.Errorf("%s：期望显式错误，实际 err=nil", tc.name)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s：错误 = %v, want errors.Is(..., %v)", tc.name, err, tc.want)
		case tc.clue != "" && !contains(err.Error(), tc.clue):
			t.Errorf("%s：错误 %q 未包含线索 %q", tc.name, err.Error(), tc.clue)
		}
		// 校验在入库之前：一条查询/写入都不该发生，模板表保持为空。
		wantOps(t, tc.name+"：校验先于写库", e.ops(m), nil)
		if n := len(e.tmpl.Rows()); n != 0 {
			t.Fatalf("%s：拒绝后模板行数 = %d, want 0", tc.name, n)
		}
	}
}

func TestUpsertTemplateRejectsEmptyTemplateCode(t *testing.T) {
	e := newEnv(t)
	_, err := e.callUpsert(t, upsertReq("", tplTitle, "正文", false))
	wantErrContains(t, "模板码为空", err, "template_code is required")

	// 模板码两侧空白会被裁掉后落库（否则同一名实为两个模板）。
	_, err = e.callUpsert(t, upsertReq("  "+codeShip+"  ", tplTitle, tplBody, false))
	wantNoErr(t, "裁剪模板码", err)
	row := e.onlyTemplate(t)
	wantEQ(t, "裁剪模板码", "template_code", row.TemplateCode, codeShip)
}

func TestUpsertTemplateVersionConflictPropagates(t *testing.T) {
	e := newEnv(t)
	// 唯一键 (code, channel, lang, version) 冲突只会出现在并发写入：这里注入到 Insert，
	// 断言 logic/repository 不把它吞成「保存成功」——否则会丢掉运营刚提交的文案。
	conflict := errors.New("insert notification_template: duplicate version")
	e.tmplSpy.Fail("Insert", conflict)

	reply, err := e.callUpsert(t, upsertReq(codeShip, tplTitle, tplBody, false))
	wantErrIs(t, "版本冲突", err, conflict)
	if reply != nil {
		t.Errorf("版本冲突时不得返回模板，实际 %+v", reply)
	}
	wantEQ(t, "版本冲突", "模板行数", len(e.tmpl.Rows()), 0)
}

func TestUpsertTemplateStorageErrorsPropagate(t *testing.T) {
	readDown := errors.New("select notification_template: db down")

	e := newEnv(t)
	e.tmplSpy.Fail("MaxVersion", readDown)
	_, err := e.callUpsert(t, upsertReq(codeShip, tplTitle, tplBody, false))
	wantErrIs(t, "查最大版本失败", err, readDown)
	wantEQ(t, "查最大版本失败", "模板行数", len(e.tmpl.Rows()), 0)

	e2 := newEnv(t)
	e2.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, "旧", "旧正文", 1, model.TemplateStateDraft)
	writeDown := errors.New("update notification_template: db down")
	e2.tmplSpy.Fail("UpdateDraftContent", writeDown)
	_, err = e2.callUpsert(t, upsertReq(codeShip, "新", "新正文", false))
	wantErrIs(t, "覆盖草稿失败", err, writeDown)
	// 写失败后草稿内容必须仍是旧值（假件只在 UPDATE 成功时改行）。
	wantEQ(t, "覆盖草稿失败", "草稿未被改坏", e2.onlyTemplate(t).TitleTpl, "旧")
}

// countTemplatesByState 统计某状态的模板版本数。
func countTemplatesByState(rows []*model.NotificationTemplate, state int32) int {
	n := 0
	for _, r := range rows {
		if r.State == state {
			n++
		}
	}
	return n
}
