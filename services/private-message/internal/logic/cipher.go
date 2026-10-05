// 本文件是 logic 包的手写扩展（正文信封加密与内容指纹），不是 goctl 生成产物。
//
// 隐私级别 P4（私密通信内容）的落地口径（README「正文加密与留存」）：
//   - 明文永不入库：写入前 AES-256-GCM 加密成 nonce‖ciphertext，密钥版本记在 key_version；
//   - 明文永不入日志/入事件：加解密方法不打印任何入参，调用方同理；
//   - 内容指纹是 keyed hash（HMAC-SHA256 + pepper），只用于风控查重，不可反推原文；
//   - 密钥缺失一律 ErrCipherKeyMissing，绝不退化成明文落库（AGENTS.md §6）。

package logic

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"go-video/services/private-message/internal/config"
	"go-video/services/private-message/model"
)

const (
	// aesGCMKeyBytes 固定 32 字节：配置注释承诺的是 AES-256，
	// 收下 16/24 字节密钥会让运维以为在跑 128/192，属静默降级。
	aesGCMKeyBytes = 32
	// gcmNonceBytes 是 AES-GCM 标准 nonce 长度（12 字节，NIST 推荐）。
	gcmNonceBytes = 12
	// gcmOverheadBytes 是 tag 长度；密文必须严格长于 nonce+tag 才可能是本方法产出的包。
	// 解密时先检查长度，避免把「被清理成空串」的行当成密钥轮换事故报 ErrDecryptFailed。
	gcmOverheadBytes = 16
)

// messageCipher 是当前密钥版本下的加密器。每请求构造成本极低（只校验长度 + 建 GCM），
// 不做进程级缓存：密钥轮换后新请求必须立刻用新版本，缓存反而是轮换的阻碍。
type messageCipher struct {
	aead       cipher.AEAD
	pepper     []byte
	keyVersion int32
}

// newMessageCipher 从配置构造加密器。
// 未注入密钥材料时返回 model.ErrCipherKeyMissing：调用方必须把这次写入整体拒掉，
// 不允许「先存明文、等密钥到位再补加密」——那等于把 P4 内容写进了 binlog 与备份。
func newMessageCipher(c config.Config) (*messageCipher, error) {
	if !c.CipherConfigured() {
		return nil, model.ErrCipherKeyMissing
	}
	key, err := decodeKey(c.Cipher.DataKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: data key 无法解析", model.ErrCipherKeyMissing)
	}
	if len(key) != aesGCMKeyBytes {
		return nil, fmt.Errorf("%w: data key 必须是 %d 字节（AES-256）", model.ErrCipherKeyMissing, aesGCMKeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: aes 初始化失败", model.ErrCipherKeyMissing)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: gcm 初始化失败", model.ErrCipherKeyMissing)
	}
	return &messageCipher{
		aead:       aead,
		pepper:     []byte(c.Cipher.HashPepper),
		keyVersion: c.Cipher.KeyVersion,
	}, nil
}

// decodeKey 同时接受标准与无填充 base64：不同 Secret 渲染管道对 padding 的处理不一致，
// 但两种编码解出的字节序列必须完全一致，这里不做任何「解不出就当密钥」的降级。
func decodeKey(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("empty key")
	}
	if b64, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		return b64, nil
	}
	return base64.RawStdEncoding.DecodeString(trimmed)
}

// encrypt 返回 nonce‖ciphertext（GCM tag 由 Go 实现附在密文尾部）。
func (mc *messageCipher) encrypt(plaintext string) ([]byte, error) {
	if mc == nil || mc.aead == nil {
		return nil, model.ErrCipherKeyMissing
	}
	nonce := make([]byte, gcmNonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("%w: nonce 生成失败", model.ErrCipherKeyMissing)
	}
	sealed := mc.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return sealed, nil
}

// decrypt 还原明文。失败统一返回 ErrDecryptFailed：
// 对外不区分「无权限」「密钥版本缺失」「数据损坏」，避免用错误码探测他人会话（README 授权口径）。
// 错误文本只带 msg_id 之类主键由调用方拼接，本函数不回传密文。
func (mc *messageCipher) decrypt(blob []byte) (string, error) {
	if mc == nil || mc.aead == nil {
		return "", model.ErrCipherKeyMissing
	}
	if len(blob) < gcmNonceBytes+gcmOverheadBytes {
		return "", ErrCipherBlobTruncated
	}
	nonce := blob[:gcmNonceBytes]
	ct := blob[gcmNonceBytes:]
	plain, err := mc.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		// GCM 认证失败可能来自密钥版本缺失（轮换遗漏）也可能来自数据被改写，
		// 两者都不能被调用方区分，也不能被日志区分成「原文片段」。
		return "", model.ErrDecryptFailed
	}
	return string(plain), nil
}

// hash 计算 content_hash（HMAC-SHA256(pepper, plaintext) 的 hex，64 字符 = CHAR(64) 列宽）。
// 用途是风控查重与重复骚扰识别：同一 pepper 下相同明文得到相同指纹，
// 但没有 pepper 就无法反查明文，因此它不是「内容哈希索引」意义上的泄露面。
func (mc *messageCipher) hash(plaintext string) string {
	if mc == nil || len(mc.pepper) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, mc.pepper)
	mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

// version 返回写入 pm_message.key_version 的密钥版本。
func (mc *messageCipher) version() int32 { return mc.keyVersion }

// ErrCipherBlobTruncated 密文长度不足以构成一个 GCM 包（多为手工写坏或清理残留）。
// 与 ErrDecryptFailed 分开是为了让运维日志能定位「数据形态问题」，
// 但对外（RPC 响应）仍统一映射成解密失败口径，不给探测者新信息。
var ErrCipherBlobTruncated = errors.New("private-message: cipher blob truncated")
