package logic

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go-video/common/ratelimit"
	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 本文件是 open-platform logic 层的共享件：门禁判定、边界裁剪、投影与游标编解码。
//
// 三条贯穿全章的硬约束（AGENTS.md §5/§9）：
//  1. 凭证只在「首次签发的非重放响应」里出现一次，任何投影/日志/事件都不含密钥材料；
//  2. 判定链一律 fail closed：拿不到 pepper、读不到库、没有匹配规则，都返回错误而不是放行；
//  3. 所有分页/批量都有配置化上限，服务端不执行无界扫描。

// nowUnix logic 层时钟。model 层的 nowUnix 未导出（写路径统一由 model 取时钟），
// 判定与投影要读时钟但不写时钟，因此这里独立取一次，避免跨包复制时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// deny_reason 固定枚举码：写入 op_api_call_log.deny_reason 与校验响应。
// 用固定码而不是 err.Error()，避免把内部原因（含表名/SQL 片段）外发。
const (
	denyNone             = ""
	denyInactive         = "inactive"
	denyExpired          = "expired"
	denyRevoked          = "revoked"
	denyAppSuspended     = "app_suspended"
	denyScopeMissing     = "scope_missing"
	denyConsentMissing   = "consent_missing"
	denyQuotaExceeded    = "quota_exceeded"
	denySignatureInvalid = "signature_invalid"
)

// 各类输入的长度上限（对齐 deploy/migrations/open-platform 的列宽，
// 超长在 logic 层就拒，避免 MySQL 截断出「存进去的和申请的不一样」的隐性数据损坏）。
const (
	maxAppNameRunes        = 64
	maxAppDescRunes        = 500
	maxClientTokenRunes    = 64
	maxIdempotencyRunes    = 128
	maxRequestIDRunes      = 128
	maxReasonRunes         = 255
	maxStateRunes          = 128
	maxEventIDRunes        = 64
	maxWebhookDescRunes    = 255
	maxAPICodeRunes        = 64
	maxScopeTokenRunes     = 64
	maxPathRunes           = 255
	maxNonceRunes          = 64
	maxAppKeyRunes         = 64
	maxBizRefRunes         = 64
	credentialBytes        = 32
	saltBytes              = 16
	challengeBytes         = 16
	appKeyBytes            = 12
	maxScopeCountPerReq    = 50
	maxRedirectURICount    = 100
	appKeyCollisionRetry   = 5
	tokenTypeBearer        = "Bearer"
	touchUsedMinInterval   = 60
	challengeTTLSeconds    = 900
	idempotencyTTLSeconds  = 86400
	maxQuotaWindowSeconds  = 86400 * 30
	defaultWebhookURLBytes = 512
	nonceKeyPrefix         = "op:nonce:"
	tokenLookupKeyPrefix   = "op:tknid:"
	idempotencyKeyPrefix   = "op:idem:"
)

// credentialPepper 取凭证哈希的 HMAC pepper。
//
// 未注入密钥材料时返回 ErrSecretVerificationUnavailable：这是全链路统一的 fail-closed 闸门，
// 对应 svc 注释「配置缺失不阻断启动，但哈希/验签路径必须报错」。
// 本服务没有下游 RPC 依赖（数据自治），因此「可选依赖为 nil」的失败面就是这里。
func credentialPepper(s *svc.ServiceContext) (string, error) {
	if s == nil || !s.Config.SecurityConfigured() {
		return "", model.ErrSecretVerificationUnavailable
	}
	return s.Config.Security.CredentialPepper, nil
}

// randomCredential 生成一枚 256bit 随机凭证明文（授权码 / access / refresh / client_secret）。
func randomCredential() (string, error) {
	return model.RandomHex(credentialBytes)
}

// randomSalt 生成入库用的随机盐（hex）。
func randomSalt() (string, error) {
	return model.RandomHex(saltBytes)
}

// newAppKey 生成公开标识。随机段保证即使自增主键被猜到也无法伪造 app_key 入口。
func newAppKey() (string, error) {
	rnd, err := model.RandomHex(appKeyBytes)
	if err != nil {
		return "", err
	}
	return "opk_" + rnd, nil
}

