package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Stages 3 + 4: multi-connection downloading with dynamic segmentation.
//
// STAGE 3 - SPLITTING. The file is split into pieces ("segments"), e.g. a
// 100 MB file with 8 connections becomes 8 pieces of 12.5 MB. Each connection
// downloads its own piece with a request like "Range: bytes=26214400-39321599"
// and writes the bytes straight into their correct position in the .part file.
//
// The .part file is created at full size up front, so every connection can
// write anywhere in it at any time. Because of that, the .part's size says
// nothing about progress - the .meta file records how much of each piece is
// done, and is saved every second so a cancelled or crashed download can
// resume each piece where it stopped.
//
// STAGE 4 - DYNAMIC SEGMENTATION. Connections never run at the same speed. With
// a fixed split, fast connections finish early and sit idle while the slowest
// piece drags on alone. So when a connection runs out of work, it finds the
// piece with the most bytes left, cuts what's left of it in half, and takes
// the second half. The slow connection keeps the first half and simply stops
// earlier. Every connection stays busy until the file is nearly done.
//
// Each connection runs in its own goroutine, with its own retry loop and
// stall watchdog, so one bad connection doesn't stop the others.
//
// REFINEMENTS borrowed from studying FDM 3.9.7's engine (ideas only; no code):
//   - RAMP-UP: start with ONE connection and add the next only while every
//     existing one is receiving data. A server that limits connections refuses
//     one extra request, then we stop adding - instead of firing 8 at once.
//   - CONNECT FIRST, THEN SPLIT: a split only takes effect once its new
//     connection has actually started. If the server refuses it, nothing
//     changes and no piece is left orphaned.
//   - PREDICTED SPLIT POINT: the split is placed ahead of where the slow
//     connection will have got to by the time the new one is ready.
//   - SLOW-CONNECTION RESTART: a connection that drops to a third of its early
//     speed for minutes is dropped and reconnected (often to a faster route).

const (
	// The initial split never makes pieces smaller than this.
	minSegmentSize = 512 * 1024
	// A piece is only cut in half if both halves would be at least this big.
	// Each new piece costs a round trip to the server before data flows
	// (around half a second on a slow, distant connection), so cutting a
	// nearly finished piece would make things slower, not faster.
	minSplitSize = 256 * 1024
	// ...except for a piece that would still take a long time at its
	// connection's speed (slowSplitAfter): a crawling connection with 400 KB
	// left is worth helping. Its halves may be as small as this.
	tinySplitSize = 64 * 1024
	metaSaveEvery = time.Second
)

// Tuning knobs. Variables only so the tests can shrink them.
var (
	// hangupGrace: after hanging up mid-reply on a server that limits
	// connections, wait this long before the next request, so the server has
	// noticed and freed our place.
	hangupGrace = 500 * time.Millisecond
	// initialConnections start together; the rest are added one at a time.
	// Three: most servers that limit connections allow at least that many.
	initialConnections = 3
	// rampInterval is the shortest gap between adding connections.
	rampInterval = 250 * time.Millisecond
	// splitLookahead: how far ahead of a slow connection to place a split,
	// in seconds of its current speed - roughly how long a new connection
	// takes to start receiving data on a slow, distant link.
	splitLookahead = 2.0
	// slowSplitAfter: a piece that would take longer than this to finish is
	// split even if it's small (down to tinySplitSize halves).
	slowSplitAfter = 4 * time.Second
	// crawlFactor: a connection this many times slower than the others has
	// the whole rest of its piece taken over, not just half.
	crawlFactor = 4.0
	// Slow-connection restart: after slowBaselineSamples one-second samples
	// set a connection's normal speed, it's restarted if it then stays at
	// 1/slowFactor of that (or less) for slowRestartAfter.
	slowSampleEvery     = time.Second
	slowBaselineSamples = 20
	slowFactor          = 3.0
	slowRestartAfter    = 3 * time.Minute
)

// ErrTooSlow means a connection was dropped on purpose because it had become
// far slower than it started out; it's reconnected straight away.
var ErrTooSlow = errors.New("connection became very slow; reconnecting")

