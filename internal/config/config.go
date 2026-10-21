// Package config loads defaults, optional JSON, and explicit environment overrides.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Server struct {
		Listen     string `json:"listen"`
		HTTPListen string `json:"http_listen"`
		Domain     string `json:"domain"`
		TLSMode    string `json:"tls_mode"`
		TLSCert    string `json:"tls_cert"`
		TLSKey     string `json:"tls_key"`
	} `json:"server"`
	ACME struct {
		AcceptTOS    bool   `json:"accept_tos"`
		Email        string `json:"email"`
		DirectoryURL string `json:"directory_url"`
		Directory    string `json:"directory"`
	} `json:"acme"`
	Auth struct {
		Enabled     bool   `json:"enabled"`
		Username    string `json:"username"`
		PasswordEnv string `json:"password_env"`
		Password    string `json:"-"`
	} `json:"auth"`
	Cache struct {
		Directory string `json:"directory"`
		MaxSizeGB int64  `json:"max_size_gb"`
	} `json:"cache"`
	Registries map[string]RegistryConfig `json:"registries"`
}
type RegistryConfig struct {
	Upstream    string `json:"upstream"`
	Username    string `json:"username,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`
}

func Load() (Config, error) {
	var c Config
	c.Server.Listen = ":8443"
	c.Server.HTTPListen = ":8080"
	c.Auth.Enabled = true
	c.Auth.PasswordEnv = "PROXY_PASSWORD"
	c.Cache.Directory = "/data/cache"
	c.Cache.MaxSizeGB = 40
	c.ACME.Directory = "/data/acme"
	c.ACME.AcceptTOS = true
	c.Registries = map[string]RegistryConfig{"docker.io": {Upstream: "https://registry-1.docker.io"}, "ghcr.io": {Upstream: "https://ghcr.io"}, "quay.io": {Upstream: "https://quay.io"}}
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		b, e := os.ReadFile(p)
		if e != nil {
			return c, fmt.Errorf("CONFIG_FILE: %w", e)
		}
		if e = json.Unmarshal(b, &c); e != nil {
			return c, fmt.Errorf("CONFIG_FILE JSON: %w", e)
		}
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
	c.Auth.Password = os.Getenv(c.Auth.PasswordEnv)
	if v, ok := os.LookupEnv("PROXY_PASSWORD"); ok {
		c.Auth.Password = v
	}
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
func (c Config) Validate() error {
	if !c.Auth.Enabled {
		return fmt.Errorf("auth.enabled must be true")
	}
	if strings.TrimSpace(c.Auth.Username) == "" || strings.Contains(c.Auth.Username, ":") {
		return fmt.Errorf("PROXY_USERNAME / auth.username must be nonempty and contain no colon")
	}
	p := strings.ToLower(strings.TrimSpace(c.Auth.Password))
	if p == "" || strings.Contains(p, "replace") || strings.Contains(p, "替换") || p == "changeme" || p == "your-password" {
		return fmt.Errorf("PROXY_PASSWORD (or auth.password_env) must contain a real nonempty password, not an example placeholder")
	}
	if c.Server.Domain != "" && !validDomain(c.Server.Domain) {
		return fmt.Errorf("PROXY_DOMAIN must be a single DNS hostname, without scheme, port, path or wildcard")
	}
	if c.Server.TLSMode == "cloudflare" && (c.Server.TLSCert != "" || c.Server.TLSKey != "") {
		return fmt.Errorf("TLS_MODE=cloudflare conflicts with manual TLS_CERT/TLS_KEY")
	}
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		return fmt.Errorf("TLS_CERT and TLS_KEY / server.tls_cert and server.tls_key must both be set")
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
		if replacement, ok := map[string]string{"dockerhub": "docker.io", "ghcr": "ghcr.io", "quay": "quay.io"}[name]; ok {
			return fmt.Errorf("legacy registry key %q: migrate to %q", name, replacement)
		}
		if !ValidRegistryDomain(name) {
			return fmt.Errorf("invalid registry key %q: use a registry domain (optionally with port), not a short alias", name)
		}
		u, e := url.Parse(r.Upstream)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("registry %s upstream must be an HTTPS URL without credentials, query or fragment", name)
		}
		if (r.Username == "") != (r.PasswordEnv == "") {
			return fmt.Errorf("registry %s requires both username and password_env", name)
		}
		if r.PasswordEnv != "" && os.Getenv(r.PasswordEnv) == "" {
			return fmt.Errorf("registry %s password_env %s is empty", name, r.PasswordEnv)
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
