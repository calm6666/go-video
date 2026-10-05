package logic

// permission_logic_test.go 覆盖 CreatePermission / ListPermissions。
//
// 要紧的结论依次是：
//   - 权限点的 (resource, action) 唯一性靠「先 FindByResourceAction 预检 + 库上
//     uniq_resource_action」两道，两道都要有结论：预检给出可识别的哨兵，
//     唯一键挡住并发窗口里挤进来的第二条；
//   - 入参校验必须**全部先于**任何 SQL/缓存写（守卫与校验拒绝时零依赖调用），
//     否则一次非法请求就会污染 RBAC/菜单版本、把既有缓存整体作废；
//   - 应答回传的 permission_id / ctime 必须与库里那一行一致（它们是后台列表页的
//     唯一定位依据，回错就等于前端指向了另一行）；
//   - 列表侧的「域筛选到底进没进 SQL、按什么顺序 ORDER BY、分页怎么切」
//     用整段 WHERE 文本 + 绑定实参的轨迹来钉，而不是只数返回了几行。
//
// 本轮核实到的生产现状（不改生产代码，逐条钉住并登记 README）：
//   - role.go:268-272 对 domain 只做 ToLower+TrimSpace，**没有长度与字符集校验**，
//     而 op_permission.domain 是 VARCHAR(64)（000003 迁移）→ 超长 domain 原样入库。
//   - role.go:276-287 的预检与 Insert 之间没有事务/唯一键兜底映射：并发下 1062
//     会**原文外传**，调用方拿不到 ErrPermissionExists 哨兵。
//   - role.go:265-267 的「含空格」判定不可达：isPermissionToken（role.go:348-365）
//     允许的字符集里就没有空格，且入参先过 TrimSpace。属死代码。
//   - role.go:288-292：缓存版本 bump 与审计写**不在同一事务**，审计失败时
//     权限点已落库、缓存已作废，只留下「业务已生效但无留痕」的形态。

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/operation/model"
	"go-video/services/operation/rpc"
)

// errPermProbe / errAuditProbe 是注入到替身里的探针错误。
var (
	errPermProbe  = errors.New("probe: permission write failed")
	errAuditProbe = errors.New("probe: audit write failed")
)

// createPermReq 组一个建权限点请求（opCtx 已带合法 operator_id）。
func createPermReq(operator int64, resource, action, domain, description string) *rpc.CreatePermissionReq {
	return &rpc.CreatePermissionReq{
		Ctx: opCtx(operator), Resource: resource, Action: action, Domain: domain, Description: description,
	}
}

// listPermsReq 组一个权限点列表请求。
func listPermsReq(operator int64, domain string, pn, ps int32) *rpc.ListPermissionsReq {
	return &rpc.ListPermissionsReq{Ctx: opCtx(operator), Domain: domain, Pn: pn, Ps: ps}
}

// seedPermRow 静默布一行权限点（显式给 domain，用于测按域筛选）。
func seedPermRow(t *testing.T, st *store, resource, action, domain string) int64 {
	t.Helper()
	return st.perm.put(&model.Permission{
		Resource: resource, Action: action, Domain: domain,
		Description: resource + "#" + action, Ctime: nowUnix(),
	}).PermissionID
}

// permFields 把权限点列表摘要成「域/资源#动作」，一次比对覆盖筛选与排序三列。
func permFields(rows []*rpc.PermissionItem) []string {
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.Domain+"/"+p.Resource+"#"+p.Action)
	}
	return out
}

// ============================================================================
// CreatePermission
// ============================================================================

