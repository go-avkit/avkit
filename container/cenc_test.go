// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Fixed vectors: the committed files under testdata/cenc were written with these,
// and ffmpeg decrypted them with -decryption_key.
const (
	cencKeyHex = "000102030405060708090a0b0c0d0e0f"
	cencKIDHex = "11223344556677889900aabbccddeeff"
	cencIV8Hex = "a0a1a2a3a4a5a6a7"
	cencIVHex  = "b0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
)

func cencHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cencKey(t testing.TB) ContentKey {
	t.Helper()
	k, err := NewContentKey(cencHex(t, cencKeyHex))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func cencConfig(t testing.TB, scheme EncryptionScheme) Encryption {
	t.Helper()
	e := Encryption{Scheme: scheme, Key: cencKey(t), IV: cencHex(t, cencIV8Hex)}
	copy(e.KID[:], cencHex(t, cencKIDHex))
	if scheme == SchemeCBCS {
		e.IV = cencHex(t, cencIVHex)
	}
	return e
}

func cencData(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cenc", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// trackFile is init followed by segments, a file ffmpeg reads.
func trackFile(tr *SegmentedTrack) []byte {
	out := bytes.Clone(tr.Init)
	for _, s := range tr.Segments {
		out = append(out, s.Data...)
	}
	return out
}

// packetSums is the md5 of every sample of every track of an fMP4 file, as ffmpeg's
// "-c copy -f framemd5" lists them (the hash column), in file order per track.
func packetSums(t testing.TB, data []byte) []string {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var trex *mp4.TrexBox
	if f.Moov != nil && f.Moov.Mvex != nil {
		trex = f.Moov.Mvex.Trex
	}
	var out []string
	for _, s := range f.Segments {
		for _, fr := range s.Fragments {
			fss, err := fr.GetFullSamples(trex)
			if err != nil {
				t.Fatal(err)
			}
			for _, fs := range fss {
				out = append(out, fmt.Sprintf("%x", md5.Sum(fs.Data)))
			}
		}
	}
	return out
}

// framemd5Sums reads the hash column of ffmpeg's framemd5 output.
func framemd5Sums(t testing.TB, name string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(string(cencData(t, name)), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		f := strings.Split(line, ", ")
		// stream, dts, pts, duration, size, hash[, S=side data count, size, hash]
		out = append(out, strings.TrimSpace(f[5]))
	}
	return out
}

// TestWriteCENCOracleInputs writes the encrypted files the oracle is run on;
// run it with AVKIT_WRITE_CENC=1 to refresh testdata/cenc.
func TestWriteCENCOracleInputs(t *testing.T) {
	if os.Getenv("AVKIT_WRITE_CENC") == "" {
		t.Skip("set AVKIT_WRITE_CENC=1 to rewrite testdata/cenc")
	}
	for _, scheme := range []EncryptionScheme{SchemeCENC, SchemeCBCS} {
		seg, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second), EncryptSegments(cencConfig(t, scheme)))
		if err != nil {
			t.Fatal(err)
		}
		for i := range seg.Tracks {
			name := fmt.Sprintf("%s-%s.mp4", scheme, seg.Tracks[i].Name)
			if err := os.WriteFile(filepath.Join("testdata", "cenc", name), trackFile(&seg.Tracks[i]), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestContentKeyNeverPrints(t *testing.T) {
	k := cencKey(t)
	e := cencConfig(t, SchemeCENC)
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "key", k, "enc", e)
	for _, s := range []string{
		fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s %x %q", k, k, k, k, k, k),
		fmt.Sprintf("%v %+v %#v", e, e, e), logs.String(),
	} {
		if strings.Contains(strings.ToLower(s), "0001020304") || strings.Contains(s, "[0 1 2 3") ||
			strings.Contains(s, "0x0,0x1") || strings.Contains(s, "0x00, 0x01") {
			t.Fatalf("key leaked: %s", s)
		}
	}
	if k.String() != "ContentKey(redacted)" || k.GoString() != "ContentKey(redacted)" {
		t.Fatal("placeholder changed")
	}
}

func TestNewContentKeyLength(t *testing.T) {
	for _, n := range []int{0, 15, 17, 32} {
		_, err := NewContentKey(make([]byte, n))
		if !errors.Is(err, ErrEncryption) {
			t.Errorf("%d bytes: %v", n, err)
		}
	}
}

func TestEncryptionCheckRefuses(t *testing.T) {
	good := cencConfig(t, SchemeCENC)
	pssh := psshBox(t)
	cases := map[string]func(*Encryption){
		"scheme":   func(e *Encryption) { e.Scheme = "cens" },
		"nokey":    func(e *Encryption) { e.Key = ContentKey{} },
		"iv7":      func(e *Encryption) { e.IV = make([]byte, 7) },
		"iv12":     func(e *Encryption) { e.IV = make([]byte, 12) },
		"iv32":     func(e *Encryption) { e.IV = make([]byte, 32) },
		"manypssh": func(e *Encryption) { e.PSSH = make([][]byte, MaxPSSHBoxes+1) },
		"bigpssh":  func(e *Encryption) { e.PSSH = [][]byte{make([]byte, MaxPSSHSize+1)} },
		"junk":     func(e *Encryption) { e.PSSH = [][]byte{{1, 2, 3}} },
		"notpssh":  func(e *Encryption) { e.PSSH = [][]byte{{0, 0, 0, 8, 'f', 'r', 'e', 'e'}} },
		"trailing": func(e *Encryption) { e.PSSH = [][]byte{append(bytes.Clone(pssh), 0, 0, 0, 0, 0, 0, 0, 0)} },
	}
	for name, mod := range cases {
		e := good
		mod(&e)
		if _, err := e.check(); !errors.Is(err, ErrEncryption) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A refused option is refused by Segment as well.
	e := good
	e.IV = make([]byte, 12)
	_, err := Segment(segmentSource(t, "source.mp4"), EncryptSegments(e))
	if !errors.Is(err, ErrSegmentOption) || !errors.Is(err, ErrEncryption) {
		t.Fatalf("Segment: %v", err)
	}
	if strings.Contains(err.Error(), cencKeyHex) {
		t.Fatal("key in error")
	}
}

func psshBox(t testing.TB) []byte {
	t.Helper()
	// The common system ID of W3C EME's cenc initData format.
	b, err := mp4.NewPsshBox("1077efec-c0b2-4d02-ace3-3c1e52e2fb4b", []string{cencKIDHex[:8] + "-" + cencKIDHex[8:12] + "-" +
		cencKIDHex[12:16] + "-" + cencKIDHex[16:20] + "-" + cencKIDHex[20:]}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := b.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRandomIVWhenNoneGiven(t *testing.T) {
	for scheme, n := range map[EncryptionScheme]int{SchemeCENC: 8, SchemeCBCS: 16} {
		e := cencConfig(t, scheme)
		e.IV = nil
		p, err := e.check()
		if err != nil || len(p.iv) != n {
			t.Fatalf("%s: %v, %d", scheme, err, len(p.iv))
		}
	}
}

func TestForTrackSeparatesIVs(t *testing.T) {
	e := cencConfig(t, SchemeCENC)
	e.IV = []byte{0, 0, 0, 0xff, 1, 2, 3, 4}
	p, err := e.check()
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(p.forTrack(0).iv); got != "000000ff01020304" {
		t.Errorf("track 0: %s", got)
	}
	if got := hex.EncodeToString(p.forTrack(1).iv); got != "0000010001020304" {
		t.Errorf("track 1: %s", got)
	}
	if got := hex.EncodeToString(p.forTrack(0x1ff).iv); got != "000002fe01020304" {
		t.Errorf("track 511: %s", got)
	}
	if got := hex.EncodeToString(p.iv); got != "000000ff01020304" {
		t.Errorf("the original moved: %s", got)
	}
}

func TestSegmentEncryptedStatesProtection(t *testing.T) {
	for _, scheme := range []EncryptionScheme{SchemeCENC, SchemeCBCS} {
		e := cencConfig(t, scheme)
		e.PSSH = [][]byte{psshBox(t)}
		seg, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second), EncryptSegments(e), IndexedSegments())
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range seg.Tracks {
			p := tr.Protection
			if p == nil || p.Scheme != scheme || hex.EncodeToString(p.KID[:]) != cencKIDHex || len(p.PSSH) != 1 {
				t.Fatalf("%s %s: %+v", scheme, tr.Name, p)
			}
			f, err := mp4.DecodeFile(bytes.NewReader(tr.Init))
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Moov.Psshs) != 1 {
				t.Errorf("%d pssh in init", len(f.Moov.Psshs))
			}
			// The sidx written after encryption describes the served bytes.
			for i, s := range tr.Segments {
				g, err := mp4.DecodeFile(bytes.NewReader(s.Data))
				if err != nil {
					t.Fatal(err)
				}
				ms := g.Segments[0]
				var served uint64
				for _, fr := range ms.Fragments {
					served += fr.Moof.Size() + fr.Mdat.Size()
				}
				if got := uint64(ms.Sidx.SidxRefs[0].ReferencedSize); got != served {
					t.Errorf("%s segment %d: sidx says %d, moof+mdat is %d", tr.Name, i, got, served)
				}
			}
		}
	}
}

func TestSegmentEncryptedLeavesSubtitlesClear(t *testing.T) {
	seg, err := Segment(segmentSource(t, "source.mkv"), SegmentDuration(2*time.Second), EncryptSegments(cencConfig(t, SchemeCENC)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range seg.Tracks {
		if (tr.Kind == Subtitle) != (tr.Protection == nil) {
			t.Errorf("%s: kind %v protection %v", tr.Name, tr.Kind, tr.Protection)
		}
	}
}

// TestEncryptDecryptRoundTrip checks the clear samples come back byte for byte,
// and the encrypted ones differ.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	clear, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []EncryptionScheme{SchemeCENC, SchemeCBCS} {
		for i, ct := range clear.Tracks {
			var segs [][]byte
			for _, s := range ct.Segments {
				segs = append(segs, s.Data)
			}
			ei, es, err := Encrypt(ct.Init, segs, cencConfig(t, scheme))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(ei, ct.Init) || bytes.Equal(es[0], segs[0]) {
				t.Fatal("nothing encrypted")
			}
			di, ds, err := Decrypt(ei, es, cencKey(t))
			if err != nil {
				t.Fatalf("%s %s: %v", scheme, ct.Name, err)
			}
			want, got := bytes.Clone(ct.Init), bytes.Clone(di)
			for k := range segs {
				want, got = append(want, segs[k]...), append(got, ds[k]...)
			}
			a, b := packetSums(t, want), packetSums(t, got)
			if strings.Join(a, ",") != strings.Join(b, ",") || len(a) == 0 {
				t.Fatalf("%s track %d: decrypted samples differ from the clear ones", scheme, i)
			}
			// Whole-file decryption agrees.
			whole := append(bytes.Clone(ei), bytes.Join(es, nil)...)
			df, err := DecryptFile(whole, cencKey(t))
			if err != nil || strings.Join(packetSums(t, df), ",") != strings.Join(a, ",") {
				t.Fatalf("DecryptFile: %v", err)
			}
		}
	}
}