// ---------------------------------------------------------------- 应用读取

// findApp 读取应用；不存在回 ErrAppNotFound（调用方不必每处重复 nil 判定）。
func findApp(ctx context.Context, s *svc.ServiceContext, appID int64) (*model.Application, error) {
	if appID <= 0 {
		return nil, model.ErrInvalidAppID
	}
	app, err := s.Apps.FindByID(ctx, appID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, model.ErrAppNotFound
	}
	return app, nil
}

// requireActiveApp 应用必须处于 ACTIVE：待审/停用/驳回/下线一律拒绝签发与校验。
// 停用应用的 token 立即不可用，这是 Introspect/AuthorizeRequest/换码/刷新共用的门禁。
func requireActiveApp(app *model.Application) error {
	if app == nil || !app.IsActive() {
		return model.ErrApplicationNotActive
	}
	return nil
}

// grantedSubset 校验 want ⊆ 应用已获批 scope 集，缺失即 ErrScopeNotGranted。
// 这是「token 永远不可能带着应用没拿到的权限」的最小权限边界。
func grantedSubset(ctx context.Context, s *svc.ServiceContext, appID int64, want []string) error {
	if len(want) == 0 {
		return nil
	}
	granted, err := s.AppScopes.FindGrantedScopes(ctx, appID, want)
	if err != nil {
		return err
	}
	if len(granted) != len(want) {
		for _, w := range want {
			if !model.ContainsScope(granted, w) {
				return model.ErrScopeNotGranted
			}
		}
		return model.ErrScopeNotGranted
	}
	return nil
}

// ---------------------------------------------------------------- 边界与分页

// pageSize 归一每页条数：<=0 用配置默认值，超过上限直接拒绝（不静默裁剪）。
// 静默裁剪会让调用方以为自己拿到了全部数据，属于「宽松解释」，禁止。
func pageSize(s *svc.ServiceContext, ps int32) (int32, error) {
	cfg := s.Config.OpenPlatform
	if ps < 0 {
		return 0, model.ErrInvalidPage
	}
	if ps == 0 {
		if cfg.PageSize <= 0 {
			return 0, model.ErrInvalidPage
		}
		return cfg.PageSize, nil
	}
	if cfg.MaxPageSize > 0 && ps > cfg.MaxPageSize {
		return 0, model.ErrPsTooLarge
	}
	return ps, nil
}

// encodeCursor 把 (时间位点, 主键位点) 编成不透明游标。
// 值本身可被调用方读懂但无解释义务：游标只是分页位点，不承载权限语义。
func encodeCursor(timePos, idPos int64) string {
	raw := strconv.FormatInt(timePos, 10) + "," + strconv.FormatInt(idPos, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor 解析游标；空串表示首页。非法游标回 ErrInvalidCursor。
func decodeCursor(cursor string) (int64, int64, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, 0, nil
	}
	bs, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, 0, model.ErrInvalidCursor
	}
	parts := strings.SplitN(string(bs), ",", 2)
	if len(parts) != 2 {
		return 0, 0, model.ErrInvalidCursor
	}
	timePos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, model.ErrInvalidCursor
	}
	idPos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, model.ErrInvalidCursor
	}
	if timePos < 0 || idPos < 0 {
		return 0, 0, model.ErrInvalidCursor
	}
	return timePos, idPos, nil
}

// trimPage 处理「多取一条」的分页惯用法：返回列表、下一页游标、是否有下一页。
// lastKey 由调用方给出（不同表的主键列名不同），避免这里反射取字段。
func trimPage[T any](rows []T, ps int32, lastKey func(T) (int64, int64)) (list []T, next string, hasMore bool) {
	if int32(len(rows)) <= ps {
		if len(rows) == 0 {
			return nil, "", false
		}
		t, id := lastKey(rows[len(rows)-1])
		return rows, encodeCursor(t, id), false
	}
	page := rows[:ps]
	t, id := lastKey(page[len(page)-1])
	return page, encodeCursor(t, id), true
}

