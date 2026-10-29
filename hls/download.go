package hls

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"dlmanager/engine"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Download fetches the HLS video at URL (a media or master playlist) into
// Out. The pieces are kept in a folder next to it (Out + ".pieces") until
// they're put together, so a paused or interrupted download carries on
// from the pieces it already has - even with a new playlist address (they
// expire on many sites), as long as the list of pieces is the same.
type Download struct {
	URL         string
	Headers     map[string]string
	Out         string
	Connections int // pieces downloaded at once (default 6)
	MaxRetries  int // attempts per piece in a row (default 10)
	OnProgress  func(Progress)
	Client      *http.Client

	keyMu sync.Mutex
	keys  map[string][]byte

	// received counts bytes as they arrive (pieces still downloading
	// included), for the speed. Counting only finished pieces made the speed
	// jump between 0 and bursts as pieces of a few MB completed together.
	received atomic.Int64
}

// speedWindow: the speed is the average over this long, so it reads steady.
const speedWindow = 3 * time.Second

// Progress of a download. Total is an estimate until all pieces are in
// (pieces differ in size), or -1 before the first one.
type Progress struct {
	Done, Total        int64
	Pieces, PiecesDone int
	Speed              float64 // bytes per second
}

// Result says what was made.
type Result struct {
	FragmentedMP4 bool // MP4 pieces (#EXT-X-MAP): Out is a fragmented MP4. Otherwise MPEG-TS.
}

// retry waits; variables so tests can shrink them.
var (
	retryBase    = time.Second
	stallTimeout = 30 * time.Second
)

// Run downloads and assembles the video.
func (d *Download) Run(ctx context.Context) (Result, error) {
	if d.Connections <= 0 {
		d.Connections = 6
	}
	if d.MaxRetries <= 0 {
		d.MaxRetries = 10
	}
	if d.Client == nil {
		d.Client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   16,
		}}
	}
	d.keys = map[string][]byte{}

	p, err := d.playlist(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{FragmentedMP4: p.Init != nil}

	dir := d.Out + ".pieces"
	if err := d.prepareDir(dir, p); err != nil {
		return res, err
	}

	// What's already there (from an earlier, interrupted run)?
	var done atomic.Int64
	var piecesDone atomic.Int32
	var doneMs atomic.Int64 // playing time of the finished pieces, for the size estimate
	var totalMs int64
	for _, s := range p.Segments {
		totalMs += pieceMs(s)
	}
	var todo []int
	for i := range p.Segments {
		if st, err := os.Stat(piecePath(dir, i)); err == nil {
			done.Add(st.Size())
			piecesDone.Add(1)
			doneMs.Add(pieceMs(p.Segments[i]))
		} else {
			todo = append(todo, i)
		}
	}

	// Report progress 4x a second.
	report := func(speed float64) {
		if d.OnProgress == nil {
			return
		}
		n, pd := done.Load(), int(piecesDone.Load())
		total := estimateTotal(n, pd, len(p.Segments), doneMs.Load(), totalMs)
		d.OnProgress(Progress{Done: n, Total: total, Pieces: len(p.Segments), PiecesDone: pd, Speed: speed})
	}
	stopReport := make(chan struct{})
	reportDone := make(chan struct{})
	go func() {
		defer close(reportDone)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		type sample struct {
			at    time.Time
			bytes int64
		}
		samples := []sample{{time.Now(), d.received.Load()}}
		for {
			select {
			case <-stopReport:
				return
			case now := <-tick.C:
				// Bytes arrived over the last few seconds.
				samples = append(samples, sample{now, d.received.Load()})
				for len(samples) > 2 && now.Sub(samples[0].at) > speedWindow {
					samples = samples[1:]
				}
				var speed float64
				first, last := samples[0], samples[len(samples)-1]
				if el := last.at.Sub(first.at).Seconds(); el > 0 {
					speed = float64(last.bytes-first.bytes) / el
				}
				report(speed)
			}
		}
	}()

	// Download the missing pieces, several at a time.
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstErr error
	var errOnce sync.Once
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(d.Connections, max(len(todo), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				n, err := d.piece(work, p.Segments[i], piecePath(dir, i))
				if err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("piece %d of %d: %w", i+1, len(p.Segments), err) })
					cancel()
					continue
				}
				done.Add(n)
				piecesDone.Add(1)
			}
		}()
	}
	for _, i := range todo {
		select {
		case jobs <- i:
		case <-work.Done():
		}
		if work.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	close(stopReport)
	<-reportDone
	report(0)

	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if firstErr != nil {
		return res, firstErr
	}

	// The MP4 header of fMP4 pieces, if any.
	initPath := filepath.Join(dir, "init")
	if p.Init != nil {
		if _, err := os.Stat(initPath); err != nil {
			if _, err := d.piece(ctx, *p.Init, initPath); err != nil {
				return res, fmt.Errorf("the video's header: %w", err)
			}
		}
	}
	if err := assemble(d.Out, initPath, p.Init != nil, dir, len(p.Segments)); err != nil {
		return res, err
	}
	os.RemoveAll(dir)
	return res, nil
}

