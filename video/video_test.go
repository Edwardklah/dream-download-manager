package video

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain lets this test program pretend to be yt-dlp: when FAKE_YTDLP is
// set, it answers like yt-dlp would and exits, instead of running the tests.
// (Go's own os/exec tests use this trick; it works on every system, unlike a
// shell script.)
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_YTDLP") != "" {
		fakeYtDlp()
		return
	}
	os.Exit(m.Run())
}

func fakeYtDlp() {
	args := os.Args[1:]
	os.WriteFile(os.Getenv("FAKE_YTDLP_ARGS"), []byte(strings.Join(args, "\n")), 0o644)
	url := args[len(args)-1]
	switch {
	case contains(args, "--version"):
		fmt.Println("2026.10.21")
	case strings.Contains(url, "gone"):
		fmt.Fprintln(os.Stderr, "WARNING: something minor")
		fmt.Fprintln(os.Stderr, "ERROR: [youtube] abc123XYZ00: Video unavailable. This video has been removed")
		os.Exit(1)
	case contains(args, "--skip-download"): // subtitles only
		out := args[indexOf(args, "-o")+1]
		lang := args[indexOf(args, "--sub-langs")+1]
		name := strings.Replace(out, "%(ext)s", lang+".vtt", 1)
		os.WriteFile(name, []byte("WEBVTT\n\n00:01.000 --> 00:02.000\nFrom yt-dlp\n"), 0o644)
	case strings.Contains(url, "notvideo"):
		fmt.Fprintln(os.Stderr, "ERROR: Unsupported URL: https://example.com/notvideo")
		os.Exit(1)
	default:
		data, _ := os.ReadFile(os.Getenv("FAKE_YTDLP_JSON"))
		os.Stdout.Write(data)
	}
	os.Exit(0)
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func loadFixture(t *testing.T) *Info {
	t.Helper()
	data, err := os.ReadFile("testdata/youtube.json")
	if err != nil {
		t.Fatal(err)
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	return &info
}

func TestChoices(t *testing.T) {
	cs := Choices(loadFixture(t))
	var got []string
	for _, c := range cs {
		got = append(got, c.Label+"="+c.ID)
	}
	want := []string{
		"2160p=701+140",   // AV1 (the HDR version is skipped)
		"1440p=400+140",   // AV1
		"1080p60=299+140", // high frame rate is its own choice
		"1080p=137+140",   // H.264 preferred over AV1 (399) and VP9 (248, WebM)
		"720p=136+140",    // the DRM stream is skipped
		"480p=135+140",
		"360p=134+140",   // separate streams beat "18" (sound built in)
		"Audio only=140", // original language, not the dubbed 140-1
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("choices:\n got  %v\n want %v", got, want)
	}
	c, _ := Find(cs, "137+140")
	if c.Ext != "mp4" || c.Detail != "MP4 · H.264" || c.Video.Downloader.ChunkSize != 10<<20 {
		t.Errorf("1080p choice: %+v", c)
	}
	v, _ := c.Video.Size()
	a, _ := c.Audio.Size()
	if c.Size != v+a || c.SizeApprox {
		t.Errorf("size %d, want %d", c.Size, v+a)
	}
	if audio, _ := Find(cs, "140"); audio.Video != nil || audio.Ext != "m4a" {
		t.Errorf("audio only: %+v", audio)
	}
}

// Sites without separate streams: the streams with sound built in are offered.
func TestChoicesCombinedOnly(t *testing.T) {
	info := &Info{Formats: []Format{
		{ID: "hd", Ext: "mp4", VCodec: "avc1", ACodec: "mp4a", Height: 720, Width: 1280, Protocol: "https", URL: "u", Filesize: 10},
		{ID: "sd", Ext: "mp4", VCodec: "avc1", ACodec: "mp4a", Height: 360, Width: 640, Protocol: "https", URL: "u", FilesizeAprx: 5},
		{ID: "hls", Ext: "mp4", VCodec: "avc1", ACodec: "mp4a", Height: 1080, Protocol: "m3u8_native", URL: "u"},
	}}
	cs := Choices(info)
	// The HLS stream is offered too now (package hls downloads it).
	if len(cs) != 3 || cs[0].ID != "hls" || cs[0].Detail != "HLS · H.264" || cs[1].ID != "hd" || cs[2].ID != "sd" ||
		cs[1].Audio != nil || !cs[2].SizeApprox {
		t.Errorf("got %+v", cs)
	}
}

// A vertical video (Shorts): quality is named by the short side.
func TestPortraitLabel(t *testing.T) {
	f := &Format{Width: 1080, Height: 1920, FPS: 30}
	if l := heightLabel(f); l != "1080p" {
		t.Errorf("got %s", l)
	}
}

func fakeTools(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	t.Setenv("FAKE_YTDLP", "1")
	t.Setenv("FAKE_YTDLP_ARGS", argsFile)
	abs, _ := filepath.Abs("testdata/youtube.json")
	t.Setenv("FAKE_YTDLP_JSON", abs)
	return Options{YtDlp: os.Args[0], JSArgs: []string{"--js-runtimes", "quickjs:/x/qjs"}}
}

func TestInspect(t *testing.T) {
	o := fakeTools(t)
	info, err := Inspect(context.Background(), "https://www.youtube.com/watch?v=abc123XYZ00", o)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != `A test video: with / odd \ characters?` || len(info.Formats) == 0 {
		t.Errorf("got %+v", info)
	}
	args, _ := os.ReadFile(os.Getenv("FAKE_YTDLP_ARGS"))
	a := strings.Split(string(args), "\n")
	// The address comes after "--", so it can never be taken for an option.
	if a[len(a)-2] != "--" || !contains(a, "-J") || !contains(a, "--no-playlist") || !contains(a, "quickjs:/x/qjs") {
		t.Errorf("yt-dlp was run with %q", a)
	}
}

func TestInspectErrors(t *testing.T) {
	o := fakeTools(t)
	_, err := Inspect(context.Background(), "https://www.youtube.com/watch?v=gone", o)
	if err == nil || err.Error() != "Video unavailable. This video has been removed" {
		t.Errorf("got %v", err)
	}
	_, err = Inspect(context.Background(), "https://example.com/notvideo", o)
	if !errors.Is(err, ErrNotVideo) {
		t.Errorf("got %v", err)
	}
}

// A fake GitHub: /releases/latest redirects to the tag, and the release files.
func fakeGitHub(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if repo, ok := strings.CutSuffix(r.URL.Path, "/releases/latest"); ok {
			w.Header().Set("Location", repo+"/releases/tag/v9")
			w.WriteHeader(http.StatusFound)
			return
		}
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(data)))
	}))
	t.Cleanup(ts.Close)
	ytdlpRepo, quickjsRepo = ts.URL+"/yt", ts.URL+"/qjs"
	return ts
}

