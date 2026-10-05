package policy

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 模板渲染策略（AGENTS.md §9：涉及内容投递必须有注入防护测试）。
//
// 防护目标：
//  1. 变量缺失必须显式报错，不允许把 “{{nick_name}” 之类的残片发给用户；
//  2. 变量值不得再次被当作模板解析（单次替换），因此参数无法注入新的占位符；
//  3. 模板与参数都不能改变通道或收件人 —— 渲染函数签名里就没有 channel/target 参数，
//     通道与收件人由调用请求决定，渲染只产出标题与正文文本；
//  4. 参数不得携带明文手机号/邮箱/证件号（docs/api-and-events.md §4 隐私约束）。
var (
	// ErrRenderMissingVar 模板存在未提供的变量。
	ErrRenderMissingVar = errors.New("notification/policy: template variables missing")
	// ErrRenderUnknownVar 模板使用了未知占位符（含嵌套/畸形写法）。
	ErrRenderUnknownVar = errors.New("notification/policy: unknown template placeholder")
	// ErrRenderTooLong 渲染结果超出长度限制。
	ErrRenderTooLong = errors.New("notification/policy: rendered content too long")
	// ErrSensitiveParam 参数键或值疑似明文敏感信息。
	ErrSensitiveParam = errors.New("notification/policy: sensitive value is not allowed in template_params")
	// ErrInvalidTemplate 模板本身不合法。
	ErrInvalidTemplate = errors.New("notification/policy: invalid template")
)

const (
	// maxTitleLen 渲染标题最大长度（对齐 notification_template.title_tpl VARCHAR(255)）。
	maxTitleLen = 255
	// maxBodyLen 渲染正文最大长度。
	maxBodyLen = 4000
	// maxValueLen 单个变量值最大长度。
	maxValueLen = 512
)

// placeholderRe 只接受 {{ 小写字母/数字/下划线 }} 形式的占位符。
var placeholderRe = regexp.MustCompile(`\{\{([a-z0-9_]{1,64})\}\}`)

// anyBraceRe 用于发现畸形或注入式占位符。
var anyBraceRe = regexp.MustCompile(`\{\{|\}\}`)

// forbiddenParamKeys 明确禁止出现的参数名（明文联系方式与凭证类）。
var forbiddenParamKeys = map[string]bool{
	"phone": true, "phonenumber": true, "mobile": true, "tel": true, "telephone": true,
	"email": true, "mail": true, "id_card": true, "idcard": true, "idnumber": true,
	"passport": true, "bank_card": true, "bankcard": true, "password": true,
	"pwd": true, "token": true, "secret": true, "authorization": true, "cookie": true,
	"target": true, "target_ref": true, "channel": true, "recipient": true, "recipients": true,
	"mid": true, "operator": true,
}

// cnMobileRe 中国大陆手机号形态（11 位、1 开头）。
var cnMobileRe = regexp.MustCompile(`(^|[^0-9])1[3-9][0-9]{9}([^0-9]|$)`)

// emailRe 邮箱形态。
var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// ValidateParams 校验模板参数：键不得命中禁用清单，值不得是明文号码/邮箱或包含模板控制符。
func ValidateParams(params map[string]string) error {
	for k, v := range params {
		norm := strings.ToLower(strings.TrimSpace(k))
		if norm == "" {
			return fmt.Errorf("notification/policy: empty param name")
		}
		if forbiddenParamKeys[norm] {
			return fmt.Errorf("%w: param %q is not allowed", ErrSensitiveParam, k)
		}
		if len(v) > maxValueLen {
			return fmt.Errorf("notification/policy: param %q exceeds %d bytes", k, maxValueLen)
		}
		if strings.Contains(v, "{{") || strings.Contains(v, "}}") {
			return fmt.Errorf("%w: param %q contains template control characters", ErrRenderUnknownVar, k)
		}
		if cnMobileRe.MatchString(v) || emailRe.MatchString(v) {
			return fmt.Errorf("%w: param %q looks like a plaintext phone number or email", ErrSensitiveParam, k)
		}
	}
	return nil
}

