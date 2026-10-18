// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Ogg (RFC 3533) carries each logical stream as packets cut into pages, and
// says how long they last only once per page: the granule position, whose
// meaning each codec's mapping defines. A packet's own duration has to be read
// out of the packet:
//
//   - Opus (RFC 7845): the TOC byte of RFC 6716 §3.1 states the frame size and
//     the frame count. The granule counts 48 kHz samples, pre-skip included.
//   - Vorbis (Vorbis I specification §A.2 and §4.3): a packet's mode states its
//     block size, and what it returns is a quarter of the previous block plus
//     a quarter of its own. Which mode is a long block is stated at the very
//     end of the setup header, behind codebooks this package does not decode;
//     it is found by reading the setup header backwards, the way FFmpeg's
//     libavcodec/vorbis_parser.c and liboggz do it, and checked here against
//     the granule positions, which are normative: a mode table read wrongly
//     shows up as durations that do not add up.
//   - FLAC (the Ogg mapping in RFC 9639 §10.1): one frame per packet, whose
//     header states its block size.
//
// Pages are read and their CRCs checked here: no maintained pure-Go Ogg reader
// reassembles packets across pages for more than one codec (pion's oggreader
// returns pages, for Opus only, inside the whole WebRTC module). The mappings
// follow FFmpeg's libavformat/oggdec.c, oggparseopus.c, oggparsevorbis.c and
// oggparseflac.c (2026-10-18).
//
// ⛔ The end of a stream is trimmed the way FFmpeg trims it: the last page's
// granule says how many samples the stream really holds, and the last packet's
// duration is cut to fit. What it does not say is which samples to drop, so a
// container written from this plays the full last packet and stops the track at
// the right time.

// FormatOgg is an Ogg file: one or more logical streams, chained or multiplexed.
const FormatOgg Format = 8

func init() {
	registerElementary(elementaryFormat{
		format: FormatOgg, name: "ogg", order: orderMagic,
		sniff: sniffOgg, read: readOgg,
	})
}

// oggHeaderSize is the fixed part of a page header, before its lacing values.
const oggHeaderSize = 27

// Page header_type flags (RFC 3533 §6).
const (
	oggContinued = 1
	oggBOS       = 2
	oggEOS       = 4
)

// oggNoGranule is the granule position of a page on which no packet ends.
const oggNoGranule = -1

// sniffOgg recognises a page header: the capture pattern and version 0.
func sniffOgg(data []byte) bool {
	return len(data) >= 5 && string(data[:4]) == "OggS" && data[4] == 0
}

// oggPacket is one packet, with the granule position of the page it ends on
// when it is the last packet to end there, and oggNoGranule otherwise.
type oggPacket struct {
	data    []byte
	granule int64
	eos     bool // the packet ends on the stream's last page
}

// oggStream is one logical stream while its pages are read.
type oggStream struct {
	serial  uint32
	packets []oggPacket
	partial []byte // a packet continued on the next page
	open    bool   // a packet is being continued
	seq     uint32
	eos     bool
}

// readOgg reads every page, reassembles every stream's packets, and hands each
// stream to its codec's mapping.
func readOgg(data []byte) ([]*mkvReadTrack, error) {
	streams, order, err := oggStreams(data)
	if len(order) == 0 {
		if err == nil {
			err = errorf(ErrMalformed, "ogg: no page")
		}
		return nil, err
	}
	var tracks []*mkvReadTrack
	for _, serial := range order {
		t := oggTrack(streams[serial])
		if t == nil {
			continue // a Skeleton stream describes the others, and is not one
		}
		t.track.ID = uint32(len(tracks) + 1)
		tracks = append(tracks, t)
	}
	return tracks, err
}

