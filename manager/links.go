package manager

import (
	"net/url"
	"path"
	"strings"
)

// Clipboard catching (done by the app): when you copy a link to a file
// worth downloading, dlmanager offers to download it. Only links that are
// clearly files count - a program, an archive, a video or music file, a
// PDF - judged by the end of the address, like IDM does. Copying a web
// page's address (to paste in a chat, say) never brings anything up.

// offerExts: the kinds of file a copied link is offered for.
var offerExts = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range categories {
		if c.folder == "Video" || c.folder == "Music" || c.folder == "Compressed" || c.folder == "Programs" {
			for _, ext := range strings.Fields(c.exts) {
				m[ext] = true
			}
		}
	}
	for _, ext := range strings.Fields("pdf ppt pptx bin torrent r00 r01 001") {
		m[ext] = true
	}
	delete(m, "bat") // scripts are often copied to look at, not to download
	delete(m, "cmd")
	delete(m, "ps1")
	delete(m, "sh")
	delete(m, "ts") // too often a TypeScript file on GitHub
	return m
}()

// LinkToOffer returns the download link in copied text, if the text is just
// one link to a file worth offering.
func LinkToOffer(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 4096 || strings.ContainsAny(text, " \t\r\n") {
		return "", false
	}
	u, err := url.Parse(text)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(u.Path), "."))
	if !offerExts[ext] {
		return "", false
	}
	return text, true
}
