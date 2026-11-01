// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Eyevinn/mp4ff/mp4"
)

// codecTx3g is the sample entry of 3GPP timed text (3GPP TS 26.245, clause 5.16;
// registered in MP4RA as "Timed Text stream", specification 3GPP). ffmpeg calls
// the same thing mov_text.
//
// It is carried as it stands from MP4 to MP4 -- the sample entry as CodecConfig,
// the samples untouched -- and converted to and from WebVTT only where a caller
// asks for it: WebMMuxer turns it into WebVTT because Matroska has no tx3g
// mapping, and SubtitlesAs does either conversion for a remux.
//
// ⛔ The conversions are lossy, and in different directions; the loss is
// documented in the README and pinned by the tests. tx3g to WebVTT keeps the
// text and the bold, italic and underline of the style records. Everything else
// a tx3g track says -- fonts, sizes, colours, justification, text boxes,
// karaoke, hyperlinks, wrap and blink -- has no WebVTT counterpart in a cue
// body. WebVTT to tx3g keeps the same, and drops the identifier, the cue
// settings, the NOTE comments, every tag but <b>, <i> and <u>, and the header's
// STYLE and REGION blocks.
const codecTx3g = "tx3g"

// tx3gEmptySample is the sample that shows nothing: a text length of zero and no
// modifier box. ffmpeg's movenc writes it to end a cue (mov_write_subtitle_end_packet)
// and its decoder reads a two byte sample of zeros as exactly that.
var tx3gEmptySample = []byte{0, 0}

// The style flags of a tx3g StyleRecord (TS 26.245, clause 5.15).
const (
	tx3gBold      = 1
	tx3gItalic    = 2
	tx3gUnderline = 4
	tx3gFaceMask  = tx3gBold | tx3gItalic | tx3gUnderline
)

// tx3gMinEntry is the size of a tx3g sample entry's content before its font
// table: the six reserved bytes and the data reference index of every sample
// entry (ISO/IEC 14496-12, 8.5.2), then displayFlags (4), the two justification
// bytes (2), the background colour (4), the default text box (8) and the default
// style record (12), as TS 26.245 clause 5.16 lists them.
const tx3gMinEntry = 8 + 4 + 2 + 4 + 8 + 12

// tx3gDefaultEntry is the sample entry written for a tx3g track whose caller
// states none, as when a WebVTT track is converted: the values ffmpeg's movtextenc
// writes for a track without ASS styles (encode_sample_description) -- no display
// flags, centred horizontally and at the bottom, a transparent background, a text
// box covering the track, the style of white 18 pixel text in font 1, and a font
// table naming that font "Serif".
var tx3gDefaultEntry = func() []byte {
	var b bytes.Buffer
	b.Write([]byte{0, 0, 0, 0, 0, 0, 0, 1}) // reserved, data reference index 1
	b.Write([]byte{0, 0, 0, 0})             // displayFlags
	b.Write([]byte{0x01, 0xFF})             // horizontal centred, vertical bottom
	b.Write([]byte{0, 0, 0, 0})             // background-color-rgba
	b.Write(make([]byte, 8))                // BoxRecord
	b.Write(tx3gStyleRecord(0, 0, 0))       // default style record
	b.Write([]byte{0, 0, 0, 18, 'f', 't', 'a', 'b', 0, 1, 0, 1, 5, 'S', 'e', 'r', 'i', 'f'})
	return b.Bytes()
}()

// tx3gStyleRecord is a StyleRecord (TS 26.245, clause 5.15) in font 1 at the
// size and colour of the default entry.
func tx3gStyleRecord(start, end uint16, flags byte) []byte {
	rec := make([]byte, 12)
	binary.BigEndian.PutUint16(rec[0:], start)
	binary.BigEndian.PutUint16(rec[2:], end)
	binary.BigEndian.PutUint16(rec[4:], 1)
	rec[6], rec[7] = flags, 18
	copy(rec[8:], []byte{0xFF, 0xFF, 0xFF, 0xFF})
	return rec
}

