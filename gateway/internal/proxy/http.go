package proxy

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"time"
)

// userIDHeader carries the verified subject to upstream services.
const userIDHeader = "X-User-ID"

// writeJSON writes body as a JSON response.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError writes {"message": ...}, the same shape Identity uses for its errors.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// isClean reports whether p is already in canonical form. Unclean paths ("..", "//", "." segments)
// are refused rather than normalized, so what is matched is exactly what is forwarded.
func isClean(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}

	cleaned := path.Clean(p)
	if strings.HasSuffix(p, "/") && cleaned != "/" {
		cleaned += "/"
	}

	return cleaned == p
}

// dropUnderscoreHeaders removes request headers whose names contain "_". PHP and other CGI-style
// servers fold "-" and "_" together, so a client sending X_User_ID or X_Forwarded_For could
// collide with the trusted X-User-ID or X-Forwarded-For the gateway sets. nginx drops these by
// default for the same reason.
func dropUnderscoreHeaders(h http.Header) {
	for name := range h {
		if strings.Contains(name, "_") {
			delete(h, name)
		}
	}
}

// bearerToken returns the token of an "Authorization: Bearer <token>" header, or "".
func bearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}

	return strings.TrimSpace(token)
}

// clientIP returns the address rate limits are counted against. By default that is the socket
// peer. When header is set, it is the last address in that header, which the one trusted proxy
// in front of the gateway appended (earlier entries are client-controlled). A header value that
// is not an IP address is ignored.
func clientIP(r *http.Request, header string) string {
	if header != "" {
		values := strings.Join(r.Header.Values(header), ",")
		parts := strings.Split(values, ",")

		if addr, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
			return addr.String()
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// retryAfterSeconds formats a Retry-After header value, rounding up and never below one second.
func retryAfterSeconds(d time.Duration) string {
	seconds := max(int(math.Ceil(d.Seconds())), 1)

	return strconv.Itoa(seconds)
}

// statusRecorder remembers the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	// 1xx responses are informational; the final status comes later.
	if !s.wrote && code >= 200 {
		s.status = code
		s.wrote = true
	}

	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true

	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer (the reverse proxy flushes through it).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
