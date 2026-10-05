package middleware

import (
	"os"
	"regexp"
	"testing"
)

// 本文件把权限表 routePermissions 与 op_permission 种子迁移做交叉校验。
//
// 为什么需要：中间件按「角色 → 权限点」判定，判定不中一律 403，而权限点必须先在
// op_permission 里存在才能被授予给角色。因此 routePermissions 每新增一条 (resource, action)，
// 种子迁移就必须同步多一行；否则新后台入口能过鉴权中间件却拿不到任何角色，表现为「上线即 403」，
// 且只有真实运营账号才会踩到。反向漂移（迁移里有表里没有的行）同样要报：
// 那说明有人删了路由却留着无人使用的权限点，会出现在授权页面上误导运营。
//
// 种子文件由脚本从 routePermissions 派生，本用例是派生结果的回归门禁。

var seedRowRe = regexp.MustCompile("(?m)^INSERT IGNORE INTO `op_permission`" +
	` .*VALUES \('([^']+)', '([^']+)'`)

const seedMigration = "../../../../deploy/migrations/operation/000004_seed_op_permission.sql"

func loadSeedPairs(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(seedMigration)
	if err != nil {
		t.Fatalf("读取权限点种子迁移失败：本用例以它作为已入库权限点的事实来源: %v", err)
	}
	rows := seedRowRe.FindAllStringSubmatch(string(src), -1)
	if len(rows) == 0 {
		t.Fatal("种子迁移里解析不到任何 INSERT IGNORE 行，解析已失效或文件被改写")
	}
	pairs := make(map[string]bool, len(rows))
	for _, m := range rows {
		key := m[1] + "|" + m[2]
		if pairs[key] {
			t.Errorf("种子迁移中权限点 %s 重复登记，(resource, action) 有唯一约束，重复行会被静默忽略", key)
		}
		pairs[key] = true
	}
	return pairs
}

func TestSeedCoversEveryRegisteredPermission(t *testing.T) {
	seed := loadSeedPairs(t)
	registered := map[string]bool{}
	for _, perm := range routePermissions {
		registered[perm.Resource+"|"+perm.Action] = true
	}
	for key := range registered {
		if !seed[key] {
			t.Errorf("权限点 %s 在 routePermissions 中要求，但种子迁移没有登记，对应后台入口对所有角色 403", key)
		}
	}
	for key := range seed {
		if !registered[key] {
			t.Errorf("种子迁移登记的权限点 %s 已不被任何路由要求，应从种子中删除", key)
		}
	}
	t.Logf("routePermissions 去重后 %d 个权限点，种子迁移 %d 行", len(registered), len(seed))
}