func TestCreatePermission新建权限点应答与库里那一行一致(t *testing.T) {
	e := newEnv(t)
	started := nowUnix()
	// 入参刻意带大小写与首尾空白：归一化口径（ToLower+TrimSpace）本身就是结论，
	// 而 resource/action 归一后再查唯一键，才不会因大小写重复建点。
	reply, err := createPermCall(t, e, createPermReq(9001, "  VIDEO:Submission ", " Offline ", "", "  批量下架稿件  "))
	to := nowUnix()
	wantOK(t, reply, err, "新建 video:submission#offline")

	wantOps(t, "新建权限点轨迹", e.ops(0), []string{
		// 预检必须先于写入（少了它会把「已存在」放大成驱动 1062）
		"permission.FindByResourceAction:video:submission#offline",
		"permission.Insert:video:submission#offline",
		// 两个版本 key 都要 bump：新权限点可能改变菜单可见性
		"cache.Incr:op:rbac:ver/2592000",
		"cache.Incr:op:menu:ver/2592000",
		// 审计是最后一步：前面任何一步失败都还不该留下「已创建」的留痕
		"audit_index.Insert:permission.create/ok",
	})

	id := reply.Permission.PermissionId
	if id == 0 {
		t.Fatalf("应答未带回 permission_id：%+v", reply.Permission)
	}
	wantEQ(t, "新建应答", "resource", reply.Permission.Resource, "video:submission")
	wantEQ(t, "新建应答", "action", reply.Permission.Action, "offline")
	wantEQ(t, "新建应答", "domain（未显式给出时取资源前缀）", reply.Permission.Domain, "video")
	wantEQ(t, "新建应答", "description（首尾空白已去掉）", reply.Permission.Description, "批量下架稿件")
	wantTSWindow(t, "新建应答", "ctime", reply.Permission.Ctime, started, to)

	// 应答是内存投影，替身里的行才是「真的写了什么」：两者必须逐项一致。
	row := e.st.perm.rowOf(id)
	if row == nil {
		t.Fatalf("op_permission 里没有权限点 %d（应答却把它回传了）", id)
	}
	wantEQ(t, "落库", "permission_id", row.PermissionID, id)
	wantEQ(t, "落库", "resource", row.Resource, reply.Permission.Resource)
	wantEQ(t, "落库", "action", row.Action, reply.Permission.Action)
	wantEQ(t, "落库", "domain", row.Domain, reply.Permission.Domain)
	wantEQ(t, "落库", "description", row.Description, reply.Permission.Description)
	wantEQ(t, "落库（ctime 与应答必须同一秒）", "ctime", row.Ctime, reply.Permission.Ctime)
	if e.st.perm.count() != 1 {
		t.Errorf("落库：权限点总数 = %d, want 1", e.st.perm.count())
	}

	audit := e.st.audit.only(t)
	wantEQ(t, "建点审计", "action", audit.Action, "permission.create")
	wantEQ(t, "建点审计", "resource_type", audit.ResourceType, "admin_permission")
	wantEQ(t, "建点审计", "resource_id", audit.ResourceID, itoa(id))
	wantEQ(t, "建点审计", "result", audit.Result, model.AuditResultOK)
	wantEQ(t, "建点审计", "admin_id", audit.AdminID, int64(9001))
	wantHex32(t, "建点审计", "ip_hash", audit.IPHash)
}

func TestCreatePermission显式domain只做小写与去空白(t *testing.T) {
	e := newEnv(t)
	reply, err := createPermCall(t, e, createPermReq(9001, "*", "read", "  System  ", ""))
	wantOK(t, reply, err, "显式指定 domain 的通配读权限")
	wantOps(t, "通配建点轨迹", e.ops(0), []string{
		"permission.FindByResourceAction:*#read",
		"permission.Insert:*#read",
		"cache.Incr:op:rbac:ver/2592000",
		"cache.Incr:op:menu:ver/2592000",
		"audit_index.Insert:permission.create/ok",
	})
	wantEQ(t, "新建应答", "domain", reply.Permission.Domain, "system")
	wantContains(t, "缓存内容", e.st.cache.allText(), "op:rbac:ver")
}

func TestCreatePermission资源为通配符时domain退化为通配符(t *testing.T) {
	// resource="*" 时 SplitN("*", ":", 2)[0] 仍是 "*"，于是「超级权限」被归到
	// 名为 "*" 的域里，列表页按域筛选时它落在哪个域都不落在。这是派生规则的
	// 真实后果，钉住它而不是钉一个「应该是 system」的期望。
	e := newEnv(t)
	reply, err := createPermCall(t, e, createPermReq(9001, "*", "*", "", ""))
	wantOK(t, reply, err, "全域通配权限点")
	wantEQ(t, "新建应答", "domain（派生自资源前缀）", reply.Permission.Domain, "*")
	row := e.st.perm.rowOf(reply.Permission.PermissionId)
	if row == nil {
		t.Fatalf("op_permission 里没有权限点")
	}
	wantEQ(t, "落库", "domain", row.Domain, "*")
}

