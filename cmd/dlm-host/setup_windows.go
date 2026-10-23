package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Windows process creation flags (from the Windows API).
const (
	createBreakawayFromJob = 0x01000000 // don't die with the browser's job
	createNewProcessGroup  = 0x00000200 // nor with its Ctrl+C
)

// allowForeground passes the browser's right to bring a window to the front
// on to any process - here, the app. Windows only lets the program you're
// using (or one it started, like this host) do that; without it, the app's
// window would open behind the browser.
func allowForeground() {
	const asfwAny = ^uintptr(0) // ASFW_ANY (-1): any process may
	syscall.NewLazyDLL("user32.dll").NewProc("AllowSetForegroundWindow").Call(asfwAny)
}

// startApp starts the app in the tray, as its own process. The browser runs
// this host inside a "job" (a group of processes it can end together); the
// app asks to leave that job, so it keeps running after the host exits.
func startApp() error {
	path, err := appPath()
	if err != nil {
		return err
	}
	start := func(flags uint32) error {
		cmd := exec.Command(path, "--tray")
		cmd.Dir = filepath.Dir(path)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release() // we don't wait for it
	}
	if err := start(createBreakawayFromJob | createNewProcessGroup); err != nil {
		// Some jobs don't allow leaving; start it normally then.
		return start(createNewProcessGroup)
	}
	return nil
}

// The registry keys where Chrome and Edge look for native messaging hosts.
var registryKeys = []string{
	`HKCU\Software\Google\Chrome\NativeMessagingHosts\` + HostName,
	`HKCU\Software\Microsoft\Edge\NativeMessagingHosts\` + HostName,
}

// doInstall writes the host manifest (which program to start, and which
// extension may start it) next to this program, and points the browsers at it.
func doInstall() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	manifest := map[string]any{
		"name":            HostName,
		"description":     "Dream Download Manager - receives links from the browser extension",
		"path":            self,
		"type":            "stdio",
		"allowed_origins": []string{"chrome-extension://" + ExtensionID + "/"},
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	manifestPath := filepath.Join(filepath.Dir(self), HostName+".json")
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		return err
	}
	for _, key := range registryKeys {
		// reg.exe is Windows' own registry tool. /ve = the key's default
		// value, /f = overwrite without asking.
		out, err := exec.Command("reg", "add", key, "/ve", "/t", "REG_SZ", "/d", manifestPath, "/f").CombinedOutput()
		if err != nil {
			return &regError{key, string(out), err}
		}
		os.Stdout.WriteString("registered " + key + "\n")
	}
	os.Stdout.WriteString("manifest: " + manifestPath + "\n")
	return nil
}

func doUninstall() error {
	for _, key := range registryKeys {
		exec.Command("reg", "delete", key, "/f").Run() // fine if it wasn't there
		os.Stdout.WriteString("removed " + key + "\n")
	}
	if self, err := os.Executable(); err == nil {
		os.Remove(filepath.Join(filepath.Dir(self), HostName+".json"))
	}
	return nil
}

type regError struct {
	key, out string
	err      error
}

func (e *regError) Error() string { return e.key + ": " + e.err.Error() + " " + e.out }
