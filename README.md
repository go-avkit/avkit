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
