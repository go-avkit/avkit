// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// --- fixtures ---------------------------------------------------------------

// marshalDoc writes a Matroska document out of the very structs the reader
// reads back, so a fixture cannot drift from the tree it is meant to stand for.
// The byte layout is ebml-go's own, which is the point: the reader is tested
// against the reference library's output, not against a hand-made guess.
func marshalDoc(t *testing.T, doc any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := ebml.Marshal(doc, &buf); err != nil {
		t.Fatalf("marshal matroska: %v", err)
	}
	data := buf.Bytes()
	if Sniff(data) != FormatMatroska {
		t.Fatalf("precondition: the fixture does not sniff as Matroska")
	}
	return data
}

// avcCPayload is the avcC record of testdata/tiny.mkv: a real AVC
// configuration, with one SPS and one PPS and four-byte NAL unit lengths.
const avcCPayload = "0164000affe100196764000aacd9497e5c044000000300400000030283c48965" +
	"8001000668ebe3cb22c0fdf8f800"

func avcC(t *testing.T) []byte {
	t.Helper()
	payload, err := hex.DecodeString(avcCPayload)
	if err != nil {
		t.Fatalf("decode avcC: %v", err)
	}
	return payload
}

// hvcCPayload is a structurally valid hvcC record carrying one parameter set of
// each kind, built by the reference library rather than written out by hand.
func hvcCPayload(t *testing.T) []byte {
	t.Helper()
	box := &mp4.HvcCBox{DecConfRec: hevc.DecConfRec{
		ConfigurationVersion: 1,
		LengthSizeMinusOne:   3,
		NaluArrays: []hevc.NaluArray{
			hevc.NewNaluArray(true, hevc.NALU_VPS, [][]byte{{0x40, 0x01}}),
			hevc.NewNaluArray(true, hevc.NALU_SPS, [][]byte{{0x42, 0x01}}),
			hevc.NewNaluArray(true, hevc.NALU_PPS, [][]byte{{0x44, 0x01}}),
		},
	}}
	payload, err := boxPayload(box)
	if err != nil {
		t.Fatalf("encode hvcC: %v", err)
	}
	return payload
}

// av1CPayload is the shortest av1C record a muxer accepts: marker and version,
// then the sequence profile and level.
var av1CPayload = []byte{0x81, 0x00, 0x0c, 0x00}

// opusHeadFor is the Opus identification header a Matroska file carries as the
// track's private data.
func opusHeadFor(channels byte, preSkip uint16, rate uint32) []byte {
	head := make([]byte, opusHeadSize)
	copy(head, opusHeadMagic)
	head[8] = 1
	head[9] = channels
	binary.LittleEndian.PutUint16(head[10:12], preSkip)
	binary.LittleEndian.PutUint32(head[12:16], rate)
	return head
}

// simple is a SimpleBlock of one frame.
func simple(track uint64, timecode int16, sync bool, data []byte) ebml.Block {
	return ebml.Block{TrackNumber: track, Timecode: timecode, Keyframe: sync, Data: [][]byte{data}}
}

// --- the real fixture -------------------------------------------------------

func TestReaderReadsTheMatroskaFixture(t *testing.T) {
	r, err := NewReader(fixture(t, "tiny.mkv"))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	file := r.File()
	if file.Format != "matroska" || file.Timescale != 1000 {
		t.Fatalf("file = %q at %d ticks/s, want matroska at 1000", file.Format, file.Timescale)
	}
	ids := r.TrackIDs()
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("tracks = %v, want [1]", ids)
	}
	cfg, err := r.TrackConfig(1)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	// The CodecID names AVC; the configuration states the sample entry a
	// caller writes, and the parameter sets come out of the avcC record the
	// file carries as its private data.
	if cfg.Codec != "avc1" || cfg.Kind != Video {
		t.Fatalf("codec = %q kind = %v, want avc1 video", cfg.Codec, cfg.Kind)
	}
	if cfg.Width != 32 || cfg.Height != 24 || cfg.Timescale != 1000 {
		t.Fatalf("config = %dx%d at %d", cfg.Width, cfg.Height, cfg.Timescale)
	}
	if len(cfg.SPS) != 1 || len(cfg.PPS) != 1 {
		t.Fatalf("parameter sets = %d SPS, %d PPS, want one of each", len(cfg.SPS), len(cfg.PPS))
	}
	if cfg.SPS[0][0] != 0x67 || cfg.PPS[0][0] != 0x68 {
		t.Fatalf("parameter sets were mixed up: SPS % x PPS % x", cfg.SPS[0][:1], cfg.PPS[0][:1])
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(samples))
	}
	s := samples[0]
	// The one and only block is a keyframe, and its duration can come from
	// nowhere but the track's DefaultDuration of 200 ms.
	if !s.Sync || s.Duration != 200 || s.CompositionOffset != 0 {
		t.Fatalf("sample = %+v, want a 200-tick sync sample shown when decoded",
			Sample{Duration: s.Duration, CompositionOffset: s.CompositionOffset, Sync: s.Sync})
	}
	if len(s.Data) != 1045 {
		t.Fatalf("sample data = %d bytes, want 1045", len(s.Data))
	}
	// AVC in Matroska is stored length-prefixed, which is the form a muxer
	// writes as it stands: the four-byte lengths must tile the sample exactly,
	// with nothing left over and nothing running past the end.
	units := 0
	for at := 0; at < len(s.Data); units++ {
		if at+4 > len(s.Data) {
			t.Fatalf("NAL unit %d has no length at offset %d of %d", units+1, at, len(s.Data))
		}
		at += 4 + int(binary.BigEndian.Uint32(s.Data[at:at+4]))
		if at > len(s.Data) {
			t.Fatalf("NAL unit %d runs to %d of %d bytes", units+1, at, len(s.Data))
		}
	}
	if units != 2 {
		t.Fatalf("sample holds %d NAL units, want 2", units)
	}
	if file.Tracks[0].Duration != 200 || file.Duration != 200 {
		t.Fatalf("durations: track %d, file %d, want 200 each",
			file.Tracks[0].Duration, file.Duration)
	}
	if _, err := r.Samples(9); !errors.Is(err, ErrUnknownTrack) {
		t.Errorf("Samples of a track the file has not: %v", err)
	}
	if _, err := r.TrackConfig(9); !errors.Is(err, ErrUnknownTrack) {
		t.Errorf("TrackConfig of a track the file has not: %v", err)
	}
}

