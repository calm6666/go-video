package logic

// RenderTemplate 用例：预览接口不落库、口径与投递时渲染一致、缺变量给清单而不是补空串。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callRender(t *testing.T, in *rpc.RenderTemplateReq) (*rpc.RenderTemplateReply, error) {
	t.Helper()
	return NewRenderTemplateLogic(context.Background(), e.svcCtx).RenderTemplate(in)
}

// renderReq 构造一条合法预览请求（各用例只改自己要验的字段）。
func renderReq(code string, version int32, params map[string]string) *rpc.RenderTemplateReq {
	return &rpc.RenderTemplateReq{
		Channel: rpc.Channel_CHANNEL_PUSH, TemplateCode: code, Version: version,
		Language: rpc.Language_LANGUAGE_ZH_CN, TemplateParams: params, Operator: opAdmin,
	}
}

func TestRenderTemplatePreviewPublishedVersion(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	m := e.mark()
	got, err := e.callRender(t, renderReq(codeShip, 0, map[string]string{"order_no": "SO-77"}))
	wantNoErr(t, "预览已发布版本", err)
	wantEQ(t, "预览", "title", got.GetTitle(), tplTitle)
	wantEQ(t, "预览", "body", got.GetBody(), "订单 SO-77 已发货")
	wantEQ(t, "预览", "version", got.GetVersion(), int32(1))
	wantEQ(t, "预览", "language", got.GetLanguage(), rpc.Language_LANGUAGE_ZH_CN)
	// 隐私不变量：渲染结果里不得残留未替换的占位符（否则用户看到 {{order_no}}）。
	wantNotContains(t, "预览", got.GetTitle()+got.GetBody(), "{{")
	// 预览是只读的：不落投递任务、也不动模板表。
	wantEQ(t, "预览", "投递行数", e.deliveryCount(), 0)
	wantEQ(t, "预览", "模板行数", len(e.tmpl.Rows()), 1)
	wantOps(t, "预览只查已发布版本", e.ops(m), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
	})
}

func TestRenderTemplatePreviewDraftByVersion(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, "新标题", "新订单 {{order_no}}",
		2, model.TemplateStateDraft)

	got, err := e.callRender(t, renderReq(codeShip, 2, map[string]string{"order_no": "SO-1"}))
	wantNoErr(t, "预览草稿版本", err)
	// version>0 走精确版本查询，允许预览未发布内容（运营核对用）。
	wantEQ(t, "预览草稿", "title", got.GetTitle(), "新标题")
	wantEQ(t, "预览草稿", "body", got.GetBody(), "新订单 SO-1")
	wantEQ(t, "预览草稿", "version", got.GetVersion(), int32(2))
	// 不指定版本时必须命中已发布的 v1，而不是把草稿当现网内容。
	got2, err := e.callRender(t, renderReq(codeShip, 0, map[string]string{"order_no": "SO-1"}))
	wantNoErr(t, "预览回落已发布", err)
	wantEQ(t, "预览回落已发布", "version", got2.GetVersion(), int32(1))

	// 不存在的版本显式报错。
	_, err = e.callRender(t, renderReq(codeShip, 9, map[string]string{"order_no": "SO-1"}))
	wantErrIs(t, "预览不存在的版本", err, model.ErrTemplateNotFound)
	wantTrue(t, "预览不存在的版本", "错误里带 version=9", strings.Contains(err.Error(), "version=9"))
}

func TestRenderTemplateMissingVarGivesList(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle,
		"订单 {{order_no}} 由 {{courier}} 配送")

	_, err := e.callRender(t, renderReq(codeShip, 0, map[string]string{"order_no": "SO-1"}))
	wantErrIs(t, "缺变量", err, policy.ErrRenderMissingVar)
	// 缺失清单是运营判断“模板与调用方参数是否匹配”的唯一线索，必须点名缺哪个。
	wantErrContains(t, "缺变量", err, "courier")
	wantTrue(t, "缺变量", "错误里不得把另一个变量也报成缺失",
		!strings.Contains(err.Error(), "order_no"))
	// 契约缺口（README 已知缺口）：reply.missing_vars 在错误路径上无法回传，
	// 因此这里只断言错误文本承载清单。
}

