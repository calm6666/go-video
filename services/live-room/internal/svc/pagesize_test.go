package svc

import (
	"errors"
	"testing"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/model"
)

// 分页收敛是「列表查询必须带 LIMIT」这条硬约束的唯一执行点（AGENTS.md §5），
// 而它只依赖配置结构，不碰数据库，所以这里直接构造 ServiceContext 调用，
// 不给 DB/Cache 赋值——真接到 SQL 之前就该被拦住的入参，绝不该走到连接池。

func newSvcConf(maxList, defList, maxArea int32) *ServiceContext {
	return &ServiceContext{Config: config.Config{LiveRoom: config.LiveRoomConf{
		MaxListPageSize:     maxList,
		DefaultListPageSize: defList,
		MaxAreaPageSize:     maxArea,
	}}}
}

// TestPageSizeRejectsOversizedRequest 锁定「超限直接拒绝、不静默截断」：
// 截断会让调用方以为还有下一页，写出错误的分页循环。
func TestPageSizeRejectsOversizedRequest(t *testing.T) {
	s := newSvcConf(100, 20, 200)
	for _, req := range []int32{101, 200, 1 << 20} {
		got, err := s.PageSize(req)
		if !errors.Is(err, model.ErrPageSizeTooLarge) {
			t.Errorf("page_size=%d 应返回 ErrPageSizeTooLarge，实际 %v / %v", req, got, err)
		}
		if got != 0 {
			t.Errorf("拒绝时不得返回可用页大小，实际 %d", got)
		}
	}
	// 边界：正好等于上限必须放行。
	if got, err := s.PageSize(100); err != nil || got != 100 {
		t.Errorf("page_size=100（=上限）应放行，实际 %d / %v", got, err)
	}
}

// TestPageSizeFallsBackToDefault 非正数页大小取默认值，不能变成「无 LIMIT」。
func TestPageSizeFallsBackToDefault(t *testing.T) {
	s := newSvcConf(100, 20, 200)
	for _, req := range []int32{0, -1, -1 << 20} {
		got, err := s.PageSize(req)
		if err != nil {
			t.Errorf("page_size=%d 应回落默认值而不是报错：%v", req, err)
		}
		if got != 20 {
			t.Errorf("page_size=%d 应回落到默认 20，实际 %d", req, got)
		}
	}
}

// TestPageSizeGuardsAgainstUnusableConfig 配置被写成 0/负数时必须退到兜底常量：
// 「无上限」与「无默认值」都会让列表查询变成全表扫描。
func TestPageSizeGuardsAgainstUnusableConfig(t *testing.T) {
	cases := []struct {
		name             string
		maxList, defList int32
		wantDefault      int
		wantMax          int32
	}{
		{"max 配 0", 0, 20, 20, fallbackMaxListPageSize},
		{"max 配负数", -5, 20, 20, fallbackMaxListPageSize},
		{"default 配 0", 100, 0, int(fallbackDefaultListPageSize), 100},
		{"default 配负数", 100, -1, int(fallbackDefaultListPageSize), 100},
		{"default 大于 max 时按 max 收敛", 10, 999, 10, 10},
		{"两者都缺配", 0, 0, int(fallbackDefaultListPageSize), fallbackMaxListPageSize},
	}
	for _, c := range cases {
		s := newSvcConf(c.maxList, c.defList, 200)
		got, err := s.PageSize(0)
		if err != nil || got != c.wantDefault {
			t.Errorf("%s：默认页大小应为 %d，实际 %d / %v", c.name, c.wantDefault, got, err)
		}
		// 兜底上限生效：超一档必须拒。
		if _, err := s.PageSize(c.wantMax + 1); !errors.Is(err, model.ErrPageSizeTooLarge) {
			t.Errorf("%s：上限应为 %d，%d 却被放行（%v）", c.name, c.wantMax, c.wantMax+1, err)
		}
	}
}

// TestAreaPageSizeUsesItsOwnCeiling 分区是小表，上限比房间列表大，
// 但两者不得串用——房间列表页拿到 200 的额度就等于放宽了主列表扫描。
func TestAreaPageSizeUsesItsOwnCeiling(t *testing.T) {
	s := newSvcConf(100, 20, 200)
	if got, err := s.AreaPageSize(150); err != nil || got != 150 {
		t.Errorf("AreaPageSize(150) 应放行，实际 %d / %v", got, err)
	}
	if _, err := s.AreaPageSize(201); !errors.Is(err, model.ErrPageSizeTooLarge) {
		t.Errorf("AreaPageSize(201) 应被拒，实际 %v", err)
	}
	if _, err := s.PageSize(150); !errors.Is(err, model.ErrPageSizeTooLarge) {
		t.Errorf("房间列表借用了分区上限：PageSize(150) 应被拒，实际 %v", err)
	}
	// 缺配 max_area 时退到兜底，而不是退到「无限」。
	s2 := newSvcConf(100, 20, 0)
	if _, err := s2.AreaPageSize(fallbackMaxAreaPageSize + 1); !errors.Is(err, model.ErrPageSizeTooLarge) {
		t.Errorf("AreaPageSize 未用上兜底上限 %d：%v", fallbackMaxAreaPageSize, err)
	}
}
