package logic

// menu_logic_test.go 覆盖 SaveMenu / GetMenu。
//
// 要紧的结论依次是：
//   - SaveMenu 的**全部**入参校验都在任何 SQL 之前（守卫拒绝时零依赖调用）；
//   - 新建路径与更新路径对同一个非法输入必须给同样的答复——本轮实测**不一致**：
//     parent_id 指向不存在的节点时更新路径拒（ErrMenuNotFound）、新建路径照收，
//     于是能造出孤儿节点，而孤儿节点又会让后续**合法**的更新被父链上溯拒掉；
//   - 写读成对：SaveMenu 只 bump op:menu:ver（不按键删），GetMenu 按版本换槽回源，
//     全量树按版本缓存、按管理员在内存里过滤，因此**不同管理员共用同一份缓存**；
//   - GetMenu 的可见性依据是「角色并集 + 节点 required_permission」，
//     隐藏节点与解析不出权限串的节点都不外发，但**缓存里存的是未过滤的全量树**。
//
// 本轮核实到的生产现状（不改生产代码，逐条钉住并登记 README）：
//   - menu.go:215-232：更新路径返回的是新 new 出来的 node（只带八列业务字段），
//     而 model/menu.go:85-94 的 Update **不回写** ctime/mtime → 应答的
//     ctime/mtime 恒为 0，而库里 mtime 已刷成当前时间。新建路径反而没问题
//     （model/menu.go:64-83 会把自增 ID 与两个时间戳写回入参指针）。
//   - menu.go:157-160：required_permission 只校验**格式**，不校验权限点是否真的
//     存在，也不查这一行有没有被任何角色引用 → 写错一个字母，菜单项对该管理员
//     永久不可见，且保存时没有任何提示。
//   - menu.go:172-191：新建路径不校验 parent_id 存在、不做父链深度检查
//     （assertNoMenuCycle / maxMenuDepth 只在更新路径调用）。
//   - menu.go:186-189 与 228-231：缓存 bump 与审计写不在同一事务，
//     审计失败时节点已落库、缓存已作废。
//   - getmenulogic.go:31-34 + menu.go:106-108：logic 把非正的 admin_id 一律回退成
//     操作者本人，因此 repository 里 `adminID <= 0 → ErrInvalidOperator` 这条守卫
//     在 logic 入口上不可达。

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/operation/model"
	"go-video/services/operation/rpc"
)

// saveMenuReq 组一个菜单保存请求（menuID=0 表示新建）。
func saveMenuReq(operator, menuID, parentID int64, name, path, icon, required string, sort int32, state int32) *rpc.SaveMenuReq {
	return &rpc.SaveMenuReq{
		Ctx: opCtx(operator), MenuId: menuID, ParentId: parentID, Name: name, Path: path,
		Icon: icon, Sort: sort, RequiredPermission: required, State: state,
	}
}

// getMenuReq 组一个取菜单请求（adminID=0 表示取操作者本人的）。
func getMenuReq(operator, adminID int64) *rpc.GetMenuReq {
	return &rpc.GetMenuReq{Ctx: opCtx(operator), AdminId: adminID}
}

// menuFields 把菜单列表摘要成「id/名/sort」，一次比对覆盖筛选与排序。
func menuFields(rows []*rpc.MenuItem) []string {
	out := make([]string, 0, len(rows))
	for _, m := range rows {
		out = append(out, fmt.Sprintf("%d/%s/%d", m.MenuId, m.Name, m.Sort))
	}
	return out
}

// ============================================================================
// SaveMenu：新建
// ============================================================================

func TestSaveMenu新建节点落库并回传真实主键与时间戳(t *testing.T) {
	e := newEnv(t)
	started := nowUnix()
	// required 刻意用大写：SaveMenu 会 ToLower+TrimSpace 后入库（菜单可见性判定
	// 依赖它与 op_permission 的小写值对上）。State=0 走「视为显示」的兜底。
	reply, err := saveMenuCall(t, e, saveMenuReq(9001, 0, 0, "  稿件管理  ", "/content/submissions", "file", "VIDEO:Submission#READ", 10, 0))
	to := nowUnix()
	wantOK(t, reply, err, "新建根菜单节点")
	wantOps(t, "新建菜单轨迹", e.ops(0), []string{
		"menu.Insert:0/稿件管理",
		// 只 bump 版本、不按 key 删：菜单树是按版本分槽的整棵树
		"cache.Incr:op:menu:ver/2592000",
		"audit_index.Insert:menu.save/ok",
	})

	id := reply.Menu.MenuId
	if id == 0 {
		t.Fatalf("应答未带回 menu_id：%+v", reply.Menu)
	}
	wantEQ(t, "新建应答", "name（已去首尾空白）", reply.Menu.Name, "稿件管理")
	wantEQ(t, "新建应答", "required_permission（已归一为小写）", reply.Menu.RequiredPermission, "video:submission#read")
	wantEQ(t, "新建应答", "state（0 视为显示）", reply.Menu.State, model.StateEnable)
	wantEQ(t, "新建应答", "path", reply.Menu.Path, "/content/submissions")
	wantEQ(t, "新建应答", "icon", reply.Menu.Icon, "file")
	wantEQ(t, "新建应答", "sort", reply.Menu.Sort, int32(10))
	wantEQ(t, "新建应答", "parent_id", reply.Menu.ParentId, int64(0))
	wantTSWindow(t, "新建应答", "ctime", reply.Menu.Ctime, started, to)
	wantTSWindow(t, "新建应答", "mtime", reply.Menu.Mtime, started, to)

	row := e.st.menu.get(id)
	if row == nil {
		t.Fatalf("op_menu 里没有节点 %d（应答却把它回传了）", id)
	}
	wantEQ(t, "落库", "menu_id", row.MenuID, id)
	wantEQ(t, "落库", "name", row.Name, reply.Menu.Name)
	wantEQ(t, "落库", "required_permission", row.RequiredPermission, reply.Menu.RequiredPermission)
	wantEQ(t, "落库", "state", row.State, reply.Menu.State)
	wantEQ(t, "落库", "ctime 与应答同一秒", row.Ctime, reply.Menu.Ctime)
	wantEQ(t, "落库", "mtime 与应答同一秒", row.Mtime, reply.Menu.Mtime)

	audit := e.st.audit.only(t)
	wantEQ(t, "存菜单审计", "action", audit.Action, "menu.save")
	wantEQ(t, "存菜单审计", "resource_type", audit.ResourceType, "admin_menu")
	wantEQ(t, "存菜单审计", "resource_id", audit.ResourceID, itoa(id))
	wantEQ(t, "存菜单审计", "result", audit.Result, model.AuditResultOK)
	wantEQ(t, "存菜单审计", "admin_id", audit.AdminID, int64(9001))
}

