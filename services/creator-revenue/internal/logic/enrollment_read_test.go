package logic

// 本文件锁两个「参与关系」读入口（GetEnrollment / ListEnrollments）的判定口径：
//   - 闸门必须发生在任何数据访问之前（`len(db.reads)==0` + `len(db.calls)==0`），
//     非法 mid / 越界状态不该烧掉一次查询；
//   - 「查不到」与「查不动」必须分开：未参加 → found=false（正常业务结论），
//     读故障 → 原样上抛错误（创作者端首页不能把故障渲染成「你还没加入计划」）；
//   - 顺序期望一律抄 model 的 ORDER BY 事实源（cr_enrollment.go:172 `ORDER BY mid ASC`），
//     分页拼接结果与全量回读逐行比对，而不是手抄字面量；
//   - 折出来的 offset/limit 必须真的进了查询（db.window），只看回显 page/size 判不出截断；
//   - 归属校验现状：两个入口的**请求里都没有调用方身份位**，所以服务端无从校验
//     「你是不是你」——已按现状钉住并登记 README（报名域）。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"
)

func runGetEnrollment(t *testing.T, ctx *svc.ServiceContext, in *rpc.GetEnrollmentReq) (*rpc.GetEnrollmentReply, error) {
	t.Helper()
	return NewGetEnrollmentLogic(context.Background(), ctx).GetEnrollment(in)
}

func runListEnrollments(t *testing.T, ctx *svc.ServiceContext, in *rpc.ListEnrollmentsReq) (*rpc.ListEnrollmentsReply, error) {
	t.Helper()
	return NewListEnrollmentsLogic(context.Background(), ctx).ListEnrollments(in)
}

// enrSeed 造一行参与关系（mid/时间戳全是明显假值，不含真实身份）。
func enrSeed(mid int64, state int32, remark string) *model.Enrollment {
	return &model.Enrollment{
		Mid: mid, State: state, AgreedRuleVersion: 3,
		EnrolledAt: oldEnrolledAt, LeftAt: oldLeftAt,
		Operator: txOperator, Remark: remark,
		Ctime: oldEnrolledAt, Mtime: oldEnrolledAt + 60,
	}
}

// enrollMidSeq 把回包里的 mid 按返回顺序取出来，用于逐行比对排序事实源。
func enrollMidSeq(rows []*rpc.EnrollmentInfo) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Mid)
	}
	return out
}

func mustEnrollSeq(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("mid 序列长度不符：期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个 mid 不符：期望 %v，实际 %v", i, want, got)
		}
	}
}

// ---------------------------------------------------------------- GetEnrollment

func TestGetEnrollmentGatesTouchNoData(t *testing.T) {
	cases := []struct {
		name   string
		in     *rpc.GetEnrollmentReq
		target error
	}{
		{"mid 为零", &rpc.GetEnrollmentReq{Mid: 0}, model.ErrInvalidMid},
		{"mid 为负", &rpc.GetEnrollmentReq{Mid: -100}, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			reply, err := runGetEnrollment(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			// 读侧闸门：既不查 model，也不发 SQL。
			if len(db.reads) != 0 || len(db.calls) != 0 {
				t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
			}
		})
	}
}

func TestGetEnrollmentReadyGatePrecedesMidCheck(t *testing.T) {
	ctx, db := newTestSvc(t)
	ctx.Enrollments = nil // DataSource 未配置时 ServiceContext 里各 model 为 nil

	// 先 Ready 后校验 mid：配置缺失时连「mid 非法」都判不到，回的是 ErrDBNotConfigured。
	reply, err := runGetEnrollment(t, ctx, &rpc.GetEnrollmentReq{Mid: -1})
	mustErrIs(t, err, model.ErrDBNotConfigured)
	if reply != nil {
		t.Fatalf("未配置库不得回结论：%+v", reply)
	}
	if len(db.reads) != 0 || len(db.calls) != 0 {
		t.Fatalf("Ready 闸门不该访问数据：%v / %v", db.reads, db.calls)
	}
}

