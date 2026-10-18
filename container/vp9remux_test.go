// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// vp9WebM writes a real WebM holding the VP9 frames the chain test uses.
func vp9WebM(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	id, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "vp09", Timescale: 90000, Width: 320, Height: 180,
		VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Sample{
		{Data: vp9KeyFrame, Duration: 3000, Sync: true},
		{Data: vp9InterFrame, Duration: 3000},
		{Data: vp9InterFrame, Duration: 3000},
	} {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// What #31 reported: remuxing a VP9 track out of Matroska failed with
// "vp09 needs a level; 0 is not one", because Matroska states no level and
// vpcC has no value for "unknown".
//
// The level is taken from the BITSTREAM, not derived from the container's
// width, height and frame rate. A container-side derivation would also have
// got a level, and it would have been a worse description: the profile would
// have stayed at zero and the colour at "unspecified", where the frame header
// states both.
func TestRemuxOfVP9FromMatroskaTakesItsConfigurationFromTheFrames(t *testing.T) {
	r, err := NewReader(vp9WebM(t))
	if err != nil {
		t.Fatal(err)
	}

	// The reader still says what the container says, and no more: this is the
	// position vp9chain_test defends, and it is unchanged.
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VPx == nil || cfg.VPx.Level != 0 {
		t.Fatalf("the reader invented a level the container cannot know: %+v", cfg.VPx)
	}

	var out bytes.Buffer
	if err := Remux(&out, r); err != nil {
		if strings.Contains(err.Error(), "needs a level") {
			t.Fatalf("still refused for want of a level: %v", err)
		}
		t.Fatalf("remux: %v", err)
	}

	// And what came out describes the track as the frames do.
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, err := back.TrackConfig(back.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.VPx == nil || got.VPx.Level == 0 {
		t.Fatalf("the output carries no level: %+v", got.VPx)
	}
	derived, err := ConfigFromSamples("vp09", mustSamples(t, r), SampleTimescale(cfg.Timescale))
	if err != nil {
		t.Fatal(err)
	}
	if got.VPx.Level != derived.VPx.Level || got.VPx.Profile != derived.VPx.Profile {
		t.Errorf("output vpcC = %+v; the frames say level %d profile %d",
			got.VPx, derived.VPx.Level, derived.VPx.Profile)
	}
}

// Join and JoinProgressive were the two operations the README named and that
// did not go through describeFromSamples: a VP9 track out of Matroska failed in
// both with "vp09 needs a level; 0 is not one" where Remux succeeded. They now
// write the vpcC the frames state, as Remux does.
func TestJoinOfVP9FromMatroskaTakesItsConfigurationFromTheFrames(t *testing.T) {
	src, err := NewReader(vp9WebM(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := src.TrackConfig(src.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	derived, err := ConfigFromSamples("vp09", mustSamples(t, src), SampleTimescale(cfg.Timescale))
	if err != nil {
		t.Fatal(err)
	}
	for name, join := range map[string]func(io.Writer, []*Reader, ...RemuxOption) error{
		"Join": Join, "JoinProgressive": JoinProgressive,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := NewReader(vp9WebM(t))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := join(&out, []*Reader{r}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			back, err := NewReader(out.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			got, err := back.TrackConfig(back.TrackIDs()[0])
			if err != nil {
				t.Fatal(err)
			}
			if got.VPx == nil || *got.VPx != *derived.VPx {
				t.Errorf("output vpcC = %+v; the frames say %+v", got.VPx, derived.VPx)
			}
		})
	}
}

// And a track whose frames cannot describe it is refused by Join too, naming
// the track, instead of reaching the muxer with a vpcC of zeroes.
func TestJoinOfVP9WithUndescribableSamplesIsRefused(t *testing.T) {
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	id, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "vp09", Timescale: 90000, Width: 320, Height: 180,
		VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{0xFF, 0xFF, 0xFF, 0xFF}, Duration: 3000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	err = Join(io.Discard, []*Reader{r})
	if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "states no level and its samples do not either") {
		t.Fatalf("err = %v, want the frames' refusal", err)
	}
}

func mustSamples(t *testing.T, r *Reader) []Sample {
	t.Helper()
	s, err := r.Samples(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A VP9 track whose samples cannot describe it either must still be refused,
// naming the track, rather than written with a vpcC of zeroes.
func TestRemuxOfVP9WithUndescribableSamplesIsRefused(t *testing.T) {
	r, err := NewReader(vp9WebM(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	// Not a VP9 frame header at all.
	_, err = describeFromSamples(cfg, []Sample{{Data: []byte{0xFF, 0xFF, 0xFF, 0xFF}, Duration: 3000, Sync: true}})
	if !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("samples that describe nothing were accepted: %v", err)
	}
}

// Everything that is not a levelless VP9 track passes through untouched, so
// this cannot quietly rewrite a configuration that was already right.
func TestDescribeFromSamplesLeavesEverythingElseAlone(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  TrackConfig
	}{
		{"another codec", TrackConfig{Codec: "avc1"}},
		{"VP9 with no vpcC at all", TrackConfig{Codec: "vp09"}},
		{"VP9 that already states a level", TrackConfig{Codec: "vp09", VPx: &VPxConfig{Level: 31}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := describeFromSamples(c.cfg, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.VPx != c.cfg.VPx {
				t.Error("the configuration was replaced")
			}
		})
	}
	// A levelless VP9 track with no samples has nothing to read, and must not
	// fail on that account: the muxer's own refusal is the right one to hear.
	cfg := TrackConfig{Codec: "vp09", VPx: &VPxConfig{Level: 0}}
	if _, err := describeFromSamples(cfg, nil); err != nil {
		t.Errorf("no samples should not be an error here: %v", err)
	}
	_ = io.Discard
}
