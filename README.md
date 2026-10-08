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
