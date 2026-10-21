package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthReadinessAndHTTPIsolation(t *testing.T) {
	ready := false
	calls := 0
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(401) })
	h := Handler(proxy, func() bool { return ready })
	for _, tc := range []struct {
		p    string
		code int
	}{{"/healthz", 200}, {"/readyz", 503}, {"/v2/", 503}} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", tc.p, nil))
		if r.Code != tc.code {
			t.Fatalf("%s: %d", tc.p, r.Code)
		}
	}
	ready = true
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/readyz", nil))
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/v2/", nil))
	if r.Code != 401 || calls != 1 {
		t.Fatal("proxy dispatch")
	}
	r = httptest.NewRecorder()
	Redirect("images.example.com").ServeHTTP(r, httptest.NewRequest("GET", "http://evil.example/v2/?a=b", nil))
	if r.Code != 308 || r.Header().Get("Location") != "https://images.example.com/v2/?a=b" || calls != 1 {
		t.Fatal(r)
	}
}
