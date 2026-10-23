package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fileServer serves files of random data at /<name>, with Range support and
// an adjustable speed, and counts how many requests are in progress.
type fileServer struct {
	mu       sync.Mutex
	files    map[string][]byte
	delay    time.Duration // pause per 32 KB chunk
	inflight atomic.Int32
	peak     atomic.Int32
	notFound atomic.Bool // answer 404 to everything
	cookie   string      // if set, requests without this Cookie get 403 (a logged-in download)
}

func newFileServer(t *testing.T, delay time.Duration) (*fileServer, *httptest.Server) {
	fs := &fileServer{files: map[string][]byte{}, delay: delay}
	ts := httptest.NewServer(fs)
	t.Cleanup(ts.Close)
	return fs, ts
}

func (fs *fileServer) add(name string, size int) []byte {
	data := make([]byte, size)
	rand.Read(data)
	fs.mu.Lock()
	fs.files[name] = data
	fs.mu.Unlock()
	return data
}

func (fs *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/expired") {
		http.Error(w, "this address has expired", http.StatusForbidden)
		return
	}
	if fs.notFound.Load() {
		http.NotFound(w, r)
		return
	}
	fs.mu.Lock()
	data, ok := fs.files[strings.TrimPrefix(r.URL.Path, "/")]
	fs.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if fs.cookie != "" && r.Header.Get("Cookie") != fs.cookie {
		http.Error(w, "log in first", http.StatusForbidden)
		return
	}
	probe := r.Header.Get("Range") == "bytes=0-0"
	var once sync.Once
	finished := func() {} // called just before the last bytes go out
	if !probe {
		n := fs.inflight.Add(1)
		finished = func() { once.Do(func() { fs.inflight.Add(-1) }) }
		defer finished()
		for p := fs.peak.Load(); n > p && !fs.peak.CompareAndSwap(p, n); p = fs.peak.Load() {
		}
	}
	start, end := 0, len(data)-1
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		a, b, _ := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
		start, _ = strconv.Atoi(a)
		if b != "" {
			end, _ = strconv.Atoi(b)
		}
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	}
	body := data[start : end+1]
	w.Header().Set("ETag", `"x"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	for i := 0; i < len(body); i += 32 * 1024 {
		last := i+32*1024 >= len(body)
		if last {
			// Stop counting this request BEFORE the client gets its final
			// bytes; otherwise the next download can start while this
			// handler is still returning, and look like an overlap.
			finished()
		}
		if _, err := w.Write(body[i:min(i+32*1024, len(body))]); err != nil {
			return
		}
		if fs.delay > 0 && !probe && !last {
			w.(http.Flusher).Flush()
			time.Sleep(fs.delay)
		}
	}
}

// newManager makes a manager whose downloads go into a temp folder.
func newManager(t *testing.T, dir string, s Settings) *Manager {
	t.Helper()
	m, err := New(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Folder == "" {
		s.Folder = filepath.Join(dir, "files")
	}
	m.SetSettings(s)
	// Stop running downloads before the test's temp folder is deleted, the
	// same way the app does when it closes.
	t.Cleanup(m.Shutdown)
	return m
}

// waitFor polls until cond is true or fails the test after 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func status(m *Manager, id int) Status {
	it, _ := m.Get(id)
	return it.Status
}

func countRunning(m *Manager) int {
	n := 0
	for _, it := range m.List() {
		switch it.Status {
		case Starting, Downloading, Retrying:
			n++
		}
	}
	return n
}

func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: content differs (%d vs %d bytes)", filepath.Base(path), len(got), len(want))
	}
}

func TestQueueRespectsMaxActive(t *testing.T) {
	fs, ts := newFileServer(t, 3*time.Millisecond)
	m := newManager(t, t.TempDir(), Settings{MaxActive: 2, Connections: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	var ids []int
	data := map[int][]byte{}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("f%d.bin", i)
		d := fs.add(name, 600_000)
		it, err := m.Add(ts.URL+"/"+name, false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
		data[it.ID] = d
	}

	maxSeen := 0
	waitFor(t, "all downloads to finish", func() bool {
		maxSeen = max(maxSeen, countRunning(m))
		for _, id := range ids {
			if status(m, id) != Completed {
				return false
			}
		}
		return true
	})
	if maxSeen > 2 || fs.peak.Load() > 2 {
		t.Errorf("more than 2 ran at once (manager saw %d, server saw %d)", maxSeen, fs.peak.Load())
	}
	for _, id := range ids {
		it, _ := m.Get(id)
		checkFile(t, it.SavedPath, data[id])
	}
}

func TestStartNowSkipsTheQueue(t *testing.T) {
	fs, ts := newFileServer(t, 5*time.Millisecond)
	m := newManager(t, t.TempDir(), Settings{MaxActive: 1, Connections: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	fs.add("a.bin", 2_000_000)
	fs.add("b.bin", 300_000)
	a, _ := m.Add(ts.URL+"/a.bin", false)
	b, _ := m.Add(ts.URL+"/b.bin", false)
	waitFor(t, "a to start", func() bool { return status(m, a.ID) == Downloading })
	if status(m, b.ID) != Queued {
		t.Fatalf("b should wait its turn, got %s", status(m, b.ID))
	}

	m.StartNow(b.ID)
	waitFor(t, "b to finish while a is still running", func() bool { return status(m, b.ID) == Completed })
	if status(m, a.ID) == Completed {
		t.Error("a finished first; the test didn't show b skipping the queue")
	}
}

func TestPauseAndResume(t *testing.T) {
	fs, ts := newFileServer(t, 5*time.Millisecond)
	m := newManager(t, t.TempDir(), Settings{MaxActive: 3, Connections: 4})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	data := fs.add("big.bin", 3_000_000)
	it, _ := m.Add(ts.URL+"/big.bin", false)
	waitFor(t, "some progress", func() bool { x, _ := m.Get(it.ID); return x.Downloaded > 300_000 })

	m.Pause(it.ID)
	waitFor(t, "paused", func() bool { return status(m, it.ID) == Paused })
	paused, _ := m.Get(it.ID)
	if _, err := os.Stat(filepath.Join(paused.Folder, paused.Filename+".part")); err != nil {
		t.Fatalf("pausing should keep the .part file: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if x, _ := m.Get(it.ID); x.Status != Paused {
		t.Fatalf("a paused download started again by itself: %s", x.Status)
	}

	m.Resume(it.ID)
	waitFor(t, "finished after resume", func() bool { return status(m, it.ID) == Completed })
	done, _ := m.Get(it.ID)
	checkFile(t, done.SavedPath, data)
	if filepath.Base(done.SavedPath) != "big.bin" {
		t.Errorf("resume should keep the name, got %s", filepath.Base(done.SavedPath))
	}
}

// Closing the app mid-download, then opening it again: the list comes back,
// the running download is queued and continues, the paused one stays paused.
func TestSurvivesRestart(t *testing.T) {
	fs, ts := newFileServer(t, 5*time.Millisecond)
	dir := t.TempDir()
	m := newManager(t, dir, Settings{MaxActive: 3, Connections: 2})
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)

	runData := fs.add("run.bin", 3_000_000)
	fs.add("pause.bin", 3_000_000)
	run, _ := m.Add(ts.URL+"/run.bin", false)
	pause, _ := m.Add(ts.URL+"/pause.bin", false)
	waitFor(t, "both downloading", func() bool {
		r, _ := m.Get(run.ID)
		p, _ := m.Get(pause.ID)
		return r.Downloaded > 200_000 && p.Downloaded > 200_000
	})
	m.Pause(pause.ID)
	waitFor(t, "paused", func() bool { return status(m, pause.ID) == Paused })

	m.Shutdown() // the app closes
	cancel()

	// The app opens again.
	m2 := newManager(t, dir, Settings{MaxActive: 3, Connections: 2})
	list := m2.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 saved downloads, got %d", len(list))
	}
	if s := status(m2, run.ID); s != Queued {
		t.Errorf("the running download should come back queued, got %s", s)
	}
	if s := status(m2, pause.ID); s != Paused {
		t.Errorf("the paused download should stay paused, got %s", s)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m2.Start(ctx2)
	waitFor(t, "the interrupted download to finish", func() bool { return status(m2, run.ID) == Completed })
	done, _ := m2.Get(run.ID)
	checkFile(t, done.SavedPath, runData)
	if status(m2, pause.ID) != Paused {
		t.Error("the paused download started on its own after restart")
	}

	// New IDs must not clash with saved ones.
	fs.add("new.bin", 1000)
	n, _ := m2.Add(ts.URL+"/new.bin", false)
	if n.ID <= pause.ID {
		t.Errorf("new download got ID %d, which clashes with saved IDs", n.ID)
	}
}

func TestRemoveDeletesPartialFiles(t *testing.T) {
	fs, ts := newFileServer(t, 5*time.Millisecond)
	m := newManager(t, t.TempDir(), Settings{MaxActive: 3, Connections: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	fs.add("x.bin", 3_000_000)
	it, _ := m.Add(ts.URL+"/x.bin", false)
	waitFor(t, "progress", func() bool { x, _ := m.Get(it.ID); return x.Downloaded > 200_000 })
	x, _ := m.Get(it.ID)
	part := filepath.Join(x.Folder, x.Filename+".part")

	m.Remove(it.ID, true) // remove while running, and delete what was downloaded
	if _, ok := m.Get(it.ID); ok {
		t.Fatal("still in the list")
	}
	waitFor(t, ".part deleted", func() bool {
		_, err1 := os.Stat(part)
		_, err2 := os.Stat(part + ".meta")
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})
}

func TestRemoveKeepsFinishedFile(t *testing.T) {
	fs, ts := newFileServer(t, 0)
	m := newManager(t, t.TempDir(), Settings{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	fs.add("keep.bin", 100_000)
	it, _ := m.Add(ts.URL+"/keep.bin", false)
	waitFor(t, "done", func() bool { return status(m, it.ID) == Completed })
	done, _ := m.Get(it.ID)
	m.Remove(it.ID, true)
	if _, err := os.Stat(done.SavedPath); err != nil {
		t.Error("removing a finished download from the list must never delete the file")
	}
}

func TestDuplicateIsRejected(t *testing.T) {
	fs, ts := newFileServer(t, 5*time.Millisecond)
	m := newManager(t, t.TempDir(), Settings{})
	fs.add("d.bin", 1_000_000)
	if _, err := m.Add(ts.URL+"/d.bin", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ts.URL+"/d.bin", false); err != ErrDuplicate {
		t.Errorf("expected ErrDuplicate, got %v", err)
	}
	if _, err := m.Add("not a link", false); err == nil {
		t.Error("expected an error for a non-link")
	}
}

func TestFailedThenRetry(t *testing.T) {
	fs, ts := newFileServer(t, 0)
	m := newManager(t, t.TempDir(), Settings{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	data := fs.add("r.bin", 200_000)
	fs.notFound.Store(true)
	it, _ := m.Add(ts.URL+"/r.bin", false)
	waitFor(t, "failed", func() bool { return status(m, it.ID) == Failed })
	if x, _ := m.Get(it.ID); !strings.Contains(x.Problem, "404") {
		t.Errorf("the reason should be shown, got %q", x.Problem)
	}

	fs.notFound.Store(false) // the server is fixed
	m.Resume(it.ID)          // "Retry"
	waitFor(t, "completed after retry", func() bool { return status(m, it.ID) == Completed })
	done, _ := m.Get(it.ID)
	checkFile(t, done.SavedPath, data)
}

func TestRaisingMaxActiveStartsMore(t *testing.T) {
	fs, ts := newFileServer(t, 5*time.Millisecond)
	m := newManager(t, t.TempDir(), Settings{MaxActive: 1, Connections: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	for i := 0; i < 3; i++ {
		fs.add(fmt.Sprintf("m%d.bin", i), 2_000_000)
		m.Add(fmt.Sprintf("%s/m%d.bin", ts.URL, i), false)
	}
	waitFor(t, "one running", func() bool { return countRunning(m) == 1 })
	m.SetSettings(Settings{MaxActive: 3, Connections: 1, Folder: m.Settings().Folder})
	waitFor(t, "three running", func() bool { return countRunning(m) == 3 })
}

func TestSettingsAreCleaned(t *testing.T) {
	s := Settings{MaxActive: 99, Connections: -5}.clean()
	if s.MaxActive != 10 || s.Connections != 1 || s.Folder == "" {
		t.Errorf("got %+v", s)
	}
	s = Settings{}.clean()
	if s.MaxActive != 3 || s.Connections != 8 {
		t.Errorf("defaults not applied: %+v", s)
	}
}

// A link from the browser comes with its cookies (here: the server refuses
// downloads without them) and a suggested name. Both must be used, kept
// across a restart for resuming - and the cookies never shown to the window.
func TestBrowserDetailsAreUsedAndKept(t *testing.T) {
	fs, ts := newFileServer(t, 0)
	fs.cookie = "session=abc"
	dir := t.TempDir()
	m := newManager(t, dir, Settings{MaxActive: 3, Connections: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	data := fs.add("dl", 600_000)
	headers := map[string]string{"Cookie": "session=abc", "Referer": ts.URL + "/page"}
	it, err := m.AddWith(ts.URL+"/dl", AddOptions{Filename: "report.pdf", Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "finished", func() bool { return status(m, it.ID) == Completed })
	done, _ := m.Get(it.ID)
	if done.Problem != "" || filepath.Base(done.SavedPath) != "report.pdf" {
		t.Fatalf("got %q, problem %q", done.SavedPath, done.Problem)
	}
	checkFile(t, done.SavedPath, data)

	// The interface's view has no cookies in it.
	if b, _ := json.Marshal(m.List()); strings.Contains(string(b), "session=abc") {
		t.Error("cookies were sent to the interface")
	}
	// The saved list keeps them, and a restarted manager gets them back.
	m.Shutdown()
	m2, err := New(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.entries) != 1 || m2.entries[0].headers["Cookie"] != "session=abc" {
		t.Errorf("headers weren't kept: %+v", m2.entries)
	}
	if st, err := os.Stat(filepath.Join(dir, downloadsFile)); err == nil && runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("downloads.json is readable by others: %v", st.Mode().Perm())
	}
}
