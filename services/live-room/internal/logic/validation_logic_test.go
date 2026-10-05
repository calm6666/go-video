package logic

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	riskrpc "go-video/services/risk-control/rpc"
)

func TestCheckTitle(t *testing.T) {
	if got, err := checkTitle("  标题  ", 80); err != nil || got != "标题" {
		t.Fatalf("应去首尾空白：got=%q err=%v", got, err)
	}
	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, err := checkTitle(bad, 80); !errors.Is(err, model.ErrTitleInvalid) {
			t.Fatalf("空标题 %q 应被拒绝：%v", bad, err)
		}
	}
	// 列宽按字符数（rune）而不是字节数：中文标题 80 字必须能过。
	if _, err := checkTitle(strings.Repeat("直", 80), 80); err != nil {
		t.Fatalf("80 个中文字符应通过：%v", err)
	}
	if _, err := checkTitle(strings.Repeat("直", 81), 80); !errors.Is(err, model.ErrTitleInvalid) {
		t.Fatalf("81 个中文字符应被拒绝：%v", err)
	}
}

func TestCheckCover(t *testing.T) {
	if got, err := checkCover("object/key/cover.png"); err != nil || got != "object/key/cover.png" {
		t.Fatalf("站内引用应通过：%q %v", got, err)
	}
	if got, err := checkCover(""); err != nil || got != "" {
		t.Fatalf("空封面合法（尚未上传）：%q %v", got, err)
	}
	// 签名地址与公共 URL 不得进库（AGENTS.md §6）。
	for _, bad := range []string{"https://cdn.example.com/a.png", "http://a/b", "x://y", "HTTPS://a"} {
		if _, err := checkCover(bad); !errors.Is(err, model.ErrCoverTooLong) {
			t.Fatalf("封面 %q 必须被拒绝：%v", bad, err)
		}
	}
	if _, err := checkCover(strings.Repeat("a", maxCoverRefRunes+1)); !errors.Is(err, model.ErrCoverTooLong) {
		t.Fatalf("超长封面应被拒绝：%v", err)
	}
}

func TestCheckRef(t *testing.T) {
	if got, err := checkRef("stream_id", " s-1 "); err != nil || got != "s-1" {
		t.Fatalf("引用应去空白：%q %v", got, err)
	}
	if got, err := checkRef("stream_id", ""); err != nil || got != "" {
		t.Fatalf("空引用合法（推流后回填）：%q %v", got, err)
	}
	// 明文 IP、带空格与 URL 形态都视为不透明引用被破坏。
	for _, bad := range []string{"1 1 2 3", "https://a/b", strings.Repeat("x", maxRefRunes+1)} {
		if _, err := checkRef("device_hash", bad); !errors.Is(err, model.ErrStreamRefMismatch) {
			t.Fatalf("引用 %q 必须被拒绝：%v", bad, err)
		}
	}
}

func TestCheckReason(t *testing.T) {
	if _, err := checkReason("reason", "", false); err != nil {
		t.Fatalf("非必填空原因应通过：%v", err)
	}
	if _, err := checkReason("reason", "", true); !errors.Is(err, model.ErrReasonTooLong) {
		t.Fatalf("必填原因缺失应拒绝：%v", err)
	}
	if got, err := checkReason("reason", "  违规内容  ", false); err != nil || got != "违规内容" {
		t.Fatalf("原因应去空白：%q %v", got, err)
	}
	// 超长必须拒绝而不是截断：审计文本要与库值一致。
	if _, err := checkReason("reason", strings.Repeat("r", maxReasonRunes+1), false); !errors.Is(err, model.ErrReasonTooLong) {
		t.Fatalf("超长原因应拒绝：%v", err)
	}
	if _, err := checkReason("reason", strings.Repeat("r", maxReasonRunes), false); err != nil {
		t.Fatalf("边界内应通过：%v", err)
	}
}