// --- end to end -------------------------------------------------------------

// webmDoc is a two-track WebM: AV1 video at 25 frames a second and Opus audio
// in 20 ms packets, over two clusters.
func webmDoc() *mkvReadDoc {
	return &mkvReadDoc{
		Header: mkvHeader{DocType: "webm"},
		Segment: mkvReadSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale, Duration: 120},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{
				{
					TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_AV1",
					CodecPrivate:    av1CPayload,
					DefaultDuration: 40_000_000,
					Video:           &mkvVideo{PixelWidth: 640, PixelHeight: 360},
				},
				{
					TrackNumber: 2, TrackType: mkvTrackAudio, CodecID: "A_OPUS",
					CodecPrivate:    opusHeadFor(2, 312, 48000),
					DefaultDuration: 20_000_000,
					Audio:           &mkvAudio{Channels: 2, SamplingFrequency: 48000},
				},
			}},
			Cluster: []mkvCluster{
				{
					Timecode: 0,
					SimpleBlock: []ebml.Block{
						simple(1, 0, true, []byte("v0")),
						simple(2, 0, true, []byte("a0")),
						simple(2, 20, true, []byte("a1")),
						simple(1, 40, false, []byte("v1")),
						simple(2, 40, true, []byte("a2")),
					},
				},
				{
					Timecode: 60,
					SimpleBlock: []ebml.Block{
						simple(1, 20, false, []byte("v2")),
						simple(2, 0, true, []byte("a3")),
						simple(2, 20, true, []byte("a4")),
						simple(2, 40, true, []byte("a5")),
					},
				},
			},
		},
	}
}

func TestReaderRemuxesAWebMIntoFragmentedMP4(t *testing.T) {
	r, err := NewReader(marshalDoc(t, webmDoc()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r.File().Format != "webm" {
		t.Fatalf("format = %q, want webm", r.File().Format)
	}
	if ids := r.TrackIDs(); len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("tracks = %v, want [1 2]", ids)
	}
	// The video runs at 40 ms a frame across a cluster boundary; the last
	// frame's duration can only come from DefaultDuration.
	video, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples(video): %v", err)
	}
	if len(video) != 3 {
		t.Fatalf("video samples = %d, want 3", len(video))
	}
	for i, s := range video {
		if s.Duration != 40 {
			t.Fatalf("video sample %d lasts %d ticks, want 40", i+1, s.Duration)
		}
		if want := i == 0; s.Sync != want {
			t.Fatalf("video sample %d sync = %v, want %v", i+1, s.Sync, want)
		}
	}
	if got := string(video[2].Data); got != "v2" {
		t.Fatalf("video sample 3 = %q, want v2 (the cluster's timecode was not added)", got)
	}
	audio, err := r.Samples(2)
	if err != nil {
		t.Fatalf("Samples(audio): %v", err)
	}
	if len(audio) != 6 {
		t.Fatalf("audio samples = %d, want 6", len(audio))
	}
	for i, s := range audio {
		if s.Duration != 20 || !s.Sync {
			t.Fatalf("audio sample %d = %d ticks, sync %v, want 20 and true", i+1, s.Duration, s.Sync)
		}
	}
	// The Opus identification header travels as it stands, and the pre-skip
	// it states is lifted out of it.
	audioCfg, err := r.TrackConfig(2)
	if err != nil {
		t.Fatalf("TrackConfig(audio): %v", err)
	}
	if audioCfg.Codec != "Opus" || audioCfg.PreSkip != 312 || audioCfg.SampleRate != 48000 {
		t.Fatalf("audio config = %q pre-skip %d rate %d", audioCfg.Codec, audioCfg.PreSkip, audioCfg.SampleRate)
	}
	if !bytes.Equal(audioCfg.CodecConfig, opusHeadFor(2, 312, 48000)) {
		t.Fatalf("identification header = % x", audioCfg.CodecConfig)
	}

	// Now write the whole thing out as a fragmented MP4 with the muxer as it
	// stands, and read the result back: what came out of the WebM has to
	// survive the round trip sample for sample.
	var out bytes.Buffer
	m := NewMuxer(&out)
	ids := make([]uint32, 0, 2)
	for _, id := range r.TrackIDs() {
		cfg, err := r.TrackConfig(id)
		if err != nil {
			t.Fatalf("TrackConfig(%d): %v", id, err)
		}
		written, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatalf("AddTrack(%d): %v", id, err)
		}
		ids = append(ids, written)
	}
	for i, id := range r.TrackIDs() {
		samples, err := r.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		for _, s := range samples {
			if err := m.WriteSample(ids[i], s); err != nil {
				t.Fatalf("WriteSample(%d): %v", ids[i], err)
			}
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatalf("NewReader of the remux: %v", err)
	}
	for i, want := range []int{3, 6} {
		samples, err := back.Samples(ids[i])
		if err != nil {
			t.Fatalf("remuxed Samples(%d): %v", ids[i], err)
		}
		if len(samples) != want {
			t.Fatalf("remuxed track %d has %d samples, want %d", ids[i], len(samples), want)
		}
		if !samples[0].Sync {
			t.Fatalf("remuxed track %d does not start at a sync sample", ids[i])
		}
	}
	if got := back.File().Tracks[0].Codec; got != "av01" {
		t.Fatalf("remuxed video codec = %q, want av01", got)
	}
}