func TestSaveMenu入参校验全部先于任何SQL与缓存写(t *testing.T) {
	longName := strings.Repeat("菜", 65)
	okName := strings.Repeat("菜", 64)
	cases := []struct {
		name     string
		in       *rpc.SaveMenuReq
		wantSent error
		needle   string
	}{
		{
			name: "无操作者上下文",
			in: &rpc.SaveMenuReq{
				Name: "稿件管理",
			},
			wantSent: ErrInvalidOperator,
		},
		{
			name:     "operator_id 为 0",
			in:       saveMenuReq(0, 0, 0, "稿件管理", "", "", "", 0, 0),
			wantSent: ErrInvalidOperator,
		},
		{
			name:   "name 为空",
			in:     saveMenuReq(9001, 0, 0, "", "/a", "", "", 0, 0),
			needle: "menu name required and must be <= 64 chars",
		},
		{
			name:   "name 全空白",
			in:     saveMenuReq(9001, 0, 0, "   ", "/a", "", "", 0, 0),
			needle: "menu name required and must be <= 64 chars",
		},
		{
			name:   "name 按字计长超 64",
			in:     saveMenuReq(9001, 0, 0, longName, "/a", "", "", 0, 0),
			needle: "menu name required and must be <= 64 chars",
		},
		{
			name:   "path 超 255",
			in:     saveMenuReq(9001, 0, 0, "稿件管理", "/"+strings.Repeat("a", 255), "", "", 0, 0),
			needle: "menu path too long (max 255)",
		},
		{
			name:   "icon 超 64",
			in:     saveMenuReq(9001, 0, 0, "稿件管理", "/a", strings.Repeat("i", 65), "", 0, 0),
			needle: "menu icon too long (max 64)",
		},
		{
			name:   "required 用冒号分隔且不含 #",
			in:     saveMenuReq(9001, 0, 0, "稿件管理", "/a", "", "video:submission:read", 0, 0),
			needle: "must be formatted as resource#action",
		},
		{
			name:   "required 缺 # 分隔符",
			in:     saveMenuReq(9001, 0, 0, "稿件管理", "/a", "", "read", 0, 0),
			needle: `missing "#" separator`,
		},
		{
			name:     "required 动作段非法",
			in:       saveMenuReq(9001, 0, 0, "稿件管理", "/a", "", "video:submission#rea-d", 0, 0),
			wantSent: ErrPermissionInvalid,
		},
		{
			name:   "state 非 1/2",
			in:     saveMenuReq(9001, 0, 0, "稿件管理", "/a", "", "", 0, 3),
			needle: "invalid menu state 3",
		},
		{
			name:   "state 为负",
			in:     saveMenuReq(9001, 0, 0, "稿件管理", "/a", "", "", 0, -1),
			needle: "invalid menu state -1",
		},
		{
			name:   "parent_id 为负",
			in:     saveMenuReq(9001, 0, -1, "稿件管理", "/a", "", "", 0, 0),
			needle: "invalid parent_id -1",
		},
		{
			name: "name 恰为 64 字通过（对照：长度按字不按字节）",
			in:   saveMenuReq(9001, 0, 0, okName, "/a", "", "", 0, 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			reply, err := saveMenuCall(t, e, tc.in)
			wanted := tc.wantSent != nil || tc.needle != ""
			if wanted {
				if reply != nil {
					t.Errorf("%s：应答 = %+v, want nil", tc.name, reply)
				}
				if tc.wantSent != nil {
					wantErrIs(t, tc.name, err, tc.wantSent)
				} else {
					wantErr(t, tc.name, err)
					wantContains(t, tc.name, errText(err), tc.needle)
				}
				wantZeroOps(t, tc.name+"（校验拒绝后不得触碰依赖）", e.ops(0))
				return
			}
			// 对照分支：64 个汉字（192 字节）必须过——按字节实现会在这条红。
			wantOK(t, reply, err, tc.name)
			wantOps(t, tc.name+"轨迹", e.ops(0), []string{
				"menu.Insert:0/" + okName,
				"cache.Incr:op:menu:ver/2592000",
				"audit_index.Insert:menu.save/ok",
			})
		})
	}
}