func TestCreatePermission入参校验全部先于任何SQL与缓存写(t *testing.T) {
	cases := []struct {
		name     string
		in       *rpc.CreatePermissionReq
		wantSent error
		needle   string
	}{
		{
			name:     "无操作者上下文",
			in:       &rpc.CreatePermissionReq{Resource: "video:submission", Action: "read"},
			wantSent: ErrInvalidOperator,
		},
		{
			name:     "operator_id 为 0",
			in:       createPermReq(0, "video:submission", "read", "", ""),
			wantSent: ErrInvalidOperator,
		},
		{
			name:     "operator_id 为负",
			in:       createPermReq(-7, "video:submission", "read", "", ""),
			wantSent: ErrInvalidOperator,
		},
		{
			name:     "resource 为空",
			in:       createPermReq(9001, "", "read", "", ""),
			wantSent: ErrPermissionInvalid,
		},
		{
			name:     "action 为空",
			in:       createPermReq(9001, "video:submission", "", "", ""),
			wantSent: ErrPermissionInvalid,
		},
		{
			name:     "resource 含非法字符",
			in:       createPermReq(9001, "video:submission!", "read", "", ""),
			wantSent: ErrPermissionInvalid,
		},
		{
			name: "resource 含大写以外的连字符",
			in:   createPermReq(9001, "video-submission", "read", "", ""),
			// "-" 不在 isPermissionToken 的允许集内（只允许 a-z0-9_*:）
			wantSent: ErrPermissionInvalid,
		},
		{
			name: "通配符混排被拒",
			in:   createPermReq(9001, "video:*:submission", "read", "", ""),
			// 只有 "*"、"*:x"、"x:*" 三种形态被接受
			wantSent: ErrPermissionInvalid,
		},
		{
			name:     "resource 超长（>64）",
			in:       createPermReq(9001, "v"+strings.Repeat("a", 64), "read", "", ""),
			wantSent: ErrPermissionInvalid,
		},
		{
			// 钉住死代码的**实际拦截者**：含空格的 resource 同样报 ErrPermissionInvalid，
			// 但走的是 isPermissionToken，role.go:265-267 那条显式空格判定永不执行。
			name:     "resource 内含空格",
			in:       createPermReq(9001, "video submission", "read", "", ""),
			wantSent: ErrPermissionInvalid,
		},
		{
			name:     "action 内含空格",
			in:       createPermReq(9001, "video:submission", "off line", "", ""),
			wantSent: ErrPermissionInvalid,
		},
		{
			name:   "description 超 255 字",
			in:     createPermReq(9001, "video:submission", "read", "", strings.Repeat("说", 256)),
			needle: "permission description too long (max 255)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			reply, err := createPermCall(t, e, tc.in)
			if reply != nil {
				t.Errorf("%s：应答 = %+v, want nil", tc.name, reply)
			}
			if tc.wantSent != nil {
				wantErrIs(t, tc.name, err, tc.wantSent)
			} else {
				wantErr(t, tc.name, err)
			}
			if tc.needle != "" {
				wantContains(t, tc.name, errText(err), tc.needle)
			}
			// 这一步是本用例的主断言：非法入参一条 SQL、一次缓存写都不许发。
			wantZeroOps(t, tc.name+"（守卫拒绝后不得触碰依赖）", e.ops(0))
		})
	}
}

