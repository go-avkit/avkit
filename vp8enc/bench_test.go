// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package vp8enc_test

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
	"testing"
	"time"

	"github.com/go-avkit/avkit/container"
	"github.com/go-avkit/avkit/vp8enc"
	"github.com/go-avkit/avkit/vp8enc/internal/uiseq"
	"github.com/thesyncim/govpx"
)

// TestMeasure is the measuring harness against libvpx, idle unless -cmd is
// given (go test -run TestMeasure -args -cmd gen ...):
//
//	gen   writes the UI sequence as a Y4M file, which vpxenc and ffmpeg read
//	enc   encodes a Y4M file with vp8enc, reports speed and size
//	psnr  decodes a WebM file with govpx and compares it with the Y4M
var (
	mCmd    = flag.String("cmd", "", "TestMeasure: gen, enc or psnr")
	mY4M    = flag.String("y4m", "seq.y4m", "Y4M file")
	mWebM   = flag.String("webm", "out.webm", "WebM file")
	mN      = flag.Int("n", 300, "frames (gen)")
	mW      = flag.Int("w", 1280, "width (gen)")
	mH      = flag.Int("h", 720, "height (gen)")
	mFPS    = flag.Int("fps", 30, "frame rate")
	mKbps   = flag.Int("kbps", 500, "target bitrate (enc)")
	mCPU    = flag.Int("cpu", 0, "CPUUsed (enc)")
	mKF     = flag.Int("kf", 120, "keyframe interval (enc)")
	mScreen = flag.Bool("screen", true, "screen content mode (enc)")
	mRT     = flag.Bool("realtime", false, "realtime deadline (enc)")
	mReps   = flag.Int("reps", 1, "encode repetitions, best time reported (enc)")
)

func TestMeasure(t *testing.T) {
	var err error
	switch *mCmd {
	case "":
		t.Skip("measuring harness: pass -args -cmd gen|enc|psnr")
	case "gen":
		err = genY4M(*mY4M, *mN, *mW, *mH, *mFPS)
	case "enc":
		err = encY4M(*mY4M, *mWebM, vp8enc.Options{FPS: *mFPS, BitrateKbps: *mKbps, CPUUsed: *mCPU,
			KeyFrameInterval: *mKF, ScreenContent: *mScreen, Realtime: *mRT}, *mReps)
	case "psnr":
		err = psnrWebM(*mY4M, *mWebM)
	default:
		t.Fatalf("unknown -cmd %q", *mCmd)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func genY4M(path string, n, w, h, fps int) error {
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

func encY4M(y4m, webm string, o vp8enc.Options, reps int) error {
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
		best = min(best, d)
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

func psnrWebM(y4m, webm string) error {
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
	var sumY, sumAll float64
	for i, s := range ss {
		if i >= len(src) {
			break
		}
		if err := dec.Decode(s.Data); err != nil {
			return fmt.Errorf("sample %d: %w", i, err)
		}
		img, ok := dec.NextFrame()
		if !ok {
			return fmt.Errorf("sample %d: no picture", i)
		}
		py, pa := cmpFrame(src[i], img)
		sumY, sumAll = sumY+py, sumAll+pa
	}
	n := float64(min(len(ss), len(src)))
	fmt.Printf("%s: %d samples for %d frames, %d bytes, PSNR Y %.2f dB, Y+U+V %.2f dB (mean over frames)\n",
		webm, len(ss), len(src), len(data), sumY/n, sumAll/n)
	return nil
}

func cmpFrame(a *image.YCbCr, b govpx.Image) (y, all float64) {
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
