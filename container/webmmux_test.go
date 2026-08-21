// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
)

// webmFile is what a written file is read back into. The declarations are
// ebml-go's own, so what these tests assert is what the reference library sees
// in the file rather than what this package believed it wrote.
type webmFile struct {
	Header  webm.EBMLHeader `ebml:"EBML"`
	Segment webm.Segment    `ebml:"Segment"`
}

// readWebM parses a written file with ebml-go.
func readWebM(t *testing.T, data []byte) webmFile {
	t.Helper()
	var f webmFile
	if err := ebml.Unmarshal(bytes.NewReader(data), &f); err != nil {
		t.Fatalf("read back with ebml-go: %v", err)
	}
	return f
}

// webmBlock is one block as read back, with its timestamp made absolute again.
type webmBlock struct {
	cluster  int
	track    uint64
	tick     int64
	relative int16
	keyframe bool
	data     []byte
	// duration is what a block group states, and zero for a simple block,
	// which has nowhere to state one.
	duration uint64
	group    bool
}

// webmBlocks lists every block of every cluster, in both of the forms a cluster
// can hold them: the last frame of each track is a block group, because that is
// the only form that can state how long a frame lasts.
func webmBlocks(f webmFile) []webmBlock {
	var out []webmBlock
	for i, cluster := range f.Segment.Cluster {
		for _, b := range cluster.SimpleBlock {
			out = append(out, webmBlock{
				cluster:  i,
				track:    b.TrackNumber,
				tick:     int64(cluster.Timecode) + int64(b.Timecode),
				relative: b.Timecode,
				keyframe: b.Keyframe,
				data:     b.Data[0],
			})
		}
		for _, g := range cluster.BlockGroup {
			out = append(out, webmBlock{
				cluster:  i,
				track:    g.Block.TrackNumber,
				tick:     int64(cluster.Timecode) + int64(g.Block.Timecode),
				relative: g.Block.Timecode,
				// A block group states no flag: a frame with no reference is
				// a keyframe, which is how a reader tells them apart.
				keyframe: g.ReferenceBlock == 0,
				data:     g.Block.Data[0],
				duration: g.BlockDuration,
				group:    true,
			})
		}
	}
	return out
}

// clusterOpensOn is the block a cluster starts at, whichever form holds it.
func clusterOpensOn(f webmFile, cluster int) webmBlock {
	var first webmBlock
	found := false
	for _, b := range webmBlocks(f) {
		if b.cluster != cluster {
			continue
		}
		if !found || b.relative < first.relative {
			first, found = b, true
		}
	}
	return first
}

// The two tracks the round-trip tests write: VP9 at the 90 kHz an MPEG-TS
// source hands over, and Opus at its own 48 kHz.
const (
	webmVideoTimescale = 90000
	webmVideoFrame     = 3003 // 1/29.97 s, the timing that rounds worst
	webmAudioTimescale = 48000
	webmAudioFrame     = 960 // 20 ms
	webmSyncEvery      = 6
)

func webmVideoConfig() TrackConfig {
	return TrackConfig{
		Kind: Video, Codec: "V_VP9", Timescale: webmVideoTimescale,
		Width: 640, Height: 360,
	}
}

func webmAudioConfig() TrackConfig {
	return TrackConfig{
		Kind: Audio, Codec: "A_OPUS", Timescale: webmAudioTimescale,
		Channels: 2, SampleRate: 48000, PreSkip: 312, Language: "eng",
	}
}

// webmSampleData is a frame's payload: distinct per track and per index, so a
// block read back can be traced to the sample that produced it.
func webmSampleData(track uint32, i int) []byte {
	return []byte{byte(track), byte(i), byte(i >> 8), 0xA5}
}

// writeWebMScript writes a video and an audio track interleaved by presentation
// time, which is how a caller feeding two streams keeps them in step, and
// returns the file.
func writeWebMScript(t *testing.T, videoFrames, audioFrames int, opts ...WebMOption) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, opts...)
	video, err := m.AddTrack(webmVideoConfig())
	if err != nil {
		t.Fatalf("AddTrack(video): %v", err)
	}
	audio, err := m.AddTrack(webmAudioConfig())
	if err != nil {
		t.Fatalf("AddTrack(audio): %v", err)
	}
	v, a := 0, 0
	for v < videoFrames || a < audioFrames {
		videoTime := float64(v*webmVideoFrame) / webmVideoTimescale
		audioTime := float64(a*webmAudioFrame) / webmAudioTimescale
		if v < videoFrames && (a >= audioFrames || videoTime <= audioTime) {
			if err := m.WriteSample(video, Sample{
				Data: webmSampleData(video, v), Duration: webmVideoFrame,
				Sync: v%webmSyncEvery == 0,
			}); err != nil {
				t.Fatalf("WriteSample(video %d): %v", v, err)
			}
			v++
			continue
		}
		if err := m.WriteSample(audio, Sample{
			Data: webmSampleData(audio, a), Duration: webmAudioFrame, Sync: true,
		}); err != nil {
			t.Fatalf("WriteSample(audio %d): %v", a, err)
		}
		a++
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// expectedTick is the tick a time in track units lands on, rounded to the
// nearest tick, computed here without the muxer's 128-bit arithmetic.
func expectedTick(units, timescale, ticksPerSecond uint64) int64 {
	return int64((units*ticksPerSecond + timescale/2) / timescale)
}

func TestWebMMuxerRoundTripsThroughDemuxAndEBML(t *testing.T) {
	const videoFrames, audioFrames = 12, 20
	data := writeWebMScript(t, videoFrames, audioFrames)

	// What the package's own demuxer makes of it.
	file, err := Demux(data)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if file.Format != "webm" {
		t.Errorf("format = %q, want webm", file.Format)
	}
	if file.Timescale != 1000 {
		t.Errorf("timescale = %d, want 1000 ticks per second", file.Timescale)
	}
	if len(file.Tracks) != 2 {
		t.Fatalf("tracks = %+v", file.Tracks)
	}
	video, audio := file.Tracks[0], file.Tracks[1]
	if video.ID != 1 || video.Kind != Video || video.Codec != "V_VP9" ||
		video.Width != 640 || video.Height != 360 || video.Language != "und" {
		t.Errorf("video track = %+v", video)
	}
	if audio.ID != 2 || audio.Kind != Audio || audio.Codec != "A_OPUS" ||
		audio.Channels != 2 || audio.SampleRate != 48000 || audio.Language != "eng" {
		t.Errorf("audio track = %+v", audio)
	}

	// What the reference library makes of it.
	f := readWebM(t, data)
	if f.Header.DocType != "webm" {
		t.Errorf("doc type = %q, want webm", f.Header.DocType)
	}
	if f.Segment.Info.TimecodeScale != uint64(time.Millisecond) {
		t.Errorf("timecode scale = %d ns", f.Segment.Info.TimecodeScale)
	}
	if f.Segment.Info.MuxingApp != webmMuxingApp || f.Segment.Info.WritingApp != webmMuxingApp {
		t.Errorf("apps = %q / %q", f.Segment.Info.MuxingApp, f.Segment.Info.WritingApp)
	}
	// A streaming segment cannot state a duration: nothing knows it yet.
	if f.Segment.Info.Duration != 0 {
		t.Errorf("duration = %v, want none in a streaming segment", f.Segment.Info.Duration)
	}
	if f.Segment.SeekHead != nil || f.Segment.Cues != nil {
		t.Errorf("this muxer writes no SeekHead and no Cues, got %+v / %+v",
			f.Segment.SeekHead, f.Segment.Cues)
	}
	if len(f.Segment.Tracks.TrackEntry) != 2 {
		t.Fatalf("track entries = %+v", f.Segment.Tracks.TrackEntry)
	}
	// 12 frames of 1/29.97 s is 400 ms, well inside the default one-second
	// cluster, so the whole file is one cluster starting at zero.
	if len(f.Segment.Cluster) != 1 || f.Segment.Cluster[0].Timecode != 0 {
		t.Fatalf("clusters = %+v", f.Segment.Cluster)
	}

	blocks := webmBlocks(f)
	if len(blocks) != videoFrames+audioFrames {
		t.Fatalf("blocks = %d, want %d", len(blocks), videoFrames+audioFrames)
	}
	var v, a int
	for _, b := range blocks {
		switch b.track {
		case 1:
			want := expectedTick(uint64(v*webmVideoFrame), webmVideoTimescale, 1000)
			if b.tick != want {
				t.Errorf("video block %d at tick %d, want %d", v, b.tick, want)
			}
			if b.keyframe != (v%webmSyncEvery == 0) {
				t.Errorf("video block %d keyframe = %v", v, b.keyframe)
			}
			if !bytes.Equal(b.data, webmSampleData(1, v)) {
				t.Errorf("video block %d data = %x", v, b.data)
			}
			v++
		case 2:
			if want := int64(a * 20); b.tick != want {
				t.Errorf("audio block %d at tick %d, want %d", a, b.tick, want)
			}
			if !b.keyframe {
				t.Errorf("audio block %d is not a keyframe", a)
			}
			if !bytes.Equal(b.data, webmSampleData(2, a)) {
				t.Errorf("audio block %d data = %x", a, b.data)
			}
			a++
		default:
			t.Fatalf("a block names track %d", b.track)
		}
	}
	if v != videoFrames || a != audioFrames {
		t.Errorf("blocks per track = %d / %d", v, a)
	}
}

// segmentID is the element every Matroska file's payload lives in.
var segmentID = []byte{0x18, 0x53, 0x80, 0x67}

// unknownSize is the data size an element whose length is not yet known states:
// a one-byte length marker followed by seven bytes of ones.
var unknownSize = []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

// segmentSizeBytes is what follows the Segment element's identifier.
func segmentSizeBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	i := bytes.Index(data, segmentID)
	if i < 0 {
		t.Fatalf("the file holds no Segment element")
	}
	return data[i+len(segmentID) : i+len(segmentID)+len(unknownSize)]
}

