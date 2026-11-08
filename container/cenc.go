// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Common Encryption (CENC) of fragmented MP4 and CMAF, clear key only.
//
// The cipher work is mp4ff's (mp4/crypto.go, v0.62.0): this file wraps it, adds
// the checks a caller-supplied configuration and an untrusted file need, and
// leaves no licence logic (no key server, no DRM): the caller brings the key and,
// if a DRM system needs them, ready-made pssh boxes.
//
// Sources opened: mp4ff's mp4/crypto.go and its InitProtect/EncryptFragment,
// DecryptInit/DecryptFragment; W3C "Encrypted Media Extensions - ISO Common
// Encryption ('cenc') Protection Scheme" (w3.org/TR/eme-stream-mp4: schemes cenc
// and cbcs, tenc default KID, pssh boxes kept together); Apple's "MPEG-2 Stream
// Encryption Format for HTTP Live Streaming" (Sample-AES: NAL unit type byte and
// 31 bytes clear, 1:9 skip pattern of 16-byte blocks, IV reset per protected
// block, audio as one contiguous protected section). ISO/IEC 23001-7 itself is
// paywalled and was not read; what it says here is what mp4ff and the W3C text
// repeat of it: cenc is AES-128 CTR over the protected ranges of each sample,
// with a per-sample IV of 8 or 16 bytes and subsample (clear, protected) byte
// counts in senc; cbcs is AES-128 CBC with a constant IV and, for video, a 1:9
// crypt:skip block pattern inside each protected range, which starts after the
// slice header; a NAL unit's length prefix and header, and a slice's header, are
// always clear. The oracle is ffmpeg's mov demuxer (-decryption_key); see
// testdata/cenc.

// EncryptionScheme is a Common Encryption scheme type (the schm box's four
// characters).
type EncryptionScheme string

const (
	// SchemeCENC is AES-128 CTR; video is encrypted in subsamples that keep the
	// NAL unit headers and slice headers clear.
	SchemeCENC EncryptionScheme = "cenc"
	// SchemeCBCS is AES-128 CBC with a constant IV; video uses the 1:9 pattern,
	// audio is encrypted whole (a pattern of 0:0).
	SchemeCBCS EncryptionScheme = "cbcs"
)

// Limits on what is read from a file or a caller.
const (
	// MaxPSSHBoxes and MaxPSSHSize bound the pssh boxes a caller passes.
	MaxPSSHBoxes = 16
	MaxPSSHSize  = 1 << 20
)

// Errors reported by encryption and decryption.
var (
	// ErrEncryption means an Encryption is not valid: a key that is not 16
	// bytes, an IV that is not 8 or 16, an unknown scheme, a bad pssh box.
	ErrEncryption = errors.New("container: invalid encryption configuration")
	// ErrEncrypt means a track could not be encrypted, with the reason.
	ErrEncrypt = errors.New("container: cannot encrypt")
	// ErrDecrypt means data could not be decrypted: not protected, malformed
	// protection boxes, or a subsample map that overruns its sample.
	ErrDecrypt = errors.New("container: cannot decrypt")
)

// ContentKey is a 16-byte AES content key. It never prints: every formatting
// verb, and log/slog, show a placeholder, and no error of this package holds its
// bytes.
type ContentKey struct{ b []byte }

// NewContentKey checks that b is 16 bytes and copies it.
func NewContentKey(b []byte) (ContentKey, error) {
	if len(b) != 16 {
		return ContentKey{}, fmt.Errorf("%w: a content key is 16 bytes, got %d", ErrEncryption, len(b))
	}
	return ContentKey{b: bytes.Clone(b)}, nil
}

// String is a placeholder.
func (ContentKey) String() string { return "ContentKey(redacted)" }

// GoString is a placeholder.
func (ContentKey) GoString() string { return "ContentKey(redacted)" }

// LogValue keeps the key out of structured logs.
func (ContentKey) LogValue() slog.Value { return slog.StringValue("redacted") }

