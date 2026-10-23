package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dlmanager/bridge"
	"dlmanager/manager"
	"dlmanager/video"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Video downloads, as the window sees them:
//
//  1. A pasted link that IsVideoPage says is a video page (or a page sent by
//     the browser extension) opens the quality picker.
//  2. The picker calls LookUpVideo: yt-dlp lists the qualities.
//  3. AddVideo puts the chosen one in the download list.
//
// yt-dlp itself is fetched once, from Settings (InstallVideoTools).

type videoState struct {
	tools   video.Tools
	backend video.Backend

	toolsBusy atomic.Bool

	pendingMu sync.Mutex
	pending   VideoRequest // a video page from the browser, waiting for the window
	// The browser's cookies and identity for video pages it sent, by page.
	// Kept here, never sent to the window: cookies are private.
	browserHeaders map[string]map[string]string
	// What the button told us about each page (tab title, embedded player...).
	hints map[string]bridge.Request
}

// hintFor returns what the browser said about a page, if anything.
func (s *DownloadService) hintFor(pageURL string) bridge.Request {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	return s.hints[pageURL]
}

// cleanTitle tidies a browser tab's title into a file name: "Watch X
// English Sub online Free on Site" -> "X English Sub". Sites put their name
// at the end after a separator; drop it if what's left is still a title.
func cleanTitle(t string) string {
	t = strings.TrimSpace(t)
	for _, sep := range []string{" | ", " - ", " – ", " — "} {
		if i := strings.LastIndex(t, sep); i > 10 {
			t = t[:i]
		}
	}
	t = strings.TrimPrefix(t, "Watch ")
	if i := strings.Index(strings.ToLower(t), " online"); i > 10 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// headersFor returns the browser details that came with a page, if any.
func (s *DownloadService) headersFor(pageURL string) map[string]string {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	return s.browserHeaders[pageURL]
}

// VideoRequest is a video page sent by the browser extension.
type VideoRequest struct {
	URL    string `json:"url"`
	Source string `json:"source"` // the page's video file, if it's an ordinary one (fallback)
}

// Events for the frontend.
const (
	toolsEvent        = "video-tools"   // progress of the tools download
	videoRequestEvent = "video-request" // the browser sent a video page
)

// ToolsStatus describes the video tools.
type ToolsStatus struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Busy      bool   `json:"busy"`
	File      string `json:"file"` // being downloaded
	Done      int64  `json:"done"`
	Total     int64  `json:"total"`
	Error     string `json:"error"`
}

// VideoChoice is one quality in the picker.
type VideoChoice struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Detail     string `json:"detail"`
	Size       int64  `json:"size"`
	SizeApprox bool   `json:"sizeApprox"`
	Ext        string `json:"ext"`
}

// VideoLookup is what the picker shows.
type VideoLookup struct {
	NeedsTools bool            `json:"needsTools"` // yt-dlp must be downloaded first
	PageURL    string          `json:"pageUrl"`
	Title      string          `json:"title"`
	Uploader   string          `json:"uploader"`
	Duration   float64         `json:"duration"`
	Thumbnail  string          `json:"thumbnail"`
	Site       string          `json:"site"`
	Choices    []VideoChoice   `json:"choices"`
	Suggested  string          `json:"suggested"` // the choice to pre-select
	Subtitles  []VideoSubtitle `json:"subtitles"`
}

// VideoSubtitle is a subtitle language in the picker.
type VideoSubtitle struct {
	ID   string `json:"id"`   // "en", "auto:en-orig"
	Name string `json:"name"` // "English", "English (auto-generated)"
	Auto bool   `json:"auto"`
}

func init() {
	application.RegisterEvent[ToolsStatus](toolsEvent)
	application.RegisterEvent[string](videoRequestEvent)
}

func (s *DownloadService) setUpVideo(dataDir string) {
	s.tools = video.Tools{Dir: filepath.Join(dataDir, "tools")}
	s.backend = video.Backend{Tools: s.tools}
	s.manager.SetVideoBackend(s.backend)
}

// IsVideoPage: should this pasted link open the quality picker?
func (s *DownloadService) IsVideoPage(link string) bool { return video.IsVideoPage(link) }

// LookUpVideo asks yt-dlp what qualities a video page offers.
func (s *DownloadService) LookUpVideo(pageURL string) (VideoLookup, error) {
	if !s.tools.Installed() {
		return VideoLookup{NeedsTools: true, PageURL: pageURL}, nil
	}
	hint := s.hintFor(pageURL)
	info, choices, err := s.backend.Look(s.ctx, pageURL, s.headersFor(pageURL))
	if err != nil {
		// Sites change and yt-dlp catches up: if a look-up fails, check
		// (at most daily) whether a newer yt-dlp is out, and offer it.
		go s.checkTools(24 * time.Hour)
		if errors.Is(err, video.ErrNotVideo) {
			if hint.NotLoaded {
				return VideoLookup{}, errors.New("this player hasn't loaded the video yet: press play, then click Download again")
			}
			return VideoLookup{}, errors.New("no video was found on that page")
		}
		return VideoLookup{}, err
	}
	if len(choices) == 0 {
		return VideoLookup{}, errors.New("this video comes in a form Dream Download Manager can't download yet")
	}
	l := VideoLookup{
		PageURL: info.PageURL, Title: info.Title, Uploader: info.Uploader,
		Duration: info.Duration, Thumbnail: info.Thumbnail, Site: info.Extractor,
	}
	if l.PageURL == "" {
		l.PageURL = pageURL
	}
	// A player embedded from another site is named after itself ("MegaPlay
	// Player"), and a page yt-dlp only knows in general is often named
	// after the site: the browser tab's title says better what it is.
	if t := cleanTitle(hint.Title); t != "" && (hint.Embedded || info.Extractor == "Generic") {
		l.Title = t
	}
	for _, c := range choices {
		l.Choices = append(l.Choices, VideoChoice{
			ID: c.ID, Label: c.Label, Detail: c.Detail, Size: c.Size, SizeApprox: c.SizeApprox, Ext: c.Ext,
		})
	}
	l.Suggested = suggest(choices)
	for _, sub := range video.SubtitleChoices(info) {
		l.Subtitles = append(l.Subtitles, VideoSubtitle{ID: sub.ID, Name: sub.Name, Auto: sub.Auto})
	}
	return l, nil
}

