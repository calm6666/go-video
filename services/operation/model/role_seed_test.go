package model

// 角色种子迁移（000005 / 000006_seed_op_role_grants*.sql）的一致性守卫。
//
// 这里同样只读文件、不连 MySQL（AGENTS.md §9）。它拦的都是「上线后只以 403 或空角色形式暴露、
// 编译期与单测都不报」的真实事故：
//   1. 域角色名由 `CONCAT('domain_', op_permission.domain)` 派生：域名一旦带连字符/大写/过长，
//      建出来的角色名就不满足 repository 的 rolePattern，运营在授权页面上挑不到它（静默零权限角色）；
//   2. 绑定语句若被改成硬编码 permission_id/role_id：两表主键都是 AUTO_INCREMENT，跨环境取值不同，
//      迁移会在某些库上把权限点挂到错误的角色上；
//   3. 在本迁移里补 `op_permission` 行会绕过 seed_permission_test.go——那个门禁只读 000004，
//      于是多出来的权限点没有任何路由要求、却能被授予；
//   4. 角色 INSERT 必须排在绑定 INSERT 之前，否则同一次执行里 JOIN 不到刚插入的角色，
//      而版本号已经记下，重跑不会补上；
//   5. 种子一旦开始写 `op_admin_role`/`op_admin_user`，就等于迁移替人做了组织决策；
//   6. 派生绑定只在被执行的那一刻生效：权限点种子（000004）后加的域/点，若没有一条**更晚版本号**
//      的派生绑定迁移，就永远不会有角色持有它——新库跑完所有迁移仍然 403。
//      scripts/migrate.ps1 按版本号跳过已执行迁移，所以这条只能靠文件名顺序静态判定。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	permSeedFile = "../../../deploy/migrations/operation/000004_seed_op_permission.sql"
	// rolePatternFile 是判定侧的字符集来源；本门禁与它必须同一口径。
	rolePatternFile = "../../../services/operation/internal/repository/role.go"
	seedDir         = "../../../deploy/migrations/operation"
)

// roleSeedFiles 是派生绑定迁移，**必须按版本号升序**书写：每一条都要能在「已跑过前面所有条」的库上
// 单独执行就收敛全部域与权限点，因此每条都自带完整的域角色 + 三段派生 SELECT。
// 在 000004 里新增权限点（尤其是新域）时，必须追加一条版本号更大的同类迁移并登记到这里，
// 否则 TestGrantsMigrationNeverTrailsPermissionSeed 会直接失败。
var roleSeedFiles = []string{
	"../../../deploy/migrations/operation/000005_seed_op_role_grants.sql",
	"../../../deploy/migrations/operation/000006_seed_op_role_grants_stage12.sql",
}

// domainRolePrefix 必须与迁移里的 CONCAT('domain_', ...) 一致：改名会让门禁与事实分叉。
const domainRolePrefix = "domain_"

var (
	// permissionPointRe 抓权限点种子的 (resource, action, domain) 三元组。
	permissionPointRe = regexp.MustCompile("(?m)^INSERT IGNORE INTO `op_permission` .*VALUES \\('([^']+)', '([^']+)', '([^']*)'")
	// seedTargetRe 抓每条 INSERT 的目标表，用于按语句归类。
	seedTargetRe = regexp.MustCompile("INSERT IGNORE INTO `([a-z_]+)`")
	// seededRoleTailRe 是 state=1、operator=0、ctime=0、mtime=0 的公共尾巴。
	seededRoleTailRe = regexp.MustCompile(`1, 0, 0, 0`)
	// seedRoleNameRe 与 repository.rolePattern 等价（小写字母开头，小写字母/数字/下划线，总长 2~32）。
	seedRoleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	// migrationVersionRe 抓迁移文件名的 6 位版本号。
	migrationVersionRe = regexp.MustCompile(`^(\d{6})_.*\.sql$`)
)

// statements 返回去掉行注释后的语句序列，保持原文顺序（顺序断言要用）。
func statements(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, chunk := range strings.Split(sqlOnly(readFile(t, path)), ";") {
		if s := strings.TrimSpace(chunk); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s 解析不到任何语句：注释剥离口径或文件本身已漂移", path)
	}
	return out
}

