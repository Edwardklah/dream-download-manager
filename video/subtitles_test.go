package video

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVTTToSRT(t *testing.T) {
	vtt := "\uFEFFWEBVTT\r\nKind: captions\r\nLanguage: en\r\n\r\n" +
		"STYLE\r\n::cue { color: yellow }\r\n\r\n" +
		"NOTE a comment\r\nover two lines\r\n\r\n" +
		"intro\r\n00:01.000 --> 00:03.500 align:start position:10%\r\n<v Narrator>Hello &amp; <i>welcome</i></v>\r\n<c.yellow>to the show</c>\r\n\r\n" +
		"1:02:03.004 --> 1:02:05.000\r\nLate &lt;line&gt;\r\n"
	got, err := VTTToSRT([]byte(vtt))
	if err != nil {
		t.Fatal(err)
	}
	want := "1\r\n00:00:01,000 --> 00:00:03,500\r\nHello & <i>welcome</i>\r\nto the show\r\n\r\n" +
		"2\r\n01:02:03,004 --> 01:02:05,000\r\nLate <line>\r\n\r\n"
	if string(got) != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	if _, err := VTTToSRT([]byte("1\n00:00:01,000 --> 00:00:02,000\nhi\n")); err == nil {
		t.Error("an SRT file was accepted as WebVTT")
	}
}

// YouTube's automatic captions roll: each cue repeats the previous line and
// adds the next, word by word. The SRT should show each line once.
func TestVTTToSRTRollingCaptions(t *testing.T) {
	vtt := `WEBVTT
Kind: captions
Language: en

00:00:00.400 --> 00:00:02.990 align:start position:0%
 
so<00:00:00.799><c> today</c><00:00:01.120><c> we're</c><00:00:01.360><c> building</c>

00:00:02.990 --> 00:00:03.000 align:start position:0%
so today we're building
 

00:00:03.000 --> 00:00:05.510 align:start position:0%
so today we're building
a<00:00:03.360><c> download</c><00:00:03.760><c> manager</c>

00:00:05.510 --> 00:00:05.520 align:start position:0%
a download manager
 

00:00:05.520 --> 00:00:08.000 align:start position:0%
a download manager
in<00:00:05.900><c> Go</c>
`
	got, err := VTTToSRT([]byte(vtt))
	if err != nil {
		t.Fatal(err)
	}
	want := "1\r\n00:00:00,400 --> 00:00:03,000\r\nso today we're building\r\n\r\n" +
		"2\r\n00:00:03,000 --> 00:00:05,520\r\na download manager\r\n\r\n" +
		"3\r\n00:00:05,520 --> 00:00:08,000\r\nin Go\r\n\r\n"
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestSubtitleChoices(t *testing.T) {
	info := &Info{
		Language: "en",
		Subtitles: map[string][]SubFormat{
			"fr":        {{Ext: "vtt", Name: "French"}},
			"en":        {{Ext: "vtt", Name: "English"}},
			"live_chat": {{Ext: "json"}},
		},
		AutoCaptions: map[string][]SubFormat{
			"en-orig": {{Ext: "vtt", Name: "English (Original)"}},
			"en":      {{Ext: "vtt", Name: "English"}},
			"de":      {{Ext: "vtt", Name: "German"}}, // a machine translation: not offered
		},
	}
	var got []string
	for _, s := range SubtitleChoices(info) {
		got = append(got, s.ID+"="+s.Name)
	}
	want := "en=English fr=French auto:en-orig=English (auto-generated)"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if fs := SubtitleFormats(info, "auto:en-orig"); len(fs) != 1 || fs[0].Name != "English (Original)" {
		t.Errorf("formats: %+v", fs)
	}
}

// Saved next to the video, named like it, converted to SRT. Cookies don't
// go to a different server.
func TestSaveSubtitle(t *testing.T) {
	var cookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		w.Write([]byte("WEBVTT\n\n00:01.000 --> 00:02.000\nHi there\n"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	video := filepath.Join(dir, "My video.mp4")
	formats := []SubFormat{{Ext: "json3", URL: srv.URL + "/j"}, {Ext: "vtt", URL: srv.URL + "/v"}}
	path, err := Backend{}.SaveSubtitle(context.Background(), "https://example.com/watch", "auto:en-orig", formats,
		video, map[string]string{"Cookie": "secret=1"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "My video.en.srt" {
		t.Errorf("saved as %s", path)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "00:00:01,000 --> 00:00:02,000\r\nHi there") {
		t.Errorf("got %q", data)
	}
	if cookie != "" {
		t.Error("the site's cookies were sent to another server")
	}
	// ASS subtitles (styled) are kept as they are.
	ass := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[Script Info]\nTitle: x\n"))
	}))
	defer ass.Close()
	path, err = Backend{}.SaveSubtitle(context.Background(), "https://example.com/watch", "ja",
		[]SubFormat{{Ext: "ass", URL: ass.URL}}, video, nil)
	if err != nil || filepath.Base(path) != "My video.ja.ass" {
		t.Errorf("ass: %s %v", path, err)
	}
}

// When the subtitle file can't be fetched directly, yt-dlp fetches it.
func TestSaveSubtitleFallsBackToYtDlp(t *testing.T) {
	fakeTools(t)
	tools := Tools{Dir: t.TempDir()}
	exe, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(tools.YtDlp(), exe, 0o755)

	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer refused.Close()
	dir := t.TempDir()
	path, err := Backend{Tools: tools}.SaveSubtitle(context.Background(), "https://www.youtube.com/watch?v=abc", "en",
		[]SubFormat{{Ext: "vtt", URL: refused.URL}}, filepath.Join(dir, "V [1].mp4"), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if filepath.Base(path) != "V [1].en.srt" || !strings.Contains(string(data), "From yt-dlp") {
		t.Errorf("%s: %q", path, data)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*dlm-sub*")); len(left) > 0 {
		t.Errorf("leftovers: %v", left)
	}
	args, _ := os.ReadFile(os.Getenv("FAKE_YTDLP_ARGS"))
	if a := strings.Split(string(args), "\n"); !contains(a, "--write-subs") || contains(a, "--write-auto-subs") {
		t.Errorf("args %q", a)
	}
}

// Overlapping subtitles (YouTube's automatic captions as SRT) become
// subtitles that don't overlap, the older line on top - players stack
// overlapping ones newest-first, so the text read backwards.
func TestOverlappingSRTFlattened(t *testing.T) {
	in := "1\r\n00:00:00,000 --> 00:00:05,000\r\nfirst line\r\n\r\n" +
		"2\r\n00:00:02,500 --> 00:00:07,500\r\nsecond line\r\n\r\n" +
		"3\r\n00:00:05,000 --> 00:00:09,000\r\nthird line\r\n\r\n"
	want := "1\r\n00:00:00,000 --> 00:00:02,500\r\nfirst line\r\n\r\n" +
		"2\r\n00:00:02,500 --> 00:00:05,000\r\nfirst line\r\nsecond line\r\n\r\n" +
		"3\r\n00:00:05,000 --> 00:00:07,500\r\nsecond line\r\nthird line\r\n\r\n" +
		"4\r\n00:00:07,500 --> 00:00:09,000\r\nthird line\r\n\r\n"
	if got := string(fixSRT([]byte(in))); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// An SRT file without overlaps is kept exactly as it was.
func TestSRTWithoutOverlapsUntouched(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:02,000\n<i>Hello</i>\n\n2\n00:00:02,000 --> 00:00:03,500\nWorld\n"
	if got := string(fixSRT([]byte(in))); got != in {
		t.Errorf("changed:\n%q", got)
	}
}