// 现状哨兵：GetEnrollment 是本服务 9 个读入口里唯一不做 in==nil 兜底的那个
// （其余 8 个开头都有 `if in == nil { in = &...{}}`）。gRPC 解簇不会传 nil，
// 所以线上不可达，但「同一个包的读入口对空请求两种态度」本身就是漂移源。
// 若哪天补上兜底，这条用例会红，届时请连 README 报名域那条登记一起撤掉。
func TestGetEnrollmentNilRequestPanics(t *testing.T) {
	ctx, db := newTestSvc(t)
	recovered := func() (v any) {
		defer func() { v = recover() }()
		_, _ = runGetEnrollment(t, ctx, nil)
		return nil
	}()
	if recovered == nil {
		t.Fatal("现状已变：nil 请求不再 panic，请撤销 README 报名域的该项登记")
	}
	if len(db.reads) != 0 || len(db.calls) != 0 {
		t.Fatalf("崩之前也不该碰数据：%v / %v", db.reads, db.calls)
	}
}

func TestGetEnrollmentProjectsEveryColumn(t *testing.T) {
	ctx, db := newTestSvc(t)
	row := db.addEnrollment(enrSeed(txMid, model.EnrollmentStateSuspended, "违规暂停"))

	reply, err := runGetEnrollment(t, ctx, &rpc.GetEnrollmentReq{Mid: txMid})
	mustNoErr(t, err)
	if !reply.Found || reply.Enrollment == nil {
		t.Fatalf("在档就该 found=true：%+v", reply)
	}
	got := reply.Enrollment
	if got.Mid != row.Mid || got.State != rpc.EnrollmentState(row.State) {
		t.Fatalf("主体/状态投影错误：%+v", got)
	}
	if got.AgreedRuleVersion != row.AgreedRuleVersion {
		t.Fatalf("已确认规则版本缺失：%+v", got)
	}
	if got.EnrolledAt != row.EnrolledAt || got.LeftAt != row.LeftAt {
		t.Fatalf("时间戳投影错误：enrolled=%d left=%d", got.EnrolledAt, got.LeftAt)
	}
	// updated_at 取的是 mtime（不是 ctime）：运营面板按它判断「最后一次改动」。
	if got.UpdatedAt != row.Mtime || got.UpdatedAt == row.Ctime {
		t.Fatalf("updated_at 口径错误：got=%d mtime=%d ctime=%d", got.UpdatedAt, row.Mtime, row.Ctime)
	}
	if got.Operator != row.Operator || got.Remark != row.Remark {
		t.Fatalf("责任人/备注投影错误：%+v", got)
	}
	// 一次读、零条 SQL：读侧不得有任何写语句副作用。
	db.assertReads(t, 0, "fake:enrollments.FindOne")
	if len(db.calls) != 0 {
		t.Fatalf("读侧执行了 SQL：%v", db.calls)
	}
}

func TestGetEnrollmentNeverEnrolledIsFoundFalseNotError(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addEnrollment(enrSeed(999, model.EnrollmentStateEnrolled, "别人的号"))

	reply, err := runGetEnrollment(t, ctx, &rpc.GetEnrollmentReq{Mid: txMid})
	mustNoErr(t, err)
	if reply.Found || reply.Enrollment != nil {
		t.Fatalf("未参加必须回 found=false + 空壳：%+v", reply)
	}
	db.assertReads(t, 0, "fake:enrollments.FindOne")
}

func TestGetEnrollmentReadFaultIsNotSilentlyNotFound(t *testing.T) {
	ctx, db := newTestSvc(t)
	boom := errors.New("connection refused")
	db.readFailOn["fake:enrollments.FindOne"] = boom

	reply, err := runGetEnrollment(t, ctx, &rpc.GetEnrollmentReq{Mid: txMid})
	if !errors.Is(err, boom) {
		t.Fatalf("读故障必须原样上抛，实际 %v", err)
	}
	if reply != nil {
		t.Fatalf("故障伪装成 found=false 就是伪成功：%+v", reply)
	}
	db.assertReads(t, 0, "fake:enrollments.FindOne")
}

