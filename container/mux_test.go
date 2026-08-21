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

// writeOneTrack writes a single-track fragmented MP4 and returns its bytes, so
// each codec below is exercised through the whole muxer rather than through
// describe alone.
func writeOneTrack(t *testing.T, cfg TrackConfig, samples int) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack(%s): %v", cfg.Codec, err)
	}
	for i := 0; i < samples; i++ {
		if err := m.WriteSample(id, Sample{
			Data: []byte{byte(i), 0x11, 0x22, 0x33}, Duration: 960, Sync: true,
		}); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// sampleEntryOf returns the first sample description of the written file, which
// is the thing a player reads to decide whether it can decode the track.
func sampleEntryOf(t *testing.T, data []byte) *mp4.StsdBox {
	t.Helper()
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode what was written: %v", err)
	}
	if parsed.Moov == nil || len(parsed.Moov.Traks) != 1 {
		t.Fatalf("the file does not hold exactly one track")
	}
	return parsed.Moov.Traks[0].Mdia.Minf.Stbl.Stsd
}

func TestMuxerWritesOpusFromTheTrackFields(t *testing.T) {
	cfg := TrackConfig{
		Kind: Audio, Codec: "Opus", Timescale: 48000,
		Channels: 2, SampleRate: 44100, PreSkip: 312,
	}
	data := writeOneTrack(t, cfg, 3)

	stsd := sampleEntryOf(t, data)
	if stsd.Opus == nil {
		t.Fatalf("no Opus sample entry: %v", stsd.Children)
	}
	// The entry states the rate the decoder outputs, whatever the track was
	// recorded at: that is what the Opus-in-ISOBMFF mapping requires.
	if stsd.Opus.SampleRate != opusOutputRate {
		t.Errorf("sample entry rate = %d, want %d", stsd.Opus.SampleRate, opusOutputRate)
	}
	if stsd.Opus.ChannelCount != 2 {
		t.Errorf("channel count = %d, want 2", stsd.Opus.ChannelCount)
	}
	d := stsd.Opus.Dops
	if d == nil {
		t.Fatal("the Opus entry carries no dOps")
	}
	// The recorded rate and the pre-skip live here, and they must survive
	// the big-endian encoding of the box.
	if d.InputSampleRate != 44100 || d.PreSkip != 312 || d.OutputChannelCount != 2 {
		t.Fatalf("dOps = %+v", d)
	}
	if d.ChannelMappingFamily != 0 || len(d.ChannelMapping) != 0 {
		t.Errorf("dOps maps channels explicitly: %+v", d)
	}

	// Read back through the package's own reader: the configuration a caller
	// gets must be the one it gave, and the samples must all be there.
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	ids := r.TrackIDs()
	if len(ids) != 1 {
		t.Fatalf("track ids = %v", ids)
	}
	got, err := r.TrackConfig(ids[0])
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if got.Codec != "Opus" || got.Channels != 2 || got.SampleRate != 44100 || got.PreSkip != 312 {
		t.Fatalf("read back %+v", got)
	}
	if want := opusHeadBytes(d); !bytes.Equal(got.CodecConfig, want) {
		t.Fatalf("identification header = % x, want % x", got.CodecConfig, want)
	}
	samples, err := r.Samples(ids[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("read %d samples, want 3", len(samples))
	}
	for i, s := range samples {
		if !s.Sync || s.Duration != 960 || s.Data[0] != byte(i) {
			t.Fatalf("sample %d = %+v", i, s)
		}
	}
}

func TestMuxerWritesOpusFromAnIdentificationHeader(t *testing.T) {
	// A five-channel track: family 0 could not describe it, so the mapping
	// the header states is the only way to write it.
	head := append([]byte(opusHeadMagic), 1, 5)
	head = append(head, 0x38, 0x01)             // pre-skip 312, little-endian
	head = append(head, 0x80, 0xBB, 0x00, 0x00) // 48000 Hz, little-endian
	head = append(head, 0x00, 0xFF)             // output gain -256, little-endian
	head = append(head, 1)                      // mapping family 1
	head = append(head, 3, 2)                   // 3 streams, 2 of them coupled
	head = append(head, 0, 4, 1, 2, 3)          // one index per channel

	data := writeOneTrack(t, TrackConfig{
		Kind: Audio, Codec: "opus", Timescale: 48000, CodecConfig: head,
	}, 2)

	d := sampleEntryOf(t, data).Opus.Dops
	if d.OutputChannelCount != 5 || d.PreSkip != 312 || d.InputSampleRate != 48000 {
		t.Fatalf("dOps = %+v", d)
	}
	if d.OutputGain != -256 {
		t.Errorf("output gain = %d, want -256", d.OutputGain)
	}
	if d.ChannelMappingFamily != 1 || d.StreamCount != 3 || d.CoupledCount != 2 {
		t.Fatalf("dOps = %+v", d)
	}
	if !bytes.Equal(d.ChannelMapping, []byte{0, 4, 1, 2, 3}) {
		t.Fatalf("channel mapping = % x", d.ChannelMapping)
	}
	// The header a caller gets back must be the one it gave, byte for byte:
	// that is what makes a WebM written from this file play the same.
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if !bytes.Equal(cfg.CodecConfig, head) {
		t.Fatalf("header = % x, want % x", cfg.CodecConfig, head)
	}
	// And OpusHead states the same thing from a configuration alone.
	again, err := OpusHead(cfg)
	if err != nil {
		t.Fatalf("OpusHead: %v", err)
	}
	if !bytes.Equal(again, head) {
		t.Fatalf("OpusHead = % x, want % x", again, head)
	}
}

func TestMuxerRefusesOpusItCannotDescribe(t *testing.T) {
	head := func(b ...byte) []byte { return append([]byte(opusHeadMagic), b...) }
	full := func(channels, family byte) []byte {
		return head(1, channels, 0, 0, 0x80, 0xBB, 0, 0, 0, 0, family)
	}
	cases := []struct {
		name string
		cfg  TrackConfig
	}{
		{"no channel count", TrackConfig{Codec: "Opus", Timescale: 48000, SampleRate: 48000}},
		{"no sample rate", TrackConfig{Codec: "Opus", Timescale: 48000, Channels: 2}},
		{"more channels than family 0 can map",
			TrackConfig{Codec: "Opus", Timescale: 48000, Channels: 6, SampleRate: 48000}},
		{"a header that is not one",
			TrackConfig{Codec: "Opus", Timescale: 48000, CodecConfig: []byte("not a header at all")}},
		{"a header that stops short",
			TrackConfig{Codec: "Opus", Timescale: 48000, CodecConfig: head(1, 2, 0, 0)}},
		{"a header version this cannot read",
			TrackConfig{Codec: "Opus", Timescale: 48000,
				CodecConfig: head(2, 2, 0, 0, 0x80, 0xBB, 0, 0, 0, 0, 0)}},
		{"a header stating no channel",
			TrackConfig{Codec: "Opus", Timescale: 48000, CodecConfig: full(0, 0)}},
		{"a header mapping too many channels implicitly",
			TrackConfig{Codec: "Opus", Timescale: 48000, CodecConfig: full(3, 0)}},
		{"a header that stops before its mapping",
			TrackConfig{Codec: "Opus", Timescale: 48000, CodecConfig: append(full(3, 1), 1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := NewMuxer(&buf).AddTrack(tc.cfg); !errors.Is(err, ErrTrackConfig) {
				t.Fatalf("err = %v, want ErrTrackConfig", err)
			}
			if buf.Len() != 0 {
				t.Errorf("%d bytes were written for a track that was refused", buf.Len())
			}
		})
	}
}

func TestMuxerWritesVP9(t *testing.T) {
	cfg := TrackConfig{
		Kind: Video, Codec: "vp09", Timescale: 90000, Width: 1920, Height: 1080,
		VPx: &VPxConfig{
			Profile: 2, Level: 41, BitDepth: 10, ChromaSubsampling: 3, FullRange: true,
			ColourPrimaries: 9, TransferCharacteristics: 16, MatrixCoefficients: 9,
		},
	}
	data := writeOneTrack(t, cfg, 2)

	stsd := sampleEntryOf(t, data)
	if stsd.VpXX == nil || stsd.VpXX.VppC == nil {
		t.Fatalf("no vp09 sample entry with a vpcC: %v", stsd.Children)
	}
	if stsd.VpXX.Type() != "vp09" {
		t.Errorf("sample entry = %s", stsd.VpXX.Type())
	}
	if stsd.VpXX.Width != 1920 || stsd.VpXX.Height != 1080 {
		t.Errorf("frame size = %dx%d", stsd.VpXX.Width, stsd.VpXX.Height)
	}
	v := stsd.VpXX.VppC
	// Every field matters to a decoder: a wrong bit depth or a wrong colour
	// code point plays as a picture, only the wrong one.
	if v.Profile != 2 || v.Level != 41 || v.BitDepth != 10 || v.ChromaSubsampling != 3 {
		t.Fatalf("vpcC = %+v", v)
	}
	if v.VideoFullRangeFlag != 1 {
		t.Errorf("full range flag = %d, want 1", v.VideoFullRangeFlag)
	}
	if v.ColourPrimaries != 9 || v.TransferCharacteristics != 16 || v.MatrixCoefficients != 9 {
		t.Fatalf("vpcC colour = %+v", v)
	}

	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if got.Codec != "vp09" || got.Width != 1920 || got.Height != 1080 {
		t.Fatalf("read back %+v", got)
	}
	if got.VPx == nil || *got.VPx != *cfg.VPx {
		t.Fatalf("read back VPx = %+v, want %+v", got.VPx, cfg.VPx)
	}
}

func TestMuxerWritesVP8(t *testing.T) {
	data := writeOneTrack(t, TrackConfig{
		Kind: Video, Codec: "vp08", Timescale: 90000, Width: 640, Height: 480,
		VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8, ColourPrimaries: 2,
			TransferCharacteristics: 2, MatrixCoefficients: 2},
	}, 1)
	stsd := sampleEntryOf(t, data)
	if stsd.VpXX == nil || stsd.VpXX.Type() != "vp08" {
		t.Fatalf("no vp08 sample entry: %v", stsd.Children)
	}
	if v := stsd.VpXX.VppC; v == nil || v.Level != 10 || v.BitDepth != 8 {
		t.Fatalf("vpcC = %+v", v)
	}
}

func TestMuxerRefusesVPxItCannotDescribe(t *testing.T) {
	sound := VPxConfig{Profile: 0, Level: 10, BitDepth: 8}
	with := func(f func(*VPxConfig)) *VPxConfig {
		v := sound
		f(&v)
		return &v
	}
	cases := []struct {
		name string
		cfg  TrackConfig
	}{
		{"no vpcC configuration at all",
			TrackConfig{Codec: "vp09", Timescale: 90000, Width: 640, Height: 480}},
		{"no frame size",
			TrackConfig{Codec: "vp09", Timescale: 90000, VPx: &sound}},
		{"a frame wider than a sample entry can state",
			TrackConfig{Codec: "vp09", Timescale: 90000, Width: 70000, Height: 480, VPx: &sound}},
		{"a frame taller than a sample entry can state",
			TrackConfig{Codec: "vp09", Timescale: 90000, Width: 640, Height: 70000, VPx: &sound}},
		{"a profile that does not exist",
			TrackConfig{Codec: "vp09", Timescale: 90000, Width: 640, Height: 480,
				VPx: with(func(v *VPxConfig) { v.Profile = 4 })}},
		{"no level",
			TrackConfig{Codec: "vp09", Timescale: 90000, Width: 640, Height: 480,
				VPx: with(func(v *VPxConfig) { v.Level = 0 })}},
		{"a bit depth no VP9 profile allows",
			TrackConfig{Codec: "vp09", Timescale: 90000, Width: 640, Height: 480,
				VPx: with(func(v *VPxConfig) { v.BitDepth = 9 })}},
		{"a chroma subsampling that does not exist",
			TrackConfig{Codec: "vp09", Timescale: 90000, Width: 640, Height: 480,
				VPx: with(func(v *VPxConfig) { v.ChromaSubsampling = 4 })}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := NewMuxer(&buf).AddTrack(tc.cfg); !errors.Is(err, ErrTrackConfig) {
				t.Fatalf("err = %v, want ErrTrackConfig", err)
			}
			if buf.Len() != 0 {
				t.Errorf("%d bytes were written for a track that was refused", buf.Len())
			}
		})
	}
}

