// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"
	"unicode/utf16"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Chapters: named points on a file's timeline, which a player lists to seek by.
//
// MP4. Two forms exist and this package reads both:
//
//   - Nero's chpl box in moov/udta. It is in no published specification (mp4ff's
//     ChplBox says so), but Nero, FFmpeg and GPAC write it and FFmpeg's
//     mov_read_chpl reads it: a version, flags, 4 reserved bytes, a one-byte
//     count, then per chapter a 64-bit start in units of 100 ns and a one-byte
//     length and the UTF-8 title. It states no end, so a chapter lasts until the
//     next begins. It is the form written: a count above 255 or a title above 255
//     bytes has no place in it and is refused rather than cut short.
//   - A QuickTime chapter track: a track referenced by a "chap" entry of another
//     track's tref, whose text samples (a 16-bit length, the text in UTF-8, or
//     UTF-16 behind a byte order mark, then optional atoms such as "encd") are the
//     titles, each lasting its sample's duration. FFmpeg's movenc writes one next
//     to the chpl in an MP4, and its mov_read_chapters reads it. It is read, and
//     when a file has one it is what the chapters are: it states the ends.
//
// ⛔ The chapter track is not written. A chpl alone is read by FFmpeg, GPAC and
// VLC; Apple's players look for the track. Writing one means a text track with
// its own sample entry beside the media, which none of the muxers have a place
// for yet.
//
// Matroska. RFC 9559 §5.1.7: Chapters holds EditionEntry elements, each holding
// ChapterAtom elements with a ChapterUID, a ChapterTimeStart and optionally a
// ChapterTimeEnd, both in nanoseconds and not scaled by the TimestampScale
// (§20.2.1, §20.2.2), and ChapterDisplay elements with a ChapString and a
// ChapLanguage. The chapters of the default edition are read (the one whose
// EditionFlagDefault is 1, else the first), and an ordered edition -- one whose
// chapters are the timeline, to be played in the order stated (§20.1.3) -- is
// skipped: its atoms are not points on the file's own timeline, and writing them
// as such into an MP4 would change what the file plays. Nested ChapterAtom
// elements and every ChapterDisplay after the first are not read, as FFmpeg's
// matroskadec does not.
//
// MPEG-TS has no chapters.

// ErrChapters means chapters cannot be written as they stand: starts out of
// order, an end before its start, a title that is not text, a language that is
// not a three-letter code, or more of them than the container can hold.
var ErrChapters = errors.New("container: invalid chapters")

// maxChapters bounds how many chapters a reader keeps and a writer accepts. A
// Matroska file can state as many as it has bytes for, and no player lists
// anything near this.
const maxChapters = 1 << 16

// Chapter is one chapter: a name, and where on the file's timeline it begins.
type Chapter struct {
	// Start is where the chapter begins, from the start of the file.
	Start time.Duration
	// End is where it ends. Zero means the file does not say, and the chapter
	// lasts until the next one begins, or to the end of the file. An MP4's chpl
	// states no end, so a chapter written there loses it.
	End time.Duration
	// Title is the chapter's name, as UTF-8 without NUL.
	Title string
	// Language is the ISO 639-2 code of Title ("eng"), as a Matroska file states
	// it. An MP4 states none, and an empty one is written as "und".
	Language string
}

// validate refuses what neither container could state faithfully.
func validateChapters(cs []Chapter) error {
	if len(cs) > maxChapters {
		return fmt.Errorf("%w: %d chapters, more than the %d this package writes", ErrChapters, len(cs), maxChapters)
	}
	var prev time.Duration
	for i, c := range cs {
		switch {
		case c.Start < 0:
			return fmt.Errorf("%w: chapter %d starts at %v, before the file does", ErrChapters, i+1, c.Start)
		case c.Start < prev:
			return fmt.Errorf("%w: chapter %d starts at %v, before chapter %d at %v", ErrChapters, i+1, c.Start, i, prev)
		case c.End != 0 && c.End < c.Start:
			return fmt.Errorf("%w: chapter %d ends at %v, before it starts at %v", ErrChapters, i+1, c.End, c.Start)
		case !isText(c.Title):
			return fmt.Errorf("%w: the title of chapter %d is not UTF-8 text without NUL", ErrChapters, i+1)
		case c.Language != "" && !isLanguage(c.Language):
			return fmt.Errorf("%w: the language %q of chapter %d is not a three-letter ISO 639-2 code", ErrChapters, c.Language, i+1)
		}
		prev = c.Start
	}
	return nil
}

