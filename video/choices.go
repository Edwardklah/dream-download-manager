package video

import (
	"fmt"
	"sort"
	"strings"
)

// Choice is one option offered to the user.
type Choice struct {
	ID         string  `json:"id"`     // "137+140", "140", "18"
	Label      string  `json:"label"`  // "1080p", "720p60", "Audio only"
	Detail     string  `json:"detail"` // "MP4 · H.264"
	Size       int64   `json:"size"`   // bytes, 0 if unknown
	SizeApprox bool    `json:"sizeApprox"`
	Ext        string  `json:"ext"` // of the finished file
	Video      *Format `json:"-"`   // nil for audio only
	Audio      *Format `json:"-"`   // nil if Video already has sound
}

// Choices turns the formats into a short list, best first, plus "Audio
// only". It only offers what we can download and finish ourselves:
//
//   - a complete stream (picture and sound together) in any container - an
//     ordinary file or an HLS playlist - needs no joining at all;
//   - separate picture and sound are joined: MP4 (H.264, HEVC or AV1, with
//     AAC sound) by package mux, WebM (VP9 or AV1, with Opus sound) by
//     package mkv.
//
// For each quality the stream is chosen by: an ordinary file over HLS (one
// download, not hundreds), then compatibility - H.264 plays everywhere, so
// it's preferred up to 1080p, where YouTube still offers it - then separate
// streams over combined ones (usually better quality), then bitrate. HDR
// versions are skipped: they look washed out on most screens.
func Choices(info *Info) []Choice {
	var videoOnly, combined, audioMP4, audioWebM []*Format
	for i := range info.Formats {
		f := &info.Formats[i]
		if !f.Usable() {
			continue
		}
		switch {
		case f.HasVideo() && f.HasAudio() || f.unknownCodecs():
			combined = append(combined, f)
		case f.HasVideo():
			mp4OK := f.Ext == "mp4" && (isAVC(f) || isAV1(f) || isHEVC(f))
			webmOK := f.Ext == "webm" && f.Direct() // joined by package mkv
			if (mp4OK || webmOK) && !isHDR(f) {
				videoOnly = append(videoOnly, f)
			}
		case f.HasAudio():
			switch {
			case f.Ext == "m4a" || (f.Ext == "mp4" && strings.HasPrefix(f.ACodec, "mp4a")):
				audioMP4 = append(audioMP4, f)
			case f.Ext == "webm" && f.Direct():
				audioWebM = append(audioWebM, f)
			}
		}
	}
	// Picture and sound must be joined in the same container: MP4 video
	// with M4A sound, WebM video with WebM (Opus) sound.
	audio := bestAudio(audioMP4)
	webmAudio := bestAudio(audioWebM)
	audioFor := func(v *Format) *Format {
		if v.Ext == "webm" {
			return webmAudio
		}
		return audio
	}

	// Best video stream per label (height + high frame rate).
	best := map[string]*Format{}
	var order []string
	pick := func(f *Format) {
		label := heightLabel(f)
		if label == "" {
			return
		}
		cur, ok := best[label]
		if !ok {
			order = append(order, label)
		}
		if !ok || better(f, cur) {
			best[label] = f
		}
	}
	for _, f := range videoOnly {
		if audioFor(f) != nil {
			pick(f)
		}
	}
	for _, f := range combined {
		pick(f)
	}

	var out []Choice
	for _, label := range order {
		v := best[label]
		c := Choice{Label: label, Ext: "mp4", Video: v, Detail: detail(v)}
		if v.HasAudio() || v.unknownCodecs() {
			c.ID = v.ID
			if v.Direct() && v.Ext != "" {
				c.Ext = v.Ext // a complete file is kept as it comes (.mp4, .webm, .mkv...)
			}
		} else {
			a := audioFor(v)
			c.ID = v.ID + "+" + a.ID
			c.Audio = a
			if v.Ext == "webm" {
				c.Ext = "webm"
			}
		}
		c.Size, c.SizeApprox = sizes(c.Video, c.Audio)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := shortSide(out[i].Video), shortSide(out[j].Video); a != b {
			return a > b
		}
		return out[i].Video.FPS > out[j].Video.FPS
	})
	if audio != nil {
		c := Choice{ID: audio.ID, Label: "Audio only", Ext: "m4a", Audio: audio,
			Detail: strings.TrimSpace(fmt.Sprintf("M4A · AAC %s", kbps(audio)))}
		c.Size, c.SizeApprox = sizes(nil, audio)
		out = append(out, c)
	}
	return out
}

