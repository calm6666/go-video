package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// errPathTooLong 请求路径超出流水列宽（op_api_call_log.path VARCHAR(255)）。
// 与「缺 method/path」分开：前者是入参形状非法，后者是必填缺失，
// 两条混成一个哨兵会让调用方以为「补上路径就行」而实际上要改的是请求长度。
var errPathTooLong = errors.New("open-platform: path too long")

type AuthorizeRequestLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAuthorizeRequestLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuthorizeRequestLogic {
	return &AuthorizeRequestLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 网关前置聚合检查：凭证 + scope + 配额扣减 + 调用流水（request_id 幂等）。
//
// 契约依据：proto:384-415（两种凭证模式、request_id 唯一即幂等、响应字段）、
// proto:27-28（配额是投影，丢失只短时放宽限额）、README:61（无生效规则 → ErrQuotaPolicyNotFound
// 的 fail-closed 口径）。门禁顺序本身即契约：参数 → 幂等回放 → 凭证 → scope → 配额 → 流水。
//
// 本期只开放 access_token（用户授权）模式：应用级签名模式在参数阶段即返回
// model.ErrSignatureModeUnavailable（见 signatureModeUnavailable 的说明），
// 不消耗 nonce、不扣配额、不写流水。
func (l *AuthorizeRequestLogic) AuthorizeRequest(in *rpc.AuthorizeRequestReq) (*rpc.AuthorizeRequestReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 参数形状先于任何读写：非法入参零副作用。
	//    request_id 是配额扣减的唯一幂等锚点，缺失时禁止服务端补随机值绕过（proto:399）。
	requestID, err := idPart(in.RequestId, maxRequestIDRunes,
		model.ErrRequestIDRequired, model.ErrRequestIDRequired)
	if err != nil {
		return nil, err
	}
	apiCode, err := requireLen(in.ApiCode, maxAPICodeRunes, errAPICodeRequired, errAPICodeRequired)
	if err != nil {
		return nil, err
	}
	method := strings.TrimSpace(in.Method)
	path := strings.TrimSpace(in.Path)
	if method == "" || path == "" {
		return nil, errMethodPathRequired
	}
	if utf8Len(path) > maxPathRunes {
		return nil, errPathTooLong
	}
	requiredScope := strings.TrimSpace(in.RequiredScope)
	if requiredScope != "" {
		if err := validScopeToken(requiredScope); err != nil {
			return nil, err
		}
	}
	// 商业化红线（AGENTS.md §1）在判定前拦一次：命中未开放类目的 api_code/scope
	// 不允许进入配额扣减与流水，否则「被拒的调用」也会积累成可用配额消耗。
	if err := rejectForbiddenScopes([]string{apiCode, requiredScope}); err != nil {
		return nil, err
	}

	// 2. 凭证模式二选一，不做「猜一种」（proto:385-386）。
	tokenMode := strings.TrimSpace(in.AccessToken) != ""
	signatureMode := in.AppKey != "" || in.Signature != "" || in.Timestamp != 0 || in.Nonce != ""
	if tokenMode == signatureMode {
		// 都不给（无从判定身份）或都给（两套结论可能不一致）都是非法组合。
		return nil, model.ErrInvalidGrantType
	}
	if signatureMode {
		return nil, signatureModeUnavailable(in)
	}

	// 3. 幂等回放：命中 uniq_request_id 即原样重放首次判定，不再扣配额、不再写流水。
	//    这一步必须在任何写之前——网关重投与客户端重试只允许产生一次真实计数。
	existing, err := s.CallLogs.FindByRequestID(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return l.replay(ctx, existing)
	}

	// 4. 凭证与 scope 判定：复用 IntrospectToken 的判定链（tokencheck.go），不另写一套。
	//    api_code 在这里只作为配额键：仓库没有「接口 → 所需权限点」目录，
	//    服务端无法由 api_code 推导 required_scope，因此该值必须由网关给出（README 已知缺口）。
	res, err := evaluateAccessToken(ctx, s, strings.TrimSpace(in.AccessToken), 0, requiredScope)
	if err != nil {
		return nil, err
	}

	// 4b. 凭证不通过：这是业务结论（gRPC OK），不是错误。
	//    能定位到应用时仍落一条拒绝流水（观测与风控要看得到），定位不到时无 app_id 可写
	//    （op_api_call_log 的 app_id NOT NULL 且 model 拒绝 <=0），只回结论。
	if !res.active || res.token == nil {
		deny := res.deny
		if deny == "" {
			deny = denyInactive
		}
		var appID, mid, tokenID, grantID int64
		if res.token != nil {
			appID, mid, tokenID, grantID = res.token.AppID, res.token.Mid, res.token.TokenID, res.token.GrantID
		}
		if appID <= 0 {
			logx.WithContext(ctx).Infof("open-platform: 授权拒绝 request_id=%s api_code=%s deny=%s "+
				"(无应用归属，不落流水)", requestID, apiCode, deny)
			return &rpc.AuthorizeRequestReply{Allowed: false, DenyReason: deny}, nil
		}
		release, err := writePermit(ctx, s)
		if err != nil {
			return nil, err
		}
		defer release()
		callLogID, raced, err := l.persist(in, requestID, apiCode, method, path, appID, mid, tokenID, grantID,
			model.CallDenied, deny, 0, 0)
		if err != nil {
			return nil, err
		}
		if raced != nil {
			return l.replay(ctx, raced)
		}
		return &rpc.AuthorizeRequestReply{
			Allowed: false, DenyReason: deny, AppId: appID, Mid: mid,
			Scope: res.scopes, CallLogId: callLogID,
		}, nil
	}

	// 5. 写阶段：配额扣减 + 流水都写库，因此统一先取进程级写令牌。
	//    被限流的请求既不扣配额也不写流水（proto 语义：限流发生在写库之前）。
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	now := nowUnix()
	// 6. 配额扣减：生效层级内每条规则各扣一次，任一超限即拒（quota.go 是唯一实现，
	//    与 ListQuotaUsage 共用，保证「看到的限额」与「实际扣的限额」不漂移）。
	//
	//    fail closed 的边界：chargeQuota 返回错误（含 ErrQuotaPolicyNotFound「一条规则都没匹配上」）
	//    时本方法显式回错，绝不伪造「配额通过」。缺规则是运营配置事故，
	//    回错误让网关告警，比静默放行或静默拒绝都可解释（README:141 的全局兜底 seed 即为此存在）。
	dec, err := chargeQuota(ctx, s, res.token.AppID, apiCode, now)
	if err != nil {
		return nil, err
	}
	limit, remaining, resetAt := int64(0), int64(0), int64(0)
	if w, ok := dec.tightest(); ok {
		limit, remaining, resetAt = w.limit, w.remaining, w.windowEnd
	}

	// 6b. 超限：只拒本次请求，不回滚已发生的记账——投影必须如实反映「被打过来的次数」，
	//     藏起被拒用量等于让运营看不见冲击（quota.go 文件头第 3 条）。
	if w, bad := dec.blocked(); bad {
		// 两类拒绝（used 超 limit / 显式 quota_limit<=0 即「禁用该接口」）对客户端是同一个
		// 可执行动作：「现在别打了，等窗口结束」，因此共用 deny_quota_exceeded + retry_after 提示。
		callLogID, raced, err := l.persist(in, requestID, apiCode, method, path,
			res.token.AppID, res.token.Mid, res.token.TokenID, res.token.GrantID,
			model.CallDenied, denyQuotaExceeded, nonNeg(w.limit), 0)
		if err != nil {
			return nil, err
		}
		if raced != nil {
			return l.replay(ctx, raced)
		}
		logx.WithContext(ctx).Infof("open-platform: 配额拒绝 app_id=%d mid=%d api_code=%s limit=%d used=%d "+
			"window=%ds request_id=%s", res.token.AppID, res.token.Mid, apiCode, w.limit, w.used,
			w.windowSeconds, requestID)
		return &rpc.AuthorizeRequestReply{
			Allowed:           false,
			DenyReason:        denyQuotaExceeded,
			AppId:             res.token.AppID,
			Mid:               res.token.Mid,
			Scope:             res.scopes,
			QuotaLimit:        nonNeg(w.limit),
			WindowResetAt:     w.windowEnd,
			RetryAfterSeconds: dec.retryAfter(now),
			CallLogId:         callLogID,
		}, nil
	}

	// 7. 放行：流水落库（唯一事实源，配额可由它重算）。
	callLogID, raced, err := l.persist(in, requestID, apiCode, method, path,
		res.token.AppID, res.token.Mid, res.token.TokenID, res.token.GrantID,
		model.CallAllowed, denyNone, limit, remaining)
	if err != nil {
		return nil, err
	}
	if raced != nil {
		return l.replay(ctx, raced)
	}
	return &rpc.AuthorizeRequestReply{
		Allowed:        true,
		AppId:          res.token.AppID,
		Mid:            res.token.Mid,
		Scope:          res.scopes,
		QuotaLimit:     limit,
		QuotaRemaining: remaining,
		WindowResetAt:  resetAt,
		CallLogId:      callLogID,
	}, nil
}

// replay 按已落库的首次判定组装响应（不再扣配额、不再写流水）。
//
// 三个字段无法重放：scope / window_reset_at / retry_after_seconds —— 冻结的
// op_api_call_log 里没有对应列（无 scope 快照，也无窗口长度与窗口终点）。
// 这里不假装能还原：scope 回空列表，等待提示改为「按当前生效层级只读算一次」（peekQuota 不扣减），
// 拿不到就留 0。该缺口已登记进交付报告，补它需要新增流水列（属迁移评审）。
func (l *AuthorizeRequestLogic) replay(ctx context.Context, row *model.ApiCallLog) (*rpc.AuthorizeRequestReply, error) {
	reply := &rpc.AuthorizeRequestReply{
		Allowed:        row.Allowed == model.CallAllowed,
		DenyReason:     row.DenyReason,
		AppId:          row.AppID,
		Mid:            row.Mid,
		QuotaLimit:     row.QuotaLimit,
		QuotaRemaining: row.QuotaRemaining,
		CallLogId:      row.CallLogID,
	}
	if !reply.Allowed && row.DenyReason == denyQuotaExceeded {
		// 只为给出一个可执行的等待提示，不参与判定结论；只读，不产生任何计数。
		if wins, err := peekQuota(ctx, l.svcCtx, row.AppID, row.APICode, nowUnix(), 0); err == nil {
			dec := &quotaDecision{windows: wins}
			if w, bad := dec.blocked(); bad {
				reply.WindowResetAt = w.windowEnd
				reply.RetryAfterSeconds = dec.retryAfter(nowUnix())
			}
		} else {
			logx.WithContext(ctx).Errorf("open-platform: 重放时配额窗口回读失败 request_id=%s: %v",
				row.RequestID, err)
		}
	}
	logx.WithContext(ctx).Infof("open-platform: AuthorizeRequest 命中幂等重放 request_id=%s call_log_id=%d "+
		"allowed=%t deny=%s", row.RequestID, row.CallLogID, reply.Allowed, reply.DenyReason)
	return reply, nil
}

// persist 写一条调用流水，返回 (call_log_id, 并发重放到的既有行, err)。
//
// created=false 只可能来自「同一 request_id 的并发提交」：先落库者的判定才是权威结论，
// 本线程的判定（含已扣的一次配额投影）直接丢弃并回它的行——
// 否则同一个 request_id 会因线程先后而返回两种结论。
// 这里的重复扣减只发生在毫秒级并发的极小概率窗口，且 op_quota_usage 是投影
// （proto:27-28 明确「计数丢失只会短时放宽限额」），可由 RecomputeQuota 从流水收敛。
//
// 冻结的 op_api_call_log 没有「配额窗口终点」列，因此 window_reset_at 只回给调用方、
// 不落库——这也是幂等重放无法还原该字段的原因（见 replay）。
func (l *AuthorizeRequestLogic) persist(in *rpc.AuthorizeRequestReq, requestID, apiCode, method, path string,
	appID, mid, tokenID, grantID int64, allowed int8, deny string, limit, remaining int64) (int64,
	*model.ApiCallLog, error) {
	ctx, s := l.ctx, l.svcCtx
	now := nowUnix()
	id, created, err := s.CallLogs.Insert(ctx, &model.ApiCallLog{
		RequestID:  requestID,
		AppID:      appID,
		APICode:    apiCode,
		Mid:        mid,
		TokenID:    tokenID,
		GrantID:    grantID,
		Allowed:    allowed,
		DenyReason: deny,
		Method:     method,
		Path:       path,
		// 只落摘要与脱敏 IP：请求体原文与 token/secret 明文一律不入流水（AGENTS.md §7）。
		BodyDigest:     model.NormalizeBodyDigest(in.BodyDigest),
		ClientIPMasked: model.MaskClientIP(in.ClientIp),
		QuotaLimit:     limit,
		QuotaRemaining: remaining,
		Ctime:          now,
	})
	if err != nil {
		return 0, nil, err
	}
	if created {
		return id, nil, nil
	}
	existing, err := s.CallLogs.FindByRequestID(ctx, requestID)
	if err != nil {
		return 0, nil, err
	}
	logx.WithContext(ctx).Errorf("open-platform: request_id 并发重复提交，重放先落库的判定 request_id=%s", requestID)
	return id, existing, nil
}

// signatureModeUnavailable 应用级签名模式的失败关闭点。
//
// 为什么不能实现（不是「还没写」）：契约要求 signature = HMAC-SHA256(明文 client_secret, canonical)
// （proto:386、README:124-128），而服务端按 AGENTS.md §5/§7 只存
// HMAC(pepper, salt||明文) 的单向哈希（model/op_app_secret.go 文件头），
// 既拿不到明文也拿不到可解密的密钥材料，冻结的 DDL 里也没有存公钥的列。
// 用哈希当 HMAC 密钥去重算会得到一把「谁也算不对」的键，等于把该模式永久判死却仍消耗 nonce、
// 扣配额、写流水——所以这里在参数阶段就返回 ErrSignatureModeUnavailable。
//
// 修复需要契约级评审（改为 HMAC(哈希, canonical)，或 Ed25519 公钥签名），
// 本轮不擅自改 proto/DDL。in 只用于日志标识，绝不落签名与 nonce 原文之外的凭证材料。
func signatureModeUnavailable(in *rpc.AuthorizeRequestReq) error {
	logx.Errorf("open-platform: 签名模式不可用（服务端无法还原 client_secret 明文）app_key=%s request_id=%s",
		strings.TrimSpace(in.AppKey), strings.TrimSpace(in.RequestId))
	return model.ErrSignatureModeUnavailable
}
