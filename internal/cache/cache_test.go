package cache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvictionAndSeparateCertificateState(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "acme")
	os.Mkdir(certDir, 0700)
	cert := filepath.Join(certDir, "account-and-cert")
	os.WriteFile(cert, []byte("preserve"), 0600)
	c := New(filepath.Join(dir, "cache"), 6)
	store := func(key, body string) {
		t.Helper()
		r := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		w := httptest.NewRecorder()
		c.StoreAndStream(w, key, r)
		if w.Body.String() != body {
			t.Fatal("stream changed")
		}
	}
	a, b := Key("up", "a"), Key("up", "b")
	store(a, "first")
	store(b, "second")
	if f, _, e := c.Open(a); e == nil {
		f.Close()
		t.Fatal("old blob not evicted")
	}
	f, _, e := c.Open(b)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	os.RemoveAll(filepath.Join(dir, "cache"))
	if b, e := os.ReadFile(cert); e != nil || string(b) != "preserve" {
		t.Fatal("certificate state lost", e)
	}
}
