package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeApp struct {
	mu     sync.Mutex
	added  []Request
	videos []Request
	shown  int
	addErr error
}

func (a *fakeApp) OpenVideo(r Request) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.videos = append(a.videos, r)
	return nil
}

func (a *fakeApp) AddFromBrowser(r Request) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.addErr != nil {
		return "", a.addErr
	}
	a.added = append(a.added, r)
	return "file.zip", nil
}

func (a *fakeApp) ShowWindow() {
	a.mu.Lock()
	a.shown++
	a.mu.Unlock()
}

// start runs a bridge server in a fresh folder and waits until it's ready.
func start(t *testing.T, app App) string {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, app) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	for i := 0; ; i++ {
		if _, err := ReadInfo(dir); err == nil {
			return dir
		}
		if i == 200 {
			t.Fatal("bridge never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pingApp is a fakeApp that also counts pings (the optional Pinger).
type pingApp struct {
	fakeApp
	pings int
}

func (a *pingApp) Pinged() {
	a.mu.Lock()
	a.pings++
	a.mu.Unlock()
}

func TestPingIsReported(t *testing.T) {
	app := &pingApp{}
	dir := start(t, app)
	for i := 0; i < 2; i++ {
		if rep, err := Call(context.Background(), dir, "ping", nil); err != nil || !rep.OK {
			t.Fatalf("ping: %+v, %v", rep, err)
		}
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.pings != 2 {
		t.Errorf("pings = %d, want 2", app.pings)
	}
	// An App without Pinged works as before (TestAddReachesTheApp uses one).
}

func TestAddReachesTheApp(t *testing.T) {
	app := &fakeApp{}
	dir := start(t, app)

	req := &Request{URL: "https://example.com/file.zip", Cookies: "a=1", Referrer: "https://example.com/"}
	rep, err := Call(context.Background(), dir, "add", req)
	if err != nil || !rep.OK || rep.Filename != "file.zip" {
		t.Fatalf("got %+v, %v", rep, err)
	}
	if len(app.added) != 1 || app.added[0].URL != req.URL || app.added[0].Cookies != "a=1" {
		t.Errorf("app got %+v", app.added)
	}
	if rep, err := Call(context.Background(), dir, "open", nil); err != nil || !rep.OK || app.shown != 1 {
		t.Errorf("open: %+v %v (shown %d)", rep, err, app.shown)
	}
	page := &Request{URL: "https://www.youtube.com/watch?v=abc"}
	if rep, err := Call(context.Background(), dir, "video", page); err != nil || !rep.OK || len(app.videos) != 1 || app.videos[0].URL != page.URL {
		t.Errorf("video: %+v %v %+v", rep, err, app.videos)
	}
}

func TestAppErrorsAreReported(t *testing.T) {
	dir := start(t, &fakeApp{addErr: errors.New("already in the list")})
	rep, err := Call(context.Background(), dir, "add", &Request{URL: "https://example.com/x"})
	if err != nil || rep.OK || rep.Error != "already in the list" {
		t.Errorf("got %+v, %v", rep, err)
	}
}

// Requests without the token, or from a web page, are refused.
func TestStrangersAreRefused(t *testing.T) {
	app := &fakeApp{}
	dir := start(t, app)
	info, _ := ReadInfo(dir)
	url := fmt.Sprintf("http://127.0.0.1:%d/add", info.Port)
	body := `{"url":"https://evil.example/x.exe"}`

	for _, c := range []struct {
		name    string
		headers map[string]string
	}{
		{"no token", nil},
		{"wrong token", map[string]string{"Authorization": "Bearer " + strings.Repeat("0", 64)}},
		{"from a web page", map[string]string{"Authorization": "Bearer " + info.Token, "Origin": "https://evil.example"}},
	} {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: got %s", c.name, resp.Status)
		}
	}
	if len(app.added) != 0 {
		t.Errorf("a refused request still added %v", app.added)
	}
}

func TestNotRunning(t *testing.T) {
	dir := t.TempDir()
	if _, err := Call(context.Background(), dir, "ping", nil); !errors.Is(err, ErrNotRunning) {
		t.Errorf("no file: got %v", err)
	}
	// A file left behind by an app that crashed: nobody is listening.
	writeInfo(filepath.Join(dir, FileName), Info{Port: 1, Token: "x"})
	if _, err := Call(context.Background(), dir, "ping", nil); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stale file: got %v", err)
	}
}

func TestInfoFileRemovedOnStop(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, &fakeApp{}) }()
	for i := 0; i < 200; i++ {
		if _, err := ReadInfo(dir); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Errorf("bridge.json still there after stopping")
	}
}

func TestNativeMessages(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, Message{Type: "add", Request: Request{URL: "https://a/b"}}); err != nil {
		t.Fatal(err)
	}
	// 4-byte little-endian length first.
	if n := int(buf.Bytes()[0]) | int(buf.Bytes()[1])<<8; n != buf.Len()-4 {
		t.Errorf("length prefix %d, body %d", n, buf.Len()-4)
	}
	m, err := ReadMessage(&buf)
	if err != nil || m.Type != "add" || m.URL != "https://a/b" {
		t.Errorf("got %+v, %v", m, err)
	}
	// A huge length is refused, not allocated.
	if _, err := ReadMessage(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0x7f})); err == nil {
		t.Error("accepted a 2 GB message")
	}
}

func TestHeaders(t *testing.T) {
	h := Request{Cookies: "a=1", Referrer: "https://x/", UserAgent: "UA"}.Headers()
	if h["Cookie"] != "a=1" || h["Referer"] != "https://x/" || h["User-Agent"] != "UA" {
		t.Errorf("got %v", h)
	}
	if len(Request{}.Headers()) != 0 {
		t.Error("empty request should give no headers")
	}
}