// oggStreams reads the pages of data and reassembles their packets, stream by
// stream, in the order the streams begin.
func oggStreams(data []byte) (map[uint32]*oggStream, []uint32, error) {
	streams := map[uint32]*oggStream{}
	var order []uint32
	for at := 0; at < len(data); {
		page, size, err := oggPage(data[at:])
		if err != nil {
			return streams, order, fmt.Errorf("%w (page at byte %d)", err, at)
		}
		s := streams[page.serial]
		switch {
		case s == nil && page.flags&oggBOS == 0:
			return streams, order, errorf(ErrMalformed, "ogg: stream %08x starts without a beginning-of-stream page", page.serial)
		case s == nil:
			s = &oggStream{serial: page.serial}
			streams[page.serial] = s
			order = append(order, page.serial)
		case s.eos:
			return streams, order, errorf(ErrMalformed, "ogg: stream %08x continues after its end-of-stream page", page.serial)
		case page.seq != s.seq+1:
			// A gap is a lost page, and the packet it held -- or the
			// part of one -- is gone with it.
			return streams, order, errorf(ErrMalformed, "ogg: stream %08x jumps from page %d to page %d",
				page.serial, s.seq, page.seq)
		}
		s.seq = page.seq
		if err := s.add(page); err != nil {
			return streams, order, err
		}
		at += size
	}
	for _, serial := range order {
		if streams[serial].open {
			return streams, order, fmt.Errorf("%w: ogg stream %08x ends inside a packet", ErrTruncated, serial)
		}
	}
	return streams, order, nil
}

// oggPageHeader is what a page header states.
type oggPageHeader struct {
	flags   byte
	granule int64
	serial  uint32
	seq     uint32
	lacing  []byte
	body    []byte
}

// oggPage reads the page at the start of data, and how many bytes it takes.
func oggPage(data []byte) (oggPageHeader, int, error) {
	if len(data) < oggHeaderSize {
		return oggPageHeader{}, 0, truncatedAt("ogg", len(data), oggHeaderSize-len(data))
	}
	if string(data[:4]) != "OggS" || data[4] != 0 {
		return oggPageHeader{}, 0, errorf(ErrMalformed, "ogg: no page header where one should start")
	}
	segments := int(data[26])
	header := oggHeaderSize + segments
	if len(data) < header {
		return oggPageHeader{}, 0, truncatedAt("ogg", len(data), header-len(data))
	}
	lacing := data[oggHeaderSize:header]
	bodySize := 0
	for _, l := range lacing {
		bodySize += int(l)
	}
	if len(data) < header+bodySize {
		return oggPageHeader{}, 0, truncatedAt("ogg", len(data)-header, bodySize)
	}
	page := data[:header+bodySize]
	if stated := binary.LittleEndian.Uint32(data[22:]); oggCRC(page) != stated {
		return oggPageHeader{}, 0, errorf(ErrMalformed, "ogg: page CRC %08x, the page says %08x", oggCRC(page), stated)
	}
	return oggPageHeader{
		flags:   data[5],
		granule: int64(binary.LittleEndian.Uint64(data[6:])),
		serial:  binary.LittleEndian.Uint32(data[14:]),
		seq:     binary.LittleEndian.Uint32(data[18:]),
		lacing:  lacing,
		body:    page[header:],
	}, len(page), nil
}

// oggCRCTable is RFC 3533 §6's CRC-32: the polynomial 0x04C11DB7, unreflected,
// initialised to zero, with no final XOR -- not the reflected CRC-32 of
// hash/crc32, whose bit order is the other way round.
var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

// oggCRC is the checksum of a page, computed as if its own CRC field were zero.
func oggCRC(page []byte) uint32 {
	var c uint32
	for i, b := range page {
		if i >= 22 && i < 26 {
			b = 0
		}
		c = c<<8 ^ oggCRCTable[byte(c>>24)^b]
	}
	return c
}

// add takes a page's segments into the stream: a lacing value below 255 ends
// a packet, and 255 continues it into the next segment, on this page or the
// next.
func (s *oggStream) add(page oggPageHeader) error {
	// RFC 3533 §6: the flag is set exactly when the page's first segment
	// continues the packet the previous page left open.
	if continued := page.flags&oggContinued != 0; continued != s.open {
		return errorf(ErrMalformed, "ogg: stream %08x page %d: continued flag %t, packet left open %t",
			s.serial, page.seq, continued, s.open)
	}
	if page.flags&oggEOS != 0 {
		s.eos = true
	}
	ended := -1
	start := 0
	for i, l := range page.lacing {
		end := start + int(l)
		piece := page.body[start:end]
		start = end
		if l == 255 {
			s.partial = append(s.partial, piece...)
			s.open = true
			continue
		}
		packet := piece
		if s.open {
			packet = append(s.partial, piece...)
		}
		s.partial, s.open = nil, false
		s.packets = append(s.packets, oggPacket{data: packet, granule: oggNoGranule})
		ended = i
	}
	if s.open && s.eos {
		return errorf(ErrMalformed, "ogg: stream %08x ends inside a packet", s.serial)
	}
	if ended >= 0 {
		last := &s.packets[len(s.packets)-1]
		last.granule = page.granule
		last.eos = page.flags&oggEOS != 0
	}
	return nil
}

