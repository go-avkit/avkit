// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asticode/go-astits"
)

// The fixtures below are written with this package's own Muxer: an input whose
// every sample the test knows by heart is the only way to tell a copy from a
// re-encode.

// fixtureTrack is one track of a fixture: how it is declared, and what it holds.
type fixtureTrack struct {
	cfg     TrackConfig
	samples []Sample
}

func av1Track(timescale uint32, width, height int) TrackConfig {
	// The minimal av1C record: marker and version, then profile, level and
	// tier. AV1 needs no parameter set, which keeps a fixture to one line.
	return TrackConfig{Kind: Video, Codec: "av01", Timescale: timescale,
		Width: width, Height: height, CodecConfig: []byte{0x81, 0x00, 0x0c, 0x00}}
}

func aacTrack(sampleRate int) TrackConfig {
	return TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000,
		Channels: 2, SampleRate: sampleRate}
}

// marked builds n samples whose bytes name their track and their position, so
// a copy can be recognised sample by sample. Every gop-th one is a sync
// sample; a gop of zero leaves the track without one.
func marked(mark byte, n int, dur uint32, gop int) []Sample {
	out := make([]Sample, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Sample{
			Data:     []byte{mark, byte(i)},
			Duration: dur,
			Sync:     gop > 0 && i%gop == 0,
		})
	}
	return out
}

func muxedFixture(t *testing.T, tracks ...fixtureTrack) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	ids := make([]uint32, len(tracks))
	for i, tr := range tracks {
		id, err := m.AddTrack(tr.cfg)
		if err != nil {
			t.Fatalf("fixture AddTrack(%d): %v", i, err)
		}
		ids[i] = id
	}
	for i, tr := range tracks {
		for j, s := range tr.samples {
			if err := m.WriteSample(ids[i], s); err != nil {
				t.Fatalf("fixture WriteSample(%d, %d): %v", i, j, err)
			}
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("fixture Close: %v", err)
	}
	return buf.Bytes()
}

func fixtureReader(t *testing.T, tracks ...fixtureTrack) *Reader {
	t.Helper()
	r, err := NewReader(muxedFixture(t, tracks...))
	if err != nil {
		t.Fatalf("fixture reader: %v", err)
	}
	return r
}

// readBack reads an output the way a player would: its tracks in file order,
// with the samples of each. A track carrying nothing reads as no samples,
// which is a result in its own right rather than a failure.
func readBack(t *testing.T, data []byte) ([]TrackConfig, [][]Sample) {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("the output does not read back: %v", err)
	}
	var cfgs []TrackConfig
	var samples [][]Sample
	for _, id := range r.TrackIDs() {
		cfg, err := r.TrackConfig(id)
		if err != nil {
			t.Fatalf("TrackConfig(%d): %v", id, err)
		}
		got, err := r.Samples(id)
		if err != nil && !errors.Is(err, ErrNoSamples) {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		cfgs = append(cfgs, cfg)
		samples = append(samples, got)
	}
	return cfgs, samples
}

// assertSamples compares a copy against what it was copied from, byte for byte.
func assertSamples(t *testing.T, what string, got, want []Sample) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s holds %d samples, want %d", what, len(got), len(want))
	}
	for i := range want {
		switch {
		case !bytes.Equal(got[i].Data, want[i].Data):
			t.Errorf("%s sample %d = %v, want %v", what, i, got[i].Data, want[i].Data)
		case got[i].Duration != want[i].Duration:
			t.Errorf("%s sample %d lasts %d, want %d", what, i, got[i].Duration, want[i].Duration)
		case got[i].Sync != want[i].Sync:
			t.Errorf("%s sample %d sync = %v, want %v", what, i, got[i].Sync, want[i].Sync)
		case got[i].CompositionOffset != want[i].CompositionOffset:
			t.Errorf("%s sample %d composition offset = %d, want %d",
				what, i, got[i].CompositionOffset, want[i].CompositionOffset)
		}
	}
}

