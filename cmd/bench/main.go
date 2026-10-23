// bench is a local test server for comparing download managers.
//
// It serves made-up files on this computer only (127.0.0.1), so testing
// uses no internet data. Every request is written to a log, so we can see
// exactly how IDM, FDM and dlmanager behave: how many connections they open
// and when, which byte ranges they ask for, how they react when the server
// refuses, slows down, or cuts them off.
//
//	bench -log .dev\bench\log.jsonl          run the server
//	bench -report .dev\bench\log.jsonl       summarise what each client did
//
// The files are generated from their byte positions, so nothing is stored
// and every download can be checked against the SHA-256 shown on the index
// page.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- Settings ---------------------------------------------------------------

var (
	addr     = flag.String("addr", "127.0.0.1:8090", "address to listen on (keep it 127.0.0.1)")
	sizeMB   = flag.Int("size", 40, "file size in MB")
	perConn  = flag.Int("rate", 500, "speed limit per connection, KB/s (0 = none)")
	totalKB  = flag.Int("total", 2000, "speed limit for all connections together, KB/s, like a real internet line (0 = none)")
	logPath  = flag.String("log", "", "write the request log here (JSON lines)")
	report   = flag.String("report", "", "summarise this log instead of running the server")
	last     = flag.Int("last", 0, "with -report: only the last N runs (0 = all)")
	runGapS  = flag.Int("gap", 15, "with -report: idle seconds that separate two test runs")
	fileSize int64
)

// The test situations. Each has its own address, so you pick one by link.
var scenarios = []struct{ name, about string }{
	{"normal", "an ordinary server: ranges allowed, no connection limit"},
	{"limit3", "allows 3 requests at once and answers the rest with 429 Too Many Requests (like proof.ovh.net)"},
	{"slowdown", "the first connection to receive 1 MB slows to 25 KB/s (a connection gone bad)"},
	{"drops", "every connection is cut off after 2 MB (an unreliable line)"},
	{"norange", "ignores Range requests: always sends the whole file, so it can't be resumed or split"},
}

const (
	limit3Max      = 3
	slowdownAfter  = 1 << 20
	slowdownRateKB = 25
	dropAfter      = 2 << 20
	etag           = `"bench-v1"`
)

var lastModified = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat)