// isLanguage reports whether s is three lowercase ASCII letters.
func isLanguage(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// setChapters is the check every muxer makes before taking chapters: they have
// to be writable, and they have to arrive before what states them is written.
func setChapters(cs []Chapter, closed, tooLate bool) ([]Chapter, error) {
	switch {
	case closed:
		return nil, ErrClosed
	case tooLate:
		return nil, fmt.Errorf("%w: chapters cannot be set once writing has begun", ErrChapters)
	}
	if err := validateChapters(cs); err != nil {
		return nil, err
	}
	return slices.Clone(cs), nil
}

// SetChapters states the chapters of the file, in a chpl box of moov/udta.
//
// The movie box goes out with the first fragment, so this has to come before the
// first sample; later it is refused. An MP4's chpl holds at most 255 chapters and
// titles of at most 255 bytes, and more is refused with ErrChapters rather than
// cut.
func (m *Muxer) SetChapters(cs []Chapter) error {
	got, err := setChapters(cs, m.closed, m.started)
	if err == nil {
		err = checkChpl(got)
	}
	if err != nil {
		return err
	}
	m.chapters = got
	return nil
}

// SetChapters states the chapters of the file, in a chpl box of moov/udta. The
// movie box is written last, so this is taken at any time before Close.
func (m *ProgressiveMuxer) SetChapters(cs []Chapter) error {
	got, err := setChapters(cs, m.closed, false)
	if err == nil {
		err = checkChpl(got)
	}
	if err != nil {
		return err
	}
	m.chapters = got
	return nil
}

// SetChapters states the chapters of the file, as the Chapters element RFC 9559
// §5.1.7 defines: one edition, one ChapterAtom each, written after the Tracks as
// FFmpeg writes it. What precedes the first cluster is written with the first
// sample, so this has to come before it.
func (m *WebMMuxer) SetChapters(cs []Chapter) error {
	got, err := setChapters(cs, m.closed, m.started)
	if err != nil {
		return err
	}
	m.chapters = got
	return nil
}

// ReplaceChapters writes cs in place of the chapters the input carries. Without
// it Remux, Cut, Concat and Join carry the chapters of their input (shifted and
// trimmed by Cut, and put after one another by Concat), and
// ReplaceChapters(nil) is how a caller writes none. A track extracted as an
// audio-only file carries none either way.
func ReplaceChapters(cs []Chapter) RemuxOption {
	return func(s *remuxSettings) {
		s.chapters = &cs
	}
}

// outputChapters are the chapters a copy of srcs writes when it does not move
// them: what the caller said, or else the first input's.
func (s *remuxSettings) outputChapters(srcs []*Reader) []Chapter {
	if s.chapters != nil {
		return *s.chapters
	}
	return srcs[0].File().Chapters
}

// chplLimit is what a chpl box can state: a one-byte count and one-byte titles.
const chplLimit = 255

// checkChpl refuses chapters a chpl cannot hold.
func checkChpl(cs []Chapter) error {
	if len(cs) > chplLimit {
		return fmt.Errorf("%w: a chpl box holds at most %d chapters, not %d", ErrChapters, chplLimit, len(cs))
	}
	for i, c := range cs {
		if len(c.Title) > chplLimit {
			return fmt.Errorf("%w: the title of chapter %d is %d bytes, and a chpl box holds at most %d",
				ErrChapters, i+1, len(c.Title), chplLimit)
		}
	}
	return nil
}

// chplOf is the chpl box stating cs, in the form Nero and FFmpeg write: version
// 1, and starts in units of 100 ns.
func chplOf(cs []Chapter) *mp4.ChplBox {
	box := &mp4.ChplBox{Version: 1}
	for _, c := range cs {
		box.Chapters = append(box.Chapters, mp4.NeroChapter{Start: uint64(c.Start / 100), Title: c.Title})
	}
	return box
}

// mp4Chapters reads the chapters of a movie's chpl.
func mp4Chapters(moov *mp4.MoovBox) []Chapter {
	for _, child := range moov.Children {
		udta, ok := child.(*mp4.UdtaBox)
		if !ok {
			continue
		}
		for _, c := range udta.Children {
			if chpl, ok := c.(*mp4.ChplBox); ok {
				return chaptersOfChpl(chpl)
			}
		}
	}
	return nil
}

// chaptersOfChpl is a chpl box as chapters: the titles that are text, in start
// order, as many as fit maxChapters.
func chaptersOfChpl(chpl *mp4.ChplBox) []Chapter {
	var out []Chapter
	for _, c := range chpl.Chapters {
		if len(out) == maxChapters {
			break
		}
		title := c.Title
		if !isText(title) {
			title = ""
		}
		// A start counted in units of 100 ns that does not fit a Duration is
		// not a position in a file, and saturates as a Matroska one does.
		start := time.Duration(math.MaxInt64)
		if c.Start <= math.MaxInt64/100 {
			start = c.StartTime()
		}
		out = append(out, Chapter{Start: start, Title: title})
	}
	sortChapters(out)
	return out
}

// sortChapters puts chapters in start order, keeping the file's order among
// equal starts.
func sortChapters(cs []Chapter) {
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Start < cs[j].Start })
}

