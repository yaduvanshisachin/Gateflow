package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"path"
	"strings"
)

const requestIDHeader = "X-Request-Id"

// statusRecorder captures the status code and body size for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	// 1xx informational responses (100, 103) can precede the real one and do
	// not count; 101 Switching Protocols is final and does.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer, which is what
// keeps streaming (SSE) and, later, WebSocket upgrades working through us.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// writeError sends the gateway's own errors (as opposed to upstream ones) as
// JSON, tagged with the request ID so a user can quote it in a bug report.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	body := map[string]string{
		"error":      code,
		"message":    msg,
		"request_id": h.Get(requestIDHeader),
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// requestID keeps a well-formed inbound ID (so traces can span systems) and
// otherwise mints one. Restricting the charset and length keeps junk and
// log-injection attempts out of our logs and upstream headers.
func requestID(r *http.Request) string {
	if v := r.Header.Get(requestIDHeader); validRequestID(v) {
		return v
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func validRequestID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// cleanPath normalizes the request path. We route on the cleaned path AND
// forward the cleaned path, so the gateway and the upstream can never
// disagree about which resource "/admin/../public" refers to.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	return path.Clean(p)
}

// stripPrefix removes a matched prefix, always leaving a leading "/".
func stripPrefix(p, prefix string) string {
	if prefix == "/" {
		return p
	}
	rest := strings.TrimPrefix(p, prefix)
	if rest == "" {
		return "/"
	}
	return rest
}

// joinPath appends the request path to a target's optional base path.
func joinPath(base, p string) string {
	if base == "" || base == "/" {
		return p
	}
	return strings.TrimRight(base, "/") + p
}

// clientIP is the direct TCP peer. Because we drop client-supplied
// X-Forwarded-* headers, this is the only address we can vouch for.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
