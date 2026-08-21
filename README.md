# go-avkit

[![ci](https://github.com/go-avkit/avkit/actions/workflows/ci.yml/badge.svg)](https://github.com/go-avkit/avkit/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-avkit/avkit.svg)](https://pkg.go.dev/github.com/go-avkit/avkit)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-avkit/avkit)](https://goreportcard.com/report/github.com/go-avkit/avkit)
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
| `container` | Sniff and demux MP4/ISO-BMFF, Matroska/WebM and MPEG-TS into a unified `File`/`Track` model (kind, codec, dimensions, channels, timing); read the samples and per-track configuration of an MP4 or a transport stream; mux a fragmented MP4 or an MPEG-TS from tracks delivered separately; copy, cut, concatenate and drop tracks with `Remux`, `Cut` and `Concat`. No re-encoding anywhere. |

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
