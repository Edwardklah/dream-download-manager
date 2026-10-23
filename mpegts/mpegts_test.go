package mpegts

import (
	"bytes"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

	"dlmanager/mux"
)

// The test files were made with ffmpeg from its test patterns (3 s, tiny):
//
//	h264_aac.ts    H.264 with B-frames + AAC, as HLS sites send
//	hevc_aac.ts    H.265 + AAC
//	wrap.ts        like h264_aac, but the 33-bit clock wraps around mid-file
//	mp3.ts         H.264 + MP3: not convertible
//	video_only.ts  and audio_only.ts: separate streams, to be joined
//
// ffmpeg -f lavfi -i testsrc2=size=160x90:rate=25 -f lavfi -i sine=frequency=440:sample_rate=44100 -t 3 \
//   -c:v libx264 -bf 2 -g 25 -c:a aac -b:a 64k -f mpegts h264_aac.ts   (and so on)

func convertTS(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := ToMP4(filepath.Join("testdata", name), out); err != nil {
		t.Fatalf("ToMP4(%s): %v", name, err)
	}
	return out
}

// ordinary: the result is an ordinary (not fragmented) MP4.
func ordinary(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parsed, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.IsFragmented() {
		t.Fatal("the result is still fragmented")
	}
}

func ffprobe(t *testing.T, path, entries string) []string {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-count_packets", "-show_entries", entries,
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// decodesCleanly: ffmpeg decodes every frame without a single error.
func decodesCleanly(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	var stderr bytes.Buffer
	cmd := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil || stderr.Len() > 0 {
		t.Fatalf("ffmpeg found errors decoding %s: %v\n%s", path, err, stderr.String())
	}
}

type probed struct {
	codec   string
	start   float64
	packets int
}

func probeStreams(t *testing.T, path string) []probed {
	var res []probed
	seen := map[string]bool{} // a .ts lists its streams twice (also under its programme)
	for _, l := range ffprobe(t, path, "stream=index,codec_name,start_time,nb_read_packets") {
		f := strings.Split(l, ",")
		if len(f) < 4 || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		start, _ := strconv.ParseFloat(f[2], 64)
		n, _ := strconv.Atoi(f[3])
		res = append(res, probed{f[1], start, n})
	}
	return res
}

// Every frame arrives, the result decodes cleanly, and picture and sound
// start at the same moments relative to each other as in the .ts.
func TestConvert(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "hevc_aac.ts", "wrap.ts"} {
		t.Run(name, func(t *testing.T) {
			out := convertTS(t, name)
			ordinary(t, out)
			decodesCleanly(t, out)
			in := probeStreams(t, filepath.Join("testdata", name))
			got := probeStreams(t, out)
			if len(got) != len(in) {
				t.Fatalf("streams: got %v, want %v", got, in)
			}
			for i := range in {
				if got[i].codec != in[i].codec || got[i].packets != in[i].packets {
					t.Errorf("stream %d: got %+v, want %+v", i, got[i], in[i])
				}
			}
			// The gap between picture and sound must be kept (to a
			// millisecond or so): that's lip sync.
			wantGap := in[0].start - in[1].start
			gotGap := got[0].start - got[1].start
			if math.Abs(wantGap-gotGap) > 0.002 {
				t.Errorf("picture-sound offset: got %.4f s, want %.4f s", gotGap, wantGap)
			}
		})
	}
}

// The samples are copied byte for byte: decoding the MP4 gives exactly the
// frames decoding the .ts gives.
func TestSameFramesAsSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := convertTS(t, "h264_aac.ts")
	hash := func(path, stream string) string {
		b, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", stream, "-f", "framemd5", "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		// Keep only the frames' hashes (the last field), not their times.
		var hs []string
		for _, l := range strings.Split(string(b), "\n") {
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			f := strings.Split(l, ",")
			hs = append(hs, strings.TrimSpace(f[len(f)-1]))
		}
		return strings.Join(hs, "\n")
	}
	for _, s := range []string{"0:v", "0:a"} {
		if hash(filepath.Join("testdata", "h264_aac.ts"), s) != hash(out, s) {
			t.Errorf("%s: the decoded frames differ from the .ts's", s)
		}
	}
}

