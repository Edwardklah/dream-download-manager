package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// How a download runs:
//
//  1. Probe the file: size, name, range support, ETag/Last-Modified.
//  2. Look for an unfinished "<name>.part" from an earlier attempt. If its
//     ".part.meta" says it's the same file and the server supports ranges,
//     continue from where it stopped. Otherwise start fresh.
//  3. Download it:
//     - MULTI-CONNECTION (segmented.go) if the server supports ranges and the
//     file is big enough: split it into pieces and fetch them in parallel.
//     - SINGLE-CONNECTION (this file) otherwise.
//  4. If a connection drops or freezes, wait a moment and continue from the
//     last byte saved (or from zero if the server doesn't allow ranges).
//  5. When every byte has arrived, rename "<name>.part" to "<name>" and
//     delete the .meta file.

// Status is the state a download is in.
//
// GO NOTE: Go has no "enum" keyword. The usual pattern is a named type plus
// a block of constants of that type.
type Status string

const (
	StatusProbing     Status = "probing"
	StatusDownloading Status = "downloading"
	StatusRetrying    Status = "retrying" // waiting before the next attempt
	StatusCompleted   Status = "completed"
	StatusCancelled   Status = "cancelled"
	StatusFailed      Status = "failed"
)

// Progress is a snapshot sent to the interface while downloading.
type Progress struct {
	Status     Status
	Downloaded int64         // bytes saved so far (including resumed bytes)
	Total      int64         // total bytes; -1 if unknown
	Speed      float64       // bytes per second (recent average)
	ETA        time.Duration // time left; -1 if unknown
	Filename   string
	Err        error // why it failed, or why we're retrying

	Resumed    bool          // true if this download continued an earlier .part
	ResumedAt  int64         // bytes already saved when it continued
	Attempt    int           // retry number when Status is StatusRetrying
	MaxRetries int           // retry limit
	RetryIn    time.Duration // wait before the next attempt

	// Multi-connection downloads only:
	Connections int            // connections in use (1 for single-connection)
	Active      int            // connections receiving data right now
	Segments    []SegmentState // each piece and its progress (for a progress map)
	Splits      int            // pieces split so idle connections could help (stage 4)
	Retrying    int            // connections waiting to reconnect after a problem
	LastProblem string         // the most recent connection problem, "" if none yet

	// HostLimit is the remembered connection limit for this server that was
	// applied (0 = none). HostLimitManual: the user set it, rather than learned.
	HostLimit       int
	HostLimitManual bool
}

// SegmentState describes one piece of a multi-connection download.
type SegmentState struct {
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	Done     int64  `json:"done"`
	Active   bool   `json:"active"`   // currently receiving data
	Retrying bool   `json:"retrying"` // waiting to reconnect after a problem
	Problem  string `json:"problem"`  // the last problem this piece's connection hit
}

// Percent returns 0-100, or -1 if the total size is unknown.
//
// GO NOTE: this is a "method": a function attached to a type.
// (p Progress) is the "receiver" - like "self" in Python.
func (p Progress) Percent() float64 {
	if p.Total <= 0 {
		return -1
	}
	return float64(p.Downloaded) / float64(p.Total) * 100
}

