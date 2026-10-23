package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Lock files stop two downloads from writing the same .part file - for
// example the same link started in two windows, or twice in the app.
//
// While a download runs, it holds "<name>.part.lock". Creating that file uses
// O_EXCL ("fail if it already exists"), which the operating system performs as
// one indivisible step, so if two downloads try at the same instant, exactly
// one wins.
//
// A crash (or pulling the plug) leaves the lock file behind, and nothing would
// ever remove it. So the owner "touches" the lock every few seconds to show
// it's still alive, and a lock that hasn't been touched for staleLockAfter is
// treated as abandoned and taken over.

// ErrAlreadyDownloading means this exact file is already being downloaded into
// the same folder by another download that's still running.
var ErrAlreadyDownloading = errors.New("this file is already being downloaded into this folder (in another window?)")

var (
	lockHeartbeat  = 5 * time.Second  // how often a running download touches its lock
	staleLockAfter = 20 * time.Second // a lock older than this is abandoned
)

// partLock is a held lock. Call release() when the download ends.
type partLock struct {
	path string
	stop chan struct{}
	once sync.Once
}

func lockPath(partPath string) string { return partPath + ".lock" }

// lockHeld reports whether another live download holds the lock for partPath.
func lockHeld(partPath string) bool {
	info, err := os.Stat(lockPath(partPath))
	return err == nil && time.Since(info.ModTime()) < staleLockAfter
}

// acquireLock takes the lock for partPath. It returns errLocked if another
// live download holds it.
func acquireLock(partPath string) (*partLock, error) {
	path := lockPath(partPath)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid %d\n", os.Getpid()) // just for people who open it
			f.Close()
			l := &partLock{path: path, stop: make(chan struct{})}
			// The interval is read HERE and passed in, rather than read inside
			// the new goroutine, so the goroutine never touches shared state.
			go l.heartbeat(lockHeartbeat)
			return l, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, &diskError{err}
		}
		// Someone has it. Alive, or left behind by a crash?
		if lockHeld(partPath) {
			return nil, errLocked
		}
		os.Remove(path) // abandoned: clear it and try once more
	}
	return nil, errLocked
}

var errLocked = errors.New("locked by another download")

// heartbeat touches the lock file until release() is called.
func (l *partLock) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			now := time.Now()
			os.Chtimes(l.path, now, now)
		}
	}
}

// release stops the heartbeat and deletes the lock. Safe to call twice.
func (l *partLock) release() {
	if l == nil {
		return
	}
	// GO NOTE: sync.Once runs the function the first time only - so a second
	// release() doesn't close an already-closed channel (which would crash).
	l.once.Do(func() {
		close(l.stop)
		os.Remove(l.path)
	})
}

// DiscardPartial deletes the leftovers of an unfinished download of
// folder/filename: its .part, .part.meta and any abandoned .part.lock. It
// refuses (ErrAlreadyDownloading) if a running download still holds the lock,
// and never touches the finished file itself.
func DiscardPartial(folder, filename string) error {
	if filename == "" {
		return nil
	}
	part := filepath.Join(folder, filename+".part")
	if lockHeld(part) {
		return ErrAlreadyDownloading
	}
	for _, p := range []string{part, metaPath(part), lockPath(part)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &diskError{err}
		}
	}
	return nil
}
