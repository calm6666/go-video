package logic

// setofficialdoclogic_test.go 覆盖 SetOfficialDoc（logic/setofficialdoclogic.go:24
// → repository/official.go:18）。
//
// 这是本批唯一会**落联系人隐私**的写入口，所以主断言是存储形态：
// OfficialExtra（联系人、联系电话、邮箱、地址、统一社会信用代码、营业执照、身份证明）
// 经 model.OfficialExtra.String()（model/official.go:125，只是 json.Marshal）
// **原样明文**写进 user_official_doc.extra（TEXT 列，见
// deploy/migrations/user-profile/000005_create_user_official.sql:64），
// 既没有脱敏也没有加密；同一份信用代码再写一次进键值表 user_official_doc_addit。
// 本文件把「明文进、明文出、两次写」钉成事实并登记 README。
//
// 其余口径：
//   - 必填守卫（model/official.go:169 Validate）发生在任何触库之前，错误是 ErrRequestErr；
//   - 状态被服务端强制成「待审核」（official.go:22），入参 state 一律作废；
//   - UPSERT 的列集里没有 reject_reason（model/official.go:204-208），所以驳回原因活下来；
//     但 extra 是**整列替换**，第二次提交没带的字段会静默消失（无字段级合并）；
//   - Role/Realname 由 int32 直接截成 int8（official.go:23、:27），259 会被当成企业认证；
//   - 附加表写失败被 logx.Errorf 吞掉（official.go:55-58），主表写失败则换成
//     ErrSubmitOfficialDocFailed 上抛，原始错误只剩日志（调用方无法分辨故障种类）。
//   - 全程不走事务、不发事件、不失效缓存：提交 ≠ 生效（生效件在 user_official，另路写）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx/logtest"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

func officialDocReq(mid int64) *rpc.OfficialDocReq {
	return &rpc.OfficialDocReq{
		Mid: mid, Name: "认证主体", State: model.OfficialStatePass, Role: model.OfficialRoleBusiness,
		Title: "认证称号", Desc: "认证描述", RejectReason: "调用方伪造的驳回原因",
		Realname: 1, Operator: "联系人甲", Telephone: docFakeTel, Email: docFakeMail,
		Address: "示例路 1 号", Company: "示例公司", CreditCode: "91310000MA1K0000XX",
		Organization: "示例机构", OrganizationType: "2", BusinessLicense: "license-token",
		BusinessScale: "大型", BusinessLevel: "A", BusinessAuth: "auth-token",
		Supplement: "补充材料", Professional: "资质证明", Identification: docFakeCard,
		SubmitSource: "android",
	}
}

// storedDoc 读回真实行的值拷贝（不存在返回 nil）。
func storedDoc(e *env, mid int64) *model.OfficialDoc { return e.st.officialDoc.get(mid) }

func TestSetOfficialDocGuardTable(t *testing.T) {
	cases := []struct {
		label string
		mut   func(*rpc.OfficialDocReq)
	}{
		{"mid=0", func(r *rpc.OfficialDocReq) { r.Mid = 0 }},
		{"mid 为负", func(r *rpc.OfficialDocReq) { r.Mid = -7 }},
		{"主体名为空", func(r *rpc.OfficialDocReq) { r.Name = "" }},
		{"role=0 未认证", func(r *rpc.OfficialDocReq) { r.Role = model.OfficialRoleUnauth }},
		{"role 为负", func(r *rpc.OfficialDocReq) { r.Role = -1 }},
		{"称号为空", func(r *rpc.OfficialDocReq) { r.Title = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			req := officialDocReq(43001)
			tc.mut(req)
			reply, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(req)
			wantErrIs(t, tc.label, err, repository.ErrRequestErr)
			wantEQ(t, tc.label, "reply 为 nil", reply == nil, true)
			wantNoCall(t, tc.label, e.st, 0) // 守卫先于主表与附加表
			wantEQ(t, tc.label, "主表行数", len(e.st.officialDoc.rows), 0)
			wantEQ(t, tc.label, "附加表行数", len(e.st.addit.rows), 0)
		})
	}

	// 守卫只看这四个字段：其余 19 个（含电话、邮箱、身份证明）一个都不校验格式。
	t.Run("必填齐备则联系方式不做任何校验", func(t *testing.T) {
		e := newEnv(t)
		req := officialDocReq(43002)
		req.Telephone, req.Email, req.CreditCode, req.Identification = "1", "不是邮箱", "x", "1"
		_, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(req)
		wantNoErr(t, "格式不校验", err)
		wantEQ(t, "格式不校验", "照样落库", storedDoc(e, 43002) != nil, true)
	})
}

