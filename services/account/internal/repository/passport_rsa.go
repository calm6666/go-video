package repository

// 本文件提供登录密码的 RSA 加解密（参考 passport-login 的 RSA 密码加密传输）。
// 客户端用网关 /x/passport-login/key 返回的公钥加密密码，服务端用私钥解密。
// 密钥来自配置（Secret/Vault 注入）；未配置时（开发模式）按明文密码处理。

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
)

// PassportRSA 登录密码 RSA 加解密器（结构同实名证件加密，职责独立）。
type PassportRSA struct {
	publicKey  []byte
	privateKey []byte
}

// NewPassportRSA 构造加解密器；pub/priv 为 PEM 格式密钥。
// 两者都为空时返回 nil（表示未配置密钥，登录按明文密码处理——仅限开发环境）。
func NewPassportRSA(pub, priv string) *PassportRSA {
	if pub == "" && priv == "" {
		return nil
	}
	return &PassportRSA{publicKey: []byte(pub), privateKey: []byte(priv)}
}

// CardEncrypt RSA 公钥加密（返回 base64）。
func (c *PassportRSA) CardEncrypt(text []byte) ([]byte, error) {
	if len(text) == 0 {
		return []byte{}, nil
	}
	block, _ := pem.Decode(c.publicKey)
	if block == nil {
		return nil, errors.New("account/passport-rsa: invalid public key pem")
	}
	pubInterface, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := pubInterface.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("account/passport-rsa: public key is not rsa")
	}
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, pub, text)
	if err != nil {
		return nil, err
	}
	data := make([]byte, base64.StdEncoding.EncodedLen(len(encrypted)))
	base64.StdEncoding.Encode(data, encrypted)
	return data, nil
}

// CardDecrypt RSA 私钥解密（入参为 base64 密文）。
func (c *PassportRSA) CardDecrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil
	}
	block, _ := pem.Decode(c.privateKey)
	if block == nil {
		return nil, errors.New("account/passport-rsa: invalid private key pem")
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// 兼容 PKCS8 私钥
		if k, e := x509.ParsePKCS8PrivateKey(block.Bytes); e == nil {
			if rk, ok := k.(*rsa.PrivateKey); ok {
				privateKey = rk
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	size, err := base64.StdEncoding.Decode(decoded, data)
	if err != nil {
		return nil, err
	}
	return rsa.DecryptPKCS1v15(rand.Reader, privateKey, decoded[:size])
}