// errSplitAbandoned: a new connection meant to take over half of a piece
// didn't start (refused, failed, or the piece had moved on). Nothing was
// changed; the worker just stops.
var errSplitAbandoned = errors.New("split abandoned")

// errSplitStale: by the time a split's connection started, the original
// connection had already passed the split point, so it isn't needed.
var errSplitStale = errors.New("split no longer needed")

// errDropConnection tells a worker to stop: the server limits how many
// connections we may open, and this one is surplus.
var errDropConnection = errors.New("connection dropped: server limits connections")

// ErrFileChanged means the server started sending a different version of the
// file in the middle of a multi-connection download.
var ErrFileChanged = errors.New("the file changed on the server during the download; start it again")

// segment is one piece of the file being downloaded by one connection.
//
// GO NOTE: several goroutines use each segment (its worker, the progress
// reporter, the meta saver, and in stage 4 any worker that splits it), so
// its fields are protected by a mutex. Only code holding the lock
// (mu.Lock ... mu.Unlock) may read or change them.
type segment struct {
	mu       sync.Mutex
	start    int64  // first byte of the piece
	end      int64  // one past the last byte; shrinks when the piece is split
	done     int64  // bytes saved, counting from start
	taken    bool   // a worker owns this piece
	active   bool   // its connection is receiving data right now
	retrying bool   // waiting to reconnect after a problem
	problem  string // the last problem its connection hit

	// Speed of this piece's connection, for predicting where it will be.
	rate      float64 // bytes per second (smoothed)
	rateBytes int64   // bytes since rateAt
	rateN     int     // how many times rate has been measured
	rateAt    time.Time

	// Splitting (connect first, then split):
	splitting bool     // a new connection is trying to take over our second half
	victim    *segment // set on a NEW piece until its split is committed

	// hungUp: the piece finished before the server's reply did (its end
	// moved, or the reply was open-ended), so we hung up mid-reply.
	hungUp bool
}

// takeHungUp reports (and clears) whether the last reply was hung up early.
func (s *segment) takeHungUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hungUp
	s.hungUp = false
	return h
}

// position returns where to continue writing, and where the piece ends.
func (s *segment) position() (pos, end int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.start + s.done, s.end
}

// write saves up to len(data) bytes at the piece's current position, never
// past its end, and records them as done. It returns how many bytes were
// written and whether the piece is now complete.
//
// The lock is held for the whole write. That matters for stage 4: if a split
// could happen between checking "where does my piece end?" and writing, this
// connection might write bytes that now belong to the new piece - and both
// would count them. Holding the lock makes check-write-record one step that a
// split can't interrupt. (Writing 64 KB into the operating system's file cache
// takes microseconds, so nobody waits long.)
func (s *segment) write(file *os.File, data []byte) (n int64, complete bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pos := s.start + s.done
	n = min(int64(len(data)), s.end-pos)
	if n > 0 {
		// GO NOTE: WriteAt writes at an exact position in the file without
		// moving a shared "cursor", so many goroutines can write different
		// parts of the same file at the same time safely.
		if _, err := file.WriteAt(data[:n], pos); err != nil {
			return 0, false, err
		}
		s.done += n
		s.trackRateLocked(n)
	}
	return n, s.start+s.done >= s.end, nil
}

// trackRateLocked keeps a smoothed bytes-per-second figure for the piece.
func (s *segment) trackRateLocked(n int64) {
	now := time.Now()
	if s.rateAt.IsZero() {
		s.rateAt = now
	}
	s.rateBytes += n
	if el := now.Sub(s.rateAt).Seconds(); el >= 0.5 {
		r := float64(s.rateBytes) / el
		if s.rate == 0 {
			s.rate = r
		} else {
			s.rate = 0.7*s.rate + 0.3*r // smooth out bursts
		}
		s.rateBytes, s.rateAt = 0, now
		s.rateN++
	}
}

// pending reports whether this is a new piece whose split isn't committed yet.
func (s *segment) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.victim != nil
}