func TestWebMMuxerLeavesTheSegmentOpenUntilAskedToBufferIt(t *testing.T) {
	const videoFrames, audioFrames = 12, 20
	streamed := writeWebMScript(t, videoFrames, audioFrames)
	buffered := writeWebMScript(t, videoFrames, audioFrames, BufferedSegment())

	if got := segmentSizeBytes(t, streamed); !bytes.Equal(got, unknownSize) {
		t.Errorf("a streamed segment states its size as %x, want the unknown-size marker %x",
			got, unknownSize)
	}
	if got := segmentSizeBytes(t, buffered); bytes.Equal(got, unknownSize) {
		t.Errorf("a buffered segment must state its real size, got the unknown-size marker")
	}

	f := readWebM(t, buffered)
	// The longest track is the video: 12 frames of 3003 at 90 kHz is
	// 400.4 ms, which the millisecond tick states as 400.
	want := float64(expectedTick(videoFrames*webmVideoFrame, webmVideoTimescale, 1000))
	if f.Segment.Info.Duration != want {
		t.Errorf("duration = %v ticks, want %v", f.Segment.Info.Duration, want)
	}
	if len(f.Segment.Cluster) != 1 {
		t.Fatalf("clusters = %+v", f.Segment.Cluster)
	}
	if got := len(webmBlocks(f)); got != videoFrames+audioFrames {
		t.Errorf("blocks = %d, want %d", got, videoFrames+audioFrames)
	}
	// Both modes describe the same media, so both demux to the same thing.
	for name, data := range map[string][]byte{"streamed": streamed, "buffered": buffered} {
		file, err := Demux(data)
		if err != nil {
			t.Fatalf("Demux(%s): %v", name, err)
		}
		if len(file.Tracks) != 2 || file.Format != "webm" {
			t.Errorf("%s demuxes as %+v", name, file)
		}
	}
	if file, err := Demux(buffered); err != nil {
		t.Fatalf("Demux(buffered): %v", err)
	} else if file.Duration != uint64(want) {
		t.Errorf("demuxed duration = %d, want %v", file.Duration, want)
	}
}

func TestWebMMuxerIsByteForByteReproducible(t *testing.T) {
	first := writeWebMScript(t, 8, 10)
	second := writeWebMScript(t, 8, 10)
	if !bytes.Equal(first, second) {
		t.Errorf("two runs of the same input differ: %d and %d bytes", len(first), len(second))
	}
}

func TestWebMMuxerStartsAClusterOnAKeyframe(t *testing.T) {
	// A hundred milliseconds is three frames of 1/29.97 s, so every third
	// keyframe-aligned frame opens a cluster.
	data := writeWebMScript(t, 13, 0, ClusterDuration(100*time.Millisecond), BufferedSegment())
	f := readWebM(t, data)
	// Sync samples fall on 0, 6 and 12; the first opens the file's cluster and
	// each of the others is more than 100 ms past the cluster it would join.
	want := []uint64{0, 200, 400}
	if len(f.Segment.Cluster) != len(want) {
		t.Fatalf("clusters = %d, want %d", len(f.Segment.Cluster), len(want))
	}
	for i, cluster := range f.Segment.Cluster {
		if cluster.Timecode != want[i] {
			t.Errorf("cluster %d starts at %d, want %d", i, cluster.Timecode, want[i])
		}
		if !clusterOpensOn(f, i).keyframe {
			t.Errorf("cluster %d does not start on a keyframe", i)
		}
	}
	if got := len(webmBlocks(f)); got != 13 {
		t.Errorf("blocks = %d, want 13", got)
	}
}

func TestWebMMuxerStartsAClusterWhenABlockWouldNotFitTheOneOpen(t *testing.T) {
	// One microsecond ticks leave a block 32.767 ms of room from its cluster,
	// which a frame of 33.37 ms exceeds: every frame needs a cluster of its
	// own, keyframe or not.
	const frames = 5
	data := writeWebMScript(t, frames, 0,
		TimestampScale(time.Microsecond), ClusterDuration(time.Hour), BufferedSegment())
	f := readWebM(t, data)
	if len(f.Segment.Cluster) != frames {
		t.Fatalf("clusters = %d, want one per frame (%d)", len(f.Segment.Cluster), frames)
	}
	for i, b := range webmBlocks(f) {
		want := expectedTick(uint64(i*webmVideoFrame), webmVideoTimescale, 1_000_000)
		if b.tick != want {
			t.Errorf("block %d at tick %d, want %d", i, b.tick, want)
		}
		if b.relative != 0 {
			t.Errorf("block %d states %d against its own cluster, want 0", i, b.relative)
		}
	}
	// The finer tick is stated in the file, and read back by the demuxer.
	if f.Segment.Info.TimecodeScale != uint64(time.Microsecond) {
		t.Errorf("timecode scale = %d ns", f.Segment.Info.TimecodeScale)
	}
	file, err := Demux(data)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if file.Timescale != 1_000_000 {
		t.Errorf("demuxed timescale = %d", file.Timescale)
	}
}

