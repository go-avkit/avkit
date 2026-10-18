// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The sample entries a subtitle track is written with. Every timed-text track
// this package reads arrives as one of them, whatever container it came from:
// WebVTT, and SRT turned into WebVTT, as wvtt; TTML as stpp.
const (
	codecWebVTT = "wvtt"
	codecTTML   = "stpp"
)

// webvttSignature opens every WebVTT file, and so every configuration a wvtt
// sample entry carries in its vttC box (ISO/IEC 14496-30, 7.4).
const webvttSignature = "WEBVTT"

// The Matroska codec ids of timed text this package reads.
//
// S_TEXT/WEBVTT is the Matroska mapping (codec specs, "S_TEXT/WEBVTT"; the
// subtitles section, "WebVTT"): the cue text in the block, its settings,
// identifier and the comments before it in a BlockAddition of ID 1, and the
// file header in CodecPrivate.
//
// D_WEBVTT/* is the older WebM mapping (webmproject.org container guidelines,
// "WebVTT"): identifier, settings and text in the block, one per line, and no
// CodecPrivate. It is the one ffmpeg reads and writes, in .mkv as in .webm.
const (
	mkvWebVTT     = "S_TEXT/WEBVTT"
	mkvSRT        = "S_TEXT/UTF8"
	webmWebVTT    = "D_WEBVTT/SUBTITLES"
	webmWebVTTAll = "D_WEBVTT/"
)

// mkvWebVTTAddition is the BlockAddID of the BlockMore an S_TEXT/WEBVTT block
// carries its settings, identifier and comments in. The mapping says it MUST be
// 1, which is also the value an absent BlockAddID stands for.
const mkvWebVTTAddition = 1

// webvttCue is one WebVTT cue, with what each container keeps of it.
//
// ISO/IEC 14496-30 states a cue as a vttc box -- iden for the identifier, sttg
// for the settings, payl for the text -- and the NOTE blocks before it as vtta
// boxes. Matroska states the same four things in a block and its addition. The
// times are in the track's own units, and end is exclusive.
type webvttCue struct {
	id, settings, text string
	comments           []string
	start, end         uint64
	// source is the vsid a cue split across several samples carries, so that
	// the pieces can be told to be one cue; zero when it states none.
	source uint32
}

// key is what makes two pieces of a split cue the same cue. A source id, when
// there is one, is the cue's identity; without one, a cue is what it says.
func (c *webvttCue) key() string {
	return strconv.FormatUint(uint64(c.source), 10) + "\x00" + c.id + "\x00" + c.settings + "\x00" + c.text
}

// readWebVTTSample takes one wvtt sample apart into the cues it shows.
//
// ISO/IEC 14496-30 makes a sample either one vtte box -- nothing is shown --
// or one or more vttc and vtta boxes. Anything else in a sample is refused,
// because a cue read out of a sample that does not follow that rule is a cue
// read by guess.
//
// A vtta box is a NOTE block, and it belongs to the cue that follows it, which
// is where Matroska keeps it too. One that precedes no cue has no cue to go with
// and is dropped: no container this package writes has anywhere else for it.
func readWebVTTSample(data []byte) ([]webvttCue, error) {
	r := bytes.NewReader(data)
	var (
		cues     []webvttCue
		comments []string
		empty    bool
	)
	for r.Len() > 0 {
		pos := uint64(len(data) - r.Len())
		box, err := mp4.DecodeBox(pos, r)
		if err != nil {
			return nil, fmt.Errorf("%w: a wvtt sample does not decode at byte %d: %v", ErrSample, pos, err)
		}
		switch b := box.(type) {
		case *mp4.VtteBox:
			empty = true
		case *mp4.VttaBox:
			comments = append(comments, b.CueAdditionalText)
		case *mp4.VttcBox:
			cues = append(cues, cueOfBox(b, comments))
			comments = nil
		default:
			return nil, fmt.Errorf("%w: a wvtt sample holds a %q box, which ISO/IEC 14496-30 does not put in one",
				ErrSample, box.Type())
		}
	}
	if empty && (len(cues) > 0 || len(comments) > 0) {
		return nil, fmt.Errorf("%w: a wvtt sample holds a vtte box beside cues; it must be alone", ErrSample)
	}
	return cues, nil
}

