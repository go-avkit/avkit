package container

import "fmt"

// AC-3 and Enhanced AC-3 state their configuration inside every frame rather
// than in a header a container carries, which is why Matroska usually states no
// CodecPrivate for them. An MP4 sample entry needs a dac3 or dec3 record, so
// one is built from what a frame says about itself.
//
// Syntax and tables are ETSI TS 102 366, read against FFmpeg's
// libavcodec/ac3_parser.c and ac3tab.c (2026-10-08). Only the fields those two
// records need are read: this is not a decoder and does not pretend to be one.

const ac3SyncWord = 0x0B77

// ac3SampleRates is indexed by fscod. The fourth value is reserved, and a frame
// that states it is refused rather than given a rate of zero.
var ac3SampleRates = [4]int{48000, 44100, 32000, 0}

// ac3Channels is indexed by acmod and counts the full-bandwidth channels, which
// is every channel but the low-frequency one.
var ac3Channels = [8]int{2, 1, 2, 3, 3, 4, 4, 5}

// ac3BitRates is indexed by the frame size code's upper five bits, in kbit/s.
var ac3BitRates = [19]int{
	32, 40, 48, 56, 64, 80, 96, 112, 128,
	160, 192, 224, 256, 320, 384, 448, 512, 576, 640,
}

// eac3Blocks is indexed by numblkscod: how many audio blocks a frame holds.
var eac3Blocks = [4]int{1, 2, 3, 6}

// ac3Frame is what one sync frame states about itself.
type ac3Frame struct {
	EAC3        bool
	FSCod       byte
	BSID        byte
	BSMod       byte
	ACMod       byte
	LFEOn       bool
	BitRateCode byte // AC-3 only; E-AC-3 states a frame size instead
	FrameSize   int  // bytes
	SampleRate  int
	Channels    int // including the low-frequency channel
	NumBlocks   int
	StreamType  byte // E-AC-3: 0 independent, 1 dependent, 2 AC-3 converted
	SubstreamID byte
}

// parseAC3Frame reads one sync frame's header.
//
// ⛔ Which syntax follows the sync word is decided by a field that comes AFTER
// several others: bsid sits 29 bits in, and a frame with bsid above 10 is
// Enhanced AC-3, whose header shares nothing with AC-3's beyond those first
// sixteen bits. So it is read ahead without consuming, the way the format
// itself requires.
func parseAC3Frame(data []byte) (ac3Frame, error) {
	var f ac3Frame
	if len(data) < 6 {
		return f, fmt.Errorf("%w: a sync frame is at least 6 bytes, not %d", ErrCodecMismatch, len(data))
	}
	if int(data[0])<<8|int(data[1]) != ac3SyncWord {
		return f, fmt.Errorf("%w: no 0x0B77 sync word", ErrCodecMismatch)
	}
	r := newAC3Bits(data[2:])

	// bsid is 29 bits past the sync word, whichever syntax this is.
	f.BSID = byte(r.peek(29) & 0x1F)
	if f.BSID > 16 {
		return f, fmt.Errorf("%w: bit stream identification %d", ErrCodecMismatch, f.BSID)
	}
	f.NumBlocks = 6
	if f.BSID <= 10 {
		return parseAC3Classic(&r, f)
	}
	return parseEAC3(&r, f)
}

// parseAC3Classic reads the header of a frame that is plain AC-3.
func parseAC3Classic(r *ac3Bits, f ac3Frame) (ac3Frame, error) {
	r.skip(16) // crc1
	f.FSCod = byte(r.bits(2))
	if f.FSCod == 3 {
		return f, fmt.Errorf("%w: reserved sample rate code", ErrCodecMismatch)
	}
	frameSizeCode := byte(r.bits(6))
	if frameSizeCode > 37 {
		return f, fmt.Errorf("%w: frame size code %d", ErrCodecMismatch, frameSizeCode)
	}
	f.BitRateCode = frameSizeCode >> 1
	r.skip(5) // bsid, already read ahead
	f.BSMod = byte(r.bits(3))
	f.ACMod = byte(r.bits(3))
	switch {
	case f.ACMod == 2:
		r.skip(2) // dsurmod
	default:
		if f.ACMod&1 != 0 && f.ACMod != 1 {
			r.skip(2) // cmixlev
		}
		if f.ACMod&4 != 0 {
			r.skip(2) // surmixlev
		}
	}
	f.LFEOn = r.bits(1) == 1
	if r.err != nil {
		return ac3Frame{}, r.err
	}
	// Above bsid 8 the rate is halved once per step, which is how the format
	// states its low-rate variants.
	shift := uint(0)
	if f.BSID > 8 {
		shift = uint(f.BSID) - 8
	}
	f.SampleRate = ac3SampleRates[f.FSCod] >> shift
	f.Channels = ac3Channels[f.ACMod]
	if f.LFEOn {
		f.Channels++
	}
	f.FrameSize = ac3FrameSize(frameSizeCode, f.FSCod)
	return f, nil
}

