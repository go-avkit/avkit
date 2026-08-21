// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// ErrMediaTooLarge means the media a progressive file has to hold before its
// sample tables can be written exceeds what the muxer is allowed to keep in
// memory. A writer that can seek reports it only for a single sample larger
// than the whole limit, having handed everything else over already.
var ErrMediaTooLarge = errors.New("container: media data exceeds the muxer's memory limit")

// DefaultChunkDuration is how much of one track's media a chunk holds when the
// caller states nothing: short enough that the tracks stay interleaved for a
// player reading the file front to back, long enough that the chunk tables stay
// small.
const DefaultChunkDuration = 500 * time.Millisecond

// DefaultMediaMemoryLimit is how much media data a progressive muxer holds
// before it gives up. See ProgressiveMuxer for what is held and why.
const DefaultMediaMemoryLimit = 256 << 20

// DefaultProgressiveBrand is the major brand written in the ftyp of a
// progressive file. It is not the brand a fragmented file claims: a progressive
// file promises none of the segment structure a DASH brand announces.
const DefaultProgressiveBrand = "isom"

// progressiveMovieTimescale is the unit of the durations the movie header and
// the track headers state. Milliseconds are enough for a duration, and each
// track keeps its own timescale for its samples.
const progressiveMovieTimescale = 1000

// mdatHeaderLen and mdatLargeHeaderLen are the two forms of an mdat header: a
// 32-bit size, and the 64-bit size a box announces by stating a size of one.
const (
	mdatHeaderLen      = 8
	mdatLargeHeaderLen = 16
)

// maxMdatPayload is the largest payload an mdat box with a 32-bit size field
// can announce, and so the most media a buffering muxer can be asked to hold.
const maxMdatPayload = 1<<32 - 1 - mdatHeaderLen

// maxChunkOffset32 is the largest file offset a stco entry can state. A file
// whose chunks reach further needs co64, and a writer that gets this wrong
// produces something that plays until the offsets silently wrap.
//
// It is a variable rather than a constant so a test can lower it and exercise
// the crossover on a file of a few hundred bytes; the value itself is checked
// against a writer positioned either side of the real boundary.
var maxChunkOffset32 uint64 = math.MaxUint32

// ProgressiveOption configures a ProgressiveMuxer.
type ProgressiveOption func(*progressiveSettings)

type progressiveSettings struct {
	chunkDuration time.Duration
	brand         string
	memoryLimit   uint64
}

// ChunkDuration sets how much of a track's media one chunk holds. A value of
// zero or less restores the default.
func ChunkDuration(d time.Duration) ProgressiveOption {
	return func(s *progressiveSettings) {
		if d <= 0 {
			d = DefaultChunkDuration
		}
		s.chunkDuration = d
	}
}

// ProgressiveBrand sets the major brand written in ftyp. An empty brand
// restores the default.
func ProgressiveBrand(b string) ProgressiveOption {
	return func(s *progressiveSettings) {
		if b != "" {
			s.brand = b
		}
	}
}

// MediaMemoryLimit sets how much media data the muxer holds at once, which for
// a writer that cannot seek is the whole file. Zero restores the default, and a
// limit above what an mdat box with a 32-bit size can announce is lowered to it,
// since more than that could not be written out as one box.
func MediaMemoryLimit(n uint64) ProgressiveOption {
	return func(s *progressiveSettings) {
		switch {
		case n == 0:
			n = DefaultMediaMemoryLimit
		case n > maxMdatPayload:
			n = maxMdatPayload
		}
		s.memoryLimit = n
	}
}

