// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
)

// An AVI file is a RIFF form of type "AVI ": a header list that describes each
// stream -- a stream header "strh" and a stream format "strf", which is a
// BITMAPINFOHEADER for video and a WAVEFORMATEX for audio -- then a "movi" list
// holding the streams' chunks in the order they were written, each named by its
// stream's two-digit number (Microsoft, "AVI RIFF File Reference"). A file over
// 1 GiB continues in further RIFF forms of type "AVIX", each with a "movi" list
// of its own (OpenDML AVI File Format Extensions 1.02, §4).
//
// The chunks are walked in file order, the RIFF forms one after another. Neither
// the legacy idx1 index nor the OpenDML indexes ("indx", "ix##") are read: they
// are how a player seeks, and a reader that walks every chunk has nothing to
// seek for. What they would add is a keyframe flag, and that is read from the
// frames themselves instead, as the transport stream reader does, because an
// MP4 written from a wrong flag cannot be seeked without saying so.
//
// The header layouts and the codec tags are read against FFmpeg's
// libavformat/avidec.c and riff.c (2026-10-18). Codecs:
//
//   - video: H.264 ("avc1", the start-code form AVI carries, converted to the
//     length-prefixed form an MP4 holds), MPEG-4 Part 2 ("mp4v": XviD, DivX and
//     the rest of riff.c's list) and Motion JPEG ("mjpg");
//   - audio: linear PCM (as WAV reads it), MPEG audio (as an .mp3 reads),
//     AC-3, and AAC either raw with its AudioSpecificConfig in the stream
//     format or in ADTS frames.
//
// Anything else is reported by its FourCC or WAVE format tag, with
// ErrUnsupportedCodec, and not read.
//
// ⛔ AVI states no presentation time, only an order, so a stream whose pictures
// are reordered -- B-frames -- cannot be carried into a container that needs the
// composition offsets AVI never had. Such a track is refused, named, rather
// than handed over in decode order to play back jerking: H.264 by its slice
// types, MPEG-4 Part 2 by its VOP coding types.

// FormatAVI is a RIFF AVI file, OpenDML extensions included.
const FormatAVI Format = 9

func init() {
	registerElementary(elementaryFormat{
		format: FormatAVI, name: "avi", order: orderMagic,
		sniff: sniffAVI, read: readAVI,
	})
}

// sniffAVI recognises the RIFF form.
func sniffAVI(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "AVI "
}

// aviStream is one stream while the file is walked.
type aviStream struct {
	kind        string // the strh fccType: "vids", "auds", "txts", "dats"
	scale, rate uint32
	format      []byte // the strf chunk
	chunks      [][]byte
	// dropped counts the empty chunks after each chunk: a video encoder
	// writes one for a frame it repeats, and the frame before lasts longer.
	// One before the first frame has no frame to lengthen and is passed over.
	dropped []int
}

// riffWalk calls fn for every chunk of body, which starts at offset base of
// the file. A chunk whose size runs past body is cut to what there is and
// reported as the cut.
func riffWalk(body []byte, fn func(id string, chunk []byte) error) error {
	for at := 0; at+riffChunkHeader <= len(body); {
		id := string(body[at : at+4])
		size := int64(binary.LittleEndian.Uint32(body[at+4:]))
		start := at + riffChunkHeader
		end := int64(start) + size
		var cut error
		if end > int64(len(body)) {
			cut = truncatedAt("avi", len(body)-start, int(min(end-int64(len(body)), 1<<31)))
			end = int64(len(body))
		}
		if err := fn(id, body[start:end]); err != nil {
			return err
		}
		if cut != nil {
			return cut
		}
		at = int(end + end&1) // chunks are padded to an even length
	}
	return nil
}