func TestCheckDedupIdentifiers(t *testing.T) {
	for _, fn := range []func(string) error{checkRequestID, checkEventID} {
		if err := fn(""); err == nil {
			t.Fatal("空去重标识必须拒绝：没有幂等键的写重放就是两次副作用")
		}
		if err := fn(strings.Repeat("k", maxDedupIDBytes+1)); !errors.Is(err, model.ErrDedupIDTooLong) {
			t.Fatalf("超列宽应拒绝：%v", err)
		}
		if err := fn(strings.Repeat("k", maxDedupIDBytes)); err != nil {
			t.Fatalf("边界内应通过：%v", err)
		}
	}
	if err := checkRequestID(" req-1 "); err != nil {
		t.Fatalf("去空白后应通过：%v", err)
	}
	if err := checkRoomID(0); !errors.Is(err, model.ErrInvalidRoomID) {
		t.Fatalf("room_id=0 应拒绝：%v", err)
	}
	if err := checkSessionID(-1); !errors.Is(err, model.ErrInvalidSessionID) {
		t.Fatalf("session_id<0 应拒绝：%v", err)
	}
	if err := checkMid(0); !errors.Is(err, model.ErrInvalidMid) {
		t.Fatalf("mid=0 应拒绝：%v", err)
	}
	if err := checkOperator(0); !errors.Is(err, model.ErrOperatorRequired) {
		t.Fatalf("无操作者的处置应拒绝：%v", err)
	}
}

func TestTraceIDSanitizedAndTextTruncated(t *testing.T) {
	if got := sanitizeTraceID("  abc  "); got != "abc" {
		t.Fatalf("trace_id 应去空白，实得 %q", got)
	}
	if got := sanitizeTraceID(strings.Repeat("t", maxTraceIDBytes+10)); len(got) != maxTraceIDBytes {
		t.Fatalf("trace_id 应截到列宽，实得 %d", len(got))
	}
	if got := truncateRunes("直播间标题", 3); got != "直播间" {
		t.Fatalf("按 rune 截断错误：%q", got)
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatalf("未超宽不应改动：%q", got)
	}
}

