package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// What the first-run welcome needs (Welcome in frontend/src/main.tsx): where
// the browser extension's files are, which browsers are installed, and
// knowing when the extension has reached the app. (It can't open the
// browsers' extensions pages for you: Chrome and Edge refuse chrome:// and
// edge:// addresses from other programs, so the welcome has you paste them.)

// ExtensionFolder is where the browser extension's files are: in the
// "extension" folder next to the program once installed, or the project's own
// extension folder when run from a build (gui\bin). "" if neither is there.
func (s *DownloadService) ExtensionFolder() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	for _, d := range []string{filepath.Join(dir, "extension"), filepath.Join(dir, "..", "..", "extension")} {
		if exists(filepath.Join(d, "manifest.json")) {
			if abs, err := filepath.Abs(d); err == nil {
				return abs
			}
			return d
		}
	}
	return ""
}

// OpenExtensionFolder opens the extension's folder in the file manager.
func (s *DownloadService) OpenExtensionFolder() error {
	dir := s.ExtensionFolder()
	if dir == "" {
		return errors.New("the extension's files weren't found next to the program")
	}
	if runtime.GOOS == "windows" {
		return exec.Command("explorer", dir).Start()
	}
	return s.app.Browser.OpenFile(dir)
}

// Browsers lists the browsers found on this computer that can take the
// extension ("chrome", "edge"), so the welcome only offers those.
func (s *DownloadService) Browsers() []string {
	var found []string
	for _, b := range []string{"chrome", "edge"} {
		if browserPath(b) != "" {
			found = append(found, b)
		}
	}
	return found
}

// browserSeen: the extension has reached the app since it started. The
// extension asks ("ping") each time its button is pressed.
var browserSeen atomic.Bool

// Pinged is called by the bridge for each ping from the extension.
func (s *DownloadService) Pinged() {
	if !browserSeen.Swap(true) && s.app != nil {
		s.app.Event.Emit(browserEvent, true)
	}
}

// BrowserConnected: has the extension reached the app since it started?
func (s *DownloadService) BrowserConnected() bool { return browserSeen.Load() }

const browserEvent = "browser-connected"

func init() { application.RegisterEvent[bool](browserEvent) }
