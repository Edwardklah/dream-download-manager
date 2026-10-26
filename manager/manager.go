// Package manager runs a list of downloads: the queue, pausing and resuming,
// and saving the list so it survives closing the app.
//
// It sits between the engine (which knows how to download ONE file) and the
// interface (which shows the list and buttons). It has no user interface of
// its own and no Wails code, so it can be tested on its own - see
// manager_test.go.
//
// How the queue works:
//   - At most Settings.MaxActive downloads run at once (3 by default). The
//     rest wait, in the order they were added, and start as others finish.
//   - "Start now" lets one download skip the queue and run immediately,
//     whatever the limit.
//   - Pausing stops a download and keeps its .part file; resuming puts it
//     back in the queue, and the engine continues from where it stopped.
//   - When the app closes, running downloads are stopped cleanly and marked
//     as queued, so they carry on the next time the app starts.
package manager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"dlmanager/engine"
)

// Status is where a download is in its life.
type Status string

const (
	Queued      Status = "queued"      // waiting for a free slot
	Starting    Status = "starting"    // checking the file with the server
	Downloading Status = "downloading" // receiving data
	Retrying    Status = "retrying"    // waiting to reconnect after a problem
	Paused      Status = "paused"      // stopped by you; resume to continue
	Completed   Status = "completed"
	Failed      Status = "failed" // stopped by an error; retry to try again
)