// --- block groups -----------------------------------------------------------

// referencedGroup writes a BlockGroup whose ReferenceBlock is written even when
// it is zero. ebml-go's own omitempty drops a stated zero on the way out, and
// the whole point of the field is that a reference of zero is a reference.
type referencedGroup struct {
	BlockDuration  uint64     `ebml:"BlockDuration,omitempty"`
	ReferenceBlock []int64    `ebml:"ReferenceBlock"`
	Block          ebml.Block `ebml:"Block"`
}

type referencedCluster struct {
	Timecode   uint64            `ebml:"Timecode"`
	BlockGroup []referencedGroup `ebml:"BlockGroup"`
}

type referencedSegment struct {
	Info    mkvInfo             `ebml:"Info"`
	Tracks  mkvTracks           `ebml:"Tracks"`
	Cluster []referencedCluster `ebml:"Cluster"`
}

type referencedDoc struct {
	Header  mkvHeader         `ebml:"EBML"`
	Segment referencedSegment `ebml:"Segment"`
}

func TestReaderReadsBlockGroups(t *testing.T) {
	doc := &referencedDoc{
		Header: mkvHeader{DocType: "matroska"},
		Segment: referencedSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{
				TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_AV1",
				CodecPrivate: av1CPayload,
				Video:        &mkvVideo{PixelWidth: 16, PixelHeight: 16},
			}}},
			Cluster: []referencedCluster{{
				Timecode: 0,
				BlockGroup: []referencedGroup{
					// No ReferenceBlock: a frame a player may start at.
					{Block: simple(1, 0, false, []byte("k")), BlockDuration: 33},
					// A reference of zero is still a reference, and a plain
					// integer field could not have told it from none.
					{Block: simple(1, 33, true, []byte("p")), ReferenceBlock: []int64{0}},
					{Block: simple(1, 66, true, []byte("q")),
						ReferenceBlock: []int64{-33}, BlockDuration: 33},
				},
			}},
		},
	}
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(samples))
	}
	// Sync comes from the group, not from the block's own keyframe flag: the
	// first group names no reference and the other two do.
	for i, want := range []bool{true, false, false} {
		if samples[i].Sync != want {
			t.Fatalf("sample %d sync = %v, want %v", i+1, samples[i].Sync, want)
		}
	}
	for i, want := range []uint32{33, 33, 33} {
		if samples[i].Duration != want {
			t.Fatalf("sample %d lasts %d ticks, want %d", i+1, samples[i].Duration, want)
		}
	}
	if string(samples[2].Data) != "q" {
		t.Fatalf("sample 3 = %q, want q", samples[2].Data)
	}
}

// mixedCluster writes a cluster's blocks in the order simple, group, simple.
// Two fields naming the same element is how that order is put in the bytes:
// ebml-go writes the fields in the order the struct declares them.
type mixedCluster struct {
	Timecode uint64          `ebml:"Timecode"`
	First    []ebml.Block    `ebml:"SimpleBlock"`
	Group    []mkvBlockGroup `ebml:"BlockGroup"`
	Second   []ebml.Block    `ebml:"SimpleBlock"`
}

type mixedSegment struct {
	Info    mkvInfo        `ebml:"Info"`
	Tracks  mkvTracks      `ebml:"Tracks"`
	Cluster []mixedCluster `ebml:"Cluster"`
}

type mixedDoc struct {
	Header  mkvHeader    `ebml:"EBML"`
	Segment mixedSegment `ebml:"Segment"`
}

func TestReaderKeepsTheFileOrderOfMixedBlockForms(t *testing.T) {
	doc := &mixedDoc{
		Header: mkvHeader{DocType: "matroska"},
		Segment: mixedSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{
				TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_AV1",
				CodecPrivate: av1CPayload, DefaultDuration: 10_000_000,
				Video: &mkvVideo{PixelWidth: 16, PixelHeight: 16},
			}}},
			Cluster: []mixedCluster{{
				Timecode: 0,
				First:    []ebml.Block{simple(1, 0, true, []byte("one"))},
				Group: []mkvBlockGroup{{
					Block:          simple(1, 10, false, []byte("two")),
					ReferenceBlock: []int64{-10},
				}},
				Second: []ebml.Block{simple(1, 20, false, []byte("three"))},
			}},
		},
	}
	data := marshalDoc(t, doc)
	// The bytes really do interleave the two forms: the block group's id sits
	// between the two simple blocks' ids.
	first := bytes.Index(data, []byte("one"))
	group := bytes.Index(data, []byte("two"))
	second := bytes.Index(data, []byte("three"))
	if !(first < group && group < second) {
		t.Fatalf("precondition: the fixture is not interleaved (%d, %d, %d)", first, group, second)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	var got []string
	for _, s := range samples {
		got = append(got, string(s.Data))
	}
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("samples = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("samples came back as %v, want %v: the two block forms were not merged in file order", got, want)
		}
	}
}

