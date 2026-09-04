package daemon_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// executorReadMethods reads internal/operations/executor.go and returns the
// Executor methods that run their operation through ExecuteRead. Derived from
// the source rather than listed here, so a convenience method that quietly
// switches from ExecuteRead to Execute (or a new one that wraps Execute) is
// classified correctly without anyone updating this test.
func executorReadMethods(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "operations", "executor.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}
	read := map[string]bool{"ExecuteRead": true}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ExecuteRead" {
				read[fn.Name.Name] = true
			}
			return true
		})
	}
	return read
}

// fastHandlers are the write-path handlers allowed to keep the server's
// WriteTimeout, each with the reason it is bounded. A listed handler that
// starts clearing the deadline, or stops handing a write to the executor,
// fails the test too, so the list cannot rot.
var fastHandlers = map[string]string{
	"handleSetPassword":               "one ALTER ROLE over the bridge",
	"handleCreateDatabase":            "CREATE DATABASE plus at most one CREATE ROLE",
	"handleAddDatabaseUser":           "a fixed set of GRANT statements",
	"handleResetDatabaseUserPassword": "one ALTER ROLE",
	"handleCronBackupCreate":          "store writes only",
	"handleCronBackupDelete":          "store writes only",
	"handleRemoveLocalBackup":         "an unlink and a store write",
	"handleSnapshotRemoveLocal":       "an unlink and a store write",
	"handleDebugBackupTimeShift":      "debug build only; store writes",
}

// Two halves of one contract, both read off the source with go/parser:
//
//   - A handler that hands a WRITE operation to the executor passes
//     context.Background(), never r.Context(). Operations are uninterruptible
//     by design (see CLAUDE.md, "Operations are uninterruptible by design"):
//     a client's timeout or disconnect must not abort a half-done pg_dump,
//     restore or container rebuild.
//   - Because such an operation cannot be interrupted, the handler must also
//     clear the response write deadline first — or when it outlives the
//     server's WriteTimeout the CLI reads a bare EOF while the daemon carries
//     on, and the operator is left to find out whether the thing happened
//     (review item 22; delete-db-user was the one that slipped through). A
//     handler that is genuinely bounded is listed in fastHandlers instead.
//
// The convenience methods (CreateDatabaseOp and friends) count as writes
// unless executor.go shows them going through ExecuteRead. Read-only handlers
// are exempt from both halves: they never take the lock.
func TestWritePathHandlersClearTheWriteDeadline(t *testing.T) {
	files, err := filepath.Glob("handlers*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no handler files found: %v", err)
	}
	readMethods := executorReadMethods(t)
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "handle") {
				continue
			}
			write, background, clears := false, true, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch exprPath(sel.X) {
				case "s.executor":
					if !readMethods[sel.Sel.Name] {
						write = true
						background = background && passesBackgroundContext(call)
					}
				case "s":
					if sel.Sel.Name == "clearWriteDeadline" {
						clears = true
					}
				}
				return true
			})
			if !write {
				continue
			}
			name := fn.Name.Name
			seen[name] = true
			if !background {
				t.Errorf("%s in %s hands a write operation to the executor on the request context; write-path handlers pass context.Background() so a client disconnect cannot abort the operation", name, file)
			}
			reason, listed := fastHandlers[name]
			switch {
			case clears && listed:
				t.Errorf("%s clears the write deadline but is also listed as fast (%q); remove it from fastHandlers", name, reason)
			case !clears && !listed:
				t.Errorf("%s in %s hands a write operation to the executor on a background context without calling s.clearWriteDeadline; "+
					"clear it, or list it in fastHandlers with the reason it cannot outlive WriteTimeout", name, file)
			}
		}
	}
	for name := range fastHandlers {
		if !seen[name] {
			t.Errorf("fastHandlers lists %s, which no longer hands a write to the executor (or was renamed); remove it", name)
		}
	}
}

// exprPath renders a selector chain such as s.executor as "s.executor".
func exprPath(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprPath(v.X) + "." + v.Sel.Name
	}
	return ""
}

// passesBackgroundContext reports whether any argument is context.Background().
func passesBackgroundContext(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		inner, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && exprPath(sel) == "context.Background" {
			return true
		}
	}
	return false
}
