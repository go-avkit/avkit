// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrMetadata means a Metadata cannot be written as it stands: a value that is
// not text, a cover with no picture in it or in a format no container here can
// label, or metadata handed over after the part of the file that states it has
// gone out.
var ErrMetadata = errors.New("container: invalid metadata")

// The picture formats a cover can be stated in. They are the three an MP4
// covr item can label (Apple's well-known data types 13, 14 and 27), and the
// first two are the only ones the Matroska attachments specification says a
// cover SHOULD use.
const (
	MIMEJPEG = "image/jpeg"
	MIMEPNG  = "image/png"
	MIMEBMP  = "image/bmp"
)

// Metadata is what a file says about itself as a whole, rather than about one
// of its tracks: its title, who made it, and the picture a player shows for it.
//
// Every field is optional and an empty string means "not stated". The same
// model is read out of and written into both containers that have one, each in
// its own spelling:
//
//	field        MP4 (moov/udta/meta, ilst)   Matroska
//	Title        ©nam                          Info/Title, or a TITLE tag
//	Artist       ©ART                          ARTIST
//	Album        ©alb                          ALBUM
//	Date         ©day                          DATE_RELEASED (DATE is read too)
//	Comment      ©cmt                          COMMENT
//	Description  desc                          DESCRIPTION
//	Genre        ©gen                          GENRE
//	Encoder      ©too                          ENCODER
//	Cover        covr                          an attachment, cover.jpg or cover.png
//
// An MPEG transport stream has no place for any of it: reading one yields an
// empty Metadata, and TSMuxer takes none.
type Metadata struct {
	Title       string
	Artist      string
	Album       string
	Date        string
	Comment     string
	Description string
	Genre       string
	Encoder     string
	// Cover is the picture a player shows for the file, nil when there is
	// none.
	Cover *Picture

	// mp4Items are the ilst items of an MP4 that none of the fields above
	// states -- a track number, a sort name, a freeform iTunes item --
	// kept as the bytes they were read as, so an MP4 output carries them back
	// unchanged. A Matroska output has nowhere to put them and leaves them out.
	mp4Items [][]byte
	// mkvTags are the file-level Matroska simple tags none of the fields above
	// states, kept the same way for a Matroska output.
	mkvTags []mkvSimpleTag
}

// Picture is an image carried in a file, as its bytes and its media type.
type Picture struct {
	// MIMEType is MIMEJPEG, MIMEPNG or MIMEBMP.
	MIMEType string
	Data     []byte
}

// IsZero reports whether md states nothing at all, not even an item this
// package carries without naming.
func (md Metadata) IsZero() bool {
	return md.Title == "" && md.Artist == "" && md.Album == "" && md.Date == "" &&
		md.Comment == "" && md.Description == "" && md.Genre == "" && md.Encoder == "" &&
		md.Cover == nil && len(md.mp4Items) == 0 && len(md.mkvTags) == 0
}

// metadataText pairs each text field with a name for the errors that refuse it.
func (md *Metadata) metadataText() []struct{ name, value string } {
	return []struct{ name, value string }{
		{"title", md.Title}, {"artist", md.Artist}, {"album", md.Album},
		{"date", md.Date}, {"comment", md.Comment}, {"description", md.Description},
		{"genre", md.Genre}, {"encoder", md.Encoder},
	}
}

// validate refuses what neither container could state faithfully.
//
// Both say their text is UTF-8 -- ilst data type 1, the Matroska UTF-8 element
// type -- so text that is not is refused rather than written as something a
// reader would have to guess the encoding of. A NUL is refused too: a Matroska
// reader, this package's own included, ends a string at the first one, so a
// value holding one would come back shorter than it went in.
func (md *Metadata) validate() error {
	for _, f := range md.metadataText() {
		if !isText(f.value) {
			return fmt.Errorf("%w: the %s is not UTF-8 text without NUL", ErrMetadata, f.name)
		}
	}
	if c := md.Cover; c != nil {
		if len(c.Data) == 0 {
			return fmt.Errorf("%w: the cover holds no picture", ErrMetadata)
		}
		if _, ok := coverTypes[c.MIMEType]; !ok {
			return fmt.Errorf("%w: a cover in %q cannot be labelled; use %s, %s or %s",
				ErrMetadata, c.MIMEType, MIMEJPEG, MIMEPNG, MIMEBMP)
		}
	}
	return nil
}

