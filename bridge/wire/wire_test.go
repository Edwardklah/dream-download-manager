package wire

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hand-written response reader handles the three ways an HTTP body can
// be sent, and rejects what isn't HTTP.
func TestReadResponse(t *testing.T) {
	for name, c := range map[string]struct {
		raw    string
		body   string
		status int
		bad    bool
	}{
		"length":   {raw: "HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\n{\"ok\":true}", body: `{"ok":true}`, status: 200},
		"chunked":  {raw: "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\n{\"ok\"\r\n6\r\n:true}\r\n0\r\n\r\n", body: `{"ok":true}`, status: 200},
		"to end":   {raw: "HTTP/1.0 200 OK\r\n\r\n{}", body: "{}", status: 200},
		"refused":  {raw: "HTTP/1.1 403 Forbidden\r\nContent-Length: 10\r\n\r\nforbidden\n", body: "forbidden\n", status: 403},
		"not http": {raw: "SSH-2.0-OpenSSH\r\n", bad: true},
		"huge":     {raw: "HTTP/1.1 200 OK\r\nContent-Length: 999999999\r\n\r\n", bad: true},
	} {
		body, status, err := readResponse(bufio.NewReader(strings.NewReader(c.raw)))
		if c.bad {
			if err == nil {
				t.Errorf("%s: accepted", name)
			}
			continue
		}
		if err != nil || string(body) != c.body || status != c.status {
			t.Errorf("%s: %q %d %v", name, body, status, err)
		}
	}
}

func TestCallRefusesOddActions(t *testing.T) {
	dir := t.TempDir()
	writeTestInfo(t, dir)
	if _, err := Call(t.Context(), dir, "add\r\nX-Evil: 1", nil); err == nil || err == ErrNotRunning {
		t.Errorf("got %v", err)
	}
}

func writeTestInfo(t *testing.T, dir string) {
	t.Helper()
	if err := writeFile(dir, `{"port":1,"token":"abc","pid":1}`); err != nil {
		t.Fatal(err)
	}
}

func writeFile(dir, s string) error {
	return os.WriteFile(filepath.Join(dir, FileName), []byte(s), 0o600)
}
