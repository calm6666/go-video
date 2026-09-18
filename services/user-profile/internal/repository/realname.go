package repository

// 本文件移植自参考仓库 service/realname.go + dao/mysql.go + dao/memcache.go 的
// 实名认证聚合逻辑：验证码发放与校验、证件号加密入库（RSA）、查重、
// 身份证生日/性别解析、成年判断、脱敏信息与证件反查。

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

var (
	// realnameSalt 证件号哈希盐（移植自参考仓库 _realnameSalt）。
	realnameSalt = "biliidentification@#$%^&*()(*&^%$#"
	// imgPrefix/imgSuffix 证件照 token 路径包装（参考 _imgPrefix/_imgSuffix）。
	imgPrefix = "/idenfiles/"
	imgSuffix = ".txt"

	idCard18Regexp = regexp.MustCompile(`^\d{17}[\d|x]$`)
	idCard15Regexp = regexp.MustCompile(`^\d{15}$`)

	genderUnknown = "unknown"
	genderMale    = "male"
	genderFemale  = "female"
)

// 实名领域错误（logic 层据此映射错误信息）。
var (
	// ErrRealnameCaptureSendTooMany 验证码发送过于频繁。
	ErrRealnameCaptureSendTooMany = errors.New("realname capture send too many")
	// ErrRealnameCaptureInvalid 验证码无效（未发送或已过期）。
	ErrRealnameCaptureInvalid = errors.New("realname capture invalid")
	// ErrRealnameCaptureErr 验证码错误。
	ErrRealnameCaptureErr = errors.New("realname capture error")
	// ErrRealnameCaptureErrTooMany 验证码错误次数过多。
	ErrRealnameCaptureErrTooMany = errors.New("realname capture error too many")
	// ErrRealnameCardBindAlready 证件号已绑定其他账号。
	ErrRealnameCardBindAlready = errors.New("realname card already bound")
	// ErrRealnameApplyAlready 已提交过实名申请。
	ErrRealnameApplyAlready = errors.New("realname already applied")
	// ErrRealnameCardNumErr 证件号格式错误。
	ErrRealnameCardNumErr = errors.New("realname card number error")
)

// realnameCachePayload 实名信息缓存结构（含解密后的证件号 RealCard）。
type realnameCachePayload struct {
	// Cached 是否命中缓存（false 表示 miss）
	Cached bool `json:"cached"`
	// Mid 用户 ID
	Mid int64 `json:"mid"`
	// Channel 实名渠道
	Channel int8 `json:"channel"`
	// Realname 真实姓名
	Realname string `json:"realname"`
	// Country 国家
	Country int16 `json:"country"`
	// CardType 证件类型
	CardType int8 `json:"card_type"`
	// Card 证件号密文（base64）
	Card string `json:"card"`
	// RealCard 解密后的证件号（仅认证通过时填充）
	RealCard string `json:"real_card"`
	// Status 流程状态
	Status int8 `json:"status"`
	// Reason 驳回原因
	Reason string `json:"reason"`
}

// realnameInfo 读取实名信息（缓存→DB→解密→异步回填，参考 service.realnameInfo）。
// 始终返回非 nil；未申请时 Status=RealnameApplyStatusNone。
func (r *Repository) realnameInfo(ctx context.Context, mid int64) (*realnameCachePayload, error) {
	cacheOK := true
	var payload realnameCachePayload
	if err := r.cache.getJSON(ctx, keyRealname(mid), &payload); err != nil {
		cacheOK = false
		logx.Errorf("user-profile/realname: get realname cache mid=%d err=%v", mid, err)
	}
	if payload.Cached {
		return &payload, nil
	}
	info, err := r.realnameModel.FindOne(ctx, mid)
	if err != nil {
		return nil, err
	}
	if info == nil {
		payload = realnameCachePayload{
			Cached: true,
			Mid:    mid,
			Status: model.RealnameApplyStatusNone,
		}
	} else {
		payload = realnameCachePayload{
			Cached:   true,
			Mid:      info.Mid,
			Channel:  info.Channel,
			Realname: info.Realname,
			Country:  info.Country,
			CardType: info.CardType,
			Card:     info.Card,
			Status:   info.Status,
			Reason:   info.Reason,
		}
		if info.Status == model.RealnameApplyStatusPass && info.Card != "" {
			if decrypted, err := r.cryptor.CardDecrypt([]byte(info.Card)); err != nil {
				logx.Errorf("user-profile/realname: decrypt card mid=%d err=%v", mid, err)
			} else {
				payload.RealCard = string(decrypted)
			}
		}
	}
	if cacheOK {
		p := payload
		_ = r.async.Do(ctx, func(c context.Context) {
			r.cache.setJSON(c, keyRealname(mid), p, cacheTTLRealname)
		})
	}
	return &payload, nil
}

