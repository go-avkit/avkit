# go-avkit

[![ci](https://github.com/go-avkit/avkit/actions/workflows/ci.yml/badge.svg)](https://github.com/go-avkit/avkit/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-avkit/avkit.svg)](https://pkg.go.dev/github.com/go-avkit/avkit)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

Pure-Go (CGO=0) audio/video toolkit. It reads and writes time-based media
containers with no `libav`/`ffmpeg` linkage and no external binaries, which is
enough to remux — to move tracks between containers without re-encoding them.

Container parsing is delegated to the maintained reference libraries
([Eyevinn/mp4ff](https://github.com/Eyevinn/mp4ff) for ISO-BMFF,
[at-wat/ebml-go](https://github.com/at-wat/ebml-go) for EBML,
[asticode/go-astits](https://github.com/asticode/go-astits) for MPEG-TS);
go-avkit projects their box/element/packet trees onto one small, format-neutral
model, and converts the payloads each container spells differently.

## Packages

| Package     | What it does |
|-------------|--------------|
| `container` | Sniff and demux MP4/ISO-BMFF, Matroska/WebM and MPEG-TS — and, demux only, ADTS, native FLAC, Ogg and WAV — into a unified `File`/`Track` model (kind, codec, dimensions, channels, timing); read the samples and per-track configuration of an MP4 or a transport stream, one file or a sequence of segments; mux a fragmented or a progressive MP4, or an MPEG-TS, from tracks delivered separately; carry WebVTT, SRT and TTML subtitle tracks between MP4 and Matroska/WebM; copy, cut, join, concatenate and drop tracks with `Remux`, `Cut`, `Join` and `Concat`; and rewrite an HEVC track's sample entry to `hvc1` with `ConformHVC1`. No re-encoding anywhere. |

### Why `ConformHVC1` is there

An HEVC track in an MP4 may declare itself `hev1` or `hvc1`. The two differ in
where the parameter sets live — `hvc1` carries them in the sample entry,
`hev1` allows them in the stream — and **macOS refuses `hev1`**: measured over a
library of 1807 HEVC files, QuickLook produced a thumbnail for every `hvc1` file
and for none of the `hev1` ones, while ffmpeg decoded both without complaint.

`ConformHVC1` moves the sets into the entry and rewrites the four-character code,
which is a change of container and not of pictures: the samples are the same
bytes. `CheckHVC1` answers the question without changing anything, so a caller
can tell a file that needs it from one that does not.

⛔ It takes the **stream** as authority wherever the samples state a set, keeps
the entry's sets where they do not, and **refuses** a stream whose sets change
part way through — because a single entry cannot describe two of them, and
silently keeping the first would produce a file that decodes correctly until it
does not.

## Functional coverage

What this reads and writes, measured from the code rather than remembered. The
table is held by `TestTheCoverageOfConfigFromSamplesIsWhatTheREADMESays`, so
adding a codec to the dispatch without adding it here fails the suite.

### Containers

| | sniff | demux | mux |
|---|---|---|---|
| MP4 / ISO-BMFF | ✓ | ✓ one file, a sequence of segments, or fragmented | ✓ fragmented **and** progressive |
| Matroska / WebM | ✓ | ✓ | ✓ |
| MPEG-TS | ✓ | ✓ | ✓ |
| ADTS (raw AAC) | ✓ | ✓ | — |
| native FLAC | ✓ | ✓ | — |
| Ogg | ✓ | ✓ Opus, Vorbis and FLAC streams; Theora, Speex, VP8 and Kate named, not read | — |
| WAV (RIFF, RF64, BW64) | ✓ | ✓ linear PCM; other format tags named, not read | — |

The last four are read whole, in one pass, by `Demux` and `NewReader` (not by
`NewFileReader`, which serves MP4 alone), and are never written. Nothing else: no
AVI, no FLV, no MP3 elementary stream yet, no MXF, and no HLS or DASH manifests.
A file that is none of these sniffs as `FormatUnknown` rather than being guessed
at.

#### How the one-pass formats are read

- **ADTS** (ISO/IEC 13818-7 §6.2, 14496-3 §1.A.2): each header is stripped and
  the first becomes the `AudioSpecificConfig`; a frame of several raw data blocks
  is refused, because an MP4 sample is one block and only a decoder can find
  where they part. Channel configuration 0 reads, and its `TrackConfig` says why
  it cannot be written.
- **FLAC** (RFC 9639): `CodecConfig` is the marker and STREAMINFO, Matroska's
  `A_FLAC` CodecPrivate. Frames are found where a header's CRC-8 holds, the frame
  before it closes with a CRC-16 of zero, and the frame or sample number follows on.
- **Ogg** (RFC 3533; RFC 7845; Vorbis I §A; RFC 9639 §10.1): page CRCs are
  checked, packets reassembled across pages, and each packet timed from its own
  header — the Opus TOC, the Vorbis mode, the FLAC block size. Vorbis's mode table
  is found by reading the setup header backwards, as FFmpeg does, and **checked
  against the granule positions**, which are normative. The last packet is cut to
  the last granule. Opus pre-skip is `TrackConfig.PreSkip`.
- **Streams around frames**: an ID3v2 tag at the head or between frames, an
  ID3v1 or APEv2 tag at the tail, and resynchronisation over rubbish — but only
  onto a frame that is followed by another one.
- **A stream cut short** is read as far as its last whole frame, page or block,
  and `ErrTruncated` comes back **beside** the reader, as for Matroska.

Every packet of the fixtures in `container/testdata/sine.*` is compared with what
FFmpeg's own demuxer hands over (`-c copy -f framemd5`, stored beside each). The
one disagreement is FFmpeg's: it gives this Vorbis file's last packet a duration
of −192, and the test holds the granule instead.

### Per-track configuration, from the samples

`ConfigFromSamples` has **three** answers, and they are not two:

| | codecs | what it means |
|---|---|---|
| **derived from the samples** | `avc1`, `avc3`, `hvc1`, `hev1`, `av01`, `vp09`, `mp4a` | the elementary stream states its own configuration, and it is read out of it |
| **stated by the container** | `vp08`, `opus`, `ac-3`, `ec-3`, `wvtt`, `stpp` | `ErrNotInSamples`. The codec is known and its samples **cannot** describe it — VP8 defines no level, an Opus stream carries no identification header, an AC-3 sync frame's bit stream information is not read here, a WebVTT cue never repeats the file header and a TTML document never states its sample entry. Take the configuration from the container, which has it |
| **not known** | everything else | `ErrUnsupportedCodec` alone. There is nothing to take |

⛔ The middle row used to be indistinguishable from the last: both carried
`ErrUnsupportedCodec`, so a caller could not tell "take it from the container"
from "give up". `ErrNotInSamples` **wraps** the older sentinel, so code written
before it existed still matches.

### Tracks a container can carry

| | video | audio | subtitles |
|---|---|---|---|
| MP4 demux | any — the four-character code is read from the sample entry, not from a list | any | any; the configuration of `wvtt` and `stpp` |
| MP4 mux | `avc1`, `hvc1`, `av01`, `vp08`, `vp09`, `mjpg` | `mp4a`, `opus`, `ac-3`, `ec-3` | `wvtt`, `stpp` |
| Matroska demux | AVC, HEVC, AV1, VP8, VP9 | AAC, AC-3, E-AC-3, FLAC, Opus, Vorbis | WebVTT (`S_TEXT/WEBVTT`, `D_WEBVTT/*`) and SRT (`S_TEXT/UTF8`), both as `wvtt` |
| WebM mux | AV1, VP8, VP9 | AAC, Opus, Vorbis | `wvtt`, as `D_WEBVTT/SUBTITLES` or `S_TEXT/WEBVTT` |
| MPEG-TS | `avc1`, `hvc1`, `av01`, in and out | `mp4a`, `ac-3`, `ec-3`, `mp1`, `mp2`, `mp3`, in and out; **not** `opus` (see [MPEG-TS carriage](#mpeg-ts-carriage)) | none, in or out |
| ADTS demux | — | `mp4a` | — |
| FLAC demux | — | `fLaC` | — |
| Ogg demux | — | `Opus`, `vorb`, `fLaC` | — |
| WAV demux | — | `ipcm` (signed integer), `fpcm` (float), `raw ` (unsigned 8-bit) | — |

See [Subtitles](#subtitles) for what each conversion keeps.

PCM is reported under the ISO/IEC 23003-5 entries with its `pcmC` record as
`CodecConfig`, and refused by the MP4 muxer **by name**: mp4ff does not write
those entries yet. FLAC and MP3 into MP4 and Matroska are lot L2 of
[#54](https://github.com/go-avkit/avkit/issues/54).

### What is deliberately absent

**No re-encoding, anywhere, and no decoding.** Nothing here turns a sample into
pixels or into audio. Remuxing, cutting, joining and concatenating move the same
sample bytes between containers; a codec that cannot be carried is refused
rather than transcoded.

The bitstream syntax itself lives in sibling modules —
[`h264`](https://github.com/go-avkit/h264) (and the derivations of clause 8.2),
[`h265`](https://github.com/go-avkit/h265), [`vp8`](https://github.com/go-avkit/vp8),
[`vp9`](https://github.com/go-avkit/vp9),
[`boolcoder`](https://github.com/go-avkit/boolcoder) and
[`bitstream`](https://github.com/go-avkit/bitstream). **There is no AAC, MPEG
audio, Opus, FLAC, AC-3 or AV1 bitstream reader** in the organisation: those
codecs are handled here only as far as a container needs them — an MPEG audio
or AC-3 frame header, for instance, is read for the length of the frame and
nothing more.

## Install

```sh
go get github.com/go-avkit/avkit
```

## Usage

```go
package main

import (
	"fmt"
	"os"

	"github.com/go-avkit/avkit/container"
)

func main() {
	data, err := os.ReadFile("clip.mp4") // or .mkv / .webm
	if err != nil {
		panic(err)
	}

	f, err := container.Demux(data)
	if err != nil {
		panic(err)
	}

	fmt.Printf("%s, %.3fs, %d track(s)\n", f.Format, f.DurationSeconds(), len(f.Tracks))
	for _, t := range f.Tracks {
		fmt.Printf("  #%d %s %s %dx%d %dch/%dHz %.3fs %q\n",
			t.ID, t.Kind, t.Codec, t.Width, t.Height, t.Channels, t.SampleRate,
			t.DurationSeconds(), t.Language)
	}
}
```

`container.Sniff` identifies the format from the leading bytes without a full
parse; `container.Demux` dispatches to the right demuxer.

## MP4 files mp4ff refuses on its own

Two shapes that ISO/IEC 14496-12 allows, and that `ffprobe` reads, are refused by
mp4ff. go-avkit reads them, the same way through `Demux`, `NewReader`,
`NewFileReader` and `OpenFile`:

- **Several adjacent `mdat` boxes.** A writer that flushes a recording in pieces
  leaves its media in two or three boxes, one after the other. Samples are found
  by absolute offset, so the boundaries mean nothing to a reader, and mp4ff is
  shown the run as one box. Only the 4-byte size field is changed, and only in a
  view of the file. The file itself is not written to, and samples are still
  read from it. Boxes with something between them, a run too long for a 32-bit
  size, and the mdat boxes of a fragmented file are still refused.
- **Bytes after a complete movie box**: a stray byte, zero padding, or a header
  announcing a box that is not there. The movie box says where every sample is,
  so what follows it describes nothing a reader needs.

Measured over 2375 MP4 files: 17 are refused. 16 of them `ffprobe` refuses too
(no movie box), and the last one is an AVI file with a `.mp4` name.

Edit lists (`elst`) are not applied: samples come back in decode order, once
each, as the sample tables list them.

## Encoder priming survives a remux

An AAC encoder emits a frame before the media proper — about 21 ms at 48 kHz —
which a decoder consumes and does not present. Matroska states it as
`CodecDelay`, an MP4 as an edit list, and a remux that dropped it handed the
output a frame the input never presented: audio and video parted company by that
much, and the output was lossless for video only.

`TrackConfig.StartDelay` now carries it. Readers fill it in from whichever form
the source used, `Muxer.AddTrack` writes an edit list for it, and a caller who
knows better can overwrite it. Nothing has to be asked for.

It is not `PreSkip`. `PreSkip` is Opus stating its own priming inside its
identification header, and it travels with the codec configuration wherever the
track goes. `StartDelay` is what the **container** says, for any codec, and it
has to be rewritten into the form the output container uses.

Both directions are covered, since a fix for only one of them would be half a
fix: a Matroska `CodecDelay` becomes an `elst` on the way out, and an MP4's
`elst` is read back on the way in.

**The precision is the output track's timescale.** Matroska counts in
milliseconds by default and that timescale is carried over, so a 21.333 ms
priming is stated as 21 — rounded to the nearest tick, not down. That is a third
of a millisecond lost where a whole 21.333 ms frame was gained before.

Only one shape of edit list is read: a single entry starting at a positive media
time at rate 1, which is what priming looks like. Several entries, an empty edit
or another rate describe a presentation this package does not reproduce, and
reading a delay out of one would state something the output does not do.

## AC-3 and Enhanced AC-3 out of Matroska

Matroska usually carries **no `CodecPrivate` for AC-3 or E-AC-3** — those codecs
state their configuration inside every frame instead. So a track read from one
arrived with nothing an MP4 sample entry could be built from, and the muxer
refused it: `a dec3 record is at least 5 bytes, not 0`.

The sync frame is now read, and the `dac3` or `dec3` record built from what it
says: sample rate code, bit stream identification and mode, channel mode, the
low-frequency channel, and the rate. `Remux` and the operations beside it fill
it in through the same `describeFromSamples` that serves VP9.

⛔ Which syntax follows the sync word is decided by a field **29 bits past it**:
a bit stream identification above ten is Enhanced AC-3, whose header shares
nothing with AC-3's beyond those first sixteen bits. It is read ahead without
consuming, as the format requires.

⛔ The frame-length table is **not a formula**. At 48 kHz and 32 kHz the length
is proportional to the rate; at 44.1 kHz it is not, and two codes of the same
rate differ by one word because a frame there cannot hold a whole number of
samples. It is transcribed, not computed.

Only what the two records need is read. This is not a decoder, and a dependent
substream — which describes part of another stream rather than a track — is
passed over in favour of the independent one that states the track.

## MPEG-TS carriage

Every codec a transport stream carries has its own way of saying so, and each is
read and written as the text that defines it says:

| codec | read | written | defined by |
|---|---|---|---|
| AC-3 (`ac-3`) | stream type `0x81`; or private data `0x06` with the DVB `AC-3_descriptor` (`0x6A`) or the registration descriptor `"AC-3"` | `0x81`, registration `"AC-3"`, PES `stream_id` `0xBD` | ATSC A/52:2012 Annex A §3, §4.1, §4.2, §5; ETSI EN 300 468 V1.17.1 Annex D.2–D.3 |
| Enhanced AC-3 (`ec-3`) | `0x87`; or `0x06` with the `enhanced_AC-3_descriptor` (`0x7A`) or registration `"EAC3"` | `0x87`, registration `"AC-3"`, `stream_id` `0xBD` | A/52 Annex G §2, §3.1–§3.3; EN 300 468 Annex D.4–D.5 |
| MPEG audio (`mp1`, `mp2`, `mp3`) | `0x03` or `0x04` | `0x03` at 32/44.1/48 kHz, `0x04` at 16/22.05/24 kHz, `stream_id` `0xC0` | ISO/IEC 13818-1 Table 2-22 and 2-34; ISO/IEC 11172-3 and 13818-3 §2.4 for the frame header |
| AV1 (`av01`) | `0x06` with registration `"AV01"`, in the start-code form or as raw OBUs | `0x06`, registration `"AV01"`, then the AV1 video descriptor (`0x80`), `stream_id` `0xBD`, every OBU behind a start code and escaped | AOM *Carriage of AV1 in MPEG-2 TS*, Working Group Draft of 25 March 2026, §2.1, §2.2, §3.1–§3.4 |

**One convention is written for AC-3: ATSC's System A**, which is also what
FFmpeg's `mpegtsenc.c` writes unless told `-mpegts_flags system_b`. The DVB form
is read, and a DVB stream copied through `TSMuxer` comes out in the ATSC form.

⛔ For Enhanced AC-3, FFmpeg writes the registration descriptor `"EAC3"` next to
stream type `0x87`. A/52 Annex G §2 says otherwise: System A extends **the AC-3
registration descriptor** to Enhanced AC-3 "in combination with the E-AC-3
stream_type", and keeps `"EAC3"` for systems that have no stream type of their
own. This package writes what the standard says. Both FFmpeg and this package
decide by the stream type first, so both forms read in both.

⛔ astits chooses a PES `stream_id` from the stream type when it is left at zero,
and gets two of them wrong for this purpose: MPEG-1 audio (`0x03`) gets
`private_stream_1`, and AC-3 (`0x81`) gets `0xFD`, the extended stream
identifier, which needs a PES extension astits does not write. The identifier is
therefore always stated.

⛔ **A PES unit need not hold whole frames.** ISO/IEC 13818-1 (2.4.3.7) has a
unit's PTS refer to the first access unit that begins in it, which only means
something because a unit may also hold the end of the one before. FFmpeg always
aligns them, so a reader assuming it would pass every test made from FFmpeg's
output. A frame that straddles two units is carried over and timed from the unit
it began in.

An Enhanced AC-3 frame of a dependent substream (or of an independent substream
other than the first) joins the frame before it: A/52 Annex G §3.3 makes the
access unit every substream of one period, and an MP4 `ec-3` sample is that
access unit. A `dac3` or `dec3` record is built from the first frame, as it is
for Matroska, so a transport stream's AC-3 goes straight into an MP4.

**The frames, not the program map, name the codec.** Stream types `0x03` and
`0x04` do not say which layer they carry (FFmpeg names both `mp3` until its
parser has seen a frame, and so does this until it has read one), and an
E-AC-3 stream under stream type `0x81` is named `ec-3`. MPEG audio in an MP4 is
`mp4a` with object type indication `0x6B` or `0x69` (MP4RA), which the MP4
muxers do not write yet: they refuse `mp1`, `mp2` and `mp3` by saying so.

**AV1 follows a draft**, and the draft says it should be cited only as one. It
is still the only text that defines the carriage, and FFmpeg neither writes nor
reads AV1 in a transport stream: `ffprobe` probes such a stream and names it
VVC, from the start codes. Each OBU is written behind a three-byte start code
with an `emulation_prevention_three_byte` after any two zero bytes followed by 0
to 3 (§3.2), every OBU stating its size (§3.1 recommends it "for applications
that need easy conversion to MP4"). ⛔ The draft does not say what happens where
an OBU ending in zero bytes meets the next start code; such an OBU is closed with
`0x03`, as H.264 closes an RBSP, and a reader honouring `obu_size` never sees
it. The AV1 video descriptor copies the `av1C` record's profile, level, tier and
colour bytes and states `hdr_wcg_idc` 3, "no indication", since an `av1C` does
not say. Reading drops temporal delimiters, which an MP4 sample SHOULD NOT hold
(AV1-ISOBMFF §2.4), and gives an OBU that states no size a size field. A unit
that does not begin with a start code is read as raw OBUs, which is what this
package read before it wrote the carriage.

**Opus is refused, deliberately.** The only text that maps it onto a transport
stream is a draft ETSI specification, *TS 1xx xxx V0.1.3 (2015-10)*, published
by Xiph and never adopted. FFmpeg implements it (`opus_control_header`, the
`"Opus"` registration descriptor and a DVB extension descriptor); this package
writes what a published specification defines, and an Opus stream is not read
either.

## VP9 out of Matroska

Matroska states no VP9 profile and no level: both live in the frame header, and
there is no `CodecPrivate` to carry them. `vpcC` has no value for "unknown" —
zero is a real profile but not a real level — so remuxing such a track used to
fail with `vp09 needs a level; 0 is not one`.

`Remux`, `Cut`, `Concat` and `Join` now read the configuration **out of the
frames** when the container could not state it, through the same
`ConfigFromSamples` a caller can use directly.

The reader is unchanged and still reports only what the container says. That is
deliberate: a level could also be derived from the container's width, height and
frame rate, and it would be a worse description — the profile would stay at zero
and the colour at "unspecified", where the frame header states both. The
container is not made to claim knowledge it does not have.

A VP9 track whose samples cannot describe it either is still refused, naming the
track, rather than written with a `vpcC` of zeroes.

## A Matroska file that ends before its structure does

A download in progress, or one that stopped, is a Matroska file cut mid-cluster.
It used to be refused whole — `unexpected EOF`, and nothing of it readable —
which costs essentially the entire file: `ffmpeg` says `File ended prematurely`
and still remuxes 16 645 456 bytes of a 16 777 216-byte one.

`NewReader` now reads what came before the cut and returns it **together with**
`ErrTruncated`:

```go
r, err := container.NewReader(data)
if err != nil && !errors.Is(err, container.ErrTruncated) {
    return err          // anything else is still fatal
}
// r is usable; r.Truncated() is true
```

A caller that writes the ordinary `if err != nil { return err }` keeps the old
refusal **exactly**, which is why the sentinel comes with the reader rather than
instead of it: the safe behaviour stays the default and the useful one is one
line away.

Measured on a 120-sample file of many clusters, cut at a byte boundary:

| cut at | samples kept |
|---|---|
| 90% | 120 / 120 |
| 75% | 105 / 120 |
| 50% | 69 / 120 |
| 25% | 33 / 120 |

**The strictness is not traded away.** ebml-go has an ignore-unknown mode that
swallows read and size errors and returns whatever it had — a truncated file
would read as a *short* one, with a duration and a sample count that are simply
wrong and nothing downstream able to tell. That mode stays off. Instead the file
is cut at the last complete element boundary and **that prefix is parsed
strictly**, so a parse that succeeds still says the bytes really were well
formed. A cluster written with an unknown size — the normal shape of anything
recorded live — is walked from the inside, which is what makes the loss
proportionate to the cut rather than total.

`ErrTruncated` does not claim the file was cut cleanly: a file damaged in the
middle stops the walk at the damage and reports the same thing, because from
here the two are indistinguishable.

`Remux`, `Cut`, `Concat` and `Join` **refuse** a truncated input, since the
output would be an ordinary file with nothing in it saying media is missing from
the end. `AllowTruncated()` says to write it anyway.

## Subtitles

Text tracks used to be dropped. They are now carried, in one form: whatever a
track came from, `Reader` hands it back as the samples of an ISO/IEC 14496-30
WebVTT track (`wvtt`), and every muxer that writes subtitles takes that form.

- **A `wvtt` sample** covers a stretch of time and holds the cues shown during
  it — `vttc` boxes, each with its `payl` text and its optional `iden`
  identifier and `sttg` settings — or one `vtte` box when nothing is shown. The
  samples follow one another with no gap, which is what lets a subtitle track
  be interleaved, cut and joined like any other.
- **Matroska / WebM in.** The WebM mapping `D_WEBVTT/*` (identifier, settings
  and text in the block, the form ffmpeg writes even in `.mkv`), the Matroska
  mapping `S_TEXT/WEBVTT` (the file header in `CodecPrivate`; settings,
  identifier and `NOTE` comments in a `BlockAddition` of ID 1) and SRT
  (`S_TEXT/UTF8`, whose text goes in as it stands: its `<b>`, `<i>`, `<u>` are
  WebVTT's). Each block is a cue timed by its `BlockDuration` — or the track's
  `DefaultDuration` — and a cue without either is refused, not guessed.
  ⛔ Overlapping cues are legal there and not in a `wvtt` track, so the timeline
  is cut wherever a cue starts or ends, and a cue shown across several samples
  is repeated in each with a `vsid` saying the copies are one cue.
- **MP4 out.** `Muxer` and `ProgressiveMuxer` write the `wvtt` sample entry with
  the header in `vttC`, under the `text` handler and a null media header, as
  14496-30 requires.
- **WebM / Matroska out.** `WebMMuxer` puts the pieces of a split cue back
  together and writes each cue as a block group stating its duration.
  ⛔ It uses the WebM mapping `D_WEBVTT/SUBTITLES` while that loses nothing —
  it is the only one ffmpeg reads (9.0.2 has no `S_TEXT/WEBVTT` at all) and
  the only one a WebM file may hold — which is when the header is the bare
  `WEBVTT` signature. A header with a `STYLE` or `REGION` block has nowhere to
  go there, so such a track is written in the Matroska mapping and the file
  becomes `matroska`. The WebM mapping has no room for `NOTE` comments either,
  and drops them; nothing renders them.
- **Timestamps inside a cue's text** (`<00:03:15.000>`) count from the start
  of the presentation in an MP4 and, the Matroska mapping says, from the block
  in `S_TEXT/WEBVTT`. They are rewritten between the two, and left as they
  stand in `D_WEBVTT`, as ffmpeg does.
- **TTML (`stpp`)** goes from MP4 to MP4 with its sample entry and its
  documents unchanged. It has no Matroska mapping here and is refused by
  `WebMMuxer`.
- **`Cut`** starts a subtitle track exactly where the pictures start: the
  sample the cut falls in is shortened, not snapped back to, since a silence
  between two lines is one sample and snapping would show every later cue early
  by its length. **`Concat`** ends each input's subtitle track where its longest
  track ends, so the next input's cues follow the pictures and not the last cue.
  ⛔ Both refuse to move a TTML track, or a WebVTT cue whose text states times of
  its own, away from zero: the times inside would not move with it.
  `DropTracks` leaves such a track out.
- **MPEG-TS** carries no subtitles here, in either direction. `TSMuxer` says so
  rather than calling the codec unknown.

Not carried: SSA/ASS, bitmap subtitles (VobSub, PGS, DVB), 3GPP `tx3g`, the
experimental `wvtc`/`stpc`, and the distinction between WebM's captions,
descriptions and metadata kinds, which all read as subtitles.

## Guarantees

- **Pure Go, CGO=0.** No `libav`, no `exec` to `ffmpeg`.
- **100% statement coverage**, enforced in CI, error branches included.
- **Six 64-bit targets** exercised each run: `amd64`, `arm64` (native) and
  `riscv64`, `loong64`, `ppc64le`, `s390x` (under qemu) — the last also covering
  big-endian byte-order correctness.

## Scope

Today go-avkit reads container *structure and metadata*. Codec bitstream
decoding (H.264/HEVC/AV1/VP9, AAC/Opus …) lives in sibling packages as they land.

## License

BSD-3-Clause — see [LICENSE](LICENSE). Copyright the go-avkit authors.