// dac3Payload is an AC-3 configuration record as a container carries it: the
// content of the box, without its header.
func dac3Payload(t *testing.T, box mp4.Box) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := box.Encode(&buf); err != nil {
		t.Fatalf("encode %s: %v", box.Type(), err)
	}
	if buf.Len() <= 8 {
		t.Fatalf("%s encoded to %d bytes", box.Type(), buf.Len())
	}
	return buf.Bytes()[8:]
}

func TestMuxerWritesAC3AndEAC3(t *testing.T) {
	t.Run("ac-3", func(t *testing.T) {
		// 48 kHz, bit stream identification 8, stereo with no LFE.
		payload := dac3Payload(t, &mp4.Dac3Box{FSCod: 0, BSID: 8, ACMod: 2, BitRateCode: 10})
		data := writeOneTrack(t, TrackConfig{
			Kind: Audio, Codec: "ac-3", Timescale: 48000, CodecConfig: payload,
		}, 2)
		stsd := sampleEntryOf(t, data)
		if stsd.AC3 == nil || stsd.AC3.Dac3 == nil {
			t.Fatalf("no ac-3 sample entry with a dac3: %v", stsd.Children)
		}
		if d := stsd.AC3.Dac3; d.BSID != 8 || d.ACMod != 2 || d.BitRateCode != 10 {
			t.Fatalf("dac3 = %+v", d)
		}
		r, err := NewReader(data)
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		cfg, err := r.TrackConfig(r.TrackIDs()[0])
		if err != nil {
			t.Fatalf("TrackConfig: %v", err)
		}
		if cfg.Codec != "ac-3" {
			t.Errorf("codec = %q", cfg.Codec)
		}
		if !bytes.Equal(cfg.CodecConfig, payload) {
			t.Fatalf("record = % x, want % x", cfg.CodecConfig, payload)
		}
	})
	t.Run("ec-3", func(t *testing.T) {
		payload := dac3Payload(t, &mp4.Dec3Box{
			DataRate: 192, NumIndSub: 1,
			EC3Subs: []mp4.EC3Sub{{FSCod: 0, BSID: 16, ACMod: 2}},
		})
		data := writeOneTrack(t, TrackConfig{
			Kind: Audio, Codec: "ec-3", Timescale: 48000, CodecConfig: payload,
		}, 2)
		stsd := sampleEntryOf(t, data)
		if stsd.EC3 == nil || stsd.EC3.Dec3 == nil {
			t.Fatalf("no ec-3 sample entry with a dec3: %v", stsd.Children)
		}
		if d := stsd.EC3.Dec3; d.DataRate != 192 || len(d.EC3Subs) != 1 {
			t.Fatalf("dec3 = %+v", d)
		}
		r, err := NewReader(data)
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		cfg, err := r.TrackConfig(r.TrackIDs()[0])
		if err != nil {
			t.Fatalf("TrackConfig: %v", err)
		}
		if !bytes.Equal(cfg.CodecConfig, payload) {
			t.Fatalf("record = % x, want % x", cfg.CodecConfig, payload)
		}
	})
}