func TestRemuxCopiesEveryTrackAsItStood(t *testing.T) {
	video := marked('v', 8, 512, 4)
	audio := marked('a', 6, 1024, 1)
	// Frame reordering must survive the copy too.
	video[1].CompositionOffset = 512
	src := fixtureReader(t,
		fixtureTrack{av1Track(12800, 640, 360), video},
		fixtureTrack{aacTrack(48000), audio})

	var out bytes.Buffer
	if err := Remux(&out, src); err != nil {
		t.Fatalf("Remux: %v", err)
	}
	cfgs, samples := readBack(t, out.Bytes())
	if len(cfgs) != 2 {
		t.Fatalf("the copy holds %d tracks, want 2", len(cfgs))
	}
	if cfgs[0].Codec != "av01" || cfgs[0].Timescale != 12800 ||
		cfgs[0].Width != 640 || cfgs[0].Height != 360 {
		t.Errorf("video track = %+v", cfgs[0])
	}
	if cfgs[1].Codec != "mp4a" || cfgs[1].SampleRate != 48000 {
		t.Errorf("audio track = %+v", cfgs[1])
	}
	assertSamples(t, "video", samples[0], video)
	assertSamples(t, "audio", samples[1], audio)
}

func TestRemuxInterleavesTracksByTime(t *testing.T) {
	// Both tracks run for about the same time: 8 video samples of 40ms and
	// 15 audio samples of 21.3ms.
	video := marked('v', 8, 512, 1)
	audio := marked('a', 15, 1024, 1)
	src := fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), video},
		fixtureTrack{aacTrack(48000), audio})

	var out bytes.Buffer
	// One fragment per video sample, so what lands in each one is visible.
	if err := Remux(&out, src, MuxOptions(FragmentDuration(40*time.Millisecond))); err != nil {
		t.Fatalf("Remux: %v", err)
	}
	if got := countBoxes(t, out.Bytes(), "moof"); got != 8 {
		t.Fatalf("moof boxes = %d, want one per video sample", got)
	}
	// A copy that wrote one track after the other would leave every audio
	// sample in the last fragment. Interleaved, each fragment carries the
	// audio that plays alongside its video.
	for i, frag := range fragmentSampleCounts(t, out.Bytes()) {
		for id, n := range frag {
			if n == 0 {
				t.Errorf("fragment %d carries nothing for track %d", i+1, id)
			}
		}
	}
	_, samples := readBack(t, out.Bytes())
	assertSamples(t, "video", samples[0], video)
	assertSamples(t, "audio", samples[1], audio)
}

// fragmentSampleCounts reports, per fragment, how many samples each track got.
func fragmentSampleCounts(t *testing.T, data []byte) []map[uint32]int {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var out []map[uint32]int
	for _, seg := range r.mp4.Segments {
		for _, frag := range seg.Fragments {
			counts := map[uint32]int{}
			for _, traf := range frag.Moof.Trafs {
				counts[traf.Tfhd.TrackID] = len(traf.Trun.Samples)
			}
			out = append(out, counts)
		}
	}
	if len(out) == 0 {
		t.Fatal("the output holds no fragment")
	}
	return out
}

func TestRemuxPassesMuxOptionsOn(t *testing.T) {
	src := fixtureReader(t, fixtureTrack{av1Track(12800, 64, 48), marked('v', 2, 512, 1)})
	var out bytes.Buffer
	if err := Remux(&out, src, MuxOptions(Brand("iso6"))); err != nil {
		t.Fatalf("Remux: %v", err)
	}
	if !bytes.Contains(out.Bytes()[:32], []byte("iso6")) {
		t.Errorf("the chosen brand did not reach the output: %q", out.Bytes()[:32])
	}
}

func TestEveryOperationNeedsAnInput(t *testing.T) {
	if err := Remux(io.Discard, nil); !errors.Is(err, ErrNoTracks) {
		t.Errorf("Remux: err = %v, want ErrNoTracks", err)
	}
	if err := Cut(io.Discard, nil, time.Second, 2*time.Second); !errors.Is(err, ErrNoTracks) {
		t.Errorf("Cut: err = %v, want ErrNoTracks", err)
	}
	if err := Concat(io.Discard, nil); !errors.Is(err, ErrNoTracks) {
		t.Errorf("Concat: err = %v, want ErrNoTracks", err)
	}
}

func TestDropTracksLeavesTheRest(t *testing.T) {
	video := marked('v', 4, 512, 1)
	src := fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), video},
		fixtureTrack{aacTrack(48000), marked('a', 4, 1024, 1)})
	// The fixture's tracks are 1 and 2, in the order they were added.
	if ids := src.TrackIDs(); len(ids) != 2 || ids[1] != 2 {
		t.Fatalf("the fixture does not have the tracks this test drops: %v", ids)
	}

	var out bytes.Buffer
	if err := Remux(&out, src, DropTracks(2)); err != nil {
		t.Fatalf("Remux: %v", err)
	}
	cfgs, samples := readBack(t, out.Bytes())
	if len(cfgs) != 1 || cfgs[0].Kind != Video {
		t.Fatalf("the copy holds %+v, want the video track alone", cfgs)
	}
	assertSamples(t, "video", samples[0], video)
}