func TestSaveMenu新建不校验父节点存在孤儿节点直接入库(t *testing.T) {
	// 生产现状（缺口）：menu.go:172-191 的新建分支在 Insert 之前**不查父节点**，
	// 也不做父链深度检查；menu.go:203-214 的更新分支两道都做。
	// 同一份非法输入两条路径答复不同，见下一条用例的连锁后果。
	e := newEnv(t)
	reply, err := saveMenuCall(t, e, saveMenuReq(9001, 0, 999, "孤儿节点", "/orphan", "", "", 0, 0))
	wantOK(t, reply, err, "parent_id 指向不存在的 999")
	wantOps(t, "孤儿节点轨迹（没有任何 menu.FindOne 预检）", e.ops(0), []string{
		"menu.Insert:999/孤儿节点",
		"cache.Incr:op:menu:ver/2592000",
		"audit_index.Insert:menu.save/ok",
	})
	wantNoOpsWith(t, "新建路径不校验父级", e.ops(0), "menu.FindOne")
	row := e.st.menu.get(reply.Menu.MenuId)
	if row == nil {
		t.Fatalf("op_menu 里没有节点")
	}
	wantEQ(t, "落库", "parent_id 原样入库", row.ParentID, int64(999))

	// 本服务的 GetMenu 返回**扁平**列表，孤儿节点照样出现在应答里；
	// 把它排成树是 gateway/admin 的职责，因此这里看到的是「多出一项」。
	adminID := seedAdminRow(t, e.st, "menu_root", nil)
	e.st.log.reset()
	menu, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, menu, err, "读回含孤儿的菜单")
	wantOps(t, "读菜单只回源一次", e.ops(0), []string{
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, adminID),
		"admin_user.FindOne:" + itoa(adminID),
		"role.LoadAdminGrants:" + itoa(adminID),
		"cache.Setex:" + rbacSnapshotKey(0, adminID) + "/60",
		// 上面那次 SaveMenu 已经把 op:menu:ver 建到 1，所以这里不再出现
		// menuVersion 的「首次分配」Incr（对照 TestGetMenu 的 ttl 用例：那里用
		// seedMenuFixture 直插 model，版本 key 缺席，读侧才会补一次 Incr）。
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:1",
		"menu.ListAll",
		"cache.Setex:op:menu:1/300",
	})
	wantEQ(t, "孤儿节点仍出现在扁平列表里", "items 数", int64(len(menu.Items)), int64(1))
}

func TestSaveMenu孤儿节点让后续合法更新也被父链上溯拒掉(t *testing.T) {
	// 上一条缺口的**爆炸半径**：先造出 parent_id=999 的孤儿（实现接受），
	// 之后把一个**正常**节点挂到该孤儿下面时，menu.go:237-258 的父链上溯
	// 走到 999 时读不到行，返回 ErrMenuNotFound —— 报错指向的却是那次合法的更新。
	e := newEnv(t)
	orphan, err := saveMenuCall(t, e, saveMenuReq(9001, 0, 999, "孤儿", "", "", "", 0, 0))
	wantOK(t, orphan, err, "造孤儿")
	root, err := saveMenuCall(t, e, saveMenuReq(9001, 0, 0, "根", "", "", "", 0, 0))
	wantOK(t, root, err, "建根节点")
	e.st.log.reset()

	reply, err := saveMenuCall(t, e, saveMenuReq(9001, root.Menu.MenuId, orphan.Menu.MenuId, "根", "", "", "", 0, 0))
	if reply != nil {
		t.Errorf("挂到孤儿下：应答 = %+v, want nil", reply)
	}
	wantErrIs(t, "挂到孤儿下", err, ErrMenuNotFound)
	wantOps(t, "上溯到缺失的父级", e.ops(0), []string{
		"menu.FindOne:" + itoa(root.Menu.MenuId),   // 被更新节点
		"menu.FindOne:" + itoa(orphan.Menu.MenuId), // 直接父级（存在）
		"menu.FindOne:" + itoa(orphan.Menu.MenuId), // assertNoMenuCycle 又读一次父级
		"menu.FindOne:999",                         // 上溯到孤儿声明的父级 → 缺失
	})
	// 失败后库里形态：两个节点都没被改动、无审计。
	wantEQ(t, "失败后不得改动被更新节点", "parent_id", e.st.menu.get(root.Menu.MenuId).ParentID, int64(0))
	if len(e.st.audit.all()) != 2 {
		t.Errorf("失败后：审计行数 = %d, want 2（仅两次成功新建）", len(e.st.audit.all()))
	}
}

// ============================================================================
// SaveMenu：更新
// ============================================================================

