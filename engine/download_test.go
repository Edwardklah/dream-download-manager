package engine

// Tests for the engine. Run them from the project folder with:
//
//	go test ./engine -v
//
// GO NOTE: test files end in "_test.go" and are ignored by normal builds.
// Every function named TestXxx(t *testing.T) is a test. The httptest package
// starts a real HTTP server on your own machine, so we can make it misbehave
// on purpose (drop connections, freeze, change the file) and check that the
// engine copes.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer serves one file and can be told to misbehave.
type fakeServer struct {
	data          []byte
	etag          string
	ranges        bool          // support Range requests?
	dropAfter     int           // close the connection after this many bytes (0 = never)
	dropTimes     int32         // ...but only for this many requests (-1 = always)
	stallAfter    int           // stop sending (but keep the connection open) after N bytes
	stallTimes    int32         // ...for this many requests
	status        int           // if set, always reply with this status code
	requests      atomic.Int32  // how many GET requests were made
	mu            sync.Mutex    // guards rangeHeaders (handlers run in their own goroutines)
	rangeHeaders  []string      // Range header of each request
	refusedRanges []string      // ...and of each refused one
	sentBytes     atomic.Int64  // total body bytes sent (to measure wasted data)
	inflight      atomic.Int32  // requests being served right now
	maxInflight   atomic.Int32  // the most at any one time
	maxConns      int32         // if set, refuse requests beyond this many at once...
	limitStatus   int           // ...with this status (default 503)
	refused       atomic.Int32  // how many requests were refused that way
	chunkDelay    time.Duration // if set, pause between 32 KB chunks (a slow connection)
	slowRequests  int32         // the first N data requests use slowDelay instead (one bad connection)
	slowDelay     time.Duration
	// The first data request turns slow after slowdownAfter bytes (0 = never).
	slowdownAfter int
	slowdownDelay time.Duration
	slowdownUsed  atomic.Bool
	// How many requests are connected but haven't been sent any data yet.
	connecting     atomic.Int32
	peakConnecting atomic.Int32
	etagMu         sync.Mutex    // guards etag, which some tests change mid-download
	block          chan struct{} // closed when the test ends, to release stalled handlers
}

func newFakeServer(size int) *fakeServer {
	data := make([]byte, size)
	rand.Read(data)
	return &fakeServer{data: data, etag: `"v1"`, ranges: true, dropTimes: -1, block: make(chan struct{})}
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	now := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxInflight.Load()
		if now <= m || s.maxInflight.CompareAndSwap(m, now) {
			break
		}
	}
	if s.maxConns > 0 && now > s.maxConns {
		s.refused.Add(1)
		s.mu.Lock()
		s.refusedRanges = append(s.refusedRanges, r.Header.Get("Range"))
		s.mu.Unlock()
		status := s.limitStatus
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status) // "too many connections"
		return
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	isProbe := r.Header.Get("Range") == "bytes=0-0"
	if !isProbe && s.status == 0 {
		c := s.connecting.Add(1)
		for p := s.peakConnecting.Load(); c > p && !s.peakConnecting.CompareAndSwap(p, c); p = s.peakConnecting.Load() {
		}
		var once sync.Once
		sending := func() { once.Do(func() { s.connecting.Add(-1) }) }
		defer sending()
		w = &firstWriteHook{ResponseWriter: w, hook: sending}
	}
	if !isProbe {
		s.mu.Lock()
		s.rangeHeaders = append(s.rangeHeaders, r.Header.Get("Range"))
		s.mu.Unlock()
	}

	start, end := 0, len(s.data)-1
	partial := false
	if rng := r.Header.Get("Range"); rng != "" && s.ranges {
		ifRange := r.Header.Get("If-Range")
		if ifRange == "" || ifRange == s.getETag() {
			spec := strings.TrimPrefix(rng, "bytes=")
			parts := strings.SplitN(spec, "-", 2)
			start, _ = strconv.Atoi(parts[0])
			if parts[1] != "" {
				end, _ = strconv.Atoi(parts[1])
			}
			if start >= len(s.data) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			partial = true
		}
	}
	body := s.data[start : end+1]

	w.Header().Set("ETag", s.getETag())
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.data)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if isProbe {
		w.Write(body)
		return
	}

	if s.stallAfter > 0 && atomic.AddInt32(&s.stallTimes, -1) >= 0 && len(body) > s.stallAfter {
		w.Write(body[:s.stallAfter])
		s.sentBytes.Add(int64(s.stallAfter))
		w.(http.Flusher).Flush()
		select { // freeze until the client gives up or the test ends
		case <-r.Context().Done():
		case <-s.block:
		}
		return
	}
	if s.dropAfter > 0 && len(body) > s.dropAfter &&
		(atomic.LoadInt32(&s.dropTimes) < 0 || atomic.AddInt32(&s.dropTimes, -1) >= 0) {
		w.Write(body[:s.dropAfter])
		s.sentBytes.Add(int64(s.dropAfter))
		// Hijack takes over the raw connection so we can slam it shut mid-file.
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
		return
	}
	delay := s.chunkDelay
	if atomic.LoadInt32(&s.slowRequests) > 0 && atomic.AddInt32(&s.slowRequests, -1) >= 0 {
		delay = s.slowDelay
	}
	slowdownHere := s.slowdownAfter > 0 && s.slowdownUsed.CompareAndSwap(false, true)
	if delay > 0 || slowdownHere {
		for i := 0; i < len(body); i += 32 * 1024 {
			end := min(i+32*1024, len(body))
			if slowdownHere && i >= s.slowdownAfter {
				delay = s.slowdownDelay
			}
			if _, err := w.Write(body[i:end]); err != nil {
				return
			}
			s.sentBytes.Add(int64(end - i))
			w.(http.Flusher).Flush()
			time.Sleep(delay)
		}
		return
	}
	w.Write(body)
	s.sentBytes.Add(int64(len(body)))
}

