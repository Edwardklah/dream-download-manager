package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dlmanager"
	"dlmanager/update"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Keeping Dream DM itself up to date (package update does the fetching).
//
// About once a day the app asks GitHub for the newest release - one small
// request. If it's newer than this version, the window shows "Dream DM
// 0.9.2 is available" with Update now, What's new and Later. Update now
// downloads the setup file (~6 MB), checks it against its published
// checksum, runs it with no windows, and closes; the setup puts the new
// version where this one is and opens it again. Downloads in progress pick
// up where they left off.
//
// On Linux the notice only links to the release: updating there goes
// through the .deb or .rpm package.

// The version, from the same file the Windows setup and the Linux packages
// take theirs from (fixed.file_version): a release is "change the version
// there, tag, push".
//
//go:embed build/windows/info.json
var infoJSON []byte

var appVersion = func() string {
	var info struct {
		Fixed struct {
			FileVersion string `json:"file_version"`
		} `json:"fixed"`
	}
	json.Unmarshal(infoJSON, &info)
	if info.Fixed.FileVersion == "" {
		return "0.0.0"
	}
	return info.Fixed.FileVersion
}()

// AppVersion is this program's version, like "0.9.1".
func (s *DownloadService) AppVersion() string { return appVersion }

// AppUpdate is the window's view of a newer Dream DM.
type AppUpdate struct {
	Available  bool   `json:"available"`
	Current    string `json:"current"`
	Latest     string `json:"latest"`
	Notes      string `json:"notes"`      // the release's page ("What's new")
	CanInstall bool   `json:"canInstall"` // Windows, installed by the setup: Update now
	// What changed since this version (from the new release's changelog),
	// for "What's new"; nil if it couldn't be read (then: the release page).
	Changes *update.Summary `json:"changes"`
	Busy    bool            `json:"busy"` // downloading it
	Done    int64           `json:"done"`
	Total   int64           `json:"total"`
	Error   string          `json:"error"`
}

const appUpdateEvent = "app-update"

func init() { application.RegisterEvent[AppUpdate](appUpdateEvent) }

type appUpdateState struct {
	appMu     sync.Mutex
	appUpdate AppUpdate
	appBusy   atomic.Bool
	updated   *update.Summary // just updated: what's new, shown once
}

// What's remembered between runs: when GitHub was last asked, and a
// version you said "Later" to (not offered again until a newer one).
type appUpdateFile struct {
	Checked   time.Time `json:"checked"`
	Latest    string    `json:"latest"`
	Dismissed string    `json:"dismissed"`
	LastRun   string    `json:"lastRun"` // the version that ran last time
}

func appUpdatePath() string {
	dir, err := dataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "app-update.json")
}

func readAppUpdateFile() appUpdateFile {
	var f appUpdateFile
	if data, err := os.ReadFile(appUpdatePath()); err == nil {
		json.Unmarshal(data, &f)
	}
	return f
}

func writeAppUpdateFile(f appUpdateFile) {
	if data, err := json.Marshal(f); err == nil {
		os.WriteFile(appUpdatePath(), data, 0o644)
	}
}

// PendingAppUpdate: a newer Dream DM, if the last check found one.
func (s *DownloadService) PendingAppUpdate() AppUpdate {
	s.appMu.Lock()
	defer s.appMu.Unlock()
	return s.appUpdate
}

func (s *DownloadService) setAppUpdate(u AppUpdate) {
	s.appMu.Lock()
	s.appUpdate = u
	s.appMu.Unlock()
	if s.app != nil {
		s.app.Event.Emit(appUpdateEvent, u)
	}
}

// appUpdateLoop checks a little after the app starts, then every few
// hours while it runs; GitHub is asked at most once a day.
func (s *DownloadService) appUpdateLoop() {
	cleanOldUpdates()
	select {
	case <-s.ctx.Done():
		return
	case <-time.After(45 * time.Second): // not while the app is starting
	}
	for {
		s.checkAppUpdate(false)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(6 * time.Hour):
		}
	}
}

// CheckAppUpdateNow asks GitHub straight away (Settings' "Check now"), and
// returns what it found.
func (s *DownloadService) CheckAppUpdateNow() AppUpdate {
	s.checkAppUpdate(true)
	return s.PendingAppUpdate()
}