func TestSaveMenu更新成功但应答的ctime与mtime恒为0(t *testing.T) {
	// 生产现状（缺口）：menu.go:215-232 返回的是自己 new 出来的 node，
	// 而 model/menu.go:85-94 的 Update 不回写任何列 ⇒ 应答 ctime/mtime 恒 0，
	// 库里 mtime 却已刷成当前时间。新建路径（model/menu.go:64-83 有写回）反而是对的，
	// 所以前端在「新增」后能看到时间、在「编辑」后看到 1970-01-01。
	e := newEnv(t)
	const seededCtime, seededMtime = 1_700_000_000, 1_700_000_001
	id := seedMenu(t, e.st, 0, "旧名字", "video:submission#read", func(e2 *model.Menu) {
		e2.Path, e2.Icon, e2.Sort, e2.Ctime, e2.Mtime = "/old", "old", 5, seededCtime, seededMtime
	})
	e.st.log.reset()

	started := nowUnix()
	reply, err := saveMenuCall(t, e, saveMenuReq(9001, id, 0, "新名字", "/new", "new", "VIDEO:SUBMISSION#OFFLINE", 7, model.StateDisable))
	to := nowUnix()
	wantOK(t, reply, err, "更新菜单节点")
	wantOps(t, "更新菜单轨迹", e.ops(0), []string{
		"menu.FindOne:" + itoa(id),
		fmt.Sprintf("menu.Update:%d/新名字/video:submission#offline/2", id),
		"cache.Incr:op:menu:ver/2592000",
		"audit_index.Insert:menu.save/ok",
	})

	wantEQ(t, "更新应答", "menu_id", reply.Menu.MenuId, id)
	wantEQ(t, "更新应答", "name", reply.Menu.Name, "新名字")
	wantEQ(t, "更新应答", "path", reply.Menu.Path, "/new")
	wantEQ(t, "更新应答", "sort", reply.Menu.Sort, int32(7))
	wantEQ(t, "更新应答", "state", reply.Menu.State, model.StateDisable)
	wantEQ(t, "更新应答", "required_permission", reply.Menu.RequiredPermission, "video:submission#offline")
	// 缺口本体：两个时间戳都被吞成 0。
	wantEQ(t, "更新应答（当前实现吞掉时间戳）", "ctime", reply.Menu.Ctime, int64(0))
	wantEQ(t, "更新应答（当前实现吞掉时间戳）", "mtime", reply.Menu.Mtime, int64(0))

	// 库里却是好的：ctime 不动、mtime 已刷新。应答与库不一致这一结论两头都钉住。
	row := e.st.menu.get(id)
	if row == nil {
		t.Fatalf("op_menu 里没有节点 %d", id)
	}
	wantEQ(t, "落库", "ctime 保持不变", row.Ctime, seededCtime)
	wantTSWindow(t, "落库", "mtime 已刷新（应答里却是 0）", row.Mtime, started, to)
	if row.Mtime == reply.Menu.Mtime {
		t.Errorf("落库：mtime 与应答相同（%d），说明 Update 的回写口径变了", row.Mtime)
	}
}

func TestSaveMenu更新时父级指向自身直接拒(t *testing.T) {
	e := newEnv(t)
	id := seedMenu(t, e.st, 0, "自身父级", "", nil)
	e.st.log.reset()

	reply, err := saveMenuCall(t, e, saveMenuReq(9001, id, id, "自身父级", "", "", "", 0, 0))
	if reply != nil {
		t.Errorf("自引用：应答 = %+v, want nil", reply)
	}
	wantErrIs(t, "自引用", err, ErrMenuParentSelf)
	// 这条判定在 FindOne 之后、上溯之前，因此只有一读。
	wantOps(t, "自引用轨迹", e.ops(0), []string{"menu.FindOne:" + itoa(id)})
	wantNoOpsWith(t, "自引用不得写库", e.ops(0), "menu.Update")
	wantNoOpsWith(t, "自引用不得作废缓存", e.ops(0), "cache.Incr")
	wantNoOpsWith(t, "自引用不得留审计", e.ops(0), "audit_index.Insert")
}

func TestSaveMenu更新不存在的节点返回ErrMenuNotFound(t *testing.T) {
	e := newEnv(t)
	reply, err := saveMenuCall(t, e, saveMenuReq(9001, 404, 0, "不存在", "", "", "", 0, 0))
	if reply != nil {
		t.Errorf("更新缺失节点：应答 = %+v, want nil", reply)
	}
	wantErrIs(t, "更新缺失节点", err, ErrMenuNotFound)
	wantOps(t, "更新缺失节点轨迹", e.ops(0), []string{"menu.FindOne:404"})
	wantEQ(t, "失败后库里形态", "op_menu 行数", int64(e.st.menu.count()), int64(0))
}

func TestSaveMenu更新时父节点不存在直接拒(t *testing.T) {
	// 与新建路径的对照面：同样 parent_id=999，更新路径**会**查父级并拒。
	e := newEnv(t)
	id := seedMenu(t, e.st, 0, "正常节点", "", nil)
	e.st.log.reset()

	_, err := saveMenuCall(t, e, saveMenuReq(9001, id, 999, "正常节点", "", "", "", 0, 0))
	wantErrIs(t, "父级不存在", err, ErrMenuNotFound)
	wantOps(t, "父级不存在轨迹", e.ops(0), []string{
		"menu.FindOne:" + itoa(id),
		"menu.FindOne:999",
	})
}

