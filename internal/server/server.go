package server

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// accessLogf is replaced in tests to capture access log output.
var accessLogf = func(format string, a ...any) {
	fmt.Printf(time.Now().Format(time.RFC3339)+" "+format+"\n", a...)
}

func Handler(proxy http.Handler, ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready() {
			http.Error(w, "not ready", 503)
			return
		}
		w.Write([]byte("ready\n"))
	})
	v2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready() {
			http.Error(w, "not ready", 503)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	mux.Handle("/m/", v2)
	mux.Handle("/v2/", v2)
	mux.Handle("/v2", v2)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() { logAccess(rec, r, time.Since(start)) }()
		w = rec
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Reject ambiguous paths before ServeMux can clean or redirect them.
		if r.URL.RawPath != "" || strings.ContainsAny(r.URL.Path, "\\%") || strings.Contains(r.URL.Path, "//") {
			http.Error(w, "invalid path", 400)
			return
		}
		for _, part := range strings.Split(r.URL.Path, "/") {
			if part == "." || part == ".." {
				http.Error(w, "invalid path", 400)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code and body size for the access log while
// keeping the upstream response writer capabilities (unwrapping, flushing, sendfile).
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.wrote = true
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if !s.wrote {
		s.wrote = true
		s.status = http.StatusOK
	}
	var (
		n   int64
		err error
	)
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(r)
	} else {
		n, err = io.CopyBuffer(s.ResponseWriter, r, make([]byte, 32*1024))
	}
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

// logAccess writes one line per request. Mirror tokens are redacted because they
// grant anonymous pull access and must never reach the container log.
func logAccess(rec *statusRecorder, r *http.Request, d time.Duration) {
	// Health probes are frequent; only log them when they fail.
	if (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") && rec.status < 400 {
		return
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/m/") {
		if i := strings.IndexByte(p[len("/m/"):], '/'); i >= 0 {
			p = "/m/<token>" + p[len("/m/")+i:]
		} else {
			p = "/m/<token>"
		}
	}
	accessLogf("%s %s %d %d %s", r.Method, p, rec.status, rec.bytes, d.Round(time.Millisecond))
}

func Redirect(domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if domain == "" {
			http.NotFound(w, r)
			return
		}
		u := url.URL{Scheme: "https", Host: domain, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
	})
}
func New(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
}
