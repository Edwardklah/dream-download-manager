package engine

// Tests for multi-connection downloading (stage 3). They reuse the fake
// server from download_test.go.

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// cancelSoon returns a context the "user" cancels shortly after the
// download starts receiving data.
func cancelSoon(d *Download) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	d.OnProgress = func(p Progress) {
		if p.Status == StatusDownloading {
			once.Do(func() { time.AfterFunc(300*time.Millisecond, cancel) })
		}
	}
	return ctx
}

func TestMultiConnectionDownload(t *testing.T) {
	s := newFakeServer(4 << 20) // 4 MB
	s.chunkDelay = 5 * time.Millisecond
	d, _ := setup(t, s)
	d.Connections = 8

	var maxConns, segCount int
	d.OnProgress = func(p Progress) {
		maxConns = max(maxConns, p.Connections)
		if segCount == 0 {
			segCount = len(p.Segments) // the initial layout, before any splits
		}
	}
	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if maxConns != 8 || segCount != 8 {
		t.Errorf("expected 8 connections and 8 pieces, got %d and %d", maxConns, segCount)
	}
	// 8 pieces; at least several must have run at the same time.
	if m := s.maxInflight.Load(); m < 4 {
		t.Errorf("pieces don't seem to run in parallel: at most %d requests at once", m)
	}
	// The first request asks for the whole file ("bytes=0-") and is hung up
	// when its piece is done, so a little more than the file is sent - the
	// same cost as one split. Nothing more may be wasted.
	if sent := s.sentBytes.Load(); sent < int64(len(s.data)) || sent > int64(len(s.data))+minSplitSize {
		t.Errorf("server sent %d bytes for a %d byte file", sent, len(s.data))
	}
}

func TestSmallFileUsesOneConnection(t *testing.T) {
	s := newFakeServer(300_000) // below the split threshold
	d, _ := setup(t, s)
	d.Connections = 8

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if n := len(s.rangeHeaders); n != 1 {
		t.Errorf("small file should use 1 request, used %d", n)
	}
}

func TestMultiConnectionSurvivesDrops(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.dropAfter = 100_000 // every connection dies after 100 KB
	d, _ := setup(t, s)
	d.Connections = 8

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	// Every retry continues its own piece: no byte is sent twice.
	if sent := s.sentBytes.Load(); sent != int64(len(s.data)) {
		t.Errorf("server sent %d bytes for a %d byte file: data was re-downloaded", sent, len(s.data))
	}
}

func TestMultiConnectionStall(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.stallAfter = 50_000
	s.stallTimes = 3 // three of the connections freeze once
	d, _ := setup(t, s)
	d.Connections = 8

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
}

func TestMultiConnectionResumeAfterCancel(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.stallAfter = 100_000
	s.stallTimes = 8 // every piece sends 100 KB, then freezes
	d, ts := setup(t, s)
	d.Connections = 8
	d.StallTimeout = 10 * time.Second

	if _, err := d.Run(cancelSoon(d)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}
	m, err := readMeta(d.Folder + "/file.bin.part")
	if err != nil || len(m.Segments) != 8 {
		t.Fatalf("expected a meta file listing 8 pieces, got %v %v", m, err)
	}

	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: 8}
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
	if resumedAt != 8*100_000 {
		t.Errorf("expected to resume with 800000 bytes saved, got %d", resumedAt)
	}
	if sent := s.sentBytes.Load(); sent != int64(len(s.data)) {
		t.Errorf("server sent %d bytes for a %d byte file: data was re-downloaded", sent, len(s.data))
	}
}

// A .part left by a single-connection download is continued with several
// connections: the saved bytes are kept and the rest is split.
func TestSingleToMultiResume(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.stallAfter = 1_000_000
	s.stallTimes = 1
	d, ts := setup(t, s)
	d.Connections = 1
	d.StallTimeout = 10 * time.Second
	if _, err := d.Run(cancelSoon(d)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}

	s.chunkDelay = 5 * time.Millisecond
	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: 8}
	var resumedAt int64 = -1
	var pieces int
	d2.OnProgress = func(p Progress) {
		if p.Resumed {
			resumedAt = p.ResumedAt
		}
		if pieces == 0 {
			pieces = len(p.Segments) // the initial layout, before any splits
		}
	}
	path, err := d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	// 1 finished piece + the remaining 3,194,304 bytes split into pieces of at
	// least 512 KB: that's 6 pieces, not 8.
	if resumedAt != 1_000_000 || pieces != 7 {
		t.Errorf("expected resume at 1000000 with 1 done + 6 new pieces, got %d and %d pieces", resumedAt, pieces)
	}
}

// Some servers only allow a few connections per user and refuse the rest
// with "503 Service Unavailable". The refused pieces retry and finish later.
func TestServerConnectionLimit(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.maxConns = 3
	d, _ := setup(t, s)
	d.Connections = 8

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
}

func TestFileChangesDuringMultiDownload(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.stallAfter = 100_000
	s.stallTimes = 1 // one connection freezes, so it has to reconnect later
	s.chunkDelay = 5 * time.Millisecond
	d, _ := setup(t, s)
	d.Connections = 4

	// Soon after the download starts, the server switches to a new version.
	// When the frozen connection reconnects, it must notice.
	var once sync.Once
	d.OnProgress = func(p Progress) {
		if p.Status == StatusDownloading {
			once.Do(func() { s.setETag(`"v2"`) })
		}
	}
	_, err := d.Run(context.Background())
	if !errors.Is(err, ErrFileChanged) {
		t.Fatalf("expected ErrFileChanged, got %v", err)
	}
}

