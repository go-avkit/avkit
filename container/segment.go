// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/av1"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// Segmenting is the cutting half of HLS and DASH packaging: one file becomes,
// for each track, an initialisation segment and a run of media segments, cut at
// the same instants in every track. The playlists and manifests that name the
// segments are written elsewhere (go-streamkit); a Segmentation carries what they
// state.
//
// Sources: RFC 8216 §3.3 and §4.3.3 (every fMP4 segment has an EXT-X-MAP, segment
// durations rounded stay within the target), draft-pantos-hls-rfc8216bis §3.1.4
// (WebVTT segments and X-TIMESTAMP-MAP), Apple's HLS Authoring Specification §7.4
// (video segments start with an IDR) and §8.22 (renditions share segment
// boundaries), and the DASH-IF timing model (segments of one adaptation set cover
// the same spans of the sample timeline). ffmpeg's hls and dash muxers are the
// oracle; see testdata/segment.

// Segmenting limits and defaults.
const (
	// DefaultSegmentDuration is Apple's recommended target (HLS Authoring
	// Specification §7.5).
	DefaultSegmentDuration = 6 * time.Second
	// MinSegmentDuration and MaxSegmentDuration bound the target: below the
	// first a file is cut into more segments than a player can use, above the
	// second a "segment" is the file.
	MinSegmentDuration = 100 * time.Millisecond
	MaxSegmentDuration = time.Hour
	// MaxSegments is the most segments one track is cut into.
	MaxSegments = 100000
)

// Errors reported when segmenting.
var (
	// ErrSegmentOption means an option states a value that cannot be honoured.
	ErrSegmentOption = errors.New("container: invalid segmenting option")
	// ErrSegmentTrack means a track cannot be segmented, with the reason.
	ErrSegmentTrack = errors.New("container: cannot segment this track")
	// ErrSegmentName means a name that would be written to disk is not a plain
	// file name.
	ErrSegmentName = errors.New("container: unsafe segment file name")
)

// SegmentOption configures Segment.
type SegmentOption func(*segmentSettings)

type segmentSettings struct {
	target  time.Duration
	tracks  []uint32
	indexed bool
}

// SegmentDuration sets the target length of a segment. The first video track's
// keyframes decide where segments really end: a segment is closed at the first
// keyframe at or after the target.
func SegmentDuration(d time.Duration) SegmentOption {
	return func(s *segmentSettings) { s.target = d }
}

// SegmentTracks selects the source tracks to segment, by identifier. By default
// every track is, and one that cannot be is refused rather than left out.
func SegmentTracks(ids ...uint32) SegmentOption {
	return func(s *segmentSettings) { s.tracks = append([]uint32(nil), ids...) }
}

// IndexedSegments puts a styp and a sidx box (ISO/IEC 14496-12 §8.16.2 and
// §8.16.3) in front of each media segment's moof, so that a DASH client can
// address it as an indexed segment.
func IndexedSegments() SegmentOption {
	return func(s *segmentSettings) { s.indexed = true }
}

// MediaSegment is one cut of a track.
type MediaSegment struct {
	// Data is the segment as it is written to a file: a moof and an mdat, or
	// for a subtitle track a WebVTT document.
	Data []byte
	// Start and Duration are in the track's Timescale. Start is the decode
	// time of the first sample (the segment's tfdt).
	Start, Duration uint64
}

// SegmentedTrack is one source track cut into segments.
type SegmentedTrack struct {
	// Name is a generated, file-safe name ("video1", "audio1", "text1") that
	// the file names are built from. It is never taken from the source.
	Name string
	// ID is the source track's identifier.
	ID   uint32
	Kind Kind
	// Config is the track's configuration as the source states it.
	Config TrackConfig
	// Codecs is the RFC 6381 codecs parameter, "avc1.4d400b".
	Codecs string
	// MIME is "video/mp4", "audio/mp4" or "text/vtt".
	MIME string
	// Timescale is the unit of every Start and Duration of Segments.
	Timescale uint32
	// Init is the initialisation segment (ftyp and moov), nil for a subtitle
	// track, whose WebVTT segments need none.
	Init     []byte
	Segments []MediaSegment
	// Bandwidth is the peak and AverageBandwidth the mean bit rate of the
	// media segments, in bits per second; 0 for a subtitle track.
	Bandwidth, AverageBandwidth uint64
}

