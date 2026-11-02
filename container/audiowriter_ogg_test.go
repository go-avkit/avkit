// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/at-wat/ebml-go"
)

// awOpusPacket is a code 0 packet of 20 ms CELT (RFC 6716 §3.1: configuration
// 31), n bytes long, whose bytes after the table of contents are anything.
func awOpusPacket(n int, toc byte) []byte {
	p := make([]byte, n)
	p[0] = toc
	for i := 1; i < n; i++ {
		p[i] = byte(i * 7)
	}
	return p
}

const (
	awTOC20ms  = 0xF8 // 960 samples
	awTOC2p5ms = 0xE0 // 120 samples
)

func awOpusConfig(t *testing.T) TrackConfig {
	t.Helper()
	head, err := OpusHead(TrackConfig{Channels: 1, SampleRate: 48000, PreSkip: 312})
	if err != nil {
		t.Fatal(err)
	}
	return TrackConfig{Kind: Audio, Codec: "Opus", Timescale: 48000, Channels: 1, SampleRate: 48000, CodecConfig: head}
}

// awPages lists the pages of an Ogg file.
func awPages(t *testing.T, data []byte) []oggPageHeader {
	t.Helper()
	var pages []oggPageHeader
	for at := 0; at < len(data); {
		p, n, err := oggPage(data[at:])
		if err != nil {
			t.Fatalf("page at %d: %v", at, err)
		}
		pages = append(pages, p)
		at += n
	}
	return pages
}

func TestOggRoundTrip(t *testing.T) {
	for _, c := range []struct {
		src    string
		track  string
		format Format
	}{
		{"sine.opus", "opus", FormatOgg}, {"ffmpeg-opus.mp4", "opus", FormatOgg}, {"ffmpeg-opus.mkv", "opus", FormatOgg},
		{"sine.ogg", "vorbis", FormatOgg}, {"ffmpeg-vorbis.mkv", "vorbis", FormatOgg},
		{"sine.flac", "flac", FormatOgg}, {"ffmpeg-flac.mp4", "flac", FormatOgg}, {"sine.oga", "flac", FormatOgg},
	} {
		t.Run(c.src, func(t *testing.T) {
			cfg, samples := awTrack(t, c.src)
			out := awWrite(t, c.format, cfg, samples)
			got, back := awReadBack(t, out)
			awSameSamples(t, c.src, back, samples)
			if !strings.EqualFold(got.Codec, cfg.Codec) || got.Channels != cfg.Channels || got.SampleRate != cfg.SampleRate {
				t.Errorf("read back %s %d ch %d Hz, wrote %s %d ch %d Hz", got.Codec, got.Channels, got.SampleRate,
					cfg.Codec, cfg.Channels, cfg.SampleRate)
			}
			if strings.EqualFold(cfg.Codec, "Opus") && got.PreSkip != cfg.PreSkip {
				t.Errorf("pre-skip %d, wrote %d", got.PreSkip, cfg.PreSkip)
			}
			pages := awPages(t, out)
			first, last := pages[0], pages[len(pages)-1]
			if first.flags&oggBOS == 0 || last.flags&oggEOS == 0 || len(first.lacing) != 1 {
				t.Errorf("first page flags %x lacing %d, last page flags %x", first.flags, len(first.lacing), last.flags)
			}
			for i, p := range pages {
				if p.seq != uint32(i) || p.serial != first.serial {
					t.Fatalf("page %d: sequence %d, serial %08x", i, p.seq, p.serial)
				}
				if i > 0 && p.flags&oggBOS != 0 || i < len(pages)-1 && p.flags&oggEOS != 0 {
					t.Fatalf("page %d: flags %x", i, p.flags)
				}
			}
		})
	}
}

func TestOggPagesFollowTheLacingRules(t *testing.T) {
	cfg := awOpusConfig(t)
	// Packet sizes that exercise the lacing: empty segments after a run of 255s,
	// a packet that fits a page by one byte, and one that cannot.
	sizes := []int{1, 254, 255, 256, 509, 510, 511, 1000, 65024, 65025, 65026, 70000, 3, 255 * 3}
	var samples []Sample
	for _, n := range sizes {
		samples = append(samples, Sample{Data: awOpusPacket(n, awTOC20ms), Duration: 960, Sync: true})
	}
	out := awWrite(t, FormatOgg, cfg, samples)
	_, back := awReadBack(t, out)
	awSameSamples(t, "lacing", back, samples)

	var continued, unfinished int
	for _, p := range awPages(t, out) {
		if p.flags&oggContinued != 0 {
			continued++
		}
		if p.granule == oggNoGranule {
			unfinished++
		}
	}
	if continued == 0 || unfinished == 0 {
		t.Errorf("the 70000-byte packet should span pages: %d continued, %d with no packet ending", continued, unfinished)
	}
}

