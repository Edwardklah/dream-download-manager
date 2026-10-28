// Command vtime times a video look-up, step by step, with the app's own
// tools: how long yt-dlp takes just to start, and how long a full look-up
// takes (the wait before the quality list appears). For finding what makes
// look-ups slow; it downloads nothing.
//
//	go run ./cmd/vtime https://www.youtube.com/watch?v=...
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"dlmanager/video"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Println("usage: vtime <video page link>")
		os.Exit(2)
	}
	page := os.Args[1]
	cfg, err := os.UserConfigDir()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	t := video.Tools{Dir: filepath.Join(cfg, "dlmanager", "tools")}
	if !t.Installed() {
		fmt.Println("the video tools aren't installed:", t.YtDlp())
		os.Exit(1)
	}
	o := t.Options()
	fmt.Println("yt-dlp:", t.YtDlp())
	fmt.Println("JavaScript:", o.JSArgs)
	if p, err := exec.LookPath("deno"); err == nil {
		fmt.Println("deno found (yt-dlp uses it first):", p)
	}

	// Starting up only: yt-dlp --version, three times (the first can be
	// slower: nothing cached yet, and antivirus scanning).
	for i := 1; i <= 1; i++ {
		start := time.Now()
		out, err := exec.Command(t.YtDlp(), "--version").Output()
		fmt.Printf("start-up %d: %v  (%s, err %v)\n", i, time.Since(start).Round(time.Millisecond), trim(out), err)
	}

	// One full look-up as the app does it.
	for i := 1; i <= 0; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		start := time.Now()
		info, err := video.Inspect(ctx, page, o)
		cancel()
		el := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Printf("look-up %d: %v  error: %v\n", i, el, err)
			continue
		}
		fmt.Printf("look-up %d: %v  (%q, %d formats)\n", i, el, info.Title, len(info.Formats))
	}

	// With a cache folder of our own, twice: does the second run skip
	// downloading YouTube's player script?
	cache := filepath.Join(cfg, "dlmanager", "cache")
	for i := 1; i <= 2; i++ {
		fmt.Printf("\n--- step by step, cache in %s, run %d ---\n", cache, i)
		verbose(t.YtDlp(), o.JSArgs, page, "--cache-dir", cache)
	}
}

// verbose runs the same look-up with yt-dlp's -v and prints each line of
// its log with the time it appeared, to see which step is slow.
func verbose(ytdlp string, js []string, page string, extra ...string) {
	args := append([]string{"-J", "--no-playlist", "--ignore-config", "-v"}, js...)
	args = append(args, extra...)
	args = append(args, "--", page)
	cmd := exec.Command(ytdlp, args...)
	cmd.Stdout = io.Discard
	errp, err := cmd.StderrPipe()
	if err != nil {
		fmt.Println(err)
		return
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Println(err)
		return
	}
	sc := bufio.NewScanner(errp)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if len(line) > 160 {
			line = line[:160] + "..."
		}
		fmt.Printf("%6.2fs  %s\n", time.Since(start).Seconds(), line)
	}
	cmd.Wait()
	fmt.Printf("%6.2fs  (done)\n", time.Since(start).Seconds())
}

func trim(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
