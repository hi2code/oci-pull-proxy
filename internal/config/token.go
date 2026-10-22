package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var safeToken = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var envAssignment = regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?([^ \t=:#]+)[ \t]*[:=]`)

// ResolveToken saves an automatically generated TOKEN in .env. It preserves the
// inode for file bind mounts and leaves other settings and comments unchanged.
// Each .env belongs to one running instance, as do the cache and certificate directories.
func ResolveToken(configured, path string) (string, error) {
	if configured == "v2" {
		return "", fmt.Errorf("TOKEN value v2 is reserved")
	}
	if configured != "" {
		if !safeToken.MatchString(configured) {
			return "", fmt.Errorf("TOKEN must contain only URL-safe letters, digits, - and _")
		}
		return configured, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return "", fmt.Errorf("open .env for TOKEN persistence: %w; mount .env writable by the container user", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf(".env must be a regular file")
	}
	const maxEnvSize = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxEnvSize+1))
	if err != nil {
		return "", fmt.Errorf("read .env: %w", err)
	}
	if len(data) > maxEnvSize {
		return "", fmt.Errorf(".env exceeds 1 MiB")
	}
	text := string(data)
	token, start, end, err := envToken(text)
	if err != nil {
		return "", err
	}
	if token != "" {
		return token, nil
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		return "", fmt.Errorf("generate TOKEN: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(random)
	if start >= 0 {
		separator := ""
		if end < len(text) && text[end] == '#' {
			separator = " "
		}
		text = text[:start] + token + separator + text[end:]
	} else {
		newline := "\n"
		if strings.Contains(text, "\r\n") {
			newline = "\r\n"
		}
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += newline
		}
		text += "TOKEN=" + token + newline
	}
	if _, err = f.WriteAt([]byte(text), 0); err != nil {
		return "", fmt.Errorf("save TOKEN to .env: %w", err)
	}
	if err = f.Truncate(int64(len(text))); err != nil {
		return "", fmt.Errorf("truncate .env: %w", err)
	}
	if err = f.Sync(); err != nil {
		return "", fmt.Errorf("sync .env: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("close .env: %w", err)
	}
	return token, nil
}

// envToken locates only TOKEN's value so the rest of .env can be preserved byte
// for byte. Quoted values may span lines; TOKEN-looking text inside them is data.
func envToken(text string) (token string, start, end int, err error) {
	start, end = -1, -1
	for pos := 0; pos < len(text); {
		lineEnd := len(text)
		if n := strings.IndexByte(text[pos:], '\n'); n >= 0 {
			lineEnd = pos + n
		}
		line := text[pos:lineEnd]
		match := envAssignment.FindStringSubmatchIndex(line)
		if match == nil {
			if s := strings.TrimSpace(line); s == "TOKEN" || s == "export TOKEN" {
				return "", -1, -1, fmt.Errorf(".env TOKEN requires an assignment")
			}
			pos = lineEnd + 1
			continue
		}
		key := line[match[2]:match[3]]
		valueStart := pos + match[1]
		valueEnd := lineEnd
		raw := text[valueStart:valueEnd]
		trimmed := strings.TrimLeft(raw, " \t")
		value := ""
		if len(trimmed) > 0 && (trimmed[0] == '\'' || trimmed[0] == '"') {
			valueStart += len(raw) - len(trimmed)
			quote := text[valueStart]
			closeAt := valueStart + 1
			for ; closeAt < len(text); closeAt++ {
				if text[closeAt] == '\\' {
					closeAt++
					continue
				}
				if text[closeAt] == quote {
					break
				}
			}
			if closeAt >= len(text) {
				return "", -1, -1, fmt.Errorf(".env contains an unterminated quoted value")
			}
			value = text[valueStart+1 : closeAt]
			valueEnd = closeAt + 1
			lineEnd = len(text)
			if n := strings.IndexByte(text[valueEnd:], '\n'); n >= 0 {
				lineEnd = valueEnd + n
			}
			tail := strings.TrimSpace(text[valueEnd:lineEnd])
			if key == "TOKEN" && tail != "" && !strings.HasPrefix(tail, "#") {
				return "", -1, -1, fmt.Errorf("invalid text after .env TOKEN")
			}
		} else {
			// An unquoted inline comment starts after whitespace (or an empty value).
			for i, c := range raw {
				if c == '#' && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
					raw = raw[:i]
					break
				}
			}
			value = strings.TrimSpace(raw)
			if value != "" {
				valueStart += len(raw) - len(strings.TrimLeft(raw, " \t"))
			}
			valueEnd = valueStart + len(value)
		}
		if key == "TOKEN" {
			if value == "v2" {
				return "", -1, -1, fmt.Errorf(".env TOKEN value v2 is reserved")
			}
			if start >= 0 {
				return "", -1, -1, fmt.Errorf(".env contains duplicate TOKEN assignments; keep only one")
			}
			if value != "" && !safeToken.MatchString(value) {
				return "", -1, -1, fmt.Errorf(".env TOKEN must contain only URL-safe letters, digits, - and _")
			}
			token, start, end = value, valueStart, valueEnd
		}
		pos = lineEnd + 1
	}
	return token, start, end, nil
}
