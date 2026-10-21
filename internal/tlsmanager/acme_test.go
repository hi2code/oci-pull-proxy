package tlsmanager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"example.com/oci-pull-proxy/internal/config"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// localCA implements the ACME HTTP boundary, validates the served HTTP-01
// challenge, and signs the actual CSR submitted by autocert. No public CA is used.
type localCA struct {
	mu           sync.Mutex
	url          string
	challengeURL string
	ca           *x509.Certificate
	key          *ecdsa.PrivateKey
	orders       map[string]*localOrder
	issued       int
	fail         atomic.Bool
	short        bool
}
type localOrder struct {
	id         string
	authorized bool
	cert       []byte
}

func newLocalCA(t *testing.T, short bool) (*localCA, *httptest.Server) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Local ACME test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	root, e = x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	ca := &localCA{ca: root, key: key, orders: map[string]*localOrder{}, short: short}
	ts := httptest.NewTLSServer(http.HandlerFunc(ca.serve))
	ca.url = ts.URL
	t.Cleanup(ts.Close)
	return ca, ts
}
func (c *localCA) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))
	w.Header().Set("Content-Type", "application/json")
	send := func(v any) { json.NewEncoder(w).Encode(v) }
	if c.fail.Load() {
		w.WriteHeader(400)
		send(map[string]any{"type": "urn:ietf:params:acme:error:serverInternal", "detail": "local test outage"})
		return
	}
	switch r.URL.Path {
	case "/directory":
		send(map[string]any{"newNonce": c.url + "/nonce", "newAccount": c.url + "/account", "newOrder": c.url + "/order", "meta": map[string]string{"termsOfService": c.url + "/terms"}})
	case "/nonce":
		w.WriteHeader(200)
	case "/account":
		w.Header().Set("Location", c.url+"/account/1")
		w.WriteHeader(201)
		send(map[string]any{"status": "valid", "orders": c.url + "/orders"})
	case "/order":
		id := strconv.Itoa(len(c.orders) + 1)
		o := &localOrder{id: id}
		c.orders[id] = o
		w.Header().Set("Location", c.url+"/orders/"+id)
		w.WriteHeader(201)
		send(c.order(o))
	default:
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		o := c.orders[parts[1]]
		if o == nil {
			http.NotFound(w, r)
			return
		}
		switch parts[0] {
		case "authz":
			status := "pending"
			if o.authorized {
				status = "valid"
			}
			send(map[string]any{"status": status, "identifier": map[string]string{"type": "dns", "value": "images.example.com"}, "challenges": []any{map[string]string{"type": "http-01", "url": c.url + "/challenge/" + o.id, "token": "token" + o.id, "status": status}}})
		case "challenge":
			resp, e := http.Get(c.challengeURL + "/.well-known/acme-challenge/token" + o.id)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			defer resp.Body.Close()
			var b strings.Builder
			buf := make([]byte, 1024)
			n, _ := resp.Body.Read(buf)
			b.Write(buf[:n])
			if resp.StatusCode != 200 || !strings.HasPrefix(b.String(), "token"+o.id+".") {
				http.Error(w, "challenge failed", 400)
				return
			}
			o.authorized = true
			send(map[string]string{"type": "http-01", "url": c.url + "/challenge/" + o.id, "token": "token" + o.id, "status": "valid"})
		case "orders":
			send(c.order(o))
		case "finalize":
			var jws struct{ Payload string }
			json.NewDecoder(r.Body).Decode(&jws)
			payload, _ := base64.RawURLEncoding.DecodeString(jws.Payload)
			var p struct{ CSR string }
			json.Unmarshal(payload, &p)
			der, _ := base64.RawURLEncoding.DecodeString(p.CSR)
			csr, e := x509.ParseCertificateRequest(der)
			if e != nil || csr.CheckSignature() != nil || !o.authorized {
				http.Error(w, "invalid CSR or authorization", 400)
				return
			}
			lifetime := 24 * time.Hour
			if c.short && c.issued == 0 {
				lifetime = 6 * time.Second
			}
			now := time.Now()
			leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(c.issued + 2)), DNSNames: csr.DNSNames, NotBefore: now.Add(-time.Second), NotAfter: now.Add(lifetime), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
			cert, e := x509.CreateCertificate(rand.Reader, leaf, c.ca, csr.PublicKey, c.key)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			o.cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})
			o.cert = append(o.cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.ca.Raw})...)
			c.issued++
			send(c.order(o))
		case "cert":
			w.Header().Set("Content-Type", "application/pem-certificate-chain")
			w.Write(o.cert)
		default:
			http.NotFound(w, r)
		}
	}
}
func (c *localCA) order(o *localOrder) map[string]any {
	status := "pending"
	if o.authorized {
		status = "ready"
	}
	if o.cert != nil {
		status = "valid"
	}
	return map[string]any{"status": status, "identifiers": []any{map[string]string{"type": "dns", "value": "images.example.com"}}, "authorizations": []string{c.url + "/authz/" + o.id}, "finalize": c.url + "/finalize/" + o.id, "certificate": c.url + "/cert/" + o.id}
}
func (c *localCA) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.issued }
func localManager(t *testing.T, ca *localCA, ts *httptest.Server, dir string) *Manager {
	t.Helper()
	var cfg config.Config
	cfg.Server.Domain = "images.example.com"
	cfg.Server.TLSMode = "auto"
	cfg.ACME.AcceptTOS = true
	cfg.ACME.Directory = dir
	cfg.ACME.DirectoryURL = ts.URL + "/directory"
	m, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	m.auto.Client.HTTPClient.Transport = &observedTransport{base: ts.Client().Transport, m: m}
	m.retry = 10 * time.Millisecond
	h := m.HTTPHandler(http.NotFoundHandler())
	challenge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.Host = "images.example.com"; h.ServeHTTP(w, r) }))
	t.Cleanup(challenge.Close)
	ca.mu.Lock()
	ca.challengeURL = challenge.URL
	ca.mu.Unlock()
	return m
}
func waitFor(t *testing.T, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func hello(domain string) *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{ServerName: domain, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}}
}
func TestLocalACMEIssuanceRenewalRestartAndDomainRestriction(t *testing.T) {
	ca, ts := newLocalCA(t, true)
	dir := t.TempDir()
	m := localManager(t, ca, ts, dir)
	if m.Ready() {
		t.Fatal("premature readiness")
	}
	if _, e := m.TLSConfig().GetCertificate(hello("other.example.com")); e == nil {
		t.Fatal("wrong domain allowed")
	}
	if ca.count() != 0 {
		t.Fatal("wrong-domain issuance")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, 10*time.Second, m.Ready)
	waitFor(t, 15*time.Second, func() bool { return ca.count() >= 2 })
	cancel()
	cert, e := m.TLSConfig().GetCertificate(hello("images.example.com"))
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.ca)
	if _, e = cert.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "images.example.com"}); e != nil {
		t.Fatal(e)
	}
	n := ca.count()
	ca.fail.Store(true)
	m2 := localManager(t, ca, ts, dir)
	if _, e = m2.TLSConfig().GetCertificate(hello("images.example.com")); e != nil {
		t.Fatal("restart failed to use persisted cert:", e)
	}
	if !m2.Ready() || ca.count() != n {
		t.Fatal("restart requested new cert")
	}
}
func TestLocalACMEInitialFailureRetries(t *testing.T) {
	ca, ts := newLocalCA(t, false)
	ca.fail.Store(true)
	m := localManager(t, ca, ts, t.TempDir())
	m.retry = time.Second
	var failures atomic.Int32
	var logMu sync.Mutex
	var logs strings.Builder
	m.logf = func(f string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(&logs, f+"\n", a...)
		if strings.Contains(f, "排查建议") {
			failures.Add(1)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, 5*time.Second, func() bool { return failures.Load() > 0 })
	if m.Ready() {
		t.Fatal("failed issuance ready")
	}
	ca.fail.Store(false)
	// autocert deliberately retains failed issuance state for one minute.
	waitFor(t, 80*time.Second, m.Ready)
	if _, err := m.TLSConfig().GetCertificate(hello("images.example.com")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		logMu.Lock()
		defer logMu.Unlock()
		return strings.Contains(logs.String(), "HTTPS 已就绪")
	})
	logMu.Lock()
	defer logMu.Unlock()
	text := logs.String()
	pos := -1
	for _, stage := range []string{"等待 HTTPS 证书", "local test outage", "下一次尝试时间", "HTTPS 已就绪", "docker login", "docker pull"} {
		next := strings.Index(text, stage)
		if next <= pos {
			t.Fatalf("missing/out-of-order %s: %s", stage, text)
		}
		pos = next
	}
	if strings.Count(text, "HTTPS 已就绪") != 1 {
		t.Fatal(text)
	}
}