func TestMuxerRefusesAC3ItCannotDescribe(t *testing.T) {
	for _, codec := range []string{"ac-3", "ec-3"} {
		t.Run(codec+" with no record", func(t *testing.T) {
			var buf bytes.Buffer
			_, err := NewMuxer(&buf).AddTrack(TrackConfig{Codec: codec, Timescale: 48000})
			if !errors.Is(err, ErrTrackConfig) {
				t.Fatalf("err = %v, want ErrTrackConfig", err)
			}
		})
		t.Run(codec+" with a record that is not one", func(t *testing.T) {
			var buf bytes.Buffer
			_, err := NewMuxer(&buf).AddTrack(TrackConfig{
				Codec: codec, Timescale: 48000, CodecConfig: []byte{0x01},
			})
			if !errors.Is(err, ErrTrackConfig) {
				t.Fatalf("err = %v, want ErrTrackConfig", err)
			}
		})
	}
}

// TestConfigRecordDecodedAsAnotherBox covers the guard that would catch a
// decoder handing back a box of the wrong type: the sample entry would
// otherwise be built from something that is not the record it claims to be.
func TestConfigRecordDecodedAsAnotherBox(t *testing.T) {
	dac3, dec3 := decodeDac3Box, decodeDec3Box
	defer func() { decodeDac3Box, decodeDec3Box = dac3, dec3 }()
	wrong := func(mp4.BoxHeader, uint64, io.Reader) (mp4.Box, error) {
		return &mp4.FreeBox{}, nil
	}
	decodeDac3Box, decodeDec3Box = wrong, wrong
	for _, codec := range []string{"ac-3", "ec-3"} {
		var buf bytes.Buffer
		_, err := NewMuxer(&buf).AddTrack(TrackConfig{
			Codec: codec, Timescale: 48000, CodecConfig: []byte{0x01, 0x02, 0x03, 0x04, 0x05},
		})
		if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "decoded as") {
			t.Fatalf("%s: err = %v, want the wrong-type guard", codec, err)
		}
	}
}

