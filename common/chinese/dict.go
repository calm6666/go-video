package chinese

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"github.com/go-ego/cedar"
)

// dict 包含 cedar Trie 与字典值列表。
type dict struct {
	Trie   *cedar.Cedar
	Values [][]string
}

// buildFromFile 按字典文件名加载内容并构建 Trie。
// 字典内容每行格式：key<TAB>value1[<TAB>value2...]；少于 2 列的行被忽略。
func buildFromFile(fileName string) (*dict, error) {
	trie := cedar.New()
	values := [][]string{}
	bs, err := Loader(fileName)
	if err != nil {
		return nil, fmt.Errorf("chinese: load dict %q: %w", fileName, err)
	}
	strs := strings.Split(string(bs), "\n")
	for _, line := range strs {
		items := strings.Split(strings.TrimSpace(line), "\t")
		if len(items) < 2 {
			continue
		}
		if err := trie.Insert([]byte(items[0]), len(values)); err != nil {
			return nil, fmt.Errorf("chinese: insert %q: %w", items[0], err)
		}
		if len(items) > 2 {
			values = append(values, items[1:])
		} else {
			values = append(values, strings.Fields(items[1]))
		}
	}
	return &dict{Trie: trie, Values: values}, nil
}

// prefixMatch 用 Trie 找出 str 的所有前缀匹配，返回 key -> values 映射。
func (d *dict) prefixMatch(str string) (map[string][]string, error) {
	if d.Trie == nil {
		return nil, fmt.Errorf("chinese: Trie is nil")
	}
	res := make(map[string][]string)
	for _, id := range d.Trie.PrefixMatch([]byte(str), 0) {
		key, err := d.Trie.Key(id)
		if err != nil {
			return nil, fmt.Errorf("chinese: get key: %w", err)
		}
		value, err := d.Trie.Value(id)
		if err != nil {
			return nil, fmt.Errorf("chinese: get value: %w", err)
		}
		res[string(key)] = d.Values[value]
	}
	return res, nil
}

var (
	// defaultRead 远程加载的初始缓冲区大小。
	defaultRead int64 = 16 * 1024 // 16kb
	// defaultURL 远程字典基础 URL。
	defaultURL = "http://i0.hdslb.com/bfs/static/"
)

// loaderPtr 是全局字典加载函数，使用 atomic.Value 以支持并发读、初始化阶段替换。
var loaderPtr atomic.Value // func(string) ([]byte, error)

// dictRootPtr 是本地字典目录，使用 atomic.Value 以支持并发读、初始化阶段替换。
var dictRootPtr atomic.Value // string

func init() {
	loaderPtr.Store(defaultLoader)
	dictRootPtr.Store("opencc")
}

// Loader 返回当前生效的字典加载函数。
// 默认实现优先从 DictRoot 读取本地文件，找不到时回退到远程 URL。
var Loader = func(file string) ([]byte, error) {
	fn := loaderPtr.Load().(func(string) ([]byte, error))
	return fn(file)
}

// SetDictLoader 替换全局字典加载器。仅供初始化阶段或测试使用，
// 不应在运行时并发调用。传入 nil 恢复默认实现。
func SetDictLoader(fn func(file string) ([]byte, error)) {
	if fn == nil {
		loaderPtr.Store(defaultLoader)
		return
	}
	loaderPtr.Store(fn)
}

// SetDictRoot 设置本地字典目录。默认 "opencc"。
// 仅供初始化阶段调用，运行时不应修改。
func SetDictRoot(root string) {
	if root == "" {
		root = "opencc"
	}
	dictRootPtr.Store(root)
}

// defaultLoader 先尝试本地文件，再回退到远程 URL。
func defaultLoader(file string) ([]byte, error) {
	root := dictRootPtr.Load().(string)
	if root != "" {
		local := filepath.Join(root, file)
		if bs, err := os.ReadFile(local); err == nil {
			return bs, nil
		}
	}
	if bs, err := fetchFromURL(file); err == nil && len(bs) > 0 {
		return bs, nil
	}
	return nil, fmt.Errorf("chinese: dict file %q not found under %q and remote fetch failed", file, root)
}

// fetchFromURL 最多重试 3 次从 defaultURL 下载字典文件。
func fetchFromURL(file string) ([]byte, error) {
	client := http.Client{Timeout: 10 * time.Second}
	var lastErr error
	for i := 0; i < 3; i++ {
		resp, err := client.Get(defaultURL + file)
		if err != nil {
			lastErr = err
			logx.Errorf("chinese: bfs client url=%s file=%s err=%v", defaultURL, file, err)
			time.Sleep(time.Millisecond * 50)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("chinese: bfs client status=%d file=%s", resp.StatusCode, file)
			_ = resp.Body.Close()
			time.Sleep(time.Millisecond * 50)
			continue
		}
		bs, err := readAll(resp.Body, defaultRead)
		_ = resp.Body.Close()
		if err == nil {
			return bs, nil
		}
		lastErr = err
		logx.Errorf("chinese: bfs client url=%s file=%s err=%v", defaultURL, file, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("chinese: bfs client unavailable file=%s", file)
	}
	return nil, lastErr
}

// readAll 安全地读取 r 全部内容，初始缓冲区为 capacity。
func readAll(r io.Reader, capacity int64) (b []byte, err error) {
	buf := bytes.NewBuffer(make([]byte, 0, capacity))
	defer func() {
		e := recover()
		if e == nil {
			return
		}
		if panicErr, ok := e.(error); ok && panicErr == bytes.ErrTooLarge {
			err = panicErr
		} else {
			panic(e)
		}
	}()
	_, err = buf.ReadFrom(r)
	return buf.Bytes(), err
}
