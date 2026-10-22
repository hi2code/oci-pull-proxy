// Package config loads defaults and environment variables supplied by .env deployments.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Config struct {
	Token  string
	Server struct {
		Listen     string
		HTTPListen string
		Domain     string
		TLSMode    string
		TLSCert    string
		TLSKey     string
	}
	ACME struct {
		AcceptTOS    bool
		Email        string
		DirectoryURL string
		Directory    string
	}
	Auth struct {
		Enabled  bool
		Username string
		Password string
	}
	Cache struct {
		Directory string
		MaxSizeGB int64
	}
	Registries map[string]RegistryConfig
}
type RegistryConfig struct {
	Upstream string
	Username string
	Password string
}

func Load() (Config, error) {
	var c Config
	c.Token = os.Getenv("TOKEN")
	c.Server.Listen = ":8443"
	c.Server.HTTPListen = ":8080"
	c.Auth.Enabled = true
	c.Cache.Directory = "/data/cache"
	c.Cache.MaxSizeGB = 40
	c.ACME.Directory = "/data/acme"
	c.ACME.AcceptTOS = true
	c.Registries = map[string]RegistryConfig{"docker.io": {Upstream: "https://registry-1.docker.io"}, "ghcr.io": {Upstream: "https://ghcr.io"}, "quay.io": {Upstream: "https://quay.io"}}
	if err := c.loadRegistries(); err != nil {
		return c, err
	}
	for k, p := range map[string]*string{"PROXY_DOMAIN": &c.Server.Domain, "PROXY_USERNAME": &c.Auth.Username, "TLS_MODE": &c.Server.TLSMode, "TLS_CERT": &c.Server.TLSCert, "TLS_KEY": &c.Server.TLSKey, "HTTPS_LISTEN": &c.Server.Listen, "HTTP_LISTEN": &c.Server.HTTPListen, "CACHE_DIRECTORY": &c.Cache.Directory, "ACME_DIRECTORY": &c.ACME.Directory, "ACME_EMAIL": &c.ACME.Email, "ACME_DIRECTORY_URL": &c.ACME.DirectoryURL} {
		if v, ok := os.LookupEnv(k); ok {
			*p = v
		}
	}
	if v, ok := os.LookupEnv("ACME_ACCEPT_TOS"); ok && c.Server.TLSMode != "cloudflare" {
		b, e := strconv.ParseBool(v)
		if e != nil {
			return c, fmt.Errorf("ACME_ACCEPT_TOS must be true or false")
		}
		c.ACME.AcceptTOS = b
	}
	if v, ok := os.LookupEnv("CACHE_MAX_SIZE_GB"); ok {
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil {
			return c, fmt.Errorf("CACHE_MAX_SIZE_GB must be an integer")
		}
		c.Cache.MaxSizeGB = n
	}
	c.Auth.Password = os.Getenv("PROXY_PASSWORD")
	c.Server.Domain = strings.ToLower(strings.TrimSpace(c.Server.Domain))
	if c.Server.TLSMode == "" {
		if c.Server.TLSCert != "" && c.Server.TLSKey != "" {
			c.Server.TLSMode = "manual"
		} else {
			c.Server.TLSMode = "auto"
		}
	}
	return c, c.Validate()
}

