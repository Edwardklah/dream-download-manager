package engine

import (
	"context"
	"testing"
	"time"
)

func TestGiveWay(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	GiveWay(ctx, 1<<20) // not yielding: no wait
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("waited without a look-up running")
	}

	done := Yield()
	GiveWay(ctx, yieldRate/2) // first read goes ahead
	start = time.Now()
	GiveWay(ctx, yieldRate/2) // the next waits its turn: ~0.5 s
	if el := time.Since(start); el < 400*time.Millisecond || el > 900*time.Millisecond {
		t.Errorf("waited %v while yielding, want ~0.5 s", el)
	}

	// The look-up ends while a read is waiting: it goes ahead at once.
	GiveWay(ctx, yieldRate*5) // books 5 s
	go func() { time.Sleep(200 * time.Millisecond); done() }()
	start = time.Now()
	GiveWay(ctx, 1)
	if el := time.Since(start); el > time.Second {
		t.Errorf("still waited %v after the look-up ended", el)
	}
	if Yielding() {
		t.Error("still yielding")
	}
	done() // twice is harmless
	if yielders.Load() != 0 {
		t.Error("count went negative")
	}
}