// parseEAC3 reads the header of a frame that is Enhanced AC-3.
func parseEAC3(r *ac3Bits, f ac3Frame) (ac3Frame, error) {
	f.EAC3 = true
	f.StreamType = byte(r.bits(2))
	if f.StreamType == 3 {
		return ac3Frame{}, fmt.Errorf("%w: reserved frame type", ErrCodecMismatch)
	}
	f.SubstreamID = byte(r.bits(3))
	f.FrameSize = (int(r.bits(11)) + 1) * 2
	if f.FrameSize < 6 {
		return ac3Frame{}, fmt.Errorf("%w: a frame of %d bytes", ErrCodecMismatch, f.FrameSize)
	}
	f.FSCod = byte(r.bits(2))
	if f.FSCod == 3 {
		// A reduced rate: the real code is in the next field and the rate is
		// halved.
		code := byte(r.bits(2))
		if code == 3 {
			return ac3Frame{}, fmt.Errorf("%w: reserved sample rate code", ErrCodecMismatch)
		}
		f.SampleRate = ac3SampleRates[code] / 2
	} else {
		f.NumBlocks = eac3Blocks[r.bits(2)]
		f.SampleRate = ac3SampleRates[f.FSCod]
	}
	f.ACMod = byte(r.bits(3))
	f.LFEOn = r.bits(1) == 1
	// No error check: this path reads at most 26 bits, and the caller's peek of
	// 29 could not have succeeded without them being there. The AC-3 path below
	// is different -- it reads up to forty -- and does check.
	f.Channels = ac3Channels[f.ACMod]
	if f.LFEOn {
		f.Channels++
	}
	return f, nil
}

// BitRate is the frame's rate in kbit/s, which a dec3 record states and an AC-3
// frame gives by a code.
func (f ac3Frame) BitRate() int {
	if !f.EAC3 {
		if int(f.BitRateCode) >= len(ac3BitRates) {
			return 0
		}
		shift := uint(0)
		if f.BSID > 8 {
			shift = uint(f.BSID) - 8
		}
		return ac3BitRates[f.BitRateCode] >> shift
	}
	if f.NumBlocks == 0 || f.SampleRate == 0 {
		return 0
	}
	// 8 bits a byte over the time the frame covers, which is 256 samples a
	// block.
	return 8 * f.FrameSize * f.SampleRate / (f.NumBlocks * 256) / 1000
}

// ac3Bits reads fields in a straight line and keeps the first error, like the
// readers in the sibling bitstream packages.
type ac3Bits struct {
	data []byte
	pos  int // in bits
	err  error
}

func newAC3Bits(data []byte) ac3Bits { return ac3Bits{data: data} }

func (r *ac3Bits) bits(n int) uint32 {
	if r.err != nil {
		return 0
	}
	if r.pos+n > len(r.data)*8 {
		r.err = fmt.Errorf("%w: the frame ends inside its header", ErrCodecMismatch)
		return 0
	}
	var v uint32
	for i := 0; i < n; i++ {
		b := r.data[(r.pos+i)/8] >> (7 - uint((r.pos+i)%8)) & 1
		v = v<<1 | uint32(b)
	}
	r.pos += n
	return v
}

func (r *ac3Bits) skip(n int) { r.bits(n) }

// peek reads without consuming, which is how bsid is reached before the syntax
// it decides is known.
func (r *ac3Bits) peek(n int) uint32 {
	at := r.pos
	v := r.bits(n)
	r.pos = at
	return v
}

