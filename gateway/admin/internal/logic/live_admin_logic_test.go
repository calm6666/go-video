package logic

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 live-room 的口径：rpc→types 投影逐字段不丢（含终端面裁掉的
// last_stream_seq / record_id / moderation_task_id / ban reason / area operator_mid）、
// 写入口的三道门槛（会话身份、operator_mid、request_id）、CloseRoom 的 admin 位只能由网关固定、
// 整段覆盖的配置入参不得带上服务端字段，以及未配置下游时一律 fail-closed。
//
// 「房间状态机是否允许这次迁移」「禁播时长与到期时间怎么算」「分区名称是否唯一、停用后是否仍有
// 子分区或在线房间」「operator_mid 是否是该房间生效房主」都是 services/live-room 的领域规则，
// 网关不复算（AGENTS.md §5/§8/§9）：下面断言的是「网关把入参原样交给下游 + 下游结论原样回传」，
// 而不是「网关自己判过」。
// 打桩方式与 cron/audit 测试一致：内嵌生成的 client 接口 + 覆盖所需方法，
// 不建 gRPC 连接、不碰数据库。

var errLiveFakeDownstream = errors.New("live-room downstream unavailable")

// liveAdminFake 是 live-room RPC 的假客户端：记录入参、返回预置响应或预置错误。
type liveAdminFake struct {
	liveroomrpc.LiveRoomClient

	err   error
	calls int

	getRoomReq   *liveroomrpc.GetRoomReq
	getRoomReply *liveroomrpc.GetRoomReply
	listRoomsReq *liveroomrpc.ListRoomsReq
	listRooms    *liveroomrpc.ListRoomsReply
	closeReq     *liveroomrpc.CloseRoomReq
	closeReply   *liveroomrpc.CloseRoomReply
	banReq       *liveroomrpc.BanRoomReq
	banReply     *liveroomrpc.BanRoomReply
	liftReq      *liveroomrpc.LiftBanReq
	liftReply    *liveroomrpc.LiftBanReply
	listBansReq  *liveroomrpc.ListRoomBansReq
	listBans     *liveroomrpc.ListRoomBansReply
	getSessReq   *liveroomrpc.GetSessionReq
	getSess      *liveroomrpc.GetSessionReply
	listSessReq  *liveroomrpc.ListSessionsReq
	listSess     *liveroomrpc.ListSessionsReply
	settingReq   *liveroomrpc.UpdateRoomSettingReq
	settingReply *liveroomrpc.UpdateRoomSettingReply
	anchorReq    *liveroomrpc.ListAnchorsReq
	anchors      *liveroomrpc.ListAnchorsReply
	areaReq      *liveroomrpc.UpsertAreaReq
	areaReply    *liveroomrpc.UpsertAreaReply
	listAreaReq  *liveroomrpc.ListAreasReq
	listAreas    *liveroomrpc.ListAreasReply
}

func (f *liveAdminFake) GetRoom(_ context.Context, in *liveroomrpc.GetRoomReq,
	_ ...grpc.CallOption) (*liveroomrpc.GetRoomReply, error) {
	f.calls++
	f.getRoomReq = in
	return f.getRoomReply, f.err
}

func (f *liveAdminFake) ListRooms(_ context.Context, in *liveroomrpc.ListRoomsReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListRoomsReply, error) {
	f.calls++
	f.listRoomsReq = in
	return f.listRooms, f.err
}

func (f *liveAdminFake) CloseRoom(_ context.Context, in *liveroomrpc.CloseRoomReq,
	_ ...grpc.CallOption) (*liveroomrpc.CloseRoomReply, error) {
	f.calls++
	f.closeReq = in
	return f.closeReply, f.err
}

func (f *liveAdminFake) BanRoom(_ context.Context, in *liveroomrpc.BanRoomReq,
	_ ...grpc.CallOption) (*liveroomrpc.BanRoomReply, error) {
	f.calls++
	f.banReq = in
	return f.banReply, f.err
}

func (f *liveAdminFake) LiftBan(_ context.Context, in *liveroomrpc.LiftBanReq,
	_ ...grpc.CallOption) (*liveroomrpc.LiftBanReply, error) {
	f.calls++
	f.liftReq = in
	return f.liftReply, f.err
}

func (f *liveAdminFake) ListRoomBans(_ context.Context, in *liveroomrpc.ListRoomBansReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListRoomBansReply, error) {
	f.calls++
	f.listBansReq = in
	return f.listBans, f.err
}

func (f *liveAdminFake) GetSession(_ context.Context, in *liveroomrpc.GetSessionReq,
	_ ...grpc.CallOption) (*liveroomrpc.GetSessionReply, error) {
	f.calls++
	f.getSessReq = in
	return f.getSess, f.err
}

func (f *liveAdminFake) ListSessions(_ context.Context, in *liveroomrpc.ListSessionsReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListSessionsReply, error) {
	f.calls++
	f.listSessReq = in
	return f.listSess, f.err
}

func (f *liveAdminFake) UpdateRoomSetting(_ context.Context, in *liveroomrpc.UpdateRoomSettingReq,
	_ ...grpc.CallOption) (*liveroomrpc.UpdateRoomSettingReply, error) {
	f.calls++
	f.settingReq = in
	return f.settingReply, f.err
}

func (f *liveAdminFake) ListAnchors(_ context.Context, in *liveroomrpc.ListAnchorsReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListAnchorsReply, error) {
	f.calls++
	f.anchorReq = in
	return f.anchors, f.err
}

func (f *liveAdminFake) UpsertArea(_ context.Context, in *liveroomrpc.UpsertAreaReq,
	_ ...grpc.CallOption) (*liveroomrpc.UpsertAreaReply, error) {
	f.calls++
	f.areaReq = in
	return f.areaReply, f.err
}

func (f *liveAdminFake) ListAreas(_ context.Context, in *liveroomrpc.ListAreasReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListAreasReply, error) {
	f.calls++
	f.listAreaReq = in
	return f.listAreas, f.err
}