func TestDropTracksRefusesAnUnknownTrack(t *testing.T) {
	src := fixtureReader(t, fixtureTrack{av1Track(12800, 64, 48), marked('v', 2, 512, 1)})
	err := Remux(io.Discard, src, DropTracks(99))
	if !errors.Is(err, ErrUnknownTrack) {
		t.Fatalf("err = %v, want ErrUnknownTrack", err)
	}
}

func TestDropTracksRefusesDroppingEveryTrack(t *testing.T) {
	src := fixtureReader(t, fixtureTrack{av1Track(12800, 64, 48), marked('v', 2, 512, 1)})
	var out bytes.Buffer
	if err := Remux(&out, src, DropTracks(1)); !errors.Is(err, ErrNoTracks) {
		t.Fatalf("err = %v, want ErrNoTracks", err)
	}
	if out.Len() != 0 {
		t.Errorf("a file with no track was written all the same: %d bytes", out.Len())
	}
}

func TestRemuxRefusesATrackTheMuxerCannotDeclare(t *testing.T) {
	// A transport stream joined mid-broadcast: its pictures arrive without
	// the parameter sets an MP4 sample entry has to name.
	var ts bytes.Buffer
	m := astits.NewMuxer(context.Background(), &ts)
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 256, StreamType: astits.StreamTypeH264Video,
	}); err != nil {
		t.Fatal(err)
	}
	m.SetPCRPID(256)
	for i := 0; i < 2; i++ {
		if _, err := m.WriteData(&astits.MuxerData{PID: 256, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xE0, OptionalHeader: &astits.PESOptionalHeader{
				MarkerBits:      2,
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
				PTS:             &astits.ClockReference{Base: int64(i) * 3600},
			}},
			Data: annexB([]byte{0x65, 0x88, 0x84, 0x21}),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	src, err := NewReader(ts.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The precondition this test rests on: the samples are there, only the
	// configuration is not.
	if samples, err := src.Samples(256); err != nil || len(samples) != 2 {
		t.Fatalf("samples = %d, %v", len(samples), err)
	}
	if cfg, err := src.TrackConfig(256); err != nil || len(cfg.SPS) != 0 {
		t.Fatalf("the stream was expected to carry no SPS: %+v, %v", cfg, err)
	}
	if err := Remux(io.Discard, src); !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("err = %v, want ErrTrackConfig", err)
	}
}

func TestRemuxRefusesATrackWithoutSamples(t *testing.T) {
	// A track that was declared and then never written to: the reader cannot
	// tell that from a sample table it failed to read, so a copy refuses.
	src := fixtureReader(t, fixtureTrack{aacTrack(48000), nil})
	if err := Remux(io.Discard, src); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("err = %v, want ErrNoSamples", err)
	}
}

func TestRemuxReportsAWriteFailure(t *testing.T) {
	src := fixtureReader(t, fixtureTrack{av1Track(12800, 64, 48), marked('v', 2, 512, 1)})
	if err := Remux(&failWriter{}, src); err == nil {
		t.Fatal("a failing writer was ignored")
	}
}

func TestRemuxReportsAConfigurationItCannotRead(t *testing.T) {
	original := trackConfig
	defer func() { trackConfig = original }()
	trackConfig = func(*Reader, uint32) (TrackConfig, error) {
		return TrackConfig{}, errors.New("unreadable")
	}
	src := fixtureReader(t, fixtureTrack{av1Track(12800, 64, 48), marked('v', 2, 512, 1)})
	if err := Remux(io.Discard, src); err == nil {
		t.Fatal("a track that cannot be described must be reported")
	}
}

// cutFixture is an eight-picture video with a sync sample every four — one
// group of pictures lasting 160ms — alongside audio whose every frame is a
// sync sample, as AAC's are.
func cutFixture(t *testing.T, audioSamples int) (*Reader, []Sample, []Sample) {
	t.Helper()
	video := marked('v', 8, 512, 4)             // 40ms each at 12800
	audio := marked('a', audioSamples, 1024, 1) // 21.33ms each at 48000
	return fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), video},
		fixtureTrack{aacTrack(48000), audio}), video, audio
}