// cueOfBox is what one vttc box says. A cue time box (ctim) is not read: it
// states where a split cue was in the original document, which a cue's own
// start already says once the pieces are joined again.
func cueOfBox(b *mp4.VttcBox, comments []string) webvttCue {
	c := webvttCue{comments: comments}
	if b.Vsid != nil {
		c.source = b.Vsid.SourceID
	}
	if b.Iden != nil {
		c.id = b.Iden.CueID
	}
	if b.Sttg != nil {
		c.settings = b.Sttg.Settings
	}
	if b.Payl != nil {
		c.text = b.Payl.CueText
	}
	return c
}

// encodeBox exists so the failure it guards can be tested: the boxes are
// encoded into a buffer, which cannot fail, and mp4ff's encoders only report the
// writer's failures.
var encodeBox = func(box mp4.Box, w io.Writer) error { return box.Encode(w) }

// webvttSamples turns cues into the samples of a wvtt track, which unlike the
// blocks of a Matroska track cover the whole timeline from zero without a gap.
//
// ISO/IEC 14496-30 does not let samples overlap, and cues may: a roll-up
// caption keeps the line before on screen while the next arrives. So the
// timeline is cut wherever a cue starts or ends, every stretch becomes a sample
// holding the cues shown during it -- a vtte box when there are none -- and a
// cue shown across several stretches is repeated in each, with a vsid saying
// the copies are one cue. That is what lets the cue be put back together, by
// this package's WebMMuxer among others, rather than shown as several.
//
// The NOTE blocks before a cue go into the first sample that shows it, ahead of
// its vttc box, as 14496-30's vtta boxes.
func webvttSamples(cues []webvttCue) ([]Sample, error) {
	slices.SortStableFunc(cues, func(a, b webvttCue) int { return cmp.Compare(a.start, b.start) })
	edges := []uint64{0}
	for _, c := range cues {
		edges = append(edges, c.start, c.end)
	}
	slices.Sort(edges)
	edges = slices.Compact(edges)
	// A cue shown in more than one stretch carries a source id, unique in the
	// track; one shown in a single stretch needs none.
	var nextSource uint32
	for i := range cues {
		first, _ := slices.BinarySearch(edges, cues[i].start)
		if edges[first+1] < cues[i].end {
			nextSource++
			cues[i].source = nextSource
		}
	}
	var out []Sample
	written := make([]bool, len(cues))
	for i := 0; i+1 < len(edges); i++ {
		from, to := edges[i], edges[i+1]
		if to-from > math.MaxUint32 {
			return nil, fmt.Errorf("%w: a stretch of %d ticks between two cues is longer than a sample can last",
				ErrMatroska, to-from)
		}
		var boxes bytes.Buffer
		for j := range cues {
			c := &cues[j]
			if c.start > from || c.end < to {
				continue
			}
			if !written[j] {
				for _, note := range c.comments {
					if err := encodeBox(&mp4.VttaBox{CueAdditionalText: note}, &boxes); err != nil {
						return nil, fmt.Errorf("container: encode vtta: %w", err)
					}
				}
				written[j] = true
			}
			if err := encodeBox(cueBox(c), &boxes); err != nil {
				return nil, fmt.Errorf("container: encode vttc: %w", err)
			}
		}
		// Two empty stretches never follow one another: an edge after an
		// empty stretch is where a cue starts, since only a start can end
		// one. So each vtte sample covers a whole gap on its own.
		data := vtteSample
		if boxes.Len() > 0 {
			data = boxes.Bytes()
		}
		out = append(out, Sample{Data: data, Duration: uint32(to - from), Sync: true})
	}
	return out, nil
}

// vtteSample is the sample that shows nothing: one empty vtte box.
var vtteSample = []byte{0, 0, 0, 8, 'v', 't', 't', 'e'}

// cueBox is a cue as ISO/IEC 14496-30 states it, its boxes in the order the
// standard lists them: source id, identifier, settings, then the text, which is
// the one box a vttc must hold.
func cueBox(c *webvttCue) *mp4.VttcBox {
	b := &mp4.VttcBox{}
	if c.source != 0 {
		b.AddChild(&mp4.VsidBox{SourceID: c.source})
	}
	if c.id != "" {
		b.AddChild(&mp4.IdenBox{CueID: c.id})
	}
	if c.settings != "" {
		b.AddChild(&mp4.SttgBox{Settings: c.settings})
	}
	b.AddChild(&mp4.PaylBox{CueText: c.text})
	return b
}

