package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func source(t *testing.T, path string) *ast.File {
	t.Helper()
	f, e := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func literal(e ast.Expr) string {
	if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
		s, _ := strconv.Unquote(b.Value)
		return s
	}
	return ""
}
func documented(pattern string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(contract, -1) {
		out[m[1]] = true
	}
	return out
}
func compareInventory(t *testing.T, actual, docs map[string]bool) {
	t.Helper()
	for k := range actual {
		if !docs[k] {
			t.Errorf("public surface not documented: %s", k)
		}
	}
	for k := range docs {
		if !actual[k] {
			t.Errorf("documented surface does not exist: %s", k)
		}
	}
	t.Logf("verified %d entries", len(actual))
}

func TestDocumentedEndpointsMatchRegisteredPublicRoutes(t *testing.T) {
	actual := map[string]bool{}
	// Literal registrations and the route maps both use METHOD /path strings.
	// Inspect the production AST, so a new route cannot silently escape this test.
	route := regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS) /[^ ]+$`)
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		path := file.Name()
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		ast.Inspect(source(t, path), func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok {
				if s := literal(e); route.MatchString(s) {
					actual[s] = true
				}
			}
			return true
		})
	}
	// Only the inventory bullets count; prose uses path abbreviations and queries.
	docs := documented("(?m)^- `((?:GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS) /[^`]+)`$")
	compareInventory(t, actual, docs)
}

func TestDocumentedPublicPagesAndLocalEndpoints(t *testing.T) {
	main, err := os.ReadFile("../../web/src/main.tsx")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`path === "(/[^"]+)"`).FindAllStringSubmatch(string(main), -1) {
		if strings.HasPrefix(m[1], "/dev/") {
			continue
		}
		if !strings.Contains(contract, "`"+m[1]+"`") {
			t.Errorf("SPA route missing from agent contract: %s", m[1])
		}
	}
	f := source(t, "../host/daemon/sender.go")
	paths := map[string]bool{}
	localPaths := []string{"/sender", "/local/v1/sender", "/local/v1/frame", "/local/v1/features", "/owner", "/assets/"}
	ast.Inspect(f, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok {
			v := literal(e)
			for _, path := range localPaths {
				if v == path {
					paths[v] = true
				}
			}
		}
		return true
	})
	for _, path := range localPaths {
		if !paths[path] || !strings.Contains(contract, path) {
			t.Errorf("local endpoint missing in code or contract: %s", path)
		}
	}
	t.Logf("verified %d local endpoint patterns and public SPA routes", len(paths))
}

func switchCases(f *ast.File, tag, function string) map[string]bool {
	out := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || function != "" && fn.Name.Name != function {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			s, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			var b strings.Builder // Tag matching without source-position dependence.
			switch v := s.Tag.(type) {
			case *ast.Ident:
				b.WriteString(v.Name)
			case *ast.SelectorExpr:
				if x, ok := v.X.(*ast.Ident); ok {
					b.WriteString(x.Name + "." + v.Sel.Name)
				}
			}
			if b.String() != tag {
				return true
			}
			for _, stmt := range s.Body.List {
				for _, e := range stmt.(*ast.CaseClause).List {
					if v := literal(e); v != "" {
						out[v] = true
					}
				}
			}
			return true
		})
	}
	return out
}

