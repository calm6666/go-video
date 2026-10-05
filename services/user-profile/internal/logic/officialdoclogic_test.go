package logic

// officialdoclogic_test.go 覆盖 OfficialDoc（logic/officialdoclogic.go:24
// → repository/official.go:63）。
//
// 这个方法是官方认证「提交件」的唯一读出口，所以要钉的是**出口形态**：
//   - 认证附加资料（联系人、联系电话、邮箱、地址、统一社会信用代码、营业执照、身份证明）
//     存在 user_official_doc.extra 这一列的**明文 JSON** 里（model/official.go:125 只做
//     json.Marshal，无脱敏、无加密），OfficialDoc 再逐字段原样端出去（official.go:81-95）。
//     本文件把这条明文链钉成可执行事实，并在 README 已知缺口里登记；
//   - 与之相反，生效认证表 user_official 与内存快照完全不在这条链路上
//     （两张表、两套语义：提交件 vs 生效件），所以序列里一次 official.* 都不许出现；
//   - 这条链路**没有缓存**（不像 Base/Moral），每次调用一次 SELECT，重复调用不合并；
//   - 没有 mid<=0 守卫：mid=0 与负 mid 照查，查不到就是 ErrNoOfficialDoc。
//
// extra 的解析失败是**降级**不是错误（model/official.go:134 ParseExtra 吞掉 Unmarshal 错误）：
// 列被写坏时接口仍 200，只是 16 个附加字段全空——调用方无法与「用户没填」区分。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// 明显的假值（隐私红线：不用任何真实号码/邮箱/证件号）。
const (
	docFakeTel  = "13800000000"
	docFakeCard = "000000000000000000"
	docFakeMail = "seed-contact@example.invalid"
)

// docExtra 是 OfficialExtra.String() 的同源构造：生产写入口就用它拼 extra 列，
// 布景必须同口径，否则解出来的字段全零、断言会变成测替身。
func docExtra() model.OfficialExtra {
	return model.OfficialExtra{
		Realname:         1,
		Operator:         "联系人甲",
		Telephone:        docFakeTel,
		Email:            docFakeMail,
		Address:          "示例路 1 号",
		Company:          "示例公司",
		CreditCode:       "91310000MA1K0000XX",
		Organization:     "示例机构",
		OrganizationType: "2",
		BusinessLicense:  "license-token",
		BusinessScale:    "大型",
		BusinessLevel:    "A",
		BusinessAuth:     "auth-token",
		Supplement:       "补充材料",
		Professional:     "资质证明",
		Identification:   docFakeCard,
	}
}

func seedDoc(e *env, mid int64, state, role int8, extra string) *model.OfficialDoc {
	doc := &model.OfficialDoc{
		Mid: mid, Name: "认证主体", State: state, Role: role, Title: "认证称号",
		Desc: "认证描述", RejectReason: "材料不全", Extra: extra,
		SubmitSource: "android", SubmitTime: 1700000000,
	}
	e.st.officialDoc.rows[mid] = doc
	return doc
}

