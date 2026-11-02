package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// The PGS fixture is built here, by hand, from the segment layouts of the
// Blu-ray presentation graphics stream, because ffmpeg 9.0.2 has a PGS decoder
// and no encoder (`ffmpeg -encoders` lists dvbsub and dvdsub only) and no file of
// the population may be committed. What it holds is synthetic: a 4x2 pixel
// opaque rectangle of one palette colour, shown at one second and cleared at
// three. testdata/subtitles-pgs.sup is this stream as a .sup file, and
// testdata/subtitles-pgs.mkv is ffmpeg's `-c:s copy` of it beside a black
// picture, so that ffmpeg's matroskaenc, not this package, decided what a PGS
// block holds.

const (
	pgsPCS = 0x16 // presentation composition segment
	pgsWDS = 0x17 // window definition segment
	pgsPDS = 0x14 // palette definition segment
	pgsODS = 0x15 // object definition segment
	pgsEND = 0x80 // end of display set segment
)

// pgsSegment is one segment as it sits in a Matroska block: its type, its 16-bit
// size and its payload.
func pgsSegment(kind byte, payload ...byte) []byte {
	out := []byte{kind, 0, 0}
	binary.BigEndian.PutUint16(out[1:], uint16(len(payload)))
	return append(out, payload...)
}

// pgsDisplaySets returns the two display sets of the fixture, each as the bytes
// of one Matroska block: an epoch start that draws the rectangle, and a normal
// case that clears the screen.
func pgsDisplaySets() [2][]byte {
	pcs := func(number uint16, state byte, objects ...byte) []byte {
		p := []byte{0x07, 0x80, 0x04, 0x38, 0x10, byte(number >> 8), byte(number), state, 0, 0, byte(len(objects) / 8)}
		return pgsSegment(pgsPCS, append(p, objects...)...)
	}
	wds := pgsSegment(pgsWDS, 1, 0, 0x00, 0x10, 0x00, 0x10, 0x00, 0x04, 0x00, 0x02)
	pds := pgsSegment(pgsPDS, 0, 0, 1, 235, 128, 128, 255)
	// object 0 in window 0, not cropped, at (16, 16); a 4x2 object whose two
	// lines are each a run of four pixels of colour 1, then the end of line.
	rle := []byte{0x00, 0x84, 0x01, 0x00, 0x00, 0x00, 0x84, 0x01, 0x00, 0x00}
	ods := []byte{0, 0, 0, 0xC0, 0, 0, byte(4 + len(rle)), 0, 4, 0, 2}
	ods = append(ods, rle...)
	show := bytes.Join([][]byte{
		pcs(1, 0x80, 0, 0, 0, 0, 0x00, 0x10, 0x00, 0x10), wds, pds, pgsSegment(pgsODS, ods...), pgsSegment(pgsEND),
	}, nil)
	clear := bytes.Join([][]byte{pcs(2, 0x00), pgsSegment(pgsWDS, 0), pgsSegment(pgsEND)}, nil)
	return [2][]byte{show, clear}
}

// pgsSup is the stream as a .sup file: every segment behind the "PG" magic and
// its 90 kHz PTS and DTS, which Matroska replaces with the block's timestamp.
func pgsSup() []byte {
	var out bytes.Buffer
	for i, set := range pgsDisplaySets() {
		pts := uint32(90000 * (1 + 2*i))
		for rest := set; len(rest) > 0; {
			size := 3 + int(binary.BigEndian.Uint16(rest[1:]))
			out.WriteString("PG")
			out.Write(binary.BigEndian.AppendUint32(nil, pts))
			out.Write(binary.BigEndian.AppendUint32(nil, 0))
			out.Write(rest[:size])
			rest = rest[size:]
		}
	}
	return out.Bytes()
}

