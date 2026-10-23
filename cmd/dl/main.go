// Command dl is a temporary command-line interface for testing the engine.
// The real desktop app (Wails + React) comes in stage 5.
//
// Usage:
//
//	go run ./cmd/dl [-c 8] [-stop-after 10s] <url> [folder]
//	go run ./cmd/dl -limits                     (servers with remembered limits)
//	go run ./cmd/dl -set-limit proof.ovh.net=3  (set one yourself)
//	go run ./cmd/dl -forget proof.ovh.net
//
// Press Ctrl+C to cancel; the .part file is kept. Run the same command again
// to resume. -stop-after cancels automatically, as if you pressed Ctrl+C,
// which is handy for testing resume. -c sets the number of connections
// (default 8; -c 1 forces a single connection).
//
// GO NOTE: "package main" plus a "func main()" makes a runnable program.
// Every other package (like engine) is a library.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dlmanager/engine" // our own package: module name (go.mod) + folder
)

func main() {
	// GO NOTE: the flag package parses options like "-stop-after 10s".
	// flag.Duration understands "10s", "2m", "1h30m". Options must come
	// before the URL; flag.Args() is whatever is left after them.
	stopAfter := flag.Duration("stop-after", 0, "cancel after this long, e.g. 10s (for testing resume)")
	connections := flag.Int("c", 8, "number of connections (1-32)")
	showLimits := flag.Bool("limits", false, "show the servers whose connection limits are remembered, and exit")
	setLimit := flag.String("set-limit", "", "remember a limit yourself, e.g. -set-limit proof.ovh.net=3, and exit")
	forget := flag.String("forget", "", "forget a server's remembered limit, e.g. -forget proof.ovh.net, and exit")
	noMemory := flag.Bool("no-memory", false, "don't use or update remembered server limits")
	flag.Usage = func() {
		fmt.Println("Usage: dl [options] <url> [folder]")
		fmt.Println("       dl -limits | -set-limit host=N | -forget host")
		flag.PrintDefaults()
	}
	flag.Parse()

	// The table of servers that limit connections (see engine/hostlimits.go).
	var limits *engine.HostLimits
	if !*noMemory {
		path, err := engine.DefaultHostLimitsPath()
		if err == nil {
			limits, err = engine.LoadHostLimits(path)
		}
		if err != nil {
			fmt.Println("Warning: can't load remembered server limits:", err)
			limits = nil
		}
	}
	if *showLimits || *setLimit != "" || *forget != "" {
		os.Exit(manageLimits(limits, *showLimits, *setLimit, *forget))
	}

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	url := flag.Arg(0)
	folder := flag.Arg(1) // "" if not given
	if folder == "" {
		home, _ := os.UserHomeDir() // "_" throws away a return value we don't need
		folder = filepath.Join(home, "Downloads")
	}

	// A context that is cancelled when you press Ctrl+C.
	// The engine watches it and stops cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *stopAfter > 0 {
		// GO NOTE: stopAfter is a POINTER (flag fills it in during Parse),
		// so we read its value with *stopAfter.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *stopAfter)
		defer cancel()
	}

	// When output goes to a file instead of the screen, redrawing the bar
	// with "\r" makes a mess, so print a plain line every 2 seconds instead.
	plainOutput = !isTerminal(os.Stdout)

	dl := &engine.Download{
		URL:         url,
		Folder:      folder,
		Connections: *connections,
		Limits:      limits,
		OnProgress:  show,
	}

	start := time.Now()
	path, err := dl.Run(ctx)

	if dl.Info != nil {
		answer := "no"
		if dl.Info.SupportsRanges {
			answer = "yes"
		}
		fmt.Println("Server supports ranges (resume / multi-connection):", answer)
	}
	fmt.Println("Time:", humanTime(time.Since(start)))
	if err != nil {
		os.Exit(1)
	}
	fmt.Println("Saved to", path)
}

