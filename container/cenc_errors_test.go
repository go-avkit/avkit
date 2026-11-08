// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// clearTrack is one track of source.mp4 cut into a clear init and segments.
func clearTrack(t *testing.T, track int) ([]byte, [][]byte) {
	t.Helper()
	seg, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var segs [][]byte
	for _, s := range seg.Tracks[track].Segments {
		segs = append(segs, s.Data)
	}
	return seg.Tracks[track].Init, segs
}

func encTrack(t *testing.T, track int, scheme EncryptionScheme) ([]byte, [][]byte) {
	t.Helper()
	init, segs := clearTrack(t, track)
	ei, es, err := Encrypt(init, segs, cencConfig(t, scheme))
	if err != nil {
		t.Fatal(err)
	}
	return ei, es
}

// failEncodeAt makes the nth call (from 1) of encodeMemory fail.
func failEncodeAt(n int) func() {
	calls := 0
	encodeMemory = func(e interface{ Encode(io.Writer) error }) ([]byte, error) {
		calls++
		if calls == n {
			return nil, errors.New("disk full")
		}
		var b bytes.Buffer
		err := e.Encode(&b)
		return b.Bytes(), err
	}
	return func() {
		encodeMemory = func(e interface{ Encode(io.Writer) error }) ([]byte, error) {
			var b bytes.Buffer
			err := e.Encode(&b)
			return b.Bytes(), err
		}
	}
}

func TestEncryptRefusals(t *testing.T) {
	init, segs := clearTrack(t, 0)
	good := cencConfig(t, SchemeCENC)
	bad := good
	bad.IV = make([]byte, 5)
	if _, _, err := Encrypt(init, segs, bad); !errors.Is(err, ErrEncryption) {
		t.Errorf("config: %v", err)
	}
	if _, _, err := Encrypt(nil, segs, good); !errors.Is(err, ErrEncrypt) {
		t.Errorf("no init: %v", err)
	}
	// source.mp4's moov holds two tracks; one init segment holds one.
	if _, _, err := Encrypt(segFile(t, "source.mp4"), segs, good); !errors.Is(err, ErrEncrypt) {
		t.Errorf("two tracks: %v", err)
	}
	if _, _, err := Encrypt(init, [][]byte{[]byte("junk")}, good); !errors.Is(err, ErrEncrypt) {
		t.Errorf("junk segment: %v", err)
	}
	// An indexed segment: its sidx would be wrong once the moof grows.
	idx, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second), IndexedSegments())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Encrypt(init, [][]byte{idx.Tracks[0].Segments[0].Data}, good); !errors.Is(err, ErrEncrypt) {
		t.Errorf("sidx: %v", err)
	}
	// A video sample whose NAL unit length overruns it.
	broken := bytes.Clone(segs[0])
	at := bytes.Index(broken, []byte("mdat")) + 4
	broken[at], broken[at+1], broken[at+2], broken[at+3] = 0xff, 0xff, 0xff, 0xff
	if _, _, err := Encrypt(init, [][]byte{broken}, good); !errors.Is(err, ErrEncrypt) {
		t.Errorf("bad NAL: %v", err)
	}
	// A fragment encryptor that cannot be built: a key AES does not take.
	_, te, err := newTrackEncryptor(init, mustCheck(t, good))
	if err != nil {
		t.Fatal(err)
	}
	te.p = &protection{key: []byte{1}, iv: te.p.iv}
	if _, err := te.segment(segs[0]); !errors.Is(err, ErrEncrypt) {
		t.Errorf("key: %v", err)
	}
}

