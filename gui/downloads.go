package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"time"

	"dlmanager/bridge"
	"dlmanager/engine"
	"dlmanager/manager"
	"dlmanager/video"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// DownloadService connects the download manager (dlmanager/manager) to the
// window. It's deliberately thin: all the real logic - the queue, pausing,
// saving the list - lives in the manager package, where it's tested.
//
// Its EXPORTED methods can be called from the frontend: Wails generates
// matching TypeScript functions in frontend/bindings. Changes go the other
// way as a "downloads" event carrying the whole list.
type DownloadService struct {
	app     *application.App
	ctx     context.Context
	manager *manager.Manager
	limits  *engine.HostLimits
	dirty   atomic.Bool // the list changed since the last event

	videoState  // video downloads: see video.go
	extrasState // scheduler, when done, updates, clipboard: see extras.go
}

// The event the frontend listens to: the full list, at most 4x a second.
const downloadsEvent = "downloads"

func init() {
	// Registering the event lets Wails generate a typed TypeScript API for it.
	application.RegisterEvent[[]manager.Item](downloadsEvent)
}

func NewDownloadService() *DownloadService { return &DownloadService{} }

// ServiceStartup is called by Wails when the app starts.
func (s *DownloadService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	s.ctx = ctx
	s.app = application.Get()

	dir, err := dataDir()
	if err != nil {
		return err
	}
	if path, err := engine.DefaultHostLimitsPath(); err == nil {
		s.limits, _ = engine.LoadHostLimits(path) // no memory is fine, just less smart
	}
	s.manager, err = manager.New(dir, s.limits, func() { s.dirty.Store(true) })
	if err != nil {
		return err
	}
	s.setUpVideo(dir)
	applyTheme(s.manager.Settings().Theme)
	s.manager.Start(ctx)
	s.startExtras()
	go s.sendUpdates()

	// Listen for links from the browser extension (through dlm-host).
	go func() {
		if err := bridge.Serve(ctx, dir, s); err != nil {
			log.Printf("browser bridge: %v", err) // the app still works without it
		}
	}()
	return nil
}

// AddFromBrowser is called (through the bridge) for each link the browser
// extension sends: a download the browser caught, or "Download with
// dlmanager" from the right-click menu. (Being exported, Wails also makes
// it callable from the window - harmless: the window can add links anyway.)
func (s *DownloadService) AddFromBrowser(req bridge.Request) (string, error) {
	// A link to a video page (right-clicked on YouTube, say): downloading
	// the page itself would be useless, so open the quality picker instead.
	if video.IsVideoPage(req.URL) {
		return "", s.OpenVideo(req)
	}
	it, err := s.manager.AddWith(req.URL, manager.AddOptions{
		Filename: req.Filename,
		Headers:  req.Headers(),
	})
	if errors.Is(err, manager.ErrDuplicate) {
		bringToFront()
		return "", errors.New("it's already in your list")
	}
	if err != nil {
		return "", err
	}
	bringToFront() // like IDM: show that the download has arrived
	return it.Filename, nil
}

// ShowWindow brings the window to the front (the extension's "Open" button).
func (s *DownloadService) ShowWindow() { bringToFront() }

// ServiceShutdown is called when the app quits: stop downloads cleanly so
// each saves its progress and continues next time.
func (s *DownloadService) ServiceShutdown() error {
	if s.manager != nil {
		s.manager.Shutdown()
	}
	return nil
}

// dataDir is where the download list and settings are kept:
// %AppData%\dlmanager on Windows.
func dataDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dlmanager"), nil
}

// ---- Downloads ----

// Add queues a link; startNow skips the queue.
func (s *DownloadService) Add(url string, startNow bool) (manager.Item, error) {
	return s.manager.Add(url, startNow)
}