// clipRunes 按 rune 数裁剪到列宽内（MySQL 的 VARCHAR(n) 计字符数而非字节数）。
// 用于 reason/description 这类「审计文案」：宁可截断也不让整笔写失败，
// 但凭证类字段一律不用本函数——它们必须精确匹配，截断等于放宽。
func clipRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || utf8Len(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}

func utf8Len(s string) int { return len([]rune(s)) }

// requireLen 校验字符串非空且不超长（rune 计），返回可安全入库的结果。
// 空值与超长的哨兵由调用方给：同一个「必填」在不同方法里的语义不同
// （注册缺 name 是 InvalidArgument，缺 operator_mid 是 PermissionDenied）。
func requireLen(s string, max int, emptyErr, tooLongErr error) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", emptyErr
	}
	if utf8Len(s) > max {
		return "", tooLongErr
	}
	return s, nil
}

// optionalLen 同上，但允许空串（表示「不修改」）。
func optionalLen(s string, max int, tooLongErr error) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if utf8Len(s) > max {
		return "", tooLongErr
	}
	return s, nil
}

// ---------------------------------------------------------------- 身份门禁

// requireOperator 要求运营身份（配额规则、scope 审批、重算等运营面）。
func requireOperator(mid int64) error {
	if mid <= 0 {
		return model.ErrOperatorRequired
	}
	return nil
}

// requireOwnerOrOperator 要求调用者是应用归属者或运营。
// isOperator=true 时仍要求 operator_mid>0：审计必须能落到具体的人，
// 「匿名运营」在破坏性操作（改状态、轮换密钥、吊销）上不可接受。
func requireOwnerOrOperator(app *model.Application, mid int64, isOperator bool) error {
	if mid <= 0 {
		return model.ErrOwnerRequired
	}
	if isOperator {
		return nil
	}
	if app == nil || app.OwnerMid != mid {
		return model.ErrOwnerRequired
	}
	return nil
}

// ---------------------------------------------------------------- 写侧限流

// writePermit 申请一个进程级写令牌，返回必须调用的释放函数。
//
// 令牌桶保护的是 MySQL：本服务所有写路径共用同一桶。
// 无余量时返回 ErrRateLimited（可重试错误），不排队等待——开放平台的调用方
// 本来就要处理 429 语义，排队只会把超时压力转嫁给网关。
// 拿不到令牌时调用方必须在写库之前返回，因此「被限流的请求」既不扣配额也不写流水。
func writePermit(ctx context.Context, s *svc.ServiceContext) (func(), error) {
	if s == nil || s.WriteLimiter == nil {
		return nil, model.ErrRateLimited
	}
	done, err := s.WriteLimiter.Allow(ctx)
	if err != nil {
		return nil, model.ErrRateLimited
	}
	return func() { done(ratelimit.Success) }, nil
}

// ---------------------------------------------------------------- nonce 防重放

