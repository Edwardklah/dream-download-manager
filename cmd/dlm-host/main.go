// dlm-host connects the browser extension to the app.
//
// The browser starts it for every message from the extension (that's how
// "native messaging" works), passing the message on stdin. It forwards the
// message to the running app over the bridge (see package bridge), starting
// the app first if it isn't running, writes the answer to stdout, and exits.
//
// Setup, once:
//
//	dlm-host -install     tell Chrome and Edge where to find this program
//	dlm-host -uninstall   undo that
//
// Both only touch this user's settings (HKEY_CURRENT_USER), so no
// administrator rights are needed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"dlmanager/bridge/wire"
)

// HostName is how the extension asks for us: chrome.runtime.sendNativeMessage(HostName, ...)
const HostName = "com.dlmanager.host"

// ExtensionID is our extension's fixed ID (from the "key" in its manifest).
// Only this extension may start the host.
const ExtensionID = "hjchnlipjbjlmobfaekngdjdjldjhpdk"

func main() {
	install := flag.Bool("install", false, "register with Chrome and Edge")
	uninstall := flag.Bool("uninstall", false, "unregister")
	flag.Parse()

	switch {
	case *install:
		if err := doInstall(); err != nil {
			fmt.Fprintln(os.Stderr, "install failed:", err)
			os.Exit(1)
		}
		return
	case *uninstall:
		if err := doUninstall(); err != nil {
			fmt.Fprintln(os.Stderr, "uninstall failed:", err)
			os.Exit(1)
		}
		return
	}

	// Started by the browser. Anything on stdout must be a message, so
	// problems are reported as {"ok":false,"error":...}.
	msg, err := wire.ReadMessage(os.Stdin)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			wire.WriteMessage(os.Stdout, wire.Reply{Error: err.Error()})
		}
		return
	}
	wire.WriteMessage(os.Stdout, handle(msg))
}

func handle(msg wire.Message) wire.Reply {
	dir, err := dataDir()
	if err != nil {
		return wire.Reply{Error: err.Error()}
	}
	ctx := context.Background()

	switch msg.Type {
	case "ping":
		// Only asks: never starts the app (the popup asks every time it opens).
		if _, err := wire.Call(ctx, dir, "ping", nil); err != nil {
			return wire.Reply{Error: err.Error()}
		}
		return wire.Reply{OK: true}
	case "open":
		return call(ctx, dir, "open", nil)
	case "add", "video":
		u, err := url.Parse(msg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return wire.Reply{Error: "not a web link"}
		}
		return call(ctx, dir, msg.Type, &msg.Request)
	default:
		return wire.Reply{Error: "unknown message type " + msg.Type}
	}
}

// call sends a request to the app, starting it first if it isn't running.
func call(ctx context.Context, dir, action string, req *wire.Request) wire.Reply {
	allowForeground() // so the app's window can come in front of the browser
	rep, err := wire.Call(ctx, dir, action, req)
	if errors.Is(err, wire.ErrNotRunning) {
		if err := startApp(); err != nil {
			return wire.Reply{Error: "couldn't start Dream Download Manager: " + err.Error()}
		}
		// Wait for it to be ready (it writes bridge.json once it's listening).
		deadline := time.Now().Add(15 * time.Second)
		for {
			time.Sleep(200 * time.Millisecond)
			rep, err = wire.Call(ctx, dir, action, req)
			if !errors.Is(err, wire.ErrNotRunning) || time.Now().After(deadline) {
				break
			}
		}
	}
	if err != nil {
		return wire.Reply{Error: err.Error()}
	}
	return rep
}

// appPath is gui.exe, which sits next to this program.
func appPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	// DreamDM.exe (Linux: dreamdm); gui.exe is what it was called before it
	// had a name.
	for _, name := range []string{"DreamDM.exe", "dreamdm", "DreamDM", "gui.exe"} {
		path := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("Dream DM's program not found next to %s", self)
}

// dataDir must match the app's: %AppData%\dlmanager.
func dataDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dlmanager"), nil
}
