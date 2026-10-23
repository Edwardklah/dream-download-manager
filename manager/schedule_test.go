package manager

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCategory(t *testing.T) {
	for name, want := range map[string]string{
		"lecture.MP4": "Video", "song.m4a": "Music", "photos.tar.gz": "Compressed", "setup.exe": "Programs",
		"ubuntu.iso": "Programs", "notes.pdf": "Documents", "cat.webp": "Pictures", "data.bin": "", "README": "",
	} {
		if got := Category(name); got != want {
			t.Errorf("Category(%q) = %q, want %q", name, got, want)
		}
	}
}

// Finished files go into a folder by kind; others stay where they are; a
// name that's taken there gets a number.
func TestSortIntoFolders(t *testing.T) {
	fs, ts := newFileServer(t, 0)
	clip := fs.add("clip.mp4", 50_000)
	zip := fs.add("a.zip", 40_000)
	bin := fs.add("data.bin", 30_000)
	dir := t.TempDir()
	m := newManager(t, dir, Settings{MaxActive: 3, Connections: 2, SortIntoFolders: true})
	folder := m.Settings().Folder
	os.MkdirAll(filepath.Join(folder, "Video"), 0o755)
	os.WriteFile(filepath.Join(folder, "Video", "clip.mp4"), []byte("an older file"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	want := map[string][]byte{
		filepath.Join(folder, "Video", "clip (1).mp4"): clip,
		filepath.Join(folder, "Compressed", "a.zip"):   zip,
		filepath.Join(folder, "data.bin"):              bin,
	}
	var ids []int
	for _, name := range []string{"clip.mp4", "a.zip", "data.bin"} {
		it, err := m.Add(ts.URL+"/"+name, false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}
	for _, id := range ids {
		waitFor(t, "finished", func() bool { return status(m, id) == Completed })
		it, _ := m.Get(id)
		data, ok := want[it.SavedPath]
		if !ok {
			t.Errorf("saved at %s", it.SavedPath)
			continue
		}
		checkFile(t, it.SavedPath, data)
		if it.Filename != filepath.Base(it.SavedPath) {
			t.Errorf("filename %q, saved %q", it.Filename, it.SavedPath)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(folder, "Video", "clip.mp4")); string(data) != "an older file" {
		t.Error("an existing file was overwritten")
	}
}

// A video goes straight into Video (its streams and subtitles too).
func TestVideoIntoVideoFolder(t *testing.T) {
	m, _, _, _ := videoSetup(t, 0)
	s := m.Settings()
	s.SortIntoFolders = true
	m.SetSettings(s)
	it, err := m.AddVideo("https://www.youtube.com/watch?v=abc", VideoOptions{
		ChoiceID: "137+140", Title: "Sorted", Ext: "mp4", Subtitle: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(it.Folder) != "Video" {
		t.Errorf("folder %s", it.Folder)
	}
	waitFor(t, "finished", func() bool { return status(m, it.ID) == Completed })
	for _, name := range []string{"Sorted.mp4", "Sorted.en.srt"} {
		if _, err := os.Stat(filepath.Join(s.Folder, "Video", name)); err != nil {
			t.Error(err)
		}
	}
}

// fakeClock is a pretend clock the tests move by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(hhmm string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, _ := clock(hhmm)
	day := time.Date(c.now.Year(), c.now.Month(), c.now.Day(), 0, 0, 0, 0, c.now.Location())
	t := day.Add(time.Duration(m) * time.Minute)
	if !t.After(c.now) {
		t = t.AddDate(0, 0, 1) // the next day
	}
	c.now = t
	return t
}

// scheduled makes a started manager with a schedule and a pretend clock
// showing `at`.
func scheduled(t *testing.T, s Schedule, at string) (*Manager, *fakeClock) {
	t.Helper()
	m, err := New(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.Local)}
	c.set(at)
	m.now = c.Now
	m.SetSettings(Settings{MaxActive: 3, Connections: 2, Folder: filepath.Join(t.TempDir(), "files"), Schedule: s})
	t.Cleanup(m.Shutdown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.Start(ctx)
	return m, c
}

// Start 00:00, stop 05:00: downloads wait until midnight, stop at 5 (back
// to waiting, progress kept) - and "Start now" skips the wait.
func TestScheduleWindow(t *testing.T) {
	fs, ts := newFileServer(t, 20*time.Millisecond)
	fs.add("big", 4_000_000)
	fs.add("now", 100_000)
	m, c := scheduled(t, Schedule{Enabled: true, Start: "00:00", Stop: "05:00"}, "22:00")

	it, _ := m.Add(ts.URL+"/big", false)
	if st := m.QueueState(); !st.Held || st.StartsAt != "00:00" {
		t.Errorf("queue state %+v", st)
	}
	urgent, _ := m.Add(ts.URL+"/now", true) // Start now: no waiting
	waitFor(t, "start now to finish", func() bool { return status(m, urgent.ID) == Completed })
	m.tick(c.set("23:59"))
	time.Sleep(50 * time.Millisecond)
	if s := status(m, it.ID); s != Queued {
		t.Fatalf("before midnight: %s, want queued", s)
	}

	m.tick(c.set("00:00"))
	waitFor(t, "downloading after midnight", func() bool { return status(m, it.ID) == Downloading })
	if st := m.QueueState(); st.Held || st.StopsAt != "05:00" {
		t.Errorf("queue state %+v", st)
	}
	waitFor(t, "some progress", func() bool { x, _ := m.Get(it.ID); return x.Downloaded > 200_000 })

	m.tick(c.set("05:00"))
	waitFor(t, "back to waiting at 5", func() bool { return status(m, it.ID) == Queued && countRunning(m) == 0 })
	time.Sleep(50 * time.Millisecond)
	if s := status(m, it.ID); s != Queued {
		t.Errorf("after 5: %s, want queued (waiting for the next night)", s)
	}
	if x, _ := m.Get(it.ID); x.Downloaded == 0 {
		t.Error("progress was lost")
	}
}

// The computer was asleep: waking up inside the window starts the queue;
// waking up after it does nothing.
func TestScheduleAfterSleep(t *testing.T) {
	fs, ts := newFileServer(t, 0)
	fs.add("f", 10_000)
	m, c := scheduled(t, Schedule{Enabled: true, Start: "00:00", Stop: "05:00"}, "23:00")
	it, _ := m.Add(ts.URL+"/f", false)
	m.tick(c.set("07:00")) // slept through the whole window
	time.Sleep(50 * time.Millisecond)
	if s := status(m, it.ID); s != Queued {
		t.Fatalf("after the window: %s", s)
	}
	m.tick(c.set("02:00")) // the next night, woke up in the middle of it
	waitFor(t, "finished", func() bool { return status(m, it.ID) == Completed })
}

// Turning the schedule on inside the window doesn't wait for tomorrow.
func TestScheduleEnabledInsideWindow(t *testing.T) {
	fs, ts := newFileServer(t, 0)
	fs.add("f", 10_000)
	m, _ := scheduled(t, Schedule{Enabled: true, Start: "01:00", Stop: "06:00"}, "03:00")
	it, _ := m.Add(ts.URL+"/f", false)
	waitFor(t, "finished", func() bool { return status(m, it.ID) == Completed })
}

// No stop time: from the start time until the queue is empty; what's added
// after that waits for the next start.
func TestScheduleUntilEmpty(t *testing.T) {
	fs, ts := newFileServer(t, 0)
	fs.add("a", 10_000)
	fs.add("b", 10_000)
	m, c := scheduled(t, Schedule{Enabled: true, Start: "00:00"}, "20:00")
	a, _ := m.Add(ts.URL+"/a", false)
	m.tick(c.set("00:00"))
	waitFor(t, "a finished", func() bool { return status(m, a.ID) == Completed })
	waitFor(t, "held again", func() bool { return m.QueueState().Held })
	b, _ := m.Add(ts.URL+"/b", false)
	time.Sleep(50 * time.Millisecond)
	if s := status(m, b.ID); s != Queued {
		t.Fatalf("b: %s, want queued until the next night", s)
	}
	m.tick(c.set("00:00"))
	waitFor(t, "b finished", func() bool { return status(m, b.ID) == Completed })
}

// "All done" fires once the last download finishes - not when you pause.
func TestOnAllDone(t *testing.T) {
	fs, ts := newFileServer(t, 5*time.Millisecond)
	fs.add("a", 300_000)
	fs.add("b", 600_000)
	m := newManager(t, t.TempDir(), Settings{MaxActive: 3, Connections: 2})
	var fired atomic.Int32
	m.SetOnAllDone(func() { fired.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	p, _ := m.Add(ts.URL+"/b", false)
	waitFor(t, "running", func() bool { return status(m, p.ID) == Downloading })
	m.Pause(p.ID)
	waitFor(t, "paused", func() bool { return status(m, p.ID) == Paused })
	time.Sleep(30 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatal("pausing the last download counted as all done")
	}
	a, _ := m.Add(ts.URL+"/a", false)
	m.Resume(p.ID)
	waitFor(t, "both finished", func() bool { return status(m, a.ID) == Completed && status(m, p.ID) == Completed })
	waitFor(t, "fired", func() bool { return fired.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if n := fired.Load(); n != 1 {
		t.Errorf("fired %d times, want once", n)
	}
}

func TestScheduleClean(t *testing.T) {
	s := Schedule{Enabled: true, Start: "25:00", Stop: "x"}.clean()
	if s.Enabled || s.Stop != "" {
		t.Errorf("got %+v", s)
	}
	s = Schedule{Enabled: true, Start: "1:30", Stop: "01:30"}.clean()
	if !s.Enabled || s.Stop != "" {
		t.Errorf("stop = start should mean no stop: %+v", s)
	}
	// Settings saved by an older version get the new settings' defaults.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, settingsFile), []byte(`{"maxActive":2,"connections":4,"folder":"/x"}`), 0o600)
	m, _ := New(dir, nil, nil)
	if got := m.Settings(); got.MaxActive != 2 || !got.SortIntoFolders || !got.WatchClipboard || got.Schedule.Enabled {
		t.Errorf("got %+v", got)
	}
}

func TestLinkToOffer(t *testing.T) {
	for text, want := range map[string]bool{
		"https://example.com/files/setup.exe":                 true,
		"  https://cdn.example.com/a/b/Movie.Name.2024.MKV\n": true,
		"https://example.com/get/archive.tar.gz?token=abc":    true,
		"https://example.com/report.pdf":                      true,
		"https://www.youtube.com/watch?v=abc":                 false, // a page, not a file
		"https://example.com/":                                false,
		"https://github.com/x/y/blob/main/app.ts":             false,
		"https://example.com/photo.jpg":                       false,
		"see https://example.com/setup.exe":                   false, // not just a link
		"ftp://example.com/setup.exe":                         false,
		"C:\\Downloads\\setup.exe":                            false,
	} {
		if _, got := LinkToOffer(text); got != want {
			t.Errorf("LinkToOffer(%q) = %v, want %v", text, got, want)
		}
	}
}

// Each item says its kind, for the window's filter - from its name, or
// from the link before the name is known.
func TestItemCategory(t *testing.T) {
	m := newManager(t, t.TempDir(), Settings{MaxActive: 1, Connections: 1})
	a, _ := m.Add("https://example.com/files/setup.exe?x=1", false)
	b, _ := m.AddWith("https://example.com/get?id=7", AddOptions{Filename: "Album.flac"})
	c, _ := m.Add("https://example.com/page", false)
	if a.Category != "Programs" || b.Category != "Music" || c.Category != "" {
		t.Errorf("got %q %q %q", a.Category, b.Category, c.Category)
	}
}

func TestThemeSetting(t *testing.T) {
	if s := (Settings{Theme: "neon"}).clean(); s.Theme != "neon" {
		t.Errorf("got %q", s.Theme)
	}
	if s := (Settings{Theme: "rainbow"}).clean(); s.Theme != "black" {
		t.Errorf("unknown theme kept: %q", s.Theme)
	}
}
