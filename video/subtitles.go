package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Subtitles are saved as a separate file next to the video, named like it:
// "Episode 5.mp4" gets "Episode 5.en.srt". Players (VLC, MPC, most TVs)
// pick that up by themselves.
//
// yt-dlp lists two kinds:
//   - "subtitles": made by people (the uploader, the site);
//   - "automatic_captions": YouTube's speech recognition, plus machine
//     translations of it into ~100 languages. We offer only the original
//     one - the translations of a machine transcript are rarely worth it.
//
// Sites mostly serve WebVTT (.vtt), the web's subtitle format. It's
// converted to SubRip (.srt), which every player and TV reads. ASS/SSA
// subtitles (styled, common for anime) are kept as they are: converting
// them would throw away their positioning and styling.

// SubFormat is one file a subtitle language comes in.
type SubFormat struct {
	Ext      string            `json:"ext"` // "vtt", "srt", "ttml", "json3"...
	URL      string            `json:"url"`
	Name     string            `json:"name"` // "English"
	Protocol string            `json:"protocol"`
	Headers  map[string]string `json:"http_headers"`
}

// Subtitle is a language offered in the picker.
type Subtitle struct {
	ID   string // "en", or "auto:en-orig" for automatic captions
	Name string // "English", "English (auto-generated)"
	Auto bool
}

const autoPrefix = "auto:"

// skipSubs are "subtitle" tracks that aren't subtitles.
var skipSubs = map[string]bool{"live_chat": true, "rechat": true, "danmaku": true, "comments": true}

// SubtitleChoices lists a video's subtitle languages: people-made ones
// first, by name, then the original-language automatic captions.
func SubtitleChoices(info *Info) []Subtitle {
	var subs []Subtitle
	for key, fs := range info.Subtitles {
		if skipSubs[key] || len(fs) == 0 {
			continue
		}
		subs = append(subs, Subtitle{ID: key, Name: subName(key, fs)})
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })

	// Automatic captions: the original language only. yt-dlp marks it
	// "xx-orig"; older versions don't, then it's the video's language.
	auto := ""
	for key := range info.AutoCaptions {
		if strings.HasSuffix(key, "-orig") {
			auto = key
		}
	}
	if auto == "" && info.Language != "" && len(info.AutoCaptions[info.Language]) > 0 {
		auto = info.Language
	}
	if auto != "" {
		name := strings.TrimSuffix(subName(auto, info.AutoCaptions[auto]), " (Original)")
		subs = append(subs, Subtitle{ID: autoPrefix + auto, Name: name + " (auto-generated)", Auto: true})
	}
	return subs
}

func subName(key string, fs []SubFormat) string {
	for _, f := range fs {
		if f.Name != "" {
			return f.Name
		}
	}
	return key
}

// SubtitleFormats returns the files a subtitle choice comes in.
func SubtitleFormats(info *Info, id string) []SubFormat {
	if key, ok := strings.CutPrefix(id, autoPrefix); ok {
		return info.AutoCaptions[key]
	}
	return info.Subtitles[id]
}

// SubtitleFormats looks a page up (or uses the picker's minutes-old
// look-up) and returns the files subtitle id comes in
// (none, if the page doesn't offer it any more: SaveSubtitle then asks
// yt-dlp).
func (b Backend) SubtitleFormats(ctx context.Context, pageURL, id string, headers map[string]string) ([]SubFormat, error) {
	info, err := b.latest(ctx, pageURL, headers)
	if err != nil {
		return nil, err
	}
	return SubtitleFormats(info, id), nil
}

// subFileLang is the language part of the file name: "en" for "en",
// "auto:en-orig" and "en-orig".
func subFileLang(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(id, autoPrefix), "-orig")
}

