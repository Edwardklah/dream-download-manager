# Go lesson 3: running connections in parallel

Stage 3 downloads one file over several connections at once. This lesson
explains how it works and the Go tools it uses: WaitGroups, mutexes, closing
channels, `WriteAt`, and why HTTP/2 had to be switched off.

Read it alongside `engine/segmented.go`.

## Part A: how a multi-connection download works

### 1. Deciding whether to split

`useSegments()` in `segmented.go` decides. A file is split when:

- the server supports ranges (the probe got a `206` reply),
- the size is known, and
- the file is at least 1 MB (twice the 512 KB minimum piece size).

Otherwise the single-connection code from stages 1-2 runs, unchanged. Small
files aren't worth splitting: each piece costs a full round trip to the server
before any data flows, and on a high-latency connection that overhead would
eat the gain.

### 2. Splitting

`buildSegments()` divides the file into pieces. With 8 connections, a 100 MB
file becomes 8 pieces of 12.5 MB:

```
piece 1: bytes        0 - 13,107,199
piece 2: bytes 13,107,200 - 26,214,399
...
piece 8: bytes 91,750,400 - 104,857,599   (the last piece also takes any leftover bytes)
```

Each piece is requested with its own header, for example
`Range: bytes=13107200-26214399`. The server sends only that slice.

No piece is made smaller than 512 KB, so a 3 MB file gets 6 pieces, not 8.

### 3. Writing pieces into one file

The `.part` file is created at the **full size** before downloading starts.
Each connection writes its bytes directly to their final position using
`WriteAt`. Nothing needs joining at the end: when the last piece finishes, the
`.part` is already the complete file, and it's simply renamed.

Before the file is enlarged, the list of pieces is saved to the `.meta`
file. The order matters. A full-size `.part` with no piece list would look like
a finished single-connection download. If the PC crashed at exactly that
moment, the next run would accept a file full of zeros as complete.

### 4. Remembering progress

With one connection, the `.part` file's size *is* the progress. With several,
it isn't: the file is full size from the start, with gaps still to be filled.
So the `.meta` file records each piece and how much of it is done:

```json
"segments": [
  { "start": 0,      "end": 655360,  "done": 655360 },
  { "start": 655360, "end": 1310720, "done": 412000 },
  ...
]
```

It's saved every second while downloading, and once more when the download
stops for any reason. To resume, each piece continues from `start + done`.

Two protections against damaged meta files:

- `writeMeta` writes to a temporary file, then renames it over the old one. A
  rename is all-or-nothing, so a crash mid-save leaves the old version intact
  rather than half a file.
- `validSegments` rejects a piece list with gaps, overlaps or impossible values.
  If the list can't be trusted, the download starts over instead of producing a
  file with holes in it.

A `.part` left by a **single-connection** download can be continued with
several connections: the bytes already saved become one finished piece, and the
remainder is split up.

### 5. One bad connection doesn't stop the others

Each piece has its own retry loop and stall watchdog, with the same rules as
stage 2. If piece 3's connection drops, piece 3 waits and reconnects while the
other seven carry on. The whole download only fails on an error that retrying
can't fix, such as a full disk, or the file changing on the server mid-download.

Some servers only allow a few connections per user and answer the rest with
`503 Service Unavailable`. Those pieces retry with backoff and get their turn
when a slot frees up. `TestServerConnectionLimit` checks this against a fake
server that allows only 3.

## Part B: the Go tools

### 1. `sync.WaitGroup`: waiting for goroutines to finish

Starting goroutines is easy. Knowing when they've **all** finished needs a
counter, and that's what a WaitGroup is:

```go
var wg sync.WaitGroup
for i := 0; i < workers; i++ {
    wg.Add(1)            // one more goroutine running
    go func() {
        defer wg.Done()  // one fewer, when this goroutine ends
        // ... download pieces ...
    }()
}
wg.Wait()                // blocks until the count is back to zero
```

Always call `Add` **before** `go`, not inside the goroutine. Otherwise `Wait`
might run before the goroutine has had a chance to add itself, see a count of
zero, and return too early.

### 2. Workers and a shared queue

There isn't one goroutine per piece. There are `workers` goroutines
(the number of connections), and each repeatedly claims the next unfinished
piece:

```go
for {
    seg := d.claimSegment()
    if seg == nil {
        return       // nothing left to do
    }
    d.runSegment(workCtx, file, seg)
}
```

This is called a **worker pool**. It matters when resuming: a download stopped
with 8 pieces, resumed with `-c 2`, has 2 workers sharing 8 pieces. In stage 4,
a worker that runs out of pieces will help with someone else's instead of
stopping.

### 3. `sync.Mutex`: one goroutine at a time

Each piece is read and written by several goroutines: its worker, the progress
reporter, the meta saver. Lesson 2 used `atomic` for a single flag. A piece has
several related fields (`start`, `end`, `done`, `taken`, `active`) that must
change together, so it uses a **mutex**, a lock:

