// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package ebmlmasters

import (
	"slices"
	"strings"
	"testing"
)

const table = `package ebml

import "bytes"

func helper() {}

var table = elementTable{
	ElementA: elementDef{[]byte{0x1A, 0x45}, DataTypeMaster, true},
	ElementB: elementDef{[]byte{0xA3}, DataTypeBlock, false},
	ElementC: elementDef{[]byte{0xE7}, DataTypeUInt, false},
	ElementD: elementDef{[]byte{0x41, 0xE4}, DataTypeMaster, false},
}
`

func TestParseReadsMastersAndBlocks(t *testing.T) {
	s, err := Parse([]byte(table))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Masters, []uint64{0x1A45, 0x41E4}) || !slices.Equal(s.Blocks, []uint64{0xA3}) {
		t.Fatalf("Parse = %#x", s)
	}
}

func TestParseRefusesWhatItCannotRead(t *testing.T) {
	entry := func(e string) string { return "package p\nvar table = elementTable{\n" + e + "\n}\n" }
	good := "A: elementDef{[]byte{0xA3}, DataTypeBlock, false},\nB: elementDef{[]byte{0xA0}, DataTypeMaster, false},"
	for name, src := range map[string]string{
		"not Go":           "package",
		"no table":         "package p\nvar other = 1\n",
		"table not a lit":  "package p\nvar table = 1\n",
		"no master":        entry("A: elementDef{[]byte{0xA3}, DataTypeBlock, false},"),
		"no block":         entry("A: elementDef{[]byte{0xA0}, DataTypeMaster, false},"),
		"not key value":    entry(good + "\nelementDef{[]byte{1}, DataTypeInt, false},"),
		"shape":            entry(good + "\nC: 1,"),
		"short shape":      entry(good + "\nC: elementDef{1},"),
		"ID not a lit":     entry(good + "\nC: elementDef{1, DataTypeInt, false},"),
		"empty ID":         entry(good + "\nC: elementDef{[]byte{}, DataTypeInt, false},"),
		"five byte ID":     entry(good + "\nC: elementDef{[]byte{1, 2, 3, 4, 5}, DataTypeInt, false},"),
		"byte not literal": entry(good + "\nC: elementDef{[]byte{x}, DataTypeInt, false},"),
		"byte not number":  entry(good + "\nC: elementDef{[]byte{\"a\"}, DataTypeInt, false},"),
		"byte too big":     entry(good + "\nC: elementDef{[]byte{0x1FF}, DataTypeInt, false},"),
		"type not a name":  entry(good + "\nC: elementDef{[]byte{1}, 7, false},"),
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGenerateWritesBothSets(t *testing.T) {
	out, err := Generate([]byte(table), "v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ebml-go v9.9.9", "var ebmlMasters", "0x41E4: true", "var ebmlBlocks", "0xA3: true"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("generated source lacks %q:\n%s", want, out)
		}
	}
	if _, err := Generate([]byte("package"), "v"); err == nil {
		t.Error("generated from a broken table")
	}
}