// SaveSubtitle saves subtitle id of a video next to the video file, and
// returns the subtitle file's path. formats are the files the language
// comes in (from the latest look-up); if none can be fetched directly,
// yt-dlp fetches it. headers are the browser's, if the page came from it.
func (b Backend) SaveSubtitle(ctx context.Context, pageURL, id string, formats []SubFormat,
	videoPath string, headers map[string]string) (string, error) {
	stem := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + "." + subFileLang(id)

	if f := pickSubFormat(formats); f != nil {
		data, err := fetchSubtitle(ctx, f, pageURL, headers)
		if err == nil {
			return writeSubtitle(stem, f.Ext, data)
		}
		// YouTube sometimes refuses direct requests for subtitles: yt-dlp
		// knows how to ask.
	}
	if !b.Tools.Installed() {
		return "", ErrNoTools
	}
	path, ext, err := b.ytdlpSubtitle(ctx, pageURL, id, stem, headers)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	os.Remove(path)
	if err != nil {
		return "", err
	}
	return writeSubtitle(stem, ext, data)
}

// pickSubFormat: SRT as it is, then WebVTT (converted), then ASS (kept).
// Formats delivered as a playlist of pieces are left to yt-dlp.
func pickSubFormat(formats []SubFormat) *SubFormat {
	for _, want := range []string{"srt", "vtt", "ass", "ssa"} {
		for i := range formats {
			f := &formats[i]
			if f.Ext == want && f.URL != "" && !strings.HasPrefix(f.Protocol, "m3u8") && !strings.HasPrefix(f.Protocol, "http_dash") {
				return f
			}
		}
	}
	return nil
}

// maxSubtitle: subtitles are small; anything bigger is a mistake.
const maxSubtitle = 20 << 20

func fetchSubtitle(ctx context.Context, f *SubFormat, pageURL string, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		// The site's cookies only go to the site itself.
		if k == "Cookie" && !sameHost(pageURL, f.URL) {
			continue
		}
		req.Header.Set(k, v)
	}
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitle+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSubtitle {
		return nil, errors.New("the subtitle file is far too big")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, errors.New("the subtitle file is empty")
	}
	return data, nil
}

// sameHost: does addr belong to page's site (the same host, or one of its
// subdomains)?
func sameHost(page, addr string) bool {
	p, err1 := url.Parse(page)
	a, err2 := url.Parse(addr)
	if err1 != nil || err2 != nil {
		return false
	}
	site := strings.TrimPrefix(p.Hostname(), "www.")
	host := a.Hostname()
	return host == site || strings.HasSuffix(host, "."+site)
}

// writeSubtitle writes stem + ".srt" (WebVTT converted), or the subtitle
// in its own format if it's already SRT or ASS.
func writeSubtitle(stem, ext string, data []byte) (string, error) {
	switch ext {
	case "vtt":
		srt, err := VTTToSRT(data)
		if err != nil {
			return "", err
		}
		data, ext = srt, "srt"
	case "srt":
		data = fixSRT([]byte(strings.TrimPrefix(string(data), "\uFEFF")))
	case "ass", "ssa":
	default:
		// Formats we don't convert (TTML, json3...): kept as they are, so
		// at least the text is there.
	}
	path := stem + "." + ext
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// ytdlpSubtitle has yt-dlp fetch one subtitle language (without the
// video), and returns the file it wrote and its format.
func (b Backend) ytdlpSubtitle(ctx context.Context, pageURL, id, stem string, headers map[string]string) (string, string, error) {
	o, cleanup, err := b.options(pageURL, headers)
	if err != nil {
		return "", "", err
	}
	defer cleanup()
	key, auto := strings.CutPrefix(id, autoPrefix)
	write := "--write-subs"
	if auto {
		write = "--write-auto-subs"
	}
	// yt-dlp names the file <output>.<language>.<format>; we give it a
	// temporary output name in the same folder, so nothing of ours is
	// overwritten if it fails.
	tmp := stem + ".dlm-sub"
	args := []string{
		"--ignore-config", "--no-playlist", "--no-warnings", "--skip-download",
		write, "--sub-langs", key, "--sub-format", "srt/vtt/ass/best",
		"-o", strings.ReplaceAll(tmp, "%", "%%") + ".%(ext)s",
	}
	args = append(args, o.args()...)
	args = append(args, "--", pageURL)

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Tools.YtDlp(), args...)
	hideWindow(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", ytdlpError(stderr.String(), err)
	}
	matches, _ := filepath.Glob(globEscape(tmp) + ".*")
	for _, m := range matches {
		ext := strings.TrimPrefix(filepath.Ext(m), ".")
		if ext != "part" && ext != "tmp" {
			return m, ext, nil
		}
	}
	return "", "", errors.New("the site didn't provide those subtitles")
}

