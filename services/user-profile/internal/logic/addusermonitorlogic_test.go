package logic

// addusermonitorlogic_test.go 覆盖 AddUserMonitor（把用户加入监控名单）。
//
// 链路最短的一档：logic → Repository.AddUserMonitor → monitorModel.Add，
// model 层是一句
// `INSERT INTO user_monitor (mid, operator, remark) VALUES (?,?,?)
//  ON DUPLICATE KEY UPDATE operator = VALUES(operator), remark = VALUES(remark), is_deleted = 0`。
// 本方法要钉的写侧不变量是「名单归属 + 幂等 + 复位软删除」，
// 外加一条重要的** absence**：监控名单决定「资料变更是否强制进审核」，
// 而把它加进去这一步不留任何审计痕迹（无 member_log、无事件、无事务），
// 且 operator 完全由调用方自填、可任意伪造。
//
// 注意：本方法不校验 mid，所以「名单归属」只能钉成「以传入 mid 为主键的一条记录」，
// 不存在「这个人是否真的存在」的检查。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// TestAddUserMonitorHasNoPreconditionGuard 钉「mid / operator / remark 一律不校验」。
//
// TODO(缺陷) 应拒未拒：
//   - mid <= 0：以 mid=0 / 负数为主键写入名单（真实 MySQL 的 `mid` BIGINT UNSIGNED 会拒负数，
//     本层不拦），且 user_base 里根本没有这个人——监控一个不存在的账号；
//   - operator 空串：DDL `operator` VARCHAR(64) NOT NULL DEFAULT ”，空串合法，
//     于是「谁加的监控」这件事可以整片留空；
//   - operator 超 64 字符 / remark 超 255 字符：不裁剪不报错，只能等 MySQL 抛 1406；
//   - 无调用者身份约束：operator 是调用方自填的字符串，不来自会话，事后无法追责。
func TestAddUserMonitorHasNoPreconditionGuard(t *testing.T) {
	cases := []struct {
		label    string
		mid      int64
		operator string
		remark   string
	}{
		{"mid=0 照样入名单", 0, "运营小A", "刷弹幕"},
		{"mid 负数照样入名单", -5, "运营小A", "刷弹幕"},
		{"operator 空串不拒绝", 34001, "", "无"},
		{"remark 空串不拒绝", 34002, "运营小A", ""},
		{"operator 超 DDL VARCHAR(64) 不裁剪", 34003, strings.Repeat("作", 100), "无"},
		{"remark 超 DDL VARCHAR(255) 不裁剪", 34004, "运营小A", strings.Repeat("x", 1000)},
		{"remark 含换行/注入形态原样入库", 34005, "运营小A", "a'); DROP TABLE user_monitor; --\n第二行"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			l := NewAddUserMonitorLogic(context.Background(), e.svcCtx)
			reply, err := l.AddUserMonitor(&rpc.AddUserMonitorReq{Mid: tc.mid, Operator: tc.operator, Remark: tc.remark})
			wantNoErr(t, tc.label, err)
			if reply == nil {
				t.Fatalf("%s：成功时 reply 必须非 nil", tc.label)
			}
			// 只有 1 步写：没有事务、没有事件、没有缓存失效、没有日志。
			wantOps(t, tc.label, e.ops(0), []string{"monitor.Add:" + itoa(tc.mid)})
			wantEQ(t, tc.label, "事务次数", e.st.conn.transactions, 0)
			wantEQ(t, tc.label, "Outbox 行数", e.st.outbox.count(), 0)
			wantEQ(t, tc.label, "member_log 行数（入监控不留审计）", e.st.logs.count(), 0)

			row := e.st.monitor.rows[tc.mid]
			if row == nil {
				t.Fatalf("%s：user_monitor 没有落库", tc.label)
			}
			wantEQ(t, tc.label, "mid", row.Mid, tc.mid)
			wantEQ(t, tc.label, "operator 逐字未改", row.Operator, tc.operator)
			wantEQ(t, tc.label, "remark 逐字未改", row.Remark, tc.remark)
			wantEQ(t, tc.label, "is_deleted 入名单即 0", int(row.IsDeleted), 0)
		})
	}
}