func TestCutSnapsBackToThePrecedingSyncSample(t *testing.T) {
	src, video, audio := cutFixture(t, 12)
	// The precondition: 200ms falls inside video sample 5, which is not a
	// sync sample, and the last one before it opens the group at 160ms.
	if video[5].Sync || !video[4].Sync {
		t.Fatalf("the fixture's sync grid is not what this test needs: %+v", video)
	}

	var out bytes.Buffer
	if err := Cut(&out, src, 200*time.Millisecond, 0); err != nil {
		t.Fatalf("Cut: %v", err)
	}
	_, samples := readBack(t, out.Bytes())
	assertSamples(t, "video", samples[0], video[4:])
	// The audio snaps to its own last frame at or before 160ms, which is the
	// eighth: 7 * 1024 / 48000 = 149ms, the next one starting at 170ms.
	assertSamples(t, "audio", samples[1], audio[7:])
}

func TestCutStopsBeforeTheEnd(t *testing.T) {
	src, video, audio := cutFixture(t, 12)
	var out bytes.Buffer
	if err := Cut(&out, src, 0, 160*time.Millisecond); err != nil {
		t.Fatalf("Cut: %v", err)
	}
	_, samples := readBack(t, out.Bytes())
	// Video sample 4 starts exactly at 160ms and is left out; audio frame 8
	// starts at 170ms, so eight frames are kept.
	assertSamples(t, "video", samples[0], video[:4])
	assertSamples(t, "audio", samples[1], audio[:8])
}

func TestCutRequireSyncStartRefusesBetweenSyncSamples(t *testing.T) {
	src, _, _ := cutFixture(t, 12)
	err := Cut(io.Discard, src, 200*time.Millisecond, 0, RequireSyncStart())
	if !errors.Is(err, ErrNoSyncSample) {
		t.Fatalf("err = %v, want ErrNoSyncSample", err)
	}
	// The message must say where a cut would have worked.
	if !strings.Contains(err.Error(), "0.160s") {
		t.Errorf("the message does not point at the nearest sync sample: %v", err)
	}
	// On a sync sample it goes through, and starts exactly there.
	onGrid, video, _ := cutFixture(t, 12)
	var out bytes.Buffer
	if err := Cut(&out, onGrid, 160*time.Millisecond, 0, RequireSyncStart()); err != nil {
		t.Fatalf("Cut on a sync sample: %v", err)
	}
	_, samples := readBack(t, out.Bytes())
	assertSamples(t, "video", samples[0], video[4:])
}

func TestCutSnapsForwardWhenNoSyncSamplePrecedes(t *testing.T) {
	// A file whose first pictures depend on a later one: nothing can be
	// decoded before the sample at 120ms, so that is where a cut at zero
	// begins.
	video := marked('v', 6, 512, 3)
	video[0].Sync = false
	src := fixtureReader(t, fixtureTrack{av1Track(12800, 320, 180), video})

	var out bytes.Buffer
	if err := Cut(&out, src, 0, 0); err != nil {
		t.Fatalf("Cut: %v", err)
	}
	_, samples := readBack(t, out.Bytes())
	assertSamples(t, "video", samples[0], video[3:])
}

func TestCutThatSelectsNothingStillNamesItsTracks(t *testing.T) {
	video := marked('v', 6, 512, 3)
	video[0].Sync = false
	src := fixtureReader(t, fixtureTrack{av1Track(12800, 320, 180), video})

	var out bytes.Buffer
	// The cut ends before the first decodable sample, at 120ms, begins.
	if err := Cut(&out, src, 0, 40*time.Millisecond); err != nil {
		t.Fatalf("Cut: %v", err)
	}
	cfgs, samples := readBack(t, out.Bytes())
	if len(cfgs) != 1 || cfgs[0].Codec != "av01" {
		t.Fatalf("the track must still be named: %+v", cfgs)
	}
	if len(samples[0]) != 0 {
		t.Errorf("samples = %+v, want none", samples[0])
	}
}

func TestCutRejectsAnImpossibleRange(t *testing.T) {
	src, _, _ := cutFixture(t, 4)
	for _, c := range []struct {
		name       string
		start, end time.Duration
	}{
		{"a negative start", -time.Second, 0},
		{"an end before the start", 2 * time.Second, time.Second},
		{"an empty range", time.Second, time.Second},
	} {
		if err := Cut(io.Discard, src, c.start, c.end); !errors.Is(err, ErrTimeRange) {
			t.Errorf("%s: err = %v, want ErrTimeRange", c.name, err)
		}
	}
}

