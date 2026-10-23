# Go lesson 4: dynamic segmentation, and locks done right

Stage 4 keeps every connection busy until the end of a download. The idea is
simple; making it safe while eight goroutines are writing the same file is the
interesting part. This lesson covers both.

Read it alongside `claimSegment` and `segment.write` in `engine/segmented.go`.

## Part A: what changed

### The problem stage 3 left

The stage 3 test on your connection ended like this:

```
91.4%   198 KB/s   conns 6/8
96.0%   213 KB/s   conns 3/8
98.6%   118 KB/s   conns 1/8
99.7%    56 KB/s   conns 1/8
```

Each connection owned one fixed piece. Fast connections finished and stopped,
and the last stretch ran on one slow connection.

### The fix: split the biggest piece

When a worker runs out of free pieces, `claimSegment` now:

1. finds the piece with the most bytes still to download,
2. cuts what's left of it in half,
3. shrinks that piece so it ends at the halfway point, and
4. gives the worker a new piece from the halfway point to the old end.

```
before:  piece 5  [=====>.................]      start 0, at 400 KB, ends at 1,000 KB
after:   piece 5  [=====>.......]                start 0, at 400 KB, ends at 700 KB
         piece 9                 [..........]    starts at 700 KB, ends at 1,000 KB (new worker)
```

The original connection keeps the first half, the bytes it is about to
receive anyway, and just stops sooner. The new connection asks the server for
`bytes=700000-999999`.

It repeats until nothing is worth splitting. "Worth it" means both halves would
be at least 256 KB (`minSplitSize`). Every new piece costs a round trip before
data flows, about half a second on a slow, distant connection. Splitting a
nearly finished piece would add delay rather than save time.

### Two smaller changes

- **All connections start, even when resuming.** If you resume a download
  with 2 unfinished pieces and 8 connections, all 8 workers start; the extra 6
  immediately split the 2 pieces between them. In stage 3, only 2 would
  have run.
- **The progress line shows splits** (`splits 4`), so you can see it happen.

### What the test measured

`TestSplittingBeatsSlowConnection` downloads a 16 MB file over 4 connections,
where the server makes one connection 10 times slower than the others. It runs
the same download twice, with splitting off and then on:

```
without splitting: 1.45s, with splitting: 0.52s (4 splits)
```

That's 2.8 times faster, because the fast connections took over most of the slow
connection's piece. The test also measures **wasted data**: when a slow
connection's piece is cut short, the bytes already on their way to it are
thrown away. The test fails if that waste exceeds 256 KB per split.

## Part B: the concurrency problem, and how it's solved

### The race that splitting creates

In stage 3, a piece's end never changed, so a worker could do this safely:

```go
pos, end := seg.position()        // 1. where am I, where does my piece end?
take := min(n, end-pos)           // 2. how much to write
file.WriteAt(buf[:take], pos)     // 3. write it
seg.advance(take)                 // 4. record it
```

Now another worker can split the piece between step 1 and step 3. The worker
would then write using an `end` that is out of date, possibly past the new end,
into bytes that now belong to the new piece. Both pieces would count those
bytes, and the piece list would claim more progress than is real.

With today's settings, that can't actually happen. A split always leaves the
current connection at least 256 KB (`minSplitSize`) ahead of its position, and
a single write is at most 64 KB (`chunkSize`), so a stale `end` is never close
enough to matter.

But look at what that safety depends on: two constants, in two different places,
that have nothing to do with each other. Raise `chunkSize` to 512 KB for speed one
day, or lower `minSplitSize` to 32 KB, and the bug appears. It would show up
rarely, only when the timing lines up, with no error message, just occasionally
corrupted downloads. That is the worst kind of bug to track down.

Correct concurrent code shouldn't depend on a coincidence like that. So the
writer and the splitter use the same lock, and the code is correct for any values
of those constants.

### The fix: check, write and record under one lock

`segment.write` does all three steps while holding the piece's lock:

```go
func (s *segment) write(file *os.File, data []byte) (n int64, complete bool, err error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    pos := s.start + s.done
    n = min(int64(len(data)), s.end-pos)   // the CURRENT end
    if n > 0 {
        file.WriteAt(data[:n], pos)
        s.done += n
    }
    return n, s.start+s.done >= s.end, nil
}
```

And the splitter takes the same lock before moving the end:

```go
victim.mu.Lock()
pos := victim.start + victim.done
...
victim.end = mid
victim.mu.Unlock()
```

Now a split happens entirely before a write or entirely after it, never in
the middle. That's what a lock guarantees: the code between `Lock` and
`Unlock` can't interleave with any other code holding the same lock.

Two consequences worth understanding:

- **The split point is calculated inside the lock**, from the position at that
  moment. The number used to pick the victim (`remaining()`, read earlier) may
  already be out of date, so it's checked again after locking.
- **Holding a lock during a disk write is normally a warning sign**, since other
  goroutines wait the whole time. It's acceptable here because `WriteAt` hands
  64 KB to the operating system's file cache, which takes microseconds, and the
  only goroutines that ever wait are the splitter and the progress reporter.

### Lock ordering, and deadlocks

There are now two kinds of lock:

- `d.segMu` protects the **list** of pieces (splitting inserts into it).
- each piece's `s.mu` protects that piece's fields.

