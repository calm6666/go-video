package model

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 本文件补的是 migration_parity_test.go 覆盖不到的一半：
// 10 张表里只有 3 张把批量 INSERT 的列清单提成了包级常量
// （behaviorEventInsertCols / metricWindowInsertCols / retentionCohortInsertCols），
// 其余 7 张的列清单内联在方法里的字符串拼接中
// （user_interest.ReplaceForMid、consumer_offset.Claim、dead_letter.InsertIfAbsent、
// metric_definition.InsertIfAbsent、window_watermark.Advance、
// content_projection.Apply、aggregation_job.InsertIfAbsent）。
//
// 把手写清单搬进测试等于又造出一份需要同步的文本——门禁会永远「对得上」，
// 因为两边都是抄的。所以这里用 go/ast 直接读实现源码，把拼接表达式**求值**成
// 真实列清单，再与 deploy/migrations/spm 比对，并钉：
//   - 列数 == VALUES 表达式条数（不等就是 "column count doesn't match value count"）；
//   - 批量写入的 rowPlaceholders(行数, _) 第一个实参常量 == 列数
//     （抄错一个数字，多值 INSERT 的占位符总数与参数总数就错位）；
//   - 显式清单不得含自增主键 id（含 id 意味着调用方在往主键里写 0）；
//   - 清单里每一列都必须存在于该表的迁移定义（写不存在的列是运行期 Unknown column）。
// 还钉住「每张 model 表都至少有一条被解析到的 INSERT」，防止解析器静默漏表。
//
// 刻意不钉「清单顺序 == 迁移声明顺序」：显式列清单按名字映射，
// content_projection.Apply 就把 subject_type/subject_id 写在投影列之后，
// 那是合法写法；真正的错位风险（args 的 append 顺序与清单不一致）是运行期属性。

// insertSite 是一处从实现源码解析出来的 INSERT。
type insertSite struct {
	file    string
	line    int
	table   string
	columns []string
	// rowCount 是 rowPlaceholders(常量, _) 的第一个实参；0 表示这条是单行 VALUES 字面量。
	rowCount int
	// valueItems / valueHolders 是 VALUES 段的表达式条数与其中 ? 的条数（单行字面量才有）。
	valueItems   int
	valueHolders int
	// dynamic 记录 VALUES 段既不是字面量也不是 rowPlaceholders 的情形（必须报告，不静默放过）。
	dynamic string
}

