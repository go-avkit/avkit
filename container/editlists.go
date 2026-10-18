// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"
)

// ErrEditList means a track's edit list cannot be carried into the output the
// way the operation would need it: the presentation it describes would come out
// different, and writing it anyway would be writing something the source does
// not say.
var ErrEditList = errors.New("container: edit list cannot be carried")

// EmptyEdit is the media time of an empty edit: a stretch of the presentation
// during which the track shows nothing (ISO/IEC 14496-12 §8.6.6.3).
const EmptyEdit int64 = -1

// NormalRate is the media rate of an edit that plays its media as it is, 1.0 in
// the signed 16.16 fixed point of §8.6.6.3. Zero is a dwell: the media at the
// edit's media time is held for the edit's duration.
const NormalRate int32 = 1 << 16

// EditList is a track's edit list as an MP4 states it (ISO/IEC 14496-12
// §8.6.5, §8.6.6): which stretches of the track's media make up its
// presentation, one after another.
//
// It is what the MP4 reader hands over and what the MP4 muxers write back, so a
// track copied from one MP4 into another keeps the presentation it had, edit for
// edit. It is not applied to the samples: Reader.Samples still returns every
// sample once, in decoding order.
type EditList struct {
	// MovieTimescale is the unit of every SegmentDuration, per second: the
	// timescale of the movie header of the file the list came from. A muxer
	// writing another movie timescale converts the durations, rounding up so
	// that no edit ends before the media it covers does.
	MovieTimescale uint32
	// Edits are the entries, in presentation order.
	Edits []Edit
}

// Edit is one entry of an edit list.
type Edit struct {
	// SegmentDuration is how long the edit lasts in the presentation, in the
	// list's MovieTimescale. Zero is allowed in a fragmented file and means
	// "to the end of the media", §8.6.6.1.
	SegmentDuration uint64
	// MediaTime is where in the track's media the edit starts, in the track's
	// own timescale and in COMPOSITION time, counted from the decoding time of
	// the first sample Reader.Samples returns. EmptyEdit marks an empty edit.
	MediaTime int64
	// Rate is the media rate, in signed 16.16 fixed point: NormalRate, or 0
	// for a dwell. QuickTime allows others; ISO/IEC 14496-12 does not.
	Rate int32
}

// mp4EditList reads a track's edit list, or nil when it has none. An edit box
// holds one list at most (§8.6.5.1) and a list of no entries says nothing,
// which is how ffmpeg's mov_read_elst treats one too.
//
// The media times are rebased onto the samples Reader.Samples returns, which
// start decoding at zero: a fragmented file may start its first fragment at a
// later decoding time (its tfdt), and its edit list counts from the origin of
// that timeline, not from its first sample. A media time that then falls before
// the first sample is kept as it is, negative, and the muxers refuse it: it
// names media this file does not hold.
func mp4EditList(trak *mp4.TrakBox, movieTimescale uint32, firstDecode uint64) *EditList {
	if trak.Edts == nil || len(trak.Edts.Elst) == 0 || len(trak.Edts.Elst[0].Entries) == 0 {
		return nil
	}
	el := &EditList{MovieTimescale: movieTimescale}
	for _, e := range trak.Edts.Elst[0].Entries {
		media := e.MediaTime
		if media != EmptyEdit {
			media -= int64(firstDecode)
		}
		el.Edits = append(el.Edits, Edit{SegmentDuration: e.SegmentDuration, MediaTime: media, Rate: e.MediaRateFixed32()})
	}
	return el
}

// check refuses an edit list that does not describe a presentation.
func (el *EditList) check() error {
	switch {
	case el.MovieTimescale == 0:
		return fmt.Errorf("%w: an edit list has no movie timescale", ErrTrackConfig)
	case len(el.Edits) == 0:
		return fmt.Errorf("%w: an edit list has no edit", ErrTrackConfig)
	case el.Edits[len(el.Edits)-1].MediaTime == EmptyEdit:
		// §8.6.6.1: "The last edit in a track shall never be an empty edit."
		return fmt.Errorf("%w: an edit list ends with an empty edit", ErrTrackConfig)
	}
	for i, e := range el.Edits {
		if e.MediaTime < EmptyEdit {
			return fmt.Errorf("%w: edit %d starts at media time %d, before the track's first sample",
				ErrTrackConfig, i+1, e.MediaTime)
		}
	}
	return nil
}

