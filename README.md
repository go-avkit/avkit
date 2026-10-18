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
| `container` | Sniff and demux MP4/ISO-BMFF, Matroska/WebM and MPEG-TS into a unified `File`/`Track` model (kind, codec, dimensions, channels, timing); read the samples and per-track configuration of an MP4 or a transport stream, one file or a sequence of segments; mux a fragmented or a progressive MP4, or an MPEG-TS, from tracks delivered separately; copy, cut, join, concatenate and drop tracks with `Remux`, `Cut`, `Join` and `Concat`; and rewrite an HEVC track's sample entry to `hvc1` with `ConformHVC1`. No re-encoding anywhere. |

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

Nothing else: no AVI, no FLV, no Ogg, no WAV, no MXF, and no HLS or DASH
manifests. A file that is none of the three sniffs as `FormatUnknown` rather
than being guessed at.

### Per-track configuration, from the samples

`ConfigFromSamples` has **three** answers, and they are not two:

| | codecs | what it means |
|---|---|---|
| **derived from the samples** | `avc1`, `avc3`, `hvc1`, `hev1`, `av01`, `vp09`, `mp4a` | the elementary stream states its own configuration, and it is read out of it |
| **stated by the container** | `vp08`, `opus`, `ac-3`, `ec-3` | `ErrNotInSamples`. The codec is known and its samples **cannot** describe it — VP8 defines no level, an Opus stream carries no identification header, an AC-3 sync frame's bit stream information is not read here. Take the configuration from the container, which has it |
| **not known** | everything else | `ErrUnsupportedCodec` alone. There is nothing to take |

⛔ The middle row used to be indistinguishable from the last: both carried
`ErrUnsupportedCodec`, so a caller could not tell "take it from the container"
from "give up". `ErrNotInSamples` **wraps** the older sentinel, so code written
before it existed still matches.

### Tracks a container can carry

| | video | audio |
|---|---|---|
| MP4 demux | any — the four-character code is read from the sample entry, not from a list | any |
| MP4 mux | `avc1`, `hvc1`, `av01`, `vp08`, `vp09`, `mjpg` | `mp4a`, `opus`, `ac-3`, `ec-3` |
| Matroska demux | AVC, HEVC, AV1, VP8, VP9 | AAC, AC-3, E-AC-3, FLAC, Opus, Vorbis |
| WebM mux | AV1, VP8, VP9 | AAC, Opus, Vorbis |
| MPEG-TS | `avc1`, `hvc1`, `av01` in, `avc1`/`hvc1` out | `mp4a` |

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
[`bitstream`](https://github.com/go-avkit/bitstream). **There is no AAC, Opus,
FLAC, AC-3 or AV1 bitstream reader** in the organisation: those codecs are
handled here only as far as a container needs them.

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
list for it, and a caller who knows better can overwrite it. Nothing has to be
asked for. `WebMMuxer` does not write it: it states `CodecDelay` for Opus only,
from the identification header's pre-skip.

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
| `Cut`, `Concat` of several inputs | both move the media the list counts in, so it cannot be copied. A list that only says where the presentation starts (one edit at the normal rate, reaching the end of the media) is dropped, and the muxer states that start itself. Anything else is refused with `ErrEditList`: an empty edit placing the track later than its siblings, several edits, a dwell or another rate. So is an edit ending before the media does (how a gapless AAC file trims its padding) when the output keeps the track's end. |
| `WebMMuxer` | Matroska has presentation times and no edit lists. An optional empty edit and one edit at the normal rate become a shift of the track's times: the empty edit delays them by its length, and an edit starting before the first frame's composition time shows it that much later. Anything else is refused with `ErrTrackConfig`. A priming after the first frame is shown, as `WebMMuxer` shows any priming but Opus's. An audio edit that stops before the media does is written as the `DiscardPadding` (RFC 9559 §5.1.3.5.7) of every block it cuts into. |
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

Matroska to MP4 is not covered: the Matroska reader does not read
`DiscardPadding` back.

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
