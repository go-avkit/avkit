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

func mp4ffDir(t *testing.T) (dir, version string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}} {{.Version}}", "github.com/Eyevinn/mp4ff").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dir, version, _ = strings.Cut(strings.TrimSpace(string(out)), " ")
	return filepath.Join(dir, "mp4"), version
}

func TestGenerateWritesTheTable(t *testing.T) {
	dir, version := mp4ffDir(t)
	out, err := Generate(dir, version)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DO NOT EDIT", version, `"stsd":`, "mp4StringForms", `"stpp":`, "mp4AnyKind", `"ilst":`, `"text":`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the generated file lacks %q", want)
		}
	}
	// A directory with no box.go, and one whose box.go is not Go.
	empty := t.TempDir()
	if _, err := Generate(empty, version); err == nil {
		t.Error("Generate accepted a directory with no box.go")
	}
	if err := os.WriteFile(filepath.Join(empty, "box.go"), []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(empty, version); err == nil {
		t.Error("Generate accepted a box.go that is not Go")
	}
	// A box.go that does read, in a directory whose other sources do not.
	src, _ := mp4ffSource(t)
	if err := os.WriteFile(filepath.Join(empty, "box.go"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "bad.go"), []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(empty, version); err == nil {
		t.Error("Generate accepted a directory with a source that is not Go")
	}
}

// fakePackage writes a package whose decoder table has n entries after the ones
// given, and the functions given.
func fakePackage(t *testing.T, entries, funcs string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("package mp4\nvar decoders map[string]D\nfunc init() {\n\tdecoders = map[string]D{\n")
	for i := range 120 {
		fmt.Fprintf(&b, "\t\t\"t%03d\": Leaf,\n", i)
	}
	b.WriteString(entries)
	b.WriteString("\t}\n}\n")
	b.WriteString(funcs)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "box.go"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// Tests and other directories are not read.
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.go"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestContainersFollowTheCallsToTheLookup(t *testing.T) {
	dir := fakePackage(t, `
		"box1": Direct,
		"box2": Indirect,
		"box3": pkg.Qualified,
		"box4": func() {},
		"box5": Unrelated,
		7: Leaf,
		Leaf,
		name: Leaf,
	`, `
func Direct() { _ = decoders["x"] }
func Indirect() { helper() }
func helper() { Direct() }
func Qualified() { Direct() }
func Unrelated() {}
func Leaf() {}
type T struct{}
func (T) Method()
`)
	got, err := Containers(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"box1", "box2", "box3", "box4"}
	if s := Sorted(got); !slices.Equal(s, want) {
		t.Errorf("Containers = %v, want %v", s, want)
	}
}

func TestContainersRefuseWhatTheyCannotRead(t *testing.T) {
	if _, err := Containers(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing directory")
	}
	if _, err := Containers(fakePackage(t, "", "func Leaf() {}\n")); err == nil {
		t.Error("a table with no container")
	}
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "a.go"), []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Containers(bad); err == nil {
		t.Error("a source that is not Go")
	}
}

func TestEveryBoxTheProbeSeesReadChildrenTheSourceSaysSo(t *testing.T) {
	dir, _ := mp4ffDir(t)
	static, err := Containers(dir)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := mp4ffSource(t)
	probed, err := Probe(src)
	if err != nil {
		t.Fatal(err)
	}
	for typ := range probed.Skips {
		if !static[typ] {
			t.Errorf("%q reads children when probed and not by its source", typ)
		}
	}
	for _, typ := range []string{"text", "tx3g", "mp4a", "stpp", "ilst", "moov"} {
		if !static[typ] {
			t.Errorf("%q is not a container by its source", typ)
		}
	}
	if static["mdat"] || static["free"] || static["stts"] {
		t.Error("a leaf box is a container by its source")
	}
}
