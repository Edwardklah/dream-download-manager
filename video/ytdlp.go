package video

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// options are the yt-dlp options for a page: our tools, plus the browser's
// cookies and identity if we have them. cleanup deletes the temporary
// cookies file.
func (b Backend) options(pageURL string, headers map[string]string) (Options, func(), error) {
	o := b.Tools.Options()
	cleanup := func() {}
	o.UserAgent = headers["User-Agent"]
	if cookie := headers["Cookie"]; cookie != "" {
		path, err := writeCookieFile(pageURL, cookie)
		if err != nil {
			return o, cleanup, err
		}
		o.Cookies = path
		cleanup = func() { os.Remove(path) }
	}
	return o, cleanup, nil
}

// writeCookieFile turns the browser's cookies for a page ("a=1; b=2") into
// the "cookies.txt" file yt-dlp reads (the old Netscape format: one line
// per cookie - domain, subdomains too?, path, HTTPS only?, expiry, name,
// value). It's readable by this user only and deleted after use: cookies
// are as good as your password for the site.
func writeCookieFile(pageURL, cookie string) (string, error) {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "", err
	}
	domain := "." + strings.TrimPrefix(u.Hostname(), "www.")
	secure := "FALSE" // cookies marked "HTTPS only" aren't sent to http:// pages
	if u.Scheme == "https" {
		secure = "TRUE"
	}
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n")
	for _, part := range strings.Split(cookie, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || strings.ContainsAny(name+value, "\t\n\r") {
			continue
		}
		fmt.Fprintf(&b, "%s\tTRUE\t/\t%s\t0\t%s\t%s\n", domain, secure, name, value)
	}
	f, err := os.CreateTemp("", "dlmanager-cookies-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	os.Chmod(f.Name(), 0o600)
	if _, err := f.WriteString(b.String()); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// YtDlpProgress reports a yt-dlp download.
type YtDlpProgress func(done, total int64, speed float64)

// YtDlpDownload lets yt-dlp download format spec of a page into out - the
// fallback for formats our own downloader can't handle. yt-dlp keeps a
// .part file and continues it if run again (pause and resume work).
func (b Backend) YtDlpDownload(ctx context.Context, pageURL, spec, out string, headers map[string]string, progress YtDlpProgress) error {
	if !b.Tools.Installed() {
		return ErrNoTools
	}
	o, cleanup, err := b.options(pageURL, headers)
	if err != nil {
		return err
	}
	defer cleanup()

	args := []string{
		"--ignore-config", "--no-playlist", "--no-warnings", "--newline", "--no-mtime",
		"-f", spec,
		// yt-dlp reads "%(...)s" in -o as placeholders: double any % in the path.
		"-o", strings.ReplaceAll(out, "%", "%%"),
		"--concurrent-fragments", "4", // pieces at once, for piece formats
		"--progress-template", "download:DLM %(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s %(progress.speed)s",
	}
	args = append(args, o.args()...)
	args = append(args, "--", pageURL)

	cmd := exec.CommandContext(ctx, b.Tools.YtDlp(), args...)
	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if done, total, speed, ok := parseProgress(sc.Text()); ok && progress != nil {
			progress(done, total, speed)
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ytdlpError(stderr.String(), err)
	}
	if _, err := os.Stat(out); err != nil {
		return fmt.Errorf("yt-dlp finished, but the file isn't there: %w", err)
	}
	return nil
}

// parseProgress reads one of our progress lines:
// "DLM <downloaded> <total> <estimate> <speed>" ("NA" where unknown).
func parseProgress(line string) (done, total int64, speed float64, ok bool) {
	f := strings.Fields(line)
	if len(f) != 5 || f[0] != "DLM" {
		return 0, 0, 0, false
	}
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return -1
		}
		return v
	}
	done = int64(num(f[1]))
	total = int64(num(f[2]))
	if total <= 0 {
		total = int64(num(f[3]))
	}
	speed = max(num(f[4]), 0)
	return max(done, 0), total, speed, done >= 0
}