// 现状哨兵（高危）：查他人参与状态没有任何归属校验。
// GetEnrollmentReq 只有 mid 一个字段，请求里不存在调用方身份位，
// logic 也无从判断「谁在问」——所以只要网关按 mid 转发，任何登录用户都能读到
// 别人的参加状态、已确认规则版本、运营备注（remark 里常写处置原因）。
// 收严位置：proto 增加 caller_mid（或由网关在 metadata 里带身份）+ 本入口比对归属。
func TestGetEnrollmentHasNoOwnershipCheckOnOtherMid(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addEnrollment(enrSeed(txMid, model.EnrollmentStateSuspended, "刷量处置中"))

	// 请求方换成另一个账号（200），查的仍是 100 的行。
	reply, err := runGetEnrollment(t, ctx, &rpc.GetEnrollmentReq{Mid: txMid})
	mustNoErr(t, err)
	if !reply.Found {
		t.Fatal("现状：归属不匹配照样回完整行")
	}
	if reply.Enrollment.State != rpc.EnrollmentState(model.EnrollmentStateSuspended) ||
		reply.Enrollment.Remark != "刷量处置中" || reply.Enrollment.AgreedRuleVersion != 3 {
		t.Fatalf("现状：他人可读到状态/备注/已确认版本：%+v", reply.Enrollment)
	}
}

// ---------------------------------------------------------------- ListEnrollments

func TestListEnrollmentsGatesTouchNoData(t *testing.T) {
	cases := []struct {
		name   string
		in     *rpc.ListEnrollmentsReq
		target error
	}{
		{"状态越界（上界）", &rpc.ListEnrollmentsReq{State: rpc.EnrollmentState(4)}, model.ErrEnrollmentStateTransition},
		{"状态越界（负数）", &rpc.ListEnrollmentsReq{State: rpc.EnrollmentState(-1)}, model.ErrEnrollmentStateTransition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			reply, err := runListEnrollments(t, ctx, c.in)
			mustErrIs(t, err, c.target)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			if len(db.reads) != 0 || len(db.calls) != 0 {
				t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
			}
		})
	}
}

func TestListEnrollmentsNilRequestIsTolerated(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addEnrollment(enrSeed(100, model.EnrollmentStateEnrolled, "a"))

	// 与 ListRevenueRules/ListSettlements 同一口径：nil 请求折成「不过滤的第一页」而不是崩。
	reply, err := runListEnrollments(t, ctx, nil)
	mustNoErr(t, err)
	if reply.Total != 1 || len(reply.Enrollments) != 1 {
		t.Fatalf("nil 请求应回全量第一页：%+v", reply)
	}
	if reply.Page != 1 || reply.Size != 100 {
		t.Fatalf("分页兜底错误：page=%d size=%d", reply.Page, reply.Size)
	}
	db.assertReads(t, 0, "fake:enrollments.List", "fake:enrollments.Count")
}

// 排序事实源是 model/cr_enrollment.go:172 的 `ORDER BY mid ASC`。
// 种子按 mid 乱序插入（并让主键顺序与 mid 顺序相反），插入序若泄漏进结果就会当场红。
func TestListEnrollmentsOrdersByMidAsc(t *testing.T) {
	ctx, db := newTestSvc(t)
	for _, e := range []*model.Enrollment{
		enrSeed(500, model.EnrollmentStateEnrolled, "e"),
		enrSeed(100, model.EnrollmentStateLeft, "a"),
		enrSeed(400, model.EnrollmentStateEnrolled, "d"),
		enrSeed(200, model.EnrollmentStateSuspended, "b"),
		enrSeed(300, model.EnrollmentStateEnrolled, "c"),
	} {
		db.addEnrollment(e)
	}

	reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{Size: 10})
	mustNoErr(t, err)
	mustEnrollSeq(t, enrollMidSeq(reply.Enrollments), 100, 200, 300, 400, 500)
	if reply.Total != 5 {
		t.Fatalf("total 应含全部状态：got=%d", reply.Total)
	}
	if w := db.window("fake:enrollments.List"); w.offset != 0 || w.limit != 10 {
		t.Fatalf("折好的分页位没进查询：%+v", w)
	}
}