func (s *DownloadService) List() []manager.Item  { return s.manager.List() }
func (s *DownloadService) Pause(id int) error    { return s.manager.Pause(id) }
func (s *DownloadService) Resume(id int) error   { return s.manager.Resume(id) }
func (s *DownloadService) StartNow(id int) error { return s.manager.StartNow(id) }
func (s *DownloadService) PauseAll()             { s.manager.PauseAll() }
func (s *DownloadService) ResumeAll()            { s.manager.ResumeAll() }
func (s *DownloadService) ClearCompleted()       { s.manager.ClearCompleted() }
func (s *DownloadService) Remove(id int, deletePartial bool) error {
	return s.manager.Remove(id, deletePartial)
}

// OpenFile opens a finished download with its default program.
func (s *DownloadService) OpenFile(id int) error {
	it, ok := s.manager.Get(id)
	if !ok || it.SavedPath == "" {
		return errors.New("this download hasn't finished yet")
	}
	if _, err := os.Stat(it.SavedPath); err != nil {
		return errors.New("the file isn't there any more (moved or deleted?)")
	}
	return s.app.Browser.OpenFile(it.SavedPath)
}

// ShowInFolder opens the download's folder, with the file selected.
func (s *DownloadService) ShowInFolder(id int) error {
	it, ok := s.manager.Get(id)
	if !ok {
		return errors.New("download not found")
	}
	target := it.SavedPath
	if target == "" || !exists(target) {
		target = it.Folder
	}
	if runtime.GOOS == "windows" {
		if target == it.SavedPath {
			return exec.Command("explorer", "/select,", target).Start()
		}
		return exec.Command("explorer", target).Start()
	}
	return s.app.Browser.OpenFile(filepath.Dir(target))
}

// ---- Settings ----

func (s *DownloadService) GetSettings() manager.Settings { return s.manager.Settings() }

func (s *DownloadService) SetSettings(settings manager.Settings) manager.Settings {
	settings = s.manager.SetSettings(settings)
	applyTheme(settings.Theme) // the icon follows the theme's colours
	return settings
}

// ChooseFolder shows a folder picker and returns the choice ("" if cancelled).
func (s *DownloadService) ChooseFolder() (string, error) {
	return s.app.Dialog.OpenFile().
		SetTitle("Choose where downloads are saved").
		CanChooseDirectories(true).
		CanChooseFiles(false).
		CanCreateDirectories(true).
		SetDirectory(s.manager.Settings().Folder).
		PromptForSingleSelection()
}

// ---- Server connection limits (see engine/hostlimits.go) ----

// ServerLimit is one row of the table shown in Settings.
type ServerLimit struct {
	Host           string    `json:"host"`
	MaxConnections int       `json:"maxConnections"`
	Manual         bool      `json:"manual"` // set by you (never expires) rather than learned
	Updated        time.Time `json:"updated"`
}

func (s *DownloadService) ServerLimits() []ServerLimit {
	if s.limits == nil {
		return nil
	}
	names, all := s.limits.All()
	rows := make([]ServerLimit, 0, len(names))
	for _, n := range names {
		l := all[n]
		rows = append(rows, ServerLimit{Host: n, MaxConnections: l.MaxConnections, Manual: l.Manual, Updated: l.Updated})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host < rows[j].Host })
	return rows
}

func (s *DownloadService) SetServerLimit(host string, maxConnections int) error {
	if s.limits == nil {
		return errors.New("server memory isn't available")
	}
	if host == "" || maxConnections < 1 {
		return errors.New("enter a server name and a number of connections")
	}
	return s.limits.Set(host, maxConnections)
}

func (s *DownloadService) ForgetServerLimit(host string) error {
	if s.limits == nil {
		return nil
	}
	return s.limits.Forget(host)
}

// ---- Updates to the window ----

// sendUpdates sends the list to the window up to 4 times a second, only when
// something changed. (People can't read numbers changing faster, and fewer
// updates means less work for the window.)
func (s *DownloadService) sendUpdates() {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if s.dirty.Swap(false) {
				s.app.Event.Emit(downloadsEvent, s.manager.List())
				s.sendQueueState()
			}
		}
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
