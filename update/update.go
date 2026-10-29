// Package update keeps Dream DM itself up to date, from its GitHub releases.
//
// A release is made by pushing a version tag (v0.9.2): GitHub builds it
// (.github/workflows/build.yml) and attaches the Windows setup file and its
// SHA-256 checksum. The app asks GitHub for the newest release now and then
// (Latest: one small request, no download), and when there's a newer one
// and you say so, downloads the setup file, checks it against the checksum
// (Fetch), and runs it.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases come from. (A variable so tests can point it at
// a fake GitHub.)
var Repo = "https://github.com/Edwardklah/dream-download-manager"

// ReleasePage is the page about a release, for "What's new".
func ReleasePage(version string) string { return Repo + "/releases/tag/v" + version }

// SetupName is the Windows setup file of a version.
func SetupName(version string) string { return "DreamDM-Setup-" + version + ".exe" }

// Latest is the newest release's version, like "0.9.2". GitHub answers
// /releases/latest with a redirect to the release's tag, so this is one
// small request with nothing to download.
func Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // the redirect itself is the answer
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	_, tag, ok := strings.Cut(resp.Header.Get("Location"), "/releases/tag/")
	v := strings.TrimPrefix(path.Base(tag), "v")
	if !ok || parse(v) == nil {
		return "", fmt.Errorf("no release found (%s)", resp.Status)
	}
	return v, nil
}

// Newer: is version a later than b? Versions are "major.minor.patch".
func Newer(a, b string) bool {
	x, y := parse(a), parse(b)
	if x == nil || y == nil {
		return false
	}
	for i := range 3 {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func parse(v string) []int {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	n := make([]int, 3)
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return nil
		}
		n[i] = x
	}
	return n
}

// ErrChecksum: the downloaded setup file isn't the one the release lists.
var ErrChecksum = errors.New("the downloaded update didn't match its checksum, so it wasn't installed")

// Fetch downloads version's Windows setup file into dir and checks it
// against the release's checksum file (<setup>.sha256). It returns the
// file's path; a file that doesn't match is deleted, never returned.
// progress (may be nil) is told bytes so far and the total.
func Fetch(ctx context.Context, version, dir string, progress func(done, total int64)) (string, error) {
	name := SetupName(version)
	base := Repo + "/releases/download/v" + version + "/"
	sum, err := get(ctx, base+name+".sha256", 4096, nil)
	if err != nil {
		return "", fmt.Errorf("couldn't get the update's checksum: %w", err)
	}
	want := strings.ToLower(strings.Fields(string(sum) + " ")[0])
	if len(want) != 64 {
		return "", errors.New("the update's checksum file is not what was expected")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, name)
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = get(ctx, base+name, 200<<20, func(body io.Reader, total int64) error {
		var done int64
		buf := make([]byte, 64<<10)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					return werr
				}
				h.Write(buf[:n])
				done += int64(n)
				if progress != nil {
					progress(done, total)
				}
			}
			if rerr == io.EOF {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("couldn't download the update: %w", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		os.Remove(tmp)
		return "", ErrChecksum
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

// get fetches url. With read nil it returns the body (at most limit
// bytes); otherwise read consumes it.
func get(ctx context.Context, url string, limit int64, read func(io.Reader, int64) error) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	if resp.ContentLength > limit {
		return nil, errors.New("too big")
	}
	body := io.LimitReader(resp.Body, limit)
	if read != nil {
		return nil, read(body, resp.ContentLength)
	}
	return io.ReadAll(body)
}