// readAVI reads every RIFF form of an AVI file.
func readAVI(data []byte) ([]*mkvReadTrack, error) {
	var streams []*aviStream
	first := true
	walkErr := riffWalk(data, func(id string, form []byte) error {
		if id != "RIFF" || len(form) < 4 {
			return nil // what follows the forms is not the file's
		}
		kind := string(form[:4])
		switch {
		case first && kind != "AVI ":
			return errorf(ErrMalformed, "avi: the first RIFF form is %q", kind)
		case !first && kind != "AVIX":
			return nil
		}
		first = false
		return riffWalk(form[4:], func(id string, chunk []byte) error {
			if id != "LIST" || len(chunk) < 4 {
				return nil
			}
			switch string(chunk[:4]) {
			case "hdrl":
				if streams != nil {
					return errorf(ErrMalformed, "avi: a second header list")
				}
				var err error
				streams, err = aviHeaders(chunk[4:])
				return err
			case "movi":
				return aviMovie(chunk[4:], streams)
			}
			return nil
		})
	})
	if walkErr != nil && !errors.Is(walkErr, ErrTruncated) {
		return nil, walkErr
	}
	if streams == nil {
		if walkErr != nil {
			return nil, walkErr
		}
		return nil, errorf(ErrMalformed, "avi: no header list")
	}
	tracks := make([]*mkvReadTrack, 0, len(streams))
	for i, s := range streams {
		t := aviTrack(s)
		t.track.ID = uint32(i + 1)
		tracks = append(tracks, t)
	}
	return tracks, walkErr
}

// aviHeaders reads the stream lists of the header list.
func aviHeaders(hdrl []byte) ([]*aviStream, error) {
	streams := []*aviStream{}
	err := riffWalk(hdrl, func(id string, chunk []byte) error {
		if id != "LIST" || len(chunk) < 4 || string(chunk[:4]) != "strl" {
			return nil
		}
		s := &aviStream{}
		err := riffWalk(chunk[4:], func(id string, c []byte) error {
			switch id {
			case "strh":
				if len(c) < 36 {
					return errorf(ErrMalformed, "avi: a stream header of %d bytes", len(c))
				}
				s.kind = string(c[:4])
				s.scale = binary.LittleEndian.Uint32(c[20:])
				s.rate = binary.LittleEndian.Uint32(c[24:])
			case "strf":
				s.format = c
			}
			return nil
		})
		if s.kind == "" && err == nil {
			err = errorf(ErrMalformed, "avi: a stream list without a stream header")
		}
		streams = append(streams, s)
		return err
	})
	if len(streams) == 0 && err == nil {
		err = errorf(ErrMalformed, "avi: the header list describes no stream")
	}
	return streams, err
}

// aviMovie files the chunks of a movie list under their streams. A chunk is
// named by two decimal digits and a two-letter type -- "00dc", "01wb" -- and a
// "rec " list groups chunks without changing what they are. Index chunks
// ("ix##") and anything not named by a stream of the file are passed over.
func aviMovie(movi []byte, streams []*aviStream) error {
	if streams == nil {
		return errorf(ErrMalformed, "avi: the movie list comes before the header list")
	}
	return riffWalk(movi, func(id string, chunk []byte) error {
		if id == "LIST" {
			if len(chunk) >= 4 && string(chunk[:4]) == "rec " {
				return aviMovie(chunk[4:], streams)
			}
			return nil
		}
		if id[0] < '0' || id[0] > '9' || id[1] < '0' || id[1] > '9' {
			return nil
		}
		n := int(id[0]-'0')*10 + int(id[1]-'0')
		if n >= len(streams) || id[2:] == "pc" { // a palette change is not a frame
			return nil
		}
		s := streams[n]
		if len(chunk) == 0 {
			if len(s.dropped) > 0 {
				s.dropped[len(s.dropped)-1]++
			}
			return nil
		}
		s.chunks = append(s.chunks, chunk)
		s.dropped = append(s.dropped, 0)
		return nil
	})
}

// aviTrack turns a stream into a track.
func aviTrack(s *aviStream) *mkvReadTrack {
	switch s.kind {
	case "vids":
		return aviVideo(s)
	case "auds":
		return aviAudio(s)
	}
	t := &mkvReadTrack{track: Track{Kind: Other, Language: "und"}}
	if s.kind == "txts" {
		t.track.Kind = Subtitle
	}
	return streamError(t, errorf(ErrUnsupportedCodec, "avi: a stream of type %q is not read", s.kind))
}

// aviFourCCs maps the BITMAPINFOHEADER compression codes riff.c lists for the
// three video codecs read here onto their sample entries. They are compared
// in upper case: riff.c lists several in both.
var aviFourCCs = map[string]string{}