// mp4Edits is the edit box stating el in a movie whose timescale is
// movieTimescale, and the length of the presentation it describes there, which
// is the duration of the track header that holds it (§8.3.2.3).
//
// A last edit of duration zero runs to the end of the media, which only a
// fragmented file may say (§8.6.6.1). Given mediaEnd -- when the last frame
// stops being shown, in the track's timescale -- that length is stated
// instead, which is what a progressive file copied from a fragmented one needs;
// a mediaEnd of zero keeps the zero.
func (el *EditList) mp4Edits(movieTimescale, timescale uint32, mediaEnd int64) (*mp4.EdtsBox, uint64) {
	entries := make([]mp4.ElstEntry, len(el.Edits))
	var total uint64
	for i, e := range el.Edits {
		entries[i] = mp4.ElstEntry{
			SegmentDuration: scaleDurationUp(e.SegmentDuration, el.MovieTimescale, movieTimescale),
			MediaTime:       e.MediaTime,
		}
		if i == len(el.Edits)-1 && e.SegmentDuration == 0 && mediaEnd > e.MediaTime {
			entries[i].SegmentDuration = scaleDurationUp(uint64(mediaEnd-e.MediaTime), timescale, movieTimescale)
		}
		entries[i].SetMediaRateFixed32(e.Rate)
		total += entries[i].SegmentDuration
	}
	return newEdts(entries...), total
}

// fragmentedEditList refuses an edit list a fragmented file cannot be relied on
// to present. ISO/IEC 14496-12 allows any list in the initial movie, but the
// readers of fragments honour one shape only, the one startOnly describes:
// ffmpeg turns its edit list support off for a fragmented file ("multiple edit
// list entries, not supported in fragmented MP4 files", mov.c) and keeps an
// empty edit and a start time, and Media Source Extensions players do the same
// or less. Measured: a two-edit list copied into a fragmented file presents all
// 100 frames where the source presents 50. A list of that kind is refused here
// rather than written for nobody to read; ProgressiveMuxer writes it.
func fragmentedEditList(el *EditList) error {
	if err := el.check(); err != nil {
		return err
	}
	if _, _, ok := el.startOnly(); !ok {
		return fmt.Errorf("%w: a fragmented file cannot be relied on to present an edit list of %d edits; "+
			"write it as a progressive MP4", ErrTrackConfig, len(el.Edits))
	}
	return nil
}

// startOnly reports whether the list only says where the presentation starts:
// an optional empty edit delaying it by lead (in the movie timescale), then one
// edit at the normal rate. That is the only shape a container without edit
// lists, or an operation that moves samples about, has an answer for.
func (el *EditList) startOnly() (lead uint64, edit Edit, ok bool) {
	edits := el.Edits
	if len(edits) > 0 && edits[0].MediaTime == EmptyEdit {
		lead, edits = edits[0].SegmentDuration, edits[1:]
	}
	if len(edits) != 1 || edits[0].MediaTime < 0 || edits[0].Rate != NormalRate {
		return 0, Edit{}, false
	}
	return lead, edits[0], true
}

// webmShift is what an edit list says that Matroska, which has presentation
// times and no edit lists, can state: where the track's media starts being
// shown, and how long after the start of the presentation, both in the track's
// timescale.
type webmShift struct {
	media, lead int64
	// end is where the edit stops presenting the media, in composition time
	// and in the track's timescale, for an audio track whose edit states a
	// length; zero for any other. Matroska says the same with DiscardPadding:
	// see webmTrim.
	end int64
}

// webmEdit reads a track's edit list for WebMMuxer, or refuses it. A list that
// only says where the presentation starts becomes a shift of every block's
// time; several edits, a dwell or another rate would need the media cut up or
// repeated, which a remux does not do, and are refused rather than dropped.
func webmEdit(cfg TrackConfig) (*webmShift, error) {
	el := cfg.EditList
	if el == nil {
		return nil, nil
	}
	if err := el.check(); err != nil {
		return nil, err
	}
	lead, edit, ok := el.startOnly()
	if !ok {
		return nil, fmt.Errorf("%w: matroska has no edit lists and cannot state one of %d edits",
			ErrTrackConfig, len(el.Edits))
	}
	// Rounded to the nearest tick, as a priming is.
	ticks := (lead*uint64(cfg.Timescale) + uint64(el.MovieTimescale)/2) / uint64(el.MovieTimescale)
	shift := &webmShift{media: edit.MediaTime, lead: int64(ticks)}
	// An edit that ends before the media does trims the encoder's padding off
	// the end -- what ffmpeg's AAC encoder leaves after the last sample, and
	// what its MP4s state as the edit's segment_duration. Its length is
	// converted to the track's timescale rounded to the nearest tick, as
	// ffmpeg's mov.c converts it (get_edit_list_entry, av_rescale). Only an
	// audio track's is kept: DiscardPadding is silence (RFC 9559 §5.1.3.5.7),
	// and a picture cannot be partly shown.
	if cfg.Kind == Audio && edit.SegmentDuration > 0 {
		shift.end = edit.MediaTime + int64(scaleDurationNearest(edit.SegmentDuration, el.MovieTimescale, cfg.Timescale))
	}
	return shift, nil
}