// Item is one download as the interface sees it.
type Item struct {
	ID         int       `json:"id"`
	URL        string    `json:"url"`
	Folder     string    `json:"folder"`
	Filename   string    `json:"filename"`  // known once the server has been asked
	SavedPath  string    `json:"savedPath"` // the finished file
	Status     Status    `json:"status"`
	StartNow   bool      `json:"startNow"` // skips the queue
	Downloaded int64     `json:"downloaded"`
	Total      int64     `json:"total"` // -1 if unknown
	Problem    string    `json:"problem"`
	AddedAt    time.Time `json:"addedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	// Video downloads (Kind "video"): URL is the video's page, FormatID the
	// chosen quality ("137+140"), and the file is made from one or two
	// streams downloaded one after the other.
	Kind     string `json:"kind,omitempty"` // "" = an ordinary file, "video"
	Title    string `json:"title,omitempty"`
	Quality  string `json:"quality,omitempty"` // "1080p", "Audio only"
	FormatID string `json:"formatId,omitempty"`
	Stage    string `json:"stage,omitempty"`    // what a video is doing: "Video", "Audio", "Joining"...
	Subtitle string `json:"subtitle,omitempty"` // subtitle language saved with it ("en", "auto:en-orig")

	// Note is a remark that isn't an error ("the subtitles couldn't be
	// saved"): the download itself went fine.
	Note string `json:"note,omitempty"`

	// Category is the file's kind ("Video", "Compressed"...; "" for
	// others), for the window's filter. Worked out, not saved.
	Category string `json:"category"`

	// KeepFolder: you chose the folder, so the file isn't sorted into a
	// Video, Music... folder inside it.
	KeepFolder bool `json:"keepFolder,omitempty"`

	// Live details, only meaningful while running (not saved).
	Speed       float64               `json:"speed"`
	ETASeconds  float64               `json:"etaSeconds"`
	Connections int                   `json:"connections"`
	Active      int                   `json:"active"`
	Retrying    int                   `json:"retrying"`
	Splits      int                   `json:"splits"`
	HostLimit   int                   `json:"hostLimit"`
	Segments    []engine.SegmentState `json:"segments"`
}

// Settings the user can change.
type Settings struct {
	MaxActive   int    `json:"maxActive"`   // downloads at once (1-10)
	Connections int    `json:"connections"` // connections per download (1-32)
	Folder      string `json:"folder"`      // where new downloads go

	SortIntoFolders  bool     `json:"sortIntoFolders"`  // Video, Music, Compressed... (categories.go)
	Schedule         Schedule `json:"schedule"`         // hold the queue until a set time (schedule.go)
	CheckToolUpdates bool     `json:"checkToolUpdates"` // look for newer yt-dlp now and then (the app does it)
	WatchClipboard   bool     `json:"watchClipboard"`   // offer copied download links (the app does it)
	Theme            string   `json:"theme"`            // the window's colours: "black", "dream", "light"...
	Welcomed         bool     `json:"welcomed"`         // the first-run welcome has been shown
	AskBrowser       bool     `json:"askBrowser"`       // downloads caught in the browser wait for Download / Later (the app does it)
}

// Themes the window has (gui/frontend/src/style.css).
var Themes = []string{"black", "dream", "light", "ember", "forest", "neon", "ocean", "sand", "terminal"}

// DefaultSettings: 3 at a time, 8 connections, the Downloads folder,
// sorted into folders by kind.
func DefaultSettings() Settings {
	home, _ := os.UserHomeDir()
	return Settings{
		MaxActive: 3, Connections: 8, Folder: filepath.Join(home, "Downloads"),
		SortIntoFolders: true, CheckToolUpdates: true, WatchClipboard: true, Theme: "black", AskBrowser: true,
		Schedule: Schedule{Start: "00:00", Stop: "06:00"},
	}
}

// clean fills in missing settings with defaults and keeps numbers in range.
func (s Settings) clean() Settings {
	d := DefaultSettings()
	if s.MaxActive == 0 {
		s.MaxActive = d.MaxActive
	}
	s.MaxActive = min(max(s.MaxActive, 1), 10)
	if s.Connections == 0 {
		s.Connections = d.Connections
	}
	s.Connections = min(max(s.Connections, 1), 32)
	if strings.TrimSpace(s.Folder) == "" {
		s.Folder = d.Folder
	}
	s.Schedule = s.Schedule.clean()
	if !slices.Contains(Themes, s.Theme) {
		s.Theme = d.Theme
	}
	return s
}

// entry is an Item plus what the manager needs to control it.
type entry struct {
	Item
	// headers: sent with every request for this download - from the browser
	// extension: cookies (for links that need you logged in), the page the
	// link was on, the browser's identity. Saved, but never sent to the
	// interface: cookies are private.
	headers       map[string]string
	cancel        context.CancelFunc // stops it while running
	running       bool
	stopAs        Status // what to become when the running download stops (Paused or Queued)
	removed       bool
	deletePartial bool
}

// Manager runs the downloads. Create it with New, then call Start.
type Manager struct {
	dir      string // where downloads.json and settings.json are kept
	limits   *engine.HostLimits
	onChange func()

	mu       sync.Mutex
	ctx      context.Context // nil until Start
	settings Settings
	entries  []*entry // in queue order
	nextID   int
	running  sync.WaitGroup
	closing  bool
	video    VideoBackend // nil: no video downloads

	// The scheduler (schedule.go).
	now       func() time.Time // the clock; the tests use a pretend one
	lastTick  time.Time
	released  bool // the schedule's time has come: the queue may run
	onAllDone func()
}

// New loads the saved download list and settings from dir (creating it if
// needed). onChange is called, without any lock held, whenever anything
// changes - the interface uses it to know when to redraw. It may be nil.
func New(dir string, limits *engine.HostLimits, onChange func()) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, limits: limits, onChange: onChange, settings: DefaultSettings(), now: time.Now}
	m.load()
	return m, nil
}

// Start lets downloads run. Everything stops when ctx is cancelled (or on
// Shutdown).
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	s := m.settings.Schedule
	m.released = s.Enabled && s.Stop != "" && s.inWindow(m.now())
	m.lastTick = m.now()
	m.scheduleLocked()
	m.mu.Unlock()
	m.notify()

	go func() { // the scheduler's clock
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.tick(m.now())
			}
		}
	}()
}

// Shutdown stops all running downloads cleanly (each saves its progress),
// marks them as queued so they continue next time, and saves the list.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closing = true
	for _, e := range m.entries {
		if e.running {
			e.stopAs = Queued
			e.cancel()
		}
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() { m.running.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // don't hang the app's exit
	}
	m.mu.Lock()
	m.saveLocked()
	m.mu.Unlock()
}

// ErrDuplicate means the same link is already in the list, unfinished, for
// the same folder.
var ErrDuplicate = errors.New("that link is already in the list")

// Add puts a link in the queue (or starts it at once with startNow).
func (m *Manager) Add(rawURL string, startNow bool) (Item, error) {
	return m.AddWith(rawURL, AddOptions{StartNow: startNow})
}

// AddOptions are the extras a link can come with.
type AddOptions struct {
	StartNow bool
	Paused   bool              // added paused ("Later"): starts when you press play
	Filename string            // suggested name (the browser's); empty = the server's
	Folder   string            // a folder you chose; empty = the download folder (sorted by kind)
	Headers  map[string]string // extra request headers (cookies, referrer...)
}

// AddWith queues a link with extra options.
func (m *Manager) AddWith(rawURL string, o AddOptions) (Item, error) {
	startNow := o.StartNow
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Item{}, errors.New("that doesn't look like a download link (it should start with http:// or https://)")
	}

	m.mu.Lock()
	folder := m.settings.Folder
	if o.Folder != "" {
		folder = o.Folder
	}
	for _, e := range m.entries {
		if e.URL == rawURL && e.Folder == folder && e.Status != Completed {
			m.mu.Unlock()
			return Item{}, ErrDuplicate
		}
	}
	m.nextID++
	e := &entry{Item: Item{
		ID: m.nextID, URL: rawURL, Folder: folder, Status: Queued,
		StartNow: startNow, Total: -1, ETASeconds: -1, AddedAt: time.Now(),
		KeepFolder: o.Folder != "",
	}, headers: o.Headers}
	if o.Paused {
		e.Status, e.StartNow = Paused, false
	}
	if o.Filename != "" {
		e.Filename = engine.SanitizeFilename(o.Filename)
	}
	m.entries = append(m.entries, e)
	m.scheduleLocked()
	m.saveLocked()
	item := e.snapshot()
	m.mu.Unlock()
	m.notify()
	return item, nil
}

// Pause stops a download (running or queued) and keeps its progress.
func (m *Manager) Pause(id int) error {
	return m.change(id, func(e *entry) error {
		switch {
		case e.running:
			e.stopAs = Paused
			e.cancel()
		case e.Status == Queued || e.Status == Failed:
			e.Status = Paused
			e.StartNow = false
		}
		return nil
	})
}

// Resume puts a paused or failed download back in the queue.
func (m *Manager) Resume(id int) error {
	return m.change(id, func(e *entry) error {
		if !e.running && (e.Status == Paused || e.Status == Failed) {
			e.Status = Queued
			e.Problem = ""
		}
		return nil
	})
}

// StartNow makes a download skip the queue and run immediately.
func (m *Manager) StartNow(id int) error {
	return m.change(id, func(e *entry) error {
		if e.Status == Completed || e.running {
			return nil
		}
		e.Status = Queued
		e.Problem = ""
		e.StartNow = true
		return nil
	})
}

// Remove takes a download off the list, stopping it first if it's running.
// With deletePartial, its unfinished .part files are deleted too. A finished
// file is never deleted.
func (m *Manager) Remove(id int, deletePartial bool) error {
	m.mu.Lock()
	i, e := m.findLocked(id)
	if e == nil {
		m.mu.Unlock()
		return errNotFound(id)
	}
	m.entries = append(m.entries[:i], m.entries[i+1:]...)
	e.removed = true
	e.deletePartial = deletePartial && e.Status != Completed
	discardNow := e.deletePartial && !e.running
	if e.running {
		e.cancel() // run() deletes the partial files once the download has stopped
	}
	m.scheduleLocked()
	m.saveLocked()
	it := e.Item
	m.mu.Unlock()

	if discardNow {
		discardPartial(it)
	}
	m.notify()
	return nil
}

// PauseAll pauses every download that isn't finished.
func (m *Manager) PauseAll() {
	for _, it := range m.List() {
		if it.Status != Completed && it.Status != Paused {
			m.Pause(it.ID)
		}
	}
}

// ResumeAll puts every paused or failed download back in the queue.
func (m *Manager) ResumeAll() {
	for _, it := range m.List() {
		if it.Status == Paused || it.Status == Failed {
			m.Resume(it.ID)
		}
	}
}

// ClearCompleted removes finished downloads from the list (not the files).
func (m *Manager) ClearCompleted() {
	m.mu.Lock()
	kept := m.entries[:0]
	for _, e := range m.entries {
		if e.Status != Completed {
			kept = append(kept, e)
		}
	}
	m.entries = kept
	m.saveLocked()
	m.mu.Unlock()
	m.notify()
}

// List returns every download, in queue order.
func (m *Manager) List() []Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]Item, len(m.entries))
	for i, e := range m.entries {
		list[i] = e.snapshot()
	}
	return list
}

// Get returns one download.
func (m *Manager) Get(id int) (Item, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, e := m.findLocked(id); e != nil {
		return e.snapshot(), true
	}
	return Item{}, false
}

// Settings returns the current settings.
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// SetSettings changes the settings. Raising MaxActive starts waiting
// downloads straight away; lowering it lets running ones finish.
func (m *Manager) SetSettings(s Settings) Settings {
	m.mu.Lock()
	old := m.settings.Schedule
	m.settings = s.clean()
	m.scheduleChangedLocked(old)
	m.scheduleLocked()
	m.saveLocked()
	s = m.settings
	m.mu.Unlock()
	m.notify()
	return s
}

// ---- internals ----

func errNotFound(id int) error { return fmt.Errorf("no download with id %d", id) }

// change runs fn on one entry under the lock, then reschedules and saves.
func (m *Manager) change(id int, fn func(*entry) error) error {
	m.mu.Lock()
	_, e := m.findLocked(id)
	if e == nil {
		m.mu.Unlock()
		return errNotFound(id)
	}
	err := fn(e)
	m.scheduleLocked()
	m.saveLocked()
	m.mu.Unlock()
	m.notify()
	return err
}

func (m *Manager) findLocked(id int) (int, *entry) {
	for i, e := range m.entries {
		if e.ID == id {
			return i, e
		}
	}
	return -1, nil
}

// scheduleLocked starts queued downloads while there are free slots.
// "Start now" downloads don't count toward the limit and always start.
func (m *Manager) scheduleLocked() {
	if m.ctx == nil || m.closing {
		return
	}
	active := 0
	for _, e := range m.entries {
		if e.running && !e.StartNow {
			active++
		}
	}
	held := m.heldLocked() // waiting for the schedule's start time
	for _, e := range m.entries {
		if e.Status != Queued || e.running {
			continue
		}
		if e.StartNow {
			m.startLocked(e)
		} else if active < m.settings.MaxActive && !held {
			m.startLocked(e)
			active++
		}
	}
}

func (m *Manager) startLocked(e *entry) {
	ctx, cancel := context.WithCancel(m.ctx)
	e.cancel = cancel
	e.running = true
	e.stopAs = Paused
	e.Status = Starting
	e.Problem = ""
	e.Note = ""
	var job func(context.Context) (string, error)
	if e.Kind == KindVideo {
		job = m.videoJob(e)
	} else if e.Kind == KindSubtitles {
		job = m.subtitleJob(e)
	} else {
		d := &engine.Download{
			URL:         e.URL,
			Folder:      e.Folder,
			Filename:    e.Filename, // keep the same name (and .part) when resuming
			Headers:     e.headers,
			Connections: m.settings.Connections,
			Limits:      m.limits,
			OnProgress:  func(p engine.Progress) { m.progress(e, p) },
		}
		job = d.Run
	}
	m.running.Add(1)
	go m.run(e, job, ctx)
}

// run downloads one entry and records how it ended.
func (m *Manager) run(e *entry, job func(context.Context) (string, error), ctx context.Context) {
	defer m.running.Done()
	path, err := job(ctx)

	if err == nil && e.Kind == "" { // (videos and subtitles are put in their folder when added)
		m.mu.Lock()
		sort := m.settings.SortIntoFolders
		m.mu.Unlock()
		if sort && !e.KeepFolder { // a folder you chose is kept as it is
			path = sortIntoCategory(path)
		}
	}

	m.mu.Lock()
	e.running = false
	e.cancel = nil
	e.Speed, e.ETASeconds, e.Active, e.Retrying = 0, -1, 0, 0
	e.Stage = ""
	switch {
	case err == nil:
		e.Status = Completed
		e.SavedPath = path
		e.Filename = filepath.Base(path)
		e.FinishedAt = time.Now()
		e.Downloaded = e.Total
		e.StartNow = false
		e.Segments = nil
		e.Problem = ""
	case errors.Is(err, context.Canceled):
		e.Status = e.stopAs // Paused (by you) or Queued (app closing)
		if e.stopAs == Paused {
			e.StartNow = false
		}
	default:
		e.Status = Failed
		e.Problem = err.Error()
		e.StartNow = false
	}
	discard := e.removed && e.deletePartial
	discardItem := e.Item
	m.scheduleLocked()
	var allDone func()
	// "All done" when the last download finishes (or fails) - not when you
	// pause it, nor when the app is closing.
	ended := e.Status == Completed || e.Status == Failed
	if ended && m.queueDoneLocked() && !m.closing {
		allDone = m.onAllDone
		// Without a stop time, the schedule's run ends when the queue does:
		// what's added later waits for the next start time.
		if s := m.settings.Schedule; s.Enabled && s.Stop == "" {
			m.released = false
		}
	}
	m.saveLocked()
	m.mu.Unlock()

	if discard {
		discardPartial(discardItem)
	}
	m.notify()
	if allDone != nil {
		allDone()
	}
}

// discardPartial deletes what an unfinished download left behind: its .part
// file, or for a video, its stream files.
func discardPartial(it Item) {
	if it.Kind == KindVideo {
		for _, name := range streamNames(it) {
			engine.DiscardPartial(it.Folder, name)
			os.Remove(filepath.Join(it.Folder, name)) // a stream that finished
		}
		return
	}
	engine.DiscardPartial(it.Folder, it.Filename)
}

// progress copies the engine's report into the entry.
func (m *Manager) progress(e *entry, p engine.Progress) {
	m.mu.Lock()
	switch p.Status {
	case engine.StatusProbing:
		e.Status = Starting
	case engine.StatusDownloading:
		e.Status = Downloading
	case engine.StatusRetrying:
		e.Status = Retrying
	default:
		// completed / cancelled / failed are recorded by run(), which knows why.
	}
	if p.Filename != "" && p.Filename != "download" {
		e.Filename = p.Filename
	}
	e.Downloaded = p.Downloaded
	e.Total = p.Total
	e.Speed = p.Speed
	e.ETASeconds = -1
	if p.ETA >= 0 {
		e.ETASeconds = p.ETA.Seconds()
	}
	e.Connections = p.Connections
	e.Active = p.Active
	e.Retrying = p.Retrying
	e.Splits = p.Splits
	e.HostLimit = p.HostLimit
	if p.Segments != nil {
		e.Segments = p.Segments
	}
	if p.LastProblem != "" {
		e.Problem = p.LastProblem
	}
	if p.Status == engine.StatusRetrying && p.Err != nil {
		e.Problem = p.Err.Error()
	}
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

func (e *entry) snapshot() Item {
	it := e.Item
	name := it.Filename
	if name == "" { // not known until the server answers: guess from the link
		if u, err := url.Parse(it.URL); err == nil {
			name = path.Base(u.Path)
		}
	}
	it.Category = Category(name)
	if it.Segments != nil {
		it.Segments = append([]engine.SegmentState(nil), it.Segments...)
	}
	return it
}
