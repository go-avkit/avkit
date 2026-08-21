// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/mp4"
)

// Errors reported when reading samples.
var (
	// ErrNoSamples means the container holds no readable sample for the
	// track, either because its tables are absent or because it is empty.
	ErrNoSamples = errors.New("container: no readable sample")
	// ErrSampleData means a sample table points outside the file.
	ErrSampleData = errors.New("container: sample data out of range")
	// ErrUnsupportedFormat means samples cannot be read from this format
	// yet.
	ErrUnsupportedFormat = errors.New("container: reading samples from this format is not supported")
)

// Reader reads the samples of an MP4, progressive or fragmented, alongside the
// metadata Demux already reports.
//
// It is the counterpart of Muxer: TrackConfig hands back exactly what AddTrack
// needs, and Samples hands back exactly what WriteSample takes, so a track can
// be copied from one file into another without re-encoding and without the
// caller knowing what a sample table is.
type Reader struct {
	data  []byte
	file  *File
	mp4   *mp4.File
	traks map[uint32]*mp4.TrakBox
	// ts holds the tracks of a transport stream, whose samples are read in
	// one pass rather than addressed through tables.
	ts map[uint32]*tsTrack
}

// newTSReader reads a transport stream in one pass: it has no table to seek
// with, so its tracks are converted as they are met.
func newTSReader(data []byte) (*Reader, error) {
	tracks, err := readTS(data)
	if err != nil {
		return nil, err
	}
	r := &Reader{data: data, ts: map[uint32]*tsTrack{}, traks: map[uint32]*mp4.TrakBox{}}
	file := &File{Format: "mpegts", Timescale: TSTimescale}
	for _, t := range tracks {
		file.Tracks = append(file.Tracks, t.track)
		if t.track.Duration > file.Duration {
			file.Duration = t.track.Duration
		}
		r.ts[t.track.ID] = t
	}
	r.file = file
	return r, nil
}

// NewReader reads the structure of an MP4 and prepares its sample tables.
func NewReader(data []byte) (*Reader, error) {
	switch format := Sniff(data); format {
	case FormatMP4:
	case FormatMPEGTS:
		return newTSReader(data)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFormat, describeFormat(format))
	}
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("container: decode mp4: %w", err)
	}
	file, err := mp4File(parsed)
	if err != nil {
		return nil, err
	}
	r := &Reader{data: data, file: file, mp4: parsed, traks: map[uint32]*mp4.TrakBox{}}
	if moov := movieBox(parsed); moov != nil {
		for _, trak := range moov.Traks {
			r.traks[trak.Tkhd.TrackID] = trak
		}
	}
	return r, nil
}

// describeFormat names a format for an error message.
func describeFormat(format Format) string {
	switch format {
	case FormatMatroska:
		return "matroska"
	case FormatMP4:
		return "mp4"
	case FormatMPEGTS:
		return "mpegts"
	default:
		return "unknown format"
	}
}

// movieBox is the moov of a file, wherever it sits: a fragmented file keeps it
// in its initialisation segment.
func movieBox(f *mp4.File) *mp4.MoovBox {
	if f.Moov != nil {
		return f.Moov
	}
	if f.Init != nil {
		return f.Init.Moov
	}
	return nil
}

// File returns the container's metadata, as Demux reports it.
func (r *Reader) File() *File { return r.file }

// TrackIDs lists the tracks, in file order.
func (r *Reader) TrackIDs() []uint32 {
	out := make([]uint32, 0, len(r.file.Tracks))
	for _, t := range r.file.Tracks {
		out = append(out, t.ID)
	}
	return out
}

// TrackConfig describes a track the way Muxer.AddTrack wants it, so a track
// can be copied without the caller reading a single box.
func (r *Reader) TrackConfig(trackID uint32) (TrackConfig, error) {
	if r.ts != nil {
		t, ok := r.ts[trackID]
		if !ok {
			return TrackConfig{}, fmt.Errorf("%w: %d", ErrUnknownTrack, trackID)
		}
		return t.config, nil
	}
	trak, ok := r.traks[trackID]
	if !ok {
		return TrackConfig{}, fmt.Errorf("%w: %d", ErrUnknownTrack, trackID)
	}
	var track Track
	for _, t := range r.file.Tracks {
		if t.ID == trackID {
			track = t
		}
	}
	cfg := TrackConfig{
		Kind:       track.Kind,
		Codec:      track.Codec,
		Timescale:  track.Timescale,
		Width:      track.Width,
		Height:     track.Height,
		Channels:   track.Channels,
		SampleRate: track.SampleRate,
		Language:   track.Language,
	}
	stsd := trak.Mdia.Minf.Stbl.Stsd
	switch {
	case stsd.AvcX != nil && stsd.AvcX.AvcC != nil:
		cfg.SPS, cfg.PPS = stsd.AvcX.AvcC.SPSnalus, stsd.AvcX.AvcC.PPSnalus
	case stsd.HvcX != nil && stsd.HvcX.HvcC != nil:
		cfg.VPS, cfg.SPS, cfg.PPS = hevcParameterSets(stsd.HvcX.HvcC)
	case stsd.Av01 != nil && stsd.Av01.Av1C != nil:
		// A record that cannot be read leaves the field empty: the rest of
		// the configuration is still worth having.
		if payload, err := boxPayload(stsd.Av01.Av1C); err == nil {
			cfg.CodecConfig = payload
		}
	case stsd.Mp4a != nil && stsd.Mp4a.Esds != nil:
		cfg.AudioObjectType = audioObjectType(stsd.Mp4a.Esds)
	}
	return cfg, nil
}

