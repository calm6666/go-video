package model

// 本文件覆盖 model 包的等级推导、性别映射、标志位与官方认证辅助逻辑。
// 规则与参考仓库 member 服务 model 包保持一致。

import "testing"

// TestBuildLevel 覆盖 0-6 级与满级的全部阈值分支（参考 model.BuildLevel）。
func TestBuildLevel(t *testing.T) {
	cases := []struct {
		exp     int64
		cur     int32
		min     int32
		nextExp int32
	}{
		{exp: 0, cur: 0, min: 0, nextExp: 1},
		{exp: 1 * ExpMulti, cur: 1, min: 1, nextExp: 200},
		{exp: 199 * ExpMulti, cur: 1, min: 1, nextExp: 200},
		{exp: 200 * ExpMulti, cur: 2, min: 200, nextExp: 1500},
		{exp: 1500 * ExpMulti, cur: 3, min: 1500, nextExp: 4500},
		{exp: 4500 * ExpMulti, cur: 4, min: 4500, nextExp: 10800},
		{exp: 10800 * ExpMulti, cur: 5, min: 10800, nextExp: 28800},
		{exp: 28800 * ExpMulti, cur: 6, min: 28800, nextExp: LevelMax},
		{exp: 99999999 * ExpMulti, cur: 6, min: 28800, nextExp: LevelMax},
	}
	for _, c := range cases {
		cur, min, nowExp, nextExp := BuildLevel(c.exp, true)
		if cur != c.cur || min != c.min || nextExp != c.nextExp {
			t.Errorf("BuildLevel(%d) = (%d,%d,%d,%d), want cur=%d min=%d next=%d",
				c.exp, cur, min, nowExp, nextExp, c.cur, c.min, c.nextExp)
		}
		// sexp=true 时 now_exp 应为折算后的当前经验
		if want := int32(c.exp / ExpMulti); nowExp != want {
			t.Errorf("BuildLevel(%d, true) nowExp = %d, want %d", c.exp, nowExp, want)
		}
		// sexp=false 时 now_exp 应为 0
		if _, _, nowExp0, _ := BuildLevel(c.exp, false); nowExp0 != 0 {
			t.Errorf("BuildLevel(%d, false) nowExp = %d, want 0", c.exp, nowExp0)
		}
	}
}

// TestSexStr 覆盖数字性别到展示字符串的映射（参考 BaseInfo.SexStr）。
func TestSexStr(t *testing.T) {
	cases := map[int64]string{0: "保密", 1: "男", 2: "女", 9: "保密"}
	for in, want := range cases {
		if got := SexStr(in); got != want {
			t.Errorf("SexStr(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestUserFlag 覆盖标志位工具函数（参考 model/user_flag.go）。
func TestUserFlag(t *testing.T) {
	var flag uint
	flag = SetAttr(flag, NickUpdated)
	if !HasAttr(flag, NickUpdated) {
		t.Fatal("HasAttr(NickUpdated) = false after SetAttr")
	}
	if HasAttr(flag, uint(2)) {
		t.Fatal("HasAttr(2) = true, want false")
	}
}

// TestOfficialExtraRoundtrip 覆盖附加资料 JSON 序列化往返。
func TestOfficialExtraRoundtrip(t *testing.T) {
	extra := OfficialExtra{
		Realname: 1, Operator: "张三", Telephone: "13800000000", Email: "a@b.c",
		Address: "上海", Company: "示例公司", CreditCode: "91310000XXXXXXXXXX",
		Organization: "", BusinessLicense: "license", BusinessScale: "50-100人",
		BusinessLevel: "A", BusinessAuth: "auth", Supplement: "{}",
		Professional: "cert", Identification: "id", OfficialSite: "https://x.example",
		RegisteredCapital: "100万",
	}
	got := ParseExtra(extra.String())
	if got != extra {
		t.Errorf("ParseExtra(String()) = %+v, want %+v", got, extra)
	}
	// 空字符串解析为零值
	if z := ParseExtra(""); z != (OfficialExtra{}) {
		t.Errorf("ParseExtra(\"\") = %+v, want zero", z)
	}
}

// TestOfficialDocValidate 覆盖认证文档必填校验（参考 OfficialDoc.Validate）。
func TestOfficialDocValidate(t *testing.T) {
	valid := &OfficialDoc{Mid: 1, Name: "主体", Role: OfficialRoleUp, Title: "UP主"}
	if !valid.Validate() {
		t.Fatal("valid doc rejected")
	}
	invalid := []*OfficialDoc{
		nil,
		{Mid: 0, Name: "x", Role: 1, Title: "t"},
		{Mid: 1, Name: "", Role: 1, Title: "t"},
		{Mid: 1, Name: "x", Role: 0, Title: "t"},
		{Mid: 1, Name: "x", Role: 1, Title: ""},
	}
	for i, d := range invalid {
		if d.Validate() {
			t.Errorf("invalid doc #%d accepted", i)
		}
	}
}