// TestMuxerRefusesAC3RecordsThatWouldCrashTheLibrary guards a real trap: the
// sample rate table the sample entry is built from has three entries, and it is
// indexed by a code a record can state as any of four values. Three bytes of
// untrusted container data are enough to run off its end, so the record is
// checked here before it gets that far.
func TestMuxerRefusesAC3RecordsThatWouldCrashTheLibrary(t *testing.T) {
	cases := []struct {
		name, codec string
		payload     []byte
	}{
		{"a dac3 stating a reserved sample rate code", "ac-3", []byte{0xFF, 0xFF, 0xFF}},
		{"a dac3 too short to be one", "ac-3", []byte{0x01}},
		{"a dec3 too short to be one", "ec-3", []byte{0x01, 0x02}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			// A panic here is the failure this test exists to catch, so it is
			// not recovered: the test must crash loudly if the guard goes.
			_, err := NewMuxer(&buf).AddTrack(TrackConfig{
				Codec: tc.codec, Timescale: 48000, CodecConfig: tc.payload,
			})
			if !errors.Is(err, ErrTrackConfig) {
				t.Fatalf("err = %v, want ErrTrackConfig", err)
			}
		})
	}
}

// TestMuxerRefusesADec3WithNoSubstream stages what the decoder would hand back
// for a record naming nothing. The sample entry is built from the first
// substream, so an empty list would be read as one that is there.
func TestMuxerRefusesADec3WithNoSubstream(t *testing.T) {
	original := decodeDec3Box
	defer func() { decodeDec3Box = original }()
	decodeDec3Box = func(mp4.BoxHeader, uint64, io.Reader) (mp4.Box, error) {
		return &mp4.Dec3Box{DataRate: 192}, nil
	}
	var buf bytes.Buffer
	_, err := NewMuxer(&buf).AddTrack(TrackConfig{
		Codec: "ec-3", Timescale: 48000, CodecConfig: []byte{1, 2, 3, 4, 5},
	})
	if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "no substream") {
		t.Fatalf("err = %v, want the empty record refused", err)
	}
}

