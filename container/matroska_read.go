// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// ErrMatroska means the Matroska document states something that cannot be
// carried over as samples: a timestamp scale no timescale can express, a laced
// block nothing times, a last sample nothing measures.
var ErrMatroska = errors.New("container: matroska")

// nanosecondsPerSecond is what a Matroska TimestampScale is counted in.
const nanosecondsPerSecond = 1_000_000_000

// The Matroska Colour code points this reader acts on. Range says how the
// samples use their bit depth, ChromaSiting where a chroma sample sits against
// the luma ones.
const (
	mkvRangeFull       = 2
	mkvSitingColocated = 1
)

// The vpcC chroma subsampling codes, which Matroska states as a horizontal and
// a vertical count instead.
const (
	vpxChroma420Vertical  = 0
	vpxChroma420Colocated = 1
	vpxChroma422          = 2
	vpxChroma444          = 3
)

// mkvReadDoc is the whole EBML/Matroska tree a reader needs: the header, the
// segment's Info and Tracks, and every Cluster with the blocks in it.
type mkvReadDoc struct {
	Header  mkvHeader      `ebml:"EBML"`
	Segment mkvReadSegment `ebml:"Segment"`
}

type mkvReadSegment struct {
	Info    mkvInfo      `ebml:"Info"`
	Tracks  mkvTracks    `ebml:"Tracks"`
	Cluster []mkvCluster `ebml:"Cluster"`
}

// mkvCluster is one cluster: the timestamp its blocks are counted from, then
// the blocks themselves in the two forms a cluster may hold them in.
type mkvCluster struct {
	Timecode    uint64          `ebml:"Timecode"`
	SimpleBlock []ebml.Block    `ebml:"SimpleBlock"`
	BlockGroup  []mkvBlockGroup `ebml:"BlockGroup"`
}

// mkvBlockGroup wraps a block with what a SimpleBlock cannot state: how long it
// lasts, and the references that make it something a player cannot start at.
//
// ReferenceBlock is a slice because it is its absence, not its value, that
// marks a sync sample: a plain integer could not tell a reference of zero — a
// frame referring to one at the same timestamp — from no reference at all.
type mkvBlockGroup struct {
	BlockDuration  uint64     `ebml:"BlockDuration,omitempty"`
	ReferenceBlock []int64    `ebml:"ReferenceBlock,omitempty"`
	Block          ebml.Block `ebml:"Block"`
}

// mkvReadTrack is one Matroska track, read and converted.
//
// A file may carry a track this package cannot describe next to tracks it can,
// so what went wrong is kept per track rather than failing the whole file: the
// readable tracks stay readable.
type mkvReadTrack struct {
	track      Track
	config     TrackConfig
	samples    []Sample
	configErr  error
	samplesErr error
}

// mkvBlockRef is one block of a cluster, with the cluster timestamp its own is
// counted from and the group that wraps it, if any.
type mkvBlockRef struct {
	clusterTime uint64
	block       *ebml.Block
	group       *mkvBlockGroup
}

// mkvBlock is one of a track's blocks, in file order: the frames it carries,
// the presentation time the file states for it, whether a player may start
// there, and how long the file says it lasts — zero when it says nothing.
type mkvBlock struct {
	frames [][]byte
	time   int64
	stated int64
	sync   bool
}

// mkvFrame is one frame of a track, once a laced block has been taken apart.
type mkvFrame struct {
	data   []byte
	pts    int64
	stated int64
	sync   bool
}

