# 06 - The browser extension

Goal: click a download in Chrome or Edge, and it lands in dlmanager instead
of the browser's own downloader - the way IDM works.

## The path a link takes

```
 Chrome extension  ──native messaging──▶  dlm-host.exe  ──HTTP, 127.0.0.1──▶  the app
 (extension/)          (stdin/stdout)      (cmd/dlm-host)    + secret token     (gui, bridge/)
```

Three pieces, because a browser extension is locked in a sandbox: it can't
open files, start programs, or talk to the network freely. Browsers give
extensions exactly one sanctioned door to a program on your computer:
**native messaging**.

### 1. Native messaging (extension ⇄ dlm-host)

- The extension calls `chrome.runtime.sendNativeMessage("com.dlmanager.host", {...})`.
- The browser looks up `com.dlmanager.host` in the registry
  (`HKCU\Software\Google\Chrome\NativeMessagingHosts\...`), which points to a
  small JSON "host manifest". That names the program to start
  (`dlm-host.exe`) and which extension IDs may start it.
- The browser starts the program and writes the message to its **stdin**:
  a 4-byte length, then JSON. The program answers on **stdout** the same way,
  and exits. (`bridge/native.go`)

Security comes from the browser: only an extension whose ID is in
`allowed_origins` can start the host. Our ID is fixed by the `"key"` in
`extension/manifest.json` - an extension's ID is derived from its public key,
so the same key always gives the same ID, even when loaded unpacked from a
folder. (The private key isn't kept: it's only needed to publish a packaged
extension.)

`dlm-host -install` writes the host manifest and the registry entries for
Chrome and Edge. Only `HKEY_CURRENT_USER` - your own settings - so no admin
rights.

### 2. The bridge (dlm-host ⇄ the app)

dlm-host is started fresh for every message, but the app is a long-running
program. They meet through a tiny web server inside the app
(`bridge/bridge.go`):

- it listens on `127.0.0.1` - reachable only from this computer - on a port
  the system picks;
- it writes the port and a **random 32-byte token** to
  `%AppData%\dlmanager\bridge.json`;
- dlm-host reads that file and sends the token with each request.

Why the token matters: **any web page can make your browser send requests to
127.0.0.1**. Without a secret, a malicious page could quietly add downloads.
A page can't read files on your disk, so it can't learn the token. As a
second lock, browsers add an `Origin` header to requests made by pages, and
dlm-host never does, so the app refuses any request that has one.

Go points:

- `subtle.ConstantTimeCompare` compares the token in the same time whether
  the first byte or the last is wrong - otherwise an attacker could time the
  answers and guess it byte by byte.
- `net.Listen("tcp", "127.0.0.1:0")` - port 0 asks the system for any free
  port; `ln.Addr()` tells you which one you got.
- Patterns like `"POST /add"` in `http.ServeMux` (Go 1.22+) match the method
  too.

If the app isn't running, dlm-host starts it (`gui.exe --tray`) and waits for
`bridge.json` to appear. On Windows the browser runs dlm-host inside a "job"
(processes it can end together), so the app is started with
`CREATE_BREAKAWAY_FROM_JOB` to keep running after dlm-host exits
(`cmd/dlm-host/setup_windows.go`). Files ending in `_windows.go` are only
compiled on Windows; `setup_other.go` has `//go:build !windows` so the
package still builds - and its tests run - elsewhere.

### 3. The extension (`extension/`)

Manifest V3: the background part is a **service worker** (`background.js`)
that the browser starts when an event arrives and stops when idle - so
nothing is kept in variables between events except by chance; settings live
in `chrome.storage`.

Catching a download:

1. `chrome.downloads.onCreated` fires when the browser starts a download.
2. We **cancel** it at once and erase it from the browser's list.
3. We send the link to dlmanager. If dlmanager can't take it (not set up, an
   error), we start the download in the browser again
   (`chrome.downloads.download`), so it's never lost.

(The first version paused the browser's download and only cancelled it once
dlmanager answered. But dlmanager may need a few seconds to start, and the
browser's paused copy sat in its list meanwhile: it looked like both were
downloading. Cancel first, retry on failure, is cleaner.)

### Coming to the front

Windows doesn't let a program in the background push its window in front of
the one you're using - otherwise any program could steal your typing. The
program you're using (Chrome) *can* hand that right on, and so can a program
it started: dlm-host calls `AllowSetForegroundWindow(ASFW_ANY)` before
talking to the app. As a backup the app makes its window "always on top" for
0.7 s, which puts it in front without taking the focus.

Skipped on purpose: private (incognito) windows, `blob:`/`data:` addresses
(only the browser has those - they're made by the page itself), and
downloads started by other extensions.

### Cookies, referrer, user agent

Many downloads only work for the logged-in browser: a file in your Google
Drive, a course page, a router's admin page. The server recognises you by
your **cookies**. So the extension sends, with each link:

- the cookies the browser would send to that address (`chrome.cookies`),
- the **Referer** - the page the link was on (some servers refuse "hotlinked"
  downloads without it),
- the browser's **User-Agent**, so the server treats dlmanager like the
  browser.

The engine already sends any extra headers it's given (`Download.Headers`).
The manager keeps them with the download so it can still resume after a
restart, in `downloads.json`, which is now saved readable by your user only.
They're never sent to the window: cookies are as good as a password for the
site, so they go only where they're needed.

## Testing without a browser

`cmd/dlm-host/main_test.go` runs the whole path except the browser itself: a
real bridge server, the host's message handling, and checks that non-web
links (`file:`, `javascript:`) are refused. `bridge/bridge_test.go` checks
that requests without the token, with a wrong one, or from a web page are all
refused.