// ProgressiveMuxer writes a progressive — non-fragmented — MP4: ftyp, then one
// mdat holding every sample, then a moov whose sample tables address them.
//
// It is the counterpart of Muxer, which writes the fragmented form a DASH or
// HLS presentation needs. A progressive file is what everything else expects:
// a player with no streaming stack, a hardware decoder, a tool that seeks by
// sample table. Neither re-encodes: samples are written as handed over.
//
// # Layout and memory
//
// The sample tables state where each chunk of media sits in the file, so they
// cannot be written before the media. mdat therefore comes first and moov last,
// which is what lets the media data go straight out to the writer.
//
// The media a muxer holds never exceeds MediaMemoryLimit, 256 MB by default,
// and a sample that would take it past that is refused with ErrMediaTooLarge
// rather than the muxer growing without end. What that limit has to cover
// depends on what the writer can do.
//
//   - A writer that is also an [io.Seeker] — an [os.File], say — is handed the
//     media as it arrives, so what is held is one open chunk per track: the
//     bound in practice is ChunkDuration of media per track, half a second by
//     default, and the limit is only reached by a caller asking for chunks
//     larger than it. The mdat of such a file carries a 64-bit size field,
//     because its value is only known once every sample has been written and
//     patching it must not move the media that follows.
//   - Any other writer — a [bytes.Buffer], a socket — cannot be sent an mdat
//     header whose size is not known yet, so the media is held until Close and
//     the whole file has to fit the limit. A file larger than that wants a
//     writer that can seek.
//
// The sample tables are held either way and cannot be bounded: moov has to
// list every sample. They cost some twenty bytes per sample, so two hours of
// 30 fps video beside one audio track is around 25 MB.
type ProgressiveMuxer struct {
	w        io.Writer
	seeker   io.Seeker // non-nil when the media can go out as it arrives
	settings progressiveSettings

	tracks []*progressiveTrack
	byID   map[uint32]*progressiveTrack

	// buffered holds the mdat payload of a writer that cannot seek.
	buffered []byte
	// pendingBytes is how much media sits in the tracks' open chunks. It and
	// the buffered payload are what the memory limit covers.
	pendingBytes uint64
	// written counts the payload bytes accounted for, whether they went out
	// or are still held.
	written uint64
	// payloadBase is where the mdat payload starts in the file, which is what
	// turns a chunk's position within the payload into the offset a chunk
	// table states.
	payloadBase uint64
	// mdatSizeAt is where the mdat size field sits, for a writer that will be
	// seeked back to it.
	mdatSizeAt uint64

	started bool
	closed  bool
}

// progressiveTrack is one track's writing state: the tables under construction
// and the samples not yet part of a chunk.
type progressiveTrack struct {
	id        uint32
	trak      *mp4.TrakBox
	timescale uint32

	count uint32 // samples written so far
	total uint64 // their total duration, in timescale units
	sizes []uint32

	// durCounts and durDeltas are the stts runs: a duration and how many
	// consecutive samples share it.
	durCounts, durDeltas []uint32
	// cttsCounts and cttsOffsets are the same run-length form for the
	// composition offsets, kept whether or not they are ever needed: a run
	// of equal offsets costs one entry, and zero is an offset like any other.
	cttsCounts  []uint32
	cttsOffsets []int32
	// shifted records that some sample presents away from its decode time,
	// which is the only reason to write a ctts at all.
	shifted bool
	// syncs are the numbers of the samples a player may start at.
	syncs []uint32

	// chunkOffsets are where each chunk starts within the mdat payload, and
	// chunkSamples how many samples each holds.
	chunkOffsets []uint64
	chunkSamples []uint32

	pending      [][]byte
	pendingDur   uint64
	pendingStart uint64 // decode time of the first pending sample
}

// NewProgressiveMuxer returns a ProgressiveMuxer writing to w. When w can also
// seek, the media is written as it arrives; otherwise it is held until Close.
func NewProgressiveMuxer(w io.Writer, opts ...ProgressiveOption) *ProgressiveMuxer {
	settings := progressiveSettings{
		chunkDuration: DefaultChunkDuration,
		brand:         DefaultProgressiveBrand,
		memoryLimit:   DefaultMediaMemoryLimit,
	}
	for _, opt := range opts {
		opt(&settings)
	}
	m := &ProgressiveMuxer{w: w, settings: settings, byID: map[uint32]*progressiveTrack{}}
	if s, ok := w.(io.Seeker); ok {
		m.seeker = s
	}
	return m
}

