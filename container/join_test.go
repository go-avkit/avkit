// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// readerOf is one input of a join: a file written here, read back as a caller
// would hand it over.
func readerOf(t *testing.T, data []byte) *Reader {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}

func videoInput(t *testing.T) []byte {
	t.Helper()
	return muxedFixture(t, fixtureTrack{cfg: av1Track(30000, 320, 180),
		samples: marked(0xA1, 8, 1000, 4)})
}

// distinct builds samples long enough to be found in a file without matching
// something else by chance: two bytes of marker occur everywhere.
func distinct(mark byte, n int, dur uint32) []Sample {
	out := make([]Sample, 0, n)
	for i := 0; i < n; i++ {
		data := make([]byte, 24)
		for j := range data {
			data[j] = mark
		}
		data[1], data[2] = byte(i), mark^byte(i)
		out = append(out, Sample{Data: data, Duration: dur, Sync: true})
	}
	return out
}

func audioInput(t *testing.T) []byte {
	t.Helper()
	return muxedFixture(t, fixtureTrack{cfg: aacTrack(48000),
		samples: marked(0xB2, 8, 1024, 1)})
}

// TestJoinPutsTracksSideBySide is what a stream delivered as separate
// representations needs: the picture of one input and the sound of another,
// written as one film.
func TestJoinPutsTracksSideBySide(t *testing.T) {
	for _, tc := range []struct {
		name  string
		join  func(*bytes.Buffer, []*Reader) error
		moofs bool
	}{
		{"fragmented", func(b *bytes.Buffer, srcs []*Reader) error { return Join(b, srcs) }, true},
		{"progressive", func(b *bytes.Buffer, srcs []*Reader) error { return JoinProgressive(b, srcs) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			srcs := []*Reader{readerOf(t, videoInput(t)), readerOf(t, audioInput(t))}
			if err := tc.join(&out, srcs); err != nil {
				t.Fatalf("join: %v", err)
			}
			held, err := HoldsFragments(bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatalf("HoldsFragments: %v", err)
			}
			if held != tc.moofs {
				t.Fatalf("the file holds fragments = %v, want %v", held, tc.moofs)
			}
			r := readerOf(t, out.Bytes())
			ids := r.TrackIDs()
			if len(ids) != 2 {
				t.Fatalf("the join holds %d tracks, want the picture and the sound", len(ids))
			}
			for i, id := range ids {
				samples, err := r.Samples(id)
				if err != nil {
					t.Fatalf("Samples(%d): %v", id, err)
				}
				if len(samples) != 8 {
					t.Fatalf("track %d holds %d samples, want 8", id, len(samples))
				}
				mark := byte(0xA1)
				if i == 1 {
					mark = 0xB2
				}
				for j, s := range samples {
					if len(s.Data) < 2 || s.Data[0] != mark || s.Data[1] != byte(j) {
						t.Fatalf("track %d sample %d = % x, want the one written", id, j, s.Data)
					}
				}
			}
		})
	}
}

// recordingMuxer notes the order samples are written in, which is what tells an
// interleaved join from one that writes a whole track before the next.
type recordingMuxer struct {
	order []byte // the first byte of each sample, in the order written
	clock map[uint32]int
}

func (m *recordingMuxer) AddTrack(TrackConfig) (uint32, error) {
	if m.clock == nil {
		m.clock = map[uint32]int{}
	}
	id := uint32(len(m.clock) + 1)
	m.clock[id] = 0
	return id, nil
}

func (m *recordingMuxer) WriteSample(id uint32, s Sample) error {
	m.order = append(m.order, s.Data[0])
	return nil
}

