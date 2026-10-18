// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// The fixtures metadata-ffmpeg.mp4 and metadata-ffmpeg.mkv were written by
// ffmpeg 9.0.2, the oracle of this lot:
//
//	ffmpeg -f lavfi -i testsrc=s=64x48:d=0.2:r=10 -i metadata-cover.jpg -map 0 -map 1 \
//	  -c:v:0 libx264 -preset ultrafast -c:v:1 copy -disposition:v:1 attached_pic \
//	  -metadata title="Tiny Title" -metadata artist="Some Artist" -metadata album="An Album" \
//	  -metadata date=2024 -metadata comment="A comment" -metadata description="A description" \
//	  -metadata genre=Documentary -metadata encoding_tool="enc tool" -metadata track=3/9 \
//	  metadata-ffmpeg.mp4
//	ffmpeg -f lavfi -i testsrc=s=64x48:d=0.2:r=10 -c:v libvpx-vp9 -b:v 50k \
//	  -attach metadata-cover.jpg -metadata:s:t mimetype=image/jpeg \
//	  -metadata title=... (the same seven) -metadata custom_key=x metadata-ffmpeg.mkv
//
// ffprobe -show_format reads back exactly the values asserted below.

// ffmpegMetadata is what both fixtures state, in this package's model.
func ffmpegMetadata(encoder string) Metadata {
	return Metadata{
		Title: "Tiny Title", Artist: "Some Artist", Album: "An Album", Date: "2024",
		Comment: "A comment", Description: "A description", Genre: "Documentary",
		Encoder: encoder,
	}
}

// assertMetadata compares the exported fields and the cover's bytes.
func assertMetadata(t *testing.T, what string, got, want Metadata) {
	t.Helper()
	for i, f := range got.metadataText() {
		if w := want.metadataText()[i]; f.value != w.value {
			t.Errorf("%s: %s = %q, want %q", what, f.name, f.value, w.value)
		}
	}
	switch {
	case want.Cover == nil && got.Cover != nil:
		t.Errorf("%s: a cover came back where none went in", what)
	case want.Cover != nil && got.Cover == nil:
		t.Errorf("%s: the cover was lost", what)
	case want.Cover != nil:
		if got.Cover.MIMEType != want.Cover.MIMEType || !bytes.Equal(got.Cover.Data, want.Cover.Data) {
			t.Errorf("%s: cover %s of %d bytes, want %s of %d, byte for byte", what,
				got.Cover.MIMEType, len(got.Cover.Data), want.Cover.MIMEType, len(want.Cover.Data))
		}
	}
}

func coverFixture(t *testing.T) *Picture {
	return &Picture{MIMEType: MIMEJPEG, Data: readFixture(t, "metadata-cover.jpg")}
}

func TestMetadataReadsWhatFFmpegWrote(t *testing.T) {
	cases := []struct {
		fixture string
		want    Metadata
	}{
		// ffmpeg writes its "encoding_tool" key as ©too.
		{"metadata-ffmpeg.mp4", ffmpegMetadata("enc tool")},
		// And to Matroska its own name as ENCODER, the date as DATE (read as
		// the fallback it is) and the title in Info.
		{"metadata-ffmpeg.mkv", ffmpegMetadata("Lavf63.1.102")},
	}
	for _, c := range cases {
		c.want.Cover = coverFixture(t)
		data := readFixture(t, c.fixture)
		f, err := Demux(data)
		if err != nil {
			t.Fatalf("Demux(%s): %v", c.fixture, err)
		}
		assertMetadata(t, "Demux "+c.fixture, f.Metadata, c.want)
		r, err := NewReader(data)
		if err != nil {
			t.Fatalf("NewReader(%s): %v", c.fixture, err)
		}
		assertMetadata(t, "NewReader "+c.fixture, r.File().Metadata, c.want)
		// The cover is not a track: an MP4 states it in covr, a Matroska file
		// as an attachment, and neither is a stream with samples.
		if n := len(r.TrackIDs()); n != 1 {
			t.Errorf("%s: %d tracks, want the one video track", c.fixture, n)
		}
	}
}

