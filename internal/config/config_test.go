package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func base(t *testing.T) {
	t.Helper()
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("PROXY_DOMAIN", "images.example.com")
	t.Setenv("PROXY_USERNAME", "alice")
	t.Setenv("PROXY_PASSWORD", "a-real-long-secret")
	t.Setenv("ACME_ACCEPT_TOS", "true")
}
func TestDefaultsAndPrecedence(t *testing.T) {
	base(t)
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if c.Server.TLSMode != "auto" || c.Cache.MaxSizeGB != 40 || len(c.Registries) != 3 {
		t.Fatalf("defaults: %+v", c)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"server":{"domain":"json.example.com"},"auth":{"username":"json"},"cache":{"max_size_gb":7}}`), 0600)
	t.Setenv("CONFIG_FILE", p)
	c, e = Load()
	if e != nil {
		t.Fatal(e)
	}
	if c.Server.Domain != "images.example.com" || c.Auth.Username != "alice" || c.Cache.MaxSizeGB != 7 {
		t.Fatal("priority")
	}
	t.Setenv("PROXY_PASSWORD", "")
	if _, e = Load(); e == nil {
		t.Fatal("empty override accepted")
	}
}
func TestACMEAcceptanceDefaultsAndOverrides(t *testing.T) {
	base(t)
	if err := os.Unsetenv("ACME_ACCEPT_TOS"); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil || !c.ACME.AcceptTOS {
		t.Fatalf("automatic HTTPS should work without an ACME_ACCEPT_TOS override: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"acme":{"accept_tos":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", path)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ACME_ACCEPT_TOS") {
		t.Fatalf("explicit JSON false must be respected: %v", err)
	}
	t.Setenv("ACME_ACCEPT_TOS", "true")
	if _, err := Load(); err != nil {
		t.Fatalf("environment should override JSON: %v", err)
	}
}
func TestInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ k, v, w string }{
		{"PROXY_PASSWORD", "", "PROXY_PASSWORD"}, {"PROXY_PASSWORD", "replace-with-a-long-random-secret", "PROXY_PASSWORD"}, {"PROXY_DOMAIN", "", "PROXY_DOMAIN"}, {"PROXY_DOMAIN", "https://bad.example", "PROXY_DOMAIN"}, {"ACME_ACCEPT_TOS", "false", "ACME_ACCEPT_TOS"}, {"ACME_ACCEPT_TOS", "oops", "ACME_ACCEPT_TOS"}, {"TLS_MODE", "off", "TLS_MODE"}, {"CACHE_MAX_SIZE_GB", "-1", "CACHE_MAX_SIZE_GB"},
	} {
		t.Run(tc.k+tc.v, func(t *testing.T) {
			base(t)
			t.Setenv(tc.k, tc.v)
			_, e := Load()
			if e == nil || !strings.Contains(e.Error(), tc.w) {
				t.Fatalf("want %s error, got %v", tc.w, e)
			}
		})
	}
}
func TestManualCertificateAndConflicts(t *testing.T) {
	base(t)
	t.Setenv("PROXY_DOMAIN", "")
	t.Setenv("ACME_ACCEPT_TOS", "false")
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"server":{"tls_cert":"/cert.pem","tls_key":"/key.pem"},"auth":{"enabled":true,"username":"alice","password_env":"CUSTOM_SECRET"}}`), 0600)
	t.Setenv("CONFIG_FILE", p)
	t.Setenv("CUSTOM_SECRET", "custom-secret")
	os.Unsetenv("PROXY_PASSWORD")
	c, e := Load()
	if e != nil || c.Server.TLSMode != "manual" || c.Auth.Password != "custom-secret" {
		t.Fatalf("%+v %v", c, e)
	}
	t.Setenv("TLS_MODE", "auto")
	if _, e = Load(); e == nil {
		t.Fatal("conflict accepted")
	}
}

func TestCloudflareConfiguration(t *testing.T) {
	base(t)
	t.Setenv("TLS_MODE", "cloudflare")
	t.Setenv("HTTPS_LISTEN", "unused")
	t.Setenv("ACME_ACCEPT_TOS", "unused")
	t.Setenv("ACME_DIRECTORY_URL", "unused")
	t.Setenv("ACME_DIRECTORY", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.HTTPListen != ":8080" {
		t.Fatal(c.Server.HTTPListen)
	}
	for _, tc := range []struct{ k, v, want string }{
		{"PROXY_DOMAIN", "", "PROXY_DOMAIN"},
		{"TLS_CERT", "/unused.pem", "conflicts"},
		{"TLS_KEY", "/unused.key", "conflicts"},
		{"HTTP_LISTEN", "bad", "HTTP_LISTEN"},
	} {
		t.Run(tc.k, func(t *testing.T) {
			t.Setenv(tc.k, tc.v)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s: %v", tc.want, err)
			}
		})
	}
}

func TestRegistryDomainKeys(t *testing.T) {
	base(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Registries["docker.io"].Upstream != "https://registry-1.docker.io" || c.Registries["ghcr.io"].Upstream == "" || c.Registries["quay.io"].Upstream == "" {
		t.Fatal("missing domain defaults")
	}
	for _, key := range []string{"docker.io", "ghcr.io", "quay.io", "registry.example:5000", "localhost", "localhost:5000"} {
		c.Registries = map[string]RegistryConfig{key: {Upstream: "https://registry.example"}}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	for _, key := range []string{"dockerhub", "ghcr", "quay", "custom", "https://registry.example", "registry.example/path", "registry.example:abc", "registry.example:0", "registry.example:65536", "registry..example", "Registry.example", "registry.example:"} {
		c.Registries = map[string]RegistryConfig{key: {Upstream: "https://registry.example"}}
		if err := c.Validate(); err == nil {
			t.Errorf("accepted %q", key)
		}
	}
	for old, replacement := range map[string]string{"dockerhub": "docker.io", "ghcr": "ghcr.io", "quay": "quay.io"} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"registries":{"`+old+`":{"upstream":"https://registry.example"}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONFIG_FILE", path)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), replacement) {
			t.Errorf("missing migration guidance: %v", err)
		}
	}
}