func TestSaveMenu父链成环被拒且成环判定的上界会把自引用读成层级过深(t *testing.T) {
	// assertNoMenuCycle（menu.go:237-258）用 `i < maxMenuDepth` 限制上溯跳数。
	// 布一条 1<-2<-…<-9 的 9 级链，把 1 挂到 9 下面：走完 8 跳时 cur 正好回到 1，
	// 但循环计数已耗尽，于是**没有**执行最后的 seen 命中判定，
	// 报的是「层级超过 8」而不是 ErrMenuParentSelf。两种答复都算拒绝，
	// 钉住的是「上界一到就先判深度、再判环」这个真实次序。
	e := newEnv(t)
	ids := make([]int64, 0, 9)
	for i := 0; i < 9; i++ {
		var parent int64
		if i > 0 {
			parent = ids[i-1]
		}
		ids = append(ids, seedMenu(t, e.st, parent, fmt.Sprintf("节点%d", i+1), "", nil))
	}
	e.st.log.reset()

	reply, err := saveMenuCall(t, e, saveMenuReq(9001, ids[0], ids[8], "节点1", "", "", "", 0, 0))
	if reply != nil {
		t.Errorf("深链回挂：应答 = %+v, want nil", reply)
	}
	wantErr(t, "深链回挂", err)
	wantContains(t, "深链回挂报错（先撞深度上界）", errText(err), "deeper than 8 levels")
	if errors.Is(err, ErrMenuParentSelf) {
		t.Errorf("深链回挂：答复是 ErrMenuParentSelf，说明上界与环的判定次序变了，请同步 README")
	}
	// 上溯把父链每一跳都读了一遍（父级预检 + 8 跳），但没有任何写。
	wantCount(t, "深链上溯的读次数", e.ops(0), "menu.FindOne", 10)
	wantNoOpsWith(t, "深链回挂不得写库", e.ops(0), "menu.Update")
	wantNoOpsWith(t, "深链回挂不得作废缓存", e.ops(0), "cache.Incr")
	wantNoOpsWith(t, "深链回挂不得留审计", e.ops(0), "audit_index.Insert")
}

func TestSaveMenu审计写失败时节点与缓存失效已生效(t *testing.T) {
	// 生产现状（缺口，与 CreatePermission 同源）：menu.go:186-189 先 bump 版本、
	// 再写审计，且整段无事务。审计失败时调用方看到 error，但菜单**已经**改了、
	// 缓存**已经**作废，只剩「谁改的」这条留痕缺失。
	e := newEnv(t)
	e.st.audit.failWith("Insert", errAuditProbe)

	reply, err := saveMenuCall(t, e, saveMenuReq(9001, 0, 0, "审计失败节点", "", "", "", 0, 0))
	if reply != nil {
		t.Errorf("审计失败：应答 = %+v, want nil", reply)
	}
	wantErr(t, "审计失败", err)
	wantContains(t, "审计失败报错", errText(err), "write audit index failed")
	wantOps(t, "审计失败轨迹", e.ops(0), []string{
		"menu.Insert:0/审计失败节点",
		"cache.Incr:op:menu:ver/2592000",
		"audit_index.Insert:menu.save/ok",
	})
	if e.st.menu.count() != 1 {
		t.Errorf("审计失败后：op_menu 行数 = %d, want 1（业务写不回滚）", e.st.menu.count())
	}
	raw, ok := e.st.cache.rawOf(keyMenuVersion)
	if !ok {
		t.Fatalf("审计失败后：op:menu:ver 应已被 bump")
	}
	wantEQ(t, "审计失败后版本已推进", "op:menu:ver", raw, "1")
	if len(e.st.audit.all()) != 0 {
		t.Errorf("审计失败后：op_audit_index 行数 = %d, want 0", len(e.st.audit.all()))
	}
}

func TestSaveMenu的required_permission不校验权限点是否存在(t *testing.T) {
	// 生产现状（缺口）：menu.go:157-160 只过 parseRequiredPermission（格式），
	// 从不查 op_permission 有没有这一行。于是把 "video:submission#read" 误写成
	// "video:submission:reda" 会**保存成功**，但没有任何管理员能满足它，
	// 该菜单项对所有人永久不可见，且保存时零提示。
	e := newEnv(t)
	reply, err := saveMenuCall(t, e, saveMenuReq(9001, 0, 0, "拼错的菜单", "/typo", "", "video:submission#reda", 1, 0))
	wantOK(t, reply, err, "required_permission 指向不存在的权限点")
	// 全程没有任何一次 op_permission 读取 —— 这就是「不校验存在性」的证据。
	wantNoOpsWith(t, "存菜单不查权限点表", e.ops(0), "permission.")

	// 后果：给一个**确实**拥有 video:submission#read 的管理员读菜单，这一项被丢掉。
	adminID := seedAdminRow(t, e.st, "menu_typo_admin", nil)
	seedGrant(t, e.st, adminID, "内容查看", "video:submission#read")
	e.st.log.reset()
	menu, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, menu, err, "读回菜单")
	wantOps(t, "已授权管理员的可见集合", menuFields(menu.Items), []string{})
	wantNoOpsWith(t, "读菜单不得查权限点表", e.ops(0), "permission.")
}