func TestCreatePermission描述255字通过而256字被拒(t *testing.T) {
	// 长度按**字**而不是字节：255 个汉字（765 字节）必须过，256 个必须拒。
	// 用 len(description) 实现 would 在这条上直接红。
	e := newEnv(t)
	_, err := createPermCall(t, e, createPermReq(9001, "video:submission", "read", "", strings.Repeat("说", 255)))
	wantNoErr(t, "255 个汉字的描述", err)
	wantCount(t, "长描述请求", e.ops(0), "permission.Insert", 1)

	e2 := newEnv(t)
	_, err = createPermCall(t, e2, createPermReq(9001, "video:submission", "read", "", strings.Repeat("说", 256)))
	wantErr(t, "256 个汉字的描述", err)
	wantContains(t, "超长报错", errText(err), "max 255")
	wantZeroOps(t, "256 字描述（校验在 SQL 之前）", e2.ops(0))
}

func TestCreatePermissiondomain无长度校验超长原样入库(t *testing.T) {
	// 生产现状：description 有 255 字上限，domain 却**完全没有**长度与字符集校验，
	// 而 deploy/migrations/operation/000003 里 op_permission.domain 是 VARCHAR(64)。
	// 真实 MySQL 严格模式下这条 INSERT 会报 1406（Data too long），
	// 而本仓库的列宽约束在替身里不可见 ⇒ 记为缺口，见 README。
	// 这里钉的是「实现确实原样收下并落库」这一半：换任何一版加了校验的实现，
	// 本用例都会红，从而逼着改动同步更新登记。
	e := newEnv(t)
	long := strings.Repeat("d", 100)
	reply, err := createPermCall(t, e, createPermReq(9001, "video:submission", "read", long, ""))
	wantOK(t, reply, err, "100 字的 domain")
	wantEQ(t, "新建应答", "domain 长度", int64(len([]rune(reply.Permission.Domain))), int64(100))
	row := e.st.perm.rowOf(reply.Permission.PermissionId)
	if row == nil {
		t.Fatalf("op_permission 里没有权限点")
	}
	wantEQ(t, "落库", "domain 原样入库", row.Domain, long)

	// 对照组：同一份实现里 description 的 255 字上限确实生效——
	// 缺校验不是「所有字段都没校验」，而是只有 domain 漏了。
	e2 := newEnv(t)
	_, err = createPermCall(t, e2, createPermReq(9001, "video:submission", "read", "video", strings.Repeat("x", 256)))
	wantErr(t, "同一请求换成超长 description", err)
	wantZeroOps(t, "超长 description", e2.ops(0))
}

func TestCreatePermission重复权限点返回哨兵且零副作用(t *testing.T) {
	e := newEnv(t)
	existing := seedPermRow(t, e.st, "video:submission", "offline", "video")

	reply, err := createPermCall(t, e, createPermReq(9001, "video:submission", "offline", "video", "重复建点"))
	if reply != nil {
		t.Errorf("重复建点：应答 = %+v, want nil", reply)
	}
	wantErrIs(t, "重复建点", err, ErrPermissionExists)
	wantContains(t, "重复建点报错", errText(err), "already exists")
	// 只有预检那一次读：不写权限点、不作废缓存、不留审计。
	wantOps(t, "重复建点轨迹", e.ops(0), []string{
		"permission.FindByResourceAction:video:submission#offline",
	})
	wantEQ(t, "重复建点（既有行未被改动）", "permission_id", e.st.perm.rowOf(existing).PermissionID, existing)
	if e.st.perm.count() != 1 {
		t.Errorf("重复建点：权限点总数 = %d, want 1", e.st.perm.count())
	}
	if len(e.st.audit.all()) != 0 {
		t.Errorf("重复建点：不应留下任何审计行，实际 %d 条", len(e.st.audit.all()))
	}
}