// oggTrack hands a stream to its codec's mapping, which its first packet
// names, or reports a codec this package does not read.
func oggTrack(s *oggStream) *mkvReadTrack {
	if len(s.packets) == 0 {
		// A beginning-of-stream page with no whole packet on it: the file
		// ended inside the first one, or the stream never had one.
		err := errorf(ErrMalformed, "ogg: stream %08x holds no whole packet", s.serial)
		return &mkvReadTrack{track: Track{Language: "und"}, configErr: err, samplesErr: err}
	}
	first := s.packets[0].data
	switch {
	case bytes.HasPrefix(first, []byte(opusHeadMagic)):
		return oggOpus(s)
	case bytes.HasPrefix(first, []byte("\x01vorbis")):
		return oggVorbis(s)
	case bytes.HasPrefix(first, []byte("\x7fFLAC")):
		return oggFLAC(s)
	case bytes.HasPrefix(first, []byte("fishead\x00")):
		return nil
	}
	name, kind := "", Other
	for _, known := range oggOtherCodecs {
		if bytes.HasPrefix(first, []byte(known.magic)) {
			name, kind = known.name, known.kind
		}
	}
	t := &mkvReadTrack{track: Track{Kind: kind, Codec: name, Language: "und"}}
	t.configErr = errorf(ErrUnsupportedCodec, "ogg: stream %08x holds %s, whose mapping is not read",
		s.serial, oggCodecName(name, first))
	t.samplesErr = t.configErr
	return t
}

// oggOtherCodecs are the mappings that are named rather than read, so a caller
// is told what a stream holds.
var oggOtherCodecs = []struct {
	magic, name string
	kind        Kind
}{
	{"\x80theora", "theora", Video},
	{"Speex   ", "speex", Audio},
	{"OVP80", "vp08", Video},
	{"\x80kate", "kate", Subtitle},
}

// oggCodecName is how a refusal names what a stream holds.
func oggCodecName(name string, first []byte) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("a codec whose first packet starts %q", first[:min(len(first), 8)])
}

// streamError records why a stream cannot be read on its track.
func streamError(t *mkvReadTrack, err error) *mkvReadTrack {
	if t.configErr == nil {
		t.configErr = err
	}
	t.samplesErr = err
	t.samples = nil
	return t
}

// oggOpus reads an Ogg Opus stream (RFC 7845 §5): the identification header,
// the comment header, then audio packets.
func oggOpus(s *oggStream) *mkvReadTrack {
	t := audioTrack("Opus", opusOutputRate, 0, 0)
	head, err := opusHead(s.packets[0].data)
	if err != nil {
		return streamError(t, err)
	}
	rate := int(head.InputSampleRate)
	if rate == 0 {
		rate = opusOutputRate // §5.1: zero means unspecified
	}
	t.track.Channels, t.track.SampleRate = int(head.OutputChannelCount), rate
	t.config.Channels, t.config.SampleRate = t.track.Channels, rate
	t.config.CodecConfig = s.packets[0].data
	t.config.PreSkip = head.PreSkip
	if len(s.packets) < 2 || !bytes.HasPrefix(s.packets[1].data, []byte("OpusTags")) {
		return streamError(t, errorf(ErrMalformed, "ogg: Opus stream %08x has no comment header after its identification header", s.serial))
	}
	audio := s.packets[2:]
	durations := make([]int64, len(audio))
	for i, p := range audio {
		d, err := opusPacketDuration(p.data)
		if err != nil {
			return streamError(t, fmt.Errorf("%w (packet %d of stream %08x)", err, i, s.serial))
		}
		durations[i] = d
	}
	oggSamples(t, audio, durations, durations)
	return t
}