func TestInlineInsertListsMatchMigration(t *testing.T) {
	sites := collectInsertSites(t)
	if len(sites) == 0 {
		t.Fatal("源码里没解析出任何 INSERT INTO spm_*：解析器失效，本门禁会静默通过")
	}
	tables := loadMigrationTables(t)
	specs := map[string]tableSpec{}
	for _, s := range tableSpecs() {
		specs[s.table] = s
	}

	seen := map[string][]insertSite{}
	for _, s := range sites {
		seen[s.table] = append(seen[s.table], s)
		tbl, ok := tables[s.table]
		if !ok {
			t.Errorf("%s:%d: 往迁移里不存在的表 %s 写入", s.file, s.line, s.table)
			continue
		}
		t.Run(fmt.Sprintf("%s/%s:%d", s.table, s.file, s.line), func(t *testing.T) {
			// -v 下打印解析结果：本门禁的失效方式是「解析出空清单」，
			// 光看 PASS 分不清是真对齐还是没解析到。
			t.Logf("列清单(%d): %v；VALUES 表达式 %d 个（? %d 个）；批量行数列数 %d",
				len(s.columns), s.columns, s.valueItems, s.valueHolders, s.rowCount)
			if len(s.columns) == 0 {
				t.Fatalf("列清单解析为空")
			}
			if dup := duplicated(s.columns); len(dup) > 0 {
				t.Errorf("INSERT 列清单里 %v 重复出现，后写的参数会覆盖前列", dup)
			}
			if contains(s.columns, "id") {
				t.Errorf("显式 INSERT 清单含自增主键 id：调用方在往主键里写 0，主键序列会被污染")
			}
			// 只钉「列存在」，不钉「清单顺序 == 迁移声明顺序」：
			// 显式列清单的 INSERT 按名字映射（`INSERT INTO t (b, a)` 与迁移里 a 在前无关），
			// 真正的错位风险是「args 的 append 顺序与清单不一致」，那是运行期属性。
			// 「迁移列序 == 结构体字段序」这条可读性约定由
			// TestMigrationColumnOrderMatchesModelStruct 单独钉。
			for _, c := range s.columns {
				if !tbl.hasColumn(c) {
					t.Errorf("INSERT 列 %s 在迁移 %s 里不存在", c, tbl.file)
				}
			}
			// 列数与 VALUES 表达式条数必须相等。
			switch {
			case s.rowCount > 0:
				if s.rowCount != len(s.columns) {
					t.Errorf("rowPlaceholders 的行数列数常量是 %d，列清单实际 %d 列：多值 INSERT 的"+
						"占位符总数与参数总数会错位（MySQL 报 column count doesn't match value count）",
						s.rowCount, len(s.columns))
				}
			case s.valueItems > 0:
				if s.valueItems != len(s.columns) {
					t.Errorf("VALUES 段有 %d 个表达式，列清单 %d 列", s.valueItems, len(s.columns))
				}
				if s.valueHolders > s.valueItems {
					t.Errorf("? 的个数 %d 超过表达式条数 %d", s.valueHolders, s.valueItems)
				}
			default:
				t.Errorf("VALUES 段形态无法判定（%s）：既不是字面量清单也不是 rowPlaceholders，"+
					"本条一致性检查对该语句失效", s.dynamic)
			}
		})
	}
	// 每张表都必须至少有一条被解析到的 INSERT：漏表的表现是「这条写路径没有任何门禁」。
	var unmodeled []string
	for name := range specs {
		if len(seen[name]) == 0 {
			unmodeled = append(unmodeled, name)
		}
	}
	if len(unmodeled) > 0 {
		sort.Strings(unmodeled)
		t.Errorf("以下表没有解析到任何 INSERT（新增写路径或改写法后必须同步本门禁）: %v", unmodeled)
	}
	// 反向：tableSpec 里手写的 insertColumns 常量必须与源码解析结果一致，
	// 否则 tableSpecs 与实现就是两份各说各话的文本。
	for _, s := range tableSpecs() {
		if s.insertColumns == "" {
			continue
		}
		declared := selectColumnNames(s.insertColumns)
		var found bool
		for _, site := range seen[s.table] {
			if site.file == "" {
				continue
			}
			found = true
			if strings.Join(site.columns, ",") != strings.Join(declared, ",") {
				t.Errorf("%s（%s:%d）: 源码里的列清单 %v 与 tableSpecs 登记的 %v 不一致",
					s.table, site.file, site.line, site.columns, declared)
			}
		}
		if !found {
			t.Errorf("%s: tableSpecs 声明了 insertColumns，但源码里没解析到它的 INSERT", s.table)
		}
	}
}

// TestWritePathsTargetOnlyModeledTables 钉「本服务只写自己那 10 张表」：
// 源码里出现的每一张写入表名都必须在 tableSpecs 登记过。
// 越界写别人的表是 AGENTS.md §5 的数据所有权红线，编译与运行期都不会报错。
func TestWritePathsTargetOnlyModeledTables(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range tableSpecs() {
		declared[s.table] = true
	}
	for _, s := range collectInsertSites(t) {
		if !declared[s.table] {
			t.Errorf("%s:%d: 写入未登记的表 %s", s.file, s.line, s.table)
		}
	}
}

// --- 源码解析 ---

// modelSources 解析本包全部非测试 Go 文件。
type modelSources struct {
	fset     *token.FileSet
	files    map[string]*ast.File // 文件路径 -> AST
	strConst map[string]ast.Expr  // 包级字符串常量
	intConst map[string]int64     // 包级整型常量（rowPlaceholders 的行数实参取它换算）
}

func loadModelSources(t *testing.T) *modelSources {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("枚举 %s 下的 .go 失败: %v", dir, err)
	}
	src := &modelSources{
		fset:     token.NewFileSet(),
		files:    map[string]*ast.File{},
		strConst: map[string]ast.Expr{},
		intConst: map[string]int64{},
	}
	var parsed int
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(src.fset, p, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", p, err)
		}
		src.files[p] = f
		parsed++
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					// 字面量与拼接表达式（如 consumerOffsetFullColumns =
					// consumerOffsetColumns + ", COALESCE(payload, '') AS payload"）
					// 都登记，由 renderExpr 递归展开；非字符串常量在比对时不会命中。
					src.strConst[name.Name] = vs.Values[i]
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.INT {
						if v, err := strconv.ParseInt(lit.Value, 10, 64); err == nil {
							src.intConst[name.Name] = v
						}
					}
				}
			}
		}
	}
	if parsed == 0 {
		t.Fatalf("%s 下没有非测试 .go 文件", dir)
	}
	return src
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// localFuncConsts 收集一个函数体内声明的常量（content_projection.Apply 的 cols 就在其中）。
func localFuncConsts(fd *ast.FuncDecl) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	if fd.Body == nil {
		return out
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, name := range vs.Names {
				out[name.Name] = vs.Values[i]
			}
		}
		return true
	})
	return out
}

