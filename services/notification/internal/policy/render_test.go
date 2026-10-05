package policy

import (
	"errors"
	"strings"
	"testing"
)

// 模板渲染与注入防护（AGENTS.md §9：涉及内容投递必须有注入防护测试）。

func TestRenderReplacesVars(t *testing.T) {
	got, err := Render("你好 {{nick_name}}", "订单 {{order_no}} 已发货", map[string]string{
		"nick_name": "小明",
		"order_no":  "SO-1",
	})
	if err != nil {
		t.Fatalf("Render 失败: %v", err)
	}
	if got.Title != "你好 小明" {
		t.Errorf("标题 = %q", got.Title)
	}
	if got.Body != "订单 SO-1 已发货" {
		t.Errorf("正文 = %q", got.Body)
	}
}

// TestRenderMissingVar：变量缺失必须显式报错并给出清单，
// 绝不能把 "{{order_no}}" 这类残片发给终端用户。
func TestRenderMissingVar(t *testing.T) {
	got, err := Render("通知", "订单 {{order_no}} 由 {{courier}} 配送", map[string]string{"order_no": "SO-1"})
	if !errors.Is(err, ErrRenderMissingVar) {
		t.Fatalf("want ErrRenderMissingVar, got %v", err)
	}
	if got != nil {
		t.Fatal("渲染失败时不得返回结果")
	}
	if !strings.Contains(err.Error(), "courier") {
		t.Errorf("错误信息应包含缺失变量名，got %v", err)
	}
	if strings.Contains(err.Error(), "order_no") {
		t.Errorf("错误信息不应包含已提供的变量，got %v", err)
	}
}

// TestMissingVars 供 RenderTemplate 预览接口回传，不视为错误。
func TestMissingVars(t *testing.T) {
	miss := MissingVars("{{a}}", "{{b}} 和 {{c}} 再 {{b}}", map[string]string{"b": "x"})
	if strings.Join(miss, ",") != "a,c" {
		t.Errorf("MissingVars = %v, want [a c]", miss)
	}
	if len(MissingVars("{{a}}", "b", map[string]string{"a": "1"})) != 0 {
		t.Error("无缺失变量时应返回空集")
	}
}

// TestRenderNoSecondPassInjection：变量值里的占位符不得被二次解析。
// 攻击面：调用方把 "{{secret_note}}" 当作变量值传进来，期望渲染器二次替换越权取数。
func TestRenderNoSecondPassInjection(t *testing.T) {
	_, err := Render("通知", "内容 {{user_text}}", map[string]string{"user_text": "{{secret_note}}"})
	// 入口校验会先拒绝控制字符；即便绕过 ValidateParams，Render 也不得替换二级占位符。
	if err == nil {
		t.Fatal("参数含 {{ }} 时必须报错")
	}
	if !errors.Is(err, ErrRenderUnknownVar) {
		t.Fatalf("want ErrRenderUnknownVar, got %v", err)
	}
}

// TestRenderValueNotTreatedAsTemplate：即便变量值形似模板片段，输出也只是字面量文本。
func TestRenderValueNotTreatedAsTemplate(t *testing.T) {
	got, err := Render("标题", "正文 {{text}}", map[string]string{"text": "字面量 {not a var}"})
	if err != nil {
		t.Fatalf("Render 失败: %v", err)
	}
	if got.Body != "正文 字面量 {not a var}" {
		t.Errorf("正文被改写了: %q", got.Body)
	}
}

// TestTemplateCannotChangeChannelOrRecipient：
// 渲染函数只产出标题与正文，通道与收件人由请求决定；
// 任何试图通过参数名（channel/target/recipient/mid）或模板变量名影响投递目标的写法都要被挡下。
func TestTemplateCannotChangeChannelOrRecipient(t *testing.T) {
	for _, k := range []string{"channel", "target", "target_ref", "recipient", "recipients", "mid"} {
		if err := ValidateParams(map[string]string{k: "sms"}); !errors.Is(err, ErrSensitiveParam) {
			t.Errorf("参数名 %q 应被拒绝，got %v", k, err)
		}
	}
	// 模板里写 {{channel}} 本身合法（只是取不到值），但因为没有 channel 形参，
	// 渲染结果无法改变通道：缺变量直接失败。
	if _, err := Render("t", "把通道改成 {{channel}}", map[string]string{}); !errors.Is(err, ErrRenderMissingVar) {
		t.Errorf("模板无法读取通道，应因缺变量失败，got %v", err)
	}
	// Render 的返回值只有 Title/Body 两个字段，不存在可改写收件人的出口。
	res, err := Render("t", "正文", nil)
	if err != nil || res == nil {
		t.Fatalf("无变量模板应渲染成功: %v", err)
	}
}