// Encryption states how tracks are protected.
type Encryption struct {
	Scheme EncryptionScheme
	// Key is the content key, and KID the key identifier that tenc states and
	// that a licence is requested with.
	Key ContentKey
	KID [16]byte
	// IV is the initial value: for cenc the first per-sample IV (8 bytes, the
	// size CMAF asks for, or 16), for cbcs the constant IV (8 or 16 bytes).
	// Empty means a random one of 8 (cenc) or 16 (cbcs) bytes. A CTR IV must
	// not be reused with a key: when several tracks are encrypted together, each
	// starts from this IV with its position added to the first four bytes.
	IV []byte
	// PSSH are complete pssh boxes (ISO/IEC 23001-7 §8.1), written into the
	// initialisation segment as given.
	PSSH [][]byte
}

// protection is an Encryption checked and ready.
type protection struct {
	scheme string
	key    []byte
	kid    mp4.UUID
	iv     []byte
	pssh   []*mp4.PsshBox
	raw    [][]byte
}

// check validates e and fills in a random IV.
func (e Encryption) check() (*protection, error) {
	if e.Scheme != SchemeCENC && e.Scheme != SchemeCBCS {
		return nil, fmt.Errorf("%w: scheme %q is neither cenc nor cbcs", ErrEncryption, string(e.Scheme))
	}
	if len(e.Key.b) != 16 {
		return nil, fmt.Errorf("%w: no 16-byte content key", ErrEncryption)
	}
	iv := bytes.Clone(e.IV)
	if len(iv) == 0 {
		iv = make([]byte, 8)
		if e.Scheme == SchemeCBCS {
			iv = make([]byte, 16)
		}
		// crypto/rand.Read never returns an error.
		_, _ = rand.Read(iv)
	}
	if len(iv) != 8 && len(iv) != 16 {
		return nil, fmt.Errorf("%w: an IV is 8 or 16 bytes, got %d", ErrEncryption, len(iv))
	}
	if len(e.PSSH) > MaxPSSHBoxes {
		return nil, fmt.Errorf("%w: %d pssh boxes, at most %d", ErrEncryption, len(e.PSSH), MaxPSSHBoxes)
	}
	p := &protection{scheme: string(e.Scheme), key: e.Key.b, kid: mp4.UUID(bytes.Clone(e.KID[:])), iv: iv}
	for i, raw := range e.PSSH {
		if len(raw) > MaxPSSHSize {
			return nil, fmt.Errorf("%w: pssh box %d is %d bytes, at most %d", ErrEncryption, i, len(raw), MaxPSSHSize)
		}
		box, err := mp4.DecodeBox(0, bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%w: pssh box %d: %v", ErrEncryption, i, err)
		}
		ps, ok := box.(*mp4.PsshBox)
		if !ok || box.Size() != uint64(len(raw)) {
			return nil, fmt.Errorf("%w: pssh box %d is not exactly one pssh box", ErrEncryption, i)
		}
		p.pssh = append(p.pssh, ps)
		p.raw = append(p.raw, bytes.Clone(raw))
	}
	return p, nil
}

// forTrack is p for the track at position ordinal: the IV moved so that no two
// tracks of one key share a keystream.
func (p *protection) forTrack(ordinal int) *protection {
	q := *p
	q.iv = bytes.Clone(p.iv)
	var carry = uint32(ordinal)
	for i := 3; i >= 0 && carry != 0; i-- {
		sum := uint32(q.iv[i]) + carry&0xff
		q.iv[i] = byte(sum)
		carry = carry>>8 + sum>>8
	}
	return &q
}

// trackEncryptor encrypts one track: its init segment, then its media segments
// in order.
type trackEncryptor struct {
	ipd *mp4.InitProtectData
	p   *protection
	iv  []byte // the next IV, chained from one segment to the next
}