// TestNormalizePlatformRejectsMiniProgram 平台口径：只有四端，未定义取值一律拒绝。
func TestNormalizePlatformRejectsMiniProgram(t *testing.T) {
	cases := map[rpc.Platform]int32{
		rpc.Platform_PLATFORM_UNSPECIFIED: model.PlatformAndroid,
		rpc.Platform_PLATFORM_ANDROID:     model.PlatformAndroid,
		rpc.Platform_PLATFORM_IOS:         model.PlatformIOS,
		rpc.Platform_PLATFORM_HARMONY:     model.PlatformHarmony,
		rpc.Platform_PLATFORM_DESKTOP:     model.PlatformDesktop,
	}
	for in, want := range cases {
		got, err := normalizePlatform(in)
		if err != nil || got != want {
			t.Fatalf("platform %v -> %d/%v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []rpc.Platform{rpc.Platform(0), rpc.Platform(5), rpc.Platform(99), rpc.Platform(-1)} {
		if bad == rpc.Platform_PLATFORM_UNSPECIFIED {
			continue
		}
		if _, err := normalizePlatform(bad); !errors.Is(err, model.ErrPlatformInvalid) {
			t.Fatalf("未知平台 %d 必须拒绝（不支持小程序）：%v", bad, err)
		}
	}
	for _, p := range []int32{model.PlatformAndroid, model.PlatformIOS, model.PlatformHarmony, model.PlatformDesktop} {
		if !model.ValidPlatform(p) || platformName(p) == "" {
			t.Fatalf("平台 %d 应有稳定字符串口径", p)
		}
	}
	if platformName(model.PlatformUnspecified) != "" {
		t.Fatal("未指定平台不得给出字符串")
	}
}

func TestPaginationBoundaries(t *testing.T) {
	if got, err := clampPage(0); err != nil || got != 1 {
		t.Fatalf("page=0 应归一为 1：%d %v", got, err)
	}
	if got, err := clampPage(3); err != nil || got != 3 {
		t.Fatalf("page=3 原样：%d %v", got, err)
	}
	if _, err := clampPage(-1); !errors.Is(err, model.ErrInvalidPage) {
		t.Fatalf("负页码应拒绝：%v", err)
	}
	if got := pageOffset(1, 20); got != 0 {
		t.Fatalf("首页 offset 应为 0，实得 %d", got)
	}
	if got := pageOffset(4, 25); got != 75 {
		t.Fatalf("offset 计算错误：%d", got)
	}
	if got := pageOffset(0, 0); got != 0 {
		t.Fatalf("退化入参不应产生负 offset：%d", got)
	}
	if got := clampTotal(-5); got != 0 {
		t.Fatalf("负总数应夹到 0，实得 %d", got)
	}
	if got := clampTotal(1 << 40); got != int32(^uint32(0)>>1) {
		t.Fatalf("溢出应夹到 int32 上限，实得 %d", got)
	}
}

func TestCursorCodec(t *testing.T) {
	if got, err := decodeIDCursor(""); err != nil || got != 0 {
		t.Fatalf("空游标表示第一页：%d %v", got, err)
	}
	if got, err := decodeIDCursor(" 42 "); err != nil || got != 42 {
		t.Fatalf("游标解析错误：%d %v", got, err)
	}
	// 不可解析/非正数不得退化成「当作第一页」。
	for _, bad := range []string{"abc", "0", "-3", "1.5", "9999999999999999999999"} {
		if _, err := decodeIDCursor(bad); !errors.Is(err, model.ErrCursorInvalid) {
			t.Fatalf("游标 %q 必须报错：%v", bad, err)
		}
	}
	if encodeIDCursor(0) != "" || encodeIDCursor(-1) != "" {
		t.Fatal("非正数不应产出游标")
	}
	if encodeIDCursor(7) != "7" {
		t.Fatal("游标编码错误")
	}
}

func TestNextCursorOnlyWhenPageFull(t *testing.T) {
	rows := []*model.LiveSession{{SessionID: 30}, {SessionID: 20}, {SessionID: 10}}
	if got := nextCursor(rows, 3); got != "10" {
		t.Fatalf("满页应给出最小 session_id 游标，实得 %q", got)
	}
	if got := nextCursor(rows, 5); got != "" {
		t.Fatalf("未满页应到底，实得 %q", got)
	}
	if got := nextCursor(nil, 3); got != "" {
		t.Fatalf("空页应到底，实得 %q", got)
	}
	if got := nextCursor([]*model.LiveSession{nil}, 1); got != "" {
		t.Fatalf("脏行不应给出游标，实得 %q", got)
	}
	if got := nextCursor(rows, 0); got != "" {
		t.Fatalf("limit<=0 属异常，不应给出游标，实得 %q", got)
	}
}

func TestListFiltersRejectUnknownEnum(t *testing.T) {
	if got, err := roomStateFilter(rpc.RoomState_ROOM_STATE_UNSPECIFIED); err != nil || got != 0 {
		t.Fatalf("未指定状态表示不过滤：%d %v", got, err)
	}
	for _, bad := range []rpc.RoomState{rpc.RoomState(9), rpc.RoomState(-1)} {
		if _, err := roomStateFilter(bad); !errors.Is(err, model.ErrInvalidRoomTransition) {
			t.Fatalf("未知房间状态 %d 应拒绝：%v", bad, err)
		}
	}
	if _, err := roomStateFilter(rpc.RoomState_ROOM_STATE_LIVING); err != nil {
		t.Fatalf("已定义状态应通过：%v", err)
	}
	for _, bad := range []rpc.RoomOrder{rpc.RoomOrder(3), rpc.RoomOrder(-1)} {
		if _, err := roomOrderFilter(bad); !errors.Is(err, model.ErrRoomOrderInvalid) {
			t.Fatalf("未知排序 %d 应拒绝（不得退化成无索引排序）：%v", bad, err)
		}
	}
	for _, ok := range []rpc.RoomOrder{rpc.RoomOrder_ROOM_ORDER_UNSPECIFIED,
		rpc.RoomOrder_ROOM_ORDER_LIVING_FIRST, rpc.RoomOrder_ROOM_ORDER_CTIME_DESC} {
		if _, err := roomOrderFilter(ok); err != nil {
			t.Fatalf("已声明排序 %d 应通过：%v", ok, err)
		}
	}
	if _, err := sessionStateFilter(rpc.SessionState(9)); !errors.Is(err, model.ErrInvalidSessionTransition) {
		t.Fatalf("未知场次状态应拒绝：%v", err)
	}
	if _, err := banStateFilter(4); !errors.Is(err, model.ErrInvalidBanTransition) {
		t.Fatalf("未知禁播记录状态应拒绝：%v", err)
	}
	if _, err := banStateFilter(0); err != nil {
		t.Fatalf("0 表示不过滤：%v", err)
	}
	if _, err := areaStateFilter(-1); !errors.Is(err, model.ErrAreaStateInvalid) {
		t.Fatalf("分区 -1 是「不过滤」，写入口必须拒绝：%v", err)
	}
	if got, err := areaStateFilter(model.AreaStateDisabled); err != nil || got != model.AreaStateDisabled {
		t.Fatalf("停用取值应通过：%d %v", got, err)
	}
	if _, err := anchorRoleFilter(rpc.AnchorRole(9)); !errors.Is(err, model.ErrAnchorRoleInvalid) {
		t.Fatalf("未知角色应拒绝：%v", err)
	}
}

func TestAnchorRoleForAction(t *testing.T) {
	if got, err := anchorRoleForAction(rpc.AnchorRole_ANCHOR_ROLE_COHOST,
		int32(rpc.AnchorAction_ANCHOR_ACTION_BIND)); err != nil || got != model.AnchorRoleCohost {
		t.Fatalf("BIND 联合主播应通过：%d %v", got, err)
	}
	if _, err := anchorRoleForAction(rpc.AnchorRole_ANCHOR_ROLE_OWNER,
		int32(rpc.AnchorAction_ANCHOR_ACTION_BIND)); !errors.Is(err, model.ErrAnchorRoleInvalid) {
		t.Fatalf("BIND 不允许直接绑房主（移交走 TransferOwner）：%v", err)
	}
	if _, err := anchorRoleForAction(rpc.AnchorRole_ANCHOR_ROLE_UNSPECIFIED,
		int32(rpc.AnchorAction_ANCHOR_ACTION_BIND)); !errors.Is(err, model.ErrAnchorRoleInvalid) {
		t.Fatalf("BIND 必须给角色：%v", err)
	}
	if got, err := anchorRoleForAction(rpc.AnchorRole_ANCHOR_ROLE_UNSPECIFIED,
		int32(rpc.AnchorAction_ANCHOR_ACTION_UNBIND)); err != nil || got != model.AnchorRoleUnspecified {
		t.Fatalf("UNBIND 允许 0（解绑全部非房主）：%d %v", got, err)
	}
	if _, err := anchorRoleForAction(rpc.AnchorRole_ANCHOR_ROLE_OWNER,
		int32(rpc.AnchorAction_ANCHOR_ACTION_UNBIND)); !errors.Is(err, model.ErrCannotUnbindOwner) {
		t.Fatalf("房主行不得在此解绑：%v", err)
	}
}

func TestRiskCheckItemDecisions(t *testing.T) {
	cases := []struct {
		name     string
		in       *riskrpc.CheckActionReply
		passed   bool
		degraded bool
	}{
		{"nil 响应按未评估", nil, false, true},
		{"放行", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_ALLOW}, true, false},
		{"放行后复核", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_REVIEW}, true, false},
		{"要求校验", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_CHALLENGE, ActionCode: "need_captcha"}, false, false},
		{"拒绝", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_BLOCK}, false, false},
		{"裁决缺失", &riskrpc.CheckActionReply{}, false, false},
		{"降级下的放行也不算通过", &riskrpc.CheckActionReply{
			Decision: riskrpc.Decision_DECISION_ALLOW, Degraded: true, Basis: "fallback_db_unavailable"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			item := riskCheckItem(c.in)
			if item.GetCode() != checkRiskControl {
				t.Fatalf("code 固定为 %s，实得 %q", checkRiskControl, item.GetCode())
			}
			if item.GetPassed() != c.passed || item.GetDegraded() != c.degraded {
				t.Fatalf("passed/degraded = %t/%t, want %t/%t",
					item.GetPassed(), item.GetDegraded(), c.passed, c.degraded)
			}
			if !c.passed && item.GetDetail() == "" {
				t.Fatal("未通过项必须给出可解释说明")
			}
		})
	}
	if got := riskRetryAfter(nil); got != 0 {
		t.Fatalf("无响应不应给出重试时间：%d", got)
	}
	if got := riskRetryAfter(&riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_BLOCK}); got != 0 {
		t.Fatalf("BLOCK 不需要定时重试：%d", got)
	}
	if got := riskRetryAfter(&riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_CHALLENGE,
		ChallengeTtlSeconds: 45}); got != 45 {
		t.Fatalf("应回风控给的有效期，实得 %d", got)
	}
	if got := riskRetryAfter(&riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_CHALLENGE}); got == 0 {
		t.Fatal("CHALLENGE 未给有效期时必须有兜底间隔")
	}
}

