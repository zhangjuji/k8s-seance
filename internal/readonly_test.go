package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMethods lists selector names that mutate cluster state. Replace is
// intentionally absent: it is not a write verb this project forbids.
var writeMethods = map[string]bool{
	"Create":           true,
	"Update":           true,
	"UpdateStatus":     true,
	"Patch":            true,
	"Delete":           true,
	"DeleteCollection": true,
	"Evict":            true,
	"EvictV1":          true,
	"EvictV1beta1":     true,
}

// TestReadOnlyGuard scans every non-test Go file under internal/ and fails if
// any call expression invokes a write-class client-go method (for example
// CoreV1().Pods(ns).Delete). Requirement 2.2: the collector is read-only.
func TestReadOnlyGuard(t *testing.T) {
	root, err := os.Getwd() // go test runs with the package directory as cwd: internal/
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	var files []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("no non-test Go files found under %s; guard would be vacuous", root)
	}

	var violations []string
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if writeMethods[sel.Sel.Name] {
				pos := fset.Position(sel.Sel.Pos())
				violations = append(violations, pos.String()+": call to write method "+sel.Sel.Name)
			}
			return true
		})
	}

	for _, v := range violations {
		t.Errorf("read-only violation: %s", v)
	}
}