// ============================================================================
// GetMenu
// ============================================================================

// menuFixtureAdmins 布一批菜单节点，返回可断言的可见集合。
//
// 节点选择覆盖全部四种分支：无 required（登录即可见）、required 已授权、
// required 未授权、state 隐藏、required 解析失败。
// 插入顺序刻意打乱 sort，让「ORDER BY parent_id, sort, menu_id」真的被测到。
func seedMenuFixture(t *testing.T, e *env) {
	t.Helper()
	seedMenu(t, e.st, 0, "坏配置节点", "read", func(m *model.Menu) { m.Sort = 50 })
	seedMenu(t, e.st, 0, "稿件下架", "video:submission#offline", func(m *model.Menu) { m.Sort = 20 })
	seedMenu(t, e.st, 0, "隐藏节点", "", func(m *model.Menu) {
		m.Sort = 40
		m.State = model.StateDisable
	})
	seedMenu(t, e.st, 0, "稿件查看", "video:submission#read", func(m *model.Menu) { m.Sort = 10 })
	seedMenu(t, e.st, 0, "工作台", "", func(m *model.Menu) { m.Sort = 30 })
	e.st.log.reset()
}

func TestGetMenu按角色并集过滤并缓存全量树(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdminRow(t, e.st, "menu_viewer", nil)
	seedGrant(t, e.st, adminID, "内容查看", "video:submission#read")
	seedMenuFixture(t, e)

	reply, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, reply, err, "读菜单")
	wantOps(t, "首次读菜单轨迹", e.ops(0), []string{
		// 权限快照：先探版本、再探快照，miss 才回源两张表并回填
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, adminID),
		"admin_user.FindOne:" + itoa(adminID),
		"role.LoadAdminGrants:" + itoa(adminID),
		"cache.Setex:" + rbacSnapshotKey(0, adminID) + "/60",
		// 菜单版本 key 首访时先 bump 出 1，再按版本取树
		"cache.Get:op:menu:ver",
		"cache.Incr:op:menu:ver/2592000",
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:1",
		"menu.ListAll",
		"cache.Setex:op:menu:1/300",
	})
	// 可见集合：工作台（无 required）+ 稿件查看（已授权）。
	// 稿件下架未授权、隐藏节点 state=2、坏配置节点解析失败被跳过。
	wantOps(t, "可见菜单（按 sort 升序）", menuFields(reply.Items), []string{
		"4/稿件查看/10",
		"5/工作台/30",
	})
	wantEQ(t, "读菜单应答", "ttl", reply.Ttl, int64(300))
}

func TestGetMenu第二次读命中两级缓存不再打库(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdminRow(t, e.st, "menu_cached", nil)
	seedGrant(t, e.st, adminID, "内容查看", "video:submission#read")
	seedMenuFixture(t, e)

	first, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, first, err, "第一次读菜单")
	e.st.log.reset()

	second, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, second, err, "第二次读菜单")
	wantOps(t, "第二次读菜单轨迹（全命中）", e.ops(0), []string{
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, adminID),
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:1",
	})
	wantOps(t, "两次可见集合相同", menuFields(second.Items), menuFields(first.Items))

	// 缓存里放的是**未过滤的全量树**（含隐藏与未授权节点）：
	// 这是「一份缓存服务所有管理员」的设计前提，也是它必须按版本作废的原因。
	raw, ok := e.st.cache.rawOf(menuTreeKey(1))
	if !ok {
		t.Fatalf("缓存里没有 %s", menuTreeKey(1))
	}
	wantContains(t, "菜单树缓存内容（含隐藏节点）", raw, "隐藏节点")
	wantContains(t, "菜单树缓存内容（含未授权节点）", raw, "稿件下架")
	wantNotContains(t, "菜单树缓存不得带口令列", raw, "password_hash")
}

func TestGetMenu的ttl取自配置而不是实现兜底值(t *testing.T) {
	// normalizeOptions 的缺省 MenuTTL 是 300（repository.go:36）。若实现把回传的
	// ttl 写成常量 300、或把 setJSON 的 ttl 写死，这条会红。
	e := newEnv(t, withCacheTTLs(45, 77, 111))
	adminID := seedAdminRow(t, e.st, "menu_ttl", nil)
	seedGrant(t, e.st, adminID, "内容查看", "video:submission#read")
	seedMenuFixture(t, e)

	reply, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, reply, err, "ttl 来源检查")
	wantOps(t, "下发的树缓存 TTL", e.ops(0), []string{
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, adminID),
		"admin_user.FindOne:" + itoa(adminID),
		"role.LoadAdminGrants:" + itoa(adminID),
		"cache.Setex:" + rbacSnapshotKey(0, adminID) + "/45",
		"cache.Get:op:menu:ver",
		"cache.Incr:op:menu:ver/2592000",
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:1",
		"menu.ListAll",
		"cache.Setex:op:menu:1/111",
	})
	wantEQ(t, "读菜单应答", "ttl（配置值）", reply.Ttl, int64(111))
	ttl, ok := e.st.cache.ttlOf(menuTreeKey(1))
	if !ok {
		t.Fatalf("缓存里没有 %s", menuTreeKey(1))
	}
	wantEQ(t, "树缓存实际 TTL", "op:menu:1", ttl, 111)
}

