# 05 - What we learned from FDM, and how we used it

We read the source of Free Download Manager 3.9.7 (the last open-source
version, GPL). We took **ideas only**: no code was copied. Copying GPL code
would force our whole project under the GPL, and the ideas are what matter.

Four ideas made it into the engine. Each one is small; together they make the
engine kinder to servers and better on a variable connection.

---

## 1. Ramp up gradually

**Before:** with 8 connections we opened all 8 at once.

**FDM:** starts with one, then adds one more each time the newest one starts
receiving data - and only while every connection is healthy.

**Why it matters:** a server that allows 3 connections used to see 8 requests
arrive together and refuse 5 of them. Some servers (OVH did this to you)
treat a burst of refused requests as abuse and block you for a while. With
ramp-up the server sees requests one at a time, refuses the 4th, and we stop
there: one refusal instead of five.

**Ours** (`runSegmented` in `segmented.go`): a small controller goroutine
checks every 250 ms and starts another worker only if

- we're below the connection limit,
- the server hasn't refused us,
- every running connection is receiving data (`activeNow == alive`),
- there's work left to give it.

Go point: the controller is itself counted in the `WaitGroup`. Otherwise
`wg.Wait()` could see "zero workers" in the instant before the controller
starts one, and return too early. A goroutine that can *start* goroutines
must be counted too.

Test: `TestRampUpAvoidsRefusalBursts` - a 3-connection server, 8 asked for,
at most 2 refusals (it used to be up to 8).

## 2. Connect first, then split

**Before:** to split a piece, we shrank it first, then the new connection
asked for the second half. If that request failed, the bytes were orphaned
until someone noticed and retried them.

**FDM:** opens the new connection first, and only shrinks the old piece once
the server has actually started sending.

