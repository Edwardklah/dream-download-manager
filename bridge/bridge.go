// Package bridge carries links from the browser to the running app.
//
// The path a link takes:
//
//	browser extension --(native messaging)--> dlm-host.exe --(HTTP on 127.0.0.1)--> the app
//
// The app runs a tiny web server on 127.0.0.1 (this computer only), on a
// port chosen by the system. It writes the port and a random secret token to
// bridge.json in its data folder. dlm-host reads that file and sends the
// token with every request.
//
// Why the token: any web page you visit can make your browser send requests
// to 127.0.0.1. Without a secret, a malicious page could add downloads. A
// page can't read files on your disk, so it can't know the token.
package bridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dlmanager/bridge/wire"
)

// The messages and the client live in package wire, which dlm-host uses
// without the server (see wire.go for why). These names keep them usable as
// bridge.Request etc.
type (
	Request = wire.Request
	Reply   = wire.Reply
	Info    = wire.Info
	Message = wire.Message
)

const FileName = wire.FileName

var (
	ErrNotRunning = wire.ErrNotRunning
	ReadInfo      = wire.ReadInfo
	Call          = wire.Call
	ReadMessage   = wire.ReadMessage
	WriteMessage  = wire.WriteMessage
)

// maxBody: a link with cookies is a few KB at most.
const maxBody = 256 << 10

// App is what the server needs from the app.
type App interface {
	AddFromBrowser(Request) (filename string, err error)
	// OpenVideo shows the quality picker for the video page in req.URL.
	OpenVideo(Request) error
	ShowWindow()
}

// Pinger is optional: an App that has it hears each ping, the extension
// checking that the app is there (the welcome screen uses it to show that the
// extension is connected).
type Pinger interface{ Pinged() }

// Serve runs the app's side until ctx is cancelled. It writes the info file
// in dir once it's listening, and removes it when it stops.
func Serve(ctx context.Context, dir string, app App) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // 0 = any free port
	if err != nil {
		return err
	}
	token, err := newToken()
	if err != nil {
		ln.Close()
		return err
	}
	info := Info{Port: ln.Addr().(*net.TCPAddr).Port, Token: token, PID: os.Getpid()}
	path := filepath.Join(dir, FileName)
	if err := writeInfo(path, info); err != nil {
		ln.Close()
		return err
	}
	defer removeIfOurs(path, token)

	srv := &http.Server{
		Handler:           handler(token, app),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func handler(token string, app App) http.Handler {
	mux := http.NewServeMux()
	// GO NOTE: since Go 1.22, patterns can include the method: "POST /add"
	// only matches POST requests.
	mux.HandleFunc("POST /ping", func(w http.ResponseWriter, r *http.Request) {
		if p, ok := app.(Pinger); ok {
			p.Pinged()
		}
		reply(w, Reply{OK: true})
	})
	mux.HandleFunc("POST /open", func(w http.ResponseWriter, r *http.Request) {
		app.ShowWindow()
		reply(w, Reply{OK: true})
	})
	mux.HandleFunc("POST /video", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
			reply(w, Reply{Error: "bad request"})
			return
		}
		if err := app.OpenVideo(req); err != nil {
			reply(w, Reply{Error: err.Error()})
			return
		}
		reply(w, Reply{OK: true})
	})
	mux.HandleFunc("POST /add", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
			reply(w, Reply{Error: "bad request"})
			return
		}
		name, err := app.AddFromBrowser(req)
		if err != nil {
			reply(w, Reply{Error: err.Error()})
			return
		}
		reply(w, Reply{OK: true, Filename: name})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every request must carry the token. subtle.ConstantTimeCompare takes
		// the same time however much of the token matches, so it can't be
		// guessed piece by piece by timing the answers.
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Browsers add an Origin header to requests made by web pages. dlm-host
		// never does, so any request with one comes from a page: refuse it.
		if r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, rep Reply) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rep)
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeInfo saves the file readable by this user only (0600), through a temp
// file and rename, so dlm-host never reads half of it.
func writeInfo(path string, info Info) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(info)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeIfOurs deletes the info file, unless another copy of the app has
// replaced it with its own since.
func removeIfOurs(path, token string) {
	if info, err := ReadInfo(filepath.Dir(path)); err == nil && info.Token == token {
		os.Remove(path)
	}
}
