package logic

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 live-room 的口径：入参映射（含枚举转换）、DTO→响应投影与裁剪、
// 幂等键必填、终端入口的动作白名单，以及错误原样上抛。
// 开播资格（creator）、风控（risk-control）、资料审核（moderation）与房间状态机
// 都在 live-room 服务内判定，网关不代判也不改写结论（AGENTS.md §5/§8）。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库。

// reflectFieldExists 用于断言“内部字段没有泄漏到终端响应结构里”：
// types 结构由 goctl 按 .api 生成，这里以反射方式检查字段名，避免编译期硬引用。
func reflectFieldExists(v any, name string) (reflect.StructField, bool) {
	t := reflect.TypeOf(v)
	if t == nil {
		return reflect.StructField{}, false
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return reflect.StructField{}, false
	}
	return t.FieldByName(name)
}

type fakeLiveRoomClient struct {
	liveroomrpc.LiveRoomClient

	err   error
	calls int

	roomReq          *liveroomrpc.GetRoomReq
	roomRpl          *liveroomrpc.GetRoomReply
	roomsReq         *liveroomrpc.ListRoomsReq
	roomsRpl         *liveroomrpc.ListRoomsReply
	areasReq         *liveroomrpc.ListAreasReq
	areasRpl         *liveroomrpc.ListAreasReply
	sessionReq       *liveroomrpc.GetSessionReq
	sessionRpl       *liveroomrpc.GetSessionReply
	sessionsReq      *liveroomrpc.ListSessionsReq
	sessionsRpl      *liveroomrpc.ListSessionsReply
	anchorsReq       *liveroomrpc.ListAnchorsReq
	anchorsRpl       *liveroomrpc.ListAnchorsReply
	createReq        *liveroomrpc.CreateRoomReq
	createRpl        *liveroomrpc.CreateRoomReply
	updateInfoReq    *liveroomrpc.UpdateRoomInfoReq
	updateInfoRpl    *liveroomrpc.UpdateRoomInfoReply
	updateSettingReq *liveroomrpc.UpdateRoomSettingReq
	updateSettingRpl *liveroomrpc.UpdateRoomSettingReply
	prepareReq       *liveroomrpc.PrepareLiveReq
	prepareRpl       *liveroomrpc.PrepareLiveReply
	startReq         *liveroomrpc.StartLiveReq
	startRpl         *liveroomrpc.StartLiveReply
	endReq           *liveroomrpc.EndLiveReq
	endRpl           *liveroomrpc.EndLiveReply
	closeReq         *liveroomrpc.CloseRoomReq
	closeRpl         *liveroomrpc.CloseRoomReply
	anchorReq        *liveroomrpc.MutateAnchorReq
	anchorRpl        *liveroomrpc.MutateAnchorReply
}

func (f *fakeLiveRoomClient) record() { f.calls++ }

func (f *fakeLiveRoomClient) GetRoom(_ context.Context, in *liveroomrpc.GetRoomReq,
	_ ...grpc.CallOption) (*liveroomrpc.GetRoomReply, error) {
	f.record()
	f.roomReq = in
	return f.roomRpl, f.err
}

func (f *fakeLiveRoomClient) ListRooms(_ context.Context, in *liveroomrpc.ListRoomsReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListRoomsReply, error) {
	f.record()
	f.roomsReq = in
	return f.roomsRpl, f.err
}

func (f *fakeLiveRoomClient) ListAreas(_ context.Context, in *liveroomrpc.ListAreasReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListAreasReply, error) {
	f.record()
	f.areasReq = in
	return f.areasRpl, f.err
}

func (f *fakeLiveRoomClient) GetSession(_ context.Context, in *liveroomrpc.GetSessionReq,
	_ ...grpc.CallOption) (*liveroomrpc.GetSessionReply, error) {
	f.record()
	f.sessionReq = in
	return f.sessionRpl, f.err
}

func (f *fakeLiveRoomClient) ListSessions(_ context.Context, in *liveroomrpc.ListSessionsReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListSessionsReply, error) {
	f.record()
	f.sessionsReq = in
	return f.sessionsRpl, f.err
}

func (f *fakeLiveRoomClient) ListAnchors(_ context.Context, in *liveroomrpc.ListAnchorsReq,
	_ ...grpc.CallOption) (*liveroomrpc.ListAnchorsReply, error) {
	f.record()
	f.anchorsReq = in
	return f.anchorsRpl, f.err
}

func (f *fakeLiveRoomClient) CreateRoom(_ context.Context, in *liveroomrpc.CreateRoomReq,
	_ ...grpc.CallOption) (*liveroomrpc.CreateRoomReply, error) {
	f.record()
	f.createReq = in
	return f.createRpl, f.err
}

func (f *fakeLiveRoomClient) UpdateRoomInfo(_ context.Context, in *liveroomrpc.UpdateRoomInfoReq,
	_ ...grpc.CallOption) (*liveroomrpc.UpdateRoomInfoReply, error) {
	f.record()
	f.updateInfoReq = in
	return f.updateInfoRpl, f.err
}

func (f *fakeLiveRoomClient) UpdateRoomSetting(_ context.Context, in *liveroomrpc.UpdateRoomSettingReq,
	_ ...grpc.CallOption) (*liveroomrpc.UpdateRoomSettingReply, error) {
	f.record()
	f.updateSettingReq = in
	return f.updateSettingRpl, f.err
}

func (f *fakeLiveRoomClient) PrepareLive(_ context.Context, in *liveroomrpc.PrepareLiveReq,
	_ ...grpc.CallOption) (*liveroomrpc.PrepareLiveReply, error) {
	f.record()
	f.prepareReq = in
	return f.prepareRpl, f.err
}

func (f *fakeLiveRoomClient) StartLive(_ context.Context, in *liveroomrpc.StartLiveReq,
	_ ...grpc.CallOption) (*liveroomrpc.StartLiveReply, error) {
	f.record()
	f.startReq = in
	return f.startRpl, f.err
}

func (f *fakeLiveRoomClient) EndLive(_ context.Context, in *liveroomrpc.EndLiveReq,
	_ ...grpc.CallOption) (*liveroomrpc.EndLiveReply, error) {
	f.record()
	f.endReq = in
	return f.endRpl, f.err
}