// RealnameStatus 查询实名认证状态（参考 service.RealnameStatus）。
func (r *Repository) RealnameStatus(ctx context.Context, mid int64) (int8, error) {
	info, err := r.realnameInfo(ctx, mid)
	if err != nil {
		return 0, err
	}
	if info.Status == model.RealnameApplyStatusPass {
		return model.RealnameStatusTrue, nil
	}
	return model.RealnameStatusFalse, nil
}

// RealnameApplyStatus 查询实名申请流程状态（参考 service.RealnameApplyStatus）。
func (r *Repository) RealnameApplyStatus(ctx context.Context, mid int64) (*rpc.RealnameApplyInfoReply, error) {
	info, err := r.realnameInfo(ctx, mid)
	if err != nil {
		return nil, err
	}
	return &rpc.RealnameApplyInfoReply{
		Status: int32(info.Status),
		Remark: info.Reason,
	}, nil
}

// RealnameBrief 查询实名简要信息（参考 service.RealnameBrief）。
func (r *Repository) RealnameBrief(ctx context.Context, mid int64) (*RealnameBrief, error) {
	info, err := r.realnameInfo(ctx, mid)
	if err != nil {
		return nil, err
	}
	brief := &RealnameBrief{
		Realname: info.Realname,
		Card:     info.RealCard,
		CardType: int(info.CardType),
	}
	if info.Status == model.RealnameApplyStatusPass {
		brief.Status = model.RealnameStatusTrue
	} else {
		brief.Status = model.RealnameStatusFalse
	}
	return brief, nil
}

// RealnameBrief 实名简要信息（HTTP 与 RPC 共用转换源）。
type RealnameBrief struct {
	Realname string
	Card     string
	CardType int
	Status   int8
}

// RealnameDetail 查询实名详情（性别解析 + 手持照，参考 service.RealnameDetail）。
func (r *Repository) RealnameDetail(ctx context.Context, mid int64) (*rpc.RealnameDetailReply, error) {
	info, err := r.realnameInfo(ctx, mid)
	if err != nil {
		return nil, err
	}
	detail := &rpc.RealnameDetailReply{
		Realname: info.Realname,
		Card:     info.RealCard,
		CardType: int32(info.CardType),
		Gender:   genderUnknown,
	}
	if info.Status != model.RealnameApplyStatusPass {
		detail.Status = model.RealnameStatusFalse
		return detail, nil
	}
	detail.Status = model.RealnameStatusTrue
	if detail.CardType == model.RealnameCardTypeIdentity {
		if _, gender, err := parseIdentity(detail.Card); err != nil {
			logx.Infof("user-profile/realname: parse identity mid=%d err=%v", mid, err)
		} else {
			detail.Gender = gender
		}
	}
	if info.Channel == model.RealnameChannelMain {
		apply, err := r.applyModel.FindOne(ctx, mid)
		if err != nil {
			return nil, err
		}
		if apply != nil && apply.Status == model.RealnameApplyStatusPass && apply.HandIMG > 0 {
			if img, err := r.applyImgModel.FindOne(ctx, apply.HandIMG); err != nil {
				return nil, err
			} else if img != nil {
				detail.HandImg = r.generateIMGURL(img.IMGData)
			}
		}
	}
	return detail, nil
}

// generateIMGURL 把图片 token 路径转换为 CDN URL（参考 generateIMGURL）。
// IMGURLTemplate 未配置时原样返回 token 路径。
func (r *Repository) generateIMGURL(token string) string {
	token = strings.TrimPrefix(token, imgPrefix)
	token = strings.TrimSuffix(token, imgSuffix)
	if r.imgURLTemplate == "" {
		return token
	}
	return fmt.Sprintf(r.imgURLTemplate, token)
}

