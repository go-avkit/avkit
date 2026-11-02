// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// The Ogg writer: RFC 3533 pages, one logical stream, in the mappings of Opus
// (RFC 7845), Vorbis (Vorbis I §A) and FLAC (RFC 9639 §10.1).
//
// Pages follow FFmpeg's oggenc.c: the first header packet alone on the
// beginning-of-stream page, the other header packets together on the next one
// (Vorbis I §A.2 and RFC 7845 §3 both want the audio to begin on a fresh page),
// then audio pages that close after a second of audio or when their 255
// segments are full, with a packet that does not fit the page left for the next
// one rather than continued unless it is longer than a page. The last page
// carries the end-of-stream flag, which is why a finished page is held back
// until the next one exists.
//
// The granule position is each mapping's own (oggenc.c ogg_write_packet_internal
// computes pts + duration, and adds the pre-skip for Opus): the count of samples
// up to the end of the last packet to finish on the page, at 48 kHz and
// pre-skip included for Opus (RFC 7845 §4), at the sampling rate for Vorbis,
// where the first audio packet returns nothing (Vorbis I §A.2), and for FLAC.
// A page on which no packet ends states -1 (RFC 3533 §6).
//
// ⛔ The end of the stream is trimmed as the reader trims it. An encoder pads
// its last frame, and the granule of the last page says where the real audio
// ends (RFC 7845 §4.4, Vorbis I §A.2): when the last sample states a duration
// shorter than the packet holds, or the track's edit list ends the presentation
// before the packets do, the last page says so. A duration in a coarse timescale
// (Matroska counts in milliseconds) is rounded to its tick, so it only counts
// when it falls short by more than a tick, and the end it states is as coarse:
// an Opus track out of Matroska ends within a millisecond of where FFmpeg's
// DiscardPadding-exact end is, which the Matroska reader does not keep.

const (
	// oggMaxBody is the most a page of 255 full segments can hold.
	oggMaxBody = 255 * 255
	// oggVendor is the vendor string of the comment headers written here.
	oggVendor = "go-avkit"
)

// oggOutPage is a page waiting for its sequence number and checksum.
type oggOutPage struct {
	flags    byte
	granule  int64
	segments []byte
	body     []byte
}

type oggSink struct {
	out    *stickyWriter
	serial uint32
	seq    uint32

	// The open page.
	segments  []byte
	body      []byte
	flags     byte
	granule   int64 // of the last packet to end on it, or oggNoGranule
	pageStart int64 // the position it began at

	pending    oggOutPage
	hasPending bool

	// position is the granule position after the last packet; second is how
	// many of its units make a second; timescale is that of Sample.Duration.
	position, second int64
	timescale        uint32
	// step reads a packet: how far it moves the granule position, and how many
	// samples it holds when that is more.
	step func(data []byte) (step, duration int64, err error)
	trim int64
	// end is where the presentation stops, in granule units, when the track's
	// edit list says so (a Matroska DiscardPadding, an MP3's end padding), or
	// oggNoGranule.
	end int64
}

// oggPresentedEnd is where a track's single edit stops presenting, in units of
// 1/perSecond: the end of the audio that is not padding. Edit lists that say
// more than "from here to there" cannot be stated by a granule position and are
// left to the packets.
func oggPresentedEnd(cfg TrackConfig, perSecond int64) int64 {
	el := cfg.EditList
	if el == nil || len(el.Edits) != 1 || cfg.Timescale == 0 || el.MovieTimescale == 0 {
		return oggNoGranule
	}
	e := el.Edits[0]
	if e.Rate != NormalRate || e.MediaTime < 0 || e.SegmentDuration == 0 {
		return oggNoGranule
	}
	ticks := rescale(e.SegmentDuration, el.MovieTimescale, cfg.Timescale) + uint64(e.MediaTime)
	return int64(rescale(ticks, cfg.Timescale, uint32(perSecond)))
}

// newOggSink checks the track, builds the header packets its mapping needs and
// writes them.
func newOggSink(a *AudioWriter, cfg TrackConfig) (audioSink, error) {
	o := &oggSink{out: a.out, granule: oggNoGranule, timescale: cfg.Timescale}
	var (
		headers [][]byte
		err     error
	)
	switch audioCodec(cfg) {
	case "opus":
		headers, err = o.opus(cfg)
	case "vorb":
		headers, err = o.vorbis(cfg)
	case "flac":
		headers, err = o.flac(cfg)
	default:
		return nil, refuseAudioCodec(FormatOgg, cfg, "Opus, Vorbis and FLAC")
	}
	if err != nil {
		return nil, err
	}
	o.end = oggPresentedEnd(cfg, o.second)
	o.serial = crc32.ChecksumIEEE(headers[0])
	if a.set.serialSet {
		o.serial = a.set.serial
	}
	o.packet(headers[0], 0)
	o.flags = oggBOS
	o.cut()
	for _, h := range headers[1:] {
		o.packet(h, 0)
	}
	o.cut()
	return o, nil
}

