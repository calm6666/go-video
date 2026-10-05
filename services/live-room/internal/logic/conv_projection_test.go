// 本文件是 logic 包的手写测试：钉住「proto 枚举 ↔ model 常量 ↔ 投影」三者不漂移。
//
// 之所以要反向遍历 rpc.Xxx_name：proto 里新增一个枚举取值时，Go 代码不会有任何
// 编译错误提示「本服务还没处理它」，最坏情况是新状态被静默当成合法状态投影给终端。
// 反向覆盖把这种静默变成测试失败。

package logic

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

// TestEveryProtoEnumValueIsInterpreted 遍历 rpc 每张枚举表：
// 非 0 取值必须被本服务的取值集合承认；0 取值除 RoomOrder（0 是「默认序」而非
// 「未指定」）外一律不承认——0 值冒充合法状态是禁止伪装成功的一条常见路径。
func TestEveryProtoEnumValueIsInterpreted(t *testing.T) {
	specs := []struct {
		name        string
		values      map[int32]string
		interpreted func(int32) bool
		zeroLegal   bool
	}{
		{"RoomState", rpc.RoomState_name, model.ValidRoomState, false},
		{"VerifyState", rpc.VerifyState_name, model.ValidVerifyState, false},
		{"SessionState", rpc.SessionState_name, model.ValidSessionState, false},
		{"ReplayState", rpc.ReplayState_name, model.ValidReplayState, false},
		{"AnchorRole", rpc.AnchorRole_name, model.ValidAnchorRole, false},
		{"Platform", rpc.Platform_name, model.ValidPlatform, false},
		{"BanType", rpc.BanType_name, func(v int32) bool {
			_, err := banEndAt(v, 3600, 1000)
			return err == nil
		}, false},
		{"RoomOrder", rpc.RoomOrder_name, func(v int32) bool {
			_, err := roomOrderFilter(rpc.RoomOrder(v))
			return err == nil
		}, true},
	}
	for _, sp := range specs {
		t.Run(sp.name, func(t *testing.T) {
			seen := 0
			for num, name := range sp.values {
				seen++
				got := sp.interpreted(num)
				want := num != 0 || sp.zeroLegal
				if got != want {
					t.Fatalf("%s[%d]=%s：本服务处理为 %v，期望 %v（枚举表里的每个取值都必须有明确口径）",
						sp.name, num, name, got, want)
				}
				// 名字里带 UNSPECIFIED 的取值绝不能被当成业务状态。
				if strings.HasSuffix(name, "_UNSPECIFIED") && sp.interpreted(num) && !sp.zeroLegal {
					t.Fatalf("%s[%d]=%s 未指定值被当成合法取值", sp.name, num, name)
				}
			}
			if seen == 0 {
				t.Fatalf("%s 枚举表为空，遍历没兜到东西", sp.name)
			}
			// 未定义取值（前向兼容：新版本 proto 或脏数据）不得被承认。
			for _, bogus := range []int32{-1, 100, 9999} {
				if sp.interpreted(bogus) {
					t.Fatalf("%s(%d) 未定义取值被承认", sp.name, bogus)
				}
			}
		})
	}
}

// TestEveryEndReasonHasExactlyOneWriter 终止原因必须有明确归属：
// 「谁都能写 end_reason」等于允许伪造「被禁播」。
func TestEveryEndReasonHasExactlyOneWriter(t *testing.T) {
	for num, name := range rpc.EndReason_name {
		writers := endReasonWriters(num)
		if num == 0 {
			if len(writers) != 0 {
				t.Fatalf("END_REASON_UNSPECIFIED 不该有写入方：%v", writers)
			}
			continue
		}
		if len(writers) == 0 {
			t.Fatalf("EndReason[%d]=%s 没有任何写入方，落库后无人能解释", num, name)
		}
		// 客户端可指定的原因必须与归属集合自洽。
		if allowEndReasonForEndLive(num) != containsStr(writers, rpcEndLive) {
			t.Fatalf("EndReason[%d]=%s 的 EndLive 白名单与归属集合不一致：%v", num, name, writers)
		}
	}
	// 逐条钉死归属，避免以后有人把内部原因挪给客户端。
	want := map[int32][]string{
		model.EndReasonAnchorStop:    {rpcEndLive},
		model.EndReasonBanned:        {rpcBanRoom},
		model.EndReasonRoomClosed:    {rpcCloseRoom},
		model.EndReasonStreamTimeout: {rpcReportStreamState},
		model.EndReasonStreamReplay:  {rpcEndLive, rpcReportStreamState},
		model.EndReasonUnspecified:   nil,
	}
	for reason, ws := range want {
		if got := endReasonWriters(reason); !sameStrings(got, ws) {
			t.Fatalf("end_reason=%d 归属 = %v, want %v", reason, got, ws)
		}
	}
}

