package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMirrorTokenPersistence(t *testing.T) {
	for _, tc := range []struct{ name, before, after string }{
		{"missing", "", "TOKEN=<token>\n"},
		{"other dotted key", "OTHER.KEY='first\nTOKEN=\nlast'\n", "OTHER.KEY='first\nTOKEN=\nlast'\nTOKEN=<token>\n"},
		{"colon multiline", "OTHER: 'first\nTOKEN=\nlast'\n", "OTHER: 'first\nTOKEN=\nlast'\nTOKEN=<token>\n"},
		{"colon empty token", "TOKEN: # keep\n", "TOKEN:<token> # keep\n"},
		{"other settings", "# keep\nPROXY_PASSWORD=unchanged\nGHCR_TOKEN=private\n", "# keep\nPROXY_PASSWORD=unchanged\nGHCR_TOKEN=private\nTOKEN=<token>\n"},
		{"empty", "TOKEN=\nPROXY_USERNAME=alice\n", "TOKEN=<token>\nPROXY_USERNAME=alice\n"},
		{"comment without space", "TOKEN=# comment\n", "TOKEN=<token> # comment\n"},
		{"no final newline", "PROXY_USERNAME=alice", "PROXY_USERNAME=alice\nTOKEN=<token>\n"},
		{"quoted empty", "export TOKEN = '' # keep comment\n", "export TOKEN = <token> # keep comment\n"},
		{"crlf", "# keep\r\nTOKEN= # comment\r\n", "# keep\r\nTOKEN=<token> # comment\r\n"},
		{"multiline", "OTHER='first\nTOKEN=inside-value\nlast'\n# TOKEN=comment\n", "OTHER='first\nTOKEN=inside-value\nlast'\n# TOKEN=comment\nTOKEN=<token>\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), ".env")
			if tc.name != "missing" {
				if err := os.WriteFile(p, []byte(tc.before), 0660); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.Stat(p)
			token, err := ResolveToken("", p)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
			if err != nil || len(decoded) != 32 {
				t.Fatal("invalid generated token")
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != strings.ReplaceAll(tc.after, "<token>", token) {
				t.Fatal("other settings/comments were changed or TOKEN was not saved")
			}
			after, _ := os.Stat(p)
			if before != nil && (!os.SameFile(before, after) || before.Mode().Perm() != after.Mode().Perm()) {
				t.Fatal("replaced mounted inode or changed existing permissions")
			}
			if before == nil && after.Mode().Perm() != 0600 {
				t.Fatal("new .env has unsafe permissions")
			}
			again, err := ResolveToken("", p)
			if err != nil || again != token {
				t.Fatal("restart changed token", err)
			}
			manual, err := ResolveToken("manual_safe-123", p)
			if err != nil || manual != "manual_safe-123" {
				t.Fatal(err)
			}
			unchanged, _ := os.ReadFile(p)
			if string(unchanged) != string(data) {
				t.Fatal("existing or explicit token rewrote .env")
			}
		})
	}
}

func TestMirrorTokenReadsEnvValues(t *testing.T) {
	for _, value := range []string{"TOKEN=manual-uuid_123\n", "TOKEN: manual-uuid_123\n", "TOKEN='manual-uuid_123' # comment\n", "export TOKEN = \"manual-uuid_123\"\r\n", "TOKEN=manual-uuid_123 # comment\n"} {
		p := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(p, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		token, err := ResolveToken("", p)
		if err != nil || token != "manual-uuid_123" {
			t.Fatal("did not reuse saved token", err)
		}
		data, _ := os.ReadFile(p)
		if string(data) != value {
			t.Fatal("rewrote existing token")
		}
	}
}

func TestMirrorTokenRejectsInvalidOrInaccessibleFile(t *testing.T) {
	for _, value := range []string{"bad token", "a/b", "a=", "a\n"} {
		if _, err := ResolveToken(value, filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("accepted invalid configured token")
		}
	}
	for _, value := range []string{"TOKEN=bad token\n", "TOKEN=a/b\n", "TOKEN='unclosed\n", "TOKEN=one\nTOKEN=two\n", "TOKEN=\"valid\"garbage\n", "TOKEN\n"} {
		p := filepath.Join(t.TempDir(), ".env")
		os.WriteFile(p, []byte(value), 0600)
		if _, err := ResolveToken("", p); err == nil {
			t.Fatal("accepted invalid .env TOKEN")
		}
		data, _ := os.ReadFile(p)
		if string(data) != value {
			t.Fatal("modified invalid file")
		}
	}
	if _, err := ResolveToken("", t.TempDir()); err == nil {
		t.Fatal("accepted directory")
	}
	p := filepath.Join(t.TempDir(), "missing", ".env")
	if _, err := ResolveToken("", p); err == nil {
		t.Fatal("ignored write failure")
	}
	if token, err := ResolveToken("manual", p); err != nil || token != "manual" {
		t.Fatal("file accessed with explicit TOKEN")
	}
}

func TestMirrorTokenFilePermissionsFailClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	for _, mode := range []os.FileMode{0000, 0400, 0200} {
		p := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(p, []byte("TOKEN=\n"), mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveToken("", p); err == nil {
			t.Fatalf("accepted inaccessible file mode %o", mode)
		}
	}
}