func TestCutWithAnEndBeyondAnyDurationKeepsEverything(t *testing.T) {
	src, video, audio := cutFixture(t, 4)
	var out bytes.Buffer
	// An end this large overflows any common unit the two timescales could
	// share, which is why times are compared as rationals.
	if err := Cut(&out, src, 0, time.Duration(math.MaxInt64)); err != nil {
		t.Fatalf("Cut: %v", err)
	}
	_, samples := readBack(t, out.Bytes())
	assertSamples(t, "video", samples[0], video)
	assertSamples(t, "audio", samples[1], audio)
}

func TestCutOfAudioAloneUsesItsOwnGrid(t *testing.T) {
	audio := marked('a', 12, 1024, 1)
	src := fixtureReader(t, fixtureTrack{aacTrack(48000), audio})
	var out bytes.Buffer
	// Every frame is a sync sample, so the cut lands on the frame containing
	// 100ms: 4 * 1024 / 48000 = 85ms, the next starting at 107ms.
	if err := Cut(&out, src, 100*time.Millisecond, 0); err != nil {
		t.Fatalf("Cut: %v", err)
	}
	_, samples := readBack(t, out.Bytes())
	assertSamples(t, "audio", samples[0], audio[4:])
}

func TestCutRefusesATrackWithoutASyncSample(t *testing.T) {
	// The track a cut is placed on, and then one that merely comes along:
	// neither can start anywhere a decoder would follow.
	primary := fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), marked('v', 4, 512, 0)})
	if err := Cut(io.Discard, primary, 0, 0); !errors.Is(err, ErrNoSyncSample) {
		t.Fatalf("video without a sync sample: %v", err)
	}
	secondary := fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), marked('v', 4, 512, 2)},
		fixtureTrack{aacTrack(48000), marked('a', 4, 1024, 0)})
	if err := Cut(io.Discard, secondary, 0, 0); !errors.Is(err, ErrNoSyncSample) {
		t.Fatalf("audio without a sync sample: %v", err)
	}
}

func TestConcatAppendsTheSecondInputAfterTheFirst(t *testing.T) {
	firstVideo, firstAudio := marked('v', 4, 512, 2), marked('a', 6, 1024, 1)
	secondVideo, secondAudio := marked('w', 4, 512, 2), marked('b', 6, 1024, 1)
	first := fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), firstVideo},
		fixtureTrack{aacTrack(48000), firstAudio})
	second := fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), secondVideo},
		fixtureTrack{aacTrack(48000), secondAudio})

	var out bytes.Buffer
	// A short fragment window, so the decode time each fragment starts at is
	// there to be read.
	if err := Concat(&out, []*Reader{first, second},
		MuxOptions(FragmentDuration(40*time.Millisecond))); err != nil {
		t.Fatalf("Concat: %v", err)
	}
	cfgs, samples := readBack(t, out.Bytes())
	if len(cfgs) != 2 {
		t.Fatalf("the join holds %d tracks, want 2", len(cfgs))
	}
	// The second input's samples must follow the first's, in that order, and
	// nothing may be dropped or reordered.
	assertSamples(t, "video", samples[0], append(append([]Sample{}, firstVideo...), secondVideo...))
	assertSamples(t, "audio", samples[1], append(append([]Sample{}, firstAudio...), secondAudio...))

	// And they must follow it on the timeline too, not sit on top of it: the
	// second input's first fragment begins where the first input ended, at
	// four video samples of 512.
	starts := fragmentStarts(t, out.Bytes(), 1)
	if want := []uint64{0, 1024, 2048, 3072}; !slices.Equal(starts, want) {
		t.Errorf("video fragments begin at %v, want %v", starts, want)
	}
}

// fragmentStarts reports the decode time each fragment of a track begins at,
// as the file itself states it.
func fragmentStarts(t *testing.T, data []byte, trackID uint32) []uint64 {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var out []uint64
	for _, seg := range r.mp4.Segments {
		for _, frag := range seg.Fragments {
			for _, traf := range frag.Moof.Trafs {
				if traf.Tfhd.TrackID == trackID {
					out = append(out, traf.Tfdt.BaseMediaDecodeTime())
				}
			}
		}
	}
	return out
}