// RealnameStrippedInfo 查询脱敏实名信息（参考 service.RealnameStrippedInfo）。
func (r *Repository) RealnameStrippedInfo(ctx context.Context, mid int64) (*rpc.RealnameStrippedInfoReply, error) {
	info, err := r.realnameInfo(ctx, mid)
	if err != nil {
		return nil, err
	}
	adultType, err := r.RealnameAdult(ctx, mid)
	if err != nil {
		logx.Errorf("user-profile/realname: adult mid=%d err=%v", mid, err)
		adultType = model.RealnameAdultTypeUnknown
	}
	return &rpc.RealnameStrippedInfoReply{
		Mid:       info.Mid,
		Status:    int32(info.Status),
		Channel:   int32(info.Channel),
		Country:   int32(info.Country),
		CardType:  int32(info.CardType),
		AdultType: int32(adultType),
	}, nil
}

// RealnameAdult 判断实名成年状态（参考 service.RealnameAdult）。
func (r *Repository) RealnameAdult(ctx context.Context, mid int64) (int8, error) {
	info, err := r.realnameInfo(ctx, mid)
	if err != nil {
		return model.RealnameAdultTypeUnknown, err
	}
	if info.Status != model.RealnameApplyStatusPass {
		return model.RealnameAdultTypeUnknown, nil
	}
	if info.CardType != model.RealnameCardTypeIdentity || info.Country != model.RealnameCountryChina {
		return model.RealnameAdultTypeUnknown, nil
	}
	birthday, _, err := parseIdentity(info.RealCard)
	if err != nil {
		return model.RealnameAdultTypeUnknown, nil
	}
	adult, err := isAdult(birthday, time.Now())
	if err != nil || !adult {
		return model.RealnameAdultTypeFalse, nil
	}
	return model.RealnameAdultTypeTrue, nil
}

// RealnameCheck 校验证件号与实名信息是否一致（参考 service.RealnameCheck）。
func (r *Repository) RealnameCheck(ctx context.Context, mid int64, cardType int8, cardCode string) (bool, error) {
	if cardCode == "" {
		return false, nil
	}
	info, err := r.realnameInfo(ctx, mid)
	if err != nil {
		return false, err
	}
	if info.Status != model.RealnameApplyStatusPass {
		return false, nil
	}
	if cardType >= 0 && info.CardType != cardType {
		return false, nil
	}
	return strings.EqualFold(info.RealCard, strings.ToUpper(cardCode)), nil
}

// MidByRealnameCard 按证件号批量反查 mid（参考 service.MidByRealnameCard）。
func (r *Repository) MidByRealnameCard(ctx context.Context, cardCodes []string, country, cardType int32) (map[string]int64, error) {
	md5ToCode := make(map[string]string, len(cardCodes))
	cardMD5s := make([]string, 0, len(cardCodes))
	for _, code := range cardCodes {
		hashed := cardMD5(code, int(cardType), int(country))
		cardMD5s = append(cardMD5s, hashed)
		md5ToCode[hashed] = code
	}
	md5ToMid, err := r.realnameModel.FindMidsByCardMD5s(ctx, cardMD5s)
	if err != nil {
		return nil, err
	}
	codeToMid := make(map[string]int64, len(md5ToMid))
	for hashed, mid := range md5ToMid {
		if code, ok := md5ToCode[hashed]; ok {
			codeToMid[code] = mid
		}
	}
	return codeToMid, nil
}

// RealnameTelCapture 发送实名手机验证码（参考 service.RealnameTelCapture）。
// 参考仓库通过 SMS 服务发送；notification 服务未接入前仅记录验证码日志
// （开发模式），验证码仍写入 Redis 供校验流程使用。
func (r *Repository) RealnameTelCapture(ctx context.Context, mid int64) (int, error) {
	times, err := r.cache.captureTimes(ctx, mid)
	if err != nil {
		return 0, err
	}
	if times < 0 {
		if err := r.cache.setCaptureTimes(ctx, mid, 0); err != nil {
			return 0, err
		}
		times = 0
	}
	if times > 5 {
		return 0, ErrRealnameCaptureSendTooMany
	}
	capture := rand.Intn(900000) + 100000
	// SMS 发送：notification 服务未接入，记录日志供开发联调（生产接入后替换为真实下发）
	logx.Infof("user-profile/realname: send capture mid=%d code=%06d (sms not integrated)", mid, capture)
	if err := r.cache.setCaptureCode(ctx, mid, capture); err != nil {
		return 0, err
	}
	if err := r.cache.incrCaptureTimes(ctx, mid); err != nil {
		return 0, err
	}
	if err := r.cache.delCaptureErrTimes(ctx, mid); err != nil {
		return 0, err
	}
	return capture, nil
}

