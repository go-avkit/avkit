// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The well-known data types of an ilst data box this package reads and writes:
// Apple, QuickTime File Format, "Metadata", table "Well-known data types".
// ffmpeg reads the same ones (libavformat/mov.c, mov_read_udta_string and
// mov_read_covr).
const (
	ilstUTF8     = 1
	ilstUTF16    = 2
	ilstUTF8Sort = 4
	ilstUTF16Srt = 5
	ilstJPEG     = 13
	ilstPNG      = 14
	ilstBMP      = 27
)

// mdirHandler is the handler type of the meta box an ilst lives in, and
// applManufacturer the manufacturer code iTunes and ffmpeg write beside it
// (libavformat/movenc.c, mov_write_itunes_hdlr_tag).
const (
	mdirHandler      = "mdir"
	applManufacturer = 'a'<<24 | 'p'<<16 | 'p'<<8 | 'l'
)

// ilstDataHeader is what precedes a value inside a data box: the type
// indicator, then the locale.
const ilstDataHeader = 8

// pngSignature opens every PNG file (RFC 2083, section 3.1).
var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// ilstTextItems are the ilst items the text fields map to, in the order ffmpeg
// writes them (movenc.c, mov_write_ilst_tag).
var ilstTextItems = []struct {
	name  string
	field func(*Metadata) *string
}{
	{"\xa9nam", func(md *Metadata) *string { return &md.Title }},
	{"\xa9ART", func(md *Metadata) *string { return &md.Artist }},
	{"\xa9alb", func(md *Metadata) *string { return &md.Album }},
	{"\xa9day", func(md *Metadata) *string { return &md.Date }},
	{"\xa9too", func(md *Metadata) *string { return &md.Encoder }},
	{"\xa9cmt", func(md *Metadata) *string { return &md.Comment }},
	{"\xa9gen", func(md *Metadata) *string { return &md.Genre }},
	{"desc", func(md *Metadata) *string { return &md.Description }},
}

// encodeItem is a box as the bytes it encodes to. It is a seam because an item
// mp4ff has just decoded encodes again without fault, and the guard against
// one that does not can only be reached by staging it. (subtitle.go's
// encodeBox is the WebVTT encoder's seam, another one.)
var encodeItem = func(b mp4.Box) ([]byte, error) {
	var buf bytes.Buffer
	err := b.Encode(&buf)
	return buf.Bytes(), err
}

// mp4Metadata reads the iTunes-style metadata of a movie: the items of every
// ilst inside a meta box whose handler is mdir, inside a udta of the movie
// box.
//
// mp4ff decodes the containers -- udta, meta, hdlr, ilst -- and only the
// items are read here, because mp4ff decodes most of them as boxes it does not
// know: it keeps their bytes and loses the type of their data box, which is
// what tells a JPEG cover from a PNG one. Each item is therefore encoded back
// to the bytes it was read from and read as Apple specifies it.
//
// Only that one place is read. A QuickTime-style udta string (©nam directly in
// udta, with a length and a language), a meta box keyed by mdta, and a track's
// own udta are not.
func mp4Metadata(moov *mp4.MoovBox) Metadata {
	var md Metadata
	for _, child := range moov.Children {
		udta, ok := child.(*mp4.UdtaBox)
		if !ok {
			continue
		}
		for _, c := range udta.Children {
			meta, ok := c.(*mp4.MetaBox)
			if !ok || meta.Hdlr == nil || meta.Hdlr.HandlerType != mdirHandler {
				continue
			}
			for _, mc := range meta.Children {
				ilst, ok := mc.(*mp4.IlstBox)
				if !ok {
					continue
				}
				for _, item := range ilst.Children {
					if raw, err := encodeItem(item); err == nil {
						md.addIlstItem(raw)
					}
				}
			}
		}
	}
	return md
}

// addIlstItem reads one ilst item into the field it states, or keeps it whole
// when it states none of them -- or states one in a form this does not read,
// or states one already filled -- so an MP4 output can carry it back as it was.
func (md *Metadata) addIlstItem(raw []byte) {
	name, body, _, ok := splitBox(raw)
	if !ok {
		return
	}
	if name == "covr" {
		if pic := ilstPicture(body); pic != nil && md.Cover == nil {
			md.Cover = pic
			return
		}
	}
	for _, item := range ilstTextItems {
		if item.name != name {
			continue
		}
		field := item.field(md)
		if text, ok := ilstText(body); ok && *field == "" {
			*field = text
			return
		}
	}
	md.mp4Items = append(md.mp4Items, raw)
}