func (f *fakeLiveRoomClient) CloseRoom(_ context.Context, in *liveroomrpc.CloseRoomReq,
	_ ...grpc.CallOption) (*liveroomrpc.CloseRoomReply, error) {
	f.record()
	f.closeReq = in
	return f.closeRpl, f.err
}

func (f *fakeLiveRoomClient) MutateAnchor(_ context.Context, in *liveroomrpc.MutateAnchorReq,
	_ ...grpc.CallOption) (*liveroomrpc.MutateAnchorReply, error) {
	f.record()
	f.anchorReq = in
	return f.anchorRpl, f.err
}

func fullRoomReply() *liveroomrpc.GetRoomReply {
	return &liveroomrpc.GetRoomReply{
		Room: &liveroomrpc.RoomInfo{
			RoomId: 5001, OwnerMid: 111, Title: "深夜电台", Cover: "object/cover.jpg",
			AreaId: 13, State: liveroomrpc.RoomState_ROOM_STATE_LIVING,
			VerifyState:     liveroomrpc.VerifyState_VERIFY_STATE_PASSED,
			ActiveSessionId: 7001, ActiveStreamId: "stream-ref-1", StateVersion: 9,
			BanUntil: 0, Ctime: 1600, Mtime: 1700,
		},
		Setting: &liveroomrpc.RoomSetting{
			RoomId: 5001, DanmakuEnabled: true, ReplyEnabled: false, RecordEnabled: true,
			LinkmicEnabled: true, LiveType: 2, MinClientVersionCode: 10800, Mtime: 1700,
		},
		ActiveSession: &liveroomrpc.SessionInfo{
			SessionId: 7001, RoomId: 5001, Mid: 111,
			State:         liveroomrpc.SessionState_SESSION_STATE_LIVING,
			TitleSnapshot: "深夜电台", AreaIdSnapshot: 13, StreamId: "stream-ref-1",
			StartedAt: 1700, DurationSeconds: 60,
			EndReason:     liveroomrpc.EndReason_END_REASON_UNSPECIFIED,
			LastStreamSeq: 42,
			ReplayState:   liveroomrpc.ReplayState_REPLAY_STATE_PROCESSING,
			RecordId:      3001, RecordAssetId: 4001, RecordAid: 5001,
			ModerationTaskId: 6001, Ctime: 1700, Mtime: 1700,
		},
	}
}