// globEscape escapes the characters filepath.Glob treats as special.
func globEscape(s string) string {
	r := strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]")
	if os.PathSeparator != '\\' {
		r = strings.NewReplacer("[", "\\[", "*", "\\*", "?", "\\?", "\\", "\\\\")
	}
	return r.Replace(s)
}

// ---- WebVTT to SubRip ----

// A cue's timing line: "00:01.000 --> 00:04.000 align:start" (hours optional).
var (
	vttTiming = regexp.MustCompile(`^((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})\s+-->\s+((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})`)
	// Tags inside cue text. <i>, <b>, <u> are kept (SRT has them too); the
	// rest - <c.colour>, <v Speaker>, <00:00:01.500> word timings, <ruby> -
	// are dropped, keeping their text.
	vttTag      = regexp.MustCompile(`</?([a-zA-Z]+)[^>]*>|<\d[\d:.]*>`)
	vttWordTime = regexp.MustCompile(`<\d+:\d{2}(:\d{2})?\.\d+>`)
)

type cue struct {
	start, end time.Duration
	lines      []string
}

// VTTToSRT converts WebVTT subtitles to SubRip.
//
// YouTube's automatic captions need an extra step: they "roll" - each cue
// repeats the line before it and adds a new one, word by word, so a plain
// conversion shows every line two or three times. Those files are
// recognised by their word timings (<00:00:01.500>) and the repeats taken
// out, leaving each line once, at the time it was first said.
func VTTToSRT(data []byte) ([]byte, error) {
	text := strings.TrimPrefix(string(data), "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if !strings.HasPrefix(strings.TrimSpace(text), "WEBVTT") {
		return nil, errors.New("not a WebVTT file")
	}
	rolling := vttWordTime.MatchString(text)

	var cues []cue
	for _, block := range strings.Split(text, "\n\n") {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		// The cue's timing is on its first or second line (an optional ID
		// comes first). Header, NOTE, STYLE and REGION blocks have none.
		ti := -1
		for i := 0; i < len(lines) && i < 2; i++ {
			if vttTiming.MatchString(strings.TrimSpace(lines[i])) {
				ti = i
				break
			}
		}
		if ti < 0 {
			continue
		}
		m := vttTiming.FindStringSubmatch(strings.TrimSpace(lines[ti]))
		c := cue{start: vttTime(m[1]), end: vttTime(m[2])}
		for _, l := range lines[ti+1:] {
			l = strings.TrimSpace(cleanCueText(l))
			if l != "" {
				c.lines = append(c.lines, l)
			}
		}
		if len(c.lines) > 0 && c.end > c.start {
			cues = append(cues, c)
		}
	}
	if rolling {
		cues = unroll(cues)
	}
	return writeSRT(pairUp(cues)), nil
}

func writeSRT(cues []cue) []byte {
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\r\n%s --> %s\r\n%s\r\n\r\n", i+1, srtTime(c.start), srtTime(c.end), strings.Join(c.lines, "\r\n"))
	}
	return []byte(b.String())
}

// ---- Overlapping subtitles ----
//
// YouTube's automatic captions (as SRT too) show each line for about five
// seconds and start the next one halfway through: two subtitles on screen
// at once. Players stack those with the NEWER one on top, so the text reads
// backwards. pairUp turns them into ordinary two-line subtitles that don't
// overlap: lines 1 and 2 together, then 3 and 4, and so on - each line
// shown once, in order.

