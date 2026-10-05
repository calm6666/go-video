package logic

// ListDeliveries 用例：分页、过滤条件、total 的真实口径。
//
// 这里要钉住的四件事：
//   ① total 是「过滤后的总行数」，与页码无关，越界页不得把 total 归零；
//   ② 排序是 ctime 倒序（model/notificationdelivery.go:247 ORDER BY ctime DESC, delivery_id DESC），
//      用例用互不相同的 ctime 让顺序可预测，避免「只断言集合相等」掩盖排序回归；
//   ③ 所有过滤条件都是 AND 组合，且 0 值表示「不过滤」（State=UNSPECIFIED 不是「查 state=0」）；
//   ④ 调用方手里的 biz_key 与库里落的 biz_key 不是同一个值 —— 见
//      TestListDeliveriesCallerBizKeyIsNotTheRowKey，这条容易踩的口径必须显式钉住。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callListDeliveries(t *testing.T, in *rpc.ListDeliveriesReq) (*rpc.ListDeliveriesReply, error) {
	t.Helper()
	return NewListDeliveriesLogic(context.Background(), e.svcCtx).ListDeliveries(in)
}

// listRow 构造一行可预测排序的投递任务：delivery_id 与 ctime 都由用例给定。
func listRow(id string, mid int64, channel, state int32, ctime int64) *model.NotificationDelivery {
	return &model.NotificationDelivery{
		DeliveryId: id, BizKey: "row-" + id, BizGroupKey: "grp-" + id, Mid: mid,
		Channel: channel, TemplateCode: codeShip, TemplateVersion: 1, Lang: model.LangZhCN,
		State: state, Ctime: ctime, Mtime: ctime,
	}
}

// seedFourRows 铺 4 行：alice 3 行（ctime 300/200/100，通道与状态各不相同）+ bob 1 行（ctime 400）。
func (e *env) seedFourRows(t *testing.T) {
	t.Helper()
	e.seedDelivery(t, listRow("DLV-A", midAlice, model.ChannelPush, model.DeliveryStatePending, 300))
	e.seedDelivery(t, listRow("DLV-B", midAlice, model.ChannelSMS, model.DeliveryStateSent, 200))
	e.seedDelivery(t, listRow("DLV-C", midAlice, model.ChannelPush, model.DeliveryStateSuppressed, 100))
	e.seedDelivery(t, listRow("DLV-D", midBob, model.ChannelPush, model.DeliveryStatePending, 400))
}

// idsOf 取返回行的 delivery_id 序列（保持服务端给的顺序，不排序，这样排序回归才能被抓到）。
func idsOf(rows []*rpc.DeliveryInfo) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetDeliveryId())
	}
	return out
}

