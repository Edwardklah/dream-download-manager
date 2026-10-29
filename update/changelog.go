package update

import (
	"context"
	"sort"
	"strings"
)

// The changelog (CHANGELOG.md in the project): a section per version,
// newest first, with a few short lines each. A line starting with ★ is a
// highlight: what someone who skipped many updates should still hear about.
//
//	## 0.9.5
//	- ★ Video links you copy are looked up straight away.
//	- Fixed: subtitles came out in the wrong order.

// Release is one version's section.
type Release struct {
	Version string `json:"version"`
	Lines   []Line `json:"lines"`
}

// Line is one change.
type Line struct {
	Text      string `json:"text"`
	Highlight bool   `json:"highlight"`
}

// ParseChangelog reads a changelog's sections (lines that aren't under a
// "## version" heading, or aren't "- " items, are ignored).
func ParseChangelog(text string) []Release {
	var out []Release
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if v, ok := strings.CutPrefix(line, "## "); ok {
			v = strings.TrimPrefix(strings.Fields(v + " ")[0], "v")
			if parse(v) != nil {
				out = append(out, Release{Version: v})
			}
			continue
		}
		item, ok := strings.CutPrefix(line, "- ")
		if !ok || len(out) == 0 {
			continue
		}
		l := Line{Text: strings.TrimSpace(item)}
		if t, ok := strings.CutPrefix(l.Text, "★"); ok {
			l.Text, l.Highlight = strings.TrimSpace(t), true
		}
		if l.Text != "" {
			r := &out[len(out)-1]
			r.Lines = append(r.Lines, l)
		}
	}
	return out
}

// Summary is what changed between two versions, kept short for someone
// many versions behind.
type Summary struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Recent  []Release `json:"recent"`  // the newest few versions, in full
	Earlier []Line    `json:"earlier"` // the highlights of the versions before those
	More    int       `json:"more"`    // earlier versions not shown at all (no highlights left to show)
}

// How much a summary shows: the newest versions in full, then at most
// this many highlights from the ones before.
const (
	summaryFull       = 3
	summaryHighlights = 8
)

// Summarize: the changes after version from, up to and including to.
func Summarize(rels []Release, from, to string) Summary {
	var missed []Release
	for _, r := range rels {
		if Newer(r.Version, from) && !Newer(r.Version, to) {
			missed = append(missed, r)
		}
	}
	sort.SliceStable(missed, func(i, j int) bool { return Newer(missed[i].Version, missed[j].Version) })
	s := Summary{From: from, To: to}
	for i, r := range missed {
		if i < summaryFull {
			s.Recent = append(s.Recent, r)
			continue
		}
		shown := false
		for _, l := range r.Lines {
			if l.Highlight && len(s.Earlier) < summaryHighlights {
				s.Earlier = append(s.Earlier, l)
				shown = true
			}
		}
		if !shown {
			s.More++
		}
	}
	return s
}

// RawRepo is where a release's files are read from (raw.githubusercontent).
// A variable so tests can point it at a fake.
var RawRepo = "https://raw.githubusercontent.com/Edwardklah/dream-download-manager"

// FetchChangelog reads the changelog as it is in release version (a few
// KB), for "What's new" before updating.
func FetchChangelog(ctx context.Context, version string) ([]Release, error) {
	data, err := get(ctx, RawRepo+"/v"+version+"/CHANGELOG.md", 1<<20, nil)
	if err != nil {
		return nil, err
	}
	return ParseChangelog(string(data)), nil
}
