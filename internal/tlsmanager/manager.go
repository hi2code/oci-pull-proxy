// Package tlsmanager owns certificate state independently of the registry proxy.
package tlsmanager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"example.com/oci-pull-proxy/internal/config"
	"fmt"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	auto      *autocert.Manager
	domain    string
	manual    *tls.Certificate
	mu        sync.RWMutex
	cert      *tls.Certificate
	announced bool
	logf      func(string, ...any)
	retry     time.Duration
	startup   sync.Once
}

func New(c config.Config, logger ...func(string, ...any)) (*Manager, error) {
	m := &Manager{domain: c.Server.Domain, logf: log.Printf, retry: time.Minute}
	if len(logger) > 0 {
		m.logf = logger[0]
	}
	if c.Server.TLSMode == "manual" {
		cert, e := tls.LoadX509KeyPair(c.Server.TLSCert, c.Server.TLSKey)
		if e != nil {
			return nil, fmt.Errorf("manual TLS certificate: %w; check TLS_CERT/TLS_KEY paths and permissions", e)
		}
		if !valid(&cert, m.domain, time.Now()) {
			return nil, fmt.Errorf("manual TLS certificate is expired, not yet valid, or does not match PROXY_DOMAIN")
		}
		m.manual = &cert
		m.cert = &cert
		return m, nil
	}
	if !c.ACME.AcceptTOS {
		return nil, fmt.Errorf("ACME_ACCEPT_TOS must be true")
	}
	m.auto = &autocert.Manager{Prompt: autocert.AcceptTOS, Email: c.ACME.Email, HostPolicy: autocert.HostWhitelist(m.domain)}
	m.auto.Cache = &observedCache{Cache: autocert.DirCache(c.ACME.Directory), m: m}
	m.auto.Client = &acme.Client{DirectoryURL: c.ACME.DirectoryURL, HTTPClient: &http.Client{Timeout: 45 * time.Second, Transport: &observedTransport{base: http.DefaultTransport, m: m}}}
	return m, nil
}
func valid(c *tls.Certificate, domain string, now time.Time) bool {
	if c == nil {
		return false
	}
	leaf := c.Leaf
	if leaf == nil {
		if len(c.Certificate) == 0 {
			return false
		}
		var e error
		leaf, e = x509.ParseCertificate(c.Certificate[0])
		if e != nil {
			return false
		}
	}
	return !now.Before(leaf.NotBefore) && now.Before(leaf.NotAfter) && (domain == "" || leaf.VerifyHostname(domain) == nil)
}
func (m *Manager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return valid(m.cert, m.domain, time.Now())
}
func (m *Manager) observe(cert *tls.Certificate) {
	if !valid(cert, m.domain, time.Now()) {
		return
	}
	m.mu.Lock()
	first := !m.announced
	m.announced = true
	m.cert = cert
	m.mu.Unlock()
	if first {
		if m.domain != "" {
			m.logf("HTTPS 已就绪：https://%s\n\ndocker login %s\ndocker pull %s/alpine:latest", m.domain, m.domain, m.domain)
		} else {
			m.logf("HTTPS 已就绪（手动证书）；请将 <证书域名> 替换为证书对应的访问地址\nhttps://<证书域名>\ndocker login <证书域名>\ndocker pull <证书域名>/alpine:latest")
		}
	}
}
func (m *Manager) failure(e error) {
	state := "证书获取失败"
	if m.wasAnnounced() {
		state = "续期失败"
		if m.Ready() {
			state += "（继续使用仍有效证书）"
		}
	}
	m.logf("%s: %v；排查建议：检查域名 DNS、入站 TCP 80/443、出站 HTTPS 和证书卷权限", state, e)
}
func (m *Manager) wasAnnounced() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.announced
}

