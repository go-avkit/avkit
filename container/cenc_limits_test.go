// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"runtime"
	"runtime/debug"
	"slices"
	"testing"
)

// The bombs of go-avkit/avkit#123: the CENC functions decoded MP4 boxes without
// the bounds #122 put on the readers.

// cencInitBomb is a protected initialisation segment whose sample entry, sinf and
// schi (all boxes mp4ff descends into) hold n empty boxes.
func cencInitBomb(n int) []byte {
	inner := bytes.Repeat(box("free"), n)
	schi := box("schi", inner)
	sinf := box("sinf", schi)
	encv := box("encv", make([]byte, 78), sinf)
	stsd := box("stsd", make([]byte, 8), encv)
	moov := box("moov", box("trak", box("mdia", box("minf", box("stbl", stsd)))))
	return slices.Concat(mp4Ftyp(), moov)
}

// cencSegmentBomb is a media segment whose moof and traf hold n empty boxes.
func cencSegmentBomb(n int) []byte {
	return box("moof", box("traf", bytes.Repeat(box("free"), n)))
}

// cencCalls puts a bomb where each CENC function decodes MP4: the init, a
// segment, the whole file, a pssh box. A call with limits is nil for the plain
// function when it is asked for the defaults.
func cencCalls(t *testing.T) map[string]func(bomb []byte, l ReaderLimits, withLimits bool) error {
	t.Helper()
	key := cencKey(t)
	clearInit, clearSegs := clearTrack(t, 0)
	encInit, encSegs := encTrack(t, 0, SchemeCENC)
	cfg := cencConfig(t, SchemeCENC)
	return map[string]func([]byte, ReaderLimits, bool) error{
		"Encrypt init": func(b []byte, l ReaderLimits, w bool) error {
			var err error
			if w {
				_, _, err = EncryptWithLimits(b, clearSegs, cfg, l)
			} else {
				_, _, err = Encrypt(b, clearSegs, cfg)
			}
			return err
		},
		"Encrypt segment": func(b []byte, l ReaderLimits, w bool) error {
			var err error
			if w {
				_, _, err = EncryptWithLimits(clearInit, [][]byte{b}, cfg, l)
			} else {
				_, _, err = Encrypt(clearInit, [][]byte{b}, cfg)
			}
			return err
		},
		"Encrypt pssh": func(b []byte, l ReaderLimits, w bool) error {
			c := cfg
			c.PSSH = [][]byte{b}
			var err error
			if w {
				_, _, err = EncryptWithLimits(clearInit, clearSegs, c, l)
			} else {
				_, _, err = Encrypt(clearInit, clearSegs, c)
			}
			return err
		},
		"Decrypt init": func(b []byte, l ReaderLimits, w bool) error {
			var err error
			if w {
				_, _, err = DecryptWithLimits(b, encSegs, key, l)
			} else {
				_, _, err = Decrypt(b, encSegs, key)
			}
			return err
		},
		"Decrypt segment": func(b []byte, l ReaderLimits, w bool) error {
			var err error
			if w {
				_, _, err = DecryptWithLimits(encInit, [][]byte{b}, key, l)
			} else {
				_, _, err = Decrypt(encInit, [][]byte{b}, key)
			}
			return err
		},
		"DecryptFile": func(b []byte, l ReaderLimits, w bool) error {
			var err error
			if w {
				_, err = DecryptFileWithLimits(b, key, l)
			} else {
				_, err = DecryptFile(b, key)
			}
			return err
		},
	}
}

// TestCENCBombsAreRefusedBeforeTheyAreDecoded: every CENC function refuses a
// flood or a nesting that mp4ff would decode into Go values, with ErrReaderLimit
// and a small allocation, under a memory limit the decode would not fit in.
func TestCENCBombsAreRefusedBeforeTheyAreDecoded(t *testing.T) {
	calls := cencCalls(t)
	low := ReaderLimits{MaxBoxes: 1_000, MaxBoxDepth: 10}
	cases := []struct {
		name     string
		data     func(place string) []byte
		limits   ReaderLimits
		defaults bool
	}{
		// 1<<20 + 1 empty boxes, the file of #113: 8 MiB, 270 MB decoded.
		{"flood", func(string) []byte { return freeFlood(DefaultMaxBoxes + 1) }, ReaderLimits{}, true},
		// The same flood inside sinf and schi (init) or moof and traf (segment).
		{"flood in the CENC boxes", func(place string) []byte {
			if place == "Encrypt segment" || place == "Decrypt segment" {
				return cencSegmentBomb(DefaultMaxBoxes + 1)
			}
			return cencInitBomb(DefaultMaxBoxes + 1)
		}, ReaderLimits{}, true},
		{"flood past a lower bound", func(place string) []byte {
			switch place {
			case "Encrypt segment", "Decrypt segment":
				return cencSegmentBomb(2_000)
			case "Encrypt init", "Decrypt init":
				return cencInitBomb(2_000)
			}
			return freeFlood(2_000)
		}, low, false},
		{"nesting", func(string) []byte { return nested("sinf", DefaultMaxBoxDepth+1) }, ReaderLimits{}, true},
		{"nesting past a lower bound", func(string) []byte { return nested("schi", 20) }, low, false},
	}
	old := debug.SetMemoryLimit(512 << 20)
	defer debug.SetMemoryLimit(old)
	for _, c := range cases {
		for place, call := range calls {
			data := c.data(place)
			if place == "Encrypt pssh" && len(data) > MaxPSSHSize {
				continue // a pssh box is refused for its size first
			}
			for _, withLimits := range []bool{true, false} {
				if !withLimits && !c.defaults {
					continue
				}
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				err := call(data, c.limits, withLimits)
				runtime.ReadMemStats(&after)
				got := after.TotalAlloc - before.TotalAlloc
				t.Logf("%s / %s (limits %v): %d input bytes, %d bytes allocated, %v", c.name, place, withLimits, len(data), got, err)
				if !errors.Is(err, ErrReaderLimit) {
					t.Errorf("%s / %s (limits %v) = %v, want ErrReaderLimit", c.name, place, withLimits, err)
				}
				// The key, the clear fixture and the config allocate before the call,
				// not in it; the refusal reads headers and allocates little.
				if got > 1<<20 {
					t.Errorf("%s / %s (limits %v): allocated %d bytes for %d input bytes", c.name, place, withLimits, got, len(data))
				}
			}
		}
	}
}

