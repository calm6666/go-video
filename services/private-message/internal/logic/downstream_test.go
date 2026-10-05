// 本文件放下游三个 gRPC 依赖（social-graph / risk-control / moderation-orchestrator）的
// 进程内替身：bufconn 上起真 server，logic 侧走真 client。
//
// 为什么要起真 gRPC 而不是给 logic 塞一个接口假实现：
// 本服务的门禁适配（gate.go）拿到的依赖类型是 zrpc.Client，它只暴露 Conn() *grpc.ClientConn，
// 适配代码在 conn 之上再包一层生成的 client。也就是说「未配置 → Err*NotConfigured」
// 「调用失败 → 保守拒收」这两条最容易出事故的分支，都发生在真实的 gRPC 调用上；
// 用假接口的话这两条分支根本到不了，等于把最该测的地方绕过去。
//
// 替身默认不放行任何关系：blocked/following 是显式白名单表，
// 风控默认返回 ALLOW、机审默认返回一个真实 task_id —— 这两项是「环境已配置」的等价物，
// 而不是「永远成功」：需要失败分支的用例用 failRisk / failSubmit / denyDecision 显式注入。

package logic

import (
	"context"
	"net"
	"sync"
	"testing"

	moderationrpc "go-video/services/moderation-orchestrator/rpc"
	riskrpc "go-video/services/risk-control/rpc"
	sgr "go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// stubClient 是 zrpc.Client 的测试替身：接口只有 Conn() 一个方法。
type stubClient struct{ conn *grpc.ClientConn }

func (c *stubClient) Conn() *grpc.ClientConn { return c.conn }

var _ zrpc.Client = (*stubClient)(nil)

func pairOf(a, b int64) [2]int64 { return [2]int64{a, b} }

// --- social-graph ---

type fakeSocialGraphServer struct {
	sgr.UnimplementedSocialGraphServer

	mu         sync.Mutex
	blocked    map[[2]int64]bool // [mid, owner]：mid 拉黑了 owner
	following  map[[2]int64]bool // [mid, owner]：mid 关注了 owner
	blackReqs  []*sgr.RelationReq
	followReqs []*sgr.RelationReq
	failBlack  bool
	failFollow bool
}

func newFakeSocialGraph() *fakeSocialGraphServer {
	return &fakeSocialGraphServer{blocked: map[[2]int64]bool{}, following: map[[2]int64]bool{}}
}

func (s *fakeSocialGraphServer) IsBlacked(_ context.Context, in *sgr.RelationReq) (*sgr.RelationReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blackReqs = append(s.blackReqs, in)
	if s.failBlack {
		return nil, status.Error(codes.Unavailable, "social-graph down")
	}
	return &sgr.RelationReply{Following: s.blocked[pairOf(in.GetMid(), in.GetOwner())]}, nil
}

func (s *fakeSocialGraphServer) IsFollowing(_ context.Context, in *sgr.RelationReq) (*sgr.RelationReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.followReqs = append(s.followReqs, in)
	if s.failFollow {
		return nil, status.Error(codes.Unavailable, "social-graph down")
	}
	return &sgr.RelationReply{Following: s.following[pairOf(in.GetMid(), in.GetOwner())]}, nil
}

func (s *fakeSocialGraphServer) block(mid, owner int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked[pairOf(mid, owner)] = true
}

func (s *fakeSocialGraphServer) follow(mid, owner int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.following[pairOf(mid, owner)] = true
}

func (s *fakeSocialGraphServer) blackCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.blackReqs)
}

func (s *fakeSocialGraphServer) setFailBlack(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failBlack = v
}

func (s *fakeSocialGraphServer) setFailFollow(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFollow = v
}

// --- risk-control ---

type fakeRiskServer struct {
	riskrpc.UnimplementedRiskControlServer

	mu       sync.Mutex
	reqs     []*riskrpc.CheckActionReq
	decision riskrpc.Decision
	degraded bool
	basis    string
	reason   string
	fail     bool
}

func newFakeRisk() *fakeRiskServer {
	return &fakeRiskServer{decision: riskrpc.Decision_DECISION_ALLOW}
}