// Download describes one file to download.
type Download struct {
	URL      string
	Folder   string
	Filename string            // empty = use the name the server suggests
	Headers  map[string]string // extra request headers (cookies, referer...)

	// OnProgress is called with updates. It may be nil.
	// GO NOTE: functions are values in Go. This field holds a function
	// that takes a Progress and returns nothing.
	OnProgress func(Progress)

	// Connections: how many connections to use at once. Zero means 8.
	// 1 forces a single connection. Capped at 32.
	Connections int

	// StallTimeout: if no data arrives for this long, drop the connection
	// and retry. Zero means 30 seconds.
	StallTimeout time.Duration

	// Limits remembers how many connections each server allows (see
	// hostlimits.go). Optional: nil means no memory.
	Limits *HostLimits

	// MaxRetries: how many failed attempts IN A ROW before giving up.
	// The count resets whenever an attempt makes progress, so a long download
	// on a bad connection can survive any number of drops as long as it keeps
	// moving. Zero means 10.
	MaxRetries int

	// MaxRequestSize, if set, asks the server for at most this many bytes per
	// request, continuing with further requests. YouTube serves a big range
	// slowly but 10 MB ones at full speed (yt-dlp says so for each stream).
	MaxRequestSize int64

	// ResumeID identifies the download across address changes. Video
	// addresses expire after a few hours; with the same ResumeID, an
	// unfinished .part is continued even though the address is new.
	ResumeID string

	Info *FileInfo // filled in after probing

	client     *http.Client
	name       string
	downloaded atomic.Int64 // touched by several goroutines in multi-connection mode
	resumed    bool
	resumedAt  int64
	saved      *meta      // what was found on disk when resuming
	segments   []*segment // multi-connection mode only; guarded by segMu
	segMu      sync.Mutex // guards the segments list (each piece has its own lock too)
	splits     atomic.Int64
	alive      atomic.Int32 // connections (workers) still running
	// limited is set once the server refuses a connection with 429/503.
	// From then on we avoid extra requests: no more splitting.
	limited atomic.Bool
	// cooldownUntil (Unix nanoseconds): no new requests before this time.
	cooldownUntil atomic.Int64
	// Ramp-up: when a split's new connection fails to start, wait before
	// trying another (rampPauseUntil, Unix nanoseconds), longer each time.
	splitFailures  atomic.Int64
	rampPauseUntil atomic.Int64
	// activeNow / peakActive: connections receiving data right now, and the
	// most at once. After a refusal, the peak is what the server allows.
	activeNow  atomic.Int32
	peakActive atomic.Int32
	hostLimit  HostLimit // the remembered limit applied to this download, if any
	lock       *partLock // held while running, so no other download writes our .part
	problemMu  sync.Mutex
	problem    string // most recent connection problem in multi-connection mode
	// first: the reply to our very first request, whose body is the start of
	// the file. The connection that downloads from byte 0 takes it over.
	first atomic.Pointer[firstResponse]

	// disableSplitting turns off stage 4. Only the tests use it, to compare
	// the same download with and without dynamic segmentation.
	disableSplitting bool
	meter            *speedMeter
	lastReport       time.Time
	reportMu         sync.Mutex // OnProgress is never called by two goroutines at once
}

const (
	chunkSize           = 64 * 1024              // read 64 KB at a time
	reportInterval      = 100 * time.Millisecond // update the interface at most 10x per second
	defaultConnections  = 8
	maxConnectionsLimit = 32
	defaultStallTimeout = 30 * time.Second
	defaultMaxRetries   = 10
)

// retryBase is the first retry delay. It's a variable (not a constant) only
// so the tests can shrink it and run fast.
var retryBase = time.Second

// Run downloads the file and returns the path it was saved to.
// Cancel the context to stop it; the .part file is kept for resuming.
//
// GO NOTE: "(d *Download)" - the * means this method gets a POINTER to the
// Download, so it can modify its fields (like d.Info). Without the *, it
// would get a copy, and changes would be lost.
func (d *Download) Run(ctx context.Context) (savedPath string, err error) {
	if d.client == nil {
		d.client = newClient()
	}
	if d.StallTimeout == 0 {
		d.StallTimeout = defaultStallTimeout
	}
	if d.MaxRetries == 0 {
		d.MaxRetries = defaultMaxRetries
	}
	if d.Connections == 0 {
		d.Connections = defaultConnections
	}
	d.Connections = min(max(d.Connections, 1), maxConnectionsLimit)
	d.name = d.Filename
	if d.name == "" {
		d.name = "download"
	}
	d.meter = newSpeedMeter(3 * time.Second)

	// Report the outcome of every failure path in one place.
	// GO NOTE: this deferred function can see and change the named return
	// value "err" - a common Go pattern for "do X on every failure".
	defer func() {
		switch {
		case err == nil:
			return
		case ctx.Err() != nil: // the context was cancelled = the user cancelled
			d.report(Progress{Status: StatusCancelled})
			err = context.Canceled
		default:
			d.report(Progress{Status: StatusFailed, Err: err})
		}
	}()

	d.report(Progress{Status: StatusProbing})
	d.Info, err = d.probeWithRetry(ctx)
	if err != nil {
		return "", err
	}
	defer d.dropFirst() // if no connection took the first reply over, close it

	// If we remember that this server limits connections, start with that
	// many - instead of discovering it again through refused requests.
	if l, ok := d.Limits.Get(hostOf(d.Info.URL)); ok && l.MaxConnections < d.Connections {
		d.Connections = l.MaxConnections
		d.hostLimit = l
	}

	finalPath, partPath, err := d.prepare()
	if err != nil {
		return "", err
	}
	defer d.lock.release() // runs after the rename below, so the .part is never unguarded
	if d.resumed {
		d.dropFirst() // we carry on from where we were, not from byte 0
	}

	if d.useSegments() {
		err = d.runSegmented(ctx, partPath)
	} else {
		err = d.runSingle(ctx, partPath)
	}
	if err != nil {
		return "", err
	}

	if err = os.Rename(partPath, finalPath); err != nil {
		return "", &diskError{err}
	}
	os.Remove(metaPath(partPath)) // no longer needed; ignore errors
	d.report(Progress{Status: StatusCompleted, Total: d.downloaded.Load()})
	return finalPath, nil
}

