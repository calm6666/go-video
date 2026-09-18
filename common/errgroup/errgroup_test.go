package errgroup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// abc 用于触发 nil 指针 panic。
type abc struct {
	cba int
}

// sleep1s 睡眠 1 秒后返回，用于验证并发限制。
func sleep1s() error {
	time.Sleep(time.Second)
	return nil
}

// TestNormal 验证无错误并发任务的正常聚合。
func TestNormal(t *testing.T) {
	var (
		abcs = make(map[int]*abc)
		g    Group
		err  error
	)
	for i := 0; i < 10; i++ {
		abcs[i] = &abc{cba: i}
	}
	g.Go(func() error {
		abcs[1].cba++
		return nil
	})
	g.Go(func() error {
		abcs[2].cba++
		return nil
	})
	if err = g.Wait(); err != nil {
		t.Fatalf("expect nil err, got %v", err)
	}
	if abcs[1].cba != 2 || abcs[2].cba != 3 {
		t.Fatalf("unexpected abcs: %+v", abcs)
	}
}

// TestGOMAXPROCS 验证并发数限制：无限制时 4 个 1s 任务约 1s，限制为 2 时约 2s。
func TestGOMAXPROCS(t *testing.T) {
	// 没有并发数限制
	g := Group{}
	now := time.Now()
	g.Go(sleep1s)
	g.Go(sleep1s)
	g.Go(sleep1s)
	g.Go(sleep1s)
	g.Wait()
	sec := math.Round(time.Since(now).Seconds())
	if sec != 1 {
		t.Fatalf("expect ~1s without GOMAXPROCS, got %ds", int(sec))
	}
	// 限制并发数为 2
	g2 := Group{}
	g2.GOMAXPROCS(2)
	now = time.Now()
	g2.Go(sleep1s)
	g2.Go(sleep1s)
	g2.Go(sleep1s)
	g2.Go(sleep1s)
	g2.Wait()
	sec = math.Round(time.Since(now).Seconds())
	if sec != 2 {
		t.Fatalf("expect ~2s with GOMAXPROCS(2), got %ds", int(sec))
	}
	// context canceled：首个错误应取消派生 Context
	var canceled bool
	g3, ctx := WithContext(context.Background())
	g3.GOMAXPROCS(2)
	g3.Go(func() error {
		return fmt.Errorf("error for testing errgroup context")
	})
	g3.Go(func() error {
		time.Sleep(time.Second)
		select {
		case <-ctx.Done():
			canceled = true
		default:
		}
		return nil
	})
	g3.Wait()
	if !canceled {
		t.Fatal("expect ctx to be canceled on first error")
	}
}

// TestRecover 验证 nil 指针 panic 被恢复并作为 error 返回。
func TestRecover(t *testing.T) {
	var (
		abcs = make(map[int]*abc)
		g    Group
	)
	g.Go(func() error {
		abcs[1].cba++ // abcs[1] 为 nil，触发 panic
		return nil
	})
	g.Go(func() error {
		abcs[2].cba++ // abcs[2] 为 nil，触发 panic
		return nil
	})
	err := g.Wait()
	if err == nil {
		t.Fatal("expect panic recovered as error, got nil")
	}
	if !strings.Contains(err.Error(), "panic recovered") {
		t.Fatalf("expect error contains 'panic recovered', got %v", err)
	}
}

// TestRecover2 验证显式 panic 被恢复并作为 error 返回。
func TestRecover2(t *testing.T) {
	var g Group
	g.Go(func() error {
		panic("2233")
	})
	err := g.Wait()
	if err == nil {
		t.Fatal("expect panic recovered as error, got nil")
	}
	if !strings.Contains(err.Error(), "2233") {
		t.Fatalf("expect error contains '2233', got %v", err)
	}
}

// TestZeroGroup 验证零值 Group 的 Wait 返回首个非 nil 错误。
func TestZeroGroup(t *testing.T) {
	err1 := errors.New("errgroup_test: 1")
	err2 := errors.New("errgroup_test: 2")

	cases := []struct {
		errs []error
	}{
		{errs: []error{}},
		{errs: []error{nil}},
		{errs: []error{err1}},
		{errs: []error{err1, nil}},
		{errs: []error{err1, nil, err2}},
	}

	for _, tc := range cases {
		var g Group

		var firstErr error
		for i, err := range tc.errs {
			err := err
			g.Go(func() error { return err })

			if firstErr == nil && err != nil {
				firstErr = err
			}

			if gErr := g.Wait(); gErr != firstErr {
				t.Errorf("after g.Go(func() error { return err }) for err in %v\n"+
					"g.Wait() = %v; want %v", tc.errs[:i+1], gErr, firstErr)
			}
		}
	}
}

// TestWithContext 验证 WithContext 派生的 Context 在 Wait 后被取消。
func TestWithContext(t *testing.T) {
	errDoom := errors.New("group_test: doomed")

	cases := []struct {
		errs []error
		want error
	}{
		{want: nil},
		{errs: []error{nil}, want: nil},
		{errs: []error{errDoom}, want: errDoom},
		{errs: []error{errDoom, nil}, want: errDoom},
	}

	for _, tc := range cases {
		g, ctx := WithContext(context.Background())

		for _, err := range tc.errs {
			err := err
			g.Go(func() error { return err })
		}

		if err := g.Wait(); err != tc.want {
			t.Errorf("g.Wait() = %v; want %v", err, tc.want)
		}

		select {
		case <-ctx.Done():
		default:
			t.Errorf("expect ctx.Done() closed after Wait")
		}
	}
}
