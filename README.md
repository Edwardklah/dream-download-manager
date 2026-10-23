# Dream Download Manager

A free, open-source download manager with no ads: multi-connection downloads
that resume, video sites, subtitles, browser catching, schedules and themes.

Written in Go; the desktop app uses Wails + Preact. (The code's internal name
is still `dlmanager`.)

## Status

- [x] Stage 1 - single-connection engine: probe, streaming download, progress, speed, ETA, cancel
- [x] Stage 2 - resume, automatic retry with backoff, stall detection
- [x] Stage 3 - multi-connection downloads (8 by default), resumable piece by piece
- [x] Stage 4 - dynamic segmentation: idle connections take over half of the slowest piece
- [x] Stage 5 - desktop app (Wails + Preact): queue, pause/resume, piece strip, settings, tray
- [x] Refinements from studying FDM: gradual ramp-up, connect-then-split, predicted split point, restart of connections that turn very slow
- [x] Stage 6 - browser extension (Chrome/Edge): catches downloads, right-click "Download with dlmanager"
- [x] Stage 7 - video sites via yt-dlp: quality picker, streams downloaded by our engine, joined into MP4 in Go (no ffmpeg)
- [x] Stage 8 - everyday features: subtitles (.srt), HLS .ts converted to MP4, yt-dlp update offers, folders by kind, scheduler and "when done", clipboard catching, start with Windows
- [ ] Stage 9 - a real app: Windows setup file, first-run welcome, Linux build

## Install (Windows)

Run `DreamDM-Setup-<version>.exe`. It asks whether to install just for you
(no administrator needed) or for everyone on the PC. Uninstall from
Settings > Apps; your downloaded files are never removed.

To build the setup file yourself: `devloop` actions `app-build`, then
`app-installer` (needs NSIS: `winget install -e --id NSIS.NSIS`). The script
is `installer/windows/DreamDM.nsi`; the file lands in `dist/`.

## Try it

From this folder, in PowerShell:

```
go run ./cmd/dl https://example.com/somefile.zip
```

Files save to your Downloads folder, or add a folder after the link.
Ctrl+C cancels and keeps the `.part` file. Run the same command again to resume.
`-stop-after 10s` (before the URL) cancels automatically, for testing resume.
`-c 8` sets the number of connections (default 8; `-c 1` for a single connection).
Servers that refuse extra connections are remembered for a day, so the next
download starts within their limit. `-limits` shows them, `-set-limit host=N`
sets one yourself (never expires), `-forget host` removes one.
If the connection drops or freezes, the download retries and continues by itself.

To build a standalone program instead:

```
go build -o dl.exe ./cmd/dl
.\dl.exe https://example.com/somefile.zip
```

## Working with Claude (devloop)

`devloop.ps1` lets Claude run tests and test downloads on this PC and read the
output directly. Start it once in its own PowerShell window and leave it open:

```
.\devloop.ps1
```

It only runs a fixed list of actions (test, vet, build, version, test downloads
into `.dev\downloads`, list/empty that folder). Ctrl+C stops it.

## Tests

```
go test -race ./...
```

## Layout

```
dlmanager/
  go.mod             module name and Go version
  engine/            the download engine (a library, no user interface)
    probe.go         asks the server about the file: size, name, range support
    download.go      downloads it, resumes, retries; reports progress through a callback
    segmented.go     multi-connection downloads: split, download pieces in parallel
    meta.go          the .part.meta file that makes resuming safe
    errors.go        error types, and which errors are worth retrying
    hostlimits.go    remembers servers that limit connections
    download_test.go tests against a fake server that drops, freezes and changes files
    segmented_test.go multi-connection tests
    dynamic_test.go  dynamic segmentation tests
    refinements_test.go ramp-up and slow-connection restart tests
    lock.go          .part.lock: stops two downloads writing the same file
    client.go        HTTP client settings (timeouts, proxy)
    speed.go         speed measured over the last 3 seconds
  manager/           the download queue: how many run at once, pause/resume, saved list
  gui/               the desktop app (Wails); gui/frontend is the Preact interface
  bridge/            how links get from the browser to the app (local server + token)
  cmd/dlm-host/      the extension's helper program (native messaging host)
  extension/         the Chrome/Edge extension
  video/             video pages: yt-dlp, quality choices, the tools download
  mux/               joins video and audio streams into one MP4 (no ffmpeg)
  mkv/               joins WebM/Matroska streams
  hls/               downloads HLS playlists (.m3u8) piece by piece
  mpegts/            converts MPEG-TS (.ts) to MP4, without re-encoding
  cmd/dl/main.go     command-line interface for testing
  cmd/bench/         local test server that logs every request, to compare
                     download managers (IDM, FDM, ours) on the same situations
  learn/             Go notes, one per stage
  installer/windows/ the Windows setup script (NSIS) and its picture and licence page
  devloop.ps1        lets Claude run project commands on this PC (see above)
```

## Licence

Copyright (C) 2026 KLAH

Dream Download Manager is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by the Free
Software Foundation, either version 3 of the License, or (at your option) any
later version. It is distributed in the hope that it will be useful, but
WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
FITNESS FOR A PARTICULAR PURPOSE. See [LICENSE](LICENSE) for the full text.

**Using it in a closed-source product?** The GPL requires anything you
distribute that's built on this code to be open source under the GPL too. If
that doesn't suit you, a separate commercial licence is available - contact
KLAH.

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
