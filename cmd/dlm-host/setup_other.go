//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Linux (and other Unix-like systems).

// startApp starts the app in the tray as its own process: in a new session
// (Setsid), so it keeps running after the browser closes this host.
func startApp() error {
	path, err := appPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(path, "--tray")
	cmd.Dir = filepath.Dir(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// allowForeground: Linux desktops don't have Windows' foreground lock.
func allowForeground() {}

// On Linux, browsers find native messaging hosts by their manifest file in
// these folders (per user; a system package puts them under /etc instead).
func manifestDirs() []string {
	dir, err := os.UserConfigDir() // ~/.config
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(dir, "google-chrome", "NativeMessagingHosts"),
		filepath.Join(dir, "chromium", "NativeMessagingHosts"),
		filepath.Join(dir, "microsoft-edge", "NativeMessagingHosts"),
		filepath.Join(dir, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts"),
	}
}

// Manifest is the native messaging host manifest for the program at path.
func Manifest(path string) []byte {
	data, _ := json.MarshalIndent(map[string]any{
		"name":            HostName,
		"description":     "Dream Download Manager - receives links from the browser extension",
		"path":            path,
		"type":            "stdio",
		"allowed_origins": []string{"chrome-extension://" + ExtensionID + "/"},
	}, "", "  ")
	return data
}

// doInstall writes the manifest into each browser's folder, for this user.
func doInstall() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	data := Manifest(self)
	for _, dir := range manifestDirs() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, HostName+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		os.Stdout.WriteString("wrote " + path + "\n")
	}
	return nil
}

func doUninstall() error {
	for _, dir := range manifestDirs() {
		path := filepath.Join(dir, HostName+".json")
		if os.Remove(path) == nil {
			os.Stdout.WriteString("removed " + path + "\n")
		}
	}
	return nil
}
