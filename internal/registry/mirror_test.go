package registry

import (
	"example.com/oci-pull-proxy/internal/config"
	"example.com/oci-pull-proxy/internal/server"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mirrorTransport func(*http.Request) (*http.Response, error)

func (f mirrorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestMirrorIsolationAndAuthorization(t *testing.T) {
	c := config.Config{Token: "secret_token"}
	c.Cache.Directory = t.TempDir()
	c.Cache.MaxSizeGB = 1
	c.Auth.Enabled = true
	c.Auth.Username = "user"
	c.Auth.Password = "password"
	c.Registries = map[string]config.RegistryConfig{"docker.io": {Upstream: "https://private.example", Username: "private", Password: "private-secret"}}
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	p.client.Transport = mirrorTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "registry-1.docker.io" || r.Header.Get("Authorization") != "" || strings.Contains(r.URL.String(), c.Token) || r.URL.RawQuery != "" {
			t.Errorf("unsafe upstream request: %s", r.URL.Host)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("public"))}, nil
	})
	h := server.Handler(p, func() bool { return true })
	path := "/m/secret_token/v2/library/alpine/blobs/sha256:" + strings.Repeat("a", 64)
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.SetBasicAuth("client", "secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 2; i++ {
		if w := request(path); w.Code != 200 || w.Body.String() != "public" {
			t.Fatalf("mirror: %d %s", w.Code, w.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("cache miss: %d", calls)
	}
	for _, bad := range []string{
		strings.Replace(path, "secret_token", "wrong", 1), "/m/secret_token/v2/ghcr.io/team/image/manifests/latest",
		"/m/secret_token/v2/library/alpine/manifests/latest?ns=ghcr.io", "/m/secret_token/v2/library/alpine/manifests/latest?token=secret_token",
		"/m/secret_token/../secret_token/v2/", "/m//secret_token/v2/", "/m/%73ecret_token/v2/", "/m/secret_token%2fv2/", "/m/secret_token/v2/library%2falpine/manifests/latest",
		"/m/secret_token/v2/library/../alpine/manifests/latest", "/m/secret_token/v2/library\\alpine/manifests/latest",
	} {
		w := request(bad)
		if w.Code < 400 || w.Header().Get("Location") != "" {
			t.Errorf("accepted/redirected %q: %d", bad, w.Code)
		}
	}
	if calls != 1 {
		t.Fatal("rejected requests reached upstream")
	}
	if w := request("/m/secret_token/v2/"); w.Code != 200 {
		t.Fatal("ping failed", w.Code)
	}
	if w := request("/v2/"); w.Code != 401 {
		t.Fatal("original auth bypassed")
	}
	// An authenticated private-cache entry cannot satisfy the public mirror request.
	p.client.Transport = mirrorTransport(func(r *http.Request) (*http.Response, error) {
		body := "public"
		if r.URL.Host == "private.example" {
			body = "private"
		} else {
			calls++
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	r := httptest.NewRequest("GET", strings.TrimPrefix(path, "/m/secret_token"), nil)
	r.SetBasicAuth("user", "password")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Body.String() != "private" {
		t.Fatal("private cache polluted")
	}
	if w := request(path); w.Body.String() != "public" {
		t.Fatal("public cache polluted")
	}
}

func TestMirrorBearerAndSameUpstreamCacheIsolation(t *testing.T) {
	c := config.Config{Token: "secret_token"}
	c.Cache.Directory = t.TempDir()
	c.Cache.MaxSizeGB = 1
	c.Auth.Enabled = true
	c.Auth.Username = "u"
	c.Auth.Password = "p"
	c.Registries = map[string]config.RegistryConfig{"docker.io": {Upstream: "https://registry-1.docker.io", Username: "private-user", Password: "private-password"}}
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	anonymousTokens := 0
	p.client.Transport = mirrorTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 200, "private"
		header := http.Header{}
		if r.URL.Host == "auth.docker.io" {
			anonymousTokens++
			if r.Header.Get("Authorization") != "" {
				t.Error("private credentials sent to public token endpoint")
			}
			body = `{"token":"anonymous-hub-token"}`
		} else if r.Header.Get("Authorization") == "" {
			status = 401
			body = ""
			header.Set("WWW-Authenticate", `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`)
		} else if r.Header.Get("Authorization") == "Bearer anonymous-hub-token" {
			body = "public"
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	h := server.Handler(p, func() bool { return true })
	path := "/v2/library/alpine/manifests/sha256:" + strings.Repeat("b", 64)
	request := func(path string, auth bool) string {
		r := httptest.NewRequest("GET", path, nil)
		if auth {
			r.SetBasicAuth("u", "p")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("unexpected status %d", w.Code)
		}
		return w.Body.String()
	}
	if request(path, true) != "private" {
		t.Fatal("private request failed")
	}
	if request("/m/secret_token"+path+"?ns=docker.io", false) != "public" {
		t.Fatal("private cache leaked")
	}
	if request("/m/secret_token"+path, false) != "public" || anonymousTokens != 1 {
		t.Fatal("public cache not reused")
	}
	if request(path, true) != "private" {
		t.Fatal("public cache polluted private entry")
	}
}