// ac3FrameSizeWords is the frame length in 16-bit words, by frame size code and
// then by fscod. Transcribed from ff_ac3_frame_size_tab (2026-10-08).
//
// ⛔ It is not a formula. At 48 kHz and 32 kHz the length is proportional to the
// rate, but at 44.1 kHz it is not: consecutive codes of the same rate differ by
// one word, because a frame there cannot hold a whole number of samples. A
// reader computing the length instead of looking it up is right two times in
// three and silently short on the rest.
var ac3FrameSizeWords = [38][3]int{
	{64, 69, 96},
	{64, 70, 96},
	{80, 87, 120},
	{80, 88, 120},
	{96, 104, 144},
	{96, 105, 144},
	{112, 121, 168},
	{112, 122, 168},
	{128, 139, 192},
	{128, 140, 192},
	{160, 174, 240},
	{160, 175, 240},
	{192, 208, 288},
	{192, 209, 288},
	{224, 243, 336},
	{224, 244, 336},
	{256, 278, 384},
	{256, 279, 384},
	{320, 348, 480},
	{320, 349, 480},
	{384, 417, 576},
	{384, 418, 576},
	{448, 487, 672},
	{448, 488, 672},
	{512, 557, 768},
	{512, 558, 768},
	{640, 696, 960},
	{640, 697, 960},
	{768, 835, 1152},
	{768, 836, 1152},
	{896, 975, 1344},
	{896, 976, 1344},
	{1024, 1114, 1536},
	{1024, 1115, 1536},
	{1152, 1253, 1728},
	{1152, 1254, 1728},
	{1280, 1393, 1920},
	{1280, 1394, 1920},
}

// ac3FrameSize is the frame length in bytes, or zero for a code the table does
// not hold.
func ac3FrameSize(frameSizeCode, fscod byte) int {
	if int(frameSizeCode) >= len(ac3FrameSizeWords) || fscod > 2 {
		return 0
	}
	return ac3FrameSizeWords[frameSizeCode][fscod] * 2
}

// ac3Writer builds a record bit by bit, since neither dac3 nor dec3 lands on
// byte boundaries.
type ac3Writer struct {
	b []byte
	n uint8
}

func (w *ac3Writer) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n == 0 {
			w.b = append(w.b, 0)
			w.n = 8
		}
		w.n--
		if v>>uint(i)&1 == 1 {
			w.b[len(w.b)-1] |= 1 << w.n
		}
	}
}

// dac3Record is the AC3SpecificBox payload an MP4 sample entry carries, from
// ETSI TS 102 366 Annex F.3.
func dac3Record(f ac3Frame) []byte {
	var w ac3Writer
	w.put(uint32(f.FSCod), 2)
	w.put(uint32(f.BSID), 5)
	w.put(uint32(f.BSMod), 3)
	w.put(uint32(f.ACMod), 3)
	w.put(boolToBit(f.LFEOn), 1)
	w.put(uint32(f.BitRateCode), 5)
	w.put(0, 5) // reserved
	return w.b
}

// dec3Record is the EC3SpecificBox payload, from Annex F.6.
//
// One independent substream is described, which is what a Matroska track
// carries: num_ind_sub counts them MINUS ONE, so zero means one. A frame
// holding several would need each described, and nothing here has produced one.
func dec3Record(f ac3Frame) []byte {
	var w ac3Writer
	rate := f.BitRate()
	if rate > 0x1FFF {
		rate = 0x1FFF
	}
	w.put(uint32(rate), 13)
	w.put(0, 3) // num_ind_sub: one substream
	w.put(uint32(f.FSCod), 2)
	w.put(uint32(f.BSID), 5)
	w.put(0, 1) // reserved
	w.put(0, 1) // asvc
	w.put(uint32(f.BSMod), 3)
	w.put(uint32(f.ACMod), 3)
	w.put(boolToBit(f.LFEOn), 1)
	w.put(0, 3) // reserved
	w.put(0, 4) // num_dep_sub
	w.put(0, 1) // reserved, since num_dep_sub is zero
	return w.b
}

func boolToBit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// ac3TrackConfig fills in what a container could not state about an AC-3 or
// Enhanced AC-3 track, from the frames themselves.
func ac3TrackConfig(codec string, samples []Sample) (TrackConfig, error) {
	var cfg TrackConfig
	for _, s := range samples {
		f, err := parseAC3Frame(s.Data)
		if err != nil {
			continue // a sample that is not a sync frame is passed over
		}
		if f.EAC3 && f.StreamType == 1 {
			// A dependent substream describes a part of another one and not a
			// track; the independent one states the track.
			continue
		}
		cfg.Kind = Audio
		cfg.Codec = codec
		cfg.Channels = f.Channels
		cfg.SampleRate = f.SampleRate
		if f.EAC3 {
			cfg.CodecConfig = dec3Record(f)
		} else {
			cfg.CodecConfig = dac3Record(f)
		}
		return cfg, nil
	}
	return cfg, fmt.Errorf("%w: none of the %d %s samples begins a sync frame",
		ErrNoConfiguration, len(samples), codec)
}