func TestWebMMuxerHonoursAClusterBoundShorterThanATick(t *testing.T) {
	// A bound of nothing would put every block in a cluster of its own; the
	// muxer holds it at one tick instead, so a keyframe one whole tick past
	// the cluster's start opens the next one.
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, TimestampScale(time.Second), ClusterDuration(time.Millisecond),
		BufferedSegment())
	id, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "V_VP8", Timescale: 1,
		Width: 16, Height: 16})
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := m.WriteSample(id, Sample{Data: []byte{byte(i)}, Duration: 1, Sync: true}); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f := readWebM(t, buf.Bytes())
	if len(f.Segment.Cluster) != 3 {
		t.Fatalf("clusters = %d, want one per second-long frame", len(f.Segment.Cluster))
	}
}

func TestWebMTimestampsDoNotDriftFromTheTrackTimescale(t *testing.T) {
	// 1/29.97 s is the timing a millisecond tick states worst: 33.3667 ms.
	// Converting each frame's own cumulative time keeps every block within
	// half a tick of where it belongs; converting the previous tick and adding
	// would have drifted by a tick every three frames.
	const frames = 600
	data := writeWebMScript(t, frames, 0, ClusterDuration(time.Hour), BufferedSegment())
	f := readWebM(t, data)
	blocks := webmBlocks(f)
	if len(blocks) != frames {
		t.Fatalf("blocks = %d, want %d", len(blocks), frames)
	}
	var worst float64
	for i, b := range blocks {
		exact := float64(i) * webmVideoFrame * 1000 / webmVideoTimescale
		if off := math.Abs(float64(b.tick) - exact); off > worst {
			worst = off
		}
		if want := expectedTick(uint64(i*webmVideoFrame), webmVideoTimescale, 1000); b.tick != want {
			t.Fatalf("block %d at tick %d, want %d", i, b.tick, want)
		}
	}
	if worst > 0.5 {
		t.Errorf("the worst block is %v ticks from where it belongs, want at most half a tick", worst)
	}
	// The last frame of a ten-second file lands where a ten-second file's last
	// frame belongs, not a dozen ticks early.
	if got, want := blocks[frames-1].tick, int64(19987); got != want {
		t.Errorf("the last of %d frames is at tick %d, want %d", frames, got, want)
	}
}

func TestWebMMuxerHonoursCompositionOffsets(t *testing.T) {
	// Matroska blocks carry presentation times, so a reordered stream's offset
	// moves the block rather than riding beside it.
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, BufferedSegment())
	id, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "vp09", Timescale: 1000,
		Width: 32, Height: 32})
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	offsets := []int32{0, 40, -10, 0}
	for i, offset := range offsets {
		if err := m.WriteSample(id, Sample{
			Data: []byte{byte(i)}, Duration: 20, CompositionOffset: offset, Sync: i == 0,
		}); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f := readWebM(t, buf.Bytes())
	want := []int64{0, 60, 30, 60}
	blocks := webmBlocks(f)
	if len(blocks) != len(want) {
		t.Fatalf("blocks = %d, want %d", len(blocks), len(want))
	}
	for i, b := range blocks {
		if b.tick != want[i] {
			t.Errorf("block %d at tick %d, want %d", i, b.tick, want[i])
		}
	}
	// The duration is the last presentation time plus its frame, not the
	// decoding time the offsets moved away from.
	if f.Segment.Info.Duration != 80 {
		t.Errorf("duration = %v, want 80", f.Segment.Info.Duration)
	}
}

// webmAVCSets lifts real AVC parameter sets out of the fixture, so the private
// data a player would read is built from sets a decoder would accept.
func webmAVCSets(t *testing.T) (sps, pps [][]byte) {
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
			return avcx.AvcC.SPSnalus, avcx.AvcC.PPSnalus
		}
	}
	t.Fatal("the fixture holds no AVC track")
	return nil, nil
}

// Real HEVC parameter sets, as the reference encoder's own tests carry them,
// without their start codes.
const (
	webmHEVCVPS = "40010c01ffff016000000300900000030000030078959809"
	webmHEVCSPS = "420101016000000300900000030000030078a00502016965959a4932bc05a808080820" +
		"00000300200000030321"
	webmHEVCPPS = "4401c172b46240"
)

func webmHEVCSets(t *testing.T) (vps, sps, pps [][]byte) {
	t.Helper()
	decode := func(s string) [][]byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
		return [][]byte{b}
	}
	return decode(webmHEVCVPS), decode(webmHEVCSPS), decode(webmHEVCPPS)
}

// xiphLace lays packets out the way Matroska's Vorbis private data does: how
// many packets follow, then all but the last one's length, then the packets.
func xiphLace(packets ...[]byte) []byte {
	out := []byte{byte(len(packets) - 1)}
	for _, p := range packets[:len(packets)-1] {
		for n := len(p); ; n -= 0xFF {
			if n < 0xFF {
				out = append(out, byte(n))
				break
			}
			out = append(out, 0xFF)
		}
	}
	for _, p := range packets {
		out = append(out, p...)
	}
	return out
}

func webmVorbisHeaders() []byte {
	return xiphLace(
		append([]byte{vorbisIdentificationPacket}, []byte("vorbis\x00\x00\x00\x00\x02")...),
		append([]byte{0x03}, []byte("vorbis comment")...),
		append([]byte{0x05}, []byte("vorbis setup")...),
	)
}

