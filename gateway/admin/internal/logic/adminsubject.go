// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）：受保护路由的「操作者以会话为准」门槛。
//
// AdminPermission 中间件判定通过后，会把 operation 解析出的 AdminIdentity 挂到请求 context 上
// （middleware.WithAdmin）。logic 只有真的读到它，才有资格声称「这是一次已鉴权的后台操作」；
// 否则请求体里的 operator_* 只是调用方自报的一个字符串——任何能拼出这个 HTTP 请求的人都能把
// 一次处置记到别人头上，而台账上看不出破绽。
//
// 两类主体，两种处理，理由不同（同一个理由在 openOperatorGate / liveOperatorGate 里已经成立）：
//
//  1. **后台账号 ID 空间**（risk-control 的 operator、notification 的 op.operator_id、
//     search-indexer 只进本地审计日志的 operator_id）：会话身份**覆盖**客户端声明值，
//     不一致时留痕。这些字段本来就是「哪个后台账号做的」，覆盖不改变语义，
//     只是把「声称」换成「证明」。
//  2. **用户 mid / 自由文本操作人名**（comment/danmaku/inbox 的 operator_mid、
//     catalog/rights/video 的 operator）：与 admin_id **不是同一编号空间**
//     （admin_id 是 op_admin_user 主键，mid 是账号 mid），覆盖等于把处置记到无关用户头上。
//     因此门槛只要求「会话存在 + 主体非空」，并把 admin_id 与主体一起写进同一行日志：
//     既能追「谁点的按钮」，也能对上台账落在谁身上。
//
// 缺口：operation 目前没有暴露「后台账号 ↔ 其代表哪个 mid」的绑定契约
// （见 gateway/admin/README.md 与 admin.api 文末缺口清单），所以第 2 类只能做到
// 「已证明是谁操作的」+「记录他声称替谁操作」，无法做到「只能替自己绑定的 mid 操作」。
//
// 日志口径：只打路由、admin_id 和主体的数值/名字，绝不打 reason 正文、证件号、手机号、
// 卡号、token（AGENTS.md §4/§9）。

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/middleware"

	"github.com/zeromicro/go-zero/core/logx"
)

// errAdminSessionRequired 是 fail-closed 结论：拿不到会话身份就不执行任何下游写。
// 中途件漏挂（新路由忘了登记权限表则会被中间件本身 403，这里是第二道）或会话被判定为
// 非法时，宁可让运营看到一次「需要后台会话」的错误，也不接受自报主体。
var errAdminSessionRequired = errors.New("gateway/admin: admin session required")

// adminSession 读取中间件解析出的后台身份；没有就返回 errAdminSessionRequired。
func adminSession(ctx context.Context) (middleware.AdminIdentity, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return middleware.AdminIdentity{}, errAdminSessionRequired
	}
	return id, nil
}

// adminSessionGate 用于契约里**没有任何操作者位**的受保护路由（缓存失效、按主键推进的
// 发布/下架/过期、以及实名信息与登录日志这类 PII 读接口）。这些入口的下游台账记不下
// 「是谁拉的/是谁点的」，网关至少把 admin_id 落到访问日志里，否则事后无从追溯。
func adminSessionGate(ctx context.Context, route string) error {
	id, err := adminSession(ctx)
	if err != nil {
		return err
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d", route, id.AdminID)
	return nil
}

// adminSubjectGate 用于第 2 类主体（mid 空间的数值主体）。
// field 传下游契约里的字段名，错误消息与请求体字段保持一致（与 requireOperator 同口径）。
func adminSubjectGate(ctx context.Context, route, field string, mid int64) error {
	if err := requireOperator(field, mid); err != nil {
		return err
	}
	id, err := adminSession(ctx)
	if err != nil {
		return err
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d %s=%d", route, id.AdminID, field, mid)
	return nil
}

// adminActorGate 用于第 2 类主体里的自由文本操作人（catalog/rights/video 的 operator、
// user-profile 的 operator、moderation 的处置人姓名等）。
// 非空是门槛：空操作人落进台账就是一条无主处置记录，而本域的其它写入口
// （search-indexer 的 operator）早就要求非空，同名口径不该按域分裂。
func adminActorGate(ctx context.Context, route, field, actor string) error {
	if err := requireNonEmpty(field, actor); err != nil {
		return err
	}
	id, err := adminSession(ctx)
	if err != nil {
		return err
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d %s=%s", route, id.AdminID, field, actor)
	return nil
}

// adminOperatorID 用于第 1 类主体：**以会话为准**取后台账号 ID。
// claimed 是客户端声明的值（可能为 0，例如旧表单还没填）；不一致只留痕不拒绝——
// 判定权在中间件（已经确认了这个 admin_id 有这条路由的权限），这里没有任何需要
// 「客户端先证明」才成立的前提。
func adminOperatorID(ctx context.Context, route string, claimed int64) (int64, error) {
	id, err := adminSession(ctx)
	if err != nil {
		return 0, err
	}
	if claimed > 0 && claimed != id.AdminID {
		logx.WithContext(ctx).Errorf("gateway/admin/%s: subject mismatch session=%d claimed=%d",
			route, id.AdminID, claimed)
	}
	return id.AdminID, nil
}