func TestLiveGetRoomByRoomIdProjectsEnumsAndPresenceFlags(t *testing.T) {
	fake := &fakeLiveRoomClient{roomRpl: fullRoomReply()}
	resp, err := NewLiveRoomInfoLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		LiveRoomInfo(&types.ParamLiveRoom{RoomId: 5001, WithSetting: true, WithActiveSession: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.roomReq.GetRoomId() != 5001 || fake.roomReq.GetOwnerMid() != 0 ||
		!fake.roomReq.GetWithSetting() || !fake.roomReq.GetWithActiveSession() {
		t.Fatalf("GetRoom 入参未透传: %+v", fake.roomReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	room := resp.Data.Room
	// 枚举降为 int32，客户端拿不到 protobuf 类型。
	if room.RoomId != 5001 || room.OwnerMid != 111 || room.State != int32(liveroomrpc.RoomState_ROOM_STATE_LIVING) ||
		room.VerifyState != int32(liveroomrpc.VerifyState_VERIFY_STATE_PASSED) ||
		room.ActiveSessionId != 7001 || room.ActiveStreamId != "stream-ref-1" || room.StateVersion != 9 ||
		room.AreaId != 13 || room.Cover != "object/cover.jpg" || room.Mtime != 1700 {
		t.Fatalf("房间投影不完整: %+v", room)
	}
	if !resp.Data.HasSetting || !resp.Data.HasActiveSession {
		t.Fatalf("可选段落的存在性必须回传: %+v", resp.Data)
	}
	if resp.Data.Setting.LiveType != 2 || !resp.Data.Setting.DanmakuEnabled || resp.Data.Setting.ReplyEnabled ||
		!resp.Data.Setting.RecordEnabled || !resp.Data.Setting.LinkmicEnabled ||
		resp.Data.Setting.MinClientVersionCode != 10800 {
		t.Fatalf("配置投影不完整: %+v", resp.Data.Setting)
	}
	s := resp.Data.ActiveSession
	if s.SessionId != 7001 || s.State != int32(liveroomrpc.SessionState_SESSION_STATE_LIVING) ||
		s.ReplayState != int32(liveroomrpc.ReplayState_REPLAY_STATE_PROCESSING) ||
		s.RecordAssetId != 4001 || s.RecordAid != 5001 || s.TitleSnapshot != "深夜电台" {
		t.Fatalf("场次投影不完整: %+v", s)
	}
}

func TestLiveGetRoomTrimsInternalSessionFields(t *testing.T) {
	fake := &fakeLiveRoomClient{roomRpl: fullRoomReply()}
	resp, err := NewLiveRoomInfoLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		LiveRoomInfo(&types.ParamLiveRoom{RoomId: 5001, WithActiveSession: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// last_stream_seq（流事件乱序守卫）、record_id（live-media 内部记录）、
	// moderation_task_id（送审关联）属服务内部字段，不下发终端：投影结构里必须根本没有它们。
	for _, name := range []string{"LastStreamSeq", "RecordId", "ModerationTaskId"} {
		if _, ok := reflectFieldExists(resp.Data.ActiveSession, name); ok {
			t.Fatalf("内部字段 %s 不应出现在终端投影里", name)
		}
	}
}

func TestLiveGetRoomWithoutOptionalSectionsMarksAbsent(t *testing.T) {
	fake := &fakeLiveRoomClient{roomRpl: &liveroomrpc.GetRoomReply{
		Room: &liveroomrpc.RoomInfo{RoomId: 5001},
	}}
	resp, err := NewLiveRoomInfoLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		LiveRoomInfo(&types.ParamLiveRoom{RoomId: 5001})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 未请求（或服务无配置行）时必须靠 has_* 区分，不能让端上把零值误读成「全部功能关闭」。
	if resp.Data.HasSetting || resp.Data.HasActiveSession {
		t.Fatalf("has_* = %v/%v, want false/false", resp.Data.HasSetting, resp.Data.HasActiveSession)
	}
	if resp.Data.Setting.RoomId != 0 || resp.Data.ActiveSession.SessionId != 0 {
		t.Fatalf("缺失段落应投影为零值占位: %+v", resp.Data)
	}
}

func TestLiveGetRoomRejectsEmptyRoomInsteadOfZeroSuccess(t *testing.T) {
	fake := &fakeLiveRoomClient{roomRpl: &liveroomrpc.GetRoomReply{}}
	if _, err := NewLiveRoomInfoLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		LiveRoomInfo(&types.ParamLiveRoom{RoomId: 5001}); err == nil {
		t.Fatal("成功但无 room 属于契约异常，不能投影成 room_id=0 的假房间")
	}
}

func TestLiveGetRoomByUpUsesOwnerMid(t *testing.T) {
	fake := &fakeLiveRoomClient{roomRpl: fullRoomReply()}
	if _, err := NewLiveRoomByUpLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		LiveRoomByUp(&types.ParamLiveRoomByUp{OwnerMid: 111, WithActiveSession: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.roomReq.GetOwnerMid() != 111 || fake.roomReq.GetRoomId() != 0 ||
		!fake.roomReq.GetWithActiveSession() {
		t.Fatalf("按 UP 主查询必须走 owner_mid: %+v", fake.roomReq)
	}
	// 主播无生效房间时服务返回 NotFound，网关不得降级成空成功。
	notFound := &fakeLiveRoomClient{err: errors.New("liveroom: room not found")}
	if _, err := NewLiveRoomByUpLogic(context.Background(), &svc.ServiceContext{LiveRoom: notFound}).
		LiveRoomByUp(&types.ParamLiveRoomByUp{OwnerMid: 111}); err == nil {
		t.Fatal("NotFound 必须上抛")
	}
}

func TestLiveListRoomsMapsEnumsAndPageEcho(t *testing.T) {
	fake := &fakeLiveRoomClient{roomsRpl: &liveroomrpc.ListRoomsReply{
		Rooms: []*liveroomrpc.RoomInfo{{RoomId: 5001, State: liveroomrpc.RoomState_ROOM_STATE_READY}},
		Total: 1, Page: 2, PageSize: 20,
	}}
	resp, err := NewListLiveRoomsLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		ListLiveRooms(&types.ParamLiveRooms{OwnerMid: 111, AreaId: 13, State: 2, Order: 1, Page: 2, PageSize: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.roomsReq
	if in.GetOwnerMid() != 111 || in.GetAreaId() != 13 ||
		in.GetState() != liveroomrpc.RoomState_ROOM_STATE_READY ||
		in.GetOrder() != liveroomrpc.RoomOrder_ROOM_ORDER_LIVING_FIRST || in.GetPage() != 2 || in.GetPageSize() != 20 {
		t.Fatalf("ListRooms 入参/枚举未透传: %+v", in)
	}
	if resp.Data.Total != 1 || resp.Data.Page != 2 || resp.Data.PageSize != 20 {
		t.Fatalf("分页回显不完整: %+v", resp.Data)
	}
	if len(resp.Data.Rooms) != 1 || resp.Data.Rooms[0].State != int32(liveroomrpc.RoomState_ROOM_STATE_READY) {
		t.Fatalf("房间列表投影不完整: %+v", resp.Data.Rooms)
	}
	if resp.Data.Rooms == nil {
		t.Fatal("列表必须是非 nil 切片")
	}
}

func TestLiveListRoomsEmptyIsNotNil(t *testing.T) {
	fake := &fakeLiveRoomClient{roomsRpl: &liveroomrpc.ListRoomsReply{}}
	resp, err := NewListLiveRoomsLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		ListLiveRooms(&types.ParamLiveRooms{Page: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.Rooms == nil {
		t.Fatal("该分区暂无直播间是合法结果，必须投影成 []")
	}
	// page_size 省略时保持 0，由服务取默认并截断到上限。
	if fake.roomsReq.GetPageSize() != 0 {
		t.Fatalf("page_size = %d, want 0", fake.roomsReq.GetPageSize())
	}
}

func TestLiveListAreasPassesMinusOneSentinelsAndTrimsOpsFields(t *testing.T) {
	fake := &fakeLiveRoomClient{areasRpl: &liveroomrpc.ListAreasReply{
		Areas: []*liveroomrpc.AreaInfo{{
			AreaId: 13, AreaName: "娱乐", ParentAreaId: 1, Sort: 5, State: 1,
			OperatorMid: 999, Ctime: 1600, Mtime: 1700,
		}},
		Total: 1,
	}}
	resp, err := NewListLiveAreasLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		ListLiveAreas(&types.ParamLiveAreas{ParentAreaId: -1, State: -1, Page: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.areasReq.GetParentAreaId() != -1 || fake.areasReq.GetState() != -1 || fake.areasReq.GetPage() != 1 {
		t.Fatalf("ListAreas 哨兵值未透传: %+v", fake.areasReq)
	}
	if len(resp.Data.Areas) != 1 || resp.Data.Areas[0].AreaName != "娱乐" || resp.Data.Areas[0].Sort != 5 {
		t.Fatalf("分区投影不完整: %+v", resp.Data.Areas)
	}
	for _, name := range []string{"OperatorMid", "Ctime", "Mtime"} {
		if _, ok := reflectFieldExists(resp.Data.Areas[0], name); ok {
			t.Fatalf("运营审计字段 %s 不应下发终端", name)
		}
	}
}

func TestLiveListAreasOnlyTopLevelUsesZero(t *testing.T) {
	fake := &fakeLiveRoomClient{areasRpl: &liveroomrpc.ListAreasReply{}}
	if _, err := NewListLiveAreasLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		ListLiveAreas(&types.ParamLiveAreas{ParentAreaId: 0, State: 1, Page: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 0 与「不过滤」不同：parent_area_id=0 表示只取一级分区，不能被网关改写成 -1。
	if fake.areasReq.GetParentAreaId() != 0 {
		t.Fatalf("parent_area_id = %d, want 0", fake.areasReq.GetParentAreaId())
	}
	if fake.areasReq.GetState() != 1 {
		t.Fatalf("state = %d, want 1", fake.areasReq.GetState())
	}
}

func TestLiveGetSessionMapsOffsetAndRequiresKey(t *testing.T) {
	fake := &fakeLiveRoomClient{sessionRpl: &liveroomrpc.GetSessionReply{
		Session: &liveroomrpc.SessionInfo{
			SessionId: 7001, RoomId: 5001, State: liveroomrpc.SessionState_SESSION_STATE_ENDED,
			EndReason: liveroomrpc.EndReason_END_REASON_ANCHOR_STOP, DurationSeconds: 120,
			ReplayState: liveroomrpc.ReplayState_REPLAY_STATE_AVAILABLE, RecordAid: 5001,
		},
	}}
	resp, err := NewGetLiveSessionLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		GetLiveSession(&types.ParamLiveSession{RoomId: 5001, Offset: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.sessionReq.GetRoomId() != 5001 || fake.sessionReq.GetOffset() != 2 || fake.sessionReq.GetSessionId() != 0 {
		t.Fatalf("GetSession 入参未透传: %+v", fake.sessionReq)
	}
	s := resp.Data.Session
	if s.SessionId != 7001 || s.State != int32(liveroomrpc.SessionState_SESSION_STATE_ENDED) ||
		s.EndReason != int32(liveroomrpc.EndReason_END_REASON_ANCHOR_STOP) ||
		s.ReplayState != int32(liveroomrpc.ReplayState_REPLAY_STATE_AVAILABLE) || s.RecordAid != 5001 {
		t.Fatalf("场次投影不完整: %+v", s)
	}
	// 空条件查询先拒，不打到服务上。
	blocked := &fakeLiveRoomClient{}
	if _, err := NewGetLiveSessionLogic(context.Background(), &svc.ServiceContext{LiveRoom: blocked}).
		GetLiveSession(&types.ParamLiveSession{}); err == nil {
		t.Fatal("session_id 与 room_id 全为 0 必须拒绝")
	}
	if blocked.calls != 0 {
		t.Fatalf("参数校验失败不得调用下游，calls=%d", blocked.calls)
	}
}

func TestLiveListSessionsMapsCursorAndState(t *testing.T) {
	fake := &fakeLiveRoomClient{sessionsRpl: &liveroomrpc.ListSessionsReply{
		Sessions:   []*liveroomrpc.SessionInfo{{SessionId: 7001}, nil},
		NextCursor: "7001",
	}}
	resp, err := NewListLiveSessionsLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		ListLiveSessions(&types.ParamLiveSessions{
			RoomId: 5001, Mid: 111, State: int32(liveroomrpc.SessionState_SESSION_STATE_ENDED),
			Cursor: "prev", PageSize: 50,
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.sessionsReq
	if in.GetRoomId() != 5001 || in.GetMid() != 111 || in.GetCursor() != "prev" || in.GetPageSize() != 50 ||
		in.GetState() != liveroomrpc.SessionState_SESSION_STATE_ENDED {
		t.Fatalf("ListSessions 入参/枚举未透传: %+v", in)
	}
	if resp.Data.NextCursor != "7001" || len(resp.Data.Sessions) != 2 {
		t.Fatalf("游标/列表投影不完整: %+v", resp.Data)
	}
	empty := &fakeLiveRoomClient{sessionsRpl: &liveroomrpc.ListSessionsReply{}}
	emptyResp, err := NewListLiveSessionsLogic(context.Background(), &svc.ServiceContext{LiveRoom: empty}).
		ListLiveSessions(&types.ParamLiveSessions{RoomId: 5001})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if emptyResp.Data.Sessions == nil {
		t.Fatal("空历史必须是 []，不是 null")
	}
}

func TestLiveListAnchorsMapsRoleAndTrimsRecordId(t *testing.T) {
	fake := &fakeLiveRoomClient{anchorsRpl: &liveroomrpc.ListAnchorsReply{
		Anchors: []*liveroomrpc.AnchorInfo{{
			Id: 4242, RoomId: 5001, Mid: 222, Role: liveroomrpc.AnchorRole_ANCHOR_ROLE_MANAGER,
			State: 1, Ctime: 1600, Mtime: 1700,
		}},
		Total: 1,
	}}
	resp, err := NewListLiveAnchorsLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		ListLiveAnchors(&types.ParamLiveAnchors{
			RoomId: 5001, Role: int32(liveroomrpc.AnchorRole_ANCHOR_ROLE_MANAGER),
			OnlyEnabled: true, Page: 1, PageSize: 20,
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.anchorsReq
	if in.GetRoomId() != 5001 || in.GetRole() != liveroomrpc.AnchorRole_ANCHOR_ROLE_MANAGER ||
		!in.GetOnlyEnabled() || in.GetPage() != 1 || in.GetPageSize() != 20 {
		t.Fatalf("ListAnchors 入参/枚举未透传: %+v", in)
	}
	a := resp.Data.Anchors[0]
	if a.RoomId != 5001 || a.Mid != 222 || a.Role != int32(liveroomrpc.AnchorRole_ANCHOR_ROLE_MANAGER) ||
		a.State != 1 || a.Ctime != 1600 {
		t.Fatalf("主播绑定投影不完整: %+v", a)
	}
	if _, ok := reflectFieldExists(a, "Id"); ok {
		t.Fatal("绑定记录主键 id 是服务内部行标识，不下发终端")
	}
}

func TestLiveCreateRoomRequiresRequestIdAndOmitsEmptySetting(t *testing.T) {
	fake := &fakeLiveRoomClient{createRpl: &liveroomrpc.CreateRoomReply{
		RoomId: 5001, State: liveroomrpc.RoomState_ROOM_STATE_PENDING,
		VerifyState: liveroomrpc.VerifyState_VERIFY_STATE_REVIEWING, Replayed: true, ModerationTaskId: 6001,
	}}
	resp, err := NewCreateLiveRoomLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		CreateLiveRoom(&types.ParamLiveRoomCreate{
			Mid: 111, Title: "深夜电台", AreaId: 13, Platform: int32(liveroomrpc.Platform_PLATFORM_HARMONY),
			AppVersion: "1.2.3", RequestId: "req-1",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.createReq
	if in.GetMid() != 111 || in.GetTitle() != "深夜电台" || in.GetAreaId() != 13 ||
		in.GetPlatform() != liveroomrpc.Platform_PLATFORM_HARMONY || in.GetAppVersion() != "1.2.3" ||
		in.GetRequestId() != "req-1" {
		t.Fatalf("CreateRoom 入参未透传: %+v", in)
	}
	// 一个配置字段都没填 → setting 必须为 nil，由服务落默认配置，网关不伪造默认开关。
	if in.GetSetting() != nil {
		t.Fatalf("未填配置时应传 nil，got %+v", in.GetSetting())
	}
	if resp.Data.RoomId != 5001 || resp.Data.State != int32(liveroomrpc.RoomState_ROOM_STATE_PENDING) ||
		resp.Data.VerifyState != int32(liveroomrpc.VerifyState_VERIFY_STATE_REVIEWING) ||
		!resp.Data.Replayed || resp.Data.ModerationTaskId != 6001 {
		t.Fatalf("创建结果投影不完整: %+v", resp.Data)
	}

	blocked := &fakeLiveRoomClient{}
	if _, err := NewCreateLiveRoomLogic(context.Background(), &svc.ServiceContext{LiveRoom: blocked}).
		CreateLiveRoom(&types.ParamLiveRoomCreate{Mid: 111, Title: "t", AreaId: 13, RequestId: " "}); err == nil {
		t.Fatal("request_id 缺失必须拒绝，否则重试会建出第二个房间")
	}
	if blocked.calls != 0 {
		t.Fatalf("参数校验失败不得调用下游，calls=%d", blocked.calls)
	}
}

func TestLiveCreateRoomPassesFilledSettingBlock(t *testing.T) {
	fake := &fakeLiveRoomClient{createRpl: &liveroomrpc.CreateRoomReply{RoomId: 5001}}
	if _, err := NewCreateLiveRoomLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		CreateLiveRoom(&types.ParamLiveRoomCreate{
			Mid: 111, Title: "t", AreaId: 13, RequestId: "req-2",
			LinkmicEnabled: true, LiveType: 3,
		}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := fake.createReq.GetSetting()
	if s == nil {
		t.Fatal("填了任意配置字段就必须整段提交")
	}
	if !s.GetLinkmicEnabled() || s.GetLiveType() != 3 {
		t.Fatalf("配置整段透传错误: %+v", s)
	}
	// 未填的开关保持零值下传，由服务按默认配置补齐，网关不伪造默认开/关。
	if s.GetDanmakuEnabled() || s.GetReplyEnabled() || s.GetRecordEnabled() {
		t.Fatalf("未填开关被网关伪造为开启: %+v", s)
	}
	if s.GetRoomId() != 0 {
		t.Fatal("创建时不得伪造 room_id")
	}
}

func TestLiveUpdateRoomInfoPassesSparseFields(t *testing.T) {
	fake := &fakeLiveRoomClient{updateInfoRpl: &liveroomrpc.UpdateRoomInfoReply{
		Room:     &liveroomrpc.RoomInfo{RoomId: 5001, Title: "新标题", State: liveroomrpc.RoomState_ROOM_STATE_READY},
		Replayed: false, ModerationTaskId: 6002,
	}}
	resp, err := NewUpdateLiveRoomInfoLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		UpdateLiveRoomInfo(&types.ParamLiveRoomInfoUpdate{
			RoomId: 5001, OperatorMid: 111, Title: "新标题", RequestId: "req-3",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.updateInfoReq
	if in.GetRoomId() != 5001 || in.GetOperatorMid() != 111 || in.GetTitle() != "新标题" ||
		in.GetCover() != "" || in.GetAreaId() != 0 || in.GetRequestId() != "req-3" {
		t.Fatalf("UpdateRoomInfo 入参未透传: %+v", in)
	}
	if resp.Data.Room.Title != "新标题" || resp.Data.Room.State != int32(liveroomrpc.RoomState_ROOM_STATE_READY) ||
		resp.Data.Replayed || resp.Data.ModerationTaskId != 6002 {
		t.Fatalf("更新结果投影不完整: %+v", resp.Data)
	}
	blocked := &fakeLiveRoomClient{}
	if _, err := NewUpdateLiveRoomInfoLogic(context.Background(), &svc.ServiceContext{LiveRoom: blocked}).
		UpdateLiveRoomInfo(&types.ParamLiveRoomInfoUpdate{RoomId: 5001, OperatorMid: 111}); err == nil {
		t.Fatal("request_id 缺失必须拒绝，否则会重复触发送审")
	}
}

func TestLiveUpdateRoomSettingIsFullOverwrite(t *testing.T) {
	fake := &fakeLiveRoomClient{updateSettingRpl: &liveroomrpc.UpdateRoomSettingReply{
		Setting:  &liveroomrpc.RoomSetting{RoomId: 5001, DanmakuEnabled: false, LiveType: 1, Mtime: 1800},
		Replayed: true,
	}}
	resp, err := NewUpdateLiveRoomSettingLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		UpdateLiveRoomSetting(&types.ParamLiveRoomSettingUpdate{
			RoomId: 5001, OperatorMid: 111, DanmakuEnabled: false, ReplyEnabled: false,
			RecordEnabled: false, LinkmicEnabled: false, LiveType: 1, MinClientVersionCode: 0,
			RequestId: "req-4",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := fake.updateSettingReq.GetSetting()
	// 整段覆盖：false 就是显式关闭，网关不得做「只提交改动字段」的合并。
	if s.GetDanmakuEnabled() || s.GetReplyEnabled() || s.GetRecordEnabled() || s.GetLinkmicEnabled() {
		t.Fatalf("false 必须按显式关闭下发: %+v", s)
	}
	if s.GetLiveType() != 1 || s.GetRoomId() != 5001 {
		t.Fatalf("配置整段透传错误: %+v", s)
	}
	if fake.updateSettingReq.GetOperatorMid() != 111 || fake.updateSettingReq.GetRequestId() != "req-4" {
		t.Fatalf("UpdateRoomSetting 入参未透传: %+v", fake.updateSettingReq)
	}
	if !resp.Data.Replayed || resp.Data.Setting.Mtime != 1800 || resp.Data.Setting.DanmakuEnabled {
		t.Fatalf("配置更新结果投影不完整: %+v", resp.Data)
	}
}

func TestLivePrepareProjectsCheckItemsIncludingDegraded(t *testing.T) {
	fake := &fakeLiveRoomClient{prepareRpl: &liveroomrpc.PrepareLiveReply{
		RoomId: 5001, State: liveroomrpc.RoomState_ROOM_STATE_READY,
		Checks: []*liveroomrpc.PrepareCheckItem{
			{Code: "anchor_qualification", Passed: true, Detail: "创作者资格通过"},
			{Code: "risk_control", Passed: false, Detail: "风控下游不可用", Degraded: true},
			nil,
		},
		Ready: false, DenyCode: "risk_denied", RetryAfterSeconds: 30, Replayed: false,
	}}
	resp, err := NewPrepareLiveLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		PrepareLive(&types.ParamLivePrepare{
			RoomId: 5001, Mid: 111, Platform: int32(liveroomrpc.Platform_PLATFORM_DESKTOP),
			DeviceHash: "dev-hash", IpHash: "ip-hash", RequestId: "req-5",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.prepareReq
	if in.GetRoomId() != 5001 || in.GetMid() != 111 ||
		in.GetPlatform() != liveroomrpc.Platform_PLATFORM_DESKTOP ||
		in.GetDeviceHash() != "dev-hash" || in.GetIpHash() != "ip-hash" || in.GetRequestId() != "req-5" {
		t.Fatalf("PrepareLive 入参未透传: %+v", in)
	}
	if len(resp.Data.Checks) != 3 {
		t.Fatalf("checks 长度 = %d, want 3", len(resp.Data.Checks))
	}
	// 顺序与逐项结论原样透出；degraded 不得被美化成通过。
	if resp.Data.Checks[0].Code != "anchor_qualification" || !resp.Data.Checks[0].Passed {
		t.Fatalf("第 1 项结论被改写: %+v", resp.Data.Checks[0])
	}
	if resp.Data.Checks[1].Code != "risk_control" || !resp.Data.Checks[1].Degraded || resp.Data.Checks[1].Passed {
		t.Fatalf("degraded 项必须原样下发: %+v", resp.Data.Checks[1])
	}
	if resp.Data.Checks[2].Code != "" {
		t.Fatalf("nil 项应投影为零值: %+v", resp.Data.Checks[2])
	}
	if resp.Data.Ready || resp.Data.DenyCode != "risk_denied" || resp.Data.RetryAfterSeconds != 30 ||
		resp.Data.State != int32(liveroomrpc.RoomState_ROOM_STATE_READY) {
		t.Fatalf("前置检查结论投影不完整: %+v", resp.Data)
	}
	blocked := &fakeLiveRoomClient{}
	if _, err := NewPrepareLiveLogic(context.Background(), &svc.ServiceContext{LiveRoom: blocked}).
		PrepareLive(&types.ParamLivePrepare{RoomId: 5001, Mid: 111}); err == nil {
		t.Fatal("request_id 缺失必须拒绝：前置检查会推进房间状态")
	}
	if blocked.calls != 0 {
		t.Fatalf("参数校验失败不得调用下游，calls=%d", blocked.calls)
	}
}

func TestLiveStartPassesStreamRefOnly(t *testing.T) {
	fake := &fakeLiveRoomClient{startRpl: &liveroomrpc.StartLiveReply{
		SessionId: 7001, State: liveroomrpc.RoomState_ROOM_STATE_LIVING,
		StartedAt: 1700, StateVersion: 10, Replayed: true,
	}}
	resp, err := NewStartLiveLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		StartLive(&types.ParamLiveStart{RoomId: 5001, Mid: 111, StreamId: "stream-ref-1", RequestId: "req-6"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.startReq
	if in.GetRoomId() != 5001 || in.GetMid() != 111 || in.GetStreamId() != "stream-ref-1" || in.GetRequestId() != "req-6" {
		t.Fatalf("StartLive 入参未透传: %+v", in)
	}
	if resp.Data.SessionId != 7001 || resp.Data.State != int32(liveroomrpc.RoomState_ROOM_STATE_LIVING) ||
		resp.Data.StartedAt != 1700 || resp.Data.StateVersion != 10 || !resp.Data.Replayed {
		t.Fatalf("开播结果投影不完整: %+v", resp.Data)
	}
	blocked := &fakeLiveRoomClient{}
	if _, err := NewStartLiveLogic(context.Background(), &svc.ServiceContext{LiveRoom: blocked}).
		StartLive(&types.ParamLiveStart{RoomId: 5001, Mid: 111}); err == nil {
		t.Fatal("request_id 缺失必须拒绝：开播重试要回放到同一 session_id")
	}
}

func TestLiveEndForcesAnchorStopReason(t *testing.T) {
	fake := &fakeLiveRoomClient{endRpl: &liveroomrpc.EndLiveReply{
		SessionId: 7001, SessionState: liveroomrpc.SessionState_SESSION_STATE_ENDED,
		RoomState: liveroomrpc.RoomState_ROOM_STATE_READY, DurationSeconds: 3600, Replayed: false,
	}}
	resp, err := NewEndLiveLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		EndLive(&types.ParamLiveEnd{RoomId: 5001, Mid: 111, RequestId: "req-7"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 省略 end_reason 时网关补 1（主播主动下播），而不是传 0 让服务落到非法值。
	if fake.endReq.GetEndReason() != liveroomrpc.EndReason_END_REASON_ANCHOR_STOP {
		t.Fatalf("end_reason = %v, want ANCHOR_STOP", fake.endReq.GetEndReason())
	}
	if fake.endReq.GetSessionId() != 0 || fake.endReq.GetRoomId() != 5001 ||
		fake.endReq.GetRequestId() != "req-7" {
		t.Fatalf("EndLive 入参未透传: %+v", fake.endReq)
	}
	if resp.Data.SessionState != int32(liveroomrpc.SessionState_SESSION_STATE_ENDED) ||
		resp.Data.RoomState != int32(liveroomrpc.RoomState_ROOM_STATE_READY) ||
		resp.Data.DurationSeconds != 3600 || resp.Data.SessionId != 7001 {
		t.Fatalf("下播结果投影不完整: %+v", resp.Data)
	}
	// 禁播/关房/断流等原因不能让终端代填（否则可伪造一场直播的终止原因）。
	for _, r := range []int32{2, 3, 4, 5, 9} {
		blocked := &fakeLiveRoomClient{}
		if _, err := NewEndLiveLogic(context.Background(), &svc.ServiceContext{LiveRoom: blocked}).
			EndLive(&types.ParamLiveEnd{RoomId: 5001, Mid: 111, EndReason: r, RequestId: "req-7"}); err == nil {
			t.Fatalf("end_reason=%d 必须被拒绝", r)
		}
		if blocked.calls != 0 {
			t.Fatalf("end_reason=%d 被拒时不得调用下游", r)
		}
	}
}

func TestLiveCloseNeverMarksAdmin(t *testing.T) {
	fake := &fakeLiveRoomClient{closeRpl: &liveroomrpc.CloseRoomReply{
		State: liveroomrpc.RoomState_ROOM_STATE_FINISHED, TerminatedSessionId: 7001, Replayed: true,
	}}
	resp, err := NewCloseLiveRoomLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		CloseLiveRoom(&types.ParamLiveClose{RoomId: 5001, OperatorMid: 111, Reason: "不播了", RequestId: "req-8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.closeReq.GetAdmin() {
		t.Fatal("终端入口必须固定 admin=false，运营关闭走 gateway/admin")
	}
	if fake.closeReq.GetRoomId() != 5001 || fake.closeReq.GetOperatorMid() != 111 ||
		fake.closeReq.GetReason() != "不播了" || fake.closeReq.GetRequestId() != "req-8" {
		t.Fatalf("CloseRoom 入参未透传: %+v", fake.closeReq)
	}
	if resp.Data.State != int32(liveroomrpc.RoomState_ROOM_STATE_FINISHED) ||
		resp.Data.TerminatedSessionId != 7001 || !resp.Data.Replayed {
		t.Fatalf("关闭结果投影不完整: %+v", resp.Data)
	}
}

func TestLiveMutateAnchorLimitsAction(t *testing.T) {
	fake := &fakeLiveRoomClient{anchorRpl: &liveroomrpc.MutateAnchorReply{
		RoomId: 5001, TargetMid: 222, State: 1, BoundCount: 3, Replayed: false,
	}}
	resp, err := NewMutateLiveAnchorLogic(context.Background(), &svc.ServiceContext{LiveRoom: fake}).
		MutateLiveAnchor(&types.ParamLiveAnchorMutate{
			RoomId: 5001, OperatorMid: 111, TargetMid: 222,
			Action: int32(liveroomrpc.AnchorAction_ANCHOR_ACTION_BIND),
			Role:   int32(liveroomrpc.AnchorRole_ANCHOR_ROLE_MANAGER), RequestId: "req-9",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.anchorReq
	if in.GetRoomId() != 5001 || in.GetOperatorMid() != 111 || in.GetTargetMid() != 222 ||
		in.GetAction() != liveroomrpc.AnchorAction_ANCHOR_ACTION_BIND ||
		in.GetRole() != liveroomrpc.AnchorRole_ANCHOR_ROLE_MANAGER || in.GetRequestId() != "req-9" {
		t.Fatalf("MutateAnchor 入参/枚举未透传: %+v", in)
	}
	if resp.Data.State != 1 || resp.Data.BoundCount != 3 || resp.Data.TargetMid != 222 || resp.Data.Replayed {
		t.Fatalf("绑定结果投影不完整: %+v", resp.Data)
	}
	for _, a := range []int32{0, 3, 9} {
		blocked := &fakeLiveRoomClient{}
		if _, err := NewMutateLiveAnchorLogic(context.Background(), &svc.ServiceContext{LiveRoom: blocked}).
			MutateLiveAnchor(&types.ParamLiveAnchorMutate{
				RoomId: 5001, OperatorMid: 111, TargetMid: 222, Action: a, RequestId: "req-9",
			}); err == nil {
			t.Fatalf("action=%d 必须被拒绝", a)
		}
		if blocked.calls != 0 {
			t.Fatalf("action=%d 被拒时不得调用下游", a)
		}
	}
}

func TestLiveLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	calls := []struct {
		name string
		run  func() error
	}{
		{"liveRoomInfo", func() error {
			_, err := NewLiveRoomInfoLogic(context.Background(), empty).LiveRoomInfo(&types.ParamLiveRoom{RoomId: 1})
			return err
		}},
		{"liveRoomByUp", func() error {
			_, err := NewLiveRoomByUpLogic(context.Background(), empty).LiveRoomByUp(&types.ParamLiveRoomByUp{OwnerMid: 1})
			return err
		}},
		{"listLiveRooms", func() error {
			_, err := NewListLiveRoomsLogic(context.Background(), empty).ListLiveRooms(&types.ParamLiveRooms{Page: 1})
			return err
		}},
		{"listLiveAreas", func() error {
			_, err := NewListLiveAreasLogic(context.Background(), empty).ListLiveAreas(&types.ParamLiveAreas{})
			return err
		}},
		{"getLiveSession", func() error {
			_, err := NewGetLiveSessionLogic(context.Background(), empty).GetLiveSession(&types.ParamLiveSession{RoomId: 1})
			return err
		}},
		{"listLiveSessions", func() error {
			_, err := NewListLiveSessionsLogic(context.Background(), empty).ListLiveSessions(&types.ParamLiveSessions{RoomId: 1})
			return err
		}},
		{"listLiveAnchors", func() error {
			_, err := NewListLiveAnchorsLogic(context.Background(), empty).ListLiveAnchors(&types.ParamLiveAnchors{RoomId: 1})
			return err
		}},
		{"createLiveRoom", func() error {
			_, err := NewCreateLiveRoomLogic(context.Background(), empty).
				CreateLiveRoom(&types.ParamLiveRoomCreate{Mid: 1, Title: "t", AreaId: 1, RequestId: "r"})
			return err
		}},
		{"updateLiveRoomInfo", func() error {
			_, err := NewUpdateLiveRoomInfoLogic(context.Background(), empty).
				UpdateLiveRoomInfo(&types.ParamLiveRoomInfoUpdate{RoomId: 1, OperatorMid: 1, RequestId: "r"})
			return err
		}},
		{"updateLiveRoomSetting", func() error {
			_, err := NewUpdateLiveRoomSettingLogic(context.Background(), empty).
				UpdateLiveRoomSetting(&types.ParamLiveRoomSettingUpdate{RoomId: 1, OperatorMid: 1, RequestId: "r"})
			return err
		}},
		{"prepareLive", func() error {
			_, err := NewPrepareLiveLogic(context.Background(), empty).
				PrepareLive(&types.ParamLivePrepare{RoomId: 1, Mid: 1, RequestId: "r"})
			return err
		}},
		{"startLive", func() error {
			_, err := NewStartLiveLogic(context.Background(), empty).
				StartLive(&types.ParamLiveStart{RoomId: 1, Mid: 1, RequestId: "r"})
			return err
		}},
		{"endLive", func() error {
			_, err := NewEndLiveLogic(context.Background(), empty).
				EndLive(&types.ParamLiveEnd{RoomId: 1, Mid: 1, RequestId: "r"})
			return err
		}},
		{"closeLiveRoom", func() error {
			_, err := NewCloseLiveRoomLogic(context.Background(), empty).
				CloseLiveRoom(&types.ParamLiveClose{RoomId: 1, OperatorMid: 1, RequestId: "r"})
			return err
		}},
		{"mutateLiveAnchor", func() error {
			_, err := NewMutateLiveAnchorLogic(context.Background(), empty).
				MutateLiveAnchor(&types.ParamLiveAnchorMutate{RoomId: 1, OperatorMid: 1, TargetMid: 2, Action: 1, RequestId: "r"})
			return err
		}},
	}
	for _, tc := range calls {
		if err := tc.run(); err == nil {
			t.Fatalf("%s：未配置 LiveRoomRPC 时必须报错", tc.name)
		}
	}
}

func TestLiveLogicsPropagateDownstreamError(t *testing.T) {
	// 网关只做透传：下游任何错误都必须原样上抛，
	// 不返回零值房间/空列表成功（否则端上会显示一个不存在的房间）。
	sentinel := errors.New("liveroom: not implemented")
	empty := &svc.ServiceContext{LiveRoom: &fakeLiveRoomClient{err: sentinel}}
	calls := []struct {
		name string
		run  func() error
	}{
		{"liveRoomInfo", func() error {
			_, err := NewLiveRoomInfoLogic(context.Background(), empty).LiveRoomInfo(&types.ParamLiveRoom{RoomId: 1})
			return err
		}},
		{"liveRoomByUp", func() error {
			_, err := NewLiveRoomByUpLogic(context.Background(), empty).LiveRoomByUp(&types.ParamLiveRoomByUp{OwnerMid: 1})
			return err
		}},
		{"listLiveRooms", func() error {
			_, err := NewListLiveRoomsLogic(context.Background(), empty).ListLiveRooms(&types.ParamLiveRooms{Page: 1})
			return err
		}},
		{"listLiveAreas", func() error {
			_, err := NewListLiveAreasLogic(context.Background(), empty).ListLiveAreas(&types.ParamLiveAreas{})
			return err
		}},
		{"getLiveSession", func() error {
			_, err := NewGetLiveSessionLogic(context.Background(), empty).GetLiveSession(&types.ParamLiveSession{RoomId: 1})
			return err
		}},
		{"listLiveSessions", func() error {
			_, err := NewListLiveSessionsLogic(context.Background(), empty).ListLiveSessions(&types.ParamLiveSessions{RoomId: 1})
			return err
		}},
		{"listLiveAnchors", func() error {
			_, err := NewListLiveAnchorsLogic(context.Background(), empty).ListLiveAnchors(&types.ParamLiveAnchors{RoomId: 1})
			return err
		}},
		{"createLiveRoom", func() error {
			_, err := NewCreateLiveRoomLogic(context.Background(), empty).
				CreateLiveRoom(&types.ParamLiveRoomCreate{Mid: 1, Title: "t", AreaId: 1, RequestId: "r"})
			return err
		}},
		{"updateLiveRoomInfo", func() error {
			_, err := NewUpdateLiveRoomInfoLogic(context.Background(), empty).
				UpdateLiveRoomInfo(&types.ParamLiveRoomInfoUpdate{RoomId: 1, OperatorMid: 1, RequestId: "r"})
			return err
		}},
		{"updateLiveRoomSetting", func() error {
			_, err := NewUpdateLiveRoomSettingLogic(context.Background(), empty).
				UpdateLiveRoomSetting(&types.ParamLiveRoomSettingUpdate{RoomId: 1, OperatorMid: 1, RequestId: "r"})
			return err
		}},
		{"prepareLive", func() error {
			_, err := NewPrepareLiveLogic(context.Background(), empty).
				PrepareLive(&types.ParamLivePrepare{RoomId: 1, Mid: 1, RequestId: "r"})
			return err
		}},
		{"startLive", func() error {
			_, err := NewStartLiveLogic(context.Background(), empty).
				StartLive(&types.ParamLiveStart{RoomId: 1, Mid: 1, RequestId: "r"})
			return err
		}},
		{"endLive", func() error {
			_, err := NewEndLiveLogic(context.Background(), empty).
				EndLive(&types.ParamLiveEnd{RoomId: 1, Mid: 1, RequestId: "r"})
			return err
		}},
		{"closeLiveRoom", func() error {
			_, err := NewCloseLiveRoomLogic(context.Background(), empty).
				CloseLiveRoom(&types.ParamLiveClose{RoomId: 1, OperatorMid: 1, RequestId: "r"})
			return err
		}},
		{"mutateLiveAnchor", func() error {
			_, err := NewMutateLiveAnchorLogic(context.Background(), empty).
				MutateLiveAnchor(&types.ParamLiveAnchorMutate{RoomId: 1, OperatorMid: 1, TargetMid: 2, Action: 2, RequestId: "r"})
			return err
		}},
	}
	for _, tc := range calls {
		if err := tc.run(); !errors.Is(err, sentinel) {
			t.Fatalf("%s err = %v, want %v", tc.name, err, sentinel)
		}
	}
}
