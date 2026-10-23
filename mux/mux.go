// Package mux joins a video-only and an audio-only MP4 file into one MP4.
//
// Why this is needed: in HD, YouTube (and many sites) send the picture and the
// sound as two separate files - "DASH" streams - so a player can switch
// quality without re-downloading the sound. To keep a video, both have to be
// put back together into one file. Most downloaders use ffmpeg for that (a
// ~100 MB program); for MP4 we can do it in Go with the mp4ff library, since
// joining needs no re-encoding: the compressed picture and sound are copied
// byte for byte, only the "index" around them is rebuilt.
//
// How an MP4 file is organised (simplified):
//
//	ftyp  "this is an MP4"
//	moov  the index: tracks, codecs, and for each sample (a frame of video,
//	      or a short block of sound) its size, duration and where it is
//	mdat  the samples themselves
//
// A DASH stream is "fragmented": a small moov with no samples, then many
// moof+mdat pairs, each indexing a few seconds of samples. mp4ff's
// Defragment turns that into the ordinary form above, which every player
// handles well (and can seek in quickly).
//
// The trick here: Defragment finds every sample through positions recorded in
// the file. So we present the two downloads as ONE file - the video file
// followed by the audio file, with the audio's positions shifted by the
// video's length and a header listing both tracks - and let it write a
// single, ordinary MP4. Nothing is copied twice and nothing big is held in
// memory.
package mux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/Eyevinn/mp4ff/mp4"
)

// ErrUnsupported means the files aren't in a form this package can join
// (for example, not MP4). The caller can fall back to another tool.
var ErrUnsupported = errors.New("these files can't be joined here")

// Merge joins videoPath (video only) and audioPath (audio only) into outPath.
// Either may be empty, to just tidy a single stream into an ordinary MP4
// (for "audio only" downloads).
func Merge(videoPath, audioPath, outPath string) error {
	var inputs []*input
	for _, p := range []string{videoPath, audioPath} {
		if p == "" {
			continue
		}
		in, err := open(p)
		if err != nil {
			closeAll(inputs)
			return err
		}
		inputs = append(inputs, in)
	}
	defer closeAll(inputs)
	if len(inputs) == 0 {
		return errors.New("nothing to join")
	}

	// One input (with any number of tracks) only needs tidying into an
	// ordinary MP4; two must each hold one track, to be combined.
	var combined *mp4.File
	var reader io.ReadSeeker
	if len(inputs) == 1 {
		combined, reader = inputs[0].mp4, inputs[0].file
	} else {
		for _, in := range inputs {
			if len(in.mp4.Moov.Traks) != 1 {
				return fmt.Errorf("%w: each stream to join should hold one track", ErrUnsupported)
			}
		}
		var err error
		if combined, reader, err = combine(inputs); err != nil {
			return err
		}
	}

	// Write to a temp file and rename at the end, so a crash or a full disk
	// never leaves a broken file under the real name.
	tmp := outPath + ".joining"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := mp4.Defragment(combined, reader, out); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, outPath)
}

// input is one opened stream.
type input struct {
	file   *os.File
	size   int64
	mp4    *mp4.File
	offset int64 // where this file starts in the combined view
}

func open(path string) (*input, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// Lazy: read only the boxes' descriptions, not the (huge) sample data.
	parsed, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %s isn't a readable MP4: %v", ErrUnsupported, path, err)
	}
	if parsed.Moov == nil || parsed.Moov.Mvhd == nil || len(parsed.Moov.Traks) == 0 {
		f.Close()
		return nil, fmt.Errorf("%w: %s has no tracks", ErrUnsupported, path)
	}
	if !parsed.IsFragmented() {
		f.Close()
		return nil, fmt.Errorf("%w: %s isn't a DASH (fragmented) stream", ErrUnsupported, path)
	}
	return &input{file: f, size: st.Size(), mp4: parsed}, nil
}

func closeAll(inputs []*input) {
	for _, in := range inputs {
		in.file.Close()
	}
}

