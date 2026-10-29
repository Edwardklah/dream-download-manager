package update

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

const sample = `# Changelog

Some words at the top.

## 0.9.6
- ★ Big new thing.
- Small fix.

## 0.9.5
- Another small fix.

## 0.9.4
- ★ Readable Update button.

## v0.9.3
- Something.
`

func TestParseChangelog(t *testing.T) {
	r := ParseChangelog(strings.ReplaceAll(sample, "\n", "\r\n"))
	if len(r) != 4 || r[0].Version != "0.9.6" || r[3].Version != "0.9.3" {
		t.Fatalf("got %+v", r)
	}
	if !r[0].Lines[0].Highlight || r[0].Lines[0].Text != "Big new thing." || r[0].Lines[1].Highlight {
		t.Errorf("lines %+v", r[0].Lines)
	}
}

func TestSummarizeShort(t *testing.T) {
	s := Summarize(ParseChangelog(sample), "0.9.4", "0.9.6")
	if len(s.Recent) != 2 || s.Recent[0].Version != "0.9.6" || s.Recent[1].Version != "0.9.5" || len(s.Earlier) != 0 || s.More != 0 {
		t.Errorf("got %+v", s)
	}
}

// Far behind: the newest few in full, then only highlights, then a count.
func TestSummarizeLongGap(t *testing.T) {
	var b strings.Builder
	for v := 40; v >= 1; v-- {
		fmt.Fprintf(&b, "## 1.0.%d\n- change %d\n", v, v)
		if v%5 == 0 {
			fmt.Fprintf(&b, "- ★ highlight %d\n", v)
		}
	}
	s := Summarize(ParseChangelog(b.String()), "1.0.0", "1.0.40")
	if len(s.Recent) != summaryFull || s.Recent[0].Version != "1.0.40" {
		t.Fatalf("recent %+v", s.Recent)
	}
	// 1.0.37 down to 1.0.1: highlights at 35, 30, ... 5 = 7 of them.
	if len(s.Earlier) != 7 || s.Earlier[0].Text != "highlight 35" {
		t.Errorf("earlier %+v", s.Earlier)
	}
	if s.More != 37-7 {
		t.Errorf("more %d", s.More)
	}
}

// The project's own changelog reads, and every section has lines.
func TestProjectChangelog(t *testing.T) {
	data, err := os.ReadFile("../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	r := ParseChangelog(string(data))
	if len(r) == 0 {
		t.Fatal("no versions found")
	}
	for _, rel := range r {
		if len(rel.Lines) == 0 {
			t.Errorf("%s has no lines", rel.Version)
		}
	}
}

// Releasing without a changelog entry is easy to do; the tests catch it:
// the app's version (gui/build/windows/info.json) must have a section.
func TestChangelogHasThisVersion(t *testing.T) {
	data, err := os.ReadFile("../gui/build/windows/info.json")
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Fixed struct {
			FileVersion string `json:"file_version"`
		} `json:"fixed"`
		Info map[string]struct {
			ProductVersion string `json:"ProductVersion"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	v := info.Fixed.FileVersion
	if pv := info.Info["0409"].ProductVersion; pv != v {
		t.Errorf("info.json: file_version %s but ProductVersion %s - change both", v, pv)
	}
	log, _ := os.ReadFile("../CHANGELOG.md")
	for _, r := range ParseChangelog(string(log)) {
		if r.Version == v {
			return
		}
	}
	t.Errorf("CHANGELOG.md has no section for %s (the version in gui/build/windows/info.json)", v)
}