// manageLimits handles -limits, -set-limit and -forget. Returns an exit code.
func manageLimits(limits *engine.HostLimits, show bool, set, forget string) int {
	if limits == nil {
		fmt.Println("Server memory isn't available (see the warning above, or remove -no-memory).")
		return 1
	}
	if set != "" {
		host, n, ok := strings.Cut(set, "=")
		count, err := strconv.Atoi(n)
		if !ok || host == "" || err != nil || count < 1 {
			fmt.Println("Use -set-limit host=N, e.g. -set-limit proof.ovh.net=3")
			return 1
		}
		if err := limits.Set(host, count); err != nil {
			fmt.Println("Couldn't save:", err)
			return 1
		}
		fmt.Printf("Saved: %s - at most %d connections\n", host, min(count, 32))
	}
	if forget != "" {
		if err := limits.Forget(forget); err != nil {
			fmt.Println("Couldn't save:", err)
			return 1
		}
		fmt.Println("Forgot", forget)
	}
	if show {
		names, all := limits.All()
		if len(names) == 0 {
			fmt.Println("No servers remembered yet.")
		}
		for _, name := range names {
			l := all[name]
			how := "learned"
			if l.Manual {
				how = "set by you"
			}
			fmt.Printf("%-30s %2d connections  (%s, %s)\n", name, l.MaxConnections, how, l.Updated.Format("2006-01-02 15:04"))
		}
	}
	return 0
}

// isTerminal reports whether f is a console window rather than a file or pipe.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

var (
	plainOutput      bool
	lastPlainLine    time.Time
	lastProblemShown string
)

// resumeShown makes sure "Resuming from..." is printed only once.
// GO NOTE: a variable declared outside any function is package-level
// (global to this package).
var resumeShown, limitShown bool

// show prints one progress update. The engine calls it up to 10x a second.
func show(p engine.Progress) {
	// GO NOTE: switch in Go doesn't fall through to the next case,
	// so there's no need for "break" like in C or JavaScript.
	switch p.Status {
	case engine.StatusProbing:
		fmt.Println("Checking file...")

	case engine.StatusRetrying:
		fmt.Printf("\nProblem: %v\nRetrying in %s (attempt %d of %d)...\n",
			p.Err, p.RetryIn, p.Attempt, p.MaxRetries)

	case engine.StatusDownloading:
		if p.Resumed && !resumeShown {
			resumeShown = true
			fmt.Printf("Resuming from %s\n", humanSize(p.ResumedAt))
		}
		if p.HostLimit > 0 && !limitShown {
			limitShown = true
			how := "remembered from an earlier download"
			if p.HostLimitManual {
				how = "your setting"
			}
			fmt.Printf("Using %d connections: this server limits connections (%s)\n", p.HostLimit, how)
		}
		var line string
		if pct := p.Percent(); pct >= 0 {
			filled := int(pct / 100 * 30)
			bar := strings.Repeat("#", filled) + strings.Repeat("-", 30-filled)
			line = fmt.Sprintf("[%s] %5.1f%%  %s/%s  %s/s  ETA %s",
				bar, pct, humanSize(p.Downloaded), humanSize(p.Total),
				humanSize(int64(p.Speed)), humanTime(p.ETA))
			if p.Connections > 1 {
				line += fmt.Sprintf("  conns %d/%d", p.Active, p.Connections)
			}
			if p.Splits > 0 {
				line += fmt.Sprintf("  splits %d", p.Splits)
			}
			if p.Retrying > 0 {
				line += fmt.Sprintf("  retrying %d", p.Retrying)
			}
			// Print each new connection problem once, on its own line.
			if p.LastProblem != "" && p.LastProblem != lastProblemShown {
				lastProblemShown = p.LastProblem
				if !plainOutput {
					fmt.Println()
				}
				fmt.Println("Connection problem:", p.LastProblem)
			}
		} else {
			line = fmt.Sprintf("%s received  %s/s", humanSize(p.Downloaded), humanSize(int64(p.Speed)))
		}
		if plainOutput {
			if time.Since(lastPlainLine) >= 2*time.Second {
				lastPlainLine = time.Now()
				fmt.Println(line)
			}
			return
		}
		// "\r" returns to the start of the line, so the bar redraws in place.
		fmt.Printf("\r%-90s", line)

	case engine.StatusCompleted:
		fmt.Printf("\nDone: %s (%s)\n", p.Filename, humanSize(p.Downloaded))

	case engine.StatusCancelled:
		fmt.Printf("\nCancelled. Partial file kept as %s.part\n", p.Filename)

	case engine.StatusFailed:
		if errors.Is(p.Err, engine.ErrIncomplete) {
			fmt.Printf("\nIncomplete download: %v\n", p.Err)
		} else {
			fmt.Printf("\nFailed: %v\n", p.Err)
		}
	}
}

func humanSize(n int64) string {
	if n < 0 {
		return "?"
	}
	f := float64(n)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f TB", f)
}

func humanTime(d time.Duration) string {
	if d < 0 {
		return "--:--"
	}
	s := int(d.Seconds())
	h, m, sec := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%02d:%02d", m, sec)
}
