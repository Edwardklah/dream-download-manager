package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dlmanager/engine"
	"dlmanager/hls"
	"dlmanager/video"
)

// KindVideo marks a download made from a video page (see Item.Kind).
const KindVideo = "video"

// VideoBackend is what video downloads need from outside the manager:
// looking a page up (yt-dlp) and putting the streams together (the MP4
// joiner). The app passes video.Backend; the tests pass a fake.
type VideoBackend interface {
	Resolve(ctx context.Context, pageURL, choiceID string, headers map[string]string) (*video.Plan, error)
	// YtDlpDownload lets yt-dlp download a format itself (the fallback).
	YtDlpDownload(ctx context.Context, pageURL, spec, out string, headers map[string]string, progress video.YtDlpProgress) error
	// Finish makes the finished file from the downloaded streams and
	// returns its path (the extension may differ from out's: an HLS video
	// in MPEG-TS stays a .ts file).
	Finish(streams []string, out string) (string, error)
	// SubtitleFormats looks a page up for the files a subtitle language
	// comes in (for "subtitles only").
	SubtitleFormats(ctx context.Context, pageURL, id string, headers map[string]string) ([]video.SubFormat, error)
	// SaveSubtitle saves a subtitle language next to the finished video.
	SaveSubtitle(ctx context.Context, pageURL, id string, formats []video.SubFormat, videoPath string,
		headers map[string]string) (string, error)
}

// SetVideoBackend enables video downloads.
func (m *Manager) SetVideoBackend(b VideoBackend) {
	m.mu.Lock()
	m.video = b
	m.mu.Unlock()
}

// VideoOptions describe a video to add (from the quality picker).
type VideoOptions struct {
	ChoiceID string // video.Choice.ID, e.g. "137+140"
	Quality  string // its label, e.g. "1080p"
	Title    string
	Ext      string // of the finished file: "mp4", "m4a"
	StartNow bool
	Headers  map[string]string // from the browser, if it came from there
	Subtitle string            // a subtitle language to save too (video.Subtitle.ID), or ""
}

// AddVideo queues a video download.
func (m *Manager) AddVideo(pageURL string, o VideoOptions) (Item, error) {
	u, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Item{}, errors.New("that doesn't look like a web address")
	}
	if o.ChoiceID == "" || o.Ext == "" {
		return Item{}, errors.New("no quality chosen")
	}
	pageURL = u.String()

	m.mu.Lock()
	if m.video == nil {
		m.mu.Unlock()
		return Item{}, errors.New("video downloads aren't available")
	}
	folder := m.settings.Folder
	if m.settings.SortIntoFolders {
		// The video goes straight into its category's folder, so its
		// streams and subtitles are there too.
		if cat := Category("x." + o.Ext); cat != "" {
			folder = filepath.Join(folder, cat)
		}
	}
	for _, e := range m.entries {
		if e.URL == pageURL && e.FormatID == o.ChoiceID && e.Status != Completed {
			m.mu.Unlock()
			return Item{}, ErrDuplicate
		}
	}
	title := o.Title
	if title == "" {
		title = "video"
	}
	m.nextID++
	e := &entry{Item: Item{
		ID: m.nextID, URL: pageURL, Folder: folder, Status: Queued,
		StartNow: o.StartNow, Total: -1, ETASeconds: -1, AddedAt: time.Now(),
		Kind: KindVideo, Title: title, Quality: o.Quality, FormatID: o.ChoiceID, Subtitle: o.Subtitle,
		// The finished file's name is fixed now, so the stream files (and a
		// resume after a restart) always match it.
		Filename: m.freeNameLocked(folder, engine.SanitizeFilename(title)+"."+o.Ext),
	}, headers: o.Headers}
	m.entries = append(m.entries, e)
	m.scheduleLocked()
	m.saveLocked()
	item := e.snapshot()
	m.mu.Unlock()
	m.notify()
	return item, nil
}

