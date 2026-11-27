// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Command vp8bench measures vp8enc against libvpx's vpxenc on one input: gen
// writes the UI sequence as a Y4M file (what vpxenc reads), enc encodes a Y4M
// file with vp8enc and reports speed and size, psnr decodes a WebM file with
// govpx and compares it with the Y4M it came from. It is a measuring tool for
// the maintainers, not part of the library.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-avkit/avkit/container"
	"github.com/go-avkit/avkit/vp8enc"
	"github.com/go-avkit/avkit/vp8enc/internal/uiseq"
	"github.com/thesyncim/govpx"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: vp8bench gen|enc|psnr [flags]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	var (
		y4m    = fs.String("y4m", "seq.y4m", "Y4M file")
		webm   = fs.String("webm", "out.webm", "WebM file")
		n      = fs.Int("n", 300, "frames (gen)")
		w      = fs.Int("w", 1280, "width (gen)")
		h      = fs.Int("h", 720, "height (gen)")
		fps    = fs.Int("fps", 30, "frame rate")
		kbps   = fs.Int("kbps", 500, "target bitrate (enc)")
		cpu    = fs.Int("cpu", 0, "CPUUsed (enc)")
		kf     = fs.Int("kf", 120, "keyframe interval (enc)")
		screen = fs.Bool("screen", true, "screen content mode (enc)")
		rt     = fs.Bool("realtime", false, "realtime deadline (enc)")
		reps   = fs.Int("reps", 1, "encode repetitions, best time reported (enc)")
	)
	_ = fs.Parse(os.Args[2:])
	switch os.Args[1] {
	case "gen":
		check(gen(*y4m, *n, *w, *h, *fps))
	case "enc":
		check(enc(*y4m, *webm, vp8enc.Options{FPS: *fps, BitrateKbps: *kbps, CPUUsed: *cpu, KeyFrameInterval: *kf, ScreenContent: *screen, Realtime: *rt}, *reps))
	case "psnr":
		check(psnr(*y4m, *webm))
	default:
		fail("unknown command " + os.Args[1])
	}
}