// newTrackEncryptor protects a one-track initialisation segment and returns it
// with what encrypts the track's media segments.
func newTrackEncryptor(init []byte, p *protection) ([]byte, *trackEncryptor, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(init))
	if err != nil || f.Init == nil {
		return nil, nil, fmt.Errorf("%w: no initialisation segment", ErrEncrypt)
	}
	ipd, err := mp4.InitProtect(f.Init, p.key, p.iv, p.scheme, p.kid, p.pssh)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrEncrypt, err)
	}
	out, err := encodeMemory(f.Init)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrEncrypt, err)
	}
	return out, &trackEncryptor{ipd: ipd, p: p, iv: p.iv}, nil
}

// encodeMemory writes a box or segment to memory; it is a variable so that the
// failure of an encoding, which a memory buffer never reports, can be tested.
var encodeMemory = func(e interface{ Encode(io.Writer) error }) ([]byte, error) {
	var b bytes.Buffer
	err := e.Encode(&b)
	return b.Bytes(), err
}

// segment encrypts a media segment, a moof and an mdat, and returns the new
// bytes; the segment must not hold a sidx, which the larger moof would make wrong.
func (e *trackEncryptor) segment(seg []byte) ([]byte, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(seg))
	if err != nil || len(f.Segments) != 1 || f.Segments[0].Sidx != nil {
		return nil, fmt.Errorf("%w: not a single media segment without a sidx", ErrEncrypt)
	}
	enc, err := e.ipd.NewFragmentEncryptor(e.p.key, e.iv)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncrypt, err)
	}
	for _, frag := range f.Segments[0].Fragments {
		if err := enc.EncryptFragment(frag); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrEncrypt, err)
		}
	}
	e.iv = enc.IV()
	out, err := encodeMemory(f.Segments[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncrypt, err)
	}
	return out, nil
}

// Encrypt protects one track of a fragmented MP4: its initialisation segment
// (ftyp and moov, one track) and its media segments, each a moof and an mdat
// without a styp or sidx, in decode order. The inputs are not modified. Video
// (avc1, avc3, hvc1, hev1, av01, vp09 for cenc) is encrypted in subsamples, audio
// whole.
func Encrypt(init []byte, segments [][]byte, enc Encryption) ([]byte, [][]byte, error) {
	p, err := enc.check()
	if err != nil {
		return nil, nil, err
	}
	outInit, te, err := newTrackEncryptor(init, p)
	if err != nil {
		return nil, nil, err
	}
	out := make([][]byte, len(segments))
	for i, s := range segments {
		if out[i], err = te.segment(s); err != nil {
			return nil, nil, fmt.Errorf("segment %d: %w", i, err)
		}
	}
	return outInit, out, nil
}

// checkTenc refuses the protection parameters of a track that the decryption
// does not honour, before any sample is read: an unknown scheme, an IV size other
// than 0, 8 or 16, a constant IV that is not 8 or 16 bytes, a pattern outside the
// 4-bit fields or one that skips without encrypting.
func checkTenc(scheme string, tenc *mp4.TencBox) error {
	if tenc == nil {
		return fmt.Errorf("%w: no tenc box", ErrDecrypt)
	}
	switch tenc.DefaultPerSampleIVSize {
	case 0:
		if n := len(tenc.DefaultConstantIV); n != 8 && n != 16 {
			return fmt.Errorf("%w: constant IV of %d bytes", ErrDecrypt, n)
		}
	case 8, 16:
	default:
		return fmt.Errorf("%w: per-sample IV size %d", ErrDecrypt, tenc.DefaultPerSampleIVSize)
	}
	crypt, skip := tenc.DefaultCryptByteBlock, tenc.DefaultSkipByteBlock
	switch {
	case scheme == "cenc" && (crypt != 0 || skip != 0):
		return fmt.Errorf("%w: a pattern in a cenc track", ErrDecrypt)
	case crypt > 15 || skip > 15 || (crypt == 0 && skip != 0):
		return fmt.Errorf("%w: pattern %d:%d", ErrDecrypt, crypt, skip)
	}
	return nil
}