func TestOfficialDocProjectsEveryFieldFromPlaintextExtra(t *testing.T) {
	const mid = int64(42001)
	e := newEnv(t)
	seedDoc(e, mid, model.OfficialStateNoPass, model.OfficialRoleBusiness, docExtra().String())

	l := NewOfficialDocLogic(context.Background(), e.svcCtx)
	reply, err := l.OfficialDoc(&rpc.MidReq{Mid: mid})
	wantNoErr(t, "认证文档查询", err)
	// 表列直接映射
	wantEQ(t, "认证文档查询", "mid", reply.GetMid(), mid)
	wantEQ(t, "认证文档查询", "name", reply.GetName(), "认证主体")
	wantEQ(t, "认证文档查询", "state（不裁剪 0/1/2/3 以外的值）", reply.GetState(), int32(model.OfficialStateNoPass))
	wantEQ(t, "认证文档查询", "role", reply.GetRole(), int32(model.OfficialRoleBusiness))
	wantEQ(t, "认证文档查询", "title", reply.GetTitle(), "认证称号")
	wantEQ(t, "认证文档查询", "desc", reply.GetDesc(), "认证描述")
	wantEQ(t, "认证文档查询", "reject_reason", reply.GetRejectReason(), "材料不全")
	// extra JSON 里的 16 个字段逐个回读（少映射一个就是空串，立刻红）
	wantEQ(t, "认证文档查询", "realname", reply.GetRealname(), int32(1))
	wantEQ(t, "认证文档查询", "operator", reply.GetOperator(), "联系人甲")
	wantEQ(t, "认证文档查询", "address", reply.GetAddress(), "示例路 1 号")
	wantEQ(t, "认证文档查询", "company", reply.GetCompany(), "示例公司")
	wantEQ(t, "认证文档查询", "credit_code", reply.GetCreditCode(), "91310000MA1K0000XX")
	wantEQ(t, "认证文档查询", "organization", reply.GetOrganization(), "示例机构")
	wantEQ(t, "认证文档查询", "organization_type", reply.GetOrganizationType(), "2")
	wantEQ(t, "认证文档查询", "business_license", reply.GetBusinessLicense(), "license-token")
	wantEQ(t, "认证文档查询", "business_scale", reply.GetBusinessScale(), "大型")
	wantEQ(t, "认证文档查询", "business_level", reply.GetBusinessLevel(), "A")
	wantEQ(t, "认证文档查询", "business_auth", reply.GetBusinessAuth(), "auth-token")
	wantEQ(t, "认证文档查询", "supplement", reply.GetSupplement(), "补充材料")
	wantEQ(t, "认证文档查询", "professional", reply.GetProfessional(), "资质证明")

	// 现状哨兵（缺陷，README 已知缺口）：联系方式与身份证明是**明文**出口。
	// 修法方向：extra 落库前对 telephone/email/identification 做脱敏或加密，
	// 或在 OfficialDoc 出口按调用方角色裁剪；改生产代码前不要动这两行的期望。
	wantEQ(t, "认证文档查询", "telephone 明文回吐", reply.GetTelephone(), docFakeTel)
	wantEQ(t, "认证文档查询", "identification 明文回吐", reply.GetIdentification(), docFakeCard)
	// 存储形态：读回真实行，extra 列里就是可 grep 的明文键值对（不是摘要、不是密文）。
	stored := e.st.officialDoc.get(mid)
	if stored == nil {
		t.Fatal("布景行不见了")
	}
	if !strings.Contains(stored.Extra, `"telephone":"`+docFakeTel+`"`) ||
		!strings.Contains(stored.Extra, `"identification":"`+docFakeCard+`"`) {
		t.Errorf("期望 extra 列含明文联系字段，实际=%s", stored.Extra)
	}

	// 出口只到 extra 的 16 个字段：submit_source / submit_time 读了列但没有出口字段。
	wantNoPII(t, "认证文档查询：提交来源不该出现在出口", []string{"android"},
		map[string]string{"reply": reply.String()})
	if !strings.Contains(reply.String(), docFakeTel) {
		t.Errorf("现状哨兵：reply 的字符串形态里应能看到明文电话，实际=%s", reply.String())
	}

	// 一次 SELECT，不碰缓存、不碰生效认证表。
	wantOps(t, "认证文档查询序列", e.ops(0), []string{"officialDoc.FindOne:42001"})
	wantNoOpsWith(t, "认证文档查询不得读快照", e.ops(0), "official.")
	wantNoOpsWith(t, "认证文档查询不得走缓存", e.ops(0), "cache.")
}