func TestRoleSeedGrantsEveryPermissionDomain(t *testing.T) {
	rows := permissionPointRe.FindAllStringSubmatch(readFile(t, permSeedFile), -1)
	if len(rows) == 0 {
		t.Fatal("权限点种子解析不到任何行：permissionPointRe 已失效")
	}
	domains := map[string]int{}
	for _, m := range rows {
		if m[3] == "" {
			t.Errorf("权限点 %s#%s 的 domain 为空：域角色按 domain 派生，空 domain 只会被 super_admin 覆盖，"+
				"等于这个后台入口没有对应的域角色", m[1], m[2])
			continue
		}
		domains[m[3]]++
	}
	for domain, n := range domains {
		if name := domainRolePrefix + domain; !seedRoleNameRe.MatchString(name) {
			t.Errorf("域角色名 %q（该域 %d 个权限点）不满足 repository 的 rolePattern，授权页面上建不出也选不到它", name, n)
		}
	}
	for _, seed := range roleSeedFiles {
		src := readFile(t, seed)
		if !strings.Contains(src, "CONCAT('"+domainRolePrefix+"', ") {
			t.Errorf("%s 缺少 %q 域角色派生语句：它必须自带完整派生，才能在已跑过前一条的库上单独收敛",
				filepath.Base(seed), domainRolePrefix)
		}
	}
	t.Logf("权限点 %d 行、派生出 %d 个域角色名，全部满足 rolePattern；派生绑定迁移 %d 条",
		len(rows), len(domains), len(roleSeedFiles))
}

// TestRepositoryRolePatternUnchanged：判定侧改了角色名规则时必须同步本门禁，否则会放行非法角色名。
func TestRepositoryRolePatternUnchanged(t *testing.T) {
	if !strings.Contains(readFile(t, rolePatternFile), "^[a-z][a-z0-9_]{1,31}$") {
		t.Fatal("repository 的 rolePattern 字面量已变化：请把 seedRoleNameRe 改成同一口径")
	}
}

func TestRoleSeedStructureIsDriftProof(t *testing.T) {
	for _, seed := range roleSeedFiles {
		checkSeedStructure(t, seed)
	}
}

// checkSeedStructure 对单条派生绑定迁移做结构判定（口径见文件头 1~5 条）。
func checkSeedStructure(t *testing.T, path string) {
	t.Helper()
	var roleStmts, bindStmts []string
	firstRoleAt, firstBindAt := -1, -1
	for i, s := range statements(t, path) {
		targets := seedTargetRe.FindAllStringSubmatch(s, -1)
		if len(targets) == 0 {
			t.Fatalf("%s 里出现非 INSERT 语句：%s", filepath.Base(path), firstLine(s))
		}
		for _, tb := range targets {
			switch tb[1] {
			case "op_role":
				roleStmts = append(roleStmts, s)
				if firstRoleAt < 0 {
					firstRoleAt = i
				}
			case "op_role_permission":
				bindStmts = append(bindStmts, s)
				if firstBindAt < 0 {
					firstBindAt = i
				}
			default:
				t.Errorf("%s 不得写 %s：op_permission 只能由 000004 登记（否则绕过与 routePermissions 的双向门禁），"+
					"而 op_admin_role/op_admin_user 是「谁拿哪个角色」的组织决策，不由迁移代劳", filepath.Base(path), tb[1])
			}
		}
	}
	if len(roleStmts) == 0 || len(bindStmts) == 0 {
		t.Fatalf("%s 结构漂移：op_role 语句 %d 条、op_role_permission 语句 %d 条",
			filepath.Base(path), len(roleStmts), len(bindStmts))
	}
	if firstRoleAt > firstBindAt {
		t.Errorf("%s：角色 INSERT 必须排在绑定 INSERT 之前，否则同次执行绑不到角色，建出一批零权限角色，"+
			"而版本号已记录、重跑不会补", filepath.Base(path))
	}
	for _, s := range bindStmts {
		if !strings.Contains(strings.ToUpper(s), "SELECT") || strings.Contains(strings.ToUpper(s), "VALUES") {
			t.Errorf("%s：绑定必须由 SELECT 现算 role_id/permission_id——两表主键是 AUTO_INCREMENT，写死数字会在别的库上挂错角色\n  %s",
				filepath.Base(path), firstLine(s))
		}
	}
	for _, s := range roleStmts {
		if !seededRoleTailRe.MatchString(s) {
			t.Errorf("%s：种子角色必须以 state=1、operator=0、ctime=mtime=0 建立，LoadAdminGrants 的 JOIN 带 r.state = 1，"+
				"state=2 等于建了个空壳\n  %s", filepath.Base(path), firstLine(s))
		}
	}
	// 三条派生绑定缺一不可：少了域角色绑定就没有任何角色持有新域权限点，
	// 少了 super_admin/readonly 绑定则该角色成了空壳。
	for _, want := range []string{"`domain` = SUBSTRING(r.`name`", "`action` = 'read'", "r.`name` = 'super_admin'"} {
		if !strings.Contains(readFile(t, path), want) {
			t.Errorf("%s 找不到派生条件 %q：三段派生（域角色/readonly/super_admin）必须齐全",
				filepath.Base(path), want)
		}
	}
	t.Logf("%s：op_role 语句 %d 条、op_role_permission 语句 %d 条，顺序与派生口径正确",
		filepath.Base(path), len(roleStmts), len(bindStmts))
}