// CheckTemplate 校验模板文本本身（入库前与渲染前都会执行）。
func CheckTemplate(titleTpl, bodyTpl string) error {
	if strings.TrimSpace(bodyTpl) == "" {
		return fmt.Errorf("%w: body template is empty", ErrInvalidTemplate)
	}
	if len(titleTpl) > maxTitleLen || len(bodyTpl) > maxBodyLen {
		return fmt.Errorf("%w: title<=%d body<=%d bytes", ErrInvalidTemplate, maxTitleLen, maxBodyLen)
	}
	for _, seg := range []string{titleTpl, bodyTpl} {
		// 先扫全部 "{{" / "}}"，再扫合法占位符：数量不一致说明存在畸形或注入写法。
		openClose := len(anyBraceRe.FindAllString(seg, -1))
		valid := len(placeholderRe.FindAllString(seg, -1)) * 2
		if openClose != valid {
			return fmt.Errorf("%w: malformed or nested placeholder is not allowed", ErrInvalidTemplate)
		}
		if strings.Contains(seg, "\r") {
			return fmt.Errorf("%w: carriage return is not allowed", ErrInvalidTemplate)
		}
	}
	if strings.Contains(titleTpl, "\n") {
		return fmt.Errorf("%w: title must be single line", ErrInvalidTemplate)
	}
	return nil
}

// TemplateVars 返回模板用到的变量名（升序去重）。
func TemplateVars(titleTpl, bodyTpl string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, name := range append(placeholderRe.FindAllStringSubmatch(titleTpl, -1),
		placeholderRe.FindAllStringSubmatch(bodyTpl, -1)...) {
		if len(name) > 1 && !seen[name[1]] {
			seen[name[1]] = true
			out = append(out, name[1])
		}
	}
	sort.Strings(out)
	return out
}

// Rendered 渲染结果。
type Rendered struct {
	Title string
	Body  string
}

// Render 单次替换渲染：变量缺失返回 ErrRenderMissingVar 并给出缺失清单。
// 替换后的值不会被再次解析，因此参数无法注入新的占位符或改变输出结构。
func Render(titleTpl, bodyTpl string, params map[string]string) (*Rendered, error) {
	if err := CheckTemplate(titleTpl, bodyTpl); err != nil {
		return nil, err
	}
	missing := make([]string, 0, 4)
	render := func(tpl string) (string, error) {
		var errs []string
		out := placeholderRe.ReplaceAllStringFunc(tpl, func(ph string) string {
			name := ph[2 : len(ph)-2]
			v, ok := params[name]
			if !ok {
				missing = append(missing, name)
				errs = append(errs, name)
				return ph
			}
			return v
		})
		if len(errs) > 0 {
			return "", fmt.Errorf("%w: %s", ErrRenderMissingVar, strings.Join(errs, ","))
		}
		if anyBraceRe.MatchString(out) {
			return "", fmt.Errorf("%w: unresolved placeholder remains", ErrRenderUnknownVar)
		}
		return out, nil
	}
	title, err := render(titleTpl)
	if err != nil {
		return nil, err
	}
	body, err := render(bodyTpl)
	if err != nil {
		return nil, err
	}
	if len(title) > maxTitleLen {
		return nil, fmt.Errorf("%w: title exceeds %d bytes", ErrRenderTooLong, maxTitleLen)
	}
	if len(body) > maxBodyLen {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrRenderTooLong, maxBodyLen)
	}
	return &Rendered{Title: title, Body: body}, nil
}

// MissingVars 返回模板缺失的变量名（供 RenderTemplate 预览接口回传，不视为错误）。
func MissingVars(titleTpl, bodyTpl string, params map[string]string) []string {
	vars := TemplateVars(titleTpl, bodyTpl)
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		if _, ok := params[v]; !ok {
			out = append(out, v)
		}
	}
	return out
}
