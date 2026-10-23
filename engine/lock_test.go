package engine

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The same link started twice into the same folder: the second must stop
// with ErrAlreadyDownloading, and the first must finish untouched.
func TestSameFileTwiceIsRefused(t *testing.T) {
	for _, conns := range []int{1, 8} {
		s := newFakeServer(4 << 20)
		s.chunkDelay = 5 * time.Millisecond // slow enough to still be running
		d, ts := setup(t, s)
		d.Connections = conns

		started := make(chan struct{})
		var once bool
		d.OnProgress = func(p Progress) {
			if p.Status == StatusDownloading && !once {
				once = true
				close(started)
			}
		}
		result := make(chan error, 1)
		var path string
		go func() {
			var err error
			path, err = d.Run(context.Background())
			result <- err
		}()
		<-started

		d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: conns}
		if _, err := d2.Run(context.Background()); !errors.Is(err, ErrAlreadyDownloading) {
			t.Errorf("%d conn: second download should be refused, got %v", conns, err)
		}
		if err := <-result; err != nil {
			t.Fatalf("%d conn: first download failed: %v", conns, err)
		}
		checkFile(t, path, s.data)
		if exists(lockPath(path + ".part")) {
			t.Errorf("%d conn: lock file left behind", conns)
		}
	}
}

// Two DIFFERENT files that happen to have the same name, at the same time:
// the second gets a numbered name, and both are correct.
func TestDifferentFilesSameNameAtOnce(t *testing.T) {
	a := newFakeServer(2 << 20)
	a.chunkDelay = 5 * time.Millisecond
	da, _ := setup(t, a)

	b := newFakeServer(2 << 20)
	tsB := httptest.NewServer(b)
	t.Cleanup(func() { close(b.block); tsB.Close() })
	db := &Download{URL: tsB.URL + "/file.bin", Folder: da.Folder, Connections: 1}

	started := make(chan struct{})
	var once bool
	da.OnProgress = func(p Progress) {
		if p.Status == StatusDownloading && !once {
			once = true
			close(started)
		}
	}
	result := make(chan error, 1)
	var pathA string
	go func() {
		var err error
		pathA, err = da.Run(context.Background())
		result <- err
	}()
	<-started

	pathB, err := db.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	checkFile(t, pathA, a.data)
	checkFile(t, pathB, b.data)
	if filepath.Base(pathA) != "file.bin" || filepath.Base(pathB) != "file (1).bin" {
		t.Errorf("expected file.bin and file (1).bin, got %s and %s", filepath.Base(pathA), filepath.Base(pathB))
	}
}

// A lock left behind by a crash must not block the file forever.
func TestStaleLockIsTakenOver(t *testing.T) {
	s := newFakeServer(4 << 20)
	s.stallAfter = 300_000
	s.stallTimes = 1
	d, ts := setup(t, s)
	d.StallTimeout = 10 * time.Second
	if _, err := d.Run(cancelSoon(d)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}

	// Pretend the download crashed: its lock is still there, untouched for a minute.
	lp := lockPath(filepath.Join(d.Folder, "file.bin.part"))
	os.WriteFile(lp, []byte("pid 1\n"), 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(lp, old, old)

	d2 := &Download{URL: ts.URL + "/file.bin", Folder: d.Folder, Connections: 1}
	var resumed bool
	d2.OnProgress = func(p Progress) { resumed = resumed || p.Resumed }
	path, err := d2.Run(context.Background())
	if err != nil {
		t.Fatalf("a stale lock blocked the download: %v", err)
	}
	checkFile(t, path, s.data)
	if !resumed {
		t.Error("should have resumed the crashed download")
	}
}

func TestLockHeartbeatKeepsLockAlive(t *testing.T) {
	oldBeat, oldStale := lockHeartbeat, staleLockAfter
	lockHeartbeat, staleLockAfter = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { lockHeartbeat, staleLockAfter = oldBeat, oldStale })

	part := filepath.Join(t.TempDir(), "x.part")
	l, err := acquireLock(part)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // well past staleLockAfter
	if !lockHeld(part) {
		t.Error("a live lock went stale: the heartbeat isn't refreshing it")
	}
	if _, err := acquireLock(part); !errors.Is(err, errLocked) {
		t.Errorf("second acquire should fail while held, got %v", err)
	}
	l.release()
	l.release() // twice is safe
	if exists(lockPath(part)) {
		t.Error("release didn't remove the lock")
	}
}