func TestCreatePermission并发唯一冲突原文外传而非可识别哨兵(t *testing.T) {
	// 生产现状（缺口）：预检与 INSERT 之间的并发窗口里，驱动返回的 1062
	// 被 role.go:284-286 **原样外传**，既不映射成 ErrPermissionExists，
	// 也不像 op_admin_user/op_config 那样用 RowsAffected 自判冲突。
	// 于是 gateway/admin 侧「权限点已存在」的友好提示在并发下退化成一个 5xx。
	e := newEnv(t)
	e.st.perm.failOn("Insert", 1, dupErr("op_permission.uniq_resource_action", "video:submission-offline"))

	reply, err := createPermCall(t, e, createPermReq(9001, "video:submission", "offline", "", ""))
	if reply != nil {
		t.Errorf("并发冲突：应答 = %+v, want nil", reply)
	}
	wantErr(t, "并发冲突", err)
	wantContains(t, "并发冲突报错（驱动原文外传）", errText(err), "Error 1062")
	wantContains(t, "并发冲突报错（含冲突值与键名）", errText(err), "video:submission-offline")
	wantContains(t, "并发冲突报错（含键名）", errText(err), "op_permission.uniq_resource_action")
	// 关键：**没有**被归一成哨兵 —— 调用方无法用 errors.Is 判定「已存在」。
	if errors.Is(err, ErrPermissionExists) {
		t.Errorf("并发冲突：当前实现把 1062 归一成了 ErrPermissionExists，" +
			"README 登记的缺口已修，请把这条用例改成正向断言并删除登记")
	}
	wantOps(t, "并发冲突轨迹", e.ops(0), []string{
		"permission.FindByResourceAction:video:submission#offline",
		"permission.Insert:video:submission#offline",
	})
	// 失败后库里形态：权限点没落库、缓存版本没被 bump、审计零条。
	if e.st.perm.count() != 0 {
		t.Errorf("并发冲突后：op_permission 行数 = %d, want 0", e.st.perm.count())
	}
	wantNoOpsWith(t, "并发冲突后不得作废缓存", e.ops(0), "cache.Incr")
	if _, ok := e.st.cache.rawOf(keyRBACVersion); ok {
		t.Errorf("并发冲突后：op:rbac:ver 不应存在")
	}
}

func TestCreatePermission写入失败原文外传且不碰缓存与审计(t *testing.T) {
	e := newEnv(t)
	e.st.perm.failWith("Insert", errPermProbe)
	_, err := createPermCall(t, e, createPermReq(9001, "video:submission", "offline", "", ""))
	wantErr(t, "权限点写入失败", err)
	// 探针错误**原样**外传（没有被包装掉），logic 只 Errorf 一轮。
	if !errors.Is(err, errPermProbe) {
		t.Errorf("写入失败：错误 = %v, want errors.Is(%v)", err, errPermProbe)
	}
	wantOps(t, "写入失败轨迹", e.ops(0), []string{
		"permission.FindByResourceAction:video:submission#offline",
		"permission.Insert:video:submission#offline",
	})
}

func TestCreatePermission审计写失败时权限点与缓存失效已生效(t *testing.T) {
	// 生产现状（缺口）：CreatePermission 全程无事务。审计写是最后一步，
	// 它失败时错误会上抛（调用方看到失败），但权限点**已经落库**、
	// RBAC 与菜单版本**已经作废**，于是出现「业务已生效 + 应答报错 + 无留痕」。
	// 运维只能靠 logx.Errorf 的 trace_id 补偿（audit.go:124-134）。
	e := newEnv(t)
	e.st.audit.failWith("Insert", errAuditProbe)

	reply, err := createPermCall(t, e, createPermReq(9001, "video:submission", "offline", "", ""))
	if reply != nil {
		t.Errorf("审计失败：应答 = %+v, want nil（审计失败必须让调用方知道）", reply)
	}
	wantErr(t, "审计失败", err)
	wantContains(t, "审计失败报错", errText(err), "write audit index failed")
	if !errors.Is(err, errAuditProbe) {
		t.Errorf("审计失败：错误链里丢了探针 %v → %v", errAuditProbe, err)
	}
	wantOps(t, "审计失败轨迹（业务写与版本 bump 都已发生）", e.ops(0), []string{
		"permission.FindByResourceAction:video:submission#offline",
		"permission.Insert:video:submission#offline",
		"cache.Incr:op:rbac:ver/2592000",
		"cache.Incr:op:menu:ver/2592000",
		"audit_index.Insert:permission.create/ok",
	})

	// 残留形态逐条回读：权限点在、版本已推进、审计不在。
	if e.st.perm.count() != 1 {
		t.Errorf("审计失败后：op_permission 行数 = %d, want 1（业务写不回滚）", e.st.perm.count())
	}
	for _, k := range []string{keyRBACVersion, keyMenuVersion} {
		raw, ok := e.st.cache.rawOf(k)
		if !ok {
			t.Errorf("审计失败后：缓存版本 %s 已被 bump，不该缺失", k)
			continue
		}
		wantEQ(t, "审计失败后版本已推进", "op:…ver", raw, "1")
	}
	if len(e.st.audit.all()) != 0 {
		t.Errorf("审计失败后：op_audit_index 行数 = %d, want 0", len(e.st.audit.all()))
	}
}