func TestLocalACMERenewalFailureKeepsValidCertificateThenExpires(t *testing.T) {
	ca, ts := newLocalCA(t, true)
	m := localManager(t, ca, ts, t.TempDir())
	var failed atomic.Bool
	m.logf = func(f string, a ...any) {
		if strings.Contains(fmt.Sprintf(f, a...), "续期失败") {
			if strings.Contains(f, "下一次尝试时间") {
				t.Error("renewal must not predict retry time")
			}
			failed.Store(true)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, 10*time.Second, m.Ready)
	ca.fail.Store(true)
	waitFor(t, 5*time.Second, failed.Load)
	if !m.Ready() {
		t.Fatal("lost readiness before certificate expired")
	}
	if _, e := m.TLSConfig().GetCertificate(hello("images.example.com")); e != nil {
		t.Fatal("valid cert unavailable during renewal failure", e)
	}
	waitFor(t, 5*time.Second, func() bool { return !m.Ready() })
	if _, e := m.TLSConfig().GetCertificate(hello("images.example.com")); e == nil {
		t.Fatal("expired certificate served")
	}
}

func TestCachedStartupSkipsAcquisitionMessage(t *testing.T) {
	ca, ts := newLocalCA(t, false)
	dir := t.TempDir()
	first := localManager(t, ca, ts, dir)
	if _, err := first.TLSConfig().GetCertificate(hello("images.example.com")); err != nil {
		t.Fatal(err)
	}
	ca.fail.Store(true)
	m := localManager(t, ca, ts, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs strings.Builder
	m.logf = func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		logs.WriteString(s + "\n")
		if strings.Contains(s, "HTTPS 已就绪") {
			cancel()
		}
	}
	m.Run(ctx)
	if !m.Ready() || strings.Contains(logs.String(), "等待") || strings.Contains(logs.String(), "正在获取") || strings.Count(logs.String(), "HTTPS 已就绪") != 1 {
		t.Fatal(logs.String())
	}
}

func TestInitialRetryLogMatchesSchedule(t *testing.T) {
	ca, ts := newLocalCA(t, false)
	ca.fail.Store(true)
	m := localManager(t, ca, ts, t.TempDir())
	m.retry = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deadlines []time.Time
	var observed []time.Time
	var logs []string
	m.logf = func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		if !strings.Contains(s, "下一次尝试时间：") {
			return
		}
		fields := strings.Split(s, "下一次尝试时间：")
		deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(fields[1]))
		if err != nil {
			t.Error(err)
			cancel()
			return
		}
		deadlines = append(deadlines, deadline)
		observed = append(observed, time.Now())
		logs = append(logs, s)
		if len(deadlines) == 3 {
			cancel()
		}
	}
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("missing retry schedule")
	}
	for i, delay := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
		if !strings.Contains(logs[i], "本次等待："+delay.String()) || !strings.Contains(logs[i], "证书获取失败") {
			t.Fatal(logs[i])
		}
		delta := deadlines[i].Sub(observed[i])
		if delta < delay-50*time.Millisecond || delta > delay {
			t.Fatalf("wrong deadline: %v want %v", delta, delay)
		}
		if i > 0 && observed[i].Before(deadlines[i-1]) {
			t.Fatal("retried before logged deadline")
		}
	}
	if !strings.Contains(logs[0], "local test outage") {
		t.Fatal(logs[0])
	}
}