// runSingle is the single-connection download with retries (stages 1-2).
func (d *Download) runSingle(ctx context.Context, partPath string) error {
	failures := 0 // failed attempts in a row
	for {
		before := d.downloaded.Load()
		err := d.transfer(ctx, partPath)
		if err == nil {
			return nil // got everything
		}
		if ctx.Err() != nil || !retryable(err) {
			return err
		}
		if errors.Is(err, ErrTooSlow) && d.Info.SupportsRanges || errors.Is(err, errChunkDone) {
			failures = 0
			continue // dropped on purpose, or the next chunk: reconnect at once
		}

		if d.downloaded.Load() > before && d.Info.SupportsRanges {
			// That attempt made progress, so the connection works.
			// (Not for servers without ranges: every retry restarts from zero
			// and re-downloads data you already had, so those keep counting.)
			failures = 0
		}
		failures++
		if failures > d.MaxRetries {
			return fmt.Errorf("gave up after %d attempts: %w", d.MaxRetries, err)
		}
		if !d.Info.SupportsRanges {
			// Without range support we can't ask for "the rest" - only the
			// whole file again. transfer() truncates the .part when it starts at 0.
			d.downloaded.Store(0)
		}

		delay := max(retryDelay(failures), retryAfter(err))
		d.report(Progress{
			Status: StatusRetrying, Err: err,
			Attempt: failures, MaxRetries: d.MaxRetries, RetryIn: delay,
		})
		if err := sleepCtx(ctx, delay); err != nil {
			return err
		}
	}
}

// hasUnfinished reports whether the folder holds an unfinished download of
// this URL (a .part.meta naming it). We can't know the file's name before
// the server answers, so we look at all of them; download folders have few.
func (d *Download) hasUnfinished() bool {
	entries, err := os.ReadDir(d.Folder)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if part, ok := strings.CutSuffix(e.Name(), ".meta"); ok && strings.HasSuffix(part, ".part") {
			if m, err := readMeta(filepath.Join(d.Folder, part)); err == nil && d.isOurs(m) {
				return true
			}
		}
	}
	return false
}

// takeFirst hands the first reply to the connection starting at byte 0 -
// once: the next caller gets nil.
func (d *Download) takeFirst() *firstResponse { return d.first.Swap(nil) }

// dropFirst closes the first reply if no connection took it.
func (d *Download) dropFirst() {
	if f := d.takeFirst(); f != nil {
		f.close()
	}
}