func TestNewMatroskaReaderSurfacesABlockOrderFailure(t *testing.T) {
	original := blockOrder
	defer func() { blockOrder = original }()
	blockOrder = func([]bool, []mkvCluster) ([]mkvBlockRef, error) {
		return nil, fmt.Errorf("%w: staged", ErrMatroska)
	}
	if _, err := NewReader(marshalDoc(t, webmDoc())); !errors.Is(err, ErrMatroska) {
		t.Fatalf("err = %v, want ErrMatroska", err)
	}
}

func TestBlockOrderReportsRecordsThatDisagree(t *testing.T) {
	cluster := func(simples, groups int) mkvCluster {
		c := mkvCluster{}
		for i := 0; i < simples; i++ {
			c.SimpleBlock = append(c.SimpleBlock, simple(1, int16(i), true, []byte{byte(i)}))
		}
		for i := 0; i < groups; i++ {
			c.BlockGroup = append(c.BlockGroup, mkvBlockGroup{Block: simple(1, int16(i), true, []byte{byte(i)})})
		}
		return c
	}
	cases := map[string]struct {
		groups   []bool
		clusters []mkvCluster
	}{
		"fewer records than blocks":    {[]bool{false}, []mkvCluster{cluster(2, 0)}},
		"more groups than the cluster": {[]bool{true, true}, []mkvCluster{cluster(1, 1)}},
		"more simple than the cluster": {[]bool{false, false}, []mkvCluster{cluster(1, 1)}},
		"records left over":            {[]bool{false, false}, []mkvCluster{cluster(1, 0)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := blockOrder(tc.groups, tc.clusters); !errors.Is(err, ErrMatroska) {
				t.Fatalf("err = %v, want ErrMatroska", err)
			}
		})
	}
	// And the well-formed case walks both records together.
	refs, err := blockOrder([]bool{false, true}, []mkvCluster{cluster(1, 1)})
	if err != nil {
		t.Fatalf("blockOrder: %v", err)
	}
	if len(refs) != 2 || refs[0].group != nil || refs[1].group == nil {
		t.Fatalf("refs = %+v, want a simple block then a group", refs)
	}
}

// --- lacing -----------------------------------------------------------------

func lacedDoc(mode ebml.LacingMode, frames [][]byte, defaultDuration, blockDuration uint64) *mkvReadDoc {
	block := ebml.Block{TrackNumber: 1, Timecode: 0, Keyframe: true, Lacing: mode, Data: frames}
	cluster := mkvCluster{Timecode: 0}
	if blockDuration != 0 {
		cluster.BlockGroup = []mkvBlockGroup{{Block: block, BlockDuration: blockDuration}}
	} else {
		cluster.SimpleBlock = []ebml.Block{block}
	}
	return &mkvReadDoc{
		Header: mkvHeader{DocType: "matroska"},
		Segment: mkvReadSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{
				TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_OPUS",
				CodecPrivate:    opusHeadFor(1, 0, 48000),
				DefaultDuration: defaultDuration,
				Audio:           &mkvAudio{Channels: 1, SamplingFrequency: 48000},
			}}},
			Cluster: []mkvCluster{cluster},
		},
	}
}

func TestReaderUnlacesEveryLacingMatroskaDefines(t *testing.T) {
	frames := [][]byte{[]byte("aaa"), []byte("bb"), []byte("cccc")}
	fixed := [][]byte{[]byte("aa"), []byte("bb"), []byte("cc")}
	cases := map[string]struct {
		mode   ebml.LacingMode
		frames [][]byte
	}{
		"xiph":  {ebml.LacingXiph, frames},
		"ebml":  {ebml.LacingEBML, frames},
		"fixed": {ebml.LacingFixed, fixed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A laced block really is laced: it carries a lace header the
			// same frames written unlaced would not have.
			laced := marshalDoc(t, lacedDoc(tc.mode, tc.frames, 20_000_000, 0))
			plain := marshalDoc(t, lacedDoc(ebml.LacingNo, [][]byte{bytes.Join(tc.frames, nil)}, 20_000_000, 0))
			if len(laced) <= len(plain) {
				t.Fatalf("precondition: %s lacing added no lace header (%d bytes against %d)",
					name, len(laced), len(plain))
			}
			r, err := NewReader(laced)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			samples, err := r.Samples(1)
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			if len(samples) != len(tc.frames) {
				t.Fatalf("samples = %d, want %d: the block was not unlaced", len(samples), len(tc.frames))
			}
			for i, want := range tc.frames {
				if !bytes.Equal(samples[i].Data, want) {
					t.Fatalf("sample %d = %q, want %q", i+1, samples[i].Data, want)
				}
				// Every laced frame lasts the track's default duration, and
				// they are all as startable as the block that held them.
				if samples[i].Duration != 20 || !samples[i].Sync {
					t.Fatalf("sample %d = %d ticks, sync %v, want 20 and true",
						i+1, samples[i].Duration, samples[i].Sync)
				}
			}
		})
	}
}