// claimNonce 在 Redis 上以 SETNX 占用一个 nonce。
//
// 返回 reused=false 表示「本次首次出现」。缓存不可用时返回错误而不是放行：
// nonce 是防重放的唯一屏障，缓存故障时放行等于放弃防重放（fail closed）。
func claimNonce(ctx context.Context, s *svc.ServiceContext, purpose, key, nonce string) (bool, error) {
	if s.Cache == nil {
		return false, model.ErrSecretVerificationUnavailable
	}
	ttl := int(s.Config.Security.NonceTTLSeconds)
	if ttl <= 0 {
		ttl = 600
	}
	ok, err := s.Cache.SetnxExCtx(ctx, nonceKeyPrefix+purpose+":"+key+":"+nonce, "1", ttl)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// ---------------------------------------------------------------- 幂等结果缓存

// saveIdempotency 记录首次结论（JSON）。失败只降级不报错：本域的硬幂等锚点
// 始终是数据库唯一键与状态机条件（uniq_app_scope / revoked_at=0 等），
// 缓存丢结果的后果是「重算」而不是「重复生效」。
func saveIdempotency(ctx context.Context, s *svc.ServiceContext, scope, key, result string) {
	if s.Cache == nil || key == "" {
		return
	}
	if err := s.Cache.SetexCtx(ctx, idempotencyKeyPrefix+scope+":"+key, result, idempotencyTTLSeconds); err != nil {
		logx.WithContext(ctx).Errorf("open-platform: 幂等结果写入失败 scope=%s: %v", scope, err)
	}
}

// claimIdempotency 占用幂等键。返回 (first, cachedResult, err)：
// first=false 时 cachedResult 是首次结论（可能为空串，表示结论未落盘，调用方按当前状态重算）。
func claimIdempotency(ctx context.Context, s *svc.ServiceContext, scope, key string) (bool, string, error) {
	if key == "" {
		return false, "", model.ErrIdempotencyKeyRequired
	}
	if s.Cache == nil {
		return false, "", model.ErrSecretVerificationUnavailable
	}
	full := idempotencyKeyPrefix + scope + ":" + key
	ok, err := s.Cache.SetnxExCtx(ctx, full, "", idempotencyTTLSeconds)
	if err != nil {
		return false, "", err
	}
	if ok {
		return true, "", nil
	}
	raw, err := s.Cache.GetCtx(ctx, full)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, "", nil
		}
		return false, "", err
	}
	return false, raw, nil
}

// ---------------------------------------------------------------- scope 校验

// normalizeScopes 规范化 scope 列表（去重升序）并校验条数上限与单个标识的字符合法性。
// 入参为空时返回 nil：由调用方决定「空表示继承」还是「空即拒绝」。
func normalizeScopes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxScopeCountPerReq {
		return nil, model.ErrTooManyScopes
	}
	for _, sc := range in {
		if err := validScopeToken(sc); err != nil {
			return nil, err
		}
	}
	return model.SplitScopes(model.JoinScopes(in)), nil
}

// validScopeToken 校验单个 scope/api_code 标识：长度与非空白字符。
// 允许点、下划线、冒号与连字符，拒绝空白与其它分隔符（逗号是列表分隔符，混进来会破坏
// JoinScopes/SplitScopes 的可逆性，进而让 FIND_IN_SET 定点撤销判定漏掉行）。
func validScopeToken(v string) error {
	v = strings.TrimSpace(v)
	if v == "" || utf8Len(v) > maxScopeTokenRunes {
		return model.ErrScopeUnknown
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == ':', c == '*':
		default:
			return model.ErrScopeUnknown
		}
	}
	return nil
}

// rejectForbiddenScopes 命中未开放类目（会员/订单/支付/投币/分成/广告）直接拒绝。
// 商业化红线在 logic 与 model 两层各拦一次：model.Upsert/QuotaPolicy 也调它，
// 保证绕过 logic 的运维直写同样落不了库。
func rejectForbiddenScopes(scopes []string) error {
	for _, sc := range scopes {
		if model.IsForbiddenScopeCategory(sc) {
			return model.ErrForbiddenScopeCategory
		}
	}
	return nil
}

// requireKnownScopes 校验 scope 全部存在于目录且已开放，返回目录定义。
func requireKnownScopes(ctx context.Context, s *svc.ServiceContext, scopes []string) (map[string]*model.Scope, error) {
	if len(scopes) == 0 {
		return map[string]*model.Scope{}, nil
	}
	defs, err := s.Scopes.FindByScopes(ctx, scopes)
	if err != nil {
		return nil, err
	}
	for _, sc := range scopes {
		def, ok := defs[sc]
		if !ok || def == nil {
			return nil, model.ErrScopeUnknown
		}
		if def.Enabled != 1 {
			return nil, model.ErrScopeDisabled
		}
	}
	return defs, nil
}

