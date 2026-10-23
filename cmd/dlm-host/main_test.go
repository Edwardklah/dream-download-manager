package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dlmanager/bridge"
)

type app struct{ got []bridge.Request }

func (a *app) AddFromBrowser(r bridge.Request) (string, error) {
	a.got = append(a.got, r)
	return "x.zip", nil
}
func (a *app) ShowWindow()                    {}
func (a *app) OpenVideo(bridge.Request) error { return nil }

// The whole path: a message from the browser reaches the app.
func TestMessageReachesApp(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config) // where os.UserConfigDir points on Linux
	t.Setenv("APPDATA", config)         // ...and on Windows
	dir := filepath.Join(config, "dlmanager")

	// Not running yet: ping says so (and doesn't try to start it).
	if rep := handle(bridge.Message{Type: "ping"}); rep.OK {
		t.Error("ping succeeded with no app running")
	}

	a := &app{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Serve(ctx, dir, a)
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(filepath.Join(dir, bridge.FileName)); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rep := handle(bridge.Message{Type: "ping"}); !rep.OK {
		t.Errorf("ping: %+v", rep)
	}
	rep := handle(bridge.Message{Type: "add", Request: bridge.Request{URL: "https://example.com/x.zip", Cookies: "c=1"}})
	if !rep.OK || rep.Filename != "x.zip" || len(a.got) != 1 || a.got[0].Cookies != "c=1" {
		t.Errorf("add: %+v, app got %+v", rep, a.got)
	}
	// Only web links are passed on.
	for _, bad := range []string{"file:///C:/Windows/notepad.exe", "javascript:alert(1)", ""} {
		if rep := handle(bridge.Message{Type: "add", Request: bridge.Request{URL: bad}}); rep.OK {
			t.Errorf("%q was accepted", bad)
		}
	}
	if rep := handle(bridge.Message{Type: "format-disk"}); rep.OK {
		t.Error("unknown message accepted")
	}
}