```go
func (s *segment) advance(n int64) {
    s.mu.Lock()     // wait until no other goroutine holds the lock, then take it
    s.done += n
    s.mu.Unlock()   // let others in
}
```

The rule: **every** access to those fields goes through the lock, both reads and
writes. That's why the fields are lowercase (private) and only accessed through
small methods like `position()`, `advance()` and `state()`. Nothing outside can
forget to lock.

`claimSegment` shows why the lock must cover check-and-change together:

```go
s.mu.Lock()
free := !s.taken && s.start+s.done < s.end
if free {
    s.taken = true
}
s.mu.Unlock()
```

Without the lock, two workers could both see `taken == false` at the same
moment, both set it to `true`, and download the same piece twice.

`defer s.mu.Unlock()` right after `Lock()` is the common style for methods that
return a value: the lock is released however the method returns.

### 4. Closing a channel as a broadcast signal

The progress reporter runs in its own goroutine, ticking 10 times a second. It
needs to be told when to stop:

```go
stop := make(chan struct{})   // a channel that carries no data, only signals

go func() {
    for {
        select {
        case <-stop:          // stop was closed: finish
            return
        case <-tick.C:        // a tick: report progress
            ...
        }
    }
}()

wg.Wait()     // all connections finished
close(stop)   // tell the reporter
```

Receiving from a **closed** channel never blocks; it returns immediately. So
`close` acts as a signal that every goroutine waiting on the channel sees at
once. `struct{}` is an empty type that takes no memory, used when only the
signal matters.

`reporterDone` works the other way round: the reporter closes it when it exits,
and `<-reporterDone` waits for that. This guarantees the reporter has fully
stopped before the final meta save.

### 5. Stopping everyone on a fatal error

```go
workCtx, stopWork := context.WithCancel(ctx)

fail := func(err error) {
    errMu.Lock()
    if firstErr == nil {
        firstErr = err   // keep only the first error; the rest are consequences
    }
    errMu.Unlock()
    stopWork()           // cancel workCtx: every other connection stops
}
```

`fail` is a **closure**: a function that uses variables from the surrounding
code (`firstErr`, `errMu`, `stopWork`). All the workers share those variables,
so `firstErr` is protected by its own mutex.

### 6. `WriteAt`: writing to a position

A normal `file.Write` writes at the file's current position and moves it
forward, like a cursor. If eight goroutines shared one cursor, they'd fight
over it, each moving it before another writes.

`file.WriteAt(data, offset)` writes at an exact offset and doesn't use or move
the cursor. Eight goroutines can call it at the same time on the same file, as
long as their byte ranges don't overlap, and ours never do.

### 7. `min` and `max`

Go 1.21 added built-in `min` and `max` functions:

```go
d.Connections = min(max(d.Connections, 1), maxConnections)   // clamp to 1..32
take := min(int64(n), end-pos)                                // don't write past the piece
```

## Part C: why HTTP/2 is switched off

HTTP/2, the newer version of HTTP, sends many requests over **one** TCP
connection, interleaved. For web pages that's faster: dozens of small files
share one connection instead of opening dozens.

For us it would be a trap. Go uses HTTP/2 automatically when the server
supports it, so our "8 connections" would quietly become 8 requests through a
single connection, with a single connection's speed. Everything would look
right in the code and the tests, and real downloads would be no faster.

So `client.go` forces HTTP/1.1, where each request in flight gets its own TCP
connection:

```go
ForceAttemptHTTP2: false,
TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
```

The empty `TLSNextProto` map is Go's documented way to switch HTTP/2 off.
IDM, aria2 and other download managers do the same thing for the same reason.

## Part D: testing parallel code

Parallel code fails in ways that depend on timing, so a test that passes once
proves little. Stage 3's tests were run 20 times in a row with the race
detector before being accepted, and that caught real problems:

- The local test server was so fast that a 4 MB download finished before the
  first progress report, so tests saw no progress at all. The fake server gained
  a `chunkDelay` option to simulate a slow connection, and the engine now
  reports the piece layout immediately at the start. That also helps the future
  interface, which can draw the pieces before any data arrives.
- A test changed the server's ETag from one goroutine while the server read it
  from another: a data race in the test itself, now guarded by a mutex.

`go test -count=20 ./engine` runs everything 20 times. Use it whenever you
change concurrent code.

## Exercises

1. Run a download with `-c 1`, then the same file with `-c 8`, and compare the
   speed and time. (I'll run this comparison on your connection too.)
2. Start a multi-connection download with `-stop-after 5s`, then open the
   `.part.meta` file in the download folder with Notepad. Find a piece whose
   `done` is less than `end - start`. How many bytes does it still need?
3. In `claimSegment`, what would go wrong if `s.mu.Lock()` and `s.mu.Unlock()`
   were removed? Describe the sequence of events with two workers.
4. Why does `buildSegments` give the leftover bytes to the **last** piece? What
   would happen to a 1,000,003-byte file split into 2 pieces of
   `1000003 / 2 = 500001` bytes each if it didn't?
