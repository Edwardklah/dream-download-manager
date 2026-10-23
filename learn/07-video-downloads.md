# 07 - Video downloads (YouTube and others)

Paste a YouTube link (or press "Download the video on this page" in the
browser) and dlmanager shows the qualities on offer; pick one and it's
downloaded like any other file: several connections, pause, resume.

## Why video sites are different

A YouTube page isn't a file. The video lives at addresses like
`https://rr3---sn-xyz.googlevideo.com/videoplayback?...&sig=...`, which:

- are worked out by JavaScript in the page (deliberately hard: YouTube hides
  them behind small puzzles that change often),
- **expire** after a few hours,
- come as **two files** in HD: the picture and the sound separately ("DASH"),
  so a player can switch quality without fetching the sound again.

So a video download is three jobs: find the stream addresses, download the
streams, put picture and sound back together.

## 1. Finding the streams: yt-dlp

[yt-dlp](https://github.com/yt-dlp/yt-dlp) is the open-source program that
knows how to read YouTube and over a thousand other sites. Keeping up with
those sites is a full-time job for its many contributors - we use it rather
than redo it. FDM does the same.

We only ask it one question: `yt-dlp -J <page>` prints everything about the
video as JSON - title, thumbnail, and every stream ("format") with its codec,
size, resolution and address (`video/video.go`). We download the streams
ourselves, because our engine is much faster than yt-dlp's single connection.

Since late 2025, yt-dlp needs a separate JavaScript program to solve
YouTube's puzzles. It supports Deno (~40 MB), Node and QuickJS; we fetch
**QuickJS-ng (2 MB)**, and yt-dlp uses Deno or Node instead if you have them.
Both tools are downloaded once into `%AppData%\dlmanager\tools`, by our own
engine, from the projects' GitHub releases; yt-dlp is checked against the
SHA-256 checksums it publishes (`video/tools.go`).

Go points:

- `exec.CommandContext` runs a program and kills it if the context ends
  (here: a 90-second timeout).
- `"--"` before the address: everything after it is an argument, never an
  option. A link starting with `-` can't sneak an option in.
- A console program started from a window program opens a black console
  window on Windows, unless you pass `CREATE_NO_WINDOW`
  (`video/hide_windows.go`).
- **Testing a program you don't have**: `TestMain` lets the test binary
  pretend to be yt-dlp when an environment variable is set. The tests run
  *themselves* as the fake yt-dlp - it works on every system, unlike a shell
  script.

## 2. Choosing a quality

yt-dlp lists 20-30 formats for a YouTube video. `video/choices.go` turns
them into a short list, one per quality:

- For each height, H.264 ("avc1") is preferred up to 1080p - it plays on
  everything. Above that YouTube only offers VP9 or AV1; we take AV1 in MP4
  (Windows plays it with the free "AV1 Video Extension").
- HDR versions are skipped - they look washed out on most screens.
- The sound: the **original language** first (videos can have dubbed tracks,
  `language_preference`), then the highest bitrate, in M4A (AAC).
- Plus "Audio only".
- Portrait videos (Shorts) are named by their short side: 1080x1920 is
  "1080p".

The picker suggests the best quality up to 1080p.

## 3. Downloading the streams: the engine, with two additions

- **`MaxRequestSize`**: YouTube serves a big range slowly but 10 MB ones at
  full speed - yt-dlp tells us so for each stream (`http_chunk_size`). The
  engine now asks for at most that much per request and continues with the
  next.
- **`ResumeID`**: a paused video gets *new* addresses when it resumes (the old
  ones expired). The `.part.meta` file now stores an ID - page + format - so
  the half-downloaded stream is recognised and continued anyway.

The manager (`manager/video.go`) runs a video as steps: look the video up
(fresh addresses), download the video stream, then the audio stream, join.
If a stream address expires mid-download ("403 Forbidden"), it looks the
video up again once and carries on.

Two bugs the tests caught on the way:

- Saving progress rewrote the `.part.meta` without the ResumeID, so a resume
  after the address changed started over.
- With chunked requests, a piece split off a slow connection stopped after
  its first chunk and was never finished. The engine's own chunk test hadn't
  caught it because no splits happened there - so a test now forces them.

And one bug the tests found by **hanging**: `videoJob` was called while the
manager held its lock, and then tried to take the same lock. Go's mutexes
aren't re-entrant: the second `Lock()` waits forever for the first, which
is held by... itself. A deadlock. The cure: a function that's called with the
lock held must not take it (`startLocked`, `videoJob` - the `Locked` suffix
is the convention that says so).

## 4. Joining picture and sound: `mux/`

Most downloaders use ffmpeg (~100 MB) for this. Joining needs no re-encoding -
the compressed picture and sound are copied byte for byte - so it can be done
in Go, with the open-source [mp4ff](https://github.com/Eyevinn/mp4ff) library.

How an MP4 is laid out:

```
ftyp   "this is an MP4"
moov   the index: tracks, codecs, and for every sample (a video frame, or
       a short block of sound) its size, duration and position
mdat   the samples
```

A DASH stream is **fragmented**: a moov with no samples, then many
`moof`+`mdat` pairs, each indexing a few seconds. mp4ff's `Defragment` turns
that into the ordinary form, which every player opens and seeks quickly.

The trick: Defragment finds every sample through the positions recorded in
the file. So we show it **the two downloads as one file** - the video file
followed by the audio file (`concatReaderAt`), with the audio's fragments'
positions shifted by the video's length and its track added to the video's
moov - and it writes one ordinary MP4 with both tracks. Nothing is copied
twice, nothing big is held in memory. The fragments are sorted by time first,
so picture and sound end up **interleaved** (about 2 seconds of each in turn),
which is what players expect.

Checked by the tests: every sample of both inputs is in the output, byte for
byte and in order; ffmpeg decodes the result without a single error.

One deliberate choice: our joiner keeps each stream's timing exactly as it
came. ffmpeg's join shifts the picture so its first frame is at 0:00 - with
B-frames that moved it 67 ms earlier than the sound in our test. Keeping the
stream's own timing matches what the browser does when it plays the video on
the site.

## 5. The browser

- The popup has **"Download the video on this page"**, for any site.
- On the big video sites, right-clicking the page offers **"Download this
  video with dlmanager"**.
- Right-clicking a *link* to a video ("Download with dlmanager") also works:
  the app sees it's a video page (`video.IsVideoPage`) and opens the picker,
  instead of downloading the web page.

## Fair use

Downloading is for videos you have the right to keep - your own uploads,
Creative Commons, lectures you're allowed to save. Some sites' terms forbid
downloading; DRM-protected streams (paid films) are skipped entirely
(`has_drm`).

## 6. The button on videos (`extension/content.js`)

Like IDM, a **Download** button appears on a video when you point at it. The
difference is what it does:

- **IDM** watches the network and offers every video stream that loads,
  which is why it so often offers you the ad.
- **Ours** doesn't look at the network at all. It sends the page's address
  to yt-dlp, which finds the page's *real* video and skips ads by design. Only
  if yt-dlp doesn't know the site is the file the player plays offered instead.

Filters, so it doesn't sit on ads in the first place:

- no button inside frames served by ad companies (doubleclick.net and co.),
- none on videos smaller than 400x225 (thumbnails, previews, ad slots),
- none on clips under 30 seconds when a longer video is on the page,
- none on muted autoplaying videos that aren't the page's biggest.

How it works in the page:

- A **content script** runs in every page and every frame (`all_frames`), so
  it also reaches YouTube players embedded in other sites; there, the frame's
  own address (youtube.com/embed/...) is what's sent.
- Players cover the `<video>` with their own controls, so "the mouse is over
  the video" is worked out from **coordinates**, not from the element under
  the mouse.
- The button lives in a **closed shadow root**: the page's CSS can't restyle
  it, and our CSS can't leak into the page.
- It sends nothing anywhere until you click.

A bug the mock-page test caught: the button was positioned while still
hidden, when its width reads as 0, so it hung half outside the video. Show
first (invisibly), measure, then place.

## 7. More sites: HLS, WebM, a fallback, and your login

YouTube sends ordinary MP4 files. Many sites don't. Four additions make most
of them work:

### HLS (`hls/`)

Most sites stream in **pieces**: a playlist (`.m3u8`) lists hundreds of
2-10 second files. `hls.Download` reads the playlist (picking the best
quality from a "master" playlist), downloads the pieces several at a time,
and puts them together in order.

- **Resumable**: each finished piece is saved in a folder next to the
  download (`.pieces`), through a temp file and rename, so a piece on disk is
  always whole. A resume skips the pieces already there - even with a new
  playlist address (they expire), because the folder is matched by the list
  of pieces, not by the address.
- **AES-128**: pieces are often locked with a key served next to the
  playlist. That's a standard part of HLS, not copy protection: AES in CBC
  mode, the IV given or derived from the piece's sequence number, then
  PKCS#7 padding removed (`decrypt`). Real DRM (SAMPLE-AES, FairPlay `skd://`
  keys, Widevine) is refused with a clear message.
- The result is **MPEG-TS** (kept as a `.ts` file - every player plays them)
  or, for **fMP4** pieces (an `#EXT-X-MAP` header, then MP4 fragments), an MP4
  tidied by package mux.

Tested with real HLS made by ffmpeg - locked TS pieces and fMP4 pieces - and
the results decode without a single error.

### WebM / Matroska (`mkv/`)

VP9 video and Opus sound come in WebM, which is written in **EBML**: a tree
of elements, each `ID | size | content`, IDs and sizes in a variable-length
encoding where the leading zero bits say how many bytes follow. The joiner
reads each stream's Info, its one TrackEntry and where its Clusters are (a
few seconds of frames each), then writes one file: both tracks (renumbered 1
and 2), the clusters of both in time order, and **Cues** - an index of the
video's key frames, so players can seek. The Segment's size and the
SeekHead's positions are only known at the end: they're written as
fixed-size placeholders and filled in afterwards (`WriteAt`).

A detail worth knowing: in EBML a size of all 1-bits means "unknown size",
so 127 can't be written in one byte (`0xFF`) - it takes two. The test checks
exactly that.

### The fallback: let yt-dlp download it

Some sites use forms we don't handle (DASH pieces, other playlist kinds). If
nothing in the list is something we can download ourselves, the picker
offers **"Best available"**, which yt-dlp downloads itself (`--newline` and
a `--progress-template` give us one progress line per update to read).

### Your login (cookies)

Some sites only show videos to logged-in users (Instagram, for one). When a
video page comes from the browser, the extension sends your cookies for that
site; the app writes them to a temporary `cookies.txt` (readable by you
only, deleted straight after) for yt-dlp, and stores them with the download
so a resume can look the video up again. They're never sent to the window -
and never to other servers: a video is often delivered by a separate server
(a CDN), and the site's cookies don't go there (`sameSite`).
