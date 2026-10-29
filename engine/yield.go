package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Giving way to a video look-up.
//
// Looking a video up fetches a few MB (for YouTube: the page and a 2-3 MB
// player script). With downloads running flat out over many connections,
// those requests got a small share of the line: a look-up that takes ~8 s
// on a quiet line took 45 s. While a look-up runs (Yield), every download
// slows to a trickle (yieldRate for all of them together), so the look-up
// gets the line; they speed up again as soon as it's done. Connections stay
// open, so nothing restarts.

// yieldRate: bytes per second, for all downloads together, while yielding.
const yieldRate = 64 << 10

var (
	yielders  atomic.Int32
	yieldMu   sync.Mutex
	yieldNext time.Time // when the next read may go ahead
)

// Yield slows all downloads until the returned function is called.
// Several yields at once are fine; downloads speed up after the last ends.
func Yield() (done func()) {
	yielders.Add(1)
	var once sync.Once
	return func() { once.Do(func() { yielders.Add(-1) }) }
}

// Yielding: is a look-up running?
func Yielding() bool { return yielders.Load() > 0 }

// GiveWay is called after each read of n bytes. While yielding it waits
// long enough to keep all downloads together near yieldRate; otherwise it
// returns at once.
func GiveWay(ctx context.Context, n int) {
	if !Yielding() || n <= 0 {
		return
	}
	yieldMu.Lock()
	now := time.Now()
	if yieldNext.Before(now) {
		yieldNext = now
	}
	wait := yieldNext.Sub(now)
	yieldNext = yieldNext.Add(time.Duration(n) * time.Second / yieldRate)
	yieldMu.Unlock()
	if wait <= 0 {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case <-time.After(100 * time.Millisecond):
			if !Yielding() { // the look-up finished: carry on now
				return
			}
		}
	}
}