// opusPacketDuration is how many 48 kHz samples an Opus packet holds, from its
// TOC byte (RFC 6716 §3.1): the configuration states the frame size, and the
// code states one frame, two, or a count in the next byte.
func opusPacketDuration(p []byte) (int64, error) {
	if len(p) == 0 {
		return 0, errorf(ErrMalformed, "opus: an empty packet (RFC 6716 §3.4 R1)")
	}
	config := p[0] >> 3
	var frame int64
	switch {
	case config < 12: // SILK: 10, 20, 40 or 60 ms
		frame = [4]int64{480, 960, 1920, 2880}[config&3]
	case config < 16: // hybrid: 10 or 20 ms
		frame = 480 << (config & 1)
	default: // CELT: 2.5, 5, 10 or 20 ms
		frame = 120 << (config & 3)
	}
	switch p[0] & 3 {
	case 0:
		return frame, nil
	case 1, 2:
		return 2 * frame, nil
	}
	if len(p) < 2 {
		return 0, errorf(ErrMalformed, "opus: a code 3 packet without its frame count (RFC 6716 §3.2.5)")
	}
	count := int64(p[1] & 0x3F)
	// §3.4 R5: at least one frame, and no more than 120 ms of them.
	if count == 0 || count*frame > 5760 {
		return 0, errorf(ErrMalformed, "opus: %d frames of %d samples (RFC 6716 §3.4 R5)", count, frame)
	}
	return count * frame, nil
}

// oggSamples turns a stream's audio packets into samples. durations are what
// each sample lasts; granuleSteps are what each packet adds to the granule
// position, which differs from its duration only for Vorbis's first packet.
//
// The last packet is cut to what the last granule position says is there
// (RFC 7845 §4.4, Vorbis I §A.2): an encoder pads its last frame, and the
// granule is where the real audio ends. Where the stream starts is read off the
// first page that states a granule and is not the last one, as FFmpeg reads it,
// so a stream cut out of a longer one is not trimmed by the samples it lost at
// the front.
func oggSamples(t *mkvReadTrack, audio []oggPacket, durations, granuleSteps []int64) {
	var position, before int64
	for i, p := range audio {
		before += granuleSteps[i]
		if p.granule != oggNoGranule && !p.eos {
			position = p.granule - before
			break
		}
	}
	for i, p := range audio {
		position += granuleSteps[i]
		d := durations[i]
		if p.eos && p.granule != oggNoGranule && p.granule < position && i == len(audio)-1 {
			d = max(d-(position-p.granule), 1)
		}
		t.samples = append(t.samples, Sample{Data: p.data, Duration: uint32(d), Sync: true})
	}
}

// oggVorbis reads an Ogg Vorbis stream (Vorbis I §4.2 and §A.2): three header
// packets, then audio packets.
func oggVorbis(s *oggStream) *mkvReadTrack {
	t := audioTrack("vorb", 0, 0, 0)
	if len(s.packets) < 3 {
		return streamError(t, errorf(ErrMalformed, "ogg: Vorbis stream %08x has %d of its 3 header packets",
			s.serial, len(s.packets)))
	}
	id, comment, setup := s.packets[0].data, s.packets[1].data, s.packets[2].data
	v, err := parseVorbisID(id)
	if err != nil {
		return streamError(t, err)
	}
	t.track.Channels, t.track.SampleRate, t.track.Timescale = v.channels, v.rate, uint32(v.rate)
	t.config.Channels, t.config.SampleRate, t.config.Timescale = v.channels, v.rate, uint32(v.rate)
	if !bytes.HasPrefix(comment, []byte("\x03vorbis")) || !bytes.HasPrefix(setup, []byte("\x05vorbis")) {
		return streamError(t, errorf(ErrMalformed, "ogg: Vorbis stream %08x does not carry its comment and setup headers next",
			s.serial))
	}
	// The three headers, Xiph-laced, are what Matroska's A_VORBIS
	// CodecPrivate holds and what the WebM muxer takes.
	t.config.CodecConfig = xiphLaced(id, comment, setup)
	longModes, err := vorbisModes(setup)
	if err != nil {
		return streamError(t, err)
	}
	audio := s.packets[3:]
	durations, steps, err := vorbisDurations(v, longModes, audio)
	if err != nil {
		return streamError(t, fmt.Errorf("%w (stream %08x)", err, s.serial))
	}
	if err := checkGranules(audio, steps); err != nil {
		return streamError(t, fmt.Errorf("%w (stream %08x)", err, s.serial))
	}
	oggSamples(t, audio, durations, steps)
	return t
}