// checkTx3gEntry refuses a sample entry that cannot be a tx3g one. What follows
// the fixed fields must be whole boxes, since the font table is a box, and a
// payload that is not is a description a player would read past its end.
func checkTx3gEntry(payload []byte) error {
	if len(payload) < tx3gMinEntry {
		return fmt.Errorf("%w: a tx3g sample entry holds %d bytes, fewer than the %d its fixed fields take",
			ErrTrackConfig, len(payload), tx3gMinEntry)
	}
	rest := payload[tx3gMinEntry:]
	for len(rest) > 0 {
		if len(rest) < 8 {
			return fmt.Errorf("%w: a tx3g sample entry ends with %d bytes that are not a box",
				ErrTrackConfig, len(rest))
		}
		size := uint64(binary.BigEndian.Uint32(rest))
		if size < 8 || size > uint64(len(rest)) {
			return fmt.Errorf("%w: a %q box of a tx3g sample entry states size %d with %d bytes left",
				ErrTrackConfig, rest[4:8], size, len(rest))
		}
		rest = rest[size:]
	}
	return nil
}

// describeTx3g writes the tx3g sample entry a caller hands over as it stands, or
// the default one when it hands over none.
//
// mp4ff has no tx3g box -- 0.61.0 decodes one as an unknown box and writes it
// back byte for byte, which is all a pass-through needs -- so the entry is an
// unknown box holding the payload.
func describeTx3g(trak *mp4.TrakBox, cfg TrackConfig) error {
	payload := cfg.CodecConfig
	if len(payload) == 0 {
		payload = tx3gDefaultEntry
	}
	if err := checkTx3gEntry(payload); err != nil {
		return err
	}
	trak.Mdia.Minf.Stbl.Stsd.AddChild(mp4.CreateUnknownBox(codecTx3g, uint64(8+len(payload)), bytes.Clone(payload)))
	return nil
}

// tx3gEntry is the tx3g sample entry of a track, or nil when its first sample
// description is not one.
func tx3gEntry(stsd *mp4.StsdBox) *mp4.UnknownBox {
	if len(stsd.Children) == 0 {
		return nil
	}
	entry, _ := stsd.Children[0].(*mp4.UnknownBox)
	if entry == nil || entry.Type() != codecTx3g {
		return nil
	}
	return entry
}

// dropTx3gEndMarkers leaves out the samples that show nothing for no time.
//
// ⛔ ffmpeg ends every mov_text track with one: its movenc writes a terminating
// empty sample when the file is closed (mov_write_trailer calls
// mov_write_subtitle_end_packet), which has the two zero bytes of an empty text
// and, being the last, no duration. Every mov_text track ffmpeg writes ends this
// way (the fixture's sixth sample), so it is the ordinary case, not a defect. It
// changes nothing a player shows, and every
// muxer here refuses a sample that lasts no time, so it is left behind rather
// than carried: the track ends where its last cue does, which is where the
// marker stood.
func dropTx3gEndMarkers(samples []Sample) []Sample {
	return slices.DeleteFunc(samples, func(s Sample) bool {
		return s.Duration == 0 && bytes.Equal(s.Data, tx3gEmptySample)
	})
}

// tx3gSample is what one tx3g sample says that WebVTT can state.
type tx3gSample struct {
	text string
	// faces holds one style flag byte per character of the text, as the style
	// records paint them; nil when the sample has no styl box.
	faces []byte
	// karaoke says the sample holds a krok box, whose times count from the
	// start of the sample and so do not survive the sample being shortened.
	karaoke bool
}

