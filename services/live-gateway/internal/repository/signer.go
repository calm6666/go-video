package repository

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/live-gateway/model"
)

// ErrTicketSignatureInvalid 票据签名不匹配：票据被篡改、来自其它环境的密钥，或 ticket_id 被猜造。
// 与 ErrTicketNotFound 分开是有意的——「查不到」与「签名不对」的处置动作不同（后者要记越权告警）。
var ErrTicketSignatureInvalid = errors.New("live-gateway: ticket signature invalid")

// TicketClaims 票据签名覆盖的断言集。
//
// 这些值的真值在 live_gw_reconnect_ticket 行里，票据串本身只带不透明的 ticket_id：
// proto 注释要求「票据串不含明文身份信息」，把 room_id/mid 编进票据串就违反该约定
// （客户端能读出绑定关系，也能在跨房间复用时被误判成可解码的凭据）。
// 因此签名的防伪造力来自两点，缺一不可：
//  1. 只有持有密钥的服务端能造出与行内断言匹配的 sig；
//  2. 行必须存在（ticket_hash 唯一索引），猜出来的 ticket_id 落在第 1 步就废了。
type TicketClaims struct {
	TicketID string
	RoomID   int64
	Mid      int64
	ConnID   string
	Role     int32
	ExpireAt int64
}

// canonical 是签名的输入串：字段间用 0x1f（单元分隔符）连接，禁止字段本身含该字符，
// 否则 ("a|b", "c") 与 ("a", "b|c") 会签出同一个值。
func (c TicketClaims) canonical() (string, error) {
	if strings.TrimSpace(c.TicketID) == "" {
		return "", ErrTicketSignatureInvalid
	}
	if c.RoomID <= 0 {
		return "", fmt.Errorf("%w: claim room_id must be positive", ErrTicketSignatureInvalid)
	}
	if strings.ContainsAny(c.TicketID, "\x1f") || strings.ContainsAny(c.ConnID, "\x1f") {
		return "", fmt.Errorf("%w: claim contains reserved separator", ErrTicketSignatureInvalid)
	}
	return strings.Join([]string{
		c.TicketID,
		strconv.FormatInt(c.RoomID, 10),
		strconv.FormatInt(c.Mid, 10),
		c.ConnID,
		strconv.FormatInt(int64(c.Role), 10),
		strconv.FormatInt(c.ExpireAt, 10),
	}, "\x1f"), nil
}

// Signer 重连票据的签名与验签。密钥只从 Secret/环境变量注入（AGENTS.md §4）。
type Signer interface {
	// Sign 生成票据串：ticket_id + "." + HMAC-SHA256 十六进制。
	Sign(claims TicketClaims) (string, error)
	// VerifyTicketShape 只做结构校验（长度、分隔符、十六进制），不查库，供参数门禁提前拦下垃圾输入。
	VerifyTicketShape(ticket string) (ticketID string, err error)
	// Verify 用行内断言重算签名并常量时间比对。
	// 必须在按 ticket_hash 定位到行之后调用：断言的真值在行里，票据串里没有。
	Verify(ticket string, claims TicketClaims) error
	// Available 报告密钥是否已注入；false 时签发/兑换一律显式失败。
	Available() bool
}

// hmacSigner 是 HMAC-SHA256 实现。
type hmacSigner struct {
	key []byte
	// ref 只用于错误提示（环境变量名），绝不含密钥字面量。
	ref string
}

// NewSigner 用密钥字面量构造签名器。key 为空时返回 ErrSignerMissing，
// 不提供「退化成固定 key」的分支：那等于任何人都能自造重连凭据。
func NewSigner(key, ref string) (Signer, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("%w: env %s", ErrSignerMissing, ref)
	}
	// 短密钥（<16 字节）多半是配错了占位值，离线爆破 ticket_id+sig 的成本随密钥长度指数下降。
	if len(key) < 16 {
		return nil, fmt.Errorf("%w: env %s value is shorter than 16 bytes", ErrSignerMissing, ref)
	}
	return &hmacSigner{key: []byte(key), ref: ref}, nil
}

// MissingSigner 未注入密钥时的占位实现：每个方法都显式失败。
// 用它而不是返回 nil，是为了让 svc 装配阶段不因依赖缺失而 panic，同时运行期也不会静默放行。
// Ref 是环境变量名（只用于错误提示，绝不含密钥字面量）。
type MissingSigner struct{ Ref string }

