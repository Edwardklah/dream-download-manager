package mpegts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// errStop ends the first pass early, once every stream is set up.
var errStop = errors.New("stop")

// setupLimit: how far into the file the first pass looks for each stream's
// setup before giving up on that stream.
const setupLimit = 64 << 20

// findSetup is the first pass: read the PMT, then each stream's first
// frames for what the MP4's header needs.
func findSetup(path string) ([]*stream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := newDemuxer()
	pk := newPackets(io.LimitReader(f, setupLimit))
	onPES := func(s *stream, b []byte) error {
		if s.ready || s.unsupport {
			return nil
		}
		p, ok := parsePES(b)
		if !ok || (s.video() && p.pts < 0) { // a picture's follow-on piece: no start codes to read
			return nil
		}
		setUp(s, p.data)
		for _, s := range d.order {
			if !s.ready && !s.unsupport {
				return nil
			}
		}
		return errStop
	}
	for {
		pkt, err := pk.next()
		if err != nil {
			break
		}
		if err := d.feed(pkt, onPES); errors.Is(err, errStop) {
			break
		}
	}
	d.flush(onPES)

	var streams []*stream
	for _, s := range d.order {
		if s.unsupport {
			return nil, fmt.Errorf("%w: it holds a kind of picture or sound MP4 conversion doesn't handle (stream type 0x%02X)", ErrUnsupported, s.typ)
		}
		if s.ready {
			streams = append(streams, s)
		}
	}
	if len(streams) == 0 {
		return nil, fmt.Errorf("%w: no picture or sound found", ErrUnsupported)
	}
	return streams, nil
}

// setUp takes a stream's setup from one PES packet's data, if it's there.
func setUp(s *stream, data []byte) {
	switch s.typ {
	case typeH264:
		sps, pps := avc.GetParameterSetsFromByteStream(data)
		if len(sps) > 0 && len(pps) > 0 {
			s.sps, s.pps, s.ready = sps, pps, true
		}
	case typeHEVC:
		vps, sps, pps := hevc.GetParameterSetsFromByteStream(data)
		if len(vps) > 0 && len(sps) > 0 && len(pps) > 0 {
			s.vps, s.sps, s.pps, s.ready = vps, sps, pps, true
		}
	case typeAAC:
		for i := 0; i+7 <= len(data); i++ {
			if h, ok := adtsHeader(data[i:]); ok {
				if h.chans == 0 || h.freq == 0 {
					s.unsupport = true // channel layout given in-stream: rare, not handled
					return
				}
				s.aacObj, s.aacFreq, s.aacChans, s.ready = h.obj, h.freq, h.chans, true
				return
			}
		}
	}
}

// adts is an ADTS header: the 7 or 9 bytes before each AAC frame.
type adts struct {
	obj    byte // object type: profile + 1 (2 = AAC LC)
	freq   int
	chans  byte
	header int // 7, or 9 with a checksum
	length int // header + frame
}

var adtsFreqs = []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func adtsHeader(b []byte) (adts, bool) {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF6 != 0xF0 { // sync, MPEG-4/2, layer 0
		return adts{}, false
	}
	h := adts{obj: b[2]>>6 + 1, chans: (b[2]&1)<<2 | b[3]>>6, header: 7}
	if b[1]&1 == 0 {
		h.header = 9
	}
	if fi := int(b[2]>>2) & 0x0F; fi < len(adtsFreqs) {
		h.freq = adtsFreqs[fi]
	}
	h.length = int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5])>>5
	if h.length < h.header || h.freq == 0 {
		return adts{}, false
	}
	return h, true
}

// ---- The second pass: frames into MP4 fragments ----

const (
	ticks90k   = 90000
	maxJump    = 10 * ticks90k // a bigger jump in time is a discontinuity
	wrap33     = int64(1) << 33
	fragLength = 1.0 // seconds of one track per fragment
)

// track is one stream on its way into the MP4.
type track struct {
	s         *stream
	id        uint32
	timescale int64

	// Time: TS timestamps are 33-bit and wrap around every 26.5 hours, and
	// HLS streams can jump (a "discontinuity" between pieces). Each track
	// keeps its own running clock that never goes backwards.
	lastRaw int64
	wraps   int64
	offset  int64 // added to fix jumps
	started bool

	// Video only: the picture being put together. Usually one PES packet
	// is one picture, but some encoders send a picture in several, with the
	// timestamp only on the first; the rest are joined on until the next
	// timestamp arrives.
	frame *pes

	pending *mp4.FullSample // waits for the next sample to know its duration
	lastDur uint32
	batch   []mp4.FullSample
	batchT  uint64

	// Audio only: bytes not yet split into frames, and where PES packets'
	// timestamps start in them.
	abuf    []byte
	marks   []mark
	nextDTS int64 // in the track's timescale; -1 = not started
}

type mark struct {
	pos int
	pts int64
}

// unwrap turns a 33-bit timestamp into an always-increasing one.
func (t *track) unwrap(raw int64) int64 {
	if t.started {
		switch {
		case raw < t.lastRaw-wrap33/2:
			t.wraps++
		case raw > t.lastRaw+wrap33/2:
			t.wraps--
		}
	}
	t.started = true
	t.lastRaw = raw
	return raw + t.wraps*wrap33
}