func TestOggPagesCloseAfterASecondOrWhenFull(t *testing.T) {
	cfg := awOpusConfig(t)
	var long, tiny []Sample
	for range 120 { // 2.4 s of 20 ms packets
		long = append(long, Sample{Data: awOpusPacket(20, awTOC20ms), Duration: 960})
	}
	for range 600 { // 600 packets of 2.5 ms: 255 segments fill a page first
		tiny = append(tiny, Sample{Data: awOpusPacket(10, awTOC2p5ms), Duration: 120})
	}
	pages := awPages(t, awWrite(t, FormatOgg, cfg, long))
	// Two header pages, then a page at 50 packets and another at 100, and the rest.
	if len(pages) != 5 {
		t.Fatalf("%d pages, want 5", len(pages))
	}
	if want := int64(312 + 0); pages[0].granule != 0 || pages[1].granule != 0 || want == 0 {
		t.Errorf("header granules %d %d", pages[0].granule, pages[1].granule)
	}
	// RFC 7845 §4: the granule position counts the samples decoded, so the
	// first page's 50 packets are 48000 and not 48000 plus the pre-skip.
	if pages[2].granule != 48000 || pages[3].granule != 96000 || pages[4].granule != 120*960 {
		t.Errorf("granules %d %d %d", pages[2].granule, pages[3].granule, pages[4].granule)
	}
	for _, p := range awPages(t, awWrite(t, FormatOgg, cfg, tiny))[2:] {
		if len(p.lacing) > 255 {
			t.Fatalf("a page of %d segments", len(p.lacing))
		}
	}
}

func TestOggSerialAndHeaders(t *testing.T) {
	cfg := awOpusConfig(t)
	samples := []Sample{{Data: awOpusPacket(30, awTOC20ms), Duration: 960}}
	a, b := awWrite(t, FormatOgg, cfg, samples), awWrite(t, FormatOgg, cfg, samples)
	if !bytes.Equal(a, b) {
		t.Error("the same track gave two different files")
	}
	fixed := awPages(t, awWrite(t, FormatOgg, cfg, samples, OggSerial(0xCAFE)))
	if fixed[0].serial != 0xCAFE {
		t.Errorf("serial = %x", fixed[0].serial)
	}
	pages := awPages(t, a)
	if !bytes.HasPrefix(pages[0].body, []byte("OpusHead")) || !bytes.HasPrefix(pages[1].body, []byte("OpusTags")) {
		t.Errorf("headers %q %q", pages[0].body[:8], pages[1].body[:8])
	}
	// A track with no audio is still a stream: its last page is the comment header.
	empty := awPages(t, awWrite(t, FormatOgg, cfg, nil))
	if len(empty) != 2 || empty[1].flags&oggEOS == 0 || empty[1].granule != 0 {
		t.Errorf("empty stream: %+v", empty)
	}
}

func TestOggFLACHeaderIsTheMapping(t *testing.T) {
	cfg, samples := awTrack(t, "sine.flac")
	pages := awPages(t, awWrite(t, FormatOgg, cfg, samples))
	first := pages[0].body
	// RFC 9639 §10.1: 0x7F "FLAC", version 1.0, one header packet to follow,
	// "fLaC", the STREAMINFO block header and STREAMINFO.
	if want := []byte{0x7F, 'F', 'L', 'A', 'C', 1, 0, 0, 1, 'f', 'L', 'a', 'C', 0, 0, 0, 34}; !bytes.HasPrefix(first, want) ||
		len(first) != 51 || !bytes.Equal(first[17:], cfg.CodecConfig[8:8+34]) {
		t.Errorf("mapping header % x", first)
	}
	if pages[1].body[0] != 0x84 {
		t.Errorf("the comment block is % x, want the last-block VORBIS_COMMENT 0x84", pages[1].body[:4])
	}
}