// splitBox reads the box at the head of b: its type, its content, and what
// follows it. A box whose size points past b, or is smaller than its own
// header, is not one, and ok is false.
//
// ⛔ Nothing is allocated from a size read here: content and rest are slices of
// b, so a size field of four gigabytes costs nothing but the refusal.
func splitBox(b []byte) (name string, content, rest []byte, ok bool) {
	if len(b) < 8 {
		return "", nil, nil, false
	}
	size, header := uint64(binary.BigEndian.Uint32(b)), uint64(8)
	switch size {
	case 0: // the box runs to the end of what holds it
		size = uint64(len(b))
	case 1: // a 64-bit size follows the type
		if len(b) < 16 {
			return "", nil, nil, false
		}
		size, header = binary.BigEndian.Uint64(b[8:]), 16
	}
	if size < header || size > uint64(len(b)) {
		return "", nil, nil, false
	}
	return string(b[4:8]), b[header:size], b[size:], true
}

// ilstFirstData is the type indicator and the value of the first data box of
// an item, and false when the item holds none that can be read.
func ilstFirstData(body []byte) (uint32, []byte, bool) {
	for len(body) > 0 {
		name, content, rest, ok := splitBox(body)
		if !ok {
			return 0, nil, false
		}
		if name == "data" && len(content) >= ilstDataHeader {
			return binary.BigEndian.Uint32(content), content[ilstDataHeader:], true
		}
		body = rest
	}
	return 0, nil, false
}

// ilstText is the text an item's first data box states, when it states text
// both containers can carry.
//
// Only the first data box is read. An item may hold several -- ffmpeg joins
// them with a semicolon -- and the second and later values are not carried.
func ilstText(body []byte) (string, bool) {
	typ, value, ok := ilstFirstData(body)
	if !ok {
		return "", false
	}
	var text string
	switch typ {
	case ilstUTF8, ilstUTF8Sort:
		text = string(value)
	case ilstUTF16, ilstUTF16Srt:
		if len(value)%2 != 0 {
			return "", false
		}
		units := make([]uint16, len(value)/2)
		for i := range units {
			units[i] = binary.BigEndian.Uint16(value[2*i:])
		}
		text = string(utf16.Decode(units))
	default:
		return "", false
	}
	return text, isText(text)
}

// ilstPicture is the picture a covr item's first data box holds, or nil when
// it holds none of the three types a cover is labelled with.
//
// The label is not trusted between JPEG and PNG: as ffmpeg does (mov.c,
// mov_read_covr), the bytes decide, since writers have been found to label a
// PNG as a JPEG and nothing reads the picture by its label. A covr item holding
// several pictures gives the first.
func ilstPicture(body []byte) *Picture {
	typ, value, ok := ilstFirstData(body)
	if !ok || len(value) == 0 {
		return nil
	}
	switch typ {
	case ilstJPEG, ilstPNG:
		if bytes.HasPrefix(value, pngSignature) {
			return &Picture{MIMEType: MIMEPNG, Data: value}
		}
		return &Picture{MIMEType: MIMEJPEG, Data: value}
	case ilstBMP:
		return &Picture{MIMEType: MIMEBMP, Data: value}
	}
	return nil
}

// mp4Udta is the udta box stating md: a meta box with an mdir handler and the
// ilst that holds the items, which is the shape ffmpeg writes for an MP4
// (movenc.c, mov_write_udta_tag and mov_write_meta_tag) and the one Apple
// specifies. It is nil when md states nothing an MP4 can carry.
func mp4Udta(md *Metadata) *mp4.UdtaBox {
	if md == nil {
		return nil
	}
	ilst := &mp4.IlstBox{}
	for _, item := range ilstTextItems {
		if value := *item.field(md); value != "" {
			ilst.AddChild(ilstItem(item.name, ilstUTF8, []byte(value)))
		}
	}
	if c := md.Cover; c != nil {
		ilst.AddChild(ilstItem("covr", coverTypes[c.MIMEType].ilst, c.Data))
	}
	for _, raw := range md.mp4Items {
		name, body, _, _ := splitBox(raw)
		ilst.AddChild(mp4.CreateUnknownBox(name, uint64(8+len(body)), body))
	}
	if len(ilst.Children) == 0 {
		return nil
	}
	hdlr := &mp4.HdlrBox{HandlerType: mdirHandler, Reserved: [3]uint32{applManufacturer}}
	meta := mp4.CreateMetaBox(0, hdlr)
	meta.AddChild(ilst)
	udta := &mp4.UdtaBox{}
	udta.AddChild(meta)
	return udta
}

// ilstItem is one item holding one data box: the type indicator, a locale of
// zero, which means "any", and the value.
func ilstItem(name string, typ uint32, value []byte) mp4.Box {
	data := make([]byte, 8+ilstDataHeader+len(value))
	binary.BigEndian.PutUint32(data, uint32(len(data)))
	copy(data[4:], "data")
	binary.BigEndian.PutUint32(data[8:], typ)
	copy(data[16:], value)
	return mp4.CreateUnknownBox(name, uint64(8+len(data)), data)
}

// encodeInit writes the initialisation segment, with the metadata in its movie
// box. The udta is added here, once every track has been, so that it follows
// them.
func (m *Muxer) encodeInit() error {
	if udta := mp4Udta(m.metadata); udta != nil {
		m.init.Moov.AddChild(udta)
	}
	return m.init.Encode(m.w)
}