// TestValidateParamsSensitiveValue：明文手机号/邮箱不得进入模板参数。
func TestValidateParamsSensitiveValue(t *testing.T) {
	cases := map[string]string{
		"nick":    "联系我 13800001111 谢谢",
		"note":    "发到 someone@example.com",
		"long":    strings.Repeat("a", maxValueLen+1),
		"control": "前缀 {{x}} 后缀",
	}
	for k, v := range cases {
		if err := ValidateParams(map[string]string{k: v}); err == nil {
			t.Errorf("参数 %q=%q 应被拒绝", k, v)
		}
	}
	if err := ValidateParams(map[string]string{"nick": "小明", "amount": "12.50"}); err != nil {
		t.Errorf("正常参数被误伤: %v", err)
	}
	// 11 位数字但夹在长数字串中不算手机号（避免误杀订单号）。
	if err := ValidateParams(map[string]string{"order": "202413800001111222"}); err != nil {
		t.Errorf("订单号被误判为手机号: %v", err)
	}
}

// TestCheckTemplateMalformed：畸形/嵌套占位符与多行标题必须拒绝入库。
func TestCheckTemplateMalformed(t *testing.T) {
	cases := []struct {
		name            string
		title, body     string
		wantErrUnwrapTo error
	}{
		{"空正文", "标题", "  ", ErrInvalidTemplate},
		{"半截占位符", "标题", "订单 {{order_no} 已发货", ErrInvalidTemplate},
		{"多余右花括号", "标题", "正常 {{a}} 尾巴 }}", ErrInvalidTemplate},
		{"嵌套占位符", "标题", "{{ {{a}} }}", ErrInvalidTemplate},
		{"大写变量名", "标题", "{{OrderNo}}", ErrInvalidTemplate},
		{"标题换行", "第一行\n第二行", "正文", ErrInvalidTemplate},
		{"回车符", "标题", "正文\r\n尾巴", ErrInvalidTemplate},
		{"正文超长", "标题", strings.Repeat("啊", maxBodyLen), ErrInvalidTemplate},
	}
	for _, c := range cases {
		if err := CheckTemplate(c.title, c.body); !errors.Is(err, c.wantErrUnwrapTo) {
			t.Errorf("%s: want %v, got %v", c.name, c.wantErrUnwrapTo, err)
		}
	}
	if err := CheckTemplate("审核通过", "昵称 {{nick}} 的作品已过审"); err != nil {
		t.Errorf("合法模板被拒: %v", err)
	}
}

// TestRenderTooLong：模板本身合法、但变量值撑爆长度上限时不能静默截断后发送。
// （模板自身超长在 CheckTemplate 阶段就以 ErrInvalidTemplate 拒绝，见上表。）
func TestRenderTooLong(t *testing.T) {
	long := strings.Repeat("a", maxValueLen) // 512 字节，单值合法
	if _, err := Render("{{a}}", "正文", map[string]string{"a": long}); !errors.Is(err, ErrRenderTooLong) {
		t.Errorf("渲染后标题超出 255 字节应报错, got %v", err)
	}
	body := strings.Repeat("{{a}}", 8) // 模板 40 字节合法，渲染后 4096 字节
	if _, err := Render("标题", body, map[string]string{"a": long}); !errors.Is(err, ErrRenderTooLong) {
		t.Errorf("渲染后正文超出 4000 字节应报错, got %v", err)
	}
	// 边界：刚好不超过则成功。
	if _, err := Render("{{a}}", "正文", map[string]string{"a": strings.Repeat("a", maxTitleLen)}); err != nil {
		t.Errorf("刚好 255 字节应通过: %v", err)
	}
}

func TestTemplateVarsSortedDeduped(t *testing.T) {
	got := TemplateVars("{{b}} {{a}}", "{{a}} 和 {{c}}")
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("TemplateVars = %v, want [a b c]", got)
	}
}