// AddTrack declares a track and returns its identifier. Every track must be
// added before the first sample is written, and every declared track must
// carry at least one sample: a progressive file whose first track has an empty
// sample table is indistinguishable from the initialisation segment of a
// fragmented one.
func (m *ProgressiveMuxer) AddTrack(cfg TrackConfig) (uint32, error) {
	switch {
	case m.closed:
		return 0, ErrClosed
	case m.started:
		return 0, fmt.Errorf("%w: tracks cannot be added once writing has begun", ErrTrackConfig)
	case cfg.Timescale == 0:
		return 0, fmt.Errorf("%w: %s track has no timescale", ErrTrackConfig, cfg.Codec)
	}
	lang := cfg.Language
	if lang == "" {
		lang = "und"
	}
	id := uint32(len(m.tracks)) + 1
	trak := mp4.CreateEmptyTrak(id, cfg.Timescale, handlerFor(cfg.Kind), lang)
	// The sample entry a progressive file needs is the one a fragmented file
	// needs, so it is written by the same code.
	if err := describe(trak, cfg); err != nil {
		return 0, err
	}
	t := &progressiveTrack{id: id, trak: trak, timescale: cfg.Timescale}
	m.tracks = append(m.tracks, t)
	m.byID[id] = t
	return id, nil
}

// WriteSample appends one frame to a track. Frames are gathered into chunks and
// the chunks of every track are written in decode order, so a player reading
// the file front to back meets each track's media where it needs it.
//
// The sample's data is not copied, so a caller writing out of a buffer it means
// to reuse has to hand over a slice of its own.
func (m *ProgressiveMuxer) WriteSample(trackID uint32, s Sample) error {
	switch {
	case m.closed:
		return ErrClosed
	case len(m.tracks) == 0:
		return ErrNoTracks
	case len(s.Data) == 0:
		return fmt.Errorf("%w: no data", ErrSample)
	case s.Duration == 0:
		return fmt.Errorf("%w: no duration", ErrSample)
	}
	t, ok := m.byID[trackID]
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownTrack, trackID)
	}
	if err := m.begin(); err != nil {
		return err
	}
	if err := m.reserve(len(s.Data)); err != nil {
		return err
	}
	if len(t.pending) == 0 {
		t.pendingStart = t.total
	}
	t.pending = append(t.pending, s.Data)
	m.pendingBytes += uint64(len(s.Data))
	t.pendingDur += uint64(s.Duration)
	t.count++
	t.total += uint64(s.Duration)
	t.sizes = append(t.sizes, uint32(len(s.Data)))
	t.addDuration(s.Duration)
	t.addComposition(s.CompositionOffset)
	if s.Sync {
		t.syncs = append(t.syncs, t.count)
	}
	if t.pendingDur >= m.chunkLimit(t) {
		return m.flush()
	}
	return nil
}

// held is how much media sits in memory: the tracks' open chunks, and the mdat
// payload of a writer that cannot be sent it yet.
func (m *ProgressiveMuxer) held() uint64 {
	return m.pendingBytes + uint64(len(m.buffered))
}

// reserve makes room for one sample's bytes. A muxer already holding as much as
// it is allowed to closes its open chunks, which for a writer that can seek
// hands them over and frees the room; one that cannot seek keeps the payload
// until Close, so past its limit there is nothing left to free and it says so.
func (m *ProgressiveMuxer) reserve(n int) error {
	if m.held()+uint64(n) <= m.settings.memoryLimit {
		return nil
	}
	if err := m.flush(); err != nil {
		return err
	}
	if held := m.held() + uint64(n); held > m.settings.memoryLimit {
		return fmt.Errorf("%w: %d bytes of media held, limit %d",
			ErrMediaTooLarge, held, m.settings.memoryLimit)
	}
	return nil
}

// chunkLimit is how much of this track's media closes a chunk, in the track's
// own timescale. A timescale so coarse that the chunk duration rounds to
// nothing still closes a chunk every sample rather than never.
func (m *ProgressiveMuxer) chunkLimit(t *progressiveTrack) uint64 {
	limit := uint64(m.settings.chunkDuration.Seconds() * float64(t.timescale))
	if limit == 0 {
		limit = 1
	}
	return limit
}

