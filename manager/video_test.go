package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dlmanager/video"
)

// fakeVideo stands in for yt-dlp and the joiner: the "video" is two files
// on a test server, and joining just puts them one after the other.
type fakeVideo struct {
	base        string // the test server's address
	mu          sync.Mutex
	resolves    int
	expireFirst bool  // the first lookup hands out addresses that have expired
	err         error // make every lookup fail with this
	audioOnly   bool
}

func (f *fakeVideo) YtDlpDownload(ctx context.Context, pageURL, spec, out string, headers map[string]string, progress video.YtDlpProgress) error {
	return os.WriteFile(out, []byte("downloaded by yt-dlp: "+spec), 0o644)
}

func (f *fakeVideo) Resolve(ctx context.Context, pageURL, choiceID string, headers map[string]string) (*video.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.err != nil {
		return nil, f.err
	}
	prefix := "/"
	if f.expireFirst && f.resolves == 1 {
		prefix = "/expired-"
	}
	if f.audioOnly {
		return &video.Plan{Title: "Song", Ext: "m4a", Streams: []video.Stream{
			{FormatID: "140", URL: f.base + prefix + "a.bin", Size: 300_000},
		}}, nil
	}
	info := &video.Info{Subtitles: map[string][]video.SubFormat{"en": {{Ext: "vtt", URL: "sub-en"}}}}
	return &video.Plan{Title: "My Video", Ext: "mp4", Info: info, Streams: []video.Stream{
		{FormatID: "137", URL: f.base + prefix + "v.bin", Size: 1_200_000, ChunkSize: 256 << 10,
			Headers: map[string]string{"User-Agent": "test-agent"}},
		{FormatID: "140", URL: f.base + prefix + "a.bin", Size: 300_000},
	}}, nil
}

func (f *fakeVideo) Finish(streams []string, out string) (string, error) {
	var all []byte
	for _, s := range streams {
		data, err := os.ReadFile(s)
		if err != nil {
			return "", err
		}
		all = append(all, data...)
	}
	return out, os.WriteFile(out, all, 0o644)
}

func (f *fakeVideo) SaveSubtitle(ctx context.Context, pageURL, id string, formats []video.SubFormat,
	videoPath string, headers map[string]string) (string, error) {
	if id == "broken" {
		return "", errors.New("the site refused")
	}
	if len(formats) != 1 || formats[0].URL != "sub-"+id {
		return "", fmt.Errorf("wrong formats for %s: %+v", id, formats)
	}
	path := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + "." + id + ".srt"
	return path, os.WriteFile(path, []byte("subtitles"), 0o644)
}

func (f *fakeVideo) SubtitleFormats(ctx context.Context, pageURL, id string, headers map[string]string) ([]video.SubFormat, error) {
	if id == "missing" {
		return nil, errors.New("no subtitles in that language")
	}
	return []video.SubFormat{{Ext: "vtt", URL: "sub-" + id}}, nil
}

func videoSetup(t *testing.T, delay time.Duration) (*Manager, *fileServer, *fakeVideo, []byte) {
	t.Helper()
	fs, ts := newFileServer(t, delay)
	v := fs.add("v.bin", 1_200_000)
	a := fs.add("a.bin", 300_000)
	m := newManager(t, t.TempDir(), Settings{MaxActive: 3, Connections: 4})
	fv := &fakeVideo{base: ts.URL}
	m.SetVideoBackend(fv)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.Start(ctx)
	return m, fs, fv, append(append([]byte{}, v...), a...)
}

func streamLeftovers(t *testing.T, folder string) []string {
	t.Helper()
	var left []string
	entries, _ := os.ReadDir(folder)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".stream") {
			left = append(left, e.Name())
		}
	}
	return left
}