// TestMuxerRefusesADec3SubstreamWithAReservedRate covers the substreams past
// the first, which the sample entry does not read but a player does.
func TestMuxerRefusesADec3SubstreamWithAReservedRate(t *testing.T) {
	payload := dac3Payload(t, &mp4.Dec3Box{
		DataRate: 192, NumIndSub: 2,
		EC3Subs: []mp4.EC3Sub{{FSCod: 0, BSID: 16, ACMod: 2}, {FSCod: 3, BSID: 16, ACMod: 2}},
	})
	var buf bytes.Buffer
	_, err := NewMuxer(&buf).AddTrack(TrackConfig{
		Codec: "ec-3", Timescale: 48000, CodecConfig: payload,
	})
	if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "substream 1") {
		t.Fatalf("err = %v, want the second substream named", err)
	}
}

func TestMuxerWritesStereoOpusFromAnImplicitMapping(t *testing.T) {
	// Mapping family 0: the two channels are left and right, and the header
	// says nothing more.
	head := append([]byte(opusHeadMagic), 1, 2)
	head = append(head, 0x58, 0x02)             // pre-skip 600
	head = append(head, 0x44, 0xAC, 0x00, 0x00) // 44100 Hz
	head = append(head, 0x00, 0x00)             // no output gain
	head = append(head, 0)                      // mapping family 0
	data := writeOneTrack(t, TrackConfig{
		Kind: Audio, Codec: "Opus", Timescale: 48000, CodecConfig: head,
	}, 2)
	d := sampleEntryOf(t, data).Opus.Dops
	if d.OutputChannelCount != 2 || d.PreSkip != 600 || d.InputSampleRate != 44100 {
		t.Fatalf("dOps = %+v", d)
	}
	if d.ChannelMappingFamily != 0 || len(d.ChannelMapping) != 0 {
		t.Fatalf("an implicit mapping was written as an explicit one: %+v", d)
	}
	if got := opusHeadBytes(d); !bytes.Equal(got, head) {
		t.Fatalf("header = % x, want % x", got, head)
	}
}

