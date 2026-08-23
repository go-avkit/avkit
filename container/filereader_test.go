// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fmp4Sample and progressiveSample are the same two tracks written in the two
// shapes an MP4 comes in, and transportStreamSample is a format this reader
// does not serve.
func fmp4Sample(t *testing.T) []byte {
	t.Helper()
	return muxedFixture(t,
		fixtureTrack{cfg: av1Track(30000, 320, 180), samples: marked(0xA1, 12, 1000, 4)},
		fixtureTrack{cfg: aacTrack(48000), samples: marked(0xB2, 12, 1024, 1)})
}

func progressiveSample(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf)
	id, err := m.AddTrack(av1Track(30000, 320, 180))
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	for _, s := range marked(0xC3, 12, 1000, 4) {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatalf("WriteSample: %v", err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func transportStreamSample(t *testing.T) []byte {
	t.Helper()
	return tsFixture(t, 4)
}

// countingSource counts what a reader actually pulls out of a file, which is
// the whole point of reading one from disk.
type countingSource struct {
	src  FileSource
	read int64
	fail error
}

func (c *countingSource) Read(p []byte) (int, error) { return c.src.Read(p) }

func (c *countingSource) Seek(offset int64, whence int) (int64, error) {
	return c.src.Seek(offset, whence)
}

func (c *countingSource) ReadAt(p []byte, off int64) (int, error) {
	if c.fail != nil {
		return 0, c.fail
	}
	n, err := c.src.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

// TestFileReaderReadsTheSameSamples is the property that matters: a file read
// from disk must give exactly what the same bytes give in memory. Anything else
// would be a second way of reading a file that quietly disagrees with the first.
func TestFileReaderReadsTheSameSamples(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"fragmented", fmp4Sample(t)},
		{"progressive", progressiveSample(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, err := NewReader(tc.data)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			onDisk, err := NewFileReader(bytes.NewReader(tc.data), int64(len(tc.data)))
			if err != nil {
				t.Fatalf("NewFileReader: %v", err)
			}
			want, got := held.TrackIDs(), onDisk.TrackIDs()
			if len(got) != len(want) || len(got) == 0 {
				t.Fatalf("track ids = %v, want %v", got, want)
			}
			for i, id := range want {
				if got[i] != id {
					t.Fatalf("track %d = %d, want %d", i, got[i], id)
				}
				wantCfg, err := held.TrackConfig(id)
				if err != nil {
					t.Fatal(err)
				}
				gotCfg, err := onDisk.TrackConfig(id)
				if err != nil {
					t.Fatalf("TrackConfig from disk: %v", err)
				}
				if gotCfg.Codec != wantCfg.Codec || gotCfg.Timescale != wantCfg.Timescale {
					t.Fatalf("track %d = %+v, want %+v", id, gotCfg, wantCfg)
				}
				wantSamples, err := held.Samples(id)
				if err != nil {
					t.Fatal(err)
				}
				gotSamples, err := onDisk.Samples(id)
				if err != nil {
					t.Fatalf("Samples from disk: %v", err)
				}
				if len(gotSamples) != len(wantSamples) {
					t.Fatalf("track %d: %d samples from disk, %d in memory",
						id, len(gotSamples), len(wantSamples))
				}
				for j := range wantSamples {
					if !bytes.Equal(gotSamples[j].Data, wantSamples[j].Data) {
						t.Fatalf("track %d sample %d differs", id, j)
					}
					if gotSamples[j].Duration != wantSamples[j].Duration ||
						gotSamples[j].Sync != wantSamples[j].Sync ||
						gotSamples[j].CompositionOffset != wantSamples[j].CompositionOffset {
						t.Fatalf("track %d sample %d = %+v, want %+v",
							id, j, gotSamples[j], wantSamples[j])
					}
				}
			}
		})
	}
}

// bigSample is a file whose media dwarfs its tables, which is the only shape in
// which "the media stays on disk" means anything: a small fixture is read whole
// by the four kilobytes that identify the format.
func bigSample(t *testing.T) []byte {
	t.Helper()
	const frames, frameSize = 256, 32 << 10
	samples := make([]Sample, 0, frames)
	for i := 0; i < frames; i++ {
		data := make([]byte, frameSize)
		data[0], data[1] = byte(i), byte(i>>8)
		samples = append(samples, Sample{Data: data, Duration: 1000, Sync: i%8 == 0})
	}
	return muxedFixture(t, fixtureTrack{cfg: av1Track(30000, 320, 180), samples: samples})
}

// TestFileReaderHoldsOnlyWhatItIsAskedFor checks the reason this exists: the
// media stays on disk. A reader that quietly slurped the file would pass every
// test above and still fail the only job it has.
func TestFileReaderHoldsOnlyWhatItIsAskedFor(t *testing.T) {
	data := bigSample(t)
	if len(data) < 8<<20 {
		t.Fatalf("the fixture is %d bytes, too small for this to mean anything", len(data))
	}
	counter := &countingSource{src: bytes.NewReader(data)}
	r, err := NewFileReader(counter, int64(len(data)))
	if err != nil {
		t.Fatalf("NewFileReader: %v", err)
	}
	// Reading the structure must not pull the media in. What it does read is
	// the head it identifies the format by, and the boxes that are not media.
	afterTables := counter.read
	if afterTables > int64(len(data))/8 {
		t.Fatalf("reading the tables took %d of %d bytes", afterTables, len(data))
	}
	// And it keeps no copy of the file.
	if r.data != nil {
		t.Fatalf("the reader holds %d bytes of the file", len(r.data))
	}
	samples, err := r.Samples(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no sample was read")
	}
	// What it then read is the samples themselves, not the file around them.
	var total int64
	for _, s := range samples {
		total += int64(len(s.Data))
	}
	if pulled := counter.read - afterTables; pulled != total {
		t.Fatalf("read %d bytes for %d bytes of samples", pulled, total)
	}
}

// TestFileReaderDoesNotGrowWithTheFile is the claim a caller relies on: a file
// larger than the memory to hand can be read. The heap is measured while the
// reader is alive and its tables are in use, and compared against reading the
// same bytes in memory, which necessarily holds them all.
func TestFileReaderDoesNotGrowWithTheFile(t *testing.T) {
	data := bigSample(t)
	measure := func(build func() *Reader) uint64 {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		r := build()
		ids := r.TrackIDs()
		if len(ids) == 0 {
			t.Fatal("no track")
		}
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(r)
		if after.HeapAlloc < before.HeapAlloc {
			return 0
		}
		return after.HeapAlloc - before.HeapAlloc
	}
	held := measure(func() *Reader {
		// A copy, so what is measured is this reader's own hold on it.
		buf := append([]byte(nil), data...)
		r, err := NewReader(buf)
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		return r
	})
	onDisk := measure(func() *Reader {
		r, err := NewFileReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("NewFileReader: %v", err)
		}
		return r
	})
	if held < uint64(len(data))/2 {
		t.Fatalf("reading in memory held %d bytes for a file of %d, so this measures nothing",
			held, len(data))
	}
	if onDisk > held/4 {
		t.Fatalf("reading from disk held %d bytes, in memory %d: it follows the file's size",
			onDisk, held)
	}
}