// RealnameTelCaptureCheck 校验手机验证码（参考 service.RealnameTelCaptureCheck）。
func (r *Repository) RealnameTelCaptureCheck(ctx context.Context, mid int64, captureCode int) error {
	serverCode, err := r.cache.captureCode(ctx, mid)
	if err != nil {
		return err
	}
	if serverCode < 0 {
		return ErrRealnameCaptureInvalid
	}
	if captureCode != serverCode {
		return ErrRealnameCaptureErr
	}
	return nil
}

// RealnameApplyArg 实名申请参数。
type RealnameApplyArg struct {
	Mid           int64
	CaptureCode   int
	Realname      string
	CardType      int8
	CardCode      string
	Country       int16
	HandIMGToken  string
	FrontIMGToken string
	BackIMGToken  string
}

// RealnameApply 提交实名认证申请（参考 service.RealnameApply）。
// 流程：校验验证码 → 校验是否已申请 → 证件查重 → 身份证格式校验 →
// 证件照 token 落库 → 证件号 RSA 加密 → 写申请单 → 失效验证码与实名缓存。
func (r *Repository) RealnameApply(ctx context.Context, arg *RealnameApplyArg) error {
	if err := r.checkCapture(ctx, arg.Mid, arg.CaptureCode); err != nil {
		return err
	}
	cardMD5v := cardMD5(arg.CardCode, int(arg.CardType), int(arg.Country))
	info, err := r.realnameInfo(ctx, arg.Mid)
	if err != nil {
		return err
	}
	if info.Status == model.RealnameApplyStatusPending || info.Status == model.RealnameApplyStatusPass {
		return ErrRealnameApplyAlready
	}
	if err := r.checkCardDup(ctx, cardMD5v); err != nil {
		return err
	}

	// 证件照 token 落库（与参考仓库一致的 /idenfiles/<token>.txt 包装）
	handIMG := &model.RealnameApplyImage{IMGData: imgPrefix + arg.HandIMGToken + imgSuffix, CTime: time.Now().Unix()}
	frontIMG := &model.RealnameApplyImage{IMGData: imgPrefix + arg.FrontIMGToken + imgSuffix, CTime: time.Now().Unix()}
	backIMG := &model.RealnameApplyImage{IMGData: imgPrefix + arg.BackIMGToken + imgSuffix, CTime: time.Now().Unix()}
	if handIMG.IMGData != imgPrefix+imgSuffix {
		if err := r.applyImgModel.Insert(ctx, handIMG); err != nil {
			return err
		}
	}
	if frontIMG.IMGData != imgPrefix+imgSuffix {
		if err := r.applyImgModel.Insert(ctx, frontIMG); err != nil {
			return err
		}
	}
	if backIMG.IMGData != imgPrefix+imgSuffix {
		if err := r.applyImgModel.Insert(ctx, backIMG); err != nil {
			return err
		}
	}

	// 证件号加密（兼容已加密的 base64 输入：以 "=" 结尾视为密文透传）
	encryptedCard := arg.CardCode
	if !strings.HasSuffix(arg.CardCode, "=") {
		if arg.CardType == model.RealnameCardTypeIdentity && !isIDCard(arg.CardCode) {
			return ErrRealnameCardNumErr
		}
		enc, err := r.cryptor.CardEncrypt([]byte(arg.CardCode))
		if err != nil {
			return err
		}
		encryptedCard = string(enc)
	}

	apply := &model.RealnameApply{
		Mid:      arg.Mid,
		Realname: arg.Realname,
		Country:  arg.Country,
		CardType: arg.CardType,
		CardNum:  encryptedCard,
		CardMD5:  cardMD5v,
		HandIMG:  handIMG.ID,
		FrontIMG: frontIMG.ID,
		BackIMG:  backIMG.ID,
		Status:   model.RealnameApplyStatusPending,
		CTime:    time.Now().Unix(),
	}
	if err := r.applyModel.Insert(ctx, apply); err != nil {
		return err
	}
	_ = r.async.Do(ctx, func(c context.Context) {
		_ = r.cache.delCaptureCode(c, arg.Mid)
		_ = r.cache.delRealnameCache(c, arg.Mid)
	})
	return nil
}