func TestGetMenu不同管理员共用同一份树缓存但可见集合不同(t *testing.T) {
	e := newEnv(t)
	viewer := seedAdminRow(t, e.st, "menu_only_read", nil)
	seedGrant(t, e.st, viewer, "内容查看", "video:submission#read")
	operator := seedAdminRow(t, e.st, "menu_only_offline", nil)
	seedGrant(t, e.st, operator, "内容操作", "video:submission#offline")
	seedMenuFixture(t, e)

	first, err := getMenuCall(t, e, getMenuReq(viewer, 0))
	wantOK(t, first, err, "只读管理员")
	e.st.log.reset()

	second, err := getMenuCall(t, e, getMenuReq(operator, 0))
	wantOK(t, second, err, "只可下架管理员")
	// 关键：第二个管理员**没有**触发 menu.ListAll（树缓存与管理员无关），
	// 只有自己的权限快照回源。
	wantOps(t, "第二个管理员轨迹", e.ops(0), []string{
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, operator),
		"admin_user.FindOne:" + itoa(operator),
		"role.LoadAdminGrants:" + itoa(operator),
		"cache.Setex:" + rbacSnapshotKey(0, operator) + "/60",
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:1",
	})
	wantOps(t, "只读管理员可见集合", menuFields(first.Items), []string{"4/稿件查看/10", "5/工作台/30"})
	wantOps(t, "下架管理员可见集合（同一份树、不同的过滤）", menuFields(second.Items),
		[]string{"2/稿件下架/20", "5/工作台/30"})
}

func TestGetMenu写后立即读到新树且旧版本槽不再被引用(t *testing.T) {
	// 写读成对：SaveMenu 只 Incr 版本、不删任何树槽；GetMenu 用新版本号取槽，
	// 于是必然 miss 并回源。旧槽留在缓存里等 TTL 自然过期（不清理是有意的：
	// 避免生产 Redis 上的 KEYS/SCAN）。
	e := newEnv(t)
	adminID := seedAdminRow(t, e.st, "menu_writer", nil)
	seedGrant(t, e.st, adminID, "内容查看", "video:submission#read")
	seedMenuFixture(t, e)

	before, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, before, err, "写前读")
	wantOps(t, "写前可见集合", menuFields(before.Items), []string{"4/稿件查看/10", "5/工作台/30"})
	e.st.log.reset()

	saved, err := saveMenuCall(t, e, saveMenuReq(9001, 0, 0, "新增项", "/new", "", "video:submission#read", 5, 0))
	wantOK(t, saved, err, "写")
	after, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, after, err, "写后读")
	wantOps(t, "写 + 写后读轨迹", e.ops(0), []string{
		"menu.Insert:0/新增项",
		"cache.Incr:op:menu:ver/2592000",
		"audit_index.Insert:menu.save/ok",
		// 权限快照仍命中（存菜单不动 RBAC 版本）
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, adminID),
		// 版本已是 2 → 换槽 → miss → 回源 → 回填
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:2",
		"menu.ListAll",
		"cache.Setex:op:menu:2/300",
	})
	wantOps(t, "写后可见集合（新增项按 sort=5 排在最前）", menuFields(after.Items),
		[]string{"6/新增项/5", "4/稿件查看/10", "5/工作台/30"})
	wantEQ(t, "旧版本槽仍在（靠 TTL 自然过期）", "op:menu:1 存在", e.st.cache.hasKey(menuTreeKey(1)), true)
}

func TestGetMenu的adminId非正数一律回退成操作者本人(t *testing.T) {
	// getmenulogic.go:29-32：`if adminID <= 0 { adminID = actor.AdminID }`。
	// 因此 repository/menu.go:106-108 的 `adminID <= 0 → ErrInvalidOperator`
	// 在 logic 入口上不可达 —— 传 -5 得到的是**自己**的菜单而不是报错。
	e := newEnv(t)
	adminID := seedAdminRow(t, e.st, "menu_self", nil)
	seedGrant(t, e.st, adminID, "内容查看", "video:submission#read")
	seedMenuFixture(t, e)

	self, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, self, err, "admin_id=0")
	e.st.log.reset()
	neg, err := getMenuCall(t, e, getMenuReq(adminID, -5))
	wantOK(t, neg, err, "admin_id=-5（回退而不是报错）")
	wantOps(t, "回退后的读库对象是操作者本人", e.ops(0), []string{
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, adminID),
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:1",
	})
	wantOps(t, "两条答复相同 ⇒ 确实取的是同一个人的菜单", menuFields(neg.Items), menuFields(self.Items))
}