// pieceMs is a piece's playing time in milliseconds (#EXTINF).
func pieceMs(s Segment) int64 { return int64(s.Duration * 1000) }

// estimateTotal guesses the finished size from the pieces done so far: bytes
// per second of video times the video's length. Pieces often differ in
// length (short ones at the start are common), so going by the average piece
// read low early on and kept growing. Without durations in the playlist it
// falls back to the average piece. -1 before the first piece.
func estimateTotal(done int64, piecesDone, pieces int, doneMs, totalMs int64) int64 {
	switch {
	case piecesDone <= 0:
		return -1
	case piecesDone >= pieces:
		return done
	case doneMs > 0 && totalMs > 0:
		return done * totalMs / doneMs
	default:
		return done * int64(pieces) / int64(piecesDone)
	}
}

// playlist fetches the playlist; for a master playlist, the best quality's.
func (d *Download) playlist(ctx context.Context) (*Playlist, error) {
	addr := d.URL
	for range 3 { // master -> media (and no endless chains)
		body, final, err := d.get(ctx, addr, 0, 0, 4<<20)
		if err != nil {
			return nil, fmt.Errorf("couldn't get the video's playlist: %w", err)
		}
		p, variants, err := Parse(final, string(body))
		if err != nil {
			return nil, err
		}
		if p != nil {
			if !p.Ended {
				return nil, ErrLive
			}
			return p, nil
		}
		best := variants[0]
		for _, v := range variants {
			if v.Bandwidth > best.Bandwidth {
				best = v
			}
		}
		addr = best.URL
	}
	return nil, errors.New("playlist chain too long")
}

