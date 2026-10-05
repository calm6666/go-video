package repository

// 本文件实现管理员口令散列。
//
// 为什么不复用 account 的 MD5(pwd + salt)：account 沿用了参考仓库的
// MD5+盐方案（其 login.go 已标注“生产建议后续升级 bcrypt”）。AGENTS.md §9 要求
// 安全强度不得倒退，管理员口令是全站最高权限凭证，强度必须不低于用户口令，
// 因此这里使用 PBKDF2-HMAC-SHA256（Go 标准库 crypto/hmac + crypto/sha256 实现，
// 不引入新依赖），并把算法与迭代数一并写入 password_hash 前缀，便于后续升级时
// 按 pwd_algo 平滑迁移。校验使用常数时间比较，避免时序侧信道。
//
// 存储格式：pbkdf2_sha256$<迭代数>$<盐 hex>$<派生密钥 hex>

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// PwdAlgo 当前管理员口令算法标识，写入 op_admin_user.pwd_algo。
	PwdAlgo = "pbkdf2_sha256"

	// defaultIterations PBKDF2 迭代次数（OWASP 对 PBKDF2-HMAC-SHA256 的建议量级）。
	defaultIterations = 210000
	// minIterations 允许出现在存量散列中的最小迭代数（低于此值视为不安全散列）。
	minIterations = 1000
	// saltLen 盐长度（字节）。
	saltLen = 16
	// dkLen 派生密钥长度（字节），等于 SHA-256 输出长度。
	dkLen = sha256.Size

	// MinPasswordLength 管理员口令最小长度（rune 数）。
	MinPasswordLength = 10
	// MaxPasswordLength 管理员口令最大长度，限制 PBKDF2 的输入规模（防 DoS）。
	MaxPasswordLength = 64
)

// passwordStrengthOK 校验口令长度：10~64 个字符，且不包含空白首尾。
func passwordStrengthOK(pwd string) bool {
	if len([]rune(pwd)) < MinPasswordLength || len([]rune(pwd)) > MaxPasswordLength {
		return false
	}
	if strings.TrimSpace(pwd) != pwd {
		// 首尾空白多半是复制粘贴产物，直接拒绝以免管理员以为口令含空格。
		return false
	}
	return true
}

// HashAdminPassword 生成管理员口令散列，返回 "算法$迭代$盐$散列" 与算法标识。
func HashAdminPassword(pwd string) (string, string, error) {
	return hashAdminPasswordWithIterations(pwd, defaultIterations)
}

// hashAdminPasswordWithIterations 供测试使用低迭代数（避免单测耗时过长）。
func hashAdminPasswordWithIterations(pwd string, iterations int) (string, string, error) {
	if iterations < minIterations {
		return "", "", fmt.Errorf("operation/password: iterations %d below minimum %d", iterations, minIterations)
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", "", fmt.Errorf("operation/password: read salt: %w", err)
	}
	dk := pbkdf2SHA256([]byte(pwd), salt, iterations, dkLen)
	return fmt.Sprintf("%s$%d$%s$%s", PwdAlgo, iterations, hex.EncodeToString(salt), hex.EncodeToString(dk)), PwdAlgo, nil
}

// VerifyAdminPassword 校验口令是否匹配存量散列；散列格式非法时返回错误而非“不匹配”，
// 以便调用方区分“口令错”和“数据坏”。
func VerifyAdminPassword(pwd, stored string) (bool, error) {
	algo, iterations, salt, dk, err := parseAdminPassword(stored)
	if err != nil {
		return false, err
	}
	if algo != PwdAlgo {
		return false, fmt.Errorf("operation/password: unsupported pwd_algo %q", algo)
	}
	got := pbkdf2SHA256([]byte(pwd), salt, iterations, len(dk))
	return hmac.Equal(got, dk), nil
}

// parseAdminPassword 拆解存储格式，不做口令校验。
func parseAdminPassword(stored string) (algo string, iterations int, salt, dk []byte, err error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 {
		return "", 0, nil, nil, errors.New("operation/password: malformed password hash")
	}
	iterations, err = strconv.Atoi(parts[1])
	if err != nil || iterations < minIterations {
		return "", 0, nil, nil, fmt.Errorf("operation/password: invalid iterations %q", parts[1])
	}
	salt, err = hex.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return "", 0, nil, nil, errors.New("operation/password: invalid salt")
	}
	dk, err = hex.DecodeString(parts[3])
	if err != nil || len(dk) == 0 {
		return "", 0, nil, nil, errors.New("operation/password: invalid derived key")
	}
	return parts[0], iterations, salt, dk, nil
}

// pbkdf2SHA256 按 RFC 2898/8018 实现 PBKDF2（PRF = HMAC-SHA256）。
// 只使用标准库，避免为本服务引入新的模块依赖。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	const hLen = sha256.Size
	if iter < 1 {
		iter = 1
	}
	if keyLen <= 0 {
		return nil
	}
	numBlocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, numBlocks*hLen)
	var intBlock [4]byte
	for blockIdx := 1; blockIdx <= numBlocks; blockIdx++ {
		binary.BigEndian.PutUint32(intBlock[:], uint32(blockIdx))
		// U1 = PRF(P, S || INT(i))
		u := pbkdf2PRF(password, append(append([]byte{}, salt...), intBlock[:]...))
		t := make([]byte, hLen)
		copy(t, u)
		for j := 2; j <= iter; j++ {
			// Uj = PRF(P, U(j-1))；T = U1 ^ U2 ^ ... ^ Uc
			u = pbkdf2PRF(password, u)
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// pbkdf2PRF 是 PBKDF2 的伪随机函数 HMAC-SHA256(password, data)。
func pbkdf2PRF(password, data []byte) []byte {
	m := hmac.New(sha256.New, password)
	m.Write(data)
	return m.Sum(nil)
}