func TestGetMenu允许代查他人菜单且读接口不写审计(t *testing.T) {
	// 代查本身是设计（注释见 getmenulogic.go:28），钉的是它**不留痕**：
	// 后台能悄悄看到别人的可见范围。路由侧有 AdminPermission 兜着（gateway/admin），
	// 因此这里只把「无审计」这一形态记进用例。
	e := newEnv(t)
	target := seedAdminRow(t, e.st, "menu_target", nil)
	seedGrant(t, e.st, target, "内容操作", "video:submission#offline")
	caller := seedAdminRow(t, e.st, "menu_caller", nil)
	seedMenuFixture(t, e)

	reply, err := getMenuCall(t, e, getMenuReq(caller, target))
	wantOK(t, reply, err, "代查他人菜单")
	wantOps(t, "代查的读库对象是 target 而不是 caller", e.ops(0), []string{
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, target),
		"admin_user.FindOne:" + itoa(target),
		"role.LoadAdminGrants:" + itoa(target),
		"cache.Setex:" + rbacSnapshotKey(0, target) + "/60",
		"cache.Get:op:menu:ver",
		"cache.Incr:op:menu:ver/2592000",
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:1",
		"menu.ListAll",
		"cache.Setex:op:menu:1/300",
	})
	wantOps(t, "看到的是 target 的可见集合", menuFields(reply.Items), []string{"2/稿件下架/20", "5/工作台/30"})
	wantNoOpsWith(t, "读菜单不得留审计", e.ops(0), "audit_index.Insert")
}

func TestGetMenu守卫拒绝后零依赖调用(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.GetMenuReq
	}{
		{"无操作者上下文", &rpc.GetMenuReq{AdminId: 1001}},
		{"operator_id 为 0 且未指定代查对象", getMenuReq(0, 0)},
		{"operator_id 为负且代查对象也是 0", getMenuReq(-3, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedMenuFixture(t, e)
			reply, err := getMenuCall(t, e, tc.in)
			if reply != nil {
				t.Errorf("%s：应答 = %+v, want nil", tc.name, reply)
			}
			wantErrIs(t, tc.name, err, ErrInvalidOperator)
			wantZeroOps(t, tc.name+"（守卫拒绝后不得查库）", e.ops(0))
		})
	}
}

func TestGetMenu账号非正常态时拒答且不碰菜单表(t *testing.T) {
	// 快照 state 不是 normal ⇒ ErrAdminDisabled（menu.go:113-115）。
	// 三条状态各测一遍：禁用(2) 与锁定(3) 都必须拒，正常(1) 必须过。
	cases := []struct {
		name      string
		state     int32
		wantSent  error
		wantItems []string
	}{
		{"正常账号", model.AdminStateNormal, nil, []string{"4/稿件查看/10", "5/工作台/30"}},
		{"禁用账号", model.AdminStateDisable, ErrAdminDisabled, nil},
		{"锁定账号", model.AdminStateLock, ErrAdminDisabled, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			adminID := seedAdminRow(t, e.st, "menu_state", func(u *model.AdminUser) { u.State = tc.state })
			seedGrant(t, e.st, adminID, "内容查看", "video:submission#read")
			seedMenuFixture(t, e)

			reply, err := getMenuCall(t, e, getMenuReq(adminID, 0))
			if tc.wantSent != nil {
				if reply != nil {
					t.Errorf("%s：应答 = %+v, want nil", tc.name, reply)
				}
				wantErrIs(t, tc.name, err, tc.wantSent)
				wantNoOpsWith(t, tc.name+"（不得触碰菜单表）", e.ops(0), "menu.")
				return
			}
			wantOK(t, reply, err, tc.name)
			wantOps(t, tc.name, menuFields(reply.Items), tc.wantItems)
		})
	}
}

func TestGetMenu管理员不存在返回ErrAdminNotFound(t *testing.T) {
	e := newEnv(t)
	seedMenuFixture(t, e)
	caller := seedAdminRow(t, e.st, "menu_caller2", nil)

	reply, err := getMenuCall(t, e, getMenuReq(caller, 9999))
	if reply != nil {
		t.Errorf("代查不存在的账号：应答 = %+v, want nil", reply)
	}
	wantErrIs(t, "代查不存在的账号", err, ErrAdminNotFound)
	wantOps(t, "缺失账号在读到授权前就被拒", e.ops(0), []string{
		"cache.Get:op:rbac:ver",
		"cache.Get:op:rbac:0:9999",
		"admin_user.FindOne:9999",
	})
	wantNoOpsWith(t, "账号不存在时不得回源授权表", e.ops(0), "role.LoadAdminGrants")
	wantNoOpsWith(t, "账号不存在时不得读菜单", e.ops(0), "menu.")
}

func TestGetMenu空菜单树也照样回填缓存并给出空列表(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdminRow(t, e.st, "menu_empty", nil)
	seedGrant(t, e.st, adminID, "内容查看", "video:submission#read")
	e.st.log.reset()

	reply, err := getMenuCall(t, e, getMenuReq(adminID, 0))
	wantOK(t, reply, err, "空菜单")
	wantOps(t, "空树轨迹", e.ops(0), []string{
		"cache.Get:op:rbac:ver",
		"cache.Get:" + rbacSnapshotKey(0, adminID),
		"admin_user.FindOne:" + itoa(adminID),
		"role.LoadAdminGrants:" + itoa(adminID),
		"cache.Setex:" + rbacSnapshotKey(0, adminID) + "/60",
		"cache.Get:op:menu:ver",
		"cache.Incr:op:menu:ver/2592000",
		"cache.Get:op:menu:ver",
		"cache.Get:op:menu:1",
		"menu.ListAll",
		"cache.Setex:op:menu:1/300",
	})
	if reply.Items == nil || len(reply.Items) != 0 {
		t.Errorf("空树：Items = %v, want 非 nil 空切片", reply.Items)
	}
}
