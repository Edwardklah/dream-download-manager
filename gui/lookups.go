package main

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dlmanager/video"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Video look-ups as jobs, so one can start before the picker opens and the
// picker just picks up its result:
//
//   - A video link you copy is looked up straight away, in the background,
//     if Settings says so (PrefetchVideos). It costs a few MB of data per
//     link, so it's off unless switched on.
//   - The picker (LookUpVideo) joins a look-up of the same page that's
//     still running, or uses one that finished in the last few minutes,
//     instead of starting again.
//   - While it runs, each step yt-dlp reports goes to the window
//     ("video-step"), so the picker can show more than a spinner.

type lookJob struct {
	done   chan struct{} // closed when finished
	lookup VideoLookup
	err    error
	at     time.Time // when it finished

	mu   sync.Mutex
	step string // what yt-dlp is doing now
}

// A finished look-up is reused for this long (stream addresses last hours).
const lookFresh = 5 * time.Minute

// VideoStep is a look-up's progress, for the picker.
type VideoStep struct {
	URL  string `json:"url"`
	Step string `json:"step"`
}

const videoStepEvent = "video-step"

func init() { application.RegisterEvent[VideoStep](videoStepEvent) }

type lookState struct {
	looksMu     sync.Mutex
	looks       map[string]*lookJob
	prefetching atomic.Bool // one copied link at a time
}

// lookJob returns the look-up of pageURL: one already running or minutes
// old, or a new one, started now.
func (s *DownloadService) lookJob(pageURL string) *lookJob {
	key := pageURL + headersKey(s.headersFor(pageURL))
	s.looksMu.Lock()
	defer s.looksMu.Unlock()
	if s.looks == nil {
		s.looks = map[string]*lookJob{}
	}
	for k, j := range s.looks { // forget old ones
		if finished(j) && time.Since(j.at) > lookFresh {
			delete(s.looks, k)
		}
	}
	if j := s.looks[key]; j != nil && (!finished(j) || j.err == nil && time.Since(j.at) <= lookFresh) {
		return j
	}
	j := &lookJob{done: make(chan struct{})}
	s.looks[key] = j
	go func() {
		j.lookup, j.err = s.lookUp(pageURL, func(step string) {
			j.mu.Lock()
			j.step = step
			j.mu.Unlock()
			if s.app != nil {
				s.app.Event.Emit(videoStepEvent, VideoStep{URL: pageURL, Step: step})
			}
		})
		j.at = time.Now()
		close(j.done)
	}()
	return j
}

func finished(j *lookJob) bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// VideoLookupStep: what the look-up of a page is doing now ("" if none is
// running), for a picker opened while it's already under way.
func (s *DownloadService) VideoLookupStep(pageURL string) string {
	s.looksMu.Lock()
	j := s.looks[pageURL+headersKey(s.headersFor(pageURL))]
	s.looksMu.Unlock()
	if j == nil || finished(j) {
		return ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.step
}

// prefetch starts looking up a copied video link, if Settings asks for it.
// One at a time: copying several links quickly doesn't start several.
func (s *DownloadService) prefetch(text string) {
	link := strings.TrimSpace(text)
	if !s.manager.Settings().PrefetchVideos || strings.ContainsAny(link, " \t\r\n") ||
		!video.IsVideoPage(link) || !s.tools.Installed() || s.inList(link) {
		return
	}
	if !s.prefetching.CompareAndSwap(false, true) {
		return
	}
	j := s.lookJob(link)
	go func() {
		<-j.done
		s.prefetching.Store(false)
	}()
}

// headersKey: a look-up only counts for the same browser details (cookies
// can change what a site shows).
func headersKey(h map[string]string) string {
	if len(h) == 0 {
		return ""
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("\x00" + k + "=" + h[k])
	}
	return b.String()
}