// suggest pre-selects the best quality up to 1080p: more is rarely worth
// the data, and 1080p in H.264 plays on everything.
func suggest(choices []video.Choice) string {
	for _, c := range choices {
		if c.Video != nil && shortSide(c.Video) <= 1080 {
			return c.ID
		}
	}
	if len(choices) > 0 {
		return choices[0].ID
	}
	return ""
}

func shortSide(f *video.Format) int {
	if f.Width > 0 && f.Width < f.Height {
		return f.Width
	}
	return f.Height
}

// AddVideo puts a chosen quality in the download list. subtitle is a
// subtitle language to save with it (VideoSubtitle.ID), or "".
func (s *DownloadService) AddVideo(pageURL string, c VideoChoice, title, subtitle string, startNow bool) (manager.Item, error) {
	headers := s.pageHeaders(pageURL)
	return s.manager.AddVideo(pageURL, manager.VideoOptions{
		ChoiceID: c.ID, Quality: c.Label, Title: title, Ext: c.Ext, StartNow: startNow, Headers: headers,
		Subtitle: subtitle,
	})
}

// AddSubtitles downloads just a video's subtitles (language subtitle), not
// the video: for a video you already have, or to fix the subtitles yourself.
func (s *DownloadService) AddSubtitles(pageURL, title, subtitle string) (manager.Item, error) {
	return s.manager.AddSubtitles(pageURL, title, subtitle, s.pageHeaders(pageURL))
}

// pageHeaders returns the browser details for a video page, if it came
// from the browser. yt-dlp may have tidied the address (webpage_url), so
// it's matched by the original too.
func (s *DownloadService) pageHeaders(pageURL string) map[string]string {
	if h := s.headersFor(pageURL); h != nil {
		return h
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for page, h := range s.browserHeaders {
		if video.SamePage(page, pageURL) {
			return h
		}
	}
	return nil
}

// VideoTools reports whether yt-dlp is installed, and its version.
func (s *DownloadService) VideoTools() ToolsStatus {
	return ToolsStatus{Installed: s.tools.Installed(), Version: s.tools.Version(s.ctx), Busy: s.toolsBusy.Load()}
}

// InstallVideoTools downloads (or updates) yt-dlp and QuickJS in the
// background, reporting progress as "video-tools" events.
func (s *DownloadService) InstallVideoTools() {
	if !s.toolsBusy.CompareAndSwap(false, true) {
		return // already running
	}
	go func() {
		defer s.toolsBusy.Store(false)
		var last time.Time
		err := s.tools.Install(s.ctx, func(file string, done, total int64) {
			if time.Since(last) < 200*time.Millisecond && done < total {
				return
			}
			last = time.Now()
			s.app.Event.Emit(toolsEvent, ToolsStatus{Busy: true, File: file, Done: done, Total: total})
		})
		st := ToolsStatus{Installed: s.tools.Installed(), Version: s.tools.Version(context.Background())}
		if err != nil {
			st.Error = err.Error()
		} else {
			s.toolsUpdated()
		}
		s.app.Event.Emit(toolsEvent, st)
	}()
}

// OpenVideo is called through the bridge when the browser extension sends
// a video page: show the window with the quality picker for it.
func (s *DownloadService) OpenVideo(req bridge.Request) error {
	vr := VideoRequest{URL: req.URL}
	if src := req.Source; strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://") {
		vr.Source = src
	}
	s.pendingMu.Lock()
	s.pending = vr
	if h := req.Headers(); len(h) > 0 {
		if s.browserHeaders == nil {
			s.browserHeaders = map[string]map[string]string{}
		}
		delete(h, "Referer") // the page itself; not needed to read it
		s.browserHeaders[req.URL] = h
	}
	if s.hints == nil {
		s.hints = map[string]bridge.Request{}
	}
	s.hints[req.URL] = bridge.Request{Title: req.Title, Embedded: req.Embedded, NotLoaded: req.NotLoaded}
	s.pendingMu.Unlock()
	bringToFront()
	// A window that's already open gets the event (and then asks); one
	// that's just been created asks with TakeVideoRequest once it has loaded.
	s.app.Event.Emit(videoRequestEvent, req.URL)
	return nil
}

// TakeVideoRequest returns (once) a video page the browser sent.
func (s *DownloadService) TakeVideoRequest() VideoRequest {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	vr := s.pending
	s.pending = VideoRequest{}
	return vr
}