// newMatroskaReader reads a Matroska or WebM file whole. There is no sample
// table to seek with, so every cluster is walked and every block converted in
// one pass, the way a transport stream is.
func newMatroskaReader(data []byte) (*Reader, error) {
	doc, groups, err := unmarshalMatroska(data)
	if err != nil {
		return nil, err
	}
	scale := doc.Segment.Info.TimecodeScale
	if scale == 0 {
		scale = defaultTimecodeScale
	}
	timescale, err := mkvTimescale(scale)
	if err != nil {
		return nil, err
	}
	refs, err := blockOrder(groups, doc.Segment.Cluster)
	if err != nil {
		return nil, err
	}
	blocks := map[uint64][]mkvBlock{}
	for _, ref := range refs {
		number := ref.block.TrackNumber
		blocks[number] = append(blocks[number], mkvBlockOf(ref))
	}
	r := &Reader{data: data, mkv: map[uint32]*mkvReadTrack{}}
	file := &File{Format: matroskaFormat(doc.Header.DocType), Timescale: timescale}
	segDuration := int64(doc.Segment.Info.Duration)
	for _, te := range doc.Segment.Tracks.TrackEntry {
		t := mkvReadTrackOf(te, timescale, scale, segDuration, blocks[te.TrackNumber])
		file.Tracks = append(file.Tracks, t.track)
		if t.track.Duration > file.Duration {
			file.Duration = t.track.Duration
		}
		r.mkv[t.track.ID] = t
	}
	r.file = file
	return r, nil
}

// mkvReadTrackOf converts one TrackEntry and the blocks that name it.
func mkvReadTrackOf(te mkvTrackEntry, timescale uint32, scale uint64,
	segDuration int64, blocks []mkvBlock) *mkvReadTrack {
	t := &mkvReadTrack{track: mkvTrack(te, timescale, 0)}
	t.config, t.configErr = mkvTrackConfig(te, timescale)
	if t.configErr == nil {
		// The sample entry, not the CodecID, is what a caller writing this
		// track elsewhere states, and an Opus identification header knows the
		// track's own rate better than the Audio element does.
		t.track.Codec = t.config.Codec
		t.track.Channels, t.track.SampleRate = t.config.Channels, t.config.SampleRate
	}
	// DefaultDuration is stated in nanoseconds; every other duration of a
	// Matroska file is counted in timestamp ticks, so it is converted here
	// once. A frame shorter than a tick rounds to nothing and is treated as
	// unstated rather than as a zero-length frame.
	perFrame := int64((te.DefaultDuration + scale/2) / scale)
	t.samples, t.samplesErr = mkvSamples(uint32(te.TrackNumber), blocks, perFrame, segDuration)
	for _, s := range t.samples {
		t.track.Duration += uint64(s.Duration)
	}
	return t
}

// unmarshalMatroska reads the whole document with the reference ebml-go
// library, recording on the way which form each of a cluster's blocks was
// written in — see blockOrder for why that has to be recorded rather than read
// back off the document.
//
// Unknown-element tolerance is deliberately off, for the same reason
// demuxMatroska leaves it off: ebml-go's ignore-unknown mode swallows every
// read and size error and returns whatever it had, so a truncated file would
// read as a short one.
func unmarshalMatroska(data []byte) (*mkvReadDoc, []bool, error) {
	var doc mkvReadDoc
	var groups []bool
	hook := func(e *ebml.Element) {
		switch e.Type {
		case ebml.ElementSimpleBlock:
			groups = append(groups, false)
		case ebml.ElementBlockGroup:
			groups = append(groups, true)
		}
	}
	if err := ebml.Unmarshal(bytes.NewReader(data), &doc, ebml.WithElementReadHooks(hook)); err != nil {
		return nil, nil, err
	}
	return &doc, groups, nil
}

