package logic

// SendNotification 的补充用例：模板可用性与语言回落（与 sendnotificationlogic_test.go 同属一个方法的另一组断言）。

import (
	"errors"
	"testing"

	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/policy"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// TestSendNotificationDraftTemplateNotDeliverable 草稿版本不可用于投递：
// FindPublished 只认 state=published，草稿存在也必须报模板不存在（不能拿草稿内容外发）。
func TestSendNotificationDraftTemplateNotDeliverable(t *testing.T) {
	e := newEnv(t)
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody,
		1, model.TemplateStateDraft)

	reply, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantErrIs(t, "只有草稿", err, model.ErrTemplateNotFound)
	if reply != nil {
		t.Errorf("拒绝时不得返回受理响应，实际 %+v", reply)
	}
	wantEQ(t, "只有草稿", "投递行数", e.deliveryCount(), 0)
	// 已下线的版本同样不可用（草稿 -> 发布 -> 下线 后不得再被命中）。
	e.seedTemplate(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody,
		2, model.TemplateStateOffline)
	_, err = e.send(t, pushReq("ship-2", recip(midAlice, "device-token-a")))
	wantErrIs(t, "只有已下线版本", err, model.ErrTemplateNotFound)
}

// TestSendNotificationTemplateLookupErrorPropagates 模板读库失败原样上抛，不退化成“模板不存在”。
func TestSendNotificationTemplateLookupErrorPropagates(t *testing.T) {
	e := newEnv(t)
	dbDown := errors.New("select notification_template: db down")
	e.tmplSpy.Fail("FindByState", dbDown)

	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantErrIs(t, "模板读失败", err, dbDown)
	wantEQ(t, "模板读失败", "投递行数", e.deliveryCount(), 0)
}

// TestSendNotificationLanguageFallbackOrder 语言回落顺序：本人指定 -> 请求默认 -> 全局默认，
// 命中哪一语言就如实落 lang，不伪造成本人指定的语言。
//
// 回落链在 send.Enqueuer.langCandidates（enqueue.go:316）里按「去重保序」构造：
// 若请求不带 default_language、全局默认又是 zh-CN，则候选集只剩 [zh-CN]，
// 根本不会出现第二次查询 —— 那条路径由 sendnotificationlogic_test.go 的
// TestSendNotificationValidationRejectedExplicitly「模板缺版本」用例覆盖。
// 本用例要证明的是「回落」，因此必须真的给出第二级候选（请求级 default_language）。
func TestSendNotificationLanguageFallbackOrder(t *testing.T) {
	e := newEnv(t)
	// 只发布 zh-TW 版本。
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhTW, "出貨通知", "訂單 {{order_no}} 已出貨")

	m := e.mark()
	in := pushReq("ship-1",
		&rpc.Recipient{Mid: midAlice, TargetRef: "token-a", Language: rpc.Language_LANGUAGE_ZH_CN})
	in.DefaultLanguage = rpc.Language_LANGUAGE_ZH_TW
	_, err := e.send(t, in)
	wantNoErr(t, "语言回落", err)
	row := e.onlyDelivery(t)
	wantEQ(t, "语言回落", "lang", row.Lang, model.LangZhTW)
	wantEQ(t, "语言回落", "template_version", row.TemplateVersion, int32(1))
	// 先查本人语言 zh-CN、未命中再查请求默认 zh-TW：两次都必须是 state=published，
	// 不能顺手命中草稿；全局默认 zh-CN 与第一级重复，被去重后不产生第三次查询。
	wantOps(t, "语言回落查询顺序", e.ops(m), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
		"tmpl.FindByState:order_shipped/1/zh-TW/st2",
		"dnd.FindOne:1001",
		"deliv.Insert:gk=ship-1/mid1001",
	})
	// 反证回落次序：补上本人语言的已发布版本后，同一请求必须止步于第一级，
	// 不再产生第二次查询（否则上面的 zh-TW 命中只是「碰巧查了两遍」）。
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	in2 := pushReq("ship-2",
		&rpc.Recipient{Mid: midAlice, TargetRef: "token-a", Language: rpc.Language_LANGUAGE_ZH_CN})
	in2.DefaultLanguage = rpc.Language_LANGUAGE_ZH_TW
	m2 := e.mark()
	_, err = e.send(t, in2)
	wantNoErr(t, "语言命中本人指定", err)
	wantOps(t, "语言命中本人指定", e.ops(m2), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
		"dnd.FindOne:1001",
		"deliv.Insert:gk=ship-2/mid1001",
	})
	wantEQ(t, "语言命中本人指定", "lang", e.deliveryByGroup(t, "ship-2").Lang, model.LangZhCN)
}

// TestSendNotificationAnonymousRecipientSkipsPerUserChecks mid=0 的接收人无法按用户配额、
// 也没有偏好可查：必须仍然落库，而不是被静默跳过（通道级限流本期不做）。
func TestSendNotificationAnonymousRecipientSkipsPerUserChecks(t *testing.T) {
	e := newEnv(t, withNotify(func(c *config.NotificationConf) { c.DailyQuotaPerMid = 1 }))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	m := e.mark()
	reply, err := e.send(t, pushReq("ship-1", recip(0, "anonymous-token")))
	wantNoErr(t, "匿名接收人", err)
	row := e.onlyDelivery(t)
	wantEQ(t, "匿名接收人", "mid", row.Mid, int64(0))
	wantEQ(t, "匿名接收人", "state", row.State, model.DeliveryStatePending)
	wantEQ(t, "匿名接收人", "响应 state", reply.GetDeliveries()[0].GetState(), rpc.DeliveryState_DELIVERY_PENDING)
	wantOps(t, "匿名接收人不碰偏好与配额", e.ops(m), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
		"deliv.Insert:gk=ship-1/mid0",
	})
}

// TestSendNotificationRenderGuardsMatchDispatchGuards 入口试渲染与投递时渲染必须同口径：
// 入口放行而投递失败的模板会稳定产出死信，入口拦死而投递能过的模板则永远投不出去。
func TestSendNotificationRenderGuardsMatchDispatchGuards(t *testing.T) {
	e := newEnv(t)
	// 模板需要两个变量，请求只给一个：入口必须报缺失清单，且不落库。
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle,
		"订单 {{order_no}} 已发货，运费 {{fee}}")
	m := e.mark()
	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantErrIs(t, "变量缺失", err, policy.ErrRenderMissingVar)
	wantErrContains(t, "变量缺失", err, "fee")
	wantEQ(t, "变量缺失", "投递行数", e.deliveryCount(), 0)
	if got := len(e.opsContaining(m, "deliv.Insert")); got != 0 {
		t.Errorf("变量缺失不得落库，实际写入 %d 行", got)
	}

	// 补齐变量后同一 biz_key 可继续受理（入口校验不是“一次失败永久拉黑”）。
	in := pushReq("ship-1", recip(midAlice, "device-token-a"))
	in.TemplateParams = map[string]string{"order_no": "SO-1", "fee": "6"}
	_, err = e.send(t, in)
	wantNoErr(t, "补齐变量", err)
	row := e.onlyDelivery(t)
	wantEQ(t, "补齐变量", "params_json", row.ParamsJson, `{"fee":"6","order_no":"SO-1"}`)
	wantEQ(t, "补齐变量", "state", row.State, model.DeliveryStatePending)
}
