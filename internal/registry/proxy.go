package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"example.com/oci-pull-proxy/internal/auth"
	"example.com/oci-pull-proxy/internal/cache"
	"example.com/oci-pull-proxy/internal/config"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Proxy struct {
	public         *Proxy
	cacheNamespace string
	cfg            config.Config
	client         *http.Client
	cache          *cache.DiskCache
	upstreams      map[string]*url.URL
	upstreamCreds  map[string]credentials
}
type credentials struct{ username, password string }

var digestRef = regexp.MustCompile(`/(?:blobs|manifests)/sha256:[a-f0-9]{64}$`)

func New(cfg config.Config) (*Proxy, error) {
	p := &Proxy{cfg: cfg, client: &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return errors.New("upstream redirect must use HTTPS")
		}
		// Blob redirects carry signed query strings; log only scheme/host/path.
		logf("跟随上游重定向（第 %d 跳）：%s://%s%s", len(via), req.URL.Scheme, req.URL.Host, req.URL.Path)
		return nil
	}}, cache: cache.New(cfg.Cache.Directory, cfg.Cache.MaxSizeGB*1024*1024*1024), upstreams: map[string]*url.URL{}, upstreamCreds: map[string]credentials{}}
	for name, rc := range cfg.Registries {
		u, e := url.Parse(rc.Upstream)
		if e != nil {
			return nil, e
		}
		p.upstreams[name] = u
		if rc.Username != "" {
			p.upstreamCreds[name] = credentials{rc.Username, rc.Password}
		}
	}
	publicConfig := cfg
	publicConfig.Auth.Enabled = false
	publicConfig.Registries = nil
	publicConfig.Token = ""
	hub, _ := url.Parse("https://registry-1.docker.io")
	p.public = &Proxy{cfg: publicConfig, client: p.client, cache: p.cache, cacheNamespace: "public-docker-hub:", upstreams: map[string]*url.URL{"docker.io": hub}, upstreamCreds: map[string]credentials{}}
	return p, nil
}
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/m/") {
		p.serveMirror(w, r)
		return
	}
	p.handle(w, r)
}

var logf = func(f string, a ...any) { fmt.Printf(time.Now().Format(time.RFC3339)+" "+f+"\n", a...) }

// redactPath hides the mirror token while keeping the rest of the route readable.
func redactPath(p string) string {
	if !strings.HasPrefix(p, "/m/") {
		return p
	}
	rest := p[len("/m/"):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return "/m/<token>" + rest[i:]
	}
	return "/m/<token>"
}