func TestVideoDownload(t *testing.T) {
	m, _, fv, want := videoSetup(t, 0)
	it, err := m.AddVideo("https://www.youtube.com/watch?v=abc", VideoOptions{
		ChoiceID: "137+140", Quality: "1080p", Title: "My Video", Ext: "mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if it.Kind != KindVideo || it.Filename != "My Video.mp4" {
		t.Errorf("added %+v", it)
	}
	waitFor(t, "finished", func() bool {
		x, _ := m.Get(it.ID)
		if x.Status == Failed {
			t.Fatalf("failed: %s", x.Problem)
		}
		return x.Status == Completed
	})
	done, _ := m.Get(it.ID)
	got, _ := os.ReadFile(done.SavedPath)
	if !bytes.Equal(got, want) {
		t.Fatalf("the joined file differs (%d vs %d bytes); problem %q", len(got), len(want), done.Problem)
	}
	if left := streamLeftovers(t, done.Folder); len(left) > 0 {
		t.Errorf("stream files left behind: %v", left)
	}
	if fv.resolves != 1 || done.Stage != "" || done.Downloaded != done.Total {
		t.Errorf("resolves %d, stage %q, %d of %d", fv.resolves, done.Stage, done.Downloaded, done.Total)
	}
	// The same video in the same quality again is a duplicate...
	if _, err := m.AddVideo("https://www.youtube.com/watch?v=abc", VideoOptions{
		ChoiceID: "137+140", Title: "My Video", Ext: "mp4"}); err != nil {
		t.Errorf("a finished video should be addable again, got %v", err)
	}
}

func TestVideoNameNotTaken(t *testing.T) {
	m, _, _, _ := videoSetup(t, 50*time.Millisecond)
	folder := m.Settings().Folder
	os.MkdirAll(folder, 0o755)
	os.WriteFile(filepath.Join(folder, "My Video.mp4"), []byte("someone else's file"), 0o644)
	it, _ := m.AddVideo("https://example.com/watch/1", VideoOptions{ChoiceID: "137+140", Title: "My Video", Ext: "mp4"})
	it2, _ := m.AddVideo("https://example.com/watch/2", VideoOptions{ChoiceID: "137+140", Title: "My Video", Ext: "mp4"})
	if it.Filename != "My Video (1).mp4" || it2.Filename != "My Video (2).mp4" {
		t.Errorf("names %q %q", it.Filename, it2.Filename)
	}
	// A duplicate while the first is still in the list:
	if _, err := m.AddVideo("https://example.com/watch/1", VideoOptions{ChoiceID: "137+140", Title: "x", Ext: "mp4"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("expected a duplicate, got %v", err)
	}
}

// Stream addresses expire after a few hours. A "403 Forbidden" makes the
// manager look the video up again and carry on.
func TestVideoExpiredAddressIsRefreshed(t *testing.T) {
	m, _, fv, want := videoSetup(t, 0)
	fv.expireFirst = true
	it, _ := m.AddVideo("https://www.youtube.com/watch?v=abc", VideoOptions{ChoiceID: "137+140", Title: "My Video", Ext: "mp4"})
	waitFor(t, "finished", func() bool { s := status(m, it.ID); return s == Completed || s == Failed })
	done, _ := m.Get(it.ID)
	got, _ := os.ReadFile(done.SavedPath)
	if done.Status != Completed || !bytes.Equal(got, want) || fv.resolves != 2 {
		t.Errorf("status %s (%q), resolves %d", done.Status, done.Problem, fv.resolves)
	}
}

// Pause partway, resume: the video is looked up again and continues.
func TestVideoPauseAndResume(t *testing.T) {
	m, _, fv, want := videoSetup(t, 5*time.Millisecond)
	it, _ := m.AddVideo("https://www.youtube.com/watch?v=abc", VideoOptions{ChoiceID: "137+140", Title: "My Video", Ext: "mp4"})
	waitFor(t, "some progress", func() bool { x, _ := m.Get(it.ID); return x.Downloaded > 200_000 })
	m.Pause(it.ID)
	waitFor(t, "paused", func() bool { return status(m, it.ID) == Paused })
	m.Resume(it.ID)
	waitFor(t, "finished", func() bool { return status(m, it.ID) == Completed })
	done, _ := m.Get(it.ID)
	got, _ := os.ReadFile(done.SavedPath)
	if !bytes.Equal(got, want) || fv.resolves != 2 {
		t.Errorf("content ok=%v, resolves %d", bytes.Equal(got, want), fv.resolves)
	}
}

func TestVideoRemoveDeletesStreams(t *testing.T) {
	m, _, _, _ := videoSetup(t, 5*time.Millisecond)
	it, _ := m.AddVideo("https://www.youtube.com/watch?v=abc", VideoOptions{ChoiceID: "137+140", Title: "My Video", Ext: "mp4"})
	waitFor(t, "some progress", func() bool { x, _ := m.Get(it.ID); return x.Downloaded > 200_000 })
	folder := it.Folder
	m.Remove(it.ID, true)
	waitFor(t, "files removed", func() bool {
		entries, _ := os.ReadDir(folder)
		return len(entries) == 0
	})
}

func TestVideoLookupFails(t *testing.T) {
	m, _, fv, _ := videoSetup(t, 0)
	fv.err = errors.New("Video unavailable")
	it, _ := m.AddVideo("https://www.youtube.com/watch?v=gone", VideoOptions{ChoiceID: "137+140", Title: "x", Ext: "mp4"})
	waitFor(t, "failed", func() bool { return status(m, it.ID) == Failed })
	if x, _ := m.Get(it.ID); x.Problem != "Video unavailable" {
		t.Errorf("problem %q", x.Problem)
	}
}

func TestAudioOnlyVideo(t *testing.T) {
	m, fs, fv, _ := videoSetup(t, 0)
	fv.audioOnly = true
	it, _ := m.AddVideo("https://www.youtube.com/watch?v=abc", VideoOptions{ChoiceID: "140", Quality: "Audio only", Title: "Song", Ext: "m4a"})
	waitFor(t, "finished", func() bool { return status(m, it.ID) == Completed })
	done, _ := m.Get(it.ID)
	got, _ := os.ReadFile(done.SavedPath)
	fs.mu.Lock()
	want := fs.files["a.bin"]
	fs.mu.Unlock()
	if filepath.Base(done.SavedPath) != "Song.m4a" || !bytes.Equal(got, want) {
		t.Errorf("saved %s, content ok=%v", done.SavedPath, bytes.Equal(got, want))
	}
}

// A site that streams with HLS: the manager downloads the pieces with
// package hls and finishes the file the same way.
type hlsVideo struct {
	fakeVideo
	playlist string
}

func (f *hlsVideo) Resolve(ctx context.Context, pageURL, choiceID string, headers map[string]string) (*video.Plan, error) {
	return &video.Plan{Title: "Streamed", Ext: "mp4", Streams: []video.Stream{
		{FormatID: "hls-720", HLS: true, URL: f.playlist},
	}}, nil
}

func TestHLSVideo(t *testing.T) {
	var pieces [][]byte
	var want []byte
	for i := range 12 {
		p := bytes.Repeat([]byte{byte(i)}, 30_000+i)
		pieces = append(pieces, p)
		want = append(want, p...)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		for i := range pieces {
			fmt.Fprintf(&b, "#EXTINF:4,\npiece%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, "/piece%d.ts", &i); err != nil || i >= len(pieces) {
			http.NotFound(w, r)
			return
		}
		w.Write(pieces[i])
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	m := newManager(t, t.TempDir(), Settings{MaxActive: 3, Connections: 4})
	m.SetVideoBackend(&hlsVideo{playlist: ts.URL + "/index.m3u8"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	it, err := m.AddVideo("https://streams.example.com/watch/1", VideoOptions{ChoiceID: "hls-720", Title: "Streamed", Ext: "mp4"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "finished", func() bool {
		x, _ := m.Get(it.ID)
		if x.Status == Failed {
			t.Fatalf("failed: %s", x.Problem)
		}
		return x.Status == Completed
	})
	done, _ := m.Get(it.ID)
	got, _ := os.ReadFile(done.SavedPath)
	if !bytes.Equal(got, want) {
		t.Errorf("content differs: %d vs %d bytes", len(got), len(want))
	}
	if left := streamLeftovers(t, done.Folder); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

// The fallback: a format only yt-dlp can download.
type ytdlpVideo struct{ fakeVideo }

func (f *ytdlpVideo) Resolve(ctx context.Context, pageURL, choiceID string, headers map[string]string) (*video.Plan, error) {
	f.mu.Lock()
	f.resolves++
	f.mu.Unlock()
	if headers["Cookie"] != "session=1" {
		return nil, errors.New("the browser's cookies didn't arrive")
	}
	return &video.Plan{Title: "Clip", Ext: "mp4", Streams: []video.Stream{{FormatID: "dash-1", YtDlp: "dash-1"}}}, nil
}

func TestYtDlpFallbackWithCookies(t *testing.T) {
	m := newManager(t, t.TempDir(), Settings{MaxActive: 3, Connections: 4})
	m.SetVideoBackend(&ytdlpVideo{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	it, err := m.AddVideo("https://www.instagram.com/reel/abc/", VideoOptions{
		ChoiceID: "ytdlp:dash-1", Title: "Clip", Ext: "mp4", Headers: map[string]string{"Cookie": "session=1"}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "finished", func() bool {
		x, _ := m.Get(it.ID)
		if x.Status == Failed {
			t.Fatalf("failed: %s", x.Problem)
		}
		return x.Status == Completed
	})
	done, _ := m.Get(it.ID)
	if got, _ := os.ReadFile(done.SavedPath); string(got) != "downloaded by yt-dlp: dash-1" {
		t.Errorf("got %q", got)
	}
}

func TestSameSite(t *testing.T) {
	for _, c := range []struct {
		page, addr string
		want       bool
	}{
		{"https://www.instagram.com/reel/x", "https://instagram.com/api/v", true},
		{"https://www.instagram.com/reel/x", "https://scontent.cdninstagram.com/v.mp4", false},
		{"https://example.com/watch", "https://media.example.com/v.mp4", true},
		{"https://example.com/watch", "https://notexample.com/v.mp4", false},
	} {
		if got := sameSite(c.page, c.addr); got != c.want {
			t.Errorf("sameSite(%s, %s) = %v", c.page, c.addr, got)
		}
	}
}

// A subtitle language chosen in the picker is saved next to the video; if
// that fails, the video is still fine - with a note saying so.
func TestVideoWithSubtitles(t *testing.T) {
	m, _, _, _ := videoSetup(t, 0)
	finished := func(id int) Item {
		waitFor(t, "finished", func() bool {
			x, _ := m.Get(id)
			if x.Status == Failed {
				t.Fatalf("failed: %s", x.Problem)
			}
			return x.Status == Completed
		})
		x, _ := m.Get(id)
		return x
	}
	it, err := m.AddVideo("https://www.youtube.com/watch?v=abc", VideoOptions{
		ChoiceID: "137+140", Title: "Subbed", Ext: "mp4", Subtitle: "en"})
	if err != nil {
		t.Fatal(err)
	}
	done := finished(it.ID)
	if data, err := os.ReadFile(filepath.Join(done.Folder, "Subbed.en.srt")); err != nil || string(data) != "subtitles" {
		t.Errorf("subtitles: %q %v", data, err)
	}
	if done.Note != "" || done.Subtitle != "en" {
		t.Errorf("note %q, subtitle %q", done.Note, done.Subtitle)
	}

	it, _ = m.AddVideo("https://www.youtube.com/watch?v=def", VideoOptions{
		ChoiceID: "137+140", Title: "Unlucky", Ext: "mp4", Subtitle: "broken"})
	done = finished(it.ID)
	if !strings.Contains(done.Note, "subtitles couldn't be saved: the site refused") {
		t.Errorf("note %q", done.Note)
	}
	if _, err := os.Stat(done.SavedPath); err != nil {
		t.Errorf("the video should be there: %v", err)
	}
}

// Subtitles only, without the video: saved under the video's name, next to
// where the video would go.
func TestSubtitlesOnly(t *testing.T) {
	m, _, _, _ := videoSetup(t, 0)
	it, err := m.AddSubtitles("https://www.youtube.com/watch?v=abc", "Episode 5: The End?", "en", nil)
	if err != nil {
		t.Fatal(err)
	}
	if it.Kind != KindSubtitles {
		t.Errorf("kind %q", it.Kind)
	}
	waitFor(t, "saved", func() bool {
		x, _ := m.Get(it.ID)
		if x.Status == Failed {
			t.Fatalf("failed: %s", x.Problem)
		}
		return x.Status == Completed
	})
	done, _ := m.Get(it.ID)
	if data, err := os.ReadFile(done.SavedPath); err != nil || string(data) != "subtitles" {
		t.Fatalf("%s: %q %v", done.SavedPath, data, err)
	}
	if filepath.Base(done.SavedPath) != "Episode 5_ The End_.en.srt" || done.Filename != filepath.Base(done.SavedPath) {
		t.Errorf("saved as %s (%s)", done.SavedPath, done.Filename)
	}
	if _, err := m.AddSubtitles("https://www.youtube.com/watch?v=abc", "x", "", nil); err == nil {
		t.Error("no language should be refused")
	}
	bad, _ := m.AddSubtitles("https://www.youtube.com/watch?v=abc", "x", "missing", nil)
	waitFor(t, "failed", func() bool { return status(m, bad.ID) == Failed })
}
