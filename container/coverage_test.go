package container

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// oneSample is enough to get past the "no sample to read" guard; what the bytes
// mean does not matter, because this test asks WHICH refusal comes back and not
// whether a configuration could be derived.
func oneSample() []Sample { return []Sample{{Data: make([]byte, 64)}} }

// TestTheCoverageOfConfigFromSamplesIsWhatTheREADMESays.
//
// ⛔ There are THREE outcomes, not two, and a table that merged the last two
// would be wrong in both directions:
//
//  1. a configuration is derived from the samples;
//  2. the codec is KNOWN and its samples cannot state it (ErrNotInSamples) --
//     the configuration is in the container and should be taken from there;
//  3. the codec is not known at all (ErrUnsupportedCodec alone) -- there is
//     nothing to take.
//
// The README carries that table, and this is what stops it drifting: the sets
// below are read from the code's behaviour, so adding a codec to the dispatch
// without adding it to the README fails here.
func TestTheCoverageOfConfigFromSamplesIsWhatTheREADMESays(t *testing.T) {
	derives := []string{"avc1", "avc3", "hvc1", "hev1", "av01", "vp09", "mp4a"}
	inContainer := []string{"vp08", "opus", "ac-3", "ec-3"}
	unknown := []string{"mjpg", "mp3", "flac", "theora", "vorbis", "alac", "", "wmv3"}

	for _, codec := range derives {
		_, err := ConfigFromSamples(codec, oneSample())
		if errors.Is(err, ErrUnsupportedCodec) {
			t.Errorf("%q is listed as derivable and was refused as unsupported: %v",
				codec, err)
		}
	}
	for _, codec := range inContainer {
		_, err := ConfigFromSamples(codec, oneSample())
		if !errors.Is(err, ErrNotInSamples) {
			t.Errorf("%q is listed as stated by the container, want ErrNotInSamples, got %v",
				codec, err)
		}
	}
	for _, codec := range unknown {
		_, err := ConfigFromSamples(codec, oneSample())
		if !errors.Is(err, ErrUnsupportedCodec) {
			t.Errorf("%q is listed as unknown and was not refused: %v", codec, err)
		}
		if errors.Is(err, ErrNotInSamples) {
			t.Errorf("%q is unknown, not known-and-unstated: %v", codec, err)
		}
	}

	// ⛔ ErrNotInSamples must keep matching the older sentinel, or every caller
	// written before it existed stops recognising a refusal it used to handle.
	if !errors.Is(ErrNotInSamples, ErrUnsupportedCodec) {
		t.Error("ErrNotInSamples no longer wraps ErrUnsupportedCodec")
	}

	// And the README must name each one IN THE SECTION THAT LISTS THEM.
	//
	// ⛔ Not anywhere in the file. These codecs appear again in the "Tracks a
	// container can carry" tables, so a search over the whole README lets the
	// per-track-configuration table lose an entry while another table keeps the
	// check quiet. An ablation that removed `vp08` from it passed, which is the
	// SECOND time this exact blindness has been built today -- the capability
	// list in go-widgets/application had it too.
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	section, ok := sectionOf(string(readme), "### Per-track configuration")
	if !ok {
		t.Fatal("the README has no per-track-configuration section to check")
	}
	for _, group := range [][]string{derives, inContainer} {
		for _, codec := range group {
			if codec == "" {
				continue
			}
			if !strings.Contains(section, "`"+codec+"`") {
				t.Errorf("the per-track-configuration section does not name %q, "+
					"which ConfigFromSamples answers for", codec)
			}
		}
	}
}

// sectionOf is the text of one markdown section: from its heading to the next
// heading at the same level or above.
func sectionOf(text, heading string) (string, bool) {
	i := strings.Index(text, heading)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(heading):]
	for _, next := range []string{"\n### ", "\n## ", "\n# "} {
		if j := strings.Index(rest, next); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest, true
}

// TestAnMP4WithNoMovieBoxIsRefusedByNewReader.
//
// ⛔ This closes a gap the coverage gate could not see. NewReader's
// mp4File-failed branch was never entered, and the package total still PRINTED
// 100.0% because the gate reads the rounded aggregate -- 94.1% on one function
// disappears into it. A gate passing on rounding is the same as no gate for
// whatever it rounds away.
//
// The input is the smallest thing that gets there: a bare ftyp box. It sniffs
// as MP4 (ftyp sits at offset 4), it decodes without error because a file of
// one valid box is a valid file, and then it has no moov -- which is the one
// condition mp4File refuses on.
func TestAnMP4WithNoMovieBoxIsRefusedByNewReader(t *testing.T) {
	ftypOnly := []byte{
		0x00, 0x00, 0x00, 0x10, // box size: 16
		'f', 't', 'y', 'p',
		'i', 's', 'o', 'm', // major brand
		0x00, 0x00, 0x02, 0x00, // minor version
	}
	if got := Sniff(ftypOnly); got != FormatMP4 {
		t.Fatalf("Sniff = %v, want FormatMP4; the rest of this test depends on it", got)
	}
	r, err := NewReader(ftypOnly)
	if err == nil {
		t.Fatalf("a file with no movie box was accepted: %+v", r)
	}
	if !strings.Contains(err.Error(), "moov") {
		t.Fatalf("error = %v, want it to name the missing moov box", err)
	}
}