// sleepCtx waits for the delay, or returns early if ctx is cancelled.
//
// GO NOTE: select waits on several channels at once and runs whichever
// case is ready first. time.After(delay) is a channel that receives a value
// after the delay; ctx.Done() is closed when ctx is cancelled.
func sleepCtx(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// probeWithRetry makes the first request (see openFirst), retrying
// temporary failures (useful on bad connections, where even the first
// request can time out). Its reply, if it can be used, is kept in d.first.
func (d *Download) probeWithRetry(ctx context.Context) (*FileInfo, error) {
	// Resuming? Then we'll carry on from where we stopped, not from byte 0,
	// and a reply streaming the file from the start would be data thrown
	// away. Ask the careful one-byte question instead.
	resuming := d.hasUnfinished()
	for failures := 1; ; failures++ {
		var info *FileInfo
		var first *firstResponse
		var err error
		if resuming {
			info, err = Probe(ctx, d.client, d.URL, d.Headers)
		} else {
			info, first, err = openFirst(ctx, d.client, d.URL, d.Headers, d.MaxRequestSize)
		}
		if err == nil {
			if first != nil {
				d.first.Store(first)
			}
			return info, nil
		}
		if ctx.Err() != nil || !retryable(err) || failures >= d.MaxRetries {
			return info, err
		}
		delay := max(retryDelay(failures), retryAfter(err))
		d.report(Progress{Status: StatusRetrying, Err: err,
			Attempt: failures, MaxRetries: d.MaxRetries, RetryIn: delay})
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// retryDelay: 1s, 2s, 4s, 8s, 16s, then 30s each time.
// Waiting longer after each failure ("exponential backoff") avoids hammering
// a struggling server or a connection that's down.
func retryDelay(failures int) time.Duration {
	maxDelay := 30 * retryBase
	if failures > 6 {
		return maxDelay
	}
	delay := retryBase << (failures - 1) // << doubles: 1s, 2s, 4s, ...
	return min(delay, maxDelay)
}

// prepare picks the file names and decides whether to resume.
func (d *Download) prepare() (finalPath, partPath string, err error) {
	if d.Filename == "" {
		d.name = d.Info.Filename
	}
	if err := os.MkdirAll(d.Folder, 0o755); err != nil {
		return "", "", &diskError{err}
	}
	wanted := filepath.Join(d.Folder, d.name)

	// Is there an unfinished download of THIS file? It may be under the plain
	// name or a numbered one like "file (1).zip", so check them all.
	if found, m, done := d.findUnfinished(wanted); found != "" {
		finalPath = found
		partPath = found + ".part"
		// Take the lock first. If another download holds it, this exact file
		// is being downloaded right now: stop, rather than both writing to it.
		lock, err := acquireLock(partPath)
		if errors.Is(err, errLocked) {
			return "", "", ErrAlreadyDownloading
		}
		if err != nil {
			return "", "", err
		}
		d.lock = lock
		if done >= 0 {
			d.saved = m
			d.downloaded.Store(done)
			d.resumed = true
			d.resumedAt = done
		}
		// done -1: it's ours but can't be resumed (file changed on the server,
		// or no range support). Reuse the same name and start it over from zero.
	} else {
		// A fresh download: never overwrite an existing file or .part, and
		// never use a name another running download has locked. If another
		// download grabs the same name in the instant between choosing and
		// locking it, choose again: uniquePath will skip it now.
		for {
			finalPath = uniquePath(wanted)
			partPath = finalPath + ".part"
			lock, err := acquireLock(partPath)
			if err == nil {
				d.lock = lock
				break
			}
			if !errors.Is(err, errLocked) {
				return "", "", err
			}
		}
	}
	d.name = filepath.Base(finalPath)

	m := &meta{
		URL: d.URL, FinalURL: d.Info.URL, Size: d.Info.Size, ResumeID: d.ResumeID,
		ETag: d.Info.ETag, LastModified: d.Info.LastModified,
	}
	if d.saved != nil {
		m.Segments = d.saved.Segments
	}
	if err := writeMeta(partPath, m); err != nil {
		d.lock.release()
		return "", "", &diskError{err}
	}
	return finalPath, partPath, nil
}

// findUnfinished looks for a .part file left by an earlier download of the
// same URL, under "name.ext", "name (1).ext", "name (2).ext" and so on.
// It returns the final path that .part belongs to, its meta, and how many
// bytes are already saved - or -1 if it must start over. If several match,
// it picks the one with the most bytes. Returns "" if there's none.
func (d *Download) findUnfinished(wanted string) (found string, m *meta, done int64) {
	ext := filepath.Ext(wanted)
	stem := strings.TrimSuffix(wanted, ext)
	done = -1

	candidate := wanted
	for n := 1; exists(candidate) || exists(candidate+".part") || exists(lockPath(candidate+".part")); n++ {
		part := candidate + ".part"
		cm, err := readMeta(part)
		if err == nil && (d.isOurs(cm) || cm.FinalURL == d.Info.URL) {
			keep := d.resumableBytes(part, cm)
			if found == "" || keep > done {
				found, m, done = candidate, cm, keep
			}
		}
		candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
	}
	return found, m, done
}

// resumableBytes says how many bytes of an earlier .part can be kept,
// or -1 if none can.
func (d *Download) resumableBytes(part string, m *meta) int64 {
	stat, err := os.Stat(part)
	if err != nil || !d.Info.SupportsRanges || !m.sameFile(d.Info) {
		return -1
	}
	if len(m.Segments) > 0 {
		// Multi-connection .part: the file is full size from the start, so
		// its size says nothing. The meta says which bytes are real.
		if !validSegments(m.Segments, d.Info.Size) || stat.Size() != d.Info.Size {
			return -1
		}
		var done int64
		for _, s := range m.Segments {
			done += s.Done
		}
		return done
	}
	// Single-connection .part: everything in it is real.
	if stat.Size() > d.Info.Size {
		return -1
	}
	return stat.Size()
}

// transfer makes ONE single-connection attempt, continuing from d.downloaded.
// It returns nil only when the whole file has arrived.
func (d *Download) transfer(ctx context.Context, partPath string) error {
	start := d.downloaded.Load()

	// Stall detection. attemptCtx is a child of ctx: cancelling it stops this
	// attempt only, while cancelling ctx (the user) also stops attemptCtx.
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()

	// GO NOTE: time.AfterFunc runs a function in a separate GOROUTINE after a
	// delay. A goroutine is a lightweight thread - Go can run thousands at once.
	// Because two goroutines touch "stalled", it's an atomic.Bool, which is
	// safe to read and write from several goroutines at the same time.
	var stalled atomic.Bool
	watchdog := time.AfterFunc(d.StallTimeout, func() {
		stalled.Store(true)
		cancelAttempt() // makes the blocked Read below return an error
	})
	defer watchdog.Stop()

	reqEnd := int64(-1) // to the end
	if d.MaxRequestSize > 0 && d.Info.SupportsRanges && d.Info.Size > 0 {
		reqEnd = min(start+d.MaxRequestSize, d.Info.Size) - 1
	}
	resp, err := d.startResponse(attemptCtx, start, reqEnd)
	if err != nil {
		return d.attemptErr(err, &stalled, start)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		// 206: we got the rest. (Content-Range should start where we asked.)
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", start)) {
			return fmt.Errorf("server sent the wrong part: %s", resp.Header.Get("Content-Range"))
		}
	case resp.StatusCode == http.StatusOK:
		// 200: the whole file from byte 0. Either we asked for that, or the
		// file changed / the server ignored our range. Start over.
		start = 0
		d.downloaded.Store(0)
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && start == d.Info.Size:
		// 416 when asking from the very end: we already have every byte.
		return nil
	default:
		return newHTTPError(resp)
	}

	file, err := openPart(partPath, start)
	if err != nil {
		return err
	}
	defer file.Close()

	slow := newSlowDetector()      // restarts a connection that has become very slow
	buf := make([]byte, chunkSize) // GO NOTE: make() creates a slice (resizable array) of 64 KB
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(d.StallTimeout) // data arrived: restart the stall timer
			if _, err := file.Write(buf[:n]); err != nil {
				return &diskError{err}
			}
			d.downloaded.Add(int64(n))
			GiveWay(ctx, n) // a video look-up is running: let it have the line
			d.reportProgress(1, 1, nil)
		}
		if readErr == io.EOF {
			break // the server finished sending
		}
		// Checked after saving the bytes, so none are thrown away. Not without
		// range support: there a restart would lose everything.
		if d.Info.SupportsRanges && slow.tooSlow(n) {
			return ErrTooSlow // runSingle reconnects (and resumes) at once
		}
		if readErr != nil {
			return d.attemptErr(readErr, &stalled, d.downloaded.Load())
		}
	}

	if err := file.Close(); err != nil {
		return &diskError{err}
	}
	// A connection can end early without an error. If we know the size,
	// make sure we got all of it.
	got := d.downloaded.Load()
	if reqEnd >= 0 && got == reqEnd+1 && got < d.Info.Size {
		return errChunkDone // that request's share is done; runSingle asks for the next
	}
	if d.Info.Size >= 0 && got != d.Info.Size {
		return fmt.Errorf("%w: got %d of %d bytes", ErrIncomplete, got, d.Info.Size)
	}
	return nil
}

