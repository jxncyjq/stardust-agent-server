package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// /openapi.json 是非 GUI 客户端唯一能读到的契约：不在里面的路由，等于对生成客户端
// 的人不存在。而它是**手写**的一张表，与 ServeHTTP 里那个 switch 各活各的——于是
// 每加一个端点就漏一个，直到有人手工比对才发现。四个 /v1/browser/... 与
// /v1/tasks/{id}/interrupt 就是这么漏了很久的。
//
// 这条测试把两者对上：从 ServeHTTP 的路由 switch 里**读出**它实际分派的路径，与
// 规范里声明的路径逐条比较。它是自维护的——下一个新端点不进规范就红，不需要谁记得
// 去比对。

// routeInSpecExceptions 是**故意**不进 OpenAPI 的路径。
//
// 空的：目前没有这样的路径。它存在是为了让「不写进契约」成为一个要写下理由的决定，
// 而不是一次遗忘。
var routeInSpecExceptions = map[string]string{}

func TestEveryServedRouteIsInTheOpenAPIContract(t *testing.T) {
	served := routesFromServeHTTP(t)
	if len(served) < 20 {
		t.Fatalf("only %d routes parsed out of ServeHTTP; the parser has drifted from the code "+
			"and this test is no longer checking anything", len(served))
	}

	var declared []string
	for path := range BuildOpenAPISpec().Paths {
		declared = append(declared, path)
	}

	var missing []string
	for _, route := range served {
		if _, allowed := routeInSpecExceptions[route]; allowed {
			continue
		}
		if !declaredCovers(route, declared) {
			missing = append(missing, route)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these routes are served but absent from /openapi.json, so a generated client cannot "+
			"reach them:\n  %s", strings.Join(missing, "\n  "))
	}
}

// TestTheContractDoesNotPromiseRoutesNobodyServes 是反方向：一个规范里有、代码里
// 没有的路径，会让客户端生成一个永远 404 的方法。
func TestTheContractDoesNotPromiseRoutesNobodyServes(t *testing.T) {
	served := routesFromServeHTTP(t)

	var phantom []string
	for path := range BuildOpenAPISpec().Paths {
		if !servedCovers(path, served) {
			phantom = append(phantom, path)
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Errorf("these paths are in /openapi.json but nothing serves them:\n  %s", strings.Join(phantom, "\n  "))
	}
}

// normalizePathParams 把 {id}/{name}/{ticketID} 之类的占位符统一成 {}，使
// "/v1/tasks/{id}" 与解析出来的 "/v1/tasks/{}" 可比。
var pathParam = regexp.MustCompile(`\{[^}]*\}`)

func normalizePathParams(path string) string { return pathParam.ReplaceAllString(path, "{}") }

// declaredCovers 报告规范里有没有覆盖住这条**被服务的**路由。
//
// 两种算覆盖：逐字相同；或者这条路由本身只是一个前缀分支（"/v1/tasks/{}"，源码里
// 读不出它下面还有几段），而规范里有路径落在它下面。
//
// 反过来**不算**：规范里写了 "/v1/tasks/{id}"，不能替一条真实存在的
// "/v1/tasks/{id}/interrupt" 背书——那正是这条测试要抓的那类遗漏。
func declaredCovers(route string, declared []string) bool {
	normalized := normalizePathParams(route)
	routePrefix, routeIsPrefixOnly := strings.CutSuffix(normalized, "/{}")
	for _, candidate := range declared {
		other := normalizePathParams(candidate)
		if other == normalized {
			return true
		}
		if routeIsPrefixOnly && strings.HasPrefix(other, routePrefix+"/") {
			return true
		}
	}
	return false
}

// servedCovers 是反方向：规范里的一条路径，有没有真的被某个分支服务到。
//
// 这里允许「被一个前缀分支覆盖」——"/v1/tasks/{id}/result" 就是由按前缀分派的那个
// 分支处理的，源码里读不出这一段，把它判成幽灵路径只会教人去加例外。
func servedCovers(path string, served []string) bool {
	normalized := normalizePathParams(path)
	for _, route := range served {
		other := normalizePathParams(route)
		if other == normalized {
			return true
		}
		if prefix, ok := strings.CutSuffix(other, "/{}"); ok && strings.HasPrefix(normalized, prefix+"/") {
			return true
		}
	}
	return false
}

// routesFromServeHTTP 从 ServeHTTP 的路由 switch 里读出它实际分派的路径。
//
// 按 case 分支收集字面量，而不是全文扫字符串：一个分支里的 HasPrefix + HasSuffix
// 合起来才是一条路径（"/v1/browser/sessions/" + "/stream"），拆开看谁也不是。
func routesFromServeHTTP(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "http.go", nil, 0)
	if err != nil {
		t.Fatalf("parse http.go: %v", err)
	}

	var routes []string
	ast.Inspect(file, func(n ast.Node) bool {
		decl, ok := n.(*ast.FuncDecl)
		if !ok || decl.Name.Name != "ServeHTTP" {
			return true
		}
		ast.Inspect(decl.Body, func(inner ast.Node) bool {
			clause, ok := inner.(*ast.CaseClause)
			if !ok {
				return true
			}
			if route, ok := routeFromCase(clause); ok {
				routes = append(routes, route)
			}
			return true
		})
		return false
	})
	return routes
}

