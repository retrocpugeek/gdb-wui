package wire

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestRequestTypesNamesEveryType reads this package's own source and insists
// that every Type* constant is in RequestTypes.
//
// It exists because being absent from that list is silent: the two tests that
// check the dispatch table and docs/protocol.md both iterate it, so a type
// missing here is a type neither of them examines. Four of them — the
// decompiler's edits — were routed, documented and shipped in exactly that
// state, and nothing failed.
func TestRequestTypesNamesEveryType(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	listed := make(map[string]bool, len(RequestTypes))
	for _, typ := range RequestTypes {
		listed[typ] = true
	}

	found := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
						continue
					}
					name := value.Names[0].Name
					if !strings.HasPrefix(name, "Type") {
						continue
					}
					lit, ok := value.Values[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					typ, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					found++
					if !listed[typ] {
						t.Errorf("%s = %q is not in RequestTypes, so no test checks that "+
							"it is routed or documented", name, typ)
					}
				}
			}
		}
	}
	// A parser that found nothing would pass silently, which is the one way
	// this test could be worse than not having it.
	if found < len(RequestTypes) {
		t.Fatalf("found %d Type constants in the source but RequestTypes has %d; "+
			"the source scan is not working", found, len(RequestTypes))
	}
}