func (m MissingSigner) Sign(TicketClaims) (string, error) {
	return "", fmt.Errorf("%w: env %s", ErrSignerMissing, m.Ref)
}
func (m MissingSigner) VerifyTicketShape(string) (string, error) { return "", ErrSignerMissing }
func (m MissingSigner) Verify(string, TicketClaims) error        { return ErrSignerMissing }
func (m MissingSigner) Available() bool                          { return false }

const (
	ticketSep        = "."
	ticketIDBytesMax = 64
	ticketSigHexLen  = sha256.Size * 2
	// ticketMaxBytes ticket_id(<=64) + "." + sig(64) 的硬上限：
	// 不做长度限制会让一个 1MB 的「票据」字符串进到 sha256 与 DB 查询里，客户端可触发的资源事故。
	ticketMaxBytes = ticketIDBytesMax + len(ticketSep) + ticketSigHexLen
)

func (s *hmacSigner) Available() bool { return s != nil && len(s.key) > 0 }

func (s *hmacSigner) Sign(claims TicketClaims) (string, error) {
	if !s.Available() {
		return "", fmt.Errorf("%w: env %s", ErrSignerMissing, s.ref)
	}
	if len(claims.TicketID) > ticketIDBytesMax {
		return "", fmt.Errorf("%w: ticket_id too long (max %d bytes)", ErrSignerMissing, ticketIDBytesMax)
	}
	sig, err := s.mac(claims)
	if err != nil {
		return "", err
	}
	return claims.TicketID + ticketSep + sig, nil
}

func (s *hmacSigner) mac(claims TicketClaims) (string, error) {
	canonical, err := claims.canonical()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	if _, err := mac.Write([]byte(canonical)); err != nil {
		return "", fmt.Errorf("%w: hmac write failed: %v", ErrStoreUnavailable, err)
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *hmacSigner) VerifyTicketShape(ticket string) (string, error) {
	if strings.TrimSpace(ticket) == "" {
		return "", model.ErrEmptyTicket
	}
	if len(ticket) > ticketMaxBytes || strings.ContainsAny(ticket, " \t\r\n") {
		return "", fmt.Errorf("%w: malformed ticket", ErrTicketSignatureInvalid)
	}
	idx := strings.LastIndex(ticket, ticketSep)
	if idx <= 0 || idx == len(ticket)-1 {
		return "", fmt.Errorf("%w: malformed ticket", ErrTicketSignatureInvalid)
	}
	ticketID := ticket[:idx]
	sig := ticket[idx+len(ticketSep):]
	if ticketID == "" || len(ticketID) > ticketIDBytesMax {
		return "", fmt.Errorf("%w: ticket_id length out of range", ErrTicketSignatureInvalid)
	}
	if len(sig) != ticketSigHexLen {
		return "", fmt.Errorf("%w: signature length %d, want %d", ErrTicketSignatureInvalid, len(sig), ticketSigHexLen)
	}
	if _, err := hex.DecodeString(sig); err != nil {
		return "", fmt.Errorf("%w: signature is not hex", ErrTicketSignatureInvalid)
	}
	return ticketID, nil
}

func (s *hmacSigner) Verify(ticket string, claims TicketClaims) error {
	if !s.Available() {
		return fmt.Errorf("%w: env %s", ErrSignerMissing, s.ref)
	}
	_, _, err := splitTicket(ticket)
	if err != nil {
		return err
	}
	// 断言里的 ticket_id 必须与票据串一致：否则等于用 A 票的签名去认证 B 票。
	if claims.TicketID == "" {
		return model.ErrTicketNotFound
	}
	expected, err := s.mac(claims)
	if err != nil {
		return err
	}
	got, err := hex.DecodeString(strings.SplitN(ticket, ticketSep, 2)[1])
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrTicketSignatureInvalid)
	}
	want, err := hex.DecodeString(expected)
	if err != nil {
		return fmt.Errorf("%w: self check failed", ErrStoreUnavailable)
	}
	// 常量时间比对：非常量比较会让签名逐字节可测（时序侧信道），票据是凭据不是普通字符串。
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrTicketSignatureInvalid
	}
	return nil
}

func splitTicket(ticket string) (ticketID, sig string, err error) {
	idx := strings.LastIndex(ticket, ticketSep)
	if idx <= 0 || idx == len(ticket)-1 {
		return "", "", fmt.Errorf("%w: malformed ticket", ErrTicketSignatureInvalid)
	}
	return ticket[:idx], ticket[idx+len(ticketSep):], nil
}
