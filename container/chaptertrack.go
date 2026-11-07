// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The QuickTime chapter track, written beside the chpl as FFmpeg's movenc does
// (mov_create_chapter_track): a track with the "text" handler and a "gmhd" media
// header, named by a "chap" entry of the first track's tref, whose samples are
// the titles -- a 16-bit length, the UTF-8 text, and the "encd" atom FFmpeg
// writes after it -- each lasting from its chapter's start to the next one's,
// and the last to the end of the media.
//
// The sample entry and the media header are FFmpeg's own, byte for byte (the
// entry less its "btrt", which states a bit rate nothing reads), taken from a
// file it wrote: Apple's QuickTime File Format specification was not opened.
//
// ⛔ A chapter that has no length is a sample of duration 0, which is how
// FFmpeg states one and how its reader reads one back. The first chapter has to
// begin at 0, since the samples follow one another from the start of the track:
// a list that begins later has no chapter track, only the chpl, rather than a
// track whose first title is shown from 0.

// chapterEncd is the "encd" atom of a chapter sample: the text encoding, 256
// being UTF-8 (the QuickTime text encoding of "kCFStringEncodingUTF8").
var chapterEncd = []byte{0, 0, 0, 12, 'e', 'n', 'c', 'd', 0, 0, 1, 0}

// chapterEntry is the body of the QuickTime "text" sample entry FFmpeg writes
// for a chapter track: a data reference index of 1, display flags of 1, and a
// font table naming font 1.
var chapterEntry = []byte{
	0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0xd, 'f', 't', 'a', 'b', 0, 1, 0, 1, 0,
}

// chapterGmhd is the base media header of that track: "gmin" and a "text" box.
var chapterGmhd = []byte{
	0, 0, 0, 0x4c, 'g', 'm', 'h', 'd', 0, 0, 0, 0x18, 'g', 'm', 'i', 'n', 0, 0, 0, 0, 0, 0x40, 0x80, 0, 0x80, 0, 0x80, 0, 0, 0, 0, 0,
	0, 0, 0, 0x2c, 't', 'e', 'x', 't', 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x40, 0, 0, 0,
}

// chapterPlan is the samples of a chapter track, all but the last of which are
// known before the media is.
type chapterPlan struct {
	timescale uint32
	// samples are the titles of every chapter but the last, with their durations.
	samples []Sample
	// start is where the last chapter begins, in ticks, and last its sample's
	// data: the sample lasts to the end of the media, which only the end knows.
	start uint64
	last  []byte
}

// planChapterTrack plans the chapter track of these chapters at this timescale,
// or reports there is none to write.
func planChapterTrack(cs []Chapter, timescale uint32) (chapterPlan, bool) {
	if len(cs) == 0 || cs[0].Start != 0 {
		return chapterPlan{}, false
	}
	plan := chapterPlan{timescale: timescale}
	ticks := func(i int) uint64 { return scaleDuration(uint64(cs[i].Start), nsScale, timescale) }
	for i := 0; i+1 < len(cs); i++ {
		plan.samples = append(plan.samples, Sample{
			Data: chapterSample(cs[i].Title), Duration: uint32(min(ticks(i+1)-ticks(i), 1<<32-1)), Sync: true,
		})
	}
	last := len(cs) - 1
	plan.start, plan.last = ticks(last), chapterSample(cs[last].Title)
	return plan, true
}

// chapterSample is the sample stating a title: its length, the text, the atom.
func chapterSample(title string) []byte {
	data := binary.BigEndian.AppendUint16(nil, uint16(len(title)))
	return append(append(data, title...), chapterEncd...)
}

// lastSample is the sample of the last chapter, given where the media ends in
// ticks. A media that ends before the chapter begins leaves it no length.
func (p chapterPlan) lastSample(end uint64) Sample {
	var d uint64
	if end > p.start {
		d = min(end-p.start, 1<<32-1)
	}
	return Sample{Data: p.last, Duration: uint32(d), Sync: true}
}