func (s *fakeServer) getETag() string {
	s.etagMu.Lock()
	defer s.etagMu.Unlock()
	return s.etag
}

func (s *fakeServer) setETag(e string) {
	s.etagMu.Lock()
	s.etag = e
	s.etagMu.Unlock()
}

// setup starts the fake server and returns a Download pointing at it.
func setup(t *testing.T, s *fakeServer) (*Download, *httptest.Server) {
	t.Helper()
	retryBase = 10 * time.Millisecond   // fast retries for tests
	minCooldown = 50 * time.Millisecond // and short cool-downs
	rampInterval = 5 * time.Millisecond // and quick ramp-up
	hangupGrace = 50 * time.Millisecond
	ts := httptest.NewServer(s)
	// GO NOTE: t.Cleanup registers code to run when the test finishes.
	t.Cleanup(func() { close(s.block); ts.Close() })
	d := &Download{
		URL:          ts.URL + "/file.bin",
		Folder:       t.TempDir(), // a fresh folder, deleted after the test
		StallTimeout: 300 * time.Millisecond,
		Connections:  1, // single-connection tests; multi-connection ones override this
	}
	return d, ts
}

// checkFile fails the test unless path contains exactly want.
func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file content differs from the original (%d vs %d bytes)", len(got), len(want))
	}
	if _, err := os.Stat(path + ".part.meta"); err == nil {
		t.Errorf(".meta file was not cleaned up")
	}
}

func TestSimpleDownload(t *testing.T) {
	s := newFakeServer(1 << 20) // 1 MB
	d, _ := setup(t, s)
	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
}

func TestRetriesAndResumesAfterDrops(t *testing.T) {
	s := newFakeServer(1 << 20)
	s.dropAfter = 200_000 // every connection dies after ~200 KB
	d, _ := setup(t, s)

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	// Each retry must continue where the last one stopped, not restart.
	// (The first request is "bytes=0-": it asks about the file AND starts it.)
	if s.rangeHeaders[0] != "bytes=0-" || !strings.HasPrefix(s.rangeHeaders[1], "bytes=200000-") {
		t.Errorf("expected resume ranges, got %v", s.rangeHeaders)
	}
	if sent := s.sentBytes.Load(); sent != int64(len(s.data)) {
		t.Errorf("server sent %d bytes for a %d byte file: data was re-downloaded", sent, len(s.data))
	}
}

func TestStallIsDetected(t *testing.T) {
	s := newFakeServer(1 << 20)
	s.stallAfter = 100_000
	s.stallTimes = 2 // freeze twice, then behave
	d, _ := setup(t, s)

	var sawStall atomic.Bool
	d.OnProgress = func(p Progress) {
		if p.Status == StatusRetrying && errors.Is(p.Err, ErrStalled) {
			sawStall.Store(true)
		}
	}
	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if !sawStall.Load() {
		t.Error("stall was never reported")
	}
}

func TestResumeAfterCancel(t *testing.T) {
	s := newFakeServer(1 << 20)
	s.stallAfter = 300_000 // first request freezes, so we can cancel mid-file
	s.stallTimes = 1
	d, ts := setup(t, s)
	d.StallTimeout = 10 * time.Second // don't let the stall detector interfere

	// The server sends 300 KB and freezes. Shortly after the download starts,
	// the "user" presses Cancel.
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	d.OnProgress = func(p Progress) {
		if p.Status == StatusDownloading {
			once.Do(func() { time.AfterFunc(300*time.Millisecond, cancel) })
		}
	}
	if _, err := d.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}

	// Start again, like running the same command a second time.
	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: d.Connections}
	var resumedAt int64 = -1
	d2.OnProgress = func(p Progress) {
		if p.Resumed {
			resumedAt = p.ResumedAt
		}
	}
	path, err := d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if resumedAt != 300_000 {
		t.Errorf("expected to resume at 300000, resumed at %d", resumedAt)
	}
	if filepath.Base(path) != "file.bin" {
		t.Errorf("resumed download should keep its name, got %s", filepath.Base(path))
	}
}