func (s *fakeRiskServer) CheckAction(_ context.Context, in *riskrpc.CheckActionReq) (*riskrpc.CheckActionReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, in)
	if s.fail {
		return nil, status.Error(codes.Unavailable, "risk-control down")
	}
	reply := &riskrpc.CheckActionReply{Decision: s.decision, Degraded: s.degraded, Basis: s.basis}
	if s.reason != "" {
		reply.Punishment = &riskrpc.PunishmentSnapshot{ReasonCode: s.reason}
	}
	return reply, nil
}

func (s *fakeRiskServer) setDecision(d riskrpc.Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decision = d
}

func (s *fakeRiskServer) setDegraded(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.degraded = v
}

func (s *fakeRiskServer) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func (s *fakeRiskServer) requests() []*riskrpc.CheckActionReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*riskrpc.CheckActionReq(nil), s.reqs...)
}

// --- moderation-orchestrator ---

type fakeModerationServer struct {
	moderationrpc.UnimplementedModerationOrchestratorServer

	mu      sync.Mutex
	reqs    []*moderationrpc.SubmitReq
	taskID  int64
	empty   bool // 返回 task_id=0：「调用成功但没建任务」
	fail    bool
	callsOK int
}

func newFakeModeration() *fakeModerationServer {
	return &fakeModerationServer{taskID: 777001}
}

func (s *fakeModerationServer) SubmitForReview(_ context.Context, in *moderationrpc.SubmitReq) (*moderationrpc.TaskReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, in)
	if s.fail {
		return nil, status.Error(codes.Unavailable, "moderation down")
	}
	if s.empty {
		return &moderationrpc.TaskReply{}, nil
	}
	s.callsOK++
	id := s.taskID
	s.taskID++
	return &moderationrpc.TaskReply{Task: &moderationrpc.Task{TaskId: id, SubmissionId: in.GetSubmissionId()}}, nil
}

func (s *fakeModerationServer) setTaskID(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taskID = id
}

func (s *fakeModerationServer) setEmpty(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.empty = v
}

func (s *fakeModerationServer) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func (s *fakeModerationServer) requests() []*moderationrpc.SubmitReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*moderationrpc.SubmitReq(nil), s.reqs...)
}

func (s *fakeModerationServer) submitCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// --- 起服务与接线 ---

type downstream struct {
	sg      *fakeSocialGraphServer
	risk    *fakeRiskServer
	moder   *fakeModerationServer
	cleanup []func()
}

// serve 在 bufconn 上注册一个 gRPC server 并回一个已连上的 client conn。
func serveOnBufconn(t *testing.T, name string, register func(grpc.ServiceRegistrar)) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	register(srv)
	go func() {
		_ = srv.Serve(lis)
	}()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("%s 建客户端失败：%v", name, err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	})
	return conn
}

// attachDownstream 把三个下游都接上（默认「已配置」形态）。
// 用例若要多测「未配置」，新建一个 env 不接线即可（默认字段就是 nil）。
func (e *env) attachDownstream(t *testing.T) *downstream {
	t.Helper()
	d := &downstream{sg: newFakeSocialGraph(), risk: newFakeRisk(), moder: newFakeModeration()}
	e.svc.SocialGraph = &stubClient{conn: serveOnBufconn(t, "social-graph", func(r grpc.ServiceRegistrar) {
		sgr.RegisterSocialGraphServer(r, d.sg)
	})}
	e.svc.RiskControl = &stubClient{conn: serveOnBufconn(t, "risk-control", func(r grpc.ServiceRegistrar) {
		riskrpc.RegisterRiskControlServer(r, d.risk)
	})}
	e.svc.Moderation = &stubClient{conn: serveOnBufconn(t, "moderation", func(r grpc.ServiceRegistrar) {
		moderationrpc.RegisterModerationOrchestratorServer(r, d.moder)
	})}
	return d
}

// attachSocialGraphOnly 只接 social-graph：风控/机审保持未配置，
// 用来证明「已配置的那条链路不会替未配置的那条兜底放行」。
func (e *env) attachSocialGraphOnly(t *testing.T) *fakeSocialGraphServer {
	t.Helper()
	s := newFakeSocialGraph()
	e.svc.SocialGraph = &stubClient{conn: serveOnBufconn(t, "social-graph", func(r grpc.ServiceRegistrar) {
		sgr.RegisterSocialGraphServer(r, s)
	})}
	return s
}
