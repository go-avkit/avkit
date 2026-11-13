// Copyright (c) 2026, go-avkit
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"strings"
	"testing"
)

// freeformItem is the `----` item an Apple encoder writes: mean, name, data.
func freeformItem(mean, name string, value []byte) []byte {
	version := make([]byte, 4)
	return box("----",
		box("mean", version, []byte(mean)),
		box("name", version, []byte(name)),
		dataBox(ilstUTF8, value))
}

const smpbText = " 00000000 00000840 000002C0 0000000000007D00 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000"

func TestParseGapless(t *testing.T) {
	want := &Gapless{Delay: 0x840, Padding: 0x2C0, Samples: 0x7D00}
	for name, c := range map[string]struct {
		in   string
		want *Gapless
	}{
		"afconvert":          {smpbText, want},
		"NUL terminated":     {smpbText + "\x00", want},
		"text after the NUL": {"0 840 2c0 7\x00d00", &Gapless{Delay: 0x840, Padding: 0x2C0, Samples: 7}},
		"four fields":        {"0 840 2c0 7d00", want},
		"lower case, tabs":   {"0\t840\t2c0\t7d00\n", want},
		"more than four":     {"0 840 2c0 7d00 zzz", want},
		"limit":              {"0 840 2c0 10000000000", &Gapless{Delay: 0x840, Padding: 0x2C0, Samples: 1 << 40}},
		"sixteen digits":     {"0 840 2c0 000000000000abcd", &Gapless{Delay: 0x840, Padding: 0x2C0, Samples: 0xabcd}},
		"reserved is free":   {"ffffffffffffffff 1 2 3", &Gapless{Delay: 1, Padding: 2, Samples: 3}},
		"empty":              {"", nil},
		"three fields":       {"0 840 2c0", nil},
		"not hex":            {"0 84g 2c0 7d00", nil},
		"a prefix":           {"0 0x840 2c0 7d00", nil},
		"a sign":             {"0 +840 2c0 7d00", nil},
		"seventeen digits":   {"0 00000000000000840 2c0 7d00", nil},
		"reserved too wide":  {"00000000000000000 840 2c0 7d00", nil},
		"delay beyond limit": {"0 10000000001 2c0 7d00", nil},
		"padding beyond":     {"0 840 10000000001 7d00", nil},
		"samples beyond":     {"0 840 2c0 10000000001", nil},
		"only a NUL":         {"\x00 0 840 2c0 7d00", nil},
	} {
		got := parseGapless(c.in)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s: parseGapless(%q) = %+v, want %+v", name, c.in, got, c.want)
		}
	}
}

func TestFreeform(t *testing.T) {
	v := make([]byte, 4)
	name := box("name", v, []byte("iTunSMPB"))
	mean := box("mean", v, []byte("com.apple.iTunes"))
	data := dataBox(ilstUTF8, []byte("text"))
	for label, c := range map[string]struct {
		parts []byte
		ok    bool
		name  string
		value string
	}{
		"mean name data":      {append(append(append([]byte{}, mean...), name...), data...), true, "iTunSMPB", "text"},
		"data name mean":      {append(append(append([]byte{}, data...), name...), mean...), true, "iTunSMPB", "text"},
		"no mean":             {append(append([]byte{}, name...), data...), false, "", ""},
		"no name":             {append(append([]byte{}, mean...), data...), false, "", ""},
		"no data":             {append(append([]byte{}, mean...), name...), false, "", ""},
		"first name wins":     {append(append(append(append([]byte{}, mean...), name...), box("name", v, []byte("other"))...), data...), true, "iTunSMPB", "text"},
		"first data wins":     {append(append(append(append([]byte{}, mean...), name...), data...), dataBox(ilstUTF8, []byte("later"))...), true, "iTunSMPB", "text"},
		"short mean":          {append(append(append([]byte{}, box("mean", []byte{0})...), name...), data...), false, "", ""},
		"short name":          {append(append(append([]byte{}, mean...), box("name", v[:3])...), data...), false, "", ""},
		"short data":          {append(append(append([]byte{}, mean...), name...), box("data", v)...), false, "", ""},
		"unknown box between": {append(append(append(append([]byte{}, mean...), box("free")...), name...), data...), true, "iTunSMPB", "text"},
		"broken box stops":    {append(append(append([]byte{}, mean...), name...), 0, 0, 0, 3, 'x'), false, "", ""},
		"empty":               {nil, false, "", ""},
	} {
		gotName, value, ok := freeform(c.parts)
		if ok != c.ok || ok && (gotName != c.name || string(value) != c.value) {
			t.Errorf("%s: freeform = %q %q %v, want %q %q %v", label, gotName, value, ok, c.name, c.value, c.ok)
		}
	}
}

