// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package mp4boxes

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// dispatchTables are the variables mp4ff looks a box's decoder up in: a function
// that mentions one decodes a box it was not given, which is to say a child.
var dispatchTables = map[string]bool{"decoders": true, "decodersSR": true, "boxTypes": true}

// Containers reads the source of mp4ff's mp4 package in dir and returns the box
// types whose decoder can decode other boxes: the types the decoder tables map to
// a function that, through any chain of calls in the package, reaches the lookup
// of a decoder. A type that is not in the result has a decoder that never does,
// and mp4ff never reads a box from inside such a box.
//
// The chain is followed by name alone, over every function and method of the
// package, so a name two of them share merges them: the result can hold a type
// that has no children, and never lacks one that has. That is the direction a
// bound needs. A decoder that is not a named function (a literal) is taken to be
// a container, for the same reason.
func Containers(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	refs := map[string]map[string]bool{}
	var tables []*ast.CompositeLit
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		tables = append(tables, decoderTables(file)...)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "init" {
				continue // init builds the tables, and so mentions every decoder
			}
			set := refs[fn.Name.Name]
			if set == nil {
				set = map[string]bool{}
				refs[fn.Name.Name] = set
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					set[id.Name] = true
				}
				return true
			})
		}
	}
	reaches := map[string]bool{}
	for name, set := range refs {
		for table := range dispatchTables {
			if set[table] {
				reaches[name] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for name, set := range refs {
			if reaches[name] {
				continue
			}
			for callee := range set {
				if reaches[callee] {
					reaches[name], changed = true, true
					break
				}
			}
		}
	}
	out := map[string]bool{}
	seen := 0
	for _, lit := range tables {
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.BasicLit)
			if !ok || key.Kind != token.STRING {
				continue
			}
			typ, _ := strconv.Unquote(key.Value) // a string literal of the parser always is
			seen++
			if decoderReaches(kv.Value, reaches) {
				out[typ] = true
			}
		}
	}
	if seen < 100 || len(out) == 0 {
		return nil, fmt.Errorf("mp4boxes: no usable decoder tables in %s (%d entries, %d containers)", dir, seen, len(out))
	}
	return out, nil
}

// decoderTables are the map literals assigned to decoders or decodersSR.
func decoderTables(file *ast.File) []*ast.CompositeLit {
	var out []*ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && (id.Name == "decoders" || id.Name == "decodersSR") {
			if lit, ok := as.Rhs[0].(*ast.CompositeLit); ok {
				out = append(out, lit)
			}
		}
		return true
	})
	return out
}

// decoderReaches says whether the decoder an entry names reaches a lookup: a
// named function, by the chain computed, and anything else, to be safe.
func decoderReaches(v ast.Expr, reaches map[string]bool) bool {
	switch x := v.(type) {
	case *ast.Ident:
		return reaches[x.Name]
	case *ast.SelectorExpr:
		return reaches[x.Sel.Name]
	}
	return true
}

// Sorted returns the keys of a set in order.
func Sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
