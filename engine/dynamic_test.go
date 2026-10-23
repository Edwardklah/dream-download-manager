package engine

// Tests for dynamic segmentation (stage 4): idle connections split the
// biggest remaining piece and take over its second half.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// One connection is 20x slower than the rest. Without splitting, the whole
// download waits for that one piece. With splitting, the others take over most
// of it, and the download should finish in well under half the time.
func TestSplittingBeatsSlowConnection(t *testing.T) {
	run := func(split bool) (time.Duration, int, *fakeServer) {
		s := newFakeServer(8 << 20) // 8 MB -> 4 pieces of 2 MB
		s.slowRequests = 1          // one piece's connection is 20x slower...
		s.slowDelay = 20 * time.Millisecond
		s.chunkDelay = 1 * time.Millisecond // ...than the others
		d, _ := setup(t, s)
		d.Connections = 4
		d.disableSplitting = !split

		start := time.Now()
		path, err := d.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		checkFile(t, path, s.data)
		return time.Since(start), int(d.splits.Load()), s
	}

	without, _, _ := run(false)
	with, splits, s := run(true)
	t.Logf("without splitting: %v, with splitting: %v (%d splits)", without, with, splits)

	if splits == 0 {
		t.Fatal("no pieces were split")
	}
	// Typically 3-4x faster; 0.6 leaves room for a busy test machine.
	if with > without*6/10 {
		t.Errorf("splitting should be much faster: %v vs %v without", with, without)
	}
	// When a slow connection's piece is cut short, it hangs up; the bytes
	// already on their way to it are wasted. That must stay small.
	size := int64(len(s.data))
	if waste := s.sentBytes.Load() - size; waste > int64(splits)*minSplitSize {
		t.Errorf("%d bytes wasted for %d splits", waste, splits)
	}
}

// Cancel after some splits have happened, then resume: the saved piece list
// must be valid and the result exact.
func TestResumeAfterSplits(t *testing.T) {
	s := newFakeServer(16 << 20)
	s.slowRequests = 2
	s.slowDelay = 20 * time.Millisecond
	s.chunkDelay = 2 * time.Millisecond
	d, ts := setup(t, s)
	d.Connections = 4

	ctx, cancel := context.WithCancel(context.Background())
	d.OnProgress = func(p Progress) {
		if p.Splits >= 2 {
			cancel()
		}
	}
	if _, err := d.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}
	m, err := readMeta(d.Folder + "/file.bin.part")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) <= 4 || !validSegments(m.Segments, int64(len(s.data))) {
		t.Fatalf("expected a valid piece list with splits, got %d pieces", len(m.Segments))
	}

	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: 4}
	path, err := d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
}

// Many slow connections and lots of splitting: whatever happens, every byte
// must land exactly once.
func TestHeavySplittingStaysCorrect(t *testing.T) {
	s := newFakeServer(8 << 20)
	s.slowRequests = 6
	s.slowDelay = 8 * time.Millisecond
	s.chunkDelay = 1 * time.Millisecond
	s.dropAfter = 900_000 // and connections drop too
	d, _ := setup(t, s)
	d.Connections = 8

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
}