// TestAddUserMonitorIsIdempotentUpsert 重复添加：库里仍只有一行，
// 后一次覆盖前两次的 operator/remark（不是「首次写入者胜出」），
// 且每次都真的发生一次写（无「已在名单就跳过」的短路）。
func TestAddUserMonitorIsIdempotentUpsert(t *testing.T) {
	e := newEnv(t)
	l := NewAddUserMonitorLogic(context.Background(), e.svcCtx)

	_, err := l.AddUserMonitor(&rpc.AddUserMonitorReq{Mid: 34100, Operator: "第一位运营", Remark: "第一次原因"})
	wantNoErr(t, "第一次添加", err)
	_, err = l.AddUserMonitor(&rpc.AddUserMonitorReq{Mid: 34100, Operator: "第二位运营", Remark: "第二次原因"})
	wantNoErr(t, "第二次添加", err)

	wantOps(t, "重复添加的调用序列（按顺序两次）", e.ops(0), []string{
		"monitor.Add:34100",
		"monitor.Add:34100",
	})
	wantEQ(t, "重复添加", "名单行数（主键幂等，不会双记）", len(e.st.monitor.rows), 1)
	wantEQ(t, "重复添加", "operator 被后一次覆盖", e.st.monitor.rows[34100].Operator, "第二位运营")
	wantEQ(t, "重复添加", "remark 被后一次覆盖", e.st.monitor.rows[34100].Remark, "第二次原因")

	// 读侧：IsInMonitor 必须为 true，且读路径只有 monitor.InMonitor 一步（该表无缓存）。
	e.st.log.reset()
	rp, err := NewIsInMonitorLogic(context.Background(), e.svcCtx).IsInMonitor(&rpc.MidReq{Mid: 34100})
	wantNoErr(t, "写后读", err)
	wantOps(t, "写后读", e.ops(0), []string{"monitor.InMonitor:34100"})
	if !rp.GetIsInMonitor() {
		t.Fatal("写后读：加了名单却查不到")
	}
}

// TestAddUserMonitorRevivesSoftDeletedEntry 复现 ON DUPLICATE 的 is_deleted = 0：
// 已软删除的名单项重新添加后必须回到「在监控中」，且 operator/remark 被换新。
func TestAddUserMonitorRevivesSoftDeletedEntry(t *testing.T) {
	e := newEnv(t)
	e.st.monitor.put(&model.UserMonitor{Mid: 34200, Operator: "旧操作人", Remark: "旧备注", IsDeleted: 1})

	// 布景后确认：软删除状态下读侧是 false。
	if in, err := e.st.repo.IsInMonitor(context.Background(), 34200); err != nil || in {
		t.Fatalf("布景校验：is_deleted=1 时 IsInMonitor = %v/%v, want false/nil", in, err)
	}
	e.st.log.reset()

	l := NewAddUserMonitorLogic(context.Background(), e.svcCtx)
	_, err := l.AddUserMonitor(&rpc.AddUserMonitorReq{Mid: 34200, Operator: "新操作人", Remark: "新原因"})
	wantNoErr(t, "复活软删除项", err)
	wantOps(t, "复活软删除项", e.ops(0), []string{"monitor.Add:34200"})

	row := e.st.monitor.rows[34200]
	wantEQ(t, "复活软删除项", "is_deleted 被清回 0", int(row.IsDeleted), 0)
	wantEQ(t, "复活软删除项", "operator 换新", row.Operator, "新操作人")
	wantEQ(t, "复活软删除项", "remark 换新", row.Remark, "新原因")
	wantEQ(t, "复活软删除项", "名单仍只有一行", len(e.st.monitor.rows), 1)

	e.st.log.reset()
	in, err := NewIsInMonitorLogic(context.Background(), e.svcCtx).IsInMonitor(&rpc.MidReq{Mid: 34200})
	wantNoErr(t, "复活后读", err)
	if !in.GetIsInMonitor() {
		t.Fatal("复活后读：仍是不在监控中（is_deleted 没被清掉）")
	}
}