// ============================================================================
// ListPermissions
// ============================================================================

// seedPermSet 布一批权限点，**插入顺序刻意与 ORDER BY 结果不同**，
// 这样「排序是库做的而不是 map 遍历顺序」才算真被钉住。
func seedPermSet(t *testing.T, st *store) {
	t.Helper()
	seedPermRow(t, st, "video:submission", "read", "video")
	seedPermRow(t, st, "video:submission", "offline", "video")
	seedPermRow(t, st, "rights:window", "expire", "rights")
	seedPermRow(t, st, "catalog:episode", "read", "catalog")
	st.log.reset()
}

func TestListPermissions守卫拒绝后零依赖调用(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListPermissionsReq
	}{
		{"无操作者上下文", &rpc.ListPermissionsReq{Domain: "video"}},
		{"operator_id 为 0", listPermsReq(0, "video", 1, 20)},
		{"operator_id 为负", listPermsReq(-1, "video", 1, 20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedPermSet(t, e.st)
			reply, err := listPermsCall(t, e, tc.in)
			if reply != nil {
				t.Errorf("%s：应答 = %+v, want nil", tc.name, reply)
			}
			wantErrIs(t, tc.name, err, ErrInvalidOperator)
			wantZeroOps(t, tc.name+"（守卫拒绝后不得查库）", e.ops(0))
		})
	}
}

func TestListPermissions域筛选真的进了SQL(t *testing.T) {
	e := newEnv(t)
	seedPermSet(t, e.st)

	reply, err := listPermsCall(t, e, listPermsReq(9001, "video", 1, 20))
	wantOK(t, reply, err, "按域筛选 video")
	// WHERE 文本 + 绑定实参 + 分页三件事一次性精确比对：
	// 只断言「返回了 2 行」的话，把 domain 条件写死成 LIKE '%video' 也能过。
	wantOps(t, "域筛选轨迹", e.ops(0), []string{
		"permission.List:WHERE 1 = 1 AND domain = ?[video]/1/20",
	})
	wantEQ(t, "域筛选", "total", reply.Total, int64(2))
	wantOps(t, "域筛选结果（域/资源#动作）", permFields(reply.Items), []string{
		"video/video:submission#offline",
		"video/video:submission#read",
	})
}

func TestListPermissions域参数只去空白不转小写(t *testing.T) {
	// role.go:311 只做 TrimSpace。传 "  VIDEO  " 时**绑定实参仍是 VIDEO**，
	// 能查到行完全依赖 op_permission 整表的 utf8mb4_unicode_ci 排序规则
	// （deploy/migrations/operation/000001 起就是 ci）。
	// 换任何 ci→cs 的排序规则改动，这一条会红，所以它同时也是排序规则依赖的哨兵。
	e := newEnv(t)
	seedPermSet(t, e.st)
	reply, err := listPermsCall(t, e, listPermsReq(9001, "  VIDEO  ", 1, 20))
	wantOK(t, reply, err, "大小写混合 + 首尾空白的域")
	wantOps(t, "域参数归一轨迹", e.ops(0), []string{
		"permission.List:WHERE 1 = 1 AND domain = ?[VIDEO]/1/20",
	})
	wantEQ(t, "域筛选（ci 等值）", "total", reply.Total, int64(2))
}