func TestSetOfficialDocWritesPlaintextExtraAndForcesPendingState(t *testing.T) {
	const mid = int64(43010)
	e := newEnv(t)
	from := time.Now().Unix() - 1

	reply, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(officialDocReq(mid))
	wantNoErr(t, "首次提交", err)
	wantEQ(t, "首次提交", "EmptyReply 无字段", reply.String(), "")
	to := time.Now().Unix() + 1

	wantOps(t, "首次提交序列", e.ops(0), []string{
		"officialDoc.Upsert:43010/0", // state 被强制成待审核
		"addit.Upsert:43010/credit_code",
	})
	row := storedDoc(e, mid)
	if row == nil {
		t.Fatal("主表没有落库")
	}
	wantEQ(t, "首次提交", "state 作废入参、强制待审核", int(row.State), model.OfficialStateWait)
	wantEQ(t, "首次提交", "role", int(row.Role), model.OfficialRoleBusiness)
	wantEQ(t, "首次提交", "name", row.Name, "认证主体")
	wantEQ(t, "首次提交", "title", row.Title, "认证称号")
	wantEQ(t, "首次提交", "desc", row.Desc, "认证描述")
	wantEQ(t, "首次提交", "submit_source", row.SubmitSource, "android")
	wantTSWindow(t, "首次提交", "submit_time 由服务端写当前秒", row.SubmitTime, from, to)

	// 存储形态（本用例的核心）：extra 列就是明文键值对，敏感原文可被直接 grep。
	// 现状哨兵（README 已知缺口，隐私类，改生产代码前不要动这两行的期望）：
	// 修法方向是落库前对 telephone/email/identification 脱敏或加密，列内只留摘要/掩码。
	for _, want := range []string{
		`"telephone":"` + docFakeTel + `"`,
		`"identification":"` + docFakeCard + `"`,
		`"email":"` + docFakeMail + `"`,
		`"credit_code":"91310000MA1K0000XX"`,
	} {
		if !strings.Contains(row.Extra, want) {
			t.Errorf("首次提交：extra 列应含 %s（现状为明文入库），实际=%s", want, row.Extra)
		}
	}
	// 入参里的 reject_reason 不参与写列（主表新行为空，附加表也无此列）。
	wantEQ(t, "首次提交", "reject_reason 不由提交方写", row.RejectReason, "")

	// 读侧闭环：同一份明文从 OfficialDoc 原样端出来。
	doc, err := NewOfficialDocLogic(context.Background(), e.svcCtx).OfficialDoc(&rpc.MidReq{Mid: mid})
	wantNoErr(t, "首次提交后读回", err)
	wantEQ(t, "首次提交后读回", "telephone 明文出口", doc.GetTelephone(), docFakeTel)
	wantEQ(t, "首次提交后读回", "identification 明文出口", doc.GetIdentification(), docFakeCard)
	wantEQ(t, "首次提交后读回", "state 已是待审核", doc.GetState(), int32(model.OfficialStateWait))
}

// TestSetOfficialDocPreservesRejectReasonButReplacesExtra
// 重复提交的两种相反命运：驳回原因活着（UPSERT 列集不含它），
// 而 extra 整列被替换（第二次没带的字段静默消失）——没有第二行，也没有字段级合并。
func TestSetOfficialDocPreservesRejectReasonButReplacesExtra(t *testing.T) {
	const mid = int64(43011)
	e := newEnv(t)
	e.st.officialDoc.rows[mid] = &model.OfficialDoc{
		Mid: mid, Name: "旧主体", State: model.OfficialStateNoPass, Role: model.OfficialRoleUp,
		Title: "旧称号", Desc: "旧描述", RejectReason: "材料不全",
		Extra: `{"telephone":"13700000000","operator":"旧联系人"}`, SubmitSource: "ios", SubmitTime: 1,
	}

	req := officialDocReq(mid)
	req.CreditCode = "" // 第二次不带信用代码以外的差异，先看 extra 覆盖
	if _, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(req); err != nil {
		t.Fatalf("第二次提交：%v", err)
	}

	row := storedDoc(e, mid)
	wantEQ(t, "重复提交", "仍然一行（mid 主键 UPSERT）", len(e.st.officialDoc.rows), 1)
	wantEQ(t, "重复提交", "驳回原因被保留", row.RejectReason, "材料不全")
	wantEQ(t, "重复提交", "状态被打回待审核", int(row.State), model.OfficialStateWait)
	wantEQ(t, "重复提交", "主体名被覆盖", row.Name, "认证主体")
	if strings.Contains(row.Extra, "13700000000") || strings.Contains(row.Extra, "旧联系人") {
		t.Errorf("重复提交：extra 应整列替换（旧电话/旧联系人不该残留），实际=%s", row.Extra)
	}
	wantEQ(t, "重复提交", "新电话已写入", strings.Contains(row.Extra, docFakeTel), true)

	// 信用代码为空 → 附加表一次都不碰（主表 extra 里也确实是空串）。
	wantNoOpsWith(t, "信用代码为空", e.ops(0), "addit.")
	wantEQ(t, "信用代码为空", "主表 extra 里信用代码是空串",
		strings.Contains(row.Extra, `"credit_code":""`), true)
}

