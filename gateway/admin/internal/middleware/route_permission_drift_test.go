package middleware

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本文件把 routePermissions 与 goctl 生成的 internal/handler/routes.go 做交叉校验。
//
// 为什么需要：中间件对表外路由一律 403（fail-closed），所以「.api 里挂了 AdminPermission
// 但忘了登记权限点」会让这条路由永久不可用；反过来「登记了一个免鉴权路由」会让权限点变成
// 死条目。两种漂移都无法被编译器和手写用例发现，cron 域 8 条写路由就漂移过一次。
//
// routes.go 是生成产物、格式稳定，因此这里按 AddRoutes 调用块切分，
// 而不是解析 Go AST：一旦 goctl 改了注册形态，下面的 find* 会返回空集合并让测试
// 立刻失败，不会退化成永真检查。

var (
	prefixRe   = regexp.MustCompile(`rest\.WithPrefix\("([^"]+)"\)`)
	pathRe     = regexp.MustCompile(`Path:\s+"([^"]+)"`)
	methodRe   = regexp.MustCompile(`Method:\s+http\.Method(\w+)`)
	middleware = "serverCtx.AdminPermission"
)

// registeredRoute 是 routes.go 里一条注册项（方法 + 不含前缀的路径）。
type registeredRoute struct {
	method string
	path   string
}

// routeBlock 是一条 AddRoutes 调用块解析出的路由集合及其保护方式。
type routeBlock struct {
	protected bool
	prefix    string
	routes    []registeredRoute
}

func parseRouteBlocks(t *testing.T, src string) []routeBlock {
	t.Helper()
	if !strings.Contains(src, "func RegisterHandlers(") {
		t.Fatal("routes.go 形态与预期不符：找不到 RegisterHandlers，请同步更新本用例")
	}
	marker := "server.AddRoutes("
	idxs := []int{}
	for off := 0; ; {
		j := strings.Index(src[off:], marker)
		if j < 0 {
			break
		}
		off += j + len(marker)
		idxs = append(idxs, off-len(marker))
	}
	if len(idxs) < 2 {
		t.Fatalf("routes.go 中 AddRoutes 调用块过少（%d），解析很可能失效", len(idxs))
	}

	blocks := make([]routeBlock, 0, len(idxs))
	for n, start := range idxs {
		end := len(src)
		if n+1 < len(idxs) {
			end = idxs[n+1]
		}
		chunk := src[start:end]
		b := routeBlock{protected: strings.Contains(chunk, middleware)}
		if p := prefixRe.FindStringSubmatch(chunk); p != nil {
			b.prefix = p[1]
		}
		// goctl 每个注册项固定写成 Method 紧跟 Path，按成对取用；
		// 数量不等说明形态变了，直接失败而不是猜方法。
		methods := methodRe.FindAllStringSubmatch(chunk, -1)
		paths := pathRe.FindAllStringSubmatch(chunk, -1)
		if len(paths) == 0 {
			t.Fatalf("第 %d 个 AddRoutes 块解析不到任何 Path，解析已失效", n+1)
		}
		if len(methods) != len(paths) {
			t.Fatalf("第 %d 个 AddRoutes 块的 Method(%d) 与 Path(%d) 数量不等，goctl 注册形态已变化",
				n+1, len(methods), len(paths))
		}
		for i := range paths {
			b.routes = append(b.routes, registeredRoute{method: strings.ToUpper(methods[i][1]), path: paths[i][1]})
		}
		blocks = append(blocks, b)
	}
	return blocks
}

func loadRoutes(t *testing.T) []routeBlock {
	t.Helper()
	src, err := os.ReadFile("../handler/routes.go")
	if err != nil {
		t.Fatalf("读取生成的 routes.go 失败：本用例依赖它作为受保护路由的唯一事实来源: %v", err)
	}
	return parseRouteBlocks(t, string(src))
}

// protectedGetAllowlist 是「挂在受保护组里的 GET」的唯一合法集合。
//
// 本仓口径是写面挂判定、读面免鉴权（后台列表页每次刷新都会打一次 RPC，全量挂判定会把
// operation 变成读放大瓶颈），因此一条 GET 出现在受保护组里必须是**明确决定过**的例外：
// 要么它其实是写操作（形状是 GET、后果是改状态），要么它是被读主体明确的个人数据定向读。
// 原来这里是一条「免鉴权路由数不得少于受保护路由数」的比例哨兵，阶段 1-2 写路由补鉴权后
// 受保护路由已多于免鉴权路由，比例哨兵会误报；换成按路由逐项点名的白名单，
// 既能继续拦住「顺手把读面整组挂上判定」，也不会随授权面增减而失效。
var protectedGetAllowlist = map[string]string{
	"/admin/account/cache/del":         "名为 GET 实为写：调用 account DelCache 失效指定用户缓存",
	"/admin/inbox/unread/recompute":    "名为 GET 实为写：重算未读快照并回填缓存",
	"/x/member/realname/stripped/info": "被读主体明确的个人数据定向读（与 spm:interest 同口径）",
	"/x/member/realname/mid/by/card":   "证件号反查 mid，可对证件号清单批量配对，属个人数据定向读",
	"/x/member/web/login/log":          "单个用户的登录行为明细，属个人数据定向读",
}

