package engine

// Tests for the refinements learned from studying Free Download Manager:
// gradual ramp-up, connect-first splitting (see TestSplitRules) and
// restarting connections that become very slow.

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// The server allows 3 downloads (plus the probe's brief request) and answers
// the rest with 429. Adding connections one at a time, and stopping as soon
// as one is refused, should cost about ONE refusal - not one per surplus
// connection, as when all 8 start at once.
func TestRampUpAvoidsRefusalBursts(t *testing.T) {
	s := newFakeServer(8 << 20)
	s.maxConns = 4
	s.limitStatus = http.StatusTooManyRequests
	// Pieces of ~300 ms. Much shorter, and connections are so often between
	// pieces (when the server may still count the old request) that refusals
	// happen by chance - unlike real downloads, where a piece lasts seconds.
	s.chunkDelay = 10 * time.Millisecond
	d, _ := setup(t, s)
	d.Connections = 8

	peak := 0
	d.OnProgress = func(p Progress) {
		if p.Status == StatusDownloading {
			peak = max(peak, p.Active)
		}
	}
	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if n := s.refused.Load(); n > 2 {
		t.Errorf("server refused %d requests %v; ramp-up should stop at the first refusal\nall: %v",
			n, s.refusedRanges, s.rangeHeaders)
	}
	if peak < 3 {
		t.Errorf("only %d connections were ever active; the server allows 3", peak)
	}
}

// Without a limit, ramp-up must still reach every connection allowed, and it
// adds them one at a time: never a crowd of connections waiting at once.
func TestRampUpReachesAllConnections(t *testing.T) {
	s := newFakeServer(8 << 20)
	s.chunkDelay = 2 * time.Millisecond
	d, _ := setup(t, s)
	d.Connections = 8

	peak := 0
	d.OnProgress = func(p Progress) { peak = max(peak, p.Connections) }
	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if peak != 8 {
		t.Errorf("reached %d connections, want 8", peak)
	}
	// One new connection at a time from the controller; a worker moving on
	// to its next piece can overlap with it, so allow a little.
	if n := s.peakConnecting.Load(); n > 3 {
		t.Errorf("%d requests were waiting for data at once", n)
	}
}

// fastSlowDetection shrinks the slow-connection timings so a test runs in
// well under a second: 5 samples of 20 ms to learn the normal speed, then a
// restart after 100 ms at a third of it.
func fastSlowDetection(t *testing.T) {
	every, samples, after := slowSampleEvery, slowBaselineSamples, slowRestartAfter
	slowSampleEvery, slowBaselineSamples, slowRestartAfter = 20*time.Millisecond, 5, 100*time.Millisecond
	t.Cleanup(func() { slowSampleEvery, slowBaselineSamples, slowRestartAfter = every, samples, after })
}

// A connection that slows to a crawl part-way (it happens: a congested route,
// a throttled server node) is dropped and reopened, which usually gets a
// fresh, fast connection. Without that, the rest would come at a crawl.
func TestSlowConnectionIsRestarted(t *testing.T) {
	for _, conns := range []int{1, 2} {
		s := newFakeServer(conns * 6 << 20)     // 6 MB per connection
		s.chunkDelay = 2 * time.Millisecond     // ~16 MB/s
		s.slowdownAfter = 3 << 20               // then the first connection drops
		s.slowdownDelay = 30 * time.Millisecond // to ~1 MB/s
		d, _ := setup(t, s)
		fastSlowDetection(t)
		d.Connections = conns
		d.disableSplitting = true // so only a restart can rescue the slow piece

		start := time.Now()
		path, err := d.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		checkFile(t, path, s.data)
		// One request per connection, plus at least one restart.
		if n := s.requests.Load(); n < int32(conns)+1 {
			t.Errorf("%d connections: %d requests, the slow connection was never restarted", conns, n)
		}
		// At 1 MB/s the slow part alone would take ~3 s.
		if took > 1500*time.Millisecond {
			t.Errorf("%d connections: took %v; the slow connection wasn't replaced quickly", conns, took)
		}
	}
}

// A connection that is steadily slow from the start is NOT restarted: it
// isn't getting worse, and reconnecting would only waste time.
func TestSteadySlowConnectionIsKept(t *testing.T) {
	s := newFakeServer(1 << 20)
	s.chunkDelay = 15 * time.Millisecond // slow, but always the same
	d, _ := setup(t, s)
	fastSlowDetection(t)

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if n := s.requests.Load(); n != 1 { // just the download
		t.Errorf("%d requests: a steady connection was restarted", n)
	}
}

// Found with the bench server: after a refusal, the cool-down also held back
// the connections that were working fine, so each of them sat idle between
// pieces. Only the refused one should wait; the others carry straight on.
func TestCooldownDoesNotStallWorkingConnections(t *testing.T) {
	s := newFakeServer(8 << 20)
	s.maxConns = 4 // 3 downloads + the probe
	s.limitStatus = http.StatusTooManyRequests
	s.chunkDelay = time.Millisecond
	d, _ := setup(t, s)
	minCooldown = 2 * time.Second // long, so any wait on it shows
	d.Connections = 8

	start := time.Now()
	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if s.refused.Load() == 0 {
		t.Fatal("the server never refused, so this tested nothing")
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("took %v: working connections waited out the %v cool-down", took, minCooldown)
	}
}