func TestRenderTemplateRejectsInvalidRequests(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	cases := []struct {
		name string
		req  *rpc.RenderTemplateReq
		want error
		clue string
	}{
		{"未指定通道", &rpc.RenderTemplateReq{Operator: opAdmin, TemplateCode: codeShip,
			Language: rpc.Language_LANGUAGE_ZH_CN}, model.ErrInvalidChannel, ""},
		{"模板码为空", renderReq("   ", 0, nil), nil, "template_code is required"},
		{"未指定语言", &rpc.RenderTemplateReq{Channel: rpc.Channel_CHANNEL_PUSH, Operator: opAdmin,
			TemplateCode: codeShip}, ErrTemplateLanguageRequired, ""},
		{"缺操作人", &rpc.RenderTemplateReq{Channel: rpc.Channel_CHANNEL_PUSH, TemplateCode: codeShip,
			Language: rpc.Language_LANGUAGE_ZH_CN}, ErrOperatorRequired, ""},
		{"禁用参数名 phone", renderReq(codeShip, 0, map[string]string{"phone": "13800001111"}),
			policy.ErrSensitiveParam, ""},
		{"禁用参数名 channel", renderReq(codeShip, 0, map[string]string{"channel": "sms"}),
			policy.ErrSensitiveParam, ""},
		{"参数含控制符", renderReq(codeShip, 0, map[string]string{"order_no": "{{admin_note}}"}),
			policy.ErrRenderUnknownVar, ""},
	}

	for _, tc := range cases {
		m := e.mark()
		got, err := e.callRender(t, tc.req)
		if got != nil {
			t.Errorf("%s：拒绝时不得返回渲染结果，实际 %+v", tc.name, got)
		}
		switch {
		case err == nil:
			t.Errorf("%s：期望显式错误，实际 err=nil", tc.name)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s：错误 = %v, want errors.Is(..., %v)", tc.name, err, tc.want)
		case tc.clue != "" && !contains(err.Error(), tc.clue):
			t.Errorf("%s：错误 %q 未包含线索 %q", tc.name, err.Error(), tc.clue)
		}
		// 上表只允许“纯入参校验”的用例（通道/模板码/语言/操作人/参数），
		// 它们必须发生在读库之前：非法请求不产生任何查询。
		// 「模板不存在」不在表里 —— 判定它本身就要求读库，见下面的
		// TestRenderTemplateNotFoundReadsFirst。
		wantOps(t, tc.name+"：入参校验先于读库", e.ops(m), nil)
	}
}

// TestRenderTemplateNotFoundReadsFirst 模板不存在属于“读库后才有的结论”，
// 不能塞进上面的“零查询校验”表里（那是两件事）。这里按实现断真实序列：
// rendertemplatelogic.go:67 在 version=0 时经 Repository.FindPublished
// 逐个语言查 state=published，全部落空才由 logic 自己包成 ErrTemplateNotFound
// （rendertemplatelogic.go:73-76），并且不得回落到草稿。
func TestRenderTemplateNotFoundReadsFirst(t *testing.T) {
	e := newEnv(t)
	// 表里只有一个别的模板码，用来证明“不存在”不是因为假件把任意模板都返回了。
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	m := e.mark()
	got, err := e.callRender(t, renderReq("no_such", 0, map[string]string{"order_no": "SO-1"}))
	wantErrIs(t, "模板不存在", err, model.ErrTemplateNotFound)
	if got != nil {
		t.Errorf("模板不存在时不得返回渲染结果，实际 %+v", got)
	}
	// 错误里必须带定位信息，运营才能判断是漏发布还是发错语言。
	// 文案来自 repository.FindPublished（repository.go:150，langs=[...] 是候选语言全集），
	// logic 自己的 lang=/version= 分支（rendertemplatelogic.go:74）只在
	// 底层返回 (nil, nil) 时才可达，这里断实际口径。
	wantErrContains(t, "模板不存在", err, "code=no_such")
	wantErrContains(t, "模板不存在", err, "langs=[zh-CN]")
	wantOps(t, "模板不存在只查已发布版本", e.ops(m), []string{
		"tmpl.FindByState:no_such/1/zh-CN/st2",
	})

	// 只有草稿时也判不存在：预览不允许把未发布内容当现网结果返回。
	m2 := e.mark()
	e.seedTemplate(t, "draft_only", model.ChannelPush, model.LangZhCN, "草稿", "正文",
		1, model.TemplateStateDraft)
	_, err = e.callRender(t, renderReq("draft_only", 0, nil))
	wantErrIs(t, "只有草稿", err, model.ErrTemplateNotFound)
	wantOps(t, "只有草稿", e.ops(m2), []string{
		"tmpl.FindByState:draft_only/1/zh-CN/st2",
	})
}

func TestRenderTemplateStorageErrorPropagates(t *testing.T) {
	e := newEnv(t)
	dbDown := errors.New("select notification_template: db down")
	e.tmplSpy.Fail("FindByState", dbDown)
	_, err := e.callRender(t, renderReq(codeShip, 0, map[string]string{"order_no": "SO-1"}))
	wantErrIs(t, "已发布版本读库失败", err, dbDown)

	// version>0 走的是另一个方法，注入点也必须各自生效。
	e2 := newEnv(t)
	e2.tmplSpy.Fail("Find", dbDown)
	_, err = e2.callRender(t, renderReq(codeShip, 3, map[string]string{"order_no": "SO-1"}))
	wantErrIs(t, "指定版本读库失败", err, dbDown)
}