// blockOrder puts a document's blocks back into the order they were written in.
//
// ebml-go unmarshals a cluster's SimpleBlock and BlockGroup children into two
// separate slices, which keeps each form's own order but loses the order of the
// two against one another: a track that uses both forms inside one cluster —
// plain frames as simple blocks, a frame needing an explicit duration as a
// group — would have its frames read out of decode order, and a decoder handed
// frames out of order produces rubbish rather than an error. The element read
// hook is the one place that order survives, so groups says, in file order,
// whether each block was a group, and this walks the two records together.
//
// It is an indirection so that the failures it guards can be tested: the record
// the hook keeps and the document ebml-go fills in are made in the same pass
// and cannot come out disagreeing, and code that cannot be exercised is code
// nobody knows the behaviour of.
var blockOrder = func(groups []bool, clusters []mkvCluster) ([]mkvBlockRef, error) {
	var out []mkvBlockRef
	read := 0
	for i := range clusters {
		cluster := &clusters[i]
		held := len(cluster.SimpleBlock) + len(cluster.BlockGroup)
		if read+held > len(groups) {
			return nil, fmt.Errorf("%w: cluster %d holds %d blocks and only %d were recorded",
				ErrMatroska, i+1, held, len(groups)-read)
		}
		simple, group := 0, 0
		for _, isGroup := range groups[read : read+held] {
			switch {
			case isGroup && group == len(cluster.BlockGroup):
				return nil, fmt.Errorf("%w: cluster %d was recorded with more block groups than its %d",
					ErrMatroska, i+1, len(cluster.BlockGroup))
			case isGroup:
				g := &cluster.BlockGroup[group]
				out = append(out, mkvBlockRef{clusterTime: cluster.Timecode, block: &g.Block, group: g})
				group++
			case simple == len(cluster.SimpleBlock):
				return nil, fmt.Errorf("%w: cluster %d was recorded with more simple blocks than its %d",
					ErrMatroska, i+1, len(cluster.SimpleBlock))
			default:
				out = append(out, mkvBlockRef{clusterTime: cluster.Timecode,
					block: &cluster.SimpleBlock[simple]})
				simple++
			}
		}
		read += held
	}
	if read != len(groups) {
		return nil, fmt.Errorf("%w: %d blocks were recorded and %d belong to a cluster",
			ErrMatroska, len(groups), read)
	}
	return out, nil
}

// mkvBlockOf reads one block: its presentation time is its cluster's plus its
// own, and a player may start at it when the simple block says so or when the
// group around it names no block it refers to.
func mkvBlockOf(ref mkvBlockRef) mkvBlock {
	b := mkvBlock{
		frames: ref.block.Data,
		time:   int64(ref.clusterTime) + int64(ref.block.Timecode),
		sync:   ref.block.Keyframe,
	}
	if ref.group != nil {
		b.stated = int64(ref.group.BlockDuration)
		b.sync = len(ref.group.ReferenceBlock) == 0
	}
	return b
}

// mkvTimescale is how many timestamp ticks a second holds, given the
// TimestampScale that says how many nanoseconds one tick is worth. A scale that
// does not divide a second exactly has no timescale to state it, and is refused
// rather than rounded: every duration in the file would be off by the rounding.
func mkvTimescale(scale uint64) (uint32, error) {
	if scale == 0 || nanosecondsPerSecond%scale != 0 {
		return 0, fmt.Errorf("%w: a TimestampScale of %d ns is not a whole part of a second",
			ErrMatroska, scale)
	}
	return uint32(nanosecondsPerSecond / scale), nil
}

// mkvSamples turns one track's blocks into the samples a muxer takes.
func mkvSamples(trackID uint32, blocks []mkvBlock, perFrame, segDuration int64) ([]Sample, error) {
	if len(blocks) == 0 {
		return nil, fmt.Errorf("%w: track %d has no block", ErrNoSamples, trackID)
	}
	frames, err := mkvUnlace(trackID, blocks, perFrame)
	if err != nil {
		return nil, err
	}
	return mkvTimeFrames(trackID, frames, perFrame, segDuration)
}