func TestSettingRowNormalization(t *testing.T) {
	def := defaultSettingRow(7)
	if def.RoomID != 7 || def.LiveType != model.LiveTypeVideo {
		t.Fatalf("默认配置错误：%+v", def)
	}
	if def.DanmakuEnabled != model.BoolToInt32(true) || def.ReplyEnabled != model.BoolToInt32(true) {
		t.Fatal("弹幕/回复默认开启")
	}
	// 回放是显式 opt-in：缺配就录用户直播内容属于越权。
	if def.RecordEnabled != model.BoolToInt32(false) || def.LinkmicEnabled != model.BoolToInt32(false) {
		t.Fatal("录制与连麦默认关闭")
	}
	got, err := settingRowFromRequest(7, nil)
	if err != nil || *got != *def {
		t.Fatalf("nil 配置应套服务端默认：%+v %+v %v", got, def, err)
	}
	if _, err := settingRowFromRequest(7, &rpc.RoomSetting{LiveType: 9}); !errors.Is(err, model.ErrSettingInvalid) {
		t.Fatalf("未知直播类型应拒绝：%v", err)
	}
	if _, err := settingRowFromRequest(7, &rpc.RoomSetting{LiveType: 0}); err != nil {
		t.Fatalf("live_type=0 由 GetLiveType 归一前的校验兜住：%v", err)
	}
	if _, err := settingRowFromRequest(7, &rpc.RoomSetting{MinClientVersionCode: -1}); !errors.Is(err, model.ErrSettingInvalid) {
		t.Fatalf("负版本号应拒绝：%v", err)
	}
	row, err := settingRowFromRequest(7, &rpc.RoomSetting{
		DanmakuEnabled: false, ReplyEnabled: false, RecordEnabled: true,
		LinkmicEnabled: true, LiveType: model.LiveTypeScreen, MinClientVersionCode: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.RecordEnabled != model.BoolToInt32(true) || row.DanmakuEnabled != model.BoolToInt32(false) ||
		row.LiveType != model.LiveTypeScreen || row.MinClientVersionCode != 12 {
		t.Fatalf("整段覆盖语义丢失：%+v", row)
	}
}

func TestProfileDiffHelpers(t *testing.T) {
	if got, err := changedTitle("", "旧标题", 80); err != nil || got != "" {
		t.Fatalf("未传标题表示不改：%q %v", got, err)
	}
	if got, err := changedTitle("  旧标题 ", "旧标题", 80); err != nil || got != "" {
		t.Fatalf("与现值相同不算改动：%q %v", got, err)
	}
	if got, err := changedTitle("新标题", "旧标题", 80); err != nil || got != "新标题" {
		t.Fatalf("改动应回新值：%q %v", got, err)
	}
	if _, err := changedTitle(strings.Repeat("长", 81), "旧标题", 80); !errors.Is(err, model.ErrTitleInvalid) {
		t.Fatalf("超长标题应拒绝：%v", err)
	}
	if got, err := changedRef("https://a/b", "cover.png", checkCover); err == nil {
		t.Fatalf("绝对地址必须拒绝：%q", got)
	}
	if got, err := changedRef("cover.png", "cover.png", checkCover); err != nil || got != "" {
		t.Fatalf("同值不改：%q %v", got, err)
	}
	if stateIn(nil, model.RoomStateReady) {
		t.Fatal("空集合不应命中")
	}
	if ownerMidOf(nil) != 0 {
		t.Fatal("无生效房主应回 0")
	}
	if ownerMidOf(&model.LiveRoomAnchor{Mid: 9}) != 9 {
		t.Fatal("应回房主 mid")
	}
}

func TestReplayRefsEqualAndFirstErr(t *testing.T) {
	in := &rpc.AttachReplayReq{RecordId: 1, RecordAssetId: 2, RecordAid: 3}
	same := &model.LiveSession{RecordID: 1, RecordAssetID: 2, RecordAid: 3}
	diff := &model.LiveSession{RecordID: 1, RecordAssetID: 9, RecordAid: 3}
	if !replayRefsEqual(same, in) {
		t.Fatal("引用一致应判为幂等重放")
	}
	if replayRefsEqual(diff, in) {
		t.Fatal("引用不同不得判为幂等")
	}
	if err := firstErr(nil, errNoOp); err != nil {
		t.Fatalf("errNoOp 不算错误：%v", err)
	}
	if err := firstErr(nil, model.ErrRoomNotFound, model.ErrBanNotFound); !errors.Is(err, model.ErrRoomNotFound) {
		t.Fatalf("应回第一个真实错误：%v", err)
	}
}
