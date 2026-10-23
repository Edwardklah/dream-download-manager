// Package mpegts turns an MPEG-TS file (".ts", what most HLS streams are
// made of) into an ordinary MP4, without re-encoding: the compressed
// pictures and sound are copied byte for byte, only the packaging changes.
//
// Why bother: a .ts plays in VLC, but phones, TVs and many players handle
// MP4 better, and MP4 has an index, so skipping to 1:23:00 is instant
// instead of a search through the file.
//
// How MPEG-TS is organised:
//
//	188-byte packets, each starting with 0x47 and a PID (which stream it
//	belongs to). PID 0 carries the PAT ("the programme table is on PID x"),
//	that PID carries the PMT ("video is on PID 256, H.264; sound on 257,
//	AAC"). Each stream's packets, joined, form PES packets: a header with
//	the timestamps (PTS = when to show it, DTS = when to decode it, in
//	1/90000 s) and the data - one video frame, or a few audio frames.
//
// The conversion reads the file twice: once to find each stream's setup
// (the video's SPS/PPS, the sound's sample rate), which the MP4 needs at
// the start, then to copy the frames. It writes a fragmented MP4 (small
// independent pieces, easy to write in one pass) and has package mux tidy
// that into an ordinary MP4 - the same path as YouTube's DASH streams.
//
// Supported: H.264 and H.265 (HEVC) video, AAC sound - what HLS sites use.
// Anything else (MP3, AC-3, MPEG-2 video) returns ErrUnsupported, and the
// caller keeps the .ts.
package mpegts

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"dlmanager/mux"
)

// ErrUnsupported means the file holds something this package can't put in
// an MP4 (or isn't MPEG-TS at all).
var ErrUnsupported = errors.New("this MPEG-TS can't be converted to MP4")

// ToMP4 converts the .ts file in to an MP4 at out.
func ToMP4(in, out string) error {
	frag := out + ".fmp4"
	defer os.Remove(frag)
	if err := ToFragmented(in, frag); err != nil {
		return err
	}
	if err := mux.Merge(frag, "", out); err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return nil
}