// errChunkDone: a request limited by MaxRequestSize finished its share.
var errChunkDone = errors.New("chunk done")

// isOurs reports whether a saved .part.meta belongs to this download.
func (d *Download) isOurs(m *meta) bool {
	if d.ResumeID != "" && m.ResumeID == d.ResumeID {
		return true
	}
	return m.URL == d.URL
}

// newRequest builds a GET for the file. start > 0 asks for bytes from start;
// end >= 0 also sets the last byte wanted (inclusive), for one segment.
func (d *Download) newRequest(ctx context.Context, start, end int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Info.URL, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, d.Headers)
	if start > 0 || end >= 0 {
		// "bytes=N-" means from N to the end; "bytes=N-M" means N to M.
		rng := fmt.Sprintf("bytes=%d-", start)
		if end >= 0 {
			rng += fmt.Sprint(end)
		}
		req.Header.Set("Range", rng)
		// If-Range: "send the range only if the file is still this version,
		// otherwise send the whole new file." A safety check on top of the
		// meta comparison. Weak ETags (W/"...") aren't allowed here.
		if d.Info.ETag != "" && !strings.HasPrefix(d.Info.ETag, "W/") {
			req.Header.Set("If-Range", d.Info.ETag)
		} else if d.Info.LastModified != "" {
			req.Header.Set("If-Range", d.Info.LastModified)
		}
	}
	return req, nil
}