type converter struct {
	w      io.Writer
	tracks map[uint16]*track
	seq    uint32
}

func convert(r io.Reader, streams []*stream, w io.Writer) error {
	init := mp4.CreateEmptyInit()
	c := &converter{w: w, tracks: map[uint16]*track{}}
	for _, s := range streams {
		t := &track{s: s, nextDTS: -1}
		var trak *mp4.TrakBox
		lang := s.language
		if lang == "" {
			lang = "und"
		}
		var err error
		switch s.typ {
		case typeH264:
			t.timescale = ticks90k
			trak = init.AddEmptyTrack(ticks90k, "video", lang)
			err = trak.SetAVCDescriptor("avc1", s.sps, s.pps, true)
		case typeHEVC:
			t.timescale = ticks90k
			trak = init.AddEmptyTrack(ticks90k, "video", lang)
			err = trak.SetHEVCDescriptor("hvc1", s.vps, s.sps, s.pps, nil, true)
		case typeAAC:
			t.timescale = int64(s.aacFreq)
			trak = init.AddEmptyTrack(uint32(s.aacFreq), "audio", lang)
			err = trak.SetAACDescriptorWithChannelConfig(s.aacObj, s.aacChans, s.aacFreq)
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		t.id = trak.Tkhd.TrackID
		c.tracks[s.pid] = t
	}
	if err := init.Encode(w); err != nil {
		return err
	}

	d := newDemuxer()
	pk := newPackets(r)
	for {
		pkt, err := pk.next()
		if err != nil {
			break
		}
		if err := d.feed(pkt, c.onPES); err != nil {
			return err
		}
	}
	if err := d.flush(c.onPES); err != nil {
		return err
	}
	for _, s := range streams {
		if err := c.finish(c.tracks[s.pid]); err != nil {
			return err
		}
	}
	return nil
}

func (c *converter) onPES(s *stream, b []byte) error {
	t := c.tracks[s.pid]
	if t == nil {
		return nil
	}
	p, ok := parsePES(b)
	if !ok {
		return nil
	}
	if s.video() {
		return c.videoFrame(t, p)
	}
	return c.audio(t, p)
}

// videoFrame collects a picture: a PES packet with a timestamp starts a new
// one (and the previous one is complete); one without continues the picture
// before it.
//
// PES packets needn't line up with pictures, either: some encoders start a
// packet with the end of the previous picture, then the new picture's start
// code. Those first bytes belong to the picture before (dropping them cut
// every picture's last rows off).
func (c *converter) videoFrame(t *track, p pes) error {
	if p.pts < 0 && t.frame != nil {
		t.frame.data = append(t.frame.data, p.data...)
		return nil
	}
	if t.frame != nil {
		i := startCode(p.data)
		if i < 0 { // no new picture starts here after all
			t.frame.data = append(t.frame.data, p.data...)
			return nil
		}
		t.frame.data = append(t.frame.data, p.data[:i]...)
		p.data = p.data[i:]
	}
	if err := c.flushFrame(t); err != nil {
		return err
	}
	p.data = append([]byte(nil), p.data...) // ours to grow
	t.frame = &p
	return nil
}

// startCode is where the first 00 00 01 (or 00 00 00 01) start code in b
// begins, or -1.
func startCode(b []byte) int {
	i := bytes.Index(b, []byte{0, 0, 1})
	if i > 0 && b[i-1] == 0 {
		i-- // the 4-byte form
	}
	return i
}

// flushFrame turns the picture collected so far into a sample.
func (c *converter) flushFrame(t *track) error {
	if t.frame == nil {
		return nil
	}
	p := *t.frame
	t.frame = nil
	return c.videoSample(t, p)
}

// videoSample: one whole picture (an "access unit"). Its NAL units go into
// the MP4 sample with 4-byte lengths instead of the TS's 00 00 01 start
// codes.
func (c *converter) videoSample(t *track, p pes) error {
	if p.dts < 0 {
		if t.pending == nil {
			return nil // no time to place it at
		}
		// Follow on from the last frame (continuous adds the offset back).
		p.dts = int64(t.pending.DecodeTime) + int64(t.lastDur) - t.offset
		p.pts = p.dts
	} else {
		cto := (p.pts - p.dts + wrap33) % wrap33 // PTS-DTS, even across a wrap
		p.dts = t.unwrap(p.dts)
		p.pts = p.dts + cto
	}
	dts := c.continuous(t, p.dts, int64(t.lastDur))
	shift := dts - p.dts

	nalus := avc.ExtractNalusFromByteStream(p.data)
	var data []byte
	sync := false
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		if t.s.typ == typeH264 {
			switch avc.GetNaluType(n[0]) {
			case avc.NALU_AUD:
				continue // the MP4 marks frame boundaries itself
			case avc.NALU_IDR:
				sync = true
			}
		} else {
			typ := hevc.GetNaluType(n[0])
			if typ == hevc.NALU_AUD {
				continue
			}
			if typ >= 16 && typ <= 23 { // IRAP: a frame you can start from
				sync = true
			}
		}
		l := len(n)
		data = append(data, byte(l>>24), byte(l>>16), byte(l>>8), byte(l))
		data = append(data, n...)
	}
	if len(data) == 0 {
		return nil
	}
	flags := mp4.NonSyncSampleFlags
	if sync {
		flags = mp4.SyncSampleFlags
	}
	s := mp4.FullSample{
		Sample:     mp4.NewSample(flags, 0, uint32(len(data)), int32(p.pts+shift-dts)),
		DecodeTime: uint64(dts),
		Data:       data,
	}
	return c.add(t, s)
}