// checkCapture 校验验证码（含错误次数限制，参考 service.checkCapture）。
func (r *Repository) checkCapture(ctx context.Context, mid int64, capture int) error {
	errTimes, err := r.cache.captureErrTimes(ctx, mid)
	if err != nil {
		return err
	}
	if errTimes > 3 {
		_ = r.async.Do(ctx, func(c context.Context) {
			_ = r.cache.delCaptureCode(c, mid)
		})
		return ErrRealnameCaptureErrTooMany
	}
	serverCode, err := r.cache.captureCode(ctx, mid)
	if err != nil {
		return err
	}
	if serverCode < 0 {
		return ErrRealnameCaptureInvalid
	}
	if capture != serverCode {
		_ = r.async.Do(ctx, func(c context.Context) {
			if errTimes < 0 {
				_ = r.cache.setCaptureErrTimes(c, mid, 0)
			}
			_ = r.cache.incrCaptureErrTimes(c, mid)
		})
		return ErrRealnameCaptureErr
	}
	return nil
}

// checkCardDup 证件号查重（参考 service.checkCardDup）。
func (r *Repository) checkCardDup(ctx context.Context, cardMD5v string) error {
	rawInfo, err := r.realnameModel.FindOneByCardMD5(ctx, cardMD5v)
	if err != nil {
		return err
	}
	if rawInfo != nil && (rawInfo.Status == model.RealnameApplyStatusPending || rawInfo.Status == model.RealnameApplyStatusPass) {
		return ErrRealnameCardBindAlready
	}
	return nil
}

// isIDCard 校验身份证号格式（15/18 位，参考 service.isIDCard）。
func isIDCard(card string) bool {
	return idCard15Regexp.MatchString(card) || idCard18Regexp.MatchString(card)
}

// cardMD5 计算证件号哈希（参考 service.cardMD5）。
func cardMD5(card string, cardType, country int) string {
	key := fmt.Sprintf("%s_%s_%d_%d", realnameSalt, strings.ToLower(card), cardType, country)
	return fmt.Sprintf("%x", md5.Sum([]byte(key)))
}

// parseIdentity 从身份证号解析生日与性别（参考 service.ParseIdentity）。
func parseIdentity(id string) (time.Time, string, error) {
	var ystr, mstr, dstr, gstr string
	switch len(id) {
	case 15:
		ystr, mstr, dstr = "19"+id[6:8], id[8:10], id[10:12]
		gstr = id[14:15]
	case 18:
		ystr, mstr, dstr = id[6:10], id[10:12], id[12:14]
		gstr = id[16:17]
	default:
		return time.Time{}, "", fmt.Errorf("identity id invalid: %s", id)
	}
	y, err := strconv.Atoi(ystr)
	if err != nil {
		return time.Time{}, "", err
	}
	m, err := strconv.Atoi(mstr)
	if err != nil {
		return time.Time{}, "", err
	}
	d, err := strconv.Atoi(dstr)
	if err != nil {
		return time.Time{}, "", err
	}
	g, err := strconv.Atoi(gstr)
	if err != nil {
		return time.Time{}, "", err
	}
	birthday := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.Local)
	if g%2 == 1 {
		return birthday, genderMale, nil
	}
	return birthday, genderFemale, nil
}

// isAdult 判断是否年满 18 周岁（参考 service.isAdult）。
func isAdult(birthday time.Time, anchor time.Time) (bool, error) {
	ny, nm, nd := anchor.Year(), anchor.Month(), anchor.Day()
	by, bm, bd := birthday.Year(), birthday.Month(), birthday.Day()
	if anchor.Before(birthday) || ny-by > 150 {
		return false, fmt.Errorf("birthday invalid: %v anchor %v", birthday, anchor)
	}
	return (ny-by == 18 && (nm > bm || (nm == bm && nd >= bd))) || (ny-by > 18), nil
}

// RealnameAlipay* 系列（芝麻认证渠道）在参考仓库仅通过 gorpc 暴露，不在
// gRPC 契约内；本项目不移植该渠道，realname_alipay_apply 表与对应逻辑
// 待支付宝渠道需求明确后再落地（见服务 README 的移植边界说明）。