func TestSetOfficialDocCreditCodeDualWrite(t *testing.T) {
	const mid = int64(43012)
	e := newEnv(t)
	if _, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(officialDocReq(mid)); err != nil {
		t.Fatalf("提交：%v", err)
	}
	wantEQ(t, "信用代码双写", "附加表值", e.st.addit.value(mid, "credit_code"), "91310000MA1K0000XX")
	wantEQ(t, "信用代码双写", "property 键固定", e.st.log.countPrefix("addit.Upsert:"), 1)

	// 再提交一次不同的信用代码：两处都要跟着变（键值表是 UPSERT，不是第二行）。
	req := officialDocReq(mid)
	req.CreditCode = "91310000MA1K9999YY"
	e.st.log.reset()
	if _, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(req); err != nil {
		t.Fatalf("第二次提交：%v", err)
	}
	wantOps(t, "信用代码双写", e.ops(0), []string{
		"officialDoc.Upsert:43012/0", "addit.Upsert:43012/credit_code",
	})
	wantEQ(t, "信用代码双写", "附加表覆盖成新值", e.st.addit.value(mid, "credit_code"), "91310000MA1K9999YY")
	wantEQ(t, "信用代码双写", "附加表 mid 数", len(e.st.addit.rows), 1)
	wantEQ(t, "信用代码双写", "主表 extra 同步成新值",
		strings.Contains(storedDoc(e, mid).Extra, "MA1K9999YY"), true)
}

// TestSetOfficialDocRoleAndRealnameTruncateToInt8 钉 int32→int8 的静默截断：
// official.go:23 的 int8(req.Role) 与 :27 的 int8(req.Realname) 都不做范围校验，
// 于是越界值会**换成另一个值**通过或落库（259 变成合法的「企业认证」）。
func TestSetOfficialDocRoleAndRealnameTruncateToInt8(t *testing.T) {
	cases := []struct {
		label      string
		role       int32
		realname   int32
		wantReject bool
		wantRole   int8
		wantReal   int32
	}{
		{"role=259 被当成企业认证(3)", 259, 1, false, model.OfficialRoleBusiness, 1},
		{"role=256 被当成未认证(0) → 守卫拒", 256, 1, true, 0, 0},
		{"realname=300 被存成 44", model.OfficialRoleMedia, 300, false, model.OfficialRoleMedia, 44},
		{"realname=-1 原样交驱动（JSON 里是 -1）", model.OfficialRoleMedia, -1, false, model.OfficialRoleMedia, -1},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			const mid = int64(43013)
			e := newEnv(t)
			req := officialDocReq(mid)
			req.Role, req.Realname = tc.role, tc.realname
			_, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(req)
			if tc.wantReject {
				wantErrIs(t, tc.label, err, repository.ErrRequestErr)
				wantNoCall(t, tc.label, e.st, 0)
				return
			}
			wantNoErr(t, tc.label, err)
			wantEQ(t, tc.label, "落库 role", storedDoc(e, mid).Role, tc.wantRole)
			doc, derr := NewOfficialDocLogic(context.Background(), e.svcCtx).OfficialDoc(&rpc.MidReq{Mid: mid})
			wantNoErr(t, tc.label+" 读回", derr)
			wantEQ(t, tc.label, "出口 realname", doc.GetRealname(), tc.wantReal)
		})
	}
}