func TestOpusHeadRefusesAConfigurationItCannotState(t *testing.T) {
	if _, err := OpusHead(TrackConfig{Codec: "Opus", Timescale: 48000}); !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("err = %v, want ErrTrackConfig", err)
	}
}

func TestMuxerReportsARecordItCannotDecode(t *testing.T) {
	cases := []struct {
		name, codec string
		payload     []byte
	}{
		// Four bytes means one leading byte that must be zero, and is not.
		{"a dac3 with a non-zero leading byte", "ac-3", []byte{0xFF, 0x00, 0x00, 0x00}},
		// A record claiming eight substreams in five bytes.
		{"a dec3 claiming more substreams than it holds", "ec-3", []byte{0x00, 0x07, 0x00, 0x00, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			_, err := NewMuxer(&buf).AddTrack(TrackConfig{
				Codec: tc.codec, Timescale: 48000, CodecConfig: tc.payload,
			})
			if !errors.Is(err, ErrTrackConfig) {
				t.Fatalf("err = %v, want ErrTrackConfig", err)
			}
		})
	}
}

// TestMuxerReportsAFailureToBuildASampleEntry stages the library refusing to
// build the entry. It cannot happen with the values this package validates
// first, and it must still be reported rather than leaving a track with no
// sample entry at all.
func TestMuxerReportsAFailureToBuildASampleEntry(t *testing.T) {
	vpx, ac3, ec3 := setVPxEntry, setAC3Entry, setEC3Entry
	defer func() { setVPxEntry, setAC3Entry, setEC3Entry = vpx, ac3, ec3 }()
	refuse := errors.New("staged refusal")
	setVPxEntry = func(*mp4.TrakBox, string, *mp4.VppCBox, uint16, uint16) error { return refuse }
	setAC3Entry = func(*mp4.TrakBox, *mp4.Dac3Box) error { return refuse }
	setEC3Entry = func(*mp4.TrakBox, *mp4.Dec3Box) error { return refuse }

	dac3 := dac3Payload(t, &mp4.Dac3Box{FSCod: 0, BSID: 8, ACMod: 2, BitRateCode: 10})
	dec3 := dac3Payload(t, &mp4.Dec3Box{DataRate: 192, NumIndSub: 1,
		EC3Subs: []mp4.EC3Sub{{FSCod: 0, BSID: 16, ACMod: 2}}})
	cases := []TrackConfig{
		{Codec: "vp09", Timescale: 90000, Width: 640, Height: 480,
			VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8}},
		{Codec: "ac-3", Timescale: 48000, CodecConfig: dac3},
		{Codec: "ec-3", Timescale: 48000, CodecConfig: dec3},
	}
	for _, cfg := range cases {
		t.Run(cfg.Codec, func(t *testing.T) {
			var buf bytes.Buffer
			_, err := NewMuxer(&buf).AddTrack(cfg)
			if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), refuse.Error()) {
				t.Fatalf("err = %v, want the staged refusal reported", err)
			}
		})
	}
}
