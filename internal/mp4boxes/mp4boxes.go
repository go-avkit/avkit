// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package mp4boxes finds out which MP4 boxes mp4ff decodes the children of, and
// how many bytes of its own each such box holds before them. The decoder table
// is not exported, so its keys are read from the source of box.go, and each type
// is then put to mp4ff itself: a box of that type, a prefix of bytes and a child,
// which mp4ff shows if, and only if, it descends to it after that prefix.
//
// container's box walk must descend into every box mp4ff does, at every offset
// mp4ff may read its children from, so its table is generated from this and a
// test fails when mp4ff and the generated table differ.
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

// maxSkip is the longest prefix probed: a visual sample entry holds 78 bytes and a
// QuickTime version 2 audio entry 64 before their children.
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

// prefixes are the bytes put before the child in each probe of a header length:
// zeros, a QuickTime audio version of 1, and of 2 with the marker its decoder
// asks for (0x7F000000 at payload offset 44).
func prefixes(skip int) [][]byte {
	out := [][]byte{make([]byte, skip)}
	if skip >= 10 {
		v1 := make([]byte, skip)
		binary.BigEndian.PutUint16(v1[8:], 1)
		v2 := make([]byte, skip)
		binary.BigEndian.PutUint16(v2[8:], 2)
		if skip >= 48 {
			binary.BigEndian.PutUint32(v2[44:], 0x7f000000)
		}
		out = append(out, v1, v2)
	}
	return out
}

// Skips are the header lengths, counted after the 8 bytes of the box header,
// at which mp4ff reads the children of a box of type typ. A QuickTime audio
// entry has one per version, which its own bytes choose. A box with none holds
// no children, or none this probe can name.
func Skips(typ string) []int {
	var out []int
	for skip := 0; skip <= maxSkip; skip++ {
		for _, prefix := range prefixes(skip) {
			if descends(typ, prefix) {
				out = append(out, skip)
				break
			}
		}
	}
	return out
}

// StringLead is the number of bytes a box holds before three NUL-terminated
// strings of any length, which precede its children, or -1 when it is not such a
// box. Its children are then at no fixed offset: a namespace string of any
// length moves them.
func StringLead(typ string, skips []int) int {
	for _, skip := range skips {
		lead := skip - 3
		if lead < 0 {
			continue
		}
		found := true
		for _, n := range []int{1, 4, 9} {
			prefix := make([]byte, lead)
			for range 3 {
				prefix = append(append(prefix, bytes.Repeat([]byte{'A'}, n)...), 0)
			}
			found = found && descends(typ, prefix)
		}
		if found {
			return lead
		}
	}
	return -1
}

// descends puts one box to mp4ff: the prefix, then a child. Three children are
// tried, because boxes differ in how they show that they read one: a child that
// cannot be decoded (stts, with no payload) is named in the error of a parent
// that descends to it; a child that can (free) is listed by the parent's Info,
// where a parent that swallows errors (tx3g, text, tmcd) still shows it; and a
// QuickTime meta box only takes children when the first is an hdlr.
func descends(typ string, prefix []byte) bool {
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
		body := slices.Concat(prefix, childBox)
		data := slices.Concat(binary.BigEndian.AppendUint32(nil, uint32(8+len(body))), []byte(typ), body)
		box, err := mp4.DecodeBox(0, bytes.NewReader(data))
		if err != nil {
			if strings.Contains(err.Error(), fmt.Sprintf("decode %s pos %d", child, 8+len(prefix))) {
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

// holdsAnyKind says whether a box of type typ decodes a child of a type it has
// never heard of as a container, as ilst does for its items (the item types are
// the tags' own names), when it holds data, mean, name or itif boxes. The probe
// gives it a child "zzzz" that holds a data box, and looks for that data box in
// the parent's account of itself.
func holdsAnyKind(typ string, skips []int) bool {
	inner := binary.BigEndian.AppendUint32(nil, 16)
	inner = append(append(inner, "data"...), make([]byte, 8)...)
	kid := binary.BigEndian.AppendUint32(nil, uint32(8+len(inner)))
	kid = append(append(kid, "zzzz"...), inner...)
	for _, skip := range skips {
		for _, prefix := range prefixes(skip) {
			body := slices.Concat(prefix, kid)
			data := slices.Concat(binary.BigEndian.AppendUint32(nil, uint32(8+len(body))), []byte(typ), body)
			box, err := mp4.DecodeBox(0, bytes.NewReader(data))
			var info bytes.Buffer
			if err == nil && box.Info(&info, "all", "", " ") == nil && bytes.Contains(info.Bytes(), []byte("[data]")) {
				return true
			}
		}
	}
	return false
}

// Forms are what the probe learned: Skips by box type; for the boxes that hold
// strings before their children, the bytes before the strings; and the boxes
// whose children are all containers, whatever their type.
type Forms struct {
	Skips   map[string][]int
	Strings map[string]int
	AnyKind map[string]bool
}

// Probe asks mp4ff about every type of box.go.
func Probe(src []byte) (Forms, error) {
	types, err := Types(src)
	if err != nil {
		return Forms{}, err
	}
	f := Forms{Skips: map[string][]int{}, Strings: map[string]int{}, AnyKind: map[string]bool{}}
	for _, typ := range types {
		s := Skips(typ)
		if len(s) == 0 {
			continue
		}
		f.Skips[typ] = s
		if lead := StringLead(typ, s); lead >= 0 {
			f.Strings[typ] = lead
		}
		if holdsAnyKind(typ, s) {
			f.AnyKind[typ] = true
		}
	}
	return f, nil
}

// Generate returns the source of container/mp4_containers.go.
func Generate(src []byte, version string) ([]byte, error) {
	f, err := Probe(src)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated from mp4ff %s by internal/mp4boxes; DO NOT EDIT.\n", version)
	b.WriteString("// Regenerate with AVKIT_REGEN=1 go test -run TestTheWalkDescendsIntoEveryBoxMp4ffDoes ./container\n\n")
	b.WriteString("package container\n\n")
	b.WriteString("// mp4Containers are the boxes mp4ff decodes the children of, by type, with the\n")
	b.WriteString("// bytes each may hold between its 8-byte header and them: every offset mp4ff\n")
	b.WriteString("// reads children from (a QuickTime audio entry has one per version).\n")
	b.WriteString("var mp4Containers = map[string][]int{\n")
	types := make([]string, 0, len(f.Skips))
	for typ := range f.Skips {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		fmt.Fprintf(&b, "\t%s: {", strconv.Quote(typ))
		for i, s := range f.Skips[typ] {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%d", s)
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("// mp4StringForms are the boxes that hold three NUL-terminated strings of any\n")
	b.WriteString("// length before their children, by the bytes before the strings: their children\n")
	b.WriteString("// are at no fixed offset.\n")
	b.WriteString("var mp4StringForms = map[string]int{\n")
	types = types[:0]
	for typ := range f.Strings {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		fmt.Fprintf(&b, "\t%s: %d,\n", strconv.Quote(typ), f.Strings[typ])
	}
	b.WriteString("}\n\n")
	b.WriteString("// mp4AnyKind are the boxes whose children are all containers, whatever their\n")
	b.WriteString("// type (ilst: its items are named by the tags).\n")
	b.WriteString("var mp4AnyKind = map[string]bool{\n")
	types = types[:0]
	for typ := range f.AnyKind {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		fmt.Fprintf(&b, "\t%s: true,\n", strconv.Quote(typ))
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