// loadChapters reads the chapters of an MP4 reader's file: those of a QuickTime
// chapter track when it has one, else those of its chpl, which Demux already
// found.
func (r *Reader) loadChapters() {
	moov := movieBox(r.mp4)
	for _, trak := range moov.Traks {
		for _, child := range trak.Children {
			tref, ok := child.(*mp4.TrefBox)
			if !ok {
				continue
			}
			for _, ref := range tref.Children {
				typed, ok := ref.(*mp4.TrefTypeBox)
				if !ok || typed.Type() != "chap" {
					continue
				}
				for _, id := range typed.TrackIDs {
					if r.chapterTracks == nil {
						r.chapterTracks = map[uint32]bool{}
					}
					r.chapterTracks[id] = true
					if cs := r.chapterTrack(id); len(cs) > 0 && !r.chaptersFromTrack {
						r.file.Chapters, r.chaptersFromTrack = cs, true
					}
				}
			}
		}
	}
}

// isChapterTrack reports whether a track is one another track names as holding
// its chapters. Its samples are the titles, which File.Chapters states, and
// copying it as a track would write a text track the muxers have no entry for.
func (r *Reader) isChapterTrack(id uint32) bool { return r.chapterTracks[id] }

// chapterTrack reads the chapters a text track holds: each sample is a title,
// and lasts until the next begins. A track that is not a readable text track
// holds none.
func (r *Reader) chapterTrack(id uint32) []Chapter {
	trak, ok := r.traks[id]
	if !ok || trak.Mdia.Mdhd == nil || trak.Mdia.Mdhd.Timescale == 0 {
		return nil
	}
	// The titles are text samples: a handler of any other kind refers to
	// something else, which is not read as titles.
	if hdlr := trak.Mdia.Hdlr; hdlr == nil || (hdlr.HandlerType != "text" && hdlr.HandlerType != "sbtl") {
		return nil
	}
	samples, err := r.mp4Samples(trak, maxChapters)
	if err != nil {
		return nil
	}
	timescale := time.Duration(trak.Mdia.Mdhd.Timescale)
	var (
		out []Chapter
		at  uint64
	)
	for _, s := range samples {
		start := time.Duration(at) * time.Second / timescale
		at += uint64(s.Duration)
		end := time.Duration(at) * time.Second / timescale
		out = append(out, Chapter{Start: start, End: end, Title: chapterText(s.Data)})
	}
	return out
}

// chapterText is the title a QuickTime text sample holds: a 16-bit length, then
// that many bytes of UTF-8, or of UTF-16 when they begin with a byte order mark.
// Whatever follows -- the "encd" atom FFmpeg writes -- is not text. A sample
// that does not hold what it states is the empty title.
func chapterText(data []byte) string {
	if len(data) < 2 {
		return ""
	}
	n := int(binary.BigEndian.Uint16(data))
	if n > len(data)-2 {
		return ""
	}
	text := data[2 : 2+n]
	if len(text) >= 2 && len(text)%2 == 0 && (text[0] == 0xFE && text[1] == 0xFF || text[0] == 0xFF && text[1] == 0xFE) {
		order := binary.ByteOrder(binary.BigEndian)
		if text[0] == 0xFF {
			order = binary.LittleEndian
		}
		units := make([]uint16, 0, len(text)/2-1)
		for i := 2; i < len(text); i += 2 {
			units = append(units, order.Uint16(text[i:]))
		}
		if title := string(utf16.Decode(units)); isText(title) {
			return title
		}
		return ""
	}
	if title := string(text); isText(title) {
		return title
	}
	return ""
}

// The Chapters element and what it holds, as the Matroska readers and the muxer
// state them (RFC 9559 §5.1.7). ChapterTimeEnd and the flags are values whose
// absence means something a zero does not say, and so are what they are: a zero
// end is "not stated", and only a flag stated as 1 is read as set.
type mkvChapters struct {
	EditionEntry []mkvEdition `ebml:"EditionEntry"`
}

type mkvEdition struct {
	EditionUID         uint64        `ebml:"EditionUID,omitempty"`
	EditionFlagDefault uint64        `ebml:"EditionFlagDefault,omitempty"`
	EditionFlagOrdered uint64        `ebml:"EditionFlagOrdered,omitempty"`
	ChapterAtom        []mkvChapAtom `ebml:"ChapterAtom"`
}

type mkvChapAtom struct {
	ChapterUID       uint64              `ebml:"ChapterUID"`
	ChapterTimeStart uint64              `ebml:"ChapterTimeStart"`
	ChapterTimeEnd   uint64              `ebml:"ChapterTimeEnd,omitempty"`
	ChapterDisplay   []mkvChapterDisplay `ebml:"ChapterDisplay,omitempty"`
}

