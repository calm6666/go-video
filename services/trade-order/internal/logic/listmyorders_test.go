package logic

// ListMyOrders 的用例级测试（终端「我的订单」页）。
//
// 契约要点（README §7、listmyorderslogic.go 注释）：
//   - mid 强制必填：本方法是终端面，没有「跨用户」这条路径；
//   - 读路径固定走 idx_mid_state_created，因此**不接受**时间窗/订单号/支付单号等
//     运营面过滤位 —— 一旦这些位能被调用方填进来，就是一条越权读别人的通道；
//   - page/size 有界（MaxPageSize），越界拒绝而不是裁剪；
//   - 过滤必须在 SQL 侧生效（total 是过滤后的总数），否则翻页数不对、
//     客户端会以为「还有更多」而反复翻空页；
//   - 空列表回非 nil 切片；库存故障必须报错，不能伪装成空列表。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

func TestListMyOrdersRequiresMidBeforeTouchingDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListMyOrdersLogic(context.Background(), svcCtx)

	// mid 是第一个守卫：连 size 越界这种「更靠前」的入参错误都不能抢先，
	// 因为非法 mid 意味着调用方根本没带身份。
	for _, mid := range []int64{0, -1, -99999} {
		got, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: mid, Page: 1, Size: 999})
		if !errors.Is(err, model.ErrInvalidMid) {
			t.Errorf("mid=%d 应回 ErrInvalidMid，得到 %v", mid, err)
		}
		if got != nil {
			t.Errorf("mid=%d 被拒时仍回应答：%s", mid, got.String())
		}
	}
	assertCalls(t, db)
}

func TestListMyOrdersRejectsOversizedPageWithoutQuerying(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_a", Mid: 1001, State: model.StatePaid})
	l := NewListMyOrdersLogic(context.Background(), svcCtx)

	cases := []struct {
		name string
		req  *rpc.ListMyOrdersReq
	}{
		{"size 越上限（裁剪会让用户以为订单就这么点）", &rpc.ListMyOrdersReq{Mid: 1001, Size: 101}},
		{"负页码", &rpc.ListMyOrdersReq{Mid: 1001, Page: -1, Size: 10}},
		{"负 size", &rpc.ListMyOrdersReq{Mid: 1001, Size: -5}},
	}
	for _, c := range cases {
		got, err := l.ListMyOrders(c.req)
		if !errors.Is(err, model.ErrInvalidPage) {
			t.Errorf("%s：得到 %v，期望 ErrInvalidPage", c.name, err)
		}
		if got != nil {
			t.Errorf("%s：被拒时仍回应答 %s", c.name, got.String())
		}
	}
	// 报错文本必须带上限与实际值，调用方才知道该改成多少（不能只回一句 invalid page）。
	if _, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001, Size: 101}); err == nil ||
		!strings.Contains(err.Error(), "size=101") || !strings.Contains(err.Error(), "max=100") {
		t.Errorf("ErrInvalidPage 的上下文缺失: %v", err)
	}
	// 四次调用（含库里有数据）一次都没触库。
	assertCalls(t, db)
}

// TestListMyOrdersMaxPageSizeComesFromConfig 钉住分页上限取的是服务配置而不是硬编码：
// 配置缺项（<=0）时按 paginate 的 100 兜底，而不是「无限制」。
func TestListMyOrdersMaxPageSizeComesFromConfig(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Config.TradeOrder.MaxPageSize = 5
	seedOrder(t, db, &model.Order{OrderNo: "to_c1", Mid: 1001, State: model.StatePaid})
	l := NewListMyOrdersLogic(context.Background(), svcCtx)

	if _, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001, Size: 6}); !errors.Is(err, model.ErrInvalidPage) {
		t.Errorf("MaxPageSize=5 时 size=6 应被拒，得到 %v", err)
	}
	reply, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001, Size: 5})
	if err != nil {
		t.Fatalf("size=5 应放行: %v", err)
	}
	if reply.GetSize() != 5 || db.lastFilter.Limit != 5 {
		t.Errorf("生效 size 未回显/未下传: reply=%d filter=%+v", reply.GetSize(), db.lastFilter)
	}

	svcCtx.Config.TradeOrder.MaxPageSize = 0
	if _, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001, Size: 101}); !errors.Is(err, model.ErrInvalidPage) {
		t.Errorf("MaxPageSize 缺省时应按 100 兜底拒绝，得到 %v", err)
	}
	assertCalls(t, db, "ListByFilter")
}

