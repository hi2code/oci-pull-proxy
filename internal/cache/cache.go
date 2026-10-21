package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type cacheMeta struct {
	Headers map[string]string `json:"headers"`
}
type DiskCache struct {
	root     string
	maxBytes int64
	mu       sync.Mutex
}

func New(root string, maxBytes int64) *DiskCache { return &DiskCache{root: root, maxBytes: maxBytes} }
func Key(upstream, path string) string {
	sum := sha256.Sum256([]byte(upstream + "\n" + path))
	return hex.EncodeToString(sum[:])
}
func (c *DiskCache) paths(key string) (string, string) {
	dir := filepath.Join(c.root, key[:2])
	return filepath.Join(dir, key+".blob"), filepath.Join(dir, key+".json")
}
func (c *DiskCache) Open(key string) (*os.File, cacheMeta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	blob, metaPath := c.paths(key)
	f, err := os.Open(blob)
	if err != nil {
		return nil, cacheMeta{}, err
	}
	b, err := os.ReadFile(metaPath)
	if err != nil {
		f.Close()
		return nil, cacheMeta{}, err
	}
	var meta cacheMeta
	if err = json.Unmarshal(b, &meta); err != nil {
		f.Close()
		return nil, cacheMeta{}, err
	}
	_ = os.Chtimes(blob, time.Now(), time.Now())
	return f, meta, nil
}
func (c *DiskCache) StoreAndStream(w http.ResponseWriter, key string, resp *http.Response) {
	c.mu.Lock()
	blob, metaPath := c.paths(key)
	if err := os.MkdirAll(filepath.Dir(blob), 0700); err != nil {
		c.mu.Unlock()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(blob), ".cache-*")
	c.mu.Unlock()
	if err != nil {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	headers := map[string]string{}
	for _, k := range []string{"Content-Type", "Docker-Content-Digest", "ETag", "Last-Modified", "Docker-Distribution-Api-Version"} {
		if v := resp.Header.Get(k); v != "" {
			headers[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	writer := io.MultiWriter(w, tmp)
	_, copyErr := io.Copy(writer, resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err = os.Rename(tmp.Name(), blob); err != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	mb, _ := json.Marshal(cacheMeta{Headers: headers})
	mt, err := os.CreateTemp(filepath.Dir(metaPath), ".meta-*")
	if err == nil {
		_, err = mt.Write(mb)
		_ = mt.Close()
		if err == nil {
			err = os.Rename(mt.Name(), metaPath)
		}
		if err != nil {
			_ = os.Remove(mt.Name())
		}
	}
	c.evictLocked()
}
func (c *DiskCache) evictLocked() {
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	var files []item
	var total int64
	_ = filepath.Walk(c.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".blob") {
			files = append(files, item{path, info.Size(), info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	if total <= c.maxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= c.maxBytes {
			break
		}
		_ = os.Remove(f.path)
		_ = os.Remove(strings.TrimSuffix(f.path, ".blob") + ".json")
		total -= f.size
	}
}
func fileSize(f *os.File) int64 {
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	return st.Size()
}
