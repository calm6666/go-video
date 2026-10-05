package logic

import (
	"context"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// maxRotationGraceSeconds 轮换宽限期的硬上界（1 天）。
// 宽限期是「泄露的旧密钥仍可用」的时间窗，必须有服务端上界，
// 否则一次误传（grace_seconds=31536000）就把应急手段变成了长期后门。
const maxRotationGraceSeconds int64 = 86400

type RotateApplicationSecretLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRotateApplicationSecretLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RotateApplicationSecretLogic {
	return &RotateApplicationSecretLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 轮换密钥（旧密钥宽限期后可用性明确）。
//
// 契约依据：proto:224-242（新密钥立即生效、旧密钥 grace_seconds 后失效、0 表示立即失效、
// 响应一次性返回新明文）、README:52（本方法不幂等，以最后一次为准）。
func (l *RotateApplicationSecretLogic) RotateApplicationSecret(in *rpc.RotateApplicationSecretReq) (*rpc.RotateApplicationSecretReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 门禁全部先于读写：非法入参必须零副作用（既不生成密钥材料也不前移版本）。
	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}
	// 下线是终态（proto:34 状态机注释 + model.CanTransitionAppStatus 的「OFFLINE 无出边」），
	// 重新接入必须重新注册拿新 app_id，因此不给死身份续密钥。
	// 其余状态（待审/驳回/停用）都可以轮换：密钥卫生操作不是「对外服务能力」，
	// 停用中的应用恰恰最需要换掉疑似泄露的密钥。
	if app.Status == model.AppStatusOffline {
		return nil, model.ErrInvalidStateTransition
	}
	if err := requireOwnerOrOperator(app, in.OperatorMid, in.IsOperator); err != nil {
		return nil, err
	}
	// reason 必填：审计列 rotate_reason（VARCHAR(255)）要能区分「例行轮换」与「泄露应急」。
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	grace, err := rotationGrace(in.GraceSeconds)
	if err != nil {
		return nil, err
	}

	// 2. 密钥材料闸门：没有 pepper 就不允许换出一把「看似有哈希、实际可离线爆破」的密钥。
	//    新密钥独立 salt，明文只在本函数返回值里存在一次（proto:17-18）。
	pepper, err := credentialPepper(s)
	if err != nil {
		return nil, err
	}
	fresh, err := secretCredential(pepper)
	if err != nil {
		return nil, err
	}

	// 3. 现存生效密钥：宽限期内可能同时有两把（上一次轮换尚未到期）。
	//    FindActive 按 secret_id 倒序，首元素就是本次被替换的那一把。
	now := nowUnix()
	actives, err := s.Secrets.FindActive(ctx, app.AppID, now)
	if err != nil {
		return nil, err
	}
	var oldID, oldExpires int64
	if len(actives) > 0 {
		oldID = actives[0].SecretID
		// grace=0 时旧密钥当场进历史，回 now 而不是回 0：
		// 0 在契约里表示「无到期」，调用方会把「立即失效」误读成「长期有效」。
		oldExpires = now
		if grace > 0 {
			oldExpires = now + grace
		}
	}
	// 全部旧密钥统一到本次请求声明的截止时刻（只缩短、不延长既有宽限期）。
	deadline := int64(0)
	if grace > 0 {
		deadline = now + grace
	}
	newExpires := secretExpiresAt(s.Config.OpenPlatform.SecretValidDays, now)

	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	// 4. 事务边界：新密钥先落、旧密钥后置历史。中途失败最坏是「两把都可用」，
	//    反过来（先废旧再签新）会留下「一把都没有」的永久不可用身份。
	var newID int64
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		id, err := s.Secrets.InsertTx(tctx, session, &model.AppSecret{
			AppID:        app.AppID,
			Salt:         fresh.salt,
			Hash:         fresh.hash,
			ExpiresAt:    newExpires,
			RotateReason: reason,
			OperatorMid:  in.OperatorMid,
		})
		if err != nil {
			return err
		}
		newID = id
		for _, old := range actives {
			if old == nil || old.SecretID == id {
				continue
			}
			if err := s.Secrets.MarkHistoryTx(tctx, session, old.SecretID, deadline); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 5. 版本前移在事务之后：Apps.NextVersion 的 model 签名不带 session，无法并入本事务
	//    （与 GrantApplicationScopes 同一口径，理由见该文件 apply 的注释）。
	//    事务失败时版本不动，最坏是白重算一次投影；反过来会留下「版本说已变、密钥还是旧的」的假失效。
	if _, err := s.Apps.NextVersion(ctx, app.AppID); err != nil {
		return nil, err
	}

	// 6. 日志只记标识与结论：明文、salt、hash 一律不落（AGENTS.md §7）。
	logx.WithContext(ctx).Infof("open-platform: 密钥轮换 app_id=%d new_secret_id=%d old_secret_id=%d "+
		"old_expires_at=%d operator_mid=%d is_operator=%t reason=%q",
		app.AppID, newID, oldID, oldExpires, in.OperatorMid, in.IsOperator, reason)
	return &rpc.RotateApplicationSecretReply{
		ClientSecret:       fresh.plain,
		SecretId:           newID,
		OldSecretId:        oldID,
		OldSecretExpiresAt: oldExpires,
		RotatedAt:          now,
	}, nil
}

// rotationGrace 归一并校验轮换宽限秒数。
//
// 为什么不用 OpenPlatform.RotationGraceSeconds 做默认值：proto:231 把 grace_seconds=0
// 明确定义为「立即失效」，而 proto3 无法区分「未传」与「传 0」。
// 一旦用配置补默认值，就把调用方显式表达的「泄露应急（当场下线旧密钥）」改成数小时宽限，
// 属于擅自放宽安全语义。配置项对本路径无效这一点已写进交付报告。
func rotationGrace(v int64) (int64, error) {
	if v < 0 || v > maxRotationGraceSeconds {
		return 0, errGraceTooLong
	}
	return v, nil
}
