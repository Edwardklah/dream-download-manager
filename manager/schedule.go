package manager

import (
	"fmt"
	"strings"
	"time"
)

// The scheduler holds the queue until a set time - useful for night data
// bundles - and optionally stops it at another:
//
//	Start 00:00, stop 05:00: downloads wait until midnight, run until 5 am,
//	then the unfinished ones stop (keeping their progress) and continue
//	the next night.
//
//	Start 00:00, no stop time: downloads wait until midnight, then run until
//	the queue is empty; anything added after that waits for the next
//	midnight.
//
// "Start now" always skips the wait, as it skips the queue.

// Schedule is part of Settings.
type Schedule struct {
	Enabled bool   `json:"enabled"`
	Start   string `json:"start"` // "HH:MM", local time
	Stop    string `json:"stop"`  // "HH:MM", or "" for "when the queue is empty"
}

// QueueState tells the window whether the queue is waiting for its time.
type QueueState struct {
	Held     bool   `json:"held"`     // queued downloads wait for StartsAt
	StartsAt string `json:"startsAt"` // "00:00"
	StopsAt  string `json:"stopsAt"`  // while running in the window: "05:00", or ""
}

// clock reads "HH:MM" as minutes after midnight (ok=false if malformed).
func clock(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// clean keeps a schedule valid: an unreadable start time switches it off,
// an unreadable stop time means no stop time.
func (s Schedule) clean() Schedule {
	if m, ok := clock(s.Start); ok {
		s.Start = hhmm(m)
	} else {
		s.Start, s.Enabled = "00:00", false
	}
	if s.Stop != "" {
		if m, ok := clock(s.Stop); ok && hhmm(m) != s.Start {
			s.Stop = hhmm(m)
		} else {
			s.Stop = ""
		}
	}
	return s
}

func hhmm(minutes int) string { return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60) }

// lastAt returns the latest moment at or before now when the local clock
// showed hh:mm.
func lastAt(now time.Time, minutes int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), minutes/60, minutes%60, 0, 0, now.Location())
	if t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// inWindow: is now between the start and stop times (which may span
// midnight)? Only meaningful with a stop time.
func (s Schedule) inWindow(now time.Time) bool {
	start, _ := clock(s.Start)
	stop, ok := clock(s.Stop)
	if !ok {
		return false
	}
	// In the window if the last start came after the last stop.
	return lastAt(now, start).After(lastAt(now, stop))
}

// crossed: did the clock pass hh:mm after `since`, up to now?
func crossed(since, now time.Time, hhmm string) bool {
	m, ok := clock(hhmm)
	if !ok {
		return false
	}
	t := lastAt(now, m)
	return t.After(since)
}

// tick moves the scheduler on to now: releasing the queue at the start
// time, holding it again at the stop time. Called every few seconds (and
// directly by the tests, with a pretend clock).
func (m *Manager) tick(now time.Time) {
	m.mu.Lock()
	s := m.settings.Schedule
	since := m.lastTick
	if since.IsZero() || now.Before(since) {
		since = now // the first tick, or the clock was turned back
	}
	m.lastTick = now
	want := m.released
	switch {
	case !s.Enabled:
	case s.Stop != "":
		// With a stop time it's simply a window: inside it, run. (Also
		// right after the computer wakes up in the middle of one.)
		want = s.inWindow(now)
	case !m.released && crossed(since, now, s.Start):
		want = !m.queueDoneLocked() // nothing waiting: nothing to release
	}
	changed := want != m.released
	m.released = want
	if changed {
		m.applyHoldLocked()
	}
	m.mu.Unlock()
	if changed {
		m.notify()
	}
}

// heldLocked: queued downloads must wait for the schedule.
func (m *Manager) heldLocked() bool {
	return m.settings.Schedule.Enabled && !m.released
}

// applyHoldLocked starts or stops downloads after the schedule changed:
// released, the queue runs; held, downloads it started go back to waiting
// (keeping their progress). "Start now" downloads carry on either way.
func (m *Manager) applyHoldLocked() {
	if m.heldLocked() {
		for _, e := range m.entries {
			if e.running && !e.StartNow {
				e.stopAs = Queued
				e.cancel()
			}
		}
	}
	m.scheduleLocked()
	m.saveLocked()
}

// scheduleChangedLocked is called when the settings' schedule changes:
// inside the start-stop window it runs at once; otherwise it waits.
func (m *Manager) scheduleChangedLocked(old Schedule) {
	s := m.settings.Schedule
	if s == old {
		return
	}
	m.released = s.Enabled && s.Stop != "" && s.inWindow(m.now())
	m.lastTick = m.now()
	m.applyHoldLocked()
}

// QueueState reports whether the queue is waiting for the schedule.
func (m *Manager) QueueState() QueueState {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings.Schedule
	if !s.Enabled {
		return QueueState{}
	}
	if m.released {
		return QueueState{StopsAt: s.Stop}
	}
	return QueueState{Held: true, StartsAt: s.Start}
}

// queueDoneLocked: nothing is running and nothing is waiting - everything
// is finished, failed or paused.
func (m *Manager) queueDoneLocked() bool {
	for _, e := range m.entries {
		if e.running || e.Status == Queued {
			return false
		}
	}
	return true
}

// SetOnAllDone sets a function called when the last running download ends
// and nothing is waiting (for "shut down when done"). It's called without
// any lock held.
func (m *Manager) SetOnAllDone(fn func()) {
	m.mu.Lock()
	m.onAllDone = fn
	m.mu.Unlock()
}
