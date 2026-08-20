// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// avcParameterSets lifts real parameter sets out of the fixture, so the muxer
// is exercised with sample entries a decoder would accept.
func avcParameterSets(t *testing.T) (sps, pps [][]byte, width, height int) {
	t.Helper()
	f, err := os.Open("testdata/tiny.mp4")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	defer f.Close()
	parsed, err := mp4.DecodeFile(f)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for _, trak := range parsed.Moov.Traks {
		if avcx := trak.Mdia.Minf.Stbl.Stsd.AvcX; avcx != nil {
			return avcx.AvcC.SPSnalus, avcx.AvcC.PPSnalus,
				int(avcx.Width), int(avcx.Height)
		}
	}
	t.Fatal("the fixture holds no AVC track")
	return nil, nil, 0, 0
}

func videoConfig(t *testing.T) TrackConfig {
	t.Helper()
	sps, pps, w, h := avcParameterSets(t)
	return TrackConfig{
		Kind: Video, Codec: "avc1", Timescale: 12800,
		Width: w, Height: h, SPS: sps, PPS: pps, Language: "und",
	}
}

func audioConfig() TrackConfig {
	return TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000,
		Channels: 2, SampleRate: 48000, Language: "eng"}
}

func TestMuxRoundTripsThroughDemux(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatalf("AddTrack(video): %v", err)
	}
	audio, err := m.AddTrack(audioConfig())
	if err != nil {
		t.Fatalf("AddTrack(audio): %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := m.WriteSample(video, Sample{
			Data: []byte{0, 0, 0, 2, 9, 16}, Duration: 512, Sync: i == 0,
		}); err != nil {
			t.Fatalf("WriteSample(video, %d): %v", i, err)
		}
		if err := m.WriteSample(audio, Sample{
			Data: []byte{0xde, 0xad}, Duration: 1024, Sync: true,
		}); err != nil {
			t.Fatalf("WriteSample(audio, %d): %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// What this muxer writes, this package's own demuxer must read back.
	file, err := Demux(buf.Bytes())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if file.Format != "mp4" {
		t.Fatalf("Format = %q", file.Format)
	}
	if len(file.Tracks) != 2 {
		t.Fatalf("tracks = %+v", file.Tracks)
	}
	v := file.VideoTracks()
	a := file.AudioTracks()
	if len(v) != 1 || len(a) != 1 {
		t.Fatalf("kinds = %+v", file.Tracks)
	}
	_, _, wantW, wantH := avcParameterSets(t)
	if v[0].Codec != "avc1" || v[0].Width != wantW || v[0].Height != wantH {
		t.Errorf("video track = %+v", v[0])
	}
	if v[0].Timescale != 12800 {
		t.Errorf("video timescale = %d", v[0].Timescale)
	}
	if a[0].Codec != "mp4a" || a[0].SampleRate != 48000 || a[0].Language != "eng" {
		t.Errorf("audio track = %+v", a[0])
	}
}

// countBoxes reports how many top-level boxes of a name the output holds.
func countBoxes(t *testing.T, data []byte, name string) int {
	t.Helper()
	n, offset := 0, 0
	for offset+8 <= len(data) {
		size := int(uint32(data[offset])<<24 | uint32(data[offset+1])<<16 |
			uint32(data[offset+2])<<8 | uint32(data[offset+3]))
		if size < 8 || offset+size > len(data) {
			break
		}
		if string(data[offset+4:offset+8]) == name {
			n++
		}
		offset += size
	}
	return n
}

func TestMuxWritesOneFragmentPerWindow(t *testing.T) {
	var buf bytes.Buffer
	// A window of one sample: every sync sample opens a new fragment.
	m := NewMuxer(&buf, FragmentDuration(40*time.Millisecond), Brand("iso6"))
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := m.WriteSample(video, Sample{
			Data: []byte{0, 0, 0, 1, 9}, Duration: 512, Sync: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := buf.Bytes()
	if got := countBoxes(t, out, "ftyp"); got != 1 {
		t.Errorf("ftyp boxes = %d", got)
	}
	if got := countBoxes(t, out, "moov"); got != 1 {
		t.Errorf("moov boxes = %d", got)
	}
	if got := countBoxes(t, out, "moof"); got != 4 {
		t.Errorf("moof boxes = %d, want one per sample at this window", got)
	}
	if got := countBoxes(t, out, "mdat"); got != 4 {
		t.Errorf("mdat boxes = %d", got)
	}
	if !bytes.Contains(out[:32], []byte("iso6")) {
		t.Errorf("the chosen brand is missing from ftyp: %q", out[:32])
	}
	if _, err := Demux(out); err != nil {
		t.Fatalf("a fragmented file must still demux: %v", err)
	}
}

func TestMuxAV1(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	// The minimal av1C record: marker and version, then the sequence
	// profile, level and tier bytes.
	id, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "av01", Timescale: 30000, Width: 640, Height: 360,
		CodecConfig: []byte{0x81, 0x00, 0x0c, 0x00},
	})
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{0x12, 0x00}, Duration: 1001, Sync: true}); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	file, err := Demux(buf.Bytes())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if len(file.Tracks) != 1 || file.Tracks[0].Codec != "av01" {
		t.Fatalf("tracks = %+v", file.Tracks)
	}
	if file.Tracks[0].Width != 640 || file.Tracks[0].Height != 360 {
		t.Errorf("dimensions = %dx%d", file.Tracks[0].Width, file.Tracks[0].Height)
	}
}