func TestDocumentedCommandsMatchDispatch(t *testing.T) {
	t.Run("remote", func(t *testing.T) {
		compareInventory(t, switchCases(source(t, "commands.go"), "v.Op", "validateCommand"), documented("(?m)^- HTTP command `([^`]+)`$"))
	})
	t.Run("cli", func(t *testing.T) {
		actual := switchCases(source(t, "../cli/cli.go"), "cmd", "run")
		for cmd := range switchCases(source(t, "../cli/features.go"), "cmd", "runFeature") {
			actual[cmd] = true
		}
		// main handles serve and Main handles help before entering run.
		main := source(t, "../../cmd/passstream/main.go")
		ast.Inspect(main, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok && literal(e) == "serve" {
				actual["serve"] = true
			}
			return true
		})
		cli := source(t, "../cli/cli.go")
		ast.Inspect(cli, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok && literal(e) == "help" {
				actual["help"] = true
			}
			return true
		})
		compareInventory(t, actual, documented("(?m)^- CLI command `([^`]+)`$"))
	})
	t.Run("daemon", func(t *testing.T) {
		actual := switchCases(source(t, "../host/daemon/ops.go"), "op", "dispatch")
		section := strings.Split(strings.Split(contract, "## 10. Local daemon socket")[1], "## 11.")[0]
		docs := map[string]bool{}
		for _, line := range strings.Split(section, "\n") {
			if !strings.HasPrefix(line, "| `") {
				continue
			}
			cell := strings.Split(line[2:], " | ")[0]
			for _, m := range regexp.MustCompile("`([a-z.]+)`").FindAllStringSubmatch(cell, -1) {
				if m[1] != "op" {
					docs[m[1]] = true
				}
			}
		}
		compareInventory(t, actual, docs)
	})
	// §11.1 lists the ops the daemon forwards to the page: they must equal the
	// daemon's forwarded dispatch cases (plus stop) and the page's local ops.
	t.Run("daemon to page", func(t *testing.T) {
		line := ""
		for _, l := range strings.Split(strings.Split(strings.Split(contract, "### 11.1 Messages")[1], "### 11.2")[0], "\n") {
			if strings.HasPrefix(l, "| daemon → page | `{\"type\": \"command\"") {
				line = l
			}
		}
		if line == "" {
			t.Fatal("§11.1 command row not found")
		}
		docs := map[string]bool{}
		for _, m := range regexp.MustCompile("`([a-z]+(?:\\.[a-z]+)?)`").FindAllStringSubmatch(strings.SplitN(line, "the §10 ops", 2)[1], -1) {
			docs[m[1]] = true
		}
		forwarded := map[string]bool{"stop": true}
		f := source(t, "../host/daemon/ops.go")
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "dispatch" {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if c, ok := n.(*ast.CaseClause); ok && len(c.Body) == 0 {
					for _, e := range c.List {
						if v := literal(e); v != "" {
							forwarded[v] = true
						}
					}
				}
				return true
			})
		}
		// Feature ops reach the page through featureDispatch: a case whose
		// body calls d.forward is a daemon → page command too.
		ff := source(t, "../host/daemon/features.go")
		for _, decl := range ff.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "featureDispatch" {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				c, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				calls := false
				ast.Inspect(c, func(m ast.Node) bool {
					if call, ok := m.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "forward" {
							calls = true
						}
					}
					return true
				})
				if calls {
					for _, e := range c.List {
						if v := literal(e); v != "" {
							forwarded[v] = true
						}
					}
				}
				return true
			})
		}
		compareInventory(t, forwarded, docs)
		page, e := os.ReadFile("../../web/src/sender/authority.ts")
		if e != nil {
			t.Fatal(e)
		}
		local := strings.SplitN(string(page), "export function checkLocal", 2)[1]
		pageOps := map[string]bool{}
		for _, m := range regexp.MustCompile(`case "([a-z.]+)":`).FindAllStringSubmatch(local, -1) {
			pageOps[m[1]] = true
		}
		compareInventory(t, pageOps, docs)
	})
}

func TestPublishedContractIsGeneratedAndSkillIsOnlyAPointer(t *testing.T) {
	before, e := os.ReadFile("llms.txt")
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("python3", filepath.Join("..", "..", "scripts", "generate_llms.py"), "--check")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("generator: %v %s", e, out)
	}
	after, e := os.ReadFile("llms.txt")
	if e != nil {
		t.Fatal(e)
	}
	if string(before) != string(after) || string(after) != contract {
		t.Fatal("llms.txt was stale; regenerate and commit it")
	}
	if !strings.Contains(contract, "403 local_owner_required") || !strings.Contains(contract, "--pin-stdin") || !strings.Contains(contract, "action_id") {
		t.Fatal("missing agent rules")
	}
	f := setup(t)
	r, e := f.h.Client().Get(f.h.URL + "/skill.md")
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	body, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	if len(body) > 150 || !strings.Contains(string(body), "/llms.txt") {
		t.Fatal("skill must be a minimal contract pointer")
	}
}

func TestHostControlDistinguishesSenderProofsFromOwnerHTTP(t *testing.T) {
	b, e := os.ReadFile("../../docs/HOST-CONTROL.md")
	if e != nil {
		t.Fatal(e)
	}
	text := string(b)
	if strings.Contains(text, "**The page never sends a bearer token to the loopback port.**") {
		t.Fatal("owner HTTP bearer authentication is contradicted by the global principle")
	}
	for _, required := range []string{"The sender WebSocket never sends a bearer token", "`/owner`", "token is the only authentication barrier", "Lock (and the admission freeze) gates new admissions, not capture"} {
		if !strings.Contains(text, required) {
			t.Errorf("missing security boundary: %s", required)
		}
	}
}
