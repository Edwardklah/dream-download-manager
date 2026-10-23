//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// clipboardText reads the clipboard through Wails (on Linux it needs the
// desktop's main loop).
func clipboardText(app *application.App) (string, bool) { return app.Clipboard.Text() }

// setWindowIcon: on Linux the window uses the app's icon as it is.
func setWindowIcon(*application.WebviewWindow, []byte) {}

// Linux (and other systems): the same extras as system_windows.go.

func clipboardSequence() uint32 { return 0 } // unknown: the text is compared instead

func suspendComputer() error  { return exec.Command("systemctl", "suspend").Run() }
func shutDownComputer() error { return exec.Command("systemctl", "poweroff").Run() }

// The desktop starts the programs listed in ~/.config/autostart at sign-in.
func autostartFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", "dlmanager.desktop"), nil
}

func startsWithSystem() bool {
	path, err := autostartFile()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func setStartWithSystem(on bool) error {
	path, err := autostartFile()
	if err != nil {
		return err
	}
	if !on {
		os.Remove(path)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	entry := "[Desktop Entry]\nType=Application\nName=Dream Download Manager\nExec=\"" + exe + "\" --tray\nX-GNOME-Autostart-enabled=true\n"
	os.MkdirAll(filepath.Dir(path), 0o755)
	return os.WriteFile(path, []byte(entry), 0o644)
}

// browserPath finds a browser's program on the PATH.
func browserPath(browser string) string {
	names := map[string][]string{
		"chrome": {"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"},
		"edge":   {"microsoft-edge", "microsoft-edge-stable"},
	}[browser]
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}