func TestListDeliveriesPaginatesWithoutTouchingTotal(t *testing.T) {
	e := newEnv(t)
	e.seedFourRows(t)

	m := e.mark()
	p1, err := e.callListDeliveries(t, &rpc.ListDeliveriesReq{Mid: midAlice, Pn: 1, Ps: 2})
	wantNoErr(t, "第 1 页", err)
	wantOps(t, "第 1 页", e.ops(m), []string{"deliv.List:mid1001/ch0/st0/pn1/ps2"})
	wantStringsEQ(t, "第 1 页顺序（ctime 倒序）", idsOf(p1.GetDeliveries()), []string{"DLV-A", "DLV-B"})
	wantEQ(t, "第 1 页", "total", p1.GetTotal(), int64(3))

	p2, err := e.callListDeliveries(t, &rpc.ListDeliveriesReq{Mid: midAlice, Pn: 2, Ps: 2})
	wantNoErr(t, "第 2 页", err)
	wantStringsEQ(t, "第 2 页", idsOf(p2.GetDeliveries()), []string{"DLV-C"})
	wantEQ(t, "第 2 页", "total 不随页码变化", p2.GetTotal(), int64(3))

	// 越界页：空列表 + total 原样，绝不报错（运营翻过头一页是常态）。
	p9, err := e.callListDeliveries(t, &rpc.ListDeliveriesReq{Mid: midAlice, Pn: 3, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "越界页", "行数", len(p9.GetDeliveries()), 0)
	wantEQ(t, "越界页", "total 仍为全量", p9.GetTotal(), int64(3))

	// 不带 mid：全表 4 行，bob 的最新记录排在最前。
	all, err := e.callListDeliveries(t, &rpc.ListDeliveriesReq{Pn: 1, Ps: 10})
	wantNoErr(t, "不过滤用户", err)
	wantStringsEQ(t, "不过滤用户", idsOf(all.GetDeliveries()), []string{"DLV-D", "DLV-A", "DLV-B", "DLV-C"})
	wantEQ(t, "不过滤用户", "total", all.GetTotal(), int64(4))
}

func TestListDeliveriesFiltersAreAndComposedWithZeroMeansNoFilter(t *testing.T) {
	e := newEnv(t)
	e.seedFourRows(t)

	cases := []struct {
		name  string
		req   *rpc.ListDeliveriesReq
		op    string
		want  []string
		total int64
	}{
		{"按通道 sms", &rpc.ListDeliveriesReq{Channel: rpc.Channel_CHANNEL_SMS, Pn: 1, Ps: 10},
			"deliv.List:mid0/ch2/st0/pn1/ps10", []string{"DLV-B"}, 1},
		{"按状态 suppressed（终态也必须查得到）",
			&rpc.ListDeliveriesReq{State: rpc.DeliveryState_DELIVERY_SUPPRESSED, Pn: 1, Ps: 10},
			"deliv.List:mid0/ch0/st6/pn1/ps10", []string{"DLV-C"}, 1},
		{"状态未指定=不过滤", &rpc.ListDeliveriesReq{State: rpc.DeliveryState_DELIVERY_STATE_UNSPECIFIED, Pn: 1, Ps: 10},
			"deliv.List:mid0/ch0/st0/pn1/ps10", []string{"DLV-D", "DLV-A", "DLV-B", "DLV-C"}, 4},
		{"用户 + 通道（AND）", &rpc.ListDeliveriesReq{Mid: midAlice, Channel: rpc.Channel_CHANNEL_PUSH, Pn: 1, Ps: 10},
			"deliv.List:mid1001/ch1/st0/pn1/ps10", []string{"DLV-A", "DLV-C"}, 2},
		{"用户 + 状态（AND）", &rpc.ListDeliveriesReq{Mid: midAlice, State: rpc.DeliveryState_DELIVERY_SENT, Pn: 1, Ps: 10},
			"deliv.List:mid1001/ch0/st2/pn1/ps10", []string{"DLV-B"}, 1},
		{"创建时间窗口（闭区间）", &rpc.ListDeliveriesReq{StartCtime: 150, EndCtime: 300, Pn: 1, Ps: 10},
			"deliv.List:mid0/ch0/st0/pn1/ps10", []string{"DLV-A", "DLV-B"}, 2},
		{"矛盾组合不得退化成无条件全表",
			&rpc.ListDeliveriesReq{Mid: midBob, Channel: rpc.Channel_CHANNEL_SMS, Pn: 1, Ps: 10},
			"deliv.List:mid1002/ch2/st0/pn1/ps10", nil, 0},
	}
	for _, tc := range cases {
		m := e.mark()
		got, err := e.callListDeliveries(t, tc.req)
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name+"：过滤条件必须原样下推到 model", e.ops(m), []string{tc.op})
		wantEQ(t, tc.name, "total", got.GetTotal(), tc.total)
		if tc.want == nil {
			// 空结果必须是「非 nil 的空列表」，否则客户端要到处判空。
			if got.GetDeliveries() == nil {
				t.Errorf("%s：空结果 deliveries = nil, want 非 nil 空切片", tc.name)
			}
			continue
		}
		wantStringsEQ(t, tc.name, idsOf(got.GetDeliveries()), tc.want)
	}
}

