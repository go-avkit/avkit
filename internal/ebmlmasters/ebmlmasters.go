// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package ebmlmasters reads which elements ebml-go treats as masters (bodies of
// more elements) and which as blocks (bodies it unlaces) out of the source of
// its element table, which it does not export.
//
// container's frame check must descend into exactly the masters ebml-go
// descends into, so the set it uses is generated from this and a test fails
// when the module's table and the generated set differ.
package ebmlmasters

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"slices"
)

// Sets are the element IDs of ebml-go's table, marker bit kept, sorted.
type Sets struct {
	Masters []uint64
	Blocks  []uint64
}

// Parse reads the `table` variable of ebml-go's elementtable.go.
func Parse(src []byte) (Sets, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "elementtable.go", src, 0)
	if err != nil {
		return Sets{}, err
	}
	var s Sets
	found := false
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "table" || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			found = true
			for _, e := range lit.Elts {
				id, kind, err := entry(e)
				if err != nil {
					return Sets{}, err
				}
				switch kind {
				case "DataTypeMaster":
					s.Masters = append(s.Masters, id)
				case "DataTypeBlock":
					s.Blocks = append(s.Blocks, id)
				}
			}
		}
	}
	if !found || len(s.Masters) == 0 || len(s.Blocks) == 0 {
		return Sets{}, fmt.Errorf("ebmlmasters: no usable element table found (%d masters, %d blocks)", len(s.Masters), len(s.Blocks))
	}
	slices.Sort(s.Masters)
	slices.Sort(s.Blocks)
	return s, nil
}

// Generate returns the source of container/matroska_masters.go for the table
// in src, ebml-go's elementtable.go at the given module version.
func Generate(src []byte, version string) ([]byte, error) {
	sets, err := Parse(src)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated from ebml-go %s by internal/ebmlmasters; DO NOT EDIT.\n", version)
	b.WriteString("// Regenerate with AVKIT_REGEN=1 go test -run TestTheWalkDescendsIntoEveryMasterEbmlGoDoes ./container\n\n")
	b.WriteString("package container\n\n")
	b.WriteString("// ebmlMasters are the elements ebml-go descends into, and ebmlBlocks the ones it\n// unlaces, by ID with the marker bit kept.\n")
	for _, set := range []struct {
		name string
		ids  []uint64
	}{{"ebmlMasters", sets.Masters}, {"ebmlBlocks", sets.Blocks}} {
		fmt.Fprintf(&b, "var %s = map[uint64]bool{\n", set.name)
		for _, id := range set.ids {
			fmt.Fprintf(&b, "\t0x%X: true,\n", id)
		}
		b.WriteString("}\n\n")
	}
	return format.Source(b.Bytes())
}

// entry reads one `Name: elementDef{[]byte{...}, DataTypeX, top}`.
func entry(e ast.Expr) (id uint64, kind string, err error) {
	kv, ok := e.(*ast.KeyValueExpr)
	if !ok {
		return 0, "", fmt.Errorf("ebmlmasters: table entry is not a key: value")
	}
	def, ok := kv.Value.(*ast.CompositeLit)
	if !ok || len(def.Elts) < 2 {
		return 0, "", fmt.Errorf("ebmlmasters: table entry has an unexpected shape")
	}
	bytesLit, ok := def.Elts[0].(*ast.CompositeLit)
	if !ok || len(bytesLit.Elts) == 0 || len(bytesLit.Elts) > 4 {
		return 0, "", fmt.Errorf("ebmlmasters: an element ID is not 1 to 4 bytes")
	}
	for _, b := range bytesLit.Elts {
		bl, ok := b.(*ast.BasicLit)
		if !ok {
			return 0, "", fmt.Errorf("ebmlmasters: an element ID byte is not a literal")
		}
		var v uint64
		if _, err := fmt.Sscan(bl.Value, &v); err != nil || v > 0xFF {
			return 0, "", fmt.Errorf("ebmlmasters: bad ID byte %s", bl.Value)
		}
		id = id<<8 | v
	}
	t, ok := def.Elts[1].(*ast.Ident)
	if !ok {
		return 0, "", fmt.Errorf("ebmlmasters: an element type is not a name")
	}
	return id, t.Name, nil
}