// MP3 sound can't go in our MP4: the caller is told, and keeps the .ts.
func TestUnsupported(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.mp4")
	err := ToMP4(filepath.Join("testdata", "mp3.ts"), out)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a broken output file was left behind")
	}
	// Not a TS at all.
	notTS := filepath.Join(t.TempDir(), "x.ts")
	os.WriteFile(notTS, bytes.Repeat([]byte("hello"), 1000), 0o644)
	if err := ToMP4(notTS, out); !errors.Is(err, ErrUnsupported) {
		t.Errorf("not a TS: got %v, want ErrUnsupported", err)
	}
}

// Picture and sound from two separate playlists: each becomes a fragmented
// MP4, and package mux joins them, in sync.
func TestJoinSeparateStreams(t *testing.T) {
	dir := t.TempDir()
	v, a, out := filepath.Join(dir, "v.mp4"), filepath.Join(dir, "a.mp4"), filepath.Join(dir, "out.mp4")
	if err := ToFragmented(filepath.Join("testdata", "video_only.ts"), v); err != nil {
		t.Fatal(err)
	}
	if err := ToFragmented(filepath.Join("testdata", "audio_only.ts"), a); err != nil {
		t.Fatal(err)
	}
	if err := mux.Merge(v, a, out); err != nil {
		t.Fatal(err)
	}
	decodesCleanly(t, out)
	got := probeStreams(t, out)
	if len(got) != 2 || got[0].codec != "h264" || got[1].codec != "aac" || got[0].packets != 75 || got[1].packets != 131 {
		t.Fatalf("got %+v", got)
	}
	// In the .ts files the picture starts 0.08 s after the sound (B-frames).
	if gap := got[0].start - got[1].start; math.Abs(gap-0.08) > 0.002 {
		t.Errorf("picture-sound offset: got %.4f s, want 0.08 s", gap)
	}
}

// A damaged spot (garbage between packets) is skipped, not fatal.
func TestDamagedPacketsAreSkipped(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "h264_aac.ts"))
	if err != nil {
		t.Fatal(err)
	}
	cut := 188 * 200
	bad := append(append(append([]byte{}, data[:cut]...), []byte("garbage!")...), data[cut:]...)
	in := filepath.Join(t.TempDir(), "bad.ts")
	os.WriteFile(in, bad, 0o644)
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := ToMP4(in, out); err != nil {
		t.Fatal(err)
	}
	if got := probeStreams(t, out); len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
}

