package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"dlmanager/bridge"
	"dlmanager/engine"
	"dlmanager/manager"
)

// Browser offers: a download caught by the browser extension doesn't start
// straight away (unless Settings say so). The window shows it as a card -
// its name, its size and where it will go - with Download, Later, Cancel
// and a folder button. Seeing "2.3 GB" before it starts matters on a data
// bundle, and it's the moment to pick a folder.

// BrowserOffer is one waiting download, as the window shows it.
type BrowserOffer struct {
	ID       int    `json:"id"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`     // bytes; -1 if the server won't say
	Checking bool   `json:"checking"` // the size is still being asked for
	Folder   string `json:"folder"`   // where it will be saved
	Chosen   bool   `json:"chosen"`   // Folder was picked by you
}

type offer struct {
	BrowserOffer
	opts manager.AddOptions
}

const offersEvent = "browser-offers"

func init() { application.RegisterEvent[[]BrowserOffer](offersEvent) }

var (
	offersMu  sync.Mutex
	offers    []*offer
	offerSeq  int
	offerSize = &http.Client{Timeout: 20 * time.Second} // for the size check
)

// offerDownload holds a download from the browser until you choose.
func (s *DownloadService) offerDownload(req bridge.Request) string {
	name := req.Filename
	if name == "" {
		name = nameFromURL(req.URL)
	}
	offersMu.Lock()
	offerSeq++
	o := &offer{
		BrowserOffer: BrowserOffer{ID: offerSeq, URL: req.URL, Filename: name, Size: -1, Checking: true},
		opts:         manager.AddOptions{Filename: req.Filename, Headers: req.Headers()},
	}
	o.Folder = s.defaultFolder(name)
	offers = append(offers, o)
	offersMu.Unlock()
	s.emitOffers()
	bringToFront()

	// Ask the server for the size (and its name for the file) meanwhile.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		info, err := engine.Probe(ctx, offerSize, req.URL, req.Headers())
		offersMu.Lock()
		o.Checking = false
		if err == nil {
			o.Size = info.Size
			if req.Filename == "" && info.Filename != "" {
				o.Filename = info.Filename
				if !o.Chosen {
					o.Folder = s.defaultFolder(o.Filename)
				}
			}
		}
		offersMu.Unlock()
		s.emitOffers()
	}()
	return name
}

// defaultFolder: where a file of this name goes - the download folder, or
// its Video, Music... folder inside when sorting is on.
func (s *DownloadService) defaultFolder(name string) string {
	st := s.manager.Settings()
	if cat := manager.Category(name); st.SortIntoFolders && cat != "" {
		return filepath.Join(st.Folder, cat)
	}
	return st.Folder
}

func nameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "download"
	}
	if n := path.Base(u.Path); n != "" && n != "/" && n != "." {
		if un, err := url.PathUnescape(n); err == nil {
			return un
		}
		return n
	}
	return u.Hostname()
}

// BrowserOffers lists the waiting downloads (the window asks when it opens).
func (s *DownloadService) BrowserOffers() []BrowserOffer {
	offersMu.Lock()
	defer offersMu.Unlock()
	list := make([]BrowserOffer, 0, len(offers))
	for _, o := range offers {
		list = append(list, o.BrowserOffer)
	}
	return list
}

func (s *DownloadService) emitOffers() {
	if s.app != nil {
		s.app.Event.Emit(offersEvent, s.BrowserOffers())
	}
}

// take removes an offer from the waiting list and returns it.
func takeOffer(id int) *offer {
	offersMu.Lock()
	defer offersMu.Unlock()
	for i, o := range offers {
		if o.ID == id {
			offers = append(offers[:i], offers[i+1:]...)
			return o
		}
	}
	return nil
}

// AcceptOffer adds a waiting download: how is "download" (in line with the
// others, as usual), "later" (paused) or "always" (download, and stop asking).
func (s *DownloadService) AcceptOffer(id int, how string) error {
	o := takeOffer(id)
	if o == nil {
		return errors.New("that download isn't waiting any more")
	}
	opts := o.opts
	if o.Chosen {
		opts.Folder = o.Folder
	}
	opts.Paused = how == "later"
	_, err := s.manager.AddWith(o.URL, opts)
	if errors.Is(err, manager.ErrDuplicate) {
		err = errors.New("it's already in your list")
	}
	if how == "always" {
		st := s.manager.Settings()
		st.AskBrowser = false
		s.SetSettings(st)
	}
	s.emitOffers()
	return err
}

// DeclineOffer forgets a waiting download.
func (s *DownloadService) DeclineOffer(id int) {
	takeOffer(id)
	s.emitOffers()
}

// ChooseOfferFolder lets you pick where a waiting download goes. That folder
// is kept as it is (not sorted into Video, Music... inside it).
func (s *DownloadService) ChooseOfferFolder(id int) error {
	offersMu.Lock()
	var current string
	for _, o := range offers {
		if o.ID == id {
			current = o.Folder
		}
	}
	offersMu.Unlock()
	dir, err := s.app.Dialog.OpenFile().
		SetTitle("Choose where to save it").
		CanChooseDirectories(true).
		CanChooseFiles(false).
		CanCreateDirectories(true).
		SetDirectory(current).
		PromptForSingleSelection()
	if err != nil || dir == "" {
		return err
	}
	offersMu.Lock()
	for _, o := range offers {
		if o.ID == id {
			o.Folder, o.Chosen = dir, true
		}
	}
	offersMu.Unlock()
	s.emitOffers()
	return nil
}
