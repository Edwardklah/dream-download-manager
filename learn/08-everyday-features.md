# 08 - Everyday features

Stage 8 adds what you use every day rather than what makes downloads fast:
subtitles, MP4 instead of .ts, keeping yt-dlp current, folders by kind, a
scheduler, clipboard catching and starting with Windows.

## 1. Subtitles (`video/subtitles.go`)

yt-dlp's JSON has two lists of subtitles per video:

- `subtitles` - written by people (the uploader, the site);
- `automatic_captions` - YouTube's speech recognition, plus machine
  translations of it into ~100 languages. Only the original is offered (yt-dlp
  marks it `en-orig`): a machine translation of a machine transcript is
  rarely worth having.

You pick a language in the picker; it's saved next to the video with the
same name, `Lecture 5.en.srt`, which players load by themselves. The picker
remembers your language for next time (in the window's `localStorage`).

**WebVTT to SRT.** Sites mostly serve WebVTT, the web's format; we convert
it to SubRip, which every TV and player reads. They're close cousins:

```
WEBVTT                                   1
                                         00:00:01,000 --> 00:00:03,500
00:01.000 --> 00:03.500 align:start      Hello & <i>welcome</i>
<v Narrator>Hello &amp; <i>welcome</i>
```

The differences: a header, optional hours, `.` vs `,` before the
milliseconds, numbered cues, and tags - SRT keeps `<i> <b> <u>`, the rest
(`<c.yellow>`, `<v Speaker>`) is dropped, keeping its text.

**YouTube's rolling captions** are the tricky part. Each cue repeats the
line before and adds the new one word by word (`<00:00:01.120>` timings
inside the text), so a plain conversion shows every line two or three
times. `unroll` takes out, from each cue, the lines it shares with the end
of the cue before - leaving each line once, at the moment it was first said.

**ASS/SSA** (styled subtitles, common for anime) are kept as they are:
converting them would lose their positions and styling.

If fetching the subtitle file directly fails (YouTube sometimes refuses),
yt-dlp is asked to fetch it (`--skip-download --write-subs`). And if the
subtitles can't be had at all, the video still counts as finished, with a
note under it: `Item.Note` is for remarks that aren't errors.

## 2. MPEG-TS to MP4 (`mpegts/`)

HLS sites send `.ts` pieces. VLC plays them, but phones and TVs prefer MP4,
and MP4 has an index - skipping to 1:23:00 is instant. Converting doesn't
re-encode anything: the compressed pictures and sound are copied byte for
byte; only the packaging changes. (The test checks exactly that: decoding
the MP4 gives the same frames, hash for hash, as decoding the .ts.)

How a .ts is built, from the outside in:

- **188-byte packets**, each starting with `0x47` and a PID (a stream
  number). A file is just these, back to back.
- **PID 0 holds the PAT**: "the programme's table is on PID 4096".
- **The PMT** on that PID: "PID 256 is H.264 video, PID 257 is AAC sound".
- Each stream's packets, joined, make **PES packets**: a header with the
  times - **PTS** (when to show it) and **DTS** (when to decode it), in
  1/90000 s - and the data: one video frame, or a few audio frames.

Two passes over the file:

1. **Setup**: the MP4's header must describe each stream before any frame,
   so the first pass finds the video's SPS/PPS (its resolution, profile...)
   and the sound's sample rate and channels (from the first ADTS header).
2. **Copy**: each video frame's NAL units go into an MP4 sample - with a
   4-byte length in front of each instead of TS's `00 00 01` start codes.
   AAC frames lose their 7-byte ADTS header (MP4 keeps that information
   once, in its header).

Three things that make it more than a copy:

- **Time wraps around.** PTS is 33 bits: it goes back to 0 every 26.5
  hours of stream time. `unwrap` notices the jump and counts wraps.
- **Discontinuities.** HLS can splice parts with unrelated clocks (an ad
  break). A timestamp that goes back, or jumps over 10 s, shifts the rest of
  that track so it carries straight on.
- **Lip sync.** Picture and sound rarely start at the same instant (here the
  sound starts 23 ms before the picture). We write a fragmented MP4 that keeps
  each track's real start time, and mp4ff's defragmenter turns the
  difference into an *edit list* - "start this track 23 ms in". The test
  compares the offset before and after, to the millisecond.

Sites that send picture and sound as two separate TS playlists now work too:
each becomes a fragmented MP4, and package mux joins them like YouTube's.

MP3 or AC-3 sound, or MPEG-2 video, return `ErrUnsupported`: the file is
then kept as a `.ts`, as before. 94 MB converts in under 2 seconds.

## 3. Keeping yt-dlp up to date

Sites change, yt-dlp catches up almost weekly, and an old copy quietly stops
working on one site after another. `Tools.CheckForUpdate` asks GitHub for the
latest release - the same tiny redirect trick as the installer, no download
- at most once a week, and remembers the answer in `update-check.json`. A
newer version shows a notice above the list; nothing is downloaded until
you press Update. "Not now" isn't asked again for that version. When a video
look-up fails, the check runs early (at most daily): that's the moment an
update is most likely to be the fix.

yt-dlp's versions are dates (`2026.10.30`, sometimes `2026.10.30.1`), so
comparing them as text gives the right order - no version parsing needed.

## 4. Folders by kind (`manager/categories.go`)

Like IDM, and in the same folders it uses: `Downloads\Video`, `Music`, `Compressed`, `Programs`,
`Documents`, `Pictures`. An ordinary file's name is often only known once
the server answers, so it's moved into its folder **when it finishes** - a
rename on the same disk, instant. A video's kind is known when you pick it,
so it goes there from the start, and its streams and subtitles with it.

A name that's taken gets a number, `clip (1).mp4` - nothing is ever
overwritten.

A detail worth knowing: the settings file from an older version has no
`sortIntoFolders` in it. Reading it into an empty `Settings` would leave the
new switch at Go's zero value, `false`. So `load` starts from
`DefaultSettings()` and reads the file *over* it: what the file has wins,
what it doesn't have keeps its default.

## 5. The scheduler (`manager/schedule.go`)

For night bundles: downloads wait until the start time.

- **With a stop time** (00:00-05:00) it's a window. Inside it, the queue
  runs; outside, it's held. At 05:00 running downloads go back to waiting -
  stopped the same way as when the app closes, so they keep their progress -
  and continue the next night. Because it's just "am I inside the window?",
  waking the computer at 02:00 starts the queue, and waking at 07:00 after
  sleeping through the night doesn't.
- **Without a stop time**, the queue runs from the start time until it's
  empty; what's added after that waits for the next start.

"Start now" always skips the wait, as it skips the queue.

**Testing time.** Waiting until midnight in a test is out of the question,
so the manager reads the time through `m.now`, a function. The app uses
`time.Now`; the tests give it a pretend clock they move by hand, and call
`m.tick` themselves. This is the usual way to test anything time-based:
make the clock something you can pass in.

**Is 02:00 inside 23:00-05:00?** Windows that cross midnight make simple
comparisons wrong. `inWindow` asks instead: when did the clock last show the
start time, and when the stop time? If the last start came after the last
stop, we're inside. That works across midnight with no special case.

## 6. When done

The status bar's "When done" chooses sleep or shut down. When the last
download finishes (not when you pause it), the window comes up with a
60-second countdown and a Cancel button. It's one-shot: afterwards it's back
to "do nothing", so a setting forgotten from last week can't switch the
computer off today.

- Sleep: Windows' `SetSuspendState(FALSE, ...)` from `powrprof.dll`,
  called directly through `syscall` (the common `rundll32` trick passes its
  arguments wrongly and can hibernate instead).
- Shut down: `shutdown /s /t 0`, what the Start menu does.

## 7. Clipboard catching

Copy a link to a file anywhere, and dlmanager offers it above the list. Only
links that clearly end in a file worth downloading count (`LinkToOffer`:
programs, archives, video and music, PDFs - IDM's rule); copying a web
page's address to paste in a chat brings nothing up.

Watching the clipboard means looking every second. Opening the clipboard
that often could get in another program's way, so on Windows we first read
`GetClipboardSequenceNumber` - a counter Windows increases on every copy -
and only open the clipboard when it has changed.

## 8. Start with Windows

Programs listed under
`HKCU\Software\Microsoft\Windows\CurrentVersion\Run` start when you sign in.
The switch in Settings adds `"...\gui.exe" --tray` there (with `reg.exe`,
like dlm-host's setup): the app starts in the tray with no window, so the
browser extension and the scheduler work from the start. On Linux the
equivalent is a `.desktop` file in `~/.config/autostart`.
