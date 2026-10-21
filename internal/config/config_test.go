package config

import (
	"os"
	"strings"
	"testing"
)

func base(t *testing.T) {
	t.Helper()
	t.Setenv("PROXY_DOMAIN", "images.example.com")
	t.Setenv("PROXY_USERNAME", "alice")
	t.Setenv("PROXY_PASSWORD", "a-real-long-secret")
	t.Setenv("ACME_ACCEPT_TOS", "true")
}
func TestDefaultsAndEnvironmentOverrides(t *testing.T) {
	base(t)
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	if c.Server.TLSMode != "auto" || c.Cache.MaxSizeGB != 40 || len(c.Registries) != 3 {
		t.Fatalf("defaults: %+v", c)
	}
	t.Setenv("CACHE_MAX_SIZE_GB", "7")
	c, e = Load()
	if e != nil || c.Cache.MaxSizeGB != 7 {
		t.Fatalf("environment override failed: %v", e)
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
	t.Setenv("ACME_ACCEPT_TOS", "false")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ACME_ACCEPT_TOS") {
		t.Fatalf("explicit false must be respected: %v", err)
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
	t.Setenv("TLS_CERT", "/cert.pem")
	t.Setenv("TLS_KEY", "/key.pem")
	c, e := Load()
	if e != nil || c.Server.TLSMode != "manual" {
		t.Fatalf("manual certificates: %v", e)
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
}

func TestRegistryEnvironment(t *testing.T) {
	base(t)
	t.Setenv("GHCR_USERNAME", "github-user")
	t.Setenv("GHCR_TOKEN", "github-secret")
	t.Setenv("DOCKERHUB_USERNAME", "hub-user")
	t.Setenv("DOCKERHUB_TOKEN", "hub-secret")
	t.Setenv("QUAY_USERNAME", "quay-user")
	t.Setenv("QUAY_TOKEN", "quay-secret")
	t.Setenv("REGISTRY_CORP_DOMAIN", "registry.example.com:5000")
	t.Setenv("REGISTRY_CORP_USERNAME", "robot")
	t.Setenv("REGISTRY_CORP_PASSWORD", "robot-secret")
	t.Setenv("REGISTRY_LAB_DOMAIN", "lab.example.com")
	t.Setenv("REGISTRY_LAB_UPSTREAM", "https://backend.example.com/registry")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Registries["ghcr.io"].Password != "github-secret" || c.Registries["registry.example.com:5000"].Password != "robot-secret" || c.Registries["ghcr.io"].Username != "github-user" || c.Registries["ghcr.io"].Upstream != "https://ghcr.io" ||
		c.Registries["docker.io"].Username != "hub-user" || c.Registries["quay.io"].Username != "quay-user" ||
		c.Registries["registry.example.com:5000"].Username != "robot" || c.Registries["registry.example.com:5000"].Upstream != "https://registry.example.com:5000" ||
		c.Registries["lab.example.com"].Upstream != "https://backend.example.com/registry" || len(c.Registries) != 5 {
		t.Fatal("registry environment was not loaded correctly")
	}
}

func TestInvalidRegistryEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"username only", map[string]string{"GHCR_USERNAME": "alice"}, "GHCR"},
		{"token only", map[string]string{"GHCR_TOKEN": "secret-must-not-leak"}, "GHCR"},
		{"invalid username", map[string]string{"GHCR_USERNAME": "a:b", "GHCR_TOKEN": "secret-must-not-leak"}, "GHCR"},
		{"invalid domain", map[string]string{"REGISTRY_CORP_DOMAIN": "https://corp.example.com"}, "REGISTRY_CORP_DOMAIN"},
		{"duplicate domain", map[string]string{"REGISTRY_CORP_DOMAIN": "ghcr.io"}, "duplicate"},
		{"duplicate custom domain", map[string]string{"REGISTRY_CORP_DOMAIN": "corp.example.com", "REGISTRY_LAB_DOMAIN": "corp.example.com"}, "duplicate"},
		{"insecure custom upstream", map[string]string{"REGISTRY_CORP_DOMAIN": "corp.example.com", "REGISTRY_CORP_UPSTREAM": "http://corp.example.com"}, "corp.example.com"},
		{"custom password only", map[string]string{"REGISTRY_CORP_DOMAIN": "corp.example.com", "REGISTRY_CORP_PASSWORD": "secret-must-not-leak"}, "REGISTRY_CORP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s error, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "secret-must-not-leak") {
				t.Fatal("error exposes credentials")
			}
		})
	}
}