// requireWriteScopeConsent 写权限 scope 必须在目录里声明 requires_user_consent=1。
// 这是「用户勾一键同意全部也不能放行」的目录侧前提：目录里没声明同意的写 scope 一律不可授。
func requireWriteScopeConsent(defs map[string]*model.Scope, scopes []string) error {
	for _, sc := range scopes {
		def := defs[sc]
		if def == nil {
			continue
		}
		if def.IsWrite() && def.RequiresUserConsent != 1 {
			return model.ErrScopeWriteRequiresConsent
		}
	}
	return nil
}

// ---------------------------------------------------------------- URI 校验

// normalizeRedirectURIs 校验并规范化回调白名单，返回入库用的逗号串。
// 空列表是合法的（应用可以不做授权码模式），但非空时逐条必须过校验。
func normalizeRedirectURIs(s *svc.ServiceContext, in []string) (string, error) {
	if len(in) == 0 {
		return "", nil
	}
	cfg := s.Config.OpenPlatform
	if cfg.MaxRedirectURIs > 0 && int32(len(in)) > cfg.MaxRedirectURIs {
		return "", model.ErrTooManyRedirectURIs
	}
	if int32(len(in)) > maxRedirectURICount {
		// 配置被写成天文数字时的硬上限：白名单条数必须有服务端上界，
		// 否则「一次注册十万条回调」会把精确匹配退化成事实上的「任意地址」。
		return "", model.ErrTooManyRedirectURIs
	}
	seen := make(map[string]struct{}, len(in))
	list := make([]string, 0, len(in))
	for _, raw := range in {
		v, err := validPublicURI(raw, cfg.MaxRedirectURIBytes, model.ErrInvalidRedirectURI)
		if err != nil {
			return "", err
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		list = append(list, v)
	}
	if cfg.MaxRedirectURIs > 0 && int32(len(list)) > cfg.MaxRedirectURIs {
		return "", model.ErrTooManyRedirectURIs
	}
	sortStrings(list)
	return strings.Join(list, ","), nil
}

// validWebhookURL 校验回调端点地址：https only、禁止本机/内网字面地址（SSRF 护栏）。
func validWebhookURL(s *svc.ServiceContext, raw string) (string, error) {
	return validPublicURI(raw, defaultWebhookURLBytes, model.ErrInvalidWebhookURL)
}

// validPublicURI 是 redirect_uri 与 webhook url 共用的严格校验：
//   - 只允许 https（明文 http 会把凭证与授权码暴露在链路上）；
//   - 禁止 userinfo（user:pass@host 是凭证夹带）、禁止 fragment（片段不进服务端，
//     拿它做白名单匹配等于允许「同路径不同片段」绕过精确比对）；
//   - 禁止字面 IP 里的回环/私网/链路本地/CGNAT/组播段与 localhost 一类内部主机名，
//     避免把开放平台注册接口变成内网探测器（SSRF）；
//   - 不做 DNS 解析：本服务是授权面，不该依赖外部解析器；解析后的地址校验由投递 worker
//     在发起连接时再做一次（README 记录了这一分工）。
func validPublicURI(raw string, maxBytes int, invalidErr error) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", invalidErr
	}
	if maxBytes <= 0 {
		maxBytes = defaultWebhookURLBytes
	}
	if len(raw) > maxBytes {
		return "", model.ErrTooManyRedirectURIs
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", invalidErr
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", invalidErr
	}
	if u.User != nil || u.Fragment != "" {
		return "", invalidErr
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", invalidErr
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", invalidErr
	}
	if isInternalHostName(host) {
		return "", invalidErr
	}
	if ip := net.ParseIP(host); ip != nil && !model.IsPublicHostAddr(ip) {
		return "", invalidErr
	}
	return u.String(), nil
}

// isInternalHostName 识别「不可能指向公网」的主机名。
func isInternalHostName(host string) bool {
	switch {
	case host == "localhost", host == "localhost.localdomain", host == "ip6-localhost",
		host == "metadata", host == "metadata.google.internal":
		return true
	case strings.HasSuffix(host, ".localhost"), strings.HasSuffix(host, ".local"),
		strings.HasSuffix(host, ".internal"), strings.HasSuffix(host, ".in-addr.arpa"),
		strings.HasSuffix(host, ".home.arpa"), strings.HasSuffix(host, ".localdomain"):
		return true
	}
	return false
}