type mkvChapterDisplay struct {
	ChapString   string `ebml:"ChapString"`
	ChapLanguage string `ebml:"ChapLanguage,omitempty"`
}

// mkvChapterLanguage is what ChapLanguage is when the element is absent (RFC
// 9559 §5.1.7.1.4.12): "eng".
const mkvChapterLanguage = "eng"

// mkvReadChapters reads the chapters of a Matroska segment: the default
// edition's, or the first's, unless it is an ordered one, which is not a list of
// points on the file's timeline.
func mkvReadChapters(elements []mkvChapters) []Chapter {
	var editions []mkvEdition
	for _, element := range elements {
		editions = append(editions, element.EditionEntry...)
	}
	if len(editions) == 0 {
		return nil
	}
	chosen := editions[0]
	for _, e := range editions {
		if e.EditionFlagDefault == 1 {
			chosen = e
			break
		}
	}
	if chosen.EditionFlagOrdered == 1 {
		return nil
	}
	var out []Chapter
	for _, atom := range chosen.ChapterAtom {
		if len(out) == maxChapters {
			break
		}
		c := Chapter{Start: nanoseconds(atom.ChapterTimeStart)}
		if atom.ChapterTimeEnd >= atom.ChapterTimeStart {
			c.End = nanoseconds(atom.ChapterTimeEnd)
		}
		if len(atom.ChapterDisplay) > 0 {
			d := atom.ChapterDisplay[0]
			if isText(d.ChapString) {
				c.Title = d.ChapString
			}
			c.Language = d.ChapLanguage
			if c.Language == "" {
				c.Language = mkvChapterLanguage
			}
		}
		out = append(out, c)
	}
	sortChapters(out)
	return out
}

// nanoseconds is a Matroska time as a duration, saturating at the largest the
// type holds: a start of 2^63 ns is 292 years, which is not a position in a
// file.
func nanoseconds(ns uint64) time.Duration {
	if ns > uint64(1<<63-1) {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(ns)
}

// mkvChaptersOf is the Chapters element stating cs, or nothing when there are
// none: one edition, not ordered, with one atom per chapter. The UIDs are the
// chapters' positions, so two runs of the same input write the same bytes, and a
// chapter's end is written when it is known.
func mkvChaptersOf(cs []Chapter) []mkvChapters {
	if len(cs) == 0 {
		return nil
	}
	edition := mkvEdition{EditionUID: 1}
	for i, c := range cs {
		lang := c.Language
		if lang == "" {
			lang = "und"
		}
		atom := mkvChapAtom{
			ChapterUID:       uint64(i + 1),
			ChapterTimeStart: uint64(c.Start),
			ChapterTimeEnd:   uint64(c.End),
			ChapterDisplay:   []mkvChapterDisplay{{ChapString: c.Title, ChapLanguage: lang}},
		}
		edition.ChapterAtom = append(edition.ChapterAtom, atom)
	}
	return []mkvChapters{{EditionEntry: []mkvEdition{edition}}}
}

// cutChapters is what a cut of the window from to to (a to of zero or less being
// the end of the file) keeps of the chapters, counted from the window's start.
//
// A chapter is kept when it overlaps the window: it ends after from and begins
// before to. One that begins before the window begins at its start, and one that
// ends after it ends at its end, when it states an end; an unstated end stays
// unstated, since it runs until the next chapter, which is in the window or not.
// A chapter with no length is kept when it begins inside.
func cutChapters(cs []Chapter, from, to time.Duration) []Chapter {
	var out []Chapter
	for i, c := range cs {
		end := c.End
		if end == 0 {
			// Until the next chapter begins, or for ever.
			end = time.Duration(1<<63 - 1)
			if i+1 < len(cs) {
				end = cs[i+1].Start
			}
		}
		beginsBeforeEnd := to <= 0 || c.Start < to
		endsAfterStart := end > from || (end == c.Start && c.Start >= from)
		if !beginsBeforeEnd || !endsAfterStart {
			continue
		}
		cut := c
		cut.Start = max(c.Start, from) - from
		if c.End != 0 {
			cut.End = c.End - from
			if to > 0 {
				cut.End = min(c.End, to) - from
			}
		}
		out = append(out, cut)
	}
	return out
}

// shiftChapters moves chapters by by, which is how long what comes before them
// lasts: the chapters of the second input of a concatenation.
func shiftChapters(cs []Chapter, by time.Duration) []Chapter {
	out := slices.Clone(cs)
	for i := range out {
		out[i].Start += by
		if out[i].End != 0 {
			out[i].End += by
		}
	}
	return out
}