func TestReaderSplitsALacedBlockDurationExactly(t *testing.T) {
	// Ten ticks over three frames: the shares must still add up to ten, which
	// rounding each to the same value could not do.
	frames := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	r, err := NewReader(marshalDoc(t, lacedDoc(ebml.LacingEBML, frames, 0, 10)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	got := []uint32{}
	total := uint32(0)
	for _, s := range samples {
		got = append(got, s.Duration)
		total += s.Duration
	}
	if total != 10 {
		t.Fatalf("durations %v add up to %d, want 10", got, total)
	}
	if len(got) != 3 || got[0] != 3 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("durations = %v, want [3 3 4]", got)
	}
}

func TestReaderRefusesALacedBlockNothingTimes(t *testing.T) {
	frames := [][]byte{[]byte("aaa"), []byte("bb")}
	r, err := NewReader(marshalDoc(t, lacedDoc(ebml.LacingXiph, frames, 0, 0)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	_, err = r.Samples(1)
	if !errors.Is(err, ErrMatroska) {
		t.Fatalf("err = %v, want ErrMatroska", err)
	}
	// The track is still listed, and the reason travels with it.
	if len(r.File().Tracks) != 1 {
		t.Fatalf("tracks = %+v, want the track to stay listed", r.File().Tracks)
	}
}

// --- timing -----------------------------------------------------------------

// timedDoc is a one-track document whose blocks are given by timecode, so a
// test can state timings and nothing else.
func timedDoc(segDuration float64, defaultDuration uint64, blocks []mkvBlockGroup,
	clusterTimes ...uint64) *mkvReadDoc {
	doc := &mkvReadDoc{
		Header: mkvHeader{DocType: "matroska"},
		Segment: mkvReadSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale, Duration: segDuration},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{
				TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_AV1",
				CodecPrivate: av1CPayload, DefaultDuration: defaultDuration,
				Video: &mkvVideo{PixelWidth: 16, PixelHeight: 16},
			}}},
		},
	}
	for i, tc := range clusterTimes {
		cluster := mkvCluster{Timecode: tc}
		if blocks[i].BlockDuration == 0 && len(blocks[i].ReferenceBlock) == 0 {
			cluster.SimpleBlock = []ebml.Block{blocks[i].Block}
		} else {
			cluster.BlockGroup = []mkvBlockGroup{blocks[i]}
		}
		doc.Segment.Cluster = append(doc.Segment.Cluster, cluster)
	}
	return doc
}

func TestReaderStatesDecodeTimesForAReorderedStream(t *testing.T) {
	// A closed group of pictures stored in decode order: I, P, B, B. The
	// timestamps a Matroska block states are the times the frames are shown.
	doc := &mkvReadDoc{
		Header: mkvHeader{DocType: "matroska"},
		Segment: mkvReadSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{
				TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_MPEG4/ISO/AVC",
				CodecPrivate: avcC(t), DefaultDuration: 10_000_000,
				Video: &mkvVideo{PixelWidth: 32, PixelHeight: 24},
			}}},
			Cluster: []mkvCluster{{
				Timecode: 0,
				SimpleBlock: []ebml.Block{
					simple(1, 0, true, []byte("I")),
					simple(1, 30, false, []byte("P")),
					simple(1, 10, false, []byte("B")),
					simple(1, 20, false, []byte("b")),
				},
			}},
		},
	}
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 4 {
		t.Fatalf("samples = %d, want 4", len(samples))
	}
	// The frames stay in decode order, every one lasts ten ticks, and the
	// offsets put each back where it is shown: the decode times run
	// 0, 10, 20, 30 and the shown times, once the whole track is held back by
	// the ten ticks of reordering, run 10, 40, 20, 30.
	order := []string{"I", "P", "B", "b"}
	offsets := []int32{10, 30, 0, 0}
	for i, s := range samples {
		if string(s.Data) != order[i] {
			t.Fatalf("sample %d = %q, want %q: decode order was not kept", i+1, s.Data, order[i])
		}
		if s.Duration != 10 {
			t.Fatalf("sample %d lasts %d ticks, want 10", i+1, s.Duration)
		}
		if s.CompositionOffset != offsets[i] {
			t.Fatalf("sample %d is shown %d ticks after it is decoded, want %d",
				i+1, s.CompositionOffset, offsets[i])
		}
	}
	// Every frame is shown after it is decoded, which is what the delay is
	// there for, and the shown times are the ones the file stated, shifted.
	for i, s := range samples {
		if s.CompositionOffset < 0 {
			t.Fatalf("sample %d would be shown before it is decoded", i+1)
		}
	}
}

func TestReaderTakesTheLastDurationFromWhatTheFileStates(t *testing.T) {
	block := func(timecode int16, duration uint64) mkvBlockGroup {
		return mkvBlockGroup{Block: simple(1, timecode, true, []byte{byte(timecode)}),
			BlockDuration: duration}
	}
	cases := map[string]struct {
		doc  *mkvReadDoc
		want uint32
	}{
		"the block states it": {
			timedDoc(0, 0, []mkvBlockGroup{block(0, 7)}, 0), 7,
		},
		"the track states it": {
			timedDoc(0, 9_000_000, []mkvBlockGroup{block(0, 0)}, 0), 9,
		},
		"the segment states what is left": {
			timedDoc(11, 0, []mkvBlockGroup{block(0, 0)}, 0), 11,
		},
		"the sample before it is as good as it gets": {
			timedDoc(0, 0, []mkvBlockGroup{block(0, 0), block(0, 0)}, 0, 5), 5,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := NewReader(marshalDoc(t, tc.doc))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			samples, err := r.Samples(1)
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			last := samples[len(samples)-1]
			if last.Duration != tc.want {
				t.Fatalf("last sample lasts %d ticks, want %d", last.Duration, tc.want)
			}
		})
	}
}

func TestReaderRefusesALastSampleNothingMeasures(t *testing.T) {
	doc := timedDoc(0, 0, []mkvBlockGroup{{Block: simple(1, 0, true, []byte("k"))}}, 0)
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrMatroska) {
		t.Fatalf("err = %v, want ErrMatroska", err)
	}
}