func TestInstall(t *testing.T) {
	exe := []byte(strings.Repeat("pretend this is yt-dlp ", 50_000)) // ~1 MB
	sum := sha256.Sum256(exe)
	fakeGitHub(t, map[string][]byte{
		"/yt/releases/download/v9/" + ytdlpAsset:    exe,
		"/yt/releases/download/v9/SHA2-256SUMS":     []byte(hex.EncodeToString(sum[:]) + "  " + ytdlpAsset + "\nabc  other\n"),
		"/qjs/releases/download/v9/" + quickjsAsset: []byte("pretend qjs"),
	})
	tools := Tools{Dir: t.TempDir()}
	var seen []string
	err := tools.Install(context.Background(), func(file string, done, total int64) {
		if len(seen) == 0 || seen[len(seen)-1] != file {
			seen = append(seen, file)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(tools.YtDlp()); string(got) != string(exe) {
		t.Error("yt-dlp wasn't installed")
	}
	if got, _ := os.ReadFile(tools.QuickJS()); string(got) != "pretend qjs" {
		t.Error("qjs wasn't installed")
	}
	if len(seen) != 2 {
		t.Errorf("progress for %v", seen)
	}
	entries, _ := os.ReadDir(tools.Dir)
	if len(entries) != 2 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	fakeGitHub(t, map[string][]byte{
		"/yt/releases/download/v9/" + ytdlpAsset: []byte("tampered"),
		"/yt/releases/download/v9/SHA2-256SUMS":  []byte(strings.Repeat("0", 64) + "  " + ytdlpAsset + "\n"),
	})
	tools := Tools{Dir: t.TempDir()}
	err := tools.Install(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("got %v", err)
	}
	if tools.Installed() {
		t.Error("a file that failed its checksum was installed")
	}
	entries, _ := os.ReadDir(tools.Dir)
	if len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestIsVideoPage(t *testing.T) {
	for link, want := range map[string]bool{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":            true,
		"https://m.youtube.com/watch?v=dQw4w9WgXcQ&t=30":         true,
		"https://youtu.be/dQw4w9WgXcQ":                           true,
		"https://www.youtube.com/shorts/abc123":                  true,
		"https://music.youtube.com/watch?v=abc":                  true,
		"https://vimeo.com/123456789":                            true,
		"https://x.com/someone/status/1234567890":                true,
		"https://www.tiktok.com/@someone/video/1234567890":       true,
		"https://www.instagram.com/reel/Cabc123/":                true,
		"https://www.youtube.com/":                               false, // the home page
		"https://www.youtube.com/@channel":                       false,
		"https://www.youtube.com/watch":                          false, // no video id
		"https://vimeo.com/about":                                false,
		"https://example.com/file.zip":                           false,
		"https://rr1---sn.googlevideo.com/videoplayback?itag=18": false, // a stream, not a page
		"ftp://youtube.com/watch?v=x":                            false,
		"not a link":                                             false,
	} {
		if got := IsVideoPage(link); got != want {
			t.Errorf("IsVideoPage(%q) = %v", link, got)
		}
	}
}

// Sites that don't name codecs or sizes (common with HLS): the streams are
// still offered, named as the site names them.
func TestChoicesUnknownCodecs(t *testing.T) {
	info := &Info{Formats: []Format{
		{ID: "hls-2176", Protocol: "m3u8_native", URL: "u", Ext: "mp4", Height: 720, Width: 1280},
		{ID: "hls-800", Protocol: "m3u8_native", URL: "u", Ext: "mp4", Note: "Medium"},
		{ID: "dash-v", Protocol: "http_dash_segments", URL: "u", Ext: "mp4", VCodec: "avc1", ACodec: "none", Height: 1080},
		{ID: "webm", Protocol: "https", URL: "u", Ext: "webm", VCodec: "vp9", ACodec: "opus", Height: 480, Width: 854},
		{ID: "hevc", Protocol: "https", URL: "u", Ext: "mp4", VCodec: "hvc1.1.6.L120", ACodec: "none", Height: 1440, Width: 2560},
		{ID: "aac", Protocol: "https", URL: "u", Ext: "m4a", VCodec: "none", ACodec: "mp4a.40.2", ABR: 128},
	}}
	var got []string
	for _, c := range Choices(info) {
		got = append(got, c.Label+"="+c.ID+"/"+c.Ext+"/"+c.Detail)
	}
	want := []string{
		"1440p=hevc+aac/mp4/MP4 · HEVC",
		"720p=hls-2176/mp4/HLS",
		"480p=webm/webm/WEBM · VP9", // a complete WebM file is kept as it is
		"Medium=hls-800/mp4/HLS",
		"Audio only=aac/m4a/M4A · AAC 128 kbit/s",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("got\n %v\nwant\n %v", got, want)
	}
}

// A video without AV1: above 1080p YouTube only has VP9, in WebM. Those
// qualities are offered now (joined by package mkv) with WebM (Opus) sound.
func TestChoicesWebM(t *testing.T) {
	f := func(id, ext, vc, ac string, h int, tbr float64) Format {
		return Format{ID: id, Ext: ext, VCodec: vc, ACodec: ac, Height: h, Width: h * 16 / 9, TBR: tbr,
			ABR: tbr, Protocol: "https", URL: "u", Filesize: 1000}
	}
	info := &Info{Formats: []Format{
		f("137", "mp4", "avc1.640028", "none", 1080, 4000),
		f("248", "webm", "vp9", "none", 1080, 3000),
		f("271", "webm", "vp9", "none", 1440, 9000),
		f("313", "webm", "vp9", "none", 2160, 18000),
		f("140", "m4a", "none", "mp4a.40.2", 0, 129),
		f("251", "webm", "none", "opus", 0, 140),
	}}
	var got []string
	for _, c := range Choices(info) {
		got = append(got, c.Label+"="+c.ID+"/"+c.Ext)
	}
	want := "2160p=313+251/webm 1440p=271+251/webm 1080p=137+140/mp4 Audio only=140/m4a"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
}

func copyTo(t *testing.T, src, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	os.WriteFile(p, data, 0o644)
	return p
}

// Finish makes the right kind of file from each kind of download.
func TestFinish(t *testing.T) {
	b := Backend{}
	dir := t.TempDir()

	// MP4 picture + M4A sound -> MP4.
	out, err := b.Finish([]string{
		copyTo(t, "../mux/testdata/video.mp4", dir, "a.f1.stream"),
		copyTo(t, "../mux/testdata/audio.m4a", dir, "a.f2.stream"),
	}, filepath.Join(dir, "a.mp4"))
	if err != nil || filepath.Base(out) != "a.mp4" {
		t.Errorf("mp4: %s %v", out, err)
	}
	// WebM picture + WebM sound -> WebM.
	out, err = b.Finish([]string{
		copyTo(t, "../mkv/testdata/video.webm", dir, "b.f1.stream"),
		copyTo(t, "../mkv/testdata/audio.webm", dir, "b.f2.stream"),
	}, filepath.Join(dir, "b.webm"))
	if data, _ := os.ReadFile(out); err != nil || filepath.Base(out) != "b.webm" || !strings.Contains(string(data[:64]), "webm") {
		t.Errorf("webm: %s %v", out, err)
	}
	// An HLS download in MPEG-TS (H.264 + AAC) -> converted to MP4.
	out, err = b.Finish([]string{copyTo(t, "testdata/sample.ts", dir, "c.f1.stream")}, filepath.Join(dir, "c.mp4"))
	if data, _ := os.ReadFile(out); err != nil || filepath.Base(out) != "c.mp4" || len(data) < 12 || string(data[4:8]) != "ftyp" {
		t.Errorf("ts: %s %v", out, err)
	}
	// ...unless it holds sound MP4 conversion doesn't handle (MP3): kept as .ts.
	out, err = b.Finish([]string{copyTo(t, "../mpegts/testdata/mp3.ts", dir, "e.f1.stream")}, filepath.Join(dir, "e.mp4"))
	if err != nil || filepath.Base(out) != "e.ts" {
		t.Errorf("mp3 ts: %s %v", out, err)
	}
	// Separate picture and sound playlists, both MPEG-TS -> joined into MP4.
	out, err = b.Finish([]string{
		copyTo(t, "../mpegts/testdata/video_only.ts", dir, "f.f1.stream"),
		copyTo(t, "../mpegts/testdata/audio_only.ts", dir, "f.f2.stream"),
	}, filepath.Join(dir, "f.mp4"))
	if err != nil || filepath.Base(out) != "f.mp4" {
		t.Errorf("two ts: %s %v", out, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.fmp4")); len(left) > 0 {
		t.Errorf("temporary files left: %v", left)
	}
	// A complete file in any other form -> just renamed.
	out, err = b.Finish([]string{copyTo(t, "../mkv/testdata/video.webm", dir, "d.f1.stream")}, filepath.Join(dir, "d.webm"))
	if err != nil || filepath.Base(out) != "d.webm" {
		t.Errorf("complete file: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d.f1.stream")); !os.IsNotExist(err) {
		t.Error("the stream file should have been moved, not copied")
	}
}

func TestCookieFile(t *testing.T) {
	path, err := writeCookieFile("https://www.instagram.com/reel/abc/", "sessionid=XYZ; csrftoken=abc=def; bad")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	data, _ := os.ReadFile(path)
	want := "# Netscape HTTP Cookie File\n" +
		".instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tXYZ\n" +
		".instagram.com\tTRUE\t/\tTRUE\t0\tcsrftoken\tabc=def\n"
	if string(data) != want {
		t.Errorf("got\n%s", data)
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("readable by others: %v", st.Mode().Perm())
	}
}

func TestParseProgress(t *testing.T) {
	for line, want := range map[string][3]float64{
		"DLM 1048576 10485760 NA 524288.5":      {1048576, 10485760, 524288.5},
		"DLM 2000 NA 9000 NA":                   {2000, 9000, 0}, // only an estimate of the total
		"[download] Destination: something.mp4": {-1, 0, 0},
	} {
		done, total, speed, ok := parseProgress(line)
		if want[0] < 0 {
			if ok {
				t.Errorf("%q should be ignored", line)
			}
			continue
		}
		if !ok || float64(done) != want[0] || float64(total) != want[1] || speed != want[2] {
			t.Errorf("%q: %d %d %v %v", line, done, total, speed, ok)
		}
	}
}

// The update check asks GitHub at most once per period, and says whether
// the release is newer than what's installed.
func TestCheckForUpdate(t *testing.T) {
	fakeTools(t) // our fake yt-dlp says it's 2026.10.21
	asked := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.Header().Set("Location", "/yt/releases/tag/2026.10.30")
		w.WriteHeader(http.StatusFound)
	}))
	defer ts.Close()
	ytdlpRepo = ts.URL + "/yt"
	tools := Tools{Dir: t.TempDir()}
	exe, _ := os.ReadFile(os.Args[0])
	os.WriteFile(tools.YtDlp(), exe, 0o755)

	u, err := tools.CheckForUpdate(context.Background(), 7*24*time.Hour)
	if err != nil || u.Current != "2026.10.21" || u.Latest != "2026.10.30" || !u.Newer() {
		t.Fatalf("got %+v %v", u, err)
	}
	u, _ = tools.CheckForUpdate(context.Background(), 7*24*time.Hour)
	if asked != 1 || !u.Newer() {
		t.Errorf("GitHub asked %d times (want 1: the answer is remembered)", asked)
	}
	tools.CheckForUpdate(context.Background(), 0) // due again
	if asked != 2 {
		t.Errorf("GitHub asked %d times, want 2", asked)
	}
	if (UpdateCheck{Current: "2026.10.30", Latest: "2026.10.30"}).Newer() ||
		!(UpdateCheck{Current: "2026.10.30", Latest: "2026.10.30.1"}).Newer() {
		t.Error("version comparison")
	}
}

// Each system gets its own tool files, by their names in the releases.
func TestToolAssets(t *testing.T) {
	for _, c := range []struct{ goos, arch, ytdlp, qjs string }{
		{"windows", "amd64", "yt-dlp.exe", "qjs-windows-x86_64.exe"},
		{"windows", "arm64", "yt-dlp.exe", "qjs-windows-x86_64.exe"},
		{"linux", "amd64", "yt-dlp_linux", "qjs-linux-x86_64"},
		{"linux", "arm64", "yt-dlp_linux_aarch64", "qjs-linux-aarch64"},
	} {
		y, q := toolAssets(c.goos, c.arch)
		if y != c.ytdlp || q != c.qjs {
			t.Errorf("%s/%s: got %s, %s", c.goos, c.arch, y, q)
		}
	}
}

// The picker's look-up is reused once by the download, then asked afresh.
func TestRecentLookUpUsedOnce(t *testing.T) {
	c := &lookCache{m: map[string]cachedLook{}}
	info := &Info{Title: "x"}
	k := lookKey("https://example.com/v", map[string]string{"Cookie": "a=1", "User-Agent": "UA"})
	c.keep(k, info)
	if c.take(lookKey("https://example.com/v", nil)) != nil {
		t.Error("used for a request without the browser's cookies")
	}
	if c.take(k) != info {
		t.Fatal("not reused")
	}
	if c.take(k) != nil {
		t.Error("used twice")
	}
	c.m[k] = cachedLook{info, time.Now().Add(-recentFor - time.Second)}
	if c.take(k) != nil {
		t.Error("too old, but used")
	}
}