// TestJoinInterleavesByTime checks the samples are written in the order a
// player reads them, not one whole track and then the other: a file that made a
// player seek its whole length to hear it would not be a joined file. The order
// joinInto writes in is what decides that; how a particular muxer then lays
// those writes out on disk is the muxer's business, tested where it lives.
func TestJoinInterleavesByTime(t *testing.T) {
	// Video at 4 samples per second, audio at 8: the reader a join builds
	// should alternate roughly one picture to two sounds, never all of one
	// then all of the other.
	// Real timescales, real durations: a quarter-second per picture, an
	// eighth per sound, so the two timelines advance at rates a join has to
	// weave together.
	video := muxedFixture(t, fixtureTrack{cfg: av1Track(30000, 320, 180),
		samples: distinct(0xA1, 4, 7500)}) // 0.25 s each
	audio := muxedFixture(t, fixtureTrack{cfg: aacTrack(48000),
		samples: distinct(0xB2, 8, 6000)}) // 0.125 s each
	m := &recordingMuxer{}
	if err := joinInto(m, []*Reader{readerOf(t, video), readerOf(t, audio)}); err != nil {
		t.Fatalf("joinInto: %v", err)
	}
	if len(m.order) != 12 {
		t.Fatalf("wrote %d samples, want the 12 across both tracks", len(m.order))
	}
	// Neither track is written as one unbroken run: the sound is woven
	// through the picture rather than tacked on after it.
	longestRun := func(mark byte) int {
		best, run := 0, 0
		for _, b := range m.order {
			if b == mark {
				run++
				if run > best {
					best = run
				}
			} else {
				run = 0
			}
		}
		return best
	}
	if r := longestRun(0xB2); r >= 8 {
		t.Fatalf("all %d sounds were written in one run: not interleaved", r)
	}
	if r := longestRun(0xA1); r >= 4 {
		t.Fatalf("all %d pictures were written in one run: not interleaved", r)
	}
}

// TestJoinRefusesWhatCannotBeJoined covers every way a join has nothing to do.
func TestJoinRefusesWhatCannotBeJoined(t *testing.T) {
	var out bytes.Buffer
	if err := Join(&out, nil); !errors.Is(err, ErrNoTracks) {
		t.Errorf("no input: %v", err)
	}
	if err := JoinProgressive(&out, nil); !errors.Is(err, ErrNoTracks) {
		t.Errorf("no input, progressive: %v", err)
	}
	// Every track dropped leaves nothing to write.
	src := readerOf(t, videoInput(t))
	if err := Join(&out, []*Reader{src}, DropTracks(src.TrackIDs()...)); !errors.Is(err, ErrNoTracks) {
		t.Errorf("every track dropped: %v", err)
	}
}

// failingMuxer stands in for a muxer that refuses what it is given, which is
// how a track the output cannot carry, or a write that cannot land, is reported
// rather than silently dropped.
type failingMuxer struct {
	failAdd   bool
	failWrite bool
	added     int
}

func (f *failingMuxer) AddTrack(TrackConfig) (uint32, error) {
	if f.failAdd {
		return 0, errors.New("staged refusal")
	}
	f.added++
	return uint32(f.added), nil
}

func (f *failingMuxer) WriteSample(uint32, Sample) error {
	if f.failWrite {
		return errors.New("staged refusal")
	}
	return nil
}

func TestJoinReportsAMuxerThatRefuses(t *testing.T) {
	srcs := []*Reader{readerOf(t, videoInput(t))}
	if err := joinInto(&failingMuxer{failAdd: true}, srcs); err == nil {
		t.Error("a track the muxer refused was passed over")
	}
	if err := joinInto(&failingMuxer{failWrite: true}, srcs); err == nil {
		t.Error("a sample that could not be written was passed over")
	}
}

