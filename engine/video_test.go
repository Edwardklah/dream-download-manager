package engine

// Tests for the engine features video downloads need.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rangeSpan returns how many bytes a "bytes=A-B" header asks for (-1 for
// an open-ended "bytes=A-").
func rangeSpan(t *testing.T, h string) int64 {
	t.Helper()
	a, b, _ := strings.Cut(strings.TrimPrefix(h, "bytes="), "-")
	if b == "" {
		return -1
	}
	x, _ := strconv.ParseInt(a, 10, 64)
	y, _ := strconv.ParseInt(b, 10, 64)
	return y - x + 1
}

// With MaxRequestSize, no request asks for more than that, with one
// connection or several, and the file still comes out whole.
func TestMaxRequestSize(t *testing.T) {
	for _, conns := range []int{1, 4} {
		s := newFakeServer(2 << 20)
		d, _ := setup(t, s)
		d.Connections = conns
		d.MaxRequestSize = 256 << 10

		path, err := d.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		checkFile(t, path, s.data)
		for _, h := range s.rangeHeaders {
			if n := rangeSpan(t, h); n < 0 || n > d.MaxRequestSize {
				t.Errorf("%d connections: request %q asks for more than %d bytes", conns, h, d.MaxRequestSize)
			}
		}
		// 2 MB in 256 KB requests: at least 8 of them.
		if len(s.rangeHeaders) < 8 {
			t.Errorf("%d connections: only %d requests", conns, len(s.rangeHeaders))
		}
		if sent := s.sentBytes.Load(); sent > int64(len(s.data))+minSplitSize {
			t.Errorf("%d connections: %d bytes sent for %d", conns, sent, len(s.data))
		}
	}
}

// Chunked requests and splitting together: a piece split off a slow
// connection must be finished chunk by chunk, not left after its first chunk.
func TestMaxRequestSizeWithSplits(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.slowRequests = 1
	s.slowDelay = 20 * time.Millisecond
	s.chunkDelay = time.Millisecond
	d, _ := setup(t, s)
	d.Connections = 4
	d.MaxRequestSize = 128 << 10

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if d.splits.Load() == 0 {
		t.Error("no splits happened, so this tested nothing")
	}
}

// A video's address changes when it's looked up again (they expire). With
// the same ResumeID, the .part from the old address is continued.
func TestResumeIDSurvivesAddressChange(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.stallAfter = 100_000
	s.stallTimes = 8
	d, ts := setup(t, s)
	d.URL = ts.URL + "/video?sig=old"
	d.Filename = "clip.mp4"
	d.ResumeID = "youtube:abc123:137"
	d.Connections = 8
	d.StallTimeout = 10 * time.Second
	if _, err := d.Run(cancelSoon(d)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}

	s.sentBytes.Store(0)
	d2 := &Download{URL: ts.URL + "/video?sig=new", Folder: d.Folder, Filename: "clip.mp4",
		ResumeID: "youtube:abc123:137", Connections: 8}
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
	if resumedAt <= 0 {
		t.Errorf("didn't resume (resumedAt %d)", resumedAt)
	}
	if !strings.HasSuffix(path, "clip.mp4") {
		t.Errorf("saved as %s", path)
	}

	// Without the ResumeID, a new address is a different download.
	d3 := &Download{URL: ts.URL + "/video?sig=other", Folder: d.Folder, Filename: "clip.mp4", Connections: 2}
	if d3.hasUnfinished() {
		t.Error("an unrelated address matched the old .part")
	}
}