// Built-in registries use convenient names; custom registries use named groups.
func (c *Config) loadRegistries() error {
	for _, entry := range []struct{ domain, prefix string }{
		{"docker.io", "DOCKERHUB"}, {"ghcr.io", "GHCR"}, {"quay.io", "QUAY"},
	} {
		r, err := registryFromEnv(entry.prefix, entry.prefix+"_TOKEN", c.Registries[entry.domain].Upstream)
		if err != nil {
			return err
		}
		c.Registries[entry.domain] = r
	}
	// Discover groups from populated variables; no separate registry list is needed.
	groups := map[string]bool{}
	for _, env := range os.Environ() {
		key, value, _ := strings.Cut(env, "=")
		if value == "" || !strings.HasPrefix(key, "REGISTRY_") {
			continue
		}
		for _, suffix := range []string{"_DOMAIN", "_UPSTREAM", "_USERNAME", "_PASSWORD"} {
			if !strings.HasSuffix(key, suffix) {
				continue
			}
			alias := strings.TrimSuffix(strings.TrimPrefix(key, "REGISTRY_"), suffix)
			if !registryAlias.MatchString(alias) {
				return fmt.Errorf("custom registry group names must start with an uppercase letter and contain only uppercase letters, digits and underscores")
			}
			groups[alias] = true
			break
		}
	}
	aliases := make([]string, 0, len(groups))
	for alias := range groups {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		prefix := "REGISTRY_" + alias
		domain := os.Getenv(prefix + "_DOMAIN")
		if !ValidRegistryDomain(domain) {
			return fmt.Errorf("%s_DOMAIN must be a lowercase registry domain, optionally with a port", prefix)
		}
		if _, exists := c.Registries[domain]; exists {
			return fmt.Errorf("%s_DOMAIN specifies a duplicate registry; use DOCKERHUB_*, GHCR_* or QUAY_* for built-in registries", prefix)
		}
		upstream := "https://" + domain
		if v, ok := os.LookupEnv(prefix + "_UPSTREAM"); ok {
			upstream = v
		}
		r, err := registryFromEnv(prefix, prefix+"_PASSWORD", upstream)
		if err != nil {
			return err
		}
		c.Registries[domain] = r
	}
	return nil
}

var registryAlias = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func registryFromEnv(prefix, passwordKey, upstream string) (RegistryConfig, error) {
	r := RegistryConfig{Upstream: upstream, Username: os.Getenv(prefix + "_USERNAME"), Password: os.Getenv(passwordKey)}
	if (r.Username == "") != (r.Password == "") || strings.Contains(r.Username, ":") || (r.Username != "" && strings.TrimSpace(r.Username) == "") {
		return r, fmt.Errorf("%s_USERNAME and %s must both be set or both empty; username must be nonblank and contain no colon", prefix, passwordKey)
	}
	return r, nil
}