func TestListMyOrdersValidatesEnumFiltersBeforeTouchingDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_e", Mid: 1001, State: model.StatePaid})
	l := NewListMyOrdersLogic(context.Background(), svcCtx)

	cases := []struct {
		name string
		req  *rpc.ListMyOrdersReq
		want error
	}{
		{"未定义状态", &rpc.ListMyOrdersReq{Mid: 1001, State: rpc.OrderState(12)}, model.ErrInvalidStateTransition},
		{"超出枚举上界", &rpc.ListMyOrdersReq{Mid: 1001, State: rpc.OrderState(99)}, model.ErrInvalidStateTransition},
		{"负状态", &rpc.ListMyOrdersReq{Mid: 1001, State: rpc.OrderState(-1)}, model.ErrInvalidStateTransition},
		{"第三种业务类型", &rpc.ListMyOrdersReq{Mid: 1001, BizType: rpc.OrderBizType(3)}, model.ErrInvalidBizType},
		{"业务类型负值", &rpc.ListMyOrdersReq{Mid: 1001, BizType: rpc.OrderBizType(-2)}, model.ErrInvalidBizType},
		// 两个枚举都非法时状态先被判：口径必须固定，否则同一入参在不同版本回不同错误。
		{"状态与业务类型同时非法（状态优先）", &rpc.ListMyOrdersReq{Mid: 1001, State: rpc.OrderState(12),
			BizType: rpc.OrderBizType(3)}, model.ErrInvalidStateTransition},
	}
	for _, c := range cases {
		got, err := l.ListMyOrders(c.req)
		if !errors.Is(err, c.want) {
			t.Errorf("%s：得到 %v，期望 %v", c.name, err, c.want)
		}
		if got != nil {
			t.Errorf("%s：被拒时仍回应答 %s", c.name, got.String())
		}
	}
	if _, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001, State: rpc.OrderState(12)}); err == nil ||
		!strings.Contains(err.Error(), "state=12") {
		t.Errorf("非法状态要在错误里回显实际取值: %v", err)
	}
	if _, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001, BizType: rpc.OrderBizType(3)}); err == nil ||
		!strings.Contains(err.Error(), "biz_type=3") {
		t.Errorf("非法业务类型要在错误里回显实际取值: %v", err)
	}
	// UNSPECIFIED(0) 是「不过滤」而不是非法值：放行到下一段并下传零值过滤位。
	if _, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001}); err != nil {
		t.Fatalf("全 0 枚举应视为不过滤: %v", err)
	}
	if db.lastFilter.State != 0 || db.lastFilter.BizType != 0 {
		t.Errorf("UNSPECIFIED 不该被当成过滤条件（会把结果集筛成空）: %+v", db.lastFilter)
	}
	// 上面 8 次调用都被守卫拦住，只有最后一次放行。
	assertCalls(t, db, "ListByFilter")
}

// TestListMyOrdersForwardsOnlyOwnScopeFilter 是本方法的安全核心：
// 下传给 model 的 OrderFilter 必须**只有** mid/state/biz_type/offset/limit 这五位，
// order_no、payment_no、pay_method、时间窗、States(IN) 一律为零值 ——
// 这几个位是运营面（ListOrders）的能力，终端面带着它们就等于越权读别人的单。
func TestListMyOrdersForwardsOnlyOwnScopeFilter(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_f", Mid: 1001, State: model.StateFulfilled,
		BizType: model.BizMembership})
	l := NewListMyOrdersLogic(context.Background(), svcCtx)

	if _, err := l.ListMyOrders(&rpc.ListMyOrdersReq{
		Mid: 1001, State: rpc.OrderState_ORDER_STATE_FULFILLED,
		BizType: rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK, Page: 3, Size: 10,
	}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	want := &model.OrderFilter{Mid: 1001, State: model.StateFulfilled, BizType: model.BizCoinPack, Offset: 20, Limit: 10}
	if !reflect.DeepEqual(db.lastFilter, want) {
		t.Errorf("下传的过滤条件不符：\n got=%+v\nwant=%+v", db.lastFilter, want)
	}

	// 默认页口径：page/size 省略 => 第 1 页 20 条（offset 0）。
	if _, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001}); err != nil {
		t.Fatalf("默认分页调用失败: %v", err)
	}
	if !reflect.DeepEqual(db.lastFilter, &model.OrderFilter{Mid: 1001, Offset: 0, Limit: 20}) {
		t.Errorf("默认过滤条件不符：got=%+v want=mid=1001,offset=0,limit=20", db.lastFilter)
	}

	reply, err := l.ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001, Page: 4, Size: 7})
	if err != nil {
		t.Fatalf("翻页调用失败: %v", err)
	}
	if db.lastFilter.Offset != 21 || db.lastFilter.Limit != 7 {
		t.Errorf("offset 换算应为 (page-1)*size：得到 %+v", db.lastFilter)
	}
	if reply.GetPage() != 4 || reply.GetSize() != 7 {
		t.Errorf("应答必须回显生效的分页位，得到 page=%d size=%d", reply.GetPage(), reply.GetSize())
	}
}

