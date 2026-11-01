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
| `container` | Sniff and demux MP4/ISO-BMFF, Matroska/WebM and MPEG-TS — and, demux only, ADTS, MPEG audio, native FLAC, Ogg, WAV and AVI — into a unified `File`/`Track` model (kind, codec, dimensions, channels, timing); read the samples and per-track configuration of any of them, one file or (MP4, MPEG-TS) a sequence of segments; mux a fragmented or a progressive MP4, a Matroska/WebM file, or an MPEG-TS, from tracks delivered separately; carry WebVTT, SRT and TTML subtitle tracks between MP4 and Matroska/WebM; copy, cut, join, concatenate and drop tracks with `Remux`, `Cut`, `Join` and `Concat`; read and write file metadata and cover art; and rewrite an HEVC track's sample entry to `hvc1` with `ConformHVC1`. No re-encoding anywhere. |

### Why `ConformHVC1` is there

An HEVC track in an MP4 may declare itself `hev1` or `hvc1`. The two differ in
where the parameter sets live — `hvc1` carries them in the sample entry,
`hev1` allows them in the stream — and **macOS refuses `hev1`**: measured over a
library of 1807 HEVC files, QuickLook produced a thumbnail for every `hvc1` file
and for none of the `hev1` ones, while ffmpeg decoded both without complaint.

`ConformHVC1` moves the sets into the entry and rewrites the four-character code,
which is a change of container and not of pictures — but it does change the
samples' bytes when they carry sets. Every VPS, SPS and PPS NAL unit is taken out
of every sample, because `hvc1` forbids them there; every other unit (slices,
SEI, delimiters) keeps its bytes and its order, and each sample is written back
as those units behind four-byte lengths. A sample that carried no set and was
already in that form comes out byte for byte the same — every sample of 60 of
the 67 `hev1` files measured for `CheckHVC1`, whose documentation records them;
a sample holding nothing but sets is refused rather than written empty. `CheckHVC1` answers the question without
changing anything, so a caller can tell a file that needs it from one that does
not.

It is an option of `Remux`, `Cut`, `Concat`, `Join` and `JoinProgressive`
alike, so a copy, a cut or a join is made conformant in the same pass.

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
| MP4 / ISO-BMFF | ✓ | ✓ one file, a sequence of segments, or fragmented, with or without `sidx` | ✓ fragmented (optionally indexed by `sidx`) **and** progressive |
| Matroska / WebM | ✓ | ✓ | ✓ with Cues, and a SeekHead when buffered or the writer can seek |
| MPEG-TS | ✓ | ✓ | ✓ |
| ADTS (raw AAC) | ✓ | ✓ | — |
| MPEG audio (MP3, MP2, MP1) | ✓ | ✓ the Xing/Info/VBRI frame dropped, the LAME encoder delay and end padding kept | — |
| native FLAC | ✓ | ✓ | — |
| Ogg | ✓ | ✓ Opus, Vorbis and FLAC streams; Theora, Speex, VP8 and Kate named, not read | — |
| WAV (RIFF, RF64, BW64) | ✓ | ✓ linear PCM; other format tags named, not read | — |
| AVI (with OpenDML `AVIX` forms) | ✓ | ✓ H.264, MPEG-4 Part 2, Motion JPEG; PCM, MP3/MP2, AC-3, AAC; other FourCCs and tags named, not read | — |

The last six are read whole, in one pass, by `Demux` and `NewReader` (not by
`NewFileReader`, which serves MP4 alone), and are never written. Nothing else: no
FLV, no MXF, and no HLS or DASH manifests.
A file that is none of these sniffs as `FormatUnknown` rather than being guessed
at.

#### How the one-pass formats are read

- **ADTS** (ISO/IEC 13818-7 §6.2, 14496-3 §1.A.2): each header is stripped and
  the first becomes the `AudioSpecificConfig`; a frame of several raw data blocks
  is refused, because an MP4 sample is one block and only a decoder can find
  where they part. Channel configuration 0 reads, and its `TrackConfig` says why
  it cannot be written.
- **MPEG audio** (ISO/IEC 11172-3 and 13818-3 §2.4.1.3): frames are cut by the
  header `parseMPAFrame` reads, as the transport stream reader does. The first
  frame is dropped when it is a Xing, Info or VBRI frame, which describes the
  stream and holds no audio; the LAME extension's encoder delay, plus the
  decoder's 529 samples, becomes `TrackConfig.StartDelay`, so a remux keeps the
  priming out of what is presented. Its end padding, when the tag also states a
  frame count that is the stream's own, becomes `TrackConfig.EditList`: one edit
  from that priming to where FFmpeg's `mp3dec.c` stops presenting (frames × the
  samples of a frame − padding + 529), which is the edit FFmpeg's `movenc` writes for the same file,
  and which `WebMMuxer` writes as the last block's `DiscardPadding`, FFmpeg's
  Matroska form of it. A 1 s `libmp3lame` file decoded as 44 975 samples once
  copied to MP4 or Matroska, and decodes as its own 44 100 now. The unofficial
  "MPEG 2.5" rates and free format are not read.
- **FLAC** (RFC 9639): `CodecConfig` is the marker and STREAMINFO, Matroska's
  `A_FLAC` CodecPrivate. Frames are found where a header's CRC-8 holds, the frame
  before it closes with a CRC-16 of zero, and the frame or sample number follows on.
- **Ogg** (RFC 3533; RFC 7845; Vorbis I §A; RFC 9639 §10.1): page CRCs are
  checked, packets reassembled across pages, and each packet timed from its own
  header — the Opus TOC, the Vorbis mode, the FLAC block size. Vorbis's mode table
  is found by reading the setup header backwards, as FFmpeg does, and **checked
  against the granule positions**, which are normative. The last packet is cut to
  the last granule. Opus pre-skip is `TrackConfig.PreSkip`.
