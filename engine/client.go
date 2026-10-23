package engine

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// newClient builds the HTTP client used for downloads.
//
// We deliberately set NO overall timeout (http.Client.Timeout): a large file
// can legitimately take hours. Instead we time out the steps that should be
// quick - connecting, the TLS handshake, and waiting for the server's first
// reply. A connection that goes silent mid-download is caught by the stall
// watchdog in download.go.
//
// HTTP/2 is switched OFF on purpose. HTTP/2 sends many requests over ONE
// connection. That's great for web pages, but it defeats multi-connection
// downloading: our 8 "connections" would really be 8 requests squeezed through
// a single connection, with a single connection's speed limit. HTTP/1.1 gives
// each request its own TCP connection, which is what download managers want.
func newClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, // respect the system's proxy settings
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		// Keep finished connections open for reuse (by retries, or the next
		// piece), up to one per possible download connection.
		MaxIdleConnsPerHost: maxConnectionsLimit,
		IdleConnTimeout:     60 * time.Second,
		// An empty (non-nil) TLSNextProto map turns HTTP/2 off.
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &http.Client{Transport: transport}
}
