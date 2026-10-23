package mux

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// testdata/video.mp4 and audio.m4a are 3-second DASH streams (H.264 video,
// AAC audio) in the same shape YouTube sends: ftyp, moov, sidx, then
// moof+mdat fragments. Made with:
//
//	ffmpeg -f lavfi -i testsrc2=size=160x90:rate=25 -t 3 -c:v libx264 -g 25 -bf 2
//	       -pix_fmt yuv420p -b:v 60k -an -movflags dash+global_sidx -f mp4 video.mp4
//	ffmpeg -f lavfi -i sine=frequency=440:sample_rate=44100 -t 3 -c:a aac -b:a 48k
//	       -vn -frag_duration 1000000 -movflags dash+global_sidx -f mp4 audio.m4a

// samples reads every sample of every track of an ordinary (non-fragmented)
// MP4: per track, the sample count and a hash of all sample bytes in order.
func samples(t *testing.T, path string) map[string][2]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parsed, err := mp4.DecodeFile(f)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]any{}
	for _, trak := range parsed.Moov.Traks {
		stbl := trak.Mdia.Minf.Stbl
		n := stbl.Stsz.SampleNumber
		h := sha256.New()
		for nr := uint32(1); nr <= n; nr++ {
			chunkNr, firstInChunk, err := stbl.Stsc.ChunkNrFromSampleNr(int(nr))
			if err != nil {
				t.Fatal(err)
			}
			var off uint64
			if stbl.Stco != nil {
				off = uint64(stbl.Stco.ChunkOffset[chunkNr-1])
			} else {
				off = stbl.Co64.ChunkOffset[chunkNr-1]
			}
			for s := firstInChunk; s < int(nr); s++ {
				off += uint64(stbl.Stsz.GetSampleSize(s))
			}
			buf := make([]byte, stbl.Stsz.GetSampleSize(int(nr)))
			if _, err := f.ReadAt(buf, int64(off)); err != nil {
				t.Fatal(err)
			}
			h.Write(buf)
		}
		out[trak.Mdia.Hdlr.HandlerType] = [2]any{n, string(h.Sum(nil))}
	}
	return out
}

// fragmentSamples does the same for a fragmented input file.
func fragmentSamples(t *testing.T, path string) (uint32, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	trex := parsed.Moov.Mvex.Trex
	h := sha256.New()
	var n uint32
	for _, seg := range parsed.Segments {
		for _, frag := range seg.Fragments {
			full, err := frag.GetFullSamples(trex)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range full {
				h.Write(s.Data)
				n++
			}
		}
	}
	return n, string(h.Sum(nil))
}

func TestMergeVideoAndAudio(t *testing.T) {
	out := filepath.Join(t.TempDir(), "joined.mp4")
	if err := Merge("testdata/video.mp4", "testdata/audio.m4a", out); err != nil {
		t.Fatal(err)
	}

	got := samples(t, out)
	if len(got) != 2 {
		t.Fatalf("expected a video and an audio track, got %v", got)
	}
	// Every sample of both inputs is in the output, unchanged and in order.
	vn, vh := fragmentSamples(t, "testdata/video.mp4")
	an, ah := fragmentSamples(t, "testdata/audio.m4a")
	if got["vide"][0] != vn || got["vide"][1] != vh {
		t.Errorf("video samples differ: %d in, %v out", vn, got["vide"][0])
	}
	if got["soun"][0] != an || got["soun"][1] != ah {
		t.Errorf("audio samples differ: %d in, %v out", an, got["soun"][0])
	}

	// The output is an ordinary MP4 with the index first (quick to open and
	// seek), and no leftover temp file.
	f, _ := os.Open(out)
	parsed, err := mp4.DecodeFile(f)
	f.Close()
	if err != nil || parsed.IsFragmented() {
		t.Fatalf("output should be an ordinary MP4: fragmented=%v err=%v", parsed != nil && parsed.IsFragmented(), err)
	}
	if parsed.Children[1].Type() != "moov" {
		t.Errorf("moov should come right after ftyp, got %s", parsed.Children[1].Type())
	}
	if _, err := os.Stat(out + ".joining"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}

	// If ffprobe is installed, let it confirm the result: two streams of
	// about 3 seconds that decode without errors.
	if ffmpeg, err := exec.LookPath("ffmpeg"); err == nil {
		res, err := exec.Command(ffmpeg, "-v", "error", "-i", out, "-f", "null", "-").CombinedOutput()
		if err != nil || len(bytes.TrimSpace(res)) > 0 {
			t.Errorf("ffmpeg found problems decoding the result: %v %s", err, res)
		}
	}
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
		res, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_type,duration",
			"-of", "csv=p=0", out).Output()
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Fields(string(res))
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "video,2.9") && !strings.HasPrefix(lines[0], "video,3.0") {
			t.Errorf("ffprobe says %q", res)
		}
	}
}

func TestAudioOnly(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sound.m4a")
	if err := Merge("", "testdata/audio.m4a", out); err != nil {
		t.Fatal(err)
	}
	got := samples(t, out)
	an, ah := fragmentSamples(t, "testdata/audio.m4a")
	if len(got) != 1 || got["soun"][0] != an || got["soun"][1] != ah {
		t.Errorf("audio-only output differs: %v", got)
	}
}

func TestNotAnMP4(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.mp4")
	os.WriteFile(junk, []byte("<html>this is a web page, not a video</html>"), 0o644)
	err := Merge(junk, "testdata/audio.m4a", filepath.Join(dir, "out.mp4"))
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.mp4")); !os.IsNotExist(err) {
		t.Error("an output file was left behind")
	}
}

func TestConcatReaderAt(t *testing.T) {
	c := &concatReaderAt{parts: []readerPart{
		{r: strings.NewReader("hello "), offset: 0, size: 6},
		{r: strings.NewReader("world"), offset: 6, size: 5},
	}}
	buf := make([]byte, 7)
	if n, err := c.ReadAt(buf, 3); n != 7 || err != nil || string(buf) != "lo worl" {
		t.Errorf("read across the boundary: %q %d %v", buf[:n], n, err)
	}
	if n, err := c.ReadAt(buf, 8); n != 3 || err != io.EOF {
		t.Errorf("read past the end: %d %v", n, err)
	}
}

// One fragmented file with picture AND sound (what fMP4 HLS gives): tidied
// into an ordinary MP4, both tracks kept.
func TestTidyFragmentedWithTwoTracks(t *testing.T) {
	out := filepath.Join(t.TempDir(), "tidy.mp4")
	if err := Merge("testdata/both_fragmented.mp4", "", out); err != nil {
		t.Fatal(err)
	}
	got := samples(t, out)
	if len(got) != 2 || got["vide"][0].(uint32) == 0 || got["soun"][0].(uint32) == 0 {
		t.Errorf("got %v", got)
	}
	if ffmpeg, err := exec.LookPath("ffmpeg"); err == nil {
		if res, err := exec.Command(ffmpeg, "-v", "error", "-i", out, "-f", "null", "-").CombinedOutput(); err != nil || len(bytes.TrimSpace(res)) > 0 {
			t.Errorf("ffmpeg: %v %s", err, res)
		}
	}
	// Joining needs one track per input.
	if err := Merge("testdata/both_fragmented.mp4", "testdata/audio.m4a", out); !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}