func fail(s string) { fmt.Fprintln(os.Stderr, s); os.Exit(2) }
func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func gen(path string, n, w, h, fps int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	fmt.Fprintf(bw, "YUV4MPEG2 W%d H%d F%d:1 Ip A1:1 C420jpeg\n", w, h, fps)
	for i := 0; i < n; i++ {
		yc := vp8enc.ToYCbCr(uiseq.Frame(i, w, h))
		bw.WriteString("FRAME\n")
		for y := 0; y < h; y++ {
			bw.Write(yc.Y[y*yc.YStride : y*yc.YStride+w])
		}
		for _, p := range [][]byte{yc.Cb, yc.Cr} {
			for y := 0; y < (h+1)/2; y++ {
				bw.Write(p[y*yc.CStride : y*yc.CStride+(w+1)/2])
			}
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// readY4M returns the frames of a 4:2:0 Y4M file, held as one image each.
func readY4M(path string) ([]*image.YCbCr, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	r := bufio.NewReader(bytes.NewReader(data))
	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "YUV4MPEG2") {
		return nil, 0, fmt.Errorf("not a Y4M file")
	}
	var w, h, fps int
	for _, tok := range strings.Fields(line)[1:] {
		switch tok[0] {
		case 'W':
			w, _ = strconv.Atoi(tok[1:])
		case 'H':
			h, _ = strconv.Atoi(tok[1:])
		case 'F':
			fps, _ = strconv.Atoi(strings.SplitN(tok[1:], ":", 2)[0])
		}
	}
	if w <= 0 || h <= 0 || w > vp8enc.MaxDimension || h > vp8enc.MaxDimension {
		return nil, 0, fmt.Errorf("bad size %dx%d", w, h)
	}
	var out []*image.YCbCr
	for {
		if _, err := r.ReadString('\n'); err == io.EOF {
			return out, fps, nil
		} else if err != nil {
			return nil, 0, err
		}
		yc := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
		for _, p := range [][]byte{yc.Y, yc.Cb, yc.Cr} {
			if _, err := io.ReadFull(r, p); err != nil {
				return nil, 0, err
			}
		}
		out = append(out, yc)
	}
}

func enc(y4m, webm string, o vp8enc.Options, reps int) error {
	frames, fps, err := readY4M(y4m)
	if err != nil {
		return err
	}
	o.Width, o.Height = frames[0].Rect.Dx(), frames[0].Rect.Dy()
	if fps > 0 {
		o.FPS = fps
	}
	best := time.Duration(math.MaxInt64)
	var size int
	for rep := 0; rep < reps; rep++ {
		var buf bytes.Buffer
		t0 := time.Now()
		e, err := vp8enc.New(&buf, o)
		if err != nil {
			return err
		}
		for i, f := range frames {
			if err := e.AddFrame(f, time.Duration(i)*time.Second/time.Duration(o.FPS)); err != nil {
				return err
			}
		}
		if err := e.Close(); err != nil {
			return err
		}
		d := time.Since(t0)
		if d < best {
			best = d
		}
		size = buf.Len()
		if rep == reps-1 {
			if err := os.WriteFile(webm, buf.Bytes(), 0o644); err != nil {
				return err
			}
		}
	}
	fmt.Printf("vp8enc %dx%d %d frames: %.1f fps (best of %d, %v), %d bytes\n",
		o.Width, o.Height, len(frames), float64(len(frames))/best.Seconds(), reps, best.Round(time.Millisecond), size)
	return nil
}

func psnr(y4m, webm string) error {
	src, _, err := readY4M(y4m)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(webm)
	if err != nil {
		return err
	}
	r, err := container.NewReader(data)
	if err != nil {
		return err
	}
	vt := r.File().VideoTracks()
	if len(vt) != 1 {
		return fmt.Errorf("%d video tracks", len(vt))
	}
	ss, err := r.Samples(vt[0].ID)
	if err != nil {
		return err
	}
	dec, err := govpx.NewVP8Decoder(govpx.DecoderOptions{})
	if err != nil {
		return err
	}
	defer dec.Close()
	// A sample may last several frames (a dropped frame): the picture stays.
	var sumY, sumAll float64
	var shown int
	var last govpx.Image
	for i, s := range ss {
		if err := dec.Decode(s.Data); err != nil {
			return fmt.Errorf("sample %d: %w", i, err)
		}
		img, ok := dec.NextFrame()
		if !ok {
			return fmt.Errorf("sample %d: no picture", i)
		}
		last = img
		// Samples map to frames one to one unless frames were dropped.
		if shown < len(src) {
			py, pa := cmp(src[shown], last)
			sumY, sumAll, shown = sumY+py, sumAll+pa, shown+1
		}
	}
	fmt.Printf("%s: %d samples for %d frames, %d bytes, PSNR Y %.2f dB, Y+U+V %.2f dB (mean over frames)\n",
		webm, len(ss), len(src), len(data), sumY/float64(shown), sumAll/float64(shown))
	if len(ss) != len(src) {
		fmt.Printf("warning: %d samples against %d frames, frame pairing is by index\n", len(ss), len(src))
	}
	return nil
}

func cmp(a *image.YCbCr, b govpx.Image) (y, all float64) {
	w, h := b.Width, b.Height
	var sy, sc float64
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			d := float64(a.Y[j*a.YStride+i]) - float64(b.Y[j*b.YStride+i])
			sy += d * d
		}
	}
	cw, ch := (w+1)/2, (h+1)/2
	for j := 0; j < ch; j++ {
		for i := 0; i < cw; i++ {
			d := float64(a.Cb[j*a.CStride+i]) - float64(b.U[j*b.UStride+i])
			e := float64(a.Cr[j*a.CStride+i]) - float64(b.V[j*b.VStride+i])
			sc += d*d + e*e
		}
	}
	db := func(sse, n float64) float64 {
		if sse == 0 {
			return 99
		}
		return 10 * math.Log10(255*255*n/sse)
	}
	return db(sy, float64(w*h)), db(sy+sc, float64(w*h+2*cw*ch))
}