// TestListMyOrdersSortsAndProjectsPerRow 同时钉三件事：
//  1. 排序口径 created_at DESC, id DESC（最新单在前）；
//  2. 每条应答的每个字段都来自同一行（跨行串字段是最难发现的一类投影 bug）；
//  3. 别人的单既不出现在列表里，也不计入 total。
func TestListMyOrdersSortsAndProjectsPerRow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedOrder(t, db, &model.Order{OrderNo: "to_old", Mid: 1001, State: model.StatePaid,
		AmountMinor: 111, UnitPriceMinor: 111, Version: 3, CreatedAt: now - 3600, UpdatedAt: now - 3600})
	seedOrder(t, db, &model.Order{OrderNo: "to_mid", Mid: 1001, State: model.StateCancelled,
		AmountMinor: 222, UnitPriceMinor: 222, Version: 4, CreatedAt: now - 1800, UpdatedAt: now - 1800})
	seedOrder(t, db, &model.Order{OrderNo: "to_new", Mid: 1001, State: model.StateFulfilled,
		AmountMinor: 333, UnitPriceMinor: 333, Version: 5, CreatedAt: now - 60, UpdatedAt: now - 60})
	seedOrder(t, db, &model.Order{OrderNo: "to_theirs", Mid: 2002, State: model.StatePaid,
		AmountMinor: 444, UnitPriceMinor: 444, Version: 1, CreatedAt: now - 1, UpdatedAt: now - 1})

	reply, err := NewListMyOrdersLogic(context.Background(), svcCtx).
		ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if reply.GetTotal() != 3 {
		t.Errorf("total 只该数自己的单，得到 %d", reply.GetTotal())
	}
	wantOrder := []string{"to_new", "to_mid", "to_old"}
	wantState := []rpc.OrderState{
		rpc.OrderState_ORDER_STATE_FULFILLED, rpc.OrderState_ORDER_STATE_CANCELLED, rpc.OrderState_ORDER_STATE_PAID,
	}
	wantAmount := []int64{333, 222, 111}
	wantVersion := []int64{5, 4, 3}
	if len(reply.GetOrders()) != len(wantOrder) {
		t.Fatalf("返回条数 %d，期望 %d（%v）", len(reply.GetOrders()), len(wantOrder), wantOrder)
	}
	for i, o := range reply.GetOrders() {
		if o.GetOrderNo() != wantOrder[i] {
			t.Errorf("第 %d 条应为 %s，得到 %s（排序口径 created_at DESC, id DESC）",
				i, wantOrder[i], o.GetOrderNo())
		}
		if o.GetMid() != 1001 {
			t.Errorf("第 %d 条 mid=%d，列表里混进了别人的单", i, o.GetMid())
		}
		if o.GetState() != wantState[i] || o.GetAmountMinor() != wantAmount[i] ||
			o.GetUnitPriceMinor() != wantAmount[i] || o.GetVersion() != wantVersion[i] {
			t.Errorf("第 %d 条字段串行了: %s", i, o.String())
		}
	}
	assertReadOnly(t, db)
}

