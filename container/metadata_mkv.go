// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

// mkvFileLevel is the TargetTypeValue of tags about the file as a whole: 50,
// ALBUM for audio and MOVIE or EPISODE for video (RFC 9559, section
// 5.1.8.1.1.1; Matroska tagging, "Target Types"). It is also the value a
// Targets element that states none has.
const mkvFileLevel = 50

// The Matroska tag names the text fields map to (Matroska tagging, "Assigned
// Tags", and the IANA Matroska Tags Names registry).
//
// ALBUM is not an assigned name. It is the one ffmpeg writes and reads for an
// album (libavformat/matroskaenc.c, mkv_write_simpletag, upper-cases the
// key), and it is used here because a reader that knows the album knows it by
// that name; the assigned spelling, a TITLE at a level above the item's, needs
// a level for the item that a file of one movie does not have.
//
// DATE is not assigned either, and is READ for the same reason: it is what
// ffmpeg writes for its "date" key. DATE_RELEASED is what is written.
const (
	mkvTagTitle        = "TITLE"
	mkvTagDateFallback = "DATE"
)

var mkvTextTags = []struct {
	name  string
	field func(*Metadata) *string
}{
	{"ARTIST", func(md *Metadata) *string { return &md.Artist }},
	{"ALBUM", func(md *Metadata) *string { return &md.Album }},
	{"DATE_RELEASED", func(md *Metadata) *string { return &md.Date }},
	{"COMMENT", func(md *Metadata) *string { return &md.Comment }},
	{"DESCRIPTION", func(md *Metadata) *string { return &md.Description }},
	{"GENRE", func(md *Metadata) *string { return &md.Genre }},
	{"ENCODER", func(md *Metadata) *string { return &md.Encoder }},
}

// mkvCoverUID is the FileUID of the cover attachment this package writes. A
// fixed one keeps the output reproducible byte for byte; it has only to be
// unique among the file's attachments, and there is one.
const mkvCoverUID = 1

// The Tags and Attachments elements, as both the readers and the muxer state
// them (RFC 9559, sections 5.1.6 and 5.1.8).
type mkvTagsElement struct {
	Tag []mkvTag `ebml:"Tag"`
}

type mkvTag struct {
	Targets   mkvTargets     `ebml:"Targets"`
	SimpleTag []mkvSimpleTag `ebml:"SimpleTag"`
}

// mkvTargets says what a tag is about. The slices tell an element that is
// absent from one stated as zero, which for TargetTypeValue is the difference
// between the default of 50 and a value of its own.
type mkvTargets struct {
	TargetTypeValue  []uint64 `ebml:"TargetTypeValue,omitempty"`
	TagTrackUID      []uint64 `ebml:"TagTrackUID,omitempty"`
	TagEditionUID    []uint64 `ebml:"TagEditionUID,omitempty"`
	TagChapterUID    []uint64 `ebml:"TagChapterUID,omitempty"`
	TagAttachmentUID []uint64 `ebml:"TagAttachmentUID,omitempty"`
}

// mkvSimpleTag is one name and its value. TagString is a slice so that an
// empty string -- which the tagging specification gives a meaning of its own,
// "does not apply at this level" -- is written as one rather than left out.
// A SimpleTag nested in another is not read.
type mkvSimpleTag struct {
	TagName     string   `ebml:"TagName"`
	TagLanguage string   `ebml:"TagLanguage,omitempty"`
	TagString   []string `ebml:"TagString,omitempty"`
	TagBinary   []byte   `ebml:"TagBinary,omitempty"`
}

type mkvAttachments struct {
	AttachedFile []mkvAttachedFile `ebml:"AttachedFile"`
}

type mkvAttachedFile struct {
	FileName     string `ebml:"FileName"`
	FileMimeType string `ebml:"FileMimeType"`
	FileData     []byte `ebml:"FileData"`
	FileUID      uint64 `ebml:"FileUID"`
}

// mkvMetadata reads the file-level metadata of a Matroska segment.
//
// A tag is about the file when its Targets names no track, edition, chapter or
// attachment (a UID of zero names all of them, which is the same thing) and
// its level is 50 or unstated. Tags about anything else, nested simple tags,
// and every attachment but the cover are not read.
//
// A TITLE tag takes precedence over Info/Title, as the specification says a
// tag does over the element it duplicates (RFC 9559, "Tags Precedence") and as
// ffmpeg reads it. The first value of a name is the one a field takes; a
// second is kept with the names this does not read, so a Matroska output
// carries it back.
func mkvMetadata(title string, tags []mkvTagsElement, attachments []mkvAttachments) Metadata {
	var md Metadata
	if isText(title) {
		md.Title = title
	}
	titled, dated := false, ""
	for _, element := range tags {
		for _, tag := range element.Tag {
			if !aboutTheFile(tag.Targets) {
				continue
			}
			for _, st := range tag.SimpleTag {
				text, isString := mkvTagText(st)
				switch {
				case st.TagName == mkvTagTitle && isString && !titled:
					md.Title, titled = text, true
				case st.TagName == mkvTagDateFallback && isString && dated == "":
					dated = text
				case !md.takeMkvTag(st.TagName, text, isString):
					md.mkvTags = append(md.mkvTags, st)
				}
			}
		}
	}
	if md.Date == "" {
		md.Date = dated
	} else if dated != "" {
		md.mkvTags = append(md.mkvTags, mkvSimpleTag{TagName: mkvTagDateFallback, TagString: []string{dated}})
	}
	md.Cover = mkvCover(attachments)
	return md
}