func TestMuxSubtitleAndUnknownKindsGetAHandler(t *testing.T) {
	cases := map[Kind]string{Video: "video", Audio: "audio", Subtitle: "subtitle", Other: "video"}
	for kind, want := range cases {
		if got := handlerFor(kind); got != want {
			t.Errorf("handlerFor(%v) = %q, want %q", kind, got, want)
		}
	}
}

func TestAddTrackRejectsWhatItCannotDescribe(t *testing.T) {
	sps, pps, _, _ := avcParameterSets(t)
	cases := map[string]TrackConfig{
		"no timescale":       {Kind: Video, Codec: "avc1", SPS: sps, PPS: pps},
		"unknown codec":      {Kind: Video, Codec: "theora", Timescale: 1000},
		"avc without pps":    {Kind: Video, Codec: "avc1", Timescale: 1000, SPS: sps},
		"avc without sps":    {Kind: Video, Codec: "avc1", Timescale: 1000, PPS: pps},
		"hevc without vps":   {Kind: Video, Codec: "hvc1", Timescale: 1000, SPS: sps, PPS: pps},
		"aac without rate":   {Kind: Audio, Codec: "mp4a", Timescale: 48000},
		"av1 without config": {Kind: Video, Codec: "av01", Timescale: 30000},
		"av1 with a broken config": {Kind: Video, Codec: "av01", Timescale: 30000,
			CodecConfig: []byte{0x00}},
	}
	for name, cfg := range cases {
		m := NewMuxer(io.Discard)
		if _, err := m.AddTrack(cfg); err == nil {
			t.Errorf("%s: AddTrack succeeded", name)
		} else if !errors.Is(err, ErrTrackConfig) && !errors.Is(err, ErrUnsupportedCodec) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestAddTrackRefusedOnceWritingBegan(t *testing.T) {
	m := NewMuxer(io.Discard)
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 10, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTrack(audioConfig()); !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("err = %v, want ErrTrackConfig", err)
	}
}

func TestWriteSampleRejections(t *testing.T) {
	empty := NewMuxer(io.Discard)
	if err := empty.WriteSample(1, Sample{Data: []byte{1}, Duration: 1}); !errors.Is(err, ErrNoTracks) {
		t.Errorf("without a track: %v", err)
	}
	m := NewMuxer(io.Discard)
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Duration: 10}); !errors.Is(err, ErrSample) {
		t.Errorf("no data: %v", err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}}); !errors.Is(err, ErrSample) {
		t.Errorf("no duration: %v", err)
	}
	if err := m.WriteSample(id+99, Sample{Data: []byte{1}, Duration: 10}); !errors.Is(err, ErrUnknownTrack) {
		t.Errorf("unknown track: %v", err)
	}
}

func TestCloseWithoutTracks(t *testing.T) {
	m := NewMuxer(io.Discard)
	if err := m.Close(); !errors.Is(err, ErrNoTracks) {
		t.Fatalf("err = %v, want ErrNoTracks", err)
	}
	// Everything refuses to work afterwards.
	if _, err := m.AddTrack(audioConfig()); !errors.Is(err, ErrClosed) {
		t.Errorf("AddTrack after Close: %v", err)
	}
	if err := m.WriteSample(1, Sample{Data: []byte{1}, Duration: 1}); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteSample after Close: %v", err)
	}
	if err := m.Flush(); !errors.Is(err, ErrClosed) {
		t.Errorf("Flush after Close: %v", err)
	}
	if err := m.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("Close twice: %v", err)
	}
}