// pairUp joins each subtitle with the next one when they overlap, into one
// subtitle that lasts until the one after starts. Subtitles that don't
// overlap stay as they are.
func pairUp(cues []cue) []cue {
	if !overlapping(cues) {
		return cues
	}
	sorted := append([]cue(nil), cues...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	var out []cue
	for i := 0; i < len(sorted); {
		c := sorted[i]
		take := 1
		if i+1 < len(sorted) && c.end > sorted[i+1].start {
			next := sorted[i+1]
			c = cue{start: c.start, end: max(c.end, next.end), lines: append(append([]string(nil), c.lines...), next.lines...)}
			take = 2
		}
		// It gives way to whatever comes after.
		if i+take < len(sorted) && c.end > sorted[i+take].start {
			c.end = sorted[i+take].start
		}
		if c.end > c.start {
			out = append(out, c)
		}
		i += take
	}
	return out
}

func overlapping(cues []cue) bool {
	sorted := append([]cue(nil), cues...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	for i := 0; i+1 < len(sorted); i++ {
		if sorted[i].end > sorted[i+1].start {
			return true
		}
	}
	return false
}

// srtTiming is an SRT timing line: "00:00:01,000 --> 00:00:04,000".
var srtTiming = regexp.MustCompile(`^(\d+:\d{2}:\d{2}[,.]\d{1,3})\s*-->\s*(\d+:\d{2}:\d{2}[,.]\d{1,3})`)

// fixSRT pairs up an SRT file's overlapping subtitles. A file without any
// is returned exactly as it was.
func fixSRT(data []byte) []byte {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var cues []cue
	for _, block := range strings.Split(text, "\n\n") {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		ti := -1
		for i := 0; i < len(lines) && i < 2; i++ {
			if srtTiming.MatchString(strings.TrimSpace(lines[i])) {
				ti = i
				break
			}
		}
		if ti < 0 {
			continue
		}
		m := srtTiming.FindStringSubmatch(strings.TrimSpace(lines[ti]))
		c := cue{start: vttTime(m[1]), end: vttTime(m[2])}
		for _, l := range lines[ti+1:] {
			if l = strings.TrimRight(l, " \t"); l != "" {
				c.lines = append(c.lines, l)
			}
		}
		if len(c.lines) > 0 && c.end > c.start {
			cues = append(cues, c)
		}
	}
	if !overlapping(cues) {
		return data
	}
	return writeSRT(pairUp(cues))
}

// unroll removes the repeated lines of rolling captions: lines a cue shares
// with the end of the one before it were already shown.
func unroll(cues []cue) []cue {
	var out []cue
	var prev []string
	for _, c := range cues {
		lines := c.lines
		// Drop as many leading lines as match the previous cue's last ones.
		for n := min(len(prev), len(lines)); n > 0; n-- {
			if equalLines(prev[len(prev)-n:], lines[:n]) {
				lines = lines[n:]
				break
			}
		}
		prev = c.lines
		if len(lines) == 0 {
			// Nothing new: just the old line held on screen. Keep the
			// previous cue up until this one ends.
			if len(out) > 0 && c.end > out[len(out)-1].end {
				out[len(out)-1].end = c.end
			}
			continue
		}
		out = append(out, cue{start: c.start, end: c.end, lines: lines})
	}
	// A line stays up until the next one starts (they overlapped while
	// rolling).
	for i := 0; i+1 < len(out); i++ {
		if out[i].end > out[i+1].start {
			out[i].end = out[i+1].start
		}
	}
	return out
}

func equalLines(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var vttEntities = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&nbsp;", " ", "&lrm;", "", "&rlm;", "", "&quot;", `"`, "&#39;", "'",
)

func cleanCueText(l string) string {
	l = vttTag.ReplaceAllStringFunc(l, func(tag string) string {
		name := strings.ToLower(strings.Trim(tag, "</>"))
		if i := strings.IndexAny(name, " ."); i >= 0 {
			name = name[:i]
		}
		switch name {
		case "i", "b", "u":
			if strings.HasPrefix(tag, "</") {
				return "</" + name + ">"
			}
			return "<" + name + ">"
		}
		return ""
	})
	return vttEntities.Replace(l)
}

// vttTime reads "01:02.500" or "1:01:02.500".
func vttTime(s string) time.Duration {
	parts := strings.Split(strings.ReplaceAll(s, ",", "."), ":")
	total, _ := strconv.ParseFloat(parts[len(parts)-1], 64)
	unit := 60.0
	for i := len(parts) - 2; i >= 0; i-- {
		n, _ := strconv.Atoi(parts[i])
		total += float64(n) * unit
		unit *= 60
	}
	return time.Duration(math.Round(total*1000)) * time.Millisecond
}

// srtTime writes "01:02:03,500".
func srtTime(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