// parseTx3gSample reads a text sample (TS 26.245, clause 5.17): a 16-bit count of
// the bytes of the string, the string, then modifier boxes to the end of the
// sample. Unrecognised boxes are skipped, as the clause says.
//
// The string is UTF-8, or UTF-16 when it opens with a byte order mark (clause
// 5.17, with 5.3). Style offsets count characters, so they are applied over
// the decoded runes.
func parseTx3gSample(data []byte) (tx3gSample, error) {
	var s tx3gSample
	if len(data) < 2 {
		return s, fmt.Errorf("%w: a tx3g sample holds %d bytes, fewer than the two of its text length",
			ErrSample, len(data))
	}
	n := int(binary.BigEndian.Uint16(data))
	if 2+n > len(data) {
		return s, fmt.Errorf("%w: a tx3g sample states %d bytes of text and holds %d", ErrSample, n, len(data)-2)
	}
	text, err := tx3gString(data[2 : 2+n])
	if err != nil {
		return s, err
	}
	s.text = text
	var styles []byte
	rest := data[2+n:]
	for len(rest) > 0 {
		if len(rest) < 8 {
			return s, fmt.Errorf("%w: a tx3g sample ends with %d bytes that are not a box", ErrSample, len(rest))
		}
		size, header := uint64(binary.BigEndian.Uint32(rest)), uint64(8)
		switch size {
		case 0: // to the end of the sample
			size = uint64(len(rest))
		case 1:
			if len(rest) < 16 {
				return s, fmt.Errorf("%w: a tx3g sample ends inside a box header", ErrSample)
			}
			size, header = binary.BigEndian.Uint64(rest[8:]), 16
		}
		if size < header || size > uint64(len(rest)) {
			return s, fmt.Errorf("%w: a %q box of a tx3g sample states size %d with %d bytes left",
				ErrSample, rest[4:8], size, len(rest))
		}
		body := rest[header:size]
		switch string(rest[4:8]) {
		case "styl":
			if styles, err = paintTx3gStyles(body, utf8.RuneCountInString(s.text), styles); err != nil {
				return s, err
			}
		case "krok":
			s.karaoke = true
		}
		rest = rest[size:]
	}
	s.faces = styles
	return s, nil
}

// tx3gString decodes the string of a text sample. It refuses bytes that are not
// text in the encoding they state, rather than reading them as something else.
func tx3gString(b []byte) (string, error) {
	if len(b) >= 2 && (b[0] == 0xFE && b[1] == 0xFF || b[0] == 0xFF && b[1] == 0xFE) {
		var order binary.ByteOrder = binary.BigEndian
		if b[0] == 0xFF {
			order = binary.LittleEndian
		}
		body := b[2:]
		if len(body)%2 != 0 {
			return "", fmt.Errorf("%w: a UTF-16 tx3g string holds an odd number of bytes", ErrSample)
		}
		units := make([]uint16, len(body)/2)
		for i := range units {
			units[i] = order.Uint16(body[2*i:])
		}
		return string(utf16.Decode(units)), nil
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%w: a tx3g string is not UTF-8, and states no byte order mark", ErrSample)
	}
	return string(b), nil
}

// paintTx3gStyles applies the style records of a styl box to the face of every
// character they cover, over what earlier records of the sample painted. A
// record that ends before it starts is refused; one that runs past the text is
// cut at the end of it, and one that covers no character changes nothing.
func paintTx3gStyles(body []byte, chars int, faces []byte) ([]byte, error) {
	if len(body) < 2 {
		return nil, fmt.Errorf("%w: a styl box holds no entry count", ErrSample)
	}
	count := int(binary.BigEndian.Uint16(body))
	if len(body)-2 < count*12 {
		return nil, fmt.Errorf("%w: a styl box states %d style records and holds %d bytes of them",
			ErrSample, count, len(body)-2)
	}
	if faces == nil {
		faces = make([]byte, chars)
	}
	for i := range count {
		rec := body[2+12*i:]
		start, end := int(binary.BigEndian.Uint16(rec)), int(binary.BigEndian.Uint16(rec[2:]))
		if end < start {
			return nil, fmt.Errorf("%w: style record %d ends at character %d, before it starts at %d",
				ErrSample, i+1, end, start)
		}
		for c := start; c < min(end, chars); c++ {
			faces[c] = rec[6] & tx3gFaceMask
		}
	}
	return faces, nil
}

