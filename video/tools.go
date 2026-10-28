package video

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"dlmanager/engine"
)

// Tools are the helper programs video downloads need, kept in their own
// folder (%AppData%\dlmanager\tools; on Linux ~/.config/dlmanager/tools):
//
//	yt-dlp(.exe)  reads video pages                       ~17 MB (Linux ~39 MB)
//	qjs(.exe)     QuickJS-ng, a small JavaScript engine   ~2 MB
//
// YouTube hides its stream addresses behind JavaScript puzzles, and since
// late 2025 yt-dlp needs a separate JavaScript program to solve them. It
// supports Deno (~40 MB), Node and QuickJS; we fetch QuickJS because it's
// tiny. If Deno or Node is already installed, yt-dlp uses that instead.
type Tools struct {
	Dir string
}

// Where the tools come from: the projects' own GitHub releases.
var (
	ytdlpRepo   = "https://github.com/yt-dlp/yt-dlp"
	ytdlpSums   = "SHA2-256SUMS"
	quickjsRepo = "https://github.com/quickjs-ng/quickjs"

	// The files for this computer, by the names in the releases.
	ytdlpAsset, quickjsAsset = toolAssets(runtime.GOOS, runtime.GOARCH)
)

// toolAssets: which release files fit a system. yt-dlp's Linux files are
// standalone programs (Python included), so nothing else is needed.
// Windows gets the x64 files, which also run on Windows on Arm.
func toolAssets(goos, arch string) (ytdlp, quickjs string) {
	switch goos + "/" + arch {
	case "linux/amd64":
		return "yt-dlp_linux", "qjs-linux-x86_64"
	case "linux/arm64":
		return "yt-dlp_linux_aarch64", "qjs-linux-aarch64"
	case "darwin/amd64", "darwin/arm64":
		return "yt-dlp_macos", "qjs-darwin-" + map[string]string{"amd64": "x86_64", "arm64": "arm64"}[arch]
	default:
		return "yt-dlp.exe", "qjs-windows-x86_64.exe"
	}
}

func (t Tools) YtDlp() string   { return filepath.Join(t.Dir, exeName("yt-dlp")) }
func (t Tools) QuickJS() string { return filepath.Join(t.Dir, exeName("qjs")) }

func exeName(name string) string {
	if os.PathSeparator == '\\' {
		return name + ".exe"
	}
	return name
}

// Installed reports whether yt-dlp is there (the JavaScript engine is
// optional: Deno or Node may be installed already).
func (t Tools) Installed() bool {
	_, err := os.Stat(t.YtDlp())
	return err == nil
}

// Options for Inspect, using these tools.
func (t Tools) Options() Options {
	// yt-dlp keeps YouTube's player script and the puzzles it solved in
	// its cache; a folder of our own next to the tools makes sure that
	// cache is there and kept. Measured: a look-up went from ~20 s to ~8 s
	// once the script was cached.
	o := Options{YtDlp: t.YtDlp(), CacheDir: filepath.Join(filepath.Dir(t.Dir), "cache")}
	// Deno is used automatically if installed. Node and QuickJS must be
	// switched on (yt-dlp's caution: they're less locked down than Deno).
	if node, err := exec.LookPath("node"); err == nil {
		o.JSArgs = append(o.JSArgs, "--js-runtimes", "node:"+node)
	}
	if _, err := os.Stat(t.QuickJS()); err == nil {
		o.JSArgs = append(o.JSArgs, "--js-runtimes", "quickjs:"+t.QuickJS())
	}
	return o
}