func TestListPermissions空域不过滤并整表排序(t *testing.T) {
	e := newEnv(t)
	seedPermSet(t, e.st)
	reply, err := listPermsCall(t, e, listPermsReq(9001, "", 1, 20))
	wantOK(t, reply, err, "全量权限点")
	wantOps(t, "无域筛选轨迹", e.ops(0), []string{
		// domain 为空时 WHERE 里连占位符都不该出现（不是传空串去匹配）
		"permission.List:WHERE 1 = 1[]/1/20",
	})
	wantEQ(t, "全量", "total", reply.Total, int64(4))
	// ORDER BY domain, resource, action：与插入顺序（video,video,rights,catalog）不同。
	wantOps(t, "全量排序", permFields(reply.Items), []string{
		"catalog/catalog:episode#read",
		"rights/rights:window#expire",
		"video/video:submission#offline",
		"video/video:submission#read",
	})
}

func TestListPermissions分页钳制与翻页切片(t *testing.T) {
	cases := []struct {
		name      string
		pn, ps    int32
		wantTrace string
		wantFirst []string
		wantTotal int64
	}{
		{
			name: "第 1 页每页 2 条", pn: 1, ps: 2,
			wantTrace: "permission.List:WHERE 1 = 1[]/1/2",
			wantFirst: []string{"catalog/catalog:episode#read", "rights/rights:window#expire"},
			wantTotal: 4,
		},
		{
			name: "第 2 页每页 2 条", pn: 2, ps: 2,
			wantTrace: "permission.List:WHERE 1 = 1[]/2/2",
			wantFirst: []string{"video/video:submission#offline", "video/video:submission#read"},
			wantTotal: 4,
		},
		{
			name: "pn 为 0 退化为 1", pn: 0, ps: 2,
			wantTrace: "permission.List:WHERE 1 = 1[]/1/2",
			wantFirst: []string{"catalog/catalog:episode#read", "rights/rights:window#expire"},
			wantTotal: 4,
		},
		{
			name: "pn 为负退化为 1", pn: -5, ps: 2,
			wantTrace: "permission.List:WHERE 1 = 1[]/1/2",
			wantFirst: []string{"catalog/catalog:episode#read", "rights/rights:window#expire"},
			wantTotal: 4,
		},
		{
			name: "ps 为 0 退化为 20", pn: 1, ps: 0,
			wantTrace: "permission.List:WHERE 1 = 1[]/1/20",
			wantFirst: []string{
				"catalog/catalog:episode#read", "rights/rights:window#expire",
				"video/video:submission#offline", "video/video:submission#read",
			},
			wantTotal: 4,
		},
		{
			name: "ps 为负退化为 20", pn: 1, ps: -3,
			wantTrace: "permission.List:WHERE 1 = 1[]/1/20",
			wantFirst: []string{
				"catalog/catalog:episode#read", "rights/rights:window#expire",
				"video/video:submission#offline", "video/video:submission#read",
			},
			wantTotal: 4,
		},
		{
			name: "ps 超上限钳到 100", pn: 1, ps: 5000,
			wantTrace: "permission.List:WHERE 1 = 1[]/1/100",
			wantFirst: []string{
				"catalog/catalog:episode#read", "rights/rights:window#expire",
				"video/video:submission#offline", "video/video:submission#read",
			},
			wantTotal: 4,
		},
		{
			name: "ps 恰为 100 不钳", pn: 1, ps: 100,
			wantTrace: "permission.List:WHERE 1 = 1[]/1/100",
			wantFirst: []string{
				"catalog/catalog:episode#read", "rights/rights:window#expire",
				"video/video:submission#offline", "video/video:submission#read",
			},
			wantTotal: 4,
		},
		{
			name: "越界页返回空集但 total 真实", pn: 9, ps: 20,
			wantTrace: "permission.List:WHERE 1 = 1[]/9/20",
			wantFirst: nil,
			wantTotal: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedPermSet(t, e.st)
			reply, err := listPermsCall(t, e, listPermsReq(9001, "", tc.pn, tc.ps))
			wantOK(t, reply, err, tc.name)
			// 钳制口径（pn<=0→1、ps<=0→20、ps>100→100）必须**在进 model 之前**定死：
			// 轨迹里的 pn/ps 就是最终发给 SQL 的 LIMIT/OFFSET 依据。
			wantOps(t, tc.name+"轨迹", e.ops(0), []string{tc.wantTrace})
			wantEQ(t, tc.name, "total", reply.Total, tc.wantTotal)
			wantOps(t, tc.name+"结果", permFields(reply.Items), nilOrStrings(tc.wantFirst))
		})
	}
}