// TestEveryVerdictIsMapped moderation 结论必须逐个有落点，未指定结论必须报错。
func TestEveryVerdictIsMapped(t *testing.T) {
	mapped := 0
	for num, name := range rpc.ModerationVerdict_name {
		target, changed, err := verifyTargetForVerdict(model.VerifyStateReviewing, rpc.ModerationVerdict(num))
		if num == 0 {
			if err == nil {
				t.Fatalf("%s 被当成有效结论，目标状态 %d", name, target)
			}
			if !strings.HasSuffix(name, "UNSPECIFIED") {
				t.Fatalf("0 值结论名不符：%s", name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s 未映射到资料审核状态：%v", name, err)
		}
		if !model.ValidVerifyState(target) {
			t.Fatalf("%s 映射出非法审核状态 %d", name, target)
		}
		if name == "VERDICT_REVIEW" && (changed || target != model.VerifyStateReviewing) {
			t.Fatalf("转人审不该产生迁移：%d changed=%v", target, changed)
		}
		if name != "VERDICT_REVIEW" && !changed {
			t.Fatalf("%s 应产生迁移", name)
		}
		mapped++
	}
	if mapped != len(rpc.ModerationVerdict_name)-1 {
		t.Fatalf("结论映射覆盖不全：%d/%d", mapped, len(rpc.ModerationVerdict_name)-1)
	}
}

// TestEveryAnchorActionIsMapped BIND/UNBIND 之外的取值（含未指定）必须拒绝。
func TestEveryAnchorActionIsMapped(t *testing.T) {
	handled := 0
	for num, name := range rpc.AnchorAction_name {
		if !validAnchorAction(num) {
			if num != int32(rpc.AnchorAction_ANCHOR_ACTION_UNSPECIFIED) {
				t.Fatalf("%s 未被接受", name)
			}
			continue
		}
		if num == int32(rpc.AnchorAction_ANCHOR_ACTION_UNSPECIFIED) {
			t.Fatalf("%s 被当成有效动作", name)
		}
		if _, err := anchorRoleForAction(rpc.AnchorRole_ANCHOR_ROLE_COHOST, num); err != nil {
			t.Fatalf("%s 未处理：%v", name, err)
		}
		handled++
	}
	if handled != len(rpc.AnchorAction_name)-1 {
		t.Fatalf("动作覆盖不全：%d/%d", handled, len(rpc.AnchorAction_name)-1)
	}
	// 契约外的裸数值同样拒绝，杜绝「未知即当 BIND」。
	for _, num := range []int32{-1, 3, 99} {
		if validAnchorAction(num) {
			t.Fatalf("action=%d 被当成有效动作", num)
		}
	}
}

// TestPlatformEnumExcludesMiniProgram 平台口径只有 Android/iOS/HarmonyOS/桌面端
// （AGENTS.md §6）：枚举表里出现小程序类取值就是契约漂移。
func TestPlatformEnumExcludesMiniProgram(t *testing.T) {
	for num, name := range rpc.Platform_name {
		for _, banned := range []string{"MINI", "WECHAT", "ALIPAY", "H5", "WEB"} {
			if strings.Contains(name, banned) {
				t.Fatalf("平台枚举出现不受支持的端 %s(%d)", name, num)
			}
		}
	}
	// 归一后每个平台都要有稳定的下游字符串口径（risk-control 按字符串归因）。
	for _, p := range []rpc.Platform{rpc.Platform_PLATFORM_ANDROID, rpc.Platform_PLATFORM_IOS,
		rpc.Platform_PLATFORM_HARMONY, rpc.Platform_PLATFORM_DESKTOP} {
		v, err := normalizePlatform(p)
		if err != nil {
			t.Fatalf("%v 归一失败：%v", p, err)
		}
		name := platformName(v)
		if name == "" || strings.ToLower(name) != name {
			t.Fatalf("platform=%d 的下游口径异常：%q", v, name)
		}
	}
	got, err := normalizePlatform(rpc.Platform_PLATFORM_UNSPECIFIED)
	if err != nil || got != model.PlatformAndroid {
		t.Fatalf("未指定端按契约记 android：%d %v", got, err)
	}
	if _, err := normalizePlatform(rpc.Platform(7)); !strings.Contains(err.Error(), "platform") {
		t.Fatalf("未知端应拒绝并带原值：%v", err)
	}
}

// TestRpcNameConstantsMatchGeneratedServiceDesc 幂等表与状态日志按 RPC 名归因，
// 名字必须与生成的 ServiceDesc 逐字一致，且 21 个方法一个都不少。
func TestRpcNameConstantsMatchGeneratedServiceDesc(t *testing.T) {
	constants := []string{rpcCreateRoom, rpcUpdateRoomInfo, rpcGetRoom, rpcListRooms,
		rpcPrepareLive, rpcStartLive, rpcEndLive, rpcCloseRoom, rpcReportStreamState,
		rpcApplyRoomModerationResult, rpcBanRoom, rpcLiftBan, rpcListRoomBans,
		rpcGetSession, rpcListSessions, rpcAttachReplay, rpcUpdateRoomSetting,
		rpcMutateAnchor, rpcListAnchors, rpcUpsertArea, rpcListAreas}
	seen := map[string]bool{}
	for _, n := range constants {
		if n == "" || seen[n] {
			t.Fatalf("RPC 名常量为空或重复：%q", n)
		}
		seen[n] = true
	}
	methods := map[string]bool{}
	for _, m := range rpc.LiveRoom_ServiceDesc.Methods {
		methods[m.MethodName] = true
	}
	if len(methods) != len(constants) {
		t.Fatalf("方法数漂移：常量 %d 个，ServiceDesc %d 个", len(constants), len(methods))
	}
	for _, n := range constants {
		if !methods[n] {
			t.Fatalf("RPC 名常量 %q 在生成服务里不存在", n)
		}
	}
	for m := range methods {
		if !seen[m] {
			t.Fatalf("生成的方法 %q 没有对应的 RPC 名常量，写入口会漏幂等归因", m)
		}
	}
}

// --- 投影逐字段 ---

func TestRoomInfoProjectionFillsEveryField(t *testing.T) {
	row := &model.LiveRoom{
		RoomID: 11, OwnerMid: 22, Title: "标题", Cover: "cover/1.jpg", AreaID: 33,
		State: model.RoomStateLiving, VerifyState: model.VerifyStatePassed,
		ActiveSessionID: 44, ActiveStreamID: "stream-44", StateVersion: 5,
		RejectReason: "驳回说明", BanUntil: 1700, Ctime: 1600, Mtime: 1650,
	}
	info := roomInfo(row)
	assertEveryFieldSet(t, info)
	assertColumnParity(t, "LiveRoom→RoomInfo", row, info, map[string]bool{
		"Platform": true, "AppVersion": true, "ModerationTaskID": true,
	})
	if info.State != rpc.RoomState(model.RoomStateLiving) || info.VerifyState != rpc.VerifyState(model.VerifyStatePassed) {
		t.Fatalf("枚举回灌丢值：%+v", info)
	}
	if roomInfo(nil) != nil {
		t.Fatal("nil 行必须投影为 nil，不能回空对象")
	}
}

func TestSessionInfoProjectionFillsEveryField(t *testing.T) {
	row := &model.LiveSession{
		SessionID: 1, RoomID: 2, Mid: 3, State: model.SessionStateEnded,
		TitleSnapshot: "开播时标题", AreaIDSnapshot: 4, StreamID: "s-1",
		StartedAt: 10, EndedAt: 20, DurationSeconds: 10, EndReason: model.EndReasonAnchorStop,
		LastStreamSeq: 7, ReplayState: model.ReplayStateAvailable, RecordID: 8,
		RecordAssetID: 9, RecordAid: 10, ModerationTaskID: 11, Ctime: 12, Mtime: 13,
	}
	info := sessionInfo(row)
	assertEveryFieldSet(t, info)
	assertColumnParity(t, "LiveSession→SessionInfo", row, info, map[string]bool{"TraceID": true})
	if info.GetTitleSnapshot() != row.TitleSnapshot || info.GetAreaIdSnapshot() != row.AreaIDSnapshot {
		t.Fatal("开播快照必须原样下发：分区/标题后续变更不得改写历史场次")
	}
}

func TestBanInfoProjectionFillsEveryField(t *testing.T) {
	row := &model.LiveRoomBan{
		BanID: 1, RoomID: 2, Mid: 3, BanType: model.BanTypeTemporary, Reason: "违规",
		StartAt: 10, EndAt: 20, State: model.BanStateActive, OperatorMid: 30,
		LiftOperatorMid: 31, LiftReason: "误判", LiftedAt: 40, TraceID: "trace", Ctime: 50,
	}
	info := banInfo(row)
	assertEveryFieldSet(t, info)
	assertColumnParity(t, "LiveRoomBan→RoomBanInfo", row, info, map[string]bool{"TraceID": true})
}

func TestAreaAndAnchorProjectionFillEveryField(t *testing.T) {
	area := &model.LiveArea{AreaID: 1, AreaName: "游戏", ParentAreaID: 2, Sort: 3,
		State: model.AreaStateEnabled, OperatorMid: 4, Ctime: 5, Mtime: 6}
	assertEveryFieldSet(t, areaInfo(area))
	assertColumnParity(t, "LiveArea→AreaInfo", area, areaInfo(area), nil)

	anchor := &model.LiveRoomAnchor{ID: 1, RoomID: 2, Mid: 3, Role: model.AnchorRoleManager,
		State: model.BindStateEnabled, Ctime: 4, Mtime: 5}
	assertEveryFieldSet(t, anchorInfo(anchor))
	assertColumnParity(t, "LiveRoomAnchor→AnchorInfo", anchor, anchorInfo(anchor),
		map[string]bool{"OwnerRoomID": true})
}

// TestSettingInfoProjectsServerDefaultsWhenRowMissing 缺配置行必须回服务端默认，
// 不能让终端把「没有行」读成「弹幕/回复都关着」。
func TestSettingInfoProjectsServerDefaultsWhenRowMissing(t *testing.T) {
	got := settingInfo(nil)
	if got == nil {
		t.Fatal("缺行也要有投影")
	}
	if !got.GetDanmakuEnabled() || !got.GetReplyEnabled() {
		t.Fatalf("弹幕/回复默认开启：%+v", got)
	}
	if got.GetRecordEnabled() || got.GetLinkmicEnabled() {
		t.Fatalf("录制/连麦默认关闭：%+v", got)
	}
	if got.GetLiveType() != model.LiveTypeVideo {
		t.Fatalf("缺省直播类型应为视频：%d", got.GetLiveType())
	}
	row := &model.LiveRoomSetting{RoomID: 1, DanmakuEnabled: model.BoolToInt32(true),
		ReplyEnabled: model.BoolToInt32(true), RecordEnabled: model.BoolToInt32(true),
		LinkmicEnabled: model.BoolToInt32(true), LiveType: model.LiveTypeScreen,
		MinClientVersionCode: 9, Mtime: 10}
	full := settingInfo(row)
	assertEveryFieldSet(t, full)
	assertColumnParity(t, "LiveRoomSetting→RoomSetting", row, full, map[string]bool{"Ctime": true})
	if !full.GetRecordEnabled() || full.GetLiveType() != model.LiveTypeScreen {
		t.Fatalf("布尔列未还原：%+v", full)
	}
}

func TestListProjectionsSkipNilRows(t *testing.T) {
	rooms := roomInfoList([]*model.LiveRoom{nil, {RoomID: 1}})
	if len(rooms) != 1 || rooms[0].GetRoomId() != 1 {
		t.Fatalf("roomInfoList 丢行或保留 nil：%+v", rooms)
	}
	sessions := sessionInfoList([]*model.LiveSession{nil, {SessionID: 2}})
	if len(sessions) != 1 || sessions[0].GetSessionId() != 2 {
		t.Fatalf("sessionInfoList：%+v", sessions)
	}
	bans := banInfoList([]*model.LiveRoomBan{nil, {BanID: 3}})
	if len(bans) != 1 || bans[0].GetBanId() != 3 {
		t.Fatalf("banInfoList：%+v", bans)
	}
	areas := areaInfoList([]*model.LiveArea{nil, {AreaID: 4}})
	if len(areas) != 1 || areas[0].GetAreaId() != 4 {
		t.Fatalf("areaInfoList：%+v", areas)
	}
	anchors := anchorInfoList([]*model.LiveRoomAnchor{nil, {ID: 5}})
	if len(anchors) != 1 || anchors[0].GetId() != 5 {
		t.Fatalf("anchorInfoList：%+v", anchors)
	}
	for _, empty := range [][]*rpc.RoomInfo{roomInfoList(nil), roomInfoList([]*model.LiveRoom{})} {
		if empty == nil || len(empty) != 0 {
			t.Fatal("空列表要回非 nil 的零长切片，终端按数组渲染")
		}
	}
}

// --- 反射与集合辅助 ---

// assertEveryFieldSet 要求投影结果的每个导出字段都非零：
// 只要 proto 加了字段而投影没搬，这里必红。
func assertEveryFieldSet(t *testing.T, msg any) {
	t.Helper()
	v := reflect.ValueOf(msg).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("%T.%s 未被投影填充（零值）", msg, f.Name)
		}
	}
}