// TestListMyOrdersFilteringHappensInSQLNotInMemory 钉住 total 的口径：
// total 是**过滤后**的总数。若实现是「先捞一页再本地筛」，total 会是全量数，
// 客户端按 total 算页数就会翻出一堆空页（而且被筛掉的行永远看不见）。
func TestListMyOrdersFilteringHappensInSQLNotInMemory(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedOrder(t, db, &model.Order{OrderNo: "to_p1", Mid: 1001, State: model.StatePaid, CreatedAt: now - 100})
	seedOrder(t, db, &model.Order{OrderNo: "to_p2", Mid: 1001, State: model.StatePaid, CreatedAt: now - 200})
	seedOrder(t, db, &model.Order{OrderNo: "to_f1", Mid: 1001, State: model.StateFulfilled, CreatedAt: now - 300})
	seedOrder(t, db, &model.Order{OrderNo: "to_f2", Mid: 1001, State: model.StateFulfilled, CreatedAt: now - 400})

	l := NewListMyOrdersLogic(context.Background(), svcCtx)
	reply, err := l.ListMyOrders(&rpc.ListMyOrdersReq{
		Mid: 1001, State: rpc.OrderState_ORDER_STATE_PAID, Page: 1, Size: 2,
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if reply.GetTotal() != 2 {
		t.Errorf("total 应为过滤后的 2，得到 %d（像是本地筛出来的全量数）", reply.GetTotal())
	}
	if len(reply.GetOrders()) != 2 {
		t.Fatalf("页内条数 %d，期望 2", len(reply.GetOrders()))
	}
	for _, o := range reply.GetOrders() {
		if o.GetState() != rpc.OrderState_ORDER_STATE_PAID {
			t.Errorf("状态过滤没生效，混进 %s=%s", o.GetOrderNo(), o.GetState())
		}
	}

	// 翻页越界：SQL 的 LIMIT/OFFSET 落在结果集之后 —— 回空列表 + 真 total，不报错。
	over, err := l.ListMyOrders(&rpc.ListMyOrdersReq{
		Mid: 1001, State: rpc.OrderState_ORDER_STATE_PAID, Page: 9, Size: 2,
	})
	if err != nil {
		t.Fatalf("越界翻页不该报错: %v", err)
	}
	if over.GetTotal() != 2 || len(over.GetOrders()) != 0 || over.GetPage() != 9 {
		t.Errorf("越界翻页应回空列表 + total=2 + page=9，得到 total=%d 条数=%d page=%d",
			over.GetTotal(), len(over.GetOrders()), over.GetPage())
	}
}

// TestListMyOrdersEmptyListIsNotNullSlice 是客户端契约（proto 注释/README §7）。
func TestListMyOrdersEmptyListIsNotNullSlice(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_other", Mid: 2002, State: model.StatePaid})

	reply, err := NewListMyOrdersLogic(context.Background(), svcCtx).
		ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001, Page: 1, Size: 20})
	if err != nil {
		t.Fatalf("查不到不是错误: %v", err)
	}
	if reply.GetOrders() == nil {
		t.Error("orders 为 nil 切片，客户端要为 null 数组写分支")
	}
	if len(reply.GetOrders()) != 0 || reply.GetTotal() != 0 {
		t.Errorf("应为空列表 + total=0，得到 条数=%d total=%d", len(reply.GetOrders()), reply.GetTotal())
	}
	if reply.GetPage() != 1 || reply.GetSize() != 20 {
		t.Errorf("空列表也要回显分页位，得到 page=%d size=%d", reply.GetPage(), reply.GetSize())
	}
	assertReadOnly(t, db)
}

// TestListMyOrdersPropagatesStoreError 保证库存故障不会被伪装成「这个用户没有订单」。
func TestListMyOrdersPropagatesStoreError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	storeErr := errors.New("trade-order: to_order ListByFilter: driver: bad connection")
	db.listErr = storeErr

	reply, err := NewListMyOrdersLogic(context.Background(), svcCtx).
		ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001})
	if !errors.Is(err, storeErr) {
		t.Errorf("依赖错误必须原样上抛，得到 %v", err)
	}
	if reply != nil {
		t.Errorf("报错时不该回应答（空列表会被客户端缓存成「无订单」）：%s", reply.String())
	}
	assertCalls(t, db, "ListByFilter")
	assertReadOnly(t, db)
}

// TestListMyOrdersPassesContextToStore 钉住 ctx 一路传到 model：
// 超时与 trace_id 都挂在它上面，丢了等于每次读都不受 deadline 约束。
func TestListMyOrdersPassesContextToStore(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	rec := &ctxRecordingOrderModel{fakeOrderModel: fakeOrderModel{db: db}}
	svcCtx.Orders = rec
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "trace-1")

	if _, err := NewListMyOrdersLogic(ctx, svcCtx).ListMyOrders(&rpc.ListMyOrdersReq{Mid: 1001}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got, _ := rec.gotCtx.Value(ctxKey{}).(string); got != "trace-1" {
		t.Errorf("model 收到的 ctx 丢了调用方上下文，得到 %q", got)
	}
}

type ctxRecordingOrderModel struct {
	fakeOrderModel
	gotCtx context.Context
}

func (m *ctxRecordingOrderModel) ListByFilter(ctx context.Context,
	f *model.OrderFilter,
) ([]*model.Order, int64, error) {
	m.gotCtx = ctx
	return m.fakeOrderModel.ListByFilter(ctx, f)
}