// writeOneWebMTrack writes a file holding this track and a few samples.
func writeOneWebMTrack(t *testing.T, cfg TrackConfig, samples int) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, BufferedSegment())
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack(%s): %v", cfg.Codec, err)
	}
	for i := 0; i < samples; i++ {
		if err := m.WriteSample(id, Sample{
			Data: webmSampleData(id, i), Duration: 960, Sync: true,
		}); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func TestWebMMuxerDescribesEveryCodecItAccepts(t *testing.T) {
	avcSPS, avcPPS := webmAVCSets(t)
	hevcVPS, hevcSPS, hevcPPS := webmHEVCSets(t)
	av1Config := []byte{0x81, 0x00, 0x0c, 0x00}
	vorbisHeaders := webmVorbisHeaders()
	opusHeader, err := OpusHead(TrackConfig{Channels: 1, SampleRate: 24000, PreSkip: 120})
	if err != nil {
		t.Fatalf("OpusHead: %v", err)
	}

	cases := []struct {
		name    string
		cfg     TrackConfig
		id      string
		docType string
		check   func(t *testing.T, entry webm.TrackEntry)
	}{{
		// The ISO-BMFF sample entry name is accepted beside the Matroska one,
		// so a track read out of an MP4 is handed over as it came.
		name:    "vp8 under its mp4 name",
		cfg:     TrackConfig{Kind: Video, Codec: "vp08", Timescale: 1000, Width: 320, Height: 240},
		id:      "V_VP8",
		docType: "webm",
		check: func(t *testing.T, entry webm.TrackEntry) {
			if len(entry.CodecPrivate) != 0 {
				t.Errorf("VP8 needs no private data, got %x", entry.CodecPrivate)
			}
			if entry.Video == nil || entry.Video.PixelWidth != 320 || entry.Video.PixelHeight != 240 {
				t.Errorf("video = %+v", entry.Video)
			}
		},
	}, {
		// A vpcC record, which the ISO-BMFF sample entry cannot do without, has
		// no place in Matroska: it is accepted and left out.
		name: "vp9 with a record matroska does not carry",
		cfg: TrackConfig{Kind: Video, Codec: "V_VP9", Timescale: 1000, Width: 640, Height: 360,
			VPx: &VPxConfig{Profile: 0, Level: 30, BitDepth: 8}},
		id:      "V_VP9",
		docType: "webm",
		check: func(t *testing.T, entry webm.TrackEntry) {
			if len(entry.CodecPrivate) != 0 {
				t.Errorf("VP9 in Matroska carries no private data, got %x", entry.CodecPrivate)
			}
		},
	}, {
		name: "av1 states its configuration record",
		cfg: TrackConfig{Kind: Video, Codec: "av01", Timescale: 30000, Width: 640, Height: 360,
			CodecConfig: av1Config},
		id:      "V_AV1",
		docType: "webm",
		check: func(t *testing.T, entry webm.TrackEntry) {
			if !bytes.Equal(entry.CodecPrivate, av1Config) {
				t.Errorf("private data = %x, want the record handed over, %x",
					entry.CodecPrivate, av1Config)
			}
		},
	}, {
		name: "avc states its decoder configuration record",
		cfg: TrackConfig{Kind: Video, Codec: "avc1", Timescale: 12800, Width: 640, Height: 360,
			SPS: avcSPS, PPS: avcPPS},
		id: "V_MPEG4/ISO/AVC",
		// AVC is not a codec WebM allows, so the file says what it is.
		docType: "matroska",
		check: func(t *testing.T, entry webm.TrackEntry) {
			record, err := avc.DecodeAVCDecConfRec(entry.CodecPrivate)
			if err != nil {
				t.Fatalf("the private data is not an AVC record: %v", err)
			}
			if len(record.SPSnalus) != len(avcSPS) || !bytes.Equal(record.SPSnalus[0], avcSPS[0]) {
				t.Errorf("SPS = %x, want %x", record.SPSnalus, avcSPS)
			}
			if len(record.PPSnalus) != len(avcPPS) || !bytes.Equal(record.PPSnalus[0], avcPPS[0]) {
				t.Errorf("PPS = %x, want %x", record.PPSnalus, avcPPS)
			}
		},
	}, {
		name: "hevc states its decoder configuration record",
		cfg: TrackConfig{Kind: Video, Codec: "hev1", Timescale: 90000, Width: 1920, Height: 1080,
			VPS: hevcVPS, SPS: hevcSPS, PPS: hevcPPS},
		id:      "V_MPEGH/ISO/HEVC",
		docType: "matroska",
		check: func(t *testing.T, entry webm.TrackEntry) {
			record, err := hevc.DecodeHEVCDecConfRec(entry.CodecPrivate)
			if err != nil {
				t.Fatalf("the private data is not an HEVC record: %v", err)
			}
			for _, want := range []struct {
				naluType hevc.NaluType
				nalus    [][]byte
			}{
				{hevc.NALU_VPS, hevcVPS}, {hevc.NALU_SPS, hevcSPS}, {hevc.NALU_PPS, hevcPPS},
			} {
				got := record.GetNalusForType(want.naluType)
				if len(got) != 1 || !bytes.Equal(got[0], want.nalus[0]) {
					t.Errorf("%v = %x, want %x", want.naluType, got, want.nalus)
				}
			}
		},
	}, {
		name: "opus states its identification header and its delays",
		cfg: TrackConfig{Kind: Audio, Codec: "Opus", Timescale: 48000,
			Channels: 2, SampleRate: 48000, PreSkip: 312},
		id:      "A_OPUS",
		docType: "webm",
		check: func(t *testing.T, entry webm.TrackEntry) {
			want, err := OpusHead(TrackConfig{Channels: 2, SampleRate: 48000, PreSkip: 312})
			if err != nil {
				t.Fatalf("OpusHead: %v", err)
			}
			if !bytes.Equal(entry.CodecPrivate, want) {
				t.Errorf("private data = %x, want %x", entry.CodecPrivate, want)
			}
			// 312 samples at the 48 kHz a pre-skip is always counted at.
			if entry.CodecDelay != 6_500_000 {
				t.Errorf("codec delay = %d ns, want 6500000", entry.CodecDelay)
			}
			if entry.SeekPreRoll != 80_000_000 {
				t.Errorf("seek pre-roll = %d ns, want 80000000", entry.SeekPreRoll)
			}
			// An Opus decoder always outputs 48 kHz, whatever the track was
			// recorded at, and the mapping says the track states that.
			if entry.Audio == nil || entry.Audio.SamplingFrequency != 48000 ||
				entry.Audio.Channels != 2 {
				t.Errorf("audio = %+v", entry.Audio)
			}
		},
	}, {
		name: "opus from an identification header of its own",
		cfg: TrackConfig{Kind: Audio, Codec: "A_OPUS", Timescale: 48000,
			CodecConfig: opusHeader},
		id:      "A_OPUS",
		docType: "webm",
		check: func(t *testing.T, entry webm.TrackEntry) {
			if !bytes.Equal(entry.CodecPrivate, opusHeader) {
				t.Errorf("private data = %x, want the header handed over, %x",
					entry.CodecPrivate, opusHeader)
			}
			// 120 samples of pre-skip, and the header's own channel count.
			if entry.CodecDelay != 2_500_000 || entry.Audio.Channels != 1 {
				t.Errorf("delay = %d, channels = %d", entry.CodecDelay, entry.Audio.Channels)
			}
		},
	}, {
		name: "vorbis states its three setup headers",
		cfg: TrackConfig{Kind: Audio, Codec: "A_VORBIS", Timescale: 44100,
			Channels: 2, SampleRate: 44100, CodecConfig: vorbisHeaders},
		id:      "A_VORBIS",
		docType: "webm",
		check: func(t *testing.T, entry webm.TrackEntry) {
			if !bytes.Equal(entry.CodecPrivate, vorbisHeaders) {
				t.Errorf("private data = %x, want %x", entry.CodecPrivate, vorbisHeaders)
			}
			if entry.Audio == nil || entry.Audio.SamplingFrequency != 44100 ||
				entry.Audio.Channels != 2 {
				t.Errorf("audio = %+v", entry.Audio)
			}
		},
	}, {
		name: "aac states its audio specific configuration",
		cfg: TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000,
			Channels: 2, SampleRate: 48000},
		id:      "A_AAC",
		docType: "matroska",
		check: func(t *testing.T, entry webm.TrackEntry) {
			asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(entry.CodecPrivate))
			if err != nil {
				t.Fatalf("the private data is not an AudioSpecificConfig: %v", err)
			}
			if asc.ObjectType != aac.AAClc || asc.SamplingFrequency != 48000 ||
				asc.ChannelConfiguration != 2 {
				t.Errorf("configuration = %+v", asc)
			}
		},
	}, {
		// The suffixed id a Matroska file may spell reaches the same codec.
		name: "aac of a profile the caller chose",
		cfg: TrackConfig{Kind: Audio, Codec: "A_AAC/MPEG4/LC", Timescale: 48000,
			Channels: 2, SampleRate: 24000, AudioObjectType: aac.HEAACv1},
		id:      "A_AAC",
		docType: "matroska",
		check: func(t *testing.T, entry webm.TrackEntry) {
			asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(entry.CodecPrivate))
			if err != nil {
				t.Fatalf("the private data is not an AudioSpecificConfig: %v", err)
			}
			if asc.ObjectType != aac.HEAACv1 || asc.SamplingFrequency != 24000 {
				t.Errorf("configuration = %+v", asc)
			}
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := writeOneWebMTrack(t, tc.cfg, 3)
			f := readWebM(t, data)
			if f.Header.DocType != tc.docType {
				t.Errorf("doc type = %q, want %q", f.Header.DocType, tc.docType)
			}
			if len(f.Segment.Tracks.TrackEntry) != 1 {
				t.Fatalf("track entries = %+v", f.Segment.Tracks.TrackEntry)
			}
			entry := f.Segment.Tracks.TrackEntry[0]
			if entry.CodecID != tc.id {
				t.Errorf("codec id = %q, want %q", entry.CodecID, tc.id)
			}
			if entry.TrackNumber != 1 || entry.TrackUID != 1 {
				t.Errorf("track number/uid = %d/%d", entry.TrackNumber, entry.TrackUID)
			}
			tc.check(t, entry)
			if got := len(webmBlocks(f)); got != 3 {
				t.Errorf("blocks = %d, want 3", got)
			}
			// And the package's own demuxer agrees about what the file holds.
			file, err := Demux(data)
			if err != nil {
				t.Fatalf("Demux: %v", err)
			}
			if len(file.Tracks) != 1 || file.Tracks[0].Codec != tc.id {
				t.Fatalf("demuxed tracks = %+v", file.Tracks)
			}
			if file.Format != tc.docType {
				t.Errorf("demuxed format = %q, want %q", file.Format, tc.docType)
			}
			if file.Tracks[0].Language != "und" {
				t.Errorf("language = %q, want und", file.Tracks[0].Language)
			}
		})
	}
}