// scaleDurationNearest is scaleDuration rounded to the nearest unit rather than
// down, split the same way so that a long duration does not overflow.
func scaleDurationNearest(d uint64, from, to uint32) uint64 {
	whole := d / uint64(from) * uint64(to)
	rest := (d%uint64(from)*uint64(to) + uint64(from)/2) / uint64(from)
	return whole + rest
}

// trim is how much of a frame lies past the end of the presentation, in the
// track's timescale: none, part of it, or all of it. start is the frame's
// composition time, before any shift.
func (w *webmShift) trim(start, duration int64) int64 {
	if w == nil || w.end <= 0 {
		return 0
	}
	return min(max(start+duration-w.end, 0), duration)
}

// shift is how far to move the track's presentation times back, given the
// composition time of its first sample, which is where the track starts being
// shown without an edit list.
//
// An edit starting before that first frame shows the track that much later:
// a list starting at zero on a track that reorders its frames shows its first
// frame late, and so does the copy. An edit starting after it does not show
// the track any earlier: the media between is a priming. When the list is the
// one edit a priming is written as, the reader has stated it as StartDelay,
// and WebMMuxer writes that as the track's CodecDelay (#71); after an empty
// edit it is no StartDelay, and the priming is presented as it was before. An
// empty edit at the head delays the whole track by its length.
func (w *webmShift) shift(first int64) int64 {
	if w == nil {
		return first
	}
	return min(w.media, first) - w.lead
}

// presentedUntil is when the last frame of samples to be shown stops being
// shown, in composition time and in the track's timescale.
func presentedUntil(samples []Sample) int64 {
	var dts, end int64
	for _, s := range samples {
		end = max(end, dts+int64(s.CompositionOffset)+int64(s.Duration))
		dts += int64(s.Duration)
	}
	return end
}

// dropEditList is how Cut and Concat treat a track's edit list. Both move
// samples against the media timeline the list was written for -- a cut starts
// it elsewhere, a concatenation puts another input after it -- so the list
// cannot be copied. What can be kept is what the muxers state on their own:
// the presentation starts at the first sample's composition time, and the
// priming StartDelay carries after it.
//
// That is the whole of what a list says when it is one edit at the normal rate
// that runs to the end of the media, and such a list is dropped. Anything more
// is refused rather than silently lost: an empty edit placing the track later
// than its siblings, several edits, a dwell or another rate, or -- when the
// output keeps the track's last sample -- an edit that ends before the media
// does, which is how a gapless AAC file trims its encoder's padding.
//
// full is every sample of the track as the source holds it, which is what the
// list's media times count in.
func dropEditList(t sourceTrack, full []Sample, keepsTail bool, doing string) (TrackConfig, error) {
	cfg := t.cfg
	el := cfg.EditList
	if el == nil {
		return cfg, nil
	}
	lead, edit, ok := el.startOnly()
	switch {
	case !ok:
		return cfg, fmt.Errorf("%w: track %d has an edit list of %d edits, which %s cannot carry",
			ErrEditList, t.id, len(el.Edits), doing)
	case lead > 0:
		return cfg, fmt.Errorf("%w: track %d starts %d/%d s into the presentation, which %s cannot carry",
			ErrEditList, t.id, lead, el.MovieTimescale, doing)
	}
	if keepsTail && edit.SegmentDuration > 0 && el.MovieTimescale > 0 {
		end := edit.MediaTime + int64(scaleDurationUp(edit.SegmentDuration, el.MovieTimescale, cfg.Timescale))
		// One tick of the movie timescale, in the track's: what a duration
		// rounded to the coarser of the two can fall short by.
		slack := int64((cfg.Timescale + el.MovieTimescale - 1) / el.MovieTimescale)
		if media := presentedUntil(full); end+slack < media {
			return cfg, fmt.Errorf("%w: track %d presents its media only up to %d of %d ticks, which %s cannot carry",
				ErrEditList, t.id, end, media, doing)
		}
	}
	cfg.EditList = nil
	return cfg, nil
}