func init() {
	for codec, tags := range map[string]string{
		"avc1": "H264 X264 AVC1 DAVC SMV2 VSSH Q264 V264 GAVC UMSV TSHD INMC",
		"mp4v": "FMP4 DIVX DX50 XVID MP4S M4S2 ZMP4 DIV1 BLZ0 MP4V UMP4 WV1F SEDG RMP4 3IV2 WAWV " +
			"FFDS FVFW DCOD MVXM PM4V SMP4 DXGM VIDM M4T3 GEOX HDX4 DM4V DMK2 DYM4 DIGI EPHV " +
			"EM4A M4CC SN40 VSPX ULDX GEOV SIPP SM4V XVIX DREX QMP4 PLV1 GLV4 GMP4 MNM4 GTM4",
		"mjpg": "MJPG MSC2 LJPG DMB1 MJPA JR24 JPGL MJLS JPEG IJPG ACDV QIVG SLMJ CJPG IJLV " +
			"MVJP AVI1 AVI2 MTSJ ZJPG MMJP",
	} {
		for _, tag := range strings.Fields(tags) {
			aviFourCCs[tag] = codec
		}
	}
}

// aviClock is a stream's timescale and the duration of one of its chunks: the
// stream header states rate/scale chunks a second, reduced here so the
// timescale is as small as it can be.
func aviClock(s *aviStream) (uint32, uint32, error) {
	if s.scale == 0 || s.rate == 0 {
		return 0, 0, errorf(ErrMalformed, "avi: a %s stream of %d/%d chunks a second", s.kind, s.rate, s.scale)
	}
	g := gcd(s.rate, s.scale)
	return s.rate / g, s.scale / g, nil
}

// aviVideo reads a video stream.
func aviVideo(s *aviStream) *mkvReadTrack {
	t := &mkvReadTrack{track: Track{Kind: Video, Language: "und"}, config: TrackConfig{Kind: Video}}
	if len(s.format) < 40 {
		return streamError(t, errorf(ErrMalformed, "avi: a video stream format of %d bytes; a BITMAPINFOHEADER is 40", len(s.format)))
	}
	width := int(int32(binary.LittleEndian.Uint32(s.format[4:])))
	height := int(int32(binary.LittleEndian.Uint32(s.format[8:])))
	if height < 0 {
		height = -height // a top-down bitmap; the frames are the same
	}
	fourcc := string(s.format[16:20])
	codec, known := aviFourCCs[strings.ToUpper(fourcc)]
	t.track.Codec, t.config.Codec = codec, codec
	t.track.Width, t.track.Height = width, height
	t.config.Width, t.config.Height = width, height
	if !known {
		t.track.Codec = fourcc
		return streamError(t, errorf(ErrUnsupportedCodec, "avi: video FourCC %q is not read", fourcc))
	}
	scale, tick, err := aviClock(s)
	if err != nil {
		return streamError(t, err)
	}
	t.track.Timescale, t.config.Timescale = scale, scale
	var frames [][]byte
	switch codec {
	case "avc1":
		frames, err = aviAVC(t, s)
	case "mp4v":
		frames, err = aviMPEG4(t, s)
	default:
		frames = s.chunks
	}
	if err != nil {
		return streamError(t, err)
	}
	for i, f := range frames {
		var sync bool
		switch codec {
		case "avc1":
			sync = isSyncUnit(f, false)
		case "mp4v":
			sync = mpeg4VOPType(f) == mpeg4IVOP
		default:
			sync = true // every Motion JPEG frame is a picture of its own
		}
		t.samples = append(t.samples, Sample{
			Data:     f,
			Duration: tick * uint32(1+s.dropped[i]),
			Sync:     sync,
		})
	}
	return t
}