// nilOrStrings 把「期望为空」写成 nil 也能比对（Items 恒为非 nil 空切片）。
func nilOrStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func TestListPermissions读接口不写缓存不打缓存(t *testing.T) {
	// 权限点列表是**每次直查库**的：没有缓存槽、也就没有「新增权限点后列表页
	// 还要等 N 秒」的窗口。这条断言的意义在于：CreatePermission 里那次
	// bumpMenuVersion 只服务于菜单可见性，不承担列表失效职责。
	e := newEnv(t)
	seedPermSet(t, e.st)
	_, err := listPermsCall(t, e, listPermsReq(9001, "video", 1, 20))
	wantNoErr(t, "第一次列表", err)
	_, err = listPermsCall(t, e, listPermsReq(9001, "video", 1, 20))
	wantNoErr(t, "第二次列表", err)
	wantOps(t, "两次列表都是直查", e.ops(0), []string{
		"permission.List:WHERE 1 = 1 AND domain = ?[video]/1/20",
		"permission.List:WHERE 1 = 1 AND domain = ?[video]/1/20",
	})
	wantNoOpsWith(t, "权限列表不得触碰缓存", e.ops(0), "cache.")
}

func TestListPermissions投影不回传敏感列(t *testing.T) {
	// PermissionItem 只有 6 个字段（rpc/operation.pb.go:1591-1600）。
	// 这里钉的是「Description 逐字回传」与「Items 恒为非 nil 切片」两条：
	// 前者是后台列表页的说明列，后者让前端不必判空。
	e := newEnv(t)
	e.st.perm.put(&model.Permission{
		Resource: "video:submission", Action: "read", Domain: "video",
		Description: "查看稿件", Ctime: 1_700_000_000,
	})
	e.st.log.reset()

	reply, err := listPermsCall(t, e, listPermsReq(9001, "", 1, 20))
	wantOK(t, reply, err, "投影检查")
	if reply.Items == nil {
		t.Fatalf("Items = nil, want 非 nil 切片（空结果也要给 []）")
	}
	if len(reply.Items) != 1 {
		t.Fatalf("Items 长度 = %d, want 1", len(reply.Items))
	}
	wantEQ(t, "投影", "description", reply.Items[0].Description, "查看稿件")
	wantEQ(t, "投影", "ctime 原样回传（不做秒->毫秒换算）", reply.Items[0].Ctime, int64(1_700_000_000))
	wantEQ(t, "投影", "permission_id", reply.Items[0].PermissionId, int64(1))

	// 空结果：Items 仍非 nil、total 为 0。
	e2 := newEnv(t)
	reply2, err := listPermsCall(t, e2, listPermsReq(9001, "nope", 1, 20))
	wantOK(t, reply2, err, "空域列表")
	wantEQ(t, "空结果", "total", reply2.Total, int64(0))
	if reply2.Items == nil || len(reply2.Items) != 0 {
		t.Errorf("空结果：Items = %v, want 非 nil 空切片", reply2.Items)
	}
}

func TestListPermissions库错误原文外传(t *testing.T) {
	e := newEnv(t)
	seedPermSet(t, e.st)
	e.st.perm.failWith("List", errPermProbe)
	reply, err := listPermsCall(t, e, listPermsReq(9001, "video", 1, 20))
	if reply != nil {
		t.Errorf("库错误：应答 = %+v, want nil", reply)
	}
	if !errors.Is(err, errPermProbe) {
		t.Errorf("库错误：错误 = %v, want errors.Is(%v)", err, errPermProbe)
	}
	wantOps(t, "库错误轨迹", e.ops(0), []string{
		"permission.List:WHERE 1 = 1 AND domain = ?[video]/1/20",
	})
}