func TestCloseWritesTheInitSegmentOfATrackWithoutSamples(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	if _, err := m.AddTrack(videoConfig(t)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if countBoxes(t, buf.Bytes(), "moov") != 1 {
		t.Fatalf("a file naming its track must still be written: %d bytes", buf.Len())
	}
	if countBoxes(t, buf.Bytes(), "moof") != 0 {
		t.Error("no sample means no fragment")
	}
}

func TestFlushWithNothingBuffered(t *testing.T) {
	m := NewMuxer(io.Discard)
	if _, err := m.AddTrack(videoConfig(t)); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// failWriter fails after letting a chosen number of bytes through.
type failWriter struct {
	allow int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.allow <= 0 {
		return 0, errors.New("disk full")
	}
	if len(p) > f.allow {
		n := f.allow
		f.allow = 0
		return n, errors.New("disk full")
	}
	f.allow -= len(p)
	return len(p), nil
}

func TestWriteFailures(t *testing.T) {
	// The initialisation segment cannot be written.
	m := NewMuxer(&failWriter{})
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 10, Sync: true}); err == nil {
		t.Fatal("a failing writer was ignored")
	}

	// The init segment goes through, the fragment does not.
	var sized bytes.Buffer
	probe := NewMuxer(&sized)
	pid, err := probe.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.WriteSample(pid, Sample{Data: []byte{1}, Duration: 10, Sync: true}); err != nil {
		t.Fatal(err)
	}
	initSize := sized.Len()

	m2 := NewMuxer(&failWriter{allow: initSize})
	id2, err := m2.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.WriteSample(id2, Sample{Data: []byte{1}, Duration: 10, Sync: true}); err != nil {
		t.Fatalf("the init segment should still fit: %v", err)
	}
	if err := m2.Close(); err == nil {
		t.Fatal("a failing fragment write was ignored")
	}

	// Close alone, with a writer that refuses the init segment.
	m3 := NewMuxer(&failWriter{})
	if _, err := m3.AddTrack(videoConfig(t)); err != nil {
		t.Fatal(err)
	}
	if err := m3.Close(); err == nil {
		t.Fatal("a failing init write was ignored on Close")
	}
}

func TestFlushErrorDuringWriteSample(t *testing.T) {
	var sized bytes.Buffer
	probe := NewMuxer(&sized, FragmentDuration(10*time.Millisecond))
	pid, _ := probe.AddTrack(videoConfig(t))
	_ = probe.WriteSample(pid, Sample{Data: []byte{1}, Duration: 512, Sync: true})
	initSize := sized.Len()

	m := NewMuxer(&failWriter{allow: initSize}, FragmentDuration(10*time.Millisecond))
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512, Sync: true}); err != nil {
		t.Fatalf("first sample: %v", err)
	}
	// This one closes the fragment, which the writer refuses.
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512, Sync: true}); err == nil {
		t.Fatal("the failing flush was not reported")
	}
}

func TestOptionsFallBackToDefaults(t *testing.T) {
	m := NewMuxer(io.Discard, FragmentDuration(0), Brand(""))
	if m.settings.fragmentDuration != DefaultFragmentDuration {
		t.Errorf("fragment duration = %v", m.settings.fragmentDuration)
	}
	if m.settings.brand != DefaultBrand {
		t.Errorf("brand = %q", m.settings.brand)
	}
}

func TestNonSyncSamplesStayInTheSameFragment(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf, FragmentDuration(time.Millisecond))
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := m.WriteSample(id, Sample{
			Data: []byte{1, 2}, Duration: 512, Sync: i == 0, CompositionOffset: int32(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// Only the first sample is a sync sample, so nothing could be cut.
	if got := countBoxes(t, buf.Bytes(), "moof"); got != 1 {
		t.Fatalf("moof boxes = %d, want 1", got)
	}
}

func TestMuxRefusesParameterSetsItCannotRead(t *testing.T) {
	// AVC parameter sets are not HEVC ones: rather than write a file whose
	// sample entry says nothing, the muxer refuses the track.
	sps, pps, _, _ := avcParameterSets(t)
	m := NewMuxer(io.Discard)
	_, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "hvc1", Timescale: 90000, Width: 32, Height: 24,
		VPS: sps, SPS: sps, PPS: pps,
	})
	if !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("err = %v, want ErrTrackConfig", err)
	}
	if !strings.Contains(err.Error(), "not usable") {
		t.Errorf("the message must say what is wrong: %v", err)
	}
}

// stubBox stands in for a box of another type, to reach the guard that says so.
type stubBox struct{ mp4.Box }

func TestDecodeAv1CGuardsAgainstAnotherBox(t *testing.T) {
	original := decodeAv1CBox
	defer func() { decodeAv1CBox = original }()
	decodeAv1CBox = func(mp4.BoxHeader, uint64, io.Reader) (mp4.Box, error) {
		return stubBox{}, nil
	}
	if _, err := decodeAv1C([]byte{0x81, 0x00, 0x0c, 0x00}); !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("err = %v, want ErrTrackConfig", err)
	}
}

func TestFlushReportsAFragmentItCannotCreate(t *testing.T) {
	original := newFragment
	defer func() { newFragment = original }()
	newFragment = func(uint32, []uint32) (*mp4.Fragment, error) {
		return nil, errors.New("no fragment")
	}
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 10, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err == nil {
		t.Fatal("a fragment that cannot be created must be reported")
	}
}