// opus builds the two header packets of RFC 7845 §5: the identification header,
// which is the track's OpusHead (its pre-skip included) and the comment header.
func (o *oggSink) opus(cfg TrackConfig) ([][]byte, error) {
	head, err := OpusHead(cfg)
	if err != nil {
		return nil, err
	}
	tags := append([]byte("OpusTags"), writerComment()...)
	// RFC 7845 §4: the granule position counts every sample decoded, the
	// pre-skip's included; a player takes the pre-skip off to find the time.
	o.second = opusOutputRate
	o.step = func(data []byte) (int64, int64, error) {
		d, err := opusPacketDuration(data)
		return d, d, err
	}
	return [][]byte{head, tags}, nil
}

// writerComment is a Vorbis comment (Vorbis I §5.2.1) with this writer's vendor
// string and no user comment.
func writerComment() []byte {
	c := binary.LittleEndian.AppendUint32(nil, uint32(len(oggVendor)))
	c = append(c, oggVendor...)
	return binary.LittleEndian.AppendUint32(c, 0)
}

// vorbis takes the three header packets from the track's Xiph-laced
// configuration, the form Matroska carries them in, and sets up the
// block-size bookkeeping the granule position needs.
func (o *oggSink) vorbis(cfg TrackConfig) ([][]byte, error) {
	headers, err := vorbisHeaderPackets(cfg.CodecConfig)
	if err != nil {
		return nil, err
	}
	id, err := parseVorbisID(headers[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTrackConfig, err)
	}
	long, err := vorbisModes(headers[2])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTrackConfig, err)
	}
	o.second = int64(id.rate)
	modeBits := bitsFor(len(long) - 1)
	previous, first := id.blocks[0], true
	o.step = func(data []byte) (int64, int64, error) {
		if len(data) == 0 || data[0]&1 != 0 {
			return 0, 0, fmt.Errorf("%w: not a Vorbis audio packet", ErrSample)
		}
		word := uint32(data[0])
		mode := int(word >> 1 & (1<<modeBits - 1))
		if mode >= len(long) {
			return 0, 0, fmt.Errorf("%w: a Vorbis packet names mode %d of %d", ErrSample, mode, len(long))
		}
		current := id.blocks[0]
		if long[mode] {
			current = id.blocks[1]
			previous = id.blocks[word>>(1+modeBits)&1]
		}
		d := int64(previous/4 + current/4)
		previous = current
		if first {
			// Vorbis I §A.2: the first audio packet returns no sample.
			first = false
			return 0, d, nil
		}
		return d, d, nil
	}
	return headers, nil
}

// vorbisHeaderPackets splits the Xiph-laced identification, comment and setup
// headers (Matroska RFC 9559 §10.2.1) and checks which is which (Vorbis I
// §4.2.1: the packet type, then "vorbis").
func vorbisHeaderPackets(config []byte) ([][]byte, error) {
	if len(config) == 0 {
		return nil, fmt.Errorf("%w: Vorbis needs its three headers as CodecConfig", ErrTrackConfig)
	}
	unlacer, err := unlaceVorbisHeaders(bytes.NewReader(config), int64(len(config)))
	if err != nil {
		return nil, fmt.Errorf("%w: the Vorbis headers are not Xiph-laced: %v", ErrTrackConfig, err)
	}
	var packets [][]byte
	for {
		packet, err := unlacer.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: the Vorbis headers stop short: %v", ErrTrackConfig, err)
		}
		packets = append(packets, packet)
	}
	if len(packets) != vorbisSetupPackets {
		return nil, fmt.Errorf("%w: Vorbis needs %d headers, not %d", ErrTrackConfig, vorbisSetupPackets, len(packets))
	}
	for i, kind := range []byte{1, 3, 5} {
		if len(packets[i]) < 7 || packets[i][0] != kind || string(packets[i][1:7]) != "vorbis" {
			return nil, fmt.Errorf("%w: Vorbis header %d is not a type %d header", ErrTrackConfig, i, kind)
		}
	}
	return packets, nil
}

