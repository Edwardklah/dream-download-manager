package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dlmanager/manager"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The everyday extras around the download list:
//
//   - the scheduler's state, for the window (the scheduling itself is in
//     package manager);
//   - "when done": sleep or shut down the computer after the last download;
//   - checking now and then for a newer yt-dlp;
//   - clipboard catching: copy a download link anywhere, dlmanager offers it;
//   - starting with Windows (in the tray).

const (
	queueStateEvent  = "queue-state"    // manager.QueueState
	whenDoneEvent    = "when-done"      // WhenDoneState
	toolsUpdateEvent = "tools-update"   // ToolsUpdate
	clipboardEvent   = "clipboard-link" // a copied download link
)

func init() {
	application.RegisterEvent[manager.QueueState](queueStateEvent)
	application.RegisterEvent[WhenDoneState](whenDoneEvent)
	application.RegisterEvent[ToolsUpdate](toolsUpdateEvent)
	application.RegisterEvent[string](clipboardEvent)
}

type extrasState struct {
	lastQueue manager.QueueState

	whenMu       sync.Mutex
	whenAction   string // "", "sleep", "shutdown"
	whenTimer    *time.Timer
	whenDeadline time.Time

	updateMu sync.Mutex
	update   ToolsUpdate // a newer yt-dlp, if found

	clipMu   sync.Mutex
	clipLink string // a copied link waiting for the window
}

func (s *DownloadService) startExtras() {
	s.manager.SetOnAllDone(s.allDone)
	// Starting with Windows points at the program's path; if the program
	// was moved or renamed (gui.exe became DreamDM.exe), point it at this one.
	if startsWithSystem() {
		go setStartWithSystem(true)
	}
	go s.updateLoop()
	s.noteVersionRun() // before the window asks JustUpdated
	go s.appUpdateLoop()
	go s.watchClipboard()
}

// ---- The scheduler ----

// QueueState: is the queue waiting for the schedule's start time?
func (s *DownloadService) QueueState() manager.QueueState { return s.manager.QueueState() }

// sendQueueState tells the window when the queue starts or stops waiting.
// (Called with every list update.)
func (s *DownloadService) sendQueueState() {
	st := s.manager.QueueState()
	if st != s.lastQueue {
		s.lastQueue = st
		s.app.Event.Emit(queueStateEvent, st)
	}
}

// ---- When done ----

// WhenDoneState: what happens when the last download finishes.
type WhenDoneState struct {
	Action      string `json:"action"`      // "", "sleep", "shutdown"
	SecondsLeft int    `json:"secondsLeft"` // > 0 while counting down to it
}

const whenDoneDelay = 60 * time.Second

// SetWhenDone chooses what happens once the downloads finish: "" (nothing),
// "sleep" or "shutdown". It happens once, then goes back to nothing - so a
// forgotten setting can't switch the computer off another day.
func (s *DownloadService) SetWhenDone(action string) WhenDoneState {
	if action != "sleep" && action != "shutdown" {
		action = ""
	}
	s.whenMu.Lock()
	if s.whenTimer == nil {
		s.whenAction = action
	}
	st := s.whenStateLocked()
	s.whenMu.Unlock()
	return st
}

func (s *DownloadService) WhenDone() WhenDoneState {
	s.whenMu.Lock()
	defer s.whenMu.Unlock()
	return s.whenStateLocked()
}

func (s *DownloadService) whenStateLocked() WhenDoneState {
	st := WhenDoneState{Action: s.whenAction}
	if s.whenTimer != nil {
		st.SecondsLeft = max(1, int(time.Until(s.whenDeadline).Seconds()+0.5))
	}
	return st
}

// CancelWhenDone stops a countdown (and the action).
func (s *DownloadService) CancelWhenDone() {
	s.whenMu.Lock()
	if s.whenTimer != nil {
		s.whenTimer.Stop()
		s.whenTimer = nil
	}
	s.whenAction = ""
	st := s.whenStateLocked()
	s.whenMu.Unlock()
	s.app.Event.Emit(whenDoneEvent, st)
}

// allDone is called by the manager when the last download has finished.
// With an action chosen, it counts down a minute in the window - time to
// cancel - then does it.
func (s *DownloadService) allDone() {
	s.whenMu.Lock()
	action := s.whenAction
	if action == "" || s.whenTimer != nil {
		s.whenMu.Unlock()
		return
	}
	s.whenDeadline = time.Now().Add(whenDoneDelay)
	s.whenTimer = time.AfterFunc(whenDoneDelay, func() {
		s.whenMu.Lock()
		fire := s.whenTimer != nil // not cancelled meanwhile
		s.whenTimer = nil
		s.whenAction = ""
		s.whenMu.Unlock()
		if !fire {
			return
		}
		s.app.Event.Emit(whenDoneEvent, WhenDoneState{})
		// (The download list is saved after every change, so nothing is
		// lost if the computer goes off now.)
		switch action {
		case "sleep":
			suspendComputer()
		case "shutdown":
			shutDownComputer()
		}
	})
	st := s.whenStateLocked()
	s.whenMu.Unlock()
	bringToFront()
	s.app.Event.Emit(whenDoneEvent, st)
}