func (c Config) Validate() error {
	if c.Token == "v2" {
		return fmt.Errorf("TOKEN value v2 is reserved")
	}
	if c.Token != "" && !safeToken.MatchString(c.Token) {
		return fmt.Errorf("TOKEN must contain only URL-safe letters, digits, - and _")
	}
	if !c.Auth.Enabled {
		return fmt.Errorf("proxy authentication must be enabled")
	}
	if strings.TrimSpace(c.Auth.Username) == "" || strings.Contains(c.Auth.Username, ":") {
		return fmt.Errorf("PROXY_USERNAME must be nonempty and contain no colon")
	}
	p := strings.ToLower(strings.TrimSpace(c.Auth.Password))
	if p == "" || strings.Contains(p, "replace") || strings.Contains(p, "替换") || p == "changeme" || p == "your-password" {
		return fmt.Errorf("PROXY_PASSWORD must contain a real nonempty password, not an example placeholder")
	}
	if c.Server.Domain != "" && !validDomain(c.Server.Domain) {
		return fmt.Errorf("PROXY_DOMAIN must be a single DNS hostname, without scheme, port, path or wildcard")
	}
	if c.Server.TLSMode == "cloudflare" && (c.Server.TLSCert != "" || c.Server.TLSKey != "") {
		return fmt.Errorf("TLS_MODE=cloudflare conflicts with manual TLS_CERT/TLS_KEY")
	}
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		return fmt.Errorf("TLS_CERT and TLS_KEY must both be set")
	}
	switch c.Server.TLSMode {
	case "cloudflare":
		if c.Server.Domain == "" {
			return fmt.Errorf("PROXY_DOMAIN is required for Cloudflare mode")
		}
	case "auto":
		if c.Server.TLSCert != "" {
			return fmt.Errorf("TLS_MODE=auto conflicts with manual TLS_CERT/TLS_KEY")
		}
		if c.Server.Domain == "" {
			return fmt.Errorf("PROXY_DOMAIN is required for automatic HTTPS; point its DNS to this server")
		}
		if !c.ACME.AcceptTOS {
			return fmt.Errorf("automatic HTTPS requires ACME_ACCEPT_TOS=true")
		}
		if c.ACME.Directory == "" {
			return fmt.Errorf("ACME_DIRECTORY must not be empty")
		}
	case "manual":
		if c.Server.TLSCert == "" {
			return fmt.Errorf("TLS_MODE=manual requires TLS_CERT and TLS_KEY")
		}
	default:
		return fmt.Errorf("TLS_MODE must be auto, manual or cloudflare")
	}
	for k, v := range map[string]string{"HTTPS_LISTEN": c.Server.Listen, "HTTP_LISTEN": c.Server.HTTPListen} {
		if k == "HTTPS_LISTEN" && c.Server.TLSMode == "cloudflare" {
			continue
		}
		if _, _, e := net.SplitHostPort(v); e != nil {
			return fmt.Errorf("%s: expected host:port: %w", k, e)
		}
	}
	if c.Cache.Directory == "" || c.Cache.MaxSizeGB <= 0 || c.Cache.MaxSizeGB > 8589934591 {
		return fmt.Errorf("CACHE_DIRECTORY must be nonempty and CACHE_MAX_SIZE_GB must be positive and fit int64 bytes")
	}
	if c.Server.TLSMode == "auto" {
		a, _ := filepath.Abs(c.Cache.Directory)
		b, _ := filepath.Abs(c.ACME.Directory)
		if nested(a, b) || nested(b, a) {
			return fmt.Errorf("CACHE_DIRECTORY and ACME_DIRECTORY must be separate, non-nested directories")
		}
	}
	if c.Server.TLSMode != "cloudflare" && c.ACME.DirectoryURL != "" {
		u, e := url.Parse(c.ACME.DirectoryURL)
		if e != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("ACME_DIRECTORY_URL must be an HTTPS URL")
		}
	}
	if len(c.Registries) == 0 {
		return fmt.Errorf("registries must not be empty")
	}
	for name, r := range c.Registries {
		if !ValidRegistryDomain(name) {
			return fmt.Errorf("invalid registry key %q: use a registry domain (optionally with port), not a short alias", name)
		}
		u, e := url.Parse(r.Upstream)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("registry %s upstream must be an HTTPS URL without credentials, query or fragment", name)
		}
		if (r.Username == "") != (r.Password == "") || strings.Contains(r.Username, ":") || (r.Username != "" && strings.TrimSpace(r.Username) == "") {
			return fmt.Errorf("registry %s requires both username and password, with a nonblank username containing no colon", name)
		}
	}
	return nil
}
func nested(a, b string) bool {
	r, e := filepath.Rel(a, b)
	return e == nil && (r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator))))
}
func validDomain(s string) bool {
	if len(s) > 253 || net.ParseIP(s) != nil || !strings.Contains(s, ".") {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// ValidRegistryDomain accepts unambiguous registry hosts, optionally with a port.
func ValidRegistryDomain(s string) bool {
	host := s
	if strings.Contains(s, ":") {
		var port string
		var err error
		host, port, err = net.SplitHostPort(s)
		if err != nil || port == "" {
			return false
		}
		for _, c := range port {
			if c < '0' || c > '9' {
				return false
			}
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return host == "localhost" || validDomain(host)
}

// CheckDirectory proves that atomic cache writes can succeed without contacting a CA.
func CheckDirectory(path string) error {
	if e := os.MkdirAll(path, 0700); e != nil {
		return fmt.Errorf("directory %s: %w; check volume ownership and write permissions", path, e)
	}
	f, e := os.CreateTemp(path, ".write-check-*")
	if e != nil {
		return fmt.Errorf("directory %s is not writable: %w; check volume ownership", path, e)
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write([]byte("check")); e != nil {
		f.Close()
		return e
	}
	return f.Close()
}
