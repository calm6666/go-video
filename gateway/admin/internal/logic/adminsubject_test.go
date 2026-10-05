package logic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
)

// 本文件钉住两件事：
//  1. adminsubject.go 的四个门槛行为正确（有会话才放行、后台账号 ID 以会话为准）；
//  2. 「每一条 AdminPermission 受保护路由的 logic 都必须读到会话身份」这条不变量——
//     用源码扫描而不是逐条起 gRPC 假服务端来验：路由有 166 条，逐个构造 ServiceContext
//     的成本远高于收益，而漏掉的恰恰是「新加了写路由却忘了门槛」这种一眼可扫出的形态。

const gateAdminID = int64(4242)

// withAdminSession 模拟「AdminPermission 判定通过后」的请求上下文（中间件在那时挂上身份）。
func withAdminSession(id int64) context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: id, Roles: []string{"ops"}})
}

// adminOperatorString 是 notification 这类只有 operator 单列的下游、在无显示名时的回落主体串
// （口径见 conv_notification.go 的 notificationOperator）：主体号必须来自会话。
func adminOperatorString(id int64) string {
	return "admin:" + strconv.FormatInt(id, 10)
}

func TestAdminSubjectGatesFailClosedWithoutSession(t *testing.T) {
	// 每一例都给出「字段完全合法」的入参：这样报出来的错只可能是缺会话，
	// 而不是被前置的字段校验顺手挡掉——否则用例通过的原因就是错的。
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"sessionGate", func(ctx context.Context) error { return adminSessionGate(ctx, "testRoute") }},
		{"subjectGate", func(ctx context.Context) error {
			return adminSubjectGate(ctx, "testRoute", "operator_mid", 99)
		}},
		{"actorGate", func(ctx context.Context) error {
			return adminActorGate(ctx, "testRoute", "operator", "ops-a")
		}},
		{"operatorID", func(ctx context.Context) error {
			_, err := adminOperatorID(ctx, "testRoute", 99)
			return err
		}},
	} {
		t.Run(tc.name+" 无会话", func(t *testing.T) {
			if err := tc.run(context.Background()); !errors.Is(err, errAdminSessionRequired) {
				t.Fatalf("无会话应回 errAdminSessionRequired，got %v", err)
			}
		})
		t.Run(tc.name+" 会话非法", func(t *testing.T) {
			// AdminFromContext 把 admin_id<=0 判成「没有身份」，门槛必须跟上这个口径。
			for _, bad := range []int64{0, -1} {
				ctx := middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: bad})
				if err := tc.run(ctx); !errors.Is(err, errAdminSessionRequired) {
					t.Fatalf("admin_id=%d 应视同无会话，got %v", bad, err)
				}
			}
		})
		t.Run(tc.name+" 有会话则放行", func(t *testing.T) {
			if err := tc.run(withAdminSession(gateAdminID)); err != nil {
				t.Fatalf("合法会话 + 合法主体不该被拒：%v", err)
			}
		})
	}
}

func TestAdminSubjectGatesFieldOrder(t *testing.T) {
	// 主体缺失先于会话判定报错（与 openOperatorGate 同口径）：表单少填字段是调用方的
	// 第一手错误，先说清它，而不是抛一个「你需要登录」把人支去重新登录。
	t.Run("mid 非正数", func(t *testing.T) {
		err := adminSubjectGate(withAdminSession(gateAdminID), "testRoute", "operator_mid", 0)
		if err == nil || !strings.Contains(err.Error(), "operator_mid required") {
			t.Fatalf(`应点名 operator_mid，got %v`, err)
		}
		if errors.Is(err, errAdminSessionRequired) {
			t.Fatal("字段错误被会话错误盖住了")
		}
	})
	t.Run("操作人空白", func(t *testing.T) {
		err := adminActorGate(withAdminSession(gateAdminID), "testRoute", "operator", "   ")
		if err == nil || !strings.Contains(err.Error(), "operator required") {
			t.Fatalf(`应点名 operator，got %v`, err)
		}
	})
	// 无会话且字段也非法时，报的必须是字段错：两道门都要有各自说得清的错误，
	// 否则「字段校验是否真的存在」就无法从测试里看出来。
	t.Run("字段非法且无会话", func(t *testing.T) {
		if err := adminSubjectGate(context.Background(), "testRoute", "operator_mid", -5); errors.Is(err, errAdminSessionRequired) {
			t.Fatalf("字段错应优先于会话错，got %v", err)
		}
	})
}

func TestAdminOperatorIDTakesSessionOverClaimed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claimed int64
		want    int64
	}{
		{"客户端未声明由会话补齐", 0, gateAdminID},
		{"客户端声明一致", gateAdminID, gateAdminID},
		// 不一致时以会话为准并留痕：判定权在中间件（它已确认这个 admin_id 有这条路由的
		// 权限），客户端自报的 ID 没有任何额外信息量，拒绝只会让后台表单多一个能填错的位。
		{"客户端声明不一致时取会话值", 999, gateAdminID},
		{"客户端声明为负数", -7, gateAdminID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := adminOperatorID(withAdminSession(gateAdminID), "testRoute", tc.claimed)
			if err != nil {
				t.Fatalf("意外错误：%v", err)
			}
			if got != tc.want {
				t.Fatalf("claimed=%d 应得 %d，got %d", tc.claimed, tc.want, got)
			}
		})
	}
}

// ---------- 全量不变量：每条受保护路由的 logic 都必须消费会话身份 ----------

