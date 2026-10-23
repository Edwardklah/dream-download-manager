// Package engine is the download engine. It knows nothing about windows,
// buttons or terminals - it only downloads files and reports progress.
// Every interface (the CLI now, the Wails desktop app later) sits on top of it.
//
// GO NOTE: every Go file starts with "package <name>". All files in the same
// folder share one package, so probe.go and download.go can use each other's
// functions and types without importing anything.
package engine

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// DefaultUserAgent makes our requests look like a normal browser.
// Many servers reject or throttle clients that don't.
//
// GO NOTE: names starting with a CAPITAL letter are "exported" (public):
// other packages can use them. Lowercase names are private to this package.
// That is Go's whole visibility system - no "public"/"private" keywords.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

// FileInfo is what we learn about a file before downloading it.
//
// GO NOTE: a struct is a group of named fields - like a Python dataclass.
// Size is int64 (a 64-bit integer) because files can be bigger than 2 GB.
type FileInfo struct {
	URL            string // final URL, after following redirects
	Size           int64  // total size in bytes; -1 if the server won't say
	SupportsRanges bool   // true if we can request parts of the file
	Filename       string // suggested name to save as
	ETag           string // fingerprint of this version of the file (for resume)
	LastModified   string // another way to detect the file changing on the server
}

// Probe asks the server about a file without downloading it.
//
// Trick: instead of a HEAD request (which many servers handle badly), we send
// a normal GET asking for only the first byte: "Range: bytes=0-0".
//   - Reply 206 Partial Content -> ranges supported, and Content-Range
//     tells us the full size, e.g. "bytes 0-0/52428800".
//   - Reply 200 OK -> the server ignored our Range header, so ranges are NOT
//     supported. Content-Length is then the size of the whole file.
//
// Either way we close the connection without reading the body.
//
// GO NOTE: Go functions can return several values. Returning (result, error)
// is the standard pattern: Go has no exceptions. The caller checks
// "if err != nil" right after the call. You'll see this everywhere.
//
// GO NOTE: ctx (context.Context) carries cancellation. If the user presses
// Cancel, the context is cancelled and any network call using it stops.
// By convention it is always the first parameter.
func Probe(ctx context.Context, client *http.Client, rawURL string, headers map[string]string) (*FileInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("bad URL: %w", err)
	}
	setHeaders(req, headers)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	// GO NOTE: "defer" schedules a call to run when this function returns,
	// however it returns (normal return or error). It's Go's version of
	// Python's "with" block for cleanup.
	defer resp.Body.Close()

	info := &FileInfo{
		URL:          resp.Request.URL.String(), // the URL after redirects
		Size:         -1,
		Filename:     pickFilename(resp),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}

	switch resp.StatusCode {
	case http.StatusPartialContent: // 206
		info.SupportsRanges = true
		info.Size = sizeFromContentRange(resp.Header.Get("Content-Range"))
	case http.StatusOK: // 200
		info.SupportsRanges = false
		info.Size = resp.ContentLength // Go sets this to -1 when unknown
	default:
		return nil, newHTTPError(resp)
	}
	return info, nil
}

// firstResponse is the reply to openFirst whose body hasn't been read yet:
// the first bytes of the file, ready for the first connection to carry on
// with. cancel aborts its request.
type firstResponse struct {
	resp   *http.Response
	cancel context.CancelFunc
}

func (f *firstResponse) close() {
	f.resp.Body.Close()
	f.cancel()
}

// openFirst starts the download right away, the way IDM does, instead of
// asking about the file first and downloading after. It asks for
// "Range: bytes=0-" - the whole file, as a range - and the reply tells us
// everything Probe would:
//   - 206 Partial Content: ranges work; Content-Range gives the size.
//   - 200 OK without "Accept-Ranges: bytes": no range support; Content-Length
//     is the size.
//
// The reply's body is the start of the file, so the first connection can
// keep reading it instead of making a new request. That saves a full round
// trip (often half a second on a slow, distant link).
//
// Some servers answer 200 to a range covering the whole file even though
// they support ranges. If the 200 says "Accept-Ranges: bytes", or the reply
// is anything unexpected, we fall back to Probe's careful one-byte question
// and return no response.
//
// maxBytes > 0 limits the request to the first maxBytes (see
// Download.MaxRequestSize).
func openFirst(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, maxBytes int64) (*FileInfo, *firstResponse, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("bad URL: %w", err)
	}
	setHeaders(req, headers)
	if maxBytes > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", maxBytes-1))
	} else {
		req.Header.Set("Range", "bytes=0-")
	}

	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	first := &firstResponse{resp: resp, cancel: cancel}
	info := &FileInfo{
		URL:          resp.Request.URL.String(),
		Size:         -1,
		Filename:     pickFilename(resp),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}

	switch {
	case resp.StatusCode == http.StatusPartialContent &&
		strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes 0-"):
		info.SupportsRanges = true
		info.Size = sizeFromContentRange(resp.Header.Get("Content-Range"))
		return info, first, nil
	case resp.StatusCode == http.StatusOK &&
		!strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes"):
		info.Size = resp.ContentLength
		return info, first, nil
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusPartialContent,
		resp.StatusCode == http.StatusRequestedRangeNotSatisfiable: // e.g. an empty file
		first.close()
		info, err := Probe(ctx, client, rawURL, headers)
		return info, nil, err
	default:
		err := newHTTPError(resp)
		first.close()
		return nil, nil, err
	}
}

// setHeaders applies our default User-Agent plus any extra headers
// (later: cookies and referer sent by the browser extension).
func setHeaders(req *http.Request, headers map[string]string) {
	req.Header.Set("User-Agent", DefaultUserAgent)
	// GO NOTE: "for key, value := range someMap" loops over a map
	// (Go's dictionary type), like Python's "for k, v in d.items()".
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}

var contentRangeTotal = regexp.MustCompile(`/(\d+)\s*$`)

// sizeFromContentRange reads TOTAL from "bytes START-END/TOTAL".
func sizeFromContentRange(value string) int64 {
	m := contentRangeTotal.FindStringSubmatch(value)
	if m == nil {
		return -1 // TOTAL can be "*" (unknown)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// pickFilename chooses a name: the server's suggestion if it gives one,
// otherwise the last part of the URL path.
func pickFilename(resp *http.Response) string {
	// Content-Disposition looks like:  attachment; filename="report.pdf"
	// The standard library's mime package parses it for us, including the
	// encoded form  filename*=UTF-8''my%20file.zip
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := params["filename"]; name != "" {
				return SanitizeFilename(name)
			}
		}
	}
	name := path.Base(resp.Request.URL.Path)
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	if name == "/" || name == "." {
		return "download"
	}
	return SanitizeFilename(name)
}

var badChars = regexp.MustCompile(`[<>:"|?*\x00-\x1f]`)

// SanitizeFilename removes characters Windows doesn't allow in file names,
// and any folder parts. A malicious server could suggest "..\..\Windows\x.exe";
// we only ever keep the final name.
func SanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	name = badChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, " .")
	if name == "" {
		return "download"
	}
	return name
}