// Version is yt-dlp's version, like "2026.10.21" ("" if not installed).
func (t Tools) Version(ctx context.Context) string {
	if !t.Installed() {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.YtDlp(), "--version")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// InstallProgress reports a tool download: which file, bytes so far, total.
type InstallProgress func(file string, done, total int64)

// Install downloads yt-dlp (checked against its published SHA-256) and
// QuickJS, or updates them if newer versions exist. Safe to run again: an
// up-to-date tool isn't downloaded twice.
func (t Tools) Install(ctx context.Context, progress InstallProgress) error {
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	tag, err := latestTag(ctx, ytdlpRepo)
	if err != nil {
		return fmt.Errorf("couldn't find the latest yt-dlp: %w", err)
	}
	if t.Version(ctx) != tag {
		sums, err := fetchSums(ctx, releaseURL(ytdlpRepo, tag, ytdlpSums))
		if err != nil {
			return fmt.Errorf("couldn't get yt-dlp's checksums: %w", err)
		}
		want, ok := sums[ytdlpAsset]
		if !ok {
			return fmt.Errorf("yt-dlp's checksums don't list %s", ytdlpAsset)
		}
		if err := t.fetch(ctx, releaseURL(ytdlpRepo, tag, ytdlpAsset), t.YtDlp(), want, progress); err != nil {
			return fmt.Errorf("yt-dlp: %w", err)
		}
	}

	if _, err := os.Stat(t.QuickJS()); err != nil {
		qtag, err := latestTag(ctx, quickjsRepo)
		if err != nil {
			return fmt.Errorf("couldn't find the latest QuickJS: %w", err)
		}
		// QuickJS-ng publishes no checksum file; the download is over HTTPS
		// from the project's own GitHub releases.
		if err := t.fetch(ctx, releaseURL(quickjsRepo, qtag, quickjsAsset), t.QuickJS(), "", progress); err != nil {
			return fmt.Errorf("QuickJS: %w", err)
		}
	}
	return nil
}

// fetch downloads url to dest with the engine, checks its SHA-256 if one
// is given, and only then puts it in place.
func (t Tools) fetch(ctx context.Context, url, dest, wantSHA string, progress InstallProgress) error {
	name := filepath.Base(dest)
	tmpName := name + ".download"
	os.Remove(filepath.Join(t.Dir, tmpName)) // a finished leftover from an earlier try
	d := &engine.Download{
		URL: url, Folder: t.Dir, Filename: tmpName, Connections: 4,
		OnProgress: func(p engine.Progress) {
			if progress != nil {
				progress(name, p.Downloaded, p.Total)
			}
		},
	}
	got, err := d.Run(ctx)
	if err != nil {
		return err
	}
	if wantSHA != "" {
		if sum, err := fileSHA256(got); err != nil || !strings.EqualFold(sum, wantSHA) {
			os.Remove(got)
			return errors.New("the download doesn't match its published checksum")
		}
	}
	if err := os.Rename(got, dest); err != nil { // replaces an older version
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(dest, 0o755) // a program: allowed to run
	}
	return nil
}

// latestTag asks GitHub which release is the latest, without its API (which
// limits how often you may ask): /releases/latest redirects to
// /releases/tag/<tag>.
func latestTag(ctx context.Context, repo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // we want the redirect itself
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if _, tag, ok := strings.Cut(loc, "/releases/tag/"); ok && tag != "" {
		return path.Base(tag), nil
	}
	return "", fmt.Errorf("unexpected answer (%s)", resp.Status)
}

func releaseURL(repo, tag, asset string) string {
	return repo + "/releases/download/" + tag + "/" + asset
}

// fetchSums reads a "SHA2-256SUMS" file: lines of "<hash>  <file name>".
func fetchSums(ctx context.Context, url string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	sums := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			sums[strings.TrimPrefix(f[1], "*")] = f[0]
		}
	}
	return sums, sc.Err()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- Keeping yt-dlp up to date ----
//
// Video sites change all the time, and yt-dlp publishes fixes almost every
// week; an old copy quietly stops working on one site after another. So
// the app checks now and then whether a newer release exists - a single
// small request to GitHub - and offers the update. It never downloads the
// ~17 MB on its own: on a metered connection that's your call.

// UpdateCheck is the result of a check.
type UpdateCheck struct {
	Current string    // installed version, "" if not installed
	Latest  string    // newest release, "" if unknown
	Checked time.Time // when GitHub was last asked
}

// Newer: a release newer than the installed one exists. (yt-dlp's versions
// are dates, "2026.10.22", optionally ".1" - they compare as text.)
func (u UpdateCheck) Newer() bool {
	return u.Current != "" && u.Latest != "" && u.Latest > u.Current
}

const updateFile = "update-check.json"

// CheckForUpdate compares the installed yt-dlp with the latest release.
// GitHub is only asked if the last check is older than every; otherwise the
// remembered answer is used.
func (t Tools) CheckForUpdate(ctx context.Context, every time.Duration) (UpdateCheck, error) {
	u := UpdateCheck{Current: t.Version(ctx)}
	var saved struct {
		Checked time.Time `json:"checked"`
		Latest  string    `json:"latest"`
	}
	path := filepath.Join(t.Dir, updateFile)
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &saved)
	}
	if time.Since(saved.Checked) < every && saved.Latest != "" {
		u.Latest, u.Checked = saved.Latest, saved.Checked
		return u, nil
	}
	tag, err := latestTag(ctx, ytdlpRepo)
	if err != nil {
		u.Latest, u.Checked = saved.Latest, saved.Checked
		return u, err
	}
	saved.Checked, saved.Latest = time.Now(), tag
	if data, err := json.Marshal(saved); err == nil {
		os.MkdirAll(t.Dir, 0o755)
		os.WriteFile(path, data, 0o644)
	}
	u.Latest, u.Checked = tag, saved.Checked
	return u, nil
}