var (
	reAddRoutes   = regexp.MustCompile(`(?s)server\.AddRoutes\(`)
	reWithPrefix  = regexp.MustCompile(`rest\.WithPrefix\("([^"]*)"`)
	reRouteEntry  = regexp.MustCompile(`(?s)Path:\s*"([^"]*)",\s*Handler:\s*(\w+)\(serverCtx\)`)
	rePermTable   = regexp.MustCompile(`(?m)^\t"(/[^"]+)"\s*:\s*\{Resource:`)
	reTopFunc     = regexp.MustCompile(`(?m)^func (\w+)\(`)
	reHandlerFunc = regexp.MustCompile(`func (\w+)\(svcCtx \*svc\.ServiceContext\)`)
	reHandlerCall = regexp.MustCompile(`logic\.(New\w+Logic)\(`)
	reLogicCtor   = regexp.MustCompile(`func (New\w+Logic)\(`)
)

// protectedRoutesFromSource 解析 routes.go，返回挂了 AdminPermission 中间件的完整路径 → handler 名。
func protectedRoutesFromSource(t *testing.T) map[string]string {
	t.Helper()
	raw := readFile(t, filepath.Join("..", "handler", "routes.go"))
	out := map[string]string{}
	for _, block := range reAddRoutes.Split(raw, -1)[1:] {
		prefix := ""
		if m := reWithPrefix.FindStringSubmatch(block); m != nil {
			prefix = m[1]
		}
		for _, m := range reRouteEntry.FindAllStringSubmatchIndex(block, -1) {
			if !strings.Contains(block[:m[0]], "serverCtx.AdminPermission") {
				continue
			}
			out[prefix+block[m[2]:m[3]]] = block[m[4]:m[5]]
		}
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	return string(b)
}

// sessionAwareFuncs 返回包内所有「会读会话身份」的顶层函数名（含传递调用）。
// 顶层「func 名字(」只会匹配到普通函数：logic 的入口是方法「func (l *XLogic) Y(」，不在此列，
// 所以这里收集到的必然是校验/装配类的助手函数。
func sessionAwareFuncs(t *testing.T, bodies map[string]string) map[string]bool {
	t.Helper()
	aware := map[string]bool{}
	for name, body := range bodies {
		if strings.Contains(body, "middleware.AdminFromContext") {
			aware[name] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for name, body := range bodies {
			if aware[name] {
				continue
			}
			for fn := range aware {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(fn) + `\(`).MatchString(body) {
					aware[name] = true
					grew = true
					break
				}
			}
		}
	}
	return aware
}

func TestEveryProtectedRouteLogicUsesSession(t *testing.T) {
	protected := protectedRoutesFromSource(t)
	if len(protected) < 150 {
		t.Fatalf("解析出的受保护路由只有 %d 条，明显低于预期：解析规则已与 routes.go 脱节，"+
			"本用例此刻无法证明任何东西", len(protected))
	}

	// 权限表与受保护路由必须一一对应（与 route_permission_drift_test.go 互补：那边查的是
	// 「登记了没」，这里顺带确认「中间件组挂上了没」，两处任缺其一都是空门）。
	table := rePermTable.FindAllStringSubmatch(readFile(t,
		filepath.Join("..", "middleware", "adminpermissionmiddleware.go")), -1)
	if len(table) != len(protected) {
		t.Fatalf("权限表 %d 条 != 挂了 AdminPermission 的路由 %d 条", len(table), len(protected))
	}
	for _, m := range table {
		if _, ok := protected[m[1]]; !ok {
			t.Fatalf("权限表登记了 %s，但 routes.go 里没有挂 AdminPermission 的同路径路由", m[1])
		}
	}

	// handler 名 → logic 构造器 → logic 文件名
	handlerOf := map[string]string{}
	logicFileOf := map[string]string{}
	for _, name := range listGo(t, filepath.Join("..", "handler")) {
		src := readFile(t, filepath.Join("..", "handler", name))
		h := reHandlerFunc.FindStringSubmatch(src)
		c := reHandlerCall.FindStringSubmatch(src)
		if h != nil && c != nil {
			handlerOf[h[1]] = c[1]
		}
	}
	for _, name := range listGo(t, ".") {
		if !strings.HasSuffix(name, "logic.go") {
			continue
		}
		if c := reLogicCtor.FindStringSubmatch(readFile(t, name)); c != nil {
			logicFileOf[c[1]] = name
		}
	}

	// 会话感知的助手函数（跨全部非测试文件求传递闭包）
	bodies := map[string]string{}
	for _, name := range listGo(t, ".") {
		src := readFile(t, name)
		idx := reTopFunc.FindAllStringSubmatchIndex(src, -1)
		for i, m := range idx {
			end := len(src)
			if i+1 < len(idx) {
				end = idx[i+1][0]
			}
			bodies[src[m[2]:m[3]]] = src[m[1]:end]
		}
	}
	aware := sessionAwareFuncs(t, bodies)
	if len(aware) < 20 {
		t.Fatalf("识别出的会话感知助手只有 %d 个，解析可能已失效", len(aware))
	}

	var missing []string
	for path, handler := range protected {
		ctor, ok := handlerOf[handler]
		if !ok {
			missing = append(missing, path+" 的 handler "+handler+" 找不到 logic 构造器")
			continue
		}
		file, ok := logicFileOf[ctor]
		if !ok {
			missing = append(missing, path+" 的构造器 "+ctor+" 找不到 logic 文件")
			continue
		}
		src := readFile(t, file)
		if strings.Contains(src, "middleware.AdminFromContext") {
			continue
		}
		callsAware := false
		for fn := range aware {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(fn) + `\(`).MatchString(src) {
				callsAware = true
				break
			}
		}
		if !callsAware {
			missing = append(missing, path+" → "+file+" 完全没有读会话身份")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("有 %d 条受保护路由的 logic 不接受会话身份校验，客户端自报的主体可被任意伪造：\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// listGo 列出目录下的非测试 .go 文件。
func listGo(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录 %s 失败：%v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}
