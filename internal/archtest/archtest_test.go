// Package archtest holds repository-wide guard tests.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// floatAllowed lists the only places where float types may appear.
// The Prometheus client takes seconds as float64; no money goes there.
var floatAllowed = []string{
	"internal/observability/",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found: %v", err)
	}
	return root
}

// TestNoFloatsInProductionCode fails when float32, float64 or a float
// literal appears in non-test Go code outside floatAllowed. Money is
// always an integer number of minor units.
func TestNoFloatsInProductionCode(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, allowed := range floatAllowed {
			if strings.HasPrefix(rel, allowed) {
				return nil
			}
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "float32" || x.Name == "float64" {
					t.Errorf("%s: float type %s is not allowed (use money.Amount)", fset.Position(x.Pos()), x.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.FLOAT {
					t.Errorf("%s: float literal %s is not allowed", fset.Position(x.Pos()), x.Value)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 10 {
		t.Fatalf("only %d files checked: the walk is broken", checked)
	}
}

// TestNoFloatColumnsInMigrations keeps money columns integer in SQL too.
func TestNoFloatColumnsInMigrations(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	bad := regexp.MustCompile(`(?i)\b(numeric|decimal|real|double\s+precision|float[48]?|money)\b`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code, _, _ := strings.Cut(line, "--") // ignore comments
			if bad.MatchString(code) {
				t.Errorf("%s:%d: non-integer numeric type: %s", filepath.Base(f), i+1, strings.TrimSpace(line))
			}
		}
	}
}