func TestMkvLastDurationWalksEverySource(t *testing.T) {
	written := []Sample{{Duration: 4}}
	cases := []struct {
		name                        string
		stated, perFrame, remaining int64
		written                     []Sample
		want                        int64
	}{
		{"the block", 1, 2, 3, written, 1},
		{"the track", 0, 2, 3, written, 2},
		{"the segment", 0, 0, 3, written, 3},
		{"the sample before", 0, 0, 0, written, 4},
	}
	for _, tc := range cases {
		got, err := mkvLastDuration(1, tc.stated, tc.perFrame, tc.remaining, tc.written)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %d, %v, want %d", tc.name, got, err, tc.want)
		}
	}
	if _, err := mkvLastDuration(1, 0, 0, 0, nil); !errors.Is(err, ErrMatroska) {
		t.Fatalf("err = %v, want ErrMatroska", err)
	}
}

func TestReaderRefusesTimesNoSampleTableCanState(t *testing.T) {
	// Two frames shown at the same time leave the first lasting nothing.
	same := timedDoc(0, 10_000_000, []mkvBlockGroup{
		{Block: simple(1, 0, true, []byte("a"))},
		{Block: simple(1, 0, true, []byte("b"))},
	}, 0, 0)
	r, err := NewReader(marshalDoc(t, same))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrMatroska) {
		t.Fatalf("a sample lasting nothing: %v, want ErrMatroska", err)
	}
	// Frames three thousand million ticks apart last longer than a sample
	// table can say.
	far := timedDoc(0, 10_000_000, []mkvBlockGroup{
		{Block: simple(1, 0, true, []byte("a"))},
		{Block: simple(1, 0, true, []byte("b"))},
	}, 0, 5_000_000_000)
	r, err = NewReader(marshalDoc(t, far))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrMatroska) {
		t.Fatalf("a sample lasting too long: %v, want ErrMatroska", err)
	}
	// The same distance the other way round is a reordering no composition
	// offset can state.
	reordered := timedDoc(0, 10_000_000, []mkvBlockGroup{
		{Block: simple(1, 0, true, []byte("a"))},
		{Block: simple(1, 0, true, []byte("b"))},
	}, 3_000_000_000, 0)
	r, err = NewReader(marshalDoc(t, reordered))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrMatroska) {
		t.Fatalf("an offset too large to state: %v, want ErrMatroska", err)
	}
}

func TestMkvTimescale(t *testing.T) {
	for scale, want := range map[uint64]uint32{1_000_000: 1000, 1: 1_000_000_000, 1000: 1_000_000} {
		got, err := mkvTimescale(scale)
		if err != nil || got != want {
			t.Fatalf("mkvTimescale(%d) = %d, %v, want %d", scale, got, err, want)
		}
	}
	for _, scale := range []uint64{0, 3, 1_000_000_001} {
		if _, err := mkvTimescale(scale); !errors.Is(err, ErrMatroska) {
			t.Fatalf("mkvTimescale(%d) = %v, want ErrMatroska", scale, err)
		}
	}
}

func TestReaderRefusesAScaleNoTimescaleStates(t *testing.T) {
	doc := webmDoc()
	doc.Segment.Info.TimecodeScale = 3
	if _, err := NewReader(marshalDoc(t, doc)); !errors.Is(err, ErrMatroska) {
		t.Fatalf("err = %v, want ErrMatroska", err)
	}
}

func TestReaderTakesTheDefaultTimestampScale(t *testing.T) {
	doc := webmDoc()
	doc.Segment.Info.TimecodeScale = 0
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r.File().Timescale != 1000 {
		t.Fatalf("timescale = %d, want the 1000 the default scale gives", r.File().Timescale)
	}
}

// --- codecs -----------------------------------------------------------------

func TestMkvCodecNamesEveryMappingAndRefusesTheRest(t *testing.T) {
	for id, want := range map[string]string{
		"V_MPEG4/ISO/AVC":  "avc1",
		"V_MPEGH/ISO/HEVC": "hvc1",
		"V_AV1":            "av01",
		"V_VP9":            "vp09",
		"V_VP8":            "vp08",
		"A_OPUS":           "Opus",
		"A_VORBIS":         "vorb",
		"A_FLAC":           "fLaC",
		"A_AAC":            "mp4a",
		"A_AAC/MPEG4/LC":   "mp4a",
		"A_AC3":            "ac-3",
		"A_AC3/BSID9":      "ac-3",
		"A_EAC3":           "ec-3",
	} {
		got, ok := mkvCodec(id)
		if !ok || got != want {
			t.Fatalf("mkvCodec(%q) = %q, %v, want %q", id, got, ok, want)
		}
	}
	for _, id := range []string{"S_TEXT/UTF8", "V_MPEG2", ""} {
		if got, ok := mkvCodec(id); ok {
			t.Fatalf("mkvCodec(%q) = %q, want no mapping", id, got)
		}
	}
}