func TestOggEndOfStreamIsTrimmed(t *testing.T) {
	packets := func(last Sample) []Sample {
		return []Sample{
			{Data: awOpusPacket(20, awTOC20ms), Duration: 960},
			{Data: awOpusPacket(20, awTOC20ms), Duration: 960},
			last,
		}
	}
	granule := func(cfg TrackConfig, samples []Sample) int64 {
		pages := awPages(t, awWrite(t, FormatOgg, cfg, samples))
		return pages[len(pages)-1].granule
	}
	cfg := awOpusConfig(t)
	full := Sample{Data: awOpusPacket(20, awTOC20ms), Duration: 960}
	short := Sample{Data: awOpusPacket(20, awTOC20ms), Duration: 600}
	if g := granule(cfg, packets(full)); g != 3*960 {
		t.Errorf("whole packets: %d", g)
	}
	if g := granule(cfg, packets(short)); g != 2*960+600 {
		t.Errorf("a last sample of 600: %d", g)
	}
	// A duration in milliseconds is rounded: 19 ms is a whole 20 ms packet, 7 ms
	// is a cut one.
	ms := cfg
	ms.Timescale = 1000
	if g := granule(ms, packets(Sample{Data: full.Data, Duration: 19})); g != 3*960 {
		t.Errorf("19 ms: %d", g)
	}
	if g := granule(ms, packets(Sample{Data: full.Data, Duration: 7})); g != 2*960+7*48 {
		t.Errorf("7 ms: %d", g)
	}
	// A timescale finer than the granule needs no slack.
	fine := cfg
	fine.Timescale = 1000000
	if g := granule(fine, packets(Sample{Data: full.Data, Duration: 12500})); g != 2*960+600 {
		t.Errorf("12.5 ms in microseconds: %d", g)
	}
	none := cfg
	none.Timescale = 0
	if g := granule(none, packets(Sample{Data: full.Data, Duration: 5})); g != 3*960 {
		t.Errorf("no timescale: %d", g)
	}
	// The edit list ends the presentation: an edit from the pre-skip for 2400
	// samples ends at 2712 of the 2880 decoded.
	edit := cfg
	edit.EditList = &EditList{MovieTimescale: 48000, Edits: []Edit{{SegmentDuration: 2400, MediaTime: 312, Rate: NormalRate}}}
	if g := granule(edit, packets(full)); g != 2712 {
		t.Errorf("edit list: %d", g)
	}
	// Edit lists the granule cannot state are left to the packets.
	for name, el := range map[string]*EditList{
		"two edits":    {MovieTimescale: 48000, Edits: []Edit{{SegmentDuration: 1, Rate: NormalRate}, {SegmentDuration: 1, Rate: NormalRate}}},
		"no scale":     {Edits: []Edit{{SegmentDuration: 100, Rate: NormalRate}}},
		"dwell":        {MovieTimescale: 48000, Edits: []Edit{{SegmentDuration: 100}}},
		"empty edit":   {MovieTimescale: 48000, Edits: []Edit{{SegmentDuration: 100, MediaTime: -1, Rate: NormalRate}}},
		"to the end":   {MovieTimescale: 48000, Edits: []Edit{{MediaTime: 312, Rate: NormalRate}}},
		"past the end": {MovieTimescale: 48000, Edits: []Edit{{SegmentDuration: 999999, Rate: NormalRate}}},
	} {
		c := cfg
		c.EditList = el
		if g := granule(c, packets(full)); g != 3*960 {
			t.Errorf("%s: %d", name, g)
		}
	}
	noTimescale := cfg
	noTimescale.Timescale = 0
	noTimescale.EditList = edit.EditList
	if g := granule(noTimescale, packets(full)); g != 3*960 {
		t.Errorf("an edit list in no timescale: %d", g)
	}
}