// prepareDir makes the pieces folder, or checks that the pieces already in
// it belong to this list of pieces (compared without the address's query,
// which on many sites holds an expiring token).
func (d *Download) prepareDir(dir string, p *Playlist) error {
	h := sha256.New()
	for _, s := range p.Segments {
		u, _ := url.Parse(s.URL)
		fmt.Fprintf(h, "%s|%d|%d\n", u.Path, s.Offset, s.Length)
	}
	fingerprint := hex.EncodeToString(h.Sum(nil))
	listFile := filepath.Join(dir, "list")
	if old, err := os.ReadFile(listFile); err == nil && string(old) != fingerprint {
		os.RemoveAll(dir) // a different list of pieces: start over
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(listFile, []byte(fingerprint), 0o644)
}

func piecePath(dir string, i int) string { return filepath.Join(dir, fmt.Sprintf("%06d", i)) }

// piece downloads one piece (retrying), unlocks it, and saves it to path -
// through a temp file, so a piece on disk is always complete.
func (d *Download) piece(ctx context.Context, s Segment, path string) (int64, error) {
	var data []byte
	var err error
	for attempt := 1; ; attempt++ {
		data, _, err = d.get(ctx, s.URL, s.Offset, s.Length, 512<<20)
		if err == nil {
			break
		}
		if ctx.Err() != nil || !retryable(err) || attempt >= d.MaxRetries {
			return 0, err
		}
		select {
		case <-time.After(min(retryBase<<min(attempt-1, 5), 30*retryBase)):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if s.Key != nil {
		if data, err = d.decrypt(ctx, s, data); err != nil {
			return 0, err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return 0, err
	}
	return int64(len(data)), os.Rename(tmp, path)
}

// HTTPError is a refusal from the server.
type HTTPError struct{ StatusCode int }

func (e HTTPError) Error() string {
	return fmt.Sprintf("server replied %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

func retryable(err error) bool {
	var he HTTPError
	if errors.As(err, &he) {
		return he.StatusCode >= 500 || he.StatusCode == http.StatusTooManyRequests || he.StatusCode == http.StatusRequestTimeout
	}
	return !errors.Is(err, errTooBig)
}

var errTooBig = errors.New("piece is unreasonably large")

// get fetches addr (or bytes offset..offset+length-1 of it), with a stall
// watchdog: if no data arrives for stallTimeout, the attempt is abandoned.
func (d *Download) get(ctx context.Context, addr string, offset, length, limit int64) ([]byte, *url.URL, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stalled := atomic.Bool{}
	watchdog := time.AfterFunc(stallTimeout, func() { stalled.Store(true); cancel() })
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range d.Headers {
		req.Header.Set(k, v)
	}
	if length > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		if stalled.Load() {
			return nil, nil, errors.New("the server stopped sending")
		}
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, nil, HTTPError{resp.StatusCode}
	}
	var buf bytes.Buffer
	chunk := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			d.received.Add(int64(n))
			if int64(buf.Len()+n) > limit {
				return nil, nil, errTooBig
			}
			buf.Write(chunk[:n])
			engine.GiveWay(ctx, n) // a video look-up is running: let it have the line
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if stalled.Load() {
				return nil, nil, errors.New("the server stopped sending")
			}
			return nil, nil, rerr
		}
	}
	data := buf.Bytes()
	// A byte range answered with the whole file (200): cut out our part.
	if length > 0 && resp.StatusCode == http.StatusOK {
		if int64(len(data)) < offset+length {
			return nil, nil, io.ErrUnexpectedEOF
		}
		data = data[offset : offset+length]
	}
	if length > 0 && int64(len(data)) != length {
		return nil, nil, io.ErrUnexpectedEOF
	}
	if resp.ContentLength >= 0 && resp.StatusCode == http.StatusOK && length == 0 && int64(len(data)) != resp.ContentLength {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return data, resp.Request.URL, nil
}

// decrypt unlocks an AES-128 piece: AES in CBC mode, with PKCS#7 padding.
func (d *Download) decrypt(ctx context.Context, s Segment, data []byte) ([]byte, error) {
	key, err := d.key(ctx, s.Key.URL)
	if err != nil {
		return nil, err
	}
	iv := s.Key.IV
	if iv == nil { // by the spec: the piece's sequence number, as 16 bytes
		iv = make([]byte, 16)
		binary.BigEndian.PutUint64(iv[8:], s.Seq)
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("locked piece has a bad size")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(out) || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("couldn't unlock a piece (wrong key?)")
	}
	return out[:len(out)-pad], nil
}

// key fetches (once) the 16-byte key at addr.
func (d *Download) key(ctx context.Context, addr string) ([]byte, error) {
	d.keyMu.Lock()
	defer d.keyMu.Unlock()
	if k, ok := d.keys[addr]; ok {
		return k, nil
	}
	if strings.HasPrefix(addr, "skd://") {
		return nil, ErrDRM
	}
	var k []byte
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if k, _, err = d.get(ctx, addr, 0, 0, 1024); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("couldn't get the key: %w", err)
	}
	if len(k) != 16 {
		return nil, fmt.Errorf("the key has %d bytes, expected 16 (copy-protected?)", len(k))
	}
	d.keys[addr] = k
	return k, nil
}

// assemble writes the header (if any) and the pieces, in order, to out.
func assemble(out, initPath string, hasInit bool, dir string, n int) error {
	tmp := out + ".joining"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	copyFile := func(path string) error {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(f, in)
		return err
	}
	if hasInit {
		if err := copyFile(initPath); err != nil {
			f.Close()
			return err
		}
	}
	for i := range n {
		if err := copyFile(piecePath(dir, i)); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("putting the pieces together: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}
