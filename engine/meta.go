package engine

import (
	"encoding/json"
	"os"
)

// meta is saved next to the .part file as "<name>.part.meta" while a download
// is in progress. It records WHICH file the .part belongs to, and (for
// multi-connection downloads) which pieces are done, so that next time we
// can safely continue instead of starting over.
//
// Why not just look at the .part file? Because bytes alone can't tell us
// whether the file on the server is still the same one. If the server now has
// a newer version, appending its second half to the old version's first half
// gives you a corrupted file that looks complete. The ETag and Last-Modified
// values let us check that it hasn't changed.
//
// GO NOTE: the `json:"url"` parts are "struct tags". They tell the
// encoding/json package what each field is called in the JSON file.
// "omitempty" leaves the field out when it's empty.
type meta struct {
	URL          string `json:"url"`       // the URL the user gave us
	FinalURL     string `json:"final_url"` // after redirects
	Size         int64  `json:"size"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	ResumeID     string `json:"resume_id,omitempty"` // see Download.ResumeID

	// Segments is set for multi-connection downloads: each piece of the
	// file and how many of its bytes are saved. Empty for single-connection
	// downloads, where the .part file's size says how much is done.
	Segments []segmentState `json:"segments,omitempty"`
}

// segmentState is one piece of the file: bytes Start up to (not including) End,
// of which the first Done bytes are saved.
type segmentState struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

func metaPath(partPath string) string { return partPath + ".meta" }

func readMeta(partPath string) (*meta, error) {
	data, err := os.ReadFile(metaPath(partPath))
	if err != nil {
		return nil, err
	}
	var m meta
	// GO NOTE: Unmarshal fills in the struct we point it at (&m).
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeMeta saves the meta file. It writes to a temporary file first and then
// renames it over the old one. A rename is all-or-nothing, so if the program
// or PC crashes mid-save, you're left with the old meta or the new one -
// never a half-written file that can't be read.
func writeMeta(partPath string, m *meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := metaPath(partPath) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, metaPath(partPath))
}

// sameFile reports whether the file on the server (info) is still the file
// the .part was downloaded from (m).
func (m *meta) sameFile(info *FileInfo) bool {
	if m.Size != info.Size || m.Size < 0 {
		return false // different size, or size unknown: can't trust it
	}
	if m.ETag != "" && info.ETag != "" && m.ETag != info.ETag {
		return false
	}
	if m.LastModified != "" && info.LastModified != "" && m.LastModified != info.LastModified {
		return false
	}
	return true
}

// validSegments checks that saved segments make sense for a file of this
// size: they must cover the whole file, in order, with no gaps or overlaps,
// and Done must fit inside each piece. A damaged meta file is rejected, and
// the download starts over rather than risk a file with holes in it.
func validSegments(segs []segmentState, size int64) bool {
	var prevEnd int64
	for _, s := range segs {
		if s.Start != prevEnd || s.End < s.Start || s.Done < 0 || s.Done > s.End-s.Start {
			return false
		}
		prevEnd = s.End
	}
	return len(segs) > 0 && prevEnd == size
}
