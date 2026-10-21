package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"regexp"
)

var safeToken = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ResolveToken preserves the mounted inode: atomic rename would break a file bind mount.
// The token file, like the cache directories, belongs to one running instance.
func ResolveToken(configured, path string) (string, error) {
	if configured != "" {
		if !safeToken.MatchString(configured) {
			return "", fmt.Errorf("TOKEN must contain only URL-safe letters, digits, - and _")
		}
		return configured, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return "", fmt.Errorf("mirror token file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("mirror token file must be a regular file")
	}
	if err = f.Chmod(0600); err != nil {
		return "", fmt.Errorf("mirror token permissions: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 44))
	if err != nil {
		return "", fmt.Errorf("read mirror token: %w", err)
	}
	if len(data) > 0 {
		decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(string(data))
		if decodeErr != nil || len(data) != 43 || !safeToken.Match(data) || len(decoded) != 32 {
			return "", fmt.Errorf("invalid mirror token file; expected canonical 32-byte Base64URL token without whitespace")
		}
		return string(data), nil
	}
	data = make([]byte, 32)
	if _, err = rand.Read(data); err != nil {
		return "", fmt.Errorf("generate mirror token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	if _, err = f.WriteString(token); err != nil {
		return "", fmt.Errorf("save mirror token: %w", err)
	}
	if err = f.Sync(); err != nil {
		return "", fmt.Errorf("sync mirror token: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("close mirror token: %w", err)
	}
	return token, nil
}