// mkvUnlace takes each block apart into the frames it carries. A block usually
// carries one, and then it is the block's own; a laced block carries several
// that share one timestamp, and the file has to say how long the block lasts
// for them to be timed at all — lacing states no timestamp of its own.
func mkvUnlace(trackID uint32, blocks []mkvBlock, perFrame int64) ([]mkvFrame, error) {
	var out []mkvFrame
	for i, b := range blocks {
		count := int64(len(b.frames))
		if count == 1 {
			out = append(out, mkvFrame{data: b.frames[0], pts: b.time, stated: b.stated, sync: b.sync})
			continue
		}
		// Matroska laces only frames that refer to nothing and last the same
		// time, so the block's own span split evenly is exactly right.
		span := b.stated
		if span == 0 {
			span = perFrame * count
		}
		if span <= 0 {
			return nil, fmt.Errorf(
				"%w: block %d of track %d laces %d frames and the file states neither a block duration nor a default duration to time them by",
				ErrMatroska, i+1, trackID, count)
		}
		for j := int64(0); j < count; j++ {
			out = append(out, mkvFrame{
				data: b.frames[j],
				pts:  b.time + span*j/count,
				// Splitting the span this way, rather than by a rounded
				// share, keeps the frames' durations summing to it exactly.
				stated: span*(j+1)/count - span*j/count,
				sync:   b.sync,
			})
		}
	}
	return out, nil
}

// mkvTimeFrames states each frame's duration and, where a stream is stored out
// of presentation order, how far its presentation runs behind its decoding.
//
// A Matroska block states when its frame is shown, not when it is decoded, and
// a Sample states a duration rather than a time — a muxer adds them up. So the
// decode times are the presentation times put in order, each sample lasting
// until the next one is decoded, which is what makes the times a muxer writes
// out come back exactly as the file stated them. The offset between the two is
// shifted by the largest reordering in the track, so that no sample is shown
// before it is decoded; a track already in order is shifted by nothing and
// every offset is zero.
func mkvTimeFrames(trackID uint32, frames []mkvFrame, perFrame, segDuration int64) ([]Sample, error) {
	decode := make([]int64, len(frames))
	for i, f := range frames {
		decode[i] = f.pts
	}
	slices.Sort(decode)
	var delay int64
	for i, f := range frames {
		if behind := decode[i] - f.pts; behind > delay {
			delay = behind
		}
	}
	out := make([]Sample, 0, len(frames))
	for i, f := range frames {
		var duration int64
		if i+1 < len(frames) {
			duration = decode[i+1] - decode[i]
		} else {
			last, err := mkvLastDuration(trackID, f.stated, perFrame, segDuration-decode[i], out)
			if err != nil {
				return nil, err
			}
			duration = last
		}
		if duration <= 0 || duration > math.MaxUint32 {
			return nil, fmt.Errorf("%w: sample %d of track %d would last %d ticks",
				ErrMatroska, i+1, trackID, duration)
		}
		offset := f.pts + delay - decode[i]
		if offset > math.MaxInt32 {
			return nil, fmt.Errorf("%w: sample %d of track %d is shown %d ticks after it is decoded, which no sample table can state",
				ErrMatroska, i+1, trackID, offset)
		}
		out = append(out, Sample{
			Data:              f.data,
			Duration:          uint32(duration),
			CompositionOffset: int32(offset),
			Sync:              f.sync,
		})
	}
	return out, nil
}

// mkvLastDuration is how long the last sample of a track lasts. It is the one
// sample no following block can measure, so it is taken from what the file does
// state: the block's own duration, the track's default duration, what is left
// of the segment's, or, failing all three, as long as the sample before it. A
// track that states none of those, and has no sample before, cannot be given a
// duration at all, and saying so beats writing a zero a muxer would refuse.
func mkvLastDuration(trackID uint32, stated, perFrame, remaining int64, written []Sample) (int64, error) {
	switch {
	case stated > 0:
		return stated, nil
	case perFrame > 0:
		return perFrame, nil
	case remaining > 0:
		return remaining, nil
	case len(written) > 0:
		return int64(written[len(written)-1].Duration), nil
	}
	return 0, fmt.Errorf("%w: nothing in the file states how long the last sample of track %d lasts",
		ErrMatroska, trackID)
}