- **AVI** (Microsoft AVI RIFF reference; OpenDML 1.02): the chunks of every
  `movi` list are walked in file order, across the `AVIX` forms that continue a
  file past 1 GiB; neither `idx1` nor the OpenDML indexes are read, because a
  reader that walks every chunk has nothing to seek for, and keyframes are read
  from the frames. An empty video chunk lengthens the frame before it. Audio is
  read from its chunks joined, as its own elementary stream would be — PCM as
  WAV, MP3 as MP3, AC-3 frame by frame — except raw AAC, a frame a chunk.
  ⛔ AVI states no presentation time, only an order, so **H.264 with B-slices
  and MPEG-4 Part 2 with B-VOPs are refused**, by name, rather than handed over
  to play out of order. `mp4v` is read and described (its decoder-specific
  information as `CodecConfig`) and no muxer writes it yet.
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
| **derived from the samples** | `avc1`, `avc3`, `hvc1`, `hev1`, `av01`, `vp09`, `mp4a`, `ac-3`, `ec-3`, `mp1`, `mp2`, `mp3` | the elementary stream states its own configuration, and it is read out of it: for AC-3 the `dac3`/`dec3` record from the first sync frame, for MPEG audio the rate and channel count from the frame headers, which must all be of the stated layer and one rate |
| **stated by the container** | `vp08`, `opus`, `flac`, `wvtt`, `stpp`, `tx3g` | `ErrNotInSamples`. The codec is known and its samples **cannot** describe it — VP8 defines no level, an Opus stream carries no identification header, a FLAC frame may leave its rate and bit depth to STREAMINFO (RFC 9639 §9.1.2, §9.1.4), which only the stream header holds, a WebVTT cue never repeats the file header, a TTML document never states its sample entry and a tx3g sample never states the font table or the default style. Take the configuration from the container, which has it |
| **not known** | everything else | `ErrUnsupportedCodec` alone. There is nothing to take |

⛔ The middle row used to be indistinguishable from the last: both carried
`ErrUnsupportedCodec`, so a caller could not tell "take it from the container"
from "give up". `ErrNotInSamples` **wraps** the older sentinel, so code written
before it existed still matches.

### Tracks a container can carry

