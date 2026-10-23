package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dlmanager/mkv"
	"dlmanager/mpegts"
	"dlmanager/mux"
)

// Stream is one file to download for a video: the picture, the sound, or
// one stream with both.
type Stream struct {
	FormatID  string
	HLS       bool   // an HLS playlist (package hls), not one file
	YtDlp     string // if set: yt-dlp downloads this format itself (the fallback)
	URL       string
	Headers   map[string]string
	ChunkSize int64 // ask for at most this much per request (0 = no limit)
	Size      int64 // 0 if unknown
}

// Plan is what to download for a chosen quality, with fresh addresses.
type Plan struct {
	Title   string
	Ext     string   // of the finished file
	Streams []Stream // in order: video (or video+audio), then audio
	Info    *Info    // everything the look-up found (subtitles...)
}

// Backend is the real implementation of the manager's video needs.
type Backend struct {
	Tools Tools
}

// ErrNoTools means yt-dlp hasn't been downloaded yet.
var ErrNoTools = errors.New("video downloads need yt-dlp: download it in Settings first")

// Look asks about a page (for the quality picker). headers are the
// browser's (cookies, user agent), if the page came from the browser: some
// sites only show videos to logged-in users.
func (b Backend) Look(ctx context.Context, pageURL string, headers map[string]string) (*Info, []Choice, error) {
	if !b.Tools.Installed() {
		return nil, nil, ErrNoTools
	}
	o, cleanup, err := b.options(pageURL, headers)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()
	info, err := Inspect(ctx, pageURL, o)
	if err != nil {
		return nil, nil, err
	}
	choices := Choices(info)
	if len(choices) == 0 {
		// Nothing we can download ourselves - formats only yt-dlp handles
		// (some DASH and other piece formats). Let it download the best
		// complete one itself.
		if best := bestComplete(info); best != nil {
			choices = append(choices, Choice{
				ID: YtDlpPrefix + best.ID, Label: "Best available", Ext: completeExt(best),
				Detail: "Downloaded by yt-dlp", Video: best,
			})
			choices[0].Size, choices[0].SizeApprox = best.Size()
		}
	}
	return info, choices, nil
}

// YtDlpPrefix marks a choice that yt-dlp downloads itself.
const YtDlpPrefix = "ytdlp:"

// bestComplete: the best format with picture and sound together (yt-dlp
// can't join separate ones without ffmpeg), by height then bitrate.
func bestComplete(info *Info) *Format {
	var best *Format
	for i := range info.Formats {
		f := &info.Formats[i]
		if f.drm() || f.Protocol == "mhtml" || !(f.HasVideo() && f.HasAudio() || f.unknownCodecs()) {
			continue
		}
		if best == nil || shortSide(f) > shortSide(best) || shortSide(f) == shortSide(best) && f.TBR > best.TBR {
			best = f
		}
	}
	return best
}

func completeExt(f *Format) string {
	if f.Ext != "" {
		return f.Ext
	}
	return "mp4"
}

// Resolve looks the page up again and returns the streams of the chosen
// quality. Done at every start, because stream addresses expire after a few
// hours: a paused video resumes with fresh ones.
func (b Backend) Resolve(ctx context.Context, pageURL, choiceID string, headers map[string]string) (*Plan, error) {
	info, choices, err := b.Look(ctx, pageURL, headers)
	if err != nil {
		return nil, err
	}
	c, ok := Find(choices, choiceID)
	if !ok {
		return nil, fmt.Errorf("that quality (%s) isn't offered any more", choiceID)
	}
	plan := &Plan{Title: info.Title, Ext: c.Ext, Info: info}
	if spec, ok := strings.CutPrefix(choiceID, YtDlpPrefix); ok {
		size, _ := c.Video.Size()
		plan.Streams = []Stream{{FormatID: spec, YtDlp: spec, Size: size}}
		return plan, nil
	}
	for _, f := range []*Format{c.Video, c.Audio} {
		if f == nil {
			continue
		}
		size, _ := f.Size()
		plan.Streams = append(plan.Streams, Stream{
			FormatID: f.ID, HLS: f.HLS(), URL: f.URL, Headers: f.Headers,
			ChunkSize: f.Downloader.ChunkSize, Size: size,
		})
	}
	return plan, nil
}

// Finish turns the downloaded stream files into the finished file, and
// returns its path:
//   - picture + sound are joined into out;
//   - a single MP4 stream is tidied into an ordinary MP4 (or M4A);
//   - an MPEG-TS stream (from HLS) is converted to MP4 (package mpegts), or
//     kept as a .ts file if it holds codecs MP4 conversion doesn't handle;
//   - anything else that's already one complete file is just renamed.
func (b Backend) Finish(streams []string, out string) (string, error) {
	switch len(streams) {
	case 2:
		if isEBML(streams[0]) && isEBML(streams[1]) {
			return out, mkv.Merge(streams[0], streams[1], out) // WebM / Matroska
		}
		// MPEG-TS streams (separate HLS playlists for picture and sound)
		// become fragmented MP4s first, which package mux can join.
		inputs := append([]string(nil), streams...)
		for i, s := range inputs {
			if !isTS(s) {
				continue
			}
			frag := s + ".fmp4"
			defer os.Remove(frag)
			if err := mpegts.ToFragmented(s, frag); err != nil {
				return "", fmt.Errorf("couldn't join the picture and sound: %w", err)
			}
			inputs[i] = frag
		}
		return out, mux.Merge(inputs[0], inputs[1], out)
	case 1:
		if isTS(streams[0]) {
			err := mpegts.ToMP4(streams[0], out)
			if err == nil {
				return out, nil
			}
			if !errors.Is(err, mpegts.ErrUnsupported) {
				return "", err
			}
			// Codecs MP4 can't hold here (MP3, AC-3...): keep the .ts -
			// every player plays those.
			out = freeName(strings.TrimSuffix(out, filepath.Ext(out)) + ".ts")
			return out, os.Rename(streams[0], out)
		}
		err := mux.Merge(streams[0], "", out)
		if errors.Is(err, mux.ErrUnsupported) {
			return out, os.Rename(streams[0], out)
		}
		return out, err
	}
	return "", errors.New("nothing was downloaded")
}

// isTS reports whether a file is MPEG-TS: 188-byte packets, each starting
// with the byte 0x47.
func isTS(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 188*3+1)
	n, _ := io.ReadFull(f, buf)
	if n < 189 {
		return false
	}
	for i := 0; i < n; i += 188 {
		if buf[i] != 0x47 {
			return false
		}
	}
	return true
}

// isEBML reports whether a file is WebM/Matroska (starts with the EBML
// header ID 1A 45 DF A3).
func isEBML(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 4)
	_, err = io.ReadFull(f, b)
	return err == nil && b[0] == 0x1A && b[1] == 0x45 && b[2] == 0xDF && b[3] == 0xA3
}

// freeName returns path, or "name (1).ext" etc. if it's taken.
func freeName(path string) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	candidate := path
	for i := 1; ; i++ {
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
		candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
}
