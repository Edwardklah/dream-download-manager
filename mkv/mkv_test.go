package mkv

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/video.webm (VP9) and audio.webm (Opus) are 4-second DASH
// streams like YouTube's, made with:
//
//	ffmpeg -f lavfi -i testsrc2=size=160x90:rate=25 -t 4 -c:v libvpx-vp9 -b:v 60k -g 25
//	       -deadline realtime -an -dash 1 -f webm video.webm
//	ffmpeg -f lavfi -i sine=frequency=440:sample_rate=48000 -t 4 -c:a libopus -b:a 24k
//	       -vn -dash 1 -f webm audio.webm

// frames reads every block of a file: per track number, how many, and a
// hash of their frame data (the block minus its track number) in order.
func frames(t *testing.T, path string) map[uint64][2]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := byteReader(data)
	h, _ := readHeader(r, 0)
	seg, err := readHeader(r, h.dataOffset+h.size)
	if err != nil || seg.id != idSegment {
		t.Fatalf("%s: no segment", path)
	}
	count := map[uint64]int{}
	hashes := map[uint64][]byte{}
	add := func(block []byte) {
		track, n, _ := readVint(block)
		count[track]++
		hashes[track] = append(hashes[track], block[n:]...)
	}
	for off := seg.dataOffset; off < int64(len(data)); {
		e, err := readHeader(r, off)
		if err != nil {
			break
		}
		if e.id == idCluster {
			children(data[e.dataOffset:e.dataOffset+e.size], func(id uint32, c []byte) error {
				switch id {
				case idSimpleBlock:
					add(c)
				case idBlockGroup:
					children(c, func(gid uint32, gc []byte) error {
						if gid == idBlock {
							add(gc)
						}
						return nil
					})
				}
				return nil
			})
		}
		off = e.dataOffset + e.size
	}
	out := map[uint64][2]any{}
	for track, n := range count {
		sum := sha256.Sum256(hashes[track])
		out[track] = [2]any{n, string(sum[:])}
	}
	return out
}

func TestMergeWebM(t *testing.T) {
	out := filepath.Join(t.TempDir(), "joined.webm")
	if err := Merge("testdata/video.webm", "testdata/audio.webm", out); err != nil {
		t.Fatal(err)
	}
	got := frames(t, out)
	v := frames(t, "testdata/video.webm")[1]
	a := frames(t, "testdata/audio.webm")[1]
	if got[1] != v || got[2] != a || len(got) != 2 {
		t.Errorf("frames differ: video %v/%v, audio %v/%v", got[1][0], v[0], got[2][0], a[0])
	}
	data, _ := os.ReadFile(out)
	if !bytes.Contains(data[:64], []byte("webm")) {
		t.Error("should be marked as WebM")
	}
	if _, err := os.Stat(out + ".joining"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}

	// With ffmpeg installed: two streams, decoding without errors, also
	// after seeking (which uses our Cues).
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return
	}
	for _, args := range [][]string{
		{"-v", "error", "-i", out, "-f", "null", "-"},
		{"-v", "error", "-ss", "2.5", "-i", out, "-f", "null", "-"},
	} {
		if res, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil || len(bytes.TrimSpace(res)) > 0 {
			t.Errorf("ffmpeg %v: %v %s", args, err, res)
		}
	}
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
		res, _ := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_name:format=duration", "-of", "csv=p=0", out).Output()
		if s := strings.Join(strings.Fields(string(res)), " "); !strings.HasPrefix(s, "vp9 opus 4.0") && !strings.HasPrefix(s, "vp9 opus 3.9") {
			t.Errorf("ffprobe says %q", s)
		}
	}
}

func TestMergeRefusals(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.webm")
	os.WriteFile(junk, []byte("not a webm file at all"), 0o644)
	if err := Merge(junk, "testdata/audio.webm", filepath.Join(dir, "x.webm")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("junk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.webm")); !os.IsNotExist(err) {
		t.Error("output left behind")
	}
}

func TestVints(t *testing.T) {
	for _, n := range []uint64{0, 1, 126, 127, 128, 16382, 16383, 1 << 20, 1<<56 - 2} {
		b := appendSize(nil, n)
		got, l, err := readVint(b)
		if err != nil || got != n || l != len(b) {
			t.Errorf("%d: got %d (len %d of %d) %v", n, got, l, len(b), err)
		}
	}
	// 127 in one byte would be 0xFF: "unknown size". It must take two.
	if b := appendSize(nil, 127); len(b) != 2 {
		t.Errorf("127 written as %x", b)
	}
}
