// Package mkv joins a video-only and an audio-only WebM/Matroska file into
// one file - the WebM counterpart of package mux.
//
// YouTube (and others) send VP9 video and Opus sound as WebM. Like MP4,
// joining them needs no re-encoding: the compressed frames are copied as
// they are, only the structure around them is rebuilt.
//
// WebM is a restricted kind of Matroska (MKV); both are written in EBML, a
// "binary XML": a tree of elements, each written as
//
//	ID     1-4 bytes, says what the element is (e.g. 1F 43 B6 75 = Cluster)
//	size   1-8 bytes, how long the content is
//	content: a number, a string, raw bytes, or more elements
//
// IDs and sizes use a variable-length encoding ("vint"): the number of
// leading zero bits of the first byte, plus one, is the length in bytes.
//
//	1xxxxxxx                  1 byte, 7 bits of value
//	01xxxxxx xxxxxxxx         2 bytes, 14 bits
//	001xxxxx xxxxxxxx x...    3 bytes ... and so on, up to 8.
//
// A file looks like:
//
//	EBML header   "this is webm"
//	Segment
//	  SeekHead    where to find the parts below
//	  Info        timestamp scale, duration
//	  Tracks      one TrackEntry per track: number, codec, codec settings
//	  Cluster     a few seconds of frames: a timestamp, then blocks
//	  Cluster     ...
//	  Cues        an index: "at 0:30, the cluster at byte N" (for seeking)
package mkv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Element IDs used here (with their marker bits, as written in files).
const (
	idEBML            = 0x1A45DFA3
	idEBMLVersion     = 0x4286
	idEBMLReadVersion = 0x42F7
	idEBMLMaxIDLength = 0x42F2
	idEBMLMaxSizeLen  = 0x42F3
	idDocType         = 0x4282
	idDocTypeVersion  = 0x4287
	idDocTypeReadVer  = 0x4285
	idSegment         = 0x18538067
	idSeekHead        = 0x114D9B74
	idSeek            = 0x4DBB
	idSeekID          = 0x53AB
	idSeekPosition    = 0x53AC
	idInfo            = 0x1549A966
	idTimestampScale  = 0x2AD7B1
	idDuration        = 0x4489
	idMuxingApp       = 0x4D80
	idWritingApp      = 0x5741
	idTracks          = 0x1654AE6B
	idTrackEntry      = 0xAE
	idTrackNumber     = 0xD7
	idTrackUID        = 0x73C5
	idTrackType       = 0x83
	idCodecID         = 0x86
	idCues            = 0x1C53BB6B
	idCuePoint        = 0xBB
	idCueTime         = 0xB3
	idCueTrackPos     = 0xB7
	idCueTrack        = 0xF7
	idCueClusterPos   = 0xF1
	idCluster         = 0x1F43B675
	idTimestamp       = 0xE7
	idSimpleBlock     = 0xA3
	idBlockGroup      = 0xA0
	idBlock           = 0xA1
	idReferenceBlock  = 0xFB
	idPosition        = 0xA7
	idPrevSize        = 0xAB
	idVoid            = 0xEC
	idTags            = 0x1254C367
	idChapters        = 0x1043A770
	idAttachments     = 0x1941A469
)

// topLevel: elements that sit directly in the Segment. An element of
// unknown size (live streams write those) ends where one of these starts.
var topLevel = map[uint32]bool{
	idSeekHead: true, idInfo: true, idTracks: true, idCluster: true, idCues: true,
	idTags: true, idChapters: true, idAttachments: true,
}

// unknownSize marks an element whose size wasn't written.
const unknownSize = -1

var errCorrupt = errors.New("damaged WebM/Matroska file")

// header is an element's ID and content size, and where its content starts.
type header struct {
	id         uint32
	size       int64 // unknownSize if not given
	dataOffset int64
}