// 分页必须可拼接：逐页读回的行拼起来 == 一次全量读，且每页的 offset/limit 独立可见。
func TestListEnrollmentsPagesConcatenateToFullScan(t *testing.T) {
	ctx, db := newTestSvc(t)
	for i := int64(1); i <= 5; i++ {
		db.addEnrollment(enrSeed(i*10, model.EnrollmentStateEnrolled, "r"))
	}

	var paged []int64
	for page := int64(1); page <= 4; page++ {
		reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{Page: page, Size: 2})
		mustNoErr(t, err)
		if reply.Page != page || reply.Size != 2 {
			t.Fatalf("第 %d 页回显被改写：page=%d size=%d", page, reply.Page, reply.Size)
		}
		if reply.Total != 5 {
			t.Fatalf("total 不随分页变化：got=%d", reply.Total)
		}
		if w := db.window("fake:enrollments.List"); w.offset != (page-1)*2 || w.limit != 2 {
			t.Fatalf("第 %d 页的物理分页位错误：%+v", page, w)
		}
		paged = append(paged, enrollMidSeq(reply.Enrollments)...)
	}
	mustEnrollSeq(t, paged, 10, 20, 30, 40, 50)

	// 越界页必须是「空数组」而不是「补齐」或「回到第一页」。
	reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{Page: 9, Size: 2})
	mustNoErr(t, err)
	if reply.Enrollments == nil || len(reply.Enrollments) != 0 {
		t.Fatalf("越界页必须回非 nil 空数组：%#v", reply.Enrollments)
	}
	if reply.Total != 5 {
		t.Fatalf("越界页仍要回真实总数：%d", reply.Total)
	}
}

func TestListEnrollmentsStateFilterSharesPredicateWithCount(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addEnrollment(enrSeed(100, model.EnrollmentStateEnrolled, "x"))
	db.addEnrollment(enrSeed(200, model.EnrollmentStateSuspended, "y"))
	db.addEnrollment(enrSeed(300, model.EnrollmentStateSuspended, "z"))

	reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{
		State: rpc.EnrollmentState(model.EnrollmentStateSuspended), Size: 10,
	})
	mustNoErr(t, err)
	mustEnrollSeq(t, enrollMidSeq(reply.Enrollments), 200, 300)
	// total 与 rows 必须同源（同一 state 谓词）：否则面板显示「共 3 条」只列 2 条。
	if reply.Total != int64(len(reply.Enrollments)) {
		t.Fatalf("total 与过滤结果不同源：total=%d rows=%d", reply.Total, len(reply.Enrollments))
	}
	for _, r := range reply.Enrollments {
		if r.State != rpc.EnrollmentState(model.EnrollmentStateSuspended) {
			t.Fatalf("过滤漏了行：%+v", r)
		}
	}

	// state=0 不过滤：必须连 LEFT/从未在档都一起回（运营清场看的是全量）。
	all, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{Size: 10})
	mustNoErr(t, err)
	mustEnrollSeq(t, enrollMidSeq(all.Enrollments), 100, 200, 300)
}