// TestListDeliveriesCallerBizKeyIsNotTheRowKey 调用方提交的 biz_key 落库时是
// biz_group_key，而 notification_delivery.biz_key 是按接收人派生的 sha256 行级幂等键
// （send/enqueue.go:208 + policy.RowBizKey）。本方法把请求里的 biz_key 直接下推给
// DeliveryFilter.BizKey，因此「用自己给的 biz_key 查列表」一定查不到，
// 而 DeliveryFilter 已有的 BizGroupKey / SourceEventId 两个可用过滤位在 rpc 契约里没有对应字段。
// 这条不是想当然：先把真实投递跑出来，再分别用两种键查。
func TestListDeliveriesCallerBizKeyIsNotTheRowKey(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "落一行真实投递", err)

	stored := e.deliveryByGroup(t, "ship-1")
	wantEQ(t, "落库行", "biz_group_key 才是调用方给的键", stored.BizGroupKey, "ship-1")
	wantTrue(t, "落库行", "biz_key 是派生键而不是原键", stored.BizKey != "ship-1")

	byGroupKey, err := e.callListDeliveries(t, &rpc.ListDeliveriesReq{BizKey: "ship-1", Pn: 1, Ps: 10})
	wantNoErr(t, "用调用方 biz_key 查", err)
	wantEQ(t, "用调用方 biz_key 查", "命中行数", len(byGroupKey.GetDeliveries()), 0)
	wantEQ(t, "用调用方 biz_key 查", "total", byGroupKey.GetTotal(), int64(0))

	byRowKey, err := e.callListDeliveries(t, &rpc.ListDeliveriesReq{BizKey: stored.BizKey, Pn: 1, Ps: 10})
	wantNoErr(t, "用行级 biz_key 查", err)
	wantEQ(t, "用行级 biz_key 查", "命中行数", len(byRowKey.GetDeliveries()), 1)
	wantEQ(t, "用行级 biz_key 查", "delivery_id", byRowKey.GetDeliveries()[0].GetDeliveryId(), stored.DeliveryId)
}

func TestListDeliveriesNormalizesPageParamsBeforeHittingModel(t *testing.T) {
	e := newEnv(t)
	e.seedFourRows(t)

	cases := []struct {
		name     string
		pn, ps   int32
		wantPage string
	}{
		{"页码 0 回落第 1 页", 0, 10, "deliv.List:mid0/ch0/st0/pn1/ps10"},
		{"负页码回落第 1 页", -3, 10, "deliv.List:mid0/ch0/st0/pn1/ps10"},
		{"每页 0 回落 20", 1, 0, "deliv.List:mid0/ch0/st0/pn1/ps20"},
		{"每页负数回落 20", 1, -5, "deliv.List:mid0/ch0/st0/pn1/ps20"},
		// 上限在 logic 侧夹到 100，所以 model 的 ErrPsTooLarge 从 RPC 永远拿不到，
		// 这里钉住「不报错 + 夹紧」而不是钉一个不可能发生的错误。
		{"每页超上限被夹紧", 1, 500, "deliv.List:mid0/ch0/st0/pn1/ps100"},
	}
	for _, tc := range cases {
		m := e.mark()
		got, err := e.callListDeliveries(t, &rpc.ListDeliveriesReq{Pn: tc.pn, Ps: tc.ps})
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name, e.ops(m), []string{tc.wantPage})
		wantEQ(t, tc.name, "total", got.GetTotal(), int64(4))
	}
}

func TestListDeliveriesRejectsNilAndPropagatesStoreError(t *testing.T) {
	e := newEnv(t)
	e.seedFourRows(t)

	m := e.mark()
	if _, err := e.callListDeliveries(t, nil); err == nil {
		t.Error("nil 请求必须报错")
	} else {
		wantErrContains(t, "nil 请求", err, "nil request")
	}
	wantOps(t, "nil 请求不读库", e.ops(m), nil)

	down := errors.New("notification_delivery List: db down")
	e.delivSpy.Fail("List", down)
	m2 := e.mark()
	reply, err := e.callListDeliveries(t, &rpc.ListDeliveriesReq{Pn: 1, Ps: 10})
	wantErrIs(t, "投递表读失败", err, down)
	if reply != nil {
		t.Errorf("读库失败时不得返回空列表冒充「一条都没有」，实际 %+v", reply)
	}
	wantOps(t, "投递表读失败", e.ops(m2), []string{"deliv.List:mid0/ch0/st0/pn1/ps10"})
}