func (s *segment) setActive(a bool) {
	s.mu.Lock()
	s.active = a
	s.mu.Unlock()
}

// release gives the piece back so another connection can take it over.
func (s *segment) release() {
	s.mu.Lock()
	s.taken = false
	s.retrying = false
	s.mu.Unlock()
}

func (s *segment) setRetrying(retrying bool, problem string) {
	s.mu.Lock()
	s.retrying = retrying
	if problem != "" {
		s.problem = problem
	}
	s.mu.Unlock()
}

func (s *segment) state() SegmentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SegmentState{Start: s.start, End: s.end, Done: s.done,
		Active: s.active, Retrying: s.retrying, Problem: s.problem}
}

func (s *segment) remaining() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end - (s.start + s.done)
}

// useSegments decides between multi- and single-connection downloading.
func (d *Download) useSegments() bool {
	if d.saved != nil && len(d.saved.Segments) > 0 {
		return true // continuing a multi-connection download: its pieces must be finished that way
	}
	return d.Connections > 1 && d.Info.SupportsRanges && d.Info.Size >= 2*minSegmentSize
}

// buildSegments splits the file into pieces, or restores the pieces of a
// download being resumed.
func (d *Download) buildSegments() []*segment {
	var segs []*segment
	if d.saved != nil && len(d.saved.Segments) > 0 {
		for _, s := range d.saved.Segments {
			segs = append(segs, &segment{start: s.Start, end: s.End, done: s.Done})
		}
		return segs
	}

	// Fresh, or resuming a SINGLE-connection .part: the first `kept` bytes
	// are already saved and become one finished piece.
	kept := d.downloaded.Load()
	if kept > 0 {
		segs = append(segs, &segment{start: 0, end: kept, done: kept})
	}
	remaining := d.Info.Size - kept
	n := int64(d.Connections)
	if most := remaining / minSegmentSize; most < n {
		n = max(most, 1)
	}
	size := remaining / n
	for i := int64(0); i < n; i++ {
		start := kept + i*size
		end := start + size
		if i == n-1 {
			end = d.Info.Size // the last piece takes the leftover bytes
		}
		segs = append(segs, &segment{start: start, end: end})
	}
	return segs
}

