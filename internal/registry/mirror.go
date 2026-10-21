package registry

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (p *Proxy) serveMirror(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/m/")
	token, route, ok := strings.Cut(rest, "/")
	got, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(p.cfg.Token))
	if !ok || p.cfg.Token == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		// Never log the presented token: it grants anonymous pull access.
		logf("拒绝 mirror 请求：Token 无效或缺失（来自 %s），%s", clientIP(r), describeMirrorToken(p.cfg.Token))
		http.Error(w, "invalid mirror token", http.StatusUnauthorized)
		return
	}
	route = "/" + route
	if route != "/v2" && route != "/v2/" {
		name, _, err := parseRoute(route)
		if err != nil || name != "docker.io" {
			logf("拒绝 mirror 请求：仅支持 Docker Hub 公共镜像：%s", redactPath(route))
			http.Error(w, "Docker Hub public mirror only", http.StatusNotFound)
			return
		}
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) > 1 || (len(q) == 1 && (len(q["ns"]) != 1 || q.Get("ns") != "docker.io")) {
		http.Error(w, "unsupported mirror query", http.StatusBadRequest)
		return
	}
	clone := r.Clone(r.Context())
	clone.URL.Path = route
	clone.URL.RawPath = ""
	clone.URL.RawQuery = ""
	clone.RequestURI = route
	clone.Header.Del("Authorization")
	logf("mirror 请求转发：%s %s", r.Method, redactPath(route))
	p.public.handle(w, clone)
}

// describeMirrorToken reports configuration state without ever printing the secret.
func describeMirrorToken(t string) string {
	if t == "" {
		return "服务端未配置 mirror Token"
	}
	return fmt.Sprintf("服务端已配置 mirror Token（长度 %d）", len(t))
}