// TestGrantsMigrationNeverTrailsPermissionSeed：权限点种子与派生绑定的**版本顺序**门禁。
//
// 派生绑定是 INSERT...SELECT，只在被执行的那一刻按当时的 op_permission 快照生效；
// scripts/migrate.ps1 又按文件名版本号跳过已执行迁移。所以「往 000004 追加了新域/新点、
// 却没补一条版本号更大的派生绑定迁移」会让新权限点在存量库上永远没有任何角色持有，
// 表现为对应后台入口对所有角色 403，而且新库跑完全部迁移也一样 403（没有角色建出来）。
// 这条顺序无法从单个文件读出，只能扫目录比较两者最大版本号。
func TestGrantsMigrationNeverTrailsPermissionSeed(t *testing.T) {
	entries, err := os.ReadDir(seedDir)
	if err != nil {
		t.Fatalf("读取迁移目录失败：%v", err)
	}
	maxPerm, maxGrants := 0, 0
	for _, e := range entries {
		m := migrationVersionRe.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		ver, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("迁移文件名版本号解析失败 %s: %v", e.Name(), err)
		}
		body := sqlOnly(readFile(t, filepath.Join(seedDir, e.Name())))
		if strings.Contains(body, "INSERT IGNORE INTO `op_permission`") && ver > maxPerm {
			maxPerm = ver
		}
		if strings.Contains(body, "INSERT IGNORE INTO `op_role`") && ver > maxGrants {
			maxGrants = ver
		}
	}
	if maxPerm == 0 || maxGrants == 0 {
		t.Fatalf("没解析到权限点种子或派生绑定迁移（perm=%d grants=%d），扫描口径已失效", maxPerm, maxGrants)
	}
	if maxPerm > maxGrants {
		t.Errorf("权限点种子的最新版本 %06d 晚于派生绑定迁移的最新版本 %06d："+
			"新登记的权限点不会被任何角色持有，对应后台入口 403。"+
			"请补一条版本号更大的 *_seed_op_role_grants*.sql（重复既有三段派生 SELECT）并登记进 roleSeedFiles",
			maxPerm, maxGrants)
	}
}

// TestRoleSeedFilesMatchRoleSeedList：迁移目录里所有建立 op_role 的种子文件都必须登记进
// roleSeedFiles。漏登记 = 该文件的结构完全不被本门禁检查（派生口径、顺序、目标表都能随手改坏）。
func TestRoleSeedFilesMatchRoleSeedList(t *testing.T) {
	entries, err := os.ReadDir(seedDir)
	if err != nil {
		t.Fatalf("读取迁移目录失败：%v", err)
	}
	listed := map[string]bool{}
	for _, p := range roleSeedFiles {
		listed[filepath.Base(p)] = true
		if _, err := os.Stat(p); err != nil {
			t.Errorf("roleSeedFiles 里的 %s 不存在：清单与仓库已分叉", filepath.Base(p))
		}
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || migrationVersionRe.FindStringSubmatch(e.Name()) == nil {
			continue
		}
		if !strings.Contains(sqlOnly(readFile(t, filepath.Join(seedDir, e.Name()))), "INSERT IGNORE INTO `op_role`") {
			continue
		}
		found++
		if !listed[e.Name()] {
			t.Errorf("迁移 %s 建立了 op_role 却没登记进 roleSeedFiles，结构门禁完全跳过它", e.Name())
		}
	}
	// 反向相等：扫不到任何文件说明扫描口径已失效（否则本门禁会静默永真）。
	if found != len(roleSeedFiles) {
		t.Errorf("目录里扫到 %d 条 op_role 种子迁移，roleSeedFiles 却有 %d 条：清单与仓库分叉或扫描已失效",
			found, len(roleSeedFiles))
	}
	t.Logf("op_role 种子迁移扫到 %d 条，与清单一致", found)
}

// TestReadonlyRoleIsNotAnEmptyShell：readonly 靠 action='read' 派生，没有这类点就是个零权限角色。
func TestReadonlyRoleIsNotAnEmptyShell(t *testing.T) {
	n := 0
	for _, m := range permissionPointRe.FindAllStringSubmatch(readFile(t, permSeedFile), -1) {
		if m[2] == "read" {
			n++
		}
	}
	if n == 0 {
		t.Fatal("权限点种子里没有任何 action='read' 的点：readonly 角色将是零权限角色")
	}
	t.Logf("readonly 覆盖 %d 个只读权限点", n)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
