package tlsmanager

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"
)

func TestValidityRejectsExpiredFutureAndWrongDomain(t *testing.T) {
	now := time.Now()
	leaf := &x509.Certificate{DNSNames: []string{"images.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	cert := &tls.Certificate{Leaf: leaf}
	if !valid(cert, "images.example.com", now) {
		t.Fatal("valid rejected")
	}
	if valid(cert, "other.example.com", now) || valid(cert, "images.example.com", now.Add(2*time.Hour)) || valid(cert, "images.example.com", now.Add(-2*time.Hour)) || valid(nil, "", now) {
		t.Fatal("invalid accepted")
	}
}