func TestWebMAcceptsBothSpellingsOfACodec(t *testing.T) {
	// The correspondence between a Matroska id and a sample entry name is the
	// Matroska reader's; this only has to meet it, suffixed ids and all.
	cases := map[string]string{
		"V_VP9":            "V_VP9",
		"v_vp9":            "V_VP9",
		"vp09":             "V_VP9",
		"  vp09  ":         "V_VP9",
		"V_MPEG4/ISO/AVC":  "V_MPEG4/ISO/AVC",
		"avc3":             "V_MPEG4/ISO/AVC",
		"V_MPEGH/ISO/HEVC": "V_MPEGH/ISO/HEVC",
		"hev1":             "V_MPEGH/ISO/HEVC",
		"V_AV1":            "V_AV1",
		"av01":             "V_AV1",
		"V_VP8":            "V_VP8",
		"vp08":             "V_VP8",
		"A_AAC":            "A_AAC",
		"A_AAC/MPEG4/LC":   "A_AAC",
		"mp4a":             "A_AAC",
		"A_VORBIS":         "A_VORBIS",
		"vorb":             "A_VORBIS",
		"Opus":             "A_OPUS",
		"A_OPUS":           "A_OPUS",
		// Read by this package, written by nothing in it.
		"A_FLAC":   "",
		"A_AC3":    "",
		"ec-3":     "",
		"V_THEORA": "",
		"":         "",
	}
	for spelling, want := range cases {
		codec, ok := webmCodecFor(spelling)
		if ok != (want != "") {
			t.Errorf("%q: accepted = %v, want %v", spelling, ok, want != "")
			continue
		}
		if codec.id != want {
			t.Errorf("%q names %q, want %q", spelling, codec.id, want)
		}
	}
}

func TestWebMDocumentTypeFollowsTheLeastPermittedTrack(t *testing.T) {
	// One track WebM does not allow makes the whole file a Matroska file: a
	// player entitled to refuse anything but VP8, VP9, AV1, Vorbis and Opus
	// must not be told this is WebM.
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	if _, err := m.AddTrack(webmVideoConfig()); err != nil {
		t.Fatalf("AddTrack(video): %v", err)
	}
	audio, err := m.AddTrack(TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000,
		Channels: 2, SampleRate: 48000})
	if err != nil {
		t.Fatalf("AddTrack(audio): %v", err)
	}
	if err := m.WriteSample(audio, Sample{Data: []byte{1, 2}, Duration: 1024, Sync: true}); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f := readWebM(t, buf.Bytes()); f.Header.DocType != "matroska" {
		t.Errorf("doc type = %q, want matroska", f.Header.DocType)
	}
	file, err := Demux(buf.Bytes())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if file.Format != "matroska" {
		t.Errorf("format = %q, want matroska", file.Format)
	}
}

