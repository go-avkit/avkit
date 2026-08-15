// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// --- Sniff -----------------------------------------------------------------

func TestSniff(t *testing.T) {
	ftyp := append([]byte{0, 0, 0, 0x18}, []byte("ftypisom")...)
	ftyp = append(ftyp, make([]byte, 16)...)
	cases := []struct {
		name string
		data []byte
		want Format
	}{
		{"mp4", ftyp, FormatMP4},
		{"matroska", []byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0}, FormatMatroska},
		{"too-short-for-mp4", []byte("ftyp"), FormatUnknown},
		{"unknown", []byte("not a media file at all"), FormatUnknown},
		{"empty", nil, FormatUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Sniff(c.data); got != c.want {
				t.Fatalf("Sniff = %v, want %v", got, c.want)
			}
		})
	}
}

// --- Demux dispatch + fixtures ---------------------------------------------

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestDemuxMP4Fixture(t *testing.T) {
	f, err := Demux(readFixture(t, "tiny.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != "mp4" || f.Brand != "isom" {
		t.Fatalf("format/brand = %q/%q", f.Format, f.Brand)
	}
	if got := f.DurationSeconds(); got < 0.19 || got > 0.21 {
		t.Fatalf("duration = %.3fs, want ~0.2", got)
	}
	vt := f.VideoTracks()
	if len(vt) != 1 {
		t.Fatalf("video tracks = %d, want 1", len(vt))
	}
	if vt[0].Codec != "avc1" || vt[0].Width != 32 || vt[0].Height != 24 {
		t.Fatalf("video = %q %dx%d", vt[0].Codec, vt[0].Width, vt[0].Height)
	}
	if vt[0].Kind != Video || vt[0].Kind.String() != "video" {
		t.Fatalf("kind = %v", vt[0].Kind)
	}
}

func TestDemuxMatroskaFixture(t *testing.T) {
	f, err := Demux(readFixture(t, "tiny.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != "matroska" {
		t.Fatalf("format = %q", f.Format)
	}
	if len(f.Tracks) != 1 || f.Tracks[0].Kind != Video {
		t.Fatalf("tracks = %+v", f.Tracks)
	}
	if f.Tracks[0].Width != 32 || f.Tracks[0].Height != 24 {
		t.Fatalf("size = %dx%d", f.Tracks[0].Width, f.Tracks[0].Height)
	}
	// Track duration is inherited from the segment.
	if got := f.Tracks[0].DurationSeconds(); got < 0.19 || got > 0.21 {
		t.Fatalf("track duration = %.3fs, want ~0.2", got)
	}
}

func TestDemuxUnknown(t *testing.T) {
	if _, err := Demux([]byte("plain text, not a container")); err == nil {
		t.Fatal("expected error for unrecognised container")
	}
}

// --- value helpers ----------------------------------------------------------

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{
		Video: "video", Audio: "audio", Subtitle: "subtitle", Other: "other", Kind(99): "other",
	} {
		if got := k.String(); got != want {
			t.Fatalf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

func TestDurationSeconds(t *testing.T) {
	if got := (Track{Timescale: 0, Duration: 5}).DurationSeconds(); got != 0 {
		t.Fatalf("zero-timescale track = %v, want 0", got)
	}
	if got := (Track{Timescale: 1000, Duration: 500}).DurationSeconds(); got != 0.5 {
		t.Fatalf("track = %v, want 0.5", got)
	}
	if got := (&File{Timescale: 0, Duration: 5}).DurationSeconds(); got != 0 {
		t.Fatalf("zero-timescale file = %v, want 0", got)
	}
	if got := (&File{Timescale: 600, Duration: 1200}).DurationSeconds(); got != 2 {
		t.Fatalf("file = %v, want 2", got)
	}
}

func TestTracksOfKind(t *testing.T) {
	f := &File{Tracks: []Track{
		{ID: 1, Kind: Video}, {ID: 2, Kind: Audio}, {ID: 3, Kind: Audio}, {ID: 4, Kind: Subtitle},
	}}
	if got := f.VideoTracks(); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("video = %+v", got)
	}
	if got := f.AudioTracks(); len(got) != 2 {
		t.Fatalf("audio = %+v", got)
	}
}

// --- MP4 projection ---------------------------------------------------------

func TestKindFromHandler(t *testing.T) {
	for h, want := range map[string]Kind{
		"vide": Video, "soun": Audio, "subt": Subtitle, "sbtl": Subtitle, "text": Subtitle, "meta": Other,
	} {
		if got := kindFromHandler(h); got != want {
			t.Fatalf("kindFromHandler(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestMP4TrackBranches(t *testing.T) {
	// Track with only a Tkhd (no Mdia) exercises the early return.
	tr := mp4Track(&mp4.TrakBox{Tkhd: &mp4.TkhdBox{TrackID: 7, Width: mp4.Fixed32(64 << 16), Height: mp4.Fixed32(48 << 16)}})
	if tr.ID != 7 || tr.Width != 64 || tr.Height != 48 {
		t.Fatalf("tkhd-only track = %+v", tr)
	}
	if tr.Kind != Other || tr.Codec != "" {
		t.Fatalf("expected empty media info, got %+v", tr)
	}

	// Full audio track: Mdhd + soun Hdlr + audio sample entry overrides dims/rate.
	stsd := &mp4.StsdBox{}
	ase := mp4.NewAudioSampleEntryBox("mp4a")
	ase.ChannelCount, ase.SampleRate = 2, 48000
	stsd.AddChild(ase)
	trak := &mp4.TrakBox{
		Tkhd: &mp4.TkhdBox{TrackID: 2},
		Mdia: &mp4.MdiaBox{
			Mdhd: &mp4.MdhdBox{Timescale: 48000, Duration: 96000},
			Hdlr: &mp4.HdlrBox{HandlerType: "soun"},
			Minf: &mp4.MinfBox{Stbl: &mp4.StblBox{Stsd: stsd}},
		},
	}
	tr = mp4Track(trak)
	if tr.Kind != Audio || tr.Codec != "mp4a" || tr.Channels != 2 || tr.SampleRate != 48000 {
		t.Fatalf("audio track = %+v", tr)
	}
	if got := tr.DurationSeconds(); got != 2 {
		t.Fatalf("audio duration = %v, want 2", got)
	}
}

func TestApplySampleEntry(t *testing.T) {
	// Visual entry with a non-zero size overrides the track dimensions.
	tr := Track{}
	stsd := &mp4.StsdBox{}
	vse := mp4.NewVisualSampleEntryBox("hvc1")
	vse.Width, vse.Height = 1920, 1080
	stsd.AddChild(vse)
	applySampleEntry(&tr, stsd)
	if tr.Codec != "hvc1" || tr.Width != 1920 || tr.Height != 1080 {
		t.Fatalf("visual = %+v", tr)
	}

	// Visual entry with zero size leaves the (tkhd-derived) dimensions untouched.
	tr = Track{Width: 320, Height: 240}
	stsd = &mp4.StsdBox{}
	stsd.AddChild(mp4.NewVisualSampleEntryBox("avc1"))
	applySampleEntry(&tr, stsd)
	if tr.Width != 320 || tr.Height != 240 {
		t.Fatalf("zero-size visual clobbered dims: %+v", tr)
	}

	// A sample entry that is neither visual nor audio touches neither the video
	// dimensions nor the audio fields.
	tr = Track{}
	stsd = &mp4.StsdBox{}
	stsd.AddChild(&mp4.FreeBox{})
	applySampleEntry(&tr, stsd)
	if tr.Width != 0 || tr.Height != 0 || tr.Channels != 0 || tr.SampleRate != 0 {
		t.Fatalf("other entry perturbed dims/audio: %+v", tr)
	}

	// An empty stsd makes GetSampleDescription fail; the track is left unchanged.
	tr = Track{Codec: "keep"}
	applySampleEntry(&tr, &mp4.StsdBox{})
	if tr.Codec != "keep" {
		t.Fatalf("empty stsd clobbered track: %+v", tr)
	}
}

func TestDemuxMP4NoMoov(t *testing.T) {
	// A valid ftyp box with no moov: mp4ff decodes it, our wrapper reports it.
	var buf bytes.Buffer
	if err := mp4.NewFtyp("isom", 0, []string{"isom"}).Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := demuxMP4(buf.Bytes()); err == nil {
		t.Fatal("expected 'no moov' error")
	}
}

func TestDemuxMP4DecodeError(t *testing.T) {
	// Sniffs as MP4 (ftyp at [4:8]) but the box stream is malformed.
	bad := append([]byte{0x00, 0x00, 0x00, 0x08}, []byte("ftyp")...)
	bad = append(bad, 0xFF, 0xFF, 0xFF, 0xFF, 'j', 'u', 'n', 'k')
	if _, err := Demux(bad); err == nil {
		t.Fatal("expected decode error")
	}
}

// --- Matroska projection ----------------------------------------------------

func TestKindFromTrackType(t *testing.T) {
	for tt, want := range map[uint64]Kind{
		mkvTrackVideo: Video, mkvTrackAudio: Audio, mkvTrackSubtitle: Subtitle, 99: Other,
	} {
		if got := kindFromTrackType(tt); got != want {
			t.Fatalf("kindFromTrackType(%d) = %v, want %v", tt, got, want)
		}
	}
}

func TestMatroskaFormat(t *testing.T) {
	if matroskaFormat("webm") != "webm" || matroskaFormat("matroska") != "matroska" || matroskaFormat("") != "matroska" {
		t.Fatal("matroskaFormat mapping wrong")
	}
}

func TestMKVTrackBranches(t *testing.T) {
	v := mkvTrack(mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_VP9", Video: &mkvVideo{PixelWidth: 640, PixelHeight: 480}}, 1000, 5000)
	if v.Kind != Video || v.Width != 640 || v.Height != 480 || v.Duration != 5000 {
		t.Fatalf("video track = %+v", v)
	}
	a := mkvTrack(mkvTrackEntry{TrackNumber: 2, TrackType: mkvTrackAudio, CodecID: "A_OPUS", Audio: &mkvAudio{Channels: 2, SamplingFrequency: 48000}}, 1000, 5000)
	if a.Kind != Audio || a.Channels != 2 || a.SampleRate != 48000 {
		t.Fatalf("audio track = %+v", a)
	}
	s := mkvTrack(mkvTrackEntry{TrackNumber: 3, TrackType: mkvTrackSubtitle, CodecID: "S_TEXT/UTF8"}, 1000, 5000)
	if s.Kind != Subtitle || s.Width != 0 || s.Channels != 0 {
		t.Fatalf("subtitle track = %+v", s)
	}
}

// marshalMKV synthesises a minimal EBML/Matroska document from write structs.
func marshalMKV(t *testing.T, doc any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := ebml.Marshal(doc, &buf); err != nil {
		t.Fatalf("marshal mkv: %v", err)
	}
	return buf.Bytes()
}

// Write structs mirroring the Matroska tree (ebml-go marshals by tag).
type wDoc struct {
	Header  wHeader  `ebml:"EBML"`
	Segment wSegment `ebml:"Segment"`
}
type wHeader struct {
	DocType string `ebml:"EBMLDocType"`
}
type wSegment struct {
	Info   wInfo   `ebml:"Info"`
	Tracks wTracks `ebml:"Tracks"`
}
type wInfo struct {
	TimecodeScale uint64  `ebml:"TimecodeScale,omitempty"`
	Duration      float64 `ebml:"Duration,omitempty"`
}
type wTracks struct {
	TrackEntry []wTrackEntry `ebml:"TrackEntry"`
}
type wTrackEntry struct {
	TrackNumber uint64  `ebml:"TrackNumber"`
	TrackType   uint64  `ebml:"TrackType"`
	CodecID     string  `ebml:"CodecID"`
	Audio       *wAudio `ebml:"Audio"`
}
type wAudio struct {
	SamplingFrequency float64 `ebml:"SamplingFrequency"`
	Channels          uint64  `ebml:"Channels"`
}

func TestDemuxMatroskaSynthesised(t *testing.T) {
	// WebM doctype + an audio track + NO TimecodeScale (exercises the default).
	data := marshalMKV(t, &wDoc{
		Header: wHeader{DocType: "webm"},
		Segment: wSegment{
			Info: wInfo{Duration: 1000}, // ms, since default TimecodeScale = 1e6 ns
			Tracks: wTracks{TrackEntry: []wTrackEntry{
				{TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_OPUS", Audio: &wAudio{Channels: 2, SamplingFrequency: 48000}},
			}},
		},
	})
	if Sniff(data) != FormatMatroska {
		t.Fatal("synthesised bytes did not sniff as Matroska")
	}
	f, err := Demux(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != "webm" {
		t.Fatalf("format = %q, want webm", f.Format)
	}
	if f.Timescale != 1000 { // 1e9 / default 1e6
		t.Fatalf("timescale = %d, want 1000 (default scale)", f.Timescale)
	}
	if got := f.DurationSeconds(); got != 1 {
		t.Fatalf("duration = %v, want 1", got)
	}
	at := f.AudioTracks()
	if len(at) != 1 || at[0].Codec != "A_OPUS" || at[0].Channels != 2 || at[0].SampleRate != 48000 {
		t.Fatalf("audio = %+v", at)
	}
}

func TestDemuxMatroskaError(t *testing.T) {
	// EBML magic (so Sniff dispatches here) wrapping an element ID that is not in
	// the Matroska schema — ebml-go rejects it, and the wrapper surfaces the error.
	bad := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x84, 0x80, 0x80, 0x81, 0x00}
	if Sniff(bad) != FormatMatroska {
		t.Fatal("precondition: bytes must sniff as Matroska")
	}
	if _, err := Demux(bad); err == nil {
		t.Fatal("expected Matroska decode error for an unknown element")
	}
}