// ---- Keeping yt-dlp up to date ----

// ToolsUpdate: a newer yt-dlp than the installed one.
type ToolsUpdate struct {
	Available bool   `json:"available"`
	Current   string `json:"current"`
	Latest    string `json:"latest"`
}

// PendingToolsUpdate returns a newer yt-dlp found by the last check (that
// you haven't said "not now" to).
func (s *DownloadService) PendingToolsUpdate() ToolsUpdate {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	return s.update
}

// DismissToolsUpdate: "not now" - this version isn't offered again.
func (s *DownloadService) DismissToolsUpdate() {
	s.updateMu.Lock()
	latest := s.update.Latest
	s.update = ToolsUpdate{}
	s.updateMu.Unlock()
	os.WriteFile(filepath.Join(s.tools.Dir, "update-dismissed.txt"), []byte(latest), 0o644)
	s.app.Event.Emit(toolsUpdateEvent, ToolsUpdate{})
}

// updateLoop checks for a newer yt-dlp a little after the app starts, then
// every few hours while it runs. GitHub is asked at most once a week.
func (s *DownloadService) updateLoop() {
	select {
	case <-s.ctx.Done():
		return
	case <-time.After(30 * time.Second): // not while the app is starting
	}
	for {
		s.checkTools(7 * 24 * time.Hour)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(6 * time.Hour):
		}
	}
}

// checkTools asks whether a newer yt-dlp exists (if the last check is older
// than every) and offers it.
func (s *DownloadService) checkTools(every time.Duration) {
	if !s.manager.Settings().CheckToolUpdates || !s.tools.Installed() || s.toolsBusy.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	defer cancel()
	u, _ := s.tools.CheckForUpdate(ctx, every)
	dismissed, _ := os.ReadFile(filepath.Join(s.tools.Dir, "update-dismissed.txt"))
	offer := ToolsUpdate{}
	if u.Newer() && strings.TrimSpace(string(dismissed)) != u.Latest {
		offer = ToolsUpdate{Available: true, Current: u.Current, Latest: u.Latest}
	}
	s.updateMu.Lock()
	changed := offer != s.update
	s.update = offer
	s.updateMu.Unlock()
	if changed {
		s.app.Event.Emit(toolsUpdateEvent, offer)
	}
}

// toolsUpdated clears the offer once the update is installed.
func (s *DownloadService) toolsUpdated() {
	s.updateMu.Lock()
	s.update = ToolsUpdate{}
	s.updateMu.Unlock()
	s.app.Event.Emit(toolsUpdateEvent, ToolsUpdate{})
}

// ---- Clipboard catching ----

// watchClipboard looks at the clipboard about once a second. On Windows it
// first asks for the clipboard's change counter, so the clipboard itself is
// only opened when something new was copied.
//
// The clipboard is read straight from the system (clipboardText), not
// through Wails: Wails reads it on the app's main thread and waits, and this
// watcher starts while the app is still starting - before the main thread
// takes requests - so its first read waited forever, and nothing was ever
// offered.
func (s *DownloadService) watchClipboard() {
	select { // let the app finish starting
	case <-s.ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	last, _ := clipboardText(s.app) // what was there before: not offered
	seq := clipboardSequence()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if n := clipboardSequence(); n != 0 {
			if n == seq {
				continue
			}
			seq = n
		}
		text, ok := clipboardText(s.app)
		if !ok || text == last {
			continue
		}
		last = text
		s.prefetch(text) // a video link: look it up already, if Settings says so
		if !s.manager.Settings().WatchClipboard {
			continue
		}
		link, ok := manager.LinkToOffer(text)
		if !ok || s.inList(link) {
			continue
		}
		s.clipMu.Lock()
		s.clipLink = link
		s.clipMu.Unlock()
		bringToFront()
		s.app.Event.Emit(clipboardEvent, link)
	}
}

// inList: is this link already in the list (downloading, waiting or
// done)? Copying it again then offers nothing - pasting it in the link box
// still downloads it again, as "name (1)".
func (s *DownloadService) inList(link string) bool {
	for _, it := range s.manager.List() {
		if it.URL == link {
			return true
		}
	}
	return false
}

// TakeClipboardLink returns (once) a copied link waiting to be offered.
func (s *DownloadService) TakeClipboardLink() string {
	s.clipMu.Lock()
	defer s.clipMu.Unlock()
	l := s.clipLink
	s.clipLink = ""
	return l
}

// ---- Starting with Windows ----

// StartsWithSystem: does the app start (in the tray) when you sign in?
func (s *DownloadService) StartsWithSystem() bool { return startsWithSystem() }

func (s *DownloadService) SetStartWithSystem(on bool) error { return setStartWithSystem(on) }