// takeMkvTag fills the field a tag name maps to, and reports whether it did.
func (md *Metadata) takeMkvTag(name, text string, isString bool) bool {
	for _, tag := range mkvTextTags {
		if tag.name != name {
			continue
		}
		field := tag.field(md)
		if !isString || *field != "" {
			return false
		}
		*field = text
		return true
	}
	return false
}

// mkvTagText is a simple tag's value when it is non-empty text both
// containers can carry.
func mkvTagText(st mkvSimpleTag) (string, bool) {
	if len(st.TagString) == 0 || st.TagString[0] == "" || !isText(st.TagString[0]) {
		return "", false
	}
	return st.TagString[0], true
}

// aboutTheFile reports whether a tag's Targets designate the file as a whole.
func aboutTheFile(t mkvTargets) bool {
	for _, uids := range [][]uint64{t.TagTrackUID, t.TagEditionUID, t.TagChapterUID, t.TagAttachmentUID} {
		for _, uid := range uids {
			if uid != 0 {
				return false
			}
		}
	}
	return len(t.TargetTypeValue) == 0 || t.TargetTypeValue[0] == mkvFileLevel
}

// mkvCover is the attachment that is the file's cover: the one named as the
// attachments specification names the normal cover, cover.jpg or cover.png
// (cover.bmp is accepted beside them), or else the first picture attached.
// Only a picture in a format a cover can be labelled with is considered.
func mkvCover(attachments []mkvAttachments) *Picture {
	var first *Picture
	for _, element := range attachments {
		for _, f := range element.AttachedFile {
			mime := normalMIME(f.FileMimeType)
			if mime == "" || len(f.FileData) == 0 {
				continue
			}
			pic := &Picture{MIMEType: mime, Data: f.FileData}
			if f.FileName == "cover."+coverTypes[mime].extension {
				return pic
			}
			if first == nil {
				first = pic
			}
		}
	}
	return first
}

// mkvTagsOf is the Tags element stating md, or nothing when md states nothing
// a tag carries. The title is not among them: it goes in Info/Title, where
// every Matroska reader looks for it, and which the specification recommends
// over a tag for maximum compatibility (RFC 9559, "Tags Precedence"), as
// ffmpeg does (matroskaenc.c, mkv_check_tag_name).
func mkvTagsOf(md *Metadata) []mkvTagsElement {
	if md == nil {
		return nil
	}
	var simple []mkvSimpleTag
	for _, tag := range mkvTextTags {
		if value := *tag.field(md); value != "" {
			simple = append(simple, mkvSimpleTag{TagName: tag.name, TagString: []string{value}})
		}
	}
	simple = append(simple, md.mkvTags...)
	if len(simple) == 0 {
		return nil
	}
	return []mkvTagsElement{{Tag: []mkvTag{{
		Targets:   mkvTargets{TargetTypeValue: []uint64{mkvFileLevel}},
		SimpleTag: simple,
	}}}}
}

// mkvAttachmentsOf is the Attachments element carrying md's cover, named as
// the attachments specification names a normal cover, or nothing when there is
// no cover or the file is a WebM, which admits no attachment.
func mkvAttachmentsOf(md *Metadata, docType string) []mkvAttachments {
	if md == nil || md.Cover == nil || docType == "webm" {
		return nil
	}
	return []mkvAttachments{{AttachedFile: []mkvAttachedFile{{
		FileName:     "cover." + coverTypes[md.Cover.MIMEType].extension,
		FileMimeType: md.Cover.MIMEType,
		FileData:     md.Cover.Data,
		FileUID:      mkvCoverUID,
	}}}}
}

// segmentHead is what precedes the clusters inside the segment: Info, Tracks,
// and the attachments and tags the metadata calls for, in the order ffmpeg
// writes them (matroskaenc.c, mkv_write_header).
func (m *WebMMuxer) segmentHead(duration float64) webmHeadDoc {
	return webmHeadDoc{
		Info:        m.info(duration),
		Tracks:      webmTracks{TrackEntry: m.trackEntries()},
		Attachments: mkvAttachmentsOf(m.metadata, m.ebmlHeader().DocType),
		Tags:        mkvTagsOf(m.metadata),
	}
}

// mkvTitleOf is the Info/Title stating md's title.
func mkvTitleOf(md *Metadata) string {
	if md == nil {
		return ""
	}
	return md.Title
}