func TestListEnrollmentsPageSizeFoldingReachesQuery(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addEnrollment(enrSeed(100, model.EnrollmentStateEnrolled, "x"))

	cases := []struct {
		name             string
		page, size       int64
		wantPage, wantSz int64
		wantOffset       int64
	}{
		{"页宽超上限折到 MaxPageSize", 1, 99_999, 1, 100, 0},
		{"页宽 0 折到上限", 1, 0, 1, 100, 0},
		{"页宽负数折到上限", 1, -5, 1, 100, 0},
		{"页码 0 折到第一页", 0, 20, 1, 20, 0},
		{"页码负数折到第一页", -7, 20, 1, 20, 0},
		{"合法深页保留 offset", 4, 25, 4, 25, 75},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{Page: c.page, Size: c.size})
			mustNoErr(t, err)
			if reply.Page != c.wantPage || reply.Size != c.wantSz {
				t.Fatalf("回显分页错误：got page=%d size=%d want page=%d size=%d",
					reply.Page, reply.Size, c.wantPage, c.wantSz)
			}
			w := db.window("fake:enrollments.List")
			if w.offset != c.wantOffset || w.limit != c.wantSz {
				t.Fatalf("实际查询分页位错误：%+v want offset=%d limit=%d", w, c.wantOffset, c.wantSz)
			}
		})
	}
}

func TestListEnrollmentsEmptyLedgerIsNonNilSlice(t *testing.T) {
	ctx, db := newTestSvc(t)
	reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{})
	mustNoErr(t, err)
	if reply.Enrollments == nil {
		t.Fatal("空名单必须是可 range 的非 nil 空数组（区分「没有」与「没查」）")
	}
	if len(reply.Enrollments) != 0 || reply.Total != 0 {
		t.Fatalf("空台账结论错误：%+v", reply)
	}
	db.assertReads(t, 0, "fake:enrollments.List", "fake:enrollments.Count")
}

// 三类故障分开钉之「原始错误上抛」：List 与 Count 各自的失败都必须原样回给调用方，
// 不许折叠成空名单（运营据此催清场）、也不许只回前半页当成功。
func TestListEnrollmentsReadFailuresPropagateRaw(t *testing.T) {
	t.Run("列表读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		boom := errors.New("deadlock found")
		db.readFailOn["fake:enrollments.List"] = boom
		reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{})
		if !errors.Is(err, boom) {
			t.Fatalf("必须原样上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("故障不得回半截结论：%+v", reply)
		}
		db.assertReads(t, 0, "fake:enrollments.List") // Count 不该被继续调用
	})

	t.Run("计数读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		db.addEnrollment(enrSeed(100, model.EnrollmentStateEnrolled, "x"))
		boom := errors.New("read timeout")
		db.readFailOn["fake:enrollments.Count"] = boom
		reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{})
		if !errors.Is(err, boom) {
			t.Fatalf("列表成功但计数失败也必须上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("计数失败不得回带半截 total 的成功：%+v", reply)
		}
		// 轨迹必须两步齐全：只读 Count 说明漏了 List，反过来则说明吞了 Count 的失败。
		db.assertReads(t, 0, "fake:enrollments.List", "fake:enrollments.Count")
	})
}

// 现状哨兵：运营面的参与名单同样没有归属/权限位（ListEnrollmentsReq 只有 state/page/size），
// 一次调用就能翻页拿到**全平台所有作者**的 mid + 状态 + 运营备注。
// 与 GetEnrollment 的区别是量：这是可枚举的整表导出。收严位置同 README 报名域登记。
func TestListEnrollmentsExposesAllAuthorsWithoutCallerIdentity(t *testing.T) {
	ctx, db := newTestSvc(t)
	for _, mid := range []int64{100, 200, 300} {
		db.addEnrollment(enrSeed(mid, model.EnrollmentStateSuspended, "处置"))
	}
	reply, err := runListEnrollments(t, ctx, &rpc.ListEnrollmentsReq{Size: 100})
	mustNoErr(t, err)
	mustEnrollSeq(t, enrollMidSeq(reply.Enrollments), 100, 200, 300)
	for _, r := range reply.Enrollments {
		if r.Operator != txOperator || r.Remark != "处置" {
			t.Fatalf("现状：内部运营字段整体外泄：%+v", r)
		}
	}
}