// renderExpr 把拼接表达式求值成文本：
// 字符串字面量取真值，已知的包级/函数内常量递归展开，
// 其余节点（函数调用、未知标识符）保留原始源码文本以便后续识别。
func renderExpr(src *modelSources, locals map[string]ast.Expr, e ast.Expr, depth int) string {
	if depth > 12 {
		return "<递归过深>"
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if s, ok := stringLit(v); ok {
				return s
			}
		}
		return nodeText(src, v)
	case *ast.ParenExpr:
		return renderExpr(src, locals, v.X, depth+1)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return renderExpr(src, locals, v.X, depth+1) + renderExpr(src, locals, v.Y, depth+1)
		}
		return nodeText(src, v)
	case *ast.Ident:
		if expr, ok := locals[v.Name]; ok {
			return renderExpr(src, locals, expr, depth+1)
		}
		if expr, ok := src.strConst[v.Name]; ok {
			return renderExpr(src, locals, expr, depth+1)
		}
		return nodeText(src, v)
	default:
		return nodeText(src, v)
	}
}

// nodeText 取节点在源码里的原文（用于识别 rowPlaceholders(...) 这类调用）。
func nodeText(src *modelSources, n ast.Node) string {
	file, err := os.ReadFile(src.fset.Position(n.Pos()).Filename)
	if err != nil {
		return "<读取源码失败>"
	}
	start := src.fset.Position(n.Pos()).Offset
	end := src.fset.Position(n.End()).Offset
	if start > end || end > len(file) {
		return "<越界读取>"
	}
	return string(file[start:end])
}

// collectInsertSites 找出实现源码里的每一条 INSERT INTO spm_*。
func collectInsertSites(t *testing.T) []insertSite {
	t.Helper()
	src := loadModelSources(t)
	var sites []insertSite
	for path, file := range src.files {
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			// 函数内常量（content_projection.Apply 的 cols）必须在该函数的作用域里展开，
			// 否则拼出来的列清单会缺一段。
			scoped := localFuncConsts(fd)
			var matched []ast.Expr
			ast.Inspect(fd, func(n ast.Node) bool {
				expr, ok := n.(ast.Expr)
				if !ok {
					return true
				}
				// 只认「字符串拼接表达式」这一类节点：CallExpr（ExecCtx / TransactCtx 整条调用）
				// 展开不出值，落到 nodeText 兜底会把源码里的引号与 + 当成 SQL 内容读进来。
				switch expr.(type) {
				case *ast.BasicLit, *ast.BinaryExpr, *ast.ParenExpr, *ast.Ident:
				default:
					return true
				}
				if hasInsertHead(renderExpr(src, scoped, expr, 0)) {
					matched = append(matched, expr)
				}
				return true
			})
			// 只保留最外层匹配节点：拼接表达式里的每个子字面量也都「含 INSERT」，
			// 但只有整条表达式才拼得出完整列清单。
			for _, m := range matched {
				if enclosedByOther(m, matched) {
					continue
				}
				text := renderExpr(src, scoped, m, 0)
				for _, s := range parseInsertSites(src, filepath.Base(path), src.fset.Position(m.Pos()).Line, text) {
					sites = append(sites, s)
				}
			}
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].table != sites[j].table {
			return sites[i].table < sites[j].table
		}
		if sites[i].file != sites[j].file {
			return sites[i].file < sites[j].file
		}
		return sites[i].line < sites[j].line
	})
	return sites
}

// scanInsertHead 从 from 起找下一处 `INSERT INTO <spm_表名> ( *`，返回表名与左括号下标。
// 表名与左括号之间允许空白：源码里字面量常在这两处之间换行拼接（例如
// `"INSERT INTO spm_metric_definition (metric_key, ..."+` 后接 `+` 折行）。
func scanInsertHead(text string, from int) (table string, open int, ok bool) {
	i := from
	for i < len(text) {
		j := strings.Index(text[i:], "INSERT INTO ")
		if j < 0 {
			return "", 0, false
		}
		i += j + len("INSERT INTO ")
		k := i
		for k < len(text) && isTableNameByte(text[k]) {
			k++
		}
		name := text[i:k]
		p := k
		for p < len(text) && (text[p] == ' ' || text[p] == '\t' || text[p] == '\n' || text[p] == '\r') {
			p++
		}
		if strings.HasPrefix(name, "spm_") && p < len(text) && text[p] == '(' {
			return name, p, true
		}
		if k == i {
			i++ // 表名为空（`INSERT INTO (`），前进一格避免死循环
		} else {
			i = k
		}
	}
	return "", 0, false
}