// begin writes what comes before the media: the file type, and — for a writer
// that can be seeked back to it — the mdat header whose size is still unknown.
func (m *ProgressiveMuxer) begin() error {
	if m.started {
		return nil
	}
	if m.seeker != nil {
		// stco and co64 state offsets from the start of the file, so a muxer
		// writing into one that already holds something has to know where it
		// was put.
		pos, err := m.seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return fmt.Errorf("container: find the position to write at: %w", err)
		}
		if pos < 0 {
			return fmt.Errorf("container: writer reports position %d", pos)
		}
		m.payloadBase = uint64(pos)
	}
	ftyp := mp4.NewFtyp(m.settings.brand, 0x200, progressiveBrands(m.settings.brand))
	if err := ftyp.Encode(m.w); err != nil {
		return fmt.Errorf("container: write ftyp: %w", err)
	}
	m.payloadBase += ftyp.Size()
	if m.seeker != nil {
		if err := mp4.EncodeHeaderWithSize("mdat", mdatLargeHeaderLen, true, m.w); err != nil {
			return fmt.Errorf("container: write mdat header: %w", err)
		}
		m.mdatSizeAt = m.payloadBase + mdatHeaderLen
		m.payloadBase += mdatLargeHeaderLen
	}
	m.started = true
	return nil
}

// progressiveBrands is the compatible brand list of a progressive file: the
// three brands any such file satisfies, and the major brand when it is none of
// them.
func progressiveBrands(major string) []string {
	brands := []string{"isom", "iso2", "mp41"}
	if slices.Contains(brands, major) {
		return brands
	}
	return append(brands, major)
}

// flush turns the samples every track holds into one chunk each, written in
// ascending order of the decode time they start at. Interleaving is what makes
// a progressive file readable in one pass: a player that met a whole track
// before the next would have to seek back for the sound.
func (m *ProgressiveMuxer) flush() error {
	waiting := make([]*progressiveTrack, 0, len(m.tracks))
	for _, t := range m.tracks {
		if len(t.pending) > 0 {
			waiting = append(waiting, t)
		}
	}
	// Timescales differ between tracks, so the comparison is in seconds. Ties
	// keep the order the tracks were declared in.
	slices.SortStableFunc(waiting, func(a, b *progressiveTrack) int {
		return cmp.Compare(a.pendingSeconds(), b.pendingSeconds())
	})
	for _, t := range waiting {
		t.chunkOffsets = append(t.chunkOffsets, m.written)
		t.chunkSamples = append(t.chunkSamples, uint32(len(t.pending)))
		for _, data := range t.pending {
			m.pendingBytes -= uint64(len(data))
			if err := m.writeMedia(data); err != nil {
				return err
			}
		}
		t.pending, t.pendingDur = nil, 0
	}
	return nil
}

// pendingSeconds is when this track's held samples start, in seconds.
func (t *progressiveTrack) pendingSeconds() float64 {
	return float64(t.pendingStart) / float64(t.timescale)
}

// writeMedia puts one sample's bytes in the mdat payload, either out to the
// writer or into the buffer that stands in for one that cannot seek.
func (m *ProgressiveMuxer) writeMedia(data []byte) error {
	if m.seeker == nil {
		m.buffered = append(m.buffered, data...)
		m.written += uint64(len(data))
		return nil
	}
	if _, err := m.w.Write(data); err != nil {
		return fmt.Errorf("container: write media data: %w", err)
	}
	m.written += uint64(len(data))
	return nil
}

// Close writes what is left of the media, then the moov that addresses it, and
// refuses any further use.
func (m *ProgressiveMuxer) Close() error {
	if m.closed {
		return ErrClosed
	}
	m.closed = true
	if len(m.tracks) == 0 {
		return ErrNoTracks
	}
	for _, t := range m.tracks {
		if t.count == 0 {
			return fmt.Errorf("%w: track %d was declared but never written", ErrNoSamples, t.id)
		}
	}
	if err := m.flush(); err != nil {
		return err
	}
	if err := m.finishMdat(); err != nil {
		return err
	}
	moov, err := m.movie()
	if err != nil {
		return err
	}
	if err := moov.Encode(m.w); err != nil {
		return fmt.Errorf("container: write moov: %w", err)
	}
	return nil
}