// vorbisID is what the identification header states (Vorbis I §4.2.2).
type vorbisID struct {
	channels, rate int
	blocks         [2]int // the short and the long block size
}

// parseVorbisID reads a Vorbis identification header.
func parseVorbisID(p []byte) (vorbisID, error) {
	const size = 30
	if len(p) < size {
		return vorbisID{}, errorf(ErrMalformed, "vorbis: an identification header of %d bytes, not %d", len(p), size)
	}
	v := vorbisID{
		channels: int(p[11]),
		rate:     int(binary.LittleEndian.Uint32(p[12:])),
		blocks:   [2]int{1 << (p[28] & 0xF), 1 << (p[28] >> 4)},
	}
	// §4.2.2: version 0, at least one channel, a rate, block sizes from 64 to
	// 8192 with the short one no longer than the long one, and the framing
	// bit set.
	if binary.LittleEndian.Uint32(p[7:]) != 0 || v.channels == 0 || v.rate == 0 ||
		v.blocks[0] < 64 || v.blocks[1] > 8192 || v.blocks[0] > v.blocks[1] || p[29]&1 == 0 {
		return vorbisID{}, errorf(ErrMalformed, "vorbis: the identification header breaks §4.2.2")
	}
	return v, nil
}

// xiphLaced packs packets the way Xiph lacing does (Matroska §10.2.1): the
// count less one, the size of every packet but the last as a run of 255s and
// a remainder, then the packets.
func xiphLaced(packets ...[]byte) []byte {
	out := []byte{byte(len(packets) - 1)}
	for _, p := range packets[:len(packets)-1] {
		n := len(p)
		for ; n >= 255; n -= 255 {
			out = append(out, 255)
		}
		out = append(out, byte(n))
	}
	for _, p := range packets {
		out = append(out, p...)
	}
	return out
}

// vorbisModes reads which of the setup header's modes are long blocks.
//
// The modes are the setup header's last field (Vorbis I §4.2.4): a six-bit
// count less one, then per mode a block flag, two sixteen-bit fields that must
// be zero and an eight-bit mapping number, then the framing bit. Everything
// before them is codebooks, floors and residues, whose lengths only a decoder
// can work out. So the header is read backwards from the framing bit: each
// mode read that way has to have zero fields and a mapping below 64, and the
// count that stops the search is one whose six bits, read on, state that count.
// It is what FFmpeg's vorbis_parser.c does, which says itself it is not
// fool-proof; the granule check in checkGranules is what makes up for that.
func vorbisModes(setup []byte) ([]bool, error) {
	r := backBits{data: setup, pos: len(setup) * 8}
	framed := false
	for r.pos > 97 && !framed {
		framed = r.read(1) == 1
	}
	if !framed {
		return nil, errorf(ErrMalformed, "vorbis: the setup header has no framing bit")
	}
	framing := r.pos
	found := 0
	for count := 1; count <= 64 && r.pos >= 97; count++ {
		if r.read(8) > 63 || r.read(16) != 0 || r.read(16) != 0 {
			break
		}
		r.read(1) // the block flag
		peek := r
		if int(peek.read(6))+1 == count {
			found = count
		}
	}
	if found == 0 || found > 63 {
		return nil, errorf(ErrMalformed, "vorbis: no mode table at the end of the setup header")
	}
	r.pos = framing
	long := make([]bool, found)
	for i := found - 1; i >= 0; i-- {
		r.read(40)
		long[i] = r.read(1) == 1
	}
	return long, nil
}

// backBits reads a Vorbis packet's bits backwards. Vorbis packs its fields
// least significant bit first, so going back from the end meets each field's
// most significant bit first, and the value reads out in the right order.
type backBits struct {
	data []byte
	pos  int // bits not yet read, counted from the start
}

func (r *backBits) read(n int) uint32 {
	var v uint32
	for range n {
		r.pos--
		v = v<<1 | uint32(r.data[r.pos/8]>>(r.pos%8)&1)
	}
	return v
}