// TestAddUserMonitorDoesNotInvalidateAnyCache 监控名单变更后，
// 既有的 bs_/exp_/moral_ 缓存一个都不失效。
// 后果要钉出来：AddPropertyReview 用 IsInMonitor 决定是否强制进审核，
// 而监控状态本身没有缓存，所以这里安全；但**已经缓存好的 bs_ 资料不会因入名单而重算**，
// 若将来把「是否监控中」投影进 Member/Base，就会立刻出现脏读——本用例是那条改动的哨兵。
func TestAddUserMonitorDoesNotInvalidateAnyCache(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 34300, Name: "被监控者", Rank: 5000})
	e.st.cache.warmJSON(keyBase(34300), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 34300, Name: "被监控者"}})
	e.st.cache.warmInt(keyExp(34300), 999)
	e.st.cache.warmJSON(keyMoral(34300), model.UserMoral{Mid: 34300, Moral: model.DefaultMoral})

	l := NewAddUserMonitorLogic(context.Background(), e.svcCtx)
	_, err := l.AddUserMonitor(&rpc.AddUserMonitorReq{Mid: 34300, Operator: "运营", Remark: "r"})
	wantNoErr(t, "入监控不失效缓存", err)
	wantOps(t, "入监控不失效缓存", e.ops(0), []string{"monitor.Add:34300"})
	wantEQ(t, "入监控不失效缓存", "cache.Del 次数", e.st.log.countPrefix("cache.Del:"), 0)
	for _, k := range []string{keyBase(34300), keyExp(34300), keyMoral(34300)} {
		_, inJSON := e.st.cache.jsons[k]
		_, inStr := e.st.cache.strs[k]
		if !inJSON && !inStr {
			t.Errorf("入监控不失效缓存：%s 竟然不在了（与本结论不符）", k)
		}
	}
}

func TestAddUserMonitorDownstreamFailure(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("Error 1406: Data too long for column 'operator'")
	e.st.monitor.failWith("Add", boom)

	l := NewAddUserMonitorLogic(context.Background(), e.svcCtx)
	reply, err := l.AddUserMonitor(&rpc.AddUserMonitorReq{Mid: 34400, Operator: strings.Repeat("作", 100), Remark: "r"})
	wantErrIs(t, "名单写失败", err, boom)
	if reply != nil {
		t.Errorf("名单写失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "名单写失败", e.ops(0), []string{"monitor.Add:34400"})
	if _, ok := e.st.monitor.rows[34400]; ok {
		t.Error("名单写失败：行还是写进去了（假成功）")
	}

	// 读侧必须仍是「不在监控中」，不能出现写失败但读成功的裂口。
	e.st.log.reset()
	in, err := NewIsInMonitorLogic(context.Background(), e.svcCtx).IsInMonitor(&rpc.MidReq{Mid: 34400})
	wantNoErr(t, "写失败后读", err)
	if in.GetIsInMonitor() {
		t.Error("写失败后读：返回了 true（假成功）")
	}
}

// TestAddUserMonitorAffectsOnlyTheTargetMid 名单归属：只动被点名的 mid，
// 同表里其他人的 operator/remark/is_deleted 不许被顺手改掉。
func TestAddUserMonitorAffectsOnlyTheTargetMid(t *testing.T) {
	e := newEnv(t)
	e.st.monitor.put(&model.UserMonitor{Mid: 34500, Operator: "邻居操作人", Remark: "邻居备注", IsDeleted: 1})
	e.st.monitor.put(&model.UserMonitor{Mid: 34501, Operator: "另一位", Remark: "另一条", IsDeleted: 0})

	l := NewAddUserMonitorLogic(context.Background(), e.svcCtx)
	_, err := l.AddUserMonitor(&rpc.AddUserMonitorReq{Mid: 34502, Operator: "新来的", Remark: "新备注"})
	wantNoErr(t, "只动目标 mid", err)
	wantOps(t, "只动目标 mid", e.ops(0), []string{"monitor.Add:34502"})

	wantEQ(t, "只动目标 mid", "行数", len(e.st.monitor.rows), 3)
	neighbour := e.st.monitor.rows[34500]
	wantEQ(t, "只动目标 mid", "邻居 operator 保留", neighbour.Operator, "邻居操作人")
	wantEQ(t, "只动目标 mid", "邻居 remark 保留", neighbour.Remark, "邻居备注")
	wantEQ(t, "只动目标 mid", "邻居 is_deleted 仍是 1（没被顺手复活）", int(neighbour.IsDeleted), 1)
	other := e.st.monitor.rows[34501]
	wantEQ(t, "只动目标 mid", "另一位 operator 保留", other.Operator, "另一位")
	wantEQ(t, "只动目标 mid", "新行 operator", e.st.monitor.rows[34502].Operator, "新来的")
}
