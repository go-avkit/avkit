// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package media turns a container.Segmentation into the files an HLS or DASH
// presentation is served from: the segments, the master and media playlists, and
// the MPD.
//
// It is a module of its own (github.com/go-avkit/avkit/media) because it is the
// one place that needs both halves of the packaging: segmenting is
// container.Segment in go-avkit/avkit, and the playlist and manifest writers are
// the hls and dash packages of go-streamkit/streamkit. Neither of those depends
// on the other, and neither gains a dependency from this package: avkit stays
// free of streamkit's HTTP client, utls and x/net, and streamkit, a transport
// kit, stays free of every container parser. The Segmentation and the writers'
// inputs are plain data, so this package only maps one onto the other.
//
// A presentation is VOD, with one video track at most, any number of audio
// tracks (each an alternative rendition) and WebVTT subtitle tracks. Audio-only
// presentations, several video tracks (an encoding ladder), live and encryption
// are not covered.
package media

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/go-avkit/avkit/container"
	"github.com/go-streamkit/streamkit/dash"
	"github.com/go-streamkit/streamkit/hls"
)

// File names of what is written beside the segments.
const (
	MasterName   = "master.m3u8"
	ManifestName = "manifest.mpd"
)

// TextBandwidth is the bandwidth stated for a WebVTT representation, which has
// no bit rate of its own and for which an MPD requires one.
const TextBandwidth = 256

// ErrPresentation means the segmentation cannot be described as a presentation.
var ErrPresentation = errors.New("media: cannot describe this segmentation")

func fail(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPresentation, fmt.Sprintf(format, args...))
}

// PlaylistName is the file name of a track's media playlist.
func PlaylistName(t *container.SegmentedTrack) string { return t.Name + ".m3u8" }

// check refuses a segmentation the mapping does not cover.
func check(seg *container.Segmentation) (video *container.SegmentedTrack, err error) {
	for i := range seg.Tracks {
		t := &seg.Tracks[i]
		if len(t.Segments) == 0 || t.Timescale == 0 {
			return nil, fail("track %s has no segment or no timescale", t.Name)
		}
		if t.Kind == container.Video {
			if video != nil {
				return nil, fail("more than one video track (an encoding ladder) is not covered")
			}
			video = t
		}
	}
	if video == nil {
		return nil, fail("an audio-only presentation is not covered")
	}
	return video, nil
}

func seconds(ticks uint64, timescale uint32) float64 { return float64(ticks) / float64(timescale) }

// HLSFiles is a presentation's playlists.
type HLSFiles struct {
	// Master is master.m3u8.
	Master []byte
	// Media holds each track's media playlist, by file name (PlaylistName).
	Media map[string][]byte
}

// HLS writes the master playlist and one media playlist per track: the video
// track is the variant, each audio track an AUDIO rendition (the first the
// default) and each subtitle track a SUBTITLES rendition. The variant's
// BANDWIDTH is the video's peak plus the largest audio peak, its codecs those of
// the video and of every audio track.
func HLS(seg *container.Segmentation) (*HLSFiles, error) {
	video, err := check(seg)
	if err != nil {
		return nil, err
	}
	out := &HLSFiles{Media: map[string][]byte{}}
	master := hls.MasterPlaylist{IndependentSegments: true}
	inf := hls.StreamInf{
		URI: PlaylistName(video), Width: video.Config.Width, Height: video.Config.Height,
		Codecs: []string{video.Codecs},
	}
	peak, avg := video.Bandwidth, video.AverageBandwidth
	var audioPeak, audioAvg uint64
	var audios, texts int
	for i := range seg.Tracks {
		t := &seg.Tracks[i]
		mp := hls.MediaPlaylist{IndependentSegments: t.Kind == container.Video}
		if t.Init != nil {
			mp.InitURI = t.InitName()
		}
		for k, s := range t.Segments {
			mp.Segments = append(mp.Segments, hls.MediaSegment{URI: t.SegmentName(k), Duration: seconds(s.Duration, t.Timescale)})
		}
		var b bytesBuffer
		if err := mp.Write(&b); err != nil {
			return nil, fmt.Errorf("track %s: %w", t.Name, err)
		}
		out.Media[PlaylistName(t)] = b.b
		switch t.Kind {
		case container.Audio:
			audios++
			audioPeak, audioAvg = max(audioPeak, t.Bandwidth), max(audioAvg, t.AverageBandwidth)
			if !slices.Contains(inf.Codecs, t.Codecs) {
				inf.Codecs = append(inf.Codecs, t.Codecs)
			}
			master.Renditions = append(master.Renditions, hls.Rendition{
				Type: "AUDIO", GroupID: "audio", Name: renditionName(t), Language: language(t),
				URI: PlaylistName(t), Default: audios == 1, Channels: t.Config.Channels,
			})
		case container.Subtitle:
			texts++
			lang := language(t)
			if lang == "" {
				lang = "und"
			}
			master.Renditions = append(master.Renditions, hls.Rendition{
				Type: "SUBTITLES", GroupID: "subs", Name: renditionName(t), Language: lang,
				URI: PlaylistName(t), Autoselect: true,
			})
		}
	}
	if audios > 0 {
		inf.Audio = "audio"
	}
	if texts > 0 {
		inf.Subtitles = "subs"
	}
	peak, avg = peak+audioPeak, avg+audioAvg
	if peak == 0 || peak > math.MaxUint32 || avg > math.MaxUint32 {
		return nil, fail("bandwidth %d (average %d) is zero or does not fit 32 bits", peak, avg)
	}
	inf.Bandwidth, inf.AverageBandwidth = uint32(peak), uint32(avg)
	master.Variants = []hls.StreamInf{inf}
	var b bytesBuffer
	if err := master.Write(&b); err != nil {
		return nil, err
	}
	out.Master = b.b
	return out, nil
}