// ToFragmented converts the .ts file in to a fragmented MP4 at out. (Two
// of these - picture and sound from separate HLS playlists - can be joined
// with mux.Merge.)
func ToFragmented(in, out string) error {
	streams, err := findSetup(in)
	if err != nil {
		return err
	}
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()

	tmp := out + ".tmp"
	o, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(o, 1<<20)
	err = convert(f, streams, w)
	if err == nil {
		err = w.Flush()
	}
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

// Stream types in the PMT (ISO/IEC 13818-1, table 2-34, and later additions).
const (
	typeH264 = 0x1B
	typeHEVC = 0x24
	typeAAC  = 0x0F // AAC in ADTS frames
)

// unsupportedType: picture or sound we can't convert. (Other types - ID3
// tags, ad markers, subtitles - are simply left out.)
func unsupportedType(t byte) bool {
	switch t {
	case 0x01, 0x02, 0x10, // MPEG-1/2 video, MPEG-4 part 2
		0x03, 0x04, // MP3 / MPEG audio
		0x11,       // AAC in LATM
		0x42,       // AVS
		0x81, 0x87: // AC-3, E-AC-3
		return true
	}
	return false
}

// stream is one elementary stream found in the PMT.
type stream struct {
	pid      uint16
	typ      byte
	language string // from the PMT, if given ("eng")

	// Set up from the first data (pass 1).
	ready     bool
	vps       [][]byte // HEVC only
	sps, pps  [][]byte
	aacObj    byte // AAC object type (2 = LC)
	aacFreq   int
	aacChans  byte
	unsupport bool // e.g. AC-3 announced as "private data"
}

func (s *stream) video() bool { return s.typ == typeH264 || s.typ == typeHEVC }

// packets reads 188-byte TS packets, finding the 0x47 sync again if the
// file has a damaged spot.
type packets struct {
	r   *bufio.Reader
	buf [188]byte
}

func newPackets(r io.Reader) *packets { return &packets{r: bufio.NewReaderSize(r, 1<<20)} }

// next returns the next packet (valid until the next call), or io.EOF.
func (p *packets) next() ([]byte, error) {
	for {
		if _, err := io.ReadFull(p.r, p.buf[:1]); err != nil {
			return nil, io.EOF
		}
		if p.buf[0] != 0x47 {
			continue // out of step: look for the next sync byte
		}
		if _, err := io.ReadFull(p.r, p.buf[1:]); err != nil {
			return nil, io.EOF // a cut-off last packet
		}
		// A real packet start is followed by another one (or the end).
		if peek, err := p.r.Peek(1); err == nil && peek[0] != 0x47 {
			continue
		}
		return p.buf[:], nil
	}
}

// demuxer splits a TS into PES packets, per stream.
type demuxer struct {
	pmtPID  int // -1 until the PAT is read
	streams map[uint16]*stream
	order   []*stream // PMT order
	psi     map[uint16][]byte
	pes     map[uint16][]byte // a PES being collected, per PID
	gotPMT  bool
}

func newDemuxer() *demuxer {
	return &demuxer{pmtPID: -1, streams: map[uint16]*stream{}, psi: map[uint16][]byte{}, pes: map[uint16][]byte{}}
}

// feed handles one packet; onPES is called with each complete PES packet.
func (d *demuxer) feed(pkt []byte, onPES func(*stream, []byte) error) error {
	pusi := pkt[1]&0x40 != 0
	pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	afc := (pkt[3] >> 4) & 3
	payload := pkt[4:]
	if afc&2 != 0 { // adaptation field first
		n := int(payload[0]) + 1
		if n > len(payload) {
			return nil
		}
		payload = payload[n:]
	}
	if afc&1 == 0 || len(payload) == 0 {
		return nil
	}

	switch {
	case pid == 0 || int(pid) == d.pmtPID:
		d.section(pid, pusi, payload)
	case d.streams[pid] != nil:
		s := d.streams[pid]
		if pusi {
			if prev := d.pes[pid]; len(prev) > 0 {
				if err := onPES(s, prev); err != nil {
					return err
				}
			}
			d.pes[pid] = append(d.pes[pid][:0:0], payload...)
		} else if d.pes[pid] != nil {
			d.pes[pid] = append(d.pes[pid], payload...)
		}
	}
	return nil
}

// flush hands over the PES packets still being collected at the end.
func (d *demuxer) flush(onPES func(*stream, []byte) error) error {
	for _, s := range d.order {
		if p := d.pes[s.pid]; len(p) > 0 {
			d.pes[s.pid] = nil
			if err := onPES(s, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// section collects a PSI table (PAT or PMT), which may span packets.
func (d *demuxer) section(pid uint16, pusi bool, payload []byte) {
	if pusi {
		ptr := int(payload[0])
		if 1+ptr > len(payload) {
			return
		}
		d.psi[pid] = append([]byte(nil), payload[1+ptr:]...)
	} else if d.psi[pid] != nil {
		d.psi[pid] = append(d.psi[pid], payload...)
	}
	sec := d.psi[pid]
	if len(sec) < 3 {
		return
	}
	length := int(sec[1]&0x0F)<<8 | int(sec[2])
	if len(sec) < 3+length || length < 9 {
		return
	}
	sec = sec[:3+length-4] // without the CRC
	d.psi[pid] = nil
	switch {
	case pid == 0 && sec[0] == 0x00:
		for i := 8; i+4 <= len(sec); i += 4 {
			program := int(sec[i])<<8 | int(sec[i+1])
			if program != 0 { // 0 is the network table
				d.pmtPID = int(sec[i+2]&0x1F)<<8 | int(sec[i+3])
				return
			}
		}
	case sec[0] == 0x02 && !d.gotPMT:
		d.readPMT(sec)
	}
}

func (d *demuxer) readPMT(sec []byte) {
	if len(sec) < 12 {
		return
	}
	d.gotPMT = true
	i := 12 + (int(sec[10]&0x0F)<<8 | int(sec[11]))
	for i+5 <= len(sec) {
		typ := sec[i]
		pid := uint16(sec[i+1]&0x1F)<<8 | uint16(sec[i+2])
		infoLen := int(sec[i+3]&0x0F)<<8 | int(sec[i+4])
		end := min(i+5+infoLen, len(sec))
		s := &stream{pid: pid, typ: typ}
		// Descriptors: the language, and whether "private data" is AC-3.
		for j := i + 5; j+2 <= end; {
			tag, n := sec[j], int(sec[j+1])
			if j+2+n > end {
				break
			}
			body := sec[j+2 : j+2+n]
			switch tag {
			case 0x0A: // ISO 639 language
				if n >= 3 {
					s.language = string(body[:3])
				}
			case 0x6A, 0x7A, 0x81: // AC-3, E-AC-3 (DVB and ATSC)
				s.unsupport = true
			}
			j += 2 + n
		}
		if typ == typeH264 || typ == typeHEVC || typ == typeAAC || unsupportedType(typ) || s.unsupport {
			if unsupportedType(typ) {
				s.unsupport = true
			}
			d.streams[pid] = s
			d.order = append(d.order, s)
		}
		i = end
	}
}

// pes is a parsed PES packet.
type pes struct {
	pts, dts int64 // 33-bit, in 1/90000 s; -1 if absent
	data     []byte
}

func parsePES(b []byte) (pes, bool) {
	p := pes{pts: -1, dts: -1}
	if len(b) < 9 || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return p, false
	}
	flags := b[7] >> 6
	hdr := 9 + int(b[8])
	if hdr > len(b) {
		return p, false
	}
	if flags&2 != 0 && len(b) >= 14 {
		p.pts = timestamp(b[9:14])
		p.dts = p.pts
	}
	if flags == 3 && len(b) >= 19 {
		p.dts = timestamp(b[14:19])
	}
	p.data = b[hdr:]
	// A length of 0 means "until the next PES" (usual for video); otherwise
	// drop any stuffing after it.
	if n := int(b[4])<<8 | int(b[5]); n > 0 && 6+n <= len(b) && 6+n >= hdr {
		p.data = b[hdr : 6+n]
	}
	return p, true
}

// timestamp reads a 33-bit PTS/DTS spread over 5 bytes with marker bits.
func timestamp(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}