// chapterTrak makes the chapter track's trak box, its sample entry stated, and
// the tref that names it on the first track.
func chapterTrak(trak *mp4.TrakBox) {
	minf := trak.Mdia.Minf
	minf.Children[0] = mp4.CreateUnknownBox("gmhd", uint64(len(chapterGmhd)), chapterGmhd[8:])
	minf.Stbl.Stsd.AddChild(mp4.CreateUnknownBox("text", uint64(8+len(chapterEntry)), chapterEntry))
	// The track is not one to play: enabled is off, and it is in no presentation.
	trak.Tkhd.Flags = 2
}

// addChapterTrack adds the chapter track of a fragmented file, with the first
// sample: the initialisation segment, written with the first fragment, names it,
// and the titles whose length is known go out in that fragment. The last one
// waits for Close, when the length of the media is.
func (m *Muxer) addChapterTrack() {
	plan, ok := planChapterTrack(m.chapters, m.tracks[0].timescale)
	if !ok {
		return
	}
	trak := m.init.AddEmptyTrack(plan.timescale, "text", "und")
	chapterTrak(trak)
	m.tracks[0].trak.AddChild(chapterTref(trak.Tkhd.TrackID))
	t := &muxTrack{id: trak.Tkhd.TrackID, timescale: plan.timescale, trak: trak, written: true}
	for _, s := range plan.samples {
		t.addChapterSample(s)
	}
	m.chapterTrack, m.chapterPlan = t, plan
	m.tracks = append(m.tracks, t)
}

// addChapterSample queues a chapter's sample, which may have no duration.
func (t *muxTrack) addChapterSample(s Sample) {
	t.pending = append(t.pending, mp4.FullSample{
		Sample:     mp4.Sample{Flags: mp4.SyncSampleFlags, Dur: s.Duration, Size: uint32(len(s.Data))},
		DecodeTime: t.nextTime,
		Data:       s.Data,
	})
	t.nextTime += uint64(s.Duration)
	t.buffered += uint64(s.Duration)
}

// finishChapterTrack queues the last chapter's sample, which lasts to where the
// longest of the other tracks ends.
func (m *Muxer) finishChapterTrack() {
	if m.chapterTrack == nil {
		return
	}
	var end uint64
	for _, t := range m.tracks {
		if t != m.chapterTrack {
			end = max(end, scaleDuration(t.nextTime, t.timescale, m.chapterPlan.timescale))
		}
	}
	m.chapterTrack.addChapterSample(m.chapterPlan.lastSample(end))
	m.chapterTrack = nil
}

// writeChapterTrack adds the chapter track of a progressive file at Close, when
// the length of the media is known, and writes its samples like any other's. A
// sample of no duration is one WriteSample refuses, so it is let through here.
func (m *ProgressiveMuxer) writeChapterTrack() error {
	if len(m.chapters) == 0 || len(m.tracks) == 0 {
		return nil
	}
	for _, t := range m.tracks {
		if t.count == 0 {
			return nil // Close names the track
		}
	}
	plan, ok := planChapterTrack(m.chapters, m.tracks[0].timescale)
	if !ok {
		return nil
	}
	var end uint64
	for _, t := range m.tracks {
		end = max(end, scaleDuration(t.total, t.timescale, plan.timescale))
	}
	id := uint32(len(m.tracks)) + 1
	trak := mp4.CreateEmptyTrak(id, plan.timescale, "text", "und")
	chapterTrak(trak)
	m.tracks[0].trak.AddChild(chapterTref(id))
	t := &progressiveTrack{id: id, trak: trak, timescale: plan.timescale}
	m.tracks = append(m.tracks, t)
	m.byID[id] = t
	m.noDuration = true
	defer func() { m.noDuration = false }()
	for _, s := range append(plan.samples, plan.lastSample(end)) {
		if err := m.WriteSample(id, s); err != nil {
			return err
		}
	}
	return nil
}

// chapterTref is the tref naming a chapter track.
func chapterTref(id uint32) *mp4.TrefBox {
	tref := &mp4.TrefBox{}
	tref.AddChild(&mp4.TrefTypeBox{Name: "chap", TrackIDs: []uint32{id}})
	return tref
}