// bytesBuffer collects what a writer writes.
type bytesBuffer struct{ b []byte }

func (w *bytesBuffer) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }

// language is the track's language, or "" for the undetermined one.
func language(t *container.SegmentedTrack) string {
	if t.Config.Language == "und" {
		return ""
	}
	return t.Config.Language
}

// renditionName is what a player shows: the track's file-safe name, with the
// language when there is one.
func renditionName(t *container.SegmentedTrack) string {
	if l := language(t); l != "" {
		return t.Name + " (" + l + ")"
	}
	return t.Name
}

// DASH writes the static MPD: one adaptation set per track, each with a
// SegmentTemplate over the track's file names and a SegmentTimeline of its
// segments.
func DASH(seg *container.Segmentation) ([]byte, error) {
	if _, err := check(seg); err != nil {
		return nil, err
	}
	var p dash.Presentation
	for i := range seg.Tracks {
		t := &seg.Tracks[i]
		spans := make([]dash.Span, len(t.Segments))
		var longest uint64
		for k, s := range t.Segments {
			spans[k] = dash.Span{Start: s.Start, Duration: s.Duration}
			longest = max(longest, s.Duration)
		}
		if d := time.Duration(t.Duration() * float64(time.Second)); d > p.Duration {
			p.Duration = d
		}
		if d := time.Duration(seconds(longest, t.Timescale) * float64(time.Second)); d > p.MaxSegmentDuration {
			p.MaxSegmentDuration = d
		}
		bandwidth := t.Bandwidth
		if t.Kind == container.Subtitle {
			bandwidth = TextBandwidth
		}
		if bandwidth == 0 || bandwidth > math.MaxUint32 {
			return nil, fail("track %s: bandwidth %d is zero or does not fit 32 bits", t.Name, bandwidth)
		}
		rep := dash.Representation{
			ID: t.Name, Codecs: t.Codecs, Bandwidth: uint32(bandwidth), Timescale: t.Timescale,
			Media: t.Name + "_$Number%05d$." + extension(t), Timeline: dash.Timeline(spans),
		}
		if t.Init != nil {
			rep.InitURI = t.InitName()
		}
		switch t.Kind {
		case container.Video:
			rep.Width, rep.Height = t.Config.Width, t.Config.Height
			if t.Config.SARWidth > 0 && t.Config.SARHeight > 0 {
				rep.SAR = fmt.Sprintf("%d:%d", t.Config.SARWidth, t.Config.SARHeight)
			}
		case container.Audio:
			rep.AudioSamplingRate, rep.AudioChannels = t.Config.SampleRate, t.Config.Channels
		}
		if t.Kind == container.Subtitle {
			rep.Codecs = "" // plain WebVTT files state none
		}
		p.AdaptationSets = append(p.AdaptationSets, dash.AdaptationSet{
			ContentType: contentType(t.Kind), MimeType: t.MIME, Lang: language(t),
			Representations: []dash.Representation{rep},
		})
	}
	var b bytesBuffer
	if err := p.Write(&b); err != nil {
		return nil, err
	}
	return b.b, nil
}

func extension(t *container.SegmentedTrack) string {
	if t.Kind == container.Subtitle {
		return "vtt"
	}
	return "m4s"
}

func contentType(k container.Kind) string {
	if k == container.Subtitle {
		return "text"
	}
	return k.String()
}

// WriteDir writes the whole presentation into dir: the segments (through
// Segmentation.WriteDir, which cannot leave dir), the playlists and the MPD. The
// manifest and every playlist are built before the first file is written, so a
// segmentation that cannot be described leaves dir untouched.
func WriteDir(dir string, seg *container.Segmentation) error {
	files, err := HLS(seg)
	if err != nil {
		return err
	}
	mpd, err := DASH(seg)
	if err != nil {
		return err
	}
	if _, err := seg.WriteDir(dir); err != nil {
		return err
	}
	// The names are the constants above and PlaylistName of tracks whose names
	// passed the writers' checks (DASH admits letters, digits, _ and - only),
	// so they are plain file names.
	if err := os.WriteFile(filepath.Join(dir, MasterName), files.Master, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), mpd, 0o644); err != nil {
		return err
	}
	for name, data := range files.Media {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
