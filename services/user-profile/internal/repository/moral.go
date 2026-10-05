package repository

// 本文件移植自参考仓库 service/moral.go + dao/mysql.go + dao/memcache.go 的
// 节操值聚合逻辑：变更走事务（读-改-写 + 日志），变更后失效缓存并按阈值
// 触发用户通知（Outbox 事件，notification 服务待接入）。

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// 节操阈值通知文案（移植自参考仓库 model/moral.go 的 Notice，文案去外部链接）。
const (
	less6000Title        = "你的节操值已低于60"
	less6000Message      = "抱歉，你的节操值已低于60，社交类功能将不能正常使用，更多加减明细请查看节操记录。"
	less3000Title        = "你的节操值已低于30"
	less3000Message      = "抱歉，你的节操值已低于30，社交类功能将不能正常使用，更多加减明细请查看节操记录。"
	greater6000Title     = "你的节操值已恢复至60以上"
	greater6000Message   = "恭喜，你的节操值已恢复至60以上，所有功能将恢复正常使用，更多加减明细请查看节操记录。"
	punishmentTitle      = "你被举报处理扣除了%s节操值"
	punishmentMessage    = "由于发布了违规内容，你被举报处理扣除了%s节操值，具体原因请看节操记录。"
	sysPunishmentTitle   = "你被系统处理扣除了%s节操值"
	sysPunishmentMessage = "由于发布了违规内容，你被系统处理扣除了%s节操值，具体原因请看节操记录。"
	rewardTitle          = "你举报的%s已被处理"
	rewardMessage        = "您举报的%s已被管理员处理，获得了%s节操值奖励，具体详情请看节操记录。"
)

// moralNoticeType 按原因类型映射通知类型（参考 ReasonTypes.NotifyType）。
func moralNoticeType(reasonType int64) (name, notifyType string, ok bool) {
	switch reasonType {
	case model.DMReasonType:
		return "弹幕", "2_1_4", true
	case model.ReplyReasonType:
		return "评论", "2_1_3", true
	default:
		// TAG/电波/账号/管理系统等类型不触发站内通知
		return "", "", false
	}
}

// Moral 查询节操值（缓存→DB，不存在按默认值，参考 service.Moral）。
func (r *Repository) Moral(ctx context.Context, mid int64) (*rpc.MoralReply, error) {
	var payload model.UserMoral
	if err := r.cache.GetJSON(ctx, keyMoral(mid), &payload); err != nil {
		logx.Errorf("user-profile/moral: get moral cache mid=%d err=%v", mid, err)
	}
	if payload.Mid != 0 {
		return toMoralReply(&payload), nil
	}
	moral, err := r.moralModel.FindOne(ctx, mid)
	if err != nil {
		return nil, err
	}
	if moral == nil {
		moral = &model.UserMoral{Mid: mid, Moral: model.DefaultMoral}
	}
	p := *moral
	_ = r.async.Do(ctx, func(c context.Context) {
		r.cache.SetJSON(c, keyMoral(mid), p, cacheTTLMoral)
	})
	return toMoralReply(moral), nil
}

// UpdateMoralArg 单个节操变更参数（logic 层从 pb/HTTP 转换）。
type UpdateMoralArg struct {
	Mid        int64
	Delta      int64
	Origin     int64
	Reason     string
	ReasonType int64
	Operator   string
	Remark     string
	Status     int64
	IsNotify   bool
	IP         string
}

// UpdateMoral 变更单个用户节操值（事务：读-改-写 + 日志，参考 service.UpdateMoral）。
func (r *Repository) UpdateMoral(ctx context.Context, arg *UpdateMoralArg) error {
	origin, ok := model.MoralOrigins[arg.Origin]
	if !ok {
		return ErrRequestErr
	}
	if origin.NeedReason && arg.Reason == "" {
		return ErrRequestErr
	}
	ts := time.Now().Unix()
	var before, after int64
	var ul *model.UserLog
	err := r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		var err error
		before, after, err = r.updateMoralTx(c, tx, arg.Mid, arg.Delta)
		if err != nil {
			return err
		}
		ul = &model.UserLog{
			Mid:   arg.Mid,
			IP:    arg.IP,
			TS:    ts,
			LogID: uuid4(),
			Content: map[string]string{
				"from_moral": formatInt(before),
				"to_moral":   formatInt(after),
				"origin":     formatInt(arg.Origin),
				"status":     formatInt(arg.Status),
				"mid":        formatInt(arg.Mid),
				"remark":     arg.Remark,
				"operater":   arg.Operator,
				"reason":     arg.Reason,
			},
		}
		if _, err := r.logModel.Add(c, model.LogTypeMoral, ul); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := r.delMoralCache(ctx, arg.Mid); err != nil {
		logx.Errorf("user-profile/moral: del moral cache mid=%d err=%v", arg.Mid, err)
	}
	r.moralNotice(ctx, arg.Mid, before, after, arg.ReasonType, arg.Origin, arg.Operator, arg.IsNotify)
	return nil
}

