package deepcopy

import (
	"errors"
	"go/ast"
	"regexp"
	"sync"

	"golang.org/x/tools/go/packages"
)

// getReceiverNames returns a map of type name and its receiver name in the package.
func getReceiverNames(pkg *packages.Package) (map[string]string, error) {
	if pkg.Syntax == nil || len(pkg.Syntax) == 0 {
		return nil, errors.New("packages.Package has no Syntax.")
	}

	v := &recvVarVisitor{}
	for _, f := range pkg.Syntax {
		ast.Walk(v, f)
	}

	return v.vars, nil
}

type recvVarVisitor struct {
	sync.Mutex

	// vars is a map of type name and its receiver variable name.
	vars map[string]string
}

func (v *recvVarVisitor) Visit(n ast.Node) ast.Visitor {
	switch n := n.(type) {
	case *ast.File:
		if isCodeGenerated(n) {
			return nil
		}
		return v
	case *ast.FuncDecl:
		recv := n.Recv
		if recv == nil {
			return v
		}

		fld := recv.List[0]
		typ := fld.Type

		var expr ast.Expr
		switch typ := typ.(type) {
		case *ast.StarExpr:
			expr = typ.X
		default:
			expr = typ
		}

		if ident, ok := expr.(*ast.Ident); ok {
			typ := ident.Name
			varName := fld.Names[0].Name

			v.add(typ, varName)
		}
		return v
	}
	return nil
}

func (rv *recvVarVisitor) add(key, name string) {
	rv.Lock()
	defer rv.Unlock()

	if rv.vars == nil {
		rv.vars = map[string]string{}
	}

	// the first found name is always used.
	if _, ok := rv.vars[key]; !ok {
		rv.vars[key] = name
	}
}

var patAutoGen = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

func isCodeGenerated(f *ast.File) bool {
	for _, c := range f.Comments {
		for _, l := range c.List {
			if patAutoGen.MatchString(l.Text) {
				return true
			}
		}
	}
	return false
}