// runSegmented downloads the file over several connections at once.
func (d *Download) runSegmented(ctx context.Context, partPath string) error {
	d.segments = d.buildSegments()

	// Save the list of pieces BEFORE enlarging the .part file. Order matters:
	// a full-size .part with no piece list would look like a finished
	// single-connection download, and a crash at that moment would make the
	// next run accept a file full of zeros as complete.
	states, _ := d.segmentStates()
	if err := d.saveSegments(partPath, states); err != nil {
		return &diskError{err}
	}

	file, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return &diskError{err}
	}
	defer file.Close()
	// Make the .part full size so every piece can be written in place.
	if stat, err := file.Stat(); err != nil || stat.Size() != d.Info.Size {
		if err := file.Truncate(d.Info.Size); err != nil {
			return &diskError{err}
		}
	}

	// workCtx is cancelled if any connection hits an error that can't be
	// retried, which stops all the others too.
	workCtx, stopWork := context.WithCancel(ctx)
	defer stopWork()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		stopWork()
	}

	// Show the pieces right away, before any data arrives, so an interface
	// can draw them from the first moment.
	d.report(Progress{Status: StatusDownloading, Connections: d.Connections, Segments: states})

	// startWorker starts one connection. It keeps taking pieces (or halves of
	// pieces) until there's nothing left for it.
	// GO NOTE: a WaitGroup counts running goroutines. Add(1) before starting
	// each one, Done() when it finishes, and Wait() blocks until the count
	// is back to zero.
	startWorker := func() {
		wg.Add(1)
		d.alive.Add(1)
		go func() {
			defer wg.Done()
			defer d.alive.Add(-1)
			// healthy: this connection's last piece finished without trouble.
			// It may go straight on to the next one, even during a cool-down:
			// it is only taking over its own place on the server, not asking
			// for an extra connection.
			healthy := false
			for {
				seg := d.claimSegment()
				if seg == nil && d.splitPending() {
					// Another connection is splitting the piece we'd have
					// taken. Its split takes a moment to start or fail;
					// wait for that rather than quitting.
					if sleepCtx(workCtx, rampInterval) != nil {
						return
					}
					continue
				}
				if seg == nil {
					return // nothing left to download or split
				}
				err := d.runSegment(workCtx, file, seg, healthy)
				healthy = err == nil
				// We hung up on a server that limits connections. It takes a
				// moment to notice and free our place; asking again before
				// that would be refused as one too many.
				if err == nil && seg.takeHungUp() && d.limited.Load() {
					if sleepCtx(workCtx, hangupGrace) != nil {
						return
					}
				}
				switch {
				case errors.Is(err, errDropConnection):
					return // surplus connection: bow out quietly
				case errors.Is(err, errSplitAbandoned):
					// Its new connection didn't start. Wait a little before the
					// controller tries again, longer each time it happens.
					n := d.splitFailures.Add(1)
					d.rampPauseUntil.Store(time.Now().Add(retryDelay(int(n))).UnixNano())
					return
				case err != nil:
					fail(err)
					return
				}
			}
		}()
	}

	// RAMP-UP: start with a few connections (IDM opens 4 almost at once;
	// FDM starts with 1 - we take the middle road), then add the next only
	// while every running connection is receiving data. If any is
	// connecting, retrying or was refused, we hold off. A server that limits
	// connections sees one refused request, not a burst.
	// The controller is itself counted in the WaitGroup, so wg.Wait() can't
	// return while it might still add a worker.
	for range min(initialConnections, d.Connections) {
		startWorker()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(rampInterval)
		defer tick.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-tick.C:
			}
			alive := d.alive.Load()
			if alive == 0 {
				return // every connection has finished
			}
			if int(alive) >= d.Connections || d.limited.Load() ||
				d.activeNow.Load() < alive || // someone is connecting or retrying
				time.Now().UnixNano() < d.rampPauseUntil.Load() ||
				!d.hasWork() {
				continue
			}
			startWorker()
		}
	}()

	// Meanwhile, one more goroutine reports progress and saves the meta file.
	// GO NOTE: closing a channel is a signal every receiver sees at once.
	// "stop" is closed when the connections are finished, which ends this loop.
	stop := make(chan struct{})
	reporterDone := make(chan struct{})
	go func() {
		defer close(reporterDone)
		tick := time.NewTicker(reportInterval)
		defer tick.Stop()
		lastSave := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				states, active := d.segmentStates()
				d.reportProgress(int(d.alive.Load()), active, states)
				if time.Since(lastSave) >= metaSaveEvery {
					lastSave = time.Now()
					d.saveSegments(partPath, states)
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	<-reporterDone

	// If the server refused connections, remember how many it DID allow at
	// once, so the next download from it starts with that many.
	if d.limited.Load() {
		d.Limits.Learn(hostOf(d.Info.URL), max(int(d.peakActive.Load()), 1))
	}

	// Always save where every piece got to: if we were cancelled or failed,
	// this is what the next run resumes from.
	states, _ = d.segmentStates()
	if err := d.saveSegments(partPath, states); err != nil && firstErr == nil {
		firstErr = &diskError{err}
	}

	if ctx.Err() != nil {
		return ctx.Err() // the user cancelled
	}
	if firstErr != nil {
		return firstErr
	}
	for _, s := range states {
		if s.Start+s.Done < s.End {
			return fmt.Errorf("%w: a piece was left unfinished", ErrIncomplete)
		}
	}
	if err := file.Close(); err != nil {
		return &diskError{err}
	}
	return nil
}

// claimSegment gives a worker something to do:
//  1. the next unfinished piece nobody owns yet, or
//  2. (stage 4) a PROPOSED split: a new piece for the second half of the
//     piece with the most left. Nothing changes yet - the split only takes
//     effect once the new piece's connection has started (commitSplit).
//
// It returns nil when there's nothing left worth taking.
//
// GO NOTE - LOCK ORDER: this takes d.segMu (the list) and then individual
// piece locks. Everything that needs both takes them in that same order.
// If one goroutine took A then B while another took B then A, each could end
// up holding one lock and waiting forever for the other - a "deadlock".
func (d *Download) claimSegment() *segment {
	d.segMu.Lock()
	defer d.segMu.Unlock()

	for _, s := range d.segments {
		s.mu.Lock()
		free := !s.taken && s.start+s.done < s.end
		if free {
			s.taken = true
		}
		s.mu.Unlock()
		if free {
			return s
		}
	}

	victim, mid := d.findSplitLocked()
	if victim == nil {
		return nil
	}
	victim.mu.Lock()
	victim.splitting = true // no second split of the same piece meanwhile
	piece := &segment{start: mid, end: victim.end, taken: true, victim: victim}
	victim.mu.Unlock()
	return piece
}

// findSplitLocked picks the piece worth splitting and where. The caller holds
// d.segMu. Returns nil if nothing is worth splitting.
func (d *Download) findSplitLocked() (victim *segment, mid int64) {
	// Splitting still happens after the server has said "too many": the
	// connection doing the split has just finished its own piece, so it only
	// takes its own place on the server back - the count doesn't go up.
	// (Extra connections are what get refused, and the ramp-up stops adding
	// those after a refusal. Without splitting, connections that finish
	// early would sit idle while the last pieces crawl in.)
	if d.disableSplitting {
		return nil, 0
	}
	// Pick the piece that will take LONGEST to finish, not the one with the
	// most bytes left: 400 KB on a crawling connection holds the download up
	// longer than 2 MB on a fast one. A piece whose speed isn't known yet is
	// assumed to go at the average speed of the others.
	type cand struct {
		s         *segment
		pos, left int64
		rate      float64
		settled   bool // speed measured for a couple of seconds, not just starting up
	}
	var cands []cand
	var rateSum float64
	var rated int
	for _, s := range d.segments {
		s.mu.Lock()
		// Speeds of all pieces, finished ones included: "how fast is a
		// connection to this server normally?"
		if s.rate > 0 {
			rateSum += s.rate
			rated++
		}
		if !s.splitting && s.taken {
			// Where will this piece's connection be by the time a new
			// connection is ready? Split the rest from there.
			pos := s.start + s.done + int64(s.rate*splitLookahead)
			if left := s.end - pos; left > 0 {
				cands = append(cands, cand{s, pos, left, s.rate, s.rateN >= 4})
			}
		}
		s.mu.Unlock()
	}
	var best *cand
	var bestTime float64 // seconds to finish (or bytes, if no speeds are known)
	for i := range cands {
		c := &cands[i]
		rate := c.rate
		if rate <= 0 && rated > 0 {
			rate = rateSum / float64(rated)
		}
		t := float64(c.left)
		if rate > 0 {
			t = float64(c.left) / rate
		}
		if best == nil || t > bestTime {
			best, bestTime = c, t
		}
	}
	if best == nil {
		return nil, 0
	}
	slow := rated > 0 && bestTime >= slowSplitAfter.Seconds()

	// A connection far slower than the others (under 1/crawlFactor of their
	// average) isn't worth sharing with: the new connection takes over
	// everything after the split point, and the slow one only finishes the
	// little it gets while the new one connects.
	// (Only once its speed has settled: a connection that has just started
	// can look slow while TCP is still speeding up.)
	if best.settled && rated > 1 && slow {
		others := (rateSum - best.rate) / float64(rated-1)
		if best.rate*crawlFactor < others && best.left >= tinySplitSize {
			return best.s, best.pos
		}
	}
	if best.left < 2*minSplitSize && !(slow && best.left >= 2*tinySplitSize) {
		return nil, 0 // not worth splitting: let it finish
	}
	return best.s, best.pos + best.left/2
}

// splitPending reports whether some piece has a proposed split that hasn't
// started or failed yet.
func (d *Download) splitPending() bool {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	for _, s := range d.segments {
		s.mu.Lock()
		p := s.splitting
		s.mu.Unlock()
		if p {
			return true
		}
	}
	return false
}

// hasWork reports whether a new connection would find anything to do.
func (d *Download) hasWork() bool {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	for _, s := range d.segments {
		s.mu.Lock()
		free := !s.taken && s.start+s.done < s.end
		s.mu.Unlock()
		if free {
			return true
		}
	}
	v, _ := d.findSplitLocked()
	return v != nil
}

// commitSplit makes a proposed split real, now that its connection has
// started: the old piece is shortened to end where the new one begins. If the
// old piece's connection has already passed that point, the split is
// abandoned (false) - the new connection's data isn't needed.
func (d *Download) commitSplit(piece *segment) bool {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	piece.mu.Lock()
	v := piece.victim
	piece.victim = nil
	piece.mu.Unlock()
	if v == nil {
		return true
	}

	v.mu.Lock()
	ok := v.start+v.done < piece.start && v.end == piece.end
	if ok {
		v.end = piece.start
	}
	v.splitting = false
	v.mu.Unlock()
	if !ok {
		return false
	}
	// Keep the list in file order, so the meta file stays easy to check.
	// GO NOTE: slices.Insert puts a value at a position, shifting the rest along.
	at := slices.Index(d.segments, v)
	d.segments = slices.Insert(d.segments, at+1, piece)
	d.splits.Add(1)
	return true
}

// abortSplit cancels a proposed split that never started. Nothing had changed,
// so there's nothing to undo except unmarking the old piece.
func (d *Download) abortSplit(piece *segment) {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	piece.mu.Lock()
	v := piece.victim
	piece.victim = nil
	piece.mu.Unlock()
	if v != nil {
		v.mu.Lock()
		v.splitting = false
		v.mu.Unlock()
	}
}

// minCooldown is the shortest pause after a "too many" reply, for servers
// that don't send a Retry-After header. A variable only so tests can shrink it.
var minCooldown = 5 * time.Second

// startCooldown stops ALL connections from sending new requests for a while.
// Connections already receiving data carry on.
func (d *Download) startCooldown(wait time.Duration) {
	until := time.Now().Add(wait).UnixNano()
	// GO NOTE: a "compare-and-swap" loop. Several goroutines may call this at
	// once; CompareAndSwap only stores the new value if nobody changed it in
	// between, so the LATEST end time always wins, without a mutex.
	for {
		cur := d.cooldownUntil.Load()
		if until <= cur || d.cooldownUntil.CompareAndSwap(cur, until) {
			return
		}
	}
}

// waitCooldown blocks until any cool-down has passed (or ctx is cancelled).
func (d *Download) waitCooldown(ctx context.Context) error {
	wait := time.Until(time.Unix(0, d.cooldownUntil.Load()))
	if wait <= 0 {
		return nil
	}
	return sleepCtx(ctx, wait)
}

// othersActive reports whether any piece other than this one is receiving data.
func (d *Download) othersActive(self *segment) bool {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	for _, s := range d.segments {
		if s != self && s.state().Active {
			return true
		}
	}
	return false
}

// segmentStates is a snapshot of every piece, plus how many are receiving data.
func (d *Download) segmentStates() ([]SegmentState, int) {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	states := make([]SegmentState, len(d.segments))
	active := 0
	for i, s := range d.segments {
		states[i] = s.state()
		if states[i].Active {
			active++
		}
	}
	return states, active
}

func (d *Download) saveSegments(partPath string, states []SegmentState) error {
	m := &meta{
		URL: d.URL, FinalURL: d.Info.URL, Size: d.Info.Size,
		ETag: d.Info.ETag, LastModified: d.Info.LastModified, ResumeID: d.ResumeID,
	}
	for _, s := range states {
		m.Segments = append(m.Segments, segmentState{Start: s.Start, End: s.End, Done: s.Done})
	}
	return writeMeta(partPath, m)
}

// runSegment downloads one piece, retrying on temporary failures - the same
// rules as single-connection mode, but per piece.
func (d *Download) runSegment(ctx context.Context, file *os.File, seg *segment, healthy bool) error {
	// A proposed split gets ONE attempt to start. If it doesn't, nothing has
	// changed (the other piece still covers those bytes), so we just give up.
	if seg.pending() {
		err := d.fetchSegment(ctx, file, seg, healthy)
		if seg.pending() || errors.Is(err, errSplitStale) {
			d.abortSplit(seg)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isConnectionLimit(err) {
				d.limited.Store(true)
				d.startCooldown(max(retryAfter(err), minCooldown))
			}
			if err != nil && !retryable(err) {
				return err // e.g. the file changed on the server
			}
			return errSplitAbandoned
		}
		d.splitFailures.Store(0) // it started: splitting works on this server
		// From here it's an ordinary piece: the loop below finishes it (with
		// MaxRequestSize, that first request may have been only its first
		// chunk) or retries it if that request failed.
	}

	failures := 0
	for {
		before, end := seg.position()
		if before >= end {
			return nil
		}
		err := d.fetchSegment(ctx, file, seg, healthy)
		healthy = false // any retry waits out a cool-down
		if err == nil {
			continue // the loop's first check confirms the piece is done
		}
		if ctx.Err() != nil || !retryable(err) {
			return err
		}
		if errors.Is(err, ErrTooSlow) {
			failures = 0
			continue // dropped on purpose: reconnect at once, no backoff
		}

		// The server refused this connection with "too many" while other
		// connections are working: it limits how many we may open. Retrying
		// would just keep hammering it (some servers then block you entirely),
		// so give this piece back for the working connections to take over,
		// and close this one for good.
		if isConnectionLimit(err) {
			d.limited.Store(true)
			d.startCooldown(max(retryAfter(err), minCooldown))
		}
		if isConnectionLimit(err) && d.othersActive(seg) {
			seg.release()
			d.noteProblem(fmt.Sprintf("%v - the server limits connections, continuing with %d",
				err, d.alive.Load()-1))
			return errDropConnection
		}

		if after, _ := seg.position(); after > before {
			failures = 0 // that attempt made progress
		}
		failures++
		if failures > d.MaxRetries {
			return fmt.Errorf("gave up on bytes %d-%d after %d attempts: %w", before, end-1, d.MaxRetries, err)
		}
		// Tell the interface what went wrong, so a stuck download isn't a mystery.
		delay := max(retryDelay(failures), retryAfter(err)) // the server's request wins if longer
		problem := fmt.Sprintf("%v (retry %d of %d in %v)", err, failures, d.MaxRetries, delay)
		if isConnectionLimit(err) {
			problem = fmt.Sprintf("%v - the server limits connections (retry %d of %d in %v)",
				err, failures, d.MaxRetries, delay)
		}
		seg.setRetrying(true, problem)
		d.noteProblem(problem)
		limitErr := isConnectionLimit(err)
		err = sleepCtx(ctx, delay)
		seg.setRetrying(false, "")
		if err != nil {
			return err
		}
		// Refused while the others happened to be between pieces? Look again
		// now: if they're receiving data, the server is busy with them, and
		// asking again would most likely just be refused again.
		if limitErr && d.othersActive(seg) {
			seg.release()
			return errDropConnection
		}
	}
}

// fetchSegment makes ONE connection attempt for a piece, continuing from
// where it got to. Returns nil when the piece is complete.
// skipCooldown is for a connection carrying straight on after a good piece.
func (d *Download) fetchSegment(ctx context.Context, file *os.File, seg *segment, skipCooldown bool) error {
	// If the server recently said "too many", wait before asking again.
	if !skipCooldown {
		if err := d.waitCooldown(ctx); err != nil {
			return err
		}
	}
	pos, end := seg.position()

	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(d.StallTimeout, func() {
		stalled.Store(true)
		cancelAttempt()
	})
	defer watchdog.Stop()

	// With MaxRequestSize, ask for at most that much; runSegment's loop
	// then asks for the next share.
	reqEnd := end
	if d.MaxRequestSize > 0 {
		reqEnd = min(end, pos+d.MaxRequestSize)
	}
	resp, err := d.startResponse(attemptCtx, pos, reqEnd-1)
	if err != nil {
		return d.attemptErr(err, &stalled, pos)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent: // 206: the piece we asked for
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", pos)) {
			return fmt.Errorf("server sent the wrong part: %s", resp.Header.Get("Content-Range"))
		}
		// Connect first, then split: only now that this connection has
		// started does a proposed split take effect.
		if seg.pending() && !d.commitSplit(seg) {
			return errSplitStale
		}
	case http.StatusOK:
		// 200 = the WHOLE file. The server said it supports ranges, so this
		// means If-Range failed: the file is no longer the version we started.
		return ErrFileChanged
	default:
		return newHTTPError(resp)
	}

	seg.setActive(true)
	defer seg.setActive(false)
	// Track the most connections receiving data at the same time.
	now := d.activeNow.Add(1)
	defer d.activeNow.Add(-1)
	for {
		peak := d.peakActive.Load()
		if now <= peak || d.peakActive.CompareAndSwap(peak, now) {
			break
		}
	}

	slow := newSlowDetector()
	buf := make([]byte, chunkSize)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(d.StallTimeout)
			// The piece's end may have moved since we asked the server for it
			// (another connection took over the rest), so write() stops at the
			// CURRENT end and tells us when the piece is complete. We then just
			// hang up; the server's remaining bytes belong to someone else.
			written, complete, err := seg.write(file, buf[:n])
			if err != nil {
				return &diskError{err}
			}
			d.downloaded.Add(written)
			if complete {
				if resp.ContentLength >= 0 {
					_, end := seg.position()
					if pos+resp.ContentLength > end {
						seg.mu.Lock()
						seg.hungUp = true
						seg.mu.Unlock()
					}
				}
				return nil
			}
		}
		if readErr == io.EOF {
			// Done when the piece is complete, or this request's share is
			// (the piece's end may have moved closer meanwhile, by a split).
			if got, end := seg.position(); got < min(end, reqEnd) {
				return fmt.Errorf("%w: piece ended at byte %d of %d", ErrIncomplete, got, min(end, reqEnd))
			}
			return nil
		}
		if readErr == nil && slow.tooSlow(n) { // after saving, so nothing is lost
			return ErrTooSlow // runSegment reconnects at once
		}
		if readErr != nil {
			got, _ := seg.position()
			return d.attemptErr(readErr, &stalled, got)
		}
	}
}

