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

// ritePackage is the only package allowed to write to the cluster
// (Requirement 19.5), and webPackage the only one allowed to import it.
const (
	ritePackage = "github.com/zhangjuji/k8s-seance/internal/rite"
	webPackage  = "github.com/zhangjuji/k8s-seance/internal/web"
	modulePath  = "github.com/zhangjuji/k8s-seance"
)

// TestReadOnlyGuard scans every non-test Go file under internal/ and fails if
// any call expression invokes a write-class client-go method (for example
// CoreV1().Pods(ns).Delete). Requirement 2.2: the collector is read-only.
// internal/rite, the opt-in resurrection (Requirement 19), is the one exception.
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
			if path == filepath.Join(root, "rite") {
				return filepath.SkipDir
			}
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

// TestRiteIsTheOnlyWriter checks that the exemption above is not empty, so the
// guard still sees the write calls it skips, and that only internal/web
// imports internal/rite, in every non-test Go file of the module.
func TestRiteIsTheOnlyWriter(t *testing.T) {
	internalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	moduleRoot := filepath.Dir(internalDir)
	fset := token.NewFileSet()

	riteWrites := 0
	riteFiles, err := filepath.Glob(filepath.Join(internalDir, "rite", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range riteFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && writeMethods[sel.Sel.Name] {
					riteWrites++
				}
			}
			return true
		})
	}
	if riteWrites == 0 {
		t.Fatal("internal/rite has no write calls; the exemption is stale")
	}

	err = filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(moduleRoot, filepath.Dir(path))
		pkg := modulePath
		if rel != "." {
			pkg += "/" + filepath.ToSlash(rel)
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == ritePackage && pkg != webPackage && pkg != ritePackage {
				t.Errorf("%s imports internal/rite; only internal/web may", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