// detail describes a stream: "MP4 · H.264", "HLS · HEVC", "WEBM".
func detail(f *Format) string {
	container := strings.ToUpper(f.Ext)
	if f.HLS() {
		container = "HLS"
	}
	if container == "" {
		container = "Video"
	}
	if name := codecName(f); name != "" {
		return container + " · " + name
	}
	return container
}

// Find returns the choice with this ID (from a fresh Choices list).
func Find(choices []Choice, id string) (Choice, bool) {
	for _, c := range choices {
		if c.ID == id {
			return c, true
		}
	}
	return Choice{}, false
}

func isAVC(f *Format) bool {
	return strings.HasPrefix(f.VCodec, "avc1") || strings.HasPrefix(f.VCodec, "h264")
}
func isAV1(f *Format) bool { return strings.HasPrefix(f.VCodec, "av01") }
func isHEVC(f *Format) bool {
	return strings.HasPrefix(f.VCodec, "hvc1") || strings.HasPrefix(f.VCodec, "hev1") || strings.HasPrefix(f.VCodec, "h265")
}
func isHDR(f *Format) bool { return f.DynamicRange != "" && f.DynamicRange != "SDR" }

// shortSide: quality is named by the short side, so a portrait video
// (YouTube Shorts) of 1080x1920 is "1080p", like a landscape 1920x1080.
func shortSide(f *Format) int {
	if f.Width > 0 && f.Width < f.Height {
		return f.Width
	}
	return f.Height
}

func heightLabel(f *Format) string {
	h := shortSide(f)
	if h <= 0 {
		// No size given (some HLS playlists): name it as the site does.
		if f.Note != "" {
			return f.Note
		}
		return "Video " + f.ID
	}
	if f.FPS > 30 {
		return fmt.Sprintf("%dp%d", h, int(f.FPS+0.5))
	}
	return fmt.Sprintf("%dp", h)
}

// better: is a a better pick than b for the same quality label?
func better(a, b *Format) bool {
	// One ordinary file beats a playlist of hundreds of pieces.
	if a.Direct() != b.Direct() {
		return a.Direct()
	}
	// Up to 1080p prefer H.264 (plays everywhere); above, whatever is there.
	if shortSide(a) <= 1080 && isAVC(a) != isAVC(b) {
		return isAVC(a)
	}
	// MP4 over WebM: more players and editors take it.
	if (a.Ext == "mp4") != (b.Ext == "mp4") {
		return a.Ext == "mp4"
	}
	// A separate video stream beats one with sound built in (that one is
	// usually lower quality for the same height).
	if a.HasAudio() != b.HasAudio() {
		return !a.HasAudio()
	}
	return a.TBR > b.TBR
}

// bestAudio: the original language first (videos can have dubbed tracks),
// then the highest bitrate.
func bestAudio(fs []*Format) *Format {
	var best *Format
	for _, f := range fs {
		if best == nil || f.LanguagePref > best.LanguagePref ||
			f.LanguagePref == best.LanguagePref && f.Direct() && !best.Direct() ||
			f.LanguagePref == best.LanguagePref && f.Direct() == best.Direct() && rate(f) > rate(best) {
			best = f
		}
	}
	return best
}

func rate(f *Format) float64 {
	if f.ABR > 0 {
		return f.ABR
	}
	return f.TBR
}

func kbps(f *Format) string {
	if r := rate(f); r > 0 {
		return fmt.Sprintf("%.0f kbit/s", r)
	}
	return ""
}

func codecName(f *Format) string {
	switch {
	case isAVC(f):
		return "H.264"
	case isAV1(f):
		return "AV1"
	case isHEVC(f):
		return "HEVC"
	case strings.HasPrefix(f.VCodec, "vp9") || strings.HasPrefix(f.VCodec, "vp09"):
		return "VP9"
	case f.VCodec == "none":
		return ""
	}
	return f.VCodec
}

func sizes(v, a *Format) (int64, bool) {
	var total int64
	approx := false
	for _, f := range []*Format{v, a} {
		if f == nil {
			continue
		}
		n, ap := f.Size()
		if n == 0 {
			return 0, false // one part unknown: the total is unknown
		}
		total += n
		approx = approx || ap
	}
	return total, approx
}
