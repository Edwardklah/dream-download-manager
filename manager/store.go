package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// The download list and settings are saved as two small JSON files in the
// manager's folder (%AppData%\dlmanager on Windows):
//
//	downloads.json - every download and its state
//	settings.json  - the user's settings
//
// They're written with "temp file + rename", like the engine's .meta files,
// so a crash mid-save can never leave a half-written file.

// stored is one download as saved: the item plus its private request
// headers, which the interface never sees.
type stored struct {
	Item
	Headers map[string]string `json:"headers,omitempty"`
}

const (
	downloadsFile = "downloads.json"
	settingsFile  = "settings.json"
)

// load reads both files. Missing or damaged files just mean starting fresh.
func (m *Manager) load() {
	// Start from the defaults, so settings added in a newer version (not in
	// the file yet) get their default rather than "off".
	s := DefaultSettings()
	if readJSON(filepath.Join(m.dir, settingsFile), &s) == nil {
		m.settings = s.clean()
	}

	var list []stored
	if readJSON(filepath.Join(m.dir, downloadsFile), &list) != nil {
		return
	}
	for _, st := range list {
		it := st.Item
		switch it.Status {
		case Starting, Downloading, Retrying:
			// It was running when the app closed (or crashed): queue it again,
			// and the engine will resume it from its .part file.
			it.Status = Queued
		case Queued, Paused, Completed, Failed:
		default:
			continue // unknown status from a damaged file: skip it
		}
		it.Speed, it.ETASeconds, it.Active, it.Connections, it.Retrying = 0, -1, 0, 0, 0
		it.Segments = nil
		m.entries = append(m.entries, &entry{Item: it, headers: st.Headers})
		m.nextID = max(m.nextID, it.ID)
	}
}

// saveLocked writes both files. The caller holds m.mu.
func (m *Manager) saveLocked() {
	list := make([]stored, len(m.entries))
	for i, e := range m.entries {
		it := e.Item
		it.Segments = nil // live detail; the engine's .meta file has the real pieces
		list[i] = stored{Item: it, Headers: e.headers}
	}
	writeJSON(filepath.Join(m.dir, downloadsFile), list)
	writeJSON(filepath.Join(m.dir, settingsFile), m.settings)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600: only this user can read it (it can hold browser cookies).
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
