package engine

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostLimitsRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.json")
	h, err := LoadHostLimits(path) // missing file: empty table
	if err != nil {
		t.Fatal(err)
	}

	h.Learn("Proof.OVH.net:443", 3)
	if l, ok := h.Get("proof.ovh.net"); !ok || l.MaxConnections != 3 || l.Manual {
		t.Fatalf("learned limit not stored under the plain host name: %+v %v", l, ok)
	}
	h.Learn("proof.ovh.net", 5) // learning never RAISES a limit
	if l, _ := h.Get("proof.ovh.net"); l.MaxConnections != 3 {
		t.Errorf("learning raised the limit to %d", l.MaxConnections)
	}
	h.Learn("proof.ovh.net", 2) // ...but can lower it
	if l, _ := h.Get("proof.ovh.net"); l.MaxConnections != 2 {
		t.Errorf("learning didn't lower the limit: %d", l.MaxConnections)
	}

	h.Set("proof.ovh.net", 4) // manual entries win...
	h.Learn("proof.ovh.net", 1)
	if l, _ := h.Get("proof.ovh.net"); l.MaxConnections != 4 || !l.Manual {
		t.Errorf("learning overwrote a manual limit: %+v", l)
	}
	h.Set("example.com", 99) // clamped to 1-32
	if l, _ := h.Get("example.com"); l.MaxConnections != 32 {
		t.Errorf("limit not clamped: %d", l.MaxConnections)
	}

	// Saved to disk and loaded back.
	h2, err := LoadHostLimits(path)
	if err != nil {
		t.Fatal(err)
	}
	names, all := h2.All()
	if len(names) != 2 || all["proof.ovh.net"].MaxConnections != 4 {
		t.Errorf("table didn't survive a reload: %v %v", names, all)
	}

	h2.Forget("example.com")
	if _, ok := h2.Get("example.com"); ok {
		t.Error("Forget didn't remove the entry")
	}

	// A damaged file doesn't stop anything: you get an empty table.
	os.WriteFile(path, []byte("{not json"), 0o644)
	h3, err := LoadHostLimits(path)
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := h3.All(); len(names) != 0 {
		t.Errorf("expected an empty table from a damaged file, got %v", names)
	}
}

func TestLearnedLimitsExpire(t *testing.T) {
	h, _ := LoadHostLimits(filepath.Join(t.TempDir(), "hosts.json"))
	h.Learn("busy.example", 3)
	h.Set("strict.example", 2)

	old := learnedLimitTTL
	learnedLimitTTL = 0 // pretend a day has passed
	t.Cleanup(func() { learnedLimitTTL = old })
	time.Sleep(time.Millisecond)

	if _, ok := h.Get("busy.example"); ok {
		t.Error("a learned limit should expire")
	}
	if l, ok := h.Get("strict.example"); !ok || l.MaxConnections != 2 {
		t.Error("a manual limit must never expire")
	}
	if names, _ := h.All(); len(names) != 1 {
		t.Errorf("expired entries shouldn't be listed: %v", names)
	}
	// After expiry, a higher limit can be learned again.
	h.Learn("busy.example", 6)
	learnedLimitTTL = old
	if l, _ := h.Get("busy.example"); l.MaxConnections != 6 {
		t.Errorf("expected a fresh limit of 6, got %d", l.MaxConnections)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Proof.OVH.net":     "proof.ovh.net",
		"proof.ovh.net:443": "proof.ovh.net",
		"127.0.0.1:8080":    "127.0.0.1",
		"[::1]:80":          "[::1]",
		"[::1]":             "[::1]",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// The whole point: the first download from a strict server discovers its
// limit through refusals; the second starts within it and is never refused.
func TestServerLimitIsRememberedForNextDownload(t *testing.T) {
	s := newFakeServer(8 << 20)
	s.maxConns = 3
	s.limitStatus = http.StatusTooManyRequests
	s.chunkDelay = 3 * time.Millisecond
	d, ts := setup(t, s)
	d.Connections = 8
	limits, _ := LoadHostLimits(filepath.Join(t.TempDir(), "hosts.json"))
	d.Limits = limits

	path, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if s.refused.Load() == 0 {
		t.Fatal("test setup: the first download should have been refused")
	}
	learned, ok := limits.Get("127.0.0.1")
	if !ok || learned.MaxConnections < 1 || learned.MaxConnections > 3 {
		t.Fatalf("expected a learned limit of 1-3, got %+v (found: %v)", learned, ok)
	}

	// Second download from the same server.
	s.refused.Store(0)
	d2 := &Download{URL: ts.URL + "/file.bin", Folder: t.TempDir(), Connections: 8, Limits: limits}
	var applied int
	d2.OnProgress = func(p Progress) { applied = max(applied, p.HostLimit) }
	path, err = d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, path, s.data)
	if n := s.refused.Load(); n != 0 {
		t.Errorf("second download was refused %d times despite the remembered limit", n)
	}
	if applied != learned.MaxConnections {
		t.Errorf("progress should report the applied limit %d, got %d", learned.MaxConnections, applied)
	}
}