// BatchUpdateMoral 批量变更节操值（单事务，参考 service.UpdateMorals）。
func (r *Repository) BatchUpdateMoral(ctx context.Context, mids []int64, arg *UpdateMoralArg) (map[int64]int64, error) {
	origin, ok := model.MoralOrigins[arg.Origin]
	if !ok {
		return nil, ErrRequestErr
	}
	if origin.NeedReason && arg.Reason == "" {
		return nil, ErrRequestErr
	}
	ts := time.Now().Unix()
	beforeMap := make(map[int64]int64, len(mids))
	afterMap := make(map[int64]int64, len(mids))
	err := r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		for _, mid := range mids {
			before, after, err := r.updateMoralTx(c, tx, mid, arg.Delta)
			if err != nil {
				return err
			}
			beforeMap[mid] = before
			afterMap[mid] = after
			ul := &model.UserLog{
				Mid:   mid,
				IP:    arg.IP,
				TS:    ts,
				LogID: uuid4(),
				Content: map[string]string{
					"from_moral": formatInt(before),
					"to_moral":   formatInt(after),
					"origin":     formatInt(arg.Origin),
					"status":     formatInt(arg.Status),
					"remark":     arg.Remark,
					"operater":   arg.Operator,
					"reason":     arg.Reason,
				},
			}
			if _, err := r.logModel.Add(c, model.LogTypeMoral, ul); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for mid, before := range beforeMap {
		if err := r.delMoralCache(ctx, mid); err != nil {
			logx.Errorf("user-profile/moral: del moral cache mid=%d err=%v", mid, err)
		}
		r.moralNotice(ctx, mid, before, afterMap[mid], arg.ReasonType, arg.Origin, arg.Operator, arg.IsNotify)
	}
	return afterMap, nil
}

// UndoMoral 撤销节操变更（参考 service.UndoMoral）：
// 找到原日志 → 标记撤销 → 记录撤销日志 → 反向扣回节操值。
func (r *Repository) UndoMoral(ctx context.Context, logID, remark, operator string) error {
	useLog, err := r.logModel.FindByLogID(ctx, model.LogTypeMoral, logID)
	if err != nil {
		return err
	}
	if useLog == nil {
		return ErrNothingFound
	}
	// 标记原日志已撤销
	if err := r.logModel.MarkRevoked(ctx, model.LogTypeMoral, logID); err != nil {
		logx.Errorf("user-profile/moral: mark revoked log_id=%s err=%v", logID, err)
	}
	// 记录撤销日志（content 复制原日志并置 status=已撤销）
	content := make(map[string]string, len(useLog.Content)+1)
	for k, v := range useLog.Content {
		content[k] = v
	}
	content["status"] = formatInt(model.RevokedMoralStatus)
	revokedLog := &model.UserLog{
		Mid:     useLog.Mid,
		IP:      useLog.IP,
		TS:      useLog.TS,
		LogID:   useLog.LogID,
		Content: content,
	}
	if _, err := r.logModel.Add(ctx, model.LogTypeMoral, revokedLog); err != nil {
		logx.Errorf("user-profile/moral: add revoked log err=%v", err)
	}

	origin, err := strconv.ParseInt(useLog.Content["origin"], 10, 64)
	if err != nil {
		return fmt.Errorf("parse origin: %w", err)
	}
	fromMoral, err := strconv.ParseInt(useLog.Content["from_moral"], 10, 64)
	if err != nil {
		return fmt.Errorf("parse from_moral: %w", err)
	}
	toMoral, err := strconv.ParseInt(useLog.Content["to_moral"], 10, 64)
	if err != nil {
		return fmt.Errorf("parse to_moral: %w", err)
	}
	return r.UpdateMoral(ctx, &UpdateMoralArg{
		Mid:        useLog.Mid,
		Status:     model.IrrevocableMoralStatus,
		IP:         useLog.IP,
		Reason:     useLog.Content["reason"],
		Operator:   operator,
		Remark:     remark,
		Origin:     origin,
		Delta:      fromMoral - toMoral,
		ReasonType: model.SysReasonType,
		IsNotify:   false,
	})
}

// MoralLog 查询节操变更日志（最近 7 天，参考 service.MoralLog）。
func (r *Repository) MoralLog(ctx context.Context, mid int64) ([]*rpc.UserLogReply, error) {
	return r.memberLogs(ctx, model.LogTypeMoral, mid)
}

// updateMoralTx 事务内读-改-写节操值（参考 service.updateMoral/incrMoral/decMoral）。
func (r *Repository) updateMoralTx(ctx context.Context, tx sqlx.Session, mid, delta int64) (before, after int64, err error) {
	ts := time.Now().Unix()
	moral, err := r.moralModel.TxFindOne(ctx, tx, mid)
	if err != nil {
		return 0, 0, err
	}
	if moral == nil {
		if err = r.moralModel.TxInit(ctx, tx, mid, model.DefaultMoral, 0, 0, model.DefaultTime); err != nil {
			return 0, 0, err
		}
		before = model.DefaultMoral
	} else {
		before = moral.Moral
	}
	if delta > 0 {
		after = before + delta
		if after > model.MaxMoral {
			delta = model.MaxMoral - before
			after = model.MaxMoral
		}
		return before, after, r.moralModel.TxUpdate(ctx, tx, mid, delta, delta, 0)
	}
	after = before + delta // delta <= 0
	if after < 0 {
		delta = -before
		after = 0
	}
	if err = r.moralModel.TxUpdate(ctx, tx, mid, delta, 0, -delta); err != nil {
		return 0, 0, err
	}
	// 节操从基准值上方跌到下方时记录恢复时间（参考 decMoral）
	if before >= model.DefaultMoral && after < model.DefaultMoral {
		if err = r.moralModel.TxUpdateRecoverDate(ctx, tx, mid, ts); err != nil {
			return 0, 0, err
		}
	}
	return before, after, nil
}

// moralNotice 按阈值与来源触发用户通知（参考 service.moralNotice）。
// 通知通过 Outbox 事件异步投递（notification 服务待接入，见 outbox.go）。
func (r *Repository) moralNotice(ctx context.Context, mid, before, after, reasonType, origin int64, operator string, notifyChange bool) {
	rtName, notifyType, ok := moralNoticeType(reasonType)
	if !ok || notifyType == "" {
		return
	}
	delta := absInt(after - before)
	if delta == 0 {
		return
	}
	send := func(title, message string) {
		r.enqueueNotice(ctx, mid, title, message, notifyType)
	}
	switch {
	case before >= 6000 && after <= 6000 && after >= 3000:
		send(less6000Title, less6000Message)
	case before >= 3000 && after <= 3000:
		send(less3000Title, less3000Message)
	case before < 6000 && after >= 6000:
		send(greater6000Title, greater6000Message)
	}
	if !notifyChange {
		return
	}
	moralStr := fmt.Sprintf("%0.2f", float64(delta)/float64(100))
	switch {
	case origin == model.PunishmentType && operator == "系统":
		send(fmt.Sprintf(sysPunishmentTitle, moralStr), fmt.Sprintf(sysPunishmentMessage, moralStr))
	case origin == model.PunishmentType:
		send(fmt.Sprintf(punishmentTitle, moralStr), fmt.Sprintf(punishmentMessage, moralStr))
	case origin == model.ReportRewardType:
		send(fmt.Sprintf(rewardTitle, rtName), fmt.Sprintf(rewardMessage, rtName, moralStr))
	}
}

// enqueueNotice 把通知事件写入 Outbox（独立事务，通知失败不影响主流程）。
func (r *Repository) enqueueNotice(ctx context.Context, mid int64, title, message, noticeType string) {
	err := r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		return r.enqueueMoralNoticeTx(c, tx, &noticePayload{
			Mid:        mid,
			Title:      title,
			Message:    message,
			NoticeType: noticeType,
		})
	})
	if err != nil {
		logx.Errorf("user-profile/moral: enqueue notice mid=%d err=%v", mid, err)
	}
}

func absInt(v int64) int64 {
	if v > 0 {
		return v
	}
	return -v
}