// sortStrings 升序排序（列表规模在几十条内，插入排序足够，且与 model 层同一手法）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// uriAllowed 判断回调地址是否逐字命中白名单（精确匹配，绝不做前缀/子串匹配）。
// 前缀匹配会把「https://app.example.com」的白名单变成「https://app.example.com.evil.tld」的通行证。
func uriAllowed(csv, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" || csv == "" {
		return false
	}
	for _, allowed := range strings.Split(csv, ",") {
		if strings.TrimSpace(allowed) == want {
			return true
		}
	}
	return false
}

// splitCSV 把逗号列还原成列表（丢空元素）。
func splitCSV(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- payload 护栏

// credentialKeyMarkers 是事件正文里不允许出现的键名片段。
// 命中即拒绝入队：投递正文会落库并按保留期清理，绝不能成为凭证的第二存储地。
var credentialKeyMarkers = []string{
	"client_secret", "secret_key", "access_token", "refresh_token", "private_key",
	"authorization", "password", "passwd", "credential", "cookie", "set-cookie",
}

// payloadHasCredentialKey 判断事件正文是否疑似夹带凭证字段。
func payloadHasCredentialKey(payload string) bool {
	if payload == "" {
		return false
	}
	low := strings.ToLower(payload)
	for _, m := range credentialKeyMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// idPart 校验 event_id / request_id 这类幂等键：非空白、长度受限、无控制字符。
// 幂等键会进 Redis 与唯一索引，允许空白或超长字符会让键空间出现歧义。
func idPart(v string, max int, emptyErr, tooLongErr error) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", emptyErr
	}
	if utf8Len(v) > max || strings.ContainsAny(v, " \t\r\n") {
		return "", tooLongErr
	}
	return v, nil
}

// firstErr 返回第一个非 nil 错误（清理路径聚合用）。
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// ---------------------------------------------------------------- 枚举投影

func appStatusToRPC(v int32) rpc.AppStatus { return rpc.AppStatus(v) }

func appStatusFromRPC(v rpc.AppStatus) int32 { return int32(v) }

// secretStateOf 密钥概览 → 对外状态。只依据行数与生效数，不含任何密钥材料。
func secretStateOf(sum *model.SecretSummary) rpc.SecretState {
	if sum == nil || sum.Total == 0 {
		return rpc.SecretState_SECRET_STATE_UNSET
	}
	if sum.Active > 0 {
		return rpc.SecretState_SECRET_STATE_CONFIGURED
	}
	return rpc.SecretState_SECRET_STATE_REVOKED
}

func eventTypeToRPC(v int32) rpc.WebhookEventType { return rpc.WebhookEventType(v) }

func deliveryStateToRPC(v int32) rpc.WebhookDeliveryState { return rpc.WebhookDeliveryState(v) }

func boolToInt8(v bool) int8 {
	if v {
		return 1
	}
	return 0
}

// projectApp 组装应用投影。
//
// grantedScopes 与 secret 概览都由调用方批量取好：本函数只做拼装，不查库，
// 这样列表接口可以一次批量取全部应用的数据，不产生 N+1（放大成未定义规模的扫描）。
// 投影里没有 salt/hash/secret 的位置——明文只存在于签发响应，这里连摘要都不回。
func projectApp(app *model.Application, grantedScopes []string, sum *model.SecretSummary) *rpc.ApplicationInfo {
	if app == nil {
		return nil
	}
	info := &rpc.ApplicationInfo{
		AppId:        app.AppID,
		AppKey:       app.AppKey,
		Name:         app.Name,
		Description:  app.Description,
		OwnerMid:     app.OwnerMid,
		Status:       appStatusToRPC(app.Status),
		RedirectUris: splitCSV(app.RedirectURIs),
		Scopes:       grantedScopes,
		SecretState:  secretStateOf(sum),
		Version:      app.Version,
		Ctime:        app.Ctime,
		Mtime:        app.Mtime,
		OfflineAt:    app.OfflineAt,
	}
	if sum != nil {
		info.SecretRotatedAt = sum.LastCtime
	}
	return info
}

