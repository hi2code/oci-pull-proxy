package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"example.com/oci-pull-proxy/internal/config"
	"example.com/oci-pull-proxy/internal/registry"
	"example.com/oci-pull-proxy/internal/server"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCommandsAndDirectoryCheck(t *testing.T) {
	var out, errout bytes.Buffer
	t.Setenv("PROXY_PASSWORD", "")
	if Main([]string{"version"}, &out, &errout) != 0 || !strings.Contains(out.String(), "oci-pull-proxy") {
		t.Fatal("version requires configuration")
	}
	if Main([]string{"check"}, &out, &errout) == 0 {
		t.Fatal("invalid config accepted")
	}
	dir := t.TempDir()
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("PROXY_DOMAIN", "images.example.com")
	t.Setenv("PROXY_USERNAME", "alice")
	t.Setenv("PROXY_PASSWORD", "real-test-secret")
	t.Setenv("ACME_ACCEPT_TOS", "true")
	t.Setenv("CACHE_DIRECTORY", filepath.Join(dir, "cache"))
	t.Setenv("ACME_DIRECTORY", filepath.Join(dir, "acme"))
	t.Setenv("ACME_DIRECTORY_URL", "https://127.0.0.1:1/directory")
	if code := Main([]string{"check"}, &out, &errout); code != 0 {
		t.Fatal(code, errout.String())
	}
	entries, e := os.ReadDir(filepath.Join(dir, "acme"))
	if e != nil || len(entries) != 0 {
		t.Fatal("check created ACME state", entries, e)
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, []byte("not a directory"), 0600)
	t.Setenv("ACME_DIRECTORY", file)
	if Main([]string{"check"}, &out, &errout) == 0 {
		t.Fatal("unwritable directory accepted")
	}
	if Main([]string{"unknown"}, &out, &errout) != 2 {
		t.Fatal("unknown command exit")
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func manualConfig(t *testing.T, upstream *httptest.Server) config.Config {
	t.Helper()
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	key, e := x509.MarshalPKCS8PrivateKey(upstream.TLS.Certificates[0].PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600)
	var c config.Config
	c.Server.TLSMode = "manual"
	c.Server.TLSCert = certFile
	c.Server.TLSKey = keyFile
	c.Server.Listen = freeAddress(t)
	c.Server.HTTPListen = freeAddress(t)
	c.Auth.Enabled = true
	c.Auth.Username = "alice"
	c.Auth.Password = "secret"
	c.Cache.Directory = filepath.Join(dir, "cache")
	c.Cache.MaxSizeGB = 1
	c.Registries = map[string]config.RegistryConfig{"docker.io": {Upstream: upstream.URL}}
	return c
}
func TestRunStartupAndDrainsActivePullOnCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; io.WriteString(w, "manifest") }))
	defer upstream.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = upstream.Client().Transport
	defer func() { http.DefaultTransport = previous }()
	c := manualConfig(t, upstream)
	if e := Check(c); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c) }()
	client := upstream.Client()
	client.Timeout = 5 * time.Second
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, e := client.Get("https://" + c.Server.Listen + "/readyz")
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("startup not ready")
	}
	// A completed TLS handshake with no HTTP request remains StateNew.
	// net/http waits more than five seconds before treating it as idle.
	idle, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", c.Server.Listen, client.Transport.(*http.Transport).TLSClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	pulled := make(chan error, 1)
	go func() {
		r, _ := http.NewRequest("GET", "https://"+c.Server.Listen+"/v2/docker.io/library/alpine/manifests/latest", nil)
		r.SetBasicAuth("alice", "secret")
		resp, e := client.Do(r)
		if e == nil {
			defer resp.Body.Close()
			var b []byte
			b, e = io.ReadAll(resp.Body)
			if string(b) != "manifest" {
				e = fmt.Errorf("pull body %q", b)
			}
		}
		pulled <- e
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not reached")
	}
	cancel()
	select {
	case e := <-done:
		t.Fatal("shutdown did not wait for active pull", e)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if e := <-pulled; e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	// Run allows 20 seconds to drain; include scheduler headroom.
	case <-time.After(25 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	for _, a := range []string{c.Server.Listen, c.Server.HTTPListen} {
		l, e := net.Listen("tcp", a)
		if e != nil {
			t.Fatal("listener not released", e)
		}
		l.Close()
	}
}
func TestRunRollsBackHTTPSListenerWhenHTTPBindFails(t *testing.T) {
	upstream := httptest.NewTLSServer(http.NotFoundHandler())
	defer upstream.Close()
	c := manualConfig(t, upstream)
	occupied, e := net.Listen("tcp", c.Server.HTTPListen)
	if e != nil {
		t.Fatal(e)
	}
	defer occupied.Close()
	if e = Run(context.Background(), c); e == nil || !strings.Contains(e.Error(), "HTTP_LISTEN") {
		t.Fatal("missing bind error", e)
	}
	l, e := net.Listen("tcp", c.Server.Listen)
	if e != nil {
		t.Fatal("HTTPS listener leaked", e)
	}
	l.Close()
}

func TestStartupFailureLogs(t *testing.T) {
	upstream := httptest.NewTLSServer(http.NotFoundHandler())
	defer upstream.Close()
	for _, scenario := range []string{"config", "directory", "https", "http"} {
		t.Run(scenario, func(t *testing.T) {
			c := manualConfig(t, upstream)
			want := ""
			switch scenario {
			case "config":
				c.Auth.Password = ""
				want = "配置错误"
			case "directory":
				c.Cache.Directory = c.Server.TLSCert
				want = "目录权限错误"
			default:
				addr := c.Server.Listen
				if scenario == "http" {
					addr = c.Server.HTTPListen
				}
				l, err := net.Listen("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				want = "端口监听失败"
			}
			var logs bytes.Buffer
			err := run(context.Background(), c, func(f string, a ...any) { fmt.Fprintf(&logs, f+"\n", a...) })
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want %s, got %v", want, err)
			}
			if !strings.Contains(logs.String(), "正在检查配置") || strings.Contains(logs.String(), "HTTPS 已就绪") || strings.Contains(logs.String(), "服务端口已监听") {
				t.Fatal(logs.String())
			}
		})
	}
}

func TestStartupManualLogOrder(t *testing.T) {
	upstream := httptest.NewTLSServer(http.NotFoundHandler())
	defer upstream.Close()
	c := manualConfig(t, upstream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var logs strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, c, func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			s := fmt.Sprintf(f, a...)
			logs.WriteString(s + "\n")
			if strings.Contains(s, "HTTPS 已就绪") {
				cancel()
			}
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no readiness log")
	}
	mu.Lock()
	defer mu.Unlock()
	text := logs.String()
	pos := -1
	for _, stage := range []string{"正在检查配置", "配置检查通过", "服务端口已监听", "手动证书加载成功", "HTTPS 已就绪", "docker login", "docker pull"} {
		next := strings.Index(text, stage)
		if next <= pos {
			t.Fatalf("missing/out-of-order %s: %s", stage, text)
		}
		pos = next
	}
	if strings.Contains(text, "正在获取") || strings.Contains(text, c.Auth.Password) || strings.Count(text, "HTTPS 已就绪") != 1 {
		t.Fatal(text)
	}
}

func cloudflareConfig(t *testing.T, upstream *httptest.Server) config.Config {
	c := manualConfig(t, upstream)
	c.Server.TLSMode = "cloudflare"
	c.Server.TLSCert = ""
	c.Server.TLSKey = ""
	c.Server.Domain = "images.example.com"
	c.ACME.Directory = filepath.Join(t.TempDir(), "must-not-exist")
	c.ACME.DirectoryURL = "invalid-unused"
	return c
}

func TestCloudflareStartup(t *testing.T) {
	upstream := httptest.NewTLSServer(http.NotFoundHandler())
	defer upstream.Close()
	c := cloudflareConfig(t, upstream)
	// Occupying HTTPS must not affect an HTTP-only origin.
	occupied, err := net.Listen("tcp", c.Server.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs strings.Builder
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, c, func(f string, a ...any) {
			s := fmt.Sprintf(f, a...)
			logs.WriteString(s + "\n")
			if strings.Contains(s, "HTTP 回源服务已就绪") {
				close(started)
			}
		})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not started")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	req, _ := http.NewRequest("GET", "http://"+c.Server.HTTPListen+"/readyz", nil)
	req.Host = c.Server.Domain
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("CF-Connecting-IP", "173.245.48.1")
	req.Header.Set("X-Forwarded-For", "173.245.48.1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal(resp.StatusCode)
	}
	// General OPTIONS requests must reach the gate as well.
	req.Method = "OPTIONS"
	req.URL.Path = "*"
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("OPTIONS * bypassed source gate: %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("shutdown")
	}
	if _, err := os.Stat(c.ACME.Directory); !os.IsNotExist(err) {
		t.Fatalf("ACME directory touched: %v", err)
	}
	for _, bad := range []string{"HTTPS 已就绪", "证书", c.Auth.Password} {
		if strings.Contains(logs.String(), bad) {
			t.Fatal(logs.String())
		}
	}
	for _, want := range []string{"仅接受 Cloudflare 来源", "https://images.example.com", "docker login", "docker pull"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatal(logs.String())
		}
	}
}

