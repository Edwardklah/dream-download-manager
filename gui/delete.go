package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dlmanager/manager"
)

// DeleteFile deletes a finished download's file - to the Recycle Bin (the
// Trash on Linux), so it can be got back - with any subtitles saved next to
// it. removeFromList also takes the download off the list; otherwise it
// stays there, marked "File deleted".
func (s *DownloadService) DeleteFile(id int, removeFromList bool) error {
	it, ok := s.manager.Get(id)
	if !ok {
		return errors.New("download not found")
	}
	if it.Status != manager.Completed || it.SavedPath == "" {
		return errors.New("only a finished download's file can be deleted")
	}
	for _, p := range filesOf(it) {
		if _, err := os.Stat(p); err != nil {
			continue // gone already (moved or deleted by hand)
		}
		if err := moveToTrash(p); err != nil {
			return fmt.Errorf("couldn't delete %s: %w", filepath.Base(p), err)
		}
	}
	if removeFromList {
		return s.manager.Remove(id, false)
	}
	return s.manager.MarkFileDeleted(id)
}

// filesOf: a finished download's file, and the subtitle file saved with a
// video ("Title.en.srt" next to "Title.mp4").
func filesOf(it manager.Item) []string {
	files := []string{it.SavedPath}
	if it.Subtitle != "" && it.Kind != manager.KindSubtitles {
		lang := strings.TrimSuffix(strings.TrimPrefix(it.Subtitle, "auto:"), "-orig")
		stem := strings.TrimSuffix(it.SavedPath, filepath.Ext(it.SavedPath)) + "." + lang
		for _, ext := range []string{".srt", ".ass", ".vtt"} {
			files = append(files, stem+ext)
		}
	}
	return files
}

// DownloadAgain downloads a finished download again, when its file was
// deleted or isn't where it was saved any more.
func (s *DownloadService) DownloadAgain(id int) error { return s.manager.DownloadAgain(id) }

// watchFiles notices finished downloads whose file was moved or deleted
// outside Dream DM (and ones that came back), every few seconds.
func (s *DownloadService) watchFiles() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		s.manager.CheckFiles()
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
	}
}
