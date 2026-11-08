// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package media

import (
	"bytes"
	"encoding/hex"
	"strings"

	"github.com/go-avkit/avkit/container"
	"github.com/go-streamkit/streamkit/dash"
	"github.com/go-streamkit/streamkit/hls"
)

// Encryption in a presentation. container.EncryptSegments encrypts the segments
// and each SegmentedTrack states its Protection (scheme, key identifier, pssh
// boxes); this file states it in the playlists and the manifest, and never holds
// a content key: where a player gets one is the caller's to say.
//
// Sources: draft-pantos-hls-rfc8216bis 4.4.4.4 (EXT-X-KEY: with fMP4, METHOD
// SAMPLE-AES is Common Encryption scheme cbcs and SAMPLE-AES-CTR is cenc; URI
// required; KEYFORMAT names the key system) and 4.4.6.5 (EXT-X-SESSION-KEY, which
// SHOULD be added when variants or renditions share keys); for DASH the
// ContentProtection elements of dash-mpd's HbbTV and DASH-IF livesim manifests
// (mp4protection with value and cenc:default_KID, then one element per DRM system
// with its cenc:pssh). ISO/IEC 23001-7 and 23009-1 are paywalled and were not read.

// Option configures HLS, DASH and WriteDir.
type Option func(*settings)

type settings struct{ keys []Key }

// Key says how a player gets the content key of an encrypted presentation, for
// one key system (a DRM, or the clear "identity" key).
type Key struct {
	// SystemID is the key system's UUID ("edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"
	// for Widevine, "1077efec-c0b2-4d02-ace3-3c1e52e2fb4b" for the common
	// system). Empty is the "identity" key format of HLS (a clear key at URI),
	// which DASH has no element for.
	SystemID string
	// URI is where an HLS player gets the key: a licence server's URL, an
	// skd:// or data: URI, or a file. Required for HLS.
	URI string
	// FormatVersions is HLS KEYFORMATVERSIONS ("1", "1/2"); empty means 1.
	FormatVersions string
	// Value is the DASH ContentProtection value of the system ("widevine").
	Value string
	// PSSH is the system's pssh box for DASH (cenc:pssh). When nil, the box
	// among the track's Protection.PSSH with the same SystemID is used, if any.
	PSSH []byte
}

// Keys states how the encrypted tracks' keys are obtained. A presentation whose
// segmentation is encrypted needs at least one for HLS, where the playlists are
// not playable without an EXT-X-KEY; DASH states the protection scheme and key
// identifier without any.
func Keys(keys ...Key) Option {
	return func(s *settings) { s.keys = append(s.keys, keys...) }
}

func newSettings(opts []Option) settings {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	return s
}

// scheme is the one Common Encryption scheme of the encrypted tracks of seg, and
// whether any track is encrypted. Tracks of two schemes are refused: a master
// playlist's session keys state one method.
func scheme(seg *container.Segmentation) (container.EncryptionScheme, bool, error) {
	var found container.EncryptionScheme
	for i := range seg.Tracks {
		p := seg.Tracks[i].Protection
		switch {
		case p == nil:
		case found == "":
			found = p.Scheme
		case found != p.Scheme:
			return "", false, fail("tracks encrypted with %s and %s", found, p.Scheme)
		}
	}
	return found, found != "", nil
}

// systemFormat is the HLS KEYFORMAT of a key system: a urn:uuid, in lower case.
func systemFormat(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	if !uuidLike(id) {
		return "", fail("key system %q is not a UUID", id)
	}
	return "urn:uuid:" + strings.ToLower(id), nil
}

// uuidLike reports whether s is 8-4-4-4-12 hexadecimal digits.
func uuidLike(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	h := strings.ReplaceAll(s, "-", "")
	_, err := hex.DecodeString(h)
	return len(h) == 32 && err == nil
}

// hlsKeys are the EXT-X-KEY tags of an encrypted segmentation, which are also its
// EXT-X-SESSION-KEY tags.
func hlsKeys(sch container.EncryptionScheme, keys []Key) ([]hls.EncryptionKey, error) {
	method := hls.MethodSampleAESCTR
	if sch == container.SchemeCBCS {
		method = hls.MethodSampleAES
	}
	if len(keys) == 0 {
		return nil, fail("an encrypted segmentation needs Keys for HLS: the playlists are not playable without an EXT-X-KEY")
	}
	out := make([]hls.EncryptionKey, len(keys))
	for i, k := range keys {
		format, err := systemFormat(k.SystemID)
		if err != nil {
			return nil, err
		}
		out[i] = hls.EncryptionKey{Method: method, URI: k.URI, Format: format, FormatVersions: k.FormatVersions}
	}
	return out, nil
}

// psshSystem is the system ID of a pssh box (ISO/IEC 23001-7 8.1: size, type,
// version and flags, then the 16-byte SystemID) as a UUID, or "".
func psshSystem(box []byte) string {
	if len(box) < 28 || string(box[4:8]) != "pssh" {
		return ""
	}
	h := hex.EncodeToString(box[12:28])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// dashProtection are the ContentProtection elements of an encrypted track.
func dashProtection(t *container.SegmentedTrack, keys []Key) ([]dash.ContentProtection, error) {
	p := t.Protection
	kid := hex.EncodeToString(p.KID[:])
	out := []dash.ContentProtection{{
		SchemeIDURI: dash.MP4ProtectionScheme, Value: string(p.Scheme),
		DefaultKID: kid[0:8] + "-" + kid[8:12] + "-" + kid[12:16] + "-" + kid[16:20] + "-" + kid[20:],
	}}
	for _, k := range keys {
		if k.SystemID == "" {
			continue
		}
		id, err := systemFormat(k.SystemID)
		if err != nil {
			return nil, err
		}
		pssh := k.PSSH
		for _, box := range p.PSSH {
			if pssh == nil && strings.EqualFold(psshSystem(box), k.SystemID) {
				pssh = bytes.Clone(box)
			}
		}
		out = append(out, dash.ContentProtection{SchemeIDURI: id, Value: k.Value, PSSH: pssh})
	}
	return out, nil
}

// refuseKeysWithoutEncryption refuses Keys for a segmentation that is in the
// clear: they would describe a protection the segments do not have.
func refuseKeysWithoutEncryption(encrypted bool, s settings) error {
	if !encrypted && len(s.keys) > 0 {
		return fail("Keys given for a segmentation that is not encrypted")
	}
	return nil
}