// TestRegeneratePGSFixture writes testdata/subtitles-pgs.sup. It runs only when
// asked (AVKIT_REGEN_PGS=1), because the file is committed and the test below
// pins it to this generator.
func TestRegeneratePGSFixture(t *testing.T) {
	if os.Getenv("AVKIT_REGEN_PGS") == "" {
		t.Skip("set AVKIT_REGEN_PGS=1 to rewrite testdata/subtitles-pgs.sup")
	}
	if err := os.WriteFile("testdata/subtitles-pgs.sup", pgsSup(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestThePGSFixtureIsWhatTheGeneratorBuilds(t *testing.T) {
	committed, err := os.ReadFile("testdata/subtitles-pgs.sup")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, pgsSup()) {
		t.Fatal("testdata/subtitles-pgs.sup is not what pgsSup builds; run with AVKIT_REGEN_PGS=1 and rebuild the .mkv")
	}
}

func TestAPGSTrackOfMatroskaIsReadBlockForBlock(t *testing.T) {
	r := subtitleFixture(t, "subtitles-pgs.mkv")
	cfg, err := r.TrackConfig(2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != Subtitle || cfg.Codec != "pgs" || cfg.CodecConfig != nil || cfg.Timescale != 1000 {
		t.Fatalf("config = %+v, want a pgs subtitle track in milliseconds with no configuration", cfg)
	}
	if tr := r.File().Tracks[1]; tr.Codec != "pgs" || tr.Kind != Subtitle {
		t.Fatalf("track = %+v", tr)
	}
	samples, err := r.Samples(2)
	if err != nil {
		t.Fatal(err)
	}
	// ⛔ ffmpeg's matroskaenc made these blocks from the .sup; the segment
	// layouts of the specification made these. They are the same bytes, which
	// is what says a block holds the segments without the "PG" header.
	sets := pgsDisplaySets()
	if len(samples) != 2 || !bytes.Equal(samples[0].Data, sets[0]) || !bytes.Equal(samples[1].Data, sets[1]) {
		t.Fatalf("samples = %+v", samples)
	}
	// The second display set is shown two seconds after the first, and the
	// last block lasts to the end of the segment, five seconds in.
	if samples[0].Duration != 2000 || samples[1].Duration != 3000 || !samples[0].Sync || !samples[1].Sync {
		t.Errorf("samples = %+v", samples)
	}
}

func TestAPGSTrackIsCarriedFromMatroskaToMatroskaUnchanged(t *testing.T) {
	r := subtitleFixture(t, "subtitles-pgs.mkv")
	want, _ := r.Samples(2)
	for _, mode := range []struct {
		name string
		opts []WebMOption
	}{{"buffered", []WebMOption{BufferedSegment()}}, {"streamed", nil}} {
		data := webmOf(t, r, mode.opts...)
		doc, _, err := unmarshalMatroska(data)
		if err != nil {
			t.Fatal(err)
		}
		te := doc.Segment.Tracks.TrackEntry[1]
		if te.CodecID != "S_HDMV/PGS" || te.TrackType != mkvTrackSubtitle || te.CodecPrivate != nil || doc.Header.DocType != "matroska" {
			t.Errorf("%s: track %s type %d private %v in %s, want S_HDMV/PGS, type 17, none, in matroska",
				mode.name, te.CodecID, te.TrackType, te.CodecPrivate, doc.Header.DocType)
		}
		cfgs, samples := readBack(t, data)
		if cfgs[1].Codec != "pgs" {
			t.Fatalf("%s: read back as %+v", mode.name, cfgs[1])
		}
		if len(samples[1]) != len(want) {
			t.Fatalf("%s: %d blocks, want %d", mode.name, len(samples[1]), len(want))
		}
		for i := range want {
			if !bytes.Equal(samples[1][i].Data, want[i].Data) || samples[1][i].Duration != want[i].Duration {
				t.Errorf("%s: block %d = %d bytes lasting %d, want %d bytes lasting %d",
					mode.name, i+1, len(samples[1][i].Data), samples[1][i].Duration, len(want[i].Data), want[i].Duration)
			}
		}
	}
}

func TestTheCodecPrivateOfAPGSTrackIsIgnored(t *testing.T) {
	te := mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackSubtitle, CodecID: "S_HDMV/PGS", CodecPrivate: []byte("junk")}
	cfg, err := mkvTrackConfig(te, 1000)
	if err != nil || cfg.Codec != "pgs" || cfg.CodecConfig != nil {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
}

func TestAPGSTrackIsRefusedByMP4WithTheReason(t *testing.T) {
	cfg := TrackConfig{Kind: Subtitle, Codec: "pgs", Timescale: 1000}
	for who, add := range map[string]func(TrackConfig) (uint32, error){
		"Muxer":            NewMuxer(io.Discard).AddTrack,
		"ProgressiveMuxer": NewProgressiveMuxer(io.Discard).AddTrack,
	} {
		_, err := add(cfg)
		if !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), "MP4RA") {
			t.Errorf("%s: %v, want an unsupported codec that names MP4RA", who, err)
		}
	}
	r := subtitleFixture(t, "subtitles-pgs.mkv")
	if err := Remux(io.Discard, r); !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("Remux: %v", err)
	}
	// Leaving the track out is the way on.
	var buf bytes.Buffer
	if err := Remux(&buf, r, DropTracks(2)); err != nil {
		t.Fatal(err)
	}
	if cfgs, _ := readBack(t, buf.Bytes()); len(cfgs) != 1 {
		t.Errorf("%d tracks, want only the picture", len(cfgs))
	}
}