// Startup emits mode-specific progress after listeners bind and before handshakes.
func (m *Manager) Startup(ctx context.Context) {
	m.startup.Do(func() {
		if m.manual != nil {
			m.logf("手动证书加载成功")
			return
		}
		// The priming ClientHello selects autocert's ECDSA cache entry. Inspect it
		// without contacting the CA; GetCertificate still owns loading and renewal.
		if b, err := m.auto.Cache.Get(ctx, m.domain); err == nil {
			if cert, err := tls.X509KeyPair(b, b); err == nil && valid(&cert, m.domain, time.Now()) {
				return
			}
		}
		m.logf("等待 HTTPS 证书：正在获取 %s 的证书；首次申请失败会自动退避重试", m.domain)
	})
}

func (m *Manager) TLSConfig() *tls.Config {
	var c *tls.Config
	if m.auto != nil {
		c = m.auto.TLSConfig()
	} else {
		c = &tls.Config{}
	}
	c.MinVersion = tls.VersionTLS12
	c.GetCertificate = func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if m.domain != "" && !strings.EqualFold(h.ServerName, m.domain) {
			return nil, fmt.Errorf("TLS server name is not configured")
		}
		cert := m.manual
		if m.auto != nil {
			var e error
			cert, e = m.auto.GetCertificate(h)
			if e != nil {
				m.failure(e)
				return nil, e
			}
			if len(h.SupportedProtos) == 1 && h.SupportedProtos[0] == acme.ALPNProto {
				return cert, nil
			}
		}
		if !valid(cert, m.domain, time.Now()) {
			return nil, fmt.Errorf("no valid TLS certificate")
		}
		m.observe(cert)
		return cert, nil
	}
	return c
}
func (m *Manager) HTTPHandler(fallback http.Handler) http.Handler {
	if m.auto != nil {
		return m.auto.HTTPHandler(fallback)
	}
	return fallback
}

// Run starts only after both listeners are bound. autocert owns background renewal;
// this loop primes the cache, retries initial issuance, and observes renewed state.
func (m *Manager) Run(ctx context.Context) {
	m.Startup(ctx)
	if m.manual != nil {
		m.observe(m.manual)
		return
	}
	delay := m.retry
	for {
		if ctx.Err() != nil {
			return
		}
		cert, e := m.auto.GetCertificate(&tls.ClientHelloInfo{ServerName: m.domain, SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12}, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}, SupportedCurves: []tls.CurveID{tls.CurveP256}, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256}})
		if e == nil && !valid(cert, m.domain, time.Now()) {
			e = fmt.Errorf("certificate is not currently valid")
		}
		if e == nil {
			m.observe(cert)
			delay = m.retry
		}
		next := time.Now().Add(delay)
		timer := time.NewTimer(time.Until(next))
		if e != nil {
			if m.wasAnnounced() {
				m.failure(e)
			} else {
				m.logf("证书获取失败：%v；排查建议：检查域名 DNS、入站 TCP 80/443、出站 HTTPS 和证书卷权限；本次等待：%s；下一次尝试时间：%s", e, delay, next.Format(time.RFC3339Nano))
			}
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if e != nil && delay < 15*time.Minute {
			delay *= 2
			if delay > 15*time.Minute {
				delay = 15 * time.Minute
			}
		}
	}
}

type observedCache struct {
	autocert.Cache
	m *Manager
}

func (c *observedCache) Put(ctx context.Context, key string, b []byte) error {
	if e := c.Cache.Put(ctx, key, b); e != nil {
		c.m.logf("目录写入失败：保存 ACME 状态：%v；请检查证书卷权限和可用空间", e)
		return e
	}
	if cert, e := tls.X509KeyPair(b, b); e == nil {
		c.m.observe(&cert)
	}
	return nil
}

type observedTransport struct {
	base http.RoundTripper
	m    *Manager
}

func (t *observedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, e := t.base.RoundTrip(r)
	if e != nil {
		t.m.failure(fmt.Errorf("ACME request: %w", e))
	} else if resp.StatusCode >= 400 {
		t.m.failure(fmt.Errorf("ACME server returned HTTP %d", resp.StatusCode))
	}
	return resp, e
}
