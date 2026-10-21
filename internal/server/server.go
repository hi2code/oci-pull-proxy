package server

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