func main() {
	flag.Parse()
	fileSize = int64(*sizeMB) << 20
	if *report != "" {
		if err := printReport(*report, time.Duration(*runGapS)*time.Second); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := serve(); err != nil {
		log.Fatal(err)
	}
}

// ---- The file's content ---------------------------------------------------------

// fill puts the file's bytes from position off into buf. Every 8-byte block
// is a hash of its position (splitmix64), so the content looks random,
// never repeats, and needs no storage.
func fill(buf []byte, off int64) {
	var block [8]byte
	for i := 0; i < len(buf); {
		pos := off + int64(i)
		binary.LittleEndian.PutUint64(block[:], splitmix(uint64(pos/8)))
		n := copy(buf[i:], block[pos%8:])
		i += n
	}
}

func splitmix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func fileHash() string {
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for off := int64(0); off < fileSize; off += int64(len(buf)) {
		n := min(int64(len(buf)), fileSize-off)
		fill(buf[:n], off)
		h.Write(buf[:n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- Speed limits ---------------------------------------------------------------

// limiter hands out bytes at a fixed rate. One limiter is shared by every
// connection (the "internet line"); each request also has its own.
type limiter struct {
	mu   sync.Mutex
	rate float64 // bytes per second; 0 = unlimited
	next time.Time
}

func newLimiter(kbps int) *limiter { return &limiter{rate: float64(kbps) * 1024} }

// reserve books n bytes and returns when they may be sent.
func (l *limiter) reserve(n int) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.rate == 0 {
		return now
	}
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	return l.next
}

// waitBoth waits until n bytes are allowed by BOTH limiters. Booking both
// first and then waiting for the later time gives the slower of the two
// rates; waiting for one and then the other would add the delays up.
func waitBoth(ctx context.Context, a, b *limiter, n int) error {
	until := a.reserve(n)
	if t := b.reserve(n); t.After(until) {
		until = t
	}
	wait := time.Until(until)
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *limiter) setRate(kbps int) {
	l.mu.Lock()
	l.rate = float64(kbps) * 1024
	l.mu.Unlock()
}

// ---- The log ------------------------------------------------------------------

// entry is one line of the log: a request, or a TCP connection opening or
// closing. Times are milliseconds since the server started.
type entry struct {
	Ev       string `json:"ev"` // "req", "conn-open", "conn-close"
	Conn     int64  `json:"conn"`
	Start    int64  `json:"start"`
	End      int64  `json:"end,omitempty"`
	Scenario string `json:"scenario,omitempty"`
	Method   string `json:"method,omitempty"`
	Range    string `json:"range,omitempty"`
	IfRange  string `json:"ifRange,omitempty"`
	UA       string `json:"ua,omitempty"`
	Status   int    `json:"status,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Result   string `json:"result,omitempty"` // done, client-closed, cut, refused
	Size     int64  `json:"size,omitempty"`   // the file's size
	Slowed   bool   `json:"slowed,omitempty"` // the slowdown scenario slowed this one
}

type logger struct {
	mu    sync.Mutex
	w     *bufio.Writer
	f     *os.File
	start time.Time
}

func (l *logger) ms(t time.Time) int64 { return t.Sub(l.start).Milliseconds() }

func (l *logger) write(e entry) {
	if l.f == nil {
		return
	}
	b, _ := json.Marshal(e)
	l.mu.Lock()
	l.w.Write(append(b, '\n'))
	l.w.Flush() // every line straight to disk, so the log can be read while running
	l.mu.Unlock()
}

// ---- The server ------------------------------------------------------------------

type connIDKey struct{}

type server struct {
	log      *logger
	total    *limiter
	hash     string
	connIDs  atomic.Int64
	inflight sync.Map // scenario -> *atomic.Int32 (requests being served now)
	// slowdown: has a connection been slowed in this run yet? Reset when the
	// scenario has no requests left (the run is over).
	slowUsed atomic.Bool
}

func serve() error {
	s := &server{log: &logger{start: time.Now()}, total: newLimiter(*totalKB)}
	if *logPath != "" {
		if err := os.MkdirAll(filepath.Dir(*logPath), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		s.log.f, s.log.w = f, bufio.NewWriter(f)
	}
	fmt.Printf("Working out the file's SHA-256 (%d MB)...\n", *sizeMB)
	s.hash = fileHash()

	// Connection IDs let the log show whether a client reuses a TCP
	// connection for several requests (keep-alive) or opens a new one each time.
	conns := sync.Map{} // net.Conn -> id
	srv := &http.Server{
		Addr:    *addr,
		Handler: s,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			id := s.connIDs.Add(1)
			conns.Store(c, id)
			return context.WithValue(ctx, connIDKey{}, id)
		},
		ConnState: func(c net.Conn, st http.ConnState) {
			v, ok := conns.Load(c)
			if !ok {
				return
			}
			switch st {
			case http.StateNew:
				s.log.write(entry{Ev: "conn-open", Conn: v.(int64), Start: s.log.ms(time.Now())})
			case http.StateClosed, http.StateHijacked:
				conns.Delete(c)
				s.log.write(entry{Ev: "conn-close", Conn: v.(int64), Start: s.log.ms(time.Now())})
			}
		},
	}
	fmt.Printf("Serving on http://%s/  (open it in a browser for the test links)\n", *addr)
	fmt.Printf("File: %d MB, SHA-256 %s\n", *sizeMB, s.hash)
	fmt.Printf("Speed: %s per connection, %s in total\n", kb(*perConn), kb(*totalKB))
	return srv.ListenAndServe()
}

func kb(n int) string {
	if n == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d KB/s", n)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.URL.Path == "/" {
		s.index(w)
		return
	}
	if len(parts) != 2 || !knownScenario(parts[0]) {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, parts[0])
}

func knownScenario(name string) bool {
	for _, sc := range scenarios {
		if sc.name == name {
			return true
		}
	}
	return false
}

func fileName(scenario string) string {
	return fmt.Sprintf("bench-%s-%dMB.bin", scenario, *sizeMB)
}

func (s *server) index(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Download bench</title>
<style>body{font:15px system-ui;max-width:760px;margin:40px auto;padding:0 16px;line-height:1.5}
code{background:#eee;padding:2px 5px;border-radius:4px;word-break:break-all}li{margin:10px 0}</style>
<h1>Download bench</h1>
<p>Click a link to download it with the browser (or whatever catches browser downloads), or copy it into a download manager. Everything stays on this computer.</p>
<ul>`)
	for _, sc := range scenarios {
		url := fmt.Sprintf("http://%s/%s/%s", *addr, sc.name, fileName(sc.name))
		fmt.Fprintf(w, `<li><b>%s</b> - %s<br><a href="%s"><code>%s</code></a></li>`, sc.name, sc.about, url, url)
	}
	fmt.Fprintf(w, `</ul><p>Each file: %d MB. Speed: %s per connection, %s in total.</p>
<p>A correct download has SHA-256<br><code>%s</code></p>`, *sizeMB, kb(*perConn), kb(*totalKB), s.hash)
}

// serveFile answers a request for a test file, following the scenario's rules.
func (s *server) serveFile(w http.ResponseWriter, r *http.Request, scenario string) {
	start := time.Now()
	e := entry{
		Ev: "req", Start: s.log.ms(start), Scenario: scenario, Method: r.Method,
		Range: r.Header.Get("Range"), IfRange: r.Header.Get("If-Range"), UA: r.UserAgent(),
		Size: fileSize,
	}
	e.Conn, _ = r.Context().Value(connIDKey{}).(int64)
	defer func() {
		e.End = s.log.ms(time.Now())
		s.log.write(e)
	}()

	counter, _ := s.inflight.LoadOrStore(scenario, new(atomic.Int32))
	now := counter.(*atomic.Int32).Add(1)
	defer func() {
		if counter.(*atomic.Int32).Add(-1) == 0 && scenario == "slowdown" {
			s.slowUsed.Store(false)
		}
	}()

	if scenario == "limit3" && now > limit3Max {
		e.Status, e.Result = http.StatusTooManyRequests, "refused"
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", etag)
	h.Set("Last-Modified", lastModified)

	from, to := int64(0), fileSize-1
	status := http.StatusOK
	if scenario != "norange" {
		h.Set("Accept-Ranges", "bytes")
		if rg := r.Header.Get("Range"); rg != "" && ifRangeMatches(r.Header.Get("If-Range")) {
			f, t, ok := parseRange(rg, fileSize)
			if !ok {
				h.Set("Content-Range", fmt.Sprintf("bytes */%d", fileSize))
				e.Status, e.Result = http.StatusRequestedRangeNotSatisfiable, "done"
				w.WriteHeader(e.Status)
				return
			}
			from, to, status = f, t, http.StatusPartialContent
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, fileSize))
		}
	}
	h.Set("Content-Length", strconv.FormatInt(to-from+1, 10))
	e.Status = status
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		e.Result = "done"
		return
	}

	own := newLimiter(*perConn)
	buf := make([]byte, 16<<10)
	flusher, _ := w.(http.Flusher)
	for pos := from; pos <= to; {
		// The FIRST connection of a run to send 1 MB turns slow. (Not "the one
		// starting at byte 0": IDM's first request reads only ~240 KB before
		// it hands that area to another connection.)
		if scenario == "slowdown" && e.Bytes >= slowdownAfter && s.slowUsed.CompareAndSwap(false, true) {
			own.setRate(slowdownRateKB)
			e.Slowed = true
		}
		if scenario == "drops" && e.Bytes >= dropAfter {
			e.Result = "cut"
			// Panicking with ErrAbortHandler makes the server drop the
			// connection mid-reply, without logging a crash.
			panic(http.ErrAbortHandler)
		}
		n := min(int64(len(buf)), to-pos+1)
		if err := waitBoth(r.Context(), own, s.total, int(n)); err != nil {
			e.Result = "client-closed"
			return
		}
		fill(buf[:n], pos)
		if _, err := w.Write(buf[:n]); err != nil {
			e.Result = "client-closed"
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		pos += n
		e.Bytes += n
	}
	e.Result = "done"
}

// ifRangeMatches: a Range request with If-Range only gets the range if the
// file hasn't changed. (It never changes here; this checks the client sends
// something sensible.)
func ifRangeMatches(v string) bool {
	return v == "" || v == etag || v == lastModified
}

// parseRange understands "bytes=A-B", "bytes=A-" and "bytes=-N" (the last N
// bytes). Only the first range is used.
func parseRange(h string, size int64) (from, to int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found {
		return 0, 0, false
	}
	spec, _, _ = strings.Cut(spec, ",")
	a, b, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false
	}
	if a == "" { // suffix: the last N bytes
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(0, size-n), size - 1, true
	}
	from, err := strconv.ParseInt(a, 10, 64)
	if err != nil || from >= size {
		return 0, 0, false
	}
	to = size - 1
	if b != "" {
		if to, err = strconv.ParseInt(b, 10, 64); err != nil || to < from {
			return 0, 0, false
		}
		to = min(to, size-1)
	}
	return from, to, true
}

// ---- The report -----------------------------------------------------------------

// A run is one download by one client: the requests for one scenario from
// one User-Agent, until it goes quiet for a while.
type run struct {
	scenario, ua string
	reqs         []entry
	lastEnd      int64
}

func printReport(path string, gap time.Duration) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// The log is appended to across server restarts, and each restart resets
	// the clock and the connection numbers. A session starts with the first
	// connection (number 1). Requests are logged when they END, so within a
	// session they're sorted by start time before being grouped.
	var sessions [][]entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if (e.Ev == "conn-open" && e.Conn == 1) || len(sessions) == 0 {
			sessions = append(sessions, nil)
		}
		if e.Ev == "req" {
			sessions[len(sessions)-1] = append(sessions[len(sessions)-1], e)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	var runs []*run
	for _, reqs := range sessions {
		sort.Slice(reqs, func(i, j int) bool { return reqs[i].Start < reqs[j].Start })
		open := map[string]*run{}
		for _, e := range reqs {
			key := e.Scenario + "\x00" + e.UA
			r := open[key]
			if r == nil || e.Start-r.lastEnd > gap.Milliseconds() {
				r = &run{scenario: e.Scenario, ua: e.UA}
				open[key] = r
				runs = append(runs, r)
			}
			r.reqs = append(r.reqs, e)
			r.lastEnd = max(r.lastEnd, e.End)
		}
	}
	if len(runs) == 0 {
		fmt.Println("No downloads in the log yet.")
		return nil
	}
	from := 0
	if *last > 0 && len(runs) > *last {
		from = len(runs) - *last
	}
	for i := from; i < len(runs); i++ {
		describe(i+1, runs[i])
	}
	return nil
}

func describe(n int, r *run) {
	first := r.reqs[0].Start
	fileSize := r.reqs[0].Size
	var total int64
	conns := map[int64]bool{}
	refused, cut := 0, 0
	for _, e := range r.reqs {
		total += e.Bytes
		conns[e.Conn] = true
		switch e.Result {
		case "refused":
			refused++
		case "cut":
			cut++
		}
	}
	took := time.Duration(r.lastEnd-first) * time.Millisecond

	fmt.Printf("\n=== Run %d: %s, client %q\n", n, r.scenario, shortUA(r.ua))
	fmt.Printf("time %v, %d requests over %d TCP connections, %d refused, %d cut off\n",
		took.Round(100*time.Millisecond), len(r.reqs), len(conns), refused, cut)
	fmt.Printf("bytes sent %s for a %s file", mb(total), mb(fileSize))
	if total > fileSize {
		fmt.Printf(" (%s extra)", mb(total-fileSize))
	}
	if took > 0 {
		fmt.Printf(", average %s/s", mb(int64(float64(min(total, fileSize))/took.Seconds())))
	}
	fmt.Println()
	fmt.Printf("most data requests at once: %d\n", peakConcurrent(r.reqs))

	if r.scenario == "slowdown" {
		fmt.Println("(* = the connection the server slowed to a crawl)")
	}
	fmt.Println("   start  conn  method range                        status result          sent    took")
	for i, e := range r.reqs {
		if i == 60 {
			fmt.Printf("   ... %d more\n", len(r.reqs)-60)
			break
		}
		rg := e.Range
		if rg == "" {
			rg = "(whole file)"
		}
		if e.IfRange != "" {
			rg += " +If-Range"
		}
		res := e.Result
		if e.Slowed {
			res += "*"
		}
		fmt.Printf("%7.2fs %5d  %-6s %-28s %6d %-14s %8s %6.1fs\n",
			float64(e.Start-first)/1000, e.Conn, e.Method, rg, e.Status, res, mb(e.Bytes),
			float64(e.End-e.Start)/1000)
	}
}

// peakConcurrent is the most GET requests that were being answered with data
// at the same moment.
func peakConcurrent(reqs []entry) int {
	type edge struct {
		t     int64
		delta int
	}
	var edges []edge
	for _, e := range reqs {
		if e.Method == http.MethodGet && e.Result != "refused" && e.Bytes > 1 {
			edges = append(edges, edge{e.Start, +1}, edge{e.End, -1})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].t != edges[j].t {
			return edges[i].t < edges[j].t
		}
		return edges[i].delta < edges[j].delta // ends before starts at the same moment
	})
	cur, peak := 0, 0
	for _, e := range edges {
		cur += e.delta
		peak = max(peak, cur)
	}
	return peak
}

func mb(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func shortUA(ua string) string {
	if ua == "" {
		return "(none)"
	}
	if len(ua) > 70 {
		return ua[:70] + "..."
	}
	return ua
}