// audioObjectType reads the AAC profile out of the audio specific config the
// esds descriptor carries. A config that cannot be read leaves the profile at
// zero, which the muxer reads as AAC-LC.
func audioObjectType(esds *mp4.EsdsBox) byte {
	dec := esds.DecConfigDescriptor
	if dec == nil || dec.DecSpecificInfo == nil || len(dec.DecSpecificInfo.DecConfig) == 0 {
		return 0
	}
	asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(dec.DecSpecificInfo.DecConfig))
	if err != nil {
		return 0
	}
	return asc.ObjectType
}

// hevcParameterSets pulls the three parameter set kinds out of an hvcC record.
func hevcParameterSets(hvcC *mp4.HvcCBox) (vps, sps, pps [][]byte) {
	for _, array := range hvcC.NaluArrays {
		nalus := make([][]byte, 0, len(array.Nalus))
		nalus = append(nalus, array.Nalus...)
		switch array.NaluType() {
		case 32:
			vps = nalus
		case 33:
			sps = nalus
		case 34:
			pps = nalus
		}
	}
	return vps, sps, pps
}

// boxPayload re-encodes a box and returns its content, without the header. It
// is how a configuration record travels as the bytes a muxer expects.
func boxPayload(box mp4.Box) ([]byte, error) {
	var buf bytes.Buffer
	if err := box.Encode(&buf); err != nil {
		return nil, fmt.Errorf("container: encode %s: %w", box.Type(), err)
	}
	if buf.Len() <= 8 {
		return nil, fmt.Errorf("%w: %s box is empty", ErrNoSamples, box.Type())
	}
	return buf.Bytes()[8:], nil
}

// Samples reads every sample of a track, in decoding order.
func (r *Reader) Samples(trackID uint32) ([]Sample, error) {
	if r.ts != nil {
		t, ok := r.ts[trackID]
		if !ok {
			return nil, fmt.Errorf("%w: %d", ErrUnknownTrack, trackID)
		}
		if len(t.samples) == 0 {
			return nil, fmt.Errorf("%w: track %d", ErrNoSamples, trackID)
		}
		return t.samples, nil
	}
	trak, ok := r.traks[trackID]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownTrack, trackID)
	}
	if r.mp4.IsFragmented() {
		return r.fragmentedSamples(trackID)
	}
	return r.progressiveSamples(trak)
}