func mustCheck(t *testing.T, e Encryption) *protection {
	t.Helper()
	p, err := e.check()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEncryptEncodeFailures(t *testing.T) {
	init, segs := clearTrack(t, 1)
	for n := 1; n <= 2; n++ {
		restore := failEncodeAt(n)
		_, _, err := Encrypt(init, segs[:1], cencConfig(t, SchemeCENC))
		restore()
		if !errors.Is(err, ErrEncrypt) {
			t.Errorf("encode %d: %v", n, err)
		}
	}
}

func TestCutMediaEncryptionFailures(t *testing.T) {
	r := segmentSource(t, "source.mp4")
	good, err := loadSegSource(r, r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	p := mustCheck(t, cencConfig(t, SchemeCENC))
	// A protection InitProtect refuses.
	q := *p
	q.iv = make([]byte, 5)
	if _, err := cutMedia(good, good, refCuts{ts: good.cfg.Timescale}, false, &q); !errors.Is(err, ErrEncrypt) {
		t.Errorf("init: %v", err)
	}
	// A sample the segment cannot be encrypted with.
	bad := *good
	bad.samples = []Sample{{Data: []byte{0, 0, 0, 0xff, 1, 2, 3, 4, 5, 6}, Duration: 1, Sync: true}}
	bad.starts, bad.end = []uint64{0}, 1
	if _, err := cutMedia(&bad, &bad, refCuts{ts: good.cfg.Timescale}, false, p); !errors.Is(err, ErrEncrypt) {
		t.Errorf("segment: %v", err)
	}
}

func TestCheckTencRefusals(t *testing.T) {
	ok := func() *mp4.TencBox {
		return &mp4.TencBox{DefaultIsProtected: 1, DefaultPerSampleIVSize: 8}
	}
	if err := checkTenc("cenc", ok()); err != nil {
		t.Fatal(err)
	}
	cbcs := &mp4.TencBox{Version: 1, DefaultCryptByteBlock: 1, DefaultSkipByteBlock: 9, DefaultConstantIV: make([]byte, 16)}
	if err := checkTenc("cbcs", cbcs); err != nil {
		t.Fatal(err)
	}
	box := func(ivSize, crypt, skip uint8, constIV int) *mp4.TencBox {
		return &mp4.TencBox{DefaultPerSampleIVSize: ivSize, DefaultCryptByteBlock: crypt,
			DefaultSkipByteBlock: skip, DefaultConstantIV: make([]byte, constIV)}
	}
	cases := map[string]struct {
		scheme string
		box    *mp4.TencBox
	}{
		"nil":      {"cenc", nil},
		"ivsize4":  {"cenc", box(4, 0, 0, 0)},
		"ivsize32": {"cenc", box(32, 0, 0, 0)},
		"noconst":  {"cbcs", box(0, 1, 9, 0)},
		"const12":  {"cbcs", box(0, 1, 9, 12)},
		"cencpat":  {"cenc", box(8, 1, 9, 0)},
		"crypt16":  {"cbcs", box(8, 16, 0, 0)},
		"skip16":   {"cbcs", box(8, 1, 16, 0)},
		"skiponly": {"cbcs", box(8, 0, 3, 0)},
	}
	for name, c := range cases {
		if err := checkTenc(c.scheme, c.box); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckInfoRefusals(t *testing.T) {
	if err := checkInfo(mp4.DecryptInfo{TrackInfos: []mp4.DecryptTrackInfo{{TrackID: 1}}}); err != nil {
		t.Fatalf("a clear track: %v", err)
	}
	for name, sinf := range map[string]*mp4.SinfBox{
		"noschm":  {},
		"notenc":  {Schm: &mp4.SchmBox{SchemeType: "cenc"}, Schi: &mp4.SchiBox{}},
		"badtenc": {Schm: &mp4.SchmBox{SchemeType: "cenc"}, Schi: &mp4.SchiBox{Tenc: &mp4.TencBox{DefaultPerSampleIVSize: 3}}},
	} {
		err := checkInfo(mp4.DecryptInfo{TrackInfos: []mp4.DecryptTrackInfo{{TrackID: 7, Sinf: sinf}}})
		if !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// replaceIn returns b with the first old replaced by the same-length new.
func replaceIn(t *testing.T, b []byte, old, new string) []byte {
	t.Helper()
	if len(old) != len(new) || !bytes.Contains(b, []byte(old)) {
		t.Fatalf("cannot replace %q", old)
	}
	return bytes.Replace(b, []byte(old), []byte(new), 1)
}

func TestDecryptRefusals(t *testing.T) {
	key := cencKey(t)
	ei, es := encTrack(t, 0, SchemeCENC)
	_, clearSegs := clearTrack(t, 0)
	if _, _, err := Decrypt(ei, es, ContentKey{}); !errors.Is(err, ErrEncryption) {
		t.Errorf("no key: %v", err)
	}
	if _, _, err := Decrypt(nil, es, key); !errors.Is(err, ErrDecrypt) {
		t.Errorf("no init: %v", err)
	}
	if _, _, err := Decrypt(ei, [][]byte{[]byte("junk")}, key); !errors.Is(err, ErrDecrypt) {
		t.Errorf("junk segment: %v", err)
	}
	// Protected samples whose senc is missing.
	if _, _, err := Decrypt(ei, clearSegs, key); !errors.Is(err, ErrDecrypt) {
		t.Errorf("no senc: %v", err)
	}
	// A scheme that is neither cenc nor cbcs.
	if _, _, err := Decrypt(replaceIn(t, ei, "cenc", "cens"), es, key); !errors.Is(err, ErrDecrypt) {
		t.Errorf("scheme: %v", err)
	}
	// A pattern in a cenc track, and a bad IV size.
	for name, mod := range map[string]func(*mp4.TencBox){
		"pattern": func(b *mp4.TencBox) { b.Version = 1; b.DefaultCryptByteBlock = 1; b.DefaultSkipByteBlock = 9 },
		"ivsize":  func(b *mp4.TencBox) { b.DefaultPerSampleIVSize = 5 },
	} {
		f, err := mp4.DecodeFile(bytes.NewReader(ei))
		if err != nil {
			t.Fatal(err)
		}
		se := f.Moov.Trak.Mdia.Minf.Stbl.Stsd.Children[0].(*mp4.VisualSampleEntryBox)
		mod(se.Sinf.Schi.Tenc)
		var buf bytes.Buffer
		if err := f.Init.Encode(&buf); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Decrypt(buf.Bytes(), es, key); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A subsample map that overruns its sample.
	f, err := mp4.DecodeFile(bytes.NewReader(es[0]))
	if err != nil {
		t.Fatal(err)
	}
	traf := f.Segments[0].Fragments[0].Moof.Traf
	if err := traf.ParseReadSenc(8, f.Segments[0].Fragments[0].Moof.StartPos); err != nil {
		t.Fatal(err)
	}
	traf.Senc.SubSamples[0][0].BytesOfProtectedData = 1 << 30
	var buf bytes.Buffer
	if err := f.Segments[0].Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decrypt(ei, [][]byte{buf.Bytes()}, key); !errors.Is(err, ErrDecrypt) {
		t.Errorf("overrun: %v", err)
	}
	// A wrong key is not an error: CTR has no check. The samples are noise.
	other, _ := NewContentKey(make([]byte, 16))
	_, ds, err := Decrypt(ei, es, other)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ds[0], clearSegs[0]) {
		t.Error("a wrong key gave the clear samples")
	}
}

func TestDecryptRefusesAKeyInTheErrors(t *testing.T) {
	ei, es := encTrack(t, 0, SchemeCBCS)
	_, _, err := Decrypt(replaceIn(t, ei, "cbcs", "cbc1"), es, cencKey(t))
	if err == nil || strings.Contains(err.Error(), cencKeyHex) || strings.Contains(err.Error(), "\x00\x01\x02\x03") {
		t.Fatalf("%v", err)
	}
}

func TestDecryptFileRefusals(t *testing.T) {
	key := cencKey(t)
	ei, es := encTrack(t, 0, SchemeCENC)
	_, clearSegs := clearTrack(t, 0)
	whole := append(bytes.Clone(ei), bytes.Join(es, nil)...)
	if _, err := DecryptFile(nil, key); !errors.Is(err, ErrDecrypt) {
		t.Errorf("empty: %v", err)
	}
	if _, err := DecryptFile(whole, ContentKey{}); !errors.Is(err, ErrEncryption) {
		t.Errorf("no key: %v", err)
	}
	if _, err := DecryptFile(append(bytes.Clone(ei), bytes.Join(clearSegs, nil)...), key); !errors.Is(err, ErrDecrypt) {
		t.Errorf("no senc: %v", err)
	}
	for n := 1; n <= 2; n++ {
		restore := failEncodeAt(n)
		_, err := DecryptFile(whole, key)
		restore()
		if !errors.Is(err, ErrDecrypt) {
			t.Errorf("encode %d: %v", n, err)
		}
	}
}

func TestDecryptEncodeFailures(t *testing.T) {
	ei, es := encTrack(t, 1, SchemeCBCS)
	for n := 1; n <= 2; n++ {
		restore := failEncodeAt(n)
		_, _, err := Decrypt(ei, es[:1], cencKey(t))
		restore()
		if !errors.Is(err, ErrDecrypt) {
			t.Errorf("encode %d: %v", n, err)
		}
	}
}

// TestDecryptClearTrackIsUntouched: a track without protection passes through.
func TestDecryptClearTrackIsUntouched(t *testing.T) {
	init, segs := clearTrack(t, 0)
	di, ds, err := Decrypt(init, segs, cencKey(t))
	if err != nil {
		t.Fatal(err)
	}
	a := packetSums(t, append(bytes.Clone(init), bytes.Join(segs, nil)...))
	b := packetSums(t, append(bytes.Clone(di), bytes.Join(ds, nil)...))
	sameSums(t, "clear", b, a)
}