func TestOggRefusals(t *testing.T) {
	opus := awOpusConfig(t)
	add := func(cfg TrackConfig) error {
		_, err := NewAudioWriter(FormatOgg, io.Discard).AddTrack(cfg)
		return err
	}
	bad := opus
	bad.CodecConfig = []byte("OpusHead")
	if err := add(bad); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("a short OpusHead: %v", err)
	}

	vcfg, vsamples := awTrack(t, "sine.ogg")
	packets, err := vorbisHeaderPackets(vcfg.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	with := func(h ...[]byte) TrackConfig {
		c := vcfg
		c.CodecConfig = xiphLaced(h...)
		return c
	}
	clone := func(p []byte, edit func([]byte)) []byte {
		q := append([]byte(nil), p...)
		edit(q)
		return q
	}
	for name, c := range map[string]struct {
		cfg  TrackConfig
		want string
	}{
		"no headers":     {func() TrackConfig { c := vcfg; c.CodecConfig = nil; return c }(), "three headers"},
		"not laced":      {func() TrackConfig { c := vcfg; c.CodecConfig = []byte{9, 1, 2}; return c }(), "Xiph"},
		"two headers":    {with(packets[0], packets[1]), "headers, not 2"},
		"a wrong header": {with(packets[0], packets[0], packets[2]), "type 3"},
		"a short header": {with(packets[0], packets[1][:3], packets[2]), "type 3"},
		"a broken ID":    {with(clone(packets[0], func(p []byte) { p[7] = 1 }), packets[1], packets[2]), "identification header"},
		"no mode table":  {with(packets[0], packets[1], append([]byte("\x05vorbis"), 0, 0, 0, 0)), "framing bit"},
	} {
		err := add(c.cfg)
		if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want ErrTrackConfig naming %q", name, err, c.want)
		}
	}
	// A stub unlacer stands for one that stops part way, which ebml-go's never does.
	saved := unlaceVorbisHeaders
	defer func() { unlaceVorbisHeaders = saved }()
	failure := errors.New("stopped part way")
	unlaceVorbisHeaders = func(io.Reader, int64) (ebml.Unlacer, error) { return webmStubUnlacer{err: failure}, nil }
	if err := add(vcfg); !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), failure.Error()) {
		t.Errorf("headers that stop short: %v", err)
	}
	unlaceVorbisHeaders = func(io.Reader, int64) (ebml.Unlacer, error) { return nil, failure }
	if err := add(vcfg); !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), failure.Error()) {
		t.Errorf("headers that cannot be unlaced: %v", err)
	}
	unlaceVorbisHeaders = saved

	fcfg, fsamples := awTrack(t, "sine.flac")
	fcfg.CodecConfig = nil
	if err := add(fcfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("FLAC without STREAMINFO: %v", err)
	}

	// Samples the mapping cannot step over.
	step := func(cfg TrackConfig, data []byte) error {
		a := NewAudioWriter(FormatOgg, io.Discard)
		id, err := a.AddTrack(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return a.WriteSample(id, Sample{Data: data})
	}
	if err := step(opus, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("an empty Opus packet: %v", err)
	}
	if err := step(vcfg, nil); !errors.Is(err, ErrSample) {
		t.Errorf("an empty Vorbis packet: %v", err)
	}
	if err := step(vcfg, []byte{1, 2, 3}); !errors.Is(err, ErrSample) {
		t.Errorf("a Vorbis header as audio: %v", err)
	}
	// A setup header of three modes names two bits of mode, and the fourth is no mode.
	setup := []byte("\x05vorbis")
	setup = append(setup, make([]byte, 8)...)
	var bitsOut []byte
	put := func(v uint32, n int) {
		for i := range n {
			bitsOut = append(bitsOut, byte(v>>i&1))
		}
	}
	put(2, 6)
	for range 3 {
		put(0, 1+16+16+8)
	}
	put(1, 1)
	for len(bitsOut)%8 != 0 {
		bitsOut = append(bitsOut, 0)
	}
	for i := 0; i < len(bitsOut); i += 8 {
		var b byte
		for j := range 8 {
			b |= bitsOut[i+j] << j
		}
		setup = append(setup, b)
	}
	threeModes := with(packets[0], packets[1], setup)
	if err := step(threeModes, []byte{0x06}); !errors.Is(err, ErrSample) || !strings.Contains(err.Error(), "mode") {
		t.Errorf("a Vorbis mode that is not in the setup header: %v", err)
	}
	if err := step(fcfg2(t), fsamples[0].Data[:3]); !errors.Is(err, ErrSample) {
		t.Errorf("a FLAC packet that is not a frame: %v", err)
	}
	_ = vsamples
}

func fcfg2(t *testing.T) TrackConfig {
	cfg, _ := awTrack(t, "sine.flac")
	return cfg
}
