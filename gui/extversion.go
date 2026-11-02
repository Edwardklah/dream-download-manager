package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"dlmanager/update"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Is the browser extension up to date?
//
// The extension is loaded "unpacked" from the folder the setup installs, so
// when Dream DM updates, the new extension files are there but Chrome and
// Edge keep running the old ones until you press Reload on its card in
// chrome://extensions (or restart the browser). Every message from the
// extension now says its version (bridge: ExtensionSeen); when it's older
// than the one installed, the window says so, with how to reload it. The
// notice goes away by itself once the reloaded extension says hello.

// ExtensionStatus is what the window shows.
type ExtensionStatus struct {
	Outdated  bool   `json:"outdated"`
	Running   string `json:"running"`   // the version the browser runs ("" = before 0.4.1)
	Installed string `json:"installed"` // the version in the extension folder
}

const extensionEvent = "extension-status"

func init() { application.RegisterEvent[ExtensionStatus](extensionEvent) }

type extState struct {
	extMu        sync.Mutex
	ext          ExtensionStatus
	extDismissed bool // "Got it" for this session
}

// installedExtVersion reads the version from the extension folder's
// manifest.json ("" if it can't).
func (s *DownloadService) installedExtVersion() string {
	dir := s.ExtensionFolder()
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	json.Unmarshal(data, &m)
	return m.Version
}

// ExtensionSeen is called by the bridge with each message's extension
// version.
func (s *DownloadService) ExtensionSeen(version string) {
	inst := s.installedExtVersion()
	st := ExtensionStatus{
		Outdated:  inst != "" && (version == "" || update.Newer(inst, version)),
		Running:   version,
		Installed: inst,
	}
	s.extMu.Lock()
	changed := st != s.ext
	s.ext = st
	s.extMu.Unlock()
	if changed && s.app != nil {
		s.app.Event.Emit(extensionEvent, s.ExtensionStatus())
	}
}

// ExtensionStatus: the extension's state as last seen (Outdated false once
// you've said "Got it", until next time the app starts).
func (s *DownloadService) ExtensionStatus() ExtensionStatus {
	s.extMu.Lock()
	defer s.extMu.Unlock()
	st := s.ext
	if s.extDismissed {
		st.Outdated = false
	}
	return st
}

// DismissExtensionNotice: "Got it".
func (s *DownloadService) DismissExtensionNotice() {
	s.extMu.Lock()
	s.extDismissed = true
	s.extMu.Unlock()
	if s.app != nil {
		s.app.Event.Emit(extensionEvent, s.ExtensionStatus())
	}
}