// InitName is the file name of the initialisation segment.
func (t *SegmentedTrack) InitName() string { return t.Name + "_init.mp4" }

// SegmentName is the file name of media segment i (counted from 0).
func (t *SegmentedTrack) SegmentName(i int) string {
	ext := "m4s"
	if t.Kind == Subtitle {
		ext = "vtt"
	}
	return fmt.Sprintf("%s_%05d.%s", t.Name, i+1, ext)
}

// Duration is the track's length in seconds.
func (t *SegmentedTrack) Duration() float64 {
	var d uint64
	for _, s := range t.Segments {
		d += s.Duration
	}
	return float64(d) / float64(t.Timescale)
}

// Segmentation is a file cut into aligned segments.
type Segmentation struct {
	// Target is the target duration the file was cut with.
	Target time.Duration
	Tracks []SegmentedTrack
}

// segSource is one track being cut.
type segSource struct {
	track   Track
	cfg     TrackConfig
	samples []Sample
	starts  []uint64 // decode time of each sample, in the track's timescale
	end     uint64
}

// Segment cuts the tracks of r into an initialisation segment and media segments
// each, never touching a sample's bytes.
//
// The first video track (else the first audio track) is the reference: its
// segments end at the first keyframe at or after the target duration, and every
// other track is cut at the first sample that starts at or after the same
// instant (a video track, at the first keyframe after it), so a segment of one
// track covers the span of the same-numbered segment of the others. A subtitle
// track must be WebVTT (ISO/IEC 14496-30 wvtt); it is written as WebVTT
// documents, one per span, each repeating the cues that cross into it, and
// stating X-TIMESTAMP-MAP as draft-pantos-hls-rfc8216bis §3.1.4 asks.
//
// The whole result is held in memory, as the Reader's samples already are; a
// track is cut into at most MaxSegments segments.
func Segment(r *Reader, opts ...SegmentOption) (*Segmentation, error) {
	return segment(r, opts)
}

// segReader is what Segment takes of a Reader.
type segReader interface {
	File() *File
	Truncated() bool
	TrackIDs() []uint32
	TrackConfig(uint32) (TrackConfig, error)
	Samples(uint32) ([]Sample, error)
}

func segment(r segReader, opts []SegmentOption) (*Segmentation, error) {
	set := segmentSettings{target: DefaultSegmentDuration}
	for _, o := range opts {
		o(&set)
	}
	if set.target < MinSegmentDuration || set.target > MaxSegmentDuration {
		return nil, fmt.Errorf("%w: target %v outside %v..%v", ErrSegmentOption,
			set.target, MinSegmentDuration, MaxSegmentDuration)
	}
	if r.Truncated() {
		return nil, fmt.Errorf("container: refusing to segment a truncated file: %w", ErrTruncated)
	}
	ids := set.tracks
	if len(ids) == 0 {
		ids = r.TrackIDs()
	}
	var srcs []*segSource
	for _, id := range ids {
		src, err := loadSegSource(r, id)
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, src)
	}
	if len(srcs) == 0 {
		return nil, ErrNoTracks
	}
	ref := referenceTrack(srcs)
	cuts, err := referenceCuts(ref, set.target)
	if err != nil {
		return nil, err
	}
	out := &Segmentation{Target: set.target}
	count := map[Kind]int{}
	for _, src := range srcs {
		count[src.track.Kind]++
		name := fmt.Sprintf("%s%d", map[Kind]string{Video: "video", Audio: "audio", Subtitle: "text"}[src.track.Kind], count[src.track.Kind])
		var st SegmentedTrack
		if src.track.Kind == Subtitle {
			st, err = cutText(src, ref, cuts)
		} else {
			st, err = cutMedia(src, ref, cuts, set.indexed)
		}
		if err != nil {
			return nil, fmt.Errorf("track %d: %w", src.track.ID, err)
		}
		st.Name, st.ID, st.Kind, st.Config = name, src.track.ID, src.track.Kind, src.cfg
		out.Tracks = append(out.Tracks, st)
	}
	return out, nil
}