// checkInfo validates what DecryptInit found.
func checkInfo(di mp4.DecryptInfo) error {
	for _, ti := range di.TrackInfos {
		if ti.Sinf == nil {
			continue
		}
		if ti.Sinf.Schm == nil || ti.Sinf.Schi == nil {
			return fmt.Errorf("%w: track %d has no schm or schi", ErrDecrypt, ti.TrackID)
		}
		if err := checkTenc(ti.Sinf.Schm.SchemeType, ti.Sinf.Schi.Tenc); err != nil {
			return fmt.Errorf("track %d: %w", ti.TrackID, err)
		}
	}
	return nil
}

// decryptor decrypts the media segments that follow one initialisation segment.
type decryptor struct {
	di  mp4.DecryptInfo
	key []byte
}

// newDecryptor reads the protection of an initialisation segment and returns it
// stripped of encryption, with what decrypts the media segments.
func newDecryptor(init *mp4.InitSegment, key ContentKey) ([]byte, *decryptor, error) {
	if len(key.b) != 16 {
		return nil, nil, fmt.Errorf("%w: no 16-byte content key", ErrEncryption)
	}
	di, err := mp4.DecryptInit(init)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	if err := checkInfo(di); err != nil {
		return nil, nil, err
	}
	out, err := encodeMemory(init)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return out, &decryptor{di: di, key: key.b}, nil
}

// segment decrypts the fragments of s in place.
func (d *decryptor) segment(s *mp4.MediaSegment) error {
	if err := mp4.DecryptSegment(s, d.di, d.key); err != nil {
		return fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return nil
}

// Decrypt is Encrypt's inverse, and decrypts what ffmpeg, Shaka Packager or
// Bento4 wrote, for cenc and cbcs: it takes an initialisation segment and the
// media segments that follow it and returns them in the clear, with the
// protection boxes (sinf, tenc, senc, saiz, saio, pssh) removed. A sidx inside a
// segment is dropped, as its sizes no longer hold. The key is the clear content
// key; it is not matched against the track's KID. A track that is not protected
// comes back as it is.
func Decrypt(init []byte, segments [][]byte, key ContentKey) ([]byte, [][]byte, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(init))
	if err != nil || f.Init == nil {
		return nil, nil, fmt.Errorf("%w: no initialisation segment", ErrDecrypt)
	}
	outInit, d, err := newDecryptor(f.Init, key)
	if err != nil {
		return nil, nil, err
	}
	out := make([][]byte, len(segments))
	for i, s := range segments {
		g, err := mp4.DecodeFile(bytes.NewReader(s))
		if err != nil || len(g.Segments) != 1 {
			return nil, nil, fmt.Errorf("%w: segment %d is not a single media segment", ErrDecrypt, i)
		}
		if err := d.segment(g.Segments[0]); err != nil {
			return nil, nil, fmt.Errorf("segment %d: %w", i, err)
		}
		if out[i], err = encodeMemory(g.Segments[0]); err != nil {
			return nil, nil, fmt.Errorf("%w: segment %d: %v", ErrDecrypt, i, err)
		}
	}
	return outInit, out, nil
}

// DecryptFile decrypts a whole fragmented MP4 file, the initialisation segment
// and all its fragments, of any number of tracks.
func DecryptFile(data []byte, key ContentKey) ([]byte, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil || f.Init == nil {
		return nil, fmt.Errorf("%w: not a fragmented MP4 file", ErrDecrypt)
	}
	_, d, err := newDecryptor(f.Init, key)
	if err != nil {
		return nil, err
	}
	for _, s := range f.Segments {
		if err := d.segment(s); err != nil {
			return nil, err
		}
	}
	out, err := encodeMemory(f)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return out, nil
}

// Protection is what a packaging manifest states about an encrypted track: the
// scheme, the key identifier (DASH cenc:default_KID, HLS EXT-X-KEY) and the pssh
// boxes (DASH cenc:pssh).
type Protection struct {
	Scheme EncryptionScheme
	KID    [16]byte
	PSSH   [][]byte
}