// mkvCodecs maps a Matroska CodecID onto the sample entry TrackConfig states.
//
// Vorbis and FLAC are named here because a caller is owed the name of what a
// track holds even when this package cannot write it: the samples of such a
// track still read, and it is Muxer.AddTrack that reports the sample entry it
// cannot build.
var mkvCodecs = map[string]string{
	"V_MPEG4/ISO/AVC":  "avc1",
	"V_MPEGH/ISO/HEVC": "hvc1",
	"V_AV1":            "av01",
	"V_VP9":            "vp09",
	"V_VP8":            "vp08",
	"A_OPUS":           "Opus",
	"A_VORBIS":         "vorb",
	"A_FLAC":           "fLaC",
}

// mkvCodecFamilies maps the CodecIDs Matroska writes with a suffix: A_AAC and
// A_AAC/MPEG4/LC both name AAC, A_AC3 and A_AC3/BSID9 both name AC-3. Enhanced
// AC-3 is matched before AC-3 would be, since neither name is a prefix of the
// other but the intent is easier to read in order.
var mkvCodecFamilies = []struct{ prefix, codec string }{
	{"A_AAC", "mp4a"},
	{"A_EAC3", "ec-3"},
	{"A_AC3", "ac-3"},
}

// mkvCodec is the sample entry a Matroska CodecID names.
func mkvCodec(codecID string) (string, bool) {
	if codec, ok := mkvCodecs[codecID]; ok {
		return codec, true
	}
	for _, family := range mkvCodecFamilies {
		if strings.HasPrefix(codecID, family.prefix) {
			return family.codec, true
		}
	}
	return "", false
}

// mkvTrackConfig describes a Matroska track the way Muxer.AddTrack wants it,
// decoding the codec's private data into the fields each codec is described by.
func mkvTrackConfig(te mkvTrackEntry, timescale uint32) (TrackConfig, error) {
	codec, ok := mkvCodec(te.CodecID)
	if !ok {
		return TrackConfig{}, fmt.Errorf("%w: matroska codec id %q", ErrUnsupportedCodec, te.CodecID)
	}
	cfg := TrackConfig{
		Kind:      kindFromTrackType(te.TrackType),
		Codec:     codec,
		Timescale: timescale,
		Language:  te.Language,
	}
	if te.Video != nil {
		cfg.Width, cfg.Height = int(te.Video.PixelWidth), int(te.Video.PixelHeight)
	}
	if te.Audio != nil {
		cfg.Channels, cfg.SampleRate = int(te.Audio.Channels), int(te.Audio.SamplingFrequency)
	}
	if err := mkvCodecPrivate(&cfg, te); err != nil {
		return TrackConfig{}, err
	}
	return cfg, nil
}