// loadSegSource reads one track's samples and configuration, refusing a track
// no packaging can carry.
func loadSegSource(r segReader, id uint32) (*segSource, error) {
	var track *Track
	for i := range r.File().Tracks {
		if r.File().Tracks[i].ID == id {
			track = &r.File().Tracks[i]
		}
	}
	if track == nil {
		return nil, fmt.Errorf("%w: %d", ErrUnknownTrack, id)
	}
	cfg, err := r.TrackConfig(id)
	if err != nil {
		return nil, fmt.Errorf("track %d: %w", id, err)
	}
	samples, err := r.Samples(id)
	if err != nil {
		return nil, fmt.Errorf("track %d: %w", id, err)
	}
	return newSegSource(*track, cfg, samples)}

// newSegSource checks a track can be segmented and lays its samples on a
// timeline.
func newSegSource(track Track, cfg TrackConfig, samples []Sample) (*segSource, error) {
	id := track.ID
	switch cfg.Kind {
	case Video, Audio:
	case Subtitle:
		if cfg.Codec != "wvtt" {
			return nil, fmt.Errorf("%w: track %d is a %q subtitle track; only WebVTT (wvtt) is segmented",
				ErrSegmentTrack, id, cfg.Codec)
		}
	default:
		return nil, fmt.Errorf("%w: track %d is neither video, audio nor subtitles", ErrSegmentTrack, id)
	}
	if cfg.Timescale == 0 {
		return nil, fmt.Errorf("%w: track %d has no timescale", ErrSegmentTrack, id)
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("%w: track %d has no sample", ErrNoSamples, id)
	}
	src := &segSource{track: track, cfg: cfg, samples: samples, starts: make([]uint64, len(samples))}
	src.track.Kind = cfg.Kind
	var at uint64
	for i, s := range samples {
		src.starts[i] = at
		at += uint64(s.Duration)
	}
	src.end = at
	return src, nil
}

// referenceTrack is the track that decides the cuts: video, else audio, else
// whatever there is.
func referenceTrack(srcs []*segSource) *segSource {
	for _, kind := range []Kind{Video, Audio} {
		for _, s := range srcs {
			if s.track.Kind == kind {
				return s
			}
		}
	}
	return srcs[0]
}

// refCuts are the instants a track is cut at, in the reference track's
// timescale, with the reference's end.
type refCuts struct {
	at  []uint64 // start of segments 2, 3, ...
	idx []int    // the reference's sample index of each
	end uint64
	ts  uint32
}

// ticksOf converts a duration to ticks of a timescale, rounding down.
func ticksOf(d time.Duration, ts uint32) uint64 {
	hi, lo := bits.Mul64(uint64(d), uint64(ts))
	q, _ := bits.Div64(hi, lo, uint64(time.Second)) // hi < 1e9 as d <= 1h and ts < 2^32
	return q
}

// before reports whether a/ats is less than b/bts.
func before(a uint64, ats uint32, b uint64, bts uint32) bool {
	ah, al := bits.Mul64(a, uint64(bts))
	bh, bl := bits.Mul64(b, uint64(ats))
	return ah < bh || (ah == bh && al < bl)
}

// referenceCuts finds where the reference track's segments end.
func referenceCuts(ref *segSource, target time.Duration) (refCuts, error) {
	c := refCuts{end: ref.end, ts: ref.cfg.Timescale}
	want := max(ticksOf(target, c.ts), 1)
	var segStart uint64
	for i := 1; i < len(ref.samples); i++ {
		if ref.starts[i]-segStart < want {
			continue
		}
		if ref.track.Kind == Video && !ref.samples[i].Sync {
			continue
		}
		if len(c.at)+1 >= MaxSegments {
			return c, fmt.Errorf("%w: more than %d segments at a target of %v", ErrMediaTooLarge, MaxSegments, target)
		}
		c.at = append(c.at, ref.starts[i])
		c.idx = append(c.idx, i)
		segStart = ref.starts[i]
	}
	return c, nil
}

// cutIndexes are the sample indexes a track's segments begin at (after the
// first, which is 0), for the instants cuts states.
func cutIndexes(src *segSource, cuts refCuts, ref *segSource) []int {
	if src == ref {
		return cuts.idx
	}
	var idx []int
	prev := 0
	for _, at := range cuts.at {
		j := prev
		for j < len(src.samples) && (before(src.starts[j], src.cfg.Timescale, at, cuts.ts) ||
			(src.track.Kind == Video && !src.samples[j].Sync)) {
			j++
		}
		if j >= len(src.samples) {
			break
		}
		if j > prev {
			idx = append(idx, j)
			prev = j
		}
	}
	return idx
}

