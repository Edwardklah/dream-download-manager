package mkv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// ErrUnsupported means the files can't be joined here.
var ErrUnsupported = errors.New("these files can't be joined here")

// input is one opened stream: its single track and where its clusters are.
type input struct {
	f        *os.File
	scale    uint64  // nanoseconds per timestamp tick
	duration float64 // in ticks
	track    []byte  // the TrackEntry's content
	codec    string
	clusters []cluster
}

type cluster struct {
	offset, size int64 // the whole element (header included)
	time         uint64
}

// Merge joins videoPath and audioPath (each one track, WebM or Matroska)
// into out. The result is WebM if every codec is allowed in WebM (VP8, VP9,
// AV1, Opus, Vorbis), Matroska otherwise.
func Merge(videoPath, audioPath, out string) error {
	var inputs []*input
	for _, p := range []string{videoPath, audioPath} {
		in, err := open(p)
		if err != nil {
			for _, i := range inputs {
				i.f.Close()
			}
			return err
		}
		inputs = append(inputs, in)
	}
	defer func() {
		for _, in := range inputs {
			in.f.Close()
		}
	}()
	if inputs[0].scale != inputs[1].scale {
		return fmt.Errorf("%w: the streams use different time scales", ErrUnsupported)
	}

	tmp := out + ".joining"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := write(f, inputs); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

// open reads a stream's structure (not its frames).
func open(path string) (*input, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	in := &input{f: f, scale: 1_000_000}
	fail := func(err error) (*input, error) {
		f.Close()
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	end := st.Size()

	h, err := readHeader(f, 0)
	if err != nil || h.id != idEBML {
		return fail(fmt.Errorf("%w: %s isn't WebM/Matroska", ErrUnsupported, path))
	}
	seg, err := readHeader(f, h.dataOffset+h.size)
	if err != nil || seg.id != idSegment {
		return fail(fmt.Errorf("%w: %s has no Segment", ErrUnsupported, path))
	}
	segEnd := end
	if seg.size != unknownSize {
		segEnd = min(end, seg.dataOffset+seg.size)
	}

	var tracks int
	for off := seg.dataOffset; off < segEnd; {
		e, err := readHeader(f, off)
		if err != nil {
			break // a cut-off end: keep what we have
		}
		size := e.size
		if size == unknownSize {
			if size, err = scanUnknown(f, e.dataOffset, segEnd); err != nil {
				return fail(err)
			}
		}
		next := e.dataOffset + size
		switch e.id {
		case idInfo:
			data, err := readAt(f, e.dataOffset, size)
			if err != nil {
				return fail(err)
			}
			children(data, func(id uint32, c []byte) error {
				switch id {
				case idTimestampScale:
					in.scale = readUint(c)
				case idDuration:
					in.duration = readFloat(c)
				}
				return nil
			})
		case idTracks:
			data, err := readAt(f, e.dataOffset, size)
			if err != nil {
				return fail(err)
			}
			children(data, func(id uint32, c []byte) error {
				if id == idTrackEntry {
					tracks++
					in.track = append([]byte{}, c...)
					children(c, func(id uint32, v []byte) error {
						if id == idCodecID {
							in.codec = string(v)
						}
						return nil
					})
				}
				return nil
			})
		case idCluster:
			c := cluster{offset: off, size: next - off}
			// The cluster's timestamp is (almost always) its first child.
			if t, err := readHeader(f, e.dataOffset); err == nil && t.id == idTimestamp && t.size <= 8 {
				if v, err := readAt(f, t.dataOffset, t.size); err == nil {
					c.time = readUint(v)
				}
			}
			in.clusters = append(in.clusters, c)
		}
		off = next
	}
	if tracks != 1 {
		return fail(fmt.Errorf("%w: %s should hold exactly one track (it has %d)", ErrUnsupported, path, tracks))
	}
	if len(in.clusters) == 0 {
		return fail(fmt.Errorf("%w: %s holds no frames", ErrUnsupported, path))
	}
	return in, nil
}

// scanUnknown finds where an element of unknown size ends: at the next
// top-level element, or the end.
func scanUnknown(r io.ReaderAt, off, end int64) (int64, error) {
	start := off
	for off < end {
		h, err := readHeader(r, off)
		if err != nil {
			break
		}
		if topLevel[h.id] {
			break
		}
		if h.size == unknownSize {
			return 0, fmt.Errorf("%w: nested elements of unknown size", ErrUnsupported)
		}
		off = h.dataOffset + h.size
	}
	return min(off, end) - start, nil
}

func readAt(r io.ReaderAt, off, size int64) ([]byte, error) {
	if size < 0 || size > 256<<20 {
		return nil, errCorrupt
	}
	b := make([]byte, size)
	if _, err := r.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

// webmCodecs are the codecs a WebM file may contain.
var webmCodecs = []string{"V_VP8", "V_VP9", "V_AV1", "A_OPUS", "A_VORBIS"}

func isWebM(codec string) bool {
	for _, c := range webmCodecs {
		if codec == c {
			return true
		}
	}
	return false
}

// counter keeps track of how many bytes have been written.
type counter struct {
	w *bufio.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// write puts the joined file together:
//
//	EBML header, Segment { SeekHead, Info, Tracks, Clusters..., Cues }
//
// The Segment's size and the SeekHead's positions are only known at the
// end, so they're written as fixed-size placeholders and filled in then.
func write(f *os.File, inputs []*input) error {
	cw := &counter{w: bufio.NewWriterSize(f, 1<<20)}

	docType := "webm"
	for _, in := range inputs {
		if !isWebM(in.codec) {
			docType = "matroska"
		}
	}
	var hdr []byte
	hdr = uintElement(hdr, idEBMLVersion, 1)
	hdr = uintElement(hdr, idEBMLReadVersion, 1)
	hdr = uintElement(hdr, idEBMLMaxIDLength, 4)
	hdr = uintElement(hdr, idEBMLMaxSizeLen, 8)
	hdr = stringElement(hdr, idDocType, docType)
	hdr = uintElement(hdr, idDocTypeVersion, 4)
	hdr = uintElement(hdr, idDocTypeReadVer, 2)
	cw.Write(element(nil, idEBML, hdr))

	// Segment, size to be filled in (8-byte placeholder).
	seg := appendID(nil, idSegment)
	cw.Write(seg)
	segSizeAt := cw.n
	cw.Write(appendSizeLen(nil, 0, 8))
	segStart := cw.n

	// SeekHead with fixed-size positions, filled in at the end.
	seekIDs := []uint32{idInfo, idTracks, idCues}
	var seeks []byte
	for _, id := range seekIDs {
		var s []byte
		s = element(s, idSeekID, appendID(nil, id))
		s = fixedUintElement(s, idSeekPosition, 0)
		seeks = element(seeks, idSeek, s)
	}
	seekHeadAt := cw.n
	seekHead := element(nil, idSeekHead, seeks)
	cw.Write(seekHead)
	positions := map[uint32]int64{}

	// Info.
	var info []byte
	info = uintElement(info, idTimestampScale, inputs[0].scale)
	info = floatElement(info, idDuration, max(inputs[0].duration, inputs[1].duration))
	info = stringElement(info, idMuxingApp, "dlmanager")
	info = stringElement(info, idWritingApp, "dlmanager")
	positions[idInfo] = cw.n - segStart
	cw.Write(element(nil, idInfo, info))

	// Tracks: each input's entry, renumbered 1 and 2.
	var tracks []byte
	for i, in := range inputs {
		entry, err := renumberTrack(in.track, uint64(i+1))
		if err != nil {
			return err
		}
		tracks = element(tracks, idTrackEntry, entry)
	}
	positions[idTracks] = cw.n - segStart
	cw.Write(element(nil, idTracks, tracks))

	// Clusters from both, in time order, so picture and sound interleave.
	type item struct {
		in    int
		c     cluster
		order int
	}
	var all []item
	for i, in := range inputs {
		for j, c := range in.clusters {
			all = append(all, item{i, c, j})
		}
	}
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].c.time != all[b].c.time {
			return all[a].c.time < all[b].c.time
		}
		return all[a].in < all[b].in
	})
	type cue struct{ time, pos uint64 }
	var cues []cue
	for _, it := range all {
		data, err := readAt(inputs[it.in].f, it.c.offset, it.c.size)
		if err != nil {
			return err
		}
		out, keyframe, err := rewriteCluster(data, uint64(it.in+1))
		if err != nil {
			return err
		}
		// Index the video's clusters that start with a key frame: a player
		// seeking there can start decoding straight away.
		if it.in == 0 && keyframe {
			cues = append(cues, cue{it.c.time, uint64(cw.n - segStart)})
		}
		if _, err := cw.Write(out); err != nil {
			return err
		}
	}

	// Cues.
	var cuePoints []byte
	for _, c := range cues {
		var tp []byte
		tp = uintElement(tp, idCueTrack, 1)
		tp = uintElement(tp, idCueClusterPos, c.pos)
		var cp []byte
		cp = uintElement(cp, idCueTime, c.time)
		cp = element(cp, idCueTrackPos, tp)
		cuePoints = element(cuePoints, idCuePoint, cp)
	}
	positions[idCues] = cw.n - segStart
	cw.Write(element(nil, idCues, cuePoints))

	if err := cw.w.Flush(); err != nil {
		return err
	}

	// Fill in the placeholders.
	if _, err := f.WriteAt(appendSizeLen(nil, uint64(cw.n-segStart), 8), segSizeAt); err != nil {
		return err
	}
	var filled []byte
	for _, id := range seekIDs {
		var s []byte
		s = element(s, idSeekID, appendID(nil, id))
		s = fixedUintElement(s, idSeekPosition, uint64(positions[id]))
		filled = element(filled, idSeek, s)
	}
	_, err := f.WriteAt(element(nil, idSeekHead, filled), seekHeadAt)
	return err
}

// renumberTrack returns a TrackEntry's content with a new track number and
// UID (the rest - codec, its settings, language - unchanged).
func renumberTrack(entry []byte, number uint64) ([]byte, error) {
	var out []byte
	err := children(entry, func(id uint32, c []byte) error {
		switch id {
		case idTrackNumber:
			out = uintElement(out, idTrackNumber, number)
		case idTrackUID:
			out = uintElement(out, idTrackUID, number)
		default:
			out = element(out, id, c)
		}
		return nil
	})
	return out, err
}

// rewriteCluster copies a cluster with its blocks' track number changed to
// number. Each block starts with its track number as a vint, followed by a
// 2-byte relative timestamp and a flags byte (bit 0x80 = key frame, for
// SimpleBlocks). Position and PrevSize are dropped: positions changed.
func rewriteCluster(data []byte, number uint64) ([]byte, bool, error) {
	h, err := readHeader(byteReader(data), 0)
	if err != nil || h.id != idCluster {
		return nil, false, errCorrupt
	}
	content := data[h.dataOffset:]
	var out []byte
	keyframe := false
	first := true
	renumber := func(block []byte) ([]byte, error) {
		_, n, err := readVint(block)
		if err != nil || len(block) < n+3 {
			return nil, errCorrupt
		}
		nb := appendSize(nil, number) // a track number is written like a size
		return append(nb, block[n:]...), nil
	}
	err = children(content, func(id uint32, c []byte) error {
		switch id {
		case idSimpleBlock:
			b, err := renumber(c)
			if err != nil {
				return err
			}
			if first {
				keyframe = c[vintLenOf(c)+2]&0x80 != 0 // the flags byte, after the track number and timestamp
				first = false
			}
			out = element(out, idSimpleBlock, b)
		case idBlockGroup:
			var g []byte
			hasRef := false
			err := children(c, func(gid uint32, gc []byte) error {
				if gid == idReferenceBlock {
					hasRef = true
				}
				if gid == idBlock {
					b, err := renumber(gc)
					if err != nil {
						return err
					}
					g = element(g, idBlock, b)
					return nil
				}
				g = element(g, gid, gc)
				return nil
			})
			if err != nil {
				return err
			}
			if first {
				keyframe = !hasRef // a block that refers to no other frame is a key frame
				first = false
			}
			out = element(out, idBlockGroup, g)
		case idPosition, idPrevSize, idVoid:
		default:
			out = element(out, id, c)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return element(nil, idCluster, out), keyframe, nil
}

func vintLenOf(b []byte) int {
	_, n, _ := readVint(b)
	return n
}