func TestMkvTrackConfigReadsEveryCodecPrivate(t *testing.T) {
	entry := func(id string, private []byte) mkvTrackEntry {
		return mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: id,
			CodecPrivate: private, Video: &mkvVideo{PixelWidth: 32, PixelHeight: 24}}
	}
	avcCfg, err := mkvTrackConfig(entry("V_MPEG4/ISO/AVC", avcC(t)), 1000)
	if err != nil {
		t.Fatalf("avc: %v", err)
	}
	if len(avcCfg.SPS) != 1 || len(avcCfg.PPS) != 1 || avcCfg.SPS[0][0] != 0x67 {
		t.Fatalf("avc config = %+v", avcCfg)
	}
	hevcCfg, err := mkvTrackConfig(entry("V_MPEGH/ISO/HEVC", hvcCPayload(t)), 1000)
	if err != nil {
		t.Fatalf("hevc: %v", err)
	}
	if len(hevcCfg.VPS) != 1 || len(hevcCfg.SPS) != 1 || len(hevcCfg.PPS) != 1 {
		t.Fatalf("hevc config = %+v", hevcCfg)
	}
	if hevcCfg.VPS[0][0] != 0x40 || hevcCfg.SPS[0][0] != 0x42 || hevcCfg.PPS[0][0] != 0x44 {
		t.Fatalf("hevc parameter sets were mixed up: %+v", hevcCfg)
	}
	av1Cfg, err := mkvTrackConfig(entry("V_AV1", av1CPayload), 1000)
	if err != nil {
		t.Fatalf("av1: %v", err)
	}
	if !bytes.Equal(av1Cfg.CodecConfig, av1CPayload) {
		t.Fatalf("av1 config = % x", av1Cfg.CodecConfig)
	}
	// AC-3 states nothing in Matroska, and what a track does state travels as
	// it stands.
	ac3, err := mkvTrackConfig(mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackAudio,
		CodecID: "A_AC3", Audio: &mkvAudio{Channels: 2, SamplingFrequency: 48000}}, 48000)
	if err != nil {
		t.Fatalf("ac-3: %v", err)
	}
	if ac3.Codec != "ac-3" || len(ac3.CodecConfig) != 0 || ac3.Channels != 2 {
		t.Fatalf("ac-3 config = %+v", ac3)
	}
	// AAC states its profile in an audio specific config, and may state none.
	aacEntry := mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_AAC",
		CodecPrivate: []byte{0x11, 0x90}}
	withASC, err := mkvTrackConfig(aacEntry, 48000)
	if err != nil {
		t.Fatalf("aac: %v", err)
	}
	if withASC.AudioObjectType != 2 || withASC.SampleRate != 48000 {
		t.Fatalf("aac config = %+v, want AAC-LC at 48000", withASC)
	}
	aacEntry.CodecPrivate = nil
	withoutASC, err := mkvTrackConfig(aacEntry, 48000)
	if err != nil {
		t.Fatalf("aac without a config: %v", err)
	}
	if withoutASC.AudioObjectType != 0 {
		t.Fatalf("profile = %d, want the zero that stands for AAC-LC", withoutASC.AudioObjectType)
	}
}

func TestMkvTrackConfigRefusesPrivateDataItCannotRead(t *testing.T) {
	cases := map[string]mkvTrackEntry{
		"avc without a record":  {TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_MPEG4/ISO/AVC"},
		"hevc without a record": {TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_MPEGH/ISO/HEVC"},
		"av1 without a record":  {TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_AV1"},
		"opus without a header": {TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_OPUS"},
		"aac with a broken config": {TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_AAC",
			CodecPrivate: []byte{0xff}},
	}
	for name, te := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := mkvTrackConfig(te, 1000); !errors.Is(err, ErrTrackConfig) {
				t.Fatalf("err = %v, want ErrTrackConfig", err)
			}
		})
	}
}

func TestMkvTrackConfigTakesTheAACRateFromItsConfig(t *testing.T) {
	// A track whose Audio element states no rate still has one in its audio
	// specific config, and a muxer needs it.
	cfg, err := mkvTrackConfig(mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackAudio,
		CodecID: "A_AAC/MPEG4/LC", CodecPrivate: []byte{0x11, 0x90}}, 48000)
	if err != nil {
		t.Fatalf("mkvTrackConfig: %v", err)
	}
	if cfg.SampleRate != 48000 {
		t.Fatalf("rate = %d, want the 48000 the config states", cfg.SampleRate)
	}
}

func TestReaderKeepsTheOtherTracksOfAFileItCannotFullyDescribe(t *testing.T) {
	doc := webmDoc()
	doc.Segment.Tracks.TrackEntry = append(doc.Segment.Tracks.TrackEntry, mkvTrackEntry{
		TrackNumber: 3, TrackType: mkvTrackSubtitle, CodecID: "S_TEXT/UTF8",
	})
	doc.Segment.Cluster[0].SimpleBlock = append(doc.Segment.Cluster[0].SimpleBlock,
		simple(3, 0, true, []byte("hello")))
	doc.Segment.Cluster[1].SimpleBlock = append(doc.Segment.Cluster[1].SimpleBlock,
		simple(3, 40, true, []byte("there")))
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	// The track this package cannot describe is still listed, under the name
	// the file gave it.
	if ids := r.TrackIDs(); len(ids) != 3 {
		t.Fatalf("tracks = %v, want three", ids)
	}
	var subtitle Track
	for _, tr := range r.File().Tracks {
		if tr.ID == 3 {
			subtitle = tr
		}
	}
	if subtitle.Codec != "S_TEXT/UTF8" || subtitle.Kind != Subtitle {
		t.Fatalf("subtitle track = %+v", subtitle)
	}
	if _, err := r.TrackConfig(3); !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("TrackConfig(3) = %v, want ErrUnsupportedCodec", err)
	}
	// Its samples still read: it is the sample entry that cannot be built,
	// not the data that cannot be found.
	samples, err := r.Samples(3)
	if err != nil {
		t.Fatalf("Samples(3): %v", err)
	}
	if len(samples) != 2 || string(samples[0].Data) != "hello" {
		t.Fatalf("subtitle samples = %d, first %q", len(samples), samples[0].Data)
	}
	// And the tracks it can describe are untouched.
	if _, err := r.TrackConfig(1); err != nil {
		t.Fatalf("TrackConfig(1): %v", err)
	}
	if got, err := r.Samples(2); err != nil || len(got) != 6 {
		t.Fatalf("Samples(2) = %d, %v, want 6 samples", len(got), err)
	}
}