// webvttOfTx3g is the text of a cue as WebVTT writes it: the characters
// escaped as a cue body requires (WebVTT, "WebVTT cue text": & and < would
// start a character reference or a tag, and > would end one, or begin the
// --> a cue's own timing line is cut at), each run of the same face wrapped in
// <b>, <i> and <u>, nested in that order and closed in the other.
//
// A line terminator is a line feed whatever it was (U+0085, U+2028 and U+2029
// are line breaks in TS 26.245 clause 5.2), a carriage return is dropped as
// ffmpeg's movtextdec drops it, and blank lines go: an empty line ends a cue in
// WebVTT, so a cue body cannot hold one.
func webvttOfTx3g(s tx3gSample) string {
	var out strings.Builder
	var open byte
	lastNewline := true // swallows a leading one, and so a blank line
	setFace := func(face byte) {
		if face == open {
			return
		}
		for _, t := range []struct {
			flag byte
			name string
		}{{tx3gUnderline, "u"}, {tx3gItalic, "i"}, {tx3gBold, "b"}} {
			if open&t.flag != 0 {
				out.WriteString("</" + t.name + ">")
			}
		}
		for _, t := range []struct {
			flag byte
			name string
		}{{tx3gBold, "b"}, {tx3gItalic, "i"}, {tx3gUnderline, "u"}} {
			if face&t.flag != 0 {
				out.WriteString("<" + t.name + ">")
			}
		}
		open = face
	}
	for i, r := range []rune(s.text) {
		var face byte
		if i < len(s.faces) {
			face = s.faces[i]
		}
		switch r {
		case '\r':
			continue
		case '\n', 0x85, 0x2028, 0x2029:
			if lastNewline {
				continue
			}
			setFace(0)
			out.WriteByte('\n')
			lastNewline = true
			continue
		}
		setFace(face)
		lastNewline = false
		switch r {
		case '&':
			out.WriteString("&amp;")
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		case 0:
			out.WriteRune(utf8.RuneError)
		default:
			out.WriteRune(r)
		}
	}
	setFace(0)
	return strings.TrimRight(out.String(), "\n")
}

// webvttSampleOfTx3g turns one tx3g sample into the wvtt sample that shows the
// same text over the same stretch: a vtte box when it shows none.
func webvttSampleOfTx3g(data []byte) ([]byte, error) {
	s, err := parseTx3gSample(data)
	if err != nil {
		return nil, err
	}
	text := webvttOfTx3g(s)
	if text == "" {
		return vtteSample, nil
	}
	var buf bytes.Buffer
	// The box encoders fail only when their writer does.
	_ = encodeBox(cueBox(&webvttCue{text: text}), &buf)
	return buf.Bytes(), nil
}

// tx3gOfWebVTT is the plain text of a cue and the faces its <b>, <i> and <u>
// tags give each character. Every other tag -- <c>, <v>, <lang>, <ruby>, the
// timestamps of a karaoke cue -- is dropped with the text inside it kept; the
// character references WebVTT defines are turned back into the characters.
// A < with no > after it is the character itself, which is what a WebVTT parser
// does with it.
func tx3gOfWebVTT(text string) ([]rune, []byte) {
	var (
		chars []rune
		faces []byte
		depth [3]int // <b>, <i>, <u> open now
	)
	face := func() byte {
		var f byte
		for i, flag := range []byte{tx3gBold, tx3gItalic, tx3gUnderline} {
			if depth[i] > 0 {
				f |= flag
			}
		}
		return f
	}
	put := func(r rune) {
		chars = append(chars, r)
		faces = append(faces, face())
	}
	rest := text
	for rest != "" {
		r, size := utf8.DecodeRuneInString(rest)
		switch {
		case r == '<' && strings.IndexByte(rest, '>') > 0:
			tag := rest[1:strings.IndexByte(rest, '>')]
			rest = rest[strings.IndexByte(rest, '>')+1:]
			closing := strings.HasPrefix(tag, "/")
			name := strings.TrimPrefix(tag, "/")
			if end := strings.IndexAny(name, " \t.\n"); end >= 0 {
				name = name[:end]
			}
			if i := strings.Index("biu", name); len(name) == 1 && i >= 0 {
				if !closing {
					depth[i]++
				} else if depth[i] > 0 {
					depth[i]--
				}
			}
			continue
		case r == '&':
			if ref, n, ok := webvttReference(rest); ok {
				put(ref)
				rest = rest[n:]
				continue
			}
		case r == '\r':
			// A carriage return, alone or before a line feed, is one line end.
			rest = strings.TrimPrefix(rest[size:], "\n")
			put('\n')
			continue
		}
		put(r)
		rest = rest[size:]
	}
	return chars, faces
}