func isTableNameByte(b byte) bool {
	return b == '_' || b == '.' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// hasInsertHead 判断文本里是否出现了完整的 `INSERT INTO spm_x (`。
// 只认带表名与左括号的形式，避免把说明性文字当成一条语句。
func hasInsertHead(text string) bool {
	_, _, ok := scanInsertHead(text, 0)
	return ok
}

// enclosedByOther 判断 e 是否被同批匹配里的另一条严格包含。
func enclosedByOther(e ast.Expr, all []ast.Expr) bool {
	for _, o := range all {
		if o == e {
			continue
		}
		if o.Pos() <= e.Pos() && o.End() >= e.End() && (o.Pos() != e.Pos() || o.End() != e.End()) {
			return true
		}
	}
	return false
}

// parseInsertSites 从拼接出来的完整 SQL 文本里抽出全部 INSERT 定义。
func parseInsertSites(src *modelSources, file string, line int, text string) []insertSite {
	var out []insertSite
	for i := 0; i < len(text); {
		table, open, ok := scanInsertHead(text, i)
		if !ok {
			return out
		}
		cols, next, closed := balancedParens(text, open)
		if !closed {
			out = append(out, insertSite{file: file, line: line, table: table,
				dynamic: "列清单括号未闭合"})
			i = open + 1
			continue
		}
		site := insertSite{file: file, line: line, table: table, columns: splitIdentList(cols)}
		parseValuesRegion(src, &site, text[next:])
		out = append(out, site)
		i = next
	}
	return out
}

// parseValuesRegion 判定 VALUES 段的形态：
//   - `( ?, ?, '0', ... )` 单行字面量 -> 记录表达式条数与 ? 条数；
//   - `rowPlaceholders(<行数列数常量>, <行数>)` 批量 -> 记录行数列数常量；
//   - 其它形态记进 dynamic，由用例显式报错而不是静默放过。
func parseValuesRegion(src *modelSources, site *insertSite, after string) {
	trimmed := strings.TrimLeft(after, " \t\r\n")
	up := strings.ToUpper(trimmed)
	if !strings.HasPrefix(up, "VALUES") {
		site.dynamic = "列清单之后不是 VALUES：" + firstLine(trimmed)
		return
	}
	rest := strings.TrimLeft(trimmed[len("VALUES"):], " \t\r\n")
	switch {
	case strings.HasPrefix(rest, "rowPlaceholders"):
		applyRowPlaceholders(src, site, rest)
	case strings.HasPrefix(rest, "("):
		vals, _, closed := balancedParens(rest, 0)
		if !closed {
			site.dynamic = "VALUES 段括号未闭合：" + firstLine(rest)
			return
		}
		// behavior_event 写成 `VALUES (rowPlaceholders(n, 1))`：括号里套的是批量生成器。
		if idx := strings.Index(vals, "rowPlaceholders"); idx >= 0 {
			applyRowPlaceholders(src, site, vals[idx:])
			return
		}
		for _, it := range splitTopLevel(vals) {
			item := strings.TrimSpace(it)
			if item == "" {
				continue
			}
			site.valueItems++
			if item == "?" {
				site.valueHolders++
			}
		}
	default:
		site.dynamic = firstLine(rest)
	}
}

func applyRowPlaceholders(src *modelSources, site *insertSite, rest string) {
	name, n, ok := firstIntArg(src, rest)
	if ok {
		site.rowCount = n
		return
	}
	site.dynamic = "rowPlaceholders(" + name + ", ...) 的第一个实参不是可解析的整型常量"
}

// balancedParens 从 open 处的 '(' 取到配对 ')' 之间的内容，返回内容与右括号后的下标。
func balancedParens(s string, open int) (string, int, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], i + 1, true
			}
		}
	}
	return "", open, false
}

// splitIdentList 把列清单切成标识符列表；非标识符项原样保留以便报错。
func splitIdentList(list string) []string {
	parts := splitTopLevel(list)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		item := strings.TrimSpace(p)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	return out
}

// firstIntArg 取 `rowPlaceholders(<常量>, ...)` 的第一个实参值。
// 常量值一律从实现源码的包级整型常量表换算（src.intConst），测试里不抄数字。
func firstIntArg(src *modelSources, s string) (string, int, bool) {
	open := strings.Index(s, "(")
	if open < 0 {
		return "", 0, false
	}
	rest := s[open+1:]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", 0, false
	}
	name := strings.TrimSpace(rest[:comma])
	if n, err := strconv.Atoi(name); err == nil {
		return name, n, true
	}
	v, ok := src.intConst[name]
	return name, int(v), ok
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
