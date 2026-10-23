package hls

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.com/v/720/index.m3u8?token=abc")
	p, _, err := Parse(base, `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:7
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example.com/k?id=1",IV=0x000102030405060708090a0b0c0d0e0f
#EXTINF:6.006,
seg0.ts?token=abc
#EXT-X-KEY:METHOD=AES-128,URI="key2.bin"
#EXTINF:5.5,title with, a comma
/abs/seg1.ts
#EXT-X-KEY:METHOD=NONE
#EXTINF:6,
https://other.example.com/seg2.ts
#EXT-X-ENDLIST
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Segments) != 3 || !p.Ended {
		t.Fatalf("got %+v", p)
	}
	s0, s1, s2 := p.Segments[0], p.Segments[1], p.Segments[2]
	if s0.URL != "https://cdn.example.com/v/720/seg0.ts?token=abc" || s0.Seq != 7 || s0.Duration != 6.006 ||
		s0.Key == nil || s0.Key.URL != "https://keys.example.com/k?id=1" || len(s0.Key.IV) != 16 || s0.Key.IV[15] != 0x0f {
		t.Errorf("segment 0: %+v key %+v", s0, s0.Key)
	}
	if s1.URL != "https://cdn.example.com/abs/seg1.ts" || s1.Seq != 8 || s1.Key.URL != "https://cdn.example.com/v/720/key2.bin" || s1.Key.IV != nil {
		t.Errorf("segment 1: %+v key %+v", s1, s1.Key)
	}
	if s2.Key != nil || s2.URL != "https://other.example.com/seg2.ts" {
		t.Errorf("segment 2: %+v", s2)
	}
}

func TestParseMasterAndRanges(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.com/master.m3u8")
	_, vs, err := Parse(base, `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2"
360/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2800000,RESOLUTION=1280x720
720/index.m3u8
`)
	if err != nil || len(vs) != 2 || vs[1].Bandwidth != 2800000 || vs[1].URL != "https://cdn.example.com/720/index.m3u8" {
		t.Errorf("variants %+v %v", vs, err)
	}

	p, _, err := Parse(base, `#EXTM3U
#EXT-X-MAP:URI="video.mp4",BYTERANGE="800@0"
#EXT-X-BYTERANGE:1000@800
#EXTINF:4,
video.mp4
#EXT-X-BYTERANGE:1200
#EXTINF:4,
video.mp4
#EXT-X-ENDLIST
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Init == nil || p.Init.Length != 800 || p.Init.Offset != 0 {
		t.Errorf("init %+v", p.Init)
	}
	// The second range has no offset: it continues where the first ended.
	if a, b := p.Segments[0], p.Segments[1]; a.Offset != 800 || a.Length != 1000 || b.Offset != 1800 || b.Length != 1200 {
		t.Errorf("ranges %+v %+v", a, b)
	}
}

func TestParseRefusals(t *testing.T) {
	base, _ := url.Parse("https://x/y.m3u8")
	if _, _, err := Parse(base, "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://k\"\n#EXTINF:6,\na.ts\n"); !errors.Is(err, ErrDRM) {
		t.Errorf("DRM: %v", err)
	}
	if _, _, err := Parse(base, "<html>not a playlist</html>"); !errors.Is(err, ErrNotPlaylist) {
		t.Errorf("not a playlist: %v", err)
	}
}

// fakeHLS serves a playlist of n pieces of random data, locked with AES-128
// if lock is set, plus the key. Some pieces fail on their first request.
type fakeHLS struct {
	pieces   [][]byte // the clear content
	key      []byte
	lock     bool
	fmp4     bool
	live     bool
	failOnce map[int]bool
	mu       sync.Mutex
	failed   map[int]bool
	requests atomic.Int32
	slow     atomic.Int64 // nanoseconds per piece; atomic: requests may still run as a test changes it
}

func newFakeHLS(n int, lock bool) *fakeHLS {
	f := &fakeHLS{key: make([]byte, 16), lock: lock, failOnce: map[int]bool{3: true, 11: true}, failed: map[int]bool{}}
	rand.Read(f.key)
	for range n {
		p := make([]byte, 20_000+len(f.pieces)*37)
		rand.Read(p)
		f.pieces = append(f.pieces, p)
	}
	return f
}