| | video | audio | subtitles |
|---|---|---|---|
| MP4 demux | any — the four-character code is read from the sample entry, not from a list | any | any; the configuration of `wvtt`, `stpp` and `tx3g` |
| MP4 mux | `avc1`, `hvc1`, `av01`, `vp08`, `vp09`, `mjpg` | `mp4a`, `opus`, `ac-3`, `ec-3`, `fLaC`, and `mp1`/`mp2`/`mp3` as `mp4a`; **not** `vorb` (see [Audio codecs](#flac-mpeg-audio-and-ac-3-in-mp4-and-matroska)) | `wvtt`, `stpp`, `tx3g`; **not** `pgs` (MP4RA registers no sample entry for it) |
| Matroska demux | AVC, HEVC, AV1, VP8, VP9 | AAC, AC-3, E-AC-3, FLAC, MPEG audio (Layers I–III), Opus, Vorbis | WebVTT (`S_TEXT/WEBVTT`, `D_WEBVTT/*`) and SRT (`S_TEXT/UTF8`), both as `wvtt`; PGS (`S_HDMV/PGS`) as `pgs`, its blocks as they stand |
| Matroska / WebM mux | AV1, VP8, VP9 in WebM; AVC, HEVC as DocType `matroska` | Opus, Vorbis in WebM; AAC, FLAC, MPEG audio, AC-3, E-AC-3 as DocType `matroska` | `wvtt`, as `D_WEBVTT/SUBTITLES` or `S_TEXT/WEBVTT`; `tx3g`, written as the `wvtt` it converts to; `pgs` as `S_HDMV/PGS`, DocType `matroska` |
| MPEG-TS | `avc1`, `hvc1`, `av01`, in and out | `mp4a`, `ac-3`, `ec-3`, `mp1`, `mp2`, `mp3`, in and out; **not** `opus` (see [MPEG-TS carriage](#mpeg-ts-carriage)) | none, in or out; `pgs` is refused with its reason |
| ADTS demux | — | `mp4a` | — |
| MPEG audio demux | — | `mp1`, `mp2`, `mp3` | — |
| FLAC demux | — | `fLaC` | — |
| Ogg demux | — | `Opus`, `vorb`, `fLaC` | — |
| AVI demux | `avc1`, `mp4v`, `mjpg` | `ipcm`, `fpcm`, `raw `, `mp1`, `mp2`, `mp3`, `ac-3`, `mp4a` | — |
| WAV demux | — | `ipcm` (signed integer), `fpcm` (float), `raw ` (unsigned 8-bit) | — |

See [Subtitles](#subtitles) for what each conversion keeps.

PCM is reported under the ISO/IEC 23003-5 entries with its `pcmC` record as
`CodecConfig`, and refused by the MP4 muxer **by name**: mp4ff does not write
those entries yet. FLAC and MPEG audio go into MP4 and Matroska as
[Audio codecs](#flac-mpeg-audio-and-ac-3-in-mp4-and-matroska) describes.

### File metadata

| | text fields | cover | other items |
|---|---|---|---|
| MP4 read / write | `moov/udta/meta` (handler `mdir`) `ilst` | `covr`, JPEG, PNG or BMP | kept verbatim to an MP4 output |
| Matroska read / write | `Info/Title`, file-level `Tags` | attachment `cover.jpg`/`cover.png`/`cover.bmp` | kept to a Matroska output |
| WebM write | as Matroska | **not written**: WebM has no attachments | kept |
| MPEG-TS | none | none | none |

See [File metadata](#file-metadata-1) for what each field is called where.

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

Edit lists (`elst`) are read and carried (see "Edit lists, MP4 to MP4"), but not
applied: samples come back in decode order, once each, as the sample tables
list them.

## Encoder priming survives a remux

An AAC encoder emits a frame before the media proper — about 21 ms at 48 kHz —
which a decoder consumes and does not present. Matroska states it as
`CodecDelay`, an MP4 as an edit list, and a remux that dropped it handed the
output a frame the input never presented: audio and video parted company by that
much, and the output was lossless for video only.

`TrackConfig.StartDelay` now carries it. Readers fill it in from whichever form
the source used, both MP4 muxers (`Muxer` and `ProgressiveMuxer`) write an edit
list for it, `WebMMuxer` writes it as the `CodecDelay` of an audio track
(RFC 9559 §5.1.4.1.25, in nanoseconds, so exactly; an Opus track's own pre-skip
takes precedence, as the WebM Opus mapping requires), and a caller who knows
better can overwrite it. Nothing has to be asked for.

It is not `PreSkip`. `PreSkip` is Opus stating its own priming inside its
identification header, and it travels with the codec configuration wherever the
track goes. `StartDelay` is what the **container** says, for any codec, and it
has to be rewritten into the form the output container uses.

Both directions are covered, since a fix for only one of them would be half a
fix: a Matroska `CodecDelay` becomes an `elst` on the way out, an MP4's `elst`
is read back on the way in, and becomes a `CodecDelay` again in Matroska.

**The precision is the output track's timescale.** Matroska counts in
milliseconds by default and that timescale is carried over, so a 21.333 ms
priming is stated as 21 — rounded to the nearest tick, not down. That is a third
of a millisecond lost where a whole 21.333 ms frame was gained before.

A priming is read out of one shape of edit list only: a single entry starting at
a positive media time at rate 1, which is what priming looks like. The media
time is in composition time, so the first sample's composition offset is taken
off it before it is read as a priming (see below). Any other list is carried
whole as `TrackConfig.EditList` instead (see "Edit lists, MP4 to MP4"). When
that is set, the MP4 muxers write the list and do not add `StartDelay` to it.

## B-frames start at zero

A track that reorders its frames (H.264 or HEVC with B-frames) decodes its
first frame at zero and presents it at its composition offset: two frames later
for x264's default of two B-frames. avkit's copies did not say where the
presentation starts, so the first picture was shown that much late while the
sound started at zero. Measured with `ffprobe` on an ffmpeg file (`libx264 -bf 2`
with AAC), the video `start_time` was:

| source → output | before | after |
|---|---|---|
| `.mkv` → fragmented MP4 (`Remux`) | 0.040 | 0.000 |
| `.mkv` → progressive MP4 (`JoinProgressive`) | 0.040 | 0.000 |
| `.mkv` → Matroska (`WebMMuxer`) | 0.040 | 0.000 |
| `.mp4` → fragmented MP4 | 0.000 | 0.000 |
| `.mp4` → progressive MP4 | 0.080 | 0.000 |
| `.mp4` → Matroska | 0.080 | 0.000 |

The source states 0.000 for every track. The one row that was right before was
right by accident: the reader took ffmpeg's edit list for a priming, and the
fragmented muxer wrote it back.

ISO/IEC 14496-12 §8.6.6.1 recommends an edit "to establish a presentation time
of 0 for the first presented sample, when composition offsets are used", and
that is what both MP4 muxers now write: one edit whose media time is the first
sample's composition offset, plus the priming of `StartDelay` after it. It is
what ffmpeg's `movenc` writes. The progressive muxer states the length of the
edit, rounded up to the movie timescale, and gives the track header that length
(§8.3.2.3). The fragmented muxer writes a length of zero, which §8.6.6.1 allows
in a fragmented file's initial movie.

The fragmented muxer now writes its initialisation segment **with the first
fragment** instead of with the first sample. Before then it cannot know the
first composition offset of every track. ffmpeg's `-movflags delay_moov` makes
the same choice: without it, ffmpeg's own fragmented output starts this video at
0.080. A track whose first sample arrives only after the first fragment has gone
out has its priming stated alone.

Matroska states presentation times, so `WebMMuxer` takes the first sample's
composition offset off every block of the track. A frame presented before the
first one would fall before zero and is refused. Only a group of pictures begun
before the stream can hold such a frame, and a decoder starting at the stream's
first sync sample discards it anyway.

Negative composition offsets (a version 1 `ctts`, §8.6.1.3, with `cslg`,
§8.6.1.4) are the other way to get a zero start: they move the decode times
instead of the presentation. They are not used here. They rewrite every offset
where the edit list adds one box, and ffmpeg's own output that way
(`-movflags +cmaf`) reads back starting at 0.040 for the same source.

The packets are untouched: ffmpeg's `framemd5` of the video, packets and
decoded frames alike, is identical to the source's, at identical timestamps, in
all six copies.

## Edit lists, MP4 to MP4

An edit list (ISO/IEC 14496-12 §8.6.5, §8.6.6) says which stretches of a
track's media make up its presentation: an empty edit (`EmptyEdit`) delays the
track, an edit at `NormalRate` plays media from a composition time onwards, a
rate of zero holds one frame. The MP4 reader hands the whole list over as
`TrackConfig.EditList`, every entry as the file states it. Durations stay in the
source's movie timescale (`MovieTimescale`). Media times are rebased onto the
samples `Samples` returns, for a fragmented file whose first fragment starts
later on its media timeline (its `tfdt`). The list is not applied to the
samples.

What each operation does with it:

| | |
|---|---|
| `Remux`, `Join`, `JoinProgressive`, a caller's own copy | written as it stands. Durations are restated in the output's movie timescale, rounded up. A progressive copy of a fragmented file's last edit of duration zero ("to the end", §8.6.6.1, only allowed in a fragmented file) states its length. The track header lasts what the edits do (§8.3.2.3). |
| `Muxer` (fragmented) | takes an optional empty edit followed by one edit at the normal rate, and refuses more with `ErrTrackConfig`. ffmpeg turns its edit list support off for a fragmented file ("multiple edit list entries, not supported in fragmented MP4 files"): a two-edit list copied into one presented all 100 frames where the source presents 50. `ProgressiveMuxer` writes it. |
| `Cut`, `Concat` of several inputs | both move the media the list counts in, so it cannot be copied. A list that only says where the presentation starts (one edit at the normal rate, reaching the end of the media) is dropped, and the muxer states that start itself. Anything else is refused with `ErrEditList`: an empty edit placing the track later than its siblings, several edits, a dwell or another rate. So is an edit ending before the media does (how a gapless AAC file trims its padding, and how an MP3 read with its LAME end padding, or a Matroska track read with its `DiscardPadding`, states it) when the output keeps the track's end. |
| `WebMMuxer` | Matroska has presentation times and no edit lists. An optional empty edit and one edit at the normal rate become a shift of the track's times: the empty edit delays them by its length, and an edit starting before the first frame's composition time shows it that much later. Anything else is refused with `ErrTrackConfig`. A priming after the first frame is the `CodecDelay` of an audio track when the list is the single edit a priming is written as, which the reader states as `StartDelay`; after an empty edit it is shown. An audio edit that stops before the media does is written as the `DiscardPadding` (RFC 9559 §5.1.3.5.7) of every block it cuts into. |
| `TSMuxer` | ignores it. |

Measured with ffmpeg 9.0.2 on 4 s files made the way the fixtures
`bframes-delayed.mp4` and `bframes-twoedits.mp4` are:

- The sound put 0.5 s late with `-itsoffset`, which ffmpeg writes as an empty
  edit of 479 ms and then the media. The fragmented, progressive and Matroska
  copies all start the sound at 0.479, as the source does. Their packet and
  decoded-video `framemd5` are identical to the source's.
- The picture's list rewritten into two edits of 1 s, the media's first and
  third seconds. The progressive copy decodes the same 50 frames at the same
  times as the source.

### The end of an AAC track, MP4 to Matroska

ffmpeg's AAC encoder leaves padding after the last sample, and its MP4 states
it as a last sample **shorter than the frame it decodes to**: `stts` says 768
for a 1024-sample frame, and ffmpeg's `mov.c` discards what that packet decodes
past its duration. Matroska has no such reading of a block's duration, so a
`WebMMuxer` copy decoded those 256 samples as sound the source never
presented.

`WebMMuxer` now writes the trim as the last block's `DiscardPadding`, as
ffmpeg's own copy does: a block group lasting 16 ms with a padding of
5 333 333 ns, element for element the same. It does so for AAC only, whose
frames all decode to one length (1024 or 960 samples), so a last frame stated
shorter than the one before it can only mean a trim; an Opus or FLAC frame is
as long as it says. With an edit that stops before the media does (above) the
padding covers what the edit leaves out too. Decoded by ffmpeg, the last frame
of the copy of a 2 s stereo file is 768 samples long, as the source's is.

### The end of a Matroska audio track, to MP4

The other way, the Matroska reader reads `DiscardPadding` (RFC 9559 §5.1.3.5.7)
and states where it ends the presentation as `TrackConfig.EditList`: one edit
from the priming `StartDelay` states to that end, which the MP4 muxers write as
an edit ending before the media — what ffmpeg's `movenc` makes of the skipped
samples `matroskadec` hands it (`elst_end_pts`, the packet lasting
`max(frame_size, duration)` less the padding) — and `WebMMuxer` writes back as
`DiscardPadding`. A last block decodes for what the samples say it lasts and at
least as long as the block before it, which stands for `movenc`'s frame size:
ffmpeg writes a trimmed last frame both ways, as a short `BlockDuration` beside
its padding (its own encoders' output) and as no duration at all beside it
(an MP3 copied from a file with a LAME tag). The first already ends where the
padding begins, so it states no edit; the second used to be copied whole.
Measured on ffmpeg 9.0.2's `-c copy` of `testdata/sine.mp3` into Matroska, which
presents 14 335 of its 14 976 samples: avkit's MP4 copy decoded 14 976 before and
14 309 now; ffmpeg's own MP4 copy decodes 14 377. The precision is the Matroska
millisecond. Only the run of padded blocks that reaches the end of the track is
read; a padding inside the track, or a negative one at the start of a block, is
left as it was.

## The two rates of an HE-AAC track

HE-AAC codes an AAC core at half the rate it is heard at, and spectral band
replication (SBR) restores the upper band. Matroska states both rates,
`SamplingFrequency` and `OutputSamplingFrequency` (RFC 9559 §5.1.4.1.29.1 and
.29.2), and ffmpeg's matroskaenc fills them from the `AudioSpecificConfig`
(`get_aac_sample_rates`): the core rate, and the extension rate when the record
signals SBR. `WebMMuxer` wrote the track's own rate as `SamplingFrequency` and no
output rate; it now writes what matroskaenc writes. The record is read the way
ffmpeg's `ff_mpeg4audio_get_config_gb` reads it, which finds both ways the
`AudioSpecificConfig` syntax (ISO/IEC 14496-3 Table 1.15) signals SBR: the
hierarchical one, object type 5 or 29, and the backward-compatible sync
extension `0x2B7` after an AAC-LC configuration, which mp4ff's decoder does not
read. An AAC-LC track states its own rate and no
output rate, ffmpeg's own five-byte record (sync extension, SBR absent) included.

The Matroska reader takes `OutputSamplingFrequency` as the track's rate where it
is stated, as ffmpeg's matroskadec does. It used to take `SamplingFrequency`, so
ffmpeg's Matroska copy of an HE-AAC MP4 read back as 22 050 Hz audio and an MP4
written from it stated 22 050 in its sample entry. Measured on Apple's HE-AAC
(`aac_at -profile:a 4`, record `13 90 56 e5 a0`, 44.1 kHz stereo) and ffmpeg
9.0.2's `-c copy` of it: avkit's Matroska copy stated 44100 and no output rate
before, and states 22050 and 44100 now, as ffmpeg's does; both files read back
at 44 100 Hz.

## A segment index for DASH on-demand

`NewMuxer(w, container.SegmentIndex())` writes one segment index box (`sidx`,
ISO/IEC 14496-12 §8.16.3) per track after the initialisation segment and before
the first fragment. Each one references every fragment, a `moof` and its
`mdat`, with its size, its duration on that track, and whether a player can
start there. That is the layout of an Indexed Self-Initializing Media Segment
(ISO/IEC 23009-1 §6.3.5.2): one file a DASH on-demand client reads the index
of with one range request, then fetches fragment by fragment.

- **One `sidx` per track**, each naming the same byte ranges, as ffmpeg's
  `-movflags +dash+global_sidx` writes them. A track absent from a fragment
  states a reference of no duration there. A track that never had a sample
  gets no box.
- **Durations run from one fragment's earliest presentation time to the
  next**, and the last one's to the end of the track, as §8.16.3.3 defines
  them. They add up to the track's duration on the media timeline.
- **SAP type 1** where the fragment opens on a sync sample that nothing in it
  presents before. A sync sample that a later-decoded frame presents before is
  type 2 or 3, which only the bitstream can tell, so it is stated as 0
  (unknown). ffmpeg states 0 for every reference.
- **Version 1**, with 64-bit times and offsets, as ffmpeg writes it. It costs
  eight bytes a track, and no time or offset can then fail to fit.

The index describes what comes after it, so it can only be written at `Close`.
What happens until then depends on the writer:

- **A writer that can also read and seek**, such as an `os.File`, is handed each
  fragment as it is made. At `Close` the fragments are moved forward by the
  size of the index, one block at a time, and the index is written in the gap.
  ffmpeg's `global_sidx` does the same. One block is held in memory, and the
  media is written twice.
- **Any other writer** has the fragments held in memory until `Close`, up to
  `IndexMemoryLimit` (256 MB by default). Past that limit the muxer reports
  `ErrMediaTooLarge`.

Both paths produce the same bytes. Some files cannot be indexed and are refused
with `ErrMediaTooLarge`: more than 65535 fragments (one box's
`reference_count`), a fragment over 2 GiB (`referenced_size` is 31 bits), or one
lasting more than 2³² ticks.

Measured against ffmpeg 9.0.2 on the same 7 s H.264 + AAC input, cut every 2 s:

| | avkit | ffmpeg `+dash+global_sidx` |
|---|---|---|
| boxes | `ftyp moov sidx sidx (moof mdat)×4` | the same, then `mfra` |
| video `sidx` | `ept` 1024, durations 25600/25600/25600/12800 | identical |
| audio `sidx` | `ept` 0, Σ 337 024 = the track's `stts` | Σ 339 840, 2816 ticks past the media |
| Σ `referenced_size` | = the bytes from the first `moof` to the end | = up to `mfra` |
| `ffmpeg -ss 4.5`, first 3 frames | the same `framemd5` as the source | shifted by two frames |

ffmpeg's own output plays two frames off because it drops the source's edit
list.

The reader needs nothing new: a fragment says where its samples are from its
own `moof`, so a file reads the same with or without its index. A test checks
this on this muxer's output and on a file made by ffmpeg.

## A SeekHead in the WebM/Matroska muxer

`WebMMuxer` writes Cues after the last cluster in every mode. When the file can
carry a complete SeekHead (RFC 9559 §5.1.1, §6.3), the segment opens with one:
it names Info, Tracks, every other Top-Level Element written before the first
cluster, and the Cues. A Void follows it (§25.2), as ffmpeg's `matroskaenc`
writes it.

| mode | SeekHead |
|---|---|
| `BufferedSegment()` | written once, with every position known |
| streamed to an `io.Seeker` (an `os.File`) | written first with room for the Cues entry, then rewritten in place at `Close`. The writer is seeked back and returned to the end of the file. The segment is completed as ffmpeg's `matroskaenc` completes it at its trailer: Info's `Duration` (RFC 9559 §5.1.2.10) is written over an 11-byte Void that held its place, and the Segment's and every Cluster's real size over the 8-byte unknown size they were opened with |
| streamed to anything else (a socket, an HTTP response, a pipe) | **none**, so the bytes are unchanged |

The last row is deliberate. The Cues are not known when a SeekHead would have to
go out, and §6.3 wants the SeekHead to name every Top-Level Element or not to
exist at all. ffmpeg likewise writes no Cues to an unseekable output. A writer
that claims to seek and then cannot, such as `os.Stdout` on a pipe, is treated
the same way and still gets its stream.

- **Entries come from what was encoded**, not from a list of names. Every
  element written before the first cluster is named, so Tags or Attachments
  added there are indexed without this code changing.
- **The room is fixed** at seven entries, ffmpeg's `MAX_SEEKHEAD_ENTRIES`, or
  more if needed. `SeekPosition` is always written in eight bytes, as ebml-go's
  own `mkvcore` writes it, so the SeekHead can be rewritten without moving
  anything after it.
- Clusters are not named in the SeekHead. The Cues index them, as in ffmpeg's
  output.

Measured on a 7 s VP9 + Opus file:

| | SeekHead | `ffmpeg -ss 4.5` I/O |
|---|---|---|
| streamed to a buffer (unchanged) | none | 262 144 bytes read, 0 seeks |
| streamed to a file | Info, Tracks, Cues, each at its element | 131 185 bytes read, 3 seeks |
| buffered | Info, Tracks, Cues, each at its element | 131 185 bytes read, 3 seeks |
| buffered, before this change | none | 262 144 bytes read, 0 seeks |

With a SeekHead, ffmpeg goes straight to the Cues. Without one, it reads the
whole file to find them. The frames decoded after the seek match the source's
`framemd5` in all four cases.

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

⛔ **A video unit's PTS is read back, not only its DTS.** A track that reorders
its frames (H.264 or HEVC with B-frames) states both in every PES header
(13818-1 2.4.3.7, `PTS_DTS_flags` `'11'`), and their difference, taken modulo the
33-bit clock, is each sample's `CompositionOffset`; a unit stating a PTS alone is
presented when it decodes, as FFmpeg's `mpegts.c` reads it. Timed by the DTS
alone, an `x264 -bf 2` stream came out of a remux in decoding order as if it
were presented in it, and FFmpeg decoded 49 of its 50 pictures. The copy now
decodes to the same pictures at the same times as FFmpeg's own `-c copy`.

An Enhanced AC-3 frame of a dependent substream (or of an independent substream
other than the first) joins the frame before it: A/52 Annex G §3.3 makes the
access unit every substream of one period, and an MP4 `ec-3` sample is that
access unit. A `dac3` or `dec3` record is built from the first frame, as it is
for Matroska, so a transport stream's AC-3 goes straight into an MP4.

**The frames, not the program map, name the codec.** Stream types `0x03` and
`0x04` do not say which layer they carry (FFmpeg names both `mp3` until its
parser has seen a frame, and so does this until it has read one), and an
E-AC-3 stream under stream type `0x81` is named `ec-3`. MPEG audio goes on into
an MP4 as `mp4a` with object type indication `0x6B` or `0x69` (see
[Audio codecs](#flac-mpeg-audio-and-ac-3-in-mp4-and-matroska)).

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

**Every track is presented from its first presented frame.** A transport stream
has no edit list: the PTS of each unit (ISO/IEC 13818-1 §2.4.3.7) is all there
is. `TSMuxer` takes a track's first composition offset (B-frames) and its
`StartDelay` (an AAC priming) off every timestamp, so the first picture and the
first sound after the priming are shown at the same instant. The units decoded
before it keep a DTS — and a priming frame a PTS — before it, because the whole
program is moved by the offset ffmpeg's `mpegtsenc.c` uses with its command
line's `-muxdelay 0.7`: every PTS and DTS starts at 1.4 s (126000 ticks), and
the PCR runs 0.7 s ahead of the DTS of the unit carrying it. A start more than
0.7 s into a track's media is refused. Measured with `ffprobe` on an ffmpeg file
(`libx264 -bf 2` with AAC) in Matroska, the video starts 0.040 s after the audio
before and 0.021 s after it now, which is what ffmpeg's own copy states (1.480 s
against 1.459 s): the audio's priming frame comes first.

## FLAC, MPEG audio and AC-3 in MP4 and Matroska

| codec (`TrackConfig.Codec`) | MP4 (`Muxer`, `ProgressiveMuxer`) | Matroska (`WebMMuxer`, DocType `matroska`) | specification |
|---|---|---|---|
| FLAC (`fLaC`) | `fLaC` sample entry with a `dfLa` holding **every** metadata block, STREAMINFO first; `samplesize` the bit depth, `samplerate` halved while above 65535 Hz (65535 when halving leaves a fraction) | `A_FLAC`, `CodecPrivate` = `"fLaC"` + the metadata blocks | Xiph *Encapsulation of FLAC in ISO BMFF* §3.3.1–§3.3.2; RFC 9639 §8.1, §8.2, §10.2 |
| MPEG audio (`mp1`, `mp2`, `mp3`) | `mp4a`, `esds` object type `0x6B` at 32/44.1/48 kHz, `0x69` at 16/22.05/24 kHz, **no** decoder specific info; "MPEG 2.5" rates refused | `A_MPEG/L1`, `/L2`, `/L3`, no `CodecPrivate` | MP4RA `oti` registry; FFmpeg `movenc.c` `mov_write_esds_tag`, `isom.c`; Matroska codec specifications |
| AC-3 (`ac-3`), E-AC-3 (`ec-3`) | `ac-3`/`ec-3` with `dac3`/`dec3` (unchanged) | `A_AC3`, `A_EAC3`, no `CodecPrivate`, channels and rate in the Audio element | Matroska codec specifications ("Initialization: none") |
| Vorbis (`vorb`) | **refused**: MP4RA registers no sample entry for it | `A_VORBIS` in WebM (unchanged) | MP4RA `sample-entries.csv` |

`TrackConfig.CodecConfig` of a FLAC track is the Matroska form, `"fLaC"` and the
blocks, whichever container it was read from: `Reader` turns a `dfLa` back into
it, and takes the rate and channel count from STREAMINFO, which the Xiph mapping
says is the one to read. A FLAC configuration that stops before the block marked
last, puts anything before STREAMINFO, or is followed by frame bytes is refused.

An MP4's `esds` names the MPEG audio standard, not the layer, so `Reader` names
such a track `mp1`, `mp2` or `mp3` **from its first frame** (as FFmpeg's parser
does), and a track whose first sample is not a frame is refused rather than read
as AAC — which is what it was read as before. Only that first sample is read:
`TrackConfig` used to read the whole track for it, which from `NewFileReader`
fetched 960 470 bytes of a 60 s, 970 424-byte MP3-in-MP4 file, and fetches the
first frame's 417 now. ⛔ FFmpeg's own reader names every
`mp4a` of object type `0x6B` or `0x69` `mp3` (`isom.c`), Layer II included, and
decodes it correctly all the same.

Checked against FFmpeg 9.0.2: FLAC (44.1 to 192 kHz, up to six channels, 24
bits), MP3 (44.1 and 22.05 kHz), MP2, AC-3 and E-AC-3 written by FFmpeg into
Matroska, MP4 and MPEG-TS, remuxed through each of the three muxers, probe as the
same codec, rate and channel count, hold the same packets (`framemd5`), and
decode without an error message.

## VP9 out of Matroska

Matroska states no VP9 profile and no level: both live in the frame header, and
there is no `CodecPrivate` to carry them. `vpcC` has no value for "unknown" —
zero is a real profile but not a real level — so remuxing such a track used to
fail with `vp09 needs a level; 0 is not one`.

`Remux`, `Cut`, `Concat`, `Join` and `JoinProgressive` now read the
configuration **out of the frames** when the container could not state it,
through the same `ConfigFromSamples` a caller can use directly.

The reader is unchanged and still reports only what the container says. That is
deliberate: a level could also be derived from the container's width, height and
frame rate, and it would be a worse description — the profile would stay at zero
and the colour at "unspecified", where the frame header states both. The
container is not made to claim knowledge it does not have.

A VP9 track whose samples cannot describe it either is still refused, naming the
track, rather than written with a `vpcC` of zeroes.

**VP8 has no level at all**, and could not be written to MP4 for that reason:
every operation failed with `vp08 needs a level; 0 is not one`. The *VP Codec ISO
Media File Format Binding* (v1.0) gives level 0 as "Undefined" in its level
table and says "VP8 only supports a profile value of 0", so a `vp08` record
states profile 0 and level 0, and a VP8 profile other than 0 is refused. ffmpeg
refuses to write VP8 into MP4 at all (`movenc.c`: "altref frames handling is not
defined in the spec as of version v1.0"); its reader decodes avkit's `vp08` copy
of a `libvpx -auto-alt-ref 1` Matroska file frame for frame identically to the
source (`framemd5`, 50 of 50 frames, same timestamps).

## The shape of a pixel

VP8 and VP9 state no aspect ratio in their bitstream, so an MP4 can only say it
in the sample entry, as a `pasp` box (ISO/IEC 14496-12 §12.1.4). Without one,
`ffprobe` read a VP9 track remuxed from WebM as `sample_aspect_ratio=N/A` where
the WebM said `1:1`.

`TrackConfig.SARWidth` and `SARHeight` now carry it. The Matroska reader takes it
from `DisplayWidth`/`DisplayHeight` against the cropped frame (RFC 9559
§5.1.4.1.28), whose defaults state square pixels, and leaves it unknown (0:0)
under `DisplayUnit` 4; the MP4 reader takes it from `pasp`. Both MP4 muxers write
a known ratio as `pasp`, reduced, and scale the track header's width by it, as
ffmpeg's movenc does; an unknown one writes nothing. A WebM written by ffmpeg
with `setsar=4/3` remuxes to an MP4 `ffprobe` reads as `427:320`, as ffmpeg's own
`-c copy` does.

The WebM muxer writes the ratio as a display size, the way ffmpeg's matroskaenc
does (`mkv_write_video_track`), in the form the document type allows. A Matroska
file states the display aspect ratio itself, `DisplayUnit` 3 (RFC 9559
§5.1.4.1.28.12 to .14, Table 10), reduced exactly; a WebM file states the width
the frame is shown at in pixels, rounded to the nearest, and the frame's height;
square pixels write neither, the default display size stating them; and an
unknown ratio is `DisplayUnit` 4 in a Matroska file and nothing in a WebM one. An
H.264 MP4 made by ffmpeg with `setsar=4/3` at 320x240 copied to Matroska read
back as `1:1` before this and reads `4:3` now, written `16x9` under unit 3 as
ffmpeg's own copy is; its VP9 counterpart copied to WebM reads `427:320`, written
`427x240` as ffmpeg's is. Two departures from matroskaenc, both towards stating
the ratio exactly: it approximates the display aspect ratio within 2^20, and skips
it whenever the rounded pixel width equals the frame's. A ratio whose display
width would round to nothing or pass 2^31 - 1 is refused, as matroskaenc refuses
the second.

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
Two kinds are not turned into it and are carried as they are: 3GPP `tx3g` text in
an MP4, and PGS bitmaps in Matroska; they have their own sections below.

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

### 3GPP timed text (`tx3g`, `mov_text`)

`tx3g` is 3GPP TS 26.245 timed text (clause 5.16 for the sample entry, 5.17 for
the sample), which MP4RA registers as "Timed Text stream" and ffmpeg calls
`mov_text`. It is the third subtitle form in the population's MP4s, after the
`wvtt` that this package reads everything else as. **`wvtt` stays the canonical
form** — every Matroska text mapping converts to it, and its cues are
timeline-exact — and `tx3g` is a **pass-through and a conversion target**: a copy
loses nothing and a conversion does, so nothing converts unless asked.

| from → to | what happens |
|---|---|
| MP4 `tx3g` → MP4 (`Muxer`, `ProgressiveMuxer`, `Remux`, `Cut`, `Concat`, `Join`) | **unchanged**: the sample entry travels as `TrackConfig.CodecConfig`, the samples byte for byte. The track is written with the `text` handler and a null media header (3GPP TS 26.244; ffmpeg writes QuickTime's `sbtl`) |
| MP4 `tx3g` → WebM / Matroska (`WebMMuxer`) | converted **by the muxer** to the cues of `wvtt` and written as `D_WEBVTT/SUBTITLES`; hand it the `tx3g` samples as read |
| `wvtt` (from Matroska WebVTT or SRT, or MP4) → `tx3g` in MP4 | `Remux(w, r, SubtitlesAs("tx3g"))`: every sample is rewritten, every duration kept, and the entry is the default one (centred at the bottom, white 18 pixel text in font 1, font table `Serif`, as ffmpeg's `movtextenc` writes for an unstyled track) |
| `tx3g` → `wvtt` in MP4 | `Remux(w, r, SubtitlesAs("wvtt"))` |

A text sample is the string, and the `styl` box is the one modifier box that is
read. Samples that last no time are left behind: ffmpeg ends every `mov_text`
track with an empty sample of zero length, which shows nothing for no time and
which no muxer here would write.

⛔ **What the conversions lose** (pinned by the tests, which read each side back):

- **`tx3g` → WebVTT keeps** the text and the bold, italic and underline of the
  style records, as `<b>`, `<i>` and `<u>`; characters are escaped as a cue
  body requires (`&`, `<`, `>`), line terminators become line feeds and blank
  lines go (one ends a WebVTT cue). The result is what ffmpeg 9.0.2 decodes the
  fixture to, cue for cue and time for time, except that ffmpeg writes `&` and
  `<` raw.
  **It loses** font ids and sizes, text colours and the background colour, the
  justification and the default text box, `hlit`/`hclr` highlight, `krok`
  karaoke, `dlay` scroll delay, `href` links, `tbox` boxes, `twrp` wrap, `blnk`
  blinking, `disp` stereo disparity, and the display flags. The default style's
  own face flags are not applied to the text (ffmpeg's decoder does not either:
  its comment says most players ignore them, since files commonly state them
  wrongly). The UTF-16 form of the string is read and written back as UTF-8.
- **WebVTT → `tx3g` keeps** the text and `<b>`, `<i>`, `<u>`; the character
  references WebVTT defines (`&amp;`, `&lt;`, `&gt;`, `&lrm;`, `&rlm;`, `&nbsp;`)
  become the characters. **It loses** the cue identifier, the cue settings
  (position, line, alignment, size), the `NOTE` comments, every other tag
  (`<c>`, `<v>`, `<lang>`, `<ruby>`, karaoke timestamps) with its text kept, and
  the `STYLE` and `REGION` blocks of the header. Cues shown together in one
  sample become the lines of one string, one under the other; a sample whose
  text exceeds the 65535 bytes of a text length is refused.
- `Cut` shortens the first text sample like any cue, and refuses a track whose
  samples hold a `krok` box (its times count from the sample's start); `Concat`
  refuses the same and ends each input's text track where its pictures end.

### Bitmap subtitles: PGS (`S_HDMV/PGS`)

The Blu-ray presentation graphics stream is read from Matroska as `pgs`, a
subtitle track whose every block is one display set — the segments from the
presentation composition segment to the end segment, each as its type byte,
16-bit size and payload, without the `PG` header and the 90 kHz PTS and DTS of a
`.sup` file — and written back to Matroska by `WebMMuxer` as `S_HDMV/PGS`, DocType
`matroska`, **block for block and byte for byte**. The `CodecPrivate` of the track
is ignored, as the codec mapping says. The fixture's two blocks, written by
ffmpeg's `matroskaenc` from a `.sup`, are the bytes the test builds from the
segment layouts. Nothing reads inside a display set.

- **MP4 refuses it**, by name, with the reason: MP4RA's sample-entry registry
  holds no PGS or HDMV entry (`mp4ra.github.io`, `data/sample-entries.csv`,
  read 2026-11-01), so no player would know the track. `Remux` therefore fails
  on a PGS track until it is left out with `DropTracks`.
- **MPEG-TS refuses it**, though ffmpeg writes it (stream type `0x90`, which
  ffmpeg's `mpegts.c` also reads back): `0x90` is a stream type of the Blu-ray
  Disc specifications, which are not public and were not read for this package,
  and a PES for it needs the decode time of every display set, which a Matroska
  block does not state. Out of scope, said so, and not needed by the files this
  was written for — both are Matroska.
- **Timing.** A block is a display set, not an interval: ffmpeg's `matroskaenc`
  states no `BlockDuration` for it, so a sample lasts until the next block
  and the last to the end of the segment, the way every track without durations
  reads.

Not carried: SSA/ASS, the other bitmap subtitles (VobSub, DVB), HDMV text
(`S_HDMV/TEXTST`), the experimental `wvtc`/`stpc`, and the distinction between
WebM's captions, descriptions and metadata kinds, which all read as subtitles.

## File metadata

`File.Metadata` is what a file says about itself as a whole: eight text fields
and a cover picture. Every reader fills it in, `SetMetadata` on `Muxer`,
`ProgressiveMuxer` and `WebMMuxer` writes it, and `Remux`, `Cut`, `Concat` and
`Join` carry it — from the **first** input when there are several.
`ReplaceMetadata(md)` writes `md` instead, and `ReplaceMetadata(Metadata{})`
writes none.

| field | MP4 `ilst` item | Matroska |
|---|---|---|
| `Title` | `©nam` | `Info/Title`; a `TITLE` tag takes precedence when reading |
| `Artist` | `©ART` | `ARTIST` |
| `Album` | `©alb` | `ALBUM` (ffmpeg's name; not an assigned tag) |
| `Date` | `©day` | `DATE_RELEASED`; ffmpeg's `DATE` is read when it is absent |
| `Comment` | `©cmt` | `COMMENT` |
| `Description` | `desc` | `DESCRIPTION` |
| `Genre` | `©gen` | `GENRE` |
| `Encoder` | `©too` | `ENCODER` |
| `Cover` | `covr` (data type 13, 14 or 27) | an attachment, `cover.jpg`, `cover.png` or `cover.bmp` |

The MP4 side is the iTunes layout Apple's QuickTime File Format describes and
ffmpeg writes: a full `meta` box with a 33-byte `mdir`/`appl` handler, then
`ilst`, every value a UTF-8 `data` box. mp4ff decodes the containers; the items
are read here, because mp4ff keeps most of them as undecoded bytes and drops the
data type that tells a JPEG cover from a PNG one. A cover labelled JPEG that
starts with the PNG signature is read as PNG, as ffmpeg does.

Matroska tags are read when they are about the file: no track, edition, chapter
or attachment UID, and a `TargetTypeValue` of 50 or none. They are written as one
`Tag` at level 50, with the title in `Info` — where ffmpeg puts it and where
every reader looks. The cover is the attachment named `cover.jpg` or
`cover.png`, as the Matroska attachments section names the normal cover, or
else the first JPEG, PNG or BMP attached.

What is **lost**, said plainly:

- **WebM gets no cover.** The WebM guidelines admit no `Attachments`; ffmpeg's
  webm muxer drops attachments too. The text is written.
- An `ilst` item no field names — a track number, a sort name, a freeform
  item — is copied byte for byte to an MP4 output and left out of a Matroska
  one. A file-level Matroska tag no field names is copied to a Matroska output
  and left out of an MP4 one.
- Only the first `data` box of an item and the first picture of a `covr` are
  read. Nested `SimpleTag`s, tags about a track or chapter, attachments other
  than the cover (fonts), QuickTime-style `udta` strings and `mdta`-keyed
  metadata are not read.
- Text that is not UTF-8, or holds a NUL, is not read as a field (an MP4 item
  holding it is still carried verbatim), and `SetMetadata` refuses it.
- An MPEG transport stream has no place for metadata at all.

ffprobe is the oracle: the ffmpeg-written fixtures in `container/testdata` read
back field for field, and ffprobe reads avkit's MP4 (fragmented and
progressive) and Matroska output as the same tags with a byte-identical cover.

⛔ ebml-go allocates an element's value **before** reading it, so a Matroska
file of under a hundred bytes announcing a 100 MB attachment cost 100 MB. Leaf elements are
now capped at the size of the document.

## Guarantees

- **Pure Go, CGO=0.** No `libav`, no `exec` to `ffmpeg`.
- **100% statement coverage**, enforced in CI, error branches included.
- **Six 64-bit targets** exercised each run: `amd64`, `arm64` (native) and
  `riscv64`, `loong64`, `ppc64le`, `s390x` (under qemu) — the last also covering
  big-endian byte-order correctness.

## Scope

This module reads and writes containers: their structure, their tracks'
configuration and their samples, which is what copying a track from one
container into another takes. The sibling modules listed under "What is
deliberately absent" read bitstream syntax. Nothing in the organisation decodes:
there is no H.264, HEVC, AV1 or VP9 picture decoding and no AAC or Opus audio
decoding, and none is planned here.

## License

BSD-3-Clause — see [LICENSE](LICENSE). Copyright the go-avkit authors.