func projectScope(def *model.Scope, grantedState int32) *rpc.ScopeInfo {
	if def == nil {
		return nil
	}
	return &rpc.ScopeInfo{
		Scope:               def.Scope,
		DisplayName:         def.DisplayName,
		Access:              rpc.ScopeAccess(def.Access),
		RiskLevel:           rpc.ScopeRiskLevel(def.RiskLevel),
		RequiresUserConsent: def.RequiresUserConsent == 1,
		Enabled:             def.Enabled == 1,
		Reason:              def.DisableReason,
		GrantedState:        grantedState,
	}
}

func projectQuotaPolicy(p *model.QuotaPolicy) *rpc.QuotaPolicyInfo {
	if p == nil {
		return nil
	}
	return &rpc.QuotaPolicyInfo{
		PolicyId:      p.PolicyID,
		AppId:         p.AppID,
		ApiCode:       p.APICode,
		WindowSeconds: p.WindowSeconds,
		Limit:         p.QuotaLimit,
		Enabled:       p.Enabled == 1,
		Operator:      p.Operator,
		Ctime:         p.Ctime,
		Mtime:         p.Mtime,
	}
}

// projectQuotaUsage 组装用量条目。
//
// limit 回显「当前生效限额」（调用方传入），而不是 limit_snapshot（记账时的解释值）：
// 运营要看的是「现在还能打多少次」，快照差异在 README 里单独说明。
func projectQuotaUsage(u *model.QuotaUsage, apiCode string, effectiveLimit int64) *rpc.QuotaUsageInfo {
	if u == nil {
		return nil
	}
	remaining := effectiveLimit - u.Used
	if remaining < 0 {
		remaining = 0
	}
	return &rpc.QuotaUsageInfo{
		AppId:         u.AppID,
		ApiCode:       apiCode,
		WindowSeconds: u.WindowSeconds,
		WindowStart:   u.WindowStart,
		Used:          u.Used,
		Limit:         effectiveLimit,
		Remaining:     remaining,
		UpdatedAt:     u.UpdatedAt,
	}
}

func projectWebhookEndpoint(e *model.WebhookEndpoint) *rpc.WebhookEndpointInfo {
	if e == nil {
		return nil
	}
	return &rpc.WebhookEndpointInfo{
		EndpointId:     e.EndpointID,
		AppId:          e.AppID,
		EventType:      eventTypeToRPC(e.EventType),
		Url:            e.URL,
		SignKeyVersion: e.SignKeyVersion,
		Enabled:        e.Enabled == 1,
		Description:    e.Description,
		VerifiedAt:     e.VerifiedAt,
		Ctime:          e.Ctime,
		Mtime:          e.Mtime,
	}
}

func projectWebhookDelivery(d *model.WebhookDelivery) *rpc.WebhookDeliveryInfo {
	if d == nil {
		return nil
	}
	return &rpc.WebhookDeliveryInfo{
		DeliveryId:     d.DeliveryID,
		AppId:          d.AppID,
		EndpointId:     d.EndpointID,
		EventType:      eventTypeToRPC(d.EventType),
		EventId:        d.EventID,
		PayloadDigest:  d.PayloadDigest,
		State:          deliveryStateToRPC(d.State),
		Attempt:        d.Attempt,
		MaxAttempts:    d.MaxAttempts,
		NextRetryAt:    d.NextRetryAt,
		LastStatusCode: d.LastStatusCode,
		LastError:      d.LastError,
		Ctime:          d.Ctime,
		Mtime:          d.Mtime,
	}
}

