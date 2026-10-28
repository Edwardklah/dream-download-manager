// Package video finds the downloadable streams of a video page (YouTube and
// over a thousand other sites) with yt-dlp, and turns them into simple
// choices: "1080p · MP4 · 85 MB", "Audio only".
//
// yt-dlp does the hard part - reading the site, working out the real
// addresses of the streams (YouTube hides them behind JavaScript puzzles).
// We only ask it for that list ("-J" = everything as JSON) and download the
// streams ourselves with the engine, which is much faster than yt-dlp's own
// single connection.
package video

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Info is what yt-dlp tells us about a video page.
type Info struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Uploader  string   `json:"uploader"`
	Duration  float64  `json:"duration"` // seconds
	Thumbnail string   `json:"thumbnail"`
	PageURL   string   `json:"webpage_url"`
	Extractor string   `json:"extractor_key"` // the site, e.g. "Youtube"
	IsLive    bool     `json:"is_live"`
	Formats   []Format `json:"formats"`
	Language  string   `json:"language"` // the video's spoken language, if known

	// Subtitle languages, and the files each comes in. Keys are language
	// codes ("en", "pt-BR"); automatic captions' original is "xx-orig".
	Subtitles    map[string][]SubFormat `json:"subtitles"`
	AutoCaptions map[string][]SubFormat `json:"automatic_captions"`
	// Type is "video" normally; "playlist" if the link is a playlist
	// (we ask for single videos, so that only happens for some sites).
	Type string `json:"_type"`
}

// Format is one stream: video only, audio only, or both.
type Format struct {
	ID           string            `json:"format_id"`
	Ext          string            `json:"ext"`
	VCodec       string            `json:"vcodec"` // "none" = no video
	ACodec       string            `json:"acodec"` // "none" = no audio
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	FPS          float64           `json:"fps"`
	TBR          float64           `json:"tbr"` // total bitrate, kbit/s
	ABR          float64           `json:"abr"`
	Filesize     int64             `json:"filesize"`
	FilesizeAprx int64             `json:"filesize_approx"`
	URL          string            `json:"url"`
	Protocol     string            `json:"protocol"` // "https" = one ordinary file
	Note         string            `json:"format_note"`
	DynamicRange string            `json:"dynamic_range"` // "SDR", "HDR10", ...
	LanguagePref int               `json:"language_preference"`
	HasDRM       any               `json:"has_drm"` // bool, or "maybe"
	Headers      map[string]string `json:"http_headers"`
	Downloader   struct {
		ChunkSize int64 `json:"http_chunk_size"`
	} `json:"downloader_options"`
}

func (f Format) HasVideo() bool { return f.VCodec != "" && f.VCodec != "none" }
func (f Format) HasAudio() bool { return f.ACodec != "" && f.ACodec != "none" }

// Direct: the stream is one ordinary file our engine can download. (The
// alternatives are lists of small pieces - HLS ".m3u8", DASH segments -
// which only yt-dlp handles, for now.)
func (f Format) Direct() bool {
	return (f.Protocol == "https" || f.Protocol == "http") && f.URL != "" && !f.drm()
}

// HLS: the stream is an HLS playlist of pieces (package hls downloads it).
func (f Format) HLS() bool {
	return (f.Protocol == "m3u8_native" || f.Protocol == "m3u8") && f.URL != "" && !f.drm()
}

// Usable: something we can download, one way or the other.
func (f Format) Usable() bool { return f.Direct() || f.HLS() }

// Unknown codecs: some sites (and many HLS playlists) don't say what's
// inside. Such a stream is taken to be complete - picture and sound.
func (f Format) unknownCodecs() bool { return f.VCodec == "" && f.ACodec == "" }

func (f Format) drm() bool { return f.HasDRM == true || f.HasDRM == "maybe" }

// Size in bytes, and whether it's an estimate (0 = unknown).
func (f Format) Size() (int64, bool) {
	if f.Filesize > 0 {
		return f.Filesize, false
	}
	return f.FilesizeAprx, f.FilesizeAprx > 0
}

// Options for Inspect.
type Options struct {
	YtDlp     string // path to yt-dlp
	JSArgs    []string
	Cookies   string // a cookies.txt file, for videos that need you logged in
	UserAgent string // the browser's, if known
	CacheDir  string // yt-dlp's cache (YouTube's player script, solved puzzles)
	Timeout   time.Duration
}

// ErrNotVideo means yt-dlp found no video at that address.
var ErrNotVideo = errors.New("no video found at that address")

// Inspect asks yt-dlp about a page.
func Inspect(ctx context.Context, pageURL string, o Options) (*Info, error) {
	if o.Timeout == 0 {
		o.Timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	args := []string{
		"-J",            // everything about the video, as JSON
		"--no-playlist", // a video in a playlist: just that video
		"--no-warnings", "--no-progress",
		"--ignore-config", // the user's own yt-dlp settings mustn't change our results
	}
	args = append(args, o.args()...)
	args = append(args, "--", pageURL) // "--": the URL can never be read as an option

	cmd := exec.CommandContext(ctx, o.YtDlp, args...)
	hideWindow(cmd) // no black console window flashing up
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, errors.New("the site took too long to answer")
		}
		return nil, ytdlpError(stderr.String(), err)
	}
	var info Info
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		return nil, fmt.Errorf("couldn't read yt-dlp's answer: %w", err)
	}
	if info.Type == "playlist" || len(info.Formats) == 0 {
		return nil, ErrNotVideo
	}
	return &info, nil
}

// args are the options' yt-dlp arguments.
func (o Options) args() []string {
	args := append([]string{}, o.JSArgs...)
	if o.Cookies != "" {
		args = append(args, "--cookies", o.Cookies)
	}
	if o.UserAgent != "" {
		args = append(args, "--user-agent", o.UserAgent)
	}
	if o.CacheDir != "" {
		args = append(args, "--cache-dir", o.CacheDir)
	}
	return args
}

// ytdlpError picks yt-dlp's own explanation ("ERROR: ...") out of its output.
func ytdlpError(stderr string, err error) error {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if msg, ok := strings.CutPrefix(line, "ERROR: "); ok {
			// "[youtube] abc123: Video unavailable" -> "Video unavailable"
			if _, rest, found := strings.Cut(msg, "] "); found && strings.HasPrefix(msg, "[") {
				msg = rest
				if id, after, ok := strings.Cut(msg, ": "); ok && !strings.Contains(id, " ") {
					msg = after
				}
			}
			if strings.Contains(msg, "Unsupported URL") {
				return ErrNotVideo
			}
			return errors.New(msg)
		}
	}
	return fmt.Errorf("yt-dlp failed: %w", err)
}