// vorbisDurations is what each audio packet returns (Vorbis I §4.3.8): a
// quarter of the previous block and a quarter of its own. The first packet
// returns nothing in the specification; it is given the duration FFmpeg gives
// it, a short block before it, so it can be written as a sample, and its
// granule step stays zero.
func vorbisDurations(v vorbisID, long []bool, audio []oggPacket) (durations, steps []int64, err error) {
	modeBits := bitsFor(len(long) - 1)
	previous := v.blocks[0]
	for i, p := range audio {
		if len(p.data) == 0 || p.data[0]&1 != 0 {
			return nil, nil, errorf(ErrMalformed, "vorbis: packet %d is not an audio packet", i)
		}
		// The packet type, at most six bits of mode and the previous-window
		// flag all fit in the first byte: a mode count is at most 63.
		word := uint32(p.data[0])
		mode := int(word >> 1 & (1<<modeBits - 1))
		if mode >= len(long) {
			return nil, nil, errorf(ErrMalformed, "vorbis: packet %d names mode %d of %d", i, mode, len(long))
		}
		current := v.blocks[0]
		if long[mode] {
			current = v.blocks[1]
			// A long block states the previous block's size in the bit
			// after the mode number.
			previous = v.blocks[word>>(1+modeBits)&1]
		}
		d := int64(previous/4 + current/4)
		durations = append(durations, d)
		if i == 0 {
			d = 0
		}
		steps = append(steps, d)
		previous = current
	}
	return durations, steps, nil
}

// bitsFor is ilog of Vorbis I §9.2.1: how many bits the number n takes.
func bitsFor(n int) int {
	b := 0
	for ; n > 0; n >>= 1 {
		b++
	}
	return b
}

// checkGranules verifies the durations against the granule positions between
// two pages that state one. The last page may state less -- that is how the end
// is trimmed -- and nothing else may differ.
func checkGranules(audio []oggPacket, steps []int64) error {
	last := int64(oggNoGranule)
	var sum int64
	for i, p := range audio {
		sum += steps[i]
		if p.granule == oggNoGranule {
			continue
		}
		if last != oggNoGranule && p.granule != last+sum && !(p.eos && p.granule < last+sum) {
			return errorf(ErrMalformed, "vorbis: the packets up to %d add %d samples to granule %d, and the page says %d",
				i, sum, last, p.granule)
		}
		last, sum = p.granule, 0
	}
	return nil
}

// oggFLAC reads an Ogg FLAC stream (RFC 9639 §10.1): a first packet that
// carries STREAMINFO, then the other metadata blocks, then one frame per packet.
func oggFLAC(s *oggStream) *mkvReadTrack {
	const headerSize = 13 // 0x7F "FLAC", the mapping version, the header count, "fLaC"
	first := s.packets[0].data
	t := audioTrack("fLaC", 0, 0, 0)
	if len(first) < headerSize+flacBlockHeaderSize+flacStreamInfoSize || first[5] != 1 ||
		string(first[9:13]) != flacMarker || first[headerSize]&0x7F != flacStreamInfo {
		return streamError(t, errorf(ErrMalformed, "ogg: FLAC stream %08x does not open with a version 1 mapping header and STREAMINFO",
			s.serial))
	}
	body := first[headerSize+flacBlockHeaderSize:]
	info, err := parseFLACStreamInfo(body[:flacStreamInfoSize])
	if err != nil {
		return streamError(t, err)
	}
	*t = *flacTrack(info)
	audio := s.packets[1:]
	// The header count may be 0, meaning "not stated"; the metadata packets
	// end where the first frame, which starts with its sync code, does.
	for len(audio) > 0 && (len(audio[0].data) == 0 || audio[0].data[0] != 0xFF) {
		audio = audio[1:]
	}
	durations := make([]int64, len(audio))
	for i, p := range audio {
		h, ok := parseFLACFrameHeader(p.data)
		if !ok {
			return streamError(t, errorf(ErrMalformed, "ogg: FLAC stream %08x packet %d is not a frame", s.serial, i))
		}
		durations[i] = int64(h.blockSize)
	}
	oggSamples(t, audio, durations, durations)
	return t
}