func TestCloudflareHTTPBindFailure(t *testing.T) {
	upstream := httptest.NewTLSServer(http.NotFoundHandler())
	defer upstream.Close()
	c := cloudflareConfig(t, upstream)
	occupied, err := net.Listen("tcp", c.Server.HTTPListen)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if err := Run(context.Background(), c); err == nil || !strings.Contains(err.Error(), "HTTP_LISTEN") {
		t.Fatal(err)
	}
}

// The test adapter supplies a CF TCP peer so the real gate, auth, proxy and
// lifecycle can be exercised without routing test traffic through Cloudflare.
func TestCloudflareAuthenticatedPullDrains(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; io.WriteString(w, "manifest") }))
	defer upstream.Close()
	defer unblock()
	previous := http.DefaultTransport
	http.DefaultTransport = upstream.Client().Transport
	defer func() { http.DefaultTransport = previous }()
	c := cloudflareConfig(t, upstream)
	p, err := registry.New(c)
	if err != nil {
		t.Fatal(err)
	}
	guarded := server.Cloudflare(c.Server.Domain, server.Handler(p, func() bool { return true }))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "173.245.48.1:123"
		guarded.ServeHTTP(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runHTTP(ctx, c, h, func(f string, a ...any) {
			if strings.Contains(f, "HTTP 回源服务已就绪") {
				close(started)
			}
		})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup timeout")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	request := func(path, password string) (int, string, error) {
		r, _ := http.NewRequest("GET", "http://"+c.Server.HTTPListen+path, nil)
		r.Host = c.Server.Domain
		r.Header.Set("X-Forwarded-Proto", "https")
		if password != "" {
			r.SetBasicAuth("alice", password)
		}
		resp, err := client.Do(r)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), err
	}
	for _, tc := range []struct {
		path, password string
		code           int
	}{
		{"/readyz", "", 200}, {"/v2/", "", 401}, {"/v2/", "wrong", 401}, {"/v2/", "secret", 200},
		{"/v2/docker.io/library/alpine/manifests/latest", "wrong", 401},
	} {
		code, _, err := request(tc.path, tc.password)
		if err != nil || code != tc.code {
			t.Fatalf("%s: %d %v", tc.path, code, err)
		}
	}
	pulled := make(chan error, 1)
	go func() {
		code, body, err := request("/v2/docker.io/library/alpine/manifests/latest", "secret")
		if err == nil && (code != 200 || body != "manifest") {
			err = fmt.Errorf("pull: %d %q", code, body)
		}
		pulled <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no upstream request")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("did not drain: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-pulled; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timeout")
	}
	l, err := net.Listen("tcp", c.Server.HTTPListen)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}
