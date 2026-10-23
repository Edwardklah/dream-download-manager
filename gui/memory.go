package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

// releaseMemorySoon runs after the window closes. Go normally keeps freed
// memory in reserve for a while, in case it's needed again. With the window
// gone, we know it won't be, so we collect garbage and return the free memory
// to Windows straight away.
func releaseMemorySoon() {
	time.Sleep(2 * time.Second) // let the window finish tearing down
	debug.FreeOSMemory()        // runs the garbage collector, then returns free memory
}

// writeDiagnostics writes Go's own memory statistics to a file every 2
// seconds. Used only when the DLM_DIAG environment variable is set, to find
// out how much of the app's memory is Go's and how much isn't.
func writeDiagnostics(path string) {
	start := time.Now()
	for {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		text := fmt.Sprintf(
			"uptime        %v\n"+
				"heap in use   %.1f MB   (live Go objects)\n"+
				"heap idle     %.1f MB   (free, kept by Go)\n"+
				"heap released %.1f MB   (free, given back to Windows)\n"+
				"stacks        %.1f MB\n"+
				"go total      %.1f MB   (everything Go has reserved from Windows)\n"+
				"goroutines    %d\n"+
				"gc runs       %d\n",
			time.Since(start).Round(time.Second),
			mb(m.HeapInuse), mb(m.HeapIdle), mb(m.HeapReleased),
			mb(m.StackSys), mb(m.Sys-m.HeapReleased),
			runtime.NumGoroutine(), m.NumGC)
		os.WriteFile(path, []byte(text), 0o644)
		time.Sleep(2 * time.Second)
	}
}

func mb(b uint64) float64 { return float64(b) / (1 << 20) }