// assertColumnParity 双向对齐：model 列要么下发要么在显式内部清单里；
// proto 字段要么有对应列（说明投影有据），否则是新加的、还没落库的字段。
func assertColumnParity(t *testing.T, label string, row, msg any, internal map[string]bool) {
	t.Helper()
	cols := exportedFieldNames(row)
	fields := exportedFieldNames(msg)
	// 内部列清单按调用方写法（TraceID/Ctime）传入，而上面两侧名字已归一为小写，
	// 不统一口径就会漏判。
	internalLower := make(map[string]bool, len(internal))
	for k, v := range internal {
		internalLower[strings.ToLower(k)] = v
	}
	fieldSet := nameSet(fields)
	colSet := nameSet(cols)
	for _, c := range cols {
		if internalLower[c] {
			continue
		}
		if !fieldSet[c] {
			t.Errorf("%s：model 列 %s 未下发且未登记为内部列（漏投影或契约缺口）", label, c)
		}
	}
	for _, f := range fields {
		if !colSet[f] {
			t.Errorf("%s：proto 字段 %s 没有对应 model 列，投影只能回零值", label, f)
		}
	}
}

func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// exportedFieldNames 返回结构体导出字段名的归一口径（小写）。
// db 标签与 proto 字段只在大小写上不同（RoomID/RoomId），归一后即可比对。
func exportedFieldNames(v any) []string {
	tp := reflect.TypeOf(v)
	if tp.Kind() == reflect.Ptr {
		tp = tp.Elem()
	}
	out := make([]string, 0, tp.NumField())
	for i := 0; i < tp.NumField(); i++ {
		if f := tp.Field(i); f.IsExported() {
			out = append(out, strings.ToLower(f.Name))
		}
	}
	return out
}

// endReasonWriters 给出某个终止原因的写入方归属（唯一来源，供上表断言）。
func endReasonWriters(reason int32) []string {
	switch reason {
	case model.EndReasonAnchorStop:
		return []string{rpcEndLive}
	case model.EndReasonBanned:
		return []string{rpcBanRoom}
	case model.EndReasonRoomClosed:
		return []string{rpcCloseRoom}
	case model.EndReasonStreamTimeout:
		return []string{rpcReportStreamState}
	case model.EndReasonStreamReplay:
		return []string{rpcEndLive, rpcReportStreamState}
	default:
		return nil
	}
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = fmt.Sprintf
