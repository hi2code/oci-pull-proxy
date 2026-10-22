package registry

import (
	"context"
	"example.com/oci-pull-proxy/internal/cache"
	"example.com/oci-pull-proxy/internal/config"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseRoute(t *testing.T) {
	tests := []struct{ path, upstream, want string }{
		{"/v2/nginx/manifests/v1..2", "docker.io", "/v2/library/nginx/manifests/v1..2"},
		{"/v2/nginx/manifests/latest", "docker.io", "/v2/library/nginx/manifests/latest"},
		{"/v2/docker.io/nginx/tags/list", "docker.io", "/v2/library/nginx/tags/list"},
		{"/v2/getmeili/meilisearch/manifests/nightly", "docker.io", "/v2/getmeili/meilisearch/manifests/nightly"},
		{"/v2/ghcr/team/image/manifests/latest", "docker.io", "/v2/ghcr/team/image/manifests/latest"},
		{"/v2/dockerhub/library/nginx/manifests/latest", "docker.io", "/v2/dockerhub/library/nginx/manifests/latest"},
		{"/v2/quay/team/image/tags/list", "docker.io", "/v2/quay/team/image/tags/list"},
		{"/v2/registry.example:5000/team/nested/image/tags/list", "registry.example:5000", "/v2/team/nested/image/tags/list"},
		{"/v2/localhost/team/image/manifests/latest", "localhost", "/v2/team/image/manifests/latest"},
		{"/v2/ghcr.io/pgdogdev/pgdog/manifests/sha256-dec8fc0568a1e55a01ff77fac2944aa92801db7bdccf313382c417e4d191e434", "ghcr.io", "/v2/pgdogdev/pgdog/manifests/sha256-dec8fc0568a1e55a01ff77fac2944aa92801db7bdccf313382c417e4d191e434"},
		{"/v2/nginx/manifests/sha256:" + strings.Repeat("a", 64), "docker.io", "/v2/library/nginx/manifests/sha256:" + strings.Repeat("a", 64)},
		{"/v2/docker.io/library/nginx/manifests/latest", "docker.io", "/v2/library/nginx/manifests/latest"},
		{"/v2/ghcr.io/acme/widget/blobs/sha256:abc", "ghcr.io", "/v2/acme/widget/blobs/sha256:abc"},
		{"/v2/quay.io/prometheus/prometheus/manifests/v2", "quay.io", "/v2/prometheus/prometheus/manifests/v2"},
	}
	for _, tt := range tests {
		gotUp, gotPath, err := parseRoute(tt.path)
		if err != nil {
			t.Fatalf("parseRoute(%q): %v", tt.path, err)
		}
		if gotUp != tt.upstream || gotPath != tt.want {
			t.Errorf("parseRoute(%q) = (%q,%q), want (%q,%q)", tt.path, gotUp, gotPath, tt.upstream, tt.want)
		}
	}
}

func TestParseRouteRejectsUnknownAndMalformed(t *testing.T) {
	for _, path := range []string{"/v2/ghcr", "/foo", "/v2/nginx/manifests/", "/v2//nginx/manifests/latest", "/v2/team/../nginx/manifests/latest", "/v2/nginx/manifests/latest/extra", "/v2/ghcr.io/manifests/latest"} {
		if _, _, err := parseRoute(path); err == nil {
			t.Errorf("parseRoute(%q) expected error", path)
		}
	}
}

func TestParseBearerChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.example/token",service="registry.example",scope="repository:team/app:pull"`)
	if scheme != "bearer" {
		t.Fatalf("scheme = %q", scheme)
	}
	if params["realm"] != "https://auth.example/token" || params["service"] != "registry.example" || params["scope"] != "repository:team/app:pull" {
		t.Fatalf("unexpected challenge params: %#v", params)
	}
}