func TestMetadataKeepsWhatItDoesNotNameForTheSameContainer(t *testing.T) {
	// The MP4 fixture carries a track number, trkn, which no field states.
	r, err := NewReader(readFixture(t, "metadata-ffmpeg.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	md := r.File().Metadata
	if len(md.mp4Items) != 1 || string(md.mp4Items[0][4:8]) != "trkn" {
		t.Fatalf("unnamed ilst items = %q, want the trkn item", md.mp4Items)
	}
	var out bytes.Buffer
	if err := Remux(&out, r); err != nil {
		t.Fatal(err)
	}
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := back.File().Metadata.mp4Items; len(got) != 1 || !bytes.Equal(got[0], md.mp4Items[0]) {
		t.Fatalf("trkn after a remux = %x, want %x verbatim", got, md.mp4Items[0])
	}

	// The Matroska fixture carries CUSTOM_KEY, which a Matroska output keeps.
	r, err = NewReader(readFixture(t, "metadata-ffmpeg.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	md = r.File().Metadata
	if len(md.mkvTags) != 1 || md.mkvTags[0].TagName != "CUSTOM_KEY" {
		t.Fatalf("unnamed tags = %+v, want CUSTOM_KEY", md.mkvTags)
	}
	data := copyToWebM(t, r, md)
	back, err = NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.File().Metadata.mkvTags; len(got) != 1 || got[0].TagName != "CUSTOM_KEY" ||
		got[0].TagString[0] != "x" {
		t.Fatalf("CUSTOM_KEY after a WebM copy = %+v", got)
	}
}

// copyToWebM writes every track of r into a WebMMuxer, with md.
func copyToWebM(t *testing.T, r *Reader, md Metadata, opts ...WebMOption) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, opts...)
	if err := m.SetMetadata(md); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	copyTracks(t, r, m)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// copyTracks declares every track of r on m and writes its samples.
func copyTracks(t *testing.T, r *Reader, m sampleWriter) {
	t.Helper()
	ids := map[uint32]uint32{}
	for _, id := range r.TrackIDs() {
		cfg, err := r.TrackConfig(id)
		if err != nil {
			t.Fatal(err)
		}
		if ids[id], err = m.AddTrack(cfg); err != nil {
			t.Fatalf("AddTrack: %v", err)
		}
	}
	for _, id := range r.TrackIDs() {
		samples, err := r.Samples(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range samples {
			if err := m.WriteSample(ids[id], s); err != nil {
				t.Fatalf("WriteSample: %v", err)
			}
		}
	}
}

// fullMetadata states every field, with text no container would mistake.
func fullMetadata(t *testing.T) Metadata {
	return Metadata{
		Title: "Le titre — ü", Artist: "Artiste", Album: "Album", Date: "2003-05-01",
		Comment: "commentaire", Description: "deux oiseaux", Genre: "Ambient", Encoder: "avkit",
		Cover: coverFixture(t),
	}
}

func TestEveryMuxerWritesMetadataItsReaderReadsBack(t *testing.T) {
	want := fullMetadata(t)
	avc, err := NewReader(readFixture(t, "metadata-ffmpeg.mp4"))
	if err != nil {
		t.Fatal(err)
	}

	var frag bytes.Buffer
	fm := NewMuxer(&frag)
	// Before the first track: the udta still follows them.
	if err := fm.SetMetadata(want); err != nil {
		t.Fatal(err)
	}
	copyTracks(t, avc, fm)
	if err := fm.Close(); err != nil {
		t.Fatal(err)
	}

	var prog bytes.Buffer
	pm := NewProgressiveMuxer(&prog)
	copyTracks(t, avc, pm)
	// After the samples: the movie box goes out at Close.
	if err := pm.SetMetadata(want); err != nil {
		t.Fatal(err)
	}
	if err := pm.Close(); err != nil {
		t.Fatal(err)
	}

	outputs := map[string][]byte{
		"fragmented":        frag.Bytes(),
		"progressive":       prog.Bytes(),
		"matroska stream":   copyToWebM(t, avc, want),
		"matroska buffered": copyToWebM(t, avc, want, BufferedSegment()),
	}
	for what, data := range outputs {
		f, err := Demux(data)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		assertMetadata(t, what, f.Metadata, want)
		r, err := NewReader(data)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		assertMetadata(t, what+" reader", r.File().Metadata, want)
		// A cue still points at a cluster, now that more precedes them.
		if r.File().Format == "matroska" {
			if _, err := r.Samples(r.TrackIDs()[0]); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
	}

	// The udta is the shape ffmpeg writes: meta, hdlr mdir/appl, ilst.
	parsed, err := mp4.DecodeFile(bytes.NewReader(prog.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var meta *mp4.MetaBox
	for _, c := range parsed.Moov.Children {
		if u, ok := c.(*mp4.UdtaBox); ok {
			meta = u.Children[0].(*mp4.MetaBox)
		}
	}
	if meta == nil || meta.IsQuickTime() || meta.Hdlr.HandlerType != "mdir" ||
		meta.Hdlr.Reserved[0] != applManufacturer || meta.Hdlr.Size() != 33 {
		t.Fatalf("meta box = %+v, want a full box with a 33-byte mdir/appl handler", meta)
	}
}

func TestWebMCarriesTheTextAndNotTheCover(t *testing.T) {
	// VP9 keeps the file WebM, and WebM admits no attachment.
	vp9, err := NewReader(readFixture(t, "metadata-ffmpeg.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	md := fullMetadata(t)
	data := copyToWebM(t, vp9, md)
	f, err := Demux(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != "webm" {
		t.Fatalf("format = %s, want webm", f.Format)
	}
	md.Cover = nil
	assertMetadata(t, "webm", f.Metadata, md)
}

func TestNoMetadataWritesTheBytesItAlwaysDid(t *testing.T) {
	// A Metadata that states nothing is not a udta of nothing: the output is
	// the one written before metadata existed.
	r, err := NewReader(readFixture(t, "tiny.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(set bool) []byte {
		var buf bytes.Buffer
		m := NewMuxer(&buf)
		if set {
			if err := m.SetMetadata(Metadata{}); err != nil {
				t.Fatal(err)
			}
		}
		copyTracks(t, r, m)
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	plain, empty := write(false), write(true)
	if !bytes.Equal(plain, empty) || bytes.Contains(empty, []byte("udta")) {
		t.Fatal("an empty Metadata changed the output")
	}
	// And names a Matroska output can keep but an MP4 cannot write no udta.
	only := Metadata{mkvTags: []mkvSimpleTag{{TagName: "X", TagString: []string{"y"}}}}
	if mp4Udta(&only) != nil || only.IsZero() {
		t.Fatal("tags only Matroska carries made an MP4 udta, or counted as nothing")
	}
}

func TestTheWholeFileOperationsCarryTheFirstInputsMetadata(t *testing.T) {
	src := readFixture(t, "metadata-ffmpeg.mp4")
	reader := func() *Reader {
		r, err := NewReader(src)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	bare := func() *Reader {
		r, err := NewReader(readFixture(t, "metadata-ffmpeg.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		r.file.Metadata = Metadata{Title: "second"}
		return r
	}
	want := ffmpegMetadata("enc tool")
	want.Cover = coverFixture(t)
	ops := map[string]func(*bytes.Buffer, ...RemuxOption) error{
		"Remux": func(w *bytes.Buffer, o ...RemuxOption) error { return Remux(w, reader(), o...) },
		"Cut": func(w *bytes.Buffer, o ...RemuxOption) error {
			return Cut(w, reader(), 0, 100*time.Millisecond, o...)
		},
		"Concat": func(w *bytes.Buffer, o ...RemuxOption) error {
			return Concat(w, []*Reader{reader(), bare()}, o...)
		},
		"Join": func(w *bytes.Buffer, o ...RemuxOption) error {
			return Join(w, []*Reader{reader(), bare()}, o...)
		},
		"JoinProgressive": func(w *bytes.Buffer, o ...RemuxOption) error {
			return JoinProgressive(w, []*Reader{reader(), bare()}, o...)
		},
	}
	for name, op := range ops {
		var out bytes.Buffer
		if err := op(&out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f, err := Demux(out.Bytes())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertMetadata(t, name, f.Metadata, want)

		out.Reset()
		replaced := Metadata{Title: "replaced"}
		if err := op(&out, ReplaceMetadata(replaced)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f, err = Demux(out.Bytes()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertMetadata(t, name+" replaced", f.Metadata, replaced)

		out.Reset()
		if err := op(&out, ReplaceMetadata(Metadata{})); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f, err = Demux(out.Bytes()); err != nil || !f.Metadata.IsZero() {
			t.Fatalf("%s with no metadata: %+v, %v", name, f.Metadata, err)
		}

		bad := Metadata{Cover: &Picture{MIMEType: "image/webp", Data: []byte{1}}}
		if err := op(&out, ReplaceMetadata(bad)); !errors.Is(err, ErrMetadata) {
			t.Errorf("%s with a webp cover: %v, want ErrMetadata", name, err)
		}
	}
}

func TestSetMetadataRefusesWhatCannotBeWritten(t *testing.T) {
	cases := map[string]Metadata{
		"invalid UTF-8": {Artist: "\xff"},
		"NUL":           {Comment: "a\x00b"},
		"empty cover":   {Cover: &Picture{MIMEType: MIMEPNG}},
		"unknown type":  {Cover: &Picture{MIMEType: "image/gif", Data: []byte{1}}},
	}
	for what, md := range cases {
		setters := map[string]func(Metadata) error{
			"Muxer":            NewMuxer(&bytes.Buffer{}).SetMetadata,
			"ProgressiveMuxer": NewProgressiveMuxer(&bytes.Buffer{}).SetMetadata,
			"WebMMuxer":        NewWebMMuxer(&bytes.Buffer{}).SetMetadata,
		}
		for name, set := range setters {
			if err := set(md); !errors.Is(err, ErrMetadata) {
				t.Errorf("%s, %s: %v, want ErrMetadata", name, what, err)
			}
		}
	}

	// Too late, and closed.
	cfg := av1Track(1000, 64, 48)
	m := NewMuxer(&bytes.Buffer{})
	id, _ := m.AddTrack(cfg)
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 1, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetMetadata(Metadata{Title: "x"}); !errors.Is(err, ErrMetadata) {
		t.Errorf("Muxer after a sample: %v", err)
	}
	_ = m.Close()
	if err := m.SetMetadata(Metadata{}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed Muxer: %v", err)
	}

	w := NewWebMMuxer(&bytes.Buffer{})
	wid, _ := w.AddTrack(webmVideoConfig())
	if err := w.WriteSample(wid, Sample{Data: []byte{1}, Duration: 3003, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := w.SetMetadata(Metadata{Title: "x"}); !errors.Is(err, ErrMetadata) {
		t.Errorf("WebMMuxer after a sample: %v", err)
	}
	_ = w.Close()
	if err := w.SetMetadata(Metadata{}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed WebMMuxer: %v", err)
	}

	p := NewProgressiveMuxer(&bytes.Buffer{})
	_ = p.Close()
	if err := p.SetMetadata(Metadata{}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed ProgressiveMuxer: %v", err)
	}
}

func TestMetadataOfOtherReaders(t *testing.T) {
	// A transport stream has no place for metadata.
	f, err := Demux(tsFixture(t, 3))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Metadata.IsZero() {
		t.Fatalf("a transport stream read as %+v", f.Metadata)
	}
	// A sequence of segments says what the first one says.
	first := readFixture(t, "metadata-ffmpeg.mp4")
	r, err := NewSegmentedReader([][]byte{first, readFixture(t, "tiny.mp4")})
	if err != nil {
		t.Fatal(err)
	}
	if r.File().Metadata.Title != "Tiny Title" {
		t.Fatalf("segmented metadata = %+v", r.File().Metadata)
	}
}

// --- ilst items -------------------------------------------------------------

func box(name string, payload ...[]byte) []byte {
	body := slices.Concat(payload...)
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	return append(append(out, name...), body...)
}

func dataBox(typ uint32, value []byte) []byte {
	return box("data", binary.BigEndian.AppendUint32(nil, typ), make([]byte, 4), value)
}

func TestIlstItemsAreReadAsAppleSpecifiesThem(t *testing.T) {
	utf16 := []byte{0, 'h', 0, 'i', 0xd8, 0x3d, 0xde, 0x00} // "hi😀"
	png := append(append([]byte{}, pngSignature...), 1, 2, 3)
	var md Metadata
	for _, raw := range [][]byte{
		box("\xa9nam", dataBox(ilstUTF16, utf16)),
		box("\xa9nam", dataBox(ilstUTF8, []byte("second"))), // already filled: kept whole
		box("\xa9ART", box("free"), dataBox(ilstUTF8Sort, []byte("artist"))),
		box("\xa9alb", dataBox(ilstUTF16Srt, []byte{0, 'a'})),
		box("\xa9day", dataBox(21, []byte{7})),        // an integer: kept whole
		box("\xa9gen", dataBox(ilstUTF16, []byte{0})), // odd UTF-16: kept whole
		box("desc", dataBox(ilstUTF8, []byte{0xff})),  // not UTF-8: kept whole
		box("\xa9cmt", []byte{0, 0, 0, 9, 'd'}),       // a broken child: kept whole
		box("\xa9too", box("data", []byte{0, 0, 0, 1})),
		box("covr", dataBox(ilstJPEG, png)), // labelled JPEG, is PNG
		box("covr", dataBox(ilstBMP, []byte{'B', 'M'})),
		box("covr", dataBox(ilstUTF8, []byte("not a picture"))),
		box("covr", dataBox(ilstPNG, nil)),
		{0, 0, 0, 3, 'b', 'a', 'd', '!'}, // not even a box: dropped
	} {
		md.addIlstItem(raw)
	}
	if md.Title != "hi😀" || md.Artist != "artist" || md.Album != "a" {
		t.Fatalf("text = %q %q %q", md.Title, md.Artist, md.Album)
	}
	if md.Date != "" || md.Genre != "" || md.Description != "" || md.Comment != "" || md.Encoder != "" {
		t.Fatalf("a value that is not text was read as one: %+v", md)
	}
	if md.Cover == nil || md.Cover.MIMEType != MIMEPNG || !bytes.Equal(md.Cover.Data, png) {
		t.Fatalf("cover = %+v, want the PNG its bytes say it is", md.Cover)
	}
	// The second ©nam and covr, the four values that are not text, the broken
	// child, the short data box, and the two covr items that hold no picture.
	if n := len(md.mp4Items); n != 9 {
		t.Fatalf("%d items kept whole, want 9", n)
	}
	if pic := ilstPicture(dataBox(ilstBMP, []byte{'B', 'M'})); pic.MIMEType != MIMEBMP {
		t.Fatalf("BMP cover = %+v", pic)
	}
	if pic := ilstPicture(dataBox(ilstJPEG, []byte{0xff, 0xd8})); pic.MIMEType != MIMEJPEG {
		t.Fatalf("JPEG cover = %+v", pic)
	}
}

func TestSplitBoxRefusesSizesThatDoNotFit(t *testing.T) {
	large := binary.BigEndian.AppendUint64(append([]byte{0, 0, 0, 1}, "abcd"...), 20)
	large = append(large, 1, 2, 3, 4)
	cases := []struct {
		in   []byte
		ok   bool
		body int
	}{
		{nil, false, 0},
		{[]byte{0, 0, 0, 1, 'a', 'b', 'c', 'd'}, false, 0}, // 64-bit size missing
		{[]byte{0, 0, 0, 4, 'a', 'b', 'c', 'd'}, false, 0}, // smaller than its header
		{[]byte{0xff, 0xff, 0xff, 0xff, 'a', 'b', 'c', 'd'}, false, 0},
		{[]byte{0, 0, 0, 0, 'a', 'b', 'c', 'd', 9}, true, 1}, // to the end
		{large, true, 4},
	}
	for i, c := range cases {
		_, body, _, ok := splitBox(c.in)
		if ok != c.ok || len(body) != c.body {
			t.Errorf("case %d: ok=%v body=%d, want %v %d", i, ok, len(body), c.ok, c.body)
		}
	}
}

func TestAnIlstItemThatDoesNotEncodeIsLeftOut(t *testing.T) {
	saved := encodeBox
	defer func() { encodeBox = saved }()
	encodeBox = func(mp4.Box) ([]byte, error) { return nil, errors.New("staged") }
	f, err := Demux(readFixture(t, "metadata-ffmpeg.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Metadata.IsZero() {
		t.Fatalf("metadata = %+v, want nothing", f.Metadata)
	}
}

func TestOnlyTheITunesMetaBoxIsRead(t *testing.T) {
	ilst := &mp4.IlstBox{}
	ilst.AddChild(ilstItem("\xa9nam", ilstUTF8, []byte("x")))
	other := mp4.CreateMetaBox(0, &mp4.HdlrBox{HandlerType: "mdta"})
	other.AddChild(ilst)
	noHdlr := &mp4.MetaBox{}
	noHdlr.AddChild(ilst)
	itunes := mp4.CreateMetaBox(0, &mp4.HdlrBox{HandlerType: mdirHandler})
	itunes.AddChild(&mp4.FreeBox{})
	udta := &mp4.UdtaBox{}
	udta.AddChild(other)
	udta.AddChild(noHdlr)
	udta.AddChild(itunes)
	udta.AddChild(&mp4.FreeBox{})
	moov := mp4.NewMoovBox()
	moov.AddChild(udta)
	if md := mp4Metadata(moov); !md.IsZero() {
		t.Fatalf("read %+v out of meta boxes that are not iTunes metadata", md)
	}
}

// --- Matroska tags and attachments -----------------------------------------

func tagString(name, value string) mkvSimpleTag {
	return mkvSimpleTag{TagName: name, TagString: []string{value}}
}

func TestMatroskaTagsAreReadAtTheFileLevelOnly(t *testing.T) {
	file := mkvTargets{}
	tags := []mkvTagsElement{{Tag: []mkvTag{
		{Targets: mkvTargets{TagTrackUID: []uint64{7}}, SimpleTag: []mkvSimpleTag{tagString("ARTIST", "track")}},
		{Targets: mkvTargets{TargetTypeValue: []uint64{30}}, SimpleTag: []mkvSimpleTag{tagString("GENRE", "song")}},
		{Targets: mkvTargets{TargetTypeValue: []uint64{50}, TagChapterUID: []uint64{0}}, SimpleTag: []mkvSimpleTag{
			tagString("TITLE", "tag title"),
			tagString("TITLE", "second title"),
			tagString("DATE", "1999"),
			tagString("DATE", "1998"),
			tagString("ARTIST", "artist"),
			tagString("ARTIST", "another"),
			tagString("COMMENT", ""),
			{TagName: "GENRE", TagBinary: []byte{1}},
			tagString("DESCRIPTION", "\xff"),
		}},
		{Targets: file, SimpleTag: []mkvSimpleTag{tagString("DATE_RELEASED", "2004")}},
	}}}
	md := mkvMetadata("info title", tags, nil)
	if md.Title != "tag title" || md.Artist != "artist" || md.Date != "2004" {
		t.Fatalf("read %q %q %q", md.Title, md.Artist, md.Date)
	}
	if md.Genre != "" || md.Comment != "" || md.Description != "" {
		t.Fatalf("read a tag that is not file-level text: %+v", md)
	}
	var kept []string
	for _, st := range md.mkvTags {
		kept = append(kept, st.TagName)
	}
	want := []string{"TITLE", "DATE", "ARTIST", "COMMENT", "GENRE", "DESCRIPTION", "DATE"}
	if !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
	// DATE alone is the date; Info/Title stands when no TITLE tag does, and
	// is not read when it is not text.
	md = mkvMetadata("info", []mkvTagsElement{{Tag: []mkvTag{{SimpleTag: []mkvSimpleTag{tagString("DATE", "1999")}}}}}, nil)
	if md.Title != "info" || md.Date != "1999" || len(md.mkvTags) != 0 {
		t.Fatalf("read %+v", md)
	}
	if md := mkvMetadata("\xff", nil, nil); md.Title != "" {
		t.Fatalf("an Info/Title that is not UTF-8 was read: %q", md.Title)
	}
}

func TestTheMatroskaCoverIsTheNormalCover(t *testing.T) {
	jpeg, png := []byte{0xff, 0xd8}, []byte{0x89, 'P'}
	files := func(f ...mkvAttachedFile) []mkvAttachments {
		return []mkvAttachments{{AttachedFile: f}}
	}
	font := mkvAttachedFile{FileName: "font.ttf", FileMimeType: "font/ttf", FileData: []byte{1}}
	small := mkvAttachedFile{FileName: "small_cover.jpg", FileMimeType: "IMAGE/JPG", FileData: jpeg}
	empty := mkvAttachedFile{FileName: "cover.jpg", FileMimeType: MIMEJPEG}
	normal := mkvAttachedFile{FileName: "cover.png", FileMimeType: MIMEPNG, FileData: png}
	if pic := mkvCover(files(font, empty, small, normal)); pic == nil || !bytes.Equal(pic.Data, png) {
		t.Fatalf("cover = %+v, want cover.png", pic)
	}
	if pic := mkvCover(files(font, small)); pic == nil || pic.MIMEType != MIMEJPEG {
		t.Fatalf("cover = %+v, want the first picture", pic)
	}
	if pic := mkvCover(files(font)); pic != nil {
		t.Fatalf("a font was read as a cover: %+v", pic)
	}
	if mkvAttachmentsOf(nil, "matroska") != nil || mkvTagsOf(nil) != nil || mkvTitleOf(nil) != "" {
		t.Fatal("no metadata wrote something")
	}
	if mkvTagsOf(&Metadata{Title: "only a title"}) != nil {
		t.Fatal("a title alone wrote a Tags element; it belongs in Info")
	}
}

// ⛔ ebml-go allocates an element's value before it reads it. A file of a few
// dozen bytes announcing a 100 MB attachment must cost a few dozen bytes.
func TestAHostileAttachmentSizeAllocatesNothing(t *testing.T) {
	head := marshalDoc(t, &struct {
		Header mkvHeader `ebml:"EBML"`
	}{mkvHeader{DocType: "matroska"}})
	unknown := []byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	data := slices.Concat(head,
		[]byte{0x18, 0x53, 0x80, 0x67}, unknown, // Segment
		[]byte{0x19, 0x41, 0xa4, 0x69}, unknown, // Attachments
		[]byte{0x61, 0xa7}, unknown, // AttachedFile
		[]byte{0x46, 0x5c, 0x01, 0x00, 0x00, 0x00, 0x05, 0xf5, 0xe1, 0x00}, // FileData of 100 000 000 bytes
		[]byte{1, 2, 3}) // of which three are there
	for what, read := range map[string]func([]byte) error{
		"Demux":     func(b []byte) error { _, err := Demux(b); return err },
		"NewReader": func(b []byte) error { _, err := NewReader(b); return err },
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		err := read(data)
		runtime.ReadMemStats(&after)
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 10<<20 {
			t.Fatalf("%s: reading %d bytes allocated %d", what, len(data), grew)
		}
		if !errors.Is(err, ebml.ErrTooLargeElement) {
			t.Fatalf("%s: %v, want the size refused as too large", what, err)
		}
	}
	// Above ebml-go's own limit, its limit stands; at zero, which would turn
	// the limit off, a floor of one byte does.
	for _, n := range []int{0, 1 << 30} {
		if err := mkvLeafLimit(n)(&ebml.UnmarshalOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheMetadataTableIsWhatTheCodeWrites holds the README's field table to
// the two mapping tables, in its own section, so that a name added to the code
// and not to the table fails here.
func TestTheMetadataTableIsWhatTheCodeWrites(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	// The newline in front tells it from the "### File metadata" table above.
	section, ok := sectionOf(string(readme), "\n## File metadata\n")
	if !ok {
		t.Fatal("the README has no File metadata section")
	}
	var names []string
	for _, item := range ilstTextItems {
		names = append(names, item.name)
	}
	for _, tag := range mkvTextTags {
		names = append(names, tag.name)
	}
	names = append(names, "covr", mkvTagTitle, mkvTagDateFallback)
	for _, name := range names {
		if !strings.Contains(section, "`"+strings.ReplaceAll(name, "\xa9", "©")+"`") {
			t.Errorf("the File metadata section does not name %q", name)
		}
	}
}

func TestMetadataIsZero(t *testing.T) {
	if !(Metadata{}).IsZero() {
		t.Fatal("the zero Metadata states something")
	}
	for _, md := range []Metadata{
		{Title: "x"}, {Artist: "x"}, {Album: "x"}, {Date: "x"}, {Comment: "x"},
		{Description: "x"}, {Genre: "x"}, {Encoder: "x"}, {Cover: &Picture{}},
		{mp4Items: [][]byte{{1}}}, {mkvTags: []mkvSimpleTag{{}}},
	} {
		if md.IsZero() {
			t.Errorf("%+v counted as nothing", md)
		}
	}
	if normalMIME(" Image/JPG ") != MIMEJPEG || normalMIME("image/webp") != "" {
		t.Fatal("normalMIME")
	}
}