// continuous keeps a track's clock moving forward: a timestamp that goes
// back, or jumps ahead by more than 10 s, is a discontinuity, and from there
// on the track is shifted to carry straight on.
func (c *converter) continuous(t *track, dts, lastDur int64) int64 {
	dts += t.offset
	if t.pending != nil {
		last := int64(t.pending.DecodeTime)
		if dts <= last || dts-last > maxJump*t.timescale/ticks90k {
			if lastDur <= 0 {
				lastDur = t.timescale / 25
			}
			t.offset += last + lastDur - dts
			dts = last + lastDur
		}
	}
	return dts
}

// audio: a PES packet holds one or more AAC frames in ADTS (a few bytes of
// header each), and a frame may continue in the next packet. Every AAC
// frame is 1024 samples long, so the clock simply counts on; the PES
// timestamps only set where it starts, and where the stream jumps.
func (c *converter) audio(t *track, p pes) error {
	if p.pts >= 0 {
		t.marks = append(t.marks, mark{pos: len(t.abuf), pts: t.unwrap(p.pts)})
	}
	t.abuf = append(t.abuf, p.data...)

	pos := 0
	for {
		b := t.abuf[pos:]
		if len(b) < 7 {
			break
		}
		h, ok := adtsHeader(b)
		if !ok {
			pos++ // not a frame start: look further
			continue
		}
		if len(b) < h.length {
			break // the rest comes in the next packet
		}
		frameDur := int64(1024)
		// A PES timestamp at or before this frame starts sets its time.
		var when int64 = -1
		for len(t.marks) > 0 && t.marks[0].pos <= pos {
			when = t.marks[0].pts * t.timescale / ticks90k
			t.marks = t.marks[1:]
		}
		dts := t.nextDTS
		switch {
		case dts < 0 && when < 0:
			pos += h.length // nothing to place it at yet
			continue
		case dts < 0:
			dts = when
		case when >= 0 && when-dts > frameDur/2 && when-dts <= maxJump*t.timescale/ticks90k:
			dts = when // a real gap in the sound: keep it
		}
		dts = c.continuous(t, dts, frameDur) // after offset: the gap check above uses raw values
		t.nextDTS = dts - t.offset + frameDur

		data := append([]byte(nil), b[h.header:h.length]...)
		s := mp4.FullSample{
			Sample:     mp4.NewSample(mp4.SyncSampleFlags, 0, uint32(len(data)), 0),
			DecodeTime: uint64(dts),
			Data:       data,
		}
		if err := c.add(t, s); err != nil {
			return err
		}
		pos += h.length
	}
	t.abuf = append(t.abuf[:0], t.abuf[pos:]...)
	for i := range t.marks {
		t.marks[i].pos -= pos
	}
	return nil
}

// add queues a sample; the one before it now knows its duration.
func (c *converter) add(t *track, s mp4.FullSample) error {
	if p := t.pending; p != nil {
		p.Dur = uint32(s.DecodeTime - p.DecodeTime)
		t.lastDur = p.Dur
		if err := c.batch(t, *p); err != nil {
			return err
		}
	}
	t.pending = &s
	return nil
}

// batch collects samples and writes a fragment for each second or so.
func (c *converter) batch(t *track, s mp4.FullSample) error {
	if len(t.batch) == 0 {
		t.batchT = s.DecodeTime
	}
	t.batch = append(t.batch, s)
	if float64(s.DecodeTime+uint64(s.Dur)-t.batchT) >= fragLength*float64(t.timescale) {
		return c.writeBatch(t)
	}
	return nil
}

func (c *converter) writeBatch(t *track) error {
	if len(t.batch) == 0 {
		return nil
	}
	c.seq++
	frag, err := mp4.CreateFragment(c.seq, t.id)
	if err != nil {
		return err
	}
	for _, s := range t.batch {
		frag.AddFullSample(s)
	}
	t.batch = t.batch[:0]
	return frag.Encode(c.w)
}

// finish writes a track's last samples. The very last one gets the same
// duration as the one before it.
func (c *converter) finish(t *track) error {
	if err := c.flushFrame(t); err != nil { // the last picture
		return err
	}
	if p := t.pending; p != nil {
		p.Dur = t.lastDur
		if p.Dur == 0 {
			p.Dur = uint32(t.timescale / 25)
			if !t.s.video() {
				p.Dur = 1024
			}
		}
		t.pending = nil
		if err := c.batch(t, *p); err != nil {
			return err
		}
	}
	return c.writeBatch(t)
}
