// Package hls downloads videos streamed with HLS ("HTTP Live Streaming").
//
// Instead of one file, an HLS video is a playlist - a text file ending in
// .m3u8 - listing many short pieces ("segments", a few seconds each):
//
//	#EXTM3U
//	#EXT-X-TARGETDURATION:6
//	#EXT-X-KEY:METHOD=AES-128,URI="key.bin"     <- the pieces are locked with this key
//	#EXTINF:6.0,
//	segment-00001.ts
//	#EXTINF:6.0,
//	segment-00002.ts
//	...
//	#EXT-X-ENDLIST                                <- the list is complete (not a live stream)
//
// A "master" playlist lists one such playlist per quality. We download the
// pieces several at a time, unlock them if needed, and put them together in
// order. The result is an MPEG-TS file (.ts), or for "fMP4" HLS (pieces
// that are MP4 fragments, after an #EXT-X-MAP header) an MP4.
//
// The AES-128 lock is a standard part of HLS, with the key served next to
// the playlist; it's not copy protection (DRM). Playlists using DRM schemes
// ("SAMPLE-AES", "com.apple.streamingkeydelivery", Widevine...) are refused.
package hls

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Playlist is a parsed media playlist.
type Playlist struct {
	Segments []Segment
	Init     *Segment // the #EXT-X-MAP header of fMP4 pieces, or nil
	Ended    bool     // #EXT-X-ENDLIST: a finished video, not a live stream
}

// Segment is one piece.
type Segment struct {
	URL      string
	Duration float64
	Offset   int64 // with Length > 0: only these bytes of URL (#EXT-X-BYTERANGE)
	Length   int64
	Key      *Key // nil: not locked
	Seq      uint64
}

// Key is how a piece is locked: AES-128 with this key, and IV if given
// (otherwise the piece's sequence number).
type Key struct {
	URL string
	IV  []byte
}

// Variant is one quality in a master playlist.
type Variant struct {
	URL       string
	Bandwidth int64
}

// Errors.
var (
	ErrNotPlaylist = errors.New("not an HLS playlist")
	ErrDRM         = errors.New("this video is copy-protected (DRM) and can't be downloaded")
	ErrLive        = errors.New("this is a live stream; only finished videos can be downloaded")
)

// Parse reads a playlist downloaded from base. It returns either a media
// playlist, or (for a master playlist) its variants.
func Parse(base *url.URL, text string) (*Playlist, []Variant, error) {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	if !sc.Scan() || !strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "\uFEFF"), "#EXTM3U") {
		return nil, nil, ErrNotPlaylist
	}

	p := &Playlist{}
	var variants []Variant
	var (
		seq        uint64
		duration   float64
		key        *Key
		pendingBR  *[2]int64 // byte range for the next segment
		nextOffset = map[string]int64{}
		variant    *Variant // #EXT-X-STREAM-INF waiting for its URL
	)
	resolve := func(ref string) (string, error) {
		u, err := base.Parse(ref)
		if err != nil {
			return "", fmt.Errorf("bad address in playlist: %q", ref)
		}
		return u.String(), nil
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			n, _ := strconv.ParseUint(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			seq = n
		case strings.HasPrefix(line, "#EXTINF:"):
			v, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			duration, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			n, o, err := parseByteRange(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"))
			if err != nil {
				return nil, nil, err
			}
			pendingBR = &[2]int64{n, o}
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			attrs := parseAttrs(strings.TrimPrefix(line, "#EXT-X-KEY:"))
			switch method := attrs["METHOD"]; method {
			case "NONE":
				key = nil
			case "AES-128":
				if attrs["URI"] == "" {
					return nil, nil, errors.New("playlist key has no address")
				}
				k := &Key{}
				var err error
				if k.URL, err = resolve(attrs["URI"]); err != nil {
					return nil, nil, err
				}
				if iv := attrs["IV"]; iv != "" {
					b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(iv, "0x"), "0X"))
					if err != nil || len(b) != 16 {
						return nil, nil, fmt.Errorf("bad key IV %q", iv)
					}
					k.IV = b
				}
				key = k
			default: // SAMPLE-AES, SAMPLE-AES-CTR...: DRM
				return nil, nil, ErrDRM
			}
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs := parseAttrs(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			u, err := resolve(attrs["URI"])
			if err != nil || attrs["URI"] == "" {
				return nil, nil, errors.New("bad #EXT-X-MAP")
			}
			init := &Segment{URL: u}
			if br := attrs["BYTERANGE"]; br != "" {
				n, o, err := parseByteRange(br)
				if err != nil {
					return nil, nil, err
				}
				init.Length, init.Offset = n, max(o, 0)
			}
			p.Init = init
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			p.Ended = true
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			attrs := parseAttrs(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			bw, _ := strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
			variant = &Variant{Bandwidth: bw}
		case strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"):
			// A master playlist announcing its key: DRM unless plain AES-128.
			if m := parseAttrs(strings.TrimPrefix(line, "#EXT-X-SESSION-KEY:"))["METHOD"]; m != "" && m != "AES-128" && m != "NONE" {
				return nil, nil, ErrDRM
			}
		case strings.HasPrefix(line, "#"):
			// Other tags don't change what we download.
		default: // an address
			u, err := resolve(line)
			if err != nil {
				return nil, nil, err
			}
			if variant != nil {
				variant.URL = u
				variants = append(variants, *variant)
				variant = nil
				continue
			}
			s := Segment{URL: u, Duration: duration, Key: key, Seq: seq}
			if pendingBR != nil {
				s.Length, s.Offset = pendingBR[0], pendingBR[1]
				if s.Offset < 0 { // continues where the previous range of this file ended
					s.Offset = nextOffset[u]
				}
				nextOffset[u] = s.Offset + s.Length
				pendingBR = nil
			}
			p.Segments = append(p.Segments, s)
			seq++
			duration = 0
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if len(variants) > 0 {
		return nil, variants, nil
	}
	if len(p.Segments) == 0 {
		return nil, nil, ErrNotPlaylist
	}
	return p, nil, nil
}

// parseByteRange reads "length[@offset]"; offset -1 if not given.
func parseByteRange(s string) (n, offset int64, err error) {
	a, b, hasOffset := strings.Cut(strings.Trim(s, `"`), "@")
	if n, err = strconv.ParseInt(a, 10, 64); err != nil || n <= 0 {
		return 0, 0, fmt.Errorf("bad byte range %q", s)
	}
	offset = -1
	if hasOffset {
		if offset, err = strconv.ParseInt(b, 10, 64); err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("bad byte range %q", s)
		}
	}
	return n, offset, nil
}

// parseAttrs reads an attribute list: KEY=value,KEY="quoted, value",...
func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	for len(s) > 0 {
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.TrimSpace(k)
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				v, rest = rest[1:], ""
			} else {
				v, rest = rest[1:end+1], rest[end+2:]
			}
			_, rest, _ = strings.Cut(rest, ",")
		} else {
			v, rest, _ = strings.Cut(rest, ",")
		}
		attrs[strings.ToUpper(k)] = strings.TrimSpace(v)
		s = rest
	}
	return attrs
}