func TestSetOfficialDocDownstreamFailures(t *testing.T) {
	t.Run("主表写失败：换成 ErrSubmitOfficialDocFailed，原始错误只剩日志", func(t *testing.T) {
		e := newEnv(t)
		boom := errors.New("Error 1406: Data too long for column 'extra'")
		e.st.officialDoc.failWith("Upsert", boom)
		logs := logtest.NewCollector(t)

		reply, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(officialDocReq(43020))
		wantErrIs(t, "主表写失败", err, repository.ErrSubmitOfficialDocFailed)
		if errors.Is(err, boom) {
			t.Errorf("主表写失败：错误链保住了原始 SQL 错误（与现状「换成不透明哨兵」不符，请同步更新用例）")
		}
		wantEQ(t, "主表写失败", "reply 为 nil", reply == nil, true)
		// 主表失败即中止：附加表一次都不碰（两处写不是一个事务，靠顺序保证不半写）。
		wantOps(t, "主表写失败", e.ops(0), []string{"officialDoc.Upsert:43020/0"})
		wantEQ(t, "主表写失败", "主表无行", len(e.st.officialDoc.rows), 0)
		// 先证明捕获通道在工作，再断言原始错误只活在日志里。
		if !strings.Contains(logs.String(), "Data too long") {
			t.Fatalf("日志捕获通道失效，没收到原始 SQL 错误：%q", logs.String())
		}
		if strings.Contains(logs.String(), docFakeTel) {
			t.Errorf("错误日志泄漏了联系电话明文：%q", logs.String())
		}
	})

	t.Run("附加表写失败：被吞，接口仍成功且信用代码只剩 extra 一份", func(t *testing.T) {
		e := newEnv(t)
		e.st.addit.failWith("Upsert", errors.New("boom: user_official_doc_addit 写不进去"))
		logs := logtest.NewCollector(t)

		reply, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(officialDocReq(43021))
		wantNoErr(t, "附加表写失败被吞", err)
		wantEQ(t, "附加表写失败被吞", "reply 非空", reply != nil, true)
		wantOps(t, "附加表写失败被吞", e.ops(0), []string{
			"officialDoc.Upsert:43021/0", "addit.Upsert:43021/credit_code",
		})
		wantEQ(t, "附加表写失败被吞", "附加表没有值", e.st.addit.value(43021, "credit_code"), "")
		wantEQ(t, "附加表写失败被吞", "主表已落库", storedDoc(e, 43021) != nil, true)
		if !strings.Contains(logs.String(), "upsert addit") {
			t.Fatalf("日志捕获通道失效，没收到附加表失败那行：%q", logs.String())
		}
	})
}

// TestSetOfficialDocTouchesNothingElse 提交既不开事务、不发领域事件、也不动缓存与生效件：
// 「提交 ≠ 生效」在本服务里是**没有留下任何待办**的事实，审核方只能靠轮询主表发现新单。
func TestSetOfficialDocTouchesNothingElse(t *testing.T) {
	const mid = int64(43022)
	e := newEnv(t)
	// 这里刻意**不**用 newEnv(t, withOfficial(...)) 布生效件：storeOpt 在 Repository 构造前生效，
	// 而 NewWithDeps（repository/repository.go:165-168）带着空的 officials 快照调 loadOfficial，
	// :210-214 会把快照里每条都判成「新增」并 enqueueProfileUpdated —— 装配本身就吃掉 1 次事务
	// + 1 行 Outbox，本用例要断言的「提交不留待办」会被这条副作用污染。改用静默 put 布同一场景。
	e.st.official.put(mid, &model.OfficialInfo{Role: model.OfficialRoleUp, Title: "生效称号", Desc: "生效描述"})
	e.st.log.reset()

	if _, err := NewSetOfficialDocLogic(context.Background(), e.svcCtx).SetOfficialDoc(officialDocReq(mid)); err != nil {
		t.Fatalf("提交：%v", err)
	}
	wantEQ(t, "提交的影响面", "事务次数", e.st.conn.transactions, 0)
	wantEQ(t, "提交的影响面", "Outbox 行数", e.st.outbox.count(), 0)
	wantEQ(t, "提交的影响面", "缓存调用次数", e.st.log.countPrefix("cache."), 0)
	wantNoOpsWith(t, "提交的影响面", e.ops(0), "official.")
	wantNoOpsWith(t, "提交的影响面", e.ops(0), "base.")

	// 提交只落「单证表」，生效件表一行都没动、也没被读：认证不会因提交而生效，
	// 也不会通知 account 清缓存。
	wantEQ(t, "提交的影响面", "生效件仍是提交前那条", e.st.official.rows[mid].Title, "生效称号")
	wantEQ(t, "提交的影响面", "生效件行数未被追加", len(e.st.official.rows), 1)
}