func TestWebMAddTrackRefusesWhatItCannotDescribe(t *testing.T) {
	avcSPS, avcPPS := webmAVCSets(t)
	_, hevcSPS, hevcPPS := webmHEVCSets(t)
	cases := map[string]struct {
		cfg  TrackConfig
		want error
	}{
		"no timescale": {
			cfg:  TrackConfig{Kind: Video, Codec: "V_VP9", Width: 16, Height: 16},
			want: ErrTrackConfig,
		},
		"a codec nothing here can write": {
			cfg:  TrackConfig{Kind: Video, Codec: "V_THEORA", Timescale: 1000},
			want: ErrUnsupportedCodec,
		},
		"a codec this package reads but cannot write": {
			cfg:  TrackConfig{Kind: Audio, Codec: "A_FLAC", Timescale: 48000},
			want: ErrUnsupportedCodec,
		},
		"a kind that disagrees with the codec": {
			cfg:  TrackConfig{Kind: Audio, Codec: "V_VP9", Timescale: 1000, Width: 16, Height: 16},
			want: ErrTrackConfig,
		},
		"video without a frame size": {
			cfg:  TrackConfig{Kind: Video, Codec: "V_VP8", Timescale: 1000},
			want: ErrTrackConfig,
		},
		"video with no height": {
			cfg:  TrackConfig{Kind: Video, Codec: "V_VP8", Timescale: 1000, Width: 16},
			want: ErrTrackConfig,
		},
		"av1 without its configuration record": {
			cfg:  TrackConfig{Kind: Video, Codec: "V_AV1", Timescale: 1000, Width: 16, Height: 16},
			want: ErrTrackConfig,
		},
		"av1 with a record that is not one": {
			cfg: TrackConfig{Kind: Video, Codec: "V_AV1", Timescale: 1000, Width: 16, Height: 16,
				CodecConfig: []byte{0x81}},
			want: ErrTrackConfig,
		},
		"avc without parameter sets": {
			cfg:  TrackConfig{Kind: Video, Codec: "avc1", Timescale: 1000, Width: 16, Height: 16},
			want: ErrTrackConfig,
		},
		"avc without pps": {
			cfg: TrackConfig{Kind: Video, Codec: "V_MPEG4/ISO/AVC", Timescale: 1000,
				Width: 16, Height: 16, SPS: avcSPS},
			want: ErrTrackConfig,
		},
		"avc whose sps cannot be read": {
			cfg: TrackConfig{Kind: Video, Codec: "avc3", Timescale: 1000, Width: 16, Height: 16,
				SPS: [][]byte{{0x67, 0x42}}, PPS: avcPPS},
			want: ErrTrackConfig,
		},
		"hevc without vps": {
			cfg: TrackConfig{Kind: Video, Codec: "hvc1", Timescale: 1000, Width: 16, Height: 16,
				SPS: hevcSPS, PPS: hevcPPS},
			want: ErrTrackConfig,
		},
		"hevc whose sps cannot be read": {
			cfg: TrackConfig{Kind: Video, Codec: "V_MPEGH/ISO/HEVC", Timescale: 1000,
				Width: 16, Height: 16,
				VPS: hevcSPS, SPS: [][]byte{{0x42, 0x01}}, PPS: hevcPPS},
			want: ErrTrackConfig,
		},
		"opus without a channel count": {
			cfg:  TrackConfig{Kind: Audio, Codec: "A_OPUS", Timescale: 48000, SampleRate: 48000},
			want: ErrTrackConfig,
		},
		"opus with more channels than family zero maps": {
			cfg: TrackConfig{Kind: Audio, Codec: "Opus", Timescale: 48000,
				Channels: 6, SampleRate: 48000},
			want: ErrTrackConfig,
		},
		"vorbis without its setup headers": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_VORBIS", Timescale: 44100,
				Channels: 2, SampleRate: 44100},
			want: ErrTrackConfig,
		},
		"vorbis without a sample rate": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_VORBIS", Timescale: 44100, Channels: 2,
				CodecConfig: webmVorbisHeaders()},
			want: ErrTrackConfig,
		},
		"vorbis without a channel count": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_VORBIS", Timescale: 44100, SampleRate: 44100,
				CodecConfig: webmVorbisHeaders()},
			want: ErrTrackConfig,
		},
		"vorbis headers that are not laced": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_VORBIS", Timescale: 44100,
				Channels: 2, SampleRate: 44100, CodecConfig: []byte{0x02}},
			want: ErrTrackConfig,
		},
		"vorbis with two headers instead of three": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_VORBIS", Timescale: 44100,
				Channels: 2, SampleRate: 44100,
				CodecConfig: xiphLace([]byte{vorbisIdentificationPacket, 0x00}, []byte{0x03, 0x00})},
			want: ErrTrackConfig,
		},
		"vorbis whose first header is empty": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_VORBIS", Timescale: 44100,
				Channels: 2, SampleRate: 44100,
				CodecConfig: xiphLace(nil, nil, []byte{0x05, 0x00, 0x00})},
			want: ErrTrackConfig,
		},
		"vorbis whose first header is not the identification one": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_VORBIS", Timescale: 44100,
				Channels: 2, SampleRate: 44100,
				CodecConfig: xiphLace([]byte{0x03, 0x00}, []byte{0x01, 0x00}, []byte{0x05, 0x00})},
			want: ErrTrackConfig,
		},
		"aac without a sample rate": {
			cfg:  TrackConfig{Kind: Audio, Codec: "A_AAC", Timescale: 48000, Channels: 2},
			want: ErrTrackConfig,
		},
		"aac without a channel count": {
			cfg:  TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000},
			want: ErrTrackConfig,
		},
		"aac with more channels than a configuration states": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_AAC", Timescale: 48000,
				Channels: 9, SampleRate: 48000},
			want: ErrTrackConfig,
		},
		"aac of a profile nothing supports": {
			cfg: TrackConfig{Kind: Audio, Codec: "A_AAC", Timescale: 48000,
				Channels: 2, SampleRate: 48000, AudioObjectType: 3},
			want: ErrTrackConfig,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			id, err := NewWebMMuxer(&bytes.Buffer{}).AddTrack(tc.cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if id != 0 {
				t.Errorf("a refused track was given the number %d", id)
			}
		})
	}
}

func TestWebMRefusesRecordsTheEncodersCannotWrite(t *testing.T) {
	avcSPS, avcPPS := webmAVCSets(t)
	hevcVPS, hevcSPS, hevcPPS := webmHEVCSets(t)
	failure := errors.New("out of room")

	t.Run("avc", func(t *testing.T) {
		saved := encodeAVCRecord
		encodeAVCRecord = func(*avc.DecConfRec, io.Writer) error { return failure }
		defer func() { encodeAVCRecord = saved }()
		_, err := NewWebMMuxer(&bytes.Buffer{}).AddTrack(TrackConfig{
			Kind: Video, Codec: "avc1", Timescale: 1000, Width: 16, Height: 16,
			SPS: avcSPS, PPS: avcPPS,
		})
		if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), failure.Error()) {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("hevc", func(t *testing.T) {
		saved := encodeHEVCRecord
		encodeHEVCRecord = func(*hevc.DecConfRec, io.Writer) error { return failure }
		defer func() { encodeHEVCRecord = saved }()
		_, err := NewWebMMuxer(&bytes.Buffer{}).AddTrack(TrackConfig{
			Kind: Video, Codec: "hvc1", Timescale: 1000, Width: 16, Height: 16,
			VPS: hevcVPS, SPS: hevcSPS, PPS: hevcPPS,
		})
		if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), failure.Error()) {
			t.Errorf("error = %v", err)
		}
	})
}

// webmStubUnlacer is an unlacer that stops part way, which ebml-go's own never
// does but a caller of it has to survive.
type webmStubUnlacer struct{ err error }

func (u webmStubUnlacer) Read() ([]byte, error) { return nil, u.err }

func TestWebMRefusesVorbisHeadersThatStopShort(t *testing.T) {
	failure := errors.New("stopped part way")
	saved := unlaceVorbisHeaders
	unlaceVorbisHeaders = func(io.Reader, int64) (ebml.Unlacer, error) {
		return webmStubUnlacer{err: failure}, nil
	}
	defer func() { unlaceVorbisHeaders = saved }()
	_, err := NewWebMMuxer(&bytes.Buffer{}).AddTrack(TrackConfig{
		Kind: Audio, Codec: "A_VORBIS", Timescale: 44100, Channels: 2, SampleRate: 44100,
		CodecConfig: webmVorbisHeaders(),
	})
	if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), failure.Error()) {
		t.Errorf("error = %v", err)
	}
}

func TestWebMCloseReportsAHeaderItCannotWrite(t *testing.T) {
	// A track was declared and never fed, so Close is what writes the header,
	// and Close is what has to report the writer's failure.
	m := NewWebMMuxer(&webmShortWriter{limit: 0})
	if _, err := m.AddTrack(webmVideoConfig()); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	err := m.Close()
	if !errors.Is(err, errWebMShortWriter) || !strings.Contains(err.Error(), "write webm header") {
		t.Errorf("Close: %v", err)
	}
}

func TestWebMAddTrackRefusedOnceWritingBegan(t *testing.T) {
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	id, err := m.AddTrack(webmVideoConfig())
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 3003, Sync: true}); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	if _, err := m.AddTrack(webmAudioConfig()); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("AddTrack after the first sample: %v, want %v", err, ErrTrackConfig)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.AddTrack(webmAudioConfig()); !errors.Is(err, ErrClosed) {
		t.Errorf("AddTrack after Close: %v, want %v", err, ErrClosed)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 3003}); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteSample after Close: %v, want %v", err, ErrClosed)
	}
	if err := m.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close: %v, want %v", err, ErrClosed)
	}
}