func TestHoldsFragments(t *testing.T) {
	box := func(name string, size uint32, extra ...byte) []byte {
		b := make([]byte, 8)
		b[0], b[1], b[2], b[3] = byte(size>>24), byte(size>>16), byte(size>>8), byte(size)
		copy(b[4:], name)
		return append(b, extra...)
	}
	big := func(name string, size uint64) []byte {
		b := box(name, 1)
		s := make([]byte, 8)
		for i := 0; i < 8; i++ {
			s[i] = byte(size >> (56 - 8*i))
		}
		return append(b, s...)
	}
	cases := []struct {
		name string
		data []byte
		want bool
		fail bool
	}{
		{"fragments follow the moov", append(box("ftyp", 8), append(box("moov", 8), box("moof", 8)...)...), true, false},
		{"media at the top level", append(box("ftyp", 8), append(box("moov", 8), box("mdat", 8)...)...), false, false},
		{"a 64-bit size is followed", append(big("free", 24), append(make([]byte, 8), box("moof", 8)...)...), true, false},
		{"a box running to the end", append(box("ftyp", 8), box("free", 0)...), false, false},
		{"a file ending on a boundary", box("ftyp", 8), false, false},
		{"not a container at all", []byte("nothing here"), false, false},
		{"a 64-bit size it does not carry", box("free", 1, 0, 0), false, true},
		{"a box smaller than its header", box("free", 4), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := HoldsFragments(bytes.NewReader(tc.data))
			if tc.fail {
				if err == nil {
					t.Fatalf("HoldsFragments = %v, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("HoldsFragments: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HoldsFragments = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := HoldsFragments(nil); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("nothing to read: %v", err)
	}
	if _, err := HoldsFragments(&failingSource{src: bytes.NewReader(videoInput(t)), failReads: true}); err == nil {
		t.Error("a file that answers nothing was walked")
	}
}

func TestOpenFileChoosesHowToRead(t *testing.T) {
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "film.mp4")
	if err := os.WriteFile(mp4Path, videoInput(t), 0o644); err != nil {
		t.Fatal(err)
	}
	tsPath := filepath.Join(dir, "stream.ts")
	if err := os.WriteFile(tsPath, tsFixture(t, 4), 0o644); err != nil {
		t.Fatal(err)
	}
	junkPath := filepath.Join(dir, "other.bin")
	if err := os.WriteFile(junkPath, []byte("not a container at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("an MP4 stays on disk", func(t *testing.T) {
		r, closeFile, err := OpenFile(mp4Path)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer closeFile()
		if r.data != nil {
			t.Errorf("the file was read whole: %d bytes held", len(r.data))
		}
		if _, err := r.Samples(r.TrackIDs()[0]); err != nil {
			t.Fatalf("Samples: %v", err)
		}
	})
	t.Run("a transport stream is read whole", func(t *testing.T) {
		r, closeFile, err := OpenFile(tsPath)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer closeFile()
		if _, err := r.Samples(r.TrackIDs()[0]); err != nil {
			t.Fatalf("Samples: %v", err)
		}
	})
	t.Run("a file that is not there", func(t *testing.T) {
		_, closeFile, err := OpenFile(filepath.Join(dir, "absent.mp4"))
		if err == nil {
			t.Fatal("a file that is not there was opened")
		}
		if closeFile == nil {
			t.Fatal("no way to close what was not opened")
		}
		if err := closeFile(); err != nil {
			t.Errorf("closing nothing: %v", err)
		}
	})
	t.Run("a file that is neither", func(t *testing.T) {
		if _, _, err := OpenFile(junkPath); err == nil {
			t.Fatal("a file of no known format was accepted")
		}
	})
	t.Run("a directory", func(t *testing.T) {
		if _, _, err := OpenFile(dir); err == nil {
			t.Fatal("a directory was opened as a container")
		}
	})
}

// unknownAndEmptyTracks writes a two-track WebM and renames one codec to one
// nothing knows, keeping the name the same length so every size stays true. It
// gives a reader whose two tracks a join must treat differently: one described,
// one it must pass over.
func unknownAndEmptyTracks(t *testing.T) *Reader {
	t.Helper()
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	good, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "vp09", Timescale: 90000,
		Width: 320, Height: 180, VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8}})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := m.AddTrack(TrackConfig{Kind: Audio, Codec: "Opus", Timescale: 48000,
		Channels: 2, SampleRate: 48000})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.WriteSample(good, Sample{Data: []byte{0x82, byte(i)}, Duration: 3000, Sync: i == 0}); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(bad, Sample{Data: []byte{0xFC, byte(i)}, Duration: 960, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	data := bytes.Replace(buf.Bytes(), []byte("V_VP9"), []byte("V_XYZ"), 1)
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// emptyTrack is an initialisation segment naming a track with nothing in it:
// its configuration reads, its samples do not, which is the track a join must
// pass over rather than fail on.
func emptyTrack(t *testing.T) *Reader {
	t.Helper()
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	if _, err := m.AddTrack(av1Track(30000, 320, 180)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return readerOf(t, buf.Bytes())
}

// TestJoinPassesOverATrackItCannotUse checks a join keeps what it can read and
// steps past what it cannot — a codec it does not know, or a track holding
// nothing — rather than failing over one stray track.
func TestJoinPassesOverATrackItCannotUse(t *testing.T) {
	t.Run("a codec it cannot describe", func(t *testing.T) {
		var out bytes.Buffer
		if err := Join(&out, []*Reader{unknownAndEmptyTracks(t)}); err != nil {
			t.Fatalf("Join: %v", err)
		}
		r := readerOf(t, out.Bytes())
		if ids := r.TrackIDs(); len(ids) != 1 {
			t.Fatalf("the join holds %d tracks, want the one it could describe", len(ids))
		}
	})
	t.Run("a track holding nothing beside one that holds something", func(t *testing.T) {
		var out bytes.Buffer
		// The empty track's configuration reads and its samples do not; the
		// real one beside it must still be written.
		if err := Join(&out, []*Reader{emptyTrack(t), readerOf(t, videoInput(t))}); err != nil {
			t.Fatalf("Join: %v", err)
		}
		r := readerOf(t, out.Bytes())
		if ids := r.TrackIDs(); len(ids) != 1 {
			t.Fatalf("the join holds %d tracks, want the one that held samples", len(ids))
		}
	})
}

// TestJoinProgressiveReportsAJoinThatCannotBeMade covers the error path of the
// progressive join: it must report what fails, not write half a file.
func TestJoinProgressiveReportsAJoinThatCannotBeMade(t *testing.T) {
	src := readerOf(t, videoInput(t))
	var out bytes.Buffer
	if err := JoinProgressive(&out, []*Reader{src}, DropTracks(src.TrackIDs()...)); !errors.Is(err, ErrNoTracks) {
		t.Fatalf("err = %v, want ErrNoTracks", err)
	}
}

// TestOpenFileReadsAMatroskaWhole covers the read-whole path on a real file: a
// format that cannot be addressed into is read into memory instead.
func TestOpenFileReadsAMatroskaWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.webm")
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	id, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "vp09", Timescale: 90000,
		Width: 320, Height: 180, VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{0x82, 0x49, 0x01}, Duration: 3000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	r, closeFile, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer closeFile()
	if r.data == nil {
		t.Fatal("a Matroska file was not read whole")
	}
	if _, err := r.Samples(r.TrackIDs()[0]); err != nil {
		t.Fatalf("Samples: %v", err)
	}
}

// stagedFile is a file that behaves as a test tells it to, so the ways OpenFile
// can meet a filesystem giving way are reachable.
type stagedFile struct {
	src     FileSource
	statErr error
}

func (f *stagedFile) Read(p []byte) (int, error)            { return f.src.Read(p) }
func (f *stagedFile) Seek(o int64, w int) (int64, error)    { return f.src.Seek(o, w) }
func (f *stagedFile) ReadAt(p []byte, o int64) (int, error) { return f.src.ReadAt(p, o) }
func (f *stagedFile) Close() error                          { return nil }
func (f *stagedFile) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return nil, errors.New("no info")
}

func TestOpenFileReportsAFilesystemGivingWay(t *testing.T) {
	openOrig, readOrig := openFile, readFile
	defer func() { openFile, readFile = openOrig, readOrig }()

	t.Run("the file cannot be opened", func(t *testing.T) {
		openFile = func(string) (openedFile, error) { return nil, errors.New("permission denied") }
		if _, closeFile, err := OpenFile("x"); err == nil {
			t.Fatal("a file that could not be opened was accepted")
		} else if closeFile == nil || closeFile() != nil {
			t.Error("no harmless close for a file that never opened")
		}
	})
	t.Run("the file cannot be described", func(t *testing.T) {
		openFile = func(string) (openedFile, error) {
			return &stagedFile{src: bytes.NewReader(videoInput(t)), statErr: errors.New("stale handle")}, nil
		}
		if _, _, err := OpenFile("x"); err == nil {
			t.Fatal("a file that could not be stat'd was accepted")
		}
	})
	t.Run("the file vanishes before the read-whole", func(t *testing.T) {
		// It sniffs as Matroska, so NewFileReader refuses it and the whole
		// file is read next — but by then it is gone.
		openFile = func(string) (openedFile, error) {
			return &stagedFile{src: bytes.NewReader([]byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0})}, nil
		}
		readFile = func(string) ([]byte, error) { return nil, errors.New("no such file") }
		if _, _, err := OpenFile("x"); err == nil {
			t.Fatal("a file that vanished was accepted")
		}
	})
	t.Run("what is read whole is not a container", func(t *testing.T) {
		openFile = func(string) (openedFile, error) {
			return &stagedFile{src: bytes.NewReader([]byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0})}, nil
		}
		// Sniffs as Matroska on its head, is garbage in full: NewReader
		// refuses it.
		readFile = func(string) ([]byte, error) { return []byte{0x1A, 0x45, 0xDF, 0xA3, 0xFF}, nil }
		if _, _, err := OpenFile("x"); err == nil {
			t.Fatal("a file that is not a container was accepted")
		}
	})
}
