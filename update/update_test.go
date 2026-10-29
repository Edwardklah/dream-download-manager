package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A fake GitHub with release v0.9.2.
func fakeGitHub(t *testing.T, setup []byte, sum string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/r/releases/latest":
			w.Header().Set("Location", "/o/r/releases/tag/v0.9.2")
			w.WriteHeader(http.StatusFound)
		case "/o/r/releases/download/v0.9.2/DreamDM-Setup-0.9.2.exe":
			w.Write(setup)
		case "/o/r/releases/download/v0.9.2/DreamDM-Setup-0.9.2.exe.sha256":
			w.Write([]byte(sum + "  DreamDM-Setup-0.9.2.exe\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	old := Repo
	Repo = ts.URL + "/o/r"
	t.Cleanup(func() { Repo = old })
}

func TestLatestAndFetch(t *testing.T) {
	setup := []byte("pretend this is a setup program")
	h := sha256.Sum256(setup)
	fakeGitHub(t, setup, hex.EncodeToString(h[:]))

	v, err := Latest(context.Background())
	if err != nil || v != "0.9.2" {
		t.Fatalf("Latest: %q, %v", v, err)
	}
	dir := t.TempDir()
	var last int64
	p, err := Fetch(context.Background(), v, dir, func(done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(setup) || filepath.Base(p) != "DreamDM-Setup-0.9.2.exe" || last != int64(len(setup)) {
		t.Errorf("fetched %q to %s (progress %d)", got, p, last)
	}
}

func TestFetchRefusesWrongChecksum(t *testing.T) {
	fakeGitHub(t, []byte("a tampered setup"), "0000000000000000000000000000000000000000000000000000000000000000")
	dir := t.TempDir()
	if _, err := Fetch(context.Background(), "0.9.2", dir, nil); !errors.Is(err, ErrChecksum) {
		t.Fatalf("got %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.9.2", "0.9.1", true}, {"0.10.0", "0.9.9", true}, {"1.0.0", "0.99.99", true},
		{"0.9.1", "0.9.1", false}, {"0.9.0", "0.9.1", false}, {"junk", "0.9.1", false}, {"0.9.2", "", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
