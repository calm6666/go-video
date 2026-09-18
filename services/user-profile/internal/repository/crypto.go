package repository

// 本文件移植自参考仓库 service/crypto/realname.go：实名证件号使用 RSA(PKCS1v15)
// 公钥加密、base64 编码后入库，出库用私钥解密。密钥来自配置（Secret/Vault 注入）。

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
)

// CardCryptor 实名证件号 RSA 加解密器。
type CardCryptor struct {
	publicKey  []byte
	privateKey []byte
}

// NewCardCryptor 构造加解密器；pub/priv 为 PEM 格式密钥。
func NewCardCryptor(pub, priv string) *CardCryptor {
	return &CardCryptor{
		publicKey:  []byte(pub),
		privateKey: []byte(priv),
	}
}

// rsaEncrypt RSA 公钥加密。
func (c *CardCryptor) rsaEncrypt(text []byte) ([]byte, error) {
	block, _ := pem.Decode(c.publicKey)
	if block == nil {
		return nil, errors.New("user-profile/crypto: invalid public key pem")
	}
	pubInterface, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := pubInterface.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("user-profile/crypto: public key is not rsa")
	}
	return rsa.EncryptPKCS1v15(rand.Reader, pub, text)
}

// rsaDecrypt RSA 私钥解密。
func (c *CardCryptor) rsaDecrypt(text []byte) ([]byte, error) {
	block, _ := pem.Decode(c.privateKey)
	if block == nil {
		return nil, errors.New("user-profile/crypto: invalid private key pem")
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
	return rsa.DecryptPKCS1v15(rand.Reader, privateKey, text)
}

// CardEncrypt 加密证件号（返回 base64 密文）。
func (c *CardCryptor) CardEncrypt(text []byte) ([]byte, error) {
	if len(text) == 0 {
		return []byte{}, nil
	}
	encrypted, err := c.rsaEncrypt(text)
	if err != nil {
		return nil, err
	}
	data := make([]byte, base64.StdEncoding.EncodedLen(len(encrypted)))
	base64.StdEncoding.Encode(data, encrypted)
	return data, nil
}

// CardDecrypt 解密证件号（入参为 base64 密文）。
func (c *CardCryptor) CardDecrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	size, err := base64.StdEncoding.Decode(decoded, data)
	if err != nil {
		return nil, err
	}
	return c.rsaDecrypt(decoded[:size])
}
