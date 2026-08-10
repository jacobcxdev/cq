package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionSourceHasNoLegacyCodexCredentialWriters(t *testing.T) {
	forbidden := map[string]struct{}{
		"PersistCodexAccount":              {},
		"codexPersistFunc":                 {},
		"codexRefreshFunc":                 {},
		"firstCodexAccessTokenWithRefresh": {},
	}
	for _, root := range []string{".", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				ident, ok := node.(*ast.Ident)
				if !ok {
					return true
				}
				if _, exists := forbidden[ident.Name]; exists {
					t.Errorf("%s references retired direct credential writer %q", fset.Position(ident.Pos()), ident.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk production Go source under %s: %v", root, err)
		}
	}
}