// finishMdat closes the media data box: either by writing the header and the
// payload that was held for it, or by going back to the header that was written
// before its size was known.
func (m *ProgressiveMuxer) finishMdat() error {
	if m.seeker == nil {
		if err := mp4.EncodeHeaderWithSize("mdat", mdatHeaderLen+m.written, false, m.w); err != nil {
			return fmt.Errorf("container: write mdat header: %w", err)
		}
		if _, err := m.w.Write(m.buffered); err != nil {
			return fmt.Errorf("container: write media data: %w", err)
		}
		m.payloadBase += mdatHeaderLen
		m.buffered = nil
		return nil
	}
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], mdatLargeHeaderLen+m.written)
	if _, err := m.seeker.Seek(int64(m.mdatSizeAt), io.SeekStart); err != nil {
		return fmt.Errorf("container: seek back to the mdat size: %w", err)
	}
	if _, err := m.w.Write(size[:]); err != nil {
		return fmt.Errorf("container: write the mdat size: %w", err)
	}
	// moov follows the media, not the header just patched.
	if _, err := m.seeker.Seek(int64(m.payloadBase+m.written), io.SeekStart); err != nil {
		return fmt.Errorf("container: seek past the media data: %w", err)
	}
	return nil
}

// movie builds the moov box: the movie header, then one track box per track,
// each carrying the sample tables. There is no mvex: that box is what tells a
// player fragments follow.
func (m *ProgressiveMuxer) movie() (*mp4.MoovBox, error) {
	wide := m.needsWideOffsets()
	moov := mp4.NewMoovBox()
	mvhd := mp4.CreateMvhd()
	mvhd.Timescale = progressiveMovieTimescale
	mvhd.NextTrackID = uint32(len(m.tracks)) + 1
	moov.AddChild(mvhd)
	var longest uint64
	for _, t := range m.tracks {
		if err := m.sampleTables(t, wide); err != nil {
			return nil, err
		}
		scaled := scaleDuration(t.total, t.timescale, progressiveMovieTimescale)
		if scaled > longest {
			longest = scaled
		}
		setDuration(&t.trak.Tkhd.Version, &t.trak.Tkhd.Duration, scaled)
		setDuration(&t.trak.Mdia.Mdhd.Version, &t.trak.Mdia.Mdhd.Duration, t.total)
		moov.AddChild(t.trak)
	}
	setDuration(&mvhd.Version, &mvhd.Duration, longest)
	return moov, nil
}

// needsWideOffsets reports whether the file's chunks reach past what a stco
// entry can state. The decision is taken once for the whole file: a player
// meeting two tracks addressed two different ways gains nothing.
func (m *ProgressiveMuxer) needsWideOffsets() bool {
	var furthest uint64
	for _, t := range m.tracks {
		if n := len(t.chunkOffsets); n > 0 && t.chunkOffsets[n-1] > furthest {
			furthest = t.chunkOffsets[n-1]
		}
	}
	return m.payloadBase+furthest > maxChunkOffset32
}

// sampleTables fills the track's sample table box with what was gathered while
// the samples were written. The boxes are rebuilt rather than appended to,
// because the box the empty track came with holds them in a different order
// than the one ISO/IEC 14496-12 lists them in.
func (m *ProgressiveMuxer) sampleTables(t *progressiveTrack, wide bool) error {
	stbl := t.trak.Mdia.Minf.Stbl
	stsd := stbl.Stsd
	*stbl = mp4.StblBox{}
	stbl.AddChild(stsd)
	stbl.AddChild(&mp4.SttsBox{SampleCount: t.durCounts, SampleTimeDelta: t.durDeltas})
	if ctts := t.compositionTable(); ctts != nil {
		stbl.AddChild(ctts)
	}
	stsc, err := t.chunkTable()
	if err != nil {
		return err
	}
	stbl.AddChild(stsc)
	stbl.AddChild(&mp4.StszBox{SampleNumber: t.count, SampleSize: t.sizes})
	// A track with no sync sample table is one every sample can be started
	// at, which is what an audio track usually is; saying so costs nothing
	// and is what a reader assumes.
	if uint32(len(t.syncs)) != t.count {
		stbl.AddChild(&mp4.StssBox{SampleNumber: t.syncs})
	}
	stbl.AddChild(offsetTable(t.chunkOffsets, m.payloadBase, wide))
	return nil
}

