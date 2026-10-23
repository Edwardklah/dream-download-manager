# Go lesson 2: goroutines, select, error types and tests

Stage 2 adds resume, automatic retry and stall detection. To do that, it uses
Go's most important feature for this project: running things at the same time.
This lesson covers everything new in stage 2. Read it alongside
`engine/download.go`, `engine/errors.go`, `engine/meta.go` and
`engine/download_test.go`.

## Part A: how stage 2 works

### Resuming

When a download stops, two files are left in your folder:

- `name.part`: the bytes received so far.
- `name.part.meta`: a small JSON file saying which file those bytes belong to:
  the URL, the size, and the server's **ETag** and **Last-Modified** values.

Next time you download the same URL, `prepare()` finds them and checks that the
server still has the same file. The ETag is the server's fingerprint for one
version of a file; if the file is replaced, the ETag changes. If everything
matches, the engine asks for the rest:

```
Range: bytes=1572864-        (from byte 1,572,864 to the end)
If-Range: "v1"               (only if the file is still version "v1")
```

`If-Range` is a second safety check, done by the server. If the file changed in
the meantime, the server ignores the Range and sends the whole new file with
`200 OK`, and the engine starts over from zero. Either way, old and new bytes are
never mixed.

Why this matters: without these checks, a resume after the file was updated
would give you the first half of version 1 joined to the second half of version 2.
The result has the right size, looks complete, and is corrupted.

### Retrying

`Run` now contains a loop. Each pass is one connection attempt (`transfer`).
When an attempt fails, `retryable()` in `errors.go` decides whether trying again
could help:

| Problem | Retry? | Why |
|---|---|---|
| Connection dropped, timed out, froze | Yes | Networks recover |
| 500-599 server errors, 408, 429 | Yes | Temporary by definition |
| 404 Not Found, 403 Forbidden | No | Asking again gives the same answer |
| Disk full, no permission | No | Our problem, not the network's |

Between attempts it waits 1s, 2s, 4s, 8s, 16s, then 30s. This is called
**exponential backoff**. Retrying instantly in a loop would hammer a server
that's struggling and waste your data on a connection that's down.

The retry counter resets every time an attempt makes progress. On a bad
connection, a large file might drop 50 times but keep moving, and it will finish.
It only gives up after 10 failures **in a row** with no progress.

Servers without range support are the exception: every retry has to download the
whole file again, so the counter never resets there. That stops it from burning
through your data bundle on a server that keeps dropping.

### Stall detection

Sometimes a connection doesn't drop. It just goes silent: no data and no error.
Without a timeout, the download would sit at 43% forever. The engine starts a
30-second timer (the "watchdog"). Every time data arrives, the timer is reset. If
it ever runs out, the watchdog cancels the connection, and the retry loop
reconnects and resumes.

## Part B: the new Go concepts

### 1. Goroutines

A **goroutine** is a function running at the same time as the rest of your
program. Start one by putting `go` in front of a function call:

```go
go doSomething()   // starts it and moves on immediately, without waiting
```

They're much lighter than operating system threads: a Go program can run
hundreds of thousands at once. In stage 3, each connection of a multi-connection
download will be its own goroutine.

Stage 2 uses one indirectly. `time.AfterFunc` runs a function in a new goroutine
after a delay:

```go
watchdog := time.AfterFunc(d.StallTimeout, func() {
    stalled.Store(true)
    cancelAttempt()
})
```

While the main code is blocked inside `resp.Body.Read(buf)` waiting for data,
the watchdog's goroutine is counting down separately. If it fires, it cancels the
attempt's context, which makes the blocked `Read` return an error. One goroutine
rescuing another.

`func() { ... }` written inline like this is an **anonymous function**, similar
to a lambda in Python, but it can contain any number of statements.

### 2. Data races and `atomic`

When two goroutines use the same variable, and at least one writes to it, you
have a **data race**. The results become unpredictable, and the bugs appear
randomly. This is the classic danger of concurrent code.

The `stalled` flag is written by the watchdog goroutine and read by the main
one, so it can't be a plain `bool`. It's an `atomic.Bool`:

```go
var stalled atomic.Bool
stalled.Store(true)     // safe from any goroutine
if stalled.Load() {...} // safe from any goroutine
```

Atomic operations happen in one indivisible step, so another goroutine can never
see a half-finished write.

Go can detect races for you. Add `-race` when testing or building:

```
go test -race ./engine
```

It slows the program down, but it reports any race that actually occurs while
running. I ran all the tests this way.

On Windows, `-race` needs a C compiler. If you get an error mentioning `cgo` or
`gcc`, install one with `scoop install gcc`, then run
`$env:CGO_ENABLED=1` in PowerShell before `go test -race ./engine`. The
normal `go test ./engine` works without it.

For anything more complex than a single number or flag, you use a **mutex**
(`sync.Mutex`), a lock that only one goroutine can hold at a time. The fake
server in the tests uses one, because several request handlers append to the same
list:

```go
s.mu.Lock()
s.rangeHeaders = append(s.rangeHeaders, ...)
s.mu.Unlock()
```

### 3. Contexts inside contexts

Lesson 1 showed `ctx` carrying the user's Cancel. Stage 2 makes a **child**
context for each attempt:

```go
attemptCtx, cancelAttempt := context.WithCancel(ctx)
defer cancelAttempt()
```

Cancellation flows downward only:

- The user cancels `ctx` → `attemptCtx` is cancelled too → everything stops.
- The watchdog cancels `attemptCtx` → only this attempt stops. `ctx` is
  untouched, so the retry loop carries on.