// 每一条挂了 AdminPermission 的路由都必须在权限表里有 resource+action。
func TestEveryProtectedRouteIsRegistered(t *testing.T) {
	blocks := loadRoutes(t)
	var protected, public, protectedGet int
	for _, b := range blocks {
		for _, r := range b.routes {
			full := b.prefix + r.path
			if !b.protected {
				public++
				continue
			}
			protected++
			perm, ok := permissionFor(full)
			if !ok {
				t.Errorf("受保护路由 %s 未登记权限点，中间件会永久返回 403", full)
				continue
			}
			if perm.Resource == "" || perm.Action == "" {
				t.Errorf("受保护路由 %s 的权限点为空: %+v", full, perm)
			}
			if r.method == "GET" {
				protectedGet++
				if _, allowed := protectedGetAllowlist[full]; !allowed {
					t.Errorf("受保护路由 %s 是 GET 且不在例外白名单里：读面挂判定会把 operation 变成读放大瓶颈，"+
						"确有必要（写形状的 GET 或定向个人数据读）就把它连同意由一起加进 protectedGetAllowlist", full)
				}
			}
		}
	}
	if protected == 0 {
		t.Fatal("routes.go 里没有任何受 AdminPermission 保护的路由，解析或挂载已失效")
	}
	if public == 0 {
		t.Fatal("routes.go 里已经没有免鉴权路由，与「读面免鉴权」的口径不符，检查是否整组误挂中间件")
	}
	for path := range protectedGetAllowlist {
		if !isProtectedRoute(blocks, path) {
			t.Errorf("白名单条目 %s 已不是受保护路由：要么它被改回免鉴权（个人数据读面裸奔），要么路径写错", path)
		}
	}
	t.Logf("受保护路由 %d 条（其中 GET %d 条），免鉴权路由 %d 条，权限表 %d 条",
		protected, protectedGet, public, len(routePermissions))
}

func isProtectedRoute(blocks []routeBlock, full string) bool {
	for _, b := range blocks {
		if !b.protected {
			continue
		}
		for _, r := range b.routes {
			if b.prefix+r.path == full {
				return true
			}
		}
	}
	return false
}

// 权限表里的键必须真的对应一条受保护路由：否则要么路径写错（永远命中不了），
// 要么路由已从中间件组摘掉（权限点成了无人使用的死条目）。
func TestNoStalePermissionEntries(t *testing.T) {
	protected := map[string]bool{}
	for _, b := range loadRoutes(t) {
		if !b.protected {
			continue
		}
		for _, r := range b.routes {
			protected[b.prefix+r.path] = true
		}
	}
	for path := range routePermissions {
		if !protected[path] {
			t.Errorf("权限表条目 %s 没有对应的受保护路由（路径拼错或路由已改为免鉴权）", path)
		}
	}
}

// 中间件运行时用的是真实请求路径，因此带 :param 的表项必须能被具体路径命中，
// 同时不能把「段数不同/字面段不同」的路径误判进来（后者会绕过授权面判定）。
func TestParamRoutesMatchConcretePaths(t *testing.T) {
	cases := []struct {
		path     string
		resource string
		action   string
	}{
		{"/admin/video/submissions/1234/transition", "video:submission", "transition"},
		{"/admin/catalog/episodes/9/publish", "catalog:episode", "publish"},
		{"/admin/catalog/episodes/9/offline", "catalog:episode", "offline"},
		{"/admin/rights/windows/77/expire", "rights:window", "expire"},
	}
	for _, c := range cases {
		perm, ok := permissionFor(c.path)
		if !ok {
			t.Errorf("具体路径 %s 命中不了任何权限点，中间件会 403 掉这条正常路由", c.path)
			continue
		}
		if perm.Resource != c.resource || perm.Action != c.action {
			t.Errorf("路径 %s 命中了 %s/%s，应为 %s/%s", c.path, perm.Resource, perm.Action, c.resource, c.action)
		}
	}

	// 表外路径一律命中不了：段数不同、字面段不同、参数段为空的变形都不能被判成有权。
	for _, bad := range []string{
		"/admin/catalog/episodes/9/10/publish",
		"/admin/catalog/episodes/9/published",
		"/admin/catalog/episodes//publish",
		"/admin/video/submissions/1234",
		"/admin/rights/windows/77/extend",
	} {
		if _, ok := permissionFor(bad); ok {
			t.Errorf("路径 %s 不该命中任何权限点，命中即等于给未授权入口开了口子", bad)
		}
	}
}