func TestConcatDropsTracksFromEveryInput(t *testing.T) {
	firstVideo, secondVideo := marked('v', 4, 512, 2), marked('w', 4, 512, 2)
	first := fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), firstVideo},
		fixtureTrack{aacTrack(48000), marked('a', 4, 1024, 1)})
	second := fixtureReader(t,
		fixtureTrack{av1Track(12800, 320, 180), secondVideo},
		fixtureTrack{aacTrack(48000), marked('b', 4, 1024, 1)})

	var out bytes.Buffer
	if err := Concat(&out, []*Reader{first, second}, DropTracks(2)); err != nil {
		t.Fatalf("Concat: %v", err)
	}
	cfgs, samples := readBack(t, out.Bytes())
	if len(cfgs) != 1 {
		t.Fatalf("the join holds %d tracks, want the video alone", len(cfgs))
	}
	assertSamples(t, "video", samples[0], append(append([]Sample{}, firstVideo...), secondVideo...))
}

func TestConcatRefusesInputsThatDoNotMatch(t *testing.T) {
	video := marked('v', 4, 512, 2)
	audio := marked('a', 4, 1024, 1)
	otherConfig := av1Track(12800, 320, 180)
	otherConfig.CodecConfig = []byte{0x81, 0x00, 0x0d, 0x00}
	heAAC := aacTrack(48000)
	heAAC.AudioObjectType = 5

	cases := []struct {
		name  string
		first []fixtureTrack
		then  []fixtureTrack
		says  string
	}{
		{"another codec",
			[]fixtureTrack{{av1Track(12800, 320, 180), video}},
			[]fixtureTrack{{videoConfig(t), video}},
			"codec"},
		{"another timescale",
			[]fixtureTrack{{av1Track(12800, 320, 180), video}},
			[]fixtureTrack{{av1Track(25600, 320, 180), video}},
			"timescale"},
		{"another frame size",
			[]fixtureTrack{{av1Track(12800, 320, 180), video}},
			[]fixtureTrack{{av1Track(12800, 640, 360), video}},
			"frame size"},
		{"another sample rate",
			[]fixtureTrack{{aacTrack(48000), audio}},
			[]fixtureTrack{{aacTrack(44100), audio}},
			"audio"},
		{"another AAC profile",
			[]fixtureTrack{{aacTrack(48000), audio}},
			[]fixtureTrack{{heAAC, audio}},
			"AAC profile"},
		{"another configuration record",
			[]fixtureTrack{{av1Track(12800, 320, 180), video}},
			[]fixtureTrack{{otherConfig, video}},
			"parameter sets"},
		{"another track count",
			[]fixtureTrack{{av1Track(12800, 320, 180), video}, {aacTrack(48000), audio}},
			[]fixtureTrack{{av1Track(12800, 320, 180), video}},
			"track(s)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := Concat(&out, []*Reader{fixtureReader(t, c.first...), fixtureReader(t, c.then...)})
			if !errors.Is(err, ErrTrackMismatch) {
				t.Fatalf("err = %v, want ErrTrackMismatch", err)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("the message does not say what differs: %v", err)
			}
			if out.Len() != 0 {
				t.Errorf("a file was written anyway: %d bytes", out.Len())
			}
		})
	}
}

func TestConcatNamesTheInputItCouldNotRead(t *testing.T) {
	first := fixtureReader(t, fixtureTrack{av1Track(12800, 64, 48), marked('v', 2, 512, 1)})
	err := Concat(io.Discard, []*Reader{first, nil})
	if !errors.Is(err, ErrNoTracks) {
		t.Fatalf("err = %v, want ErrNoTracks", err)
	}
	if !strings.Contains(err.Error(), "input 2") {
		t.Errorf("the message does not name the input at fault: %v", err)
	}
}

func TestCompareTimesOrdersAcrossTimescales(t *testing.T) {
	// The same instant in two timescales compares equal, whichever way round.
	if got := compareTimes(512, 12800, 1920, 48000); got != 0 {
		t.Errorf("40ms against 40ms = %d, want 0", got)
	}
	if got := compareTimes(512, 12800, 1921, 48000); got != -1 {
		t.Errorf("40ms against 40.02ms = %d, want -1", got)
	}
	// A time no common unit could express without overflowing still orders.
	if got := compareTimes(math.MaxUint64, 12800, 1, 48000); got != 1 {
		t.Errorf("an enormous time against a small one = %d, want 1", got)
	}
}
