// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package mp4boxes

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func mp4ffSource(t *testing.T) (src []byte, version string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}} {{.Version}}", "github.com/Eyevinn/mp4ff").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dir, version, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	src, err = os.ReadFile(filepath.Join(dir, "mp4", "box.go"))
	if err != nil {
		t.Fatal(err)
	}
	return src, version
}

func TestTypesAreReadFromTheDecoderTable(t *testing.T) {
	src, _ := mp4ffSource(t)
	types, err := Types(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"moov", "stsd", "avc1", "\xa9ART"} {
		if !slices.Contains(types, want) {
			t.Errorf("%q is not among the %d types", want, len(types))
		}
	}
}

// A decoders table, padded to a usable size, with the shapes the reader skips.
func fakeSource(extra string) []byte {
	var b strings.Builder
	b.WriteString("package mp4\nfunc init() {\n\tother, two := 1, 2\n\tdecoders = f()\n\tdecoders = map[string]D{\n")
	for i := range 120 {
		fmt.Fprintf(&b, "\t\t\"t%03d\": X,\n", i)
	}
	b.WriteString(extra)
	b.WriteString("\t}\n}\n")
	return []byte(b.String())
}

func TestTypesRefuseWhatTheyCannotRead(t *testing.T) {
	for name, src := range map[string][]byte{
		"not Go":         []byte("package"),
		"too few types":  []byte("package mp4\nfunc init() { decoders = map[string]D{\"moov\": X} }\n"),
		"no table":       []byte("package mp4\nvar x = 1\n"),
		"a type of five": fakeSource("\t\t\"moov5\": X,\n"),
	} {
		if types, err := Types(src); err == nil {
			t.Errorf("%s: %d types, want an error", name, len(types))
		}
	}
	// Keys that are not strings, and values that are not entries, are not types.
	types, err := Types(fakeSource("\t\tname: X,\n\t\t7: X,\n\t\tY,\n"))
	if err != nil || len(types) != 120 {
		t.Errorf("Types = %d, %v; want the 120 strings", len(types), err)
	}
}

func TestSkipsOfKnownBoxes(t *testing.T) {
	for typ, want := range map[string][]int{
		"moov": {0}, "stsd": {8}, "avc1": {78}, "mp4a": {28, 44, 64}, "meta": {0, 4},
		"tx3g": {38}, "mvhd": nil, "free": nil, "stts": nil,
	} {
		if got := Skips(typ); !slices.Equal(got, want) {
			t.Errorf("Skips(%q) = %v, want %v", typ, got, want)
		}
	}
}

// A box with strings before its children has them at no fixed offset.
func TestStringLeadIsFoundForTheBoxesWithStrings(t *testing.T) {
	for typ, want := range map[string]int{"stpp": 8, "stpc": 8, "moov": -1, "stsd": -1, "avc1": -1} {
		if got := StringLead(typ, Skips(typ)); got != want {
			t.Errorf("StringLead(%q) = %d, want %d", typ, got, want)
		}
	}
}

func TestGenerateWritesTheTable(t *testing.T) {
	src, version := mp4ffSource(t)
	out, err := Generate(src, version)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DO NOT EDIT", version, `"stsd":`, `{78}`, `{28, 44, 64}`, "mp4StringForms", `"stpp":`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the generated file lacks %q", want)
		}
	}
	if _, err := Generate([]byte("package"), version); err == nil {
		t.Error("Generate accepted a source that is not Go")
	}
}