// flac builds the two header packets of RFC 9639 §10.1: the mapping header, with
// the STREAMINFO block of the track's configuration as the source states it, and
// a comment block, which the mapping wants as the last metadata block.
func (o *oggSink) flac(cfg TrackConfig) ([][]byte, error) {
	blocks, format, err := parseFLACConfig(cfg.CodecConfig)
	if err != nil {
		return nil, err
	}
	first := []byte{0x7F, 'F', 'L', 'A', 'C', 1, 0, 0, 1}
	first = append(first, flacSignature...)
	first = append(first, flacStreamInfo, 0, 0, flacStreamInfoSize)
	first = append(first, blocks[0].BlockData...)
	comment := writerComment()
	block := []byte{0x80 | flacVorbisComment, byte(len(comment) >> 16), byte(len(comment) >> 8), byte(len(comment))}
	o.second = int64(format.SampleRate)
	o.step = func(data []byte) (int64, int64, error) {
		h, ok := parseFLACFrameHeader(data)
		if !ok {
			return 0, 0, fmt.Errorf("%w: %d bytes that do not open with a FLAC frame header (RFC 9639 §9.1)",
				ErrSample, len(data))
		}
		return int64(h.blockSize), int64(h.blockSize), nil
	}
	return [][]byte{first, append(block, comment...)}, nil
}

// flacVorbisComment is the VORBIS_COMMENT metadata block type (RFC 9639 §8.1).
const flacVorbisComment = 4

func (o *oggSink) sample(s Sample) error {
	step, duration, err := o.step(s.Data)
	if err != nil {
		return err
	}
	o.trim = 0
	if o.timescale != 0 {
		shown := int64(rescale(uint64(s.Duration), o.timescale, uint32(o.second)))
		// A duration in a coarser timescale is rounded to its tick, so only a
		// shortfall of more than a tick is the packet's end being cut.
		slack := int64(0)
		if int64(o.timescale) < o.second {
			slack = (o.second + int64(o.timescale) - 1) / int64(o.timescale)
		}
		if duration-shown > slack {
			o.trim = duration - shown
		}
	}
	o.position += step
	o.packet(s.Data, o.position)
	if o.position-o.pageStart >= o.second {
		o.cut()
	}
	return nil
}

// packet laces a packet into the open page, continuing it on the next when the
// page runs out of segments (RFC 3533 §6: 255 segments of up to 255 bytes, a
// packet ending at the first segment shorter than 255).
func (o *oggSink) packet(p []byte, granule int64) {
	if len(o.body) > 0 && len(o.body)+len(p) > oggMaxBody {
		o.cut()
	}
	for left := len(p)/255 + 1; left > 0; left-- {
		if len(o.segments) == 255 {
			o.cut()
			o.flags |= oggContinued
		}
		n := min(len(p), 255)
		o.segments = append(o.segments, byte(n))
		o.body = append(o.body, p[:n]...)
		p = p[n:]
	}
	o.granule = granule
	if len(o.segments) == 255 {
		o.cut()
	}
}

// cut closes the open page. It is not written yet: the page before it is, and
// this one waits in case it turns out to be the last.
func (o *oggSink) cut() {
	if len(o.segments) == 0 {
		return
	}
	if o.hasPending {
		o.emit(o.pending, 0)
	}
	o.pending, o.hasPending = oggOutPage{o.flags, o.granule, o.segments, o.body}, true
	o.segments, o.body, o.flags, o.granule = nil, nil, 0, oggNoGranule
	o.pageStart = o.position
}

// emit writes a page: the header of RFC 3533 §6, its lacing values, the data,
// and the checksum over all of it.
func (o *oggSink) emit(p oggOutPage, extra byte) {
	page := make([]byte, oggHeaderSize, oggHeaderSize+len(p.segments)+len(p.body))
	copy(page, "OggS")
	page[5] = p.flags | extra
	binary.LittleEndian.PutUint64(page[6:], uint64(p.granule))
	binary.LittleEndian.PutUint32(page[14:], o.serial)
	binary.LittleEndian.PutUint32(page[18:], o.seq)
	page[26] = byte(len(p.segments))
	page = append(page, p.segments...)
	page = append(page, p.body...)
	binary.LittleEndian.PutUint32(page[22:], oggCRC(page))
	o.seq++
	o.out.Write(page)
}

// finish writes the last page with the end-of-stream flag and the granule
// position where the audio ends.
func (o *oggSink) finish() {
	o.cut()
	last := o.pending
	if last.granule != oggNoGranule {
		last.granule -= o.trim
		if o.end != oggNoGranule && o.end < last.granule {
			last.granule = o.end
		}
	}
	o.emit(last, oggEOS)
}