// mkvCodecPrivate decodes a track's CodecPrivate into the fields its codec is
// described by. What that data holds is the codec's own Matroska mapping, so
// each codec reads it its own way.
func mkvCodecPrivate(cfg *TrackConfig, te mkvTrackEntry) error {
	switch cfg.Codec {
	case "avc1":
		record, err := avc.DecodeAVCDecConfRec(te.CodecPrivate)
		if err != nil {
			return fmt.Errorf("%w: %s avcC: %v", ErrTrackConfig, te.CodecID, err)
		}
		cfg.SPS, cfg.PPS = record.SPSnalus, record.PPSnalus
	case "hvc1":
		record, err := hevc.DecodeHEVCDecConfRec(te.CodecPrivate)
		if err != nil {
			return fmt.Errorf("%w: %s hvcC: %v", ErrTrackConfig, te.CodecID, err)
		}
		cfg.VPS, cfg.SPS, cfg.PPS = hevcParameterSets(&mp4.HvcCBox{DecConfRec: record})
	case "av01":
		if len(te.CodecPrivate) == 0 {
			return fmt.Errorf("%w: %s states no av1C configuration", ErrTrackConfig, te.CodecID)
		}
		cfg.CodecConfig = te.CodecPrivate
	case "Opus":
		// The identification header is what a Matroska file carries and what
		// the muxer reads back, so it travels as it stands; PreSkip is lifted
		// out of it because a caller writing a track without the header still
		// needs it.
		head, err := opusHead(te.CodecPrivate)
		if err != nil {
			return err
		}
		cfg.CodecConfig = te.CodecPrivate
		cfg.PreSkip = head.PreSkip
		cfg.Channels = int(head.OutputChannelCount)
		cfg.SampleRate = int(head.InputSampleRate)
	case "mp4a":
		// AAC in Matroska may leave its audio specific config out, and then
		// the profile is the AAC-LC a zero stands for.
		if len(te.CodecPrivate) == 0 {
			return nil
		}
		asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(te.CodecPrivate))
		if err != nil {
			return fmt.Errorf("%w: %s audio specific config: %v", ErrTrackConfig, te.CodecID, err)
		}
		cfg.AudioObjectType = asc.ObjectType
		if cfg.SampleRate == 0 {
			cfg.SampleRate = asc.SamplingFrequency
		}
	case "vp08", "vp09":
		cfg.VPx = mkvVPx(te.Video)
	default:
		// AC-3, Enhanced AC-3, Vorbis and FLAC state their configuration in
		// their own form, which travels as it stands: AC-3 usually states
		// none at all in Matroska, keeping it in the frames instead.
		cfg.CodecConfig = te.CodecPrivate
	}
	return nil
}

// mkvVPx is what a WebM's Colour element says about a VP8 or VP9 track.
//
// Matroska states neither the profile nor the level of a VP8 or VP9 track —
// both live in the frames themselves — so both are left at zero, and it is
// Muxer.AddTrack that says a level is missing. Everything a Colour element does
// state is carried over; what it leaves out keeps the value ISO/IEC 23001-8
// gives to "unspecified", except the bit depth, whose only unspecified value
// would be no depth at all.
func mkvVPx(video *mkvVideo) *VPxConfig {
	cfg := &VPxConfig{
		BitDepth:                8,
		ChromaSubsampling:       vpxChroma420Vertical,
		ColourPrimaries:         2,
		TransferCharacteristics: 2,
		MatrixCoefficients:      2,
	}
	if video == nil || video.Colour == nil {
		return cfg
	}
	colour := video.Colour
	if colour.BitsPerChannel != 0 {
		cfg.BitDepth = byte(colour.BitsPerChannel)
	}
	if colour.Primaries != 0 {
		cfg.ColourPrimaries = byte(colour.Primaries)
	}
	if colour.TransferCharacteristics != 0 {
		cfg.TransferCharacteristics = byte(colour.TransferCharacteristics)
	}
	if len(colour.MatrixCoefficients) != 0 {
		cfg.MatrixCoefficients = byte(colour.MatrixCoefficients[0])
	}
	cfg.FullRange = colour.Range == mkvRangeFull
	if chroma, ok := mkvChromaSubsampling(colour); ok {
		cfg.ChromaSubsampling = chroma
	}
	return cfg
}

// mkvChromaSubsampling maps the horizontal and vertical subsampling Matroska
// states separately onto the single code a vpcC record states. Only the three
// combinations below have one, and a file stating anything else keeps the
// default rather than being given a code that means something different.
func mkvChromaSubsampling(colour *mkvColour) (byte, bool) {
	if len(colour.ChromaSubsamplingHorz) == 0 || len(colour.ChromaSubsamplingVert) == 0 {
		return 0, false
	}
	horizontal, vertical := colour.ChromaSubsamplingHorz[0], colour.ChromaSubsamplingVert[0]
	switch {
	case horizontal == 1 && vertical == 1:
		if colour.ChromaSitingVert == mkvSitingColocated {
			return vpxChroma420Colocated, true
		}
		return vpxChroma420Vertical, true
	case horizontal == 1 && vertical == 0:
		return vpxChroma422, true
	case horizontal == 0 && vertical == 0:
		return vpxChroma444, true
	}
	return 0, false
}