// projectTokenSet 组装一次性签发的令牌组。
//
// 明文 access/refresh 只在这里进响应；调用方必须保证本函数只在
// 「首次签发、非幂等重放」的路径上被调用（重放路径回空串）。
func projectTokenSet(tokenID, grantID, issuedAt, accessExp, refreshExp int64,
	scopes []string, accessToken, refreshToken string) *rpc.TokenSet {
	return &rpc.TokenSet{
		AccessToken:      accessToken,
		TokenType:        tokenTypeBearer,
		ExpiresIn:        nonNeg(accessExp - issuedAt),
		RefreshToken:     refreshToken,
		RefreshExpiresIn: nonNeg(refreshExp - issuedAt),
		Scope:            scopes,
		GrantId:          grantID,
		TokenId:          tokenID,
		IssuedAt:         issuedAt,
	}
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// ---------------------------------------------------------------- access 定位缓存
//
// 缓存语义与 blueprint 的偏差（README 已登记）：只缓存「access_hash → token_id」这一
// 定位结果，不缓存判定结论。理由是判定结论里 state 是会变的（单条撤销只改 op_token.state，
// 不动 grant 位点），缓存结论等于把「已撤销」延后传播，违反「撤销在所有路径立即失效」。
// 缓存值本身不含任何凭证明文（键是带 pepper 的 HMAC 摘要，值是主键 ID）。

// lookupTokenID 读 hash→token_id 缓存；未命中或读失败返回 0（回到 DB 真值，不放宽）。
func lookupTokenID(ctx context.Context, s *svc.ServiceContext, accessHash string) int64 {
	if s.Cache == nil || accessHash == "" {
		return 0
	}
	raw, err := s.Cache.GetCtx(ctx, tokenLookupKeyPrefix+accessHash)
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			logx.WithContext(ctx).Errorf("open-platform: token 定位缓存读取失败: %v", err)
		}
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// rememberTokenID 写 hash→token_id 定位缓存（TTL 由配置封顶在 AccessTokenTTL 内）。
func rememberTokenID(ctx context.Context, s *svc.ServiceContext, accessHash string, tokenID, expiresAt, now int64) {
	if s.Cache == nil || accessHash == "" || tokenID <= 0 {
		return
	}
	ttl := int(s.Config.OpenPlatform.IntrospectCacheSeconds)
	if ttl <= 0 {
		return
	}
	if left := expiresAt - now; left > 0 && left < int64(ttl) {
		ttl = int(left)
	}
	if ttl <= 0 {
		return
	}
	if err := s.Cache.SetexCtx(ctx, tokenLookupKeyPrefix+accessHash, strconv.FormatInt(tokenID, 10), ttl); err != nil {
		// 定位缓存写失败只影响性能：判定本身全部走 DB，结论已经成立。
		logx.WithContext(ctx).Errorf("open-platform: token 定位缓存写入失败: %v", err)
	}
}

// dropIntrospection 主动失效定位缓存（撤销路径调用，属可选优化，正确性不依赖它）。
func dropIntrospection(ctx context.Context, s *svc.ServiceContext, accessHashes ...string) {
	if s.Cache == nil {
		return
	}
	for _, h := range accessHashes {
		if h == "" {
			continue
		}
		if _, err := s.Cache.DelCtx(ctx, tokenLookupKeyPrefix+h); err != nil {
			logx.WithContext(ctx).Errorf("open-platform: token 定位缓存失效失败: %v", err)
		}
	}
}

// touchTokenUsed 限频回写 token 使用时间：同一 token 距上次 >=60s 才写一次。
// 校验是每请求都走的热点路径，不限频会把观测写变成主要负载。
// 判定用 DB 读到的 last_used_at（刚读出来的真值，不额外查询）。
func touchTokenUsed(ctx context.Context, s *svc.ServiceContext, tok *model.Token, now int64) {
	if s.Tokens == nil || tok == nil || tok.TokenID <= 0 {
		return
	}
	if tok.LastUsedAt > 0 && now-tok.LastUsedAt < touchUsedMinInterval {
		return
	}
	if err := s.Tokens.TouchUsed(ctx, tok.TokenID, now); err != nil {
		logx.WithContext(ctx).Errorf("open-platform: token %d 使用时间回写失败: %v", tok.TokenID, err)
	}
}