// clientIP is best effort: RemoteAddr is the TCP peer, not a forwarded header.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
func registryNames(m map[string]*url.URL) []string {
	a := make([]string, 0, len(m))
	for k := range m {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		logf("拒绝请求：方法不允许：%s %s 来自 %s", r.Method, redactPath(r.URL.Path), clientIP(r))
		http.Error(w, "pull-only proxy: method not allowed", 405)
		return
	}
	if p.cfg.Auth.Enabled && !p.authorized(r) {
		logf("拒绝请求：代理认证失败：%s %s 来自 %s（客户端需 docker login %s）", r.Method, redactPath(r.URL.Path), clientIP(r), r.Host)
		w.Header().Set("WWW-Authenticate", `Basic realm="oci-pull-proxy", charset="UTF-8"`)
		http.Error(w, "authentication required", 401)
		return
	}
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		w.WriteHeader(200)
		return
	}
	name, upPath, err := parseRoute(r.URL.Path)
	if err != nil {
		logf("拒绝请求：路由不合法：%s: %v", redactPath(r.URL.Path), err)
		http.Error(w, err.Error(), 404)
		return
	}
	upstream, ok := p.upstreams[name]
	if !ok {
		logf("拒绝请求：未配置的上游 %q：%s（已配置：%s）", name, redactPath(r.URL.Path), strings.Join(registryNames(p.upstreams), ", "))
		http.Error(w, "unknown upstream", 404)
		return
	}
	target := *upstream
	target.Path = strings.TrimRight(upstream.Path, "/") + upPath
	target.RawQuery = r.URL.RawQuery
	cacheable := r.Method == http.MethodGet && digestRef.MatchString(upPath)
	canStore := cacheable && r.Header.Get("Range") == ""
	key := cache.Key(p.cacheNamespace+upstream.String(), upPath+"?"+r.URL.RawQuery)
	logf("代理请求：%s %s -> %s%s（客户端 %s）", r.Method, redactPath(r.URL.Path), upstream.Host, upPath, clientIP(r))
	if cacheable {
		if f, meta, e := p.cache.Open(key); e == nil {
			defer f.Close()
			logf("缓存命中：%s", upPath)
			for k, v := range meta.Headers {
				w.Header().Set(k, v)
			}
			w.Header().Set("Accept-Ranges", "bytes")
			http.ServeContent(w, r, filepath.Base(key), time.Time{}, f)
			return
		}
		logf("缓存未命中：%s", upPath)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		logf("构造上游请求失败：%s: %v", target.Redacted(), err)
		http.Error(w, "bad request", 400)
		return
	}
	copyRequestHeaders(req.Header, r.Header)
	if c, ok := p.upstreamCreds[name]; ok {
		req.SetBasicAuth(c.username, c.password)
		logf("使用上游凭据：%s 用户 %s", name, c.username)
	}
	start := time.Now()
	resp, err := p.doUpstream(req, name)
	if err != nil {
		logf("上游失败：%s %s %s: %v（已耗时 %s）", name, r.Method, target.Redacted(), err, time.Since(start).Round(time.Millisecond))
		http.Error(w, "upstream request failed", 502)
		return
	}
	logf("上游响应：%s %s %s -> %d（%s，已耗时 %s）", name, r.Method, target.Redacted(), resp.StatusCode, resp.Header.Get("Content-Type"), time.Since(start).Round(time.Millisecond))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), resp.Body))
		logf("上游拒绝：%s %s 返回 %d，响应体：%s", name, target.Redacted(), resp.StatusCode, strings.TrimSpace(string(body)))
	}
	defer resp.Body.Close()
	if digestRef.MatchString(upPath) && resp.Header.Get("Docker-Content-Digest") == "" {
		// Blob storage redirects often drop registry-specific headers. The requested
		// digest is immutable and can safely be echoed when the final object store omits it.
		resp.Header.Set("Docker-Content-Digest", upPath[strings.LastIndex(upPath, "/")+1:])
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if r.Method == http.MethodHead || resp.StatusCode != 200 || !canStore {
		w.WriteHeader(resp.StatusCode)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, resp.Body)
		}
		return
	}
	p.cache.StoreAndStream(w, key, resp)
}

var repositoryComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
var manifestReference = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|[A-Za-z][A-Za-z0-9_+.-]*:[a-fA-F0-9]+)$`)
var blobReference = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*:[a-fA-F0-9]+$`)

func parseRoute(path string) (string, string, error) {
	if strings.Contains(path, "\\") || !strings.HasPrefix(path, "/v2/") {
		return "", "", fmt.Errorf("invalid registry path")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v2/"), "/")
	name := "docker.io"
	if strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost" {
		name = parts[0]
		if !config.ValidRegistryDomain(name) {
			return "", "", fmt.Errorf("invalid registry domain")
		}
		parts = parts[1:]
	}
	if len(parts) < 3 {
		return "", "", fmt.Errorf("expected repository and registry endpoint")
	}
	repo := parts[:len(parts)-2]
	endpoint, ref := parts[len(parts)-2], parts[len(parts)-1]
	switch endpoint {
	case "manifests":
		if !manifestReference.MatchString(ref) {
			return "", "", fmt.Errorf("invalid manifest reference")
		}
	case "blobs":
		if !blobReference.MatchString(ref) {
			return "", "", fmt.Errorf("invalid blob digest")
		}
	case "tags":
		if ref != "list" {
			return "", "", fmt.Errorf("unsupported registry endpoint")
		}
	default:
		return "", "", fmt.Errorf("unsupported registry endpoint")
	}
	for _, component := range repo {
		if !repositoryComponent.MatchString(component) {
			return "", "", fmt.Errorf("invalid repository")
		}
	}
	if name == "docker.io" && len(repo) == 1 {
		parts = append([]string{"library"}, parts...)
	}
	return name, "/v2/" + strings.Join(parts, "/"), nil
}
func (p *Proxy) authorized(r *http.Request) bool {
	return auth.Authorized(r, p.cfg.Auth.Username, p.cfg.Auth.Password)
}
func copyRequestHeaders(dst, src http.Header) {
	for _, k := range []string{"Accept", "Accept-Encoding", "Range", "If-None-Match", "If-Modified-Since", "User-Agent"} {
		for _, v := range src.Values(k) {
			dst.Add(k, v)
		}
	}
	dst.Set("Accept-Encoding", "identity")
}
func copyResponseHeaders(dst, src http.Header) {
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "ETag", "Last-Modified", "Docker-Content-Digest", "Docker-Distribution-Api-Version", "WWW-Authenticate", "Retry-After", "Accept-Ranges", "Location"} {
		for _, v := range src.Values(k) {
			dst.Add(k, v)
		}
	}
}