func TestDigestCacheOnlyMatchesImmutableSHA256Paths(t *testing.T) {
	yes := []string{"/v2/library/nginx/blobs/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "/v2/owner/app/manifests/sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	for _, p := range yes {
		if !digestRef.MatchString(p) {
			t.Errorf("expected cacheable digest path: %s", p)
		}
	}
	no := []string{"/v2/library/nginx/manifests/latest", "/v2/library/nginx/blobs/sha512:abc"}
	for _, p := range no {
		if digestRef.MatchString(p) {
			t.Errorf("unexpected cacheable path: %s", p)
		}
	}
}

func TestProxyForwardsRegistryV2Path(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/alpine/manifests/latest" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"schemaVersion":2}`)
	}))
	defer upstream.Close()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := &Proxy{cfg: config.Config{}, client: upstream.Client(), cache: cache.New(dir, 1<<20), upstreams: map[string]*url.URL{"docker.io": u}, upstreamCreds: map[string]credentials{}}
	req := httptest.NewRequest(http.MethodGet, "/v2/docker.io/library/alpine/manifests/latest", nil)
	rec := httptest.NewRecorder()
	p.handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"schemaVersion":2}` {
		t.Fatalf("body = %q", got)
	}
}

func TestLoginPushCacheAndRange(t *testing.T) {
	requests := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" {
			t.Error("client password leaked upstream")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "abcdefghij")
	}))
	defer upstream.Close()
	var cfg config.Config
	cfg.Auth.Enabled = true
	cfg.Auth.Username = "alice"
	cfg.Auth.Password = "secret"
	cfg.Cache.Directory = t.TempDir()
	cfg.Cache.MaxSizeGB = 1
	cfg.Registries = map[string]config.RegistryConfig{"docker.io": {Upstream: upstream.URL}}
	p, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	p.client = upstream.Client()
	path := "/v2/docker.io/library/alpine/blobs/sha256:" + strings.Repeat("a", 64)
	call := func(method, path, pw, rng string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if pw != "" {
			r.SetBasicAuth("alice", pw)
		}
		r.Header.Set("Range", rng)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "/v2/", "", " "); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/v2/", "wrong", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/v2/", "secret", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if w := call(method, path, "secret", ""); w.Code != 405 {
			t.Fatal(method, w.Code)
		}
	}
	if w := call("GET", path, "secret", ""); w.Code != 200 || w.Body.String() != "abcdefghij" {
		t.Fatal(w)
	}
	upstream.Close()
	for _, equivalent := range []string{"/v2/alpine/blobs/sha256:", "/v2/library/alpine/blobs/sha256:", "/v2/docker.io/alpine/blobs/sha256:"} {
		if w := call("GET", equivalent+strings.Repeat("a", 64), "secret", ""); w.Code != 200 || w.Body.String() != "abcdefghij" {
			t.Fatal(w)
		}
	}
	for _, unknown := range []string{"unknown.example", "localhost", "localhost:5000"} {
		if w := call("GET", "/v2/"+unknown+"/alpine/manifests/latest", "secret", ""); w.Code != 404 {
			t.Fatal(w)
		}
	}
	if w := call("GET", path, "secret", "bytes=2-5"); w.Code != 206 || w.Body.String() != "cdef" || w.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatal(w)
	}
	if requests != 1 {
		t.Fatal("unexpected upstream fetch", requests)
	}
}
func TestBearerTokenAndRangeMiss(t *testing.T) {
	var url string
	var tokens, blobs int
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens++
			if r.URL.Query().Get("scope") != "repository:library/alpine:pull" {
				t.Error("scope missing")
			}
			io.WriteString(w, `{"token":"test-token"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+url+`/token",service="registry",scope="repository:library/alpine:pull"`)
			w.WriteHeader(401)
			return
		}
		blobs++
		if r.Header.Get("Range") != "bytes=0-2" {
			t.Error("range lost")
		}
		w.Header().Set("Content-Range", "bytes 0-2/10")
		w.WriteHeader(206)
		io.WriteString(w, "abc")
	}))
	defer upstream.Close()
	url = upstream.URL
	var cfg config.Config
	cfg.Cache.Directory = t.TempDir()
	cfg.Cache.MaxSizeGB = 1
	cfg.Registries = map[string]config.RegistryConfig{"docker.io": {Upstream: url}}
	p, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	p.client = upstream.Client()
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("GET", "/v2/docker.io/library/alpine/blobs/sha256:"+strings.Repeat("a", 64), nil)
		r.Header.Set("Range", "bytes=0-2")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 206 || w.Body.String() != "abc" {
			t.Fatal(w)
		}
	}
	if tokens != 2 || blobs != 2 {
		t.Fatal("partial response incorrectly cached", tokens, blobs)
	}
}