// cueAssembler puts back together the cues a wvtt track split across samples,
// and hands each one over once it is known how long it lasts.
//
// A cue's end is only known when a sample arrives that does not show it, so a
// cue is held until then. Cues are handed over in the order they start, which
// is the order Matroska wants its blocks in: a cue that has ended waits behind
// one that started before it and is still on screen.
type cueAssembler struct {
	open []*assembledCue
}

type assembledCue struct {
	webvttCue
	key    string
	closed bool
}

// add takes the cues one sample shows, from start to end, and returns those it
// has finished.
func (a *cueAssembler) add(start, end uint64, cues []webvttCue) []webvttCue {
	continued := make([]bool, len(cues))
	for _, o := range a.open {
		if o.closed {
			continue
		}
		o.closed = true
		for i := range cues {
			if !continued[i] && o.end == start && cues[i].key() == o.key {
				o.end, o.closed, continued[i] = end, false, true
				break
			}
		}
	}
	for i, c := range cues {
		if continued[i] {
			continue
		}
		c.start, c.end = start, end
		a.open = append(a.open, &assembledCue{webvttCue: c, key: c.key()})
	}
	var done []webvttCue
	for len(a.open) > 0 && a.open[0].closed {
		done = append(done, a.open[0].webvttCue)
		a.open = a.open[1:]
	}
	return done
}

// drain finishes every cue still held: the track has ended, and so have they.
func (a *cueAssembler) drain() []webvttCue {
	var done []webvttCue
	for _, o := range a.open {
		done = append(done, o.webvttCue)
	}
	a.open = nil
	return done
}

// mkvCueSamples turns the blocks of a Matroska text track into the samples of a
// wvtt track: one cue per block, timed by the block's timestamp and the
// BlockDuration the mappings require, then laid out on a timeline with no gap.
func mkvCueSamples(te mkvTrackEntry, blocks []mkvBlock, perFrame int64, timescale uint32) ([]Sample, error) {
	track := te.TrackNumber
	if len(blocks) == 0 {
		return nil, fmt.Errorf("%w: track %d has no block", ErrNoSamples, track)
	}
	cues := make([]webvttCue, 0, len(blocks))
	for i, b := range blocks {
		if len(b.frames) != 1 {
			return nil, fmt.Errorf("%w: block %d of text track %d laces %d cues; each cue is a block of its own",
				ErrMatroska, i+1, track, len(b.frames))
		}
		if b.time < 0 {
			return nil, fmt.Errorf("%w: cue %d of track %d starts %d ticks before the segment does",
				ErrMatroska, i+1, track, -b.time)
		}
		// A cue says how long it is shown, which is why the mappings put it
		// in a block group; a track whose frames all last alike may state
		// that once instead, as DefaultDuration.
		duration := b.stated
		if duration <= 0 {
			duration = perFrame
		}
		if duration <= 0 {
			return nil, fmt.Errorf("%w: cue %d of track %d states no duration, and nothing else in the file does",
				ErrMatroska, i+1, track)
		}
		c, err := mkvCue(te.CodecID, b, timescale)
		if err != nil {
			return nil, fmt.Errorf("cue %d of track %d: %w", i+1, track, err)
		}
		c.start, c.end = uint64(b.time), uint64(b.time+duration)
		cues = append(cues, c)
	}
	return webvttSamples(cues)
}

// mkvCue reads one cue in whichever of the three text mappings its track uses.
func mkvCue(codecID string, b mkvBlock, timescale uint32) (webvttCue, error) {
	data := b.frames[0]
	switch codecID {
	case mkvWebVTT:
		c := webvttCue{text: string(data)}
		if err := readMatroskaCueAddition(&c, b.additional); err != nil {
			return webvttCue{}, err
		}
		// The mapping makes timestamps inside the text count from the
		// block; an MP4 counts them from the start of the presentation. A
		// block never starts before zero, so moving them later cannot fail.
		c.text, _ = shiftCueTimestamps(c.text, unitsToDuration(uint64(b.time), timescale))
		return c, nil
	case mkvSRT:
		// SRT's text goes in as it stands: its <b>, <i> and <u> are WebVTT's
		// own, and a <font> tag, which WebVTT does not have, is one a WebVTT
		// renderer skips while keeping the text inside it.
		return webvttCue{text: strings.TrimRight(string(data), "\r\n")}, nil
	}
	return readWebMCueBlock(data)
}