// HLS pieces from different sources (an ad break, a re-encoded part) can
// restart the clock: a "discontinuity". The result must still play straight
// through, all of it, without a jump.
func TestDiscontinuity(t *testing.T) {
	a, err := os.ReadFile(filepath.Join("testdata", "h264_aac.ts"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("testdata", "wrap.ts")) // a very different clock
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(t.TempDir(), "joined.ts")
	os.WriteFile(in, append(append([]byte{}, a...), b...), 0o644)
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := ToMP4(in, out); err != nil {
		t.Fatal(err)
	}
	decodesCleanly(t, out)
	got := probeStreams(t, out)
	if len(got) != 2 || got[0].packets != 150 || got[1].packets != 262 {
		t.Fatalf("got %+v, want all 150 frames and 262 sound frames", got)
	}
	lines := ffprobe(t, out, "format=duration")
	d, _ := strconv.ParseFloat(lines[0], 64)
	if d < 5.9 || d > 6.2 {
		t.Errorf("duration %.2f s, want about 6 s (two 3 s parts, no gap)", d)
	}
}

// splitVideoPES rewrites the .ts at in, the way some encoders (and sites)
// lay out the video, to test that we cope:
//   - chunk > 0: every picture arrives in several PES packets of at most
//     chunk bytes, only the first with a timestamp;
//   - shift > 0: each PES packet starts with the last shift bytes of the
//     picture before it (PES packets not lined up with pictures).
//
// Other packets pass through unchanged.
func splitVideoPES(t *testing.T, in string, chunk, shift int) string {
	t.Helper()
	raw, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	d := newDemuxer()
	for i := 0; i+188 <= len(raw); i += 188 {
		d.feed(raw[i:i+188], func(*stream, []byte) error { return nil })
	}
	vpid := uint16(0xFFFF)
	for _, s := range d.order {
		if s.video() {
			vpid = s.pid
		}
	}
	var out []byte
	var cc byte
	packetize := func(pes []byte) {
		for pos, first := 0, true; pos < len(pes); first = false {
			n := min(184, len(pes)-pos)
			pkt := []byte{0x47, byte(vpid>>8) & 0x1F, byte(vpid), 0x10 | cc&0x0F}
			if first {
				pkt[1] |= 0x40
			}
			if n < 184 { // pad with an adaptation field
				pkt[3] = 0x30 | cc&0x0F
				af := 184 - n - 1
				pkt = append(pkt, byte(af))
				if af > 0 {
					pkt = append(pkt, 0x00)
					pkt = append(pkt, bytes.Repeat([]byte{0xFF}, af-1)...)
				}
			}
			pkt = append(pkt, pes[pos:pos+n]...)
			out = append(out, pkt...)
			pos += n
			cc++
		}
	}
	var carry []byte // shift: the previous picture's tail, for the next PES
	emit := func(pes []byte, last bool) {
		if len(pes) < 9 {
			return
		}
		hdr := 9 + int(pes[8])
		head := append([]byte(nil), pes[:hdr]...)
		head[4], head[5] = 0, 0 // length 0: "until the next PES"
		data := pes[hdr:]
		if shift > 0 {
			data = append(append([]byte(nil), carry...), data...)
			carry = nil
			if !last && len(data) > shift {
				carry = append([]byte(nil), data[len(data)-shift:]...)
				data = data[:len(data)-shift]
			}
		}
		if chunk <= 0 {
			chunk = len(data) + 1
		}
		for i := 0; i < len(data); i += chunk {
			end := min(i+chunk, len(data))
			if i == 0 {
				packetize(append(head, data[:end]...))
			} else {
				// A follow-on PES: same stream, no timestamps.
				packetize(append([]byte{0, 0, 1, pes[3], 0, 0, 0x80, 0, 0}, data[i:end]...))
			}
		}
	}
	var cur []byte
	for i := 0; i+188 <= len(raw); i += 188 {
		pkt := raw[i : i+188]
		pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		if pid != vpid {
			out = append(out, pkt...)
			continue
		}
		payload := pkt[4:]
		if afc := (pkt[3] >> 4) & 3; afc&2 != 0 {
			payload = payload[int(payload[0])+1:]
		}
		if pkt[1]&0x40 != 0 {
			emit(cur, false)
			cur = append([]byte(nil), payload...)
		} else {
			cur = append(cur, payload...)
		}
	}
	emit(cur, true)
	path := filepath.Join(t.TempDir(), "split.ts")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A picture sent in several PES packets (only the first has a time) is one
// frame: its pieces are joined, not cut off or turned into frames of their
// own. (A real HLS site did this; the result played as a black picture.)
func TestPictureInSeveralPESPackets(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	src := filepath.Join("testdata", "h264_aac.ts")
	checkRewritten(t, src, splitVideoPES(t, src, 300, 0))
}

// PES packets that aren't lined up with pictures: each starts with the end
// of the picture before it, then the next picture's start code. The bytes
// before the start code belong to the previous picture. (A real HLS site
// does this: dropping them cut every picture's last rows off, and players
// showed a picture now and then on black.)
func TestPESNotLinedUpWithPictures(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	src := filepath.Join("testdata", "h264_aac.ts")
	checkRewritten(t, src, splitVideoPES(t, src, 0, 37))
	checkRewritten(t, src, splitVideoPES(t, src, 200, 37)) // both at once
}

// checkRewritten: the rewritten .ts converts to an MP4 that decodes cleanly
// to exactly the source's pictures.
func checkRewritten(t *testing.T, src, rewritten string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := ToMP4(rewritten, out); err != nil {
		t.Fatal(err)
	}
	decodesCleanly(t, out)
	frames := func(path string) string {
		b, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", "0:v", "-f", "framemd5", "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		var hs []string
		for _, l := range strings.Split(string(b), "\n") {
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			f := strings.Split(l, ",")
			hs = append(hs, strings.TrimSpace(f[len(f)-1]))
		}
		return strings.Join(hs, "\n")
	}
	if frames(out) != frames(src) {
		t.Error("the pictures differ from the original's")
	}
}