func TestFileReaderRefusesWhatItCannotServe(t *testing.T) {
	t.Run("no file at all", func(t *testing.T) {
		if _, err := NewFileReader(nil, 0); !errors.Is(err, ErrUnsupportedFormat) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a format read in one pass", func(t *testing.T) {
		ts := transportStreamSample(t)
		_, err := NewFileReader(bytes.NewReader(ts), int64(len(ts)))
		if !errors.Is(err, ErrUnsupportedFormat) {
			t.Fatalf("err = %v, want the format refused", err)
		}
	})
	t.Run("a file that cannot be read", func(t *testing.T) {
		src := &countingSource{src: bytes.NewReader(fmp4Sample(t)), fail: errors.New("closed")}
		if _, err := NewFileReader(src, 10); err == nil {
			t.Fatal("a file that answers nothing was accepted")
		}
	})
	t.Run("an empty file", func(t *testing.T) {
		if _, err := NewFileReader(bytes.NewReader(nil), 0); err == nil {
			t.Fatal("an empty file was accepted")
		}
	})
	t.Run("a sample beyond the end of the file", func(t *testing.T) {
		data := fmp4Sample(t)
		// The tables are read from the whole file and the size is stated
		// short, so the samples they name are past the end.
		r, err := NewFileReader(bytes.NewReader(data), 64)
		if err != nil {
			t.Fatalf("NewFileReader: %v", err)
		}
		if _, err := r.Samples(r.TrackIDs()[0]); !errors.Is(err, ErrSampleData) {
			t.Fatalf("err = %v, want ErrSampleData", err)
		}
	})
}

