package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestAccessLogRecordsStatusAndRedactsMirrorToken(t *testing.T) {
	var lines []string
	accessLogf = func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() {
		accessLogf = func(format string, a ...any) {
			fmt.Printf(time.Now().Format(time.RFC3339)+" "+format+"\n", a...)
		}
	})
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/m/") {
			http.Error(w, "nope", 401)
			return
		}
		w.WriteHeader(401)
		w.Write([]byte("body"))
	})
	h := Handler(proxy, func() bool { return true })

	for _, p := range []string{"/healthz", "/v2/", "/v2/library/alpine/manifests/latest", "/m/secret-token/v2/library/alpine/manifests/latest"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (healthz skipped), got %q", lines)
	}
	if !strings.Contains(lines[0], "GET /v2/ 401 4 ") {
		t.Fatalf("missing status/bytes: %q", lines[0])
	}
	if strings.Contains(strings.Join(lines, "\n"), "secret-token") {
		t.Fatalf("mirror token leaked: %q", lines)
	}
	if !strings.Contains(lines[2], "/m/<token>/v2/library/alpine/manifests/latest") {
		t.Fatalf("mirror path not redacted as expected: %q", lines[2])
	}
}

func TestAccessLogLogsFailingHealthProbe(t *testing.T) {
	var lines []string
	accessLogf = func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() {
		accessLogf = func(format string, a ...any) {
			fmt.Printf(time.Now().Format(time.RFC3339)+" "+format+"\n", a...)
		}
	})
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), func() bool { return false })
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/readyz", nil))
	if len(lines) != 1 || !strings.Contains(lines[0], "GET /readyz 503 ") {
		t.Fatalf("failing probe must be logged: %q", lines)
	}
}
