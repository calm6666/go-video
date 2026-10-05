// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"strconv"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ValidateEventSchemaLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewValidateEventSchemaLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ValidateEventSchemaLogic {
	return &ValidateEventSchemaLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 单事件干跑校验：不落库不投递，返回缺失字段与归一化结果。
//
// 与采集路径共用 validateEvent（helpers.go），刻意不另写一套规则：
// 两套规则必然漂移，最后变成「干跑通过、真跑被拒」，网关自检就成了误导。
// 采样在这里只作为 note 提示，不改变 decision（proto 口径：valid=true 即 ACCEPTED）。
func (l *ValidateEventSchemaLogic) ValidateEventSchema(in *rpc.ValidateEventSchemaReq) (*rpc.ValidateEventSchemaReply, error) {
	if in == nil || in.GetEvent() == nil {
		return nil, model.ErrEventIDRequired
	}
	source := in.GetSource()
	if source == rpc.Source_SOURCE_UNSPECIFIED {
		source = rpc.Source_SOURCE_CLIENT
	}
	if !model.ValidSource(int32(source)) {
		return nil, model.ErrSourceNotAllowed(int32(source))
	}
	lim, err := resolveLimits(l.ctx, l.svcCtx, "")
	if err != nil {
		return nil, err
	}
	// 干跑同样要取盐：脱敏字段参与「归属主体」判定，缺盐时算不出 device_hash，
	// 结论会与真跑相反（干跑报 MISSING_SUBJECT、真跑能收）。绝不静默退化成无盐哈希。
	salt, saltVersion, err := resolveSalt(l.svcCtx, lim)
	if err != nil {
		return nil, err
	}
	// context 可空（proto 注释）：空上下文即「无 mid 无设备号」，由 validateEvent 判
	// REJECT_MISSING_SUBJECT，不在这里伪造一个合法主体。
	pv, err := desensitize(salt, saltVersion, in.GetContext(), lim.ipSegmentBits)
	if err != nil {
		return nil, err
	}
	now := timeNow()
	v := validateEvent(in.GetEvent(), int32(source), pv, lim, now, "")

	out := &rpc.ValidateEventSchemaReply{
		Valid:         v.decision == model.DecisionAccepted,
		Decision:      rpc.EventDecision(v.decision),
		Reason:        rpc.RejectReason(v.reason),
		MissingFields: v.missing,
		EventType:     v.eventType,
		Topic:         v.topic,
	}
	if v.rec != nil {
		out.SchemaVersion = v.rec.SchemaVersion
	}
	notes := append([]string(nil), v.notes...)
	if lim.activeIsConfig {
		notes = append(notes, msgPolicyNoActive)
	}
	if lim.stalePolicyHint {
		notes = append(notes, msgPolicyHintStale)
	}
	if out.Valid {
		// 采样预览：让埋点方看得见「这条真跑会不会被采样丢掉」，但不改 decision 语义。
		bps := model.EffectiveSampleBps(lim.sampleRules, v.eventType,
			isQualityCategory(rpc.BehaviorCategory(v.rec.Category)))
		if bps < 0 {
			bps = lim.defaultSampleBps
		}
		if !model.SampleHit(v.eventID, bps) {
			notes = append(notes, "采样预览：sample_bps="+strconv.Itoa(int(bps))+
				"，真跑该 event_id 会被 SAMPLED_OUT")
		} else {
			notes = append(notes, "采样预览：sample_bps="+strconv.Itoa(int(bps))+"（命中保留）")
		}
	} else if v.message != "" {
		notes = append([]string{v.message}, notes...)
	}
	out.Note = fitColumn(strings.Join(notes, "; "), maxReasonBytes)
	return out, nil
}