func TestChangedFileRestartsMultiDownload(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.stallAfter = 100_000
	s.stallTimes = 8
	d, ts := setup(t, s)
	d.Connections = 8
	d.StallTimeout = 10 * time.Second
	d.Run(cancelSoon(d))

	rand.Read(s.data) // a new version of the file, same size
	s.setETag(`"v2"`)

	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: 8}
	path, err := d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
}

// A damaged meta file must never be trusted: start over instead.
func TestDamagedMetaStartsOver(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.stallAfter = 100_000
	s.stallTimes = 8
	d, ts := setup(t, s)
	d.Connections = 8
	d.StallTimeout = 10 * time.Second
	d.Run(cancelSoon(d))

	// Remove one piece from the list, leaving a gap in the file.
	mp := d.Folder + "/file.bin.part.meta"
	m, _ := readMeta(d.Folder + "/file.bin.part")
	m.Segments = append(m.Segments[:3], m.Segments[4:]...)
	writeMeta(d.Folder+"/file.bin.part", m)
	if raw, _ := os.ReadFile(mp); !strings.Contains(string(raw), "segments") {
		t.Fatal("test setup failed")
	}

	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: 8}
	var resumed bool
	d2.OnProgress = func(p Progress) { resumed = resumed || p.Resumed }
	path, err := d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if resumed {
		t.Error("resumed from a damaged meta file")
	}
}

func TestValidSegments(t *testing.T) {
	cases := []struct {
		name string
		segs []segmentState
		ok   bool
	}{
		{"whole file", []segmentState{{0, 50, 10}, {50, 100, 50}}, true},
		{"gap", []segmentState{{0, 40, 0}, {50, 100, 0}}, false},
		{"overlap", []segmentState{{0, 60, 0}, {50, 100, 0}}, false},
		{"short", []segmentState{{0, 50, 0}}, false},
		{"done too big", []segmentState{{0, 100, 101}}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := validSegments(c.segs, 100); got != c.ok {
			t.Errorf("%s: got %v, want %v", c.name, got, c.ok)
		}
	}
}

// When connections fail in multi-connection mode, the reason must reach the
// interface, so a stuck download isn't a mystery.
func TestConnectionProblemsAreReported(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.maxConns = 2 // the server refuses most connections with 503
	s.chunkDelay = 5 * time.Millisecond
	d, _ := setup(t, s)
	d.Connections = 8

	var problem string
	d.OnProgress = func(p Progress) {
		if p.LastProblem != "" {
			problem = p.LastProblem
		}
	}
	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if !strings.Contains(problem, "503") || !strings.Contains(problem, "limits connections") {
		t.Errorf("expected the 503 and the connection limit to be reported, got %q", problem)
	}
}

// The real-world case from proof.ovh.net: the server allows 3 connections and
// answers the rest with 429. Surplus connections must drop out instead of
// retrying over and over - some servers block you entirely if you keep asking.
func TestSurplusConnectionsDropOut(t *testing.T) {
	s := newFakeServer(8 << 20)
	s.maxConns = 4 // 3 downloads + the probe's brief request
	s.limitStatus = http.StatusTooManyRequests
	s.chunkDelay = 3 * time.Millisecond
	d, _ := setup(t, s)
	d.Connections = 8

	var minAlive = 99
	d.OnProgress = func(p Progress) {
		if p.Status == StatusDownloading && p.Active > 0 {
			minAlive = min(minAlive, p.Connections)
		}
	}
	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	// 8 connections, 3-4 allowed: roughly one refusal per surplus connection.
	// Retrying them all with backoff would cause many more.
	if n := s.refused.Load(); n > 8 {
		t.Errorf("server refused %d requests: surplus connections kept retrying", n)
	}
	if minAlive > 4 {
		t.Errorf("connection count never dropped (lowest %d)", minAlive)
	}
	// Splits after the limit are fine (a finished connection reuses its own
	// place), but they must not cause more refusals - checked above.
}

func TestCooldownDelaysNewRequests(t *testing.T) {
	d := &Download{}
	d.startCooldown(200 * time.Millisecond)
	d.startCooldown(50 * time.Millisecond) // a shorter one must not cut it short
	start := time.Now()
	if err := d.waitCooldown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 180*time.Millisecond {
		t.Errorf("waited only %v", waited)
	}
	// Cancelling interrupts the wait.
	d.startCooldown(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.waitCooldown(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("expected cancel, got %v", err)
	}
}

// When EVERY connection is refused, there's no one to hand pieces to:
// they must retry (with backoff) rather than all drop out.
func TestAllRefusedStillRetries(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.status = http.StatusTooManyRequests
	d, _ := setup(t, s)
	d.Connections = 8
	d.MaxRetries = 2

	_, err := d.Run(context.Background())
	if !isConnectionLimit(err) {
		t.Fatalf("expected to give up with a 429 error, got %v", err)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	for _, c := range []struct {
		header string
		want   time.Duration
	}{
		{"5", 5 * time.Second},
		{"", 0},
		{"junk", 0},
		{"99999", 5 * time.Minute}, // capped
	} {
		resp := &http.Response{StatusCode: 429, Status: "429 Too Many Requests", Header: http.Header{}}
		if c.header != "" {
			resp.Header.Set("Retry-After", c.header)
		}
		if got := newHTTPError(resp).RetryAfter; got != c.want {
			t.Errorf("Retry-After %q: got %v, want %v", c.header, got, c.want)
		}
	}
}
