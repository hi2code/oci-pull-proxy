package server

import (
	"net/http"
	"net/netip"
	"strings"
)

// Cloudflare's published ranges, checked 2026-10-21. Update with releases:
// https://www.cloudflare.com/ips-v4 and https://www.cloudflare.com/ips-v6
// No network access is needed to initialize or use this list.
var cloudflarePrefixes = func() []netip.Prefix {
	var prefixes []netip.Prefix
	for _, cidr := range strings.Fields(`
173.245.48.0/20
103.21.244.0/22
103.22.200.0/22
103.31.4.0/22
141.101.64.0/18
108.162.192.0/18
190.93.240.0/20
188.114.96.0/20
197.234.240.0/22
198.41.128.0/17
162.158.0.0/15
104.16.0.0/13
104.24.0.0/14
172.64.0.0/13
131.0.72.0/22
2400:cb00::/32
2606:4700::/32
2803:f800::/32
2405:b500::/32
2405:8100::/32
2a06:98c0::/29
2c0f:f248::/32`) {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}
	return prefixes
}()

// Cloudflare checks the TCP peer before trusting any forwarded metadata.
// It deliberately has no local/private-network bypass, including for health checks.
func Cloudflare(domain string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		trusted := false
		if err == nil && peer.Addr().Zone() == "" {
			for _, prefix := range cloudflarePrefixes {
				if prefix.Contains(peer.Addr().Unmap()) {
					trusted = true
					break
				}
			}
		}
		host := strings.ToLower(r.Host)
		proto := r.Header.Values("X-Forwarded-Proto")
		if !trusted || (host != domain && host != domain+":443") || len(proto) != 1 || proto[0] != "https" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