// slowDetector spots a connection that has become far slower than it was,
// as FDM does: it learns the connection's normal speed from its first
// slowBaselineSamples one-second samples, then flags it if it stays at
// 1/slowFactor of that or less for slowRestartAfter.
type slowDetector struct {
	sampleStart time.Time
	sampleBytes int
	samples     int
	baseline    float64 // average bytes/second over the first samples
	lowSince    time.Time
}

func newSlowDetector() *slowDetector { return &slowDetector{sampleStart: time.Now()} }

// tooSlow is called after every read with the number of bytes it returned.
func (s *slowDetector) tooSlow(n int) bool {
	s.sampleBytes += n
	el := time.Since(s.sampleStart)
	if el < slowSampleEvery {
		return false
	}
	speed := float64(s.sampleBytes) / el.Seconds()
	s.sampleStart, s.sampleBytes = time.Now(), 0

	if s.samples < slowBaselineSamples {
		s.baseline = (s.baseline*float64(s.samples) + speed) / float64(s.samples+1)
		s.samples++
		return false
	}
	if s.baseline > 0 && speed*slowFactor <= s.baseline {
		if s.lowSince.IsZero() {
			s.lowSince = time.Now()
		}
		return time.Since(s.lowSince) >= slowRestartAfter
	}
	s.lowSince = time.Time{}
	return false
}