// addChunkEntry exists so the failure it guards can be tested: the chunk table
// is built from chunk one upwards, which is the only thing the reference
// library refuses. Building the box by hand instead is not an option — it holds
// the sample description identifier in an unexported field, and encoding a box
// that never went through this call panics.
var addChunkEntry = (*mp4.StscBox).AddEntry

// chunkTable is the sample-to-chunk table, one entry per run of chunks holding
// the same number of samples rather than one entry per chunk.
func (t *progressiveTrack) chunkTable() (*mp4.StscBox, error) {
	stsc := &mp4.StscBox{}
	var samplesPerChunk uint32
	for i, n := range t.chunkSamples {
		if n == samplesPerChunk {
			continue
		}
		if err := addChunkEntry(stsc, uint32(i)+1, n, 1); err != nil {
			return nil, fmt.Errorf("container: chunk table of track %d: %w", t.id, err)
		}
		samplesPerChunk = n
	}
	return stsc, nil
}

// addDuration records one sample's duration, extending the run it belongs to
// rather than starting a new entry. A track of constant frame rate ends with a
// single stts entry however many samples it has.
func (t *progressiveTrack) addDuration(d uint32) {
	if n := len(t.durDeltas); n > 0 && t.durDeltas[n-1] == d {
		t.durCounts[n-1]++
		return
	}
	t.durDeltas = append(t.durDeltas, d)
	t.durCounts = append(t.durCounts, 1)
}

// addComposition records one sample's composition offset the same way.
func (t *progressiveTrack) addComposition(offset int32) {
	if offset != 0 {
		t.shifted = true
	}
	if n := len(t.cttsOffsets); n > 0 && t.cttsOffsets[n-1] == offset {
		t.cttsCounts[n-1]++
		return
	}
	t.cttsOffsets = append(t.cttsOffsets, offset)
	t.cttsCounts = append(t.cttsCounts, 1)
}

// compositionTable is the composition offset table, or nil for a track whose
// samples present in the order they decode and so need none.
func (t *progressiveTrack) compositionTable() *mp4.CttsBox {
	if !t.shifted {
		return nil
	}
	ctts := &mp4.CttsBox{
		EndSampleNr:  make([]uint32, 1, len(t.cttsCounts)+1),
		SampleOffset: t.cttsOffsets,
	}
	end := uint32(0)
	for _, n := range t.cttsCounts {
		end += n
		ctts.EndSampleNr = append(ctts.EndSampleNr, end)
	}
	// Version 0 states the offsets unsigned, which a frame presenting before
	// it decodes cannot be.
	if slices.ContainsFunc(t.cttsOffsets, func(o int32) bool { return o < 0 }) {
		ctts.Version = 1
	}
	return ctts
}

// offsetTable is the chunk offset table, in whichever of its two forms the
// file's own size calls for.
func offsetTable(within []uint64, base uint64, wide bool) mp4.Box {
	if wide {
		co64 := &mp4.Co64Box{ChunkOffset: make([]uint64, len(within))}
		for i, offset := range within {
			co64.ChunkOffset[i] = base + offset
		}
		return co64
	}
	stco := &mp4.StcoBox{ChunkOffset: make([]uint32, len(within))}
	for i, offset := range within {
		stco.ChunkOffset[i] = uint32(base + offset)
	}
	return stco
}

// scaleDuration converts a duration between two timescales. The remainder is
// scaled apart from the whole so that a long track does not overflow on its
// way through the multiplication.
func scaleDuration(d uint64, from, to uint32) uint64 {
	whole := d / uint64(from) * uint64(to)
	rest := d % uint64(from) * uint64(to) / uint64(from)
	return whole + rest
}

// setDuration states a duration in one of the header boxes, widening the box to
// its 64-bit form when the value no longer fits the 32-bit one. A file long
// enough for that written as version 0 would announce a duration that has
// wrapped.
func setDuration(version *byte, field *uint64, d uint64) {
	if d > math.MaxUint32 {
		*version = 1
	}
	*field = d
}
