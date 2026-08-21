// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return data
}

func TestReaderReadsTheFixtureSamples(t *testing.T) {
	r, err := NewReader(fixture(t, "tiny.mp4"))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r.File().Format != "mp4" {
		t.Fatalf("Format = %q", r.File().Format)
	}
	ids := r.TrackIDs()
	if len(ids) == 0 {
		t.Fatal("no track")
	}
	cfg, err := r.TrackConfig(ids[0])
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if cfg.Codec != "avc1" || len(cfg.SPS) == 0 || len(cfg.PPS) == 0 {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Timescale == 0 || cfg.Width == 0 || cfg.Height == 0 {
		t.Fatalf("config = %+v", cfg)
	}
	samples, err := r.Samples(ids[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no sample")
	}
	for i, s := range samples {
		if len(s.Data) == 0 || s.Duration == 0 {
			t.Fatalf("sample %d = %+v", i, s)
		}
	}
	if !samples[0].Sync {
		t.Error("the first sample of a video track must be a sync sample")
	}
}

// TestSamplesSurviveMuxAndRead is the symmetry this package promises: what the
// muxer writes, the reader reads back unchanged.
func TestSamplesSurviveMuxAndRead(t *testing.T) {
	sps, pps, w, h := avcParameterSets(t)
	written := []Sample{
		{Data: []byte{0, 0, 0, 2, 9, 16}, Duration: 512, Sync: true},
		{Data: []byte{0, 0, 0, 3, 9, 17, 18}, Duration: 512, CompositionOffset: 256},
		{Data: []byte{0, 0, 0, 1, 9}, Duration: 1024},
	}
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	video, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "avc1", Timescale: 12800,
		Width: w, Height: h, SPS: sps, PPS: pps,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range written {
		if err := m.WriteSample(video, s); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := r.Samples(video)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(got) != len(written) {
		t.Fatalf("read %d samples, wrote %d", len(got), len(written))
	}
	for i := range written {
		if !bytes.Equal(got[i].Data, written[i].Data) {
			t.Errorf("sample %d data = %x, wrote %x", i, got[i].Data, written[i].Data)
		}
		if got[i].Duration != written[i].Duration {
			t.Errorf("sample %d duration = %d, wrote %d", i, got[i].Duration, written[i].Duration)
		}
		if got[i].Sync != written[i].Sync {
			t.Errorf("sample %d sync = %v, wrote %v", i, got[i].Sync, written[i].Sync)
		}
		if got[i].CompositionOffset != written[i].CompositionOffset {
			t.Errorf("sample %d offset = %d, wrote %d",
				i, got[i].CompositionOffset, written[i].CompositionOffset)
		}
	}
	cfg, err := r.TrackConfig(video)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if len(cfg.SPS) != len(sps) || !bytes.Equal(cfg.SPS[0], sps[0]) {
		t.Errorf("the parameter sets did not survive: %+v", cfg.SPS)
	}
}

// TestTrackCopiedWithoutReadingABox is what a caller joining two streams does:
// read a track's configuration and samples, write them into another file.
func TestTrackCopiedWithoutReadingABox(t *testing.T) {
	src, err := NewReader(fixture(t, "tiny.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	id := src.TrackIDs()[0]
	cfg, err := src.TrackConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := src.Samples(id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	copyID, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack from a read configuration: %v", err)
	}
	for i, s := range samples {
		if err := m.WriteSample(copyID, s); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatalf("the copy does not read back: %v", err)
	}
	copied, err := back.Samples(copyID)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != len(samples) {
		t.Fatalf("copied %d samples of %d", len(copied), len(samples))
	}
	for i := range samples {
		if !bytes.Equal(copied[i].Data, samples[i].Data) {
			t.Fatalf("sample %d changed in the copy", i)
		}
	}
}

func TestTrackConfigOfAnAV1Track(t *testing.T) {
	av1C := []byte{0x81, 0x00, 0x0c, 0x00}
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	id, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "av01", Timescale: 30000, Width: 640, Height: 360,
		CodecConfig: av1C,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1, 2}, Duration: 1001, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(id)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if !bytes.Equal(cfg.CodecConfig, av1C) {
		t.Fatalf("av1C = %x, wrote %x", cfg.CodecConfig, av1C)
	}
}

func TestTrackConfigOfAnAACTrack(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	id, err := m.AddTrack(audioConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{0xff, 0xf1}, Duration: 1024, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(id)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if cfg.Codec != "mp4a" || cfg.SampleRate != 48000 || cfg.AudioObjectType == 0 {
		t.Fatalf("config = %+v", cfg)
	}
}

// hevcFile builds a minimal file whose sample entry carries a structurally
// valid hvcC, which is what the reader has to pull parameter sets out of.
func hevcFile(t *testing.T) []byte {
	t.Helper()
	vps, sps, pps := []byte{0x40, 0x01}, []byte{0x42, 0x01}, []byte{0x44, 0x01}
	hvcC := &mp4.HvcCBox{DecConfRec: hevc.DecConfRec{
		ConfigurationVersion: 1,
		LengthSizeMinusOne:   3, // four-byte NAL unit lengths
		NaluArrays: []hevc.NaluArray{
			hevc.NewNaluArray(true, hevc.NALU_VPS, [][]byte{vps}),
			hevc.NewNaluArray(true, hevc.NALU_SPS, [][]byte{sps}),
			hevc.NewNaluArray(true, hevc.NALU_PPS, [][]byte{pps}),
		},
	}}
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(90000, "video", "und")
	trak.Mdia.Minf.Stbl.Stsd.AddChild(mp4.CreateVisualSampleEntryBox("hvc1", 32, 24, hvcC))
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func TestTrackConfigOfAnHEVCTrack(t *testing.T) {
	r, err := NewReader(hevcFile(t))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if len(cfg.VPS) != 1 || len(cfg.SPS) != 1 || len(cfg.PPS) != 1 {
		t.Fatalf("the HEVC parameter sets did not survive: %+v", cfg)
	}
	if cfg.VPS[0][0] != 0x40 || cfg.SPS[0][0] != 0x42 || cfg.PPS[0][0] != 0x44 {
		t.Fatalf("the parameter sets were mixed up: %+v", cfg)
	}
	// A track that names no sample is reported as such, not as empty data.
	if _, err := r.Samples(r.TrackIDs()[0]); !errors.Is(err, ErrNoSamples) {
		t.Errorf("Samples of a track without any: %v", err)
	}
}

func TestNewReaderRefusesWhatItCannotRead(t *testing.T) {
	if _, err := NewReader(fixture(t, "tiny.mkv")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("matroska: %v, want ErrUnsupportedFormat", err)
	}
	if _, err := NewReader([]byte("not a container at all")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("rubbish: %v", err)
	}
	// An ftyp box and nothing else: sniffed as MP4, refused as a file for
	// naming no movie.
	header := append([]byte{0, 0, 0, 12}, []byte("ftypisom")...)
	if _, err := NewReader(header); err == nil {
		t.Error("a file with nothing but an ftyp box was accepted")
	}
	// A box claiming more bytes than the file holds cannot be parsed at all,
	// which is a different failure from a file that parses and says nothing.
	overlong := append(header, 0xff, 0xff, 0xff, 0xff)
	overlong = append(overlong, []byte("moov")...)
	if _, err := NewReader(overlong); err == nil {
		t.Error("a box longer than its file was accepted")
	}
}

func TestReaderRejectsAnUnknownTrack(t *testing.T) {
	r, err := NewReader(fixture(t, "tiny.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(999); !errors.Is(err, ErrUnknownTrack) {
		t.Errorf("Samples: %v", err)
	}
	if _, err := r.TrackConfig(999); !errors.Is(err, ErrUnknownTrack) {
		t.Errorf("TrackConfig: %v", err)
	}
}

func TestBoxPayloadOfAnEmptyBox(t *testing.T) {
	if _, err := boxPayload(&emptyBox{}); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("err = %v, want ErrNoSamples", err)
	}
	if _, err := boxPayload(&failingBox{}); err == nil {
		t.Fatal("a box that cannot be encoded was accepted")
	}
}

func TestDescribeFormat(t *testing.T) {
	cases := map[Format]string{FormatMP4: "mp4", FormatMatroska: "matroska", FormatUnknown: "unknown format"}
	for format, want := range cases {
		if got := describeFormat(format); got != want {
			t.Errorf("describeFormat(%d) = %q, want %q", format, got, want)
		}
	}
}

// emptyBox and failingBox stand in for a configuration record that carries
// nothing, and for one that cannot be written at all.
type emptyBox struct{}

func (emptyBox) Type() string                                 { return "av1C" }
func (emptyBox) Size() uint64                                 { return 8 }
func (emptyBox) Encode(w io.Writer) error                     { _, err := w.Write(make([]byte, 8)); return err }
func (emptyBox) EncodeSW(bits.SliceWriter) error              { return nil }
func (emptyBox) Info(io.Writer, string, string, string) error { return nil }

type failingBox struct{ emptyBox }

func (failingBox) Encode(io.Writer) error { return errors.New("cannot encode") }

func TestMovieBoxLooksInBothPlaces(t *testing.T) {
	if got := movieBox(&mp4.File{}); got != nil {
		t.Error("a file without a movie box yielded one")
	}
	init := mp4.CreateEmptyInit()
	if got := movieBox(&mp4.File{Init: init}); got != init.Moov {
		t.Error("the movie box of a fragmented file lives in its init segment")
	}
}

func TestTrackExtendsIsFoundOrAbsent(t *testing.T) {
	// A progressive file declares no movie extends box at all.
	progressive, err := NewReader(fixture(t, "tiny.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if got := progressive.trackExtends(progressive.TrackIDs()[0]); got != nil {
		t.Errorf("a progressive file yielded a track extends box: %+v", got)
	}
	// A fragmented one declares it for each of its tracks.
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 10, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	fragmented, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := fragmented.trackExtends(id); got == nil || got.TrackID != id {
		t.Errorf("the track's own box was not found: %+v", got)
	}
	if got := fragmented.trackExtends(id + 7); got != nil {
		t.Errorf("another track yielded a box: %+v", got)
	}
}

func TestFragmentedSamplesOfATrackWithoutAny(t *testing.T) {
	// Two tracks, samples written for one of them only: the other reports no
	// sample rather than the first track's.
	sps, pps, w, h := avcParameterSets(t)
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	video, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "avc1", Timescale: 12800,
		Width: w, Height: h, SPS: sps, PPS: pps})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := m.AddTrack(audioConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(video, Sample{Data: []byte{1, 2}, Duration: 512, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.Samples(video); err != nil || len(got) != 1 {
		t.Fatalf("video samples = %d, %v", len(got), err)
	}
	if _, err := r.Samples(audio); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("audio samples: %v, want ErrNoSamples", err)
	}
}

func TestAudioObjectTypeOfAnUnreadableConfig(t *testing.T) {
	if got := audioObjectType(&mp4.EsdsBox{}); got != 0 {
		t.Errorf("without a descriptor = %d", got)
	}
	esds := &mp4.EsdsBox{}
	esds.DecConfigDescriptor = &mp4.DecoderConfigDescriptor{
		DecSpecificInfo: &mp4.DecSpecificInfoDescriptor{DecConfig: []byte{0xff}},
	}
	if got := audioObjectType(esds); got != 0 {
		t.Errorf("with a config that cannot be read = %d", got)
	}
}

func TestFragmentedSamplesOfAMultiTrackFragment(t *testing.T) {
	// One fragment carrying both tracks is what a joined file looks like, and
	// what a player reads: every track of it must be readable.
	sps, pps, w, h := avcParameterSets(t)
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	video, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "avc1", Timescale: 12800,
		Width: w, Height: h, SPS: sps, PPS: pps})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := m.AddTrack(audioConfig())
	if err != nil {
		t.Fatal(err)
	}
	videoData := [][]byte{{0, 0, 0, 2, 9, 16}, {0, 0, 0, 1, 9}}
	audioData := [][]byte{{0xde, 0xad}, {0xbe, 0xef, 0x00}}
	for i := range videoData {
		if err := m.WriteSample(video, Sample{Data: videoData[i], Duration: 512, Sync: true}); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(audio, Sample{Data: audioData[i], Duration: 1024, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   uint32
		want [][]byte
	}{{video, videoData}, {audio, audioData}} {
		samples, err := r.Samples(c.id)
		if err != nil {
			t.Fatalf("track %d: %v", c.id, err)
		}
		if len(samples) != len(c.want) {
			t.Fatalf("track %d read %d samples, wrote %d", c.id, len(samples), len(c.want))
		}
		for i := range c.want {
			if !bytes.Equal(samples[i].Data, c.want[i]) {
				t.Errorf("track %d sample %d = %x, wrote %x",
					c.id, i, samples[i].Data, c.want[i])
			}
		}
	}
}

func TestTrafSamplesRejectsWhatItCannotRead(t *testing.T) {
	r := &Reader{data: make([]byte, 4)}
	// A track fragment with no sample run says nothing about its samples.
	noRun := &mp4.TrafBox{Tfhd: &mp4.TfhdBox{TrackID: 1}}
	if _, err := r.trafSamples(0, noRun, nil); !errors.Is(err, ErrNoSamples) {
		t.Errorf("without a run: %v", err)
	}
	// A run naming more data than the file holds must not be read. The size
	// flag makes the run's own sizes the ones that count.
	trun := &mp4.TrunBox{Flags: 0x000200, Samples: []mp4.Sample{{Size: 99, Dur: 10}}}
	past := &mp4.TrafBox{Tfhd: &mp4.TfhdBox{TrackID: 1}, Trun: trun}
	if _, err := r.trafSamples(0, past, nil); !errors.Is(err, ErrSampleData) {
		t.Errorf("beyond the end: %v", err)
	}
}

// sampleTable builds a track whose tables the reader can be pointed at
// directly, which is how the branches a real file never exercises get tested.
func sampleTable(t *testing.T, sizes []uint32, offsets []uint32, samplesPerChunk uint32) *mp4.TrakBox {
	t.Helper()
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(1000, "video", "und")
	stbl := trak.Mdia.Minf.Stbl
	stbl.Stsz = &mp4.StszBox{SampleNumber: uint32(len(sizes)), SampleSize: sizes}
	stbl.Stts = &mp4.SttsBox{SampleCount: []uint32{uint32(len(sizes))}, SampleTimeDelta: []uint32{100}}
	stbl.Stsc = &mp4.StscBox{}
	if err := stbl.Stsc.AddEntry(1, samplesPerChunk, 1); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	stbl.Stco = &mp4.StcoBox{ChunkOffset: offsets}
	return trak
}

func TestProgressiveSamplesReadsATableDirectly(t *testing.T) {
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i)
	}
	r := &Reader{data: data}
	trak := sampleTable(t, []uint32{4, 4}, []uint32{0, 8}, 1)
	samples, err := r.progressiveSamples(trak)
	if err != nil {
		t.Fatalf("progressiveSamples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %+v", samples)
	}
	if !bytes.Equal(samples[0].Data, data[0:4]) || !bytes.Equal(samples[1].Data, data[8:12]) {
		t.Fatalf("the chunk offsets were not followed: %x / %x", samples[0].Data, samples[1].Data)
	}
	if samples[0].Duration != 100 {
		t.Errorf("duration = %d", samples[0].Duration)
	}
	// Without an stss box every sample is a sync sample.
	if !samples[0].Sync || !samples[1].Sync {
		t.Error("a track without an stss box is all sync samples")
	}
}

func TestProgressiveSamplesRejectsUnusableTables(t *testing.T) {
	r := &Reader{data: make([]byte, 16)}

	bare := mp4.CreateEmptyInit().AddEmptyTrack(1000, "video", "und")
	bare.Mdia.Minf.Stbl.Stsz = nil
	if _, err := r.progressiveSamples(bare); !errors.Is(err, ErrNoSamples) {
		t.Errorf("without a sample size table: %v", err)
	}

	empty := sampleTable(t, nil, []uint32{0}, 1)
	if _, err := r.progressiveSamples(empty); !errors.Is(err, ErrNoSamples) {
		t.Errorf("without any sample: %v", err)
	}

	// A chunk offset past the end of the file must be reported, not read.
	past := sampleTable(t, []uint32{8}, []uint32{100}, 1)
	if _, err := r.progressiveSamples(past); !errors.Is(err, ErrSampleData) {
		t.Errorf("beyond the end: %v", err)
	}

	// A chunk table naming a chunk the offset table does not have.
	missing := sampleTable(t, []uint32{4, 4}, []uint32{0}, 1)
	if _, err := r.progressiveSamples(missing); err == nil {
		t.Error("a missing chunk offset was accepted")
	}

	noOffsets := sampleTable(t, []uint32{4}, []uint32{0}, 1)
	noOffsets.Mdia.Minf.Stbl.Stco = nil
	if _, err := r.progressiveSamples(noOffsets); !errors.Is(err, ErrNoSamples) {
		t.Errorf("without any offset table: %v", err)
	}
}

func TestProgressiveSamplesHonoursCttsAndStss(t *testing.T) {
	r := &Reader{data: make([]byte, 32)}
	trak := sampleTable(t, []uint32{4, 4}, []uint32{0, 8}, 1)
	stbl := trak.Mdia.Minf.Stbl
	stbl.Ctts = &mp4.CttsBox{}
	if err := stbl.Ctts.AddSampleCountsAndOffset([]uint32{2}, []int32{50}); err != nil {
		t.Fatalf("ctts: %v", err)
	}
	stbl.Stss = &mp4.StssBox{SampleNumber: []uint32{1}}
	samples, err := r.progressiveSamples(trak)
	if err != nil {
		t.Fatalf("progressiveSamples: %v", err)
	}
	if samples[0].CompositionOffset != 50 {
		t.Errorf("composition offset = %d", samples[0].CompositionOffset)
	}
	if !samples[0].Sync || samples[1].Sync {
		t.Errorf("the stss box was not honoured: %v / %v", samples[0].Sync, samples[1].Sync)
	}
}

func TestChunkOffsetTables(t *testing.T) {
	stco := &mp4.StblBox{Stco: &mp4.StcoBox{ChunkOffset: []uint32{42}}}
	if got, err := chunkOffset(stco, 1); err != nil || got != 42 {
		t.Errorf("stco = %d, %v", got, err)
	}
	if _, err := chunkOffset(stco, 9); err == nil {
		t.Error("a chunk beyond the table was accepted")
	}
	co64 := &mp4.StblBox{Co64: &mp4.Co64Box{ChunkOffset: []uint64{1 << 33}}}
	if got, err := chunkOffset(co64, 1); err != nil || got != 1<<33 {
		t.Errorf("co64 = %d, %v", got, err)
	}
	if _, err := chunkOffset(co64, 9); err == nil {
		t.Error("a chunk beyond the 64-bit table was accepted")
	}
	if _, err := chunkOffset(&mp4.StblBox{}, 1); !errors.Is(err, ErrNoSamples) {
		t.Error("a table-less stbl must be reported")
	}
}

func TestProgressiveSamplesStopsAtTheTableEnd(t *testing.T) {
	// A chunk claiming three samples where the table has one: the reader
	// stops at what the table describes.
	r := &Reader{data: make([]byte, 32)}
	trak := sampleTable(t, []uint32{4}, []uint32{0}, 3)
	samples, err := r.progressiveSamples(trak)
	if err != nil {
		t.Fatalf("progressiveSamples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want the one the table describes", len(samples))
	}
}

func TestProgressiveSamplesReportsAChunkTableItCannotWalk(t *testing.T) {
	original := containingChunks
	defer func() { containingChunks = original }()
	containingChunks = func(*mp4.StscBox, uint32, uint32) ([]mp4.Chunk, error) {
		return nil, errors.New("unwalkable table")
	}
	r := &Reader{data: make([]byte, 32)}
	if _, err := r.progressiveSamples(sampleTable(t, []uint32{4}, []uint32{0}, 1)); err == nil {
		t.Fatal("a chunk table that cannot be walked must be reported")
	}
}

func TestProgressiveSamplesReportsAnUnusableChunkTable(t *testing.T) {
	r := &Reader{data: make([]byte, 32)}
	trak := sampleTable(t, []uint32{4}, []uint32{0}, 1)
	// A chunk table with no entry at all cannot say where sample 1 lives —
	// and asking the box reader anyway panics, so this must be caught here.
	trak.Mdia.Minf.Stbl.Stsc = &mp4.StscBox{}
	if _, err := r.progressiveSamples(trak); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("err = %v, want ErrNoSamples", err)
	}
}

func TestFragmentedSamplesSkipsMalformedFragments(t *testing.T) {
	// A file whose fragments say nothing usable: no header at all, and a
	// track fragment without one. Neither may be read, and neither panics.
	r := &Reader{
		data: make([]byte, 8),
		mp4: &mp4.File{Segments: []*mp4.MediaSegment{{Fragments: []*mp4.Fragment{
			{},
			{Moof: &mp4.MoofBox{Trafs: []*mp4.TrafBox{{}}}},
		}}}},
	}
	if _, err := r.fragmentedSamples(1); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("err = %v, want ErrNoSamples", err)
	}
}