// cutMedia writes a video or audio track's segments.
func cutMedia(src *segSource, ref *segSource, cuts refCuts, indexed bool) (SegmentedTrack, error) {
	st := SegmentedTrack{Timescale: src.cfg.Timescale, MIME: "video/mp4"}
	if src.track.Kind == Audio {
		st.MIME = "audio/mp4"
	}
	codecs, err := codecsOf(src.cfg)
	if err != nil {
		return st, err
	}
	st.Codecs = codecs
	var buf bytes.Buffer
	// A fragment is closed by Flush alone: the limit is out of reach.
	m := NewMuxer(segmentSink(&buf),FragmentDuration(MaxSegmentDuration*1000), Brand(segmentBrand))
	cfg := src.cfg
	id, err := m.AddTrack(cfg)
	if err != nil {
		return st, err
	}
	bounds := append([]int{0}, cutIndexes(src, cuts, ref)...)
	bounds = append(bounds, len(src.samples))
	var total, peak uint64
	for k := 0; k+1 < len(bounds); k++ {
		lo, hi := bounds[k], bounds[k+1]
		buf.Reset()
		for _, s := range src.samples[lo:hi] {
			if src.track.Kind == Audio {
				s.Sync = true
			}
			if err := m.WriteSample(id, s); err != nil {
				return st, err
			}
		}
		if err := m.Flush(); err != nil {
			return st, err
		}
		// The first flush also wrote the initialisation segment, which is
		// everything before the first moof.
		body, off := buf.Bytes(), 0
		if k == 0 {
			for _, b := range segmentBoxes(body) {
				if b.typ == "moof" {
					break
				}
				off = b.end
			}
			st.Init = bytes.Clone(body[:off])
		}
		start, dur := src.starts[lo], src.end-src.starts[lo]
		if hi < len(src.samples) {
			dur = src.starts[hi] - src.starts[lo]
		}
		seg := bytes.Clone(body[off:])
		if indexed {
			sidx, err := segmentIndexBox(src, lo, hi, uint64(len(seg)), dur)
			if err != nil {
				return st, err
			}
			seg = withIndex(sidx, seg)
		}
		st.Segments = append(st.Segments, MediaSegment{Data: seg, Start: start, Duration: dur})
		total += uint64(len(seg))
		if dur > 0 {
			peak = max(peak, bitrate(uint64(len(seg)), dur, src.cfg.Timescale))
		}
	}
	st.Bandwidth = peak
	st.AverageBandwidth = bitrate(total, src.end, src.cfg.Timescale)
	return st, nil
}

// segmentSink is what the muxer writes into; it is a variable so that the failure
// of a write, which a memory buffer never reports, can be tested.
var segmentSink = func(b *bytes.Buffer) io.Writer { return b }

// bitrate is bytes over ticks of a timescale, in bits per second; 0 when ticks is
// 0 and saturated when it does not fit.
func bitrate(bytes, ticks uint64, ts uint32) uint64 {
	if ticks == 0 {
		return 0
	}
	h, l := bits.Mul64(bytes*8, uint64(ts))
	if h >= ticks {
		return math.MaxUint64
	}
	q, _ := bits.Div64(h, l, ticks)
	return q
}

// segmentBrand is the major brand of every initialisation segment; ffmpeg's dash
// muxer writes the same.
const segmentBrand = "iso6"

type segBox struct {
	typ        string
	start, end int
}

// segmentBoxes lists the top-level boxes of b, stopping at the first whose size
// does not fit.
func segmentBoxes(b []byte) []segBox {
	var out []segBox
	for at := 0; len(b)-at >= 8; {
		size := uint64(binary.BigEndian.Uint32(b[at:]))
		hdr := uint64(8)
		switch size {
		case 0:
			size = uint64(len(b) - at)
		case 1:
			if len(b)-at < 16 {
				return out
			}
			size, hdr = binary.BigEndian.Uint64(b[at+8:]), 16
		}
		if size < hdr || size > uint64(len(b)-at) {
			return out
		}
		out = append(out, segBox{typ: string(b[at+4 : at+8]), start: at, end: at + int(size)})
		at += int(size)
	}
	return out
}