// TestCENCLimitsCanOnlyBeLowered: a value above a default is the default again,
// so a caller cannot be talked into more than the readers hold.
func TestCENCLimitsCanOnlyBeLowered(t *testing.T) {
	calls := cencCalls(t)
	high := ReaderLimits{MaxBoxes: 1<<31 - 1, MaxBoxDepth: 1 << 20}
	if err := calls["DecryptFile"](freeFlood(DefaultMaxBoxes+1), high, true); !errors.Is(err, ErrReaderLimit) {
		t.Errorf("flood past the default, limits above it: %v", err)
	}
	if err := calls["DecryptFile"](nested("sinf", DefaultMaxBoxDepth+1), high, true); !errors.Is(err, ErrReaderLimit) {
		t.Errorf("nesting past the default, limits above it: %v", err)
	}
	// A bound lower than the file needs refuses a real file, and the zero value
	// does not.
	ei, es := encTrack(t, 0, SchemeCENC)
	whole := append(bytes.Clone(ei), bytes.Join(es, nil)...)
	key := cencKey(t)
	if _, err := DecryptFileWithLimits(whole, key, ReaderLimits{MaxBoxes: 3}); !errors.Is(err, ErrReaderLimit) {
		t.Errorf("a real file against MaxBoxes 3: %v", err)
	}
	if _, err := DecryptFileWithLimits(whole, key, ReaderLimits{}); err != nil {
		t.Errorf("a real file against the defaults: %v", err)
	}
}

// TestCENCBoxesTheWalkCovers answers whether the boxes of CENC are walked: sinf,
// schi, the protected sample entries and the movie fragment boxes are containers
// mp4ff descends into (the table is generated from mp4ff, and a test fails when
// they differ), so a flood inside them is counted; tenc, senc, saiz, saio, pssh,
// schm and frma are leaves that mp4ff keeps as fields or bytes, costing what they
// hold, and are counted as one box each.
func TestCENCBoxesTheWalkCovers(t *testing.T) {
	for _, typ := range []string{"sinf", "schi", "encv", "enca", "moof", "traf", "mvex"} {
		if !mp4Containers[typ] {
			t.Errorf("%s is a container of mp4ff and is not in the table", typ)
		}
	}
	for _, typ := range []string{"tenc", "senc", "saiz", "saio", "pssh", "schm", "frma", "trun", "tfhd"} {
		if mp4Containers[typ] {
			t.Errorf("%s is a leaf of mp4ff and is in the table", typ)
		}
	}
	// A real protected init and its segments are walked: they hold the boxes above.
	ei, es := encTrack(t, 0, SchemeCENC)
	for name, data := range map[string][]byte{"init": ei, "segment": es[0]} {
		st, err := walkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{}.bounded())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d boxes, %d deep", name, st.boxes, st.depth)
		if st.boxes < 10 || st.depth < 3 {
			t.Errorf("%s: the walk counted %d boxes %d deep", name, st.boxes, st.depth)
		}
	}
}

// TestCENCWithLimitsOfTheDefaultsChangesNothing: the plain functions and their
// WithLimits variants at the defaults give the same bytes, for both schemes.
func TestCENCWithLimitsOfTheDefaultsChangesNothing(t *testing.T) {
	key := cencKey(t)
	for _, scheme := range []EncryptionScheme{SchemeCENC, SchemeCBCS} {
		init, segs := clearTrack(t, 0)
		cfg := cencConfig(t, scheme)
		cfg.IV = bytes.Repeat([]byte{7}, 8)
		cfg.PSSH = [][]byte{psshBox(t)}
		ei, es, err := Encrypt(init, segs, cfg)
		if err != nil {
			t.Fatal(err)
		}
		wi, ws, err := EncryptWithLimits(init, segs, cfg, ReaderLimits{})
		if err != nil || !bytes.Equal(ei, wi) || !slices.EqualFunc(es, ws, bytes.Equal) {
			t.Errorf("%s: EncryptWithLimits differs (%v)", scheme, err)
		}
		di, ds, err := Decrypt(ei, es, key)
		if err != nil {
			t.Fatal(err)
		}
		wdi, wds, err := DecryptWithLimits(ei, es, key, ReaderLimits{})
		if err != nil || !bytes.Equal(di, wdi) || !slices.EqualFunc(ds, wds, bytes.Equal) {
			t.Errorf("%s: DecryptWithLimits differs (%v)", scheme, err)
		}
		whole := append(bytes.Clone(ei), bytes.Join(es, nil)...)
		a, err := DecryptFile(whole, key)
		if err != nil {
			t.Fatal(err)
		}
		b, err := DecryptFileWithLimits(whole, key, ReaderLimits{MaxBoxes: DefaultMaxBoxes, MaxBoxDepth: DefaultMaxBoxDepth})
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s: DecryptFileWithLimits differs (%v)", scheme, err)
		}
		// And what it decrypts is the clear track, as before.
		if !bytes.Equal(di, init) && len(di) == 0 {
			t.Errorf("%s: empty init", scheme)
		}
	}
}