// fragmentedSamples walks the fragments of every segment.
func (r *Reader) fragmentedSamples(trackID uint32) ([]Sample, error) {
	// The track extends box only supplies defaults for what a fragment does
	// not state; a file that declares none still reads.
	trex := r.trackExtends(trackID)
	var out []Sample
	for _, seg := range r.mp4.Segments {
		for _, frag := range seg.Fragments {
			if frag.Moof == nil {
				continue
			}
			// A fragment may carry several tracks, one track fragment each,
			// and only the track asked about is wanted.
			for _, traf := range frag.Moof.Trafs {
				if traf.Tfhd == nil || traf.Tfhd.TrackID != trackID {
					continue
				}
				samples, err := r.trafSamples(frag.Moof.StartPos, traf, trex)
				if err != nil {
					return nil, err
				}
				out = append(out, samples...)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: track %d", ErrNoSamples, trackID)
	}
	return out, nil
}

// trafSamples reads the samples of one track fragment. The run states where
// its data sits relative to the fragment header, and what the run leaves out
// the track defaults supply.
func (r *Reader) trafSamples(moofStart uint64, traf *mp4.TrafBox, trex *mp4.TrexBox) ([]Sample, error) {
	trun := traf.Trun
	if trun == nil {
		return nil, fmt.Errorf("%w: track fragment %d has no sample run",
			ErrNoSamples, traf.Tfhd.TrackID)
	}
	trun.AddSampleDefaultValues(traf.Tfhd, trex)
	offset := moofStart
	if trun.HasDataOffset() {
		offset = uint64(int64(moofStart) + int64(trun.DataOffset))
	}
	out := make([]Sample, 0, len(trun.Samples))
	for i, s := range trun.Samples {
		end := offset + uint64(s.Size)
		if end > uint64(len(r.data)) {
			return nil, fmt.Errorf("%w: sample %d of track %d ends at %d of %d bytes",
				ErrSampleData, i+1, traf.Tfhd.TrackID, end, len(r.data))
		}
		out = append(out, Sample{
			Data:              r.data[offset:end],
			Duration:          s.Dur,
			CompositionOffset: s.CompositionTimeOffset,
			Sync:              s.IsSync(),
		})
		offset = end
	}
	return out, nil
}

// trackExtends is the defaults box of a track, or nil when the file declares
// none for it.
func (r *Reader) trackExtends(trackID uint32) *mp4.TrexBox {
	moov := movieBox(r.mp4)
	if moov == nil || moov.Mvex == nil {
		return nil
	}
	for _, trex := range moov.Mvex.Trexs {
		if trex.TrackID == trackID {
			return trex
		}
	}
	return nil
}

// progressiveSamples walks the sample tables of a plain MP4: the chunks say
// where the data is, the other tables say how long each sample lasts and
// whether a player may start at it.
func (r *Reader) progressiveSamples(trak *mp4.TrakBox) ([]Sample, error) {
	stbl := trak.Mdia.Minf.Stbl
	if stbl == nil || stbl.Stsz == nil || stbl.Stts == nil || stbl.Stsc == nil {
		return nil, fmt.Errorf("%w: track %d has no sample table", ErrNoSamples, trak.Tkhd.TrackID)
	}
	count := stbl.Stsz.GetNrSamples()
	if count == 0 {
		return nil, fmt.Errorf("%w: track %d is empty", ErrNoSamples, trak.Tkhd.TrackID)
	}
	// A chunk table with no entry cannot say where sample one lives, and
	// asking anyway panics inside the box reader. A file off the network is
	// not to be trusted with that.
	if len(stbl.Stsc.Entries) == 0 {
		return nil, fmt.Errorf("%w: track %d has an empty chunk table",
			ErrNoSamples, trak.Tkhd.TrackID)
	}
	chunks, err := containingChunks(stbl.Stsc, 1, count)
	if err != nil {
		return nil, fmt.Errorf("container: chunk table: %w", err)
	}
	out := make([]Sample, 0, count)
	sampleNr := uint32(1)
	for _, chunk := range chunks {
		offset, err := chunkOffset(stbl, chunk.ChunkNr)
		if err != nil {
			return nil, err
		}
		for i := uint32(0); i < chunk.NrSamples && sampleNr <= count; i++ {
			size := stbl.Stsz.GetSampleSize(int(sampleNr))
			end := offset + uint64(size)
			if end > uint64(len(r.data)) {
				return nil, fmt.Errorf("%w: sample %d ends at %d of %d bytes",
					ErrSampleData, sampleNr, end, len(r.data))
			}
			_, dur := stbl.Stts.GetDecodeTime(sampleNr)
			s := Sample{
				Data:     r.data[offset:end],
				Duration: dur,
				Sync:     true,
			}
			if stbl.Ctts != nil {
				s.CompositionOffset = stbl.Ctts.GetCompositionTimeOffset(sampleNr)
			}
			if stbl.Stss != nil {
				s.Sync = stbl.Stss.IsSyncSample(sampleNr)
			}
			out = append(out, s)
			offset = end
			sampleNr++
		}
	}
	return out, nil
}

// containingChunks exists so the failure it guards can be tested: the tables
// this package accepts are checked before it is called.
var containingChunks = func(stsc *mp4.StscBox, first, last uint32) ([]mp4.Chunk, error) {
	return stsc.GetContainingChunks(first, last)
}

// chunkOffset is where a chunk starts in the file, from whichever of the two
// offset tables the file carries.
func chunkOffset(stbl *mp4.StblBox, chunkNr uint32) (uint64, error) {
	switch {
	case stbl.Stco != nil:
		offset, err := stbl.Stco.GetOffset(int(chunkNr))
		if err != nil {
			return 0, fmt.Errorf("container: chunk offset: %w", err)
		}
		return offset, nil
	case stbl.Co64 != nil:
		offset, err := stbl.Co64.GetOffset(int(chunkNr))
		if err != nil {
			return 0, fmt.Errorf("container: chunk offset: %w", err)
		}
		return offset, nil
	}
	return 0, fmt.Errorf("%w: no chunk offset table", ErrNoSamples)
}