// segmentIndexBox is the sidx of a segment of samples lo..hi: one reference to
// the whole segment, starting with a stream access point (ISO/IEC 14496-12
// §8.16.3). size is the referenced bytes, dur the subsegment's duration.
func segmentIndexBox(src *segSource, lo, hi int, size, dur uint64) (*mp4.SidxBox, error) {
	if size > maxReferencedSize || dur > math.MaxUint32 {
		return nil, fmt.Errorf("%w: a segment too large to index", ErrMediaTooLarge)
	}
	earliest := int64(math.MaxInt64)
	for i := lo; i < hi; i++ {
		earliest = min(earliest, max(int64(src.starts[i])+int64(src.samples[i].CompositionOffset), 0))
	}
	return &mp4.SidxBox{
		Version: 1, ReferenceID: 1, Timescale: src.cfg.Timescale,
		EarliestPresentationTime: uint64(earliest),
		SidxRefs: []mp4.SidxRef{{
			ReferencedSize: uint32(size), SubSegmentDuration: uint32(dur),
			StartsWithSAP: 1, SAPType: 1,
		}},
	}, nil
}

// withIndex puts a styp and the sidx in front of a segment.
func withIndex(sidx *mp4.SidxBox, frag []byte) []byte {
	var out bytes.Buffer
	// Encoding into memory cannot fail.
	_ = mp4.NewStyp("msdh", 0, []string{"msdh", "msix"}).Encode(&out)
	_ = sidx.Encode(&out)
	out.Write(frag)
	return out.Bytes()
}

// codecsOf is the RFC 6381 codecs parameter of a track.
func codecsOf(cfg TrackConfig) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%w: %s: %s", ErrSegmentTrack, cfg.Codec, why)
	}
	switch cfg.Codec {
	case "avc1", "avc3":
		if len(cfg.SPS) == 0 {
			return bad("no SPS")
		}
		sps, err := avc.ParseSPSNALUnit(cfg.SPS[0], false)
		if err != nil {
			return bad(err.Error())
		}
		// mp4ff spells the hex digits in capitals; ffmpeg, and the examples of
		// RFC 6381 and Apple's HLS Authoring Specification, in lower case.
		return strings.ToLower(avc.CodecString(cfg.Codec, sps)), nil
	case "hvc1", "hev1":
		if len(cfg.SPS) == 0 {
			return bad("no SPS")
		}
		sps, err := hevc.ParseSPSNALUnit(cfg.SPS[0])
		if err != nil {
			return bad(err.Error())
		}
		return hevc.CodecString(cfg.Codec, sps), nil
	case "av01":
		rec, err := av1.DecodeAV1CodecConfRec(cfg.CodecConfig)
		if err != nil {
			return bad(err.Error())
		}
		return rec.CodecString(cfg.Codec), nil
	case "vp08", "vp09":
		if cfg.VPx == nil {
			return bad("no VPx configuration")
		}
		v := cfg.VPx
		return fmt.Sprintf("%s.%02d.%02d.%02d", cfg.Codec, v.Profile, v.Level, v.BitDepth), nil
	case "mp3", "mp2", "mp1":
		if cfg.Codec == "mp3" {
			return "mp4a.6B", nil
		}
		return "mp4a.69", nil
	case "mp4a":
		asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(cfg.CodecConfig))
		if err != nil {
			if len(cfg.CodecConfig) == 0 {
				aot := cfg.AudioObjectType
				if aot == 0 {
					aot = 2
				}
				return fmt.Sprintf("mp4a.40.%d", aot), nil
			}
			return bad(err.Error())
		}
		aot := asc.ObjectType
		switch {
		case asc.PSPresentFlag:
			aot = 29
		case asc.SBRPresentFlag:
			aot = 5
		}
		return fmt.Sprintf("mp4a.40.%d", aot), nil
	case "Opus", "fLaC", "ac-3", "ec-3", "alac":
		return cfg.Codec, nil
	}
	return bad("no codecs string known")
}