That's how the code tells "the user pressed Cancel" apart from "the connection
froze": after an error, it checks `ctx.Err()`. If the parent context is cancelled,
it was the user.

`defer cancelAttempt()` matters even when nothing goes wrong: every context you
create must eventually be cancelled, or Go keeps resources for it.

### 4. Channels and `select`

A **channel** is a pipe that goroutines use to send values to each other. You
don't create any yourself in stage 2, but you use two that the standard library
gives you:

- `time.After(delay)` returns a channel that receives a value when the delay has
  passed.
- `ctx.Done()` returns a channel that is closed when the context is cancelled.

`<-ch` means "wait until something arrives on ch". `select` waits on several
channels and runs whichever is ready first:

```go
select {
case <-time.After(delay):   // the wait finished: try again
case <-ctx.Done():          // the user cancelled during the wait
    return "", ctx.Err()
}
```

Without `select`, you'd write `time.Sleep(delay)`, and pressing Cancel during a
30-second wait would do nothing for up to 30 seconds. With it, Cancel works
instantly.

### 5. Interfaces, and errors as types

Go's `error` is an **interface**: a list of methods. Specifically, it's "anything
that has a method `Error() string`". Any type with that method is an error
automatically. There's no `implements` keyword; having the method is enough.

That lets us make our own error types that carry extra information:

```go
type HTTPError struct {
    StatusCode int
    Status     string
}

func (e *HTTPError) Error() string { return "server replied " + e.Status }
```

To check for one, use `errors.As`, Go's version of `isinstance()`:

```go
var he *HTTPError
if errors.As(err, &he) {
    // err is an HTTPError (or wraps one), and he now points to it
    if he.StatusCode >= 500 { ... }
}
```

Compare the two tools:

- `errors.Is(err, ErrStalled)`: is this error, or anything it wraps, **this
  specific value**?
- `errors.As(err, &he)`: is this error, or anything it wraps, **of this type**?
  If so, give it to me.

`diskError` also has an `Unwrap()` method. That's what lets `errors.Is` and
`errors.As` look inside it at the original error. The `%w` in `fmt.Errorf`
creates the same kind of wrapping automatically.

### 6. JSON and struct tags

`encoding/json` converts between structs and JSON. The text in backticks after
each field is a **struct tag**:

```go
type meta struct {
    URL  string `json:"url"`
    ETag string `json:"etag,omitempty"`
}
```

`json:"url"` sets the key name in the file. `omitempty` leaves the key out
when the value is empty. Only exported (capitalised) fields are converted; that's
why the field is `URL` and not `url`, with the tag providing the lowercase name.

```go
data, err := json.MarshalIndent(m, "", "  ")  // struct -> JSON bytes
err := json.Unmarshal(data, &m)              // JSON bytes -> struct
```

### 7. Bit shifting

```go
delay := retryBase << (failures - 1)
```

`<<` shifts a number's bits left, which doubles it once per position:
`1s << 0` is 1s, `1s << 1` is 2s, `1s << 2` is 4s. It's a compact way to write
"double it each time".

## Part C: tests

Go has testing built in, with no extra framework. Rules:

- Test files end in `_test.go`. Normal builds ignore them.
- A test is a function `func TestSomething(t *testing.T)`.
- `t.Error(...)` reports a failure and continues; `t.Fatal(...)` reports and
  stops that test.

Run them:

```
go test ./engine -v          # -v lists each test by name
go test -race ./engine       # also detect data races
```

### How these tests work

`net/http/httptest` starts a real web server inside the test, on your own
machine. Our `fakeServer` serves random data and can be told to misbehave:

- `dropAfter`: close the connection after N bytes
- `stallAfter`: send N bytes, then go silent
- `ranges = false`: ignore Range requests, like some real servers
- `status = 404`: reply with an error code

Each test sets up one problem and checks the engine's response:

| Test | Checks |
|---|---|
| `TestRetriesAndResumesAfterDrops` | Connection dies every 200 KB; the file still completes, and the server sent each byte exactly once |
| `TestStallIsDetected` | Server freezes twice; the watchdog notices and the download completes |
| `TestResumeAfterCancel` | Cancel at 300 KB, run again; it continues from exactly 300,000 |
| `TestFileChangedOnServerRestarts` | File replaced on the server between runs; result is 100% the new version |
| `TestNoRangeSupportRestartsFromZero` | No-range server that drops twice; restarts and completes |
| `TestNoRangeGivesUpEventually` | No-range server that always drops; stops after 3 retries instead of downloading forever |
| `TestNotFoundIsNotRetried` | 404 is reported after one request |
| `TestServerErrorIsRetried` | 503 is retried |
| `TestSanitizeFilename` | Dangerous server-suggested names are cleaned |

`TestSanitizeFilename` is a **table-driven test**: a list of inputs and expected
outputs checked in a loop. It's the most common style in Go, because adding a
case is one line.

Tests are what will let us build stage 3 safely. Multi-connection downloading
rewrites the core of the engine; if all these tests still pass afterwards, we
know nothing that already worked got broken.

## Exercises

1. Run `go test ./engine -v`. All 10 tests should pass. (Optionally also
   `go test -race ./engine`, after setting up gcc as described above.)
2. In `TestSanitizeFilename`, add a case for a name with a trailing space and dot,
   like `"video.mp4. "`. What does `SanitizeFilename` return? Why does Windows
   need that?
3. Break something on purpose: in `download.go`, find
   `req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))` and change `start`
   to `0`. Run the tests. Which ones fail, and why? Undo the change afterwards.
4. Read `retryDelay`. What delay does the 7th failure get? The 20th?