func liveAdminSvc(fake liveroomrpc.LiveRoomClient) *svc.ServiceContext {
	return &svc.ServiceContext{LiveRoom: fake}
}

// liveAdminSessionCtx 模拟 AdminPermission 中间件已解析出会话身份的请求上下文。
func liveAdminSessionCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77,
		Roles:   []string{"live_ops"},
	})
}

// --- 投影逐字段不丢 ---

func liveFixtureRoom() *liveroomrpc.RoomInfo {
	return &liveroomrpc.RoomInfo{
		RoomId: 1, OwnerMid: 2, Title: "标题", Cover: "bucket/cover.jpg", AreaId: 3,
		State:           liveroomrpc.RoomState_ROOM_STATE_LIVING,
		VerifyState:     liveroomrpc.VerifyState_VERIFY_STATE_PASSED,
		ActiveSessionId: 4, ActiveStreamId: "stream-5", StateVersion: 6,
		RejectReason: "封面违规", BanUntil: 7, Ctime: 8, Mtime: 9,
	}
}

func liveFixtureSetting() *liveroomrpc.RoomSetting {
	return &liveroomrpc.RoomSetting{
		RoomId: 1, DanmakuEnabled: true, ReplyEnabled: false, RecordEnabled: true,
		LinkmicEnabled: false, LiveType: 3, MinClientVersionCode: 108, Mtime: 11,
	}
}

func liveFixtureSession() *liveroomrpc.SessionInfo {
	return &liveroomrpc.SessionInfo{
		SessionId: 21, RoomId: 22, Mid: 23,
		State:         liveroomrpc.SessionState_SESSION_STATE_ENDED,
		TitleSnapshot: "开播时标题", AreaIdSnapshot: 24, StreamId: "stream-25",
		StartedAt: 26, EndedAt: 27, DurationSeconds: 28,
		EndReason:     liveroomrpc.EndReason_END_REASON_BANNED,
		LastStreamSeq: 29,
		ReplayState:   liveroomrpc.ReplayState_REPLAY_STATE_AVAILABLE,
		RecordId:      30, RecordAssetId: 31, RecordAid: 32, ModerationTaskId: 33,
		Ctime: 34, Mtime: 35,
	}
}

func liveFixtureBan() *liveroomrpc.RoomBanInfo {
	return &liveroomrpc.RoomBanInfo{
		BanId: 41, RoomId: 42, Mid: 43,
		BanType: liveroomrpc.BanType_BAN_TYPE_TEMPORARY,
		Reason:  "运营内部说明", StartAt: 44, EndAt: 45, State: 2,
		OperatorMid: 46, LiftOperatorMid: 47, LiftReason: "误判解除", LiftedAt: 48,
		Ctime: 49,
	}
}

func liveFixtureArea() *liveroomrpc.AreaInfo {
	return &liveroomrpc.AreaInfo{
		AreaId: 51, AreaName: "网游", ParentAreaId: 52, Sort: 3, State: 1,
		OperatorMid: 53, Ctime: 54, Mtime: 55,
	}
}

func liveFixtureAnchor() *liveroomrpc.AnchorInfo {
	return &liveroomrpc.AnchorInfo{
		Id: 61, RoomId: 62, Mid: 63,
		Role:  liveroomrpc.AnchorRole_ANCHOR_ROLE_MANAGER,
		State: 1, Ctime: 64, Mtime: 65,
	}
}