// cutText writes a WebVTT track's segments.
func cutText(src *segSource, ref *segSource, cuts refCuts) (SegmentedTrack, error) {
	const msScale = 1000
	st := SegmentedTrack{Timescale: msScale, MIME: "text/vtt", Codecs: "wvtt"}
	var (
		asm  cueAssembler
		all  []webvttCue
		scal = src.cfg.Timescale
	)
	for i, s := range src.samples {
		cs, err := readWebVTTSample(s.Data)
		if err != nil {
			return st, err
		}
		all = append(all, asm.add(src.starts[i], src.starts[i]+uint64(s.Duration), cs)...)
	}
	all = append(all, asm.drain()...)
	toMS := func(ticks uint64, ts uint32) uint64 {
		// Whole seconds first, so nothing overflows below 570 million years.
		return ticks/uint64(ts)*msScale + ticks%uint64(ts)*msScale/uint64(ts)
	}
	// The windows are the reference's spans, on the millisecond clock; a track
	// longer than the reference gets a last window that reaches its own end.
	edges := []uint64{0}
	for _, at := range cuts.at {
		edges = append(edges, toMS(at, cuts.ts))
	}
	end := max(toMS(cuts.end, cuts.ts), toMS(src.end, scal))
	edges = append(edges, end)
	for k := 0; k+1 < len(edges); k++ {
		lo, hi := edges[k], edges[k+1]
		var doc strings.Builder
		doc.WriteString("WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n")
		for _, c := range all {
			cs, ce := toMS(c.start, scal), toMS(c.end, scal)
			if cs >= hi || ce <= lo {
				continue
			}
			if err := writeCue(&doc, c, cs, ce); err != nil {
				return st, err
			}
		}
		st.Segments = append(st.Segments, MediaSegment{Data: []byte(doc.String()), Start: lo, Duration: hi - lo})
	}
	return st, nil
}

// writeCue writes one cue, refusing text that would end it early or start
// another: a blank line, or an arrow, where the WebVTT grammar has none.
func writeCue(w io.Writer, c webvttCue, startMS, endMS uint64) error {
	for _, field := range []string{c.id, c.settings} {
		if strings.ContainsAny(field, "\r\n") || strings.Contains(field, "-->") {
			return fmt.Errorf("%w: a WebVTT cue identifier or setting holds a line break or an arrow", ErrSample)
		}
	}
	text := strings.ReplaceAll(c.text, "\r\n", "\n")
	if strings.Contains(text, "\n\n") || strings.Contains(text, "-->") || strings.HasPrefix(text, "\n") ||
		strings.HasSuffix(text, "\n") {
		return fmt.Errorf("%w: WebVTT cue text holds a blank line or an arrow", ErrSample)
	}
	for _, n := range c.comments {
		if strings.ContainsAny(n, "\r\n") || strings.Contains(n, "-->") {
			return fmt.Errorf("%w: a WebVTT NOTE holds a line break or an arrow", ErrSample)
		}
		fmt.Fprintf(w, "NOTE %s\n\n", n)
	}
	if c.id != "" {
		fmt.Fprintln(w, c.id)
	}
	fmt.Fprintf(w, "%s --> %s", vttClock(startMS), vttClock(endMS))
	if c.settings != "" {
		fmt.Fprintf(w, " %s", c.settings)
	}
	fmt.Fprintf(w, "\n%s\n\n", text)
	return nil
}

// vttClock formats milliseconds as a WebVTT timestamp, hh:mm:ss.mmm.
func vttClock(ms uint64) string {
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// WriteDir writes every file of the segmentation into dir, creating it. File
// names are generated, and every write goes through os.Root, so nothing can land
// outside dir whatever a name holds. An existing file of the same name is
// replaced. It returns the names written, in order.
func (s *Segmentation) WriteDir(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var names []string
	write := func(name string, data []byte) error {
		if !plainFileName(name) {
			return fmt.Errorf("%w: %q", ErrSegmentName, name)
		}
		if err := root.WriteFile(name, data, 0o644); err != nil {
			return err
		}
		names = append(names, name)
		return nil
	}
	for i := range s.Tracks {
		t := &s.Tracks[i]
		if t.Init != nil {
			if err := write(t.InitName(), t.Init); err != nil {
				return names, err
			}
		}
		for k, seg := range t.Segments {
			if err := write(t.SegmentName(k), seg.Data); err != nil {
				return names, err
			}
		}
	}
	return names, nil
}

// plainFileName reports whether name is one path element made of letters,
// digits, underscore, dash and dot, and not a dot-leading one.
func plainFileName(name string) bool {
	if name == "" || len(name) > 255 || name[0] == '.' {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}