// isText reports whether s is something both containers can carry as text.
func isText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// coverTypes maps a cover's media type to the well-known data type an ilst
// data box labels it with (Apple, QuickTime File Format, "Well-known types"),
// and to the extension of the Matroska cover file name.
var coverTypes = map[string]struct {
	ilst      uint32
	extension string
}{
	MIMEJPEG: {ilstJPEG, "jpg"},
	MIMEPNG:  {ilstPNG, "png"},
	MIMEBMP:  {ilstBMP, "bmp"},
}

// normalMIME is a picture media type as this package states it, or "" when it
// is not one a cover can be. "image/jpg" is not a registered type and is
// written by enough tools to be worth reading as what it means.
func normalMIME(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "image/jpg" {
		mime = MIMEJPEG
	}
	if _, ok := coverTypes[mime]; ok {
		return mime
	}
	return ""
}

// setMetadata is the check every muxer makes before taking metadata: it has to
// be writable, and it has to arrive before what states it is written.
func setMetadata(md Metadata, closed, tooLate bool) (*Metadata, error) {
	switch {
	case closed:
		return nil, ErrClosed
	case tooLate:
		return nil, fmt.Errorf("%w: metadata cannot be set once writing has begun", ErrMetadata)
	}
	if err := md.validate(); err != nil {
		return nil, err
	}
	if md.IsZero() {
		return nil, nil
	}
	return &md, nil
}

// SetMetadata states what the file says about itself, in moov/udta/meta.
//
// The movie box goes out with the first fragment, so this has to come before
// the first sample; later it is refused. A Metadata that states nothing writes
// no udta at all.
func (m *Muxer) SetMetadata(md Metadata) error {
	got, err := setMetadata(md, m.closed, m.started)
	if err != nil {
		return err
	}
	m.metadata = got
	return nil
}

// SetMetadata states what the file says about itself, in moov/udta/meta.
//
// The movie box is written last, so this is taken at any time before Close. A
// Metadata that states nothing writes no udta at all.
func (m *ProgressiveMuxer) SetMetadata(md Metadata) error {
	got, err := setMetadata(md, m.closed, false)
	if err != nil {
		return err
	}
	m.metadata = got
	return nil
}

// SetMetadata states what the file says about itself: the title in Info, the
// rest as file-level Tags, and the cover as an attachment named cover.jpg,
// cover.png or cover.bmp.
//
// What precedes the first cluster is written with the first sample, so this
// has to come before it.
//
// ⛔ A WebM file carries no cover. The WebM guidelines admit no Attachments
// element, and a file whose tracks keep it WebM is written without one -- as
// ffmpeg's webm muxer does, which drops attachment streams with a warning. The
// text is written either way, since WebM admits Tags.
func (m *WebMMuxer) SetMetadata(md Metadata) error {
	got, err := setMetadata(md, m.closed, m.started)
	if err != nil {
		return err
	}
	m.metadata = got
	return nil
}

// ReplaceMetadata writes md in place of the metadata the input carries. Without
// it Remux, Cut, Concat and Join copy the metadata of their first input, and
// ReplaceMetadata(Metadata{}) is how a caller writes none.
func ReplaceMetadata(md Metadata) RemuxOption {
	return func(s *remuxSettings) {
		s.metadata = &md
	}
}

// outputMetadata is what a copy of srcs writes: what the caller said, or else
// what the first input carries.
func (s *remuxSettings) outputMetadata(srcs []*Reader) Metadata {
	if s.metadata != nil {
		return *s.metadata
	}
	return srcs[0].File().Metadata
}