// freeNameLocked returns name, or "name (1).ext" etc. if that's taken by a
// file on disk or another download in the list.
func (m *Manager) freeNameLocked(folder, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	taken := func(n string) bool {
		if _, err := os.Stat(filepath.Join(folder, n)); err == nil {
			return true
		}
		for _, e := range m.entries {
			if e.Folder == folder && e.Filename == n {
				return true
			}
		}
		return false
	}
	candidate := name
	for i := 1; taken(candidate); i++ {
		candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	return candidate
}

// streamNames are the files a video's streams are downloaded to, next to
// the finished file: "Title.f137.stream", "Title.f140.stream". They're
// joined into the finished file at the end, then deleted.
func streamNames(it Item) []string {
	stem := strings.TrimSuffix(it.Filename, filepath.Ext(it.Filename))
	var names []string
	for _, id := range strings.Split(it.FormatID, "+") {
		names = append(names, fmt.Sprintf("%s.f%s.stream", stem, engine.SanitizeFilename(id)))
	}
	return names
}

// videoJob is the work for one video: look it up for fresh stream
// addresses, download each stream with the engine, join them.
// The caller (startLocked) holds m.mu, so the fields are read directly -
// locking again here would wait forever (Go's mutexes aren't re-entrant).
func (m *Manager) videoJob(e *entry) func(context.Context) (string, error) {
	backend := m.video
	it := e.Item
	headers := e.headers
	conns := m.settings.Connections

	return func(ctx context.Context) (string, error) {
		if backend == nil {
			return "", errors.New("video downloads aren't available")
		}
		if err := os.MkdirAll(it.Folder, 0o755); err != nil {
			return "", err
		}
		names := streamNames(it)
		var plan *video.Plan
		// A stream address can expire while we work (they last a few hours):
		// on "403 Forbidden", look the video up again once and carry on -
		// the engine resumes each stream by its ResumeID.
		for attempt := 0; ; attempt++ {
			m.setStage(e, "Looking up the video")
			var err error
			plan, err = backend.Resolve(ctx, it.URL, it.FormatID, headers)
			if err != nil {
				return "", err
			}
			if len(plan.Streams) != len(names) {
				return "", errors.New("the video's streams changed; remove it and add it again")
			}
			err = m.downloadStreams(ctx, e, backend, plan, names, headers, conns)
			if err != nil && attempt == 0 && expired(err) {
				continue
			}
			if err != nil {
				return "", err
			}
			break
		}

		if len(names) == 2 {
			m.setStage(e, "Joining picture and sound")
		} else {
			m.setStage(e, "Finishing the file") // e.g. converting HLS's MPEG-TS to MP4
		}
		paths := make([]string, len(names))
		for i, n := range names {
			paths[i] = filepath.Join(it.Folder, n)
		}
		out := filepath.Join(it.Folder, it.Filename)
		saved, err := backend.Finish(paths, out)
		if err != nil {
			return "", fmt.Errorf("couldn't finish the video: %w", err)
		}
		for _, p := range paths {
			os.Remove(p) // gone already if Finish renamed it
		}

		if it.Subtitle != "" {
			// The video is done whatever happens here: a problem with the
			// subtitles is noted, not a failure (and not worth downloading
			// the video again for).
			m.setStage(e, "Saving subtitles")
			var formats []video.SubFormat
			if plan.Info != nil {
				formats = video.SubtitleFormats(plan.Info, it.Subtitle)
			}
			if _, err := backend.SaveSubtitle(ctx, it.URL, it.Subtitle, formats, saved, headers); err != nil {
				m.setNote(e, "The subtitles couldn't be saved: "+err.Error())
			}
		}
		return saved, nil
	}
}

// setNote records a remark about a download that isn't an error.
func (m *Manager) setNote(e *entry, note string) {
	m.mu.Lock()
	e.Note = note
	m.mu.Unlock()
	m.notify()
}

// expired: the server refused a stream address it gave earlier.
func expired(err error) bool {
	code := 0
	var he *engine.HTTPError
	var hlsErr hls.HTTPError
	switch {
	case errors.As(err, &he):
		code = he.StatusCode
	case errors.As(err, &hlsErr):
		code = hlsErr.StatusCode
	}
	return code == http.StatusForbidden || code == http.StatusGone
}

// downloadStreams downloads the plan's streams one after the other, skipping
// any already finished in an earlier run.
func (m *Manager) downloadStreams(ctx context.Context, e *entry, backend VideoBackend, plan *video.Plan,
	names []string, headers map[string]string, conns int) error {
	var total int64
	for _, s := range plan.Streams {
		total += s.Size
	}
	var before int64 // bytes of the streams already done
	for i, s := range plan.Streams {
		name := names[i]
		stage := "Video"
		if len(plan.Streams) == 2 && i == 1 || plan.Ext == "m4a" {
			stage = "Audio"
		}
		if len(plan.Streams) == 2 {
			stage += fmt.Sprintf(" (%d of 2)", i+1)
		}
		path := filepath.Join(e.Folder, name)
		if st, err := os.Stat(path); err == nil {
			before += st.Size() // finished in an earlier run
			continue
		}
		m.setStage(e, stage)

		h := map[string]string{}
		for k, v := range headers { // browser details (cookies...), if any
			// Cookies only go to the site they belong to - not to the
			// separate servers (CDNs) that often deliver the video itself.
			if k == "Cookie" && !sameSite(e.URL, s.URL) {
				continue
			}
			h[k] = v
		}
		for k, v := range s.Headers { // what the site needs for this stream
			h[k] = v
		}
		others := total - s.Size
		base := before
		if s.YtDlp != "" {
			// A format only yt-dlp can download: let it.
			err := backend.YtDlpDownload(ctx, e.URL, s.YtDlp, path, headers, func(done, total int64, speed float64) {
				m.videoProgress(e, engine.Progress{
					Status: engine.StatusDownloading, Downloaded: done, Total: total, Speed: speed, ETA: -1,
				}, base, others)
			})
			if err != nil {
				return err
			}
			if st, err := os.Stat(path); err == nil {
				before += st.Size()
			}
			continue
		}
		if s.HLS {
			// A playlist of pieces: package hls downloads them, several at
			// a time. Its pieces folder makes it resumable on its own.
			// There are no connection strips to show (no single file split
			// into sections), so the line says how many pieces are in.
			lastDone := -1
			hd := &hls.Download{
				URL: s.URL, Headers: h, Out: path, Connections: max(conns, 2),
				OnProgress: func(p hls.Progress) {
					if p.Pieces > 0 && p.PiecesDone != lastDone {
						lastDone = p.PiecesDone
						if p.PiecesDone >= p.Pieces {
							m.setStage(e, stage+": joining the pieces")
						} else {
							m.setStage(e, fmt.Sprintf("%s: %d of %d pieces, %d at a time", stage, p.PiecesDone, p.Pieces, max(conns, 2)))
						}
					}
					m.videoProgress(e, engine.Progress{
						Status: engine.StatusDownloading, Downloaded: p.Done, Total: p.Total, Speed: p.Speed, ETA: -1,
					}, base, others)
				},
			}
			if _, err := hd.Run(ctx); err != nil {
				return err
			}
			if st, err := os.Stat(path); err == nil {
				before += st.Size()
			}
			continue
		}
		d := &engine.Download{
			URL:            s.URL,
			Folder:         e.Folder,
			Filename:       name,
			Headers:        h,
			Connections:    conns,
			Limits:         m.limits,
			MaxRequestSize: s.ChunkSize,
			// The address changes every time the video is looked up; this
			// keeps the .part resumable anyway.
			ResumeID:   e.URL + "#" + s.FormatID,
			OnProgress: func(p engine.Progress) { m.videoProgress(e, p, base, others) },
		}
		if _, err := d.Run(ctx); err != nil {
			return err
		}
		size := s.Size
		if st, err := os.Stat(path); err == nil {
			size = st.Size()
		}
		before += size
	}
	return nil
}

func (m *Manager) setStage(e *entry, stage string) {
	m.mu.Lock()
	e.Stage = stage
	if e.Status == Starting || e.Status == Queued {
		e.Status = Starting
	}
	m.mu.Unlock()
	m.notify()
}

// videoProgress reports one stream's progress as part of the whole video:
// base = bytes of the streams before this one, others = the size of the
// rest (from yt-dlp's figures).
func (m *Manager) videoProgress(e *entry, p engine.Progress, base, others int64) {
	p.Filename = "" // the stream file's name isn't the video's
	streamTotal := p.Total
	p.Downloaded += base
	if streamTotal > 0 {
		p.Total = base + streamTotal + max(others-base, 0)
	} else {
		p.Total = -1
	}
	if p.Speed > 0 && p.Total > 0 {
		p.ETA = time.Duration(float64(p.Total-p.Downloaded) / p.Speed * float64(time.Second))
	}
	m.progress(e, p)
}

// sameSite: does addr belong to page's site (the same host, or one of its
// subdomains)?
func sameSite(page, addr string) bool {
	p, err1 := url.Parse(page)
	a, err2 := url.Parse(addr)
	if err1 != nil || err2 != nil {
		return false
	}
	site := strings.TrimPrefix(p.Hostname(), "www.")
	host := a.Hostname()
	return host == site || strings.HasSuffix(host, "."+site)
}

// ---- Subtitles only ----

// KindSubtitles marks a download of just a video's subtitles: for a video
// you already have, or subtitles you want to fix yourself.
const KindSubtitles = "subtitles"

// AddSubtitles queues the subtitles of a video page, in language id
// (video.Subtitle.ID), saved as "<title>.<language>.srt".
func (m *Manager) AddSubtitles(pageURL, title, id string, headers map[string]string) (Item, error) {
	u, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Item{}, errors.New("that doesn't look like a web address")
	}
	if id == "" {
		return Item{}, errors.New("no subtitle language chosen")
	}
	pageURL = u.String()
	if title == "" {
		title = "video"
	}

	m.mu.Lock()
	if m.video == nil {
		m.mu.Unlock()
		return Item{}, errors.New("video downloads aren't available")
	}
	folder := m.settings.Folder
	if m.settings.SortIntoFolders {
		folder = filepath.Join(folder, Category("x.mp4")) // next to the videos
	}
	formatID := KindSubtitles + ":" + id
	for _, e := range m.entries {
		if e.URL == pageURL && e.FormatID == formatID && e.Status != Completed {
			m.mu.Unlock()
			return Item{}, ErrDuplicate
		}
	}
	m.nextID++
	e := &entry{Item: Item{
		ID: m.nextID, URL: pageURL, Folder: folder, Status: Queued, StartNow: true, // tiny: no need to queue
		Total: -1, ETASeconds: -1, AddedAt: time.Now(),
		Kind: KindSubtitles, Title: title, FormatID: formatID, Subtitle: id, Quality: "Subtitles",
		Filename: engine.SanitizeFilename(title) + "." + subtitleLang(id) + ".srt", // until saved: may be .ass
	}, headers: headers}
	m.entries = append(m.entries, e)
	m.scheduleLocked()
	m.saveLocked()
	item := e.snapshot()
	m.mu.Unlock()
	m.notify()
	return item, nil
}

// subtitleLang is the language part of a subtitle file's name ("en" for
// "auto:en-orig").
func subtitleLang(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(id, "auto:"), "-orig")
}

// subtitleJob looks the page up and saves the subtitles. The caller holds
// m.mu (see videoJob).
func (m *Manager) subtitleJob(e *entry) func(context.Context) (string, error) {
	backend := m.video
	it := e.Item
	headers := e.headers
	return func(ctx context.Context) (string, error) {
		if backend == nil {
			return "", errors.New("video downloads aren't available")
		}
		if err := os.MkdirAll(it.Folder, 0o755); err != nil {
			return "", err
		}
		m.setStage(e, "Looking up the subtitles")
		formats, err := backend.SubtitleFormats(ctx, it.URL, it.Subtitle, headers)
		if err != nil {
			return "", err
		}
		m.setStage(e, "Saving subtitles")
		// SaveSubtitle names the file after a video: give it the one the
		// video would have ("Title.video" -> "Title.en.srt").
		pretend := filepath.Join(it.Folder, engine.SanitizeFilename(it.Title)+".video")
		return backend.SaveSubtitle(ctx, it.URL, it.Subtitle, formats, pretend, headers)
	}
}