// combine builds the one-file view of the inputs: the first input's file
// structure with the other inputs' tracks added to its moov, and all the
// fragments in time order (so picture and sound end up interleaved in the
// output, as players like them).
func combine(inputs []*input) (*mp4.File, io.ReadSeeker, error) {
	base := inputs[0].mp4
	moov := base.Moov
	movieTimescale := moov.Mvhd.Timescale

	type timedFragment struct {
		frag *mp4.Fragment
		time float64 // seconds
	}
	var frags []timedFragment
	var readers []readerPart
	var offset int64
	nextID := maxTrackID(moov) + 1

	for i, in := range inputs {
		in.offset = offset
		readers = append(readers, readerPart{r: in.file, offset: offset, size: in.size})
		trak := in.mp4.Moov.Traks[0]
		trackID := trak.Tkhd.TrackID

		if i > 0 {
			// Add this input's track to the combined moov, with a new ID.
			oldID := trackID
			trackID = nextID
			nextID++
			trak.Tkhd.TrackID = trackID
			rescaleMovieTimes(trak, in.mp4.Moov.Mvhd.Timescale, movieTimescale)
			moov.AddChild(trak)
			if moov.Mvex == nil {
				return nil, nil, fmt.Errorf("%w: first stream has no fragment defaults", ErrUnsupported)
			}
			if mvex := in.mp4.Moov.Mvex; mvex != nil {
				for _, trex := range mvex.Trexs {
					if trex.TrackID == oldID {
						trex.TrackID = trackID
						moov.Mvex.AddChild(trex)
					}
				}
			}
			moov.Mvhd.NextTrackID = nextID
		}

		timescale := float64(trak.Mdia.Mdhd.Timescale)
		if timescale == 0 {
			return nil, nil, fmt.Errorf("%w: track with zero timescale", ErrUnsupported)
		}
		for _, seg := range in.mp4.Segments {
			for _, frag := range seg.Fragments {
				if frag.Moof == nil {
					continue
				}
				var t float64
				for _, traf := range frag.Moof.Trafs {
					traf.Tfhd.TrackID = trackID
					if traf.Tfhd.HasBaseDataOffset() {
						traf.Tfhd.BaseDataOffset += uint64(offset)
					}
					if traf.Tfdt != nil {
						t = float64(traf.Tfdt.BaseMediaDecodeTime()) / timescale
					}
				}
				// Positions are relative to this file; shift them to where it
				// sits in the combined view.
				frag.Moof.StartPos += uint64(offset)
				frag.StartPos += uint64(offset)
				frags = append(frags, timedFragment{frag, t})
			}
		}
		offset += in.size
	}

	// Interleave by time. SliceStable keeps each stream's own order for
	// fragments with equal times.
	sort.SliceStable(frags, func(a, b int) bool { return frags[a].time < frags[b].time })
	seg := &mp4.MediaSegment{}
	for _, tf := range frags {
		seg.Fragments = append(seg.Fragments, tf.frag)
	}
	base.Segments = []*mp4.MediaSegment{seg}

	reader := io.NewSectionReader(&concatReaderAt{parts: readers}, 0, offset)
	return base, reader, nil
}

func maxTrackID(moov *mp4.MoovBox) uint32 {
	var id uint32
	for _, t := range moov.Traks {
		id = max(id, t.Tkhd.TrackID)
	}
	return id
}

// rescaleMovieTimes converts a track's durations that are counted in its own
// file's movie timescale (ticks per second) to the combined file's.
func rescaleMovieTimes(trak *mp4.TrakBox, from, to uint32) {
	if from == to || from == 0 {
		return
	}
	scale := func(v uint64) uint64 { return v * uint64(to) / uint64(from) }
	trak.Tkhd.Duration = scale(trak.Tkhd.Duration)
	if trak.Edts != nil {
		for _, elst := range trak.Edts.Elst {
			for i := range elst.Entries {
				elst.Entries[i].SegmentDuration = scale(elst.Entries[i].SegmentDuration)
			}
		}
	}
}

// concatReaderAt reads several files as if they were one, one after another.
type concatReaderAt struct{ parts []readerPart }

type readerPart struct {
	r      io.ReaderAt
	offset int64
	size   int64
}

func (c *concatReaderAt) ReadAt(p []byte, off int64) (int, error) {
	total := 0
	for _, part := range c.parts {
		if len(p) == 0 {
			break
		}
		if off >= part.offset+part.size || off < part.offset {
			continue
		}
		n, err := part.r.ReadAt(p[:min(int64(len(p)), part.offset+part.size-off)], off-part.offset)
		total += n
		p, off = p[n:], off+int64(n)
		if err != nil && err != io.EOF {
			return total, err
		}
	}
	if len(p) > 0 {
		return total, io.EOF
	}
	return total, nil
}