// webmCueBlock is a cue as the WebM mapping writes it: identifier, settings and
// text, one after another, each of the first two ended by a line feed even when
// it is empty, so a reader can tell an absent identifier from the text.
func webmCueBlock(c webvttCue) []byte {
	return []byte(c.id + "\n" + c.settings + "\n" + c.text)
}

// readWebMCueBlock reads a cue written the WebM way. A carriage return before
// either line feed is accepted and dropped, and so are line terminators after
// the text, as ffmpeg's reader does.
func readWebMCueBlock(data []byte) (webvttCue, error) {
	text := string(data)
	id, rest, ok := strings.Cut(text, "\n")
	if !ok {
		return webvttCue{}, fmt.Errorf("%w: a %s block has no line for its identifier", ErrMatroska, webmWebVTT)
	}
	settings, payload, ok := strings.Cut(rest, "\n")
	if !ok {
		return webvttCue{}, fmt.Errorf("%w: a %s block has no line for its settings", ErrMatroska, webmWebVTT)
	}
	return webvttCue{
		id:       strings.TrimSuffix(id, "\r"),
		settings: strings.TrimSuffix(settings, "\r"),
		text:     strings.TrimRight(payload, "\r\n"),
	}, nil
}

// matroskaCueAddition is the BlockAddition of an S_TEXT/WEBVTT block: the
// settings, then the identifier, each on a line of its own, then the comment
// blocks before the cue, separated as they were in the file by an empty line.
// A cue stating none of the three has no addition at all, which the mapping
// says means exactly that.
func matroskaCueAddition(c webvttCue) []byte {
	if c.settings == "" && c.id == "" && len(c.comments) == 0 {
		return nil
	}
	return []byte(c.settings + "\n" + c.id + "\n" + strings.Join(c.comments, "\n\n"))
}

// readMatroskaCueAddition reads what matroskaCueAddition writes. The mapping
// lets the last comment omit its line terminator, so trailing ones are dropped
// before the comments are split.
func readMatroskaCueAddition(c *webvttCue, addition []byte) error {
	if addition == nil {
		return nil
	}
	settings, rest, ok := strings.Cut(string(addition), "\n")
	if !ok {
		return fmt.Errorf("%w: an %s BlockAddition has no line for its settings", ErrMatroska, mkvWebVTT)
	}
	id, comments, ok := strings.Cut(rest, "\n")
	if !ok {
		return fmt.Errorf("%w: an %s BlockAddition has no line for its identifier", ErrMatroska, mkvWebVTT)
	}
	c.settings, c.id = strings.TrimSuffix(settings, "\r"), strings.TrimSuffix(id, "\r")
	if comments = strings.TrimRight(comments, "\r\n"); comments != "" {
		c.comments = strings.Split(comments, "\n\n")
	}
	return nil
}

// shiftCueTimestamps moves the timestamps inside a cue's text -- the
// <00:03:15.000> of a karaoke cue -- by delta.
//
// ⛔ The two mappings disagree about what those timestamps count from. In an MP4
// they are times of the presentation, as in the WebVTT file. The Matroska
// mapping says a muxer MUST make them relative to the block's own timestamp. So
// a cue crossing between the two has them rewritten, and only those: a tag that
// does not parse as a WebVTT timestamp is left exactly as it was.
//
// A timestamp the shift would put before zero is refused rather than clamped:
// it is a cue that says it shows part of itself before it starts.
func shiftCueTimestamps(text string, delta time.Duration) (string, error) {
	if delta == 0 || !strings.Contains(text, "<") {
		return text, nil
	}
	var out strings.Builder
	rest := text
	for {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			out.WriteString(rest)
			return out.String(), nil
		}
		closing := strings.IndexByte(rest[open:], '>')
		if closing < 0 {
			out.WriteString(rest)
			return out.String(), nil
		}
		tag := rest[open+1 : open+closing]
		at, hours, ok := parseCueTimestamp(tag)
		out.WriteString(rest[:open+1])
		if ok {
			moved := at + delta
			if moved < 0 {
				return "", fmt.Errorf("%w: the cue timestamp <%s> moved by %v falls before zero",
					ErrSample, tag, delta)
			}
			out.WriteString(formatCueTimestamp(moved, hours))
		} else {
			out.WriteString(tag)
		}
		out.WriteByte('>')
		rest = rest[open+closing+1:]
	}
}

// hasCueTimestamp says whether a cue's text holds a timestamp of its own.
// Moving every timestamp a millisecond later changes the text exactly when it
// holds one, and cannot fail.
func hasCueTimestamp(text string) bool {
	shifted, _ := shiftCueTimestamps(text, time.Millisecond)
	return shifted != text
}