// The split rules on their own, without any network.
func TestSplitRules(t *testing.T) {
	newDL := func(pieces ...*segment) *Download {
		return &Download{segments: pieces}
	}

	// A piece with 600 KB left (more than 2 x 256 KB) is split in the middle -
	// but only PROPOSED: nothing changes until the new connection starts.
	a := &segment{start: 0, end: 1_000_000, done: 400_000, taken: true}
	d := newDL(a)
	got := d.claimSegment()
	if got == nil {
		t.Fatal("expected a split")
	}
	if got.start != 700_000 || got.end != 1_000_000 || !got.pending() {
		t.Fatalf("bad proposal: new piece %d-%d, pending %v", got.start, got.end, got.pending())
	}
	if a.end != 1_000_000 || len(d.segments) != 1 {
		t.Fatal("a proposed split must not change anything yet")
	}
	if d.claimSegment() != nil {
		t.Error("a piece already being split was offered for a second split")
	}
	// The new connection started: commit.
	if !d.commitSplit(got) {
		t.Fatal("commit failed")
	}
	if a.end != 700_000 || len(d.segments) != 2 || d.segments[1] != got || got.pending() {
		t.Errorf("bad commit: old piece ends %d, %d pieces", a.end, len(d.segments))
	}

	// The old connection overtook the split point while the new one was
	// connecting: the split is dropped and the old piece keeps everything.
	b := &segment{start: 0, end: 1_000_000, done: 0, taken: true}
	d = newDL(b)
	p := d.claimSegment()
	b.done = 600_000 // got past 500,000 meanwhile
	if d.commitSplit(p) || b.end != 1_000_000 || len(d.segments) != 1 || b.splitting {
		t.Error("a stale split must not be committed")
	}

	// A split whose connection never started is simply cancelled.
	c := &segment{start: 0, end: 1_000_000, taken: true}
	d = newDL(c)
	p = d.claimSegment()
	d.abortSplit(p)
	if c.splitting || c.end != 1_000_000 || len(d.segments) != 1 {
		t.Error("abort must leave everything as it was")
	}

	// The split point is placed ahead of a moving connection: 1 MB/s x 2 s
	// lookahead, then half of what's left after that.
	e := &segment{start: 0, end: 10_000_000, taken: true, rate: 1_000_000}
	if p := newDL(e).claimSegment(); p == nil || p.start != 2_000_000+4_000_000 {
		t.Errorf("expected the split at 6,000,000, got %+v", p)
	}

	// 400 KB left is not worth splitting.
	f := &segment{start: 0, end: 1_000_000, done: 600_000, taken: true}
	if newDL(f).claimSegment() != nil {
		t.Error("split a piece that was nearly finished")
	}
	// ...unless its connection is crawling: 400 KB at 25 KB/s is 16 seconds.
	// (Found with the bench server's "slowdown" test.)
	slowSmall := &segment{start: 0, end: 1_000_000, done: 600_000, taken: true, rate: 25_000}
	if newDL(slowSmall).claimSegment() == nil {
		t.Error("a small piece on a crawling connection should be split")
	}
	// The SLOWEST piece is helped first, not the biggest: 2 MB at 10 MB/s
	// finishes in 0.2 s, 600 KB at 25 KB/s takes 24 s.
	fast := &segment{start: 0, end: 3_000_000, done: 1_000_000, taken: true, rate: 10_000_000}
	crawl := &segment{start: 3_000_000, end: 4_000_000, done: 400_000, taken: true, rate: 25_000, rateN: 10}
	p = newDL(fast, crawl).claimSegment()
	if p == nil || p.victim != crawl {
		t.Fatalf("expected the crawling piece to be split, got %+v", p)
	}
	// And since it's 400x slower than the other, the new connection takes
	// over everything after the lookahead (2 s x 25 KB/s), not just half.
	if want := int64(3_000_000 + 400_000 + 50_000); p.start != want {
		t.Errorf("crawling piece: new piece should start at %d, got %d", want, p.start)
	}

	// A free piece is always taken before anything is split.
	busy := &segment{start: 0, end: 5_000_000, taken: true}
	free := &segment{start: 5_000_000, end: 6_000_000}
	if got := newDL(busy, free).claimSegment(); got != free {
		t.Error("should take the free piece first")
	}

	// With no speeds known yet, the piece with the MOST left is split.
	small := &segment{start: 0, end: 2_000_000, done: 1_000_000, taken: true}
	big := &segment{start: 2_000_000, end: 6_000_000, done: 1_000_000, taken: true}
	if got := newDL(small, big).claimSegment(); got == nil || got.start != 4_500_000 {
		t.Errorf("should split the biggest piece at 4500000, got %+v", got)
	}

	// Disabled: nothing is split.
	g := &segment{start: 0, end: 9_000_000, taken: true}
	d = newDL(g)
	d.disableSplitting = true
	if d.claimSegment() != nil {
		t.Error("split while splitting was disabled")
	}
}