// The situation from the first real-world test: an old .part with no .meta
// (left by stage 1) blocks the plain name, so the download is saved as
// "file (1).bin". Running the command again must resume THAT one.
func TestResumeNumberedName(t *testing.T) {
	s := newFakeServer(1 << 20)
	s.stallAfter = 300_000
	s.stallTimes = 1
	d, ts := setup(t, s)
	d.StallTimeout = 10 * time.Second

	// An old leftover with no .meta: we can't know what it is, so it's left alone.
	old := filepath.Join(d.Folder, "file.bin.part")
	os.WriteFile(old, []byte("old junk"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	d.OnProgress = func(p Progress) {
		if p.Status == StatusDownloading {
			once.Do(func() { time.AfterFunc(300*time.Millisecond, cancel) })
		}
	}
	if _, err := d.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}

	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: d.Connections}
	var resumedAt int64 = -1
	d2.OnProgress = func(p Progress) {
		if p.Resumed {
			resumedAt = p.ResumedAt
		}
	}
	path, err := d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if filepath.Base(path) != "file (1).bin" || resumedAt != 300_000 {
		t.Errorf("expected to resume file (1).bin at 300000, got %s at %d", filepath.Base(path), resumedAt)
	}
	if got, _ := os.ReadFile(old); string(got) != "old junk" {
		t.Error("the unrelated old .part was modified")
	}
}

func TestFileChangedOnServerRestarts(t *testing.T) {
	s := newFakeServer(1 << 20)
	s.stallAfter = 300_000
	s.stallTimes = 1
	d, ts := setup(t, s)
	d.StallTimeout = 10 * time.Second

	// The server sends 300 KB and freezes. Shortly after the download starts,
	// the "user" presses Cancel.
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	d.OnProgress = func(p Progress) {
		if p.Status == StatusDownloading {
			once.Do(func() { time.AfterFunc(300*time.Millisecond, cancel) })
		}
	}
	if _, err := d.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}

	// The server now has a NEW version of the file, same size.
	rand.Read(s.data)
	s.setETag(`"v2"`)

	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: d.Connections}
	path, err := d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Must be entirely the new version - no old bytes mixed in.
	checkFile(t, path, s.data)
}

func TestNoRangeSupportRestartsFromZero(t *testing.T) {
	s := newFakeServer(1 << 20)
	s.ranges = false
	s.dropAfter = 200_000
	s.dropTimes = 2 // fail twice, then succeed
	d, _ := setup(t, s)

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
}

func TestNoRangeGivesUpEventually(t *testing.T) {
	s := newFakeServer(1 << 20)
	s.ranges = false
	s.dropAfter = 200_000 // always fails
	d, _ := setup(t, s)
	d.MaxRetries = 3

	_, err := d.Run(context.Background())
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected to give up with ErrIncomplete, got %v", err)
	}
	// 3 retries + the first attempt = 4 downloads, not endless re-downloading.
	if got := len(s.rangeHeaders); got != 4 {
		t.Errorf("expected 4 attempts, got %d", got)
	}
}

func TestNotFoundIsNotRetried(t *testing.T) {
	s := newFakeServer(10)
	s.status = http.StatusNotFound
	d, _ := setup(t, s)

	_, err := d.Run(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != 404 {
		t.Fatalf("expected a 404 error, got %v", err)
	}
	if n := s.requests.Load(); n != 1 {
		t.Errorf("404 should not be retried, but %d requests were made", n)
	}
}

func TestServerErrorIsRetried(t *testing.T) {
	s := newFakeServer(10)
	s.status = http.StatusServiceUnavailable // 503, always
	d, _ := setup(t, s)
	d.MaxRetries = 3

	if _, err := d.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if n := s.requests.Load(); n != 3 {
		t.Errorf("expected 3 attempts for a 503, got %d", n)
	}
}

func TestSanitizeFilename(t *testing.T) {
	// GO NOTE: a "table-driven test" - a list of inputs and expected outputs,
	// checked in a loop. It's the most common style of Go test.
	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{`..\..\Windows\evil.exe`, "evil.exe"},
		{"../../etc/passwd", "passwd"},
		{"a:b*c?.txt", "a_b_c_.txt"},
		{"..", "download"},
		{"", "download"},
	}
	for _, c := range cases {
		if got := SanitizeFilename(c.in); got != c.want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// firstWriteHook calls hook just before the first byte of a reply is sent.
type firstWriteHook struct {
	http.ResponseWriter
	hook func()
}

func (f *firstWriteHook) WriteHeader(code int)        { f.hook(); f.ResponseWriter.WriteHeader(code) }
func (f *firstWriteHook) Write(b []byte) (int, error) { f.hook(); return f.ResponseWriter.Write(b) }
func (f *firstWriteHook) Flush()                      { f.ResponseWriter.(http.Flusher).Flush() }
func (f *firstWriteHook) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return f.ResponseWriter.(http.Hijacker).Hijack()
}