func TestReaderReportsATrackWithoutABlock(t *testing.T) {
	doc := webmDoc()
	doc.Segment.Cluster = nil
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("err = %v, want ErrNoSamples", err)
	}
	if r.File().Duration != 0 {
		t.Fatalf("duration = %d, want none", r.File().Duration)
	}
}

func TestNewMatroskaReaderSurfacesADecodeError(t *testing.T) {
	// EBML magic, so the sniffer sends these bytes here, wrapping an element
	// id the Matroska schema does not have.
	bad := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x84, 0x80, 0x80, 0x81, 0x00}
	if Sniff(bad) != FormatMatroska {
		t.Fatal("precondition: the bytes must sniff as Matroska")
	}
	if _, err := NewReader(bad); err == nil {
		t.Fatal("a document with an unknown element was accepted")
	}
}

// --- VP8 and VP9 ------------------------------------------------------------

func TestMkvVPxReadsWhatAColourElementStates(t *testing.T) {
	if got := mkvVPx(nil); got.BitDepth != 8 || got.MatrixCoefficients != 2 || got.FullRange {
		t.Fatalf("a track with no video element = %+v", got)
	}
	if got := mkvVPx(&mkvVideo{}); got.ColourPrimaries != 2 || got.ChromaSubsampling != 0 {
		t.Fatalf("a track with no colour element = %+v", got)
	}
	full := mkvVPx(&mkvVideo{Colour: &mkvColour{
		BitsPerChannel:          10,
		Primaries:               9,
		TransferCharacteristics: 16,
		MatrixCoefficients:      []uint64{9},
		Range:                   mkvRangeFull,
		ChromaSubsamplingHorz:   []uint64{1},
		ChromaSubsamplingVert:   []uint64{1},
		ChromaSitingVert:        mkvSitingColocated,
	}})
	want := &VPxConfig{BitDepth: 10, ChromaSubsampling: vpxChroma420Colocated, FullRange: true,
		ColourPrimaries: 9, TransferCharacteristics: 16, MatrixCoefficients: 9}
	if *full != *want {
		t.Fatalf("colour = %+v, want %+v", *full, *want)
	}
	// An identity matrix is a stated zero, which is why its absence is told
	// apart from it.
	identity := mkvVPx(&mkvVideo{Colour: &mkvColour{MatrixCoefficients: []uint64{0}}})
	if identity.MatrixCoefficients != 0 {
		t.Fatalf("a stated identity matrix came back as %d", identity.MatrixCoefficients)
	}
}

func TestMkvChromaSubsampling(t *testing.T) {
	colour := func(h, v uint64, siting uint64) *mkvColour {
		return &mkvColour{ChromaSubsamplingHorz: []uint64{h}, ChromaSubsamplingVert: []uint64{v},
			ChromaSitingVert: siting}
	}
	cases := []struct {
		colour *mkvColour
		want   byte
		ok     bool
	}{
		{colour(1, 1, 0), vpxChroma420Vertical, true},
		{colour(1, 1, mkvSitingColocated), vpxChroma420Colocated, true},
		{colour(1, 0, 0), vpxChroma422, true},
		{colour(0, 0, 0), vpxChroma444, true},
		{colour(0, 1, 0), 0, false},
		{&mkvColour{ChromaSubsamplingVert: []uint64{1}}, 0, false},
		{&mkvColour{ChromaSubsamplingHorz: []uint64{1}}, 0, false},
	}
	for i, tc := range cases {
		got, ok := mkvChromaSubsampling(tc.colour)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("case %d = %d, %v, want %d, %v", i+1, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReaderReadsAVP9TrackAndSaysWhatMatroskaCannotState(t *testing.T) {
	doc := &mkvReadDoc{
		Header: mkvHeader{DocType: "webm"},
		Segment: mkvReadSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{
				TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_VP9",
				DefaultDuration: 40_000_000,
				Video: &mkvVideo{PixelWidth: 320, PixelHeight: 240, Colour: &mkvColour{
					BitsPerChannel:        8,
					ChromaSubsamplingHorz: []uint64{1},
					ChromaSubsamplingVert: []uint64{1},
				}},
			}}},
			Cluster: []mkvCluster{{Timecode: 0,
				SimpleBlock: []ebml.Block{simple(1, 0, true, []byte("frame"))}}},
		},
	}
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	cfg, err := r.TrackConfig(1)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if cfg.Codec != "vp09" || cfg.VPx == nil {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.VPx.BitDepth != 8 || cfg.VPx.ChromaSubsampling != vpxChroma420Vertical {
		t.Fatalf("vpcC = %+v", *cfg.VPx)
	}
	// Matroska states neither a profile nor a level, and a level of zero is
	// not one: the muxer says so rather than writing a guess.
	if cfg.VPx.Level != 0 {
		t.Fatalf("level = %d, want the zero Matroska leaves it at", cfg.VPx.Level)
	}
	if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("AddTrack of a VP9 track without a level: %v, want ErrTrackConfig", err)
	}
	// Given the level the bitstream holds, the same configuration writes.
	cfg.VPx.Level = 10
	if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(cfg); err != nil {
		t.Fatalf("AddTrack once the level is known: %v", err)
	}
	if samples, err := r.Samples(1); err != nil || len(samples) != 1 || samples[0].Duration != 40 {
		t.Fatalf("samples = %+v, %v", samples, err)
	}
}