func TestIlstReadsITunSMPB(t *testing.T) {
	var md Metadata
	item := freeformItem("com.apple.iTunes", "iTunSMPB", []byte(smpbText+"\x00"))
	md.addIlstItem(item)
	want := Gapless{Delay: 0x840, Padding: 0x2C0, Samples: 0x7D00}
	if md.Gapless == nil || *md.Gapless != want {
		t.Fatalf("Gapless = %+v, want %+v", md.Gapless, want)
	}
	if len(md.mp4Items) != 1 || string(md.mp4Items[0]) != string(item) {
		t.Fatalf("the item is not kept whole: %q", md.mp4Items)
	}
	// The first one counts; a later one is kept whole and read as nothing.
	md.addIlstItem(freeformItem("com.apple.iTunes", "iTunSMPB", []byte("0 1 2 3")))
	if *md.Gapless != want || len(md.mp4Items) != 2 {
		t.Fatalf("a second iTunSMPB changed %+v, %d items", md.Gapless, len(md.mp4Items))
	}
	// Other freeform items, and an iTunSMPB that does not read, set nothing.
	var other Metadata
	other.addIlstItem(freeformItem("com.apple.iTunes", "iTunNORM", []byte(smpbText)))
	other.addIlstItem(freeformItem("com.apple.iTunes", "iTunSMPB", []byte("not four fields")))
	other.addIlstItem(box("----", box("name", make([]byte, 4), []byte("iTunSMPB")), dataBox(ilstUTF8, []byte(smpbText)))) // no mean
	if other.Gapless != nil || len(other.mp4Items) != 3 {
		t.Fatalf("Gapless = %+v with %d items kept", other.Gapless, len(other.mp4Items))
	}
	if (Metadata{Gapless: &want}).IsZero() {
		t.Fatal("a Metadata holding only a Gapless is zero")
	}
}

// TestDemuxReadsITunSMPB: an AAC file afconvert made (`afconvert -f m4af -d aac -b 64000`
// from one second of audio, macOS 27): ffprobe reads its iTunSMPB as
// " 00000000 00000840 000002C0 0000000000007D00 ..." and ffmpeg logs
// "Parsed iTunSMPB: priming 2112, remainder 704 samples 32000".
func TestDemuxReadsITunSMPB(t *testing.T) {
	data := readFixture(t, "gapless-afconvert.m4a")
	want := Gapless{Delay: 2112, Padding: 704, Samples: 32000}
	f, err := Demux(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.Metadata.Gapless == nil || *f.Metadata.Gapless != want {
		t.Fatalf("Demux: Gapless = %+v, want %+v", f.Metadata.Gapless, want)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if g := r.File().Metadata.Gapless; g == nil || *g != want {
		t.Fatalf("NewReader: Gapless = %+v, want %+v", g, want)
	}
	if !strings.Contains(string(data), "iTunSMPB") {
		t.Fatal("the fixture has no iTunSMPB")
	}
	// A remux carries the item, so the copy reads it again.
	var out bytes.Buffer
	if err := Remux(&out, r); err != nil {
		t.Fatalf("Remux: %v", err)
	}
	bf, err := Demux(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if bf.Metadata.Gapless == nil || *bf.Metadata.Gapless != want {
		t.Fatalf("after Remux: Gapless = %+v, want %+v", bf.Metadata.Gapless, want)
	}
}

func FuzzGapless(f *testing.F) {
	f.Add(smpbText)
	f.Add(smpbText + "\x00")
	f.Add("0 840 2c0 7d00")
	f.Add("0 10000000001 2c0 7d00")
	f.Add("00000000000000000 840 2c0 7d00")
	f.Fuzz(func(t *testing.T, text string) {
		g := parseGapless(text)
		if g == nil {
			return
		}
		if g.Delay > maxGapless || g.Padding > maxGapless || g.Samples > maxGapless {
			t.Fatalf("%+v beyond the limit", g)
		}
		// What was read reads again from its own text.
		again := parseGapless(" 00000000 " + hex16(g.Delay) + " " + hex16(g.Padding) + " " + hex16(g.Samples))
		if again == nil || *again != *g {
			t.Fatalf("%+v reads as %+v from its own text", g, again)
		}
	})
}

func hex16(v uint64) string {
	const digits = "0123456789ABCDEF"
	b := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&15]
		v >>= 4
	}
	return string(b)
}
