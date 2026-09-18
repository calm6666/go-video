package pipeline

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"go-video/common/timeutil"
)

// TestPipelineMaxSize 验证聚批达到 MaxSize 时触发 Do 回调。
func TestPipelineMaxSize(t *testing.T) {
	conf := &Config{
		MaxSize:  3,
		Interval: timeutil.Duration(5 * time.Second),
		Buffer:   10,
		Worker:   10,
	}
	type recv struct {
		ch     int
		values map[string][]interface{}
	}
	results := make(chan recv, 4)
	do := func(c context.Context, ch int, values map[string][]interface{}) {
		results <- recv{ch: ch, values: values}
	}
	split := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	p := NewPipeline(conf)
	p.Do = do
	p.Split = split
	p.Start()
	// 向同一分片投递 MaxSize 个，应立即触发聚批。
	p.SyncAdd(context.Background(), "1", 1)
	p.SyncAdd(context.Background(), "1", 2)
	p.SyncAdd(context.Background(), "1", 3)
	select {
	case r := <-results:
		if r.ch != 1 {
			t.Fatalf("expect shard 1, got %d", r.ch)
		}
		vs := r.values["1"]
		if len(vs) != 3 {
			t.Fatalf("expect 3 values, got %v", vs)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Do on MaxSize")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
}

// TestPipelineInterval 验证 Interval 超时触发 Do 回调。
func TestPipelineInterval(t *testing.T) {
	conf := &Config{
		MaxSize:  1000,
		Interval: timeutil.Duration(50 * time.Millisecond),
		Buffer:   10,
		Worker:   10,
	}
	type recv struct {
		ch     int
		values map[string][]interface{}
	}
	results := make(chan recv, 4)
	do := func(c context.Context, ch int, values map[string][]interface{}) {
		results <- recv{ch: ch, values: values}
	}
	split := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	p := NewPipeline(conf)
	p.Do = do
	p.Split = split
	p.Start()
	// 投递一个值，等待 Interval 超时触发。
	if err := p.Add(context.Background(), "3", 30); err != nil {
		t.Fatalf("Add err: %v", err)
	}
	select {
	case r := <-results:
		if r.ch != 3 {
			t.Fatalf("expect shard 3, got %d", r.ch)
		}
		vs := r.values["3"]
		if len(vs) != 1 || vs[0] != 30 {
			t.Fatalf("expect [30], got %v", vs)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Do on Interval")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
}

// TestPipelineSplit 验证 Split 分片：不同 key 落入不同 worker。
func TestPipelineSplit(t *testing.T) {
	conf := &Config{
		MaxSize:  1,
		Interval: timeutil.Duration(5 * time.Second),
		Buffer:   10,
		Worker:   10,
	}
	type recv struct {
		ch    int
		key   string
		value interface{}
	}
	results := make(chan recv, 10)
	do := func(c context.Context, ch int, values map[string][]interface{}) {
		for k, vs := range values {
			results <- recv{ch: ch, key: k, value: vs[0]}
		}
	}
	split := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	p := NewPipeline(conf)
	p.Do = do
	p.Split = split
	p.Start()
	// MaxSize=1，每条立即触发一次 Do。
	for i := 0; i < 5; i++ {
		p.SyncAdd(context.Background(), strconv.Itoa(i), i)
	}
	got := make(map[int]recv)
	for i := 0; i < 5; i++ {
		select {
		case r := <-results:
			got[r.ch] = r
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for Do")
		}
	}
	for i := 0; i < 5; i++ {
		r, ok := got[i]
		if !ok {
			t.Fatalf("missing shard %d", i)
		}
		if r.key != strconv.Itoa(i) {
			t.Fatalf("shard %d key mismatch: %s", i, r.key)
		}
		if r.value != i {
			t.Fatalf("shard %d value mismatch: %v", i, r.value)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
}

// TestPipelineClose 验证 Close 触发剩余聚批并等待所有 worker 退出。
func TestPipelineClose(t *testing.T) {
	conf := &Config{
		MaxSize:  1000,
		Interval: timeutil.Duration(5 * time.Second),
		Buffer:   10,
		Worker:   2,
	}
	type recv struct {
		ch     int
		values map[string][]interface{}
	}
	results := make(chan recv, 4)
	do := func(c context.Context, ch int, values map[string][]interface{}) {
		results <- recv{ch: ch, values: values}
	}
	split := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	p := NewPipeline(conf)
	p.Do = do
	p.Split = split
	p.Start()
	p.SyncAdd(context.Background(), "0", "a")
	p.SyncAdd(context.Background(), "0", "b")
	p.SyncAdd(context.Background(), "1", "c")
	// Close 触发各分片剩余聚批处理。
	if err := p.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
	collected := make(map[int]map[string][]interface{})
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			collected[r.ch] = r.values
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for Do on Close")
		}
	}
	if vs := collected[0]["0"]; len(vs) != 2 || vs[0] != "a" || vs[1] != "b" {
		t.Fatalf("shard 0 values mismatch: %v", collected[0])
	}
	if vs := collected[1]["1"]; len(vs) != 1 || vs[0] != "c" {
		t.Fatalf("shard 1 values mismatch: %v", collected[1])
	}
}

// TestPipelineErrFull 验证 channel 满时 Add 返回 ErrFull。
func TestPipelineErrFull(t *testing.T) {
	conf := &Config{
		MaxSize:  1,
		Interval: timeutil.Duration(5 * time.Second),
		Buffer:   1,
		Worker:   1,
	}
	hold := make(chan struct{})
	started := make(chan struct{}, 8)
	do := func(c context.Context, ch int, values map[string][]interface{}) {
		started <- struct{}{}
		<-hold // 阻塞 worker，使 channel 不被消费
	}
	split := func(s string) int { return 0 }
	p := NewPipeline(conf)
	p.Do = do
	p.Split = split
	p.Start()
	// 第一个值被 worker 消费后触发 Do（MaxSize=1）并阻塞在 hold 上。
	if err := p.Add(context.Background(), "k", 1); err != nil {
		t.Fatalf("first Add err: %v", err)
	}
	<-started
	// worker 阻塞，buffer 容量 1，再投递一个填满。
	if err := p.Add(context.Background(), "k", 2); err != nil {
		t.Fatalf("second Add err: %v", err)
	}
	// 此时 channel 已满，再投递应返回 ErrFull。
	if err := p.Add(context.Background(), "k", 3); !errors.Is(err, ErrFull) {
		t.Fatalf("expect ErrFull, got %v", err)
	}
	close(hold)
	if err := p.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
}

// TestPipelineSmooth 验证 Smooth 模式下各 worker 的回调时机被错开。
func TestPipelineSmooth(t *testing.T) {
	conf := &Config{
		MaxSize:  100,
		Interval: timeutil.Duration(time.Second),
		Buffer:   100,
		Worker:   10,
		Smooth:   true,
	}
	type result struct {
		index int
		ts    time.Time
	}
	var (
		mu      sync.Mutex
		results []result
	)
	do := func(c context.Context, index int, values map[string][]interface{}) {
		mu.Lock()
		results = append(results, result{index: index, ts: time.Now()})
		mu.Unlock()
	}
	split := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	p := NewPipeline(conf)
	p.Do = do
	p.Split = split
	p.Start()
	for i := 0; i < 10; i++ {
		p.Add(context.Background(), strconv.Itoa(i), 1)
	}
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	if len(results) != conf.Worker {
		mu.Unlock()
		t.Fatalf("expect results equal worker %d, got %d", conf.Worker, len(results))
	}
	for i := 1; i < len(results); i++ {
		if results[i].ts.Sub(results[i-1].ts) < 20*time.Millisecond {
			mu.Unlock()
			t.Fatalf("expect runs be smooth, gap %v", results[i].ts.Sub(results[i-1].ts))
		}
	}
	mu.Unlock()
	if err := p.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
}