// startResponse gets the reply for bytes start..end (end -1 = to the end).
// From byte 0 it takes over the first reply if it's still unused - no new
// request needed. Otherwise it sends a new request.
func (d *Download) startResponse(attemptCtx context.Context, start, end int64) (*http.Response, error) {
	if start == 0 {
		if f := d.takeFirst(); f != nil {
			// That reply's request was made before this attempt existed, so
			// tie them together: when the attempt is cancelled (stall, user
			// cancel, another connection failing), so is the reply.
			context.AfterFunc(attemptCtx, f.cancel)
			return f.resp, nil
		}
	}
	req, err := d.newRequest(attemptCtx, start, end)
	if err != nil {
		return nil, err
	}
	return d.client.Do(req)
}

// attemptErr turns low-level errors into clearer ones.
func (d *Download) attemptErr(err error, stalled *atomic.Bool, got int64) error {
	if stalled.Load() {
		return fmt.Errorf("%w for %v", ErrStalled, d.StallTimeout)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: connection closed at byte %d", ErrIncomplete, got)
	}
	return err
}

// openPart opens the .part file for writing at byte position "start".
// Anything after "start" is cut off, so a half-written chunk from a
// previous crash can't end up in the middle of the file.
func openPart(partPath string, start int64) (*os.File, error) {
	// GO NOTE: OpenFile takes "flags" combined with | (bitwise OR):
	// O_WRONLY = open for writing, O_CREATE = create it if missing.
	file, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, &diskError{err}
	}
	if err := file.Truncate(start); err != nil {
		file.Close()
		return nil, &diskError{err}
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		file.Close()
		return nil, &diskError{err}
	}
	return file, nil
}

// reportProgress sends a "downloading" update, at most 10 times a second.
func (d *Download) reportProgress(connections, active int, segs []SegmentState) {
	retrying := 0
	for _, s := range segs {
		if s.Retrying {
			retrying++
		}
	}
	d.reportMu.Lock()
	speed := d.meter.update(d.downloaded.Load())
	due := time.Since(d.lastReport) >= reportInterval
	if due {
		d.lastReport = time.Now()
	}
	d.reportMu.Unlock()
	if due {
		d.report(Progress{Status: StatusDownloading, Speed: speed,
			Connections: connections, Active: active, Segments: segs,
			Retrying: retrying, LastProblem: d.lastProblem()})
	}
}

// report fills in the common fields and calls OnProgress.
func (d *Download) report(p Progress) {
	if d.OnProgress == nil {
		return
	}
	p.Filename = d.name
	p.Downloaded = d.downloaded.Load()
	if p.Total == 0 {
		p.Total = d.total()
	}
	p.Resumed = d.resumed
	p.ResumedAt = d.resumedAt
	p.Splits = int(d.splits.Load())
	p.HostLimit = d.hostLimit.MaxConnections
	p.HostLimitManual = d.hostLimit.Manual
	p.ETA = -1
	if p.Total > 0 && p.Speed > 0 {
		p.ETA = time.Duration(float64(p.Total-p.Downloaded) / p.Speed * float64(time.Second))
	}
	d.reportMu.Lock()
	defer d.reportMu.Unlock()
	d.OnProgress(p)
}

// noteProblem records a connection problem so the interface can show it.
func (d *Download) noteProblem(msg string) {
	d.problemMu.Lock()
	d.problem = msg
	d.problemMu.Unlock()
}

func (d *Download) lastProblem() string {
	d.problemMu.Lock()
	defer d.problemMu.Unlock()
	return d.problem
}

func (d *Download) total() int64 {
	if d.Info == nil {
		return -1
	}
	return d.Info.Size
}

// uniquePath never overwrites: "file.zip" -> "file (1).zip" -> "file (2).zip".
// It also skips names whose .part or .part.lock exists (another download owns it).
func uniquePath(p string) string {
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	candidate := p
	for n := 1; exists(candidate) || exists(candidate+".part") || exists(lockPath(candidate+".part")); n++ {
		candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
	}
	return candidate
}

// hostOf returns the server name in a URL ("proof.ovh.net").
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