// routeFromCase 把一个 case 分支还原成它匹配的路径。
//
// 三种形状：精确相等（"/v1/sessions"）、前缀+后缀（"/v1/browser/sessions/" +
// "/stream"）、只有前缀（"/v1/tasks/"，其后是可变段）。只有前缀时补一个 {}，因为
// 那正是规范里要写占位符的地方。
func routeFromCase(clause *ast.CaseClause) (string, bool) {
	// 取反的 HasSuffix 要先认出来再跳过。**今天路由里一条都没有**（P4a 之后会话那几条
	// 分支改用 sessionIDFromPath 当判据，`!strings.HasSuffix` 在 http.go 里出现 0 次），
	// 所以 negated 恒为空表、下面那个 skip 分支从不命中。留着不是因为它现在有用，而是
	// 因为它是这个还原器的正确性前提：一旦有人再写回
	// `HasPrefix("/v1/sessions/") && !HasSuffix("/turns")`，把取反的后缀当成路径的一
	// 部分就会得出「/v1/sessions/{id} 没人服务」这种正好相反的结论。
	negated := map[ast.Node]bool{}
	ast.Inspect(clause, func(n ast.Node) bool {
		if unary, ok := n.(*ast.UnaryExpr); ok && unary.Op == token.NOT {
			negated[unary.X] = true
		}
		return true
	})

	var exact, prefixes, suffixes []string
	ast.Inspect(clause, func(n ast.Node) bool {
		if negated[n] {
			return false
		}
		switch node := n.(type) {
		case *ast.BinaryExpr:
			if node.Op != token.EQL {
				return true
			}
			if literal, ok := stringLiteral(node.Y); ok && strings.HasPrefix(literal, "/") {
				exact = append(exact, literal)
			}
		case *ast.CallExpr:
			name := calleeName(node)
			if len(node.Args) != 2 {
				return true
			}
			literal, ok := stringLiteral(node.Args[1])
			if !ok {
				return true
			}
			switch name {
			case "HasPrefix":
				prefixes = append(prefixes, literal)
			case "HasSuffix":
				suffixes = append(suffixes, literal)
			}
		}
		return true
	})

	switch {
	case len(exact) > 0:
		return exact[0], true
	case len(prefixes) > 0 && len(suffixes) > 0:
		return strings.TrimSuffix(prefixes[0], "/") + "/{}" + suffixes[0], true
	case len(prefixes) > 0:
		return strings.TrimSuffix(prefixes[0], "/") + "/{}", true
	default:
		return "", false
	}
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

func calleeName(call *ast.CallExpr) string {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	return ""
}