// TestOfficialDocMissingRowAndBadExtra 三种「查不到」的口径分开钉：
// 行不存在是哨兵错误；行存在但 extra 坏掉是降级（无错误、附加字段全空）。
func TestOfficialDocMissingRowAndBadExtra(t *testing.T) {
	t.Run("行不存在：ErrNoOfficialDoc，不是空 reply", func(t *testing.T) {
		e := newEnv(t)
		reply, err := NewOfficialDocLogic(context.Background(), e.svcCtx).OfficialDoc(&rpc.MidReq{Mid: 42002})
		wantErrIs(t, "无认证文档", err, repository.ErrNoOfficialDoc)
		wantEQ(t, "无认证文档", "reply 为 nil", reply == nil, true)
		wantOps(t, "无认证文档序列", e.ops(0), []string{"officialDoc.FindOne:42002"})
	})

	t.Run("extra 是坏 JSON：降级成 16 个零值且不报错", func(t *testing.T) {
		const mid = int64(42003)
		e := newEnv(t)
		seedDoc(e, mid, model.OfficialStateWait, model.OfficialRoleUp, `{"telephone": "不是对象结尾}`)
		reply, err := NewOfficialDocLogic(context.Background(), e.svcCtx).OfficialDoc(&rpc.MidReq{Mid: mid})
		wantNoErr(t, "坏 extra 降级", err)
		wantEQ(t, "坏 extra 降级", "表列照常返回", reply.GetName(), "认证主体")
		wantEQ(t, "坏 extra 降级", "电话退化成空", reply.GetTelephone(), "")
		wantEQ(t, "坏 extra 降级", "身份证明退化成空", reply.GetIdentification(), "")
		wantEQ(t, "坏 extra 降级", "realname 退化成 0", reply.GetRealname(), int32(0))
		wantNoPII(t, "坏 extra 降级", []string{docFakeTel}, map[string]string{"reply": reply.String()})
	})

	t.Run("extra 是未知键：一律忽略", func(t *testing.T) {
		const mid = int64(42004)
		e := newEnv(t)
		seedDoc(e, mid, model.OfficialStateWait, model.OfficialRoleUp, `{"unexpected":"x","telephone":"13900000000"}`)
		reply, err := NewOfficialDocLogic(context.Background(), e.svcCtx).OfficialDoc(&rpc.MidReq{Mid: mid})
		wantNoErr(t, "未知键", err)
		wantEQ(t, "未知键", "已知键仍解析", reply.GetTelephone(), "13900000000")
	})

	t.Run("读库失败：错误原样上抛（不被换成 ErrNoOfficialDoc）", func(t *testing.T) {
		e := newEnv(t)
		boom := errors.New("boom: user_official_doc 读不到")
		e.st.officialDoc.failWith("FindOne", boom)
		reply, err := NewOfficialDocLogic(context.Background(), e.svcCtx).OfficialDoc(&rpc.MidReq{Mid: 42005})
		wantErrIs(t, "读库失败", err, boom)
		wantEQ(t, "读库失败", "reply 为 nil", reply == nil, true)
	})
}

// TestOfficialDocHasNoCacheAndNoMidGuard 与其它读接口对照：这条链路没有缓存层，
// 也没有 mid<=0 守卫，重复调用就是重复 SELECT（每次一笔都打到库）。
func TestOfficialDocHasNoCacheAndNoMidGuard(t *testing.T) {
	e := newEnv(t)
	l := NewOfficialDocLogic(context.Background(), e.svcCtx)

	for i := 0; i < 2; i++ {
		_, err := l.OfficialDoc(&rpc.MidReq{Mid: 0})
		wantErrIs(t, "mid=0 仍然查库", err, repository.ErrNoOfficialDoc)
	}
	_, err := l.OfficialDoc(&rpc.MidReq{Mid: -7})
	wantErrIs(t, "负 mid 仍然查库", err, repository.ErrNoOfficialDoc)
	wantOps(t, "三次无缓存查询", e.ops(0), []string{
		"officialDoc.FindOne:0", "officialDoc.FindOne:0", "officialDoc.FindOne:-7",
	})
	wantEQ(t, "无缓存", "缓存调用次数", e.st.log.countPrefix("cache."), 0)
}
