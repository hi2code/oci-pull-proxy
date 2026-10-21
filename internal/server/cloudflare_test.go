package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCloudflareBoundary(t *testing.T) {
	h := Cloudflare("images.example.com", Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), func() bool { return true }))
	for _, tc := range []struct {
		name, peer, host, proto string
		want                    int
	}{
		{"ipv4", "173.245.48.1:123", "images.example.com", "https", 200},
		{"ipv6", "[2606:4700::1]:123", "images.example.com", "https", 200},
		{"mapped", "[::ffff:104.16.0.1]:123", "images.example.com", "https", 200},
		{"host case", "104.16.0.1:123", "IMAGES.EXAMPLE.COM:443", "https", 200},
		{"ordinary", "203.0.113.1:123", "images.example.com", "https", 403},
		{"loopback", "127.0.0.1:123", "images.example.com", "https", 403},
		{"private", "10.0.0.1:123", "images.example.com", "https", 403},
		{"malformed", "173.245.48.1", "images.example.com", "https", 403},
		{"outside range", "173.245.64.1:123", "images.example.com", "https", 403},
		{"wrong host", "173.245.48.1:123", "evil.example.com", "https", 403},
		{"host suffix", "173.245.48.1:123", "images.example.com.evil.com", "https", 403},
		{"wrong port", "173.245.48.1:123", "images.example.com:80", "https", 403},
		{"http", "173.245.48.1:123", "images.example.com", "http", 403},
		{"missing proto", "173.245.48.1:123", "images.example.com", "", 403},
		{"multiple proto", "173.245.48.1:123", "images.example.com", "https,http", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/healthz", "/readyz", "/v2/", "/unknown", "/.well-known/acme-challenge/test"} {
				r := httptest.NewRequest("GET", "http://images.example.com"+path, nil)
				r.RemoteAddr = tc.peer
				r.Host = tc.host
				r.Header.Set("X-Forwarded-Proto", tc.proto)
				r.Header.Set("CF-Connecting-IP", "173.245.48.1")
				r.Header.Set("X-Forwarded-For", "173.245.48.1")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := tc.want
				if want == 200 {
					if path == "/v2/" {
						want = 204
					} else if path != "/healthz" && path != "/readyz" {
						want = 404
					}
				}
				if w.Code != want {
					t.Fatalf("%s: got %d want %d", path, w.Code, want)
				}
			}
		})
	}
}

func TestCloudflareRejectsDuplicateProtoHeaders(t *testing.T) {
	h := Cloudflare("images.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	r := httptest.NewRequest("GET", "http://images.example.com/healthz", nil)
	r.RemoteAddr = "173.245.48.1:123"
	r.Header.Add("X-Forwarded-Proto", "https")
	r.Header.Add("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("ambiguous protocol accepted: %d", w.Code)
	}
}