func TestConfiguredDomainsAndPrivateCredentials(t *testing.T) {
	for _, domain := range []string{"docker.io", "ghcr.io", "quay.io", "registry.example:5000"} {
		t.Run(domain, func(t *testing.T) {
			var serverURL string
			tokens := 0
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens++
					user, password, ok := r.BasicAuth()
					if !ok || user != "upstream-user" || password != "private-secret" {
						t.Error("wrong token credentials")
					}
					if r.URL.Query().Get("scope") != "repository:team/nested/image:pull" {
						t.Error("wrong token scope")
					}
					io.WriteString(w, `{"token":"private-token"}`)
					return
				}
				if r.URL.Path != "/v2/team/nested/image/tags/list" || r.URL.RawQuery != "n=10" {
					t.Errorf("forwarded URL: %s", r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer private-token" {
					user, password, ok := r.BasicAuth()
					if !ok || user != "upstream-user" || password != "private-secret" {
						t.Error("wrong registry credentials")
					}
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+serverURL+`/token",scope="repository:team/nested/image:pull"`)
					w.WriteHeader(401)
					return
				}
				io.WriteString(w, `{"tags":["latest"]}`)
			}))
			defer upstream.Close()
			serverURL = upstream.URL
			t.Setenv("PROXY_DOMAIN", "images.example.com")
			t.Setenv("PROXY_USERNAME", "client")
			t.Setenv("PROXY_PASSWORD", "client-secret")
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			prefix := map[string]string{"docker.io": "DOCKERHUB", "ghcr.io": "GHCR", "quay.io": "QUAY"}[domain]
			secretKey := prefix + "_TOKEN"
			if prefix == "" {
				prefix = "REGISTRY_CORP"
				secretKey = prefix + "_PASSWORD"
				t.Setenv(prefix+"_DOMAIN", domain)
			}
			t.Setenv(prefix+"_USERNAME", "upstream-user")
			t.Setenv(secretKey, "private-secret")
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			// Keep production credential loading, but route requests to the local TLS fixture.
			rc := cfg.Registries[domain]
			rc.Upstream = upstream.URL
			cfg.Registries[domain] = rc
			p, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			p.client = upstream.Client()
			for _, password := range []string{"", "wrong", "client-secret"} {
				r := httptest.NewRequest("GET", "/v2/"+domain+"/team/nested/image/tags/list?n=10", nil)
				if password != "" {
					r.SetBasicAuth("client", password)
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				if password != "client-secret" {
					if w.Code != 401 || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic ") {
						t.Fatal(w)
					}
				} else if w.Code != 200 || w.Body.String() != `{"tags":["latest"]}` {
					t.Fatal(w)
				}
			}
			if tokens != 1 {
				t.Fatalf("token requests: %d", tokens)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDockerHubTokenHostAllowance(t *testing.T) {
	for _, name := range []string{"docker.io", "ghcr.io"} {
		t.Run(name, func(t *testing.T) {
			u, _ := url.Parse("https://registry-1.docker.io")
			calls := 0
			p := &Proxy{upstreams: map[string]*url.URL{name: u}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "auth.docker.io" {
					t.Fatalf("unexpected token host: %s", r.URL.Host)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"token":"hub-token"}`)), Header: make(http.Header)}, nil
			})}}
			token, err := p.fetchToken(context.Background(), map[string]string{"realm": "https://auth.docker.io/token"}, name)
			if name == "docker.io" {
				if err != nil || token != "hub-token" || calls != 1 {
					t.Fatalf("%s %v %d", token, err, calls)
				}
			} else if err == nil || calls != 0 {
				t.Fatal("unrelated token host allowed")
			}
			for _, realm := range []string{"http://auth.docker.io/token", "https://unknown.example/token"} {
				if _, err := p.fetchToken(context.Background(), map[string]string{"realm": realm}, name); err == nil {
					t.Fatalf("allowed %s", realm)
				}
			}
		})
	}
}

func captureLogs(t *testing.T) *[]string {
	t.Helper()
	lines := &[]string{}
	orig := logf
	logf = func(f string, a ...any) { *lines = append(*lines, fmt.Sprintf(f, a...)) }
	t.Cleanup(func() { logf = orig })
	return lines
}

func TestLogsExplainAuthFailureWithoutLeakingPassword(t *testing.T) {
	lines := captureLogs(t)
	var cfg config.Config
	cfg.Auth.Enabled = true
	cfg.Auth.Username = "alice"
	cfg.Auth.Password = "top-secret-pw"
	p := &Proxy{cfg: cfg, upstreams: map[string]*url.URL{}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	joined := strings.Join(*lines, "\n")
	if !strings.Contains(joined, "代理认证失败") || !strings.Contains(joined, "docker login") {
		t.Fatalf("missing auth hint: %q", joined)
	}
	if strings.Contains(joined, "top-secret-pw") {
		t.Fatalf("password leaked: %q", joined)
	}
}

func TestLogsRedactMirrorTokenAndBearerTokens(t *testing.T) {
	lines := captureLogs(t)
	cfg := config.Config{Token: "server-side-token"}
	p := &Proxy{cfg: cfg, upstreams: map[string]*url.URL{}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/client-supplied-token/v2/library/alpine/manifests/latest", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	joined := strings.Join(*lines, "\n")
	if !strings.Contains(joined, "Token 无效") {
		t.Fatalf("missing mirror token hint: %q", joined)
	}
	for _, secret := range []string{"client-supplied-token", "server-side-token"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("mirror token leaked: %q", joined)
		}
	}
}

func TestLogsUpstreamTokenRejection(t *testing.T) {
	lines := captureLogs(t)
	token := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
	}))
	defer token.Close()
	tu, _ := url.Parse(token.URL)
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+token.URL+`/token",service="ghcr.io",scope="repository:hi2code/missing:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer reg.Close()
	u, _ := url.Parse(reg.URL)
	p := &Proxy{cfg: config.Config{}, client: reg.Client(), cache: cache.New(t.TempDir(), 1<<20),
		upstreams: map[string]*url.URL{"ghcr.io": u}, upstreamCreds: map[string]credentials{}}
	rec := httptest.NewRecorder()
	p.handle(rec, httptest.NewRequest(http.MethodHead, "/v2/ghcr.io/hi2code/missing/manifests/v0.10.2", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	joined := strings.Join(*lines, "\n")
	for _, want := range []string{"token 被拒绝", "repository:hi2code/missing:pull", "上游失败"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in logs: %q", want, joined)
		}
	}
	if !strings.Contains(joined, token.URL) {
		t.Fatalf("token endpoint host should be visible: %q, %q", joined, tu)
	}
}