// aviAVC converts an H.264 stream's chunks, which AVI carries in the start-code
// form of Annex B, to the length-prefixed form an MP4 holds, lifting the
// parameter sets out into the configuration, and refuses a stream that has
// B-slices (see the ⛔ above) or an avcC record, whose frames are another form.
func aviAVC(t *mkvReadTrack, s *aviStream) ([][]byte, error) {
	if len(s.format) > 40 && s.format[40] == 1 {
		return nil, errorf(ErrUnsupportedCodec, "avi: H.264 with an avcC record in its stream format: "+
			"its frames are length-prefixed, which AVI writers do not use and this does not read")
	}
	frames := make([][]byte, 0, len(s.chunks))
	for i, chunk := range s.chunks {
		for _, nalu := range splitAnnexB(chunk) {
			if avcBSlice(nalu) {
				return nil, errorf(ErrUnsupportedCodec, "avi: H.264 frame %d is a B-slice: AVI states no "+
					"presentation order, and a track of reordered pictures written without one plays out of order", i)
			}
		}
		payload, params := convertAnnexB(chunk, false)
		if len(params.sps) > 0 && len(t.config.SPS) == 0 {
			t.config.SPS = params.sps
		}
		if len(params.pps) > 0 && len(t.config.PPS) == 0 {
			t.config.PPS = params.pps
		}
		frames = append(frames, payload)
	}
	if cfg, err := avcStreamConfig("avc1", naluScan{sets: parameterSets{sps: t.config.SPS, pps: t.config.PPS}}); err != nil {
		t.configErr = err
	} else {
		t.config.Width, t.config.Height = cfg.Width, cfg.Height
		t.track.Width, t.track.Height = cfg.Width, cfg.Height
	}
	return frames, nil
}

// avcBSlice reports whether a NAL unit is a slice of type B or SB: slice_type,
// the second Exp-Golomb field of the slice header (ISO/IEC 14496-10 §7.3.3),
// is 1 or 6.
func avcBSlice(nalu []byte) bool {
	if len(nalu) < 2 {
		return false
	}
	if kind := nalu[0] & 0x1F; kind != 1 && kind != 5 {
		return false
	}
	r := expGolomb{data: unescapeRBSP(nalu[1:min(len(nalu), 32)])}
	r.ue() // first_mb_in_slice
	sliceType, ok := r.ue()
	return ok && sliceType%5 == 1
}

// unescapeRBSP drops the emulation prevention bytes of §7.4.1: a 0x03 after
// two zero bytes.
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// expGolomb reads ue(v) fields (§9.1).
type expGolomb struct {
	data []byte
	pos  int
}

func (r *expGolomb) bit() (uint32, bool) {
	if r.pos >= len(r.data)*8 {
		return 0, false
	}
	b := uint32(r.data[r.pos/8]>>(7-r.pos%8)) & 1
	r.pos++
	return b, true
}

func (r *expGolomb) ue() (uint32, bool) {
	zeros := 0
	for {
		b, ok := r.bit()
		if !ok || zeros > 31 {
			return 0, false
		}
		if b == 1 {
			break
		}
		zeros++
	}
	v := uint32(0)
	for range zeros {
		b, ok := r.bit()
		if !ok {
			return 0, false
		}
		v = v<<1 | b
	}
	return 1<<zeros - 1 + v, true
}

// MPEG-4 Part 2 VOP coding types (ISO/IEC 14496-2 §6.3.5).
const (
	mpeg4IVOP = 0
	mpeg4BVOP = 2
	mpeg4None = -1
)

// mpeg4VOPType is the coding type of the first VOP a frame holds: the two bits
// after the vop_start_code 00 00 01 B6.
func mpeg4VOPType(frame []byte) int {
	i := bytes.Index(frame, []byte{0, 0, 1, 0xB6})
	if i < 0 || i+4 >= len(frame) {
		return mpeg4None
	}
	return int(frame[i+4] >> 6)
}

// aviMPEG4 checks an MPEG-4 Part 2 stream and states its configuration: the
// visual object sequence and object layer headers that precede the first VOP,
// which the stream format carries after its BITMAPINFOHEADER or the first frame
// carries in front of its picture. It is what an MP4 esds would hold as the
// decoder-specific information (ISO/IEC 14496-2 Annex K), and no muxer here
// writes mp4v yet.
func aviMPEG4(t *mkvReadTrack, s *aviStream) ([][]byte, error) {
	for i, f := range s.chunks {
		if mpeg4VOPType(f) == mpeg4BVOP {
			return nil, errorf(ErrUnsupportedCodec, "avi: MPEG-4 frame %d holds a B-VOP: AVI states no "+
				"presentation order, and a track of reordered pictures written without one plays out of order", i)
		}
	}
	switch {
	case len(s.format) > 40:
		t.config.CodecConfig = s.format[40:]
	case len(s.chunks) > 0:
		if i := bytes.Index(s.chunks[0], []byte{0, 0, 1, 0xB6}); i > 0 {
			t.config.CodecConfig = s.chunks[0][:i]
		}
	}
	return s.chunks, nil
}