// webvttReference reads one of the character references WebVTT cue text defines
// (WebVTT, "WebVTT cue text parsing rules": &amp; &lt; &gt; &lrm; &rlm; &nbsp;)
// and says how many bytes it took.
func webvttReference(s string) (rune, int, bool) {
	for _, ref := range []struct {
		name string
		char rune
	}{{"&amp;", '&'}, {"&lt;", '<'}, {"&gt;", '>'}, {"&lrm;", 0x200E}, {"&rlm;", 0x200F}, {"&nbsp;", 0xA0}} {
		if strings.HasPrefix(s, ref.name) {
			return ref.char, len(ref.name), true
		}
	}
	return 0, 0, false
}

// tx3gSampleOfWebVTT turns the cues one wvtt sample shows into the tx3g sample
// that shows the same text: the texts of the cues one under another, since a
// text sample holds one string, and a styl box for the faces. A sample showing no
// cue is the empty one.
func tx3gSampleOfWebVTT(data []byte) ([]byte, error) {
	cues, err := readWebVTTSample(data)
	if err != nil {
		return nil, err
	}
	var (
		chars []rune
		faces []byte
	)
	for _, c := range cues {
		cc, cf := tx3gOfWebVTT(c.text)
		if len(chars) > 0 && len(cc) > 0 {
			chars, faces = append(chars, '\n'), append(faces, 0)
		}
		chars, faces = append(chars, cc...), append(faces, cf...)
	}
	if len(chars) == 0 {
		return tx3gEmptySample, nil
	}
	text := string(chars)
	if len(text) > 0xFFFF {
		return nil, fmt.Errorf("%w: a cue of %d bytes does not fit a tx3g sample, whose text length has 16 bits",
			ErrSample, len(text))
	}
	out := binary.BigEndian.AppendUint16(nil, uint16(len(text)))
	out = append(out, text...)
	var records [][]byte
	for i := 0; i < len(faces); {
		j := i
		for j < len(faces) && faces[j] == faces[i] {
			j++
		}
		if faces[i] != 0 {
			// The text fits 16 bits of bytes, so every offset in it fits the
			// 16 bits of a style record.
			records = append(records, tx3gStyleRecord(uint16(i), uint16(j), faces[i]))
		}
		i = j
	}
	if len(records) > 0 {
		out = binary.BigEndian.AppendUint32(out, uint32(10+12*len(records)))
		out = append(out, "styl"...)
		out = binary.BigEndian.AppendUint16(out, uint16(len(records)))
		for _, rec := range records {
			out = append(out, rec...)
		}
	}
	return out, nil
}

// convertSubtitles rewrites a timed-text track as the other of tx3g and wvtt,
// sample for sample, so that the timeline -- every duration -- stays what it was.
// A track that is neither, or is already the one asked for, is returned as it
// was. TTML is refused: it has no counterpart in either.
func convertSubtitles(t sourceTrack, to string) (sourceTrack, error) {
	from := strings.ToLower(t.cfg.Codec)
	if from == to || !isTimedText(from) {
		return t, nil
	}
	var convert func([]byte) ([]byte, error)
	switch {
	case from == codecTx3g && to == codecWebVTT:
		convert = webvttSampleOfTx3g
	case from == codecWebVTT && to == codecTx3g:
		convert = tx3gSampleOfWebVTT
	default:
		return t, fmt.Errorf("%w: track %d is %s, which cannot be rewritten as %s",
			ErrUnsupportedCodec, t.id, from, to)
	}
	samples := make([]Sample, len(t.samples))
	for i, s := range t.samples {
		data, err := convert(s.Data)
		if err != nil {
			return t, fmt.Errorf("sample %d of track %d: %w", i+1, t.id, err)
		}
		s.Data = data
		samples[i] = s
	}
	t.samples, t.cfg.Codec, t.cfg.CodecConfig = samples, to, nil
	return t, nil
}
