// Package wire is the browser bridge's shared part: the messages, the
// bridge.json file, and the client dlm-host uses to reach the app.
//
// It's separate from package bridge (the app's server) for one reason:
// size. The server uses Go's net/http, which brings a whole web stack with
// it - HTTPS, certificates, several kinds of encryption. dlm-host doesn't
// need any of that to send one small message to the app on the same
// computer, so it imports only this package, which speaks the few lines of
// HTTP it needs over a plain connection (package net). That keeps
// dlm-host.exe less than half the size.
package wire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Request is a link sent from the browser.
type Request struct {
	URL       string `json:"url"`
	Filename  string `json:"filename,omitempty"`  // the browser's suggested name, if any
	Referrer  string `json:"referrer,omitempty"`  // the page the link was on
	Cookies   string `json:"cookies,omitempty"`   // "a=1; b=2" - for links that need you logged in
	UserAgent string `json:"userAgent,omitempty"` // the browser's, so the server treats us the same
	// Source: for a video page, the address the page's video element plays,
	// if it's an ordinary file - a fallback when yt-dlp doesn't know the site.
	Source string `json:"source,omitempty"`
	// For a video from the button: the browser tab's title, whether the
	// player is embedded from another site, and whether it had loaded
	// anything yet (some only do once you press play).
	Title     string `json:"title,omitempty"`
	Embedded  bool   `json:"embedded,omitempty"`
	NotLoaded bool   `json:"notLoaded,omitempty"`
}

// Headers turns the request's browser details into HTTP headers for the
// download. Servers that check them (logged-in downloads, anti-hotlinking)
// then see the same request the browser would have made.
func (r Request) Headers() map[string]string {
	h := map[string]string{}
	if r.Cookies != "" {
		h["Cookie"] = r.Cookies
	}
	if r.Referrer != "" {
		h["Referer"] = r.Referrer // (sic: HTTP spells it with one r)
	}
	if r.UserAgent != "" {
		h["User-Agent"] = r.UserAgent
	}
	return h
}

// Reply is the answer to a request.
type Reply struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// Info is the content of bridge.json.
type Info struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// FileName is bridge.json, kept in the app's data folder.
const FileName = "bridge.json"

// maxBody: a link with cookies is a few KB at most.
const maxBody = 256 << 10

// ReadInfo reads bridge.json from dir.
func ReadInfo(dir string) (Info, error) {
	var info Info
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil || info.Port == 0 || info.Token == "" {
		return info, errors.New("bridge.json is damaged")
	}
	return info, nil
}

// ErrNotRunning means the app isn't running (or isn't answering).
var ErrNotRunning = errors.New("Dream Download Manager isn't running")

// Call sends one request to the app: action is "ping", "open", or "add" or
// "video" (with req). It's what dlm-host uses.
//
// It's an ordinary HTTP request, written by hand:
//
//	POST /add HTTP/1.1
//	Host: 127.0.0.1:52811
//	Authorization: Bearer <token from bridge.json>
//	Content-Type: application/json
//	Content-Length: 87
//	Connection: close
//
//	{"url":"https://..."}
//
// and the answer is read the same way: a status line, headers, a blank
// line, the body.
func Call(ctx context.Context, dir, action string, req *Request) (Reply, error) {
	info, err := ReadInfo(dir)
	if err != nil {
		return Reply{}, ErrNotRunning
	}
	for _, c := range action {
		if c < 'a' || c > 'z' {
			return Reply{}, fmt.Errorf("bad action %q", action)
		}
	}
	body := []byte("{}")
	if req != nil {
		body, _ = json.Marshal(req)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(info.Port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return Reply{}, ErrNotRunning // nothing listening: the app was closed
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	_, err = fmt.Fprintf(conn, "POST /%s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		action, addr, info.Token, len(body), body)
	if err != nil {
		return Reply{}, err
	}

	data, status, err := readResponse(bufio.NewReader(conn))
	if err != nil {
		return Reply{}, fmt.Errorf("unreadable answer from the app: %w", err)
	}
	if status != 200 {
		return Reply{}, fmt.Errorf("the app refused the request (%d)", status)
	}
	var rep Reply
	if err := json.Unmarshal(data, &rep); err != nil {
		return Reply{}, err
	}
	return rep, nil
}

// readResponse reads an HTTP/1.1 response: the status code, and the body -
// sized by Content-Length, sent in chunks, or running to the end of the
// connection.
func readResponse(r *bufio.Reader) ([]byte, int, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, 0, err
	}
	// "HTTP/1.1 200 OK"
	parts := strings.Fields(line)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/1.") {
		return nil, 0, errors.New("not HTTP")
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, 0, errors.New("bad status")
	}
	length, chunked := -1, false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, 0, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of the headers
		}
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch strings.ToLower(name) {
		case "content-length":
			if length, err = strconv.Atoi(value); err != nil || length < 0 || length > maxBody {
				return nil, 0, errors.New("bad length")
			}
		case "transfer-encoding":
			chunked = strings.EqualFold(value, "chunked")
		}
	}
	switch {
	case chunked:
		var body []byte
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return nil, 0, err
			}
			size, err := strconv.ParseInt(strings.TrimSpace(strings.Split(line, ";")[0]), 16, 32)
			if err != nil || size < 0 || len(body)+int(size) > maxBody {
				return nil, 0, errors.New("bad chunk")
			}
			if size == 0 {
				return body, status, nil
			}
			chunk := make([]byte, size+2) // the chunk, then CRLF
			if _, err := io.ReadFull(r, chunk); err != nil {
				return nil, 0, err
			}
			body = append(body, chunk[:size]...)
		}
	case length >= 0:
		body := make([]byte, length)
		_, err := io.ReadFull(r, body)
		return body, status, err
	default:
		body, err := io.ReadAll(io.LimitReader(r, maxBody))
		return body, status, err
	}
}
