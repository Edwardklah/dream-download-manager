package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Categories: with Settings.SortIntoFolders on, finished files go into a
// folder by their kind, inside the download folder - like IDM:
//
//	Downloads\Video\lecture.mp4
//	Downloads\Compressed\photos.zip
//	Downloads\Programs\setup.exe
//
// Files of other kinds stay in the download folder itself.

var categories = []struct {
	folder string
	exts   string
}{
	{"Video", "mp4 mkv webm avi mov wmv flv m4v ts mts m2ts 3gp mpg mpeg ogv"},
	{"Music", "mp3 m4a aac flac wav ogg oga opus wma alac aiff ape"},
	{"Compressed", "zip rar 7z tar gz tgz bz2 tbz2 xz txz zst lz lzma cab"},
	{"Programs", "exe msi msix msixbundle appx appxbundle apk xapk deb rpm appimage dmg pkg iso img bat cmd ps1 sh jar"},
	{"Documents", "pdf doc docx xls xlsx ppt pptx odt ods odp rtf txt csv md epub mobi djvu"},
	{"Pictures", "jpg jpeg png gif webp bmp svg heic heif tif tiff avif ico psd"},
}

var categoryOf = func() map[string]string {
	m := map[string]string{}
	for _, c := range categories {
		for _, ext := range strings.Fields(c.exts) {
			m[ext] = c.folder
		}
	}
	return m
}()

// Category returns the folder a file of this name belongs in ("Video"),
// or "" if it has no category.
func Category(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return categoryOf[ext]
}

// sortIntoCategory moves a finished file into its category's folder next
// to it, and returns its new path (or the old one, if it has no category
// or can't be moved - the file is fine where it is).
func sortIntoCategory(path string) string {
	cat := Category(path)
	dir := filepath.Dir(path)
	if cat == "" || strings.EqualFold(filepath.Base(dir), cat) {
		return path
	}
	target := filepath.Join(dir, cat)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return path
	}
	dest := freePath(filepath.Join(target, filepath.Base(path)))
	if err := os.Rename(path, dest); err != nil {
		return path
	}
	return dest
}

// freePath returns path, or "name (1).ext" etc. if that's taken.
func freePath(path string) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	candidate := path
	for i := 1; ; i++ {
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
		candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
}