**Ours:** a split is now a *proposal*. `claimSegment` returns a new piece
marked with its `victim`, but nothing changes yet. When the server replies
206, `fetchSegment` calls `commitSplit`, which shrinks the victim and inserts
the new piece - but only if the victim hasn't already passed the split point
in the meantime (then it's `errSplitStale` and we just hang up). If the
request fails, `abortSplit` undoes the proposal: the victim never lost
anything.

This is a common pattern worth remembering: **prepare, then commit under the
lock, and re-check at commit time**. Things can change between the two.

## 3. Predict the split point

A piece downloading at 1 MB/s keeps moving while the new connection is being
set up. Splitting at the *current* midpoint means the old connection may have
already passed it by the time the new one starts.

So we split at the midpoint of what's left **after about 2 seconds more** at
the piece's measured speed (`splitLookahead`). Each piece keeps a smoothed
speed (`trackRateLocked`: 70% old, 30% new, so one bad second doesn't swing
it).

## 4. Restart connections that become very slow

Sometimes a connection that was fast slows to a crawl and stays there - a
congested route, an overloaded server node. Reconnecting usually gets a
fresh, fast one.

**FDM's rule, which we use:** learn the connection's normal speed from its
first 20 one-second samples; if it then stays at a third of that or less for
3 minutes, drop it and reconnect (`slowDetector` in `segmented.go`).

Details that matter:

- The check runs **after** the bytes are saved, so nothing is thrown away.
- The reconnect happens at once, without counting as a failure or waiting a
  backoff - it was our choice, not an error.
- Not for servers without range support: there a reconnect starts the whole
  file again.
- A connection that is *steadily* slow is left alone - it's not getting
  worse. (`TestSteadySlowConnectionIsKept`)

Testing trick: the timings are package variables, so the test shrinks them
(20 ms samples, 100 ms limit) and restores them with `t.Cleanup`. Three
minutes would make a terrible test.

---

## What FDM does that we deliberately don't

- **Fixed 5-second retry, 20 times.** We back off (longer each time) and obey
  the server's `Retry-After`. Kinder to struggling servers.
- **No handling of 429 / "too many connections".** We drop surplus
  connections, stop splitting, cool down, and remember the host's limit.
- **Rolling back 3000 bytes after an error** (to guard against corrupt
  tails). Our writes go straight to the right offset and we only count bytes
  that were written, so there's nothing to roll back.

Where FDM was ahead, we caught up; where we were ahead, we stayed ahead.

---

## Then we built a bench, and it found two more problems

`cmd/bench` is a small server on 127.0.0.1 that serves made-up files under
five situations (normal, limited to 3 connections, a connection that goes
bad, connections that get cut off, no range support) and logs every request.
Any download manager can be pointed at it, and `bench -report` shows what it
did: how many connections, when, which ranges, what happened to each.

Testing our own engine on it first found:

1. **The cool-down after a refusal held back the healthy connections.**
   After one 429, all three working connections sat idle for ~4 s at every
   piece boundary. Only the refused connection should wait; a connection
   that just finished a piece is only taking its own place back. Fix: a
   `healthy` flag per connection skips the cool-down
   (`TestCooldownDoesNotStallWorkingConnections`).

2. **A crawling connection kept a small tail to itself.** The rule "never
   split below 512 KB" left ~480 KB on a 25 KB/s connection: 20 seconds of
   waiting at the end. Fixes:
   - split the piece that will take the **longest to finish**, not the one
     with the most bytes left;
   - a piece that would take over 4 s may be split down to 64 KB halves;
   - a connection under a quarter of the others' speed gets its whole rest
     **taken over**, not halved.

   The slowdown test went from 31 s to 13 s - the same as a normal download.

The lesson: measure, don't guess. Both bugs passed every unit test because
the tests checked what we thought of, and the bench showed what actually
happens.

---

## Then we watched IDM on the bench

Same five situations, 40 MB files, IDM against ours.

| Situation | IDM | Ours |
|---|---|---|
| normal | 20.6 s | 20.8 s |
| limit3 | 28.1 s, 6 refused | 30.0 s, 1 refused |
| slowdown | 29.7 s (+9 s from one bad connection) | 21.3 s (+0.5 s) |
| drops | 20.6 s | 21.1 s |
| norange | 83.5 s | 83.4 s |

What IDM does, as seen from the server:

- **No separate probe.** Its first request is a normal download; it reads the
  size from the reply and opens the other connections meanwhile. We copied
  this (`openFirst` in `probe.go`): the first request asks for `bytes=0-`,
  and the connection for piece 0 takes the reply over (`startResponse`).
  One request and one round trip fewer. Two catches we handled:
  - when resuming, a reply streaming from byte 0 would be thrown away, so we
    check for an unfinished download first and ask the one-byte question;
  - a 200 reply that says `Accept-Ranges: bytes` is ambiguous, so we fall
    back to the one-byte question there too.
- **A fast start**: 4 connections within 0.1 s. We now start 3 together,
  then ramp up one at a time. Most servers that limit connections allow 3.
- **It keeps knocking on a limited server** (6 refusals in 28 s).
- **It asks for "from here to the end"** and hangs up when it reaches the next
  piece: 60-80 KB more is sent than needed. Ours asks for exact ranges.
- **A new TCP connection for every request.** Ours reuses them.
- **It never takes over from a crawling connection**, only shares out the
  rest, so a connection gone bad cost it 9 seconds.

Fixes this round found in ours:

- Splitting was switched off entirely after a refusal, so connections that
  finished early sat idle. A connection that has finished its piece only
  takes its own place back, so splitting is allowed again; the ramp-up still
  adds no extra connections.
- A connection that found the piece it wanted already being split gave up
  for good; now it waits a moment and looks again (`splitPending`).
- **Hanging up isn't instant for the server.** When we cut a reply short
  (the open-ended first one, or a split), the server keeps counting that
  connection until it notices. Asking again straight away got us refused as
  "one too many" - and a refused connection closes for good. On a server
  known to limit connections, a connection that just hung up now waits
  `hangupGrace` (0.5 s) first.

The last one is a good example of a bug you only find by watching both
sides: the client's view ("my request is done") and the server's ("that
connection is still open") disagree for a moment, and the moment matters.

---

## And today's FDM

The same five situations with a current FDM build:

| Situation | IDM | FDM | Ours |
|---|---|---|---|
| normal | 20.6 s | 23.8 s | 20.8 s |
| limit3 | 28.1 s, 6 refused | 30.3 s, 5 refused | 30.0 s, 1 refused |
| slowdown | 29.7 s | 29.0 s | 21.3 s |
| drops | 20.6 s | 22.8 s | 21.1 s |
| norange | 83.5 s | 84.2 s | 83.4 s |
| extra data per file | 60-80 KB | 150-190 KB | 0-35 KB |

Modern FDM no longer ramps up one connection at a time like the 3.9.7 code:
it downloads alone for 2-4 seconds, then opens 15 more connections at once
(16 in all). It doesn't take over from a crawling connection either. Like
IDM it asks for open-ended ranges and opens a new TCP connection per
request. Old source code tells you what a program did; only watching it
tells you what it does now.
