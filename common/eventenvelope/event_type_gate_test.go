package eventenvelope

// 全仓 event_type 命名门禁。
//
// 为什么需要：isValidEventType 只允许「小写字母 / 数字 / 点号」，而这条规则写在 common 包里，
// 各服务的事件类型常量写在各自 model 里，两边没有任何编译期联系。命名一旦带下划线，
// 报错点是 `New`/`Validate` 的**运行时**返回值，而它的后果不是「事件名不好看」：
// 生产者的 enqueue/appendOutboxEvent 恒返回错误 → 承载它的事务恒回滚（live-media 13 个写方法一次也成功不了），
// 或者非事务路径只写一条 Error 日志、事件静默丢失（user-profile 的认证快照路径）。
// 这两个缺陷都是本轮补测试时才发现的，且各自都躲过了原有全部单测。
//
// 本门禁按源码扫描而非 import 常量：跨 40+ 个服务包没有共享的注册表可枚举，
// 而 `Event* = "..."` 这个形态就是全仓事实上的事件类型声明方式（消费侧的订阅映射同样用它）。

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// eventConstRe 抓 `EventXxx = "value"` 形态的常量声明。
// 两种写法都要覆盖：const 块内的缩进声明、`const X = "..."` 单行声明，以及行尾注释。
var eventConstRe = regexp.MustCompile(`(?m)^\s*(?:const\s+)?(Event[A-Za-z0-9_]*)\s*=\s*"([^"]*)"`)

// excludedConstPrefix 是刻意排除的常量名前缀。
// EventSource* 是 live-ingest 的事件「来源标签」（entry/health/cdn/admin/sweeper），
// 与 event_type 不是一个字段，不参与本契约。
var excludedConstPrefixes = []string{"EventSource"}

// mustContain 钉住扫描确实覆盖到了已知生产者：少一个就说明正则或遍历失效了，
// 那时本用例会退化成永真检查。
var mustContain = []string{
	"livemedia.record.stopped",
	"user.profile.updated",
	"content.published",
	"behavior.play",
	"engagement.action",
	// 下面两个只能由 `const X = "..."` 单行形态声明，漏了它们说明正则又只认 const 块了。
	"live.state",
	"notification.request",
}

type foundEventType struct {
	name  string
	value string
	file  string
	line  int
}

func scanEventTypes(t *testing.T) []foundEventType {
	t.Helper()
	const repoRoot = "../.."
	var out []foundEventType
	for _, root := range []string{"services", "common", "api"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			// 生成产物（pb.go / goctl）里的 Event* 常量是 gRPC 方法名等，不属于事件类型词表。
			if strings.Contains(string(src[:min(300, len(src))]), "Code generated") {
				return nil
			}
			for i, line := range strings.Split(string(src), "\n") {
				for _, m := range eventConstRe.FindAllStringSubmatch(line, -1) {
					if excludedName(m[1]) {
						continue
					}
					out = append(out, foundEventType{name: m[1], value: m[2], file: path, line: i + 1})
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s 失败：本门禁依赖它作为事件类型声明的唯一事实来源: %v", root, err)
		}
	}
	return out
}

func excludedName(ident string) bool {
	for _, p := range excludedConstPrefixes {
		if strings.HasPrefix(ident, p) {
			return true
		}
	}
	return false
}

func TestRepoEventTypeConstantsSatisfyEnvelopeContract(t *testing.T) {
	found := scanEventTypes(t)
	if len(found) < 35 {
		t.Fatalf("只扫到 %d 个 Event* 常量，远低于预期：正则或目录遍历已失效，本门禁正在退化成永真检查", len(found))
	}
	seen := map[string]bool{}
	for _, e := range found {
		seen[e.value] = true
		if _, err := New("gate-probe", e.value, "gate-aggregate", "1", 1, nil, ""); err != nil {
			t.Errorf("%s:%d %s = %q 过不了信封契约：%v（后果：生产者写事件恒失败，事务回滚或事件静默丢失）",
				e.file, e.line, e.name, e.value, err)
		}
	}
	for _, want := range mustContain {
		if !seen[want] {
			t.Errorf("扫描结果里没有 %q：说明对应服务的常量形态变了，本门禁覆盖不到它了", want)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].value < found[j].value })
	names := make([]string, 0, len(found))
	for _, e := range found {
		names = append(names, fmt.Sprintf("%s=%s", e.name, e.value))
	}
	t.Logf("扫到 %d 个 Event* 常量声明、%d 个不同取值，全部满足小写点分隔契约", len(found), len(seen))
	if testing.Verbose() {
		t.Logf("清单：%s", strings.Join(names, " "))
	}
}

// TestConsumerAndProducerNamesAgree 保证「消费侧订阅名」与「生产侧事件名」没有各写一套。
// 命名规则合法但两边拼法不同时，消费者永远收不到，且不会有任何报错——这正是本用例要拦的。
func TestConsumerAndProducerNamesAgree(t *testing.T) {
	// 生产者：各服务 model 里的 Event* 常量；消费者：inbox / search-indexer 的订阅映射。
	// 两侧都用同一份扫描结果，因此这里只需断言被订阅的名字确实有人生产。
	constants := scanEventTypes(t)
	produced := map[string]bool{}
	for _, e := range constants {
		// 消费侧映射文件里的常量同样算「已声明」，靠目录前缀区分生产/消费两侧。
		if !strings.Contains(filepath.ToSlash(e.file), "/internal/consumer/") {
			produced[e.value] = true
		}
	}
	var subscribed []foundEventType
	for _, e := range constants {
		if strings.Contains(filepath.ToSlash(e.file), "/internal/consumer/") {
			subscribed = append(subscribed, e)
		}
	}
	if len(subscribed) == 0 {
		t.Fatal("没有扫到任何消费侧订阅常量，consumer 目录形态已变化")
	}
	for _, s := range subscribed {
		if !produced[s.value] {
			t.Errorf("%s:%d 订阅了 %q，但没有任何生产者声明这个名字（事件会被静默丢弃）", s.file, s.line, s.value)
		}
	}
}