func (f *fakeHLS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	switch {
	case strings.HasSuffix(r.URL.Path, ".m3u8"):
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:100\n")
		if f.fmp4 {
			b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
		}
		if f.lock {
			b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"/key?t=" + r.URL.Query().Get("t") + "\"\n")
		}
		for i := range f.pieces {
			fmt.Fprintf(&b, "#EXTINF:4.0,\n/seg/%d.ts?t=%s\n", i, r.URL.Query().Get("t"))
		}
		if !f.live {
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		w.Write([]byte(b.String()))
	case r.URL.Path == "/key":
		w.Write(f.key)
	case r.URL.Path == "/init.mp4":
		w.Write([]byte("INIT-HEADER"))
	case strings.HasPrefix(r.URL.Path, "/seg/"):
		var i int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/seg/"), "%d.ts", &i)
		f.mu.Lock()
		fail := f.failOnce[i] && !f.failed[i]
		f.failed[i] = true
		f.mu.Unlock()
		if fail {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		if d := time.Duration(f.slow.Load()); d > 0 {
			time.Sleep(d)
		}
		data := f.pieces[i]
		if f.lock {
			data = encrypt(f.key, uint64(100+i), data)
		}
		w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

// encrypt locks a piece the way HLS servers do: AES-128-CBC, PKCS#7
// padding, IV = the sequence number.
func encrypt(key []byte, seq uint64, data []byte) []byte {
	pad := aes.BlockSize - len(data)%aes.BlockSize
	in := append(append([]byte{}, data...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], seq)
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(in))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, in)
	return out
}

func fast(t *testing.T) {
	old := retryBase
	retryBase = 5 * time.Millisecond
	t.Cleanup(func() { retryBase = old })
}

func (f *fakeHLS) whole(withInit bool) []byte {
	var all []byte
	if withInit {
		all = append(all, "INIT-HEADER"...)
	}
	for _, p := range f.pieces {
		all = append(all, p...)
	}
	return all
}

func TestDownloadLockedPieces(t *testing.T) {
	fast(t)
	f := newFakeHLS(24, true)
	ts := httptest.NewServer(f)
	defer ts.Close()
	out := filepath.Join(t.TempDir(), "video.stream")

	var last Progress
	d := &Download{URL: ts.URL + "/v/index.m3u8?t=1", Out: out, OnProgress: func(p Progress) { last = p }}
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, f.whole(false)) || res.FragmentedMP4 {
		t.Fatalf("content differs (%d vs %d bytes), fmp4=%v", len(got), len(f.whole(false)), res.FragmentedMP4)
	}
	if last.PiecesDone != 24 || last.Done != int64(len(got)) || last.Total != last.Done {
		t.Errorf("last progress %+v", last)
	}
	if _, err := os.Stat(out + ".pieces"); !os.IsNotExist(err) {
		t.Error("the pieces folder was left behind")
	}
}

// Pause halfway (cancel), then continue with a NEW playlist address (the
// token changed): only the missing pieces are fetched.
func TestResumeWithNewAddress(t *testing.T) {
	fast(t)
	f := newFakeHLS(30, true)
	f.slow.Store(int64(20 * time.Millisecond))
	ts := httptest.NewServer(f)
	defer ts.Close()
	out := filepath.Join(t.TempDir(), "video.stream")

	ctx, cancel := context.WithCancel(context.Background())
	d := &Download{URL: ts.URL + "/v/index.m3u8?t=old", Out: out, Connections: 2, OnProgress: func(p Progress) {
		if p.PiecesDone >= 10 {
			cancel()
		}
	}}
	if _, err := d.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}
	entries, _ := os.ReadDir(out + ".pieces")
	kept := len(entries) - 1 // minus the "list" file
	if kept < 10 {
		t.Fatalf("only %d pieces kept", kept)
	}

	f.slow.Store(0)
	before := f.requests.Load()
	d2 := &Download{URL: ts.URL + "/v/index.m3u8?t=new", Out: out}
	if _, err := d2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, f.whole(false)) {
		t.Fatal("content differs after resuming")
	}
	// playlist + key + the missing pieces (no more)
	if n := int(f.requests.Load() - before); n > 2+30-kept {
		t.Errorf("%d requests to finish; %d pieces were already there", n, kept)
	}
}

func TestFragmentedMP4Pieces(t *testing.T) {
	fast(t)
	f := newFakeHLS(5, false)
	f.fmp4 = true
	ts := httptest.NewServer(f)
	defer ts.Close()
	out := filepath.Join(t.TempDir(), "v.stream")
	res, err := (&Download{URL: ts.URL + "/index.m3u8", Out: out}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !res.FragmentedMP4 || !bytes.Equal(got, f.whole(true)) {
		t.Errorf("fmp4=%v, content ok=%v", res.FragmentedMP4, bytes.Equal(got, f.whole(true)))
	}
}

func TestLiveIsRefused(t *testing.T) {
	f := newFakeHLS(3, false)
	f.live = true
	ts := httptest.NewServer(f)
	defer ts.Close()
	_, err := (&Download{URL: ts.URL + "/index.m3u8", Out: filepath.Join(t.TempDir(), "x")}).Run(context.Background())
	if !errors.Is(err, ErrLive) {
		t.Errorf("got %v", err)
	}
}

func TestMasterPicksBestQuality(t *testing.T) {
	fast(t)
	f := newFakeHLS(4, false)
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100\nlow/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=900\nhigh/index.m3u8\n"))
	})
	mux.HandleFunc("/low/index.m3u8", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "wrong one", 404) })
	mux.Handle("/", f)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	out := filepath.Join(t.TempDir(), "x")
	if _, err := (&Download{URL: ts.URL + "/master.m3u8", Out: out}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, f.whole(false)) {
		t.Error("content differs")
	}
}