func TestWebMWriteSampleRejections(t *testing.T) {
	sample := Sample{Data: []byte{1, 2, 3}, Duration: 3003, Sync: true}
	t.Run("no track", func(t *testing.T) {
		if err := NewWebMMuxer(&bytes.Buffer{}).WriteSample(1, sample); !errors.Is(err, ErrNoTracks) {
			t.Errorf("error = %v, want %v", err, ErrNoTracks)
		}
	})
	cases := map[string]struct {
		track  uint32
		sample Sample
		want   error
	}{
		"no data":             {track: 1, sample: Sample{Duration: 3003}, want: ErrSample},
		"no duration":         {track: 1, sample: Sample{Data: []byte{1}}, want: ErrSample},
		"a track nobody adde": {track: 7, sample: sample, want: ErrUnknownTrack},
		"a frame before the track starts": {
			track:  1,
			sample: Sample{Data: []byte{1}, Duration: 3003, CompositionOffset: -1},
			want:   ErrSample,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := NewWebMMuxer(&bytes.Buffer{})
			if _, err := m.AddTrack(webmVideoConfig()); err != nil {
				t.Fatalf("AddTrack: %v", err)
			}
			if err := m.WriteSample(tc.track, tc.sample); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestWebMCloseRefusesAFileThatNamesNothing(t *testing.T) {
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	if err := m.Close(); !errors.Is(err, ErrNoTracks) {
		t.Errorf("Close without a track: %v, want %v", err, ErrNoTracks)
	}
	if buf.Len() != 0 {
		t.Errorf("a file naming nothing was written anyway: %d bytes", buf.Len())
	}
	if _, err := m.AddTrack(webmVideoConfig()); !errors.Is(err, ErrClosed) {
		t.Errorf("AddTrack after a refused Close: %v, want %v", err, ErrClosed)
	}
}

func TestWebMCloseWritesTheTracksOfAFileWithoutSamples(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		t.Run(fmt.Sprintf("buffered=%v", buffered), func(t *testing.T) {
			var buf bytes.Buffer
			var opts []WebMOption
			if buffered {
				opts = append(opts, BufferedSegment())
			}
			m := NewWebMMuxer(&buf, opts...)
			if _, err := m.AddTrack(webmVideoConfig()); err != nil {
				t.Fatalf("AddTrack: %v", err)
			}
			if err := m.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			f := readWebM(t, buf.Bytes())
			if len(f.Segment.Tracks.TrackEntry) != 1 || len(f.Segment.Cluster) != 0 {
				t.Errorf("tracks = %+v, clusters = %+v",
					f.Segment.Tracks.TrackEntry, f.Segment.Cluster)
			}
			if f.Segment.Info.Duration != 0 {
				t.Errorf("duration = %v, want none", f.Segment.Info.Duration)
			}
			file, err := Demux(buf.Bytes())
			if err != nil {
				t.Fatalf("Demux: %v", err)
			}
			if len(file.Tracks) != 1 || file.Tracks[0].Codec != "V_VP9" {
				t.Errorf("demuxed tracks = %+v", file.Tracks)
			}
		})
	}
}

// webmShortWriter accepts a fixed number of bytes and fails from then on, which
// is how every write this muxer makes is made to fail in turn.
type webmShortWriter struct {
	written, limit int
}

var errWebMShortWriter = errors.New("no more room")

func (w *webmShortWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.limit {
		room := w.limit - w.written
		w.written = w.limit
		return room, errWebMShortWriter
	}
	w.written += len(p)
	return len(p), nil
}

func TestWebMWriteFailuresAreReported(t *testing.T) {
	// One run of the script tells how long a good file is; every prefix of it
	// is then offered as the room available, so every write the muxer makes is
	// the one that fails in some run.
	script := func(w io.Writer) error {
		m := NewWebMMuxer(w, ClusterDuration(50*time.Millisecond))
		video, err := m.AddTrack(webmVideoConfig())
		if err != nil {
			return err
		}
		for i := 0; i < 6; i++ {
			if err := m.WriteSample(video, Sample{
				Data: webmSampleData(video, i), Duration: webmVideoFrame, Sync: true,
			}); err != nil {
				return err
			}
		}
		return m.Close()
	}
	var good bytes.Buffer
	if err := script(&good); err != nil {
		t.Fatalf("the script itself fails: %v", err)
	}
	reported := map[string]bool{}
	for limit := 0; limit < good.Len(); limit++ {
		failure := script(&webmShortWriter{limit: limit})
		if failure == nil {
			t.Fatalf("a writer with room for %d of %d bytes reported nothing", limit, good.Len())
		}
		if !errors.Is(failure, errWebMShortWriter) {
			t.Fatalf("with room for %d bytes: %v", limit, failure)
		}
		switch {
		case strings.Contains(failure.Error(), "write webm header"):
			reported["header"] = true
		case strings.Contains(failure.Error(), "write cluster at"):
			reported["cluster"] = true
		case strings.Contains(failure.Error(), "write block group on track"):
			reported["block group"] = true
		case strings.Contains(failure.Error(), "write block on track"):
			reported["block"] = true
		default:
			t.Fatalf("with room for %d bytes, an unnamed failure: %v", limit, failure)
		}
	}
	for _, phase := range []string{"header", "cluster", "block", "block group"} {
		if !reported[phase] {
			t.Errorf("no run failed while writing the %s", phase)
		}
	}
}

func TestWebMBufferedWriteFailureIsReported(t *testing.T) {
	w := &webmShortWriter{limit: 4}
	m := NewWebMMuxer(w, BufferedSegment())
	id, err := m.AddTrack(webmVideoConfig())
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: webmVideoFrame, Sync: true}); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	err = m.Close()
	if !errors.Is(err, errWebMShortWriter) || !strings.Contains(err.Error(), "write webm segment") {
		t.Errorf("Close: %v", err)
	}
}

func TestWebMOptionsFallBackToDefaults(t *testing.T) {
	cases := []struct {
		name    string
		opts    []WebMOption
		cluster time.Duration
		tick    time.Duration
	}{
		{name: "nothing stated", cluster: DefaultClusterDuration, tick: DefaultTimestampScale},
		{name: "a cluster of no time", opts: []WebMOption{ClusterDuration(0)},
			cluster: DefaultClusterDuration, tick: DefaultTimestampScale},
		{name: "a cluster of negative time", opts: []WebMOption{ClusterDuration(-time.Second)},
			cluster: DefaultClusterDuration, tick: DefaultTimestampScale},
		{name: "a tick of no time", opts: []WebMOption{TimestampScale(0)},
			cluster: DefaultClusterDuration, tick: DefaultTimestampScale},
		{name: "a tick that does not divide a second",
			opts:    []WebMOption{TimestampScale(3 * time.Millisecond)},
			cluster: DefaultClusterDuration, tick: DefaultTimestampScale},
		{name: "a tick longer than a second", opts: []WebMOption{TimestampScale(2 * time.Second)},
			cluster: DefaultClusterDuration, tick: DefaultTimestampScale},
		{name: "both stated",
			opts:    []WebMOption{ClusterDuration(4 * time.Second), TimestampScale(time.Microsecond)},
			cluster: 4 * time.Second, tick: time.Microsecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWebMMuxer(&bytes.Buffer{}, tc.opts...)
			if m.settings.clusterDuration != tc.cluster || m.settings.tick != tc.tick {
				t.Errorf("settings = %+v, want a %v cluster of %v ticks",
					m.settings, tc.cluster, tc.tick)
			}
		})
	}
}

