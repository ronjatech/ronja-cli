package commands

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// headerlessFolderClients are the functions that build a client beside an open
// folder and deliberately send no X-Ronja-Space, keyed "file:func", each with
// why. Every other such client must be f.client(), which carries the stack's
// Space. The stackless sites (db migrate, the declaring flows, import, the
// lifts, the edge checker and the Space lookup) open no folder where they
// build their client, so the guard does not reach them.
var headerlessFolderClients = map[string]string{
	"dataapp_fork.go:newDataAppForkCmd": "the kit is the instance's public docs surface, which is in no Space",
	"bind.go:planBind":                  "its Space lookup reads features and Spaces by id, which answer in any Space, and the stack it places may still be being declared",
}

// Every client built where a folder is open sends the folder's stack's Space,
// unless headerlessFolderClients names it: a bare api.New or newClient there
// would send a Dev stack's request into the default Space.
func TestFolderClientsCarryTheStacksSpace(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var bare []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := name + ":" + fn.Name.Name
			var enclosing []ast.Node
			var walk func(n ast.Node) bool
			walk = func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncLit:
					enclosing = append(enclosing, n)
					ast.Inspect(n.Body, walk)
					enclosing = enclosing[:len(enclosing)-1]
					return false
				case *ast.CallExpr:
					if !isClientConstructor(n) {
						return true
					}
					open := hasFolder(fn.Recv, fn.Type, fn.Body)
					for _, lit := range enclosing {
						lit := lit.(*ast.FuncLit)
						open = open || hasFolder(nil, lit.Type, lit.Body)
					}
					if !open {
						return true
					}
					if _, allowed := headerlessFolderClients[key]; allowed {
						seen[key] = true
						return true
					}
					bare = append(bare, fset.Position(n.Pos()).String()+" ("+key+")")
				}
				return true
			}
			ast.Inspect(fn.Body, walk)
		}
	}
	sort.Strings(bare)
	for _, at := range bare {
		t.Errorf("%s builds a client beside an open folder without the stack's Space: use f.client(), or allowlist it with its reason", at)
	}
	for key := range headerlessFolderClients {
		if !seen[key] {
			t.Errorf("headerlessFolderClients names %s, which builds no client beside a folder: drop it", key)
		}
	}
}

// isClientConstructor reports a call to api.New or newClient, or a Space
// lookup built from a folder's credential (newSpaceLookup(ctx, f.Resolved)),
// which builds its own client.
func isClientConstructor(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if fun.Name == "newSpaceLookup" && len(call.Args) > 0 {
			arg, ok := call.Args[len(call.Args)-1].(*ast.SelectorExpr)
			return ok && arg.Sel.Name == "Resolved"
		}
		return fun.Name == "newClient"
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		return ok && pkg.Name == "api" && fun.Sel.Name == "New"
	}
	return false
}

// folderTypes are the parameter types that hand a function an open folder: a
// *folder, and the tree commands' *plannedFolder and discoveredFolder.
var folderTypes = map[string]bool{"folder": true, "plannedFolder": true, "discoveredFolder": true}

// hasFolder reports a function that takes an open folder (as receiver or
// parameter) or opens one into a local.
func hasFolder(recv *ast.FieldList, typ *ast.FuncType, body *ast.BlockStmt) bool {
	for _, fields := range []*ast.FieldList{recv, typ.Params} {
		if fields == nil {
			continue
		}
		for _, field := range fields.List {
			t := field.Type
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
			if id, ok := t.(*ast.Ident); ok && folderTypes[id.Name] {
				return true
			}
		}
	}
	opens := false
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || opens {
			return !opens
		}
		for _, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			if id, ok := call.Fun.(*ast.Ident); ok && (strings.HasPrefix(id.Name, "openFolder") || strings.HasPrefix(id.Name, "openActing")) {
				opens = true
			}
		}
		return !opens
	})
	return opens
}