func (s *DownloadService) checkAppUpdate(now bool) {
	if s.appBusy.Load() || (!now && !s.manager.Settings().CheckAppUpdates) {
		return
	}
	f := readAppUpdateFile()
	latest := f.Latest
	if now || time.Since(f.Checked) > 24*time.Hour || latest == "" {
		ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
		v, err := update.Latest(ctx)
		cancel()
		if err != nil {
			if now {
				s.setAppUpdate(AppUpdate{Current: appVersion, Error: "Couldn't check for updates: " + err.Error()})
			}
			return
		}
		latest = v
		f.Checked, f.Latest = time.Now(), v
		writeAppUpdateFile(f)
	}
	if !update.Newer(latest, appVersion) || (!now && f.Dismissed == latest) {
		s.setAppUpdate(AppUpdate{Current: appVersion, Latest: latest})
		return
	}
	u := AppUpdate{
		Available: true, Current: appVersion, Latest: latest,
		Notes: update.ReleasePage(latest), CanInstall: canInstallUpdates(),
	}
	if prev := s.PendingAppUpdate(); prev.Latest == latest && prev.Changes != nil {
		u.Changes = prev.Changes // read already
	} else {
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		if rels, err := update.FetchChangelog(ctx, latest); err == nil {
			sum := update.Summarize(rels, appVersion, latest)
			u.Changes = &sum
		}
		cancel()
	}
	s.setAppUpdate(u)
}

// noteVersionRun remembers which version is running. If it's newer than
// last time, Dream DM was just updated: JustUpdated then has what's new
// (from this version's own changelog, so it works offline).
func (s *DownloadService) noteVersionRun() {
	f := readAppUpdateFile()
	if f.LastRun != "" && update.Newer(appVersion, f.LastRun) {
		sum := update.Summarize(update.ParseChangelog(dlmanager.Changelog), f.LastRun, appVersion)
		if len(sum.Recent) > 0 {
			s.appMu.Lock()
			s.updated = &sum
			s.appMu.Unlock()
		}
	}
	if f.LastRun != appVersion {
		f.LastRun = appVersion
		writeAppUpdateFile(f)
	}
}

// JustUpdated returns, once, what's new if Dream DM was just updated (nil
// otherwise): the window shows it as "Updated to 0.9.5".
func (s *DownloadService) JustUpdated() *update.Summary {
	s.appMu.Lock()
	defer s.appMu.Unlock()
	u := s.updated
	s.updated = nil
	return u
}

// DismissAppUpdate: "Later" - this version isn't offered again (a newer
// one will be; Settings' "Check now" still shows it).
func (s *DownloadService) DismissAppUpdate() {
	u := s.PendingAppUpdate()
	f := readAppUpdateFile()
	f.Dismissed = u.Latest
	writeAppUpdateFile(f)
	s.setAppUpdate(AppUpdate{Current: appVersion, Latest: u.Latest})
}

// OpenAppUpdatePage opens the release's page in the browser.
func (s *DownloadService) OpenAppUpdatePage() {
	if u := s.PendingAppUpdate(); u.Notes != "" && s.app != nil {
		s.app.Browser.OpenURL(u.Notes)
	}
}

// OpenReleasesPage opens the list of all releases in the browser.
func (s *DownloadService) OpenReleasesPage() {
	if s.app != nil {
		s.app.Browser.OpenURL(update.Repo + "/releases")
	}
}

// InstallAppUpdate downloads the new setup, checks it, runs it and closes
// the app. Progress and problems arrive as "app-update" events.
func (s *DownloadService) InstallAppUpdate() {
	u := s.PendingAppUpdate()
	if !u.Available || !u.CanInstall || !s.appBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.appBusy.Store(false)
		u.Busy, u.Error = true, ""
		s.setAppUpdate(u)
		last := time.Time{}
		path, err := update.Fetch(s.ctx, u.Latest, updatesDir(), func(done, total int64) {
			if time.Since(last) < 250*time.Millisecond && done != total {
				return
			}
			last = time.Now()
			u.Done, u.Total = done, total
			s.setAppUpdate(u)
		})
		if err == nil {
			err = runSetup(path)
		}
		if err != nil {
			u.Busy, u.Error = false, err.Error()
			s.setAppUpdate(u)
			return
		}
		// The setup waits for us to close, replaces the program and opens
		// the new one. (The download list is saved after every change.)
		s.app.Quit()
	}()
}

// updatesDir: where downloaded setup files wait to be run.
func updatesDir() string { return filepath.Join(os.TempDir(), "DreamDM-updates") }

// cleanOldUpdates removes setup files of earlier updates.
func cleanOldUpdates() {
	entries, _ := os.ReadDir(updatesDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "DreamDM-Setup-") {
			os.Remove(filepath.Join(updatesDir(), e.Name()))
		}
	}
}