func TestWebMRefusesTimesTheScaleCannotState(t *testing.T) {
	// A track counting one unit per second, stated in nanosecond ticks, runs
	// out of range after a few frames of the longest duration a sample can
	// state — and is refused rather than wrapping around into the past.
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, TimestampScale(time.Nanosecond), BufferedSegment())
	id, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "V_VP8", Timescale: 1,
		Width: 16, Height: 16})
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	sample := Sample{Data: []byte{1}, Duration: math.MaxUint32, Sync: true}
	// A frame is refused as soon as either end of it is beyond range, so the
	// third one goes: it would start inside the range and finish outside.
	for i := 0; i < 2; i++ {
		if err := m.WriteSample(id, sample); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	err = m.WriteSample(id, sample)
	if !errors.Is(err, ErrSample) {
		t.Errorf("the sample beyond range: %v, want %v", err, ErrSample)
	}
	// Nothing out of range was accepted, so the file can be finished and the
	// duration it states is the one of what was written.
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestWebMMuxerTakesWhatTheReaderHandsBack(t *testing.T) {
	// The muxers are counterparts of the reader: a track read out of an MP4 is
	// handed over as it came, real coded frames and all, and lands in a
	// Matroska file no byte of the media has been touched in.
	source, err := os.ReadFile("testdata/tiny.mp4")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	r, err := NewReader(source)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, BufferedSegment())
	type pending struct {
		track     uint32
		timescale uint32
		time      uint64
		sample    Sample
	}
	var script []pending
	counts := map[uint32]int{}
	for _, id := range r.TrackIDs() {
		cfg, err := r.TrackConfig(id)
		if err != nil {
			t.Fatalf("TrackConfig(%d): %v", id, err)
		}
		track, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatalf("AddTrack(%s): %v", cfg.Codec, err)
		}
		samples, err := r.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		var at uint64
		for _, s := range samples {
			script = append(script, pending{
				track: track, timescale: cfg.Timescale, time: at, sample: s,
			})
			at += uint64(s.Duration)
		}
		counts[track] = len(samples)
	}
	// Interleaved by presentation time, which is how a caller feeding several
	// tracks keeps them in step.
	sort.SliceStable(script, func(i, j int) bool {
		return float64(script[i].time)/float64(script[i].timescale) <
			float64(script[j].time)/float64(script[j].timescale)
	})
	for i, p := range script {
		if err := m.WriteSample(p.track, p.sample); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	file, err := Demux(buf.Bytes())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if len(file.Tracks) != len(r.TrackIDs()) {
		t.Fatalf("tracks = %+v, want %d", file.Tracks, len(r.TrackIDs()))
	}
	// The fixture holds AVC, which WebM does not allow, so the file says
	// Matroska and states the Matroska codec id.
	if file.Format != "matroska" || file.Tracks[0].Codec != "V_MPEG4/ISO/AVC" {
		t.Errorf("file = %+v", file)
	}
	if file.Tracks[0].Width != r.File().Tracks[0].Width ||
		file.Tracks[0].Height != r.File().Tracks[0].Height {
		t.Errorf("frame size = %dx%d, want %dx%d", file.Tracks[0].Width, file.Tracks[0].Height,
			r.File().Tracks[0].Width, r.File().Tracks[0].Height)
	}

	f := readWebM(t, buf.Bytes())
	blocks := webmBlocks(f)
	if len(blocks) != len(script) {
		t.Fatalf("blocks = %d, want one per sample (%d)", len(blocks), len(script))
	}
	written := map[uint64]int{}
	for i, b := range blocks {
		written[b.track]++
		p := script[i]
		if b.track != uint64(p.track) {
			t.Errorf("block %d names track %d, want %d", i, b.track, p.track)
		}
		want := expectedTick(p.time+uint64(p.sample.CompositionOffset), uint64(p.timescale), 1000)
		if b.tick != want {
			t.Errorf("block %d at tick %d, want %d", i, b.tick, want)
		}
		if b.keyframe != p.sample.Sync {
			t.Errorf("block %d keyframe = %v, want %v", i, b.keyframe, p.sample.Sync)
		}
		// The media is copied, never re-encoded.
		if !bytes.Equal(b.data, p.sample.Data) {
			t.Errorf("block %d holds %d bytes, want the sample's %d", i, len(b.data),
				len(p.sample.Data))
		}
	}
	for track, count := range counts {
		if written[uint64(track)] != count {
			t.Errorf("track %d holds %d blocks, want %d", track, written[uint64(track)], count)
		}
	}
	// A tenth of a second either way: the fixture's duration, in ticks.
	if got := f.Segment.Info.Duration; math.Abs(got-r.File().DurationSeconds()*1000) > 100 {
		t.Errorf("duration = %v ticks, want about %v",
			got, r.File().DurationSeconds()*1000)
	}
}

func TestWebMMuxerTakesWhatTheMatroskaReaderHandsBack(t *testing.T) {
	// A Matroska file read and written again: the codec's private data goes
	// out as the parameter sets it came in as, and the samples are the same
	// bytes at the same times.
	source, err := os.ReadFile("testdata/tiny.mkv")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	r, err := NewReader(source)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	id := r.TrackIDs()[0]
	cfg, err := r.TrackConfig(id)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	samples, err := r.Samples(id)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, BufferedSegment())
	track, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack(%s): %v", cfg.Codec, err)
	}
	for i, s := range samples {
		if err := m.WriteSample(track, s); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read the result back the same way, and the track describes itself the
	// same way it did in the file it came from.
	again, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatalf("NewReader on what was written: %v", err)
	}
	back, err := again.TrackConfig(again.TrackIDs()[0])
	if err != nil {
		t.Fatalf("TrackConfig on what was written: %v", err)
	}
	if back.Codec != cfg.Codec || back.Width != cfg.Width || back.Height != cfg.Height {
		t.Errorf("configuration = %+v, want %+v", back, cfg)
	}
	if len(back.SPS) != len(cfg.SPS) || !bytes.Equal(back.SPS[0], cfg.SPS[0]) {
		t.Errorf("SPS = %x, want %x", back.SPS, cfg.SPS)
	}
	if len(back.PPS) != len(cfg.PPS) || !bytes.Equal(back.PPS[0], cfg.PPS[0]) {
		t.Errorf("PPS = %x, want %x", back.PPS, cfg.PPS)
	}
	written, err := again.Samples(again.TrackIDs()[0])
	if err != nil {
		t.Fatalf("Samples on what was written: %v", err)
	}
	if len(written) != len(samples) {
		t.Fatalf("samples = %d, want %d", len(written), len(samples))
	}
	for i := range samples {
		if !bytes.Equal(written[i].Data, samples[i].Data) {
			t.Errorf("sample %d holds %d bytes, want the %d it came with",
				i, len(written[i].Data), len(samples[i].Data))
		}
		if written[i].Sync != samples[i].Sync {
			t.Errorf("sample %d sync = %v, want %v", i, written[i].Sync, samples[i].Sync)
		}
	}
}
