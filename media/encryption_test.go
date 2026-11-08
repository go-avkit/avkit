// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package media

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	empd "github.com/Eyevinn/dash-mpd/mpd"

	"github.com/go-avkit/avkit/container"
	"github.com/go-streamkit/streamkit/dash"
	"github.com/go-streamkit/streamkit/hls"
)

// There is no ffmpeg oracle for the encryption tags: ffmpeg 9.0.2 cannot encrypt
// fMP4 HLS ("Encrypted fmp4 not yet supported") and its dash muxer writes no
// ContentProtection. The tags follow draft-pantos-hls-rfc8216bis 4.4.4.4 and
// 4.4.6.5, as streamkit's tests pin them, and the end-to-end test below checks
// what matters: the files written decrypt, with the key the playlist does not
// hold, to the samples of the clear segmentation.

const (
	widevine  = "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"
	commonSys = "1077efec-c0b2-4d02-ace3-3c1e52e2fb4b"
	kidHex    = "11223344556677889900aabbccddeeff"
	keyHex    = "000102030405060708090a0b0c0d0e0f"
)

// psshFor is a version-0 pssh box of a system with no data.
func psshFor(t *testing.T, system string) []byte {
	t.Helper()
	id, err := hex.DecodeString(strings.ReplaceAll(system, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	box := append([]byte{0, 0, 0, 32, 'p', 's', 's', 'h', 0, 0, 0, 0}, id...)
	return append(box, 0, 0, 0, 0)
}

func encrypted(t *testing.T, source string, scheme container.EncryptionScheme) *container.Segmentation {
	t.Helper()
	k, err := container.NewContentKey(mustHex(t, keyHex))
	if err != nil {
		t.Fatal(err)
	}
	enc := container.Encryption{Scheme: scheme, Key: k, IV: mustHex(t, "a0a1a2a3a4a5a6a7"),
		PSSH: [][]byte{psshFor(t, widevine), psshFor(t, commonSys)}}
	if scheme == container.SchemeCBCS {
		enc.IV = mustHex(t, "b0b1b2b3b4b5b6b7b8b9babbbcbdbebf")
	}
	copy(enc.KID[:], mustHex(t, kidHex))
	b, err := os.ReadFile(oraclePath(source))
	if err != nil {
		t.Fatal(err)
	}
	r, err := container.NewReader(b)
	if err != nil {
		t.Fatal(err)
	}
	seg, err := container.Segment(r, container.SegmentDuration(2*time.Second), container.EncryptSegments(enc))
	if err != nil {
		t.Fatal(err)
	}
	return seg
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var drmKeys = Keys(
	Key{SystemID: widevine, URI: "https://lic.example/widevine", Value: "widevine", FormatVersions: "1"},
	Key{SystemID: strings.ToUpper(commonSys), URI: "data:text/plain;base64,AAAA", Value: "ClearKey1.0"},
	Key{URI: "https://keys.example/clear.bin"},
)

func TestHLSStatesEncryptionPerMethodOfTheScheme(t *testing.T) {
	for scheme, method := range map[container.EncryptionScheme]string{container.SchemeCENC: "SAMPLE-AES-CTR", container.SchemeCBCS: "SAMPLE-AES"} {
		files, err := HLS(encrypted(t, "source.mkv", scheme), drmKeys) // video, audio, WebVTT
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"video1.m3u8", "audio1.m3u8"} {
			out := string(files.Media[name])
			for _, want := range []string{
				`#EXT-X-KEY:METHOD=` + method + `,URI="https://lic.example/widevine",KEYFORMAT="urn:uuid:` + widevine + `",KEYFORMATVERSIONS="1"`,
				`#EXT-X-KEY:METHOD=` + method + `,URI="data:text/plain;base64,AAAA",KEYFORMAT="urn:uuid:` + commonSys + `"`,
				`#EXT-X-KEY:METHOD=` + method + `,URI="https://keys.example/clear.bin"` + "\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("%s %s: missing %q in:\n%s", scheme, name, want, out)
				}
			}
			if strings.Index(out, "#EXT-X-KEY") > strings.Index(out, "#EXT-X-MAP") {
				t.Errorf("%s: the keys follow the map", name)
			}
		}
		if strings.Contains(string(files.Media["text1.m3u8"]), "EXT-X-KEY") {
			t.Error("the WebVTT playlist is keyed")
		}
		if got := strings.Count(string(files.Master), "#EXT-X-SESSION-KEY:METHOD="+method); got != 3 {
			t.Errorf("%s: %d session keys in:\n%s", scheme, got, files.Master)
		}
	}
}

func TestHLSOfAnEncryptedSegmentationRefusesWhatCannotBePlayed(t *testing.T) {
	seg := encrypted(t, "source.mp4", container.SchemeCENC)
	if _, err := HLS(seg); !errors.Is(err, ErrPresentation) {
		t.Errorf("no keys: %v", err)
	}
	if _, err := HLS(seg, Keys(Key{SystemID: "not-a-uuid", URI: "x"})); !errors.Is(err, ErrPresentation) {
		t.Errorf("bad system: %v", err)
	}
	if _, err := HLS(seg, Keys(Key{URI: "a\"b"})); err == nil {
		t.Error("a quote in the URI")
	}
	// Keys for a presentation in the clear.
	clear := segmentFile(t, "source.mp4")
	for name, f := range map[string]func() error{
		"hls":  func() error { _, err := HLS(clear, drmKeys); return err },
		"dash": func() error { _, err := DASH(clear, drmKeys); return err },
		"dir":  func() error { return WriteDir(filepath.Join(t.TempDir(), "x"), clear, drmKeys) },
	} {
		if err := f(); !errors.Is(err, ErrPresentation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Two schemes in one presentation.
	seg.Tracks[1].Protection = &container.Protection{Scheme: container.SchemeCBCS}
	if _, err := HLS(seg, drmKeys); !errors.Is(err, ErrPresentation) {
		t.Errorf("mixed: %v", err)
	}
	if _, err := DASH(seg, drmKeys); !errors.Is(err, ErrPresentation) {
		t.Errorf("mixed dash: %v", err)
	}
}

func TestDASHStatesTheProtectionOfEachEncryptedAdaptationSet(t *testing.T) {
	for _, scheme := range []container.EncryptionScheme{container.SchemeCENC, container.SchemeCBCS} {
		seg := encrypted(t, "source.mkv", scheme)
		own := psshFor(t, "00000000-0000-0000-0000-00000000dead")
		out, err := DASH(seg,
			Key{SystemID: widevine, Value: "widevine"}.option(),
			Key{SystemID: strings.ToUpper(commonSys)}.option(),
			Key{SystemID: "9a04f079-9840-4286-ab92-e65be0885f95", Value: "2.0", PSSH: own}.option(),
			Key{URI: "https://keys.example/clear.bin"}.option(), // identity: no DASH element
		)
		if err != nil {
			t.Fatal(err)
		}
		m, err := empd.ReadFromString(string(out))
		if err != nil {
			t.Fatal(err)
		}
		sets := m.Periods[0].AdaptationSets
		for i, set := range sets[:2] {
			cps := set.ContentProtections
			if len(cps) != 4 {
				t.Fatalf("%s set %d: %d elements:\n%s", scheme, i, len(cps), out)
			}
			if string(cps[0].SchemeIdUri) != dash.MP4ProtectionScheme || cps[0].Value != string(scheme) ||
				cps[0].DefaultKID != "11223344-5566-7788-9900-aabbccddeeff" {
				t.Errorf("mp4protection %+v", cps[0])
			}
			// The pssh of the segmentation's own box, found by system ID.
			if string(cps[1].SchemeIdUri) != "urn:uuid:"+widevine || cps[1].Value != "widevine" || cps[1].Pssh == nil ||
				cps[1].Pssh.Value != base64.StdEncoding.EncodeToString(psshFor(t, widevine)) {
				t.Errorf("widevine %+v", cps[1])
			}
			if string(cps[2].SchemeIdUri) != "urn:uuid:"+commonSys || cps[2].Pssh == nil ||
				cps[2].Pssh.Value != base64.StdEncoding.EncodeToString(psshFor(t, commonSys)) {
				t.Errorf("common %+v", cps[2])
			}
			// A caller's own box wins; a system with none has no cenc:pssh.
			if cps[3].Pssh == nil || cps[3].Pssh.Value != base64.StdEncoding.EncodeToString(own) {
				t.Errorf("own %+v", cps[3])
			}
		}
		if len(sets[2].ContentProtections) != 0 {
			t.Error("the WebVTT set is protected")
		}
	}
	// No Keys at all: the scheme and KID are still stated.
	out, err := DASH(encrypted(t, "source.mp4", container.SchemeCENC))
	if err != nil || strings.Count(string(out), dash.MP4ProtectionScheme) != 2 {
		t.Fatalf("%v:\n%s", err, out)
	}
	// A system with no box in the segmentation and none given.
	out, err = DASH(encrypted(t, "source.mp4", container.SchemeCENC), Key{SystemID: "9a04f079-9840-4286-ab92-e65be0885f95"}.option())
	if err != nil || strings.Contains(string(out), "cenc:pssh") {
		t.Fatalf("%v:\n%s", err, out)
	}
	if _, err := DASH(encrypted(t, "source.mp4", container.SchemeCENC), Key{SystemID: "xyz"}.option()); !errors.Is(err, ErrPresentation) {
		t.Errorf("bad system: %v", err)
	}
}

// option makes a single Key an Option, for the tests.
func (k Key) option() Option { return Keys(k) }

func TestSystemIDHelpers(t *testing.T) {
	if !uuidLike("EDEF8BA9-79d6-4ace-a3c8-27dcd51d21ed") {
		t.Error("mixed case refused")
	}
	for _, s := range []string{"", "edef8ba9-79d6-4ace-a3c8-27dcd51d21e", "edef8ba979d64acea3c827dcd51d21edxxxx", "edef8ba9-79d6-4ace-a3c8-27dcd51d21eg",
		"edef8ba9x79d6-4ace-a3c8-27dcd51d21ed", "ed-f8ba9-79d6-4ace-a3c8-27dcd51d21-d-"} {
		if uuidLike(s) {
			t.Errorf("%q accepted", s)
		}
	}
	if got := psshSystem(psshFor(t, widevine)); got != widevine {
		t.Errorf("psshSystem %q", got)
	}
	if psshSystem([]byte("short")) != "" || psshSystem(append([]byte{0, 0, 0, 32, 'f', 'r', 'e', 'e'}, make([]byte, 30)...)) != "" {
		t.Error("a non-pssh box has a system")
	}
	if f, err := systemFormat(""); f != "" || err != nil {
		t.Errorf("identity: %q %v", f, err)
	}
}

// TestAnEncryptedPresentationIsWrittenAndDecryptsWithTheKey is the end-to-end
// test: the directory WriteDir writes is read back through the playlists, the
// segments they name are decrypted with the content key, and the samples equal
// the clear segmentation's.
func TestAnEncryptedPresentationIsWrittenAndDecryptsWithTheKey(t *testing.T) {
	for _, scheme := range []container.EncryptionScheme{container.SchemeCENC, container.SchemeCBCS} {
		seg := encrypted(t, "source.mkv", scheme)
		dir := filepath.Join(t.TempDir(), "out")
		if err := WriteDir(dir, seg, drmKeys); err != nil {
			t.Fatal(err)
		}
		read := func(name string) []byte {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		base, _ := url.Parse("https://cdn.example/vod/")
		key, err := container.NewContentKey(mustHex(t, keyHex))
		if err != nil {
			t.Fatal(err)
		}
		clear := segmentFile(t, "source.mkv")
		for i, name := range []string{"video1.m3u8", "audio1.m3u8"} {
			pl, err := hls.Parse(base.JoinPath(name), read(name))
			if err != nil || pl.Init == nil || pl.Segments[0].Key == nil {
				t.Fatalf("%s: %+v %v", name, pl, err)
			}
			local := func(u string) string { return strings.TrimPrefix(u, base.String()) }
			var segs [][]byte
			for _, s := range pl.Segments {
				segs = append(segs, read(local(s.URL)))
			}
			init, out, err := container.Decrypt(read(local(pl.Init.URL)), segs, key)
			if err != nil {
				t.Fatalf("%s %s: %v", scheme, name, err)
			}
			// The decrypted init and segments are the clear segmentation's, sample for sample.
			cinit, cseg := clear.Tracks[i].Init, clear.Tracks[i].Segments
			if len(out) != len(cseg) || len(init) == 0 || bytes.Equal(segs[0], out[0]) {
				t.Fatalf("%s %s: %d segments, clear has %d", scheme, name, len(out), len(cseg))
			}
			want := mdatOf(t, cinit, cseg[0].Data)
			if got := mdatOf(t, init, out[0]); !bytes.Equal(got, want) {
				t.Errorf("%s %s: the first segment's samples differ after decryption", scheme, name)
			}
			if bytes.Equal(mdatOf(t, read(local(pl.Init.URL)), segs[0]), want) {
				t.Errorf("%s %s: the written segment is not encrypted", scheme, name)
			}
		}
		// The manifest names the same key identifier as the tenc inside.
		m, err := empd.ReadFromString(string(read(ManifestName)))
		if err != nil || m.Periods[0].AdaptationSets[0].ContentProtections[0].DefaultKID != "11223344-5566-7788-9900-aabbccddeeff" {
			t.Errorf("manifest: %v", err)
		}
	}
}

// mdatOf is the payload of the mdat in the media segment seg: what an init
// segment and a segment together carry as samples.
func mdatOf(t *testing.T, init, seg []byte) []byte {
	t.Helper()
	i := bytes.Index(seg, []byte("mdat"))
	if i < 4 {
		t.Fatal("no mdat")
	}
	size := int(seg[i-4])<<24 | int(seg[i-3])<<16 | int(seg[i-2])<<8 | int(seg[i-1])
	return seg[i+4 : i-4+size]
}
