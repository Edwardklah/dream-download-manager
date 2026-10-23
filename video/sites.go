package video

import (
	"net/url"
	"regexp"
	"strings"
)

// IsVideoPage reports whether a link looks like a page with a video on a
// well-known site, rather than a file. Pasting one of these opens the
// quality picker instead of downloading the web page itself.
//
// It's deliberately a short list of obvious patterns: for any other site
// yt-dlp supports, the browser extension's "Download the video on this
// page" asks yt-dlp directly.
func IsVideoPage(link string) bool {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	p := u.Path
	switch host {
	case "youtube.com", "music.youtube.com":
		return p == "/watch" && u.Query().Get("v") != "" ||
			hasAnyPrefix(p, "/shorts/", "/live/", "/embed/")
	case "youtu.be":
		return len(p) > 1
	case "vimeo.com":
		return vimeoID.MatchString(p)
	case "dailymotion.com":
		return strings.HasPrefix(p, "/video/")
	case "facebook.com":
		return hasAnyPrefix(p, "/watch", "/reel/") || strings.Contains(p, "/videos/")
	case "fb.watch":
		return len(p) > 1
	case "instagram.com":
		return hasAnyPrefix(p, "/reel/", "/reels/", "/p/", "/tv/")
	case "tiktok.com":
		return strings.Contains(p, "/video/")
	case "x.com", "twitter.com":
		return strings.Contains(p, "/status/")
	case "twitch.tv":
		return hasAnyPrefix(p, "/videos/") || strings.Contains(p, "/clip/")
	case "clips.twitch.tv", "streamable.com":
		return len(p) > 1
	case "reddit.com", "old.reddit.com":
		return strings.Contains(p, "/comments/")
	case "bilibili.com":
		return strings.HasPrefix(p, "/video/")
	case "soundcloud.com":
		return strings.Count(strings.Trim(p, "/"), "/") == 1 // /artist/track
	}
	return false
}

var vimeoID = regexp.MustCompile(`^/\d+`)

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// SamePage reports whether two addresses are the same page, ignoring
// "www." and a trailing slash (yt-dlp sometimes tidies addresses like that).
func SamePage(a, b string) bool {
	norm := func(s string) string {
		u, err := url.Parse(s)
		if err != nil {
			return s
		}
		return strings.TrimPrefix(u.Hostname(), "www.") + strings.TrimSuffix(u.Path, "/") + "?" + u.RawQuery
	}
	return norm(a) == norm(b)
}
