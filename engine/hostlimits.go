package engine

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// HostLimits remembers how many connections each server allows.
//
// Some servers only let one user open a few connections at once and refuse
// the rest (429 Too Many Requests / 503). The engine copes with that during a
// download, but it costs a few refused requests every time - and some servers
// block you for a while if you're refused too often. So once a server's limit
// is discovered, it's saved here, and the next download from that server
// starts with that many connections. No refusals at all.
//
// Two kinds of entry:
//   - LEARNED: recorded automatically when a server refuses connections.
//     These EXPIRE after a day (learnedLimitTTL): many servers only limit you
//     while you're busy - proof.ovh.net refused connections after a morning of
//     heavy testing, then accepted 8 again an hour later. A permanent learned
//     limit would hold every future download back for no reason.
//   - MANUAL: set by the user. Never expires, never overwritten by learning.
//     (Like IDM's "Exceptions" table, but you rarely need to touch it.)
//
// The table is saved as JSON, by default in your user config folder:
// %AppData%\dlmanager\hosts.json on Windows.
type HostLimits struct {
	mu    sync.Mutex
	path  string
	hosts map[string]HostLimit
}

// learnedLimitTTL is how long a learned limit is trusted.
var learnedLimitTTL = 24 * time.Hour

// HostLimit is the remembered limit for one server.
type HostLimit struct {
	MaxConnections int       `json:"max_connections"`
	Manual         bool      `json:"manual,omitempty"` // set by the user; learning won't change it
	Updated        time.Time `json:"updated"`
}

// DefaultHostLimitsPath is where the table is kept unless told otherwise.
func DefaultHostLimitsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dlmanager", "hosts.json"), nil
}

// LoadHostLimits reads the table from path. A missing file is fine: you get
// an empty table, and the file is created the first time something is saved.
func LoadHostLimits(path string) (*HostLimits, error) {
	h := &HostLimits{path: path, hosts: map[string]HostLimit{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &h.hosts); err != nil {
		// A damaged file shouldn't stop downloads: start a fresh table.
		h.hosts = map[string]HostLimit{}
	}
	return h, nil
}

// normalizeHost makes "PROOF.ovh.net:443" and "proof.ovh.net" the same key.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i] // drop the port (but not part of an IPv6 address)
	}
	return host
}

// Get returns the remembered limit for a server, if there is one.
func (h *HostLimits) Get(host string) (HostLimit, bool) {
	if h == nil {
		return HostLimit{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.hosts[normalizeHost(host)]
	if ok && l.expired() {
		return HostLimit{}, false
	}
	return l, ok
}

// expired reports whether a learned limit is too old to trust.
func (l HostLimit) expired() bool {
	return !l.Manual && time.Since(l.Updated) > learnedLimitTTL
}

// Learn records a limit discovered during a download. It only ever LOWERS a
// learned limit (a server that refused 4 connections today shouldn't be
// raised to 6 because one download happened to manage 6 briefly), and it
// never touches a manual entry.
func (h *HostLimits) Learn(host string, maxConnections int) error {
	if h == nil || maxConnections < 1 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := normalizeHost(host)
	if cur, ok := h.hosts[key]; ok && !cur.expired() && (cur.Manual || cur.MaxConnections <= maxConnections) {
		return nil
	}
	h.hosts[key] = HostLimit{MaxConnections: maxConnections, Updated: time.Now()}
	return h.saveLocked()
}

// Set records a manual limit for a server (1-32).
func (h *HostLimits) Set(host string, maxConnections int) error {
	maxConnections = min(max(maxConnections, 1), maxConnectionsLimit)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hosts[normalizeHost(host)] = HostLimit{MaxConnections: maxConnections, Manual: true, Updated: time.Now()}
	return h.saveLocked()
}

// Forget removes a server from the table, learned or manual.
func (h *HostLimits) Forget(host string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.hosts, normalizeHost(host))
	return h.saveLocked()
}

// All returns a copy of the table (without expired entries), names sorted.
func (h *HostLimits) All() (names []string, limits map[string]HostLimit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	limits = maps.Clone(h.hosts)
	maps.DeleteFunc(limits, func(_ string, l HostLimit) bool { return l.expired() })
	names = slices.Sorted(maps.Keys(limits))
	return names, limits
}

// saveLocked writes the table atomically (temp file + rename, like the meta
// file). The caller must hold h.mu.
func (h *HostLimits) saveLocked() error {
	if h.path == "" {
		return nil // an in-memory table (used by tests)
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h.hosts, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}