// readHeader reads the element header at off.
func readHeader(r io.ReaderAt, off int64) (header, error) {
	var buf [12]byte
	n, err := r.ReadAt(buf[:], off)
	if n == 0 {
		if err == nil {
			err = io.EOF
		}
		return header{}, err
	}
	b := buf[:n]
	idLen := vintLen(b[0])
	if idLen == 0 || idLen > 4 || idLen > len(b) {
		return header{}, errCorrupt
	}
	var id uint32
	for _, c := range b[:idLen] {
		id = id<<8 | uint32(c)
	}
	b = b[idLen:]
	if len(b) == 0 {
		return header{}, io.ErrUnexpectedEOF
	}
	sizeLen := vintLen(b[0])
	if sizeLen == 0 || sizeLen > len(b) {
		return header{}, errCorrupt
	}
	size := int64(b[0] & (0xFF >> sizeLen))
	allOnes := size == int64(0xFF>>sizeLen)
	for _, c := range b[1:sizeLen] {
		size = size<<8 | int64(c)
		allOnes = allOnes && c == 0xFF
	}
	if allOnes {
		size = unknownSize
	}
	return header{id: id, size: size, dataOffset: off + int64(idLen+sizeLen)}, nil
}

// vintLen is the length of a vint from its first byte (0 if invalid).
func vintLen(first byte) int {
	for i := 0; i < 8; i++ {
		if first&(0x80>>i) != 0 {
			return i + 1
		}
	}
	return 0
}

// readVint reads a vint value (marker removed) from b: value and length.
func readVint(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, errCorrupt
	}
	n := vintLen(b[0])
	if n == 0 || n > len(b) {
		return 0, 0, errCorrupt
	}
	v := uint64(b[0] & (0xFF >> n))
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n, nil
}

// ---- Writing ----

// appendID appends an element ID (written as is: its marker is part of it).
func appendID(b []byte, id uint32) []byte {
	switch {
	case id >= 1<<24:
		return append(b, byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
	case id >= 1<<16:
		return append(b, byte(id>>16), byte(id>>8), byte(id))
	case id >= 1<<8:
		return append(b, byte(id>>8), byte(id))
	}
	return append(b, byte(id))
}

// appendSize appends a size in the shortest vint.
func appendSize(b []byte, n uint64) []byte {
	for l := 1; l <= 8; l++ {
		if n < 1<<(7*l)-1 { // all-ones means "unknown", so one less
			return appendSizeLen(b, n, l)
		}
	}
	return appendSizeLen(b, n, 8)
}

// appendSizeLen appends a size as a vint of exactly l bytes.
func appendSizeLen(b []byte, n uint64, l int) []byte {
	v := n | 1<<(7*l) // the marker bit
	for i := l - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

// element appends a whole element with the given content.
func element(b []byte, id uint32, content []byte) []byte {
	b = appendID(b, id)
	b = appendSize(b, uint64(len(content)))
	return append(b, content...)
}

func uintElement(b []byte, id uint32, v uint64) []byte {
	var c []byte
	for v > 0 || len(c) == 0 {
		c = append([]byte{byte(v)}, c...)
		v >>= 8
	}
	return element(b, id, c)
}

// fixedUintElement writes v in exactly 8 bytes, so it can be filled in later.
func fixedUintElement(b []byte, id uint32, v uint64) []byte {
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], v)
	return element(b, id, c[:])
}

func floatElement(b []byte, id uint32, v float64) []byte {
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], math.Float64bits(v))
	return element(b, id, c[:])
}

func stringElement(b []byte, id uint32, s string) []byte { return element(b, id, []byte(s)) }

// ---- Reading values ----

func readUint(data []byte) uint64 {
	var v uint64
	for _, c := range data {
		v = v<<8 | uint64(c)
	}
	return v
}

func readFloat(data []byte) float64 {
	switch len(data) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(data)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(data))
	}
	return 0
}

// children walks the elements inside data (an element's content, read
// into memory), calling fn with each one's ID and content.
func children(data []byte, fn func(id uint32, content []byte) error) error {
	for off := 0; off < len(data); {
		h, err := readHeader(byteReader(data), int64(off))
		if err != nil {
			return err
		}
		start := int(h.dataOffset)
		if h.size == unknownSize || start+int(h.size) > len(data) {
			return fmt.Errorf("%w: element %X overruns its parent", errCorrupt, h.id)
		}
		if err := fn(h.id, data[start:start+int(h.size)]); err != nil {
			return err
		}
		off = start + int(h.size)
	}
	return nil
}

// byteReader reads a byte slice as an io.ReaderAt.
type byteReader []byte

func (b byteReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
