// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package mp4boxes finds out which MP4 boxes mp4ff decodes the children of, and
// how many bytes of its own each such box holds before them. The decoder table
// is not exported, so its keys are read from the source of box.go, and each type
// is then put to mp4ff itself: a box of that type, a run of zero bytes and a
// child that cannot be decoded, which mp4ff names in its error if, and only if,
// it descends to it at that offset.
//
// container's box walk must descend into exactly the boxes mp4ff does, and skip
// the header bytes it skips, so its table is generated from this and a test fails
// when mp4ff and the generated table differ.
package mp4boxes

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Eyevinn/mp4ff/mp4"
)

// maxSkip is the longest header probed: the longest known is a QuickTime audio
// entry of version 2 or a visual entry, 78 bytes.
const maxSkip = 128

// Types reads the box types of the `decoders` table of mp4ff's box.go.
func Types(src []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "box.go", src, 0)
	if err != nil {
		return nil, err
	}
	var types []string
	for _, lit := range compositeLits(file) {
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.BasicLit)
			if !ok || key.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(key.Value)
			if err != nil || len(s) != 4 {
				return nil, fmt.Errorf("mp4boxes: odd box type %s", key.Value)
			}
			types = append(types, s)
		}
	}
	if len(types) < 100 {
		return nil, fmt.Errorf("mp4boxes: no usable decoder table found (%d types)", len(types))
	}
	sort.Strings(types)
	return slices.Compact(types), nil
}

// compositeLits are the map literals assigned to `decoders` in box.go.
func compositeLits(file *ast.File) []*ast.CompositeLit {
	var out []*ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == "decoders" {
			if lit, ok := as.Rhs[0].(*ast.CompositeLit); ok {
				out = append(out, lit)
			}
		}
		return true
	})
	return out
}

// Skips are the header lengths, counted after the 8 bytes of the box header,
// at which mp4ff reads the children of a box of type typ. A QuickTime audio
// entry has one per version, which its own bytes choose. A box with none holds
// no children, or none this probe can name.
func Skips(typ string) []int {
	var out []int
	for skip := 0; skip <= maxSkip; skip++ {
		for version := 0; version <= 2; version++ {
			if descends(typ, skip, version) {
				out = append(out, skip)
				break
			}
		}
	}
	return out
}

// descends puts one box to mp4ff: skip zero bytes (the audio version, when it is
// not 0, at the offset a sample entry keeps it), then a child. Three children are
// tried, because boxes differ in how they show that they read one: a child that
// cannot be decoded (stts, with no payload) is named in the error of a parent
// that descends to it; a child that can (free) is listed by the parent's Info,
// where a parent that swallows errors (tx3g, text, tmcd) still shows it; and a
// QuickTime meta box only takes children when the first is an hdlr.
func descends(typ string, skip, version int) bool {
	payload := make([]byte, skip)
	if skip >= 10 {
		binary.BigEndian.PutUint16(payload[8:], uint16(version))
	} else if version != 0 {
		return false
	}
	for _, child := range []string{"stts", "hdlr", "free"} {
		if child == typ {
			continue
		}
		size := 8
		if child == "free" {
			size = 16
		}
		childBox := binary.BigEndian.AppendUint32(nil, uint32(size))
		childBox = append(append(childBox, child...), make([]byte, size-8)...)
		body := slices.Concat(payload, childBox)
		data := slices.Concat(binary.BigEndian.AppendUint32(nil, uint32(8+len(body))), []byte(typ), body)
		box, err := mp4.DecodeBox(0, bytes.NewReader(data))
		if err != nil {
			if strings.Contains(err.Error(), fmt.Sprintf("decode %s pos %d", child, 8+skip)) {
				return true
			}
			continue
		}
		var info bytes.Buffer
		if child == "free" && box.Info(&info, "all", "", " ") == nil && bytes.Contains(info.Bytes(), []byte("[free]")) {
			return true
		}
	}
	return false
}

// Table is every box type that has children, with its skips.
type Table map[string][]int

// Probe asks mp4ff about every type of box.go.
func Probe(src []byte) (Table, error) {
	types, err := Types(src)
	if err != nil {
		return nil, err
	}
	t := Table{}
	for _, typ := range types {
		if s := Skips(typ); len(s) > 0 {
			t[typ] = s
		}
	}
	return t, nil
}

// Generate returns the source of container/mp4_containers.go.
func Generate(src []byte, version string) ([]byte, error) {
	t, err := Probe(src)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated from mp4ff %s by internal/mp4boxes; DO NOT EDIT.\n", version)
	b.WriteString("// Regenerate with AVKIT_REGEN=1 go test -run TestTheWalkDescendsIntoEveryBoxMp4ffDoes ./container\n\n")
	b.WriteString("package container\n\n")
	b.WriteString("// mp4Containers are the boxes mp4ff decodes the children of, by type, with the\n")
	b.WriteString("// bytes each holds between its 8-byte header and them: one length per form it\n")
	b.WriteString("// has (a QuickTime audio entry has a form per version).\n")
	b.WriteString("var mp4Containers = map[string][]int{\n")
	types := make([]string, 0, len(t))
	for typ := range t {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		fmt.Fprintf(&b, "\t%s: {", strconv.Quote(typ))
		for i, s := range t[typ] {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%d", s)
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
