package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMirrorTokenPersistence(t *testing.T) {
	for _, empty := range []bool{false, true} {
		p := filepath.Join(t.TempDir(), ".mirror-token")
		if empty {
			if err := os.WriteFile(p, nil, 0644); err != nil {
				t.Fatal(err)
			}
		}
		first, err := ResolveToken("", p)
		if err != nil {
			t.Fatal(err)
		}
		if len(first) != 43 {
			t.Fatalf("length = %d", len(first))
		}
		second, err := ResolveToken("", p)
		if err != nil || first != second {
			t.Fatal("token not persisted", err)
		}
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0600 {
			t.Fatal("unsafe permissions")
		}
		manual, err := ResolveToken("manual_safe-123", p)
		if err != nil || manual != "manual_safe-123" {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(p)
		if string(data) != first {
			t.Fatal("manual token changed file")
		}
	}
}
func TestMirrorTokenRejectsInvalidOrInaccessibleFile(t *testing.T) {
	for _, value := range []string{"bad token", "a/b", "a=", "a\n"} {
		if _, err := ResolveToken(value, filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("accepted invalid configured token")
		}
	}
	for _, value := range []string{"bad token", "a/b", "\n", "truncated", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB"} {
		p := filepath.Join(t.TempDir(), "token")
		os.WriteFile(p, []byte(value), 0600)
		if _, err := ResolveToken("", p); err == nil {
			t.Fatal("accepted corrupt file")
		}
		data, _ := os.ReadFile(p)
		if string(data) != value {
			t.Fatal("replaced corrupt file")
		}
	}
	if _, err := ResolveToken("", t.TempDir()); err == nil {
		t.Fatal("accepted directory")
	}
	p := filepath.Join(t.TempDir(), "missing", "token")
	if _, err := ResolveToken("", p); err == nil {
		t.Fatal("ignored write failure")
	}
	if token, err := ResolveToken("manual", p); err != nil || token != "manual" {
		t.Fatal("file accessed with TOKEN configured")
	}
}

func TestMirrorTokenFilePermissionsFailClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	for _, mode := range []os.FileMode{0000, 0400, 0200} {
		p := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(p, []byte("existing_token"), mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveToken("", p); err == nil {
			t.Fatalf("accepted inaccessible file mode %o", mode)
		}
	}
}