`claimSegment` holds `segMu` and then takes piece locks one at a time.
`segmentStates` does the same. A worker writing data holds only its piece's lock
and never asks for `segMu`.

Why this matters: if goroutine 1 held lock A and waited for lock B, while
goroutine 2 held B and waited for A, both would wait forever. That's a
**deadlock**, and the program freezes with no error. The standard way to make it
impossible is to always take locks in the same order. Here, the order is: the
list first, then a piece. No code ever takes `segMu` while holding a piece lock.

That rule is written in a comment above `claimSegment`, because it's the kind of
thing that's easy to break months later without noticing.

### Hanging up early

When a piece shrinks, its connection's HTTP request still asked the server for
the old, larger range. The server keeps sending. `write` reports `complete`
once the new end is reached, and `fetchSegment` simply returns. The deferred
`resp.Body.Close()` closes the connection, and anything still in transit is
discarded. That's the "waste" the test measures. It's unavoidable without
asking the server to stop mid-response, which HTTP doesn't support; IDM has
the same cost.

## Part C: new Go details

### `slices.Insert`

```go
d.segments = slices.Insert(d.segments, at+1, piece)
```

The `slices` package (Go 1.21) has common list operations. `Insert` puts a value
at a position and shifts the rest along. Like `append`, it returns the new
slice, which you must assign back. The new piece goes right after the one it
came from, so the list stays in file order and `validSegments` can check it.

### Unexported fields as test knobs

```go
disableSplitting bool   // lowercase: only code inside package engine can set it
```

The test needs to run the same download with splitting off, to compare. The
field is lowercase, so the CLI or a future interface can't touch it, but the
tests can, because test files in the same folder belong to the same package
(`package engine`).

### `(*segment)(nil)`

```go
victim, at := (*segment)(nil), -1
```

`nil` on its own has no type, so `:=` can't work out what type `victim` should
be. `(*segment)(nil)` means "a nil pointer to a segment". It's equivalent to
writing `var victim *segment` and `at := -1` on two lines.

## Exercises

1. Run a multi-connection download and watch the `splits` count. At what
   percentage do splits start happening? Why not earlier?
2. Change `minSplitSize` to `64 * 1024` and run `go test ./engine -run Split -v`.
   Do the timings change? What would happen on your real connection, where each
   new request takes about half a second? Change it back afterwards.
3. In `claimSegment`, the remaining size is checked once while choosing the
   victim, then again after locking it. Describe a sequence of events where the
   second check fails even though the first passed.
4. Why does the original connection keep the **first** half of the remaining
   bytes, not the second?

## Part D: when the server says "too many" (learned the hard way)

The first 100 MB test against `proof.ovh.net` taught us something no fake
server had: some servers limit how many connections, and how many *requests*,
one user may make.

What happened, from the logs:

1. 8 connections opened. OVH allowed 3 and answered the other 5 with
   `429 Too Many Requests`.
2. The engine treated 429 like any temporary error: wait and retry. So 5
   connections kept knocking, every 1, 2, 4, 8... seconds.
3. OVH decided that was abuse and stopped answering your address entirely:
   first the working connections froze, then new ones couldn't connect
   (`dial tcp ... i/o timeout`). The block lasted several minutes, and it
   happened again on a different network.

The engine now behaves the way a polite downloader should:

- **Surplus connections drop out.** If the server refuses a connection with
  429 or 503 *while other connections are working*, that connection gives its
  piece back (`segment.release()`) and stops for good. The remaining
  connections finish the work. If *every* connection is refused, there's no one
  to hand the pieces to, so they retry with backoff as before.
- **No more splitting.** After the first 429/503, the download stops splitting
  pieces: each split is one more request, and this server is rationing them.
- **A shared cool-down.** After a 429/503, no connection sends a new request
  until the cool-down ends: the server's `Retry-After` time if it sent one
  (OVH asked for 10 seconds), otherwise 5 seconds. Connections already
  receiving data aren't interrupted.
- **Problems are reported.** Every connection problem now reaches the screen
  with its reason, e.g.
  `server replied 429 Too Many Requests - the server limits connections, continuing with 3`.
  Before this, a stuck download just showed `0.0 B/s`, and neither of us could
  tell why.

### Compare-and-swap

The cool-down end time is shared by all connections, and several may set it at
the same moment. Instead of a mutex, it uses an atomic **compare-and-swap**
loop:

```go
for {
    cur := d.cooldownUntil.Load()
    if until <= cur || d.cooldownUntil.CompareAndSwap(cur, until) {
        return
    }
}
```

`CompareAndSwap(old, new)` stores `new` only if the value is still `old`, and
reports whether it did, all in one indivisible step. If another goroutine
changed the value in between, the swap fails and the loop tries again with the
fresh value. The result is that the latest end time always wins, and an earlier
one can never overwrite it. For a single number, this is lighter than a mutex.

### Real servers are the real test

The fake test server never limited anything, so 26 passing tests said nothing
about this problem. Two habits from today are worth keeping:

- Test against real servers, and read the logs closely, not just the final
  pass/fail.
- When a real-world failure appears, reproduce it in a fake-server test
  (`TestSurplusConnectionsDropOut`) so it can never quietly come back.