// TestFileReaderOnARealFile walks the path a caller actually takes.
func TestFileReaderOnARealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "film.mp4")
	data := progressiveSample(t)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewFileReader(fh, info.Size())
	if err != nil {
		t.Fatalf("NewFileReader: %v", err)
	}
	samples, err := r.Samples(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no sample was read from the file")
	}
	// What it read is what a muxer can write again.
	var out bytes.Buffer
	m := NewMuxer(&out)
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	for i, s := range samples {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatalf("write sample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatalf("read what was written: %v", err)
	}
	written, err := back.Samples(back.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != len(samples) {
		t.Fatalf("wrote %d samples, read %d back", len(samples), len(written))
	}
	_ = io.Discard
}

// failingSource answers as told, so each way a file can give way is staged.
type failingSource struct {
	src       FileSource
	failReads bool
	failSeeks bool
}

func (f *failingSource) Read(p []byte) (int, error) { return f.src.Read(p) }

func (f *failingSource) Seek(offset int64, whence int) (int64, error) {
	if f.failSeeks {
		return 0, errors.New("seek: bad file descriptor")
	}
	return f.src.Seek(offset, whence)
}

func (f *failingSource) ReadAt(p []byte, off int64) (int, error) {
	if f.failReads {
		return 0, errors.New("read: input/output error")
	}
	return f.src.ReadAt(p, off)
}

func TestFileReaderReportsAFileGivingWay(t *testing.T) {
	t.Run("a file that cannot be rewound", func(t *testing.T) {
		src := &failingSource{src: bytes.NewReader(fmp4Sample(t)), failSeeks: true}
		if _, err := NewFileReader(src, 10); err == nil {
			t.Fatal("a file that cannot be rewound was accepted")
		}
	})
	t.Run("a file that is not the MP4 its head claims", func(t *testing.T) {
		// An ftyp box, then rubbish where the next box should be.
		head := append([]byte{0, 0, 0, 16}, []byte("ftypisom")...)
		head = append(head, 0, 0, 0, 0, 0, 0, 0, 0)
		junk := append(head, []byte("this is not a box at all")...)
		if _, err := NewFileReader(bytes.NewReader(junk), int64(len(junk))); err == nil {
			t.Fatal("a file that stops making sense was accepted")
		}
	})
	t.Run("an MP4 that names no movie", func(t *testing.T) {
		// A complete, valid ftyp of the twenty bytes it states, and nothing
		// else: it decodes, and there is no movie in it to read.
		only := append([]byte{0, 0, 0, 20}, []byte("ftypisom")...)
		only = append(only, 0, 0, 2, 0, 'i', 's', 'o', 'm')
		if _, err := NewFileReader(bytes.NewReader(only), int64(len(only))); err == nil {
			t.Fatal("a file naming no movie was accepted")
		}
	})
	t.Run("a file that stops answering once its tables are read", func(t *testing.T) {
		data := fmp4Sample(t)
		src := &failingSource{src: bytes.NewReader(data)}
		r, err := NewFileReader(src, int64(len(data)))
		if err != nil {
			t.Fatalf("NewFileReader: %v", err)
		}
		src.failReads = true
		if _, err := r.Samples(r.TrackIDs()[0]); !errors.Is(err, ErrSampleData) {
			t.Fatalf("err = %v, want ErrSampleData", err)
		}
	})
}

// TestSampleBytesRefusesAnImpossibleSpan covers a sample whose end lies before
// its start, which is what a size that overflows its own arithmetic leaves
// behind. Reading it would take a span of nonsense length.
func TestSampleBytesRefusesAnImpossibleSpan(t *testing.T) {
	data := fmp4Sample(t)
	for _, r := range []*Reader{
		{data: data},
		{at: bytes.NewReader(data), size: int64(len(data))},
	} {
		if _, err := r.sampleBytes(10, 5); err == nil {
			t.Fatal("a sample ending before it begins was read")
		}
	}
}