func (p *Proxy) doUpstream(req *http.Request, name string) (*http.Response, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	scheme, params := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	logf("上游要求认证：%s %s -> 401，挑战 %s realm=%s scope=%q", name, req.URL.Redacted(), scheme, params["realm"], params["scope"])
	if scheme == "bearer" && params["realm"] != "" {
		_ = resp.Body.Close()
		token, e := p.fetchToken(req.Context(), params, name)
		if e != nil {
			return nil, e
		}
		retry, e := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), nil)
		if e != nil {
			return nil, e
		}
		retry.Header = req.Header.Clone()
		retry.Header.Set("Authorization", "Bearer "+token)
		logf("使用上游 bearer token 重试：%s %s", name, req.URL.Redacted())
		return p.client.Do(retry)
	}
	if scheme == "basic" {
		if c, ok := p.upstreamCreds[name]; ok {
			_ = resp.Body.Close()
			retry, e := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), nil)
			if e != nil {
				return nil, e
			}
			retry.Header = req.Header.Clone()
			retry.SetBasicAuth(c.username, c.password)
			logf("使用上游 basic 凭据重试：%s 用户 %s", name, c.username)
			return p.client.Do(retry)
		}
		logf("上游要求 basic 凭据，但未配置 %s 用户名/密码", name)
	}
	return resp, nil
}
func parseChallenge(s string) (string, map[string]string) {
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return strings.ToLower(strings.TrimSpace(s)), map[string]string{}
	}
	scheme := strings.ToLower(strings.TrimSpace(s[:i]))
	rest := strings.TrimSpace(s[i+1:])
	out := map[string]string{}
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ,")
		eq := strings.IndexByte(rest, '=')
		if eq < 1 {
			break
		}
		key := strings.TrimSpace(rest[:eq])
		rest = strings.TrimSpace(rest[eq+1:])
		var val string
		if strings.HasPrefix(rest, "\"") {
			rest = rest[1:]
			end := strings.IndexByte(rest, '"')
			if end < 0 {
				break
			}
			val = rest[:end]
			rest = rest[end+1:]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				val = rest
				rest = ""
			} else {
				val = rest[:end]
				rest = rest[end+1:]
			}
		}
		out[key] = val
	}
	return scheme, out
}
func (p *Proxy) fetchToken(ctx context.Context, ch map[string]string, name string) (string, error) {
	u, err := url.Parse(ch["realm"])
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("invalid token realm")
	}
	upstream, ok := p.upstreams[name]
	if !ok {
		return "", fmt.Errorf("unknown upstream %q", name)
	}
	// Only allow token services hosted by the configured registry, plus Docker Hub's
	// documented auth service. This prevents a malicious challenge from causing SSRF.
	allowedHost := strings.EqualFold(u.Hostname(), upstream.Hostname()) || (name == "docker.io" && strings.EqualFold(u.Hostname(), "auth.docker.io"))
	if !allowedHost {
		return "", fmt.Errorf("token realm host %q is not allowed for upstream %q", u.Hostname(), name)
	}
	q := u.Query()
	if ch["service"] != "" {
		q.Set("service", ch["service"])
	}
	if ch["scope"] != "" {
		q.Set("scope", ch["scope"])
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if c, ok := p.upstreamCreds[name]; ok {
		req.SetBasicAuth(c.username, c.password)
		logf("向上游 token 服务发送 %s 凭据：用户 %s", name, c.username)
	}
	logf("获取上游 token：%s %s", name, u.Redacted())
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logf("上游 token 被拒绝：%s 返回 %s，响应体：%s", name, resp.Status, strings.TrimSpace(string(body)))
		return "", fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	var result struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.Token != "" {
		logf("上游 token 获取成功：%s（长度 %d）", name, len(result.Token))
		return result.Token, nil
	}
	if result.AccessToken != "" {
		logf("上游 token 获取成功：%s（长度 %d）", name, len(result.AccessToken))
		return result.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint returned no token")
}