// parseCueTimestamp reads a WebVTT timestamp: hours of two digits or more, which
// may be left out, then minutes, seconds and milliseconds of exactly two, two
// and three (WebVTT, "WebVTT timestamp"). It says whether the hours were there,
// so that a rewritten timestamp keeps the form it had.
func parseCueTimestamp(s string) (time.Duration, bool, bool) {
	clock, millis, ok := strings.Cut(s, ".")
	if !ok || len(millis) != 3 {
		return 0, false, false
	}
	parts := strings.Split(clock, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false, false
	}
	hours := len(parts) == 3
	var fields []string
	if hours {
		if len(parts[0]) < 2 {
			return 0, false, false
		}
		fields = append(fields, parts[0])
	} else {
		fields = append(fields, "00")
	}
	fields = append(fields, parts[len(parts)-2], parts[len(parts)-1], millis)
	var values [4]int64
	for i, f := range fields {
		if i > 0 && len(f) != 2+boolInt(i == 3) {
			return 0, false, false
		}
		v, err := strconv.ParseUint(f, 10, 31)
		if err != nil {
			return 0, false, false
		}
		values[i] = int64(v)
	}
	if values[1] > 59 || values[2] > 59 {
		return 0, false, false
	}
	at := time.Duration(values[0])*time.Hour + time.Duration(values[1])*time.Minute +
		time.Duration(values[2])*time.Second + time.Duration(values[3])*time.Millisecond
	return at, hours, true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// formatCueTimestamp writes a WebVTT timestamp, with hours when the original had
// them or when it needs them.
func formatCueTimestamp(at time.Duration, hours bool) string {
	ms := at.Milliseconds()
	h, m, s, f := ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000
	if hours || h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, f)
	}
	return fmt.Sprintf("%02d:%02d.%03d", m, s, f)
}

// unitsToDuration converts a time in a track's units into a time.Duration, the
// unit WebVTT timestamps are rewritten in.
func unitsToDuration(units uint64, timescale uint32) time.Duration {
	return time.Duration(scaleDuration(units, timescale, nsScale))
}

// webvttConfig is the vttC configuration a track states, or the bare signature
// when it states none: the smallest WebVTT header there is.
func webvttConfig(cfg TrackConfig) (string, error) {
	config := string(cfg.CodecConfig)
	if config == "" {
		return webvttSignature, nil
	}
	if !strings.HasPrefix(config, webvttSignature) {
		return "", fmt.Errorf("%w: a WebVTT configuration starts with %q, not %q",
			ErrTrackConfig, webvttSignature, firstLine(config))
	}
	return config, nil
}

// firstLine is the start of a text, enough to name it in an error.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 32 {
		line = line[:32]
	}
	return line
}

// describeWebVTT writes the wvtt sample entry: its vttC box holds the header of
// the WebVTT file -- the signature and any STYLE or REGION block -- which is
// what ISO/IEC 14496-30, 7.4, puts there.
func describeWebVTT(trak *mp4.TrakBox, cfg TrackConfig) error {
	config, err := webvttConfig(cfg)
	if err != nil {
		return err
	}
	entry := mp4.NewWvttBox()
	entry.AddChild(&mp4.VttCBox{Config: config})
	trak.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	return nil
}

// describeTTML writes the stpp sample entry a caller hands over, as it stands:
// its namespace, schema location and auxiliary MIME types are what the TTML
// samples were written against, and nothing here has a better idea of them.
func describeTTML(trak *mp4.TrakBox, cfg TrackConfig) error {
	if len(cfg.CodecConfig) == 0 {
		return fmt.Errorf("%w: stpp needs its sample entry as CodecConfig", ErrTrackConfig)
	}
	box, err := decodeConfigBox(codecTTML, cfg.CodecConfig, mp4.DecodeStpp)
	if err != nil {
		return err
	}
	// mp4ff hands back an stpp box for any payload it decodes.
	entry := box.(*mp4.StppBox)
	if entry.Namespace == "" {
		return fmt.Errorf("%w: stpp states no TTML namespace", ErrTrackConfig)
	}
	trak.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	return nil
}

// isTimedText says whether a codec is one whose samples are cues: shown over a
// stretch of time, rather than decoded one from another.
func isTimedText(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case codecWebVTT, codecTTML:
		return true
	}
	return false
}