// aviAudio reads an audio stream. Its chunks are cut wherever the writer
// chose, so the stream is read the way its codec's own elementary stream is,
// from the chunks joined; only raw AAC, which has no framing, is a frame a
// chunk.
func aviAudio(s *aviStream) *mkvReadTrack {
	t := audioTrack("", 0, 0, 0)
	f, err := parseWAVFormat(s.format)
	if err != nil {
		return streamError(t, err)
	}
	joined := bytes.Join(s.chunks, nil)
	var extra []byte
	if len(s.format) >= 18 {
		n := int(binary.LittleEndian.Uint16(s.format[16:]))
		extra = s.format[18:min(len(s.format), 18+n)]
	}
	var read *mkvReadTrack
	switch f.tag {
	case waveFormatPCM, waveFormatIEEEFloat:
		read, err = wavTrack(f, joined, uint64(len(joined)))
	case 0x0050, 0x0055: // MPEG audio, Layer I/II and Layer III
		read, err = firstTrack(readMPEGAudio(joined))
	case 0x2000:
		read, err = firstTrack(readAC3(joined))
	case 0x00FF, 0x1600, 0x1610:
		if len(extra) > 0 {
			read, err = aviRawAAC(f, extra, s.chunks)
		} else {
			read, err = firstTrack(readADTS(joined))
		}
	default:
		t.track.Codec = wavTagName(f.tag)
		return streamError(t, errorf(ErrUnsupportedCodec, "avi: audio format tag 0x%04x is not read", f.tag))
	}
	if read == nil {
		return streamError(t, err)
	}
	// A frame cut short at the end of the joined chunks is a stream that
	// stopped, not a file that did: what the file itself lost is reported by
	// the walk over its chunks.
	return read
}

// firstTrack is the single track a one-stream reader returns, or nil.
func firstTrack(tracks []*mkvReadTrack, err error) (*mkvReadTrack, error) {
	if len(tracks) == 0 {
		return nil, err
	}
	return tracks[0], err
}

// aviRawAAC reads AAC stored without ADTS headers: one raw data block a chunk,
// described by the AudioSpecificConfig of the stream format.
func aviRawAAC(f wavFormat, asc []byte, chunks [][]byte) (*mkvReadTrack, error) {
	t := audioTrack("mp4a", uint32(f.sampleRate), f.channels, f.sampleRate)
	if err := aacTrackConfig(&t.config, asc); err != nil {
		return nil, errorf(ErrMalformed, "avi: AAC AudioSpecificConfig: %v", err)
	}
	t.track.Channels = t.config.Channels
	for _, c := range chunks {
		t.samples = append(t.samples, Sample{Data: c, Duration: aacFrameSamples, Sync: true})
	}
	return t, nil
}

// ac3Syntax walks AC-3 sync frames, ETSI TS 102 366 §4.4.1, with the header
// reader ac3frame.go already has. Enhanced AC-3, whose independent and
// dependent substreams would have to be gathered into one sample, is not read
// this way.
var ac3Syntax = frameSyntax{
	name:       "ac-3",
	headerSize: 8,
	sync:       ac3SyncWord >> 8,
	length: func(data []byte) int {
		f, err := parseAC3Frame(data)
		if err != nil || f.EAC3 {
			return 0
		}
		return f.FrameSize
	},
	// The sampling rate and the channel layout, which a decoder locks to.
	sameStream: func(a, b []byte) bool {
		x, _ := parseAC3Frame(a)
		y, _ := parseAC3Frame(b)
		return x.FSCod == y.FSCod && x.ACMod == y.ACMod && x.LFEOn == y.LFEOn
	},
}

// readAC3 reads a stream of AC-3 sync frames.
func readAC3(data []byte) ([]*mkvReadTrack, error) {
	frames, err := ac3Syntax.walkFrames(data)
	if len(frames) == 0 {
		return nil, noFrames("ac-3", err)
	}
	var samples []Sample
	for _, f := range frames {
		samples = append(samples, Sample{Data: data[f[0] : f[0]+f[1]], Duration: 1536, Sync: true})
	}
	cfg, _ := ac3TrackConfig("ac-3", samples)
	t := audioTrack("ac-3", uint32(cfg.SampleRate), cfg.Channels, cfg.SampleRate)
	t.config.CodecConfig = cfg.CodecConfig
	t.samples = samples
	return oneTrack(t, err)
}
