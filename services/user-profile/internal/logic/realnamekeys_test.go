package logic

// realnamekeys_test.go 是实名簇（RealnameApply / RealnameDetail / RealnameStrippedInfo /
// RealnameStatus / RealnameApplyStatus / MidByRealnameCard / RealnameTelCapture）共用的
// 测试脚手架：证件号夹具、与生产同一把 PEM 的加解密回读、缓存载荷解码视图。
//
// 为什么这些要收在一处：实名簇的断言面不是「返回了什么状态」，而是
// **明文证件号有没有落到不该落的地方**（返回值 / 日志 / MySQL 列 / Redis 载荷）。
// 要判这四条，用例必须能自己解密库里的密文（证明它是真密文而不是随机串）、
// 必须能读出 Redis 里那坨 JSON 的字段名（`real_card` 是明文证件号的藏身处）。
// 两件事都依赖 repository 未导出的结构，只能靠**同名 JSON tag 的测试侧视图**耦合：
// 生产改字段名 → 视图解出零值 → 断言变红（这是刻意的耦合，不是遗漏）。
//
// 证件号夹具（都是校验位自洽的示例号，不是真实号码）：
//   - 18 位：`110101199003071211`，生日 1990-03-07、顺序码第 17 位 '1'（奇→男）；
//   - 18 位未成年：`110101201601012020`，生日 2016-01-01、第 17 位 '2'（偶→女）；
//   - 15 位：`110101900307121`，parseIdentity 走 `19` 前缀分支，生日 1990-03-07、末位 '1'（奇→男）；
//   - 畸形但形似：`11010119900x07121X`，18 位、含字母，`isIDCard` 必拒（'x' 不在末位），
//     且 `parseIdentity` 的 `Atoi(id[10:12])` 会失败——用于验证错误消息会不会带出明文。

import (
	"strings"
	"testing"

	"go-video/services/user-profile/internal/repository"
)

const (
	cardMaleAdult   = "110101199003071211" // 18 位、成年、男
	cardFemaleMinor = "110101201601012020" // 18 位、未成年、女
	cardIDCard15    = "110101900307121"    // 15 位老式身份证
	cardMalformed   = "11010119900x07121X" // 含字母，isIDCard 拒、parseIdentity 也解不出
)

// testCryptor 用**与 newRawStore 注入给 Repository 的同一对 PEM**造一个测试侧加解密器：
// 断言「库里的密文能解回原证件号」必须由它来做，而不是由实现自己说了算。
var testCryptor = repository.NewCardCryptor(testPubPEM, testPrivPEM)

// encryptCard 返回生产会写进 card_num / card 列的那串 base64 密文。
// 用例用它布 realname_info 的存量行（布景口径与生产写入口一致）。
func encryptCard(t *testing.T, plain string) string {
	t.Helper()
	bs, err := testCryptor.CardEncrypt([]byte(plain))
	wantNoErr(t, "测试侧加密 "+plain[:4]+"****", err)
	return string(bs)
}

// decryptCard 解回库里的密文；失败时把错误原样交回用例断言（解密失败本身是结论）。
func decryptCard(cipher string) (string, error) {
	bs, err := testCryptor.CardDecrypt([]byte(cipher))
	return string(bs), err
}

// realnameCacheView 是 repository.realnameCachePayload 的测试侧镜像。
// json tag 必须逐字对齐：对不齐时解出零值，实名簇的缓存断言立刻变红。
type realnameCacheView struct {
	Cached   bool   `json:"cached"`
	Mid      int64  `json:"mid"`
	Channel  int8   `json:"channel"`
	Realname string `json:"realname"`
	Country  int16  `json:"country"`
	CardType int8   `json:"card_type"`
	Card     string `json:"card"`
	RealCard string `json:"real_card"`
	Status   int8   `json:"status"`
	Reason   string `json:"reason"`
}

// cachedRealname 回读 realname_info_<mid> 当前值；key 不存在返回 false。
func cachedRealname(t *testing.T, e *env, mid int64) (realnameCacheView, bool) {
	t.Helper()
	var v realnameCacheView
	return v, e.st.cache.jsonOf(keyRealname(mid), &v)
}

// wantNoPII 断言一批文本里都不出现某个明文片段。
// needles 用「证件号本身」这类唯一串，不用泛化关键词——否则断言会白过。
func wantNoPII(t *testing.T, label string, needles []string, blobs map[string]string) {
	t.Helper()
	for name, blob := range blobs {
		for _, needle := range needles {
			if needle == "" {
				continue
			}
			if strings.Contains(blob, needle) {
				t.Errorf("%s：%s 里出现了敏感明文 %s（片段：%s）",
					label, name, needle, clip(blob, len(needle)))
			}
		}
	}
}

// clip 截取命中处附近的内容用于报错，避免把整坨载荷刷进测试输出。
func clip(s string, n int) string {
	if len(s) <= n+40 {
		return s
	}
	return s[:n+40] + "…"
}