// TestLiveProjectionKeepsEveryField 用互不相同的非零值逐字段比对：
// 投影漏一列或错位，DeepEqual 就会失败（尤其是终端面会被裁掉、后台必须保留的那几个字段）。
func TestLiveProjectionKeepsEveryField(t *testing.T) {
	if got, want := liveRoomToAPI(liveFixtureRoom()), (types.LiveRoomInfo{
		RoomId: 1, OwnerMid: 2, Title: "标题", Cover: "bucket/cover.jpg", AreaId: 3,
		State: 3, VerifyState: 3, ActiveSessionId: 4, ActiveStreamId: "stream-5",
		StateVersion: 6, RejectReason: "封面违规", BanUntil: 7, Ctime: 8, Mtime: 9,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoomInfo 投影不符:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := liveSettingToAPI(liveFixtureSetting()), (types.LiveRoomSetting{
		RoomId: 1, DanmakuEnabled: true, ReplyEnabled: false, RecordEnabled: true,
		LinkmicEnabled: false, LiveType: 3, MinClientVersionCode: 108, Mtime: 11,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoomSetting 投影不符:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := liveSessionToAPI(liveFixtureSession()), (types.LiveSessionInfo{
		SessionId: 21, RoomId: 22, Mid: 23, State: 3, TitleSnapshot: "开播时标题",
		AreaIdSnapshot: 24, StreamId: "stream-25", StartedAt: 26, EndedAt: 27,
		DurationSeconds: 28, EndReason: 2, LastStreamSeq: 29, ReplayState: 3,
		RecordId: 30, RecordAssetId: 31, RecordAid: 32, ModerationTaskId: 33,
		Ctime: 34, Mtime: 35,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("SessionInfo 投影不符:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := liveBansToAPI([]*liveroomrpc.RoomBanInfo{liveFixtureBan()}),
		[]types.LiveRoomBanInfo{{
			BanId: 41, RoomId: 42, Mid: 43, BanType: 1, Reason: "运营内部说明",
			StartAt: 44, EndAt: 45, State: 2, OperatorMid: 46, LiftOperator: 47,
			LiftReason: "误判解除", LiftedAt: 48, Ctime: 49,
		}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RoomBanInfo 投影不符:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := liveAreasToAPI([]*liveroomrpc.AreaInfo{liveFixtureArea()}),
		[]types.LiveAreaInfo{{
			AreaId: 51, AreaName: "网游", ParentAreaId: 52, Sort: 3, State: 1,
			OperatorMid: 53, Ctime: 54, Mtime: 55,
		}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AreaInfo 投影不符:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := liveAnchorsToAPI([]*liveroomrpc.AnchorInfo{liveFixtureAnchor()}),
		[]types.LiveAnchorInfo{{
			Id: 61, RoomId: 62, Mid: 63, Role: 3, State: 1, Ctime: 64, Mtime: 65,
		}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AnchorInfo 投影不符:\ngot  %+v\nwant %+v", got, want)
	}
}

// 可选消息必须用 has_* 表达「服务有没有给这一段」，不能让全 false 的零值冒充「配置全关」。
func TestLiveRoomDetailHasFlagsFollowOptionalMessages(t *testing.T) {
	full := liveRoomDetailFromReply(&liveroomrpc.GetRoomReply{
		Room: liveFixtureRoom(), Setting: liveFixtureSetting(), ActiveSession: liveFixtureSession(),
	})
	if !full.HasSetting || !full.HasActiveSession {
		t.Fatalf("给了 setting/active_session 时 has_* 必须为 true: %+v", full)
	}
	only := liveRoomDetailFromReply(&liveroomrpc.GetRoomReply{Room: liveFixtureRoom()})
	if only.HasSetting || only.HasActiveSession {
		t.Fatalf("未附带时 has_* 必须为 false:\n%+v", only)
	}
	if only.Room.RoomId != 1 {
		t.Fatalf("房间主体丢失: %+v", only.Room)
	}
}

// --- 只读面入参门槛 ---

func TestLiveRoomGetRequiresSubjectAndPassesThrough(t *testing.T) {
	fake := &liveAdminFake{getRoomReply: &liveroomrpc.GetRoomReply{
		Room: liveFixtureRoom(), Setting: liveFixtureSetting(),
	}}
	svcCtx := liveAdminSvc(fake)

	if _, err := NewLiveRoomGetLogic(context.Background(), svcCtx).
		LiveRoomGet(&types.ParamLiveRoomGet{}); !errors.Is(err, errLiveRoomSubjectRequired) {
		t.Fatalf("room_id 与 owner_mid 全缺时应提示参数缺失（而不是下游的房间不存在），实际 %v", err)
	}
	if _, err := NewLiveRoomGetLogic(context.Background(), svcCtx).
		LiveRoomGet(&types.ParamLiveRoomGet{RoomId: -1}); err == nil {
		t.Fatalf("负数主键应在网关拒掉")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用下游, calls=%d", fake.calls)
	}

	resp, err := NewLiveRoomGetLogic(context.Background(), svcCtx).
		LiveRoomGet(&types.ParamLiveRoomGet{RoomId: 1, WithSetting: true})
	if err != nil {
		t.Fatalf("正常路径应成功: %v", err)
	}
	in := fake.getRoomReq
	if in.GetRoomId() != 1 || !in.GetWithSetting() || in.GetWithActiveSession() {
		t.Fatalf("GetRoom 入参不符: %+v", in)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封字段不符: %+v", resp)
	}
	if !resp.Data.HasSetting || resp.Data.Room.RoomId != 1 || resp.Data.Setting.LiveType != 3 {
		t.Fatalf("房间详情投影不符: %+v", resp.Data)
	}
}

func TestLiveRoomListPassesFiltersAndPagingBack(t *testing.T) {
	fake := &liveAdminFake{listRooms: &liveroomrpc.ListRoomsReply{
		Rooms: []*liveroomrpc.RoomInfo{liveFixtureRoom()}, Total: 7, Page: 2, PageSize: 50,
	}}
	if _, err := NewLiveRoomListLogic(context.Background(), liveAdminSvc(fake)).
		LiveRoomList(&types.ParamLiveRoomList{State: -1}); err == nil {
		t.Fatalf("state 负数应在网关拒掉")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用下游, calls=%d", fake.calls)
	}
	resp, err := NewLiveRoomListLogic(context.Background(), liveAdminSvc(fake)).
		LiveRoomList(&types.ParamLiveRoomList{AreaId: 3, State: 5, Order: 1, Page: 2, PageSize: 50})
	if err != nil {
		t.Fatalf("列表应成功: %v", err)
	}
	in := fake.listRoomsReq
	// 枚举原样透传：状态是否可查、页大小上限都归 live-room。
	if in.GetAreaId() != 3 || in.GetState() != liveroomrpc.RoomState_ROOM_STATE_BANNED ||
		in.GetOrder() != liveroomrpc.RoomOrder_ROOM_ORDER_LIVING_FIRST || in.GetPage() != 2 {
		t.Fatalf("ListRooms 入参不符: %+v", in)
	}
	if resp.Data.Total != 7 || resp.Data.Page != 2 || resp.Data.PageSize != 50 {
		t.Fatalf("分页结论应回传服务实际值: %+v", resp.Data)
	}
	if len(resp.Data.List) != 1 || resp.Data.List[0].State != 3 {
		t.Fatalf("列表投影不符: %+v", resp.Data.List)
	}
}

// 空列表与「下游没接」是两回事：前者投影成 []，后者一律错误。
func TestLiveListsReturnNonNilSlices(t *testing.T) {
	fake := &liveAdminFake{
		listRooms: &liveroomrpc.ListRoomsReply{},
		listBans:  &liveroomrpc.ListRoomBansReply{},
		listSess:  &liveroomrpc.ListSessionsReply{},
		listAreas: &liveroomrpc.ListAreasReply{},
		anchors:   &liveroomrpc.ListAnchorsReply{},
	}
	ctx := context.Background()
	svcCtx := liveAdminSvc(fake)

	roomResp, err := NewLiveRoomListLogic(ctx, svcCtx).LiveRoomList(&types.ParamLiveRoomList{})
	if err != nil || roomResp.Data.List == nil {
		t.Fatalf("房间空列表应投影成 []: err=%v list=%v", err, roomResp.Data.List)
	}
	banResp, err := NewLiveRoomBansLogic(ctx, svcCtx).
		LiveRoomBans(&types.ParamLiveRoomBans{OperatorMid: 46})
	if err != nil || banResp.Data.List == nil {
		t.Fatalf("禁播台账空列表应投影成 []: err=%v", err)
	}
	sessResp, err := NewLiveSessionListLogic(ctx, svcCtx).
		LiveSessionList(&types.ParamLiveSessionList{RoomId: 1})
	if err != nil || sessResp.Data.List == nil {
		t.Fatalf("场次空列表应投影成 []: err=%v", err)
	}
	areaResp, err := NewLiveAreaListLogic(ctx, svcCtx).LiveAreaList(&types.ParamLiveAreaList{})
	if err != nil || areaResp.Data.List == nil {
		t.Fatalf("分区空列表应投影成 []: err=%v", err)
	}
	anchorResp, err := NewLiveAnchorListLogic(ctx, svcCtx).
		LiveAnchorList(&types.ParamLiveAnchorList{RoomId: 1})
	if err != nil || anchorResp.Data.List == nil {
		t.Fatalf("主播绑定空列表应投影成 []: err=%v", err)
	}
}

// 台账读取也必须有主体：reason 属运营内部说明，live-room 拒无归因的读取。
func TestLiveRoomBansRequiresOperator(t *testing.T) {
	fake := &liveAdminFake{listBans: &liveroomrpc.ListRoomBansReply{}}
	if _, err := NewLiveRoomBansLogic(context.Background(), liveAdminSvc(fake)).
		LiveRoomBans(&types.ParamLiveRoomBans{}); err == nil {
		t.Fatalf("operator_mid<=0 时应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("无主体时不应调用下游, calls=%d", fake.calls)
	}
	if _, err := NewLiveRoomBansLogic(context.Background(), liveAdminSvc(fake)).
		LiveRoomBans(&types.ParamLiveRoomBans{OperatorMid: 46, State: 1, Page: 1}); err != nil {
		t.Fatalf("带主体应成功: %v", err)
	}
	if fake.listBansReq.GetOperatorMid() != 46 || fake.listBansReq.GetState() != 1 {
		t.Fatalf("ListRoomBans 入参不符: %+v", fake.listBansReq)
	}
}

func TestLiveSessionReadRoutes(t *testing.T) {
	fake := &liveAdminFake{
		getSess: &liveroomrpc.GetSessionReply{Session: liveFixtureSession()},
	}
	svcCtx := liveAdminSvc(fake)
	ctx := context.Background()

	if _, err := NewLiveSessionGetLogic(ctx, svcCtx).LiveSessionGet(&types.ParamLiveSessionGet{}); !errors.Is(err, errLiveSessionSubjectRequired) {
		t.Fatalf("session_id/room_id 全缺时应提示参数缺失，实际 %v", err)
	}
	getResp, err := NewLiveSessionGetLogic(ctx, svcCtx).
		LiveSessionGet(&types.ParamLiveSessionGet{RoomId: 22, Offset: 2})
	if err != nil {
		t.Fatalf("按房间取最近一场应成功: %v", err)
	}
	if fake.getSessReq.GetRoomId() != 22 || fake.getSessReq.GetOffset() != 2 {
		t.Fatalf("GetSession 入参不符: %+v", fake.getSessReq)
	}
	if getResp.Data.Session.ModerationTaskId != 33 || getResp.Data.Session.LastStreamSeq != 29 {
		t.Fatalf("运营面必须保留内部字段: %+v", getResp.Data.Session)
	}

	if _, err := NewLiveSessionListLogic(ctx, svcCtx).
		LiveSessionList(&types.ParamLiveSessionList{}); err == nil {
		t.Fatalf("room_id 缺失时应拒绝（场次只在房间内有序）")
	}
	long := strings.Repeat("c", liveCursorMaxLen+1)
	if _, err := NewLiveSessionListLogic(ctx, svcCtx).
		LiveSessionList(&types.ParamLiveSessionList{RoomId: 1, Cursor: long}); err == nil {
		t.Fatalf("超长游标应在网关挡掉")
	}
	if _, err := NewLiveSessionListLogic(ctx, svcCtx).
		LiveSessionList(&types.ParamLiveSessionList{RoomId: 1, Cursor: "21"}); err != nil {
		t.Fatalf("短游标应原样透传: %v", err)
	}
	if fake.listSessReq.GetCursor() != "21" {
		t.Fatalf("游标必须原样透传（网关不改写不解释）: %+v", fake.listSessReq)
	}
}

// 分区列表的 -1 是「不过滤」哨兵：省略参数不能退化成「只有一级分区」或「只有停用分区」。
func TestLiveAreaListKeepsSentinelDefaults(t *testing.T) {
	fake := &liveAdminFake{listAreas: &liveroomrpc.ListAreasReply{
		Areas: []*liveroomrpc.AreaInfo{liveFixtureArea()}, Total: 1,
	}}
	if _, err := NewLiveAreaListLogic(context.Background(), liveAdminSvc(fake)).
		LiveAreaList(&types.ParamLiveAreaList{ParentAreaId: -2}); err == nil {
		t.Fatalf("小于 -1 的哨兵没有下游语义，应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用下游, calls=%d", fake.calls)
	}
	resp, err := NewLiveAreaListLogic(context.Background(), liveAdminSvc(fake)).
		LiveAreaList(&types.ParamLiveAreaList{ParentAreaId: -1, State: -1, Page: 1})
	if err != nil {
		t.Fatalf("默认参数应成功: %v", err)
	}
	if fake.listAreaReq.GetParentAreaId() != -1 || fake.listAreaReq.GetState() != -1 {
		t.Fatalf("哨兵必须原样透传: %+v", fake.listAreaReq)
	}
	if resp.Data.List[0].OperatorMid != 53 || resp.Data.List[0].Mtime != 55 {
		t.Fatalf("后台面要保留分区运营字段: %+v", resp.Data.List[0])
	}
}

func TestLiveAnchorListRequiresRoom(t *testing.T) {
	fake := &liveAdminFake{anchors: &liveroomrpc.ListAnchorsReply{
		Anchors: []*liveroomrpc.AnchorInfo{liveFixtureAnchor()}, Total: 1,
	}}
	if _, err := NewLiveAnchorListLogic(context.Background(), liveAdminSvc(fake)).
		LiveAnchorList(&types.ParamLiveAnchorList{}); err == nil {
		t.Fatalf("room_id 缺失时应拒绝")
	}
	if _, err := NewLiveAnchorListLogic(context.Background(), liveAdminSvc(fake)).
		LiveAnchorList(&types.ParamLiveAnchorList{RoomId: 62, Role: 3, Page: 1}); err != nil {
		t.Fatalf("带房间应成功: %v", err)
	}
	if fake.anchorReq.GetRole() != liveroomrpc.AnchorRole_ANCHOR_ROLE_MANAGER ||
		fake.anchorReq.GetOnlyEnabled() || fake.anchorReq.GetPage() != 1 {
		t.Fatalf("ListAnchors 入参不符: %+v", fake.anchorReq)
	}
}

// --- 写面门槛 ---

// 受保护写路由拿不到会话身份时一律 fail-closed，绝不退化成匿名处置。
func TestLiveWriteRoutesRequireSessionIdentity(t *testing.T) {
	ctx := context.Background() // 没有 AdminFromContext
	svcCtx := liveAdminSvc(&liveAdminFake{})
	cases := map[string]func() error{
		"close": func() error {
			_, err := NewLiveRoomCloseLogic(ctx, svcCtx).LiveRoomClose(
				&types.ParamLiveRoomClose{RoomId: 1, OperatorMid: 46, RequestId: "k1"})
			return err
		},
		"ban": func() error {
			_, err := NewLiveRoomBanLogic(ctx, svcCtx).LiveRoomBan(
				&types.ParamLiveRoomBan{RoomId: 1, BanType: 1, DurationSeconds: 3600,
					OperatorMid: 46, RequestId: "k1"})
			return err
		},
		"lift": func() error {
			_, err := NewLiveRoomBanLiftLogic(ctx, svcCtx).LiveRoomBanLift(
				&types.ParamLiveRoomBanLift{RoomId: 1, OperatorMid: 46, RequestId: "k1"})
			return err
		},
		"setting": func() error {
			_, err := NewLiveRoomSettingUpdateLogic(ctx, svcCtx).LiveRoomSettingUpdate(
				&types.ParamLiveRoomSettingUpdate{RoomId: 1, OperatorMid: 46, RequestId: "k1"})
			return err
		},
		"area": func() error {
			_, err := NewLiveAreaUpsertLogic(ctx, svcCtx).LiveAreaUpsert(
				&types.ParamLiveAreaUpsert{AreaName: "网游", State: 1, OperatorMid: 46, RequestId: "k1"})
			return err
		},
	}
	if len(cases) != 5 {
		t.Fatalf("live 写路由应有 5 条，实际 %d", len(cases))
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errLiveSessionRequired) {
				t.Fatalf("无会话身份时应 fail-closed，实际 err=%v", err)
			}
		})
	}
}

// operator_mid 与 request_id 都是硬门槛：处置必须有主体，没有幂等键的重放就是两次副作用。
func TestLiveWriteRoutesRequireOperatorAndRequestId(t *testing.T) {
	ctx := liveAdminSessionCtx()
	fake := &liveAdminFake{}
	svcCtx := liveAdminSvc(fake)

	if _, err := NewLiveRoomCloseLogic(ctx, svcCtx).LiveRoomClose(
		&types.ParamLiveRoomClose{RoomId: 1, RequestId: "k1"}); err == nil {
		t.Fatalf("operator_mid<=0 应拒绝")
	}
	if _, err := NewLiveRoomBanLogic(ctx, svcCtx).LiveRoomBan(
		&types.ParamLiveRoomBan{RoomId: 1, OperatorMid: 46}); err == nil {
		t.Fatalf("request_id 为空应拒绝")
	}
	if _, err := NewLiveRoomBanLiftLogic(ctx, svcCtx).LiveRoomBanLift(
		&types.ParamLiveRoomBanLift{RoomId: 1, OperatorMid: 46, RequestId: "  "}); err == nil {
		t.Fatalf("只有空白的 request_id 应拒绝")
	}
	if _, err := NewLiveRoomSettingUpdateLogic(ctx, svcCtx).LiveRoomSettingUpdate(
		&types.ParamLiveRoomSettingUpdate{OperatorMid: 46, RequestId: "k1"}); err == nil {
		t.Fatalf("room_id 缺失应拒绝")
	}
	if _, err := NewLiveAreaUpsertLogic(ctx, svcCtx).LiveAreaUpsert(
		&types.ParamLiveAreaUpsert{AreaName: "  ", State: 1, OperatorMid: 46, RequestId: "k1"}); err == nil {
		t.Fatalf("area_name 空白应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("门槛未过时不应调用下游, calls=%d", fake.calls)
	}
}

// 幂等键只判空、不改写：任何一次 TrimSpace 回写都会让它失去「同一重试同一键」的语义。
func TestLiveIdempotencyKeyPassesThroughUnchanged(t *testing.T) {
	ctx := liveAdminSessionCtx()
	fake := &liveAdminFake{liftReply: &liveroomrpc.LiftBanReply{}}
	const key = "  live-ban-lift-001  " // 前后空格属于键本身
	if _, err := NewLiveRoomBanLiftLogic(ctx, liveAdminSvc(fake)).LiveRoomBanLift(
		&types.ParamLiveRoomBanLift{RoomId: 1, OperatorMid: 46, RequestId: key}); err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if fake.liftReq.GetRequestId() != key {
		t.Fatalf("request_id 被改写了: %q", fake.liftReq.GetRequestId())
	}
}

// CloseRoom 的 admin 位是权限结论，不是表单字段：只能由网关固定为 true。
func TestLiveRoomCloseAlwaysMarksAdminSource(t *testing.T) {
	fake := &liveAdminFake{closeReply: &liveroomrpc.CloseRoomReply{
		State: liveroomrpc.RoomState_ROOM_STATE_FINISHED, TerminatedSessionId: 21, Replayed: true,
	}}
	resp, err := NewLiveRoomCloseLogic(liveAdminSessionCtx(), liveAdminSvc(fake)).
		LiveRoomClose(&types.ParamLiveRoomClose{RoomId: 1, OperatorMid: 46, Reason: "版权撤回", RequestId: "k1"})
	if err != nil {
		t.Fatalf("关闭应成功: %v", err)
	}
	in := fake.closeReq
	if !in.GetAdmin() {
		t.Fatalf("后台侧关闭必须带 admin=true，否则 live-room 会按房主判定并拒绝: %+v", in)
	}
	if in.GetOperatorMid() != 46 || in.GetReason() != "版权撤回" || in.GetRequestId() != "k1" {
		t.Fatalf("关闭入参不符: %+v", in)
	}
	if resp.Data.State != 4 || resp.Data.TerminatedSessionId != 21 || !resp.Data.Replayed {
		t.Fatalf("关闭结论应原样回传（含幂等重放标记）: %+v", resp.Data)
	}
}

// 禁播的时长/到期组合归 live-room：网关不换算秒、不补默认值，只做非负门槛。
func TestLiveRoomBanForwardsDurationVerbatim(t *testing.T) {
	fake := &liveAdminFake{banReply: &liveroomrpc.BanRoomReply{
		BanId: 41, State: liveroomrpc.RoomState_ROOM_STATE_BANNED, EndAt: 0, Replayed: false,
	}}
	if _, err := NewLiveRoomBanLogic(liveAdminSessionCtx(), liveAdminSvc(fake)).LiveRoomBan(
		&types.ParamLiveRoomBan{RoomId: 1, BanType: 2, DurationSeconds: -1,
			OperatorMid: 46, RequestId: "k1"}); err == nil {
		t.Fatalf("负数时长应在网关挡掉（下游没有对应语义）")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用下游, calls=%d", fake.calls)
	}
	// reason 为空由 live-room 判定（checkReason required=true），网关不代替它宣布必填。
	resp, err := NewLiveRoomBanLogic(liveAdminSessionCtx(), liveAdminSvc(fake)).LiveRoomBan(
		&types.ParamLiveRoomBan{RoomId: 1, BanType: 2, OperatorMid: 46, RequestId: "k1"})
	if err != nil {
		t.Fatalf("永久禁播应交给下游判定: %v", err)
	}
	if fake.banReq.GetDurationSeconds() != 0 ||
		fake.banReq.GetBanType() != liveroomrpc.BanType_BAN_TYPE_PERMANENT {
		t.Fatalf("BanRoom 入参不符: %+v", fake.banReq)
	}
	if resp.Data.BanId != 41 || resp.Data.State != 5 || resp.Data.EndAt != 0 {
		t.Fatalf("禁播结论应原样回传: %+v", resp.Data)
	}
}

// 整段覆盖的配置：客户端只能声明六个可写字段，room_id/mtime 由服务维护。
func TestLiveRoomSettingUpdateAssemblesWholeSetting(t *testing.T) {
	fake := &liveAdminFake{settingReply: &liveroomrpc.UpdateRoomSettingReply{
		Setting: liveFixtureSetting(), Replayed: true,
	}}
	resp, err := NewLiveRoomSettingUpdateLogic(liveAdminSessionCtx(), liveAdminSvc(fake)).
		LiveRoomSettingUpdate(&types.ParamLiveRoomSettingUpdate{
			RoomId: 1, OperatorMid: 46, RequestId: "k1",
			Setting: types.LiveRoomSettingInput{
				DanmakuEnabled: true, ReplyEnabled: false, RecordEnabled: true,
				LinkmicEnabled: false, LiveType: 3, MinClientVersionCode: 108,
			},
		})
	if err != nil {
		t.Fatalf("配置更新应成功: %v", err)
	}
	s := fake.settingReq.GetSetting()
	if s == nil {
		t.Fatalf("setting 必须整段传给下游（nil 会被服务当成漏传拒绝）")
	}
	if s.GetRoomId() != 0 || s.GetMtime() != 0 {
		t.Fatalf("服务端字段不得由后台声明: %+v", s)
	}
	if !s.GetDanmakuEnabled() || s.GetReplyEnabled() || !s.GetRecordEnabled() ||
		s.GetLinkmicEnabled() || s.GetLiveType() != 3 || s.GetMinClientVersionCode() != 108 {
		t.Fatalf("配置字段丢失或错位: %+v", s)
	}
	if fake.settingReq.GetRoomId() != 1 || fake.settingReq.GetOperatorMid() != 46 {
		t.Fatalf("主体与房间主键不符: %+v", fake.settingReq)
	}
	if !resp.Data.Replayed || resp.Data.Setting.LiveType != 3 || resp.Data.Setting.Mtime != 11 {
		t.Fatalf("更新后的配置应回传服务值: %+v", resp.Data)
	}
	if _, err := NewLiveRoomSettingUpdateLogic(liveAdminSessionCtx(), liveAdminSvc(fake)).
		LiveRoomSettingUpdate(&types.ParamLiveRoomSettingUpdate{
			RoomId: 1, OperatorMid: 46, RequestId: "k2",
			Setting: types.LiveRoomSettingInput{LiveType: -1}}); err == nil {
		t.Fatalf("负数 live_type 没有下游语义，应拒绝")
	}
}

// 分区新建与修改共用一个入口：created 由服务回传，网关不改写。
func TestLiveAreaUpsertKeepsCreateOrUpdateIntent(t *testing.T) {
	fake := &liveAdminFake{areaReply: &liveroomrpc.UpsertAreaReply{AreaId: 51, Created: true}}
	if _, err := NewLiveAreaUpsertLogic(liveAdminSessionCtx(), liveAdminSvc(fake)).LiveAreaUpsert(
		&types.ParamLiveAreaUpsert{AreaId: -1, AreaName: "网游", State: 1,
			OperatorMid: 46, RequestId: "k1"}); err == nil {
		t.Fatalf("负数 area_id 应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用下游, calls=%d", fake.calls)
	}
	resp, err := NewLiveAreaUpsertLogic(liveAdminSessionCtx(), liveAdminSvc(fake)).LiveAreaUpsert(
		&types.ParamLiveAreaUpsert{AreaName: "网游", ParentAreaId: 0, Sort: 3, State: 1,
			OperatorMid: 46, RequestId: "k1"})
	if err != nil {
		t.Fatalf("新建分区应成功: %v", err)
	}
	in := fake.areaReq
	if in.GetAreaId() != 0 || in.GetAreaName() != "网游" || in.GetParentAreaId() != 0 ||
		in.GetSort() != 3 || in.GetState() != 1 || in.GetOperatorMid() != 46 || in.GetRequestId() != "k1" {
		t.Fatalf("UpsertArea 入参不符: %+v", in)
	}
	if resp.Data.AreaId != 51 || !resp.Data.Created {
		t.Fatalf("created 应由服务原样回传: %+v", resp.Data)
	}
}

func TestLiveLiftBanProjectsMessage(t *testing.T) {
	fake := &liveAdminFake{liftReply: &liveroomrpc.LiftBanReply{
		BanId: 41, State: liveroomrpc.RoomState_ROOM_STATE_READY, Replayed: false, Message: "无生效禁播",
	}}
	resp, err := NewLiveRoomBanLiftLogic(liveAdminSessionCtx(), liveAdminSvc(fake)).
		LiveRoomBanLift(&types.ParamLiveRoomBanLift{RoomId: 1, OperatorMid: 46, RequestId: "k1"})
	if err != nil {
		t.Fatalf("解除禁播应成功: %v", err)
	}
	if fake.liftReq.GetBanId() != 0 {
		t.Fatalf("ban_id 省略应作为「解除当前生效记录」透传: %+v", fake.liftReq)
	}
	if resp.Data.Message != "无生效禁播" || resp.Data.State != 2 {
		t.Fatalf("服务的解释必须回传（幂等与无操作都靠它区分）: %+v", resp.Data)
	}
}

// --- 故障与降级口径 ---

// 12 条路由在未配置 LiveRoomRPC 时都必须返回「依赖未接」，不能伪装成空结果或成功。
func TestLiveRoutesFailClosedWithoutClient(t *testing.T) {
	ctx := liveAdminSessionCtx()
	empty := &svc.ServiceContext{}
	cases := map[string]func() error{
		"liveRoomGet": func() error {
			_, err := NewLiveRoomGetLogic(ctx, empty).LiveRoomGet(&types.ParamLiveRoomGet{RoomId: 1})
			return err
		},
		"liveRoomList": func() error {
			_, err := NewLiveRoomListLogic(ctx, empty).LiveRoomList(&types.ParamLiveRoomList{})
			return err
		},
		"liveRoomBans": func() error {
			_, err := NewLiveRoomBansLogic(ctx, empty).LiveRoomBans(&types.ParamLiveRoomBans{OperatorMid: 1})
			return err
		},
		"liveSessionGet": func() error {
			_, err := NewLiveSessionGetLogic(ctx, empty).LiveSessionGet(&types.ParamLiveSessionGet{SessionId: 1})
			return err
		},
		"liveSessionList": func() error {
			_, err := NewLiveSessionListLogic(ctx, empty).LiveSessionList(&types.ParamLiveSessionList{RoomId: 1})
			return err
		},
		"liveAreaList": func() error {
			_, err := NewLiveAreaListLogic(ctx, empty).LiveAreaList(&types.ParamLiveAreaList{})
			return err
		},
		"liveAnchorList": func() error {
			_, err := NewLiveAnchorListLogic(ctx, empty).LiveAnchorList(&types.ParamLiveAnchorList{RoomId: 1})
			return err
		},
		"liveRoomClose": func() error {
			_, err := NewLiveRoomCloseLogic(ctx, empty).LiveRoomClose(&types.ParamLiveRoomClose{
				RoomId: 1, OperatorMid: 1, RequestId: "k"})
			return err
		},
		"liveRoomBan": func() error {
			_, err := NewLiveRoomBanLogic(ctx, empty).LiveRoomBan(&types.ParamLiveRoomBan{
				RoomId: 1, OperatorMid: 1, RequestId: "k"})
			return err
		},
		"liveRoomBanLift": func() error {
			_, err := NewLiveRoomBanLiftLogic(ctx, empty).LiveRoomBanLift(&types.ParamLiveRoomBanLift{
				RoomId: 1, OperatorMid: 1, RequestId: "k"})
			return err
		},
		"liveRoomSettingUpdate": func() error {
			_, err := NewLiveRoomSettingUpdateLogic(ctx, empty).LiveRoomSettingUpdate(
				&types.ParamLiveRoomSettingUpdate{RoomId: 1, OperatorMid: 1, RequestId: "k"})
			return err
		},
		"liveAreaUpsert": func() error {
			_, err := NewLiveAreaUpsertLogic(ctx, empty).LiveAreaUpsert(&types.ParamLiveAreaUpsert{
				AreaName: "网游", State: 1, OperatorMid: 1, RequestId: "k"})
			return err
		},
	}
	if len(cases) != 12 {
		t.Fatalf("live 域 logic 应有 12 条，实际 %d", len(cases))
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errLiveServiceNotConfigured) {
				t.Fatalf("未配置 LiveRoomRPC 时应返回依赖不可用，实际 %v", err)
			}
		})
	}
}

func TestLiveRoutesRejectMissingRequestBody(t *testing.T) {
	ctx := liveAdminSessionCtx()
	svcCtx := liveAdminSvc(&liveAdminFake{})
	cases := map[string]func() error{
		"liveRoomGet":     func() error { _, e := NewLiveRoomGetLogic(ctx, svcCtx).LiveRoomGet(nil); return e },
		"liveRoomList":    func() error { _, e := NewLiveRoomListLogic(ctx, svcCtx).LiveRoomList(nil); return e },
		"liveRoomBans":    func() error { _, e := NewLiveRoomBansLogic(ctx, svcCtx).LiveRoomBans(nil); return e },
		"liveSessionGet":  func() error { _, e := NewLiveSessionGetLogic(ctx, svcCtx).LiveSessionGet(nil); return e },
		"liveSessionList": func() error { _, e := NewLiveSessionListLogic(ctx, svcCtx).LiveSessionList(nil); return e },
		"liveAreaList":    func() error { _, e := NewLiveAreaListLogic(ctx, svcCtx).LiveAreaList(nil); return e },
		"liveAnchorList":  func() error { _, e := NewLiveAnchorListLogic(ctx, svcCtx).LiveAnchorList(nil); return e },
		"liveRoomClose":   func() error { _, e := NewLiveRoomCloseLogic(ctx, svcCtx).LiveRoomClose(nil); return e },
		"liveRoomBan":     func() error { _, e := NewLiveRoomBanLogic(ctx, svcCtx).LiveRoomBan(nil); return e },
		"liveRoomBanLift": func() error { _, e := NewLiveRoomBanLiftLogic(ctx, svcCtx).LiveRoomBanLift(nil); return e },
		"liveRoomSettingUpdate": func() error {
			_, e := NewLiveRoomSettingUpdateLogic(ctx, svcCtx).LiveRoomSettingUpdate(nil)
			return e
		},
		"liveAreaUpsert": func() error { _, e := NewLiveAreaUpsertLogic(ctx, svcCtx).LiveAreaUpsert(nil); return e },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errLiveRequestMissing) {
				t.Fatalf("请求体缺失时应返回入参错误，实际 %v", err)
			}
		})
	}
}

// 下游错误（含「operator_mid 不是生效房主」「状态机不允许迁移」这类领域结论）一律原样上抛，
// 不吞、不改写成 code=0 的空结果。
func TestLiveDownstreamErrorPropagates(t *testing.T) {
	fake := &liveAdminFake{err: errLiveFakeDownstream}
	svcCtx := liveAdminSvc(fake)
	ctx := liveAdminSessionCtx()

	if _, err := NewLiveRoomGetLogic(ctx, svcCtx).LiveRoomGet(&types.ParamLiveRoomGet{RoomId: 1}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("GetRoom 错误应上抛: %v", err)
	}
	if _, err := NewLiveRoomListLogic(ctx, svcCtx).LiveRoomList(&types.ParamLiveRoomList{}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("ListRooms 错误应上抛: %v", err)
	}
	if _, err := NewLiveRoomBansLogic(ctx, svcCtx).LiveRoomBans(&types.ParamLiveRoomBans{OperatorMid: 46}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("ListRoomBans 错误应上抛: %v", err)
	}
	if _, err := NewLiveSessionGetLogic(ctx, svcCtx).LiveSessionGet(&types.ParamLiveSessionGet{SessionId: 21}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("GetSession 错误应上抛: %v", err)
	}
	if _, err := NewLiveSessionListLogic(ctx, svcCtx).LiveSessionList(&types.ParamLiveSessionList{RoomId: 1}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("ListSessions 错误应上抛: %v", err)
	}
	if _, err := NewLiveAreaListLogic(ctx, svcCtx).LiveAreaList(&types.ParamLiveAreaList{}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("ListAreas 错误应上抛: %v", err)
	}
	if _, err := NewLiveAnchorListLogic(ctx, svcCtx).LiveAnchorList(&types.ParamLiveAnchorList{RoomId: 62}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("ListAnchors 错误应上抛: %v", err)
	}
	// 下游把 CloseRoom 拒成「operator_mid 不是生效房主」等结论时同样上抛（网关不重试也不兜底）。
	if _, err := NewLiveRoomCloseLogic(ctx, svcCtx).LiveRoomClose(&types.ParamLiveRoomClose{
		RoomId: 1, OperatorMid: 46, RequestId: "k"}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("CloseRoom 错误应上抛: %v", err)
	}
	if _, err := NewLiveRoomBanLogic(ctx, svcCtx).LiveRoomBan(&types.ParamLiveRoomBan{
		RoomId: 1, BanType: 1, DurationSeconds: 60, OperatorMid: 46, RequestId: "k"}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("BanRoom 错误应上抛: %v", err)
	}
	if _, err := NewLiveRoomBanLiftLogic(ctx, svcCtx).LiveRoomBanLift(&types.ParamLiveRoomBanLift{
		RoomId: 1, OperatorMid: 46, RequestId: "k"}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("LiftBan 错误应上抛: %v", err)
	}
	if _, err := NewLiveRoomSettingUpdateLogic(ctx, svcCtx).LiveRoomSettingUpdate(
		&types.ParamLiveRoomSettingUpdate{RoomId: 1, OperatorMid: 46, RequestId: "k"}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("UpdateRoomSetting 错误应上抛（含 ErrAnchorNotOwner）: %v", err)
	}
	if _, err := NewLiveAreaUpsertLogic(ctx, svcCtx).LiveAreaUpsert(&types.ParamLiveAreaUpsert{
		AreaName: "网游", State: 1, OperatorMid: 46, RequestId: "k"}); !errors.Is(err, errLiveFakeDownstream) {
		t.Fatalf("UpsertArea 错误应上抛: %v", err)
	}
}

// 只读面不需要会话身份（免 AdminPermission 路由组），但主体缺失仍然要拒。
func TestLiveReadRoutesWorkWithoutSessionIdentity(t *testing.T) {
	fake := &liveAdminFake{
		getRoomReply: &liveroomrpc.GetRoomReply{Room: liveFixtureRoom()},
		getSess:      &liveroomrpc.GetSessionReply{Session: liveFixtureSession()},
	}
	ctx := context.Background()
	svcCtx := liveAdminSvc(fake)
	if _, err := NewLiveRoomGetLogic(ctx, svcCtx).LiveRoomGet(&types.ParamLiveRoomGet{OwnerMid: 2}); err != nil {
		t.Fatalf("按房主读房间不应要求后台会话: %v", err)
	}
	if fake.getRoomReq.GetOwnerMid() != 2 {
		t.Fatalf("owner_mid 应透传: %+v", fake.getRoomReq)
	}
	if _, err := NewLiveSessionGetLogic(ctx, svcCtx).LiveSessionGet(&types.ParamLiveSessionGet{SessionId: 21}); err != nil {
		t.Fatalf("读场次不应要求后台会话: %v", err)
	}
}
